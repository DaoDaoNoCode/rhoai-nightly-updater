package cluster

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tRecipeNS       = "/api/v1/namespaces/recipe-dev"
	tProjectRequest = "/apis/project.openshift.io/v1/projectrequests"
)

// tRecipe builds a minimal valid recipe whose project spec uses the given mode (create or existing).
func tRecipe(t *testing.T, mode string) types.EnvironmentRecipe {
	t.Helper()
	r, problems := recipe.Validate([]byte(`{"apiVersion":"rhoai-nightly-updater/v1alpha1","kind":"EnvironmentRecipe","metadata":{"name":"dev","description":"PR review"},
"spec":{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"` + mode + `","name":"recipe-dev","displayName":"Recipe dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true},"models":{"enabled":false},"playground":{"enabled":false},"maas":{"enabled":false}},"dashboardDev":{}}}`))
	if len(problems) > 0 {
		t.Fatalf("test recipe: %v", problems)
	}
	return r
}

// projectFake wires a fake cluster where submitting a ProjectRequest provisions
// a namespace, matching how OpenShift's project API behaves.
func projectFake(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	// On ProjectRequest POST, seed the namespace and return a Project whose
	// metadata matches, the way openshift-apiserver responds.
	f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/project.openshift.io/v1/projects" {
			// A user's projects: the namespaces (openshift-apiserver lists
			// them from its project cache, which may lag: projectListsHidden).
			f.projectLists++
			items := []any{}
			for k, obj := range f.objects {
				if k.gv == "v1" && k.plural == "namespaces" && f.projectLists > f.projectListsHidden && matchesSelector(obj, r.URL.Query().Get("labelSelector")) {
					items = append(items, map[string]any{"metadata": obj["metadata"], "status": map[string]any{"phase": "Active"}})
				}
			}
			f.write(w, 200, map[string]any{"kind": "ProjectList", "apiVersion": "project.openshift.io/v1", "items": items})
			return true
		}
		if r.Method != http.MethodPost || r.URL.Path != tProjectRequest {
			return false
		}
		f.nextID++
		ns := map[string]any{"metadata": map[string]any{"name": "recipe-dev", "uid": fmt.Sprintf("ns-uid-%d", f.nextID), "resourceVersion": "1",
			"labels":      map[string]any{"kubernetes.io/metadata.name": "recipe-dev"},
			"annotations": map[string]any{"openshift.io/requester": "alice", "openshift.io/display-name": "Recipe dev"}}}
		f.objects[fakeKey{gv: "v1", plural: "namespaces", name: "recipe-dev"}] = ns
		f.write(w, 201, map[string]any{"kind": "Project", "apiVersion": "project.openshift.io/v1", "metadata": ns["metadata"]})
		return true
	}
	return f, c
}

