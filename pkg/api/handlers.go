package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var imageRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]+(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
var namespaceRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var channelRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// customFBCImageRegex accepts any build of the RHOAI FBC repository (by tag
// and/or digest). An FBC is installed with Automatic approval into an
// AllNamespaces OperatorGroup, so it must not come from an arbitrary
// registry or Quay organization.
var customFBCImageRegex = regexp.MustCompile(`^quay\.io/rhoai/rhoai-fbc-fragment(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
var dscNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func validateReinstallRequest(req *types.ReinstallRequest) error {
	if req.TargetType == "" {
		req.TargetType = "stable"
	}
	req.Image = strings.TrimSpace(req.Image)
	req.Channel = strings.TrimSpace(req.Channel)
	if req.TargetType != "stable" && req.TargetType != "nightly" && req.TargetType != "custom" {
		return fmt.Errorf("invalid targetType: %s (must be stable, nightly, or custom)", req.TargetType)
	}
	if req.TargetType != "stable" {
		if req.Image == "" {
			return fmt.Errorf("image is required for nightly or custom reinstall")
		}
		if req.TargetType == "custom" {
			match := customFBCImageRegex.FindStringSubmatch(req.Image)
			if match == nil || (match[1] == "" && match[2] == "") {
				return fmt.Errorf("provide a quay.io/rhoai/rhoai-fbc-fragment image with a tag or a full SHA256 digest")
			}
		} else if !imageRegex.MatchString(req.Image) || !isAllowedImage(req.Image) {
			return fmt.Errorf("provide a valid FBC image from quay.io/rhoai/ or registry.redhat.io/rhoai/")
		}
	}
	if req.Channel != "" && !channelRegex.MatchString(req.Channel) {
		return fmt.Errorf("invalid channel name: %s", req.Channel)
	}
	return nil
}

// allowedImagePrefixes is the whitelist of registries from which images may be pulled.
var allowedImagePrefixes = []string{
	"quay.io/rhoai/",
	"registry.redhat.io/rhoai/",
}

// forwardSteps returns an emitter that streams pipeline steps to the client.
// A failed write means the client went away; the operation keeps running
// (it is detached from the request) and SSEWriter still records the step
// for GET /api/operation, so the error is only logged.
func forwardSteps(sseWriter *SSEWriter) func(cluster.UpdateStepEvent) {
	return func(event cluster.UpdateStepEvent) {
		if err := sseWriter.SendStep(UpdateStep{
			Step:      event.Step,
			Status:    event.Status,
			Message:   event.Message,
			Detail:    event.Detail,
			ElapsedMs: time.Since(sseWriter.StartTime()).Milliseconds(),
			ErrorCode: event.ErrorCode,
		}); err != nil {
			slog.Debug("sse: step not delivered; the operation continues", "step", event.Step, "error", err)
		}
	}
}

// sseHeartbeat sends periodic SSE comments to keep the connection alive
// through proxies with idle timeouts. Stops when done is closed.
func sseHeartbeat(w *SSEWriter, done <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			w.SendHeartbeat()
		}
	}
}

// isAllowedImage checks whether the image reference starts with an allowed registry prefix.
func isAllowedImage(image string) bool {
	lower := strings.ToLower(image)
	for _, prefix := range allowedImagePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// rateLimiter provides simple per-user rate limiting for mutation endpoints:
// one accepted request per user and endpoint per window. This is protection
// against double-clicks and duplicate tabs, not a DDoS defense. A slot is
// taken atomically when the request is accepted (so concurrent duplicates
// are refused while the first one runs) and given back when the request is
// rejected or asks not to count.
type rateLimiter struct {
	mu     sync.Mutex
	users  map[string]time.Time // key -> time the slot was taken
	window time.Duration
}

func newRateLimiter(window time.Duration) *rateLimiter {
	return &rateLimiter{users: map[string]time.Time{}, window: window}
}

// mutationLimiter is the package-level rate limiter for mutation endpoints.
var mutationLimiter = newRateLimiter(30 * time.Second)

// tryAcquire takes the slot for key unless it was taken within the window.
// It returns the slot's timestamp for release.
func (rl *rateLimiter) tryAcquire(key string) (time.Time, bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if last, ok := rl.users[key]; ok && now.Sub(last) < rl.window {
		return time.Time{}, false
	}
	if len(rl.users) > 256 {
		for k, t := range rl.users {
			if now.Sub(t) >= rl.window {
				delete(rl.users, k)
			}
		}
	}
	rl.users[key] = now
	return now, true
}

// release gives back a slot taken at the given time (and not a newer one).
func (rl *rateLimiter) release(key string, takenAt time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if t, ok := rl.users[key]; ok && t.Equal(takenAt) {
		delete(rl.users, key)
	}
}

// withMutationAuth wraps a handler that performs a cluster mutation.
// It verifies the user's identity and cluster-admin permission with the
// user's own token before anything is done with the ServiceAccount token,
// applies a per-user rate limit, and gives accepted work a bounded lifetime
// independent of the browser.
func withMutationAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		username, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		allowed, permissionErr := mutationPermission(r.Context(), extractUserToken(r))
		if permissionErr != nil {
			writeAuthError(w, permissionErr, "Cannot verify mutation permissions. No changes were made.")
			return
		}
		if !allowed {
			writeError(w, readOnlyMessage, http.StatusForbidden, "forbidden")
			return
		}

		rateLimitKey := username + ":" + r.URL.Path
		slot, acquired := mutationLimiter.tryAcquire(rateLimitKey)
		if !acquired {
			slog.Warn("rate limited", "user", username, "path", r.URL.Path)
			w.Header().Set("Retry-After", "30")
			writeError(w, "Too many requests. Please wait 30 seconds before retrying.", http.StatusTooManyRequests, "rate_limited")
			return
		}

		if !mutations.begin() {
			mutationLimiter.release(rateLimitKey, slot)
			w.Header().Set("Retry-After", "30")
			writeError(w, "The updater is restarting. No changes were made; retry once it is back.", http.StatusServiceUnavailable, "shutting_down")
			return
		}
		defer mutations.end()

		// Accepted operations can outlast the server's 180s write timeout,
		// including the non-streaming endpoints; their own deadline applies.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

		opContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
		defer cancel()
		client := cluster.NewClientWithContext(opContext, clusterToken)
		client.SetUsername(username)

		sw := &statusWriter{ResponseWriter: w, path: r.URL.Path, user: username, client: client}
		completed := false
		defer func() {
			// Rejected requests (4xx/5xx status, including 409 busy),
			// requests that opt out (dry runs) and handlers that panicked
			// do not use up the user's slot.
			if !completed || sw.status >= 400 || sw.Header().Get("X-Skip-Rate-Limit") == "true" {
				mutationLimiter.release(rateLimitKey, slot)
			}
		}()
		fn(client, sw, r)
		completed = true
	}
}

// statusWriter records the response status of a mutation and carries the
// request's identity to lockCluster, which publishes the running operation.
type statusWriter struct {
	http.ResponseWriter
	status int

	path   string
	user   string
	client *cluster.Client
	opID   string // set by lockCluster

	// The start of a JSON response body, from which the operation's
	// outcome is read; streams report it in their final event instead.
	body      []byte
	streaming bool
}

// maxCapturedResponse bounds the response bytes kept for the outcome.
const maxCapturedResponse = 16 << 10

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
		sw.streaming = strings.HasPrefix(sw.Header().Get("Content-Type"), "text/event-stream")
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
		sw.streaming = strings.HasPrefix(sw.Header().Get("Content-Type"), "text/event-stream")
	}
	if !sw.streaming && len(sw.body) < maxCapturedResponse {
		sw.body = append(sw.body, b[:min(len(b), maxCapturedResponse-len(sw.body))]...)
	}
	return sw.ResponseWriter.Write(b)
}

// Unwrap lets ResponseController reach streaming and deadline support on the
// underlying writer while the wrapper continues to track response status.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// writeError writes a JSON error response with an optional error code.
// The response always includes both "error" and "errorCode" fields for a
// consistent client-facing shape. When no explicit errorCode is provided,
// a default is derived from the HTTP status code.
func writeError(w http.ResponseWriter, msg string, code int, errorCode ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	ec := ""
	if len(errorCode) > 0 && errorCode[0] != "" {
		ec = errorCode[0]
	} else {
		ec = defaultErrorCode(code)
	}
	respMap := map[string]string{"error": msg, "errorCode": ec}
	resp, err := json.Marshal(respMap)
	if err != nil {
		w.Write([]byte(`{"error":"internal server error","errorCode":"internal"}`))
		return
	}
	w.Write(resp)
}

// defaultErrorCode returns a sensible errorCode string for the given HTTP status.
func defaultErrorCode(httpStatus int) string {
	switch httpStatus {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusUnprocessableEntity:
		return "unprocessable"
	default:
		return "internal"
	}
}

// writeJSON encodes v as JSON and writes it to w, logging any encoding error.
func writeJSON(w http.ResponseWriter, v interface{}, label string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("response encode error", "label", label, "error", err)
	}
}

// writeOperationResult writes a standard OperationResponse.
// On success it returns 200 with the result as JSON.
// On failure (result.Success == false) it returns 422 with the result as JSON.
func writeOperationResult(w http.ResponseWriter, result *types.OperationResponse, label string) {
	if !result.Success {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		if err := json.NewEncoder(w).Encode(result); err != nil {
			slog.Error("response encode error", "label", label, "error", err)
		}
		return
	}
	writeJSON(w, result, label)
}

// resolveUsername returns the request's user: the identity verified by
// withAuth/withMutationAuth when present, otherwise the oauth-proxy header.
func resolveUsername(r *http.Request) string {
	if u, ok := r.Context().Value(identityKey{}).(string); ok && u != "" {
		return u
	}
	if u := r.Header.Get("X-Forwarded-User"); u != "" {
		return u
	}
	if os.Getenv("DEV_MODE") == "true" {
		if u := os.Getenv("DEV_USER"); u != "" {
			return u
		}
	}
	return "unknown"
}

// extractUserToken returns the user's OAuth token for identity purposes.
//
// In the cluster only X-Forwarded-Access-Token counts. oauth-proxy sets it
// from the user's own session, which it got through its OAuth flow, and
// replaces any client-sent copy (setRequestHeader in openshift/oauth-proxy
// oauthproxy.go). Without --openshift-delegate-urls the proxy does not
// accept client bearer tokens at all, and with --pass-basic-auth (default
// true) it overwrites the Authorization header with Basic credentials. So a
// Bearer token would come from somewhere else; requestUser rejects it. A
// bearer token is accepted only in DEV_MODE, where there is no proxy.
func extractUserToken(r *http.Request) string {
	if token := r.Header.Get("X-Forwarded-Access-Token"); token != "" {
		return token
	}
	if os.Getenv("DEV_MODE") == "true" {
		if token, ok := bearerToken(r); ok {
			return token
		}
		if token := os.Getenv("DEV_TOKEN"); token != "" {
			return token
		}
	}
	return ""
}

// bearerToken returns the request's "Authorization: Bearer" token.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// cachedToken holds the SA token and the time it was read, so we avoid
// reading the token file on every single HTTP request.
var (
	cachedTokenMu    sync.Mutex
	cachedTokenValue string
	cachedTokenAt    time.Time
)

const tokenCacheTTL = 5 * time.Minute

// getClusterToken returns the token to use for k8s API calls.
// In-cluster: uses the ServiceAccount token (scoped RBAC), cached for 5 minutes.
// Dev mode: uses the DEV_TOKEN (only when SA token is not available).
func getClusterToken() string {
	cachedTokenMu.Lock()
	defer cachedTokenMu.Unlock()

	if cachedTokenValue != "" && time.Since(cachedTokenAt) < tokenCacheTTL {
		return cachedTokenValue
	}

	// Always prefer the in-cluster SA token
	data, err := os.ReadFile(serviceAccountTokenPath)
	if err == nil && len(data) > 0 {
		cachedTokenValue = string(data)
		cachedTokenAt = time.Now()
		return cachedTokenValue
	}
	// Fall back to DEV_TOKEN only in dev mode (not in-cluster)
	if os.Getenv("DEV_MODE") == "true" {
		token := os.Getenv("DEV_TOKEN")
		cachedTokenValue = token
		cachedTokenAt = time.Now()
		return token
	}
	return ""
}

// withAuth wraps a handler that requires authentication.
// It verifies the user's token with the API server (the user's own token,
// forwarded by oauth-proxy) and creates a Client using the ServiceAccount
// token for k8s API calls. A request without a token, or whose token the API
// server rejects, never reaches the handler.
func withAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		username, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		client := cluster.NewClientWithContext(r.Context(), clusterToken)
		client.SetUsername(username)
		fn(client, w, r)
	}
}

// HandleUserPermissions reports whether the logged-in user may mutate, using
// the same permission review that guards every mutation endpoint. A 503
// means the answer is unknown: clients must treat it as read-only.
var HandleUserPermissions = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	username := resolveUsername(r)
	allowed, err := mutationPermission(r.Context(), extractUserToken(r))
	if err != nil {
		writeAuthError(w, err, "Cannot verify mutation permissions")
		return
	}
	writeJSON(w, map[string]interface{}{
		"canMutate": allowed,
		"user":      username,
	}, "user-permissions")
})

// HandleHealth returns a fast liveness check.
// It verifies a cluster token is available (without making API calls) so
// kubelet liveness probes stay quick.
func HandleHealth(w http.ResponseWriter, r *http.Request) {
	token := getClusterToken()
	if token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status": "degraded",
			"reason": "cluster token unavailable",
		}); err != nil {
			slog.Error("response encode error", "label", "health", "error", err)
		}
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "version": Version, "commit": Commit}, "health")
}

// apiReachability caches the readiness probe's Kubernetes API check, so the
// probe (every 10s) does not call the API each time, and a short burst of
// throttling (429) or a slow API does not take the only pod out of the
// Route. The pod stays ready while the API answered within apiReadyGrace.
type apiReachability struct {
	mu          sync.Mutex
	lastCheck   time.Time
	lastSuccess time.Time
	lastErr     error
}

const (
	apiCheckInterval = 30 * time.Second
	apiReadyGrace    = 90 * time.Second
)

var apiReady = &apiReachability{}

// checkAPIVersion is the API reachability check; tests replace it.
var checkAPIVersion = func(ctx context.Context, token string) error {
	_, err := cluster.NewClientWithContext(ctx, token).GetVersion()
	return err
}

// ready reports whether the API was reachable recently, refreshing the
// check when it is older than apiCheckInterval.
func (a *apiReachability) ready(ctx context.Context, token string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.lastCheck.IsZero() || now.Sub(a.lastCheck) >= apiCheckInterval {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := checkAPIVersion(checkCtx, token)
		cancel()
		a.lastCheck, a.lastErr = now, err
		if err == nil {
			a.lastSuccess = now
		} else {
			slog.Warn("readiness check: kubernetes API unreachable", "error", err)
		}
	}
	return !a.lastSuccess.IsZero() && now.Sub(a.lastSuccess) < apiReadyGrace, a.lastErr
}

// HandleReady performs deeper readiness checks for kubelet readiness probes.
// It verifies the Kubernetes API is reachable (cached, see apiReachability).
func HandleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}

	uptime := fmt.Sprintf("%.0fs", time.Since(startTime).Seconds())
	checks["uptime"] = uptime

	if mutations.isDraining() {
		// Stop receiving new traffic while running operations finish.
		checks["server"] = "shutting down"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "draining",
			"checks": checks,
		}); err != nil {
			slog.Error("response encode error", "label", "ready", "error", err)
		}
		return
	}

	token := getClusterToken()
	if token == "" {
		checks["kubernetes"] = "token unavailable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "degraded",
			"checks": checks,
		}); err != nil {
			slog.Error("response encode error", "label", "ready", "error", err)
		}
		return
	}

	ok, lastErr := apiReady.ready(r.Context(), token)
	if !ok {
		checks["kubernetes"] = "unreachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "degraded",
			"checks": checks,
		}); err != nil {
			slog.Error("response encode error", "label", "ready", "error", err)
		}
		return
	}

	checks["kubernetes"] = "ok"
	if lastErr != nil {
		checks["kubernetes"] = "ok (last check failed; within grace period)"
	}
	writeJSON(w, map[string]interface{}{
		"status": "ok",
		"checks": checks,
	}, "ready")
}

// HandleStatus returns the current cluster and operator status.
var HandleStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	status, err := cluster.GetStatus(c)
	if err != nil {
		slog.Error("status error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to get status", code, errorCode)
		return
	}
	writeJSON(w, status, "status")
})

// HandleUpdate validates a nightly catalog update with a server-side dry
// run. Real updates go through HandleUpdateStream, which reports progress.
var HandleUpdate = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in update handler", "error", r)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.Image == "" {
		writeError(w, "image is required", http.StatusBadRequest, "validation")
		return
	}
	if !imageRegex.MatchString(req.Image) {
		writeError(w, fmt.Sprintf("invalid image format: %s", req.Image), http.StatusBadRequest, "validation")
		return
	}
	if !isAllowedImage(req.Image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", req.Image), http.StatusBadRequest, "validation")
		return
	}

	if !req.DryRun {
		writeError(w, "this endpoint only runs dry runs; use POST /api/update/stream to update", http.StatusBadRequest, "validation")
		return
	}
	w.Header().Set("X-Skip-Rate-Limit", "true")
	setOperationTarget(w, req.Image)

	slog.Info("mutation", "op", "update", "image", req.Image, "dryRun", true)

	result, err := runUpdateDryRun(c, req.Image)
	if err != nil {
		slog.Error("update dry run failed", "error", err)
		writeError(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "update")
})

// HandleUpdateStream applies a nightly catalog update and streams progress via SSE.
var HandleUpdateStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in update-stream handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.Image == "" {
		writeError(w, "image is required", http.StatusBadRequest, "validation")
		return
	}
	if !imageRegex.MatchString(req.Image) {
		writeError(w, fmt.Sprintf("invalid image format: %s", req.Image), http.StatusBadRequest, "validation")
		return
	}
	if !isAllowedImage(req.Image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", req.Image), http.StatusBadRequest, "validation")
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, req.Image)
	slog.Info("mutation", "op", "update-stream", "image", req.Image)

	result, updateErr := runUpdateStream(c, req.Image, cluster.OperationOptions{RevertDashboardDev: req.RevertDashboardDev}, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, updateErr)

	if updateErr != nil {
		slog.Error("update-stream failed", "error", updateErr)
		RecordUpdate(false)
		return
	}

	RecordUpdate(result.Success)
})

// HandleTestPullSecret tests whether the pull secret is valid.
var HandleTestPullSecret = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("mutation", "op", "test-pull-secret")

	result, err := cluster.TestPullSecret(c)
	if err != nil {
		slog.Error("test-pull-secret failed", "error", err)
		writeError(w, "test failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "test-pull-secret")
})

// HandleCreatePullSecret creates or updates the quay.io pull secret.
var HandleCreatePullSecret = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.CreatePullSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.Auth == "" {
		writeError(w, "auth is required", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, "")
	slog.Info("mutation", "op", "create-pull-secret")

	result, err := cluster.CreatePullSecret(c, req.Auth)
	if err != nil {
		slog.Error("create-pull-secret failed", "error", err)
		writeError(w, "create pull secret failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "create-pull-secret")
})

// HandleVerifyNodes verifies cluster node readiness and registry access.
var HandleVerifyNodes = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("mutation", "op", "verify-nodes")

	result, err := cluster.VerifyNodeReadiness(c)
	if err != nil {
		slog.Error("verify-nodes failed", "error", err)
		writeError(w, "verification failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "verify-nodes")
})

// HandleLatestNightly fetches the latest nightly image tag from Quay.
var HandleLatestNightly = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "latest-nightly")

	result, err := cluster.FetchLatestNightly(r.Context(), c)
	if err != nil {
		slog.Error("latest-nightly failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, fmt.Sprintf("failed to fetch latest nightly: %v", err), code, errorCode)
		return
	}
	writeJSON(w, result, "latest-nightly")
})

// HandleNightlyTags returns the last N nightly version tags from Quay.
var HandleNightlyTags = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "nightly-tags")

	result, err := cluster.FetchNightlyTags(r.Context(), c, 5)
	if err != nil {
		slog.Error("nightly-tags failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to fetch nightly tags", code, errorCode)
		return
	}
	writeJSON(w, result, "nightly-tags")
})

// HandleBuildExplorerTags returns all nightly FBC tags from Quay.
var HandleBuildExplorerTags = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "build-explorer-tags")

	fetch := cluster.FetchNightlyTagsWithBuildDates
	if r.URL.Query().Get("includeDates") == "false" {
		fetch = cluster.FetchNightlyTags
	}
	result, err := fetch(r.Context(), c, 0)
	if err != nil {
		slog.Error("build-explorer-tags failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to fetch nightly tags", code, errorCode)
		return
	}

	writeJSON(w, result, "build-explorer-tags")
})

// HandleBuildExplorerContent returns the FBC content (relatedImages) for a specific tag.
var HandleBuildExplorerContent = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	image := r.URL.Query().Get("image")
	if image == "" {
		writeError(w, "missing 'image' query parameter", http.StatusBadRequest, "validation")
		return
	}
	if !imageRegex.MatchString(image) {
		writeError(w, "invalid image reference", http.StatusBadRequest, "validation")
		return
	}
	if !isAllowedImage(image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", image), http.StatusBadRequest, "validation")
		return
	}

	labels := r.URL.Query().Get("labels") == "true"

	logImage := image
	if len(logImage) > 80 {
		logImage = logImage[:80] + "..."
	}
	slog.Info("request", "op", "build-explorer-content", "image", logImage, "labels", labels)

	result, err := cluster.ExtractFBCContent(r.Context(), c, image)
	if err != nil {
		slog.Error("build-explorer-content failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to extract FBC content", code, errorCode)
		return
	}

	if labels && len(result.RelatedImages) > 0 {
		imagesCopy := make([]types.RelatedImage, len(result.RelatedImages))
		copy(imagesCopy, result.RelatedImages)
		enriched := cluster.ResolveRelatedImageLabels(r.Context(), c, imagesCopy)
		enrichedResult := *result
		enrichedResult.RelatedImages = enriched
		writeJSON(w, &enrichedResult, "build-explorer-content")
		return
	}

	writeJSON(w, result, "build-explorer-content")
})

// prSearchMaxImages bounds the builds one PR search request names.
const prSearchMaxImages = 40

// HandleBuildExplorerContains reports which nightly builds contain a merged
// pull request: GET /api/build-explorer/contains?pr=<N>[&repo=<owner/name>][&image=<FBC ref>...].
// Without image, the installed build and the newest build of every tag are
// checked. A GitHub rate limit gives partial results (rateLimited), never an error.
var HandleBuildExplorerContains = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo := q.Get("repo")
	if repo == "" {
		repo = cluster.DefaultPRSearchRepo
	}
	if !cluster.IsPRSearchRepo(repo) {
		writeError(w, fmt.Sprintf("PR search supports only %s", strings.Join(cluster.PRSearchRepos(), ", ")), http.StatusBadRequest, "validation")
		return
	}
	pr, err := strconv.Atoi(q.Get("pr"))
	if err != nil || pr <= 0 || pr > 99999999 {
		writeError(w, "'pr' must be a pull request number", http.StatusBadRequest, "validation")
		return
	}
	images := q["image"]
	if len(images) > prSearchMaxImages {
		writeError(w, fmt.Sprintf("at most %d images per request", prSearchMaxImages), http.StatusBadRequest, "validation")
		return
	}
	for _, image := range images {
		// Digest-pinned builds only: a tag moves, so its answer would change.
		if !customFBCImageRegex.MatchString(image) || !strings.Contains(image, "@sha256:") {
			writeError(w, "each 'image' must be a digest-pinned quay.io/rhoai/rhoai-fbc-fragment reference", http.StatusBadRequest, "validation")
			return
		}
	}
	slog.Info("request", "op", "build-explorer-contains", "repo", repo, "pr", pr, "images", len(images))

	result, err := cluster.FindBuildsContainingPR(r.Context(), c, repo, pr, images)
	if err != nil {
		var searchErr *cluster.PRSearchError
		if errors.As(err, &searchErr) {
			writeError(w, searchErr.Msg, searchErr.Status, searchErr.Code)
			return
		}
		slog.Error("build-explorer-contains failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to search the builds", code, errorCode)
		return
	}
	writeJSON(w, result, "build-explorer-contains")
})

// HandleActivity returns the activity log.
var HandleActivity = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	activity, err := cluster.GetActivity(c)
	if err != nil {
		slog.Error("activity error", "error", err)
		writeError(w, "failed to get activity", http.StatusInternalServerError)
		return
	}
	writeJSON(w, activity, "activity")
})

// HandleComponents returns DSC component statuses and deployments.
var HandleComponents = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	includeLabels := r.URL.Query().Get("labels") == "true"
	components, err := cluster.GetComponents(c, includeLabels)
	if err != nil {
		slog.Error("components error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, fmt.Sprintf("Could not read components: %v", err), code, errorCode)
		return
	}
	writeJSON(w, components, "components")
})

var HandleRepairDSC = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Name                    string   `json:"name"`
		Mode                    string   `json:"mode"`
		ExpectedOperatorVersion string   `json:"expectedOperatorVersion"`
		ExpectedExtraComponents []string `json:"expectedExtraComponents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !dscNameRegex.MatchString(req.Name) || (req.Mode != "remove-invalid" && req.Mode != "remove-extra-components" && req.Mode != "reset-defaults") {
		writeError(w, "Provide a DSC name and mode: remove-invalid, remove-extra-components or reset-defaults", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Name+" ("+req.Mode+")")
	result, err := cluster.RepairDSC(c, req.Name, req.Mode, req.ExpectedOperatorVersion, req.ExpectedExtraComponents)
	if err != nil {
		writeError(w, err.Error(), http.StatusUnprocessableEntity, "validation")
		return
	}
	writeOperationResult(w, result, "repair-dsc")
})

