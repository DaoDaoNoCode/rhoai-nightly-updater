package recipe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// allFeatures describes an existing project whose recipe turns every feature on.
const allFeatures = "  project: {mode: existing, name: proj}\n" +
	"  features:\n    pipelines: {enabled: true}\n    mlflow: {enabled: true, samples: true}\n" +
	"    models: {enabled: true, model: qwen3-1-7b}\n    playground: {enabled: true}\n" +
	"    maas: {enabled: true, database: cnpg, publish: true}\n    modelRegistry: {enabled: true, sample: true}\n" +
	"    workbench: {enabled: true}\n  dashboardDev: {deploy: pr, pr: 77}\n"

func validRecipe(t *testing.T, spec string) types.EnvironmentRecipe {
	t.Helper()
	r, problems := Validate([]byte(source(latest + spec)))
	if len(problems) > 0 {
		t.Fatalf("problems %+v", problems)
	}
	return r
}

// fakeCluster supplies state that post-MLflow steps observe: a cluster brought
// to a particular point and which setup actions already executed.
type fakeCluster struct {
	samplesAdded, maasReady, maasOurs bool
	prereqReady                       map[string]bool
	exposures                         *types.ModelExposures
	model                             *types.ModelState
	published                         bool
	playground                        *types.PlaygroundState
	registry                          types.ModelRegistryStatus
	registryRec                       modelregistry.Record
	workbench                         *types.WorkbenchState
	session                           *types.DashboardOverride
	maasRec                           serving.MaaSRecord
	// readErr forces registryRecord lookups to fail when set.
	readErr error

	prereqExposures []string
	ran             []string
	modelsReq       types.ModelsRequest
	maasReqs        []types.MaaSRequest
}

func ok(msg string) (*types.OperationResponse, error) {
	return &types.OperationResponse{Success: true, Message: msg}, nil
}