func ensureProject(t *testing.T, c *Client, s *recipe.State) *types.OperationResponse {
	t.Helper()
	res, err := recipe.EnsureProject(c, s, func(types.UpdateStepEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Creation should mirror the dashboard's Create project flow: ProjectRequest
// carries display name and description; a merge patch then applies the
// dashboard label together with the tool label and recipe annotation. UID is
// persisted before patching labels; a repeat invocation is a no-op.
func TestRecipeProject_CreatedLikeTheDashboardAndRecorded(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	res := ensureProject(t, c, s)
	if !res.Success {
		t.Fatalf("ensure: %+v", res)
	}
	calls := f.callsTo("POST", tProjectRequest)
	if len(calls) != 1 || !strings.Contains(calls[0].Body, `"displayName":"Recipe dev"`) || !strings.Contains(calls[0].Body, `"description":"PR review"`) || !strings.Contains(calls[0].Body, `"kind":"ProjectRequest"`) {
		t.Fatalf("project request %+v", calls)
	}
	patches := f.callsTo("PATCH", tRecipeNS)
	if len(patches) != 1 || patches[0].ContentType != "application/json-patch+json" || patches[0].Query.Get("fieldManager") != "rhoai-nightly-updater" ||
		!strings.HasPrefix(patches[0].Body, `[{"op":"test","path":"/metadata/uid","value":"`) {
		t.Fatalf("label patch %+v", patches)
	}
	ns := f.get(tRecipeNS)
	meta := ns["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["opendatahub.io/dashboard"] != "true" || labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue || labels["kubernetes.io/metadata.name"] != "recipe-dev" ||
		labels["kueue.openshift.io/managed"] != nil || meta["annotations"].(map[string]any)["rhoai-nightly-updater/recipe"] != "dev" ||
		meta["annotations"].(map[string]any)["openshift.io/requester"] != "alice" {
		t.Fatalf("namespace metadata %v", meta)
	}
	if s.Project == nil || !s.Project.Owned || s.Project.UID != meta["uid"] {
		t.Fatalf("state project %+v", s.Project)
	}
	raw, err := record.ReadSnapshotKey(c, "recipeProject")
	if err != nil || !strings.Contains(raw, `"uid":"`+meta["uid"].(string)+`"`) || !strings.Contains(raw, `"recipe":"dev"`) {
		t.Fatalf("record %q %v", raw, err)
	}
	// Re-run: step reports done and performs no further mutations.
	before := len(f.mutations())
	s2 := &recipe.State{Recipe: tRecipe(t, "create")}
	chk, err := recipeStep(t, s2.Recipe, recipe.StepProject).Check(c, s2)
	if err != nil || !chk.Done || s2.Project == nil || s2.Project.UID != meta["uid"] {
		t.Fatalf("check after create: %+v %v %+v", chk, err, s2.Project)
	}
	if res := ensureProject(t, c, s2); !res.Success || len(f.mutations()) != before {
		t.Fatalf("second ensure %+v changed %v", res, f.mutations()[before:])
	}
}

// Namespaces that predate the recipe are rejected regardless of labels;
// terminating namespaces block until gone. Reserved system, RHOAI, and updater
// names are rejected before any GET runs.
func TestRecipeProject_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, ns, body, want string
		record               bool
	}{
		{"foreign", "recipe-dev", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`, "was not created by a recipe", false},
		{"recorded but recreated", "recipe-dev", `{"metadata":{"uid":"other-uid"}}`, "was not created by a recipe", true},
		{"terminating", "recipe-dev", `{"metadata":{"uid":"rec-uid","deletionTimestamp":"2026-11-21T10:00:00Z"}}`, "being deleted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			f.putJSON(tRecipeNS, tc.body)
			if tc.record {
				if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
					t.Fatal(err)
				}
			}
			res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")})
			if res.Success || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("ensure %+v", res)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Fatalf("changed %v", m)
			}
		})
	}
	for _, name := range []string{"rhoai-nightly-updater", "redhat-ods-applications"} {
		f, c := projectFake(t)
		r := tRecipe(t, "create")
		r.Spec.Project.Name = name
		res := ensureProject(t, c, &recipe.State{Recipe: r})
		if res.Success || !strings.Contains(res.Message, "Choose another name") || len(f.mutations()) != 0 {
			t.Fatalf("%s: %+v %v", name, res, f.mutations())
		}
	}
}

// existing mode attaches to the user's data science project without owning it;
// lacking the dashboard label, the namespace is rejected.
func TestRecipeProject_Existing(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "existing")}
	if res := ensureProject(t, c, s); res.Success || !strings.Contains(res.Message, "not a data science project") {
		t.Fatalf("missing project: %+v", res)
	}
	f.putJSON(tRecipeNS, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	if res := ensureProject(t, c, s); !res.Success || s.Project == nil || s.Project.Owned {
		t.Fatalf("existing project: %+v %+v", res, s.Project)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("changed %v", m)
	}
}

// Teardown removes a recipe-owned project only when the tool's label and the
// stored UID satisfy the delete precondition, and only after the caller
// confirmed user-owned leftovers; the snapshot entry is removed afterward.
func TestRecipeProject_Teardown(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	if res := ensureProject(t, c, s); !res.Success {
		t.Fatal(res.Message)
	}
	uid := s.Project.UID
	// User-created workload plus pipeline server objects the tool owns.
	f.putJSON("/apis/kubeflow.org/v1/namespaces/recipe-dev/notebooks/my-wb", `{}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/my-wb", `{}`)
	f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/recipe-dev/datasciencepipelinesapplications/dspa", `{"metadata":{"uid":"dspa-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/ds-pipeline-dspa-1", `{"metadata":{"labels":{"dspa":"dspa"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/tools-own", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/going", `{"metadata":{"deletionTimestamp":"2026-11-21T10:00:00Z"}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/mariadb-dspa", `{"metadata":{"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid"}]}}`)
	// Tool-owned model and the KServe pod (live RHOAI 3.7: ReplicaSet-owned,
	// part-of llminferenceservice, name <model>); plus a user model and pod.
	f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/recipe-dev/llminferenceservices/smol", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/smol-kserve-9bb6646f9-wzrps", `{"metadata":{"labels":{"app.kubernetes.io/part-of":"llminferenceservice","app.kubernetes.io/name":"smol"},"ownerReferences":[{"kind":"ReplicaSet","name":"smol-kserve-9bb6646f9","uid":"rs-uid"}]}}`)
	f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/recipe-dev/llminferenceservices/theirs", `{}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/theirs-kserve-1", `{"metadata":{"labels":{"app.kubernetes.io/part-of":"llminferenceservice","app.kubernetes.io/name":"theirs"}}}`)
	// A pod named like the model but without KServe's usual labels.
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/named-smol", `{"metadata":{"labels":{"app.kubernetes.io/name":"smol"}}}`)

	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "LLMInferenceService theirs,Notebook my-wb,PersistentVolumeClaim my-wb,Pod named-smol,Pod theirs-kserve-1" {
		t.Fatalf("leftovers %v %v", left, err)
	}
	res, err := recipe.DeleteProject(c, "recipe-dev", uid, []string{"Notebook my-wb"})
	if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || !strings.Contains(res.Message, "PersistentVolumeClaim my-wb") || strings.Contains(res.Message, "Notebook my-wb") {
		t.Fatalf("unconfirmed: %+v %v", res, err)
	}
	if len(f.callsTo("DELETE", tRecipeNS)) != 0 {
		t.Fatal("deleted with unconfirmed contents")
	}
	res, err = recipe.DeleteProject(c, "recipe-dev", uid, left)
	if err != nil || !res.Success {
		t.Fatalf("confirmed: %+v %v", res, err)
	}
	dels := f.callsTo("DELETE", tRecipeNS)
	if len(dels) != 1 || !strings.Contains(dels[0].Body, `"uid":"`+uid+`"`) || f.has(tRecipeNS) {
		t.Fatalf("delete %+v", dels)
	}
	if raw, _ := record.ReadSnapshotKey(c, "recipeProject"); strings.Contains(raw, "recipe-dev") {
		t.Fatalf("still recorded: %s", raw)
	}
	// With no snapshot entry left, deletion is refused.
	f.putJSON(tRecipeNS, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	if res, _ := recipe.DeleteProject(c, "recipe-dev", uid, nil); res.Success || !strings.Contains(res.Message, "not created by a recipe") {
		t.Fatalf("unrecorded: %+v", res)
	}
}

// UID and managed-by checks prevent delete when the name was re-provisioned or
// when labels were stripped.
func TestRecipeProject_TeardownGuards(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"recreated", `{"metadata":{"uid":"new-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`, "made again"},
		{"label removed", `{"metadata":{"uid":"rec-uid","annotations":{"rhoai-nightly-updater/recipe":"dev"}}}`, "no longer carries"},
		{"annotation missing (a tool namespace a forged record names)", `{"metadata":{"uid":"rec-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`, "no longer carries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			f.putJSON(tRecipeNS, tc.body)
			if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
				t.Fatal(err)
			}
			res, _ := recipe.DeleteProject(c, "recipe-dev", "rec-uid", nil)
			if res.Success || !strings.Contains(res.Message, tc.want) || len(f.callsTo("DELETE", tRecipeNS)) != 0 {
				t.Fatalf("%+v", res)
			}
		})
	}
	// If UID changes after the check but before delete, DeleteWithUID
	// precondition blocks removal.
	f, c := projectFake(t)
	f.putJSON(tRecipeNS, `{"metadata":{"uid":"rec-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"rhoai-nightly-updater/recipe":"dev"}}}`)
	if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
		t.Fatal(err)
	}
	f.beforeServe = func(method, path string) {
		if method == http.MethodDelete && path == tRecipeNS {
			f.putJSON(tRecipeNS, `{"metadata":{"uid":"swapped","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		}
	}
	res, _ := recipe.DeleteProject(c, "recipe-dev", "rec-uid", nil)
	if res.Success || !f.has(tRecipeNS) {
		t.Fatalf("deleted a swapped namespace: %+v", res)
	}
}

// Label patching is tied to the UID returned from ProjectRequest: if another
// namespace appears under the same name before patch, the test op fails and
// labels are not applied.
func TestRecipeProject_LabelPatchTestsTheCreatedUID(t *testing.T) {
	f, c := projectFake(t)
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == tRecipeNS {
			f.putJSON(tRecipeNS, `{"metadata":{"uid":"someone-else","labels":{"kubernetes.io/metadata.name":"recipe-dev"}}}`)
		}
	}
	res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")})
	if res.Success {
		t.Fatalf("labelled a replaced namespace: %+v", res)
	}
	labels := f.get(tRecipeNS)["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[kube.ManagedByLabelKey] != nil || labels["opendatahub.io/dashboard"] != nil {
		t.Fatalf("the other namespace got the labels: %v", labels)
	}
}

// kueue.openshift.io/managed tracks the dashboard OdhDashboardConfig:
// present when disableKueue is false, absent when true, unset (default), or
// missing entirely; unreadable config aborts before project creation.
func TestRecipeProject_KueueLabelFollowsTheDashboard(t *testing.T) {
	const cfg = "/apis/opendatahub.io/v1alpha/namespaces/redhat-ods-applications/odhdashboardconfigs/odh-dashboard-config"
	for _, tc := range []struct {
		name, body string
		fail       int
		want       bool
		ok         bool
	}{
		{"kueue on", `{"spec":{"dashboardConfig":{"disableKueue":false}}}`, 0, true, true},
		{"kueue off", `{"spec":{"dashboardConfig":{"disableKueue":true}}}`, 0, false, true},
		{"unset: the dashboard's default", `{"spec":{"dashboardConfig":{}}}`, 0, false, true},
		{"no config", "", 0, false, true},
		{"unreadable", "", 403, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			if tc.body != "" {
				f.putJSON(cfg, tc.body)
			}
			if tc.fail != 0 {
				f.fail["GET "+cfg] = tc.fail
			}
			res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")})
			if res.Success != tc.ok {
				t.Fatalf("ensure %+v", res)
			}
			if !tc.ok {
				if len(f.callsTo("POST", tProjectRequest)) != 0 {
					t.Fatal("created the project although the dashboard's settings could not be read")
				}
				return
			}
			labels := f.get(tRecipeNS)["metadata"].(map[string]any)["labels"].(map[string]any)
			if (labels["kueue.openshift.io/managed"] == "true") != tc.want {
				t.Fatalf("labels %v", labels)
			}
		})
	}
}

// Removing a label or annotation after the check but before delete aborts
// deletion because resourceVersion is part of the precondition.
func TestRecipeProject_DeleteRequiresTheCheckedVersion(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	if res := ensureProject(t, c, s); !res.Success {
		t.Fatal(res.Message)
	}
	f.beforeServe = func(method, path string) {
		if method == http.MethodDelete && path == tRecipeNS {
			f.mu.Lock()
			obj := f.objects[fakeKey{gv: "v1", plural: "namespaces", name: "recipe-dev"}]
			meta := obj["metadata"].(map[string]any)
			delete(meta["labels"].(map[string]any), kube.ManagedByLabelKey)
			f.bumpRV(meta)
			f.mu.Unlock()
		}
	}
	res, _ := recipe.DeleteProject(c, "recipe-dev", s.Project.UID, nil)
	if res.Success || !f.has(tRecipeNS) {
		t.Fatalf("deleted a namespace whose label was removed meanwhile: %+v", res)
	}
}

// Fail closed on protected namespaces: unreadable DSCInitialization means
// neither EnsureProject nor Clean up can distinguish RHOAI namespaces.
func TestRecipeProject_UnreadableDSCIRefuses(t *testing.T) {
	f, c := projectFake(t)
	f.fail["GET "+dsciV2List] = 500
	f.fail["GET /apis/dscinitialization.opendatahub.io/v1/dscinitializations"] = 500
	_, err := recipeStep(t, tRecipe(t, "create"), recipe.StepProject).Check(c, &recipe.State{Recipe: tRecipe(t, "create")})
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether recipe-dev is a namespace of RHOAI") {
		t.Fatalf("check err %v", err)
	}
	if res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")}); res.Success || len(f.callsTo("POST", tProjectRequest)) != 0 {
		t.Fatalf("ensure %+v", res)
	}
	problem, _, _, err := recipe.ProjectTeardownProblem(c, "recipe-dev", "some-uid", nil)
	if err == nil && problem == "" {
		t.Fatal("clean-up allowed without knowing RHOAI's namespaces")
	}
}

// Resume recreates a project that was recorded on a prior run but no longer
// exists instead of reusing the stale UID.
func TestRecipeProject_GoneProjectIsCreatedAgain(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create"), Project: &types.RecipeProjectRef{Name: "recipe-dev", UID: "old-uid", Owned: true}}
	res := ensureProject(t, c, s)
	if !res.Success || len(f.callsTo("POST", tProjectRequest)) != 1 || s.Project == nil || s.Project.UID == "old-uid" {
		t.Fatalf("ensure %+v, project %+v", res, s.Project)
	}
}

// When labels or annotations were added concurrently after the read, the patch
// must merge rather than replace an entire map that was missing at read time.
func TestRecipeProject_LabelPatchKeepsAConcurrentWritersMap(t *testing.T) {
	f, c := projectFake(t)
	intercept := f.intercept
	f.intercept = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !intercept(w, r, body) {
			return false
		}
		if r.Method != http.MethodPost {
			return true
		}
		// Strip annotations from the freshly created namespace (intercept holds
		// the fake lock).
		delete(f.objects[fakeKey{gv: "v1", plural: "namespaces", name: "recipe-dev"}]["metadata"].(map[string]any), "annotations")
		return true
	}
	patched := false
	f.beforeServe = func(method, path string) {
		if method != http.MethodPatch || path != tRecipeNS || patched {
			return
		}
		patched = true
		f.mu.Lock()
		defer f.mu.Unlock()
		meta := f.objects[fakeKey{gv: "v1", plural: "namespaces", name: "recipe-dev"}]["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{"someone/else": "x"}
		meta["resourceVersion"] = "7"
	}
	res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")})
	if !res.Success {
		t.Fatalf("ensure %+v", res)
	}
	ann := f.get(tRecipeNS)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["someone/else"] != "x" || len(ann) < 2 {
		t.Fatalf("annotations %v", ann)
	}
}

// openshift-apiserver lists a user's projects from its project cache,
// which catches up with a new project within about a second: the project
// step ends only once the user's list has it, so the steps after it (which
// check that list) find it. Here the first two LISTs leave it out.
func TestRecipeProject_WaitsUntilTheUserSeesIt(t *testing.T) {
	f, c := projectFake(t)
	f.projectListsHidden = 2
	var events []string
	start := kube.Clock().Now()
	res, err := recipe.EnsureProject(c, &recipe.State{Recipe: tRecipe(t, "create")}, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || !res.Success {
		t.Fatalf("ensure %+v %v", res, err)
	}
	waited := kube.Clock().Now().Sub(start)
	f.mu.Lock()
	lists := f.projectLists
	f.mu.Unlock()
	if lists != 3 || waited < 2*time.Second || waited > 3*time.Second {
		t.Errorf("%d project lists over %v, want 3 over 2 polls", lists, waited)
	}
	for _, call := range f.callsTo(http.MethodGet, "/apis/project.openshift.io/v1/projects") {
		if call.Token != fakeUserToken || call.Query.Get("labelSelector") != "opendatahub.io/dashboard=true" {
			t.Errorf("the list was not the user's data science projects: %+v", call)
		}
	}
	if !strings.Contains(strings.Join(events, "\n"), "Waiting for project recipe-dev to appear among your data science projects") {
		t.Errorf("events %q", events)
	}

	// Found created (a resume): the check is done only while the user sees it.
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	f.mu.Lock()
	f.projectLists, f.projectListsHidden = 0, 1
	f.mu.Unlock()
	step := recipeStep(t, s.Recipe, recipe.StepProject)
	chk, err := step.Check(c, s)
	if err != nil || chk.Done {
		t.Errorf("check while the user does not see it: %+v %v", chk, err)
	}
	if chk, err = step.Check(c, s); err != nil || !chk.Done {
		t.Errorf("check once the user sees it: %+v %v", chk, err)
	}
}

// A project the user's list never shows fails the project step itself,
// with what to do, after the bounded wait; the steps after it never start.
func TestRecipeProject_NeverSeenFailsTheProjectStep(t *testing.T) {
	f, c := projectFake(t)
	f.projectListsHidden = 1 << 30
	start := kube.Clock().Now()
	res, err := recipe.EnsureProject(c, &recipe.State{Recipe: tRecipe(t, "create")}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || res.ErrorCode != "timeout" ||
		!strings.Contains(res.Message, "Project recipe-dev is set up, but OpenShift still does not list it among your data science projects after 30s") ||
		!strings.Contains(res.Message, "then Resume") {
		t.Fatalf("ensure %+v %v", res, err)
	}
	if waited := kube.Clock().Now().Sub(start); waited < 30*time.Second || waited > 32*time.Second {
		t.Errorf("waited %v", waited)
	}
}
