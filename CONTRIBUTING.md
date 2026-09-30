# Contributing to RHOAI Nightly Updater

This guide covers how to develop, test, and deploy changes to the RHOAI Nightly Updater.

## Prerequisites

### Required Versions

- **Go**: See `go.mod` for the required version
- **Node.js**: 22.x
- **TypeScript**: 5.x

### Required Tools

- **Container Runtime**: Podman or Docker (auto-detected by `make build`)
- **OpenShift CLI**: `oc` (required for deployment and local dev)
- **golangci-lint**: For running Go linters (`go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`)

### Cluster Access

- You must be logged into an OpenShift cluster with `oc login` before running local dev or deploying.
- The local dev server (`dev.sh`) uses your current `oc` token to authenticate with the cluster.

## Local Development

### Quick Start

```bash
./dev.sh
```

This starts two servers:

1. **Go backend** on `:8080` — connects to your current OpenShift cluster using your `oc` token
2. **Webpack dev server** on `:9000` — proxies `/api` requests to the backend on `:8080`

Open **http://localhost:9000** in your browser.

### What `dev.sh` Does

1. Verifies Node.js is installed
2. Verifies you're logged into an OpenShift cluster (`oc whoami -t`)
3. Installs frontend dependencies (`npm install`) if `node_modules` is missing
4. Starts the Go backend with these environment variables:
   - `KUBERNETES_SERVICE_HOST` — cluster API host (extracted from `oc whoami --show-server`)
   - `KUBERNETES_SERVICE_PORT` — cluster API port
   - `DEV_TOKEN` — your current `oc` token
   - `DEV_USER` — your OpenShift username
   - `DEV_MODE=true` — enables dev mode (uses insecure TLS, skips OAuth proxy)
5. Starts the frontend dev server (`npm run dev`) which runs webpack-dev-server with:
   - Hot reload enabled
   - Proxy: `/api` → `http://localhost:8080`

### Frontend Development

The frontend uses:

- **React 18.3.0** with TypeScript 5.6.x
- **PatternFly 6.x** for UI components (`@patternfly/react-core`, `@patternfly/react-table`)
- **React Router 7.16.0** for client-side routing
- **Webpack 5** for bundling (configured in `frontend/webpack.config.js`)

#### TypeScript Configuration

- **Target**: ES2020
- **Module**: ESNext with `bundler` resolution
- **JSX**: `react-jsx` (automatic runtime, no manual React imports needed)
- **Source maps**: Enabled for debugging

#### Build Commands

```bash
cd frontend
npm run build       # Production build → frontend/dist/
npm run dev         # Dev server on :9000 with hot reload
```

#### Dev Server Proxy

The webpack dev server proxies `/api` requests to the Go backend:

```javascript
// frontend/webpack.config.js
devServer: {
  proxy: [{ context: ['/api'], target: 'http://localhost:8080' }],
}
```

## Project Structure

```
/Users/juntaowang/Desktop/ODH/rhoai-nightly-updater/
├── main.go                  # HTTP server entry point
├── go.mod                   # Go module definition
├── Makefile                 # Build/test/deploy automation
├── Containerfile            # Multi-stage Docker build
├── dev.sh                   # Local development launcher
├── .env.example             # Environment variable reference
├── pkg/                     # Go backend packages
│   ├── api/                 # HTTP handlers and auth
│   ├── cluster/             # Kubernetes API client and operations
│   ├── middleware/          # HTTP middleware (security, request ID)
│   └── types/               # Shared type definitions
├── frontend/                # React/TypeScript frontend
│   ├── src/                 # Source files
│   │   ├── index.tsx        # React app entry point
│   │   ├── App.tsx          # Root component with routing
│   │   ├── pages/           # Page components
│   │   ├── components/      # Reusable UI components
│   │   └── services/        # API client
│   ├── public/              # Static assets
│   ├── dist/                # Production build output (generated)
│   ├── package.json         # Node dependencies and scripts
│   ├── tsconfig.json        # TypeScript compiler config
│   └── webpack.config.js    # Webpack bundler config
├── deploy/                  # Kubernetes manifests
├── scripts/                 # Automation scripts
└── docs/                    # Documentation
```

