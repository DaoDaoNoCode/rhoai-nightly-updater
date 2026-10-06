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
	ensure-proxy-secret apply-template consolelink cleanup-legacy check-image-template

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

deploy:  ## First install on OpenShift (requires cluster-admin and oc login)
	@$(MAKE) --no-print-directory check-image-template
	oc get project $(NAMESPACE) >/dev/null 2>&1 || oc new-project $(NAMESPACE)
	@$(MAKE) --no-print-directory ensure-proxy-secret apply-template
	@echo "Restarting deployment to pull $(IMAGE):$(TAG)..."
	oc rollout restart deployment/$(APP_NAME) -n $(NAMESPACE)
	oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=$(ROLLOUT_TIMEOUT)
	@$(MAKE) --no-print-directory consolelink cleanup-legacy

upgrade:  ## Upgrade to :latest (or TAG=<this checkout's commit>): re-apply the template (keeps sessions), restart, wait
	@oc get deployment $(APP_NAME) -n $(NAMESPACE) >/dev/null || { echo "No deployment $(APP_NAME) in $(NAMESPACE); run 'make deploy' first."; exit 1; }
	@$(MAKE) --no-print-directory check-image-template
	@echo "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)."
	@$(MAKE) --no-print-directory ensure-proxy-secret apply-template
	oc rollout restart deployment/$(APP_NAME) -n $(NAMESPACE)
	oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=$(ROLLOUT_TIMEOUT)
	@$(MAKE) --no-print-directory consolelink cleanup-legacy
	@echo "Upgraded. Verify with ./scripts/smoke-test.sh $(NAMESPACE) $(APP_NAME)"

# An image only works with the deploy/template.yaml of its own commit: the
# template sets its ports, probes, environment and RBAC. For example an image
# built before the metrics listener never answers the probes on METRICS_PORT,
# and with the Recreate strategy the old pod is already gone, so the tool
# stays down. Image tags are commits (CI_COMMIT_SHORT_SHA, 8 characters;
# older builds have 7) or latest, whose org.opencontainers.image.revision
# label names its commit. deploy and upgrade refuse an image whose commit
# has a different template than this checkout; `make rollback` applies the
# image's own template instead. ALLOW_TEMPLATE_MISMATCH=1 skips the check.
check-image-template:
	@if [ "$(ALLOW_TEMPLATE_MISMATCH)" = "1" ]; then \
		echo "WARNING: ALLOW_TEMPLATE_MISMATCH=1: not checking that $(IMAGE):$(TAG) matches deploy/template.yaml."; exit 0; \
	fi; \
	if [ "$(TAG)" = "latest" ]; then \
		REV=$$(oc image info "$(IMAGE):latest" --filter-by-os=$(PLATFORM) -o json 2>/dev/null | sed -n 's/.*"org.opencontainers.image.revision": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -1); \
		if [ -z "$$REV" ] || ! git cat-file -e "$$REV^{commit}" 2>/dev/null; then \
			echo "WARNING: cannot tell which commit $(IMAGE):latest was built from ($${REV:-no revision label})."; \
			echo "  Make sure this checkout is up to date with origin/main (git pull) so its template matches the image."; \
			exit 0; \
		fi; \
		WHAT="$(IMAGE):latest (built from $$REV)"; \
	else \
		REV=$$(git rev-parse --verify --quiet "$(TAG)^{commit}") || { \
			echo "ERROR: TAG=$(TAG) is not a commit in this clone (run 'git fetch origin'), so the template it needs is unknown."; \
			echo "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."; exit 1; }; \
		[ "$$REV" != "$$(git rev-parse HEAD)" ] || exit 0; \
		WHAT="TAG=$(TAG) (commit $$REV)"; \
	fi; \
	git diff --quiet "$$REV" -- deploy/template.yaml && exit 0; \
	echo "ERROR: deploy/template.yaml in this checkout differs from the template $$WHAT was built with."; \
	echo "  To run an older build, use its own template:  make rollback TAG=<commit>"; \
	echo "  For :latest, update this checkout (git pull) or wait until CI has published it."; \
	echo "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."; \
	exit 1

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
	if [ -n "$$LABEL" ] && [ "$$LABEL" != "$$REV" ]; then echo "WARNING: $(IMAGE):$$IMG_TAG reports revision $$LABEL, not $$REV."; fi; \
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
		echo "Created Secret $(PROXY_SECRET)."; \
	fi

apply-template:
	@case "$(OAUTH_PROXY_IMAGE)" in *:v) echo "Cannot detect the OCP version; set OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>"; exit 1;; esac
	oc process -f deploy/template.yaml \
		-p IMAGE=$(IMAGE):$(TAG) \
		-p NAMESPACE=$(NAMESPACE) \
		-p APP_NAME=$(APP_NAME) \
		-p OAUTH_PROXY_IMAGE=$(OAUTH_PROXY_IMAGE) | oc apply -f -

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
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
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
	@echo "  make upgrade                            Re-apply the template and restart on :latest"
	@echo "  make rollback TAG=4503bb7d              Roll back to an older build with that commit's own template"
	@echo "  make rollback TAG=4503bb7d DRY_RUN=1    Only validate the rollback with the API server"
	@echo "  make build IMAGE=quay.io/myorg/myapp    Build with custom registry"
	@echo "  make build RUNTIME=docker               Build with docker instead of podman"
	@echo "  make deploy NAMESPACE=my-ns             Deploy to custom namespace"
	@echo "  make env                                Create .env file for persistent config"
	@echo "  make test                               Run all tests"
