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
	if strings.Join(st.Components, ",") != "aipipelines" {
		t.Errorf("components %v", st.Components)
	}
}

// What a run owns is read from the cluster, not from its record: the
// components it turned on while they are still Managed and recorded by
// Set up, the project while it is the one a recipe created. The S3
// storage, a pipeline server and MLflow are never claimed, whatever the
// record says.
func TestRecipeOwnership_FromTheCluster(t *testing.T) {
	t.Run("components", func(t *testing.T) {
		pc := newPrereqCluster(t)
		if res, _ := runPrereqs(t, pc.c, "pipelines"); !res.Success {
			t.Fatalf("setup %+v", res)
		}
		run := &types.RecipeRun{
			Steps: []types.RecipeRunStep{
				{ID: recipe.StepPipelinesPrereq}, {ID: recipe.StepS3, Created: true}, {ID: recipe.StepPipelineServer, Created: true}, {ID: recipe.StepMLflow, Created: true},
			},
			// mlflowoperator: Removed, and not in Set up's record.
			Components: []string{"aipipelines", "mlflowoperator"},
		}
		created, req := recipe.Ownership(pc.c, run)
		want := types.CleanupRequest{Components: []string{"aipipelines"}}
		if req == nil || !reflect.DeepEqual(*req, want) || !reflect.DeepEqual(created, map[string]bool{recipe.StepPipelinesPrereq: true}) {
			t.Fatalf("created %v, cleanup %+v", created, req)
		}
		// Someone else sets it back: no longer as the run set it.
		k, _ := parseFakePath(tDSC)
		pc.f.mu.Lock()
		ensureMap(ensureMap(ensureMap(pc.f.objects[k], "spec"), "components"), "aipipelines")["managementState"] = "Removed"
		pc.f.mu.Unlock()
		if created, req := recipe.Ownership(pc.c, run); req != nil || len(created) != 0 {
			t.Fatalf("after the change: created %v, cleanup %+v", created, req)
		}
	})
	t.Run("project", func(t *testing.T) {
		f, c := projectFake(t)
		s := &recipe.State{Recipe: tRecipe(t, "create")}
		if res := ensureProject(t, c, s); !res.Success {
			t.Fatalf("ensure %+v", res)
		}
		run := &types.RecipeRun{Project: s.Project, Steps: []types.RecipeRunStep{{ID: recipe.StepProject}}}
		created, req := recipe.Ownership(c, run)
		if req == nil || req.RecipeProject != "recipe-dev" || !created[recipe.StepProject] {
			t.Fatalf("created %v, cleanup %+v", created, req)
		}
		// Replaced by someone else (another UID, no recipe annotation).
		f.putJSON(tRecipeNS, `{"metadata":{"uid":"someone-else"}}`)
		if created, req := recipe.Ownership(c, run); req != nil || created[recipe.StepProject] {
			t.Fatalf("replaced: created %v, cleanup %+v", created, req)
		}
	})
	if created, req := recipe.Ownership(nil, nil); req != nil || len(created) != 0 {
		t.Error("no run")
	}
}

// When the prerequisites record cannot be read, the step says that its
// components are not known to the run (and so not in its Clean up)
// instead of losing them silently.
func TestRecipePrereqSteps_UnreadableRecordIsSaid(t *testing.T) {
	pc := newPrereqCluster(t)
	r := tRecipe(t, "create")
	r.Spec.Features.Pipelines.Enabled = true
	reads := 0
	serve := pc.f.beforeServe
	pc.f.beforeServe = func(method, path string) {
		serve(method, path)
		if method == http.MethodGet && strings.HasSuffix(path, "/configmaps/rhoai-nightly-updater-snapshot") {
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
	res, err := recipeStep(t, r, recipe.StepPipelinesPrereq).Run(pc.c, st, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || res == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if len(st.Components) != 0 || !strings.Contains(res.Message, "Clean up everything") || !strings.Contains(strings.Join(events, "\n"), "cannot tell which") {
		t.Fatalf("components %v, message %q, events %v", st.Components, res.Message, events)
	}
}
