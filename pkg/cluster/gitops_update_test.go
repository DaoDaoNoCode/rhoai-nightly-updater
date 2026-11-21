package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// pendingInstallPlan is the check that keeps Update from deleting a
// Subscription OLM is still installing. An answer it cannot read is an
// error (Update then refuses), never "nothing pending".
func TestPendingInstallPlan(t *testing.T) {
	ipPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, "install-abc")
	const sub = `{"status":{"installPlanRef":{"name":"install-abc"}}}`
	for _, tc := range []struct {
		name, sub, ip       string
		wantName, wantPhase string
		wantErr             string
	}{
		{name: "installing", sub: sub, ip: `{"status":{"phase":"Installing"}}`, wantName: "install-abc", wantPhase: "Installing"},
		{name: "complete", sub: sub, ip: `{"status":{"phase":"Complete"}}`},
		{name: "no install plan yet", sub: `{"status":{}}`},
		{name: "unreadable Subscription", sub: `<html>`, wantErr: "parse Subscription"},
		{name: "unreadable InstallPlan", sub: sub, ip: `<html>`, wantErr: "parse InstallPlan install-abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.json("GET", rhoai.SubscriptionPath(), 200, tc.sub)
			if tc.ip != "" {
				f.json("GET", ipPath, 200, tc.ip)
			}
			name, phase, err := pendingInstallPlan(c)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || name != "" {
					t.Fatalf("pendingInstallPlan = %q, %q, %v; want an error with %q", name, phase, err, tc.wantErr)
				}
				return
			}
			if err != nil || name != tc.wantName || phase != tc.wantPhase {
				t.Fatalf("pendingInstallPlan = %q, %q, %v; want %q, %q", name, phase, err, tc.wantName, tc.wantPhase)
			}
		})
	}
}
