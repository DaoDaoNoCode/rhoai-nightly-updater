# RHOAI Nightly Updater

A web dashboard for managing Red Hat OpenShift AI nightly builds on ROSA HCP clusters.

## Features

### Nightly Build Management
- **One-click update** with auto-fetch from Quay registry and FBC content preview
- **SSE streaming progress** — real-time step-by-step pipeline (8–13 steps depending on operation)
- **Reinstall** with full cleanup (webhooks, CRDs, stale component CRs) and channel override
- **Operator refresh** for same-version image updates without version change
- **Preflight checks** — pull secret, IDMS, operator health, registry access, node readiness
- **Downgrade prevention** — blocks update when selected version is older than current

### Cluster Visibility
- **DSC component status** — expandable breakdown (Ready / Needs Attention / Removed) with one-click fixes
- **Deployment table** — sortable, with pod-level ready count, rollout stuck detection, scheduling failure alerts
- **Git provenance** — commit SHA, diff link, and build date extracted from OCI image labels
- **Live diagnostics** — 9 automated health checks with doc-backed fix actions
- **Build Explorer** — browse all nightly tags, filter by version/type, preview FBC catalog contents inline

### Dashboard Dev (PR Testing)
- **Multi-container PR deploy** — scans 8 dashboard repos on Quay for matching PR images
- **One-click revert** to operator-managed images
- **Quick resource creator** — MinIO, per-project pipeline servers, MLflow CR lifecycle

### Platform
- **SSO authentication** via oauth-proxy with SAR gate (read-only mode for non-admins)
- **Confirmation modals** for all cluster-modifying actions
- **Activity audit log** stored in ConfigMap with user attribution
- **Dark mode**, structured JSON logging, Prometheus metrics + alerting rules
- **HA deployment** — 2 replicas with pod anti-affinity

## Architecture

```
Browser --> oauth-proxy (OpenShift SSO) --> Go backend --> Kubernetes API
                                                 |
                                        React + PF6 static files
```

- **Frontend**: React 18, PatternFly 6, TypeScript, webpack
- **Backend**: Go (net/http), ServiceAccount token for k8s API calls
- **Auth**: oauth-proxy sidecar handles SSO, passes user identity via X-Forwarded-User
- **RBAC**: Scoped ClusterRole (not cluster-admin) limited to RHOAI resources
- **Security**: NetworkPolicy blocks direct access to backend port; only oauth-proxy port exposed

### Two-Phase Progress Model

Operator mutations use a two-phase progress model:

- **Phase A -- SSE Streaming**: The backend executes Kubernetes operations (apply CatalogSource, patch Subscription, delete CSV, etc.) and streams step-by-step progress events to the frontend over Server-Sent Events.
- **Phase B -- Status Polling**: After the SSE stream completes, the frontend switches to polling `/api/status` to track OLM reconciliation (InstallPlan approval, CSV phase transitions, deployment rollouts).

### Update Pipeline (8 steps)

1. **Prerequisites** -- verify pull secret and IDMS exist
2. **Snapshot** -- save current deployment state for change detection
3. **CatalogSource** -- apply/update the FBC catalog image
4. **READY wait** -- wait for the CatalogSource pod to become READY
5. **Channel detect** -- query PackageManifest for the correct nightly channel
6. **Subscription** -- apply Subscription pointing to the nightly catalog + detected channel
7. **Delete CSV** -- remove current ClusterServiceVersion to force a fresh InstallPlan
8. **Verify** -- confirm InstallPlan is created and OLM begins reconciliation

## Pages

| Page | Description |
|---|---|
| Dashboard | Operator status, prerequisites, upgrade/reinstall with pre-flight checks, reconciliation progress, activity log |
| Components | DSC v2 components, expandable deployments with per-container pod view, direct links to pod logs in OpenShift console, deployment snapshot change detection |
| Build Explorer | Browse all FBC nightly tags, filter by version/type, search any image by digest, expand to see categorized component images with git provenance |
| Dashboard Dev | PR Deploy tab: multi-container PR deploy (scans 8 repos), one-click revert. Resources tab: MinIO setup, per-project pipeline servers, MLflow CR lifecycle with PR deploy/revert |
| Diagnostics | Live cluster health checks, auto-detected problems with severity labels, one-click fixes, contextual Learn More guidance, copyable diagnostic report |