## Running Tests

### All Tests

```bash
make test              # Run both Go and frontend tests
```

### Go Tests

```bash
make test-go           # Runs: go test ./... -v -count=1
```

**Test patterns:**

- Tests use fast timing constants instead of production timeouts
- `TestMain` overrides package-level timing constants to run quickly without changing production behavior

### Frontend Tests

```bash
make test-frontend     # Runs: cd frontend && npm test
```

Note: Currently prints `"No test script configured in frontend/package.json yet"` — frontend tests are not yet implemented.

## Linting

### All Linters

```bash
make lint              # Run both Go and frontend linters
```

### Go Linter

```bash
make lint-go           # Runs: golangci-lint run ./...
```

Requires `golangci-lint` to be installed:
```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

### Frontend Linter

```bash
make lint-frontend     # Runs: cd frontend && npm run lint
```

Note: Currently prints `"No lint script configured in frontend/package.json yet"` — frontend linting is not yet configured.

## Building

### Local Build (Containerfile)

```bash
make build             # Build using podman or docker (auto-detected)
```

**What happens**:

1. Auto-detects container runtime (`podman` or `docker`)
2. Builds a multi-stage image from `Containerfile`:
   - **Stage 1 (frontend)**: `node:22-alpine`
     - Installs dependencies: `npm ci`
     - Builds production bundle: `npm run build` → `frontend/dist/`
     - Uses `NODE_OPTIONS="--max-old-space-size=2048"` to handle large bundles
   - **Stage 2 (backend)**: `golang:1.24-alpine`
     - Downloads Go modules: `go mod download`
     - Runs tests: `go test ./...`
     - Builds static binary: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server .`
   - **Stage 3 (runtime)**: `registry.access.redhat.com/ubi9/ubi-minimal:9.6`
     - Copies compiled Go server from backend stage
     - Copies frontend static files from frontend stage
     - Sets `STATIC_DIR=/app/static`, `PORT=8080`
     - Runs as non-root user `1001`
3. Tags image as `IMAGE:TAG` and `IMAGE:GIT_SHA`

**Build variables** (override via environment, command line, or `.env` file):

- `IMAGE` — Registry and image name (default: `quay.io/juntao_wang/rhoai-nightly-updater`)
- `TAG` — Image tag (default: `latest`)
- `GIT_SHA` — Git commit SHA (default: auto-detected from `git rev-parse --short HEAD`)
- `RUNTIME` — Container runtime (default: auto-detected `podman` or `docker`)
- `PLATFORM` — Target platform (default: `linux/amd64`)
- `NO_CACHE` — Set to `1` to skip cache and force rebuild

**Examples**:

```bash
make build IMAGE=quay.io/myorg/myapp        # Custom registry
make build RUNTIME=docker                   # Force docker instead of podman
make build NO_CACHE=1                       # Force full rebuild
```

### Push to Registry

```bash
make push              # Push IMAGE:TAG and IMAGE:GIT_SHA to registry
```

Requires prior `docker login` or `podman login` to the registry.

## Code Patterns

### Backend Patterns

#### 1. HTTP Handlers with Authentication

All API handlers use one of two wrapper functions:

**`withAuth`** — For read-only endpoints (GET requests):

```go
// From pkg/api/handlers.go
var HandleStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
    // Handler receives an authenticated cluster.Client
    // Use c.get(), c.post(), etc. to call Kubernetes APIs
})
```

**`withMutationAuth`** — For mutation endpoints (POST/PUT/DELETE):

```go
var HandleUpdate = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
    // Adds per-user rate limiting (30s window)
    // Prevents concurrent cluster mutations via atomic.Bool lock
})
```

**What the wrappers do**:

1. Extract user token from `Authorization` header (set by oauth-proxy)
2. Extract cluster token from service account or `DEV_TOKEN` env var
3. Create a `cluster.Client` with the user's token
4. Call the handler function with the authenticated client
5. (`withMutationAuth` only) Check full user RBAC (`subscriptions:update` in `redhat-ods-operator`) and apply rate limiting. Operator lifecycle handlers also hold the cluster mutation lock. Accepted mutations use a browser-independent context with a 15-minute deadline.

