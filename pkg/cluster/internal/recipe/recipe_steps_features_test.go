package recipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// allFeatures holds recipe YAML where each feature is enabled against an existing project.
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

// toolMeta builds ObjectMeta for cluster objects the tool applied: managed-by
// label, field manager, UID, and supplied annotations.
func toolMeta(ns, name, uid string, annotations map[string]string) kube.ObjectMeta {
	return kube.ObjectMeta{Namespace: ns, Name: name, UID: uid, Annotations: annotations,
		Labels:        map[string]string{kube.ManagedByLabelKey: kube.ManagedByLabelValue},
		ManagedFields: []kube.ManagedFieldsEntry{{Manager: envkit.ToolFieldManager, Operation: "Apply"}}}
}

const registryNS = "rhoai-model-registries"

// fakeCluster simulates cluster state for post-MLflow steps: how far setup
// progressed and which setup calls fired. With stepPoll at zero, waits hinge
// on read counts—state flips after N polls, not on the clock.
type fakeCluster struct {
	samplesAdded, maasReady, maasOurs bool
	// sampleUnknown: at least one sample returned an unreadable state.
	sampleUnknown bool
	prereqReady   map[string]bool
	exposures     *types.ModelExposures
	model         *types.ModelState
	// modelReadyAfter: RunModels's model turns Ready starting at this read
	// index (0 means immediately; -1 means it never becomes Ready).
	modelReadyAfter, modelReads int
	// published: full MaaS publication exists (Published and Ready on the
	// status item); routedOnly: traffic goes via MaaS gateway without a
	// completed publish.
	published, routedOnly bool
	playground            *types.PlaygroundState
	// maasEntry is the model's Models as a Service entry for the
	// playground (nil: no MaaSModelRef names it); maasEntryErr fails its
	// read. playgroundReq is the last playground setup request.
	maasEntry     *types.PlaygroundModel
	maasEntryErr  error
	playgroundReq types.PlaygroundRequest
	registry      types.ModelRegistryStatus
	registryRec   modelregistry.Record
	regMeta       *kube.ObjectMeta
	registryAnn   map[string]string
	workbench     *types.WorkbenchState
	// workbenchGoneAfter: terminating workbench disappears after this many
	// status reads (-1 keeps it around forever).
	workbenchGoneAfter, workbenchReads int
	session                            *types.DashboardOverride
	dashReady                          bool
	maasRec                            serving.MaaSRecord
	dbMeta                             *kube.ObjectMeta
	// readErr forces model registry record reads to error.
	readErr error
	// legacy is the legacy flow's dashboard (no module operator); nil
	// otherwise. prMissing: the PR's images are not published.
	legacy      *types.DashboardState
	legacyReads int
	prMissing   bool
	// publishAcceptedAfter: a publish is accepted by MaaS only from this
	// read of its publication on (0: at once; -1: never).
	publishAcceptedAfter, publishReads int

	prereqExposures []string
	ran             []string
	modelsReq       types.ModelsRequest
	maasReqs        []types.MaaSRequest
}

func ok(msg string) (*types.OperationResponse, error) {
	return &types.OperationResponse{Success: true, Message: msg}, nil
}

