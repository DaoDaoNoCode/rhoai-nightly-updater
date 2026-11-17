package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestModelsStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	orig := runModels
	t.Cleanup(func() { runModels = orig })
	var got types.ModelsRequest
	runModels = func(_ *cluster.Client, req types.ModelsRequest, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got = req
		emit(cluster.UpdateStepEvent{Step: "gateway", Status: "success", Message: "Inference gateway applied"})
		emit(cluster.UpdateStepEvent{Step: "model", Status: "success", Message: "Model applied"})
		return &types.OperationResponse{Success: true, Message: "ready"}, nil
	}
	for _, body := range []string{
		`{`,
		`{"action":"setup"}`,
		`{"action":"remove","gateway":true}`,
		`{"action":"setup","project":"Bad_Name"}`,
		`{"action":"setup","project":"p","model":"llama"}`,
		`{"action":"teardown","project":"p","releasedImage":true}`,
	} {
		w := httptest.NewRecorder()
		HandleModelsStream(w, postJSON("/api/resources/models/stream", "tok", body))
		if w.Code != http.StatusBadRequest || clusterMutationInProgress.Load() {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	HandleModelsStream(w, postJSON("/api/resources/models/stream", "tok", `{"action":"setup","gateway":true,"project":"p","releasedImage":true}`))
	out := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(out, `"step":"gateway"`) || !strings.Contains(out, `"step":"model"`) || !strings.Contains(out, `"step":"operation_complete"`) {
		t.Errorf("%d %s", w.Code, out)
	}
	if got.Action != "setup" || !got.Gateway || got.Project != "p" || !got.ReleasedImage {
		t.Errorf("request %+v", got)
	}
	if clusterMutationInProgress.Load() {
		t.Error("lock held after the operation")
	}
}
