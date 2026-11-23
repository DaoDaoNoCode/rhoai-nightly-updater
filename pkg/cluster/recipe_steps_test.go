package cluster

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tDSCList = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"

func recipeStep(t *testing.T, r types.EnvironmentRecipe, id string) recipe.Step {
	t.Helper()
	for _, s := range recipe.Steps(r) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s", id)
	return recipe.Step{}
}

// dscStepFake is an installed operator whose CSV ships the default DSC,
// and whose webhook refuses the first creates as one that is not serving
// yet. The DSC reports conditions once it was read reportAfter times.
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

// The DataScienceCluster step tries a create the webhook refuses because
// it does not serve yet again, then waits until the operator reports
// conditions (not until Ready).
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

// A refusal that is not transient is not retried; conditions that never
// come end the step failed after the wait, saying Resume waits again.
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

// The prerequisite steps record the DataScienceCluster components they
// turned on (what Set up's record gained for their feature), and that they
// set their part up. A component recorded before the run is not the run's.
func TestRecipePrereqSteps_RecordTheComponentsTheyTurnedOn(t *testing.T) {
	pc := newPrereqCluster(t)
	r, problems := recipe.Validate([]byte(tRecipeBase + `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}}`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	// MLflow's component was set up on the Environment page before.
	if res, _ := runPrereqs(t, pc.c, "mlflow"); !res.Success {
		t.Fatalf("mlflow setup: %+v", res)
	}
	st := &recipe.State{Recipe: r}
	for _, id := range []string{recipe.StepPipelinesPrereq, recipe.StepMLflowPrereq} {
		res, err := recipeStep(t, r, id).Run(pc.c, st, func(types.UpdateStepEvent) {})
		if err != nil || !res.Success {
			t.Fatalf("%s: %+v %v", id, res, err)
		}
	}
	if strings.Join(st.Components, ",") != "aipipelines" || !st.Created[recipe.StepPipelinesPrereq] || !st.Created[recipe.StepMLflowPrereq] {
		t.Errorf("components %v, created %v", st.Components, st.Created)
	}
}

// A run's Clean up asks for what its steps set up (any attempt), the
// project a recipe created, and the components it turned on; nothing
// found there. A run that set up nothing Clean up removes has none.
func TestRecipeCleanupRequest(t *testing.T) {
	run := &types.RecipeRun{
		Project: &types.RecipeProjectRef{Name: "recipe-dev", UID: "u1", Owned: true},
		Steps: []types.RecipeRunStep{
			{ID: recipe.StepRHOAI, Created: true}, {ID: recipe.StepProject, Created: true}, {ID: recipe.StepS3},
			{ID: recipe.StepPipelineServer, Created: true}, {ID: recipe.StepMLflow, Created: true},
		},
		Components: []string{"aipipelines", "mlflowoperator"},
	}
	got := recipe.CleanupRequest(run)
	want := types.CleanupRequest{PipelineServers: []string{"recipe-dev"}, MLflow: true, RecipeProject: "recipe-dev", Components: []string{"aipipelines", "mlflowoperator"}}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Errorf("cleanup %+v", got)
	}
	// A project the run uses but no recipe created stays.
	run.Project.Owned = false
	if got := recipe.CleanupRequest(run); got == nil || got.RecipeProject != "" {
		t.Errorf("foreign project %+v", got)
	}
	none := &types.RecipeRun{Project: &types.RecipeProjectRef{Name: "mine"}, Steps: []types.RecipeRunStep{{ID: recipe.StepRHOAI, Created: true}, {ID: recipe.StepS3}}}
	if got := recipe.CleanupRequest(none); got != nil {
		t.Errorf("nothing to clean up: %+v", got)
	}
	if recipe.CleanupRequest(nil) != nil {
		t.Error("no run")
	}
}
