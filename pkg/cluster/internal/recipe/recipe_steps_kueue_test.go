package recipe

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const kueueRecipe = "  project: {mode: existing, name: proj}\n  features:\n    kueue: {enabled: true, rayCluster: true, dashboardOptIn: true}\n"

// fakeKueue mirrors status and operations on the Distributed workloads tab;
// fixture state reflects whatever the Set up and Turn on paths produced.
type fakeKueue struct {
	st       types.KueueStatus
	rec      kueue.Record
	cq       *kube.ObjectMeta
	profile  *kube.ObjectMeta
	reqs     []types.KueueRequest
	ann      []map[string]string
	readErr  error
	setupErr bool
}

func (fk *fakeKueue) install(t *testing.T) {
	t.Helper()
	k0, k1, k2, k3, k4 := kueueStatus, runKueue, kueueRecord, kueueCQMeta, kueueProfileMeta
	t.Cleanup(func() { kueueStatus, runKueue, kueueRecord, kueueCQMeta, kueueProfileMeta = k0, k1, k2, k3, k4 })
	kueueStatus = func(_, _ *kube.Client, project string) (*types.KueueStatus, error) {
		st := fk.st
		return &st, nil
	}
	kueueRecord = func(*kube.Client) (kueue.Record, error) {
		if fk.readErr != nil {
			return kueue.Record{}, fk.readErr
		}
		return fk.rec, nil
	}
	kueueCQMeta = func(*kube.Client) (kube.ObjectMeta, bool, error) {
		if fk.cq == nil {
			return kube.ObjectMeta{}, false, nil
		}
		return *fk.cq, true, nil
	}
	kueueProfileMeta = func(_ *kube.Client, ns string) (kube.ObjectMeta, bool, error) {
		if fk.profile == nil {
			return kube.ObjectMeta{}, false, nil
		}
		return *fk.profile, true, nil
	}
	runKueue = func(_ *kube.Client, req types.KueueRequest, annotations map[string]string, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fk.reqs = append(fk.reqs, req)
		fk.ann = append(fk.ann, annotations)
		if fk.setupErr {
			return &types.OperationResponse{Success: false, Message: "refused", ErrorCode: "validation"}, nil
		}
		switch req.Action {
		case "setup":
			if fk.cq == nil {
				m := toolMeta("", kueue.ClusterQueueName, "cq-1", annotations)
				fk.cq, fk.rec.ClusterQueueUID = &m, "cq-1"
			}
			fk.st = setUpKueue(req.Project, req.RayCluster, req.RayJob, optInLive(&fk.st))
		case "optin":
			m := toolMeta("redhat-ods-applications", kueue.ProfileName, "prof-1", annotations)
			fk.profile = &m
			fk.rec.OptIn = &kueue.OptIn{DisableKueueSet: true, ProfileUID: "prof-1", ProfileNamespace: "redhat-ods-applications"}
			fk.st.OptIn = liveOptIn(true)
		}
		return &types.OperationResponse{Success: true, Message: req.Action + " done."}, nil
	}
}

// liveOptIn is the tab's opt-in status: in effect (the switch on, the
// tool's profile there), or off.
func liveOptIn(on bool) types.KueueOptIn {
	if !on {
		return types.KueueOptIn{}
	}
	f := false
	return types.KueueOptIn{On: true, DisableKueue: &f, Profile: types.KueueObject{Kind: "HardwareProfile", Name: kueue.ProfileName, Present: true, ManagedByTool: true}}
}

// setUpKueue builds the tab-facing status for Kueue configured in a project.
func setUpKueue(project string, rayCluster, rayJob, optIn bool) types.KueueStatus {
	st := types.KueueStatus{Project: project, Label: &types.KueueLabel{Present: true, AddedByTool: true}, OptIn: liveOptIn(optIn)}
	for _, k := range []string{"ResourceFlavor", "ClusterQueue", "LocalQueue"} {
		st.Objects = append(st.Objects, types.KueueObject{Kind: k, Name: strings.ToLower(k), Present: true, ManagedByTool: true})
	}
	st.Samples = []types.KueueSample{{Kind: kueue.SampleRayCluster, Present: rayCluster, ManagedByTool: rayCluster}, {Kind: kueue.SampleRayJob, Present: rayJob, ManagedByTool: rayJob}}
	return st
}