## Quick Start (Deploy to Your Cluster)

No build required — deploy the pre-built image in one command:

```bash
oc new-project rhoai-nightly-updater

oc process -f https://raw.githubusercontent.com/bobbravo2/razzmatazz-serving-demo-rosa/main/nightly-updater/deploy/template.yaml \
  -p IMAGE=quay.io/juntao_wang/rhoai-nightly-updater:latest \
  -p NAMESPACE=rhoai-nightly-updater | oc apply -f -

oc rollout status deployment/rhoai-nightly-updater -n rhoai-nightly-updater --timeout=120s
```

Get the app URL and update the console launcher link:

```bash
ROUTE_URL=$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')
echo "App URL: $ROUTE_URL"

oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"$ROUTE_URL\"}}"
```

Open the URL in your browser — you'll be prompted to log in via OpenShift SSO. The app also appears in the OpenShift Console app launcher (grid icon).

### What gets created

- `ServiceAccount` with OAuth redirect for SSO
- `ClusterRole` + `ClusterRoleBinding` scoped to RHOAI resources (not cluster-admin)
- `Deployment` with oauth-proxy sidecar (2 containers)
- `Service`, `Route` (TLS reencrypt), `NetworkPolicy`
- `ServiceMonitor` for Prometheus metrics
- `PrometheusRule` with alerting rules
- `ConsoleLink` in the OpenShift app launcher

### Uninstall

```bash
oc delete consolelink rhoai-nightly-updater
oc delete project rhoai-nightly-updater
```

---

## Development

### Prerequisites

- OpenShift / ROSA HCP cluster
- `oc` CLI logged into the cluster
- `podman` (for building container image)
- quay.io account (for pushing image)

### Local Development

```bash
cd frontend && npm install && cd ..
make env   # creates .env file with defaults
./dev.sh
```

Open http://localhost:9000. In dev mode, the Go backend uses your `oc` token for cluster access. The webpack dev server proxies `/api` requests to the Go backend on port 8080.

Run `make help` to see all available targets.

### Makefile Variables

| Variable | Default | Description |
|---|---|---|
| `APP_NAME` | `rhoai-nightly-updater` | Application name used for deployment resources |
| `IMAGE` | `quay.io/juntao_wang/rhoai-nightly-updater` | Container image repository |
| `TAG` | `latest` | Image tag |
| `GIT_SHA` | auto-detected | Short git commit SHA (also used as secondary tag) |
| `NAMESPACE` | `rhoai-nightly-updater` | OpenShift project for deployment |
| `RUNTIME` | auto-detected (`podman` or `docker`) | Container runtime for building images |
| `PLATFORM` | `linux/amd64` | Target platform for container builds |

### Build & Deploy (from source)

```bash
make all IMAGE=quay.io/<your-user>/rhoai-nightly-updater
```

This builds the container (linux/amd64), pushes to quay.io, and deploys to the cluster with oauth-proxy authentication. The app URL is printed at the end.

Individual steps:

```bash
make build IMAGE=quay.io/<user>/rhoai-nightly-updater
make push  IMAGE=quay.io/<user>/rhoai-nightly-updater
make deploy IMAGE=quay.io/<user>/rhoai-nightly-updater
```

## Testing

Run all Go tests (~4 seconds; timing constants are overridden in tests):

```bash
make test-go
```

Run tests with verbose output:

```bash
go test -v ./... -count=1
```

Run tests for a specific package:

```bash
go test -v ./pkg/cluster/
go test -v ./pkg/api/
```

## Cluster Prerequisites

Before installing nightly builds, the cluster needs:

