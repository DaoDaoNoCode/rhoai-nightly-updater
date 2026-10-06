# Load .env file if it exists (for persistent config overrides)
-include .env

# Configurable variables — override via environment, command line, or .env file
IMAGE     ?= quay.io/juntao_wang/rhoai-nightly-updater
# The release tag HEAD is on (vX.Y.Z), else empty: deploy and upgrade then
# ask for a release checkout or an explicit TAG=main / TAG=<commit>.
RELEASE_TAG := $(shell v=$$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null); echo "$$v" | grep -Ex 'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)')
ifeq ($(origin TAG),undefined)
TAG := $(RELEASE_TAG)
TAG_DEFAULTED := 1
endif
# Same 8-character commit tag as GitLab CI's CI_COMMIT_SHORT_SHA.
GIT_SHA   ?= $(shell git rev-parse HEAD 2>/dev/null | cut -c1-8 || echo "dev")
GIT_FULL_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
# The version string a local build reports: the release tag, else the commit.
BUILD_VERSION ?= $(or $(RELEASE_TAG),$(GIT_SHA))
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
NAMESPACE ?= rhoai-nightly-updater
APP_NAME  ?= rhoai-nightly-updater
RUNTIME   ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null || echo "podman")
PLATFORM  ?= linux/amd64
# The oauth-proxy tag must match the cluster's OCP minor version;
# scripts/install.sh detects it from the logged-in cluster unless set.
OAUTH_PROXY_IMAGE ?=
# Rollouts wait for running cluster operations (up to ~17 minutes).
ROLLOUT_TIMEOUT ?= 20m
# Where the app links release notes; empty keeps the template's default.
RELEASES_URL ?=

.PHONY: all build push deploy upgrade rollback resolve-image release undeploy dev lint lint-go lint-frontend \
	test test-go test-frontend test-scripts vuln clean help env docs-fixtures docs-mock docs-screenshots docs-check

.DEFAULT_GOAL := help

all: build push  ## Build, push and deploy this checkout's build (:<commit>)
	@$(MAKE) --no-print-directory deploy TAG=$(GIT_SHA)

# :latest, :main, :vN and :vX.Y.Z are published by CI only (release tags are
# immutable, :latest follows releases). A local build is tagged with its
# commit and, optionally, a TAG of your own.
PUBLISHED_TAG = $(shell echo '$(TAG)' | grep -Ex 'latest|main|v[0-9]+|v[0-9]+\.[0-9]+\.[0-9]+')
EXTRA_TAG = $(if $(PUBLISHED_TAG),,$(filter-out $(GIT_SHA),$(TAG)))

build:  ## Build the image as :<commit> (and :TAG for your own test tag; NO_CACHE=1 skips the cache)
	@command -v $(RUNTIME) >/dev/null 2>&1 || { echo "Error: $(RUNTIME) is not installed."; exit 1; }
	$(RUNTIME) build --platform $(PLATFORM) $(if $(NO_CACHE),--no-cache,) \
		--build-arg GIT_SHA=$(GIT_FULL_SHA) --build-arg VERSION=$(BUILD_VERSION) --build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(GIT_SHA) -f Containerfile .
	$(if $(EXTRA_TAG),$(RUNTIME) tag $(IMAGE):$(GIT_SHA) $(IMAGE):$(EXTRA_TAG),@true)

push:  ## Push :<commit> (and :TAG for your own test tag); CI publishes releases, :main and :latest
	@$(if $(and $(PUBLISHED_TAG),$(if $(TAG_DEFAULTED),,1)),echo "Not pushing :$(TAG): CI publishes :latest, :main and release tags. Pushing only :$(GIT_SHA).",true)
	$(RUNTIME) push $(IMAGE):$(GIT_SHA)
	$(if $(EXTRA_TAG),$(RUNTIME) push $(IMAGE):$(EXTRA_TAG),@true)

# deploy, upgrade, rollback and resolve-image are scripts/install.sh, the
# same script every release attaches as install.sh, so the clone path and
# the no-clone path cannot drift. It resolves IMAGE:TAG to IMAGE@sha256:...
# and checks that the image was built from a commit whose
# deploy/template.yaml matches this checkout (uncommitted edits included):
# an image only works with the template of its own commit (ports, probes,
# environment, RBAC). Overrides: ALLOW_TEMPLATE_MISMATCH=1 (skip the
# revision and template checks), ALLOW_MUTABLE_TAG=1 (deploy the tag when
# no digest can be resolved). `make rollback` applies an older image's own
# template. DRY_RUN=1 only validates with the API server.
INSTALL = IMAGE='$(IMAGE)' NAMESPACE='$(NAMESPACE)' APP_NAME='$(APP_NAME)' PLATFORM='$(PLATFORM)' \
	OAUTH_PROXY_IMAGE='$(OAUTH_PROXY_IMAGE)' ROLLOUT_TIMEOUT='$(ROLLOUT_TIMEOUT)' RELEASES_URL='$(RELEASES_URL)' \
	DRY_RUN='$(DRY_RUN)' ALLOW_TEMPLATE_MISMATCH='$(ALLOW_TEMPLATE_MISMATCH)' ALLOW_MUTABLE_TAG='$(ALLOW_MUTABLE_TAG)' \
	./scripts/install.sh