// install swaps package-level step hooks for fakeCluster stubs and restores them on cleanup.
func (fc *fakeCluster) install(t *testing.T) {
	t.Helper()
	p0, p1, p2, p3, p4, p5, p6 := prereqStatus, samplesStatus, runSamples, maasStatus, maasRecord, runMaaS, modelExposures
	p7, p8, p9, p10, p11, p12, p13 := readModel, runModels, maasPublished, playgroundStatus, runPlayground, registryStatus, registryRecord
	p14, p15, p16, p17, p18, p19, p20 := runRegistry, workbenchStatus, runWorkbench, dashboardSession, deployMain, deployPR, waitDashboard
	p21, p22, p24, p25, p26, p27 := registryMeta, dbConfigMeta, dashboardReady, stepPoll, dashboardState, prPublished
	p28 := maasPlayground
	t.Cleanup(func() {
		prereqStatus, samplesStatus, runSamples, maasStatus, maasRecord, runMaaS, modelExposures = p0, p1, p2, p3, p4, p5, p6
		readModel, runModels, maasPublished, playgroundStatus, runPlayground, registryStatus, registryRecord = p7, p8, p9, p10, p11, p12, p13
		runRegistry, workbenchStatus, runWorkbench, dashboardSession, deployMain, deployPR, waitDashboard = p14, p15, p16, p17, p18, p19, p20
		registryMeta, dbConfigMeta, dashboardReady, stepPoll, dashboardState, prPublished = p21, p22, p24, p25, p26, p27
		maasPlayground = p28
	})
	stepPoll = 0
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
		if fc.sampleUnknown {
			st.Samples = append(st.Samples, types.SampleStatus{ID: "skill", Name: "Skill", State: "unknown", Message: "MLflow answered 503", Actions: []string{}})
		}
		return st, nil
	}
	runSamples = func(_ *kube.Client, req types.MLflowSamplesRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "samples:"+strings.Join(req.Samples, ","))
		fc.samplesAdded = true
		return ok("added")
	}
	maasStatus = func(*kube.Client) (*types.MaaSStatus, error) {
		st := &types.MaaSStatus{Platform: types.ResourceState{Ready: fc.maasReady, Deployed: fc.maasReady, ManagedByTool: fc.maasOurs},
			Database: &types.MaaSDatabase{Needed: !fc.maasReady}}
		if fc.published && fc.publishAcceptedAfter != 0 {
			fc.publishReads++
		}
		if fc.published || fc.routedOnly {
			accepted := fc.published && (fc.publishAcceptedAfter == 0 || (fc.publishAcceptedAfter > 0 && fc.publishReads >= fc.publishAcceptedAfter))
			it := types.MaaSModel{Published: accepted}
			it.Namespace, it.Name, it.Ready = "proj", "qwen3-1-7b", accepted
			st.Items = []types.MaaSModel{it}
		}
		return st, nil
	}
	maasRecord = func(*kube.Client) (serving.MaaSRecord, error) { return fc.maasRec, nil }
	dbConfigMeta = func(_ *kube.Client, ns string) (kube.ObjectMeta, bool, error) {
		if fc.dbMeta == nil || ns != fc.dbMeta.Namespace {
			return kube.ObjectMeta{}, false, nil
		}
		return *fc.dbMeta, true, nil
	}
	runMaaS = func(_ *kube.Client, req types.MaaSRequest, annotations map[string]string, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.maasReqs = append(fc.maasReqs, req)
		fc.ran = append(fc.ran, "maas:"+req.Action)
		if req.Action == "publish" {
			fc.published = true
			return ok("publish")
		}
		fc.maasReady, fc.maasOurs = true, true
		if fc.dbMeta == nil && req.Database != nil {
			// Setup path creates the database connection Secret.
			fc.maasRec = serving.MaaSRecord{Database: req.Database.Kind, DBNamespace: "models-as-a-service-infra"}
			m := toolMeta("models-as-a-service-infra", "maas-db-config", "secret-1", annotations)
			fc.dbMeta = &m
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
		fc.modelReads++
		m := *fc.model
		if fc.modelReadyAfter != 0 {
			m.Ready = fc.modelReadyAfter > 0 && fc.modelReads >= fc.modelReadyAfter
		}
		return m, true, nil
	}
	runModels = func(_ *kube.Client, req types.ModelsRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		if problem := serving.ValidateModelsRequest(req); problem != "" {
			return &types.OperationResponse{Success: false, Message: problem, ErrorCode: "validation"}, nil
		}
		fc.modelsReq = req
		fc.ran = append(fc.ran, "model")
		fc.model = &types.ModelState{ResourceState: types.ResourceState{Deployed: true, Ready: true, ManagedByTool: true}, MaaSPublished: req.Exposure == serving.ExposureMaaS}
		fc.published = fc.published || req.Exposure == serving.ExposureMaaS
		return ok("model")
	}
	maasPublished = func(*kube.Client, string, string) (bool, error) { return fc.published || fc.routedOnly, nil }
	playgroundStatus = func(_, _ *kube.Client, project string) (*types.PlaygroundStatus, error) {
		st := &types.PlaygroundStatus{Project: project}
		if fc.playground != nil {
			st.Items = []types.PlaygroundState{*fc.playground}
		}
		return st, nil
	}
	runPlayground = func(_ *kube.Client, req types.PlaygroundRequest, _ func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "playground")
		fc.playgroundReq = req
		fc.playground = &types.PlaygroundState{ResourceState: types.ResourceState{Namespace: req.Project, Deployed: true, Ready: true, ManagedByTool: true}, Models: req.Models}
		return ok("playground")
	}
	maasPlayground = func(_ *kube.Client, project, model string) (types.PlaygroundModel, bool, error) {
		if fc.maasEntryErr != nil {
			return types.PlaygroundModel{}, false, fc.maasEntryErr
		}
		if fc.maasEntry == nil || project != "proj" || model != "qwen3-1-7b" {
			return types.PlaygroundModel{}, false, nil
		}
		return *fc.maasEntry, true, nil
	}
	registryStatus = func(_, _ *kube.Client) (*types.ModelRegistryStatus, error) {
		st := fc.registry
		if st.State == "" {
			st.State = "absent"
		}
		st.Name, st.Namespace = modelregistry.Name, registryNS
		return &st, nil
	}
	registryRecord = func(*kube.Client) (modelregistry.Record, error) {
		if fc.readErr != nil {
			return nil, fc.readErr
		}
		return fc.registryRec, nil
	}
	registryMeta = func(_, _ *kube.Client) (kube.ObjectMeta, bool, error) {
		if fc.regMeta == nil {
			return kube.ObjectMeta{}, false, nil
		}
		return *fc.regMeta, true, nil
	}
	runRegistry = func(_ *kube.Client, req types.ModelRegistryRequest, annotations map[string]string, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "registry")
		fc.registryAnn = annotations
		if fc.registry.State == "absent" || fc.registry.State == "" {
			fc.registryRec = modelregistry.Record{registryNS + "/" + modelregistry.Name: {UID: "reg-1"}}
			m := toolMeta(registryNS, modelregistry.Name, "reg-1", annotations)
			fc.regMeta = &m
		}
		fc.registry = types.ModelRegistryStatus{State: "ready", ManagedByTool: true, Sample: &types.ModelRegistrySample{Complete: req.Samples}, Address: "https://x"}
		if req.Samples {
			emit(types.UpdateStepEvent{Step: "samples", Status: "success", Message: "Sample model mnist (version v1) is registered."})
		}
		return ok("Model registry nightly-registry is ready in " + registryNS + ".")
	}
	workbenchStatus = func(_, _ *kube.Client, project string) (*types.WorkbenchStatus, error) {
		st := &types.WorkbenchStatus{Project: project}
		if fc.workbench != nil {
			fc.workbenchReads++
			if fc.workbench.Terminating && fc.workbenchGoneAfter >= 0 && fc.workbenchReads > fc.workbenchGoneAfter {
				fc.workbench = nil
				return st, nil
			}
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
	dashboardReady = func(*kube.Client) (bool, string, error) { return fc.dashReady, "rhods-dashboard", nil }
	dashboardState = func(*kube.Client) (*types.DashboardState, error) {
		if fc.legacy == nil {
			return &types.DashboardState{}, nil
		}
		// A deployed PR's pods are ready from the second read on.
		if fc.legacy.IsCustomPR && !fc.legacy.PodReady {
			if fc.legacyReads++; fc.legacyReads >= 2 {
				fc.legacy.PodReady = true
			}
		}
		st := *fc.legacy
		return &st, nil
	}
	prPublished = func(*kube.Client, int, string) (bool, string, error) {
		if fc.prMissing {
			return false, "no pr-77 in quay.io/opendatahub/odh-dashboard", nil
		}
		return true, "", nil
	}
	deployMain = func(*kube.Client, string) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "dashboard:main")
		return ok("Deployed main.")
	}
	deployPR = func(_ *kube.Client, pr int, flavor string) (*types.OperationResponse, error) {
		fc.ran = append(fc.ran, "dashboard:pr")
		if fc.legacy != nil {
			fc.legacy = &types.DashboardState{IsCustomPR: true, PRNumber: pr}
			return ok("Deployed PR.")
		}
		fc.session = &types.DashboardOverride{Active: true, SessionRecorded: true, Mode: "pr", PRNumber: pr, Flavor: flavor}
		return ok("Deployed PR.")
	}
	waitDashboard = func(*kube.Client) ([]types.DashboardDevImage, error) {
		fc.ran = append(fc.ran, "dashboard:wait")
		fc.dashReady = true
		return nil, nil
	}
}

