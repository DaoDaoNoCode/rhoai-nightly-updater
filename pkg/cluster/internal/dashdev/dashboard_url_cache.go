package dashdev

import (
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The dashboard's address changes only when RHOAI does (an upgrade can move
// it behind the gateway), so the status, which is polled, reads it at most
// every few minutes. The status serves both profiles, and the gitops
// profile's RBAC (deploy/rbac-gitops.yaml) has no Route read, so it uses
// the Dashboard CR only (dashboardURLFromComponent): on a release without
// status.url the app launcher shows no dashboard link rather than a guess.
// A read error is cached only briefly, so a transient failure does not hide
// the link for the full five minutes.
var URLCache = kube.NewLRU[string, string](8)

// Dashboard URL cache lifetimes: an answer (a URL or none), and a read
// error.
var (
	dashboardURLTTL = 5 * time.Minute
	URLErrorTTL     = 15 * time.Second
)

// CachedURL is ReadComponentURL's answer, cached per API server in
// URLCache.
func CachedURL(c *kube.Client) string {
	if u, ok := URLCache.Get(c.BaseURL()); ok {
		return u
	}
	u, err := ReadComponentURL(c)
	ttl := dashboardURLTTL
	if err != nil {
		slog.Debug("cannot read the dashboard URL", "error", err)
		ttl = URLErrorTTL
	}
	URLCache.Add(c.BaseURL(), u, ttl)
	return u
}
