package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func shortDashboardOperatorWait(t *testing.T) {
	t.Helper()
	timeout, interval := dashboardOperatorReadyTimeout, dashboardOperatorPollInterval
	dashboardOperatorReadyTimeout, dashboardOperatorPollInterval = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { dashboardOperatorReadyTimeout, dashboardOperatorPollInterval = timeout, interval })
}

func TestDashboardDevActive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, f *dashboardDevFixture, c *Client)
		active bool
	}{
		{"clean", func(t *testing.T, f *dashboardDevFixture, c *Client) {}, false},
		{"session", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
			mustDeploy(t, c, "pr", 1, "")
		}, true},
		{"paused-outside-tool", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			zero := 0
			f.operator.Spec.Replicas = &zero
		}, true},
		{"corrupt-annotation-on-running-operator", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			f.operator.Metadata.Annotations[dashboardDevAnnotation] = "{"
		}, true},
		{"unmanaged-dashboard-workload", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			d := f.workloads["notebooks-ui"]
			d.Metadata.Annotations = map[string]string{"opendatahub.io/managed": "false"}
			f.workloads["notebooks-ui"] = d
		}, true},
		{"unmanaged-unrelated-workload", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			d := f.workloads["unrelated"]
			d.Metadata.Annotations = map[string]string{"opendatahub.io/managed": "false"}
			d.Metadata.OwnerReferences = nil
			f.workloads["unrelated"] = d
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			tc.setup(t, f, c)
			active, err := DashboardDevActive(c)
			if err != nil || active != tc.active {
				t.Fatalf("active=%v err=%v", active, err)
			}
		})
	}
}

func TestDashboardDevActiveLegacyAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dashboard string // rhods-dashboard body; "" means 404
		operator  int    // dashboard-operator status
		active    bool
		wantErr   bool
	}{
		{name: "legacy-clean", operator: 404, dashboard: `{"metadata":{},"spec":{"template":{"spec":{"containers":[{"name":"rhods-dashboard","image":"registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:1"}]}}}}`},
		{name: "legacy-unmanaged", operator: 404, dashboard: `{"metadata":{"annotations":{"opendatahub.io/managed":"false"}}}`, active: true},
		{name: "legacy-pr-image", operator: 404, dashboard: `{"metadata":{},"spec":{"template":{"spec":{"containers":[{"name":"gen-ai-ui","image":"quay.io/opendatahub/odh-mod-arch-gen-ai:pr-12"}]}}}}`, active: true},
		{name: "legacy-not-installed", operator: 404},
		{name: "operator-read-error", operator: 503, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/dashboard-operator"):
					w.WriteHeader(tc.operator)
					io.WriteString(w, `{"kind":"Status","status":"Failure"}`)
				case strings.HasSuffix(r.URL.Path, "/rhods-dashboard") && tc.dashboard != "":
					io.WriteString(w, tc.dashboard)
				default:
					w.WriteHeader(404)
					io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound"}`)
				}
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
			active, err := DashboardDevActive(c)
			if (err != nil) != tc.wantErr || active != tc.active {
				t.Fatalf("active=%v err=%v", active, err)
			}
		})
	}
}

