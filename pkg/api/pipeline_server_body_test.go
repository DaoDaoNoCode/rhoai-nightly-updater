package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// stubTeardown substitutes for pipeline server teardown and captures
// every confirmation map passed through.
func stubTeardown(t *testing.T) *[]map[string]string {
	t.Helper()
	orig := runTeardownPipelineServer
	t.Cleanup(func() { runTeardownPipelineServer = orig })
	var got []map[string]string
	runTeardownPipelineServer = func(_ *cluster.Client, _, _ string, wbs map[string]string) (*types.OperationResponse, error) {
		got = append(got, wbs)
		return &types.OperationResponse{Success: true, Message: "Deleted."}, nil
	}
	return &got
}

// maxConfirmation fills the envelope: maxConfirmWorkbenches entries, each
// carrying the maximum allowed name and UID length.
func maxConfirmation() string {
	entries := make([]string, 0, maxConfirmWorkbenches)
	for i := range maxConfirmWorkbenches {
		name := fmt.Sprintf("%03d", i) + strings.Repeat("n", maxNotebookNameLength-3)
		entries = append(entries, fmt.Sprintf("%q:%q", name, strings.Repeat("u", maxUIDLength)))
	}
	return "{" + strings.Join(entries, ",") + "}"
}

// padTo grows a JSON object body to n bytes by inserting spaces after the opening brace.
func padTo(t *testing.T, body string, n int) string {
	t.Helper()
	if len(body) > n {
		t.Fatalf("body is %d bytes, more than %d", len(body), n)
	}
	return body[:1] + strings.Repeat(" ", n-len(body)) + body[1:]
}

// Teardown accepts a body as large as the biggest valid confirmation allows and
// rejects payloads exactly one byte larger, with no server-side mutation.
func TestPipelineServerTeardown_BodyLimit(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	got := stubTeardown(t)
	body := `{"project":"proj","confirmWorkbenches":` + maxConfirmation() + `}`
	for name, tc := range map[string]struct {
		size int
		want int
	}{
		"at the limit":  {pipelineServerBodyLimit, http.StatusOK},
		"one byte over": {pipelineServerBodyLimit + 1, http.StatusBadRequest},
	} {
		before := len(*got)
		w := httptest.NewRecorder()
		HandlePipelineServerTeardown(w, postJSON(t, "/api/resources/pipeline-server/teardown", "user:"+strings.ReplaceAll(name, " ", "-"), padTo(t, body, tc.size)))
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		ran := len(*got) > before
		if ran != (tc.want == http.StatusOK) || ran && len((*got)[len(*got)-1]) != maxConfirmWorkbenches {
			t.Fatalf("%s: teardown ran %v", name, ran)
		}
	}
}

// Out-of-range confirmations yield bad request without executing teardown.
func TestPipelineServerTeardown_RefusesBadConfirmations(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	got := stubTeardown(t)
	tooMany := make([]string, 0, maxConfirmWorkbenches+1)
	for i := range maxConfirmWorkbenches + 1 {
		tooMany = append(tooMany, fmt.Sprintf(`"wb-%d":"uid"`, i))
	}
	for name, confirm := range map[string]string{
		"too many":      "{" + strings.Join(tooMany, ",") + "}",
		"bad name":      `{"Bad_Name":"uid"}`,
		"name too long": fmt.Sprintf(`{%q:"uid"}`, strings.Repeat("a", maxNotebookNameLength+1)),
		"no UID":        `{"wb":""}`,
		"UID too long":  fmt.Sprintf(`{"wb":%q}`, strings.Repeat("u", maxUIDLength+1)),
		"a list":        `["wb"]`,
	} {
		w := httptest.NewRecorder()
		HandlePipelineServerTeardown(w, postJSON(t, "/api/resources/pipeline-server/teardown", "user:"+strings.ReplaceAll(name, " ", "-"), `{"project":"proj","confirmWorkbenches":`+confirm+`}`))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(*got) != 0 {
		t.Fatalf("a refused request ran the teardown: %v", *got)
	}
}

// Cleanup stream sizing mirrors teardown: largest permissible workbench map
// fits; one additional byte triggers rejection.
func TestCleanupStream_BodyLimit(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
		return &types.OperationResponse{Success: true}
	})
	body := `{"pipelineServers":["proj"],"confirmWorkbenches":{"proj":` + maxConfirmation() + `}}`
	for name, tc := range map[string]struct {
		size int
		want int
	}{
		"at the limit":  {cleanupBodyLimit, http.StatusOK},
		"one byte over": {cleanupBodyLimit + 1, http.StatusBadRequest},
	} {
		before := len(*calls)
		w := httptest.NewRecorder()
		HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:"+strings.ReplaceAll(name, " ", "-"), padTo(t, body, tc.size)))
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if ran := len(*calls) > before; ran != (tc.want == http.StatusOK) {
			t.Fatalf("%s: clean-up ran %v", name, ran)
		}
	}
	// Confirm workbench keys must belong to a pipeline server listed for cleanup.
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:other", `{"pipelineServers":["a"],"confirmWorkbenches":{"b":{"wb":"uid"}}}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("other project: %d", w.Code)
	}
}