1. **Pull secret** -- `additional-pull-secret` in `kube-system` with quay.io/rhoai credentials (can be created through the app's UI)
2. **Image mirror** -- IDMS via `rosa create image-mirror` to redirect `registry.redhat.io/rhoai` to `quay.io/rhoai`

## Security

### Authentication

- oauth-proxy sidecar handles OpenShift SSO authentication
- Users must authenticate before accessing any page
- Session cookies use `SameSite=Strict` and are `HttpOnly`

### Authorization

- Scoped ClusterRole with permissions limited to RHOAI-specific resources:
  - Subscriptions, CSVs, CatalogSources, InstallPlans (operator management)
  - Secrets (pull secret, MinIO credentials, DSPA secrets)
  - Namespaces (read + create/delete for MinIO and DS projects)
  - Pods (read-only for monitoring)
  - Deployments, Services, PVCs, Routes (full CRUD for MinIO, Dashboard Dev, resource creator)
  - DSPAs (pipeline server lifecycle)
  - MLflow CRs (MLflow lifecycle and PR deploy)
  - Webhooks and CRDs (for rollback cleanup)
  - ClusterVersions, Consoles, ImageDigestMirrorSets (read cluster info)
  - DataScienceClusters (read DSC status)
  - PackageManifests (read available packages)
- NOT cluster-admin -- cannot access arbitrary resources

### Network Security

- Backend listens on port 8080, only reachable by oauth-proxy sidecar (same pod)
- NetworkPolicy blocks ALL external ingress to port 8080
- Only port 8443 (oauth-proxy with TLS) is exposed via the Service/Route
- Route uses TLS reencrypt termination

### Data Security

- Pull secret credentials are never logged or returned in API responses
- User identity comes from trusted oauth-proxy headers (X-Forwarded-User)
- Activity log records who performed each operation for accountability

### Dev Mode

- `DEV_MODE`/`DEV_TOKEN`/`DEV_USER` only active when the SA token is not available (never in-cluster)
- In-cluster ServiceAccount token always takes priority

## API Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/api/health` | Liveness probe |
| GET | `/api/health/ready` | Readiness probe (checks Kubernetes API connectivity) |
| GET | `/api/user/permissions` | Check if logged-in user can perform mutations |
| GET | `/api/status` | Cluster and operator status |
| GET | `/api/components` | DSC components and deployments |
| GET | `/api/components?labels=true` | Same, with git commit info (slower) |
| GET | `/api/debug` | Pod details per namespace |
| GET | `/api/activity` | Activity audit log |
| GET | `/api/latest-nightly` | Latest FBC image from Quay |
| GET | `/api/nightly-tags` | Last 5 version tags from Quay |
| GET | `/api/build-explorer/tags` | All nightly tags with build timestamps |
| GET | `/api/build-explorer/content` | FBC catalog content (component images) for a tag |
| POST | `/api/update` | Install/update nightly build |
| POST | `/api/update/stream` | SSE streaming update (step-by-step progress events) |
| POST | `/api/refresh` | Delete CSV+Subscription to trigger fresh InstallPlan |
| POST | `/api/refresh/stream` | SSE streaming refresh (step-by-step progress events) |
| POST | `/api/rollback` | Reinstall operator (stable or nightly) |
| POST | `/api/rollback/stream` | SSE streaming reinstall (step-by-step progress events) |
| POST | `/api/assist-rollout` | Detect and unblock stuck deployment rollouts |
| GET | `/api/diagnostics` | Run live cluster health checks and problem detection |
| POST | `/api/diagnostics/fix` | Apply an auto-fix for a detected problem |
| POST | `/api/setup/pull-secret` | Create/update pull secret |
| GET | `/api/test-pull-secret` | Validate pull secret |
| GET | `/api/verify-nodes` | Verify node readiness |
| GET | `/api/dashboard/state` | Current dashboard deployment state (default vs PR) |
| POST | `/api/dashboard/deploy-pr` | Deploy a PR image to the dashboard |
| POST | `/api/dashboard/revert` | Revert dashboard to operator-managed image |
| GET | `/api/resources/status` | Status of test infrastructure (MinIO, pipeline servers) |
| GET | `/api/resources/projects` | List Data Science projects visible to user |
| POST | `/api/resources/minio/setup` | Deploy MinIO with S3 bucket |
| POST | `/api/resources/minio/teardown` | Delete MinIO namespace |
| POST | `/api/resources/pipeline-server/setup` | Create pipeline server (DSPA) in a project |
| POST | `/api/resources/pipeline-server/teardown` | Remove pipeline server from a project |
| POST | `/api/resources/mlflow/setup` | Create MLflow CR in redhat-ods-applications |
| POST | `/api/resources/mlflow/teardown` | Delete MLflow CR |
| POST | `/api/resources/mlflow/deploy-pr` | Patch MLflow CR with a PR image |
| POST | `/api/resources/mlflow/revert` | Revert MLflow CR image to default |
| POST | `/api/pageview` | Record page view / feature usage (privacy-safe, aggregate only) |
| GET | `/metrics` | Prometheus metrics (uptime, requests, page views, feature usage) |

## Project Structure

```
├── main.go                     # HTTP server, routing, graceful shutdown
├── RUNBOOK.md                  # Operational runbook for troubleshooting
├── SECURITY.md                 # Security model documentation
├── pkg/
│   ├── api/
│   │   ├── handlers.go         # REST handlers with auth middleware
│   │   ├── handlers_test.go    # Tests for API handlers
│   │   ├── sse.go              # SSE streaming writer for real-time progress
│   │   └── metrics.go          # Prometheus metrics registration
│   ├── cluster/
│   │   ├── client.go           # K8s API client with scoped RBAC
│   │   ├── status.go           # Cluster status queries
│   │   ├── operations.go       # Update, reinstall, pull secret ops
│   │   ├── components.go       # DSC components and deployments
│   │   ├── debug.go            # Pod details
│   │   ├── quay.go             # Quay registry API (tag listing, digests, build dates)
│   │   ├── fbc.go              # FBC catalog parsing (OCI layer extraction, YAML parsing)
│   │   ├── image_labels.go     # Git commit extraction from OCI image config labels
│   │   ├── dashboard.go        # Dashboard Dev multi-container PR deploy/revert
│   │   ├── resources.go        # Quick Resource Creator: orchestration and status
│   │   ├── minio.go            # MinIO deployment lifecycle (setup, teardown, status)
│   │   ├── mlflow.go           # MLflow CR lifecycle (setup, teardown, PR deploy/revert)
│   │   ├── pipeline_server.go  # Pipeline server (DSPA) lifecycle per project
│   │   ├── s3.go               # S3 bucket operations (create bucket via MinIO API)
│   │   ├── authz.go            # oauth-proxy SAR gate permission checks
│   │   ├── diagnostics.go      # Live diagnostics engine (9 checks, auto-fix actions)
│   │   ├── rollout.go          # Assist Rollout: detect and unblock stuck deployments
│   │   ├── snapshot.go         # Deployment snapshot for change detection
│   │   ├── activity.go         # ConfigMap-backed activity log
│   │   ├── diagnostics_test.go # Tests for diagnostics engine and auto-fix
│   │   ├── activity_test.go    # Tests for activity log
│   │   ├── fbc_test.go         # Tests for FBC catalog parsing
│   │   ├── minio_test.go       # Tests for MinIO lifecycle
│   │   ├── status_test.go      # Tests for cluster status queries
│   │   ├── testmain_test.go    # Test suite setup
│   │   ├── dashboard_test.go   # Tests for dashboard PR deploy/revert
│   │   ├── image_labels_test.go # Tests for OCI image label extraction
│   │   ├── operations_test.go  # Tests for update/reinstall operations
│   │   └── s3_test.go          # Tests for S3 bucket operations
│   ├── middleware/
│   │   ├── security.go         # Security headers
│   │   └── security_test.go    # Tests for security headers
│   └── types/types.go          # Shared request/response types
├── frontend/
│   ├── src/
│   │   ├── App.tsx             # Layout, routing, side nav
│   │   ├── index.tsx           # React entry point
│   │   ├── types.ts            # TypeScript type definitions
│   │   ├── constants.ts        # Shared constants
│   │   ├── utils.ts            # Shared utilities
│   │   ├── pages/              # Dashboard, Components, Build Explorer, Dashboard Dev, Troubleshooting
│   │   ├── components/         # Reusable UI components
│   │   ├── hooks/              # useAsyncData custom hook
│   │   └── services/api.ts     # API client functions
│   ├── tsconfig.json           # TypeScript configuration
│   └── webpack.config.js
├── docs/
│   └── OPENSHIFT_INTEGRATION.md # OpenShift-specific behaviors and incident learnings
├── scripts/
│   └── smoke-test.sh           # Smoke test script
├── deploy/
│   └── template.yaml           # OpenShift deployment template
├── Containerfile               # Multi-stage build (Node 22 + Go 1.24 + UBI9)
├── Makefile                    # build, push, deploy, dev, test, clean
└── dev.sh                      # Local development launcher
```

## Uninstall

```bash
make undeploy
```

This deletes the namespace and all resources including the ClusterRole and ClusterRoleBinding.

## License

Apache License 2.0
