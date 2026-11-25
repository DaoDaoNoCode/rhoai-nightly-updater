package recipe

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// component: the DataScienceCluster's kueue component on; autoQueue:
	// the LocalQueue RHOAI makes in every Kueue project
	// (autoCreateQueues), "" when off.
	setting, component bool
	autoQueue          string
	// project: the recipe's project exists; incomplete: it was created but
	// its setup was interrupted before its labels (the project step runs
	// again); labelled: it carries kueue.openshift.io/managed=true; queues:
	// its LocalQueues.
	project, incomplete, labelled bool
	queues                        []string
	// toolProfile: the tool's Queue hardware profile is there (disabled:
	// an admin disabled it); adminQueue: an admin's Queue profile for that
	// LocalQueue ("" for none).
	toolProfile, toolDisabled bool
	adminQueue                string
	// sampleMissing: the tool's distributed workloads lack a sample the
	// recipe asks for, so the Kueue step runs to add it.
	sampleMissing bool
	// workbench: the tool's workbench is in the project, with profile.
	workbench bool
	profile   string
	// inputsErr fails the read of the profile inputs.
	inputsErr error
}

const (
	worldProject = "proj"
	// defaultProfile is a hardware profile without a queue, as RHOAI ships
	// its default-profile.
	defaultProfile = "default-profile"
	adminProfile   = "team-queue"
)

