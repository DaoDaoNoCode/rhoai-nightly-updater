package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

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
