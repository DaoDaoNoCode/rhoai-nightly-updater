package cluster

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func notebookPath(ns, name string) string {
	return "/apis/kubeflow.org/v1/namespaces/" + ns + "/notebooks/" + name
}

// putNotebook seeds the fake with a workbench: conns becomes the connections
// annotation (pass "" to omit it); envFrom is whatever the webhook would emit.
func putNotebook(f *resourceFake, ns, name, conns string) {
	ann := `{}`
	if conns != "" {
		ann = `{"` + storage.NotebookConnectionsAnnotation + `":"` + conns + `"}`
	}
	f.putJSON(notebookPath(ns, name), `{"metadata":{"name":"`+name+`","namespace":"`+ns+`","annotations":`+ann+`},
		"spec":{"template":{"spec":{"containers":[{"name":"`+name+`","envFrom":[{"secretRef":{"name":"`+storage.ConnectionSecretName+`"}}]}]}}}}`)
}

func notebookAnnotation(f *resourceFake, name string) (string, bool) {
	v, ok := ensureMap(ensureMap(f.get(notebookPath("proj", name)), "metadata"), "annotations")[storage.NotebookConnectionsAnnotation].(string)
	return v, ok
}

// putConnectionUsers lays down the tool's pipeline server in proj with its
// Connection, and workbenches: wb-a uses it among other Connections, wb-b
// only it, wb-c another Connection only; other/wb-d names proj's
// Connection (the webhook would refuse that; it must be left alone).
func putConnectionUsers(f *resourceFake) {
	putDSPO(f, 1)
	putDSPA(f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	f.putJSON(secretPath("proj", storage.DSPASecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(secretPath("proj", storage.ConnectionSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	putNotebook(f, "proj", "wb-a", "proj/other, proj/"+storage.ConnectionSecretName+",proj/third")
	putNotebook(f, "proj", "wb-b", "proj/"+storage.ConnectionSecretName)
	putNotebook(f, "proj", "wb-c", "proj/other")
	putNotebook(f, "other", "wb-d", "proj/"+storage.ConnectionSecretName)
}

// Pipeline-server deletion must detach its Connection from dependent workbenches
// first—otherwise the webhook rejects updates that reference a missing Secret.
// Remove only that reference, preserve the rest, clear the annotation when empty,
// leave envFrom alone (webhook-owned), act under the user's token, then delete
// the DSPA.
func TestTeardownPipelineServer_RemovesTheConnectionFromItsWorkbenchesFirst(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	u := userClientOf(c, fakeUserToken)
	resp, err := TeardownPipelineServer(u, "proj", toolDSPAUID(c, "proj"), []string{"wb-a", "wb-b"})
	if err != nil || !resp.Success {
		t.Fatalf("got %+v, %v", resp, err)
	}
	if got, _ := notebookAnnotation(f, "wb-a"); got != "proj/other,proj/third" {
		t.Errorf("wb-a keeps its other Connections: got %q", got)
	}
	if got, ok := notebookAnnotation(f, "wb-b"); ok {
		t.Errorf("wb-b's annotation must go when nothing is left, got %q", got)
	}
	for _, nb := range []string{"wb-a", "wb-b"} {
		env := f.get(notebookPath("proj", nb))["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["envFrom"]
		if env == nil {
			t.Errorf("%s: envFrom is the webhook's to change", nb)
		}
		patches := f.callsTo(http.MethodPatch, notebookPath("proj", nb))
		if len(patches) != 1 || patches[0].Token != fakeUserToken || patches[0].ContentType != "application/json-patch+json" {
			t.Fatalf("%s: patches %+v", nb, patches)
		}
		for _, test := range []string{`"op":"test","path":"/metadata/uid"`, `"op":"test","path":"/metadata/resourceVersion"`, `"op":"test","path":"/metadata/annotations/opendatahub.io~1connections"`} {
			if !strings.Contains(patches[0].Body, test) {
				t.Errorf("%s: patch %s lacks %s", nb, patches[0].Body, test)
			}
		}
		if i, d := requestIndex(f, "PATCH "+notebookPath("proj", nb)), requestIndex(f, "DELETE "+dspaPath("proj", storage.DSPAName)); i < 0 || d < 0 || i > d {
			t.Errorf("%s must lose the Connection before the DSPA is deleted (patch %d, delete %d)", nb, i, d)
		}
	}
	if got, _ := notebookAnnotation(f, "wb-c"); got != "proj/other" || hasMutation(f, "PATCH "+notebookPath("proj", "wb-c")) {
		t.Errorf("wb-c does not use the Connection and must be left alone: %q", got)
	}
	if hasMutation(f, "PATCH "+notebookPath("other", "wb-d")) {
		t.Error("a workbench of another project must be left alone")
	}
	if f.has(secretPath("proj", storage.ConnectionSecretName)) {
		t.Error("the Connection must be deleted after")
	}
}

// If a workbench picked up the Connection only after review and never got
// user confirmation, teardown fails immediately with no cluster mutations.
func TestTeardownPipelineServer_UnconfirmedWorkbenchRefusesBeforeAnyChange(t *testing.T) {
	fastPipelineTimings(t)
	for name, confirmed := range map[string][]string{"none": nil, "one of two": {"wb-a"}} {
		t.Run(name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putConnectionUsers(f)
			resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmed)
			if resp.Success || resp.ErrorCode != storage.ErrorCodeConfirmWorkbenches || !strings.Contains(resp.Message, "wb-b") || !strings.Contains(resp.Message, "Nothing was deleted") {
				t.Fatalf("got %+v", resp)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("nothing may change: %v", m)
			}
		})
	}
}

// A follow-up teardown after some workbenches already shed the Connection
// should target only what's left; prior confirmation still suffices.
func TestTeardownPipelineServer_ResumesWithTheSameConfirmation(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	putNotebook(f, "proj", "wb-a", "proj/other")
	if resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), []string{"wb-a", "wb-b"}); !resp.Success {
		t.Fatalf("got %+v", resp)
	}
	if _, ok := notebookAnnotation(f, "wb-b"); ok || hasMutation(f, "PATCH "+notebookPath("proj", "wb-a")) {
		t.Errorf("only wb-b is left to change: %v", f.mutations())
	}
}

// Stale reads abort teardown: when a workbench changes before patch (patch
// tests return the API server's generic 422), nothing is deleted and the
// caller gets a message, while workbenches patched earlier keep their updates.
func TestTeardownPipelineServer_WorkbenchChangedAbortsBeforeDelete(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == notebookPath("proj", "wb-b") {
			f.mu.Lock()
			f.bumpRV(ensureMap(f.objects[fakeKey{gv: "kubeflow.org/v1", ns: "proj", plural: "notebooks", name: "wb-b"}], "metadata"))
			f.mu.Unlock()
		}
	}
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), []string{"wb-a", "wb-b"})
	if resp.Success || resp.ErrorCode != "conflict" || !strings.Contains(resp.Message, "wb-b") || !strings.Contains(resp.Message, "changed since it was read") {
		t.Fatalf("got %+v", resp)
	}
	for _, m := range f.mutations() {
		if strings.HasPrefix(m, "DELETE ") {
			t.Errorf("nothing may be deleted: %s", m)
		}
	}
	if _, ok := notebookAnnotation(f, "wb-b"); !ok {
		t.Error("wb-b must keep its annotation")
	}
}