// hardwareProfile is a valid workbench hardware profile; queue "" is one
// without Kueue.
func hardwareProfile(t *testing.T, name, queue string, disabled bool) workbench.HardwareProfile {
	t.Helper()
	scheduling := ""
	if queue != "" {
		scheduling = `,"scheduling":{"type":"Queue","kueue":{"localQueueName":"` + queue + `"}}`
	}
	var p workbench.HardwareProfile
	raw := `{"metadata":{"name":"` + name + `","namespace":"redhat-ods-applications","annotations":{"opendatahub.io/disabled":"` + fmt.Sprint(disabled) + `"}},"spec":{"identifiers":[` +
		`{"identifier":"cpu","displayName":"CPU","resourceType":"CPU","defaultCount":"1","minCount":"1"},` +
		`{"identifier":"memory","displayName":"Memory","resourceType":"Memory","defaultCount":"2Gi","minCount":"1Gi"}]` + scheduling + `}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// inputs is what workbench.ReadProfileInputs reads in the world.
func (w *kueueWorld) inputs(t *testing.T) workbench.ProfileInputs {
	in := workbench.ProfileInputs{Global: []workbench.HardwareProfile{hardwareProfile(t, defaultProfile, "", false)},
		SettingOn: w.setting, KueueComponent: w.component, AutoQueue: w.autoQueue}
	if w.adminQueue != "" {
		in.Global = append(in.Global, hardwareProfile(t, adminProfile, w.adminQueue, false))
	}
	if w.toolProfile {
		in.Global = append(in.Global, hardwareProfile(t, kueue.ProfileName, kueue.LocalQueueName, w.toolDisabled))
	}
	if w.project {
		in.ProjectFound, in.Labelled, in.Queues = true, w.labelled, slices.Clone(w.queues)
	}
	return in
}

// choose is the Workbench tab's profile choice in the world's project.
func (w *kueueWorld) choose(t *testing.T) (workbench.ProfileChoice, string) {
	return w.inputs(t).Choose()
}

// install answers the Workbench tab's status (with its plan's refusal, as
// GetStatus does) and the profile inputs from the world.
func (w *kueueWorld) install(t *testing.T) {
	t.Helper()
	ws, wi := workbenchStatus, workbenchProfileInputs
	t.Cleanup(func() { workbenchStatus, workbenchProfileInputs = ws, wi })
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
	workbenchProfileInputs = func(*kube.Client, string) (workbench.ProfileInputs, error) {
		if w.inputsErr != nil {
			return workbench.ProfileInputs{}, w.inputsErr
		}
		return w.inputs(t), nil
	}
}

// labelProject is the project step's run: it labels the project as the
// dashboard's Create project does; RHOAI then makes its LocalQueue there
// when autoCreateQueues is on.
func (w *kueueWorld) labelProject() {
	w.project, w.incomplete, w.labelled = true, false, w.labelled || w.setting
	w.addAutoQueue()
}

func (w *kueueWorld) addAutoQueue() {
	if w.labelled && w.autoQueue != "" && !slices.Contains(w.queues, w.autoQueue) {
		w.queues = append(w.queues, w.autoQueue)
	}
}

// steps are the recipe's steps in the run's order with their After; the
// workbench keeps its own checks, the others are checked against the world.
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
				return Check{Done: !w.incomplete, Reason: "there"}, nil
			}
		case StepKueuePrereq:
			steps[i].Check = func(*kube.Client, *State) (Check, error) { return Check{Done: w.component, Reason: "on"}, nil }
		case StepKueue:
			steps[i].Check = func(*kube.Client, *State) (Check, error) {
				done := w.project && w.labelled && slices.Contains(w.queues, kueue.LocalQueueName) && !w.sampleMissing &&
					(!k.DashboardOptIn || (w.setting && w.toolProfile))
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
		case StepProject:
			w.labelProject()
			s.Project = &types.RecipeProjectRef{Name: worldProject, UID: "uid-proj", Owned: true}
		case StepKueuePrereq:
			w.component = true
		case StepKueue: // the tab's Set up, then Turn on when asked for (it keeps a profile that is there)
			w.labelled, w.sampleMissing = true, false
			if !slices.Contains(w.queues, kueue.LocalQueueName) {
				w.queues = append(w.queues, kueue.LocalQueueName)
			}
			w.addAutoQueue()
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
	return kueueLive{OptInOn: w.setting && w.toolProfile, SettingOn: w.setting, ProjectExists: w.project, ProjectManaged: w.project && w.labelled}
}

// planThenRun: the plan refuses (refusals), or the run sets the workbench
// up; a run the plan let start must not fail.
func (w *kueueWorld) planThenRun(t *testing.T, r types.EnvironmentRecipe) (refusals []string) {
	t.Helper()
	actions, problems := w.plan(r)
	if len(problems) > 0 {
		return problems
	}
	if err := w.run(t, r); err != nil {
		t.Errorf("the plan (%s) let the run start, and it failed halfway: %v", actions, err)
	}
	return nil
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
					w.project, w.labelled, w.queues, w.workbench, w.profile = false, false, nil, false, ""
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
	if len(problems) != 1 || !strings.Contains(problems[0], "the project step labels project proj kueue.openshift.io/managed=true") ||
		!strings.Contains(problems[0], "spec.features.kueue.dashboardOptIn") || !strings.HasSuffix(problems[0], "Nothing was changed.") {
		t.Errorf("without the opt-in: %q", problems)
	}
	if _, problems := w.plan(plain); len(problems) != 1 || !strings.Contains(problems[0], "distributed workloads with dashboardOptIn") {
		t.Errorf("without distributed workloads: %q", problems)
	}
	// An existing project that is Kueue's while Kueue is not available:
	// the Workbench tab's refusal, with what the recipe can change.
	labelled := &kueueWorld{project: true, labelled: true, queues: []string{kueue.LocalQueueName}, component: true}
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
	// RHOAI or the DataScienceCluster will change what the choice reads:
	// checked when the run gets there.
	if action, _, _ := planStep(testClient(), &State{Recipe: noOptIn}, stepOf(t, noOptIn, StepWorkbench), map[string]bool{StepDSC: true, StepProject: true}); action != types.RecipeStepLater {
		t.Errorf("after the DataScienceCluster: %s", action)
	}

	w.inputsErr = errors.New("forbidden")
	willRun := map[string]bool{StepProject: true}
	if action, reason, fix := planStep(testClient(), &State{Recipe: plain}, stepOf(t, plain, StepWorkbench), willRun); action != types.RecipeStepBlocked ||
		!strings.Contains(reason, "Cannot check it: forbidden") || fix == nil || fix.Code != types.RecipeFixCheckFailed || willRun[StepWorkbench] {
		t.Errorf("unreadable inputs: %s %q %+v", action, reason, fix)
	}
	workbenchStatus = func(_, _ *kube.Client, _ string) (*types.WorkbenchStatus, error) { return nil, errors.New("timeout") }
	willRun = map[string]bool{StepS3: true}
	existing := &State{Recipe: plain, Project: &types.RecipeProjectRef{Name: "proj", UID: "u", Owned: true}}
	if action, reason, _ := planStep(testClient(), existing, stepOf(t, plain, StepWorkbench), willRun); action != types.RecipeStepBlocked || !strings.Contains(reason, "timeout") {
		t.Errorf("an unreadable workbench: %s %q", action, reason)
	}
	// Checked now: a plan the Workbench tab cannot make for a read that
	// failed (here its LocalQueues) refuses as a failed check.
	workbenchStatus = func(_, _ *kube.Client, _ string) (*types.WorkbenchStatus, error) {
		return &types.WorkbenchStatus{PlanProblem: "Cannot read the project's LocalQueues: forbidden.", PlanProblemCode: "forbidden"}, nil
	}
	chk, err := checkWorkbench(testClient(), existing)
	if err != nil || !strings.Contains(chk.Blocked, "LocalQueues: forbidden") || chk.Fix == nil || chk.Fix.Code != types.RecipeFixCheckFailed {
		t.Errorf("a failed read, checked now: %+v %v", chk, err)
	}
}

// The cases the reviews raised (N12 follow-up): each is refused by the plan
// or set up by the run, never failed halfway.
func TestRecipeWorkbenchKueue_ResumeDisabledProfileAutoQueue(t *testing.T) {
	plain := validRecipe(t, "  project: {mode: create, name: proj}\n  features:\n    workbench: {enabled: true}\n")
	optIn := validRecipe(t, "  project: {mode: create, name: proj}\n  features:\n    workbench: {enabled: true}\n    kueue: {enabled: true, rayCluster: true, dashboardOptIn: true}\n")

	// A resumed run after an interrupted project setup: the project is
	// there, unlabelled and without a LocalQueue, and the project step will
	// label it now that the setting is on.
	w := &kueueWorld{setting: true, component: true, project: true, incomplete: true}
	w.install(t)
	if refusals := w.planThenRun(t, plain); len(refusals) != 1 || !strings.Contains(refusals[0], "the project step labels project proj") {
		t.Errorf("resume after an interrupted project setup: %q", refusals)
	}
	// The same with the setting off: no label, a regular profile.
	w = &kueueWorld{project: true, incomplete: true}
	w.install(t)
	if refusals := w.planThenRun(t, plain); len(refusals) != 0 || w.profile != defaultProfile {
		t.Errorf("resume, the setting off: %q %q", refusals, w.profile)
	}

	// The Kueue step runs only to add a sample, and an admin disabled the
	// tool's Queue profile: Turn on keeps it, so no profile fits.
	w = &kueueWorld{setting: true, component: true, project: true, labelled: true, queues: []string{kueue.LocalQueueName}, toolProfile: true, toolDisabled: true, sampleMissing: true}
	w.install(t)
	if refusals := w.planThenRun(t, optIn); len(refusals) != 1 || !strings.Contains(refusals[0], "no enabled workbench hardware profile") {
		t.Errorf("the tool's profile disabled: %q", refusals)
	}
	// Enabled, the same run sets the workbench up on it.
	w.toolDisabled = false
	if refusals := w.planThenRun(t, optIn); len(refusals) != 0 || w.profile != kueue.ProfileName {
		t.Errorf("the tool's profile enabled: %q %q", refusals, w.profile)
	}

	// RHOAI makes LocalQueue default in every Kueue project
	// (autoCreateQueues), and an admin's Queue profile sends to it: a new
	// project works without distributed workloads in the recipe.
	w = &kueueWorld{setting: true, component: true, autoQueue: "default", adminQueue: "default"}
	w.install(t)
	if refusals := w.planThenRun(t, plain); len(refusals) != 0 || w.profile != adminProfile {
		t.Errorf("autoCreateQueues with a Queue profile for it: %q %q", refusals, w.profile)
	}
	// Without such a profile the review refuses.
	w = &kueueWorld{setting: true, component: true, autoQueue: "default"}
	w.install(t)
	if refusals := w.planThenRun(t, plain); len(refusals) != 1 {
		t.Errorf("autoCreateQueues, no Queue profile for it: %q", refusals)
	}
}
