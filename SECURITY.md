# Security Model

## Overview

The app uses a split-token architecture:

- **User identity**: OAuth token from oauth-proxy (X-Forwarded-User header)
- **Cluster operations**: ServiceAccount token with scoped RBAC

Users authenticate through OpenShift SSO. The backend uses the user's OAuth token for self permission reviews. Reads and mutations use the ServiceAccount token, which has a fixed set of permissions defined by a custom ClusterRole.

## Authentication Flow

1. User accesses the Route URL
2. oauth-proxy redirects to OpenShift SSO for authentication
3. After login, oauth-proxy creates a session cookie and forwards requests to the backend
4. oauth-proxy sets `X-Forwarded-User` and `X-Forwarded-Access-Token` headers
5. Backend verifies `X-Forwarded-Access-Token` is present (authentication gate)
6. Before each mutation, the backend checks full user RBAC using OpenShift SubjectAccessReview with the user token and `scopes: []`. It requires `update` on `subscriptions.operators.coreos.com` in `redhat-ods-operator`. The API server derives identity and groups from the token. Denied users are read-only; review errors block mutations. The ServiceAccount performs accepted cluster operations.
7. The `X-Forwarded-User` value is recorded in the activity log for audit purposes

Additionally, oauth-proxy performs a SubjectAccessReview (SAR) check before granting access. The configured SAR requires the user to have `pods:list` permission in the `redhat-ods-operator` namespace.

## RBAC Scope

The ServiceAccount has a custom ClusterRole with permissions limited to:

| API Group | Resources | Verbs | Purpose |
|---|---|---|---|
| `config.openshift.io` | clusterversions, consoles, imagedigestmirrorsets | get, list | Read cluster info |
| `operators.coreos.com` | subscriptions, clusterserviceversions, catalogsources, installplans | get, list, create, update, patch, delete | Manage RHOAI operator |
| `packages.operators.coreos.com` | packagemanifests | get, list | Read available packages |
| `""` (core) | secrets | get, list, create, update, patch, delete | Manage pull secret, MinIO credentials, DSPA secrets |
| `""` (core) | pods | get, list | Read pods for debug/components |
| `""` (core) | nodes | get, list | Read node status for capacity checks |
| `""` (core) | namespaces | get, list, create, delete | Read namespaces; create/delete for MinIO and DS projects |
| `""` (core) | services, persistentvolumeclaims | get, list, create, update, patch, delete | Quick Resource Creator (MinIO service, PVCs) |
| `apps` | deployments | get, list, create, update, patch, delete | Component status; MinIO deployment; Dashboard Dev PR deploy/revert |
| `apps` | replicasets | get, list, create, update, patch, delete | Rollout management for stuck deployments |
| `operators.coreos.com` | operatorgroups | get, list, create, update, patch, delete | Operator lifecycle management |
| `route.openshift.io` | routes | get, list, create, update, patch, delete | MinIO routes; read dashboard/MLflow routes |
| `""` (core) | configmaps | get, create, update, patch | Activity log ConfigMap |
| `admissionregistration.k8s.io` | validatingwebhookconfigurations, mutatingwebhookconfigurations | get, list, delete | Clean up webhooks during rollback |
| `apiextensions.k8s.io` | customresourcedefinitions | get, patch | Patch CRDs during rollback |
| `datasciencecluster.opendatahub.io` | datascienceclusters | get, list, create, update, patch | Read DSC status |
| `datasciencepipelinesapplications.opendatahub.io` | datasciencepipelinesapplications | get, list, create, update, patch, delete | Pipeline server (DSPA) lifecycle |
| `mlflow.opendatahub.io` | mlflows | get, list, create, update, patch, delete | MLflow CR lifecycle and PR deploy |
| `components.platform.opendatahub.io` | * | get, list, patch | Component CR lifecycle (stuck finalizer cleanup) |
| `gateway.networking.k8s.io` | gateways | get, patch | Gateway management |
| `user.openshift.io` | users | get | Read user identity |

All mutation operations -- including Dashboard Dev PR image deployment, MinIO setup, pipeline server creation, and MLflow management -- use the **ServiceAccount token**. The user's OAuth token is used for the permission check shared by mutation endpoints and `/api/user/permissions`. The app grants its mutation capabilities to operator editors; it does not independently delegate each operation's underlying Kubernetes permissions. Viewers who pass the login gate have read-only access.

This is NOT cluster-admin. The ServiceAccount cannot access arbitrary resources, namespaces, or perform destructive operations outside the scope listed above.

## Network Isolation

