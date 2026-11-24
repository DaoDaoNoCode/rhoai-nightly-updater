package cluster

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
	env := `[]`
	if strings.Contains(conns, storage.ConnectionSecretName) {
		env = `[{"secretRef":{"name":"` + storage.ConnectionSecretName + `"}}]`
	}
	putNotebookRaw(f, ns, name, ann, env)
}

// putNotebookRaw stores a workbench with the annotations and envFrom given
// as JSON.
func putNotebookRaw(f *resourceFake, ns, name, annotations, envFrom string) {
	f.putJSON(notebookPath(ns, name), `{"metadata":{"name":"`+name+`","namespace":"`+ns+`","uid":"uid-`+name+`","annotations":`+annotations+`},
		"spec":{"template":{"spec":{"containers":[{"name":"`+name+`","envFrom":`+envFrom+`}]}}}}`)
}

// confirmBoth confirms wb-a and wb-b as putConnectionUsers made them.
func confirmBoth() map[string]string {
	return map[string]string{"wb-a": "uid-wb-a", "wb-b": "uid-wb-b"}
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
	resp, err := TeardownPipelineServer(u, "proj", toolDSPAUID(c, "proj"), confirmBoth())
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
		// A server-side dry run first, then the change, both with the user's token.
		patches := f.callsTo(http.MethodPatch, notebookPath("proj", nb))
		if len(patches) != 2 || patches[0].Query.Get("dryRun") != "All" || patches[1].Query.Get("dryRun") != "" {
			t.Fatalf("%s: patches %+v", nb, patches)
		}
		for _, p := range patches {
			if p.Token != fakeUserToken || p.ContentType != "application/json-patch+json" {
				t.Fatalf("%s: patch %+v", nb, p)
			}
		}
		for _, test := range []string{`"op":"test","path":"/metadata/uid"`, `"op":"test","path":"/metadata/resourceVersion"`, `"op":"test","path":"/metadata/annotations/opendatahub.io~1connections"`} {
			if !strings.Contains(patches[1].Body, test) {
				t.Errorf("%s: patch %s lacks %s", nb, patches[1].Body, test)
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
	for name, confirmed := range map[string]map[string]string{"none": nil, "one of two": {"wb-a": "uid-wb-a"}, "made again since": {"wb-a": "uid-wb-a", "wb-b": "uid-before"}} {
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
	if resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth()); !resp.Success {
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
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth())
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
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth())
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
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth())
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
	if resp, _ := TeardownPipelineServer(c, "proj", "", confirmBoth()); !resp.Success || f.has(secretPath("proj", storage.ConnectionSecretName)) {
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
	if len(events) != 1 || events[0].Step != "pipeline-server:proj" || events[0].ErrorCode != storage.ErrorCodeConfirmWorkbenches || events[0].Detail != "wb-a=uid-wb-a\nwb-b=uid-wb-b" {
		t.Fatalf("events %+v", events)
	}
	// Confirmed, the clean-up hands the confirmation to the teardown.
	var got map[string]string
	defer func(prev func(*Client, string, string, map[string]string) (*types.OperationResponse, error)) {
		cleanupPipelineServer = prev
	}(cleanupPipelineServer)
	cleanupPipelineServer = func(_ *Client, _, _ string, wbs map[string]string) (*types.OperationResponse, error) {
		got = wbs
		return &types.OperationResponse{Success: true}, nil
	}
	res, _ = CleanUp(c, types.CleanupRequest{PipelineServers: []string{"proj"}, ConfirmWorkbenches: map[string]map[string]string{"proj": confirmBoth()}}, nil)
	if !res.Success || !maps.Equal(got, confirmBoth()) {
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
	bothRefs := []types.ConnectionWorkbench{{Name: "wb-a", UID: "uid-wb-a", Running: true}, {Name: "wb-b", UID: "uid-wb-b"}}
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	putDSProject(f, "proj")
	f.putProject("proj")
	// wb-b is stopped; wb-env loads the Connection without naming it.
	putNotebookRaw(f, "proj", "wb-b", `{"`+storage.NotebookConnectionsAnnotation+`":"proj/`+storage.ConnectionSecretName+`","`+storage.NotebookStoppedAnnotation+`":"2026-11-24T10:00:00Z"}`, `[]`)
	putNotebookRaw(f, "proj", "wb-env", `{}`, `[{"secretRef":{"name":"`+storage.ConnectionSecretName+`"}}]`)
	u := userClientOf(c, fakeUserToken)
	st, err := GetResourcesStatusFor(c, u, "proj")
	if err != nil || len(st.PipelineServers) != 1 {
		t.Fatalf("got %+v, %v", st, err)
	}
	if ps := st.PipelineServers[0]; !slices.Equal(ps.ConnectionWorkbenches, bothRefs) || !slices.Equal(ps.ConnectionEnvOnlyWorkbenches, []string{"wb-env"}) {
		t.Errorf("admin status: %+v", ps)
	}
	for _, call := range f.callsTo(http.MethodGet, "/apis/kubeflow.org/v1/namespaces/proj/notebooks") {
		if call.Token != fakeUserToken {
			t.Errorf("notebooks listed with token %q", call.Token)
		}
	}
	pst, err := GetProjectStatus(c, u, "proj")
	if err != nil || len(pst.PipelineServers) != 1 || !slices.Equal(pst.PipelineServers[0].ConnectionWorkbenches, bothRefs) {
		t.Fatalf("project status: %+v, %v", pst, err)
	}
	f.failAs[fakeUserToken+" GET /apis/kubeflow.org/v1/namespaces/proj/notebooks"] = 403
	st, _ = GetResourcesStatusFor(c, u, "proj")
	if ps := st.PipelineServers[0]; ps.ConnectionWorkbenches != nil || !strings.Contains(ps.ConnectionWorkbenchesMessage, "may not read the workbenches") {
		t.Errorf("unreadable: %+v", ps)
	}
}

// attachDuringPatches makes wb-new attach the Connection while the
// teardown patches wb-b (after its list was read, before the delete).
func attachDuringPatches(f *resourceFake) {
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == notebookPath("proj", "wb-b") {
			putNotebook(f, "proj", "wb-new", "proj/"+storage.ConnectionSecretName)
		}
	}
}

// A workbench that attaches the Connection after the list was read is
// found by a second read right before the delete: the teardown refuses,
// deleting nothing, and names it.
func TestTeardownPipelineServer_WorkbenchAttachingDuringTheDeletionRefuses(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	attachDuringPatches(f)
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth())
	if resp.Success || resp.ErrorCode != storage.ErrorCodeConfirmWorkbenches || !strings.Contains(resp.Message, "wb-new") || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("got %+v", resp)
	}
	if hasMutation(f, "DELETE ") || !f.has(secretPath("proj", storage.ConnectionSecretName)) {
		t.Errorf("nothing may be deleted: %v", f.mutations())
	}
	// Run again with wb-new confirmed as it is: it goes through.
	f.beforeServe = nil
	confirm := map[string]string{"wb-new": "uid-wb-new"}
	if resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirm); !resp.Success {
		t.Fatalf("rerun: %+v", resp)
	}
}

