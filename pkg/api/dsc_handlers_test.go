package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The DSC change routes validate their body before anything runs, and pass
// exactly the request on.
func TestDSCChangeRoutesValidateTheirRequest(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mux := http.NewServeMux()
	Register(mux)
	var toggled []types.DSCToggleRequest
	var reset []types.DSCPlanRequest
	prevSet, prevHard := setDSCManagementState, hardResetDSC
	t.Cleanup(func() { setDSCManagementState, hardResetDSC = prevSet, prevHard })
	setDSCManagementState = func(_ *cluster.Client, req types.DSCToggleRequest) (*types.OperationResponse, error) {
		toggled = append(toggled, req)
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
	hardResetDSC = func(_ *cluster.Client, req types.DSCPlanRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		reset = append(reset, req)
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
	token := "0123456789abcdef01234567"
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"Managed","to":"Removed"}`, http.StatusOK},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve/nim","from":"Managed","to":"Removed"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"","to":"Re moved"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"Bad Name","path":"ray","from":"","to":"Managed"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","confirm":"default"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"short","confirm":"default-dsc"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","confirm":"default-dsc"}`, http.StatusOK},
		{"/api/components/dsc/fix", `{"name":"default-dsc","uid":"u-1"}`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSON(tc.path, "user:admin", tc.body))
		if w.Code != tc.want {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
		resetPackageState(t) // the rate limit is per user and route
	}
	if len(toggled) != 1 || toggled[0].Path != "kserve.nim" || toggled[0].From != "Managed" || toggled[0].To != "Removed" {
		t.Fatalf("toggled = %+v", toggled)
	}
	if len(reset) != 1 || reset[0].Confirm != "default-dsc" || reset[0].Token != token {
		t.Fatalf("reset = %+v", reset)
	}
}

// A plan names what uses each component across projects: only for a
// cluster-admin, who may make the change.
func TestDSCPlanNeedsClusterAdmin(t *testing.T) {
	setupDevMode(t)
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	mux := http.NewServeMux()
	Register(mux)
	called := false
	prev := planDSCChange
	t.Cleanup(func() { planDSCChange = prev })
	planDSCChange = func(*cluster.Client, string, string, string, bool) (*types.DSCPlan, error) {
		called = true
		return &types.DSCPlan{}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "/api/components/dsc/plan?action=hard-reset", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:reader")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("reader: %d, planned %v", w.Code, called)
	}
	allowMutations(t)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !called {
		t.Fatalf("admin: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodGet, "/api/components/dsc/plan?action=toggle&path=a/b&to=Managed", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:admin")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad path: %d", w.Code)
	}
}