// install swaps package step readers and runners for fc's fakes.
func (fc *fakeCluster) install(t *testing.T) {
	t.Helper()
	p0, p1, p2, p3, p4, p5, p6 := prereqStatus, samplesStatus, runSamples, maasStatus, maasRecord, runMaaS, modelExposures
	p7, p8, p9, p10, p11, p12, p13 := readModel, runModels, maasPublished, playgroundStatus, runPlayground, registryStatus, registryRecord
	p14, p15, p16, p17, p18, p19, p20 := runRegistry, workbenchStatus, runWorkbench, dashboardSession, deployMain, deployPR, waitDashboard
	t.Cleanup(func() {
		prereqStatus, samplesStatus, runSamples, maasStatus, maasRecord, runMaaS, modelExposures = p0, p1, p2, p3, p4, p5, p6
		readModel, runModels, maasPublished, playgroundStatus, runPlayground, registryStatus, registryRecord = p7, p8, p9, p10, p11, p12, p13
		runRegistry, workbenchStatus, runWorkbench, dashboardSession, deployMain, deployPR, waitDashboard = p14, p15, p16, p17, p18, p19, p20
	})
	if fc.prereqReady == nil {
		fc.prereqReady = map[string]bool{}
	}
	prereqStatus = func(_ *kube.Client, exposure string) (*types.PrerequisitesStatus, error) {
		fc.prereqExposures = append(fc.prereqExposures, exposure)
		st := &types.PrerequisitesStatus{Features: map[string]types.FeaturePrerequisites{}}
		for f, ready := range fc.prereqReady {
			st.Features[f] = types.FeaturePrerequisites{Ready: ready}
		}
		return st, nil
	}
	samplesStatus = func(_, _ *kube.Client, project string) (*types.MLflowSamplesStatus, error) {
		st := &types.MLflowSamplesStatus{Project: project, Available: true}
		for _, id := range []string{"genai-demo", "training"} {
			s := types.SampleStatus{ID: id, Name: id, State: "absent", Actions: []string{"add"}}
			if fc.samplesAdded {
				s.State, s.Actions = "present", []string{"remove"}
			}
			st.Samples = append(st.Samples, s)
		}
		return st, nil
	}
	runSamples = func(_ *kube.Client, req types.MLflowSamplesRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "samples:"+strings.Join(req.Samples, ","))
		fc.samplesAdded = true
		return ok("added")
	}
	maasStatus = func(*kube.Client) (*types.MaaSStatus, error) {
		return &types.MaaSStatus{Platform: types.ResourceState{Ready: fc.maasReady, Deployed: fc.maasReady, ManagedByTool: fc.maasOurs},
			Database: &types.MaaSDatabase{Needed: !fc.maasReady}}, nil
	}
	maasRecord = func(*kube.Client) (serving.MaaSRecord, error) { return fc.maasRec, nil }
	runMaaS = func(_ *kube.Client, req types.MaaSRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.maasReqs = append(fc.maasReqs, req)
		fc.ran = append(fc.ran, "maas:"+req.Action)
		if req.Action == "publish" {
			fc.published = true
		} else {
			fc.maasReady, fc.maasOurs = true, true
		}
		return ok(req.Action)
	}
	modelExposures = func(_ *kube.Client, project, model string) (*types.ModelExposures, string, string) {
		if fc.exposures != nil {
			return fc.exposures, "", ""
		}
		return &types.ModelExposures{Project: project, Model: model, Default: "default-gateway", Options: []types.ModelExposureOption{{Key: "default-gateway", Exposure: "default-gateway", Available: true}}}, "", ""
	}
	readModel = func(*kube.Client, string, string) (types.ModelState, bool, error) {
		if fc.model == nil {
			return types.ModelState{}, false, nil
		}
		return *fc.model, true, nil
	}
	runModels = func(_ *kube.Client, req types.ModelsRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.modelsReq = req
		fc.ran = append(fc.ran, "model")
		fc.model = &types.ModelState{ResourceState: types.ResourceState{Deployed: true, Ready: true, ManagedByTool: true}, MaaSPublished: req.Exposure == serving.ExposureMaaS}
		fc.published = fc.published || req.Exposure == serving.ExposureMaaS
		return ok("model")
	}
	maasPublished = func(*kube.Client, string, string) (bool, error) { return fc.published, nil }
	playgroundStatus = func(_, _ *kube.Client, project string) (*types.PlaygroundStatus, error) {
		st := &types.PlaygroundStatus{Project: project}
		if fc.playground != nil {
			st.Items = []types.PlaygroundState{*fc.playground}
		}
		return st, nil
	}
	runPlayground = func(_ *kube.Client, req types.PlaygroundRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "playground")
		fc.playground = &types.PlaygroundState{ResourceState: types.ResourceState{Namespace: req.Project, Deployed: true, Ready: true, ManagedByTool: true}}
		return ok("playground")
	}
	registryStatus = func(_, _ *kube.Client) (*types.ModelRegistryStatus, error) {
		st := fc.registry
		if st.State == "" {
			st.State = "absent"
		}
		st.Name, st.Namespace = modelregistry.Name, "rhoai-model-registries"
		return &st, nil
	}
	registryRecord = func(*kube.Client) (modelregistry.Record, error) {
		if fc.readErr != nil {
			return nil, fc.readErr
		}
		return fc.registryRec, nil
	}
	runRegistry = func(_ *kube.Client, req types.ModelRegistryRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "registry")
		if fc.registry.State == "absent" || fc.registry.State == "" {
			fc.registryRec = modelregistry.Record{"rhoai-model-registries/" + modelregistry.Name: {UID: "reg-1"}}
		}
		fc.registry = types.ModelRegistryStatus{State: "ready", ManagedByTool: true, Sample: &types.ModelRegistrySample{Complete: req.Samples}, Address: "https://x"}
		return ok("registry")
	}
	workbenchStatus = func(_, _ *kube.Client, project string) (*types.WorkbenchStatus, error) {
		st := &types.WorkbenchStatus{Project: project}
		if fc.workbench != nil {
			st.Items = []types.WorkbenchState{*fc.workbench}
		}
		return st, nil
	}
	runWorkbench = func(_ *kube.Client, req types.WorkbenchRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "workbench")
		fc.workbench = &types.WorkbenchState{ResourceState: types.ResourceState{Namespace: req.Project, Deployed: true, ManagedByTool: true}, State: "starting"}
		return ok("workbench")
	}
	dashboardSession = func(*kube.Client) (*types.DashboardOverride, error) { return fc.session, nil }
	deployMain = func(*kube.Client, string) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "dashboard:main")
		return ok("Deployed main.")
	}
	deployPR = func(_ *kube.Client, pr int, flavor string) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "dashboard:pr")
		fc.session = &types.DashboardOverride{Active: true, SessionRecorded: true, Mode: "pr", PRNumber: pr, Flavor: flavor}
		return ok("Deployed PR.")
	}
	waitDashboard = func(*kube.Client) ([]types.DashboardDevImage, error) {
		fc.ran = append(fc.ran, "dashboard:wait")
		return nil, nil
	}
}

