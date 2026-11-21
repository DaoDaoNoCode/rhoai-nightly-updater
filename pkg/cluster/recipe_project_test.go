package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tRecipeNS       = "/api/v1/namespaces/recipe-dev"
	tProjectRequest = "/apis/project.openshift.io/v1/projectrequests"
)

// tRecipe is a valid MVP recipe with project mode (create or existing).
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

// projectFake is a cluster where a ProjectRequest makes its namespace, as
// OpenShift's project API does.
func projectFake(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == tProjectRequest {
			f.putJSON(tRecipeNS, `{"metadata":{"annotations":{"openshift.io/requester":"alice","openshift.io/display-name":"Recipe dev"}}}`)
		}
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

// The project is made as the dashboard's Create project makes it: a
// ProjectRequest with the display name and description, then the
// dashboard's label (plus the tool's label and the recipe annotation) by
// merge patch. Its UID is recorded before the labels; running the step
// again skips it.
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
	if len(patches) != 1 || patches[0].ContentType != "application/merge-patch+json" || patches[0].Query.Get("fieldManager") != "rhoai-nightly-updater" {
		t.Fatalf("label patch %+v", patches)
	}
	ns := f.get(tRecipeNS)
	meta := ns["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["opendatahub.io/dashboard"] != "true" || labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue || meta["annotations"].(map[string]any)["rhoai-nightly-updater/recipe"] != "dev" {
		t.Fatalf("namespace metadata %v", meta)
	}
	if s.Project == nil || !s.Project.Owned || s.Project.UID != meta["uid"] {
		t.Fatalf("state project %+v", s.Project)
	}
	raw, err := record.ReadSnapshotKey(c, "recipeProject")
	if err != nil || !strings.Contains(raw, `"uid":"`+meta["uid"].(string)+`"`) || !strings.Contains(raw, `"recipe":"dev"`) {
		t.Fatalf("record %q %v", raw, err)
	}
	// Again: skipped, nothing written.
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

// A namespace that exists and was not created by a recipe is refused,
// whatever labels it carries; one being deleted waits; system, RHOAI and
// updater namespaces are refused before anything is read.
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

// mode existing uses a data science project of the user's and owns
// nothing; a namespace without the dashboard's label is refused.
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

// Clean up deletes a recipe project only with the tool's label and the
// recorded UID (as the delete's precondition), and only when what it
// holds that the tool did not create was confirmed; then the record
// forgets it.
func TestRecipeProject_Teardown(t *testing.T) {
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	if res := ensureProject(t, c, s); !res.Success {
		t.Fatal(res.Message)
	}
	uid := s.Project.UID
	// What the user added, and what the tool's pipeline server runs.
	f.putJSON("/apis/kubeflow.org/v1/namespaces/recipe-dev/notebooks/my-wb", `{}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/my-wb", `{}`)
	f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/recipe-dev/datasciencepipelinesapplications/dspa", `{"metadata":{"uid":"dspa-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/ds-pipeline-dspa-1", `{"metadata":{"labels":{"dspa":"dspa"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/tools-own", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/going", `{"metadata":{"deletionTimestamp":"2026-11-21T10:00:00Z"}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/mariadb-dspa", `{"metadata":{"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid"}]}}`)

	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "Notebook my-wb,PersistentVolumeClaim my-wb" {
		t.Fatalf("leftovers %v %v", left, err)
	}
	res, err := recipe.DeleteProject(c, "recipe-dev", []string{"Notebook my-wb"})
	if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || !strings.Contains(res.Message, "PersistentVolumeClaim my-wb") || strings.Contains(res.Message, "Notebook my-wb") {
		t.Fatalf("unconfirmed: %+v %v", res, err)
	}
	if len(f.callsTo("DELETE", tRecipeNS)) != 0 {
		t.Fatal("deleted with unconfirmed contents")
	}
	res, err = recipe.DeleteProject(c, "recipe-dev", left)
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
	// Not recorded (any more): refused.
	f.putJSON(tRecipeNS, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	if res, _ := recipe.DeleteProject(c, "recipe-dev", nil); res.Success || !strings.Contains(res.Message, "not created by a recipe") {
		t.Fatalf("unrecorded: %+v", res)
	}
}

// The UID and label guards: a namespace made again under the name, or one
// whose label was removed, is kept.
func TestRecipeProject_TeardownGuards(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"recreated", `{"metadata":{"uid":"new-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`, "made again"},
		{"label removed", `{"metadata":{"uid":"rec-uid"}}`, "no longer carries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			f.putJSON(tRecipeNS, tc.body)
			if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
				t.Fatal(err)
			}
			res, _ := recipe.DeleteProject(c, "recipe-dev", nil)
			if res.Success || !strings.Contains(res.Message, tc.want) || len(f.callsTo("DELETE", tRecipeNS)) != 0 {
				t.Fatalf("%+v", res)
			}
		})
	}
	// A UID that changes between the check and the delete: the
	// precondition refuses (DeleteWithUID).
	f, c := projectFake(t)
	f.putJSON(tRecipeNS, `{"metadata":{"uid":"rec-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
		t.Fatal(err)
	}
	f.beforeServe = func(method, path string) {
		if method == http.MethodDelete && path == tRecipeNS {
			f.putJSON(tRecipeNS, `{"metadata":{"uid":"swapped","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		}
	}
	res, _ := recipe.DeleteProject(c, "recipe-dev", nil)
	if res.Success || !f.has(tRecipeNS) {
		t.Fatalf("deleted a swapped namespace: %+v", res)
	}
}
