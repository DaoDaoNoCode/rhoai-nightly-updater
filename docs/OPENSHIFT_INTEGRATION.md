# OpenShift Integration Knowledge Base

This document captures every OpenShift-specific behavior that affects the RHOAI Nightly Updater. Most entries were learned from incidents. Values below were checked against the code and `deploy/template.yaml`.

## 1. Route Timeout (Default 30s)

**What:** OpenShift Routes have a default HAProxy timeout of 30 seconds. Any HTTP response that takes longer is terminated with a 504.

**Impact:** SSE streams from mutation endpoints (Update, Install a different build, Re-deploy) can run for up to 15 minutes. SSE handlers replace the ordinary 180s response deadline with a short per-write deadline; accepted operations have an independent 15-minute lifetime. The default 30s Route timeout would kill the SSE connection mid-stream. The Route must support long-lived connections for SSE.

**Fix:** `haproxy.router.openshift.io/timeout: 960s` annotation on the Route. The oauth-proxy upstream timeout is also 960s, exceeding the operation deadline.

**Note:** The `haproxy.router.openshift.io/timeout` annotation may need to be explicitly set for SSE to work reliably. Without it, HAProxy uses the global default (30s), which is far too short for streaming operations.

**Test:** After deploy, verify: `oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}'` returns `960s`.

## 2. OAuth Proxy Token Scopes

**What:** When a ServiceAccount is the OAuth client (`--openshift-service-account`, what this tool did before 4.0.0), the OAuth server allows it only the scopes `user:info`, `user:check-access`, `user:list-scoped-projects`, `user:list-projects` and `role:<any role>:<the SA's own namespace>[:!]` (library-go `pkg/oauth/oauthserviceaccountclient/oauthclientregistry.go` `getScopeRestrictionsFor`). oauth-proxy passes `--scope` on unchanged (its `LoadDefaults` only supplies the default, `user:info user:check-access`, when `--scope` is empty); the OAuth server rejects `user:full` after the user has logged in (oauth-server `pkg/oauth/handlers/grant.go`) and redirects to `/oauth/callback?error=access_denied&error_description=scope+denied%3A+user%3Afull`. oauth-proxy then shows **"403 Permission Denied / access_denied"** and logs `ErrorPage 403 Permission Denied access_denied`; the reason is only in the callback URL's `error_description`.