func testClient() *kube.Client { return kube.NewClientWithContext(context.Background(), "t") }

// stepsAfterMLflow lists recipe steps that come after MLflow.
func stepsAfterMLflow(r types.EnvironmentRecipe) []Step { return featureSteps(r) }

// readyCluster pretends the cluster already has every resource the recipe would provision.
func readyCluster() *fakeCluster {
	return &fakeCluster{
		samplesAdded: true, maasReady: true,
		prereqReady: map[string]bool{"models": true, "playground": true, "model-registry": true},
		model:       &types.ModelState{ResourceState: types.ResourceState{Deployed: true, Ready: true, ManagedByTool: true}},
		published:   true,
		playground:  &types.PlaygroundState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, Ready: true, ManagedByTool: true}},
		registry:    types.ModelRegistryStatus{State: "ready", ManagedByTool: true, Sample: &types.ModelRegistrySample{Complete: true}, Address: "https://x"},
		workbench:   &types.WorkbenchState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, ManagedByTool: true}, State: "stopped"},
		session:     &types.DashboardOverride{Active: true, SessionRecorded: true, Mode: "pr", PRNumber: 77, Flavor: "rhoai"},
	}
}

// For each new step, Check inspects live cluster state: provisioned pieces are
// skipped (Resume's already-done path), and ordering follows runner dependencies.
func TestFeatureSteps_CheckSkipsWhatIsSetUp(t *testing.T) {
	r := validRecipe(t, allFeatures)
	fc := readyCluster()
	fc.install(t)
	var ids []string
	for _, step := range stepsAfterMLflow(r) {
		ids = append(ids, step.ID)
		chk, err := step.Check(nil, &State{Recipe: r})
		if err != nil || !chk.Done || chk.Reason == "" {
			t.Errorf("%s: %+v %v", step.ID, chk, err)
		}
	}
	want := "mlflow-samples maas models-prereq model maas-publish playground-prereq playground model-registry-prereq model-registry workbench dashboard-dev"
	if got := strings.Join(ids, " "); got != want {
		t.Errorf("order %s\nwant  %s", got, want)
	}
	if len(fc.ran) != 0 {
		t.Errorf("a check set something up: %v", fc.ran)
	}
}

// When a piece is absent the step may run; when it exists but was not created
// by this tool the step blocks, and cluster state stays unchanged either way.
func TestFeatureSteps_CheckRunsOrBlocks(t *testing.T) {
	r := validRecipe(t, allFeatures)
	step := func(id string) Step {
		for _, s := range stepsAfterMLflow(r) {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no step %s", id)
		return Step{}
	}
	cases := []struct {
		name, step string
		edit       func(fc *fakeCluster)
		blocked    string // empty means the step runs
	}{
		{"samples missing", StepMLflowSamples, func(fc *fakeCluster) { fc.samplesAdded = false }, ""},
		{"MaaS not ready", StepMaaS, func(fc *fakeCluster) { fc.maasReady = false }, ""},
		{"Models prerequisites", StepModelsPrereq, func(fc *fakeCluster) { fc.prereqReady["models"] = false }, ""},
		{"no model", StepModel, func(fc *fakeCluster) { fc.model = nil }, ""},
		{"a model still starting", StepModel, func(fc *fakeCluster) { fc.model.Ready = false }, ""},
		{"someone else's model", StepModel, func(fc *fakeCluster) { fc.model.ManagedByTool = false }, "this tool did not create"},
		{"not published", StepMaaSPublish, func(fc *fakeCluster) { fc.published = false }, ""},
		{"someone else's playground", StepPlayground, func(fc *fakeCluster) { fc.playground.ManagedByTool = false }, "did not set up"},
		{"no playground", StepPlayground, func(fc *fakeCluster) { fc.playground = nil }, ""},
		{"the sample missing", StepModelRegistry, func(fc *fakeCluster) { fc.registry.Sample.Complete = false }, ""},
		{"a foreign registry", StepModelRegistry, func(fc *fakeCluster) { fc.registry = types.ModelRegistryStatus{State: "ready", Foreign: true} }, "did not create"},
		{"no workbench", StepWorkbench, func(fc *fakeCluster) { fc.workbench = nil }, ""},
		{"someone else's workbench", StepWorkbench, func(fc *fakeCluster) { fc.workbench.ManagedByTool = false }, "did not create"},
		{"another PR deployed", StepDashboardDev, func(fc *fakeCluster) { fc.session.PRNumber = 78 }, ""},
		{"no session", StepDashboardDev, func(fc *fakeCluster) { fc.session = nil }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := readyCluster()
			tc.edit(fc)
			fc.install(t)
			chk, err := step(tc.step).Check(nil, &State{Recipe: r})
			switch {
			case err != nil || chk.Done:
				t.Fatalf("%+v %v", chk, err)
			case tc.blocked == "" && chk.Blocked != "":
				t.Fatalf("blocked: %s", chk.Blocked)
			case tc.blocked != "" && !strings.Contains(chk.Blocked, tc.blocked):
				t.Fatalf("blocked %q, want %q", chk.Blocked, tc.blocked)
			}
			if len(fc.ran) != 0 {
				t.Errorf("the check set something up: %v", fc.ran)
			}
		})
	}
}

