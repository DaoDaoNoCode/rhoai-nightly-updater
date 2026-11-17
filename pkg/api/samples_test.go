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
	for _, h := range []http.HandlerFunc{HandleMLflowSamplesStatus, HandlePipelineSampleStatus} {
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

func TestPipelineSampleStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	orig := runPipelineSample
	t.Cleanup(func() { runPipelineSample = orig })
	var got types.PipelineSampleRequest
	runPipelineSample = func(_ *cluster.Client, req types.PipelineSampleRequest, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got = req
		emit(cluster.UpdateStepEvent{Step: "run", Status: "success", Message: "Started run r1"})
		return &types.OperationResponse{Success: true, Message: "started"}, nil
	}
	for _, body := range []string{`{`, `{"project":"p","action":"delete"}`, `{"project":"","action":"run"}`} {
		w := httptest.NewRecorder()
		HandlePipelineSampleStream(w, postJSON("/api/resources/samples/pipeline/stream", "tok", body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	HandlePipelineSampleStream(w, postJSON("/api/resources/samples/pipeline/stream", "tok", `{"project":"p","action":"run"}`))
	if out := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(out, `"step":"run"`) || !strings.Contains(out, `"step":"operation_complete"`) || got.Action != "run" || got.Project != "p" {
		t.Errorf("%d %s %+v", w.Code, out, got)
	}
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
