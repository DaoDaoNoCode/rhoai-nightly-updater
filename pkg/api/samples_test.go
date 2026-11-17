package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestSamplesStatusNeedsAValidProject(t *testing.T) {
	setupDevMode(t)
	for _, h := range []http.HandlerFunc{HandleMLflowSamplesStatus} {
		for _, q := range []string{"", "?project=", "?project=Bad_Name", "?project=a/b"} {
			req := httptest.NewRequest(http.MethodGet, "/api/resources/samples/x"+q, nil)
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%q: status %d, want 400", q, rec.Code)
			}
		}
	}
}

func stubMLflowSamples(t *testing.T, fn func(*cluster.Client, types.MLflowSamplesRequest, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error)) {
	t.Helper()
	orig := runMLflowSamples
	runMLflowSamples = fn
	t.Cleanup(func() { runMLflowSamples = orig })
}

func TestMLflowSamplesStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	var got types.MLflowSamplesRequest
	stubMLflowSamples(t, func(_ *cluster.Client, req types.MLflowSamplesRequest, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got = req
		emit(cluster.UpdateStepEvent{Step: cluster.SampleTraining, Status: "success", Message: "added"})
		return &types.OperationResponse{Success: true, Message: "Samples added"}, nil
	})

	for _, body := range []string{`{`, `{"project":"Bad","action":"add","samples":["training"]}`, `{"project":"p","action":"remove","samples":["genai-demo"]}`, `{"project":"p","action":"add","samples":[]}`} {
		w := httptest.NewRecorder()
		HandleMLflowSamplesStream(w, postJSON("/api/resources/samples/mlflow/stream", "tok", body))
		if w.Code != http.StatusBadRequest || clusterMutationInProgress.Load() {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	HandleMLflowSamplesStream(w, postJSON("/api/resources/samples/mlflow/stream", "tok", `{"project":"p","action":"add","samples":["training","mcp-server"]}`))
	out := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(out, `"step":"training"`) || !strings.Contains(out, `"step":"operation_complete"`) || !strings.Contains(out, `"status":"success"`) {
		t.Errorf("%d %s", w.Code, out)
	}
	if got.Project != "p" || got.Action != "add" || len(got.Samples) != 2 {
		t.Errorf("request %+v", got)
	}
	if clusterMutationInProgress.Load() {
		t.Error("lock held after the operation")
	}
}