// checkKueue consults tab state: matching the recipe means done (resume leaves
// it alone); gaps among objects, labels, samples, or requested opt-in trigger a
// run; tab-level refusal surfaces as a block (closed on failure).
func TestKueueStep_CheckSkipsRunsOrBlocks(t *testing.T) {
	r := validRecipe(t, kueueRecipe)
	cases := []struct {
		name    string
		st      func() types.KueueStatus
		done    bool
		blocked string
	}{
		{"set up as asked", func() types.KueueStatus { return setUpKueue("proj", true, false, true) }, true, ""},
		{"nothing yet", func() types.KueueStatus { return types.KueueStatus{} }, false, ""},
		{"the opt-in not on", func() types.KueueStatus { return setUpKueue("proj", true, false, false) }, false, ""},
		{"the sample missing", func() types.KueueStatus { return setUpKueue("proj", false, false, true) }, false, ""},
		{"the label missing", func() types.KueueStatus { st := setUpKueue("proj", true, false, true); st.Label = nil; return st }, false, ""},
		{"a queue being deleted", func() types.KueueStatus {
			st := setUpKueue("proj", true, false, true)
			st.Objects[1].Terminating = true
			return st
		}, false, ""},
		{"the component off (its prerequisites' step)", func() types.KueueStatus {
			return types.KueueStatus{Blocked: &types.ComponentBlock{Component: "kueue"}, Refusal: "kueue is off"}
		}, false, ""},
		{"set up in another project", func() types.KueueStatus { return types.KueueStatus{SetUpIn: "other"} }, false, "set up in project other"},
		{"a LocalQueue named default", func() types.KueueStatus { return types.KueueStatus{DefaultQueue: true} }, false, "LocalQueue named default"},
		{"v1beta2 not served", func() types.KueueStatus {
			return types.KueueStatus{Refusal: "This cluster does not serve kueue.x-k8s.io/v1beta2.", RefusalCode: "prerequisites"}
		}, false, "v1beta2"},
		{"labelPolicy None", func() types.KueueStatus { return types.KueueStatus{Refusal: "Kueue's labelPolicy is None."} }, false, "labelPolicy"},
		{"KueueReady not True", func() types.KueueStatus { return types.KueueStatus{Refusal: "KueueReady is False."} }, false, "KueueReady"},
		{"Kueue outside OLM", func() types.KueueStatus { return types.KueueStatus{Refusal: "Kueue runs outside OLM."} }, false, "OLM"},
		{"a foreign ClusterQueue", func() types.KueueStatus {
			return types.KueueStatus{Objects: []types.KueueObject{{Kind: "ClusterQueue", Name: "cq", Present: true, Foreign: true}}}
		}, false, "not created by this tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fk := &fakeKueue{st: tc.st()}
			fk.install(t)
			chk, err := checkKueue(nil, &State{Recipe: r})
			if err != nil || chk.Done != tc.done || (tc.blocked == "") != (chk.Blocked == "") || !strings.Contains(chk.Blocked, tc.blocked) {
				t.Fatalf("%+v %v", chk, err)
			}
			if len(fk.reqs) != 0 {
				t.Errorf("a check set something up: %v", fk.reqs)
			}
		})
	}
}

