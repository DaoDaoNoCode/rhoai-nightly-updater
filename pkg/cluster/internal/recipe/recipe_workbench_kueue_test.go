package recipe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// N12 (live on RHOAI 3.7.0-ea.1): a recipe with a workbench and distributed
// workloads with the dashboard's opt-in, run again after a Clean up of its
// project only, failed at the workbench step: the dashboard's Kueue setting
// was on, so the project step labelled the new project
// kueue.openshift.io/managed=true (as the dashboard's Create project does),
// and the workbench came before the Kueue step that gives the project its
// LocalQueue and the Queue hardware profile. These tests pin every cell:
// with or without distributed workloads and their opt-in, the setting on or
// off before the run, a first run, a run after a Clean up of the project, and
// a run after the workbench alone was deleted. A cell either is refused by
// the plan or its run sets the workbench up; none fails halfway.

// kueueWorld is the cluster as the workbench and Kueue steps see it.
type kueueWorld struct {
	// setting: the dashboard's Kueue setting (disableKueue false);
	// component: the DataScienceCluster's kueue component on.
	setting, component bool
	// project: the recipe's project exists; labelled: it carries
	// kueue.openshift.io/managed=true; queue: it has the tool's LocalQueue.
	project, labelled, queue bool
	// toolProfile: the tool's Queue hardware profile is there.
	toolProfile bool
	// workbench: the tool's workbench is in the project, with profile.
	workbench bool
	profile   string
}

const (
	worldProject = "proj"
	// defaultProfile is a hardware profile without a queue, as RHOAI ships
	// its default-profile.
	defaultProfile = "default-profile"
)