// Any other refusal of the patch (the webhook denying it, RBAC) stops the
// teardown as well, before anything is deleted.
func TestTeardownPipelineServer_WorkbenchPatchRefusedAbortsBeforeDelete(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	f.fail["PATCH "+notebookPath("proj", "wb-a")] = 403
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), []string{"wb-a", "wb-b"})
	if resp.Success || !strings.Contains(resp.Message, "wb-a") {
		t.Fatalf("got %+v", resp)
	}
	if hasMutation(f, "DELETE ") || hasMutation(f, "PATCH "+notebookPath("proj", "wb-b")) {
		t.Errorf("nothing after the failed patch may run: %v", f.mutations())
	}
}

// The checks fail closed: workbenches that cannot be listed refuse the
// teardown.
func TestTeardownPipelineServer_WorkbenchListFailureRefuses(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	f.fail["GET /apis/kubeflow.org/v1/namespaces/proj/notebooks"] = 403
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), []string{"wb-a", "wb-b"})
	if resp.Success || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("got %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("nothing may change: %v", m)
	}
}

// A rerun after the DSPA went but its Connection stayed (no pipeline
// server left) takes it off the workbenches before deleting it, too.
func TestTeardownPipelineServer_LeftoverConnectionLeavesWorkbenchesFirst(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	f.mu.Lock()
	delete(f.objects, fakeKey{gv: "datasciencepipelinesapplications.opendatahub.io/v1", ns: "proj", plural: "datasciencepipelinesapplications", name: storage.DSPAName})
	f.mu.Unlock()
	if resp, _ := TeardownPipelineServer(c, "proj", "", nil); resp.Success || resp.ErrorCode != storage.ErrorCodeConfirmWorkbenches || hasMutation(f, "DELETE ") {
		t.Fatalf("unconfirmed: %+v", resp)
	}
	if resp, _ := TeardownPipelineServer(c, "proj", "", []string{"wb-a", "wb-b"}); !resp.Success || f.has(secretPath("proj", storage.ConnectionSecretName)) {
		t.Fatalf("confirmed: %+v", resp)
	}
	if _, ok := notebookAnnotation(f, "wb-b"); ok {
		t.Error("wb-b must lose the Connection")
	}
}

