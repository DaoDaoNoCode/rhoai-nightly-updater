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

**Cluster-wide (ClusterRole).** The operator namespaces (`redhat-ods-*`, `seaweedfs`) and the user's pipeline projects do not exist when the template is applied, so these rules are cluster-wide. Wherever the API allows it, they are limited to fixed object names. `create` cannot be limited by name.

| What | Verbs | Limited to names? |
|---|---|---|
| Secrets | `create` | no (pipeline projects are user-chosen) |
| Secrets | `get patch delete` | `seaweedfs-s3-credentials`; `nightly-dspa-s3` (`get delete patch`); the earlier storage's `minio-secret` and legacy `dashboard-dspa-secret` (`get delete`) |
| Namespaces | `get list create`; `patch`; `delete` | `patch` only `redhat-ods-operator`; `delete` only `minio` (the earlier storage's namespace, at the end of Migrate storage) and `seaweedfs` (at the end of the storage's Tear down), each only when the user ticks it, offered only when it carries the tool label and the tool's managers wrote it (UID precondition) |
| Deployments | `get list patch create`; `delete` | `delete` only `seaweedfs` (the S3 storage, and the SeaweedFS earlier versions ran in namespace `minio`) and `minio` (MinIO of earlier versions) |
| Services | `get create`; `patch delete` | `patch delete` only `seaweedfs-s3`; `delete` only `minio-service` (the earlier storage's) |
| PVCs | `list create`; `patch delete` | `patch delete` only `seaweedfs-data`; `delete` only `seaweedfs-pvc` and `minio-pvc` (the earlier storage's data) |
| Routes | `create`; `get` | `get` on `rhods-dashboard`, `mlflow`, `seaweedfs-admin`, `minio-api`, `minio-ui`; `patch delete` on `seaweedfs-admin`; `delete` on `minio-api`, `minio-ui` (the earlier storage's) |
| NetworkPolicies | `create`; `get patch delete` | the second set only on `seaweedfs-ingress` (the name in both storage namespaces); `get delete` on `minio-ingress` (MinIO's policy of earlier versions) |
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
| DSPAs | `list create`; `get patch delete` | only `nightly-dspa` and legacy `dspa` (Migrate storage moves a `dspa` an earlier version created to the new storage) |
| Jobs | `create`; `get delete` | `get delete` only `seaweedfs-migrate`, Migrate storage's copy Job in namespace `seaweedfs` |
| MLflows | `create`; `get patch delete` | the second set only on `mlflow` |
| ClusterVersion, Console, IDMS, ConfigMap `odh-upgrade-acks` | read | — |
| `operator.openshift.io/*` (prerequisite operands such as `JobSetOperator/cluster`) | `get` | only objects named `cluster`; the kind comes from the prerequisite's CSV. This group also holds OpenShift's own operator configs (`console/cluster`, …), which become readable; they hold no secrets (template revision 4) |
| cert-manager `Certificates` | `list` | read-only; Certificates name Secrets but hold no key material (template revision 4) |

There is no cluster-wide `list` or general `get` on Secrets. The SA cannot read arbitrary Secrets.

**Audit: template revision 5 (3.0.0), the S3 storage's new names.** The storage moved from the MinIO-era names in namespace `minio` to its own names in namespace `seaweedfs`. Storage an earlier version left in `minio` is only detected, read for the migration, and deleted.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| Secrets `get patch delete` on `seaweedfs-s3-credentials` (replaces `minio-secret` for these verbs) | read, change and delete the storage's S3 keys | read or change any Secret named `seaweedfs-s3-credentials` in any namespace: the same reach as the `minio-secret` rule it replaces, under a name only this tool uses | the namespace is created by the tool after the template is applied, so the rule cannot be a Role; RBAC cannot limit a ClusterRole rule to one namespace |
| Secrets on `minio-secret`: `patch` removed, `get delete` kept | read the earlier storage's keys (the migration gives the new storage the same keys) and delete it with that storage | narrower than before | `get` is needed to keep the keys, `delete` to clean up |
| Services `patch delete` on `seaweedfs-s3`; `minio-service` down to `delete` | apply and remove the storage's Service | change or delete a Service of that name anywhere; `minio-service` can no longer be changed, only deleted | as above |
| PVCs `patch delete` on `seaweedfs-data`; `seaweedfs-pvc` down to `delete` (next to `minio-pvc`) | apply and remove the storage's data volume, delete the earlier storage's | delete a PVC of one of those names anywhere, and its data: the reach of the earlier rules | as above; PVCs are still listed, never read by name |
| Routes `get patch delete` on `seaweedfs-admin`; `minio-api`, `minio-ui` down to `get delete` | the admin UI Route; read the earlier Routes' hosts (a pipeline server may use one, so teardown checks them) and delete them | expose or remove the admin UI Route of that name; the earlier Routes can no longer be changed | as above |
| Namespaces `patch` on `minio` removed | — | narrower | the tool no longer relabels the earlier namespace |
| Namespaces `delete` on `seaweedfs` (next to `minio`) | the storage's Tear down may also delete namespace `seaweedfs`, which the tool created: only when the user ticks "Also delete namespace seaweedfs", only after every storage object is gone, only when the namespace carries the tool label and a tool manager wrote it, with a UID precondition. The dialog says it deletes everything in the namespace and lists the Deployments and PVCs the tool sees there | delete a namespace named `seaweedfs` in the cluster, with everything in it: the S3 storage and its data (pipeline artifacts), which the token could already delete object by object (`delete` on the Deployment, Secret and PVC), plus anything else someone put there. It mints no access | `delete` is limited to the one name; a namespace cannot be a Role's own resource, and the tool creates it after the template is applied |

No verb was added on any resource type; every new name replaces an old one, and the old names keep only `get` and `delete` where the migration and teardown need them.

**Audit: template revision 5 (3.0.0), Migrate storage.** Moving the earlier storage's data and pipeline servers to the new storage, and removing the old storage afterwards, needs three grants.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| Jobs (`batch`) `create` | start the copy Job `seaweedfs-migrate` (rclone, pinned by digest; `RCLONE_IMAGE` overrides it) in namespace `seaweedfs` | create a Job with any image in any namespace. Its pods run as that namespace's `default` ServiceAccount under the namespace's SCC admission, so they gain no API access the token does not already have; this is the reach of the Deployment `create` the SA already holds | `create` cannot be limited by name (the collection POST carries none), and the namespace is created by the tool, so a Role cannot hold it. The copy has to run inside the cluster network: the backend could only stream every object through its own pod |
| Deployments `list` in `minio` (existing cluster-wide grant) | name the Deployments the tool did not create, before the user decides to delete the namespace | none new | — |
| Jobs `get delete` on `seaweedfs-migrate` | read the Job's state and delete it (by UID) once its result is read | read or delete a Job of that name in any namespace | as above; the result comes back in the pod's termination message, read with the existing pod `list`, so no `pods/log` grant is added |
| DSPAs `patch` on `dspa` (next to `nightly-dspa`) | move a pipeline server an earlier version created to the new storage, and label it | change any DSPA named `dspa`, which is also the name odh-dashboard gives the pipeline server it creates. The code patches one only when its name, the old storage's address and its secret name match and the tool's field managers (`rhoai-nightly-updater`, or `Go-http-client` of the earliest versions) own its `spec.objectStorage.externalStorage` host and secret name (`dspaInfo.managedByTool`). The dashboard writes as no manager (its Node backend sends no User-Agent) or as `Mozilla` (its Go BFF forwards the browser's), so its DSPAs fail that test. The patch carries a `resourceVersion` precondition; other DSPAs are listed for the user, never changed | RBAC cannot tell who created an object; the older versions used this name |
| Namespaces `delete` on `minio` | delete namespace `minio` after Migrate storage removed the old storage, when the user ticks it (unticked by default); offered only when the namespace carries the tool label and the tool's managers wrote it (UID precondition) | delete a namespace named `minio` and everything in it | the name is fixed. Who created a namespace cannot be proven from its metadata (a managedFields entry's time is the manager's last write), so this is the user's informed choice: the dialog says it deletes everything in the namespace, including what the tool did not create, and lists the Deployments and PVCs there that the tool did not create |

The copy Job's pod: `runAsNonRoot`, `allowPrivilegeEscalation: false`, all capabilities dropped, seccomp `RuntimeDefault`, no UID (restricted-v2 assigns one), `automountServiceAccountToken: false`, CPU and memory requests and limits, `backoffLimit: 0`, `activeDeadlineSeconds: 3600`, `ttlSecondsAfterFinished: 86400`. The S3 keys reach it only through `secretKeyRef` to `seaweedfs/seaweedfs-s3-credentials`, never in its command, arguments or the operation's output. It needs no NetworkPolicy change: `seaweedfs-ingress` admits the S3 port from every pod, and so do the earlier storage's policies.

**Residual risk.** The SA can still create Secrets anywhere, create Subscriptions (OLM then installs whatever the CSV requests), delete CSVs and webhook configurations, and patch any Deployment (the module-operator restart uses this existing grant; the API cannot limit `patch` to Deployments it discovers at run time without listing every module operator by name). Treat its token like an administrator credential: anyone who can read it (or `exec` into the pod) can act as the SA. Namespace `admin` on the app's namespace is therefore effectively privileged.

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
- Mutation bodies are capped at 4096 bytes (`/api/pageview` at 256; the clean-up's list of at most 200 projects at 16 KiB), and the read timeout is 15 s.
- **Clean up** (Environment page, `POST /api/resources/cleanup/stream`) is one operation: one cluster lock and one rate-limit slot for the whole run, the same cluster-admin gate and 15-minute deadline as every mutation. It needs no new RBAC: each part runs the part's own teardown (pipeline servers, then the S3 storage, then MLflow) with its permissions and guards, so it deletes only what this tool created, never a storage a pipeline server still uses, and namespace `seaweedfs` only when asked and only when this tool created it. Project names are validated (DNS-1123, no duplicates) before the lock is used; the first part that does not succeed stops the run.
- **Feature prerequisites** (Environment page): the status reads the DataScienceCluster's `spec.components` / `status.components` management state with the `get`/`list` on `datascienceclusters` that `deploy/template.yaml` already grants (the Components page uses it), and setup refuses on the same reading. No new RBAC.
- Images must match `quay.io/rhoai/` or `registry.redhat.io/rhoai/`. A custom build to install accepts only `quay.io/rhoai/rhoai-fbc-fragment` with a tag and/or digest. PR numbers, DSC names, channels and namespaces are validated before use. The static file server is protected against path traversal.
- `/api/pageview` stores only aggregate counters, with labels limited to known page/feature names. No user identity is stored.

## Credentials

- The pull-secret token is never logged, returned, or written to the activity log. Only the `quay.io/rhoai` entry of `kube-system/additional-pull-secret` is replaced; other registries are kept.
- The Quay token is derived from the cluster pull secret on the server and never sent to the browser.
- The S3 storage's access key and secret key are random per install (or set with `SEAWEEDFS_ACCESS_KEY_ID` and `SEAWEEDFS_SECRET_ACCESS_KEY` on a first setup) and stored in `seaweedfs/seaweedfs-s3-credentials`. The secret key is also the SeaweedFS admin UI password; it reaches the pod through the environment (`WEED_ADMIN_PASSWORD`), not the command line.
- **The S3 keys** (Environment page, Pipelines tab, S3 storage: the access key and the secret key, which is also the admin UI password, shown or copied on demand, and "Copy as environment variables") come from `GET /api/resources/minio/admin-password`. It needs no new RBAC: the ServiceAccount already has `get` on Secret `seaweedfs-s3-credentials` (by `resourceNames`) for setup and Migrate storage. The handler reads the Secret only on that request, never in the status; it answers only a user who passes the cluster-admin check (such a user can read the Secret with their own token, so nothing is revealed that they could not read), only for storage this tool created (tool-labelled namespace, Secret written by the tool), with `Cache-Control: no-store`. The log line names the user, never the value. It takes no lock and changes nothing. The gitops profile has no Environment page, so the route answers 403 `profile_gitops` there, before any read (`deploy/rbac-gitops.yaml` grants no Secret read).
- `GITHUB_TOKEN` (optional) is sent only to `api.github.com`. Use a read-only token with public-repository access.

## Local development (`DEV_MODE=true`)

- Used by `dev.sh`. With no mounted SA token, every request runs with your own `oc` token (`DEV_TOKEN`) and **there is no cluster-admin gate**.
- The server binds `127.0.0.1` by default. A non-loopback `BIND_ADDRESS` exits at startup unless `DEV_ALLOW_REMOTE=true` (not recommended: anyone who can reach it acts with your token).
- Requests whose `Host` or `Origin` is not this machine on the API port or `DEV_FRONTEND_PORT` (default 9000) are refused. This blocks DNS rebinding.
- API server TLS is verified, in this order: `KUBE_CA_FILE` if set (an unusable file fails closed), the in-pod SA CA, then the system trust store. Verification is skipped only with `DEV_MODE=true` **and** `DEV_INSECURE_TLS=true`, which logs a warning.
- DEV_MODE cannot weaken an in-cluster pod. The bypass requires that no SA token be mounted.

## Known limitations and residual risks

- **SA token.** See *Residual risk* above.
- **S3 storage (SeaweedFS).** The Environment page's S3 storage runs SeaweedFS 4.54 (`ghcr.io/chrislusf/seaweedfs`, Apache-2.0, pinned by digest), the object store upstream Kubeflow Pipelines uses by default. It replaced the bundled MinIO, whose open-source line ended (advisories such as GHSA-hv4r-mvr4-25vw are fixed only in the commercial AIStor). SeaweedFS is not supported by Red Hat, publishes advisories often (bump the digest when one applies), and `weed mini` opens many listeners: master, volume and filer HTTP/gRPC, Iceberg and Lance, plus the admin's worker gRPC on loopback, and the master and filer HTTP APIs need no authentication. Mitigations:
  - NetworkPolicy `seaweedfs-ingress`: only the S3 port 8333 from pods in any namespace and the admin UI port 12535 only from the router; every other port is unreachable from the pod network. The admin's compatibility listener on the old port 23646 is turned off (`-admin.port.legacy=0`);
  - no Route to the S3 API;
  - credentials random per install; the admin UI requires a login (user `admin`, password from the Secret);
  - the admin password is passed as an environment variable, not an argument, so `ps` does not show it. Anyone who can `oc exec` into the pod or read `seaweedfs/seaweedfs-s3-credentials` can still read it.
  - logs: at startup SeaweedFS logs the S3 access key ID once ("Added admin identity from AWS environment variables: name=…, accessKey=…", `weed/s3api/auth_credentials.go:611` at 4.54). The access key ID is an identifier, not a secret, like an AWS access key ID; the secret key and the admin password are not logged (checked live on 4.48; the logging code is unchanged in 4.54). The pinned version could avoid that line only by loading the identity from an `-s3.config` JSON file (identities are then logged at verbosity 2 only), which would put the secret key into a second, tool-generated Secret; that is not worth it for an identifier.

  Any pod in the cluster that knows the access key can still reach the S3 API. The storage is meant for test data only. Storage an earlier version left in namespace `minio` (SeaweedFS behind the MinIO-era names, or MinIO itself) keeps running, with its own policies, until it is migrated or torn down.
- **Restarts.** A running operation delays a pod restart by up to ~17 minutes (see [README: cluster safety](README.md#how-the-tool-keeps-your-cluster-safe)). A SIGKILL or node loss still interrupts it. There is no durable job queue; the next start reports the interrupted operation. A replacement pod that starts while the old one drains refuses changes until the old operation ends (lease in the operation ConfigMap, no new RBAC).
- **Mirror visibility.** The GitHub mirror contains team-internal links (Bitwarden collection, Slack channels, the IPA realm and AWS SAML alias). They are access-controlled, but internal. Decide with the team whether the mirror should be private.
