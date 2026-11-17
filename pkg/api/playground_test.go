package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeGenAIToken makes genAIToken return a known token for the test.
func fakeGenAIToken(t *testing.T) {
	t.Helper()
	orig := genAIToken
	t.Cleanup(func() { genAIToken = orig })
	genAIToken = func(*http.Request) string { return "sa-token" }
}

func TestPlaygroundStream(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	fakeGenAIToken(t)
	orig := runPlayground
	t.Cleanup(func() { runPlayground = orig })
	var got types.PlaygroundRequest
	var gotToken string
	runPlayground = func(_ *cluster.Client, req types.PlaygroundRequest, token string, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		got, gotToken = req, token
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
		HandlePlaygroundStream(w, postJSON("/api/resources/playground/stream", "tok", body))
		if w.Code != http.StatusBadRequest || clusterMutationInProgress.Load() {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	HandlePlaygroundStream(w, postJSON("/api/resources/playground/stream", "tok", `{"action":"setup","project":"p","models":[{"name":"m","source":"namespace"}]}`))
	out := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(out, `"step":"install"`) || !strings.Contains(out, `"step":"operation_complete"`) {
		t.Errorf("%d %s", w.Code, out)
	}
	if got.Action != "setup" || got.Project != "p" || len(got.Models) != 1 || gotToken != "sa-token" {
		t.Errorf("request %+v, token %q", got, gotToken)
	}
	if strings.Contains(out, "sa-token") {
		t.Error("the stream shows the token")
	}
	if clusterMutationInProgress.Load() {
		t.Error("lock held after the operation")
	}
}

func TestPlaygroundStatusHandler(t *testing.T) {
	setupDevMode(t)
	fakeGenAIToken(t)
	orig := getPlaygroundStatus
	t.Cleanup(func() { getPlaygroundStatus = orig })
	var gotProject, gotToken string
	getPlaygroundStatus = func(_ *cluster.Client, token, project string) (*types.PlaygroundStatus, error) {
		gotProject, gotToken = project, token
		return &types.PlaygroundStatus{Project: project}, nil
	}
	w := httptest.NewRecorder()
	HandlePlaygroundStatus(w, postJSONMethod(http.MethodGet, "/api/resources/playground?project=Bad_Name", "tok", ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad project: %d", w.Code)
	}
	w = httptest.NewRecorder()
	HandlePlaygroundStatus(w, postJSONMethod(http.MethodGet, "/api/resources/playground?project=proj", "tok", ""))
	if w.Code != http.StatusOK || gotProject != "proj" || gotToken != "sa-token" || strings.Contains(w.Body.String(), "sa-token") {
		t.Errorf("%d %s (project %q, token %q)", w.Code, w.Body.String(), gotProject, gotToken)
	}
}

// The token for the gen-ai API is the ServiceAccount's (the cluster
// token), never the user's session token.
func TestGenAITokenIsTheClusterToken(t *testing.T) {
	setupDevMode(t)
	r := postJSON("/api/resources/playground/stream", "user-session-token", `{}`)
	r.Header.Set("X-Forwarded-Access-Token", "user-session-token")
	if got := genAIToken(r); got != getClusterToken() || got == "user-session-token" {
		t.Errorf("genAIToken = %q", got)
	}
}