// testClient is a client whose context ends after a while, so a step that
// waited forever would fail its test instead of hanging it.
func testClient() *kube.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_ = cancel // ends with the timeout
	return kube.NewClientWithContext(ctx, "t")
}

// readyCluster returns a fakeCluster with every recipe-driven piece already present.
func readyCluster() *fakeCluster {
	return &fakeCluster{
		samplesAdded: true, maasReady: true,
		prereqReady: map[string]bool{"models": true, "playground": true, "model-registry": true},
		model:       &types.ModelState{ResourceState: types.ResourceState{Deployed: true, Ready: true, ManagedByTool: true}},
		published:   true,
		playground:  &types.PlaygroundState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, Ready: true, ManagedByTool: true}, Models: []types.PlaygroundModelRef{{Name: maasEntryName, Source: "maas"}}},
		maasEntry:   readyMaaSEntry(),
		registry:    types.ModelRegistryStatus{State: "ready", ManagedByTool: true, Sample: &types.ModelRegistrySample{Complete: true}, Address: "https://x"},
		workbench:   &types.WorkbenchState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, ManagedByTool: true}, State: "stopped"},
		session:     &types.DashboardOverride{Active: true, SessionRecorded: true, Mode: "pr", PRNumber: 77, Flavor: "rhoai"},
		dashReady:   true,
	}
}

// maasEntryName is the name the playground lists the published model by
// (maas-api's, from its MaaSModelRef).
const maasEntryName = "publishers/proj/models/qwen3-1-7b"

func readyMaaSEntry() *types.PlaygroundModel {
	return &types.PlaygroundModel{Name: maasEntryName, DisplayName: "Qwen3 1.7B", Source: "maas", Status: "Ready", Ready: true, Chat: true, Subscriptions: []string{"rhoai-nightly-updater-subscription"}}
}

func stepOf(t *testing.T, r types.EnvironmentRecipe, id string) Step {
	t.Helper()
	for _, s := range featureSteps(r) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s", id)
	return Step{}
}