**Fix (4.0.0):** a real cluster-scoped OAuthClient `<APP_NAME>-<NAMESPACE>` (as RHOAI does for its `data-science` client): `grantMethod: auto` (no "Authorize Access" page), one redirect URI `https://<Route host>/oauth/callback` (the OAuth server matches scheme and host exactly), a random `secret` from Secret `<APP_NAME>-oauth-client`, `scopeRestrictions` of exactly `user:full` (what oauth-proxy asks for), and labels naming the install's app and namespace. No ownerReference: one to the install's Namespace did not make the garbage collector delete the client with it (live-checked), so `install.sh uninstall` removes it, also after the namespace was deleted by hand (SECURITY.md, The OAuthClient). oauth-proxy runs with `--client-id=<APP_NAME>-<NAMESPACE>`, `--client-secret-file=/etc/proxy/oauth/client-secret` (read raw: no trailing newline) and `--scope=user:full`. `install.sh` (and the chart's hooks) create both; see [SECURITY.md: The OAuthClient](../SECURITY.md#the-oauthclient-400).

**Architecture:** oauth-proxy's `--openshift-sar` check (`list pods` in `redhat-ods-operator`) gates sign-in. For every change, the app runs a cluster-admin SAR with the user's token, checks that the sign-in can carry the change (its own `useroauthaccesstokens` object: scope and expiry), and makes the change **with the user's token**. The ServiceAccount reads, keeps the app's records and runs the MaaS prune job. See [SECURITY.md](../SECURITY.md).

**Cookie behavior:** `--cookie-expire=23h`, below the default OAuth access-token lifetime of 24h, so a session ends before the token it carries stops working. OpenShift issues no refresh tokens. **Do not set `--cookie-refresh`**: openshift/oauth-proxy validates a refresh with the token as an `access_token` query parameter (`providers/internal_util.go`), which the API server ignores, so the request runs as `system:anonymous`, the refresh fails and the user is signed out (openshift/oauth-proxy issue #339), and a transport error logs the URL with the token. The cookie secret is read from Secret `<APP_NAME>-proxy` and must be 16, 24 or 32 bytes.

**Diagnosis:** a sign-in that ends on oauth-proxy's "403 Permission Denied / access_denied" page with `?error_description=scope+denied` in the address bar means the OAuthClient restricts the scope to something other than `user:full`, or oauth-proxy is still the ServiceAccount client; a JSON `{"error":"invalid_request"}` or `unauthorized_client` from the OAuth server means the OAuthClient is missing or its redirect URI does not match the Route ([RUNBOOK](../RUNBOOK.md#sign-in-and-the-oauthclient)).

**Source:** openshift/oauth-proxy `providers/openshift/provider.go`, `options.go`, `oauthproxy.go`; library-go `oauthclientregistry.go`; oauth-server `grant.go`; oauth-apiserver `pkg/tokenvalidation/tokenauthenticator.go` (the `sha256~` object name).

## 3. Image Tags and Digests

**What:** A mutable tag such as `:latest` can move between a check and the kubelet's pull, and a node may run a cached image.

**Fix:** `make deploy`/`make upgrade` (both `scripts/install.sh`, also attached to every release) resolve the tag to a digest and apply `IMAGE@sha256:...`. A new digest or template rolls the pods out by itself, so no `oc rollout restart` is needed. CI publishes the immutable 8-character commit tag and `:main` on every `main` pipeline; `:latest` only follows releases within its major version.

**Test:** `oc get deploy rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.spec.template.spec.containers[0].image}'` shows `@sha256:`.

## 4. WriteTimeout Budget (SSE Streaming)

**What:** SSE endpoints stream progress events incrementally to the client. The full timeout chain is: frontend EventSource → Route HAProxy → Go http.Server WriteTimeout → SSE pipeline duration. ALL must be >= the longest operation.

**Current values:**
- The server `WriteTimeout` is 180 s for ordinary requests.
- An accepted mutation clears its write deadline (`SetWriteDeadline(time.Time{})`) and runs on its own 15-minute context.
- The Route timeout and oauth-proxy `--upstream-timeout` are both 960 s; `--upstream-flush=200ms` streams the events.
- If a stream is cut, the operation continues. The page reattaches through `GET /api/operation`.

**Shutdown budget** (pkg/cluster/operator_recovery.go; `budget_test.go` checks it against the template):
- drain 980 s (15 min deadline + 60 s restore + 20 s bookkeeping);
- marker flush 10 s;
- HTTP shutdown 20 s;
- 10 s margin.

That totals 1020 s, which is `terminationGracePeriodSeconds`.

## 5. ServiceAccount Token

**What:** The ServiceAccount token is a projected volume at `/var/run/secrets/kubernetes.io/serviceaccount/token`. The kubelet rewrites the file before the token expires (once 80% of its lifetime has passed), so a reader must read the file again rather than keep the first token.

**Current behavior:** `getClusterToken()` (`pkg/api/handlers.go`) reads the file and keeps the token for 5 minutes (`tokenCacheTTL`), then reads it again: far inside the token's lifetime, and one file read per 5 minutes instead of one per request. The app's own reads, its records and the MaaS prune job use this token; changes run with the signed-in user's token (Section 2).

**Dev mode:** with `DEV_MODE=true` and no mounted token (`./dev.sh` on a workstation), the ServiceAccount token is `DEV_TOKEN` (your `oc whoami -t`), cached the same way; the same variable also stands in for the user's token, since there is no oauth-proxy.

## 6. CatalogSource Readiness

**What:** After applying a CatalogSource with a new image, OLM must:
1. Detect the change
2. Pull the new catalog image
3. Start the gRPC pod
4. Index the package content

This typically takes 10-30 seconds depending on image size and pull speed.

**Impact:** If we mutate the Subscription before the CatalogSource is READY, OLM resolves against stale content or fails.

**Fix:** CatalogSource readiness is a step in the SSE pipeline. The pipeline polls the CatalogSource state and emits SSE events to show live progress.

**Values:** `CatalogReadyTimeout` (120 s) and `CatalogPollInterval` (5 s) in `pkg/cluster/operations.go`.

## 7. OLM Version Pinning

**What:** When a CatalogSource image changes but the CSV version string stays the same (e.g., the same `v3.5.0-ea.2` in both catalogs), OLM does NOT create a new InstallPlan. It considers the operator already at the desired version.

**Impact:** Updating only the CatalogSource image is not enough.

**Fix:** Update runs, in order:
1. Delete the Subscription (the operator keeps running).
2. Replace the CatalogSource and wait for READY and the PackageManifest.
3. Delete the old InstallPlan and CSV.
4. Create the Subscription once.

The exact sequences are in [CLUSTER_CHANGES.md](CLUSTER_CHANGES.md#4-operator-lifecycle-status-page).

## 8. NetworkPolicy

**What:** OpenShift Routes go through the ingress controller pods in the `openshift-ingress` namespace. If the NetworkPolicy blocks ingress from this namespace, the Route returns 503.

**Current config:**
- Port 8443 (oauth-proxy): allow from `network.openshift.io/policy-group: ingress`
- Port 9090 (probes and `/metrics` only; the API on 8080 is bound to `127.0.0.1`): allow from `openshift-monitoring` and `openshift-user-workload-monitoring`

## 9. DSPA API Version

**What:** The tool reads and writes DataSciencePipelinesApplications at `datasciencepipelinesapplications.opendatahub.io/v1` only (`dspaAPIGroup` in `pkg/cluster/pipeline_server.go`), the version the RHOAI 3.x data-science-pipelines-operator serves.

**Fields:** before writing a pipeline server, Set up reads the served `v1` schema of the installed CRD (`dspaSpecSchema`) and sets the optional settings (managed pipelines, the MLflow integration) only when that schema declares them; when the CRD cannot be read, it says so in the log and leaves them to the operator's defaults.

## 10. MLflow CRD Scope

**What:** The MLflow CRD (`mlflow.opendatahub.io/v1`) is cluster-scoped, not namespace-scoped. Creating it with a namespace in the metadata or using `namespacedPath()` returns 404.

**Fix:** Use `clusterPath()` for all MLflow CR operations.

## 11. Rate Limiter + Dry Run

**What:** The rate limiter uses `username:path` as the key. If dry run and real update use the same endpoint path, the dry run consumes the rate limit slot.

**Fix:** The dry-run handler sets the **response** header `X-Skip-Rate-Limit: true`, and `withMutationAuth` then gives the slot back. Rejected requests (4xx/5xx, including 409 busy) also give it back.

A refused request gets HTTP 429 with errorCode `user_rate_limited` (not `rate_limited`, which means the OpenShift API is throttling) and `Retry-After` set to the seconds left in the window; the message names the tool's limit and that wait.

## 12. SSE Streaming Through Route + OAuth Proxy

**What:** SSE (Server-Sent Events) streams progress from mutation endpoints (Update, Install a different build, Re-deploy) through the OpenShift Route and oauth-proxy to the browser.

**How it works:** SSE uses standard HTTP -- not WebSocket. The server sets:
- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`

These headers are set by the SSEWriter before the first event is flushed. The response is a long-lived HTTP/1.1 connection that the server writes to incrementally.

**oauth-proxy compatibility:** SSE is a normal HTTP response with a streaming body, so it needs no protocol upgrade:
1. The request is a standard authenticated POST/GET; oauth-proxy validates the session and forwards it to the backend.
2. The backend sends `Content-Type: text/event-stream` and starts writing events.
3. oauth-proxy relays them with the template's `--upstream-flush=200ms` (it flushes the relayed response every 200 ms, so each event reaches the browser promptly) and `--upstream-timeout=960s` (Section 4).
4. HAProxy relays the response as it arrives.

**Route annotation:** the only one SSE needs is the timeout annotation (Section 1), so HAProxy does not close a long-running stream.

**Flushing:** The SSEWriter calls `http.Flusher.Flush()` after each event to ensure it is sent immediately rather than buffered. Go's `http.ResponseWriter` supports `Flusher` when not wrapped in buffering middleware.

## 13. Timing in Tests

**What:** Every wait in `pkg/cluster` (polls, retry backoffs, grace periods, and the deadlines that bound them) goes through the package clock `clk` (`pkg/cluster/clock.go`, interface in `pkg/clock`). Production uses the wall clock.

**Tests:** `TestMain` (`pkg/cluster/testmain_test.go`) replaces `clk` with `clocktest.NewAutoFake()`, a fake clock that moves forward whenever the code sleeps, so no test waits in real time. Caches tell their entries' ages by a clock of their own, which a test replaces with a `clocktest.Fake` and moves with `Advance` (`fakeCacheClock`). The timing values are package-level `var`s (for example `CatalogReadyTimeout`, `CatalogPollInterval`, `SubRetryBackoffs` in `operations.go`): `TestMain` shortens them so a fake-clock loop runs few iterations, and a test may set one to choose which deadline is reached first.

**Not on the clock:** timestamps that are recorded or compared with the cluster's own (activity entries, object ages) come from the `time` package.

**Production values:** read them in the source (`operations.go`, `PrerequisiteReadyTimeout` in `feature_setup.go`, and the other `*Timeout`/`*Poll*` variables); they are sized for real cluster latencies.