// Resuming after MaaS hits Models prerequisites first: samples and the MaaS
// platform are already on the cluster and get skipped; downstream steps fill in
// the rest in order, each issuing the same request its UI page would send.
func TestFeatureSteps_ResumeAfterMaaSContinuesWithModels(t *testing.T) {
	r := validRecipe(t, allFeatures)
	// Exercise the dashboard path while Dashboard Dev still lacks a session.
	fc := &fakeCluster{samplesAdded: true, maasReady: true, session: &types.DashboardOverride{}}
	fc.install(t)
	c := testClient()
	s := &State{Recipe: r}
	var outcome []string
	for _, step := range stepsAfterMLflow(r) {
		chk, err := step.Check(c, s)
		if err != nil || chk.Blocked != "" {
			t.Fatalf("%s: %+v %v", step.ID, chk, err)
		}
		if chk.Done {
			outcome = append(outcome, step.ID+"=skipped")
			continue
		}
		if step.ID == StepModelsPrereq || step.ID == StepPlaygroundPrereq || step.ID == StepModelRegistryPrereq {
			// RunPrerequisites belongs to the Environment page (covered there);
			// afterward the fake cluster reports those prerequisites as ready.
			fc.prereqReady[strings.TrimSuffix(step.ID, "-prereq")] = true
			if step.ID == StepModelsPrereq {
				if problem, _ := pinExposure(c, s); problem != "" {
					t.Fatal(problem)
				}
			}
			outcome = append(outcome, step.ID+"=ran")
			continue
		}
		res, err := step.Run(c, s, func(types.UpdateStepEvent) {})
		if err != nil || !res.Success {
			t.Fatalf("%s: %+v %v", step.ID, res, err)
		}
		outcome = append(outcome, step.ID+"=ran")
	}
	want := "mlflow-samples=skipped maas=skipped models-prereq=ran model=ran maas-publish=ran playground-prereq=ran playground=ran model-registry-prereq=ran model-registry=ran workbench=ran dashboard-dev=ran"
	if got := strings.Join(outcome, " "); got != want {
		t.Fatalf("resume\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(fc.ran, " "); got != "model maas:publish playground registry workbench dashboard:pr dashboard:wait" {
		t.Errorf("setups %s", got)
	}
	if fc.modelsReq.Project != "proj" || fc.modelsReq.Model != "qwen3-1-7b" || fc.modelsReq.Exposure != "default-gateway" || s.Exposure != "default-gateway" {
		t.Errorf("model request %+v, state exposure %q", fc.modelsReq, s.Exposure)
	}
	// MaaS was pre-existing so Created omits it; the registry added in this run
	// is attributed via the UID stored on the tool record.
	if len(s.Created) != 1 || s.Created[0] != (types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: "reg-1"}) {
		t.Errorf("created %+v", s.Created)
	}
}