// Check on each feature step inspects live cluster state: fully provisioned
// pieces are skipped (Resume treats them as already set up). Step IDs must
// follow the runner's dependency ordering.
func TestFeatureSteps_CheckSkipsWhatIsSetUp(t *testing.T) {
	r := validRecipe(t, allFeatures)
	fc := readyCluster()
	fc.install(t)
	var ids []string
	for _, step := range featureSteps(r) {
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

// When a required piece is missing, Check leaves the step runnable; foreign
// or third-party resources block with a reason and trigger no setup calls.
func TestFeatureSteps_CheckRunsOrBlocks(t *testing.T) {
	r := validRecipe(t, allFeatures)
	cases := []struct {
		name, step string
		edit       func(fc *fakeCluster)
		blocked    string // empty blocked means the step should run
	}{
		{"samples missing", StepMLflowSamples, func(fc *fakeCluster) { fc.samplesAdded = false }, ""},
		{"a sample whose state cannot be read", StepMLflowSamples, func(fc *fakeCluster) { fc.sampleUnknown = true }, ""},
		{"MaaS not ready", StepMaaS, func(fc *fakeCluster) { fc.maasReady = false }, ""},
		{"Models prerequisites", StepModelsPrereq, func(fc *fakeCluster) { fc.prereqReady["models"] = false }, ""},
		{"no model", StepModel, func(fc *fakeCluster) { fc.model = nil }, ""},
		{"a model still starting", StepModel, func(fc *fakeCluster) { fc.model.Ready = false }, ""},
		{"someone else's model", StepModel, func(fc *fakeCluster) { fc.model.ManagedByTool = false }, "this tool did not create"},
		{"not published", StepMaaSPublish, func(fc *fakeCluster) { fc.published = false }, ""},
		{"routed through MaaS, no publication", StepMaaSPublish, func(fc *fakeCluster) { fc.published, fc.routedOnly = false, true }, ""},
		{"someone else's playground", StepPlayground, func(fc *fakeCluster) { fc.playground.ManagedByTool = false }, "did not set up"},
		{"no playground", StepPlayground, func(fc *fakeCluster) { fc.playground = nil }, ""},
		{"the sample missing", StepModelRegistry, func(fc *fakeCluster) { fc.registry.Sample.Complete = false }, ""},
		{"a foreign registry", StepModelRegistry, func(fc *fakeCluster) { fc.registry = types.ModelRegistryStatus{State: "ready", Foreign: true} }, "did not create"},
		{"no workbench", StepWorkbench, func(fc *fakeCluster) { fc.workbench = nil }, ""},
		{"a workbench being deleted", StepWorkbench, func(fc *fakeCluster) { fc.workbench.Terminating, fc.workbenchGoneAfter = true, -1 }, ""},
		{"someone else's workbench", StepWorkbench, func(fc *fakeCluster) { fc.workbench.ManagedByTool = false }, "did not create"},
		{"another PR deployed", StepDashboardDev, func(fc *fakeCluster) { fc.session.PRNumber = 78 }, ""},
		{"the build deployed, not rolled out", StepDashboardDev, func(fc *fakeCluster) { fc.dashReady = false }, ""},
		{"no session", StepDashboardDev, func(fc *fakeCluster) { fc.session = nil }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := readyCluster()
			tc.edit(fc)
			fc.install(t)
			chk, err := stepOf(t, r, tc.step).Check(nil, &State{Recipe: r})
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

// Resume after MaaS begins at Models prerequisites: samples and MaaS already
// on the cluster are skipped; following steps run in sequence with the same
// requests their UI pages would issue.
func TestFeatureSteps_ResumeAfterMaaSContinuesWithModels(t *testing.T) {
	r := validRecipe(t, allFeatures)
	// Dashboard Dev step executes even though no override session is recorded yet.
	fc := &fakeCluster{samplesAdded: true, maasReady: true, session: &types.DashboardOverride{}, maasEntry: readyMaaSEntry()}
	fc.install(t)
	c := testClient()
	s := &State{Recipe: r, RunID: "run-1"}
	var outcome []string
	for _, step := range featureSteps(r) {
		chk, err := step.Check(c, s)
		if err != nil || chk.Blocked != "" {
			t.Fatalf("%s: %+v %v", step.ID, chk, err)
		}
		if chk.Done {
			outcome = append(outcome, step.ID+"=skipped")
			continue
		}
		if step.ID == StepModelsPrereq || step.ID == StepPlaygroundPrereq || step.ID == StepModelRegistryPrereq {
			// Prerequisite runs belong to the Environment page tests; here we
			// only flip fakeCluster so later checks see them ready.
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
	// The recipe publishes its model through MaaS: the playground uses its MaaS entry.
	if m := fc.playgroundReq.Models; len(m) != 1 || m[0] != (types.PlaygroundModelRef{Name: maasEntryName, Source: "maas"}) {
		t.Errorf("playground request %+v", fc.playgroundReq)
	}
	if fc.modelsReq.Project != "proj" || fc.modelsReq.Model != "qwen3-1-7b" || fc.modelsReq.Exposure != "default-gateway" || s.Exposure != "default-gateway" {
		t.Errorf("model request %+v, state exposure %q", fc.modelsReq, s.Exposure)
	}
	// Pre-existing MaaS must not land in Created. The registry this run
	// provisioned should be tracked with the run annotation and UID from setup.
	if len(s.Created) != 1 || s.Created[0] != (types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: "reg-1"}) || fc.registryAnn[RecipeRunAnnotation] != "run-1" {
		t.Errorf("created %+v, annotations %v", s.Created, fc.registryAnn)
	}
}

// With no recipe exposure, the project default is resolved for prerequisite
// checks and pinned on the model run. Missing recommendations block and
// surface the reason. Explicit recipe exposure values pass through unchanged.
func TestModelsExposure_ResolvedPinnedOrBlocked(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true}}\n")
	fc := &fakeCluster{exposures: &types.ModelExposures{Default: "gateway:ns1/gw", Options: []types.ModelExposureOption{
		{Key: "default-gateway", Exposure: "default-gateway"}, {Key: "gateway:ns1/gw", Exposure: "gateway", GatewayRef: "ns1/gw", Available: true}}}}
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
	if err != nil || !strings.Contains(chk.Blocked, "KServe has no default gateway.") || !strings.Contains(chk.Blocked, "Only inside the cluster always works") {
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

// M1: UI may attach a gateway ref on the MaaS option, but setup only keeps
// GatewayRef when exposure is "gateway". Recommended MaaS must omit it so
// ValidateModelsRequest accepts the payload.
func TestModelsExposure_RecommendedMaaSSendsNoGatewayRef(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, model: qwen3-1-7b}, maas: {enabled: true}}\n")
	fc := &fakeCluster{exposures: &types.ModelExposures{Default: "maas", Options: []types.ModelExposureOption{
		{Key: "maas", Exposure: "maas", GatewayRef: "openshift-ingress/maas-default-gateway", Available: true}}}, published: true}
	fc.install(t)
	s := &State{Recipe: r}
	res, err := runModel(testClient(), s, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || fc.modelsReq.Exposure != "maas" || fc.modelsReq.GatewayRef != "" || s.GatewayRef != "" {
		t.Fatalf("%+v %v request %+v", res, err, fc.modelsReq)
	}
	if problem := serving.ValidateModelsRequest(fc.modelsReq); problem != "" {
		t.Errorf("the request is refused: %s", problem)
	}
}

// M5: During resume, recipe-specified exposure overrides state from a prior
// run. Stored gateway refs survive only if the Models UI still lists that gateway.
func TestModelsExposure_ResumeRevalidatesTheRecordedOne(t *testing.T) {
	own := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, exposure: internal}}\n")
	fc := &fakeCluster{}
	fc.install(t)
	if e, ref, problem, _ := exposureOf(nil, &State{Recipe: own, Exposure: "maas"}); e != "internal" || ref != "" || problem != "" {
		t.Errorf("own: %q %q %q", e, ref, problem)
	}
	auto := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true}}\n")
	fc.exposures = &types.ModelExposures{Default: "default-gateway", Options: []types.ModelExposureOption{
		{Key: "default-gateway", Exposure: "default-gateway", Available: true}, {Key: "gateway:ns1/gw", Exposure: "gateway", GatewayRef: "ns1/gw", Available: true}}}
	if e, ref, _, _ := exposureOf(nil, &State{Recipe: auto, Exposure: "gateway", GatewayRef: "ns1/gw"}); e != "gateway" || ref != "ns1/gw" {
		t.Errorf("offered gateway: %q %q", e, ref)
	}
	if e, ref, _, _ := exposureOf(nil, &State{Recipe: auto, Exposure: "gateway", GatewayRef: "ns9/gone"}); e != "default-gateway" || ref != "" {
		t.Errorf("gateway gone: %q %q", e, ref)
	}
}

// M3: MaaS exposure requires a finished publish—not merely gateway routing
// such as when publish was interrupted after the model already existed.
func TestModelStep_MaaSExposureNeedsThePublication(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, model: qwen3-1-7b, exposure: maas}, maas: {enabled: true}}\n")
	fc := readyCluster()
	fc.published, fc.routedOnly = false, true
	fc.model.MaaSPublished = true
	fc.install(t)
	if chk, err := checkModel(nil, &State{Recipe: r}); err != nil || chk.Done {
		t.Fatalf("routing only: %+v %v", chk, err)
	}
	fc.published = true
	if chk, err := checkModel(nil, &State{Recipe: r}); err != nil || !chk.Done {
		t.Fatalf("published: %+v %v", chk, err)
	}
}

