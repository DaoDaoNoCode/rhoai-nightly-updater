package cluster

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tDSCList = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"

func recipeStep(t *testing.T, r types.EnvironmentRecipe, id string) recipe.Step {
	t.Helper()
	for _, s := range recipe.Steps(r, false) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s", id)
	return recipe.Step{}
}

// dscStepFake models an operator that is already installed: its CSV includes
// the default DSC, and the webhook blocks initial create calls while it is
// not yet serving. After reportAfter GETs, the DSC object exposes conditions.
func dscStepFake(t *testing.T, refusals, reportAfter int) (*resourceFake, *Client) {
	t.Helper()
	mockDSCSamples(t, func(*http.Request) (int, string) { return 500, "" })
	f, c := projectFake(t)
	f.putJSON(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName), `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.putJSON(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0"), installedCSVBody(almExamplesJSON))
	posts, reads := 0, 0
	f.beforeServe = func(method, path string) {
		if method == http.MethodPost && path == tDSCList {
			posts++
			f.mu.Lock()
			if posts <= refusals {
				f.fail["POST "+tDSCList] = 500
				f.failMessage["POST "+tDSCList] = `Internal error occurred: failed calling webhook "vdatasciencecluster.opendatahub.io": failed to call webhook: Post "https://webhook:443/validate": dial tcp 10.0.0.1:443: connect: connection refused`
			} else {
				delete(f.fail, "POST "+tDSCList)
			}
			f.mu.Unlock()
		}
	}
	f.listOnGet = map[string]bool{"datascienceclusters": true}
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural != "datascienceclusters" {
			return
		}
		reads++
		if reportAfter > 0 && reads >= reportAfter {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
		}
	}
	return f, c
}

// For DataScienceCluster, the step re-attempts create after a not-yet-serving
// webhook rejection, then blocks until the operator publishes conditions
// (Ready is not required).
func TestRecipeDSCStep_RetriesTheWebhookAndWaitsForConditions(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 2, 3)
	r := tRecipe(t, "create")
	step := recipeStep(t, r, recipe.StepDSC)
	s := &recipe.State{Recipe: r}
	if chk, err := step.Check(c, s); err != nil || chk.Done || chk.Blocked != "" {
		t.Fatalf("check %+v %v", chk, err)
	}
	var events []string
	res, err := step.Run(c, s, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || !res.Success {
		t.Fatalf("run %+v %v (events %v)", res, err, events)
	}
	if n := len(f.callsTo("POST", tDSCList)); n != 3 {
		t.Fatalf("%d creates, want 3", n)
	}
	if !strings.Contains(strings.Join(events, "\n"), "trying again") || !strings.Contains(strings.Join(events, "\n"), "conditions") {
		t.Fatalf("events %v", events)
	}
	if chk, err := step.Check(c, s); err != nil || !chk.Done {
		t.Fatalf("check after %+v %v", chk, err)
	}
}

// Non-transient refusals skip retry; if conditions never appear, the step
// fails once the wait elapses, with a message that Resume will wait again.
func TestRecipeDSCStep_Failures(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	t.Run("not transient", func(t *testing.T) {
		f, c := dscStepFake(t, 0, 0)
		f.fail["POST "+tDSCList] = 403
		r := tRecipe(t, "create")
		res, err := recipeStep(t, r, recipe.StepDSC).Run(c, &recipe.State{Recipe: r}, func(types.UpdateStepEvent) {})
		if err != nil || res.Success || len(f.callsTo("POST", tDSCList)) != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("no conditions", func(t *testing.T) {
		_, c := dscStepFake(t, 0, 0)
		r := tRecipe(t, "create")
		res, err := recipeStep(t, r, recipe.StepDSC).Run(c, &recipe.State{Recipe: r}, func(types.UpdateStepEvent) {})
		if err != nil || res.Success || res.ErrorCode != "timeout" || !strings.Contains(res.Message, "Resume waits again") {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("create false", func(t *testing.T) {
		_, c := dscStepFake(t, 0, 0)
		r := tRecipe(t, "create")
		r.Spec.DataScienceCluster.Create = false
		chk, err := recipeStep(t, r, recipe.StepDSC).Check(c, &recipe.State{Recipe: r})
		if err != nil || !strings.Contains(chk.Blocked, "does not create one") {
			t.Fatalf("%+v %v", chk, err)
		}
	})
}

// The prerequisites step writes into state which DataScienceCluster
// components it enabled (what Set up's record gained for the recipe's
// features, from one read before and one after the whole set).
// Components pre-recorded are excluded from what the run claims.
func TestRecipePrereqSteps_RecordTheComponentsTheyTurnedOn(t *testing.T) {
	pc := newPrereqCluster(t)
	r, problems := recipe.Validate([]byte(tRecipeBase + `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}}`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	// MLflow was already configured on the Environment page.
	if res, _ := runPrereqs(t, pc.c, "mlflow"); !res.Success {
		t.Fatalf("mlflow setup: %+v", res)
	}
	st := &recipe.State{Recipe: r}
	res, err := recipeStep(t, r, recipe.StepPrerequisites).Run(pc.c, st, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(st.Components, ",") != "aipipelines" {
		t.Errorf("components %v", st.Components)
	}
	// Each component is the first feature's that needs it.
	rec, err := features.ReadPrereqRecord(pc.c)
	if err != nil || rec.Components["aipipelines"].Feature != "pipelines" || rec.Components["mlflowoperator"].Feature != "mlflow" {
		t.Errorf("record %+v %v", rec, err)
	}
	// With the enablement it turned on, as the record has it.
	defer func() {
		// Set up again over an entry that survived a turn off (written
		// again with a new ID): the run claims it with the new ID.
		pc.f.mu.Lock()
		k, _ := parseFakePath(tDSC)
		ensureMap(ensureMap(ensureMap(pc.f.objects[k], "spec"), "components"), "aipipelines")["managementState"] = "Removed"
		pc.f.mu.Unlock()
		old := prereqRecordID(t, pc.c, "aipipelines")
		again := &recipe.State{Recipe: r}
		res, err := recipeStep(t, r, recipe.StepPrerequisites).Run(pc.c, again, func(types.UpdateStepEvent) {})
		if err != nil || !res.Success {
			t.Fatalf("again: %+v %v", res, err)
		}
		id := prereqRecordID(t, pc.c, "aipipelines")
		if id == old || !reflect.DeepEqual(again.ComponentIDs, map[string]string{"aipipelines": id}) || componentState(pc.f, "aipipelines") != "Managed" {
			t.Errorf("again: component IDs %v, record %q (was %q)", again.ComponentIDs, id, old)
		}
	}()
	if id := prereqRecordID(t, pc.c, "aipipelines"); id == "" || !reflect.DeepEqual(st.ComponentIDs, map[string]string{"aipipelines": id}) {
		t.Errorf("component IDs %v, record %q", st.ComponentIDs, id)
	}
}

// The set records every component before its first patch; when a later
// patch fails, the run claims only the components whose patch went
// through, not the ones recorded but never changed. A Resume whose retry
// turns the rest on claims them then (the record had them already), and
// the run's Clean up offers them.
func TestRecipePrereqSteps_ClaimOnlyWhatWasTurnedOn(t *testing.T) {
	pc := newPrereqCluster(t)
	r, problems := recipe.Validate([]byte(tRecipeBase + `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}}`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	patches := 0
	pc.f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodPatch || r.URL.Path != tDSC || r.URL.Query().Get("dryRun") != "" {
			return false
		}
		if patches++; patches == 2 {
			pc.f.status(w, http.StatusInternalServerError, "injected failure")
			return true
		}
		return false
	}
	st := &recipe.State{Recipe: r}
	res, err := recipeStep(t, r, recipe.StepPrerequisites).Run(pc.c, st, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || !strings.Contains(res.Message, "Failed to set mlflowoperator to Managed") {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(st.Components, ",") != "aipipelines" || componentState(pc.f, "mlflowoperator") != "Removed" {
		t.Errorf("components %v, mlflowoperator %s", st.Components, componentState(pc.f, "mlflowoperator"))
	}
	// Resume: the runner keeps the components; the retry succeeds.
	pc.f.mu.Lock()
	pc.f.intercept = nil
	pc.f.mu.Unlock()
	first := prereqRecordID(t, pc.c, "aipipelines")
	if !reflect.DeepEqual(st.ComponentIDs, map[string]string{"aipipelines": first}) {
		t.Errorf("component IDs %v", st.ComponentIDs)
	}
	resumed := &recipe.State{Recipe: r, Components: st.Components, ComponentIDs: st.ComponentIDs}
	if res, err := recipeStep(t, r, recipe.StepPrerequisites).Run(pc.c, resumed, func(types.UpdateStepEvent) {}); err != nil || !res.Success {
		t.Fatalf("resume: %+v %v", res, err)
	}
	ids := map[string]string{"aipipelines": first, "mlflowoperator": prereqRecordID(t, pc.c, "mlflowoperator")}
	if strings.Join(resumed.Components, ",") != "aipipelines,mlflowoperator" || ids["mlflowoperator"] == "" || !reflect.DeepEqual(resumed.ComponentIDs, ids) {
		t.Errorf("resumed components %v, IDs %v, record %v", resumed.Components, resumed.ComponentIDs, ids)
	}
	// The run's Clean up offers both, bound to those enablements, and
	// turns them back from that offer.
	u := userClientOf(pc.c, fakeUserToken)
	run := &types.RecipeRun{ID: "run-1", Steps: []types.RecipeRunStep{{ID: recipe.StepPrerequisites}}, Components: resumed.Components, ComponentIDs: resumed.ComponentIDs}
	created, req := recipe.Ownership(u, u, run)
	if req == nil || strings.Join(req.Components, ",") != "aipipelines,mlflowoperator" || !reflect.DeepEqual(req.ComponentIDs, ids) || !created[recipe.StepPrerequisites] {
		t.Fatalf("offer: created %v, cleanup %+v", created, req)
	}
	res, err = CleanUp(u, *req, func(UpdateStepEvent) {})
	if err != nil || !res.Success {
		t.Fatalf("clean up: %+v %v", res, err)
	}
	for _, key := range []string{"aipipelines", "mlflowoperator"} {
		if componentState(pc.f, key) != "Removed" {
			t.Errorf("%s is %s after Clean up", key, componentState(pc.f, key))
		}
	}
}

// Ownership comes from live cluster state, not the run record alone:
// components count only while still Managed and listed in Set up's record;
// the project counts only if it is the one the recipe created. S3 storage,
// pipeline server, and MLflow steps are never attributed to the run,
// regardless of what the record shows.
func TestRecipeOwnership_FromTheCluster(t *testing.T) {
	t.Run("components", func(t *testing.T) {
		pc := newPrereqCluster(t)
		if res, _ := runPrereqs(t, pc.c, "pipelines"); !res.Success {
			t.Fatalf("setup %+v", res)
		}
		id := prereqRecordID(t, pc.c, "aipipelines")
		run := &types.RecipeRun{
			ComponentIDs: map[string]string{"aipipelines": id},
			Steps: []types.RecipeRunStep{
				{ID: recipe.StepPipelinesPrereq}, {ID: recipe.StepS3, Created: true}, {ID: recipe.StepPipelineServer, Created: true}, {ID: recipe.StepMLflow, Created: true},
			},
			// mlflowoperator is Removed and absent from Set up's record.
			Components: []string{"aipipelines", "mlflowoperator"},
		}
		created, req := recipe.Ownership(pc.c, nil, run)
		want := types.CleanupRequest{Components: []string{"aipipelines"}, ComponentIDs: map[string]string{"aipipelines": id}}
		if req == nil || !reflect.DeepEqual(*req, want) || !reflect.DeepEqual(created, map[string]bool{recipe.StepPipelinesPrereq: true}) {
			t.Fatalf("created %v, cleanup %+v", created, req)
		}
		// A run of this version marks its prerequisites step, which turned
		// the components of every feature but Kueue on.
		run.Steps[0].ID = recipe.StepPrerequisites
		if created, _ := recipe.Ownership(pc.c, nil, run); !reflect.DeepEqual(created, map[string]bool{recipe.StepPrerequisites: true}) {
			t.Fatalf("this version's run: created %v", created)
		}
		// Another actor reverts management state: no longer what this run left.
		k, _ := parseFakePath(tDSC)
		pc.f.mu.Lock()
		ensureMap(ensureMap(ensureMap(pc.f.objects[k], "spec"), "components"), "aipipelines")["managementState"] = "Removed"
		pc.f.mu.Unlock()
		if created, req := recipe.Ownership(pc.c, nil, run); req != nil || len(created) != 0 {
			t.Fatalf("after the change: created %v, cleanup %+v", created, req)
		}
	})
	// Kueue's on state is Unmanaged: the run that turned it on offers to
	// turn it back while it is as the run left it, also while a queue is
	// left (Clean up deletes the setup's queues first and checks then).
	t.Run("kueue", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
		u := userClientOf(pc.c, fakeUserToken)
		kueueTurnedOnByTool(t, u)
		run := &types.RecipeRun{Steps: []types.RecipeRunStep{{ID: recipe.StepKueuePrereq}}, Components: []string{"kueue"}}
		offered := func(when string) {
			t.Helper()
			created, req := recipe.Ownership(pc.c, nil, run)
			if req == nil || !reflect.DeepEqual(*req, types.CleanupRequest{Components: []string{"kueue"}, ComponentIDs: map[string]string{"kueue": types.ComponentNotIdentified}}) || !reflect.DeepEqual(created, map[string]bool{recipe.StepKueuePrereq: true}) {
				t.Fatalf("%s: created %v, cleanup %+v", when, created, req)
			}
		}
		notOffered := func(when string) {
			t.Helper()
			if created, req := recipe.Ownership(pc.c, nil, run); req != nil || len(created) != 0 {
				t.Fatalf("%s: created %v, cleanup %+v", when, created, req)
			}
		}
		offered("as the run left it")
		pc.f.groups = map[string]string{kqGroup: kqV1beta2}
		pc.f.putJSON(kqCQ, `{"metadata":{"name":"`+kueue.ClusterQueueName+`"}}`)
		offered("with a ClusterQueue left")
		other := func(state string) {
			t.Helper()
			if _, _, err := u.Do(http.MethodPatch, tDSC, "application/merge-patch+json", []byte(`{"spec":{"components":{"kueue":{"managementState":"`+state+`"}}}}`), map[string][]string{"fieldManager": {"kubectl-edit"}}); err != nil {
				t.Fatal(err)
			}
		}
		other("Removed")
		notOffered("turned back by someone else")
		// Unmanaged again, but written by another field manager: not what the run left.
		other("Unmanaged")
		if componentState(pc.f, "kueue") != "Unmanaged" {
			t.Fatalf("kueue %s", componentState(pc.f, "kueue"))
		}
		notOffered("rewritten by someone else")
	})
	t.Run("project", func(t *testing.T) {
		f, c := projectFake(t)
		s := &recipe.State{Recipe: tRecipe(t, "create")}
		if res := ensureProject(t, c, s); !res.Success {
			t.Fatalf("ensure %+v", res)
		}
		run := &types.RecipeRun{Project: s.Project, Steps: []types.RecipeRunStep{{ID: recipe.StepProject}}}
		created, req := recipe.Ownership(c, nil, run)
		if req == nil || len(req.RecipeProjects) != 1 || req.RecipeProjects[0] != "recipe-dev" || !created[recipe.StepProject] {
			t.Fatalf("created %v, cleanup %+v", created, req)
		}
		// Clean-up includes the UID belonging to the run's project.
		if !reflect.DeepEqual(req.RecipeProjectUIDs, map[string]string{"recipe-dev": s.Project.UID}) {
			t.Fatalf("cleanup UIDs %v", req.RecipeProjectUIDs)
		}
		// A later run deletes and recreates the namespace: names match the
		// record, but the project no longer belongs to this run.
		f.deleteObject(tRecipeNS)
		if res := ensureProject(t, c, &recipe.State{Recipe: tRecipe(t, "create")}); !res.Success {
			t.Fatalf("again %+v", res)
		}
		if created, req := recipe.Ownership(c, nil, run); req != nil || created[recipe.StepProject] {
			t.Fatalf("another run's: created %v, cleanup %+v", created, req)
		}
		// Another party replaces the namespace (different UID, no recipe annotation).
		f.putJSON(tRecipeNS, `{"metadata":{"uid":"someone-else"}}`)
		if created, req := recipe.Ownership(c, nil, run); req != nil || created[recipe.StepProject] {
			t.Fatalf("replaced: created %v, cleanup %+v", created, req)
		}
	})
	if created, req := recipe.Ownership(nil, nil, nil); req != nil || len(created) != 0 {
		t.Error("no run")
	}
}

// If the cluster cannot be read for the claim, the step reports that
// component ownership is unknown for the run (so they stay out of Clean up)
// rather than dropping that information without notice.
func TestRecipePrereqSteps_UnreadableRecordIsSaid(t *testing.T) {
	pc := newPrereqCluster(t)
	r := tRecipe(t, "create")
	r.Spec.Features.Pipelines.Enabled = true
	// The step's first read, before the setup: what is on already.
	reads := 0
	serve := pc.f.beforeServe
	pc.f.beforeServe = func(method, path string) {
		serve(method, path)
		if method == http.MethodGet && strings.HasSuffix(path, "/datascienceclusters") {
			reads++
			pc.f.mu.Lock()
			if reads == 1 {
				pc.f.fail["GET "+path] = 500
			} else {
				delete(pc.f.fail, "GET "+path)
			}
			pc.f.mu.Unlock()
		}
	}
	st := &recipe.State{Recipe: r}
	var events []string
	res, err := recipeStep(t, r, recipe.StepPrerequisites).Run(pc.c, st, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || res == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if len(st.Components) != 0 || !strings.Contains(res.Message, "Clean up everything") || !strings.Contains(strings.Join(events, "\n"), "cannot tell which") {
		t.Fatalf("components %v, message %q, events %v", st.Components, res.Message, events)
	}
}
