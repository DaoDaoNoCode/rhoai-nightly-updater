package recipe

import (
	"slices"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// everyStepRecipe enables all features so Steps emits the full step list; when
// exposure is "" the model gets a dedicated step for its own access pattern.
func everyStepRecipe(exposure string) types.EnvironmentRecipe {
	var r types.EnvironmentRecipe
	r.Metadata.Name = "all"
	r.Spec.Project = &types.RecipeProject{Mode: "create", Name: "recipe-all"}
	f := &r.Spec.Features
	f.Pipelines.Enabled, f.Playground.Enabled, f.Workbench.Enabled, f.ModelRegistry.Enabled = true, true, true, true
	f.MLflow = types.RecipeMLflow{Enabled: true, Samples: true}
	f.Models = types.RecipeModels{Enabled: true, Exposure: exposure}
	f.MaaS = types.RecipeMaaS{Enabled: true, Publish: true}
	f.Kueue = types.RecipeKueue{Enabled: true, DashboardOptIn: true}
	r.Spec.DashboardDev = types.RecipeDashboardDev{Deploy: DashboardDevMain}
	return r
}

// ancestors collects every step that must finish before id, whether linked
// directly or via intermediate steps.
func ancestors(steps []Step, id string) map[string]bool {
	after := map[string][]string{}
	for _, s := range steps {
		after[s.ID] = s.After
	}
	out := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		for _, a := range after[id] {
			if !out[a] {
				out[a] = true
				walk(a)
			}
		}
	}
	walk(id)
	return out
}

// ordered is true when a and b sit on opposite sides of an After edge, which
// forbids concurrent execution.
func ordered(steps []Step, a, b string) bool {
	return ancestors(steps, a)[b] || ancestors(steps, b)[a]
}

// Order is treated as data and must satisfy: each step appears exactly once;
// every After entry points at a strictly earlier position (topological order,
// acyclic graph); each step's check runs only after every step in stepDependsOn
// has completed; prerequisite steps never share a time slice; lane membership
// matches the documented layout; Kueue follows all other features; Dashboard
// Dev is last.
func TestSteps_DependencyOrder(t *testing.T) {
	listed := map[string]int{}
	for _, id := range stepsFirst {
		listed[id]++
	}
	for _, lane := range stepLanes {
		for _, id := range lane {
			listed[id]++
		}
	}
	for _, id := range stepsLast {
		listed[id]++
	}
	for _, exposure := range []string{"", "default-gateway"} {
		steps := Steps(everyStepRecipe(exposure))
		pos := map[string]int{}
		for i, s := range steps {
			pos[s.ID] = i
			if listed[s.ID] != 1 {
				t.Errorf("step %s is in the order %d times, want once", s.ID, listed[s.ID])
			}
			for _, a := range s.After {
				if j, ok := pos[a]; !ok || j >= i {
					t.Errorf("%s comes after %s, which is not an earlier step", s.ID, a)
				}
			}
			for _, dep := range stepDependsOn[s.ID] {
				if _, ok := pos[dep]; ok && !ancestors(steps, s.ID)[dep] {
					t.Errorf("%s may start before %s, whose outcome its check depends on", s.ID, dep)
				}
			}
		}
		// Prerequisite steps (each rewrites the full record it loaded) must not
		// overlap in time.
		prereqs := []string{StepPrerequisites, StepModelsPrereq, StepKueuePrereq}
		for i, a := range prereqs {
			for _, b := range prereqs[i+1:] {
				if _, ok := pos[a]; ok {
					if _, ok := pos[b]; ok && !ordered(steps, a, b) {
						t.Errorf("%s and %s may run at the same time", a, b)
					}
				}
			}
		}
		byID := map[string]Step{}
		for _, s := range steps {
			byID[s.ID] = s
		}
		want := map[string][]string{
			StepDSC: {StepRHOAI}, StepProject: {StepDSC}, StepPrerequisites: {StepProject},
			StepS3: {StepPrerequisites}, StepPipelineServer: {StepS3}, StepWorkbench: {StepPipelineServer},
			StepMLflow: {StepPrerequisites}, StepMLflowSamples: {StepMLflow},
			StepModelRegistry: {StepPrerequisites},
			StepMaaS:          {StepPrerequisites}, StepMaaSPublish: {StepModel}, StepPlayground: {StepMaaSPublish},
			StepKueuePrereq: {StepWorkbench, StepMLflowSamples, StepModelRegistry, StepPlayground}, StepKueue: {StepKueuePrereq},
			StepDashboardDev: {StepKueue},
		}
		if exposure == "" {
			want[StepModelsPrereq], want[StepModel] = []string{StepMaaS}, []string{StepModelsPrereq}
		} else {
			want[StepModel] = []string{StepMaaS}
		}
		for id, after := range want {
			if got := byID[id].After; !slices.Equal(got, after) {
				t.Errorf("exposure %q: %s comes after %v, want %v", exposure, id, got, after)
			}
		}
		if len(byID[StepRHOAI].After) != 0 {
			t.Errorf("RHOAI comes after %v", byID[StepRHOAI].After)
		}
		// Opting into Kueue alters workbench and model-deployment creation, so
		// every non-Kueue feature step must precede it.
		for _, s := range steps {
			if s.ID != StepKueuePrereq && s.ID != StepKueue && s.ID != StepDashboardDev && !ancestors(steps, StepKueuePrereq)[s.ID] {
				t.Errorf("%s may run beside the Kueue steps", s.ID)
			}
		}
		// Lanes that do not depend on one another may execute concurrently.
		for _, pair := range [][2]string{{StepS3, StepMLflow}, {StepS3, StepMaaS}, {StepModelRegistry, StepModel}, {StepWorkbench, StepPlayground}} {
			if ordered(steps, pair[0], pair[1]) {
				t.Errorf("%s and %s wait for each other", pair[0], pair[1])
			}
		}
	}
}

