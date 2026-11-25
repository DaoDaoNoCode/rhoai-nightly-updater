package cluster

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
			if err := writeTestPrereqRecord(c, `{"components":{"`+comp.Key+`":{"previous":"Removed","feature":"`+comp.Feature+`","id":"en-1"}}}`); err != nil {
				t.Fatal(err)
			}
			run := &types.RecipeRun{Steps: []types.RecipeRunStep{{ID: step}}, Components: []string{comp.Key}, ComponentIDs: map[string]string{comp.Key: "en-1"}}
			created, req := recipe.Ownership(c, nil, run)
			if req == nil || !reflect.DeepEqual(req.Components, []string{comp.Key}) || !reflect.DeepEqual(req.ComponentIDs, map[string]string{comp.Key: "en-1"}) || !reflect.DeepEqual(created, map[string]bool{step: true}) {
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
	return recipeKueueRunAs(t, f, u, "en-1")
}

// recipeKueueRunAs is recipeKueueRun with the record's enablement ID (""
// a record and a run from before IDs).
func recipeKueueRunAs(t *testing.T, f *resourceFake, u *Client, id string) *types.RecipeRun {
	t.Helper()
	f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"kueue":{"managementState":"Removed"},"ray":{"managementState":"Managed"}}},
		"status":{"components":{"ray":{"managementState":"Managed"}},"conditions":[{"type":"KueueReady","status":"True"}]}}`)
	kueueTurnedOnByToolAs(t, u, id)
	res, err := kueue.RunAnnotated(u, types.KueueRequest{Action: "setup", Project: "proj"}, map[string]string{recipe.RecipeRunAnnotation: "run-1"}, nil)
	if err != nil || !res.Success {
		t.Fatalf("kueue setup: %+v %v", res, err)
	}
	uid, _ := ensureMap(f.get(kqCQ), "metadata")["uid"].(string)
	if uid == "" {
		t.Fatal("no ClusterQueue")
	}
	var ids map[string]string
	if id != "" {
		ids = map[string]string{"kueue": id}
	}
	return &types.RecipeRun{ID: "run-1", Steps: []types.RecipeRunStep{{ID: recipe.StepKueuePrereq}, {ID: recipe.StepKueue}},
		Components: []string{"kueue"}, ComponentIDs: ids, CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: uid}}}
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
		!reflect.DeepEqual(req.ComponentIDs, map[string]string{"kueue": kube.FirstNonEmpty(run.ComponentIDs["kueue"], types.ComponentNotIdentified)}) || !created[recipe.StepKueuePrereq] || !created[recipe.StepKueue] {
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
		if !res.Success || steps["components"] != "success" || !strings.Contains(res.Message, "Not turned back: kueue (in use: LocalQueue team/team-queue)") || strings.Contains(res.Message, "Managed") {
			t.Fatalf("clean-up %+v, steps %v", res, steps)
		}
		if componentState(f, "kueue") != "Unmanaged" || f.has(kqCQ) || !prereqRecordHas(t, u, "kueue") {
			t.Fatalf("kueue %s, ClusterQueue left %v", componentState(f, "kueue"), f.has(kqCQ))
		}
	})
	// Turned back and turned on again (another enablement) between the
	// review and the click: refused for this run, left as it is, and kept
	// in the record for its own Clean up.
	for _, reviewed := range []string{"en-1", ""} {
		// "": a record entry from before IDs, reviewed as such.
		t.Run("turned on again since the review/"+kube.FirstNonEmpty(reviewed, "no ID"), func(t *testing.T) {
			f, u := putKueueCluster(t)
			run := recipeKueueRunAs(t, f, u, reviewed)
			res, steps, _ := recipeKueueCleanUp(t, u, run, func(e UpdateStepEvent) {
				if e.Step == "kueue" && e.Status == "running" {
					for _, state := range []string{"Removed", "Unmanaged"} {
						if _, _, err := u.Do(http.MethodPatch, tDSC, "application/merge-patch+json", []byte(`{"spec":{"components":{"kueue":{"managementState":"`+state+`"}}}}`),
							map[string][]string{"fieldManager": {"rhoai-nightly-updater-dsc-kueue"}}); err != nil {
							t.Error(err)
						}
					}
					if err := writeTestPrereqRecord(u, `{"components":{"kueue":{"previous":"Removed","feature":"kueue","id":"en-2"}}}`); err != nil {
						t.Error(err)
					}
				}
			})
			if !res.Success || steps["components"] != "success" || !strings.Contains(res.Message, "Not turned back: kueue (turned on again since you reviewed this Clean up") {
				t.Fatalf("clean-up %+v, steps %v", res, steps)
			}
			if componentState(f, "kueue") != "Unmanaged" || prereqRecordID(t, u, "kueue") != "en-2" {
				t.Fatalf("kueue %s, record %q", componentState(f, "kueue"), prereqRecordID(t, u, "kueue"))
			}
		})
	}
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

// prereqRecordID is the enablement ID the prerequisites record has for key.
func prereqRecordID(t *testing.T, c *Client, key string) string {
	t.Helper()
	rec, err := features.ReadPrereqRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Components[key].ID
}

// A run claims only the enablement its step recorded: a component turned
// back and turned on again (even in the same second) has another ID. A run
// and a record from before IDs match as before; a run without an ID never
// claims an enablement that has one.
func TestRecipeOwnership_ClaimsOnlyItsEnablement(t *testing.T) {
	pc := newPrereqCluster(t)
	if res, _ := runPrereqs(t, pc.c, "pipelines"); !res.Success {
		t.Fatalf("setup %+v", res)
	}
	first := prereqRecordID(t, pc.c, "aipipelines")
	// Turned back, and turned on again right away (the same second, as a
	// rule): another enablement.
	if res, err := features.RestoreComponents(pc.c, []string{"aipipelines"}); err != nil || !res.Success || componentState(pc.f, "aipipelines") != "Removed" {
		t.Fatalf("restore %+v %v", res, err)
	}
	if res, _ := runPrereqs(t, pc.c, "pipelines"); !res.Success {
		t.Fatalf("setup again %+v", res)
	}
	second := prereqRecordID(t, pc.c, "aipipelines")
	if first == "" || second == "" || first == second {
		t.Fatalf("enablement IDs %q and %q", first, second)
	}
	offered := func(ids map[string]string) bool {
		run := &types.RecipeRun{Steps: []types.RecipeRunStep{{ID: recipe.StepPipelinesPrereq}}, Components: []string{"aipipelines"}, ComponentIDs: ids}
		_, req := recipe.Ownership(pc.c, nil, run)
		return req != nil && reflect.DeepEqual(req.Components, []string{"aipipelines"})
	}
	if offered(map[string]string{"aipipelines": first}) {
		t.Error("the run that turned it on first claims the second enablement")
	}
	if !offered(map[string]string{"aipipelines": second}) {
		t.Error("the run that turned it on now does not claim it")
	}
	if offered(nil) {
		t.Error("a run without an ID claims an enablement that has one")
	}
	// A record from before IDs, and a run from before them: as before.
	if err := writeTestPrereqRecord(pc.c, `{"components":{"aipipelines":{"previous":"Removed","feature":"pipelines"}}}`); err != nil {
		t.Fatal(err)
	}
	if !offered(nil) {
		t.Error("a run and a record from before IDs no longer match")
	}
	if offered(map[string]string{"aipipelines": second}) {
		t.Error("a run with an ID claims a record entry without one")
	}
}

// A recipe's Clean up names each component it reviewed: one it does not
// name is not turned back, nor is an empty ID (only an ID or
// ComponentNotIdentified counts). The Environment page's Clean up names
// none and is checked as before.
func TestRestoreReviewedComponents_OnlyWhatWasReviewed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewed map[string]string
		back     bool
	}{
		{"its ID", map[string]string{"kueue": "en-1"}, true},
		{"another ID", map[string]string{"kueue": "en-0"}, false},
		{"not named", map[string]string{"kserve": "en-1"}, false},
		{"an empty ID", map[string]string{"kueue": ""}, false},
		{"none, but the entry has an ID", map[string]string{"kueue": types.ComponentNotIdentified}, false},
		{"the Environment page's", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := newPrereqCluster(t)
			pc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
			u := userClientOf(pc.c, fakeUserToken)
			kueueTurnedOnByToolAs(t, u, "en-1")
			pc.f.groups = map[string]string{kqGroup: kqV1beta2}
			res, err := features.RestoreReviewedComponents(u, []string{"kueue"}, tc.reviewed)
			if err != nil || !res.Success || (componentState(pc.f, "kueue") == "Removed") != tc.back {
				t.Fatalf("%+v %v: kueue %s", res, err, componentState(pc.f, "kueue"))
			}
		})
	}
}
