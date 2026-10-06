# Load .env file if it exists (for persistent config overrides)
-include .env

# Configurable variables — override via environment, command line, or .env file
IMAGE     ?= quay.io/juntao_wang/rhoai-nightly-updater
TAG       ?= latest
# Same 8-character commit tag as GitLab CI's CI_COMMIT_SHORT_SHA.
GIT_SHA   ?= $(shell git rev-parse HEAD 2>/dev/null | cut -c1-8 || echo "dev")
GIT_FULL_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
NAMESPACE ?= rhoai-nightly-updater
APP_NAME  ?= rhoai-nightly-updater
RUNTIME   ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null || echo "podman")
PLATFORM  ?= linux/amd64
# The oauth-proxy tag must match the cluster's OCP minor version; detected
# from the logged-in cluster unless set.
OAUTH_PROXY_IMAGE ?= registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v$(shell oc version -o json 2>/dev/null | sed -n 's/.*"openshiftVersion": *"\([0-9]*\.[0-9]*\).*/\1/p')
# Rollouts wait for running cluster operations (up to ~17 minutes).
ROLLOUT_TIMEOUT ?= 20m
INSTANCE  = $(APP_NAME)-$(NAMESPACE)
PROXY_SECRET = $(APP_NAME)-proxy

.PHONY: all build push deploy upgrade rollback undeploy dev lint lint-go lint-frontend test test-go test-frontend vuln clean help env \
	ensure-proxy-secret apply-template consolelink cleanup-legacy resolve-image prune-replicasets

.DEFAULT_GOAL := help

all: build push deploy  ## Build, push, and deploy in one shot

build:  ## Build container image (auto-detects podman or docker, use NO_CACHE=1 to skip cache)
	@command -v $(RUNTIME) >/dev/null 2>&1 || { echo "Error: $(RUNTIME) is not installed."; exit 1; }
	$(RUNTIME) build --platform $(PLATFORM) $(if $(NO_CACHE),--no-cache,) \
		--build-arg GIT_SHA=$(GIT_FULL_SHA) --build-arg VERSION=$(GIT_SHA) --build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(TAG) -f Containerfile .
	$(RUNTIME) tag $(IMAGE):$(TAG) $(IMAGE):$(GIT_SHA)

push:  ## Push image to registry (latest + 8-character git SHA tags)
	$(RUNTIME) push $(IMAGE):$(TAG)
	$(RUNTIME) push $(IMAGE):$(GIT_SHA)

# deploy and upgrade apply an immutable reference: scripts/resolve-image.sh
# resolves IMAGE:TAG to IMAGE@sha256:... at run time and checks that the
# image was built from a commit whose deploy/template.yaml matches this
# checkout (uncommitted edits included). An image only works with the
# template of its own commit (ports, probes, environment, RBAC): for example
# an image built before the metrics listener never answers the probes on
# METRICS_PORT, and with the Recreate strategy the old pod is already gone.
# A failed lookup, an unknown revision or a different template stops the
# target. Overrides: ALLOW_TEMPLATE_MISMATCH=1 (skip the revision and
# template checks), ALLOW_MUTABLE_TAG=1 (deploy the tag when no digest can
# be resolved). `make rollback` applies an older image's own template.
RESOLVE_IMAGE = IMAGE='$(IMAGE)' TAG='$(TAG)' PLATFORM='$(PLATFORM)' ALLOW_TEMPLATE_MISMATCH='$(ALLOW_TEMPLATE_MISMATCH)' ALLOW_MUTABLE_TAG='$(ALLOW_MUTABLE_TAG)' ./scripts/resolve-image.sh
# The resolved reference is computed once per run with $(eval), so every
# step applies the same digest. Recipe lines that call $(MAKE) hold nothing
# else: `make -n` runs such lines, and the sub-make then only prints.
RESOLVED_REF_CHECK = @test -n "$(RESOLVED_REF)" || { echo "Nothing was changed."; exit 1; }