// Partial feature sets skip empty lanes without adding waits, while steps
// placed after the lane block still wait only on steps that actually exist.
func TestSteps_DependencyOrderWithFewFeatures(t *testing.T) {
	var r types.EnvironmentRecipe
	r.Spec.Features.Kueue.Enabled = true
	r.Spec.Features.MLflow.Enabled = true
	steps := Steps(r)
	got := map[string][]string{}
	for _, s := range steps {
		got[s.ID] = s.After
	}
	if !slices.Equal(got[StepMLflow], []string{StepPrerequisites}) || !slices.Equal(got[StepKueuePrereq], []string{StepMLflow}) {
		t.Errorf("order %v", got)
	}
	r.Spec.Features.MLflow.Enabled = false
	steps = Steps(r)
	for _, s := range steps {
		got[s.ID] = s.After
	}
	if !slices.Equal(got[StepKueuePrereq], []string{StepPrerequisites}) {
		t.Errorf("without lanes the Kueue steps come after %v", got[StepKueuePrereq])
	}
}

// RunSeconds models the critical path—the longest dependent chain, not the
// sum of durations—while capping concurrent lanes so a fourth lane queues for
// a slot and that queue time is included. Steps absent from the plan contribute
// zero duration.
func TestRunSeconds_CriticalPath(t *testing.T) {
	step := func(id string, after ...string) Step { return Step{ID: id, After: after} }
	diamond := []Step{step("a"), step("b", "a"), step("c", "a"), step("d", "c"), step("e", "b", "d")}
	if got := RunSeconds(diamond, []int{10, 100, 30, 40, 5}); got != 10+100+5 {
		t.Errorf("diamond: %d, want the longest chain 115", got)
	}
	if got := RunSeconds(diamond, []int{10, 0, 30, 40, 5}); got != 10+30+40+5 {
		t.Errorf("diamond with b set up: %d, want 85", got)
	}
	wide := []Step{step("head"), step("l1", "head"), step("l2", "head"), step("l3", "head"), step("l4", "head")}
	if got := RunSeconds(wide, []int{1, 60, 60, 60, 60}); got != 1+60+60 {
		t.Errorf("four lanes of one minute: %d, want 121 (three at once)", got)
	}
	if got := RunSeconds(wide[:4], []int{1, 60, 60, 60}); got != 61 {
		t.Errorf("three lanes: %d, want 61", got)
	}

	steps := Steps(everyStepRecipe(""))
	typical, sum, critical := make([]int, len(steps)), 0, 0
	onPath := map[string]bool{StepRHOAI: true, StepDSC: true, StepProject: true, StepPrerequisites: true, StepMaaS: true, StepModelsPrereq: true,
		StepModel: true, StepMaaSPublish: true, StepPlayground: true, StepKueuePrereq: true, StepKueue: true, StepDashboardDev: true}
	for i, s := range steps {
		typical[i], _ = s.Seconds()
		sum += typical[i]
		if onPath[s.ID] {
			critical += typical[i]
		}
	}
	if got := RunSeconds(steps, typical); got != critical || got >= sum {
		t.Errorf("every feature: %v, want the serving lane's chain %v (every step %v)", time.Duration(got)*time.Second, time.Duration(critical)*time.Second, time.Duration(sum)*time.Second)
	}
}

// Parallel steps each hold a private clone of run State; Merge applies only
// deltas from a step, leaving concurrent merges from other steps intact.
func TestState_CloneAndMerge(t *testing.T) {
	run := State{RunID: "run-1", Project: &types.RecipeProjectRef{Name: "p"}, Components: []string{"base"}, ComponentIDs: map[string]string{"base": "en-base"},
		Created: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: "k-1"}}}
	baseA, baseB := run.Clone(), run.Clone()
	a, b := baseA.Clone(), baseB.Clone()
	a.AddComponentID("aipipelines", "en-a")
	a.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "m-1"})
	a.Exposure = "maas"
	b.AddComponentID("modelregistry", "en-b")
	b.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: "r-1"})
	b.Project.UID = "uid-p"
	if len(run.Components) != 1 || run.Project.UID != "" || len(run.Created) != 1 {
		t.Fatalf("a step's copy changed the run's State: %+v", run)
	}
	run.Merge(baseA, a)
	run.Merge(baseB, b)
	if !slices.Equal(run.Components, []string{"aipipelines", "base", "modelregistry"}) || len(run.ComponentIDs) != 3 || run.ComponentIDs["modelregistry"] != "en-b" {
		t.Errorf("components %v %v", run.Components, run.ComponentIDs)
	}
	if len(run.Created) != 3 || run.Exposure != "maas" || run.Project.UID != "uid-p" || run.RunID != "run-1" {
		t.Errorf("state %+v", run)
	}
	// Merging an identical snapshot must not alter the run, including fields
	// another step may have updated after this one started.
	base := run.Clone()
	run.Exposure = "internal"
	run.Merge(base, base.Clone())
	if run.Exposure != "internal" {
		t.Errorf("an unchanged copy reset the exposure to %q", run.Exposure)
	}
}