// M4: runModel succeeds only after Ready flips true, polling until the step
// deadline. Timeout fails the step and tells Resume to retry the wait later.
func TestModelStep_WaitsUntilTheModelServes(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, exposure: internal}}\n")
	fc := &fakeCluster{modelReadyAfter: 3}
	fc.install(t)
	if res, err := runModel(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || fc.modelReads < 3 {
		t.Fatalf("%+v %v after %d reads", res, err, fc.modelReads)
	}
	never := &fakeCluster{modelReadyAfter: -1}
	never.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	c := kube.NewClientWithContext(ctx, "t")
	reads := 0
	readModel = func(*kube.Client, string, string) (types.ModelState, bool, error) {
		if reads++; reads == 5 {
			cancel() // simulate exhausting the step deadline
		}
		return types.ModelState{ResourceState: types.ResourceState{Deployed: true, ManagedByTool: true}}, true, nil
	}
	res, err := runModel(c, &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || res.ErrorCode != "timeout" || !strings.Contains(res.Message, "Resume waits again") {
		t.Fatalf("%+v %v", res, err)
	}
}

// M6: Unknown sample state must not count as present—the step adds readable
// samples, fails overall, and forces Resume to re-check.
func TestMLflowSamplesStep_UnknownStateIsNotPresent(t *testing.T) {
	r := validRecipe(t, allFeatures)
	fc := &fakeCluster{sampleUnknown: true}
	fc.install(t)
	res, err := runMLflowSamples(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || !strings.Contains(res.Message, "Skill: MLflow answered 503") || strings.Join(fc.ran, " ") != "samples:genai-demo,training" {
		t.Fatalf("%+v %v %v", res, err, fc.ran)
	}
}

// M7: Terminating workbenches are polled away before redeploy; if deletion
// never completes within the deadline, the step errors out.
func TestWorkbenchStep_WaitsForADeletedWorkbench(t *testing.T) {
	r := validRecipe(t, allFeatures)
	fc := &fakeCluster{workbench: &types.WorkbenchState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, ManagedByTool: true, Terminating: true}}, workbenchGoneAfter: 2}
	fc.install(t)
	var events []string
	res, err := runWorkbenchStep(testClient(), &State{Recipe: r}, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || !res.Success || strings.Join(fc.ran, " ") != "workbench" || !strings.Contains(strings.Join(events, "\n"), "being deleted") {
		t.Fatalf("%+v %v %v %v", res, err, fc.ran, events)
	}
	stuck := &fakeCluster{workbench: &types.WorkbenchState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, ManagedByTool: true, Terminating: true}}, workbenchGoneAfter: -1}
	stuck.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = runWorkbenchStep(kube.NewClientWithContext(ctx, "t"), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || res.ErrorCode != "timeout" || len(stuck.ran) != 0 {
		t.Fatalf("stuck: %+v %v %v", res, err, stuck.ran)
	}
}

