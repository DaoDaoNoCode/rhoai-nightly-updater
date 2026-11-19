package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The playground's gen-ai calls carry the token of the client the handler
// gets (pkg/cluster playgroundAPI): the user's own for Set up and delete,
// the ServiceAccount's for the status.
func TestPlaygroundStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	orig := runPlayground
	t.Cleanup(func() { runPlayground = orig })
	var got types.PlaygroundRequest
	asUser := false
	runPlayground = func(c *cluster.Client, req types.PlaygroundRequest, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got, asUser = req, c.ActsAsUser()
		emit(cluster.UpdateStepEvent{Step: "install", Status: "success", Message: "asked"})
		return &types.OperationResponse{Success: true, Message: "ready"}, nil
	}
	for _, body := range []string{
		`{`,
		`{"action":"setup"}`,
		`{"action":"remove","project":"p"}`,
		`{"action":"setup","project":"Bad_Name"}`,
		`{"action":"setup","project":"p","models":[{"name":"m","source":"custom"}]}`,
		`{"action":"teardown","project":"p","models":[{"name":"m","source":"maas"}]}`,
	} {
		w := httptest.NewRecorder()
		HandlePlaygroundStream(w, postJSON("/api/resources/playground/stream", "user-token", body))
		if w.Code != http.StatusBadRequest || clusterMutationInProgress.Load() {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	// A Models as a Service model is accepted again: Set up runs with the
	// user's own token.
	HandlePlaygroundStream(w, postJSON("/api/resources/playground/stream", "user-token", `{"action":"setup","project":"p","models":[{"name":"m","source":"namespace"},{"name":"gpt","source":"maas"}]}`))
	out := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(out, `"step":"install"`) || !strings.Contains(out, `"step":"operation_complete"`) {
		t.Errorf("%d %s", w.Code, out)
	}
	if got.Action != "setup" || got.Project != "p" || len(got.Models) != 2 || !asUser {
		t.Errorf("request %+v, client acts as the user: %v", got, asUser)
	}
	if strings.Contains(out, "user-token") || strings.Contains(out, "test-cluster-token") {
		t.Error("the stream shows the token")
	}
	if clusterMutationInProgress.Load() {
		t.Error("lock held after the operation")
	}
}

func TestPlaygroundStatusHandler(t *testing.T) {
	setupDevMode(t)
	orig := getPlaygroundStatus
	t.Cleanup(func() { getPlaygroundStatus = orig })
	var gotProject string
	asUser := true
	getPlaygroundStatus = func(c *cluster.Client, project string) (*types.PlaygroundStatus, error) {
		gotProject, asUser = project, c.ActsAsUser()
		return &types.PlaygroundStatus{Project: project}, nil
	}
	w := httptest.NewRecorder()
	HandlePlaygroundStatus(w, postJSONMethod(http.MethodGet, "/api/resources/playground?project=Bad_Name", "tok", ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad project: %d", w.Code)
	}
	w = httptest.NewRecorder()
	HandlePlaygroundStatus(w, postJSONMethod(http.MethodGet, "/api/resources/playground?project=proj", "tok", ""))
	// The status reads with the ServiceAccount's client (so with its
	// token, the gen-ai calls too), never the user's.
	if w.Code != http.StatusOK || gotProject != "proj" || asUser || strings.Contains(w.Body.String(), "test-cluster-token") {
		t.Errorf("%d %s (project %q, client acts as the user: %v)", w.Code, w.Body.String(), gotProject, asUser)
	}
}