#### 2. Kubernetes API Client

All Kubernetes API calls use a **raw HTTP client** (`pkg/cluster/client.go`) instead of the official Kubernetes client library.

**Why**: Avoid dependency bloat, faster builds, simpler debugging.

**Pattern**:

```go
// From pkg/cluster/client.go
func (c *Client) get(path string) ([]byte, int, error) {
    req, _ := http.NewRequestWithContext(c.ctx, "GET", c.baseURL+path, nil)
    req.Header.Set("Authorization", "Bearer "+c.token)
    req.Header.Set("Accept", "application/json")
    resp, err := c.httpClient.Do(req)
    // ... handle response, parse K8s errors
}
```

**Path builders**:

- `namespacedPath(apiGroup, resource, namespace, name)` — for namespaced resources (Pods, Deployments, Subscriptions)
- `clusterPath(apiGroup, resource, name)` — for cluster-scoped resources (ClusterVersions, Nodes)

**Example**:

```go
path := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", "ns", "sub-name")
// Returns: /apis/operators.coreos.com/v1alpha1/namespaces/ns/subscriptions/sub-name
```

**TLS Configuration**:

- Production: Uses in-cluster CA cert from `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`
- Dev mode (`DEV_MODE=true`): Uses `InsecureSkipVerify` for local testing
- Shared `http.Transport` across all clients (CA loaded once at init)

#### 3. Test Patterns

Tests use mock clients by overriding timing constants in `TestMain`. This allows tests to verify polling/retry logic without waiting for production timeouts.

### Frontend Patterns

#### 1. PatternFly Component Structure

All components use **PatternFly 6.x** components:

```tsx
// From frontend/src/App.tsx
import {
  Page,
  PageSection,
  Alert,
  Button,
  Spinner,
} from "@patternfly/react-core";
```

**Key patterns**:

- Use PatternFly's layout components (`Page`, `PageSection`, `Grid`, `Flex`)
- Use PatternFly's form components (`Form`, `FormGroup`, `TextInput`, `Select`)
- Use PatternFly's data display (`Table`, `Card`, `Label`, `EmptyState`)
- Import icons from `@patternfly/react-icons`

#### 2. State Management

Components use React hooks for state:

```tsx
const [status, setStatus] = useState<StatusResponse | null>(null);
const [loading, setLoading] = useState(true);
const [error, setError] = useState<string | null>(null);

useEffect(() => {
  // Fetch status on mount
  fetchStatus();
}, []);
```

#### 3. API Calls

API calls are centralized in `frontend/src/services/api.ts` (not shown, but inferred from imports in `App.tsx`):

```tsx
import { getStatus, getUserPermissions, trackPageView } from "./services/api";
```

## Deploying Changes

### Full Workflow (Build, Push, Deploy)

```bash
make all               # Equivalent to: make build push deploy
```

### Step-by-Step Deployment

#### 1. Build the Image

```bash
make build
```

#### 2. Push to Registry

```bash
make push              # Pushes IMAGE:TAG and IMAGE:GIT_SHA
```

#### 3. Deploy to OpenShift

```bash
make deploy            # Requires 'oc login' first
```

**What `make deploy` does**:

1. Creates namespace if it doesn't exist: `oc new-project NAMESPACE`
2. Processes the template with parameters:
   - `IMAGE=$(IMAGE):$(TAG)`
   - `NAMESPACE=$(NAMESPACE)`
   - `APP_NAME=$(APP_NAME)`
3. Applies the processed template: `oc apply -f -`
4. Restarts the deployment to pull the latest image: `oc rollout restart deployment/APP_NAME`
5. Waits for rollout to complete (120s timeout)
6. Patches the ConsoleLink with the actual route URL

**Deployment variables** (override via environment, command line, or `.env` file):

- `NAMESPACE` — Target namespace (default: `rhoai-nightly-updater`)
- `APP_NAME` — Application name (default: `rhoai-nightly-updater`)