deploy:  ## First install on OpenShift (requires cluster-admin and oc login; DRY_RUN=1 validates only)
	$(eval RESOLVED_REF := $(shell $(RESOLVE_IMAGE)))
	$(RESOLVED_REF_CHECK)
	@$(if $(DRY_RUN),true,oc get project $(NAMESPACE) >/dev/null 2>&1 || oc new-project $(NAMESPACE))
	@$(if $(DRY_RUN),true,$(MAKE) --no-print-directory ensure-proxy-secret)
	@$(MAKE) --no-print-directory apply-template IMAGE_REF=$(RESOLVED_REF)
	@$(if $(DRY_RUN),echo "Dry run only; nothing was changed.",oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=$(ROLLOUT_TIMEOUT))
	@$(if $(DRY_RUN),true,$(MAKE) --no-print-directory consolelink cleanup-legacy prune-replicasets)
	@$(if $(DRY_RUN),true,echo "Deployed $(RESOLVED_REF).")

upgrade:  ## Upgrade to :latest (or TAG=<commit>) by digest: re-apply the template (keeps sessions), wait (DRY_RUN=1 validates only)
	@oc get deployment $(APP_NAME) -n $(NAMESPACE) >/dev/null || { echo "No deployment $(APP_NAME) in $(NAMESPACE); run 'make deploy' first."; exit 1; }
	$(eval RESOLVED_REF := $(shell $(RESOLVE_IMAGE)))
	$(RESOLVED_REF_CHECK)
	@$(if $(DRY_RUN),true,echo "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes).")
	@$(if $(DRY_RUN),true,$(MAKE) --no-print-directory ensure-proxy-secret)
	@$(MAKE) --no-print-directory apply-template IMAGE_REF=$(RESOLVED_REF)
	@$(if $(DRY_RUN),echo "Dry run only; nothing was changed.",oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=$(ROLLOUT_TIMEOUT))
	@$(if $(DRY_RUN),true,$(MAKE) --no-print-directory consolelink cleanup-legacy prune-replicasets)
	@$(if $(DRY_RUN),true,echo "Upgraded to $(RESOLVED_REF). Verify with ./scripts/smoke-test.sh $(NAMESPACE) $(APP_NAME)")


resolve-image:  ## Print the digest reference deploy/upgrade would apply for IMAGE:TAG, after the template check
	@$(RESOLVE_IMAGE)

# After a successful rollout, delete the Deployment's old ReplicaSets
# (selected by its label and owner, scaled to 0, other revision). Their pod
# templates keep whatever the old pods had, for example the plaintext
# COOKIE_SECRET env of installs from before the proxy Secret, which would
# otherwise stay readable. Rolling back uses `make rollback`, not the
# ReplicaSet history.
prune-replicasets:
	@set -e; \
	CUR=$$(oc get deployment $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}'); \
	UID_=$$(oc get deployment $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.metadata.uid}'); \
	[ -n "$$CUR" ] && [ -n "$$UID_" ] || { echo "Cannot read the revision of deployment/$(APP_NAME); old ReplicaSets were kept."; exit 1; }; \
	OLD=$$(oc get rs -n $(NAMESPACE) -l app=$(APP_NAME) -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.deployment\.kubernetes\.io/revision}{" "}{.metadata.ownerReferences[0].uid}{" "}{.spec.replicas}{" "}{.status.replicas}{"\n"}{end}' \
		| awk -v cur="$$CUR" -v uid="$$UID_" '$$2 != cur && $$3 == uid && $$4 == "0" && ($$5 == "0" || $$5 == "") {print $$1}'); \
	if [ -z "$$OLD" ]; then echo "No old ReplicaSets to remove."; exit 0; fi; \
	echo "Removing old ReplicaSets (their pod templates may hold old secrets): $$OLD"; \
	oc delete rs -n $(NAMESPACE) $$OLD