// Clean up checks every pipeline server's workbenches before it deletes
// anything; the refused step lists them (one per line) for the UI to ask.
func TestCleanUp_UnconfirmedConnectionWorkbenchesRefuseUpFront(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	var events []UpdateStepEvent
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"proj"}, ConfirmUnrecorded: map[string]string{"proj": toolDSPAUID(c, "proj")}, MLflow: true}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res.Success || res.ErrorCode != storage.ErrorCodeConfirmWorkbenches {
		t.Fatalf("got %+v, %v", res, err)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("nothing may change: %v", m)
	}
	if len(events) != 1 || events[0].Step != "pipeline-server:proj" || events[0].ErrorCode != storage.ErrorCodeConfirmWorkbenches || events[0].Detail != "wb-a\nwb-b" {
		t.Fatalf("events %+v", events)
	}
	// Confirmed, the clean-up hands the confirmation to the teardown.
	var got []string
	defer func(prev func(*Client, string, string, []string) (*types.OperationResponse, error)) {
		cleanupPipelineServer = prev
	}(cleanupPipelineServer)
	cleanupPipelineServer = func(_ *Client, _, _ string, wbs []string) (*types.OperationResponse, error) {
		got = wbs
		return &types.OperationResponse{Success: true}, nil
	}
	res, _ = CleanUp(c, types.CleanupRequest{PipelineServers: []string{"proj"}, ConfirmWorkbenches: map[string][]string{"proj": {"wb-a", "wb-b"}}}, nil)
	if !res.Success || !slices.Equal(got, []string{"wb-a", "wb-b"}) {
		t.Fatalf("got %+v, confirmed %v", res, got)
	}
}

// Deleting a project a recipe created deletes its Notebooks with it (the
// webhook lets updates of a terminating Notebook through): no workbench is
// changed, and no workbench confirmation is asked for.
func TestCleanUp_RecipeProjectDeleteLeavesWorkbenchesAlone(t *testing.T) {
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	defer func(p, d any) {
		recipeProjectProblem = p.(func(*Client, string, string, []string) (string, string, []string, error))
		cleanupProject = d.(func(*Client, string, string, []string) (*types.OperationResponse, error))
	}(recipeProjectProblem, cleanupProject)
	recipeProjectProblem = func(*Client, string, string, []string) (string, string, []string, error) { return "", "", nil, nil }
	cleanupProject = func(*Client, string, string, []string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: true, Message: "Project deleted."}, nil
	}
	res, err := CleanUp(c, types.CleanupRequest{RecipeProjects: []string{"proj"}, RecipeProjectUIDs: map[string]string{"proj": "u"}}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(f.callsTo(http.MethodGet, "/apis/kubeflow.org/v1/namespaces/proj/notebooks")) != 0 || hasMutation(f, "PATCH /apis/kubeflow.org") {
		t.Errorf("no workbench is read or changed: %v", f.mutations())
	}
}

// The status names the workbenches that use the selected project's
// Connection, read with the user's token (the ServiceAccount may not list
// Notebooks); one that cannot be read says so.
func TestStatus_ListsTheConnectionsWorkbenches(t *testing.T) {
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	putDSProject(f, "proj")
	f.putProject("proj")
	u := userClientOf(c, fakeUserToken)
	st, err := GetResourcesStatusFor(c, u, "proj")
	if err != nil || len(st.PipelineServers) != 1 {
		t.Fatalf("got %+v, %v", st, err)
	}
	if ps := st.PipelineServers[0]; !slices.Equal(ps.ConnectionWorkbenches, []string{"wb-a", "wb-b"}) {
		t.Errorf("admin status: %+v", ps)
	}
	for _, call := range f.callsTo(http.MethodGet, "/apis/kubeflow.org/v1/namespaces/proj/notebooks") {
		if call.Token != fakeUserToken {
			t.Errorf("notebooks listed with token %q", call.Token)
		}
	}
	pst, err := GetProjectStatus(c, u, "proj")
	if err != nil || len(pst.PipelineServers) != 1 || !slices.Equal(pst.PipelineServers[0].ConnectionWorkbenches, []string{"wb-a", "wb-b"}) {
		t.Fatalf("project status: %+v, %v", pst, err)
	}
	f.failAs[fakeUserToken+" GET /apis/kubeflow.org/v1/namespaces/proj/notebooks"] = 403
	st, _ = GetResourcesStatusFor(c, u, "proj")
	if ps := st.PipelineServers[0]; ps.ConnectionWorkbenches != nil || !strings.Contains(ps.ConnectionWorkbenchesMessage, "may not read the workbenches") {
		t.Errorf("unreadable: %+v", ps)
	}
}
