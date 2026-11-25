package api

import (
	"encoding/json"
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
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/samples/x"+q, nil)
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

// The sample pipeline is served as a file for the user to run with their
// own token; there is no route that runs it with the app's.
func TestPipelineSampleSpecRoute(t *testing.T) {
	setupDevMode(t)
	w := httptest.NewRecorder()
	HandlePipelineSampleSpec(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/samples/pipeline/spec", nil))
	var got types.PipelineSampleSpec
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.FileName != "rhoai-nightly-sample.yaml" || !strings.Contains(got.YAML, "schemaVersion: 2.1.0") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, r := range routes {
		if strings.HasPrefix(r.Pattern, "POST /api/resources/samples/pipeline") || r.Pattern == "GET /api/resources/samples/pipeline" {
			t.Errorf("route %s runs the sample pipeline with the app's token", r.Pattern)
		}
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
		HandleMLflowSamplesStream(w, postJSON(t, "/api/resources/samples/mlflow/stream", "tok", body))
		if w.Code != http.StatusBadRequest || locks.clusterHeld() {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	HandleMLflowSamplesStream(w, postJSON(t, "/api/resources/samples/mlflow/stream", "tok", `{"project":"p","action":"add","samples":["training","mcp-server"]}`))
	out := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(out, `"step":"training"`) || !strings.Contains(out, `"step":"operation_complete"`) || !strings.Contains(out, `"status":"success"`) {
		t.Errorf("%d %s", w.Code, out)
	}
	if got.Project != "p" || got.Action != "add" || len(got.Samples) != 2 {
		t.Errorf("request %+v", got)
	}
	if locks.clusterHeld() {
		t.Error("lock held after the operation")
	}
}