- The backend listens on port 8080 inside the pod
- A **NetworkPolicy** restricts ingress to port 8443 only (the oauth-proxy HTTPS port) -- port 8080 is accessible only from `openshift-monitoring` and `openshift-user-workload-monitoring` namespaces for Prometheus scraping
- The **Service** only exposes port 8443
- The **Route** uses TLS `reencrypt` termination, meaning traffic is encrypted both from the client to the router and from the router to oauth-proxy
- oauth-proxy forwards authenticated requests to the backend over `localhost:8080` within the same pod

## Credential Handling

- Pull secret auth values are never logged or included in API responses. The `ACTION op=create-pull-secret` log line does not include the auth payload.
- The Quay registry auth token is extracted from the cluster's pull secret server-side and never exposed to the client
- Activity log records usernames and operation types but never credentials or secret values
- Request bodies for pull secret operations are limited to 4096 bytes to prevent abuse

## Session Security

- oauth-proxy session cookies use `SameSite=Strict` to prevent CSRF
- TLS is enforced end-to-end via the reencrypt Route
- The backend applies security headers (via `middleware/security.go`) to all responses

## Input Validation

- Image references are validated against a strict regex before being used in any cluster operation
- **Image registries**: Update and Build Explorer accept `quay.io/rhoai/` and `registry.redhat.io/rhoai/`. Custom reinstall accepts any explicitly supplied Quay FBC repository with a tag or full SHA256 digest. Fresh catalog preflight validates the image and channel before cleanup. Digests are honored even if a rolling tag has moved.
- Request bodies are size-limited using `http.MaxBytesReader` (4096 bytes for mutation endpoints)
- File serving includes path traversal protection -- paths are cleaned and verified to stay within the static directory

## Dashboard Dev Endpoints

`POST /api/dashboard/deploy-main` uses the same mutation authentication and rate limiting as PR deploy/revert. On installations with dashboard-operator, only deployments sharing the host's Dashboard controller owner UID are image targets, and each build component must match an operator image environment variable. The operator's own image and infrastructure dependency images are excluded. Operator replica count and image-binding recovery data are stored in its deployment annotation before pausing it.

- `POST /api/dashboard/deploy-pr` and `POST /api/dashboard/revert` are protected by `withMutationAuth` (authentication, full user RBAC permission review, and rate limit). Rate limiting is per-user AND per-endpoint — the rate limit key is `username:path` with a 30-second window. Each endpoint has its own rate limit window per user. The SA token performs all mutation operations (deployment patches, annotation updates). The user's OAuth token carries `user:check-access` and `user:info` scopes: it checks full RBAC via OpenShift self SAR but cannot perform cluster mutations. The oauth-proxy SAR check (`pods:list` in `redhat-ods-operator`) is the authorization gate that controls who can reach the app at all.

## MLflow Endpoints

- `POST /api/resources/mlflow/setup`, `teardown`, `deploy-pr`, and `revert` are protected by `withMutationAuth`. The SA token creates, patches, and deletes the MLflow CR (`mlflow.opendatahub.io/v1`). PR deploy verifies the image exists on Quay before patching.

## Known Limitations

- **Operation lifetime**: Accepted mutations continue if the browser disconnects, with a 15-minute deadline. Operator lifecycle operations keep the mutation lock until completion. Cleanup failures trigger a bounded attempt to restore the previous catalog and Subscription desired state. On SIGTERM the backend stops accepting mutations, reports not ready, and waits up to 16 minutes for running ones to finish (`SHUTDOWN_DRAIN_TIMEOUT`); the Deployment uses the `Recreate` strategy and a 1020-second termination grace period so only one pod runs operations. A crash or forced kill still interrupts in-process operations; there is no durable job queue.
- **Usage analytics are privacy-safe**: The app tracks aggregate page view and feature usage counters via Prometheus metrics (`/metrics` endpoint). No user identity, IP addresses, or session data is stored — only counters like `page_views_total{page="dashboard"} 42`. The `POST /api/pageview` endpoint requires authentication and validates label names against a strict regex with a 100-label cap to prevent cardinality attacks.
- **Dev mode bypass**: When `DEV_MODE=true` and no ServiceAccount token is available, authentication is bypassed using `DEV_TOKEN`. This path is never active in-cluster because the SA token file is always mounted.
- **TLS enforcement**: In-cluster, the backend requires the ServiceAccount CA certificate for TLS verification. `InsecureSkipVerify` is only allowed when `DEV_MODE=true` (local development). In production without the CA cert, the server exits with a fatal error.
- **Shared HTTP client**: All Quay/GitHub API calls use a shared HTTP client with connection pooling to prevent file descriptor exhaustion under load.
- **Cache TTL**: Image label and commit date caches use a 1-hour TTL with lazy eviction to prevent unbounded memory growth. FBC content is cached by digest (immutable). Tag scan results are cached for 5 minutes.