// HandleAssistRollout detects and unblocks stuck deployment rollouts.
var HandleAssistRollout = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	// Optional body {"namespace":"...","deployment":"..."} limits the action
	// to one Deployment; an empty body checks all RHOAI Deployments.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Namespace  string `json:"namespace"`
		Deployment string `json:"deployment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}

	target := ""
	if req.Deployment != "" {
		target = req.Namespace + "/" + req.Deployment
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "assist-rollout", "namespace", req.Namespace, "deployment", req.Deployment)

	var result *types.OperationResponse
	var err error
	if req.Deployment != "" {
		result, err = cluster.AssistRolloutFor(c, req.Namespace, req.Deployment)
	} else {
		result, err = cluster.AssistRollout(c)
	}
	if err != nil {
		slog.Error("assist-rollout failed", "error", err)
		writeError(w, "assist-rollout failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "assist-rollout")
})

// HandleDebug returns debug pod information.
var HandleDebug = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	debug, err := cluster.GetDebugInfo(c)
	if err != nil {
		slog.Error("debug error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to get debug info", code, errorCode)
		return
	}
	writeJSON(w, debug, "debug")
})

// HandleDashboardState returns the current state of the rhods-dashboard deployment.
var HandleDashboardState = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	state, err := cluster.GetDashboardState(c)
	if err != nil {
		status, code, message := cluster.DashboardStateError(err)
		if status != http.StatusNotFound {
			slog.Error("dashboard state error", "error", err, "errorCode", code)
			writeError(w, message, status, code)
			return
		}
		// Not deployed: still report a paused dashboard-operator so the page
		// can offer Revert (for example while a Dashboard CR deletion waits
		// for the paused operator to process its finalizer).
		body := map[string]interface{}{"error": message, "errorCode": code}
		if override, oErr := cluster.DashboardOverrideSummary(c); oErr == nil && override != nil && override.Active {
			body["override"] = override
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Error("response encode error", "label", "dashboard-state", "error", err)
		}
		return
	}
	writeJSON(w, state, "dashboard-state")
})

// HandleDashboardDeployPR deploys a PR image to the rhods-dashboard deployment.
var HandleDashboardDeployPR = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.DeployPRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.PR <= 0 {
		writeError(w, "pr must be a positive integer", http.StatusBadRequest, "validation")
		return
	}
	if !cluster.ValidDashboardFlavor(req.Flavor) {
		writeError(w, "flavor must be rhoai or odh", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, fmt.Sprintf("PR #%d", req.PR))
	slog.Info("mutation", "op", "deploy-pr", "pr", req.PR, "flavor", req.Flavor)

	result, err := cluster.DeployPRImageWithFlavor(c, req.PR, req.Flavor)
	if err != nil {
		slog.Error("deploy-pr failed", "error", err)
		writeError(w, "deploy PR image failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-pr")
})

// HandleDashboardRevert reverts the dashboard to the original operator image.
var HandleDashboardRevert = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "revert-dashboard")

	result, err := cluster.RevertDashboardImage(c)
	if err != nil {
		slog.Error("revert-dashboard failed", "error", err)
		writeError(w, "revert dashboard failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "revert-dashboard")
})

var HandleDashboardDeployMain = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	// The body is optional; an empty body deploys the default (RHOAI) flavor.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.DeployMainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if !cluster.ValidDashboardFlavor(req.Flavor) {
		writeError(w, "flavor must be rhoai or odh", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Flavor)
	slog.Info("mutation", "op", "deploy-dashboard-main", "flavor", req.Flavor)
	result, err := cluster.DeployDashboardMainWithFlavor(c, req.Flavor)
	if err != nil {
		slog.Error("deploy dashboard main failed", "error", err)
		writeError(w, "deploy dashboard main failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-dashboard-main")
})

// HandleReinstallStream performs the full uninstall/cleanup/reinstall flow
// and streams progress via SSE.
var HandleReinstallStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in reinstall-stream handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.ReinstallRequest
	// Preserve the legacy empty-body stable rollback, but reject malformed JSON.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if err := validateReinstallRequest(&req); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest, "validation")
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	target := req.TargetType
	if req.Image != "" {
		target += " " + req.Image
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "reinstall-stream", "targetType", req.TargetType, "image", req.Image)

	opts := cluster.OperationOptions{AllowDowngrade: req.AllowDowngrade, RevertDashboardDev: req.RevertDashboardDev}
	result, reinstallErr := runReinstallStream(c, req.TargetType, req.Image, req.Channel, opts, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, reinstallErr)

	if reinstallErr != nil {
		slog.Error("reinstall-stream failed", "error", reinstallErr)
		return
	}

	if req.TargetType == "stable" {
		RecordRollback()
	} else {
		RecordReinstall()
	}
	_ = result
})

// HandleRefreshStream deletes the current CSV and Subscription to trigger OLM
// to reinstall with updated images, streaming progress via SSE.
var HandleRefreshStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in refresh-stream handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, "")
	slog.Info("mutation", "op", "refresh-stream")

	result, refreshErr := runRefreshStream(c, cluster.OperationOptions{RevertDashboardDev: req.RevertDashboardDev}, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, refreshErr)

	if refreshErr != nil {
		slog.Error("refresh-stream failed", "error", refreshErr)
		return
	}
	_ = result
})

// HandleResourcesStatus returns the state of test infrastructure (MinIO, Pipeline Server).
// Uses the SA client to list all DS projects -- this is an internal team tool
// where all authenticated users see all projects.
var HandleResourcesStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	status, err := cluster.GetResourcesStatus(c)
	if err != nil {
		slog.Error("resources status error", "error", err)
		writeError(w, "failed to get resources status", http.StatusInternalServerError)
		return
	}
	writeJSON(w, status, "resources-status")
})

// HandleDSProjects returns the list of Data Science projects.
// Uses the SA client to list all DS projects -- this is an internal team tool
// where all authenticated users see all projects.
var HandleDSProjects = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	projects, err := cluster.GetDSProjects(c)
	if err != nil {
		slog.Error("ds-projects error", "error", err)
		writeError(w, "failed to list projects", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{"projects": projects}, "ds-projects")
})

// HandleMinIOSetup deploys MinIO with a bucket for pipeline artifacts.
var HandleMinIOSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "setup-minio")
	result, err := cluster.SetupMinIO(c)
	if err != nil {
		slog.Error("setup-minio failed", "error", err)
		writeError(w, "MinIO setup failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "setup-minio")
})

// HandleMinIOTeardown deletes the MinIO namespace and all its resources.
var HandleMinIOTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "teardown-minio")
	result, err := cluster.TeardownMinIO(c)
	if err != nil {
		slog.Error("teardown-minio failed", "error", err)
		writeError(w, "MinIO teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-minio")
})

// HandlePipelineServerSetup creates a pipeline server (DSPA) in the specified project.
var HandlePipelineServerSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.PipelineServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "setup-pipeline-server", "project", req.Project)
	result, err := cluster.SetupPipelineServer(c, req.Project)
	if err != nil {
		slog.Error("setup-pipeline-server failed", "error", err)
		writeError(w, "pipeline server setup failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "setup-pipeline-server")
})

// HandlePipelineServerTeardown removes the pipeline server from the specified project.
var HandlePipelineServerTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.PipelineServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "teardown-pipeline-server", "project", req.Project)
	result, err := cluster.TeardownPipelineServer(c, req.Project)
	if err != nil {
		slog.Error("teardown-pipeline-server failed", "error", err)
		writeError(w, "pipeline server teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-pipeline-server")
})

// HandleMLflowSetup creates the MLflow CR.
var HandleMLflowSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "setup-mlflow")
	result, err := cluster.SetupMLflow(c)
	if err != nil {
		slog.Error("setup-mlflow failed", "error", err)
		writeError(w, "MLflow setup failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "setup-mlflow")
})

// HandleMLflowTeardown deletes the MLflow CR.
var HandleMLflowTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "teardown-mlflow")
	result, err := cluster.TeardownMLflow(c)
	if err != nil {
		slog.Error("teardown-mlflow failed", "error", err)
		writeError(w, "MLflow teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-mlflow")
})

// HandleMLflowDeployPR patches the MLflow CR with a PR image.
var HandleMLflowDeployPR = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.DeployPRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.PR <= 0 {
		writeError(w, "pr must be a positive integer", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, fmt.Sprintf("PR #%d", req.PR))
	slog.Info("mutation", "op", "deploy-mlflow-pr", "pr", req.PR)
	result, err := cluster.DeployMLflowPR(c, req.PR)
	if err != nil {
		slog.Error("deploy-mlflow-pr failed", "error", err)
		writeError(w, "MLflow PR deploy failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-mlflow-pr")
})

// HandleMLflowRevert reverts the MLflow CR image to default.
var HandleMLflowRevert = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "revert-mlflow")
	result, err := cluster.RevertMLflowImage(c)
	if err != nil {
		slog.Error("revert-mlflow failed", "error", err)
		writeError(w, "MLflow revert failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "revert-mlflow")
})

// HandleDiagnostics runs cluster health checks and returns a diagnostic report.
var HandleDiagnostics = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "diagnostics")

	result, err := cluster.RunDiagnostics(c)
	if err != nil {
		slog.Error("diagnostics failed", "error", err)
		writeError(w, "diagnostics failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "diagnostics")
})

// HandleDiagnosticsFix attempts to auto-fix a specific problem identified by diagnostics.
var HandleDiagnosticsFix = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req struct {
		ProblemID string `json:"problemId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProblemID == "" {
		writeError(w, "problemId is required", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, req.ProblemID)
	slog.Info("mutation", "op", "diagnostics-fix", "problemId", req.ProblemID)

	result, err := cluster.ApplyFix(c, req.ProblemID)
	if err != nil {
		slog.Error("diagnostics-fix failed", "error", err, "problemId", req.ProblemID)
		writeError(w, fmt.Sprintf("Fix failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if encErr := json.NewEncoder(w).Encode(result); encErr != nil {
		slog.Error("response encode error", "label", "diagnostics-fix", "error", encErr)
	}
})

// HandleDSCPreview returns the default DSC YAML for preview: the installed
// CSV's alm-examples, or the matching upstream sample when the CSV has none.
var HandleDSCPreview = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	defaults, err := cluster.GetDefaultDSCYAML(c)
	if err != nil {
		writeError(w, err.Error(), http.StatusBadGateway, "prerequisites")
		return
	}
	writeJSON(w, map[string]string{
		"yaml":              defaults.YAML,
		"operatorVersion":   defaults.Version,
		"branch":            defaults.Branch,
		"sourceURL":         defaults.SourceURL,
		"source":            defaults.Source,
		"sourceDescription": defaults.SourceDescription,
	}, "dsc-preview")
})

// HandleCreateDSC creates a default DataScienceCluster if one doesn't exist.
var HandleCreateDSC = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "create-dsc")

	result, err := cluster.CreateDefaultDSC(c)
	if err != nil {
		slog.Error("create-dsc failed", "error", err)
		writeError(w, "failed to create DataScienceCluster", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "create-dsc")
})