func TestRevertDashboardDevForOperation(t *testing.T) {
	t.Run("inactive-is-a-no-op", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		if err := RevertDashboardDevForOperation(c); err != nil || len(f.writes) != 0 {
			t.Fatalf("err=%v writes=%v", err, f.writes)
		}
	})
	t.Run("reverts-and-waits-until-ready", func(t *testing.T) {
		shortDashboardOperatorWait(t)
		f, c := newDashboardDevFixture(t)
		f.readyOnResume = true
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		if err := RevertDashboardDevForOperation(c); err != nil {
			t.Fatal(err)
		}
		if *f.operator.Spec.Replicas != 2 || f.operator.Metadata.Annotations[dashboardDevAnnotation] != "" {
			t.Fatalf("replicas=%d annotations=%v", *f.operator.Spec.Replicas, f.operator.Metadata.Annotations)
		}
		// Idempotent: nothing is left to do.
		writes := len(f.writes)
		if err := RevertDashboardDevForOperation(c); err != nil || len(f.writes) != writes {
			t.Fatalf("second call err=%v writes=%v", err, f.writes[writes:])
		}
	})
	t.Run("D1-dashboard-deleting-while-paused", func(t *testing.T) {
		shortDashboardOperatorWait(t)
		f, c := newDashboardDevFixture(t)
		f.readyOnResume = true
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		f.dashboardDeleting = true
		override, err := DashboardOverrideSummary(c)
		if err != nil || !override.DashboardDeleting || !strings.Contains(strings.Join(override.Warnings, " "), "finalizer") {
			t.Fatalf("override=%+v err=%v", override, err)
		}
		var state types.DashboardState
		populateDashboardDevState(c, &state)
		if state.Override == nil || !state.Override.DashboardDeleting {
			t.Fatalf("state override=%+v", state.Override)
		}
		if err := RevertDashboardDevForOperation(c); err != nil || *f.operator.Spec.Replicas != 2 {
			t.Fatalf("err=%v replicas=%d", err, *f.operator.Spec.Replicas)
		}
		if override, _ := DashboardOverrideSummary(c); override.DashboardDeleting {
			t.Fatal("deletion warning shown for a running operator")
		}
	})
	t.Run("operator-never-ready", func(t *testing.T) {
		shortDashboardOperatorWait(t)
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		err := RevertDashboardDevForOperation(c)
		if err == nil || !strings.Contains(err.Error(), "did not become ready") || *f.operator.Spec.Replicas != 2 {
			t.Fatalf("err=%v replicas=%d", err, *f.operator.Spec.Replicas)
		}
	})
	t.Run("caller-cancels", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		ctx, cancel := context.WithCancel(context.Background())
		c.ctx = ctx
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		err := RevertDashboardDevForOperation(c)
		if err == nil || time.Since(start) > 10*time.Second || *f.operator.Spec.Replicas != 2 {
			t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
		}
	})
	t.Run("resume-rejected", func(t *testing.T) {
		shortDashboardOperatorWait(t)
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		f.failOperatorPatch = http.StatusForbidden
		if err := RevertDashboardDevForOperation(c); err == nil || *f.operator.Spec.Replicas != 0 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		allPRImagesPublished(t)
		c, annotations := legacyAnnotationServer(t, nil)
		if result, err := DeployPRImage(c, 42); err != nil || !result.Success {
			t.Fatalf("deploy=%+v err=%v", result, err)
		}
		if err := RevertDashboardDevForOperation(c); err != nil {
			t.Fatal(err)
		}
		if _, ok := annotations()["opendatahub.io/managed"]; ok {
			t.Fatalf("annotations=%v", annotations())
		}
	})
}

func TestDashboardDeployRefusesWhileDashboardDeleting(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	f.dashboardDeleting = true
	mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
	result, err := deployDashboardBuild(c, "pr", 1, "")
	if err != nil || result.Success || result.ErrorCode != "prerequisites" || len(f.writes) != 0 {
		t.Fatalf("result=%+v err=%v writes=%v", result, err, f.writes)
	}
}

func TestDashboardOverrideWarnings(t *testing.T) {
	t.Run("stale-warns-about-runlevel-delay", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		f.operator.Metadata.Annotations[platformVersionAnnotation] = "3.6.0"
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		f.operator.Metadata.Annotations[platformVersionAnnotation] = "3.6.1"
		override, _ := DashboardOverrideSummary(c)
		if !override.Stale || len(override.Warnings) != 1 || !strings.Contains(override.Warnings[0], "10 minutes") {
			t.Fatalf("override=%+v", override)
		}
	})
	t.Run("external-pause", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		zero := 0
		f.operator.Spec.Replicas = &zero
		override, _ := DashboardOverrideSummary(c)
		if len(override.Warnings) != 1 || !strings.Contains(override.Warnings[0], "outside Dashboard Dev") {
			t.Fatalf("override=%+v", override)
		}
	})
	t.Run("clean-has-none", func(t *testing.T) {
		_, c := newDashboardDevFixture(t)
		override, _ := DashboardOverrideSummary(c)
		if override.Active || len(override.Warnings) != 0 {
			t.Fatalf("override=%+v", override)
		}
	})
}