# Rolls back (or pins) to the build of an older commit with that commit's
# own deploy/template.yaml, so ports, probes and RBAC match its code. The
# cookie secret is kept, so sessions survive: a template that still takes a
# COOKIE_SECRET parameter gets the value of the current Secret. Legacy
# cluster objects that an old template creates are left alone here (the old
# build needs them); the next `make upgrade` removes them again. Roll
# forward with `make upgrade`. DRY_RUN=1 only validates with the API server.
rollback:  ## Roll back to an older build with that commit's own template: TAG=<commit> (DRY_RUN=1 validates only)
	@case "$(TAG)" in latest|"") echo "Usage: make rollback TAG=<commit> (an image tag such as 4503bb7d, or any commit in this clone)"; exit 1;; esac
	@case "$(OAUTH_PROXY_IMAGE)" in *:v) echo "Cannot detect the OCP version; set OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>"; exit 1;; esac
	@oc get deployment $(APP_NAME) -n $(NAMESPACE) >/dev/null || { echo "No deployment $(APP_NAME) in $(NAMESPACE); run 'make deploy' first."; exit 1; }
	@$(if $(DRY_RUN),true,$(MAKE) --no-print-directory ensure-proxy-secret)
	@set -e; \
	REV=$$(git rev-parse --verify --quiet "$(TAG)^{commit}") || { echo "Commit $(TAG) is not in this clone; run 'git fetch origin' and retry."; exit 1; }; \
	git cat-file -e "$$REV:deploy/template.yaml" 2>/dev/null || { echo "Commit $$REV has no deploy/template.yaml."; exit 1; }; \
	SHORT8=$$(echo "$$REV" | cut -c1-8); SHORT7=$$(echo "$$REV" | cut -c1-7); IMG_TAG=; \
	for t in "$(TAG)" "$$SHORT8" "$$SHORT7"; do \
		if oc image info "$(IMAGE):$$t" --filter-by-os=$(PLATFORM) >/dev/null 2>&1; then IMG_TAG=$$t; break; fi; \
	done; \
	[ -n "$$IMG_TAG" ] || { echo "No image $(IMAGE) for commit $$REV (tried the tags $(TAG), $$SHORT8 and $$SHORT7)."; exit 1; }; \
	LABEL=$$(oc image info "$(IMAGE):$$IMG_TAG" --filter-by-os=$(PLATFORM) -o json 2>/dev/null | sed -n 's/.*"org.opencontainers.image.revision": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -1); \
	if [ -n "$$LABEL" ] && [ "$$LABEL" != "$$REV" ] && git cat-file -e "$$LABEL^{commit}" 2>/dev/null; then \
		if [ "$(ALLOW_TEMPLATE_MISMATCH)" = "1" ]; then \
			echo "WARNING: ALLOW_TEMPLATE_MISMATCH=1: $(IMAGE):$$IMG_TAG reports revision $$LABEL, not $$REV; applying the template of $$REV anyway."; \
		else \
			echo "ERROR: $(IMAGE):$$IMG_TAG reports revision $$LABEL, not $$REV, so the template of $$REV may not fit it. Nothing was changed."; \
			echo "  Use TAG=$$LABEL, or set ALLOW_TEMPLATE_MISMATCH=1 to apply the template of $$REV anyway."; exit 1; \
		fi; \
	fi; \
	TMP=$$(mktemp -d); trap 'rm -rf "$$TMP"' EXIT; umask 077; \
	git show "$$REV:deploy/template.yaml" > "$$TMP/template.yaml"; \
	printf '%s\n' "IMAGE=$(IMAGE):$$IMG_TAG" "NAMESPACE=$(NAMESPACE)" "APP_NAME=$(APP_NAME)" "OAUTH_PROXY_IMAGE=$(OAUTH_PROXY_IMAGE)" > "$$TMP/params"; \
	if oc process -f "$$TMP/template.yaml" --parameters | awk 'NR > 1 {print $$1}' | grep -qx COOKIE_SECRET; then \
		SECRET=$$(oc get secret $(PROXY_SECRET) -n $(NAMESPACE) -o jsonpath='{.data.session_secret}' 2>/dev/null | base64 -d 2>/dev/null || true); \
		[ -n "$$SECRET" ] || SECRET=$$(oc get deployment $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].env[?(@.name=="COOKIE_SECRET")].value}' 2>/dev/null || true); \
		[ $${#SECRET} -eq 32 ] || { echo "Could not read the current 32-character cookie secret (Secret $(PROXY_SECRET))."; exit 1; }; \
		printf 'COOKIE_SECRET=%s\n' "$$SECRET" >> "$$TMP/params"; \
	fi; \
	echo "$(if $(DRY_RUN),Validating,Applying) $(IMAGE):$$IMG_TAG with the template of commit $$REV..."; \
	oc process -f "$$TMP/template.yaml" --param-file="$$TMP/params" --ignore-unknown-parameters | oc apply $(if $(DRY_RUN),--dry-run=server,) -f -
	@$(if $(DRY_RUN),echo "Dry run only; nothing was changed.",echo "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)." && oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=$(ROLLOUT_TIMEOUT) && echo "Rolled back. Roll forward again with: make upgrade")

# The oauth-proxy cookie secret lives in a Secret that the template only
# references, so re-applying keeps everyone's session. An existing install
# keeps the value it had in its Deployment env (COOKIE_SECRET).
ensure-proxy-secret:
	@if oc get secret $(PROXY_SECRET) -n $(NAMESPACE) >/dev/null 2>&1; then \
		echo "Secret $(PROXY_SECRET) exists; keeping it."; \
	else \
		SECRET=$$(oc get deployment $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].env[?(@.name=="COOKIE_SECRET")].value}' 2>/dev/null); \
		if [ -z "$$SECRET" ]; then SECRET=$$(head -c 64 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32); fi; \
		[ $${#SECRET} -eq 32 ] || { echo "Could not produce a 32-character cookie secret"; exit 1; }; \
		oc create secret generic $(PROXY_SECRET) -n $(NAMESPACE) --from-literal=session_secret="$$SECRET" >/dev/null && \
		echo "Created Secret $(PROXY_SECRET). Old ReplicaSets, which still hold the cookie secret in their pod template, are removed after the rollout."; \
	fi

# IMAGE_REF is the reference deploy and upgrade resolved (IMAGE@sha256:...).
IMAGE_REF ?= $(IMAGE):$(TAG)
apply-template:
	@case "$(OAUTH_PROXY_IMAGE)" in *:v) echo "Cannot detect the OCP version; set OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>"; exit 1;; esac
	oc process -f deploy/template.yaml \
		-p IMAGE=$(IMAGE_REF) \
		-p NAMESPACE=$(NAMESPACE) \
		-p APP_NAME=$(APP_NAME) \
		-p OAUTH_PROXY_IMAGE=$(OAUTH_PROXY_IMAGE) | oc apply $(if $(DRY_RUN),--dry-run=server,) -f -

