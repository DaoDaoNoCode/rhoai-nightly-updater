# Contributing to RHOAI Nightly Updater

This guide covers how to develop, test, and deploy changes to the RHOAI Nightly Updater.

## Prerequisites

### Required Versions

- **Go**: 1.27 (`go.mod`; CI and the Containerfile use golang 1.27.1)
- **Node.js**: 22.x, ≥ 22.13 for the test tooling (CI uses `node:22-alpine`)
- **TypeScript**: 5.x

### Required Tools

- **Container Runtime**: Podman or Docker (auto-detected by `make build`)
- **OpenShift CLI**: `oc` (required for deployment and local dev)
- **golangci-lint v2**: install a release binary (https://golangci-lint.run/docs/welcome/install/local/); CI pins v2.14.0

### Cluster Access

- You must be logged into an OpenShift cluster with `oc login` before running local dev or deploying.
- The local dev server (`dev.sh`) uses your current `oc` token to authenticate with the cluster.

## Local Development

### Quick Start

```bash
./dev.sh
```

This starts two servers:

1. **Go backend** on `127.0.0.1:8080` — connects to your current OpenShift cluster using your `oc` token
2. **Webpack dev server** on `127.0.0.1:9000` — proxies `/api` requests to the backend on `:8080`

Open **http://127.0.0.1:9000** in your browser.

### What `dev.sh` Does

1. Verifies Node.js is installed
2. Verifies you're logged into an OpenShift cluster (`oc whoami -t`)
3. Installs frontend dependencies (`npm install`) if `node_modules` is missing
4. Starts the Go backend with these environment variables:
   - `KUBERNETES_SERVICE_HOST` — cluster API host (extracted from `oc whoami --show-server`)
   - `KUBERNETES_SERVICE_PORT` — cluster API port
   - `DEV_TOKEN` — your current `oc` token
   - `DEV_USER` — your OpenShift username
   - `DEV_MODE=true` — enables dev mode (no oauth-proxy and no cluster-admin gate: every request uses your token; listens on `127.0.0.1` only; Host/Origin must be this machine). API server TLS is still verified (see [Environment Variables](#environment-variables))
5. Starts the frontend dev server (`npm run dev`) which runs webpack-dev-server with:
   - Hot reload enabled
   - Proxy: `/api` → `http://127.0.0.1:8080` (override with `API_TARGET`)

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
  host: '127.0.0.1',
  proxy: [{ context: ['/api'], target: 'http://127.0.0.1:8080' }],
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
make test-go           # Runs: go test -race -count=1 ./...
```

**Test patterns:**

- Tests use fast timing constants instead of production timeouts
- `TestMain` overrides package-level timing constants to run quickly without changing production behavior

### Frontend Tests

```bash
make test-frontend     # Runs: cd frontend && npm test
```

Runs the Node test runner over `frontend/tests/*.test.cjs`, then Vitest (`npm run test:unit` runs Vitest only). No browser needed.

## Linting

### All Linters

```bash
make lint              # Run both Go and frontend linters
```

### Go Linter

```bash
make lint-go           # Runs: gofmt -l (must be empty), go vet ./..., golangci-lint run ./...
```

Requires golangci-lint v2 (configuration in `.golangci.yml`), installed from the release binaries: https://golangci-lint.run/docs/welcome/install/local/. `make vuln` runs govulncheck and `npm audit --omit=dev --audit-level=high`.

### Frontend Linter

```bash
make lint-frontend     # Runs: cd frontend && npm run typecheck && npm run lint (ESLint)
```

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
     - Type-checks, lints and tests: `npm run typecheck && npm run lint && npm test`
     - Builds production bundle: `npm run build` → `frontend/dist/`
     - Uses `NODE_OPTIONS="--max-old-space-size=2048"` to handle large bundles
   - **Stage 2 (backend)**: `golang:1.27.1-alpine`
     - Downloads Go modules: `go mod download`
     - Vets and tests: `go vet ./... && go test ./...`
     - Builds a static binary with the version, commit and build date linked in
   - **Stage 3 (runtime)**: `registry.access.redhat.com/ubi9/ubi-minimal` 9.8 (all base images pinned by digest)
     - Sets the `org.opencontainers.image.revision` label to the commit (`make deploy`/`upgrade` check it)
     - Copies compiled Go server from backend stage
     - Copies frontend static files from frontend stage
     - Sets `STATIC_DIR=/app/static`, `PORT=8080`
     - Runs as non-root user `1001`
3. Tags the image as `IMAGE:GIT_SHA`, plus `IMAGE:TAG` when `TAG` is a test tag of your own (never `latest`, `main`, `vN` or `vX.Y.Z`: CI publishes those)

**Build variables** (override via environment, command line, or `.env` file):

- `IMAGE` — Registry and image name (default: `quay.io/juntao_wang/rhoai-nightly-updater`)
- `TAG` — Image tag (default: the release tag HEAD is on, else none). `deploy`/`upgrade` install this tag
- `BUILD_VERSION` — The version the binary reports (default: the release tag HEAD is on, else `GIT_SHA`)
- `GIT_SHA` — 8-character commit tag (default: the first 8 characters of the HEAD commit, the same as GitLab's `CI_COMMIT_SHORT_SHA`)
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
make push              # Push IMAGE:GIT_SHA (and IMAGE:TAG for a test tag of your own)
```

Requires prior `docker login` or `podman login` to the registry. To test your build on a cluster: `make build push`, then `make upgrade TAG=<GIT_SHA>` (or `make all`, which deploys `:<GIT_SHA>`). For Docker, set `RUNTIME=docker`.

### Automatic Image Publishing

GitLab CI (`.gitlab-ci.yml`) runs on merge requests, branch pushes and release tags (`vX.Y.Z`; other tags run nothing):

- **test** stage, all must pass, no lint exceptions:
  - `go`: gofmt, `go vet`, `go test -race`, govulncheck;
  - `golangci-lint`;
  - `frontend`: `npm ci`, typecheck, ESLint, tests, `npm audit`, production build;
  - `release-scripts`: `scripts/test-release.sh` and `scripts/test-install.sh`;
  - `release-guard` (tags only): `scripts/release.sh check`, see [Releases](#releases).
- **main**: `build-main` builds the image to a tarball; `publish-main` pushes it as the write-once `:$CI_COMMIT_SHORT_SHA` (8 characters; an existing tag is kept when it is a build of the same commit, and refused from another commit) and moves `:main` only while the commit is still the tip of `main`. It never touches `:latest`.
- **Other branches**: a manual job publishes `:$CI_COMMIT_REF_SLUG`.
- **Release tags**: `release-build` builds the image (`VERSION=<tag>`) to a tarball; `release-installer` generates `install.sh`; `release-publish` pushes `:vX.Y.Z` and moves `:vN` and `:latest` (every tag-writing job, main or release, holds the one resource group `publish-latest`); `release-notes` creates the GitLab Release. `promote-latest` is manual.
- The registry comes from the `IMAGE` variable (a project CI/CD variable overrides the default) with `QUAY_USER`/`QUAY_TOKEN` as its credentials.

GitHub Actions (`.github/workflows/ci.yml`) runs the same test-stage checks on pushes to `main` and on pull requests; `release.yml` runs them on release tags and creates the GitHub Release. Nothing is published to a registry from GitHub.

Teammates install releases, so a broken `main` only reaches people who test `:main` on purpose.

## Releases

Version numbers (`vMAJOR.MINOR.PATCH`):

- **MAJOR**: the deploy contract changed: `TEMPLATE_REVISION` in `deploy/template.yaml` (with `api.ExpectedTemplateRevision`), RBAC, or anything else that needs the template re-applied. Upgrading needs a full redeploy.
- **MINOR**: features with the same template. **PATCH**: fixes.

Image tags:

| Tag | Moves | Set by |
|---|---|---|
| `:vX.Y.Z` | never (refused if it exists from another commit) | the tag pipeline |
| `:vN` | to the newest release of major N | the tag pipeline |
| `:latest` | to a newer release of the **same** major only; an unlabelled `:latest` (from before releases) counts as v1 | the tag pipeline; across majors only the manual `promote-latest` job |
| `:main` | to every new tip of `main` | `publish-main` |
| `:<8-char commit>` | never: written once, by whichever pipeline publishes the commit first | `publish-main` or the tag pipeline |

How to cut a release:

1. On `main`, change the top `CHANGELOG.md` section from `## [X.Y.Z] - unreleased` to `## [X.Y.Z] - YYYY-MM-DD` (today) in a commit; merge it.
2. With `main` checked out, clean and up to date:
   ```bash
   make release VERSION=vX.Y.Z
   ```
   It checks the branch, the tree, `origin/main`, the CHANGELOG section and the template guard, creates an annotated tag and prints the push commands. Nothing is pushed.
3. Push the tag to both remotes, as printed: `git push origin vX.Y.Z` (GitLab: images, `install.sh`, GitLab Release) and `git push github vX.Y.Z` (GitHub Release).
4. Watch the tag pipeline. For a new MAJOR, `:latest` stays on the old major; when teammates have been told, run the manual `promote-latest` job of that pipeline.
5. Start the next section: `## [X.Y.Z+1] - unreleased` (or the next planned version).

The guard (`scripts/release.sh check`, the same in `make release` and CI) fails when the tag is not the top dated CHANGELOG section, or when `TEMPLATE_REVISION` differs from the previous release tag's (the highest lower `v*` tag) and the MAJOR is not higher. A MAJOR bump without a template change is allowed. Retrying a release pipeline is safe: an image already published from the same commit is kept; one from another commit stops the job.

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

1. Take the user token from `X-Forwarded-Access-Token` (set by oauth-proxy). A `Bearer` header is refused outside `DEV_MODE`
2. Resolve the user from that token (`users/~`); `X-Forwarded-User` is not trusted
3. Create a `cluster.Client` with the ServiceAccount token (or `DEV_TOKEN` in dev mode) and call the handler
4. (`withMutationAuth` only) Run a cluster-admin SubjectAccessReview with the user's token, take the per-user/endpoint 30 s rate-limit slot, refuse new work while shutting down, and give the handler a browser-independent context with a 15-minute deadline. Cluster-changing handlers also take the cluster lock (`lockCluster`, 409 `cluster_busy`). `TestEveryNonGetRouteIsGated` fails for a non-GET route without this wrapper. Routes are listed in `pkg/api/routes.go`

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

- `KUBE_CA_FILE` if set, else the in-cluster CA (`/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`), else the system trust store
- Verification is skipped only with `DEV_MODE=true` and `DEV_INSECURE_TLS=true`
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
make push              # Pushes IMAGE:GIT_SHA (and a test TAG of your own)
```

#### 3. Deploy to OpenShift

```bash
make deploy TAG=<GIT_SHA>   # first install; make upgrade afterwards. Requires cluster-admin and 'oc login'
```

Both run `scripts/install.sh` (the script every release attaches as `install.sh`): they resolve `IMAGE:TAG` to a digest, check that the image was built from a commit with this checkout's `deploy/template.yaml`, create the cookie Secret once, apply the template, wait for the rollout, then add the ConsoleLink, remove legacy objects and prune old ReplicaSets. `DRY_RUN=1` only validates. Overrides, rollback and the first-time `ALLOW_TEMPLATE_MISMATCH=1` are described in [RUNBOOK §2](RUNBOOK.md#2-upgrade-roll-back-or-remove-the-updater). To deploy an image you pushed yourself, push it first (`make build push`), then run `make upgrade TAG=<GIT_SHA>`. If you changed the template, commit it first.

If you change `deploy/template.yaml` in a way the running code depends on, bump `TEMPLATE_REVISION` there and `api.ExpectedTemplateRevision` together; the UI then tells admins to upgrade.

#### 4. Verify Deployment

```bash
./scripts/smoke-test.sh [namespace] [app-name]
```

Read-only. Expected values (route and proxy timeouts, strategy, grace period, template revision) are read from `deploy/template.yaml`. It checks the deployment, pods and `/api/version`, the Route/oauth-proxy settings, the RBAC objects (and that no legacy ClusterRole remains), the Service/NetworkPolicy ports, monitoring (warns when user workload monitoring is off) and the ConsoleLink. It exits 1 on any failure.

### Undeploy

```bash
make undeploy          # install.sh uninstall: the app namespace, its ClusterRole/Binding, Roles in other namespaces, ConsoleLink
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

Make variables (`IMAGE`, `TAG`, `NAMESPACE`, `APP_NAME`, `RUNTIME`, `PLATFORM`, `OAUTH_PROXY_IMAGE`, `ROLLOUT_TIMEOUT`, `RELEASES_URL`, `DRY_RUN`, `ALLOW_TEMPLATE_MISMATCH`, `ALLOW_MUTABLE_TAG`) can be set on the command line or in `.env` (`make env` creates one). Run `make help` to list them.

Runtime variables of the deployed container (`GITHUB_TOKEN`, `SEAWEEDFS_IMAGE`, `STABLE_SOURCE`, `STABLE_CHANNEL`, `DSC_SAMPLE_REF`, `LOG_LEVEL`, ...) are documented once, in [README: Configuration](README.md#configuration).

Local development only (`dev.sh` sets the first five):

| Variable | Purpose |
|---|---|
| `DEV_MODE=true` | No oauth-proxy, no cluster-admin gate; binds `127.0.0.1`; checks Host/Origin |
| `DEV_TOKEN`, `DEV_USER` | Your `oc` token and username |
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | The API server from `oc whoami --show-server` |
| `KUBE_CA_FILE` | CA bundle for an API server with a private CA. Example: `oc config view --raw --minify -o jsonpath='{..certificate-authority-data}' \| base64 -d > /tmp/ca.pem`. Without it the system trust store is used (ROSA API certificates are publicly trusted) |
| `DEV_INSECURE_TLS=true` | Skips API server certificate verification (only with `DEV_MODE`; logs a warning). Prefer `KUBE_CA_FILE` |
| `BIND_ADDRESS`, `DEV_ALLOW_REMOTE=true` | A non-loopback bind address is refused unless `DEV_ALLOW_REMOTE=true` (anyone who reaches it acts with your token) |
| `DEV_FRONTEND_PORT` | The dev-server port accepted by the Host/Origin check (default 9000) |
| `API_TARGET` | webpack dev-server proxy target (default `http://127.0.0.1:8080`) |
| `PORT`, `STATIC_DIR`, `METRICS_PORT` | Listener port (8080), static directory (`./frontend/dist`), optional probe/metrics port |

## Getting Help

Run `make help` to see all available targets with descriptions:

```bash
make help              # Show all targets and current configuration
```

## Notes

- All `Makefile` variables can be overridden via environment, command line, or `.env` file.
- The `.env` file is not committed (listed in `.gitignore`).
- Use `make env` to create a `.env` file from defaults.
- The container build runs `go vet`, the Go tests, the frontend typecheck, ESLint and tests; any failure stops the build.
