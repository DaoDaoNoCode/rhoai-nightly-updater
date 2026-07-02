package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var (
	startTime          = time.Now()
	requestsTotal      atomic.Int64
	updatesTotal       atomic.Int64
	updatesFailedTotal atomic.Int64
	rollbacksTotal     atomic.Int64
	reinstallsTotal    atomic.Int64

	// Page view counters (aggregate only, no user identity)
	pageViewsMu sync.RWMutex
	pageViews   = map[string]*atomic.Int64{}

	// Feature usage counters
	featureUsageMu sync.RWMutex
	featureUsage   = map[string]*atomic.Int64{}
)

// RecordRequest increments the total request counter.
func RecordRequest() { requestsTotal.Add(1) }

// RecordUpdate increments the update counter (success or failure).
func RecordUpdate(success bool) {
	updatesTotal.Add(1)
	if !success {
		updatesFailedTotal.Add(1)
	}
}

// RecordRollback increments the rollback counter.
func RecordRollback() { rollbacksTotal.Add(1) }

// RecordReinstall increments the reinstall counter.
func RecordReinstall() { reinstallsTotal.Add(1) }

// Allowed page and action names (prevent label cardinality explosion)
var validMetricLabel = regexp.MustCompile(`^[a-z_]{1,30}$`)

const maxMetricLabels = 100

func getOrCreateCounter(mu *sync.RWMutex, m map[string]*atomic.Int64, key string) *atomic.Int64 {
	mu.RLock()
	c, ok := m[key]
	mu.RUnlock()
	if ok {
		return c
	}
	mu.Lock()
	defer mu.Unlock()
	if c, ok = m[key]; ok {
		return c
	}
	if len(m) >= maxMetricLabels {
		return nil
	}
	m[key] = &atomic.Int64{}
	return m[key]
}

// RecordPageView increments the counter for a specific page.
func RecordPageView(page string) {
	if !validMetricLabel.MatchString(page) {
		return
	}
	if c := getOrCreateCounter(&pageViewsMu, pageViews, page); c != nil {
		c.Add(1)
	}
}

// RecordFeatureUsage increments the counter for a specific feature action.
func RecordFeatureUsage(action string) {
	if !validMetricLabel.MatchString(action) {
		return
	}
	if c := getOrCreateCounter(&featureUsageMu, featureUsage, action); c != nil {
		c.Add(1)
	}
}

// HandlePageView records a page view or feature usage event.
// Body: {"page": "dashboard"} or {"action": "update_clicked"}
// No user identity is stored — only aggregate counters.
// Requires authentication (X-Forwarded-Access-Token from oauth-proxy).
func HandlePageView(w http.ResponseWriter, r *http.Request) {
	if extractUserToken(r) == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	var req struct {
		Page   string `json:"page"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Page != "" {
		RecordPageView(req.Page)
	}
	if req.Action != "" {
		RecordFeatureUsage(req.Action)
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleMetrics serves Prometheus-compatible metrics without external dependencies.
func HandleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	uptime := time.Since(startTime).Seconds()

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_uptime_seconds Time since process start.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_uptime_seconds gauge\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_uptime_seconds %.2f\n", uptime)

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_requests_total Total API requests served.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_requests_total counter\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_requests_total %d\n", requestsTotal.Load())

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_updates_total Total update operations attempted.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_updates_total counter\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_updates_total %d\n", updatesTotal.Load())

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_updates_failed_total Total update operations that failed.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_updates_failed_total counter\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_updates_failed_total %d\n", updatesFailedTotal.Load())

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_rollbacks_total Total rollback operations.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_rollbacks_total counter\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_rollbacks_total %d\n", rollbacksTotal.Load())

	fmt.Fprintf(w, "# HELP rhoai_nightly_updater_reinstalls_total Total reinstall operations.\n")
	fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_reinstalls_total counter\n")
	fmt.Fprintf(w, "rhoai_nightly_updater_reinstalls_total %d\n", reinstallsTotal.Load())

	// Page views by page name
	pageViewsMu.RLock()
	if len(pageViews) > 0 {
		fmt.Fprintf(w, "# HELP rhoai_nightly_updater_page_views_total Page views by page.\n")
		fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_page_views_total counter\n")
		for page, counter := range pageViews {
			fmt.Fprintf(w, "rhoai_nightly_updater_page_views_total{page=%q} %d\n", page, counter.Load())
		}
	}
	pageViewsMu.RUnlock()

	// Feature usage by action
	featureUsageMu.RLock()
	if len(featureUsage) > 0 {
		fmt.Fprintf(w, "# HELP rhoai_nightly_updater_feature_usage_total Feature usage by action.\n")
		fmt.Fprintf(w, "# TYPE rhoai_nightly_updater_feature_usage_total counter\n")
		for action, counter := range featureUsage {
			fmt.Fprintf(w, "rhoai_nightly_updater_feature_usage_total{action=%q} %d\n", action, counter.Load())
		}
	}
	featureUsageMu.RUnlock()
}