// The same late refusal inside Clean up names the workbenches on the
// refused step, by name and UID, for the dialog to ask about them.
func TestCleanUp_LateWorkbenchRefusalListsThem(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	attachDuringPatches(f)
	var events []UpdateStepEvent
	res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"proj"}, ConfirmUnrecorded: map[string]string{"proj": toolDSPAUID(c, "proj")}, ConfirmWorkbenches: map[string]map[string]string{"proj": confirmBoth()}},
		func(e UpdateStepEvent) { events = append(events, e) })
	if res.Success || res.ErrorCode != storage.ErrorCodeConfirmWorkbenches {
		t.Fatalf("got %+v", res)
	}
	last := events[len(events)-1]
	if last.Step != "pipeline-server:proj" || last.Status != "failed" || last.Detail != "wb-new=uid-wb-new" {
		t.Fatalf("events %+v", events)
	}
}

// webhookDeniedBody is the API server's answer when the workbenches
// connection webhook denies a Notebook update (an admission denial: 403,
// reason Forbidden, the webhook's own message).
const webhookDeniedBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"admission webhook \"connection-notebook.opendatahub.io\" denied the request: the following connection secret(s) are invalid — they may not exist, or you may not have permission to access them: proj/gone","reason":"Forbidden","code":403}`

// The webhook denies the change of a workbench whose other Connection is
// gone: the teardown names the workbench and the webhook's reason with its
// own code, never "your account is not allowed"; refused in the dry run,
// no workbench is changed at all.
func TestTeardownPipelineServer_WebhookDenial(t *testing.T) {
	fastPipelineTimings(t)
	for name, dryRun := range map[string]bool{"in the dry run": true, "in the change": false} {
		t.Run(name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putConnectionUsers(f)
			f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
				if r.Method != http.MethodPatch || r.URL.Path != notebookPath("proj", "wb-b") || (r.URL.Query().Get("dryRun") == "All") != dryRun {
					return false
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(webhookDeniedBody))
				return true
			}
			resp, _ := TeardownPipelineServer(userClientOf(c, fakeUserToken), "proj", toolDSPAUID(c, "proj"), confirmBoth())
			if resp.Success || resp.ErrorCode != storage.ErrorCodeWebhookRefused || !strings.Contains(resp.Message, "workbench wb-b") || !strings.Contains(resp.Message, "proj/gone") ||
				!strings.Contains(resp.Message, "Fix that workbench's other connections in the dashboard first") || strings.Contains(resp.Message, kube.UserForbiddenMessage) {
				t.Fatalf("got %+v", resp)
			}
			if hasMutation(f, "DELETE ") {
				t.Errorf("nothing may be deleted: %v", f.mutations())
			}
			if dryRun && hasMutation(f, "PATCH ") {
				t.Errorf("a dry-run refusal must leave every workbench unchanged: %v", f.mutations())
			}
			if _, ok := notebookAnnotation(f, "wb-b"); !ok {
				t.Error("wb-b keeps its annotation")
			}
		})
	}
}

// A workbench that loads the Connection through envFrom only (no
// annotation entry) is not changed; the result says it will fail to start.
func TestTeardownPipelineServer_EnvOnlyWorkbenchIsReported(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putConnectionUsers(f)
	putNotebookRaw(f, "proj", "wb-env", `{}`, `[{"secretRef":{"name":"`+storage.ConnectionSecretName+`"}}]`)
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), confirmBoth())
	if !resp.Success || !strings.Contains(resp.Message, "wb-env") || !strings.Contains(resp.Message, "fail to start") {
		t.Fatalf("got %+v", resp)
	}
	if hasMutation(f, "PATCH "+notebookPath("proj", "wb-env")) {
		t.Error("wb-env must not be changed")
	}
}
