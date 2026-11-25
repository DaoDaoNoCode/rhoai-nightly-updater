package cluster

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Every component a feature setup turns on is offered by the recipe's
// Clean up of the run that turned it on, in the state that setup turns it
// on with (Unmanaged for kueue, Managed for the others), and the step that
// turned it on reads as created. A component added later with another on
// state is covered without changing this test.
func TestRecipeOwnership_EveryComponentInItsOnState(t *testing.T) {
	prereqStep := map[string]string{
		"pipelines": recipe.StepPipelinesPrereq, "mlflow": recipe.StepMLflowPrereq, "model-registry": recipe.StepModelRegistryPrereq,
		"models": recipe.StepModelsPrereq, "playground": recipe.StepPlaygroundPrereq, "kueue": recipe.StepKueuePrereq,
	}
	components := features.ComponentsForTest()
	if !slices.ContainsFunc(components, func(c features.ComponentForTest) bool { return c.On != "Managed" }) {
		t.Fatal("no component with an on state other than Managed: the table no longer covers kueue's")
	}
	for _, comp := range components {
		t.Run(comp.Feature+"/"+comp.Key, func(t *testing.T) {
			step, ok := prereqStep[comp.Feature]
			if !ok {
				t.Fatalf("feature %s has no prerequisites step in this table", comp.Feature)
			}
			f, c := newResourceFake(t)
			f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
				updateFieldOwnership(r.URL.Query().Get("fieldManager"), f.objects[k], patch, next)
			}
			f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"`+comp.Key+`":{"managementState":"Removed"}}}}`)
			// What the setup writes (patchDSCComponent): the state, under the component's own field manager.
			if _, _, err := c.Do(http.MethodPatch, tDSC, "application/merge-patch+json", []byte(`{"spec":{"components":{"`+comp.Key+`":{"managementState":"`+comp.On+`"}}}}`),
				map[string][]string{"fieldManager": {"rhoai-nightly-updater-dsc-" + comp.Key}}); err != nil {
				t.Fatal(err)
			}
			if err := writeTestPrereqRecord(c, `{"components":{"`+comp.Key+`":{"previous":"Removed","feature":"`+comp.Feature+`"}}}`); err != nil {
				t.Fatal(err)
			}
			run := &types.RecipeRun{Steps: []types.RecipeRunStep{{ID: step}}, Components: []string{comp.Key}}
			created, req := recipe.Ownership(c, nil, run)
			if req == nil || !reflect.DeepEqual(req.Components, []string{comp.Key}) || !reflect.DeepEqual(created, map[string]bool{step: true}) {
				t.Fatalf("%s turned on as %s: created %v, cleanup %+v", comp.Key, comp.On, created, req)
			}
		})
	}
}

// recipeKueueRun is a recipe run that turned the DataScienceCluster's
// kueue on (its kueue-prereq step) and set distributed workloads up in
// project proj (its kueue step, the run's annotation on what it created),
// on the cluster putKueueCluster made.
func recipeKueueRun(t *testing.T, f *resourceFake, u *Client) *types.RecipeRun {
	t.Helper()
	f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"kueue":{"managementState":"Removed"},"ray":{"managementState":"Managed"}}},
		"status":{"components":{"ray":{"managementState":"Managed"}},"conditions":[{"type":"KueueReady","status":"True"}]}}`)
	kueueTurnedOnByTool(t, u)
	res, err := kueue.RunAnnotated(u, types.KueueRequest{Action: "setup", Project: "proj"}, map[string]string{recipe.RecipeRunAnnotation: "run-1"}, nil)
	if err != nil || !res.Success {
		t.Fatalf("kueue setup: %+v %v", res, err)
	}
	uid, _ := ensureMap(f.get(kqCQ), "metadata")["uid"].(string)
	if uid == "" {
		t.Fatal("no ClusterQueue")
	}
	return &types.RecipeRun{ID: "run-1", Steps: []types.RecipeRunStep{{ID: recipe.StepKueuePrereq}, {ID: recipe.StepKueue}},
		Components: []string{"kueue"}, CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: uid}}}
}