// hardwareProfile is a valid, enabled workbench hardware profile; queue ""
// is one without Kueue.
func hardwareProfile(t *testing.T, name, queue string) workbench.HardwareProfile {
	t.Helper()
	scheduling := ""
	if queue != "" {
		scheduling = `,"scheduling":{"type":"Queue","kueue":{"localQueueName":"` + queue + `"}}`
	}
	var p workbench.HardwareProfile
	raw := `{"metadata":{"name":"` + name + `","namespace":"redhat-ods-applications"},"spec":{"identifiers":[` +
		`{"identifier":"cpu","displayName":"CPU","resourceType":"CPU","defaultCount":"1","minCount":"1"},` +
		`{"identifier":"memory","displayName":"Memory","resourceType":"Memory","defaultCount":"2Gi","minCount":"1Gi"}]` + scheduling + `}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// choose is the Workbench tab's profile choice in the world's project
// (workbench.BuildPlan): the dashboard's Kueue rule, then the first
// profile it keeps.
func (w *kueueWorld) choose(t *testing.T) (workbench.ProfileChoice, string) {
	profiles := []workbench.HardwareProfile{hardwareProfile(t, defaultProfile, "")}
	if w.toolProfile {
		profiles = append(profiles, hardwareProfile(t, kueue.ProfileName, kueue.LocalQueueName))
	}
	var queues []string
	if w.queue {
		queues = []string{kueue.LocalQueueName}
	}
	return workbench.ChooseProfile(profiles, nil, workbench.ModeFor(w.labelled, w.setting && w.component), queues)
}

// install answers the Workbench tab's status (with its plan's refusal, as
// GetStatus does) and the dashboard's setting from the world.
func (w *kueueWorld) install(t *testing.T) {
	t.Helper()
	ws, dk := workbenchStatus, dashboardKueueOn
	t.Cleanup(func() { workbenchStatus, dashboardKueueOn = ws, dk })
	workbenchStatus = func(_, _ *kube.Client, project string) (*types.WorkbenchStatus, error) {
		st := &types.WorkbenchStatus{Project: project}
		if w.workbench {
			st.Items = []types.WorkbenchState{{ResourceState: types.ResourceState{Namespace: project, Name: workbench.Name, Deployed: true, ManagedByTool: true}}}
		}
		if !w.project {
			st.PlanProblem, st.PlanProblemCode = "Project "+project+" is not there.", "not_found"
			return st, nil
		}
		if _, why := w.choose(t); why != "" {
			st.PlanProblem, st.PlanProblemCode = why, "no_profile_project"
			if strings.HasPrefix(why, "No enabled") {
				st.PlanProblemCode = "no_profile"
			}
		}
		return st, nil
	}
	dashboardKueueOn = func(*kube.Client) (bool, error) { return w.setting, nil }
}

// steps are the recipe's steps in the run's order with their After; the
// workbench keeps its own check, the others are checked against the world.
func (w *kueueWorld) steps(r types.EnvironmentRecipe) []Step {
	steps := Steps(r)
	k := r.Spec.Features.Kueue
	for i := range steps {
		switch steps[i].ID {
		case StepWorkbench:
		case StepProject:
			steps[i].Check = func(_ *kube.Client, s *State) (Check, error) {
				if !w.project {
					s.Project = nil
					return Check{}, nil
				}
				s.Project = &types.RecipeProjectRef{Name: worldProject, UID: "uid-proj", Owned: true}
				return Check{Done: true, Reason: "there"}, nil
			}
		case StepKueuePrereq:
			steps[i].Check = func(*kube.Client, *State) (Check, error) { return Check{Done: w.component, Reason: "on"}, nil }
		case StepKueue:
			steps[i].Check = func(*kube.Client, *State) (Check, error) {
				done := w.project && w.labelled && w.queue && (!k.DashboardOptIn || (w.setting && w.toolProfile))
				return Check{Done: done, Reason: "set up"}, nil
			}
		default:
			steps[i].Check = func(*kube.Client, *State) (Check, error) { return Check{Done: true, Reason: "set up"}, nil }
		}
	}
	return steps
}

// plan is what the review says: the steps' actions and its refusals.
func (w *kueueWorld) plan(r types.EnvironmentRecipe) (actions string, refused []string) {
	s := &State{Recipe: r}
	willRun := map[string]bool{}
	var out []string
	for _, step := range w.steps(r) {
		action, reason, _ := planStep(testClient(), s, step, willRun)
		out = append(out, step.ID+"="+action)
		if action == types.RecipeStepBlocked {
			refused = append(refused, step.ID+": "+reason)
		}
	}
	return strings.Join(out, " "), refused
}

// run sets the recipe up in the world, step after step in the run's order
// (this recipe has no steps side by side): each step's check first, as the
// runner does, then what the step changes on the cluster.
func (w *kueueWorld) run(t *testing.T, r types.EnvironmentRecipe) error {
	s := &State{Recipe: r}
	for _, step := range w.steps(r) {
		chk, err := step.Check(testClient(), s)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", step.ID, err)
		case chk.Blocked != "":
			return fmt.Errorf("%s refused: %s", step.ID, chk.Blocked)
		case chk.Done:
			continue
		}
		switch step.ID {
		case StepProject: // EnsureProject labels it as the dashboard's Create project does
			w.project, w.labelled = true, w.setting
			s.Project = &types.RecipeProjectRef{Name: worldProject, UID: "uid-proj", Owned: true}
		case StepKueuePrereq:
			w.component = true
		case StepKueue: // the tab's Set up, then Turn on when asked for
			w.labelled, w.queue = true, true
			if r.Spec.Features.Kueue.DashboardOptIn {
				w.setting, w.toolProfile = true, true
			}
		case StepWorkbench:
			choice, why := w.choose(t)
			if why != "" {
				return fmt.Errorf("workbench failed: %s", why)
			}
			w.workbench, w.profile = true, choice.Name
		}
	}
	return nil
}

// live is what the plan's Kueue notes read before the run.
func (w *kueueWorld) live() kueueLive {
	return kueueLive{OptInOn: w.setting && w.toolProfile, SettingOn: w.setting, ProjectExists: w.project}
}

func TestRecipeWorkbenchKueue_EveryCellRefusedOrSetUp(t *testing.T) {
	const (
		refused = "refused"
		base    = "  project: {mode: create, name: proj}\n  features:\n    workbench: {enabled: true}\n"
		noOptIn = "    kueue: {enabled: true, rayCluster: true, rayJob: true}\n"
		optIn   = "    kueue: {enabled: true, rayCluster: true, rayJob: true, dashboardOptIn: true}\n"
	)
	queue := kueue.ProfileName
	for _, tc := range []struct {
		name    string
		kueue   string
		setting bool
		// want: the workbench's profile (or refused) on a first run, a
		// run after a Clean up of the project, and a run after the
		// workbench alone was deleted.
		want [3]string
	}{
		{"no distributed workloads, the setting off", "", false, [3]string{defaultProfile, defaultProfile, defaultProfile}},
		{"no distributed workloads, the setting on", "", true, [3]string{refused, refused, refused}},
		// The project keeps the Kueue step's label while the setting stays
		// off: the dashboard offers a workbench there no profile at all.
		{"no opt-in, the setting off", noOptIn, false, [3]string{defaultProfile, defaultProfile, refused}},
		{"no opt-in, the setting on", noOptIn, true, [3]string{refused, refused, refused}},
		{"opt-in, the setting off", optIn, false, [3]string{queue, queue, queue}},
		{"opt-in, the setting on", optIn, true, [3]string{queue, queue, queue}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := validRecipe(t, base+tc.kueue)
			// Whoever turned the setting on also turned Kueue on, with the
			// tool's Queue profile (the most a cluster can offer).
			w := &kueueWorld{setting: tc.setting, component: tc.setting, toolProfile: tc.setting}
			w.install(t)
			for i, when := range []string{"first run", "after a Clean up of the project", "after the workbench was deleted"} {
				switch i {
				case 1:
					w.project, w.labelled, w.queue, w.workbench, w.profile = false, false, false, false, ""
				case 2:
					w.workbench, w.profile = false, ""
				}
				if note := kueueWorkbenchNote(r, w.live()); note != "" {
					t.Errorf("%s: the plan notes %q", when, note)
				}
				actions, problems := w.plan(r)
				if len(problems) > 0 {
					if tc.want[i] != refused {
						t.Errorf("%s: refused (%s): %v", when, actions, problems)
					}
					continue
				}
				if err := w.run(t, r); err != nil {
					t.Errorf("%s: the plan (%s) let the run start, and it failed halfway: %v", when, actions, err)
					continue
				}
				if w.profile != tc.want[i] {
					t.Errorf("%s: the workbench got %q, want %q", when, w.profile, tc.want[i])
				}
			}
		})
	}
}

// The refusal says why and what to change in the recipe, and the plan
// refuses before anything runs; a read that fails refuses too (fail
// closed).
func TestRecipeWorkbenchKueue_RefusalsAndReadFailures(t *testing.T) {
	noOptIn := validRecipe(t, "  project: {mode: create, name: proj}\n  features:\n    workbench: {enabled: true}\n    kueue: {enabled: true}\n")
	plain := validRecipe(t, "  project: {mode: create, name: proj}\n  features:\n    workbench: {enabled: true}\n")
	w := &kueueWorld{setting: true, component: true}
	w.install(t)
	_, problems := w.plan(noOptIn)
	if len(problems) != 1 || !strings.Contains(problems[0], "project proj is created with the label kueue.openshift.io/managed=true") ||
		!strings.Contains(problems[0], "spec.features.kueue.dashboardOptIn") || !strings.HasSuffix(problems[0], "Nothing was changed.") {
		t.Errorf("without the opt-in: %q", problems)
	}
	if _, problems := w.plan(plain); len(problems) != 1 || !strings.Contains(problems[0], "distributed workloads with dashboardOptIn") {
		t.Errorf("without distributed workloads: %q", problems)
	}
	// An existing project that is Kueue's while Kueue is not available:
	// the Workbench tab's refusal, with what the recipe can change.
	labelled := &kueueWorld{project: true, labelled: true, queue: true, component: true}
	labelled.install(t)
	if _, problems := labelled.plan(noOptIn); len(problems) != 1 || !strings.Contains(problems[0], "Kueue is not available on this cluster") ||
		!strings.Contains(problems[0], "spec.features.kueue.dashboardOptIn") {
		t.Errorf("a Kueue project, the setting off: %q", problems)
	}

	// A project step that is refused (another's project of that name) says
	// why itself: the workbench adds nothing.
	w.install(t)
	if action, reason, _ := planStep(testClient(), &State{Recipe: noOptIn}, stepOf(t, noOptIn, StepWorkbench), map[string]bool{StepS3: true}); action != types.RecipeStepLater {
		t.Errorf("the project refused: %s %q", action, reason)
	}

	dashboardKueueOn = func(*kube.Client) (bool, error) { return false, errors.New("forbidden") }
	willRun := map[string]bool{StepProject: true}
	if action, reason, fix := planStep(testClient(), &State{Recipe: plain}, stepOf(t, plain, StepWorkbench), willRun); action != types.RecipeStepBlocked ||
		!strings.Contains(reason, "Cannot check it: read the dashboard's Kueue setting: forbidden") || fix == nil || fix.Code != types.RecipeFixCheckFailed || willRun[StepWorkbench] {
		t.Errorf("an unreadable setting: %s %q %+v", action, reason, fix)
	}
	workbenchStatus = func(_, _ *kube.Client, _ string) (*types.WorkbenchStatus, error) { return nil, errors.New("timeout") }
	willRun = map[string]bool{StepS3: true}
	existing := &State{Recipe: plain, Project: &types.RecipeProjectRef{Name: "proj", UID: "u", Owned: true}}
	if action, reason, _ := planStep(testClient(), existing, stepOf(t, plain, StepWorkbench), willRun); action != types.RecipeStepBlocked || !strings.Contains(reason, "timeout") {
		t.Errorf("an unreadable workbench: %s %q", action, reason)
	}
}