# The ConsoleLink needs the Route host, which the router assigns.
consolelink:
	@ROUTE_HOST=$$(oc get route $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.spec.host}'); \
	[ -n "$$ROUTE_HOST" ] || { echo "Route $(APP_NAME) has no host yet"; exit 1; }; \
	echo "App URL: https://$$ROUTE_HOST"; \
	printf '%s\n' \
		'apiVersion: console.openshift.io/v1' \
		'kind: ConsoleLink' \
		'metadata:' \
		'  name: $(INSTANCE)' \
		'  labels:' \
		'    app.kubernetes.io/instance: $(INSTANCE)' \
		'spec:' \
		'  applicationMenu:' \
		'    section: Red Hat Applications' \
		'    imageURL: data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCIgZmlsbD0iI0VFMDAwMCI+PHBhdGggZD0iTTEyIDJMMyA3djEwbDkgNSA5LTVWN2wtOS01em0wIDIuMThMMTggNy4yN3Y3LjQ2TDEyIDE5LjgyIDYgMTQuNzNWNy4yN0wxMiA0LjE4eiIvPjwvc3ZnPg==' \
		"  href: https://$$ROUTE_HOST" \
		'  location: ApplicationMenu' \
		'  text: RHOAI Nightly Updater' | oc apply -f -

# Installs from before the namespace-suffixed names used a ClusterRole,
# ClusterRoleBinding and ConsoleLink named $(APP_NAME). Remove them only
# when they belong to this install (bound to this namespace's ServiceAccount
# or linking to this Route), after the new RBAC is in place.
cleanup-legacy:
	@SUBJ=$$(oc get clusterrolebinding $(APP_NAME) -o jsonpath='{.subjects[0].namespace}/{.subjects[0].name}' 2>/dev/null); \
	if [ "$$SUBJ" = "$(NAMESPACE)/$(APP_NAME)" ]; then \
		oc delete clusterrolebinding $(APP_NAME) && oc delete clusterrole $(APP_NAME) --ignore-not-found; \
	fi; \
	ROUTE_HOST=$$(oc get route $(APP_NAME) -n $(NAMESPACE) -o jsonpath='{.spec.host}' 2>/dev/null); \
	HREF=$$(oc get consolelink $(APP_NAME) -o jsonpath='{.spec.href}' 2>/dev/null); \
	if [ -n "$$HREF" ] && { [ "$$HREF" = "https://$$ROUTE_HOST" ] || [ "$$HREF" = "https://placeholder.apps.example.com" ]; }; then \
		oc delete consolelink $(APP_NAME); \
	fi