// recipeKueueCleanUp runs the recipe's Clean up of run with the request
// its offer (recipe.Ownership) gives, as the dialog sends it with every
// part ticked. It returns the result, each step's last status and the
// order the steps started in. at, when set, sees each step's event as it
// is sent ("running" comes before the step runs).
func recipeKueueCleanUp(t *testing.T, u *Client, run *types.RecipeRun, at func(UpdateStepEvent)) (*types.OperationResponse, map[string]string, []string) {
	t.Helper()
	created, req := recipe.Ownership(u, u, run)
	if req == nil || !req.Kueue || req.KueueUID != run.CreatedParts[0].UID || req.RecipeRunID != "run-1" || !reflect.DeepEqual(req.Components, []string{"kueue"}) ||
		!created[recipe.StepKueuePrereq] || !created[recipe.StepKueue] {
		t.Fatalf("offer: created %v, cleanup %+v", created, req)
	}
	steps := map[string]string{}
	var order []string
	res, err := CleanUp(u, *req, func(e UpdateStepEvent) {
		if _, seen := steps[e.Step]; !seen {
			order = append(order, e.Step)
		}
		steps[e.Step] = e.Status
		if at != nil {
			at(e)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, steps, order
}

// prereqRecordHas reports whether the prerequisites record names key.
func prereqRecordHas(t *testing.T, u *Client, key string) bool {
	t.Helper()
	rec, err := features.ReadPrereqRecord(u)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := rec.Components[key]
	return ok
}

// A recipe's Clean up, sent as its offer builds it, deletes the run's
// distributed workloads setup and then turns the DataScienceCluster's
// kueue back to what it was; a queue someone else left keeps kueue on,
// and a queue list that fails changes nothing (fail closed).
func TestRecipeCleanUp_TurnsKueueBackFromItsOffer(t *testing.T) {
	t.Run("turned back", func(t *testing.T) {
		f, u := putKueueCluster(t)
		res, steps, order := recipeKueueCleanUp(t, u, recipeKueueRun(t, f, u), nil)
		if !res.Success || strings.Join(order, " ") != "kueue components" || steps["kueue"] != "success" || steps["components"] != "success" {
			t.Fatalf("clean-up %+v, steps %v %v", res, order, steps)
		}
		if !strings.Contains(res.Message, "kueue is Removed again") || componentState(f, "kueue") != "Removed" || f.has(kqCQ) || prereqRecordHas(t, u, "kueue") {
			t.Fatalf("kueue %s, ClusterQueue left %v: %+v", componentState(f, "kueue"), f.has(kqCQ), res)
		}
	})
	t.Run("a queue someone else left", func(t *testing.T) {
		f, u := putKueueCluster(t)
		run := recipeKueueRun(t, f, u)
		f.putJSON(kqAPI+"/namespaces/team/localqueues/team-queue", `{"metadata":{"name":"team-queue","namespace":"team"},"spec":{"clusterQueue":"team-cq"}}`)
		res, steps, _ := recipeKueueCleanUp(t, u, run, nil)
		if !res.Success || steps["components"] != "success" || !strings.Contains(res.Message, "in use: LocalQueue team/team-queue") {
			t.Fatalf("clean-up %+v, steps %v", res, steps)
		}
		if componentState(f, "kueue") != "Unmanaged" || f.has(kqCQ) || !prereqRecordHas(t, u, "kueue") {
			t.Fatalf("kueue %s, ClusterQueue left %v", componentState(f, "kueue"), f.has(kqCQ))
		}
	})
	t.Run("the queue list is refused", func(t *testing.T) {
		f, u := putKueueCluster(t)
		run := recipeKueueRun(t, f, u)
		// Refused once the setup's own queues are gone: the components'
		// in-use check is what cannot read them.
		res, steps, _ := recipeKueueCleanUp(t, u, run, func(e UpdateStepEvent) {
			if e.Step == "components" && e.Status == "running" {
				f.mu.Lock()
				f.fail["GET "+kqAPI+"/clusterqueues"] = 403
				f.mu.Unlock()
			}
		})
		if res.Success || steps["kueue"] != "success" || steps["components"] != "failed" || !strings.Contains(res.Message, "Cannot tell whether anything still uses kueue, so it stays Unmanaged") {
			t.Fatalf("clean-up %+v, steps %v", res, steps)
		}
		if componentState(f, "kueue") != "Unmanaged" || !prereqRecordHas(t, u, "kueue") {
			t.Fatalf("kueue %s", componentState(f, "kueue"))
		}
	})
}
