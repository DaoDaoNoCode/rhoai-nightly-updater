# OpenShift Integration Knowledge Base

This document captures every OpenShift-specific behavior that affects the RHOAI Nightly Updater. Most entries were learned from incidents. Values below were checked against the code and `deploy/template.yaml`.

## 1. Route Timeout (Default 30s)

**What:** OpenShift Routes have a default HAProxy timeout of 30 seconds. Any HTTP response that takes longer is terminated with a 504.

**Impact:** SSE streams from mutation endpoints (Update, Reinstall, Refresh) can run for up to 15 minutes. SSE handlers replace the ordinary 180s response deadline with a short per-write deadline; accepted operations have an independent 15-minute lifetime. The default 30s Route timeout would kill the SSE connection mid-stream. The Route must support long-lived connections for SSE.

**Fix:** `haproxy.router.openshift.io/timeout: 960s` annotation on the Route. The oauth-proxy upstream timeout is also 960s, exceeding the operation deadline.

**Note:** The `haproxy.router.openshift.io/timeout` annotation may need to be explicitly set for SSE to work reliably. Without it, HAProxy uses the global default (30s), which is far too short for streaming operations.

**Test:** After deploy, verify: `oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}'` returns `960s`.

## 2. OAuth Proxy Token Scopes

**What:** SA-based OAuthClients in OpenShift are HARDCODED to `user:info user:check-access` scopes. This is set in oauth-proxy's `LoadDefaults()` function — NOT configurable via `--scope` flag for SA clients.

SA-based OAuthClients can ONLY request:
- `user:info` — read user identity
- `user:check-access` — perform access reviews (the app posts an `authorization.openshift.io` SubjectAccessReview with `scopes: []`, which evaluates the user's full RBAC)
- `role:<role>:<namespace>` — scoped role access

They CANNOT request: `user:full`, `user:list-projects`, `user:list-scoped-projects`.

**Impact:** The user's OAuth token CANNOT:
- List namespaces or projects
- Create/delete resources
- Make any K8s API calls except SSAR

All cluster operations MUST use the ServiceAccount token.

**Architecture:** oauth-proxy's `--openshift-sar` check (`list pods` in `redhat-ods-operator`) gates sign-in. For every change, the app runs a cluster-admin SAR with the user's token. If it passes, the SA does the work. See [SECURITY.md](../SECURITY.md).

**Cookie behavior:** `--cookie-expire=23h`, below the default OAuth access-token lifetime of 24h, so a session ends before the token it carries stops working. OpenShift OAuth tokens are not refreshable. The cookie secret is read from Secret `<APP_NAME>-proxy`.

**Source:** https://github.com/openshift/oauth-proxy/blob/master/providers/openshift/provider.go (LoadDefaults function)

## 3. Image Tags and Digests

**What:** A mutable tag such as `:latest` can move between a check and the kubelet's pull, and a node may run a cached image.

**Fix:** `make deploy`/`make upgrade` resolve the tag to a digest (`scripts/resolve-image.sh`) and apply `IMAGE@sha256:...`. A new digest or template rolls the pods out by itself, so no `oc rollout restart` is needed. CI publishes the immutable 8-character commit tag on every `main` pipeline, and moves `:latest` only from the tip of `main`.

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

**What:** The SA token is mounted at `/var/run/secrets/kubernetes.io/serviceaccount/token`. On OpenShift 4.x, this is a projected volume token with automatic rotation (default 1 hour, auto-refreshed).

**Impact:** The token file changes on disk periodically. Reading it on every request (as we do in dev mode via `getClusterToken()`) is correct — caching it for too long risks using an expired token.

**Current behavior:** Cached for 5 minutes with auto-refresh.

## 6. CatalogSource Readiness

**What:** After applying a CatalogSource with a new image, OLM must:
1. Detect the change
2. Pull the new catalog image
3. Start the gRPC pod
4. Index the package content

This typically takes 10-30 seconds depending on image size and pull speed.

**Impact:** If we mutate the Subscription before the CatalogSource is READY, OLM resolves against stale content or fails.

**Fix:** CatalogSource readiness is a step in the SSE pipeline. The pipeline polls the CatalogSource state and emits SSE events to show live progress.

**Note:** Check the source code for current poll interval and timeout values. Tests override these to minimal values for speed.

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

**What:** The DataSciencePipelinesApplication CRD is registered at different API versions depending on the RHOAI version:
- RHOAI 2.x: `datasciencepipelinesapplications.opendatahub.io/v1alpha1`
- RHOAI 3.x: `datasciencepipelinesapplications.opendatahub.io/v1`

**Impact:** Using the wrong version returns 404. Must check the cluster's actual CRD version.

**Current:** Hardcoded to `v1` (RHOAI 3.x). If the tool is used on RHOAI 2.x clusters, this will break.

## 10. MLflow CRD Scope

**What:** The MLflow CRD (`mlflow.opendatahub.io/v1`) is cluster-scoped, not namespace-scoped. Creating it with a namespace in the metadata or using `namespacedPath()` returns 404.

**Fix:** Use `clusterPath()` for all MLflow CR operations.

## 11. Rate Limiter + Dry Run

**What:** The rate limiter uses `username:path` as the key. If dry run and real update use the same endpoint path, the dry run consumes the rate limit slot.

**Fix:** The dry-run handler sets the **response** header `X-Skip-Rate-Limit: true`, and `withMutationAuth` then gives the slot back. Rejected requests (4xx/5xx, including 409 busy) also give it back.

## 12. SSE Streaming Through Route + OAuth Proxy

**What:** SSE (Server-Sent Events) streams progress from mutation endpoints (Update, Reinstall, Refresh) through the OpenShift Route and oauth-proxy to the browser.

**How it works:** SSE uses standard HTTP -- not WebSocket. The server sets:
- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`

These headers are set by the SSEWriter before the first event is flushed. The response is a long-lived HTTP/1.1 connection that the server writes to incrementally.

**oauth-proxy compatibility:** SSE works through oauth-proxy without any special configuration. Unlike WebSocket (which requires an `Upgrade` header and Route annotation `haproxy.router.openshift.io/hsts_header`), SSE is just a normal HTTP response with a streaming body. oauth-proxy passes the response through transparently because:
1. The initial request is a standard authenticated POST/GET
2. oauth-proxy validates the token and forwards to the backend
3. The backend sends `Content-Type: text/event-stream` and starts writing events
4. oauth-proxy and HAProxy relay the chunked response without buffering

**No special Route annotation needed for SSE protocol** (unlike WebSocket which requires `haproxy.router.openshift.io/haproxy.config: websocket`). The only Route annotation needed is the timeout annotation (Section 1) to prevent HAProxy from killing long-running streams.

**Flushing:** The SSEWriter calls `http.Flusher.Flush()` after each event to ensure it is sent immediately rather than buffered. Go's `http.ResponseWriter` supports `Flusher` when not wrapped in buffering middleware.

## 13. Configurable Timing Constants

**What:** Operation timing is controlled by package-level variables. These are `var` (not `const`) so tests can override them for speed.

**Examples of timing constants:**
- CatalogSource readiness timeouts and poll intervals
- Resource propagation wait periods
- Retry backoff durations

**Test overrides:** Tests set these to minimal values to avoid slow test suites.

**Why `var` not `const`:** Go `const` only supports primitive types and cannot hold `time.Duration` values that need to be overridden. Using package-level `var` is the standard Go pattern for testable timing constants.

**Note:** Check the source code for current timeout values. Production values are designed for real cluster latencies; test values are designed for speed.
