package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An unreachable API must be reported as a network failure, not as "the
// dashboard is not deployed" (A04-9).
func TestHandleDashboardStateReportsNetworkErrors(t *testing.T) {
	setupDevMode(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)

	r := httptest.NewRequest("GET", "/api/dashboard/state", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user-token")
	w := httptest.NewRecorder()
	HandleDashboardState(w, r)
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusServiceUnavailable || body["errorCode"] != "network" || strings.Contains(body["error"], "failed to get dashboard state") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// GET /api/dashboard/builds is a read, maps its errors like the state route
// (a network failure is 503 network, not "not deployed") and is off in the
// gitops profile.
func TestHandleDashboardBuilds(t *testing.T) {
	t.Run("registered-as-a-read", func(t *testing.T) {
		for _, r := range routes {
			if strings.HasSuffix(r.Pattern, "/api/dashboard/builds") {
				if r.Pattern != "GET /api/dashboard/builds" {
					t.Fatalf("pattern %q", r.Pattern)
				}
				return
			}
		}
		t.Fatal("GET /api/dashboard/builds is not registered")
	})
	t.Run("network-error", func(t *testing.T) {
		setupDevMode(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		_ = listener.Close()
		t.Setenv("KUBERNETES_SERVICE_HOST", host)
		t.Setenv("KUBERNETES_SERVICE_PORT", port)
		r := httptest.NewRequest("GET", "/api/dashboard/builds", nil)
		r.Header.Set("X-Forwarded-Access-Token", "user-token")
		w := httptest.NewRecorder()
		HandleDashboardBuilds(w, r)
		if body := decodeError(t, w); w.Code != http.StatusServiceUnavailable || body["errorCode"] != "network" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("gitops-profile", func(t *testing.T) {
		setupDevMode(t)
		calls := countingAPIServer(t)
		mux := gitOpsMux(t)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(http.MethodGet, "/api/dashboard/builds", "user:admin", ""))
		body := decodeError(t, w)
		if w.Code != http.StatusForbidden || body["errorCode"] != "profile_gitops" || !strings.HasPrefix(body["error"].(string), "Dashboard Dev is off") {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("%d API server connections for a refused route", n)
		}
	})
}

func TestDashboardDeployHandlersValidateFlavor(t *testing.T) {
	setupDevMode(t)
	original := mutationPermission
	mutationPermission = func(context.Context, string) (bool, error) { return true, nil }
	t.Cleanup(func() { mutationPermission = original })
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
	}{
		{"pr-unknown-flavor", HandleDashboardDeployPR, "/api/dashboard/deploy-pr", `{"pr":1,"flavor":"upstream"}`},
		{"main-unknown-flavor", HandleDashboardDeployMain, "/api/dashboard/deploy-main", `{"flavor":"upstream"}`},
		{"main-invalid-body", HandleDashboardDeployMain, "/api/dashboard/deploy-main", `{"flavor":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			r.Header.Set("X-Forwarded-Access-Token", "user-token")
			r.Header.Set("X-Forwarded-User", "flavor-"+tc.name)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"validation"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
