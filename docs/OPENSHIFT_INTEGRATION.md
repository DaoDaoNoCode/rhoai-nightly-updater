# OpenShift Integration Knowledge Base

This document captures every OpenShift-specific behavior that affects the RHOAI Nightly Updater. Each entry was learned from a production incident.

## 1. Route Timeout (Default 30s)

**What:** OpenShift Routes have a default HAProxy timeout of 30 seconds. Any HTTP response that takes longer is terminated with a 504.

**Impact:** SSE streams from mutation endpoints (Update, Reinstall, Refresh) can run up to 3 minutes (WriteTimeout set to 180s). The default 30s Route timeout would kill the SSE connection mid-stream. The Route must support long-lived connections for SSE.

**Fix:** `haproxy.router.openshift.io/timeout: 180s` annotation on the Route. This must be >= the Go server's WriteTimeout (180s) to prevent HAProxy from terminating SSE streams before the server does.

**Note:** The `haproxy.router.openshift.io/timeout` annotation may need to be explicitly set for SSE to work reliably. Without it, HAProxy uses the global default (30s), which is far too short for streaming operations.

**Test:** After deploy, verify: `oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}'` returns `180s`.

## 2. OAuth Proxy Token Scopes

**What:** SA-based OAuthClients in OpenShift are HARDCODED to `user:info user:check-access` scopes. This is set in oauth-proxy's `LoadDefaults()` function — NOT configurable via `--scope` flag for SA clients.

SA-based OAuthClients can ONLY request:
- `user:info` — read user identity
- `user:check-access` — perform SelfSubjectAccessReview
- `role:<role>:<namespace>` — scoped role access

They CANNOT request: `user:full`, `user:list-projects`, `user:list-scoped-projects`.

**Impact:** The user's OAuth token CANNOT:
- List namespaces or projects
- Create/delete resources
- Make any K8s API calls except SSAR

All cluster operations MUST use the ServiceAccount token.

**Architecture:** oauth-proxy's `--openshift-sar` check is the authorization gate. The SAR check uses the user's token (which has `user:check-access` scope — sufficient for SAR). If a user passes it, the SA does all operations.

**Cookie behavior:** Cookie expiry defaults to 168h. The underlying OAuth token expires in 24h (OpenShift default). Cookie refresh re-validates the token but does not obtain a new one — OpenShift OAuth tokens are non-refreshable.

**Note:** Check oauth-proxy configuration for current timeout values.

**Source:** https://github.com/openshift/oauth-proxy/blob/master/providers/openshift/provider.go (LoadDefaults function)

**Test:** After deploy, verify the SSAR is NOT called: `oc logs -l app=rhoai-nightly-updater -c app --tail=20 | grep -i 'SSAR\|permission check'` should return nothing.

## 3. Image Pull Policy and Caching

**What:** When using the `latest` tag, Kubernetes may serve a cached image even after pushing a new one. The `imagePullPolicy: Always` setting forces a pull, but the node's container runtime may still serve a cached layer.

**Impact:** After `podman push`, a `oc rollout restart` is needed to guarantee the new image runs.

**Fix:** Always do `oc rollout restart` after push. The Makefile's deploy target should include this.

**Test:** After deploy, verify: `oc get pods -o jsonpath='{.items[0].status.containerStatuses[0].imageID}'` matches the pushed digest.

## 4. WriteTimeout Budget (SSE Streaming)

**What:** SSE endpoints stream progress events incrementally to the client. The full timeout chain is: frontend EventSource → Route HAProxy → Go http.Server WriteTimeout → SSE pipeline duration. ALL must be >= the longest operation.

**How SSE changes the budget:** Unlike the old request/response model (where the entire operation had to complete within WriteTimeout), SSE streams partial progress as it goes. Each SSE event resets the effective "time since last write," but Go's `WriteTimeout` is measured from the start of the response, not the last write. Therefore WriteTimeout must still cover the total wall-clock time of the longest operation.

**Timeout hierarchy:**
- Frontend EventSource timeout
- Route HAProxy timeout (configured via Route annotation)
- Go WriteTimeout (configured in server)
- SSE pipeline duration (varies by operation: Update, Reinstall, Refresh)

**Rule:** Each layer must be >= the one below it. The Route timeout must be >= the Go WriteTimeout which must be >= the worst-case SSE stream duration.

**Note:** Check the source code and Route annotations for current timeout values.

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

**What:** When a CatalogSource image changes but the CSV version string stays the same (e.g., same `v3.5.0-ea.2` in both old and new catalog), OLM does NOT create a new InstallPlan. It considers the operator already at the desired version.

**Impact:** Simply updating the CatalogSource image is not enough — must delete the CSV and recreate the Subscription to force a fresh InstallPlan.

**Fix:** The Update() flow always does: apply CatalogSource → wait READY → delete CSV → delete Subscription → wait → recreate Subscription.

## 8. NetworkPolicy

**What:** OpenShift Routes go through the ingress controller pods in the `openshift-ingress` namespace. If the NetworkPolicy blocks ingress from this namespace, the Route returns 503.

**Current config:**
- Port 8443 (oauth-proxy): allow from `network.openshift.io/policy-group: ingress`
- Port 8080 (metrics): allow from `openshift-monitoring` and `openshift-user-workload-monitoring`

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

**Fix:** Dry run requests set `X-Skip-Rate-Limit` header, and `withMutationAuth` checks this header to skip `recordMutation`.

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