// With no exposure in the recipe, the project default is resolved for Models
// prerequisites and locked in for the model step; missing recommendations yield
// a blocking reason; an explicit recipe exposure is honored unchanged.
func TestModelsExposure_ResolvedPinnedOrBlocked(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true}}\n")
	fc := &fakeCluster{exposures: &types.ModelExposures{Default: "gateway:ns1/gw", Options: []types.ModelExposureOption{
		{Key: "default-gateway", Exposure: "default-gateway"}, {Key: "gateway:ns1/gw", Exposure: "gateway", GatewayRef: "ns1/gw"}}}}
	fc.install(t)
	s := &State{Recipe: r}
	if chk, err := checkModelsPrereq(nil, s); err != nil || chk.Done || chk.Blocked != "" || strings.Join(fc.prereqExposures, ",") != "gateway" {
		t.Fatalf("check %+v %v, exposures %v", chk, err, fc.prereqExposures)
	}
	if s.Exposure != "" {
		t.Fatal("a check pinned the exposure")
	}
	if res, err := runModel(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
	if fc.modelsReq.Exposure != "gateway" || fc.modelsReq.GatewayRef != "ns1/gw" || s.GatewayRef != "ns1/gw" {
		t.Errorf("request %+v", fc.modelsReq)
	}

	fc.exposures = &types.ModelExposures{Options: []types.ModelExposureOption{{Key: "default-gateway", Reason: "KServe has no default gateway."}}}
	chk, err := checkModelsPrereq(nil, &State{Recipe: r})
	if err != nil || !strings.Contains(chk.Blocked, "KServe has no default gateway.") || !strings.Contains(chk.Blocked, "spec.features.models.exposure") {
		t.Errorf("no recommendation: %+v %v", chk, err)
	}

	own := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, exposure: internal}}\n")
	if _, err := checkModelsPrereq(nil, &State{Recipe: own}); err != nil || fc.prereqExposures[len(fc.prereqExposures)-1] != "internal" {
		t.Errorf("own exposure: %v %v", err, fc.prereqExposures)
	}
	if !ExposureKept("maas", "") || !ExposureKept("gateway", "ns1/gw") || ExposureKept("gateway", "") || ExposureKept("public", "") || ExposureKept("", "") {
		t.Error("ExposureKept")
	}
}

// Under MaaS exposure the model step stays incomplete until publication lands
// (the Models tab performs publish as part of setup).
func TestModelStep_MaaSExposureNeedsThePublication(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, exposure: maas}, maas: {enabled: true}}\n")
	fc := readyCluster()
	fc.model.MaaSPublished = false
	fc.install(t)
	if chk, err := checkModel(nil, &State{Recipe: r}); err != nil || chk.Done {
		t.Fatalf("%+v %v", chk, err)
	}
	fc.model.MaaSPublished = true
	if chk, err := checkModel(nil, &State{Recipe: r}); err != nil || !chk.Done {
		t.Fatalf("%+v %v", chk, err)
	}
}

