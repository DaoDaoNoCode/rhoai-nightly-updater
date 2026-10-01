package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var imageRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]+(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
var namespaceRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var channelRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var customFBCImageRegex = regexp.MustCompile(`^quay\.io/[a-zA-Z0-9._-]+(?:/[a-zA-Z0-9._-]+)+(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
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
				return fmt.Errorf("provide a quay.io FBC image with a tag or a full SHA256 digest")
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

// clusterMutationInProgress is a non-blocking mutex that prevents concurrent
// cluster-level mutation operations (Update, Reinstall, RefreshOperator).
// When true, a cluster mutation is in progress and new mutation requests
// receive HTTP 409 Conflict instead of proceeding.
var clusterMutationInProgress atomic.Bool

// acquireClusterMutationLock tries to acquire the cluster mutation lock.
// Returns true if the lock was acquired (caller must defer releaseClusterMutationLock).
// Returns false if another operation is already in progress; the caller should
// return HTTP 409.
func acquireClusterMutationLock() bool {
	return clusterMutationInProgress.CompareAndSwap(false, true)
}

// releaseClusterMutationLock releases the cluster mutation lock.
func releaseClusterMutationLock() {
	clusterMutationInProgress.Store(false)
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

// rateLimiter provides simple per-user rate limiting for mutation endpoints.
// It stores the last mutation timestamp per username in a sync.Map.
// This is protection against accidental double-clicks, not a DDoS defense.
// A background goroutine periodically evicts expired entries to prevent
// unbounded memory growth.
type rateLimiter struct {
	// users maps username (string) -> last mutation time (time.Time)
	users   sync.Map
	window  time.Duration
}

// mutationLimiter is the package-level rate limiter for mutation endpoints.
var mutationLimiter = &rateLimiter{window: 30 * time.Second}

func init() {
	mutationLimiter.startCleanup()
}

func (rl *rateLimiter) isRateLimited(username string) bool {
	if val, ok := rl.users.Load(username); ok {
		if time.Since(val.(time.Time)) < rl.window {
			return true
		}
	}
	return false
}

func (rl *rateLimiter) recordMutation(username string) {
	rl.users.Store(username, time.Now())
}

func (rl *rateLimiter) evictExpiredEntries() {
	rl.users.Range(func(key, value any) bool {
		if time.Since(value.(time.Time)) >= rl.window {
			rl.users.Delete(key)
		}
		return true
	})
}

// startCleanup launches a background goroutine that periodically removes
// expired entries from the rate limiter map to prevent unbounded growth.
func (rl *rateLimiter) startCleanup() {
	go func() {
		// Sweep at 2x the window interval so entries are cleaned up promptly
		// but we don't burn CPU on a tight loop.
		ticker := time.NewTicker(rl.window * 2)
		defer ticker.Stop()
		for range ticker.C {
			rl.evictExpiredEntries()
		}
	}()
}

// withMutationAuth wraps a handler that performs a cluster mutation.
// It checks user RBAC before using the SA token, applies a per-user rate limit,
// and gives accepted work a bounded lifetime independent of the browser.
func withMutationAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		userToken := extractUserToken(r)
		if userToken == "" {
			writeError(w, "no auth token", http.StatusUnauthorized, "unauthorized")
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		username := resolveUsername(r)
		allowed, permissionErr := mutationPermission(r.Context(), userToken)
		if permissionErr != nil {
			writeError(w, "Cannot verify mutation permissions. No changes were made.", http.StatusServiceUnavailable, "authorization_unavailable")
			return
		}
		if !allowed {
			writeError(w, "Read-only access: updating operator Subscriptions in redhat-ods-operator is required.", http.StatusForbidden, "forbidden")
			return
		}

		rateLimitKey := username + ":" + r.URL.Path
		if mutationLimiter.isRateLimited(rateLimitKey) {
			slog.Warn("rate limited", "user", username)
			w.Header().Set("Retry-After", "30")
			writeError(w, "Too many requests. Please wait 30 seconds before retrying.", http.StatusTooManyRequests, "rate_limited")
			return
		}

		opContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
		defer cancel()
		client := cluster.NewClientWithContext(opContext, clusterToken)
		client.SetUsername(username)

		sw := &statusWriter{ResponseWriter: w}
		fn(client, sw, r)
		if sw.status == 0 || sw.status < 400 {
			if sw.Header().Get("X-Skip-Rate-Limit") != "true" {
				mutationLimiter.recordMutation(rateLimitKey)
			}
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
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

func resolveUsername(r *http.Request) string {
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
func extractUserToken(r *http.Request) string {
	if token := r.Header.Get("X-Forwarded-Access-Token"); token != "" {
		return token
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	if os.Getenv("DEV_MODE") == "true" {
		if token := os.Getenv("DEV_TOKEN"); token != "" {
			return token
		}
	}
	return ""
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
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
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
// It verifies the user is authenticated (via oauth-proxy) and creates a
// Client using the ServiceAccount token for k8s API calls.
// The logged-in user identity is captured from X-Forwarded-User header.
func withAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		userToken := extractUserToken(r)
		if userToken == "" {
			writeError(w, "no auth token", http.StatusUnauthorized, "unauthorized")
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		username := resolveUsername(r)
		client := cluster.NewClientWithContext(r.Context(), clusterToken)
		client.SetUsername(username)
		fn(client, w, r)
	}
}

// HandleUserPermissions returns the permissions for the logged-in user.
// All authenticated users are allowed to mutate -- authorization is enforced
// upstream by oauth-proxy before requests reach this backend.
var HandleUserPermissions = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	username := resolveUsername(r)
	allowed, err := mutationPermission(r.Context(), extractUserToken(r))
	if err != nil {
		writeError(w, "Cannot verify mutation permissions", http.StatusServiceUnavailable, "authorization_unavailable")
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
	writeJSON(w, map[string]string{"status": "ok"}, "health")
}

// HandleReady performs deeper readiness checks for kubelet readiness probes.
// It verifies the Kubernetes API is reachable by calling the /version endpoint.
func HandleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}

	uptime := fmt.Sprintf("%.0fs", time.Since(startTime).Seconds())
	checks["uptime"] = uptime

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

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	client := cluster.NewClientWithContext(ctx, token)
	_, err := client.GetVersion()
	if err != nil {
		slog.Warn("readiness check: kubernetes API unreachable", "error", err)
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
		writeError(w, "failed to get status", http.StatusInternalServerError)
		return
	}
	writeJSON(w, status, "status")
})

// HandleUpdate applies a nightly catalog update to the cluster.
var HandleUpdate = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
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

	if req.DryRun {
		w.Header().Set("X-Skip-Rate-Limit", "true")
	}

	slog.Info("mutation", "op", "update", "image", req.Image, "dryRun", req.DryRun)

	result, err := cluster.Update(c, req.Image, req.DryRun)
	if err != nil {
		slog.Error("update failed", "error", err)
		if !req.DryRun {
			RecordUpdate(false)
		}
		writeError(w, "update failed", http.StatusInternalServerError)
		return
	}
	if !req.DryRun {
		RecordUpdate(result.Success)
	}
	writeOperationResult(w, result, "update")
})

// HandleUpdateStream applies a nightly catalog update and streams progress via SSE.
var HandleUpdateStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
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

	slog.Info("mutation", "op", "update-stream", "image", req.Image)

	result, updateErr := cluster.UpdateStream(c, req.Image, func(event cluster.UpdateStepEvent) {
		sseWriter.SendStep(UpdateStep{
			Step:      event.Step,
			Status:    event.Status,
			Message:   event.Message,
			Detail:    event.Detail,
			ElapsedMs: time.Since(sseWriter.StartTime()).Milliseconds(),
			ErrorCode: event.ErrorCode,
		})
	})

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
		writeError(w, fmt.Sprintf("failed to fetch latest nightly: %v", err), http.StatusInternalServerError)
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
		writeError(w, "failed to fetch nightly tags", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "nightly-tags")
})

// HandleBuildExplorerTags returns all nightly FBC tags from Quay.
var HandleBuildExplorerTags = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "build-explorer-tags")

	result, err := cluster.FetchNightlyTags(r.Context(), c, 0)
	if err != nil {
		slog.Error("build-explorer-tags failed", "error", err)
		writeError(w, "failed to fetch nightly tags", http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("includeDates") != "false" {
		cluster.EnrichTagsWithBuildDates(r.Context(), c, result.Tags)
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
		writeError(w, "failed to extract FBC content", http.StatusInternalServerError)
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
		writeError(w, "failed to get components", http.StatusInternalServerError)
		return
	}
	writeJSON(w, components, "components")
})

var HandleRepairDSC = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
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
	result, err := cluster.RepairDSC(c, req.Name, req.Mode, req.ExpectedOperatorVersion, req.ExpectedExtraComponents)
	if err != nil {
		writeError(w, err.Error(), http.StatusUnprocessableEntity, "validation")
		return
	}
	writeOperationResult(w, result, "repair-dsc")
})

// HandleAssistRollout detects and unblocks stuck deployment rollouts.
var HandleAssistRollout = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
		return
	}
	defer releaseClusterMutationLock()

	slog.Info("mutation", "op", "assist-rollout")

	result, err := cluster.AssistRollout(c)
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
		writeError(w, "failed to get debug info", http.StatusInternalServerError)
		return
	}
	writeJSON(w, debug, "debug")
})

// HandleDashboardState returns the current state of the rhods-dashboard deployment.
var HandleDashboardState = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	state, err := cluster.GetDashboardState(c)
	if err != nil {
		slog.Error("dashboard state error", "error", err)
		writeError(w, "failed to get dashboard state", http.StatusInternalServerError)
		return
	}
	writeJSON(w, state, "dashboard-state")
})

// HandleDashboardDeployPR deploys a PR image to the rhods-dashboard deployment.
var HandleDashboardDeployPR = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
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

	slog.Info("mutation", "op", "deploy-pr", "pr", req.PR)

	result, err := cluster.DeployPRImage(c, req.PR)
	if err != nil {
		slog.Error("deploy-pr failed", "error", err)
		writeError(w, "deploy PR image failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-pr")
})

// HandleDashboardRevert reverts the dashboard to the original operator image.
var HandleDashboardRevert = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
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
	result, err := cluster.DeployDashboardMain(c)
	if err != nil {
		slog.Error("deploy dashboard main failed", "error", err)
		writeError(w, "deploy dashboard main failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-dashboard-main")
})

// HandleRefreshOperator deletes the current CSV to trigger OLM to reinstall
// with updated images from the current catalog.
var HandleRefreshOperator = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in refresh handler", "error", rec)
		}
	}()

	slog.Info("mutation", "op", "refresh")

	result, err := cluster.RefreshOperator(c)
	if err != nil {
		slog.Error("refresh failed", "error", err)
		writeError(w, "refresh failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "refresh")
})

// HandleRollback handles reinstall operations (rollback to stable or reinstall to a specific nightly).
// It accepts an optional ReinstallRequest body. If the body is empty or targetType is "stable",
// it performs the original rollback-to-stable flow. If targetType is "nightly" and an image is
// provided, it reinstalls using the specified nightly FBC image.
var HandleRollback = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
		return
	}
	defer releaseClusterMutationLock()

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

	slog.Info("mutation", "op", "reinstall", "targetType", req.TargetType, "image", req.Image, "channelOverride", req.Channel)

	result, err := cluster.Reinstall(c, req.TargetType, req.Image, req.Channel)
	if err != nil {
		slog.Error("reinstall failed", "error", err)
		writeError(w, "reinstall failed", http.StatusInternalServerError)
		return
	}
	if req.TargetType == "stable" {
		RecordRollback()
	} else {
		RecordReinstall()
	}
	writeOperationResult(w, result, "rollback")
})

// HandleReinstallStream performs the full uninstall/cleanup/reinstall flow
// and streams progress via SSE.
var HandleReinstallStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
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

	slog.Info("mutation", "op", "reinstall-stream", "targetType", req.TargetType, "image", req.Image)

	result, reinstallErr := cluster.ReinstallStream(c, req.TargetType, req.Image, req.Channel, func(event cluster.UpdateStepEvent) {
		sseWriter.SendStep(UpdateStep{
			Step:      event.Step,
			Status:    event.Status,
			Message:   event.Message,
			Detail:    event.Detail,
			ElapsedMs: time.Since(sseWriter.StartTime()).Milliseconds(),
			ErrorCode: event.ErrorCode,
		})
	})

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
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in refresh-stream handler", "error", rec)
		}
	}()

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	slog.Info("mutation", "op", "refresh-stream")

	result, refreshErr := cluster.RefreshOperatorStream(c, func(event cluster.UpdateStepEvent) {
		sseWriter.SendStep(UpdateStep{
			Step:      event.Step,
			Status:    event.Status,
			Message:   event.Message,
			Detail:    event.Detail,
			ElapsedMs: time.Since(sseWriter.StartTime()).Milliseconds(),
			ErrorCode: event.ErrorCode,
		})
	})

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
	projects, err := cluster.GetDSProjects(c)
	if err != nil {
		slog.Error("resources status: failed to list projects", "error", err)
		writeError(w, "failed to get resources status", http.StatusInternalServerError)
		return
	}
	status, err := cluster.GetResourcesStatus(c, projects)
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
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
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

// HandleDSCPreview returns the default DSC YAML for preview (fetched from upstream, cached).
var HandleDSCPreview = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	defaults, err := cluster.GetDefaultDSCYAML(c)
	if err != nil {
		writeError(w, err.Error(), http.StatusBadGateway, "prerequisites")
		return
	}
	yamlContent := defaults.YAML
	writeJSON(w, map[string]string{"yaml": yamlContent, "operatorVersion": defaults.Version, "branch": defaults.Branch, "sourceURL": defaults.SourceURL}, "dsc-preview")
})

// HandleCreateDSC creates a default DataScienceCluster if one doesn't exist.
var HandleCreateDSC = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !acquireClusterMutationLock() {
		writeError(w, "Another cluster operation is in progress. Please wait.", http.StatusConflict, "cluster_busy")
		return
	}
	defer releaseClusterMutationLock()
	slog.Info("mutation", "op", "create-dsc")

	result, err := cluster.CreateDefaultDSC(c)
	if err != nil {
		slog.Error("create-dsc failed", "error", err)
		writeError(w, "failed to create DataScienceCluster", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "create-dsc")
})