// M2: On resume, an image already deployed but not ready should call wait
// only—no second deploy—and mark done once the dashboard reports ready.
func TestDashboardDevStep_ResumeWaitsWithoutRedeploying(t *testing.T) {
	r := validRecipe(t, "  dashboardDev: {deploy: pr, pr: 77}\n")
	fc := readyCluster()
	fc.dashReady = false
	fc.install(t)
	if chk, _ := checkDashboardDev(nil, &State{Recipe: r}); chk.Done {
		t.Fatalf("not rolled out but done: %+v", chk)
	}
	res, err := runDashboardDev(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || strings.Join(fc.ran, " ") != "dashboard:wait" {
		t.Fatalf("%+v %v %v", res, err, fc.ran)
	}
	if chk, _ := checkDashboardDev(nil, &State{Recipe: r}); !chk.Done {
		t.Errorf("rolled out but not done: %+v", chk)
	}
}

// Dashboard Dev issues deploy plus wait when the module is present; legacy
// flows deploy PR builds without a follow-up readiness wait.
func TestDashboardDevStep_DeploysAndWaits(t *testing.T) {
	r := validRecipe(t, "  dashboardDev: {deploy: main}\n")
	fc := &fakeCluster{session: &types.DashboardOverride{}}
	fc.install(t)
	res, err := runDashboardDev(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || strings.Join(fc.ran, " ") != "dashboard:main dashboard:wait" || !strings.Contains(res.Message, "the dashboard is ready") {
		t.Fatalf("%+v %v %v", res, err, fc.ran)
	}
	legacy := &fakeCluster{legacy: &types.DashboardState{}}
	legacy.install(t)
	pr := validRecipe(t, "  dashboardDev: {deploy: pr, pr: 5, flavor: odh}\n")
	if res, err := runDashboardDev(testClient(), &State{Recipe: pr}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || strings.Join(legacy.ran, " ") != "dashboard:pr" || !legacy.legacy.PodReady {
		t.Fatalf("legacy %+v %v %v", res, err, legacy.ran)
	}
}

// H1: New MaaS platform setup stamps the DB Secret with the run annotation
// and Created UID (cnpg and development alike). Platforms the tool already
// owned must not be attributed to this run.
func TestMaaSStep_MarksWhatItCreated(t *testing.T) {
	for _, db := range []string{"cnpg", "development"} {
		t.Run(db, func(t *testing.T) {
			r := validRecipe(t, "  features: {maas: {enabled: true, database: "+db+"}}\n")
			fc := &fakeCluster{}
			fc.install(t)
			s := &State{Recipe: r, RunID: "run-1"}
			if res, err := runMaaSStep(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || !res.Success {
				t.Fatalf("%+v %v", res, err)
			}
			if len(fc.maasReqs) != 1 || fc.maasReqs[0].Database == nil || fc.maasReqs[0].Database.Kind != db {
				t.Errorf("requests %+v", fc.maasReqs)
			}
			if len(s.Created) != 1 || s.Created[0] != (types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "secret-1"}) || fc.dbMeta.Annotations[RecipeRunAnnotation] != "run-1" {
				t.Errorf("created %+v, Secret %+v", s.Created, fc.dbMeta)
			}
		})
	}
	t.Run("its database Secret there before the run", func(t *testing.T) {
		// The Secret is created only when missing, so it carries no run's annotation.
		m := toolMeta("models-as-a-service-infra", "maas-db-config", "secret-0", nil)
		fc := &fakeCluster{maasOurs: true, dbMeta: &m, maasRec: serving.MaaSRecord{Database: "development", DBNamespace: "models-as-a-service-infra"}}
		fc.install(t)
		s := &State{Recipe: validRecipe(t, "  features: {maas: {enabled: true}}\n"), RunID: "run-1"}
		if _, err := runMaaSStep(testClient(), s, func(types.UpdateStepEvent) {}); err != nil || len(s.Created) != 0 {
			t.Errorf("created %+v %v", s.Created, err)
		}
	})
}

// U6: Sample-enabled runs should claim registration in the result message.
// Pre-existing registries skip Created and annotations; unreadable records
// yield empty Created and an explanatory message.
func TestModelRegistryStep_RecordsWhatItCreated(t *testing.T) {
	r := validRecipe(t, "  features: {modelRegistry: {enabled: true, sample: true}}\n")
	fc := &fakeCluster{}
	fc.install(t)
	s := &State{Recipe: r, RunID: "run-1"}
	res, err := runModelRegistry(testClient(), s, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || len(s.Created) != 1 || s.Created[0].UID != "reg-1" || !strings.Contains(res.Message, "Sample model mnist (version v1) is registered.") {
		t.Fatalf("%+v %v created %+v", res, err, s.Created)
	}
	again := &State{Recipe: r, RunID: "run-2"}
	// The registry is created only when missing: it keeps the first run's annotation.
	if _, err := runModelRegistry(testClient(), again, func(types.UpdateStepEvent) {}); err != nil || len(again.Created) != 0 || fc.regMeta.Annotations[RecipeRunAnnotation] != "run-1" {
		t.Errorf("a registry that was there: %+v %v", again.Created, fc.registryAnn)
	}
	fc2 := &fakeCluster{readErr: errors.New("boom")}
	fc2.install(t)
	s3 := &State{Recipe: r, RunID: "run-3"}
	var events []string
	res, err = runModelRegistry(testClient(), s3, func(e types.UpdateStepEvent) { events = append(events, e.Message) })
	if err != nil || !res.Success || len(s3.Created) != 0 || !strings.Contains(res.Message, "does not offer it") {
		t.Errorf("unreadable record: %+v %v %v", res, s3.Created, events)
	}
}

// H1: Ownership cleanup lists MaaS platform and registry only when live
// objects match run annotation, recorded UID, and tool ownership. Bad claims,
// replaced resources, or read errors offer neither; successful requests cite
// vetted UIDs and run id (H2).
func TestOwnership_OffersOnlyWhatTheRunCreated(t *testing.T) {
	run := func(parts ...types.RecipeCreatedPart) *types.RecipeRun {
		return &types.RecipeRun{ID: "run-1", CreatedParts: parts}
	}
	maas := types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "secret-1"}
	reg := types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: "reg-1"}
	cases := []struct {
		name           string
		run            *types.RecipeRun
		edit           func(fc *fakeCluster)
		maas, registry bool
	}{
		{"both, as created", run(maas, reg), nil, true, true},
		{"nothing recorded, though both are this tool's and marked", run(), nil, false, false},
		{"a forged claim: the platform carries no run annotation", run(maas), func(fc *fakeCluster) { fc.dbMeta.Annotations = nil }, false, false},
		{"another run's annotation", run(maas, reg), func(fc *fakeCluster) {
			fc.dbMeta.Annotations = map[string]string{RecipeRunAnnotation: "run-9"}
			fc.regMeta.Annotations = map[string]string{RecipeRunAnnotation: "run-9"}
		}, false, false},
		{"MaaS set up again since (another database Secret)", run(maas, reg), func(fc *fakeCluster) { fc.dbMeta.UID = "secret-2" }, false, true},
		{"a forged UID claim", run(types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: "x"}), nil, false, false},
		{"a claim with no UID", run(types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform}), nil, false, false},
		{"MaaS no longer this tool's", run(maas), func(fc *fakeCluster) { fc.maasOurs = false }, false, false},
		{"the Secret not this tool's", run(maas), func(fc *fakeCluster) { fc.dbMeta.Labels = nil }, false, false},
		{"the registry made again since (another UID)", run(reg), func(fc *fakeCluster) { fc.regMeta.UID = "reg-2" }, false, false},
		{"the registry no longer in the tool's record", run(reg), func(fc *fakeCluster) { fc.registryRec = modelregistry.Record{} }, false, false},
		{"the registry record unreadable", run(reg), func(fc *fakeCluster) { fc.readErr = errors.New("boom") }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := toolMeta("infra", "maas-db-config", "secret-1", map[string]string{RecipeRunAnnotation: "run-1"})
			rm := toolMeta(registryNS, modelregistry.Name, "reg-1", map[string]string{RecipeRunAnnotation: "run-1"})
			fc := &fakeCluster{maasOurs: true, maasRec: serving.MaaSRecord{Database: "cnpg", DBNamespace: "infra"}, dbMeta: &db, regMeta: &rm,
				registryRec: modelregistry.Record{registryNS + "/" + modelregistry.Name: {UID: "reg-1"}}}
			if tc.edit != nil {
				tc.edit(fc)
			}
			fc.install(t)
			created, req := Ownership(nil, testClient(), tc.run)
			gotMaaS, gotReg := req != nil && req.MaaSPlatform, req != nil && req.ModelRegistry
			if gotMaaS != tc.maas || gotReg != tc.registry || created[StepMaaS] != tc.maas || created[StepModelRegistry] != tc.registry {
				t.Errorf("req %+v created %v", req, created)
			}
			if gotMaaS && (req.MaaSPlatformUID != "secret-1" || req.RecipeRunID != "run-1") || gotReg && req.ModelRegistryUID != "reg-1" {
				t.Errorf("the request does not name what was reviewed: %+v", req)
			}
			if !tc.maas && !tc.registry && req != nil {
				t.Errorf("a clean-up with nothing in it: %+v", req)
			}
		})
	}
	// Nil client skips registry record reads, so registry cannot be offered.
	db := toolMeta("infra", "maas-db-config", "secret-1", map[string]string{RecipeRunAnnotation: "run-1"})
	fc := &fakeCluster{maasOurs: true, maasRec: serving.MaaSRecord{Database: "cnpg", DBNamespace: "infra"}, dbMeta: &db}
	fc.install(t)
	if _, req := Ownership(nil, nil, run(reg)); req != nil {
		t.Errorf("registry offered without the user's client: %+v", req)
	}
	if p := CreatedPartProblem(nil, testClient(), "run-1", types.RecipePartMaaSPlatform, "secret-2"); !strings.Contains(p, "replaced or changed") {
		t.Errorf("a replaced platform: %q", p)
	}
}

