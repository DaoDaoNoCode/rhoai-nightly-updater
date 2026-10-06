# Security Model

The updater runs as one pod with two containers: **oauth-proxy** (OpenShift sign-in) and **app** (the Go backend and the static UI). Users act through their own identity. Cluster changes are made with the app's ServiceAccount (SA) token, and only after the user's own token has proven that the user is a cluster-admin.

## Who can do what

| Who | Can |
|---|---|
| Anyone who signs in and can `list pods` in `redhat-ods-operator` (the oauth-proxy `--openshift-sar` gate) | Open the UI and read everything it shows (read-only) |
| Users with the **cluster-admin** role | Also run every action that changes the cluster |

Changes require cluster-admin because the tool does cluster-admin work on the user's behalf. It installs an operator (OLM grants whatever the CSV asks for), writes the node pull secret in `kube-system`, deletes admission webhooks and patches CRDs. Namespace `admin`/`edit` and `dedicated-admin` are not enough.

## Authentication and authorization flow

1. The browser opens the Route. oauth-proxy runs the OpenShift OAuth flow and checks the SAR gate above.
2. oauth-proxy forwards the request to the app on `127.0.0.1:8080` with `X-Forwarded-Access-Token` (the user's OAuth token from its own session) and `X-Forwarded-User`.
3. The app resolves the user from the token (`GET user.openshift.io/v1/users/~`, cached for 1 minute per token). `X-Forwarded-User` is not trusted for identity. Every API request is authenticated this way, `/api/pageview` included.
4. Every cluster-changing request (every non-GET route except `POST /api/pageview`, including the Update dry run) also runs a SubjectAccessReview **with the user's token** (`authorization.openshift.io` SAR, `scopes: []`) for verb `*`, resource `*`, group `*`, cluster-wide. That is the cluster-admin role's own rule. A denied user gets 403 "Read-only access". If the review cannot run, the user gets 503 and nothing changes. `TestEveryNonGetRouteIsGated` checks that every route is gated.
5. Only then does the app act, with the SA token.

Token rules (pkg/api/identity.go, handlers.go):

- In the cluster, a request that carries `Authorization: Bearer` is refused with 401. oauth-proxy never forwards a client's bearer token, so such a request did not come through it. The `Authorization: Basic` header that oauth-proxy itself sets (`--pass-basic-auth`, default true) is ignored, not refused.
- Bearer tokens are accepted only with `DEV_MODE=true` (local development, no proxy).
- The template must not add `--openshift-delegate-urls` or `--pass-user-bearer-token` to oauth-proxy, because both accept client tokens. `TestOAuthProxyForwardsOnlyItsOwnSessionToken` pins this.

## ServiceAccount RBAC

`deploy/template.yaml` holds exactly the requests the code makes. `pkg/cluster/rbac_coverage_test.go` derives every API call from the code and fails when a rule is missing or unused. Cluster-scoped names carry the namespace (`<APP_NAME>-<NAMESPACE>`), so two installs do not collide.

**Namespaced Roles:**

| Namespace | Rules |
|---|---|
| the app's namespace | ConfigMaps: `create`; `get` on `rhoai-nightly-updater-{activity,snapshot,operation}`, `update` activity, `patch` snapshot and operation |
| `kube-system` | Secret `additional-pull-secret`: `get`, `patch` |
| `openshift-marketplace` | CatalogSources: `get list create patch delete`; PackageManifests: `list` |

**Cluster-wide (ClusterRole).** The operator namespaces (`redhat-ods-*`, `minio`) and the user's pipeline projects do not exist when the template is applied, so these rules are cluster-wide. Wherever the API allows it, they are limited to fixed object names. `create` cannot be limited by name.

| What | Verbs | Limited to names? |
|---|---|---|
| Secrets | `create` | no (pipeline projects are user-chosen) |
| Secrets | `get patch delete` | `minio-secret`; `nightly-dspa-s3` (`get delete patch`); legacy `dashboard-dspa-secret` (`get delete`) |
| Namespaces | `get list create`; `patch` | `patch` only `redhat-ods-operator`, `minio`. Nothing is ever deleted |
| Deployments | `get list patch create`; `delete` | `delete` only `minio` |
| Services | `get create`; `patch delete` | `patch delete` only `minio-service` |
| PVCs | `list create`; `patch delete` | `patch delete` only `minio-pvc` |
| Routes | `create`; `get` | `get` on `rhods-dashboard`, `mlflow`, `minio-api`, `minio-ui`; `patch delete` on `minio-api`, `minio-ui` |
| NetworkPolicies | `create`; `get patch delete` | the second set only on `minio-ingress` |
| Pods, ReplicaSets, Events, EndpointSlices, Nodes | `list` | read-only |
| Subscriptions | `create`; `get patch delete` | the second set only on `rhods-operator` |
| OperatorGroups | `create list`; `patch` | `patch` only `rhods-operator` |
| ClusterServiceVersions | `get list delete` | no |
| InstallPlans | `get list patch delete` | no |
| Validating/Mutating webhook configurations | `list delete` | no |
| CRDs | `list`; `get patch` | `get patch` only the DSC and DSCI CRDs |
| DataScienceClusters | `get list patch create` | no |
| DSCInitializations, Platforms | `list` (`get` Platform `default`) | read-only |
| `components.platform.opendatahub.io/*` (module CRs) | `list patch` | no; the kinds change between releases and are discovered |
| DSPAs | `list create`; `get delete` | `get delete` only `nightly-dspa` and legacy `dspa`; `patch` only `nightly-dspa` |
| MLflows | `create`; `get patch delete` | the second set only on `mlflow` |
| ClusterVersion, Console, IDMS, ConfigMap `odh-upgrade-acks` | read | — |

There is no cluster-wide `list` or general `get` on Secrets. The SA cannot read arbitrary Secrets.

**Residual risk.** The SA can still create Secrets anywhere, create Subscriptions (OLM then installs whatever the CSV requests), delete CSVs and webhook configurations, and patch any Deployment. Treat its token like an administrator credential: anyone who can read it (or `exec` into the pod) can act as the SA. Namespace `admin` on the app's namespace is therefore effectively privileged.

## Network exposure

| Port | Where | Who can reach it |
|---|---|---|
| 8080 | app, bound to `127.0.0.1` (`BIND_ADDRESS`) | only oauth-proxy in the same pod. The API is never exposed on the pod IP |
| 8443 | oauth-proxy, Service port `https`, Route (TLS `reencrypt`) | NetworkPolicy: only from the ingress (router) namespaces |
| 9090 | app, `METRICS_PORT`: `/api/health`, `/api/health/ready`, `/api/version`, `/metrics` only | NetworkPolicy: only from `openshift-monitoring` and `openshift-user-workload-monitoring`. Kubelet probes are node traffic and always allowed |

## Sessions and CSRF

- The cookie secret is in Secret `<APP_NAME>-proxy` (key `session_secret`), mounted with `--cookie-secret-file`. Re-applying the template keeps it, so sessions survive upgrades. `make deploy`/`make upgrade` create it once.
- Cookies use `--cookie-expire=23h`, below the default OAuth access-token lifetime (24h), and `SameSite=Strict; Secure; HttpOnly`. An expired session shows a "Sign in again" banner.
- The Deployment's `revisionHistoryLimit` is 2. After a successful rollout, `make deploy`/`make upgrade` delete the app's old ReplicaSets: those created before the cookie Secret held the secret in plaintext env.
- State-changing `/api/` requests must be `Content-Type: application/json` (otherwise 415). A browser cannot send that cross-site without a CORS preflight, and the backend never grants one.
- Every response carries CSP, `X-Frame-Options: DENY`, `nosniff`, HSTS and `Cache-Control: no-store` (pkg/middleware/security.go).

## Abuse limits and input validation

- Mutations: one accepted request per user and endpoint per 30 s, taken when the request is accepted. Rejected requests (4xx/5xx, including 409 busy) and dry runs do not use the slot. One cluster operation at a time (409 `cluster_busy`). A 15-minute deadline applies, independent of the browser.
- Mutation bodies are capped at 4096 bytes (`/api/pageview` at 256), and the read timeout is 15 s.
- Images must match `quay.io/rhoai/` or `registry.redhat.io/rhoai/`. A custom Reinstall accepts only `quay.io/rhoai/rhoai-fbc-fragment` with a tag and/or digest. PR numbers, DSC names, channels and namespaces are validated before use. The static file server is protected against path traversal.
- `/api/pageview` stores only aggregate counters, with labels limited to known page/feature names. No user identity is stored.

## Credentials

- The pull-secret token is never logged, returned, or written to the activity log. Only the `quay.io/rhoai` entry of `kube-system/additional-pull-secret` is replaced; other registries are kept.
- The Quay token is derived from the cluster pull secret on the server and never sent to the browser.
- MinIO's root user and password are random per install and stored in `minio/minio-secret`.
- `GITHUB_TOKEN` (optional) is sent only to `api.github.com`. Use a read-only token with public-repository access.

## Local development (`DEV_MODE=true`)

- Used by `dev.sh`. With no mounted SA token, every request runs with your own `oc` token (`DEV_TOKEN`) and **there is no cluster-admin gate**.
- The server binds `127.0.0.1` by default. A non-loopback `BIND_ADDRESS` exits at startup unless `DEV_ALLOW_REMOTE=true` (not recommended: anyone who can reach it acts with your token).
- Requests whose `Host` or `Origin` is not this machine on the API port or `DEV_FRONTEND_PORT` (default 9000) are refused. This blocks DNS rebinding.
- API server TLS is verified, in this order: `KUBE_CA_FILE` if set (an unusable file fails closed), the in-pod SA CA, then the system trust store. Verification is skipped only with `DEV_MODE=true` **and** `DEV_INSECURE_TLS=true`, which logs a warning.
- DEV_MODE cannot weaken an in-cluster pod. The bypass requires that no SA token be mounted.

## Known limitations and residual risks

- **SA token.** See *Residual risk* above.
- **MinIO.** The bundled MinIO is the final open-source release (`quay.io/hummingbird-community/minio`, RELEASE.2025-10-15T17-29-55Z, pinned by digest). Advisories published after it, for example GHSA-hv4r-mvr4-25vw, are fixed only in the commercial AIStor, and no open-source build has the fix. Mitigations:
  - no Route to the S3 API;
  - root credentials random per install;
  - NetworkPolicy `minio-ingress`: S3 port 9000 from pods in any namespace, console port 9090 only from the router.

  Any pod in the cluster that knows the access key can still reach the S3 API. MinIO is meant for test data only.
- **Restarts.** A running operation delays a pod restart by up to ~17 minutes (see [README: cluster safety](README.md#how-the-tool-keeps-your-cluster-safe)). A SIGKILL or node loss still interrupts it. There is no durable job queue; the next start reports the interrupted operation.
- **Mirror visibility.** The GitHub mirror contains team-internal links (Bitwarden collection, Slack channels, the IPA realm and AWS SAML alias). They are access-controlled, but internal. Decide with the team whether the mirror should be private.
