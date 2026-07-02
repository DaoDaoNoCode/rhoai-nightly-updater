# Load .env file if it exists (for persistent config overrides)
-include .env

# Configurable variables — override via environment, command line, or .env file
IMAGE     ?= quay.io/juntao_wang/rhoai-nightly-updater
TAG       ?= latest
GIT_SHA   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
NAMESPACE ?= rhoai-nightly-updater
APP_NAME  ?= rhoai-nightly-updater
RUNTIME   ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null || echo "podman")
PLATFORM  ?= linux/amd64

.PHONY: all build push deploy undeploy dev lint lint-go lint-frontend test test-go test-frontend clean help env

.DEFAULT_GOAL := help

all: build push deploy  ## Build, push, and deploy in one shot

build:  ## Build container image (auto-detects podman or docker, use NO_CACHE=1 to skip cache)
	@command -v $(RUNTIME) >/dev/null 2>&1 || { echo "Error: $(RUNTIME) is not installed."; exit 1; }
	$(RUNTIME) build --platform $(PLATFORM) $(if $(NO_CACHE),--no-cache,) -t $(IMAGE):$(TAG) -f Containerfile .
	$(RUNTIME) tag $(IMAGE):$(TAG) $(IMAGE):$(GIT_SHA)

push:  ## Push image to registry (latest + git SHA tags)
	$(RUNTIME) push $(IMAGE):$(TAG)
	$(RUNTIME) push $(IMAGE):$(GIT_SHA)

deploy:  ## Deploy to OpenShift (requires oc login)
	oc new-project $(NAMESPACE) 2>/dev/null || true
	oc process -f deploy/template.yaml \
		-p IMAGE=$(IMAGE):$(TAG) \
		-p NAMESPACE=$(NAMESPACE) \
		-p APP_NAME=$(APP_NAME) | oc apply -f -
	@echo ""
	@echo "Restarting deployment to pull latest image..."
	oc rollout restart deployment/$(APP_NAME) -n $(NAMESPACE)
	oc rollout status deployment/$(APP_NAME) -n $(NAMESPACE) --timeout=120s
	@echo ""
	@ROUTE_URL=$$(oc get route $(APP_NAME) -n $(NAMESPACE) -o jsonpath='https://{.spec.host}'); \
	echo "App URL: $$ROUTE_URL"; \
	echo "Patching ConsoleLink with route URL..."; \
	oc patch consolelink $(APP_NAME) --type merge \
		-p "{\"spec\":{\"href\":\"$$ROUTE_URL\"}}" 2>/dev/null || true

undeploy:  ## Remove the app from OpenShift
	oc delete consolelink $(APP_NAME) 2>/dev/null || true
	oc delete project $(NAMESPACE)

dev:  ## Start local dev server (Go backend + React frontend)
	./dev.sh

lint: lint-go lint-frontend  ## Run all linters

lint-go:  ## Run Go linter (golangci-lint)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

lint-frontend:  ## Run frontend linter
	cd frontend && npm run lint 2>/dev/null || echo "No lint script configured in frontend/package.json yet"

test: test-go test-frontend  ## Run all tests

test-go:  ## Run Go tests
	go test ./... -v -count=1

test-frontend:  ## Run frontend tests
	cd frontend && npm test 2>/dev/null || echo "No test script configured in frontend/package.json yet"

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
	@echo "  make build IMAGE=quay.io/myorg/myapp    Build with custom registry"
	@echo "  make build RUNTIME=docker               Build with docker instead of podman"
	@echo "  make deploy NAMESPACE=my-ns             Deploy to custom namespace"
	@echo "  make env                                Create .env file for persistent config"
	@echo "  make test                               Run all tests"