// MaaS records the platform in Created only when this tool newly owns it; the
// recipe's database kind is passed through solely when Set up must provision one.
func TestMaaSStep_RecordsWhatItCreated(t *testing.T) {
	r := validRecipe(t, "  features: {maas: {enabled: true, database: cnpg}}\n")
	t.Run("created", func(t *testing.T) {
		fc := &fakeCluster{maasRec: serving.MaaSRecord{POCCredsUID: "creds-1"}}
		fc.install(t)
		s := &State{Recipe: r}
		if res, err := runMaaSStep(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || !res.Success {
			t.Fatalf("%+v %v", res, err)
		}
		if len(fc.maasReqs) != 1 || fc.maasReqs[0].Database == nil || fc.maasReqs[0].Database.Kind != "cnpg" {
			t.Errorf("requests %+v", fc.maasReqs)
		}
		if len(s.Created) != 1 || s.Created[0] != (types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "creds-1"}) {
			t.Errorf("created %+v", s.Created)
		}
	})
	t.Run("this tool's already", func(t *testing.T) {
		fc := &fakeCluster{maasOurs: true}
		fc.install(t)
		s := &State{Recipe: r}
		if _, err := runMaaSStep(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || len(s.Created) != 0 {
			t.Errorf("created %+v %v", s.Created, err)
		}
	})
}

// A registry born in this run's setup lands in Created via the tool record UID;
// an already-present registry does not; unreadable records add nothing and the
// step surfaces that in its message.
func TestModelRegistryStep_RecordsWhatItCreated(t *testing.T) {
	r := validRecipe(t, "  features: {modelRegistry: {enabled: true}}\n")
	fc := &fakeCluster{}
	fc.install(t)
	s := &State{Recipe: r}
	if res, err := runModelRegistry(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || !res.Success || len(s.Created) != 1 || s.Created[0].UID != "reg-1" {
		t.Fatalf("%+v %v created %+v", res, err, s.Created)
	}
	again := &State{Recipe: r}
	if _, err := runModelRegistry(testClient(), again, func(types.UpdateStepEvent) {}); err != nil || len(again.Created) != 0 {
		t.Errorf("a registry that was there: %+v", again.Created)
	}
	fc2 := &fakeCluster{readErr: errors.New("boom")}
	fc2.install(t)
	s3 := &State{Recipe: r}
	var events []string
	res, err := runModelRegistry(testClient(), s3, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || !res.Success || len(s3.Created) != 0 || !strings.Contains(res.Message, "does not offer it") || len(events) != 1 {
		t.Errorf("unreadable record: %+v %v %v", res, s3.Created, events)
	}
}

// Clean up surfaces a cluster-wide part only when the run logged creating it
// and it still matches that snapshot; resources the run never created stay off
// the list even if this tool currently manages them.
func TestOwnership_OffersOnlyWhatTheRunCreated(t *testing.T) {
	run := func(parts ...types.RecipeCreatedPart) *types.RecipeRun {
		return &types.RecipeRun{ID: "r", CreatedParts: parts}
	}
	maas := types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "creds-1"}
	reg := types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: "reg-1"}
	cases := []struct {
		name          string
		run           *types.RecipeRun
		edit          func(fc *fakeCluster)
		maas, registy bool
	}{
		{"both, as created", run(maas, reg), nil, true, true},
		{"nothing recorded, though both are this tool's", run(), nil, false, false},
		{"MaaS set up again since (another database Secret)", run(maas, reg), func(fc *fakeCluster) { fc.maasRec.POCCredsUID = "creds-2" }, false, true},
		{"MaaS no longer this tool's", run(maas), func(fc *fakeCluster) { fc.maasOurs = false }, false, false},
		{"the registry made again since (another UID)", run(reg), func(fc *fakeCluster) {
			fc.registryRec = modelregistry.Record{"ns/" + modelregistry.Name: {UID: "reg-2"}}
		}, false, false},
		{"the registry record unreadable", run(reg), func(fc *fakeCluster) { fc.readErr = errors.New("boom") }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCluster{maasOurs: true, maasRec: serving.MaaSRecord{POCCredsUID: "creds-1"},
				registryRec: modelregistry.Record{"ns/" + modelregistry.Name: {UID: "reg-1"}}}
			if tc.edit != nil {
				tc.edit(fc)
			}
			fc.install(t)
			created, req := Ownership(nil, tc.run)
			gotMaaS, gotReg := req != nil && req.MaaSPlatform, req != nil && req.ModelRegistry
			if gotMaaS != tc.maas || gotReg != tc.registy || created[StepMaaS] != tc.maas || created[StepModelRegistry] != tc.registy {
				t.Errorf("req %+v created %v", req, created)
			}
			if !tc.maas && !tc.registy && req != nil {
				t.Errorf("a clean-up with nothing in it: %+v", req)
			}
		})
	}
}

// Dashboard Dev applies the recipe's chosen build then waits on rollout; the
// legacy path without the dashboard module has nothing to wait for.
func TestDashboardDevStep_DeploysAndWaits(t *testing.T) {
	r := validRecipe(t, "  dashboardDev: {deploy: main}\n")
	fc := &fakeCluster{session: &types.DashboardOverride{}}
	fc.install(t)
	res, err := runDashboardDev(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || strings.Join(fc.ran, " ") != "dashboard:main dashboard:wait" || !strings.Contains(res.Message, "the dashboard is ready") {
		t.Fatalf("%+v %v %v", res, err, fc.ran)
	}
	legacy := &fakeCluster{}
	legacy.install(t)
	pr := validRecipe(t, "  dashboardDev: {deploy: pr, pr: 5, flavor: odh}\n")
	if res, err := runDashboardDev(testClient(), &State{Recipe: pr}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || strings.Join(legacy.ran, " ") != "dashboard:pr" {
		t.Fatalf("legacy %+v %v %v", res, err, legacy.ran)
	}
	if chk, _ := checkDashboardDev(nil, &State{Recipe: pr}); !chk.Done {
		t.Errorf("the deployed PR is not done: %+v", chk)
	}
}
