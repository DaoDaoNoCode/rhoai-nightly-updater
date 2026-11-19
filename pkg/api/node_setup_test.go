package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Both endpoints refuse an unknown action, and a stream that changes an
// existing object refuses without the version the plan was made on, before
// any cluster call.
func TestNodeSetupValidatesTheRequest(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	called := false
	origPlan, origRun := planNodeSetup, runNodeSetup
	t.Cleanup(func() { planNodeSetup, runNodeSetup = origPlan, origRun })
	planNodeSetup = func(*cluster.Client, string) (*types.NodeSetupPlan, error) {
		called = true
		return &types.NodeSetupPlan{}, nil
	}
	runNodeSetup = func(*cluster.Client, types.NodeSetupRequest, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		called = true
		return &types.OperationResponse{Success: true}, nil
	}
	for _, tc := range []struct {
		handler func(w *httptest.ResponseRecorder, body string)
		body    string
	}{
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupPlan(w, postJSON("/api/setup/nodes/plan", "tok", b))
		}, `{}`},
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupPlan(w, postJSON("/api/setup/nodes/plan", "tok", b))
		}, `{"action":"delete-everything"}`},
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupPlan(w, postJSON("/api/setup/nodes/plan", "tok", b))
		}, `{"action":"mirror-create","uid":"a/b"}`},
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupStream(w, postJSON("/api/setup/nodes/stream", "tok", b))
		}, `{"action":"pull-secret-add"}`},
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupStream(w, postJSON("/api/setup/nodes/stream", "tok", b))
		}, `{"action":"mirror-remove","uid":"u"}`},
		{func(w *httptest.ResponseRecorder, b string) {
			HandleNodeSetupStream(w, postJSON("/api/setup/nodes/stream", "tok", b))
		}, `not json`},
	} {
		w := httptest.NewRecorder()
		tc.handler(w, tc.body)
		if w.Code != 400 {
			t.Errorf("%s: status %d %s", tc.body, w.Code, w.Body.String())
		}
		if clusterMutationInProgress.Load() {
			t.Fatalf("%s: the lock was not released", tc.body)
		}
	}
	if called {
		t.Fatal("a refused request reached the cluster code")
	}
}

// The plan is a dry run: it answers JSON and does not use up the user's
// slot, so the change can follow at once.
func TestNodeSetupPlanThenStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	origPlan, origRun := planNodeSetup, runNodeSetup
	t.Cleanup(func() { planNodeSetup, runNodeSetup = origPlan, origRun })
	planNodeSetup = func(_ *cluster.Client, action string) (*types.NodeSetupPlan, error) {
		return &types.NodeSetupPlan{Action: action, Ready: true, ResourceVersion: "42"}, nil
	}
	var got types.NodeSetupRequest
	runNodeSetup = func(_ *cluster.Client, req types.NodeSetupRequest, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got = req
		emit(cluster.UpdateStepEvent{Step: "apply", Status: "success", Message: "done"})
		return &types.OperationResponse{Success: true, Message: "Added."}, nil
	}
	w := httptest.NewRecorder()
	HandleNodeSetupPlan(w, postJSON("/api/setup/nodes/plan", "tok", `{"action":"pull-secret-add"}`))
	var plan types.NodeSetupPlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.ResourceVersion != "42" {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	HandleNodeSetupStream(w, postJSON("/api/setup/nodes/stream", "tok", `{"action":"pull-secret-add","resourceVersion":"42"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"step":"operation_complete"`) || !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("stream: %d %s", w.Code, w.Body.String())
	}
	if got.Action != "pull-secret-add" || got.ResourceVersion != "42" {
		t.Fatalf("request %+v", got)
	}
}