undeploy:  ## Remove the app, its cluster-wide RBAC, Roles in other namespaces and the ConsoleLink
	oc delete consolelink,clusterrolebinding,clusterrole -l app.kubernetes.io/instance=$(INSTANCE) --ignore-not-found
	for ns in kube-system openshift-marketplace openshift-ingress; do \
		oc delete rolebinding,role -n $$ns -l app.kubernetes.io/instance=$(INSTANCE) --ignore-not-found; \
	done
	@$(MAKE) --no-print-directory cleanup-legacy
	oc delete project $(NAMESPACE)

dev:  ## Start local dev server (Go backend + React frontend)
	./dev.sh

lint: lint-go lint-frontend  ## Run all linters

lint-go:  ## Run Go linters (gofmt, go vet, golangci-lint v2 with .golangci.yml)
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install golangci-lint v2: https://golangci-lint.run/docs/welcome/install/local/"; exit 1; }
	golangci-lint run ./...

lint-frontend:  ## Type-check and lint the frontend
	cd frontend && npm run typecheck && npm run lint

test: test-go test-frontend  ## Run all tests

test-go:  ## Run Go tests with the race detector
	go test -race -count=1 ./...

test-frontend:  ## Run frontend tests
	cd frontend && npm test

vuln:  ## Check Go and npm dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	cd frontend && npm audit --omit=dev --audit-level=high

clean:  ## Remove build artifacts
	rm -f server
	rm -rf frontend/dist
	rm -rf frontend/node_modules

env:  ## Create .env file from example for local config
	@if [ -f .env ]; then echo ".env already exists"; else \
	echo "# Nightly Updater — local config (not committed to git)" > .env; \
	echo "# IMAGE=quay.io/your-org/rhoai-nightly-updater" >> .env; \
	echo "# TAG=latest" >> .env; \
	echo "# NAMESPACE=rhoai-nightly-updater" >> .env; \
	echo "# APP_NAME=rhoai-nightly-updater" >> .env; \
	echo "# RUNTIME=podman" >> .env; \
	echo "# PLATFORM=linux/amd64" >> .env; \
	echo "Created .env — edit it with your values"; fi

help:  ## Show this help
	@echo "RHOAI Nightly Updater"
	@echo ""
	@echo "Usage: make [target] [VAR=value ...]"
	@echo ""
	@echo "Configuration (override via env, command line, or .env file):"
	@echo "  IMAGE      = $(IMAGE)"
	@echo "  TAG        = $(TAG)"
	@echo "  GIT_SHA    = $(GIT_SHA)"
	@echo "  NAMESPACE  = $(NAMESPACE)"
	@echo "  APP_NAME   = $(APP_NAME)"
	@echo "  RUNTIME    = $(RUNTIME)"
	@echo "  PLATFORM   = $(PLATFORM)"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Examples:"
	@echo "  make all                                Build, push, and deploy (uses defaults)"
	@echo "  make upgrade                            Re-apply the template on the digest of :latest"
	@echo "  make upgrade DRY_RUN=1                  Only validate the upgrade with the API server"
	@echo "  make rollback TAG=4503bb7d              Roll back to an older build with that commit's own template"
	@echo "  make rollback TAG=4503bb7d DRY_RUN=1    Only validate the rollback with the API server"
	@echo "  make build IMAGE=quay.io/myorg/myapp    Build with custom registry"
	@echo "  make build RUNTIME=docker               Build with docker instead of podman"
	@echo "  make deploy NAMESPACE=my-ns             Deploy to custom namespace"
	@echo "  make env                                Create .env file for persistent config"
	@echo "  make test                               Run all tests"