// Each step's Max and Typical durations must stay within record.OperationDeadline
// (15 minutes), with Typical positive and not above Max.
func TestFeatureSteps_FitTheStepDeadline(t *testing.T) {
	r := validRecipe(t, allFeatures)
	for _, s := range Steps(r) {
		if s.Max > record.OperationDeadline || s.Typical > s.Max || s.Typical <= 0 {
			t.Errorf("%s: typical %v, max %v, deadline %v", s.ID, s.Typical, s.Max, record.OperationDeadline)
		}
	}
	if title := stepOf(t, r, StepModel).Title; title != "Model Qwen3 1.7B (CPU, tool calling)" {
		t.Errorf("model title %q", title)
	}
	_ = fmt.Sprint
}

// Recipes for the playground's Models as a Service choice: the model
// published through MaaS, reached through MaaS, or neither.
const (
	playgroundPublish  = "  project: {mode: existing, name: proj}\n  features:\n    models: {enabled: true, model: qwen3-1-7b}\n    playground: {enabled: true}\n    maas: {enabled: true, database: cnpg, publish: true}\n"
	playgroundExposure = "  project: {mode: existing, name: proj}\n  features:\n    models: {enabled: true, model: qwen3-1-7b, exposure: maas}\n    playground: {enabled: true}\n    maas: {enabled: true, database: cnpg}\n"
	playgroundNeither  = "  project: {mode: existing, name: proj}\n  features:\n    models: {enabled: true, model: qwen3-1-7b}\n    playground: {enabled: true}\n    maas: {enabled: true, database: cnpg}\n"
)