// runKueueStep drives tab Set up (samples and image from the recipe), then Turn
// on for dashboard opt-in; cluster-scoped objects carry the run annotation, and
// the created ClusterQueue lands in Created for this run. After that, check
// reports done on resume.
func TestKueueStep_SetsUpAsTheTabAndRecordsWhatItCreated(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    kueue: {enabled: true, rayJob: true, rayImage: quay.io/x/ray:2, dashboardOptIn: true}\n")
	fk := &fakeKueue{}
	fk.install(t)
	s := &State{Recipe: r, RunID: "run-1"}
	res, err := runKueueStep(testClient(), s, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
	if len(fk.reqs) != 2 || fk.reqs[0] != (types.KueueRequest{Action: "setup", Project: "proj", RayJob: true, RayImage: "quay.io/x/ray:2"}) || fk.reqs[1].Action != "optin" {
		t.Errorf("requests %+v", fk.reqs)
	}
	for _, a := range fk.ann {
		if a[RecipeRunAnnotation] != "run-1" {
			t.Errorf("annotations %v", fk.ann)
		}
	}
	if len(s.Created) != 1 || s.Created[0] != (types.RecipeCreatedPart{Part: types.RecipePartKueue, UID: "cq-1"}) {
		t.Errorf("created %+v", s.Created)
	}
	// After setup, a follow-up check sees a fully satisfied cluster.
	if chk, err := checkKueue(nil, &State{Recipe: r}); err != nil || !chk.Done {
		t.Errorf("resume check %+v %v", chk, err)
	}
	// Pre-existing setup (e.g. from Environment) must not be attributed to this run.
	fk2 := &fakeKueue{st: setUpKueue("proj", false, false, false)}
	m := toolMeta("", kueue.ClusterQueueName, "cq-0", nil)
	fk2.cq, fk2.rec.ClusterQueueUID = &m, "cq-0"
	fk2.install(t)
	s2 := &State{Recipe: validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    kueue: {enabled: true, rayCluster: true}\n"), RunID: "run-2"}
	if res, err := runKueueStep(testClient(), s2, func(types.UpdateStepEvent) {}); err != nil || !res.Success || len(s2.Created) != 0 || len(fk2.reqs) != 1 {
		t.Errorf("existing setup: %+v %v created %+v reqs %+v", res, err, s2.Created, fk2.reqs)
	}
}

// Ownership exposes distributed-workloads teardown only when the ClusterQueue
// ties to this run (annotation, stored UID, tool record align), and—when
// dashboard opt-in is active—only if that opt-in belongs to the same run
// (teardown reverts it with the other owned parts).
func TestKueueOwnership_OffersOnlyWhatTheRunCreated(t *testing.T) {
	run := &types.RecipeRun{ID: "run-1", CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: "cq-1"}}}
	cases := []struct {
		name    string
		edit    func(fk *fakeKueue)
		offered bool
	}{
		{"created by the run, no opt-in", nil, true},
		{"created by the run, the run's opt-in", func(fk *fakeKueue) {
			p := toolMeta("apps", kueue.ProfileName, "prof-1", map[string]string{RecipeRunAnnotation: "run-1"})
			fk.profile, fk.rec.OptIn = &p, &kueue.OptIn{DisableKueueSet: true, ProfileUID: "prof-1", ProfileNamespace: "apps"}
		}, true},
		{"an opt-in made outside the run", func(fk *fakeKueue) {
			p := toolMeta("apps", kueue.ProfileName, "prof-1", nil)
			fk.profile, fk.rec.OptIn = &p, &kueue.OptIn{DisableKueueSet: true, ProfileUID: "prof-1", ProfileNamespace: "apps"}
		}, false},
		{"a forged claim: no run annotation", func(fk *fakeKueue) { fk.cq.Annotations = nil }, false},
		{"another run's annotation", func(fk *fakeKueue) { fk.cq.Annotations = map[string]string{RecipeRunAnnotation: "run-9"} }, false},
		{"the ClusterQueue made again (another UID)", func(fk *fakeKueue) { fk.cq.UID, fk.rec.ClusterQueueUID = "cq-2", "cq-2" }, false},
		{"no longer in the tool's record", func(fk *fakeKueue) { fk.rec.ClusterQueueUID = "" }, false},
		{"the record unreadable", func(fk *fakeKueue) { fk.readErr = errors.New("boom") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cq := toolMeta("", kueue.ClusterQueueName, "cq-1", map[string]string{RecipeRunAnnotation: "run-1"})
			fk := &fakeKueue{cq: &cq, rec: kueue.Record{ClusterQueueUID: "cq-1"}}
			if tc.edit != nil {
				tc.edit(fk)
			}
			fk.install(t)
			created, req := Ownership(nil, testClient(), run)
			if got := req != nil && req.Kueue; got != tc.offered || created[StepKueue] != tc.offered {
				t.Fatalf("req %+v created %v", req, created)
			}
			if tc.offered && (req.KueueUID != "cq-1" || req.RecipeRunID != "run-1") {
				t.Errorf("the request does not name what was reviewed: %+v", req)
			}
		})
	}
	if _, req := Ownership(nil, nil, run); req != nil {
		t.Errorf("offered without the user's own client: %+v", req)
	}
}