deploy:  ## First install of this release on OpenShift (cluster-admin, oc login; DRY_RUN=1 validates only)
	@TAG='$(TAG)' $(INSTALL) deploy

upgrade:  ## Upgrade to this checkout's release (or TAG=main / TAG=<commit>) by digest (DRY_RUN=1 validates only)
	@TAG='$(TAG)' $(INSTALL) upgrade

# Rolls back (or pins) to the build of an older release or commit with that
# commit's own deploy/template.yaml, so ports, probes and RBAC match its
# code. Needs an explicit TAG. Roll forward with `make upgrade`.
rollback:  ## Roll back to an older build with its own template: TAG=<release or commit> (DRY_RUN=1 validates only)
	@TAG='$(if $(TAG_DEFAULTED),,$(TAG))' $(INSTALL) rollback

resolve-image:  ## Print the digest reference deploy/upgrade would apply for IMAGE:TAG, after the template check
	@TAG='$(TAG)' $(INSTALL) resolve-image

release:  ## Tag a release: VERSION=vX.Y.Z (checks main, CHANGELOG and the template guard; prints the push commands)
	@test -n "$(VERSION)" || { echo "Usage: make release VERSION=vX.Y.Z"; exit 1; }
	@./scripts/release.sh tag '$(VERSION)'

undeploy:  ## Remove the app, its cluster-wide RBAC, Roles in other namespaces, the ConsoleLink and the namespace
	@$(INSTALL) uninstall

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

test: test-go test-frontend test-scripts  ## Run all tests

test-go:  ## Run Go tests with the race detector
	go test -race -count=1 ./...

test-frontend:  ## Run frontend tests
	cd frontend && npm test

test-scripts:  ## Test the release and install scripts (offline)
	sh scripts/test-release.sh
	bash scripts/test-install.sh

vuln:  ## Check Go and npm dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	cd frontend && npm audit --omit=dev --audit-level=high

docs-fixtures:  ## Regenerate the docs mock's Diagnostics answers from the backend's test fakes
	DOCS_FIXTURES_DIR="$(CURDIR)/docs/tools/mock/diagnostics" go test ./pkg/cluster/ -run '^TestWriteDocsFixtures$$' -count=1

docs-mock:  ## Serve the built frontend with the docs mock backend (SCENARIO=healthy, PORT=18181)
	node docs/tools/mock/server.mjs --port $(or $(PORT),18181) --scenario $(or $(SCENARIO),healthy)

docs-screenshots:  ## Regenerate every docs image and GIF from the mock (JOBS="..." for a subset; needs playwright-cli, ffmpeg, ImageMagick)
	./docs/tools/screenshots.sh $(JOBS)

docs-check:  ## Check the Markdown links, anchors and image paths, and that every docs image is used
	node docs/tools/check-docs.mjs

clean:  ## Remove build artifacts
	rm -f server
	rm -rf frontend/dist
	rm -rf frontend/node_modules

env:  ## Create .env file from example for local config
	@if [ -f .env ]; then echo ".env already exists"; else \
	echo "# Nightly Updater — local config (not committed to git)" > .env; \
	echo "# IMAGE=quay.io/your-org/rhoai-nightly-updater" >> .env; \
	echo "# TAG=v2.0.0" >> .env; \
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
	@echo "  TAG        = $(or $(TAG),(none: HEAD is not on a release tag))"
	@echo "  GIT_SHA    = $(GIT_SHA)"
	@echo "  NAMESPACE  = $(NAMESPACE)"
	@echo "  APP_NAME   = $(APP_NAME)"
	@echo "  RUNTIME    = $(RUNTIME)"
	@echo "  PLATFORM   = $(PLATFORM)"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Versions: TAG defaults to the release tag HEAD is on (git checkout vX.Y.Z)."
	@echo "Examples:"
	@echo "  git checkout v2.0.0 && make upgrade     Upgrade to a release, with its own template"
	@echo "  make upgrade DRY_RUN=1                  Only validate the upgrade with the API server"
	@echo "  make upgrade TAG=main                   Test the newest main build (template must match)"
	@echo "  make rollback TAG=v1.0.0                Roll back to a release with that release's own template"
	@echo "  make rollback TAG=4503bb7d DRY_RUN=1    Only validate a rollback to a commit build"
	@echo "  make release VERSION=v2.1.0             Tag a release on main (prints the push commands)"
	@echo "  make build IMAGE=quay.io/myorg/myapp    Build with custom registry"
	@echo "  make deploy NAMESPACE=my-ns             Deploy to custom namespace"
	@echo "  make env                                Create .env file for persistent config"
	@echo "  make test                               Run all tests"
