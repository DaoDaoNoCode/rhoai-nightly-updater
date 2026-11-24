package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeCleanUp replaces the clean-up for one test and records its requests.
func fakeCleanUp(t *testing.T, run func(req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse) *[]types.CleanupRequest {
	t.Helper()
	orig := runCleanUp
	t.Cleanup(func() { runCleanUp = orig })
	var got []types.CleanupRequest
	runCleanUp = func(_ *cluster.Client, req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got = append(got, req)
		return run(req, onStep), nil
	}
	return &got
}

func TestCleanupStream_RefusesBadRequestsBeforeAnything(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
		return &types.OperationResponse{Success: true}
	})
	for name, body := range map[string]string{
		"nothing selected":           `{}`,
		"invalid project":            `{"pipelineServers":["Bad_Name"]}`,
		"a project twice":            `{"pipelineServers":["a","a"]}`,
		"namespace without storage":  `{"mlflow":true,"deleteNamespace":true}`,
		"not JSON":                   `{`,
		"too many pipeline servers":  `{"pipelineServers":[` + strings.Repeat(`"p",`, maxCleanupProjects) + `"p"]}`,
		"invalid model project":      `{"models":["Bad_Name"]}`,
		"a model project twice":      `{"models":["a","a"],"inferenceGateway":true}`,
		"too many models":            `{"models":[` + strings.Repeat(`"p",`, maxCleanupProjects) + `"p"]}`,
		"invalid playground project": `{"playgrounds":["Bad_Name"]}`,
		"a playground project twice": `{"playgrounds":["a","a"]}`,
	} {
		w := httptest.NewRecorder()
		HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:"+strings.ReplaceAll(name, " ", "-"), body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused request ran the clean-up: %+v", *calls)
	}
}

func TestCleanupStream_StreamsEachPartAsOneOperation(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeCleanUp(t, func(req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse {
		for _, p := range req.PipelineServers {
			onStep(cluster.UpdateStepEvent{Step: cluster.CleanupStepID("pipeline-server", p), Status: "success", Message: "Pipeline server removed from '" + p + "'."})
		}
		onStep(cluster.UpdateStepEvent{Step: "storage", Status: "success", Message: "S3 storage removed."})
		return &types.OperationResponse{Success: true, Message: "Deleted everything."}
	})
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:alice", `{"pipelineServers":["a","b"],"storage":true,"deleteNamespace":true}`))
	body := w.Body.String()
	for _, want := range []string{`"step":"pipeline-server:a"`, `"step":"pipeline-server:b"`, `"step":"storage"`, `"step":"operation_complete","status":"success","message":"Deleted everything."`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
	if len(*calls) != 1 || !(*calls)[0].DeleteNamespace || len((*calls)[0].PipelineServers) != 2 {
		t.Fatalf("got %+v", *calls)
	}
	// One request is one rate-limit slot: a second clean-up right after is refused.
	w = httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:alice", `{"mlflow":true}`))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second clean-up within the window: %d", w.Code)
	}
}

func TestCleanupStream_TakesTheClusterLock(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
		return &types.OperationResponse{Success: true}
	})
	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	defer releaseClusterMutationLock()
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:bob", `{"mlflow":true}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cluster_busy") {
		t.Fatalf("while another operation runs: %d %s", w.Code, w.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatal("the clean-up ran without the lock")
	}
}

func TestCleanupStream_NeedsMutationPermission(t *testing.T) {
	setupDevMode(t)
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
		t.Fatal("ran without permission")
		return nil
	})
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:reader", `{"mlflow":true}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// C1: a clean-up of workbenches only (Clean up Workbench), or of kept
// workbench storage only, is a selection, not "nothing selected".
func TestCleanupStream_WorkbenchesOnly(t *testing.T) {
	for _, body := range []string{`{"workbenches":["a"]}`, `{"workbenchStorage":["a"]}`, `{"workbenches":["a"],"workbenchStorage":["a"]}`} {
		t.Run(body, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			calls := fakeCleanUp(t, func(req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse {
				return &types.OperationResponse{Success: true, Message: "Deleted the workbench."}
			})
			w := httptest.NewRecorder()
			HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:wb-"+strings.Trim(strings.ReplaceAll(body, `"`, ""), "{}"), body))
			if w.Code != http.StatusOK || len(*calls) != 1 {
				t.Fatalf("got %d %s, calls %+v", w.Code, w.Body.String(), *calls)
			}
			if !strings.Contains(w.Body.String(), `"operation_complete","status":"success"`) {
				t.Fatalf("stream: %s", w.Body.String())
			}
		})
	}
}