// kueueWarnings: warn if the project picks up extra features and about
// the cluster-wide dashboard opt-in; never that the dashboard marks the
// workbench (with the setting on it is created after Kueue).
func TestKueuePlan_Warnings(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    pipelines: {enabled: true}\n    workbench: {enabled: true}\n    kueue: {enabled: true, dashboardOptIn: true}\n  dashboardDev: {deploy: main}\n")
	w := kueueWarnings(r, kueueLive{})
	if len(w) != 2 || w[0].Kind != types.RecipeWarningAdvice || !strings.Contains(w[0].Message, "Project proj also gets a pipeline server and a workbench") ||
		w[1].Kind != types.RecipeWarningCluster || !strings.Contains(w[1].Message, "every new project on this cluster") {
		t.Fatalf("warnings %q", w)
	}
	if w := kueueWarnings(r, kueueLive{OptInOn: true}); len(w) != 1 || w[0].Kind != types.RecipeWarningAdvice {
		t.Errorf("opt-in already on: %+v", w)
	}
	if w := kueueWarnings(validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {kueue: {enabled: true}}\n"), kueueLive{}); len(w) != 0 {
		t.Errorf("a project of its own, opt-in off: %q", w)
	}
	// The Kueue step's result has no note on the workbench either.
	fk := &fakeKueue{}
	fk.install(t)
	res, err := runKueueStep(testClient(), &State{Recipe: r, RunID: "run-1"}, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || strings.Contains(res.Message, "bypasses") {
		t.Fatalf("step result %+v %v", res, err)
	}
}

// KueueFirst follows the dashboard's rule, not the opt-in alone: Kueue in
// the recipe and the dashboard's Kueue area on when the workloads are
// created (the setting on now, or turned on by the opt-in).
func TestKueueFirst_FromTheDashboardRule(t *testing.T) {
	recipe := func(kueue string) types.EnvironmentRecipe {
		return validRecipe(t, "  project: {mode: create, name: recipe-dev}\n  features:\n    models: {enabled: true}\n    workbench: {enabled: true}\n"+kueue)
	}
	for _, tc := range []struct {
		name      string
		r         types.EnvironmentRecipe
		settingOn bool
		want      bool
	}{
		{"opt-in, setting off", recipe("    kueue: {enabled: true, dashboardOptIn: true}\n"), false, true},
		{"opt-in, setting on", recipe("    kueue: {enabled: true, dashboardOptIn: true}\n"), true, true},
		{"no opt-in, setting on (an admin's)", recipe("    kueue: {enabled: true}\n"), true, true},
		{"no opt-in, setting off", recipe("    kueue: {enabled: true}\n"), false, false},
		{"no Kueue, setting on", recipe(""), true, false},
	} {
		if got := KueueFirst(tc.r, tc.settingOn); got != tc.want {
			t.Errorf("%s: KueueFirst %v, want %v", tc.name, got, tc.want)
		}
	}
}

// With KueueFirst the run is project -> what the features need ->
// kueue-prereq -> kueue -> {workbench, model, ...}: every lane comes after
// the Kueue step, and the list shows them in that order; without it the
// order is as before (Kueue after every lane).
func TestSteps_KueueFirstOrder(t *testing.T) {
	r := validRecipe(t, "  project: {mode: create, name: recipe-dev}\n  features:\n    pipelines: {enabled: true}\n    models: {enabled: true}\n    workbench: {enabled: true}\n    kueue: {enabled: true, dashboardOptIn: true}\n  dashboardDev: {deploy: main}\n")
	ids := func(steps []Step) string {
		var out []string
		for _, s := range steps {
			out = append(out, s.ID)
		}
		return strings.Join(out, " ")
	}
	first := Steps(r, true)
	if got := ids(first); got != "rhoai dsc project prerequisites kueue-prereq kueue s3 pipeline-server models-prereq model workbench dashboard-dev" {
		t.Errorf("kueue first: %s", got)
	}
	byID := map[string]Step{}
	for _, s := range first {
		byID[s.ID] = s
	}
	for id, after := range map[string]string{StepKueuePrereq: StepPrerequisites, StepKueue: StepKueuePrereq, StepS3: StepKueue, StepModelsPrereq: StepKueue} {
		if got := byID[id].After; len(got) != 1 || got[0] != after {
			t.Errorf("%s comes after %v, want %s", id, got, after)
		}
	}
	for _, id := range []string{StepWorkbench, StepModel, StepPipelineServer} {
		if !ancestors(first, id)[StepKueue] {
			t.Errorf("%s may start before distributed workloads", id)
		}
	}
	if got := byID[StepDashboardDev].After; len(got) != 2 || !slices.Contains(got, StepWorkbench) || !slices.Contains(got, StepModel) {
		t.Errorf("dashboard-dev after %v", got)
	}
	if got := ids(Steps(r, false)); got != "rhoai dsc project prerequisites s3 pipeline-server models-prereq model workbench kueue-prereq kueue dashboard-dev" {
		t.Errorf("unchanged order: %s", got)
	}
}

// Validate enforces Kueue schema rules: project required, samples/image/opt-in
// require enabled Kueue, and rayImage uses the same validation as the tab.
func TestValidate_KueueFields(t *testing.T) {
	cases := []struct{ spec, field, want string }{
		{"  features: {kueue: {enabled: true}}\n", "spec.features.kueue.enabled", "Distributed workloads needs a project"},
		{"  features: {kueue: {dashboardOptIn: true}}\n", "spec.features.kueue.enabled", "need distributed workloads"},
		{"  project: {mode: existing, name: p}\n  features: {kueue: {enabled: true, rayImage: quay.io/x/ray:2}}\n", "spec.features.kueue.rayImage", "set rayCluster or rayJob"},
		{"  project: {mode: existing, name: p}\n  features: {kueue: {enabled: true, rayJob: true, rayImage: \"not an image!\"}}\n", "spec.features.kueue.rayImage", "rayImage must be an image reference"},
	}
	for _, tc := range cases {
		_, problems := Validate([]byte(source(latest + tc.spec)))
		if len(problems) != 1 || problems[0].Field != tc.field || !strings.Contains(problems[0].Message, tc.want) {
			t.Errorf("%q: %+v", tc.spec, problems)
		}
	}
	if _, problems := Validate([]byte(source(latest + kueueRecipe))); len(problems) != 0 {
		t.Errorf("valid: %+v", problems)
	}
	if _, problems := Validate([]byte(source(latest + "  features: {kueue: {enabled: false, unknown: 1}}\n"))); len(problems) != 1 || !strings.Contains(problems[0].Message, "field unknown not found") {
		t.Errorf("unknown field: %+v", problems)
	}
}

// A queue or an asked-for sample an earlier version made with other sizes
// is not counted as set up: the check says so (Set up would keep it).
func TestKueueOutdated(t *testing.T) {
	st := &types.KueueStatus{
		Objects: []types.KueueObject{{Kind: "ClusterQueue", Name: "nightly-kueue", Present: true, ManagedByTool: true}},
		Samples: []types.KueueSample{{Kind: kueue.SampleRayCluster, Name: "nightly-raycluster", Present: true, ManagedByTool: true, Outdated: "old sizes"}},
	}
	if why := kueueOutdated(st, types.RecipeKueue{Enabled: true}, "proj"); why != "" {
		t.Errorf("a sample not asked for: %q", why)
	}
	if why := kueueOutdated(st, types.RecipeKueue{Enabled: true, RayCluster: true}, "proj"); !strings.Contains(why, "An earlier version of this tool made the sample nightly-raycluster (old sizes) in proj") || !strings.Contains(why, "Clean up on the Distributed workloads tab, then Resume") {
		t.Errorf("outdated sample: %q", why)
	}
	st.Samples[0].Outdated, st.Objects[0].Outdated = "", "its quota is 6 CPUs and 11Gi of memory"
	if why := kueueOutdated(st, types.RecipeKueue{Enabled: true}, "proj"); !strings.Contains(why, "ClusterQueue nightly-kueue (its quota is 6 CPUs and 11Gi of memory)") {
		t.Errorf("outdated queue: %q", why)
	}
}