// The playground uses the model's MaaS entry exactly when the model is
// offered only through MaaS: published by the run, or reached through MaaS
// (the recipe's exposure or the one the run resolved).
func TestPlaygroundThroughMaaS(t *testing.T) {
	cases := []struct {
		name, spec, resolved string
		want                 bool
	}{
		{"published through MaaS", playgroundPublish, "", true},
		{"exposure maas", playgroundExposure, "", true},
		{"resolved exposure maas", playgroundNeither, serving.ExposureMaaS, true},
		{"neither", playgroundNeither, "", false},
		{"neither, resolved to a gateway", playgroundNeither, serving.ExposureGateway, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRecipe(t, tc.spec)
			if got := playgroundThroughMaaS(&State{Recipe: r, Exposure: tc.resolved}); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if w := playgroundWarnings(r); (len(w) == 1) != (tc.want && tc.resolved == "") {
				t.Errorf("warnings %+v", w)
			} else if len(w) == 1 && (w[0].Kind != types.RecipeWarningAdvice || !strings.Contains(w[0].Message, "only through Models as a Service") || !strings.Contains(w[0].Message, "with your sign-in")) {
				t.Errorf("warning %+v", w[0])
			}
		})
	}
}

// The Playground step sends the model's MaaS entry (as the MaaS model
// list names it) when the model is offered only through MaaS, and fails
// with the reason, before any setup, when that entry cannot be used.
func TestRunPlaygroundStep_MaaSEntry(t *testing.T) {
	notReady := readyMaaSEntry()
	notReady.Ready, notReady.Status = false, "Pending"
	unoffered := readyMaaSEntry()
	unoffered.Subscriptions = nil
	cases := []struct {
		name, spec string
		entry      *types.PlaygroundModel
		err        error
		want       []types.PlaygroundModelRef
		failure    string
	}{
		{"published: MaaS entry", playgroundPublish, readyMaaSEntry(), nil, []types.PlaygroundModelRef{{Name: maasEntryName, Source: "maas"}}, ""},
		{"exposure maas: MaaS entry", playgroundExposure, readyMaaSEntry(), nil, []types.PlaygroundModelRef{{Name: maasEntryName, Source: "maas"}}, ""},
		{"neither: the project's models", playgroundNeither, readyMaaSEntry(), nil, nil, ""},
		{"not found", playgroundPublish, nil, nil, nil, "is not published through Models as a Service (no MaaSModelRef names it)"},
		{"not ready", playgroundPublish, notReady, nil, nil, "is not ready in Models as a Service yet (Pending)"},
		{"no subscription", playgroundPublish, unoffered, nil, nil, "No Models as a Service subscription offers model qwen3-1-7b"},
		{"read fails", playgroundPublish, nil, errors.New("boom"), nil, "Cannot read how Models as a Service offers model qwen3-1-7b in proj: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCluster{maasEntry: tc.entry, maasEntryErr: tc.err}
			fc.install(t)
			res, err := runPlaygroundStep(testClient(), &State{Recipe: validRecipe(t, tc.spec)}, func(types.UpdateStepEvent) {})
			if err != nil {
				t.Fatal(err)
			}
			if tc.failure != "" {
				if res.Success || !strings.Contains(res.Message, tc.failure) || len(fc.ran) != 0 {
					t.Fatalf("got %+v, ran %v", res, fc.ran)
				}
				return
			}
			if !res.Success || fc.playgroundReq.Project != "proj" || fmt.Sprint(fc.playgroundReq.Models) != fmt.Sprint(tc.want) {
				t.Fatalf("got %+v, request %+v", res, fc.playgroundReq)
			}
		})
	}
}

// With the model offered only through MaaS, the playground counts as done
// only when this tool set it up with the model's MaaS entry; one set up
// without it (or before the record kept its models) blocks, and the step
// runs when the entry cannot be confirmed.
func TestCheckPlayground_MaaSModel(t *testing.T) {
	ready := func(models ...types.PlaygroundModelRef) *types.PlaygroundState {
		return &types.PlaygroundState{ResourceState: types.ResourceState{Namespace: "proj", Deployed: true, Ready: true, ManagedByTool: true}, Models: models}
	}
	maasRef := types.PlaygroundModelRef{Name: maasEntryName, Source: "maas"}
	own := types.PlaygroundModelRef{Name: "qwen3-1-7b", Source: "namespace"}
	starting := ready(maasRef)
	starting.Ready = false
	cases := []struct {
		name       string
		spec       string
		playground *types.PlaygroundState
		entry      *types.PlaygroundModel
		done       string
		blocked    string
	}{
		{"set up with the MaaS entry", playgroundPublish, ready(maasRef), readyMaaSEntry(), "ready, with Qwen3 1.7B (through Models as a Service)", ""},
		{"still starting with it", playgroundPublish, starting, readyMaaSEntry(), "", ""},
		{"set up with other models", playgroundPublish, ready(own), readyMaaSEntry(), "", "with other models, not Qwen3 1.7B (through Models as a Service)"},
		{"models not recorded", playgroundExposure, ready(), readyMaaSEntry(), "", "does not say which models the playground in proj was set up with"},
		{"entry not found: the run says why", playgroundPublish, ready(maasRef), nil, "", ""},
		{"no playground", playgroundPublish, nil, readyMaaSEntry(), "", ""},
		{"neither: any ready playground of this tool", playgroundNeither, ready(own), nil, "The playground in proj is ready.", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCluster{playground: tc.playground, maasEntry: tc.entry}
			fc.install(t)
			chk, err := checkPlayground(testClient(), &State{Recipe: validRecipe(t, tc.spec)})
			switch {
			case err != nil:
				t.Fatal(err)
			case tc.blocked != "":
				if chk.Done || !strings.Contains(chk.Blocked, tc.blocked) || !strings.Contains(chk.Blocked, "delete it on the Playground tab, then Resume") {
					t.Fatalf("got %+v", chk)
				}
			case tc.done != "":
				if !chk.Done || chk.Blocked != "" || !strings.Contains(chk.Reason, tc.done) {
					t.Fatalf("got %+v", chk)
				}
			default:
				if chk.Done || chk.Blocked != "" {
					t.Fatalf("got %+v, want the step to run", chk)
				}
			}
		})
	}
}