#### 4. Verify Deployment

```bash
./scripts/smoke-test.sh            # Uses default namespace
./scripts/smoke-test.sh my-ns      # Test custom namespace
```

**What the smoke test checks**:

1. **Pod Status**: Pods are running and ready with correct image
2. **Route Configuration**: Route has 960s timeout annotation and valid URL
3. **ClusterRole Permissions**: Required permissions exist (deployments:patch, namespaces:create, secrets:get, mlflows:create, datasciencepipelinesapplications:create)
4. **Health Probes**: Startup, liveness, and readiness probes are configured correctly (`/api/health`, `/api/health/ready`)
5. **Observability**: ServiceMonitor and PrometheusRule exist
6. **ConsoleLink**: ConsoleLink exists and has valid URL (not placeholder)
7. **OAuth Proxy**: `pass-access-token=true` and `openshift-sar` are configured
8. **NetworkPolicy**: NetworkPolicy exists

Exit codes:
- `0` — All checks passed
- `1` — One or more checks failed

### Undeploy

```bash
make undeploy          # Deletes ConsoleLink and namespace
```

### Custom Configuration

Create a `.env` file to persist custom configuration:

```bash
make env               # Create .env from defaults
```

Edit `.env` with your values:

```bash
IMAGE=quay.io/myorg/rhoai-nightly-updater
TAG=dev
NAMESPACE=my-rhoai-updater
RUNTIME=docker
```

Then `make` commands will use your custom values.

## Environment Variables

See `.env.example` for all available environment variables:

### Build & Deploy

- `IMAGE` — Registry and image name (default: `quay.io/juntao_wang/rhoai-nightly-updater`)
- `TAG` — Image tag (default: `latest`)
- `GIT_SHA` — Git commit SHA (default: auto-detected)
- `NAMESPACE` — Deployment namespace (default: `rhoai-nightly-updater`)
- `APP_NAME` — Application name (default: `rhoai-nightly-updater`)
- `RUNTIME` — Container runtime (default: auto-detected `podman` or `docker`)
- `PLATFORM` — Target platform (default: `linux/amd64`)

### Operator Defaults

- `STABLE_SOURCE` — Catalog source name (default: `redhat-operators`)
- `STABLE_CHANNEL` — Optional GA channel pin; by default discover the highest GA version available in the configured catalog's `stable`, `fast`, and `eus` channels. The pin must exist and have a GA head. No fixed version fallback is used.
- `DSC_SAMPLE_REF` — Optional Git branch/tag for DSC samples; otherwise derived from the installed operator version, supporting numbered and unnumbered prereleases

Set these variables in the backend process environment. For a deployed app, use `oc set env deployment/rhoai-nightly-updater -n rhoai-nightly-updater DSC_SAMPLE_REF=<branch-or-tag>` (adjust the deployment and namespace if customized). Remove the override with `DSC_SAMPLE_REF-` to return to automatic version matching.

### Server

- `PORT` — HTTP server port (default: `8080`)
- `STATIC_DIR` — Frontend static files directory (default: `./frontend/dist`)
- `LOG_LEVEL` — Logging level: `debug`, `info`, `warn`, `error` (default: `info`)

### MinIO Credentials

- `MINIO_ROOT_USER` — MinIO admin username (default: `minio`)
- `MINIO_ROOT_PASSWORD` — MinIO admin password (default: random)

### Development Only (never set in production)

- `DEV_MODE` — Enable dev mode (insecure TLS, skip OAuth proxy)
- `DEV_TOKEN` — Your `oc` token (auto-set by `dev.sh`)
- `DEV_USER` — Your OpenShift username (auto-set by `dev.sh`)

## Getting Help

Run `make help` to see all available targets with descriptions:

```bash
make help              # Show all targets and current configuration
```

## Notes

- All `Makefile` variables can be overridden via environment, command line, or `.env` file.
- The `.env` file is ignored by git (listed in `.gitignore`).
- Use `make env` to create a `.env` file from defaults.
- The Go backend runs tests during the container build (`Containerfile` line 14: `RUN go test ./...`) — the build fails if tests fail.
