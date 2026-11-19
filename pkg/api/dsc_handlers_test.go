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
	var finished []types.DSCFinishRequest
	var soft []types.DSCPlanRequest
	prevSet, prevHard, prevFinish, prevSoft := setDSCManagementState, hardResetDSC, finishDSCHardReset, softResetDSC
	t.Cleanup(func() {
		setDSCManagementState, hardResetDSC, finishDSCHardReset, softResetDSC = prevSet, prevHard, prevFinish, prevSoft
	})
	softResetDSC = func(_ *cluster.Client, req types.DSCPlanRequest) (*types.OperationResponse, error) {
		soft = append(soft, req)
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
	finishDSCHardReset = func(_ *cluster.Client, req types.DSCFinishRequest) (*types.OperationResponse, error) {
		finished = append(finished, req)
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
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
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"Managed","to":"Removed","uid":"u-1","token":"` + token + `"}`, http.StatusOK},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"Managed","to":"Removed"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"Managed","to":"Removed","uid":"u-1","token":"short"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve/nim","from":"Managed","to":"Removed"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"default-dsc","path":"kserve.nim","from":"","to":"Re moved"}`, http.StatusBadRequest},
		{"/api/components/dsc/management", `{"name":"Bad Name","path":"ray","from":"","to":"Managed"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","confirm":"default"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"short","confirm":"default-dsc"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","confirm":"default-dsc"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/stream", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","confirm":"default-dsc","keepEnabled":true}`, http.StatusOK},
		{"/api/components/dsc/reset", `{"name":"default-dsc","uid":"u-1","token":"` + token + `"}`, http.StatusBadRequest},
		{"/api/components/dsc/reset", `{"name":"default-dsc","uid":"u-1","token":"` + token + `","keepEnabled":false}`, http.StatusOK},
		{"/api/components/dsc/fix", `{"name":"default-dsc","uid":"u-1"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/finish", `{"name":"default-dsc"}`, http.StatusBadRequest},
		{"/api/components/dsc/hard-reset/finish", `{"name":"default-dsc","token":"` + token + `"}`, http.StatusOK},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSON(tc.path, "user:admin", tc.body))
		if w.Code != tc.want {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
		resetPackageState(t) // the rate limit is per user and route
	}
	if len(toggled) != 1 || toggled[0].Path != "kserve.nim" || toggled[0].From != "Managed" || toggled[0].To != "Removed" || toggled[0].Token != token || toggled[0].UID != "u-1" {
		t.Fatalf("toggled = %+v", toggled)
	}
	if len(reset) != 1 || reset[0].Confirm != "default-dsc" || reset[0].Token != token || reset[0].KeepEnabled == nil || !*reset[0].KeepEnabled {
		t.Fatalf("reset = %+v", reset)
	}
	if len(soft) != 1 || soft[0].KeepEnabled == nil || *soft[0].KeepEnabled {
		t.Fatalf("soft reset = %+v", soft)
	}
	if len(finished) != 1 || finished[0].Name != "default-dsc" || finished[0].Token != token {
		t.Fatalf("finished = %+v", finished)
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
	planDSCChange = func(*cluster.Client, string, string, string, bool, bool) (*types.DSCPlan, error) {
		called = true
		return &types.DSCPlan{}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "/api/components/dsc/plan?action=hard-reset&keepEnabled=true", nil)
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

// A reset's plan says whether it keeps the components the user turned on:
// keepEnabled is required for soft-reset and hard-reset, and passed on.
func TestDSCResetPlanNeedsKeepEnabled(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mux := http.NewServeMux()
	Register(mux)
	type call struct {
		action string
		keep   bool
	}
	var calls []call
	prev := planDSCChange
	t.Cleanup(func() { planDSCChange = prev })
	planDSCChange = func(_ *cluster.Client, action, _, _ string, _, keep bool) (*types.DSCPlan, error) {
		calls = append(calls, call{action, keep})
		return &types.DSCPlan{}, nil
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"action=soft-reset", http.StatusBadRequest},
		{"action=hard-reset&keepEnabled=yes", http.StatusBadRequest},
		{"action=soft-reset&keepEnabled=true", http.StatusOK},
		{"action=hard-reset&keepEnabled=false", http.StatusOK},
		{"action=fix", http.StatusOK},
		{"action=fix&keepEnabled=true", http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/components/dsc/plan?"+tc.query, nil)
		r.Header.Set("X-Forwarded-Access-Token", "user:admin")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: %d %s", tc.query, w.Code, w.Body.String())
		}
		resetPackageState(t)
	}
	want := []call{{"soft-reset", true}, {"hard-reset", false}, {"fix", false}, {"fix", false}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %+v, want %+v", calls, want)
		}
	}
}
