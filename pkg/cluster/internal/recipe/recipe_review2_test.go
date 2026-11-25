package recipe

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// M1 (review probe): although the opt-in record claims enabled, the live
// dashboard toggle has been switched off and no Queue hardware profile
// exists. Completion fails until Run flips the opt-in back on.
func TestKueueStep_OptInDoneOnlyWhenInEffect(t *testing.T) {
	r := validRecipe(t, kueueRecipe)
	tru := true
	st := setUpKueue("proj", true, false, true)
	st.OptIn.DisableKueue = &tru
	st.OptIn.Profile = types.KueueObject{Kind: "HardwareProfile", Name: kueue.ProfileName, Present: false}
	fk := &fakeKueue{st: st}
	fk.install(t)
	if chk, err := checkKueue(nil, &State{Recipe: r}); err != nil || chk.Done {
		t.Fatalf("done with the opt-in not in effect: %+v %v", chk, err)
	}
	if res, err := runKueueStep(testClient(), &State{Recipe: r, RunID: "run-1"}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || len(fk.reqs) != 2 || fk.reqs[1].Action != "optin" {
		t.Fatalf("%+v %v requests %+v", res, err, fk.reqs)
	}
	if chk, _ := checkKueue(nil, &State{Recipe: r}); !chk.Done {
		t.Errorf("not done once the opt-in is in effect: %+v", chk)
	}
}

// M1: an opt-in on record whose profile was never provisioned—or has
// vanished—prevents setup from being advertised, verbatim; profile lookup
// uses the namespace stored on the opt-in.
func TestKueueOwnership_HalfDoneOptIn(t *testing.T) {
	run := &types.RecipeRun{ID: "run-1", CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: "cq-1"}}}
	cq := toolMeta("", kueue.ClusterQueueName, "cq-1", map[string]string{RecipeRunAnnotation: "run-1"})
	fk := &fakeKueue{cq: &cq, rec: kueue.Record{ClusterQueueUID: "cq-1", OptIn: &kueue.OptIn{DisableKueueSet: true, ConfigNamespace: "apps"}}}
	fk.install(t)
	if p := CreatedPartProblem(nil, testClient(), "run-1", types.RecipePartKueue, "cq-1"); !strings.Contains(p, "was never created") {
		t.Errorf("profile never created: %q", p)
	}
	var askedNS string
	kueueProfileMeta = func(_ *kube.Client, ns string) (kube.ObjectMeta, bool, error) {
		askedNS = ns
		return kube.ObjectMeta{}, false, nil
	}
	fk.rec.OptIn = &kueue.OptIn{DisableKueueSet: true, ProfileUID: "prof-1", ConfigNamespace: "apps"}
	if p := CreatedPartProblem(nil, testClient(), "run-1", types.RecipePartKueue, "cq-1"); !strings.Contains(p, "is gone from apps") || askedNS != "apps" {
		t.Errorf("profile gone: %q (asked %q)", p, askedNS)
	}
	if _, req := Ownership(nil, testClient(), run); req != nil {
		t.Errorf("offered: %+v", req)
	}
}

// L3: Clean up documents that teardown restores the dashboard setting
// solely when this run's opt-in enabled it (record shows the tool flipped
// it, and the run's profile ties the opt-in to the run); profiles created
// while the setting was already on stay unchanged.
func TestKueueOwnership_SaysWhetherTheSettingGoesBack(t *testing.T) {
	run := &types.RecipeRun{ID: "run-1", CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartKueue, UID: "cq-1"}}}
	for _, set := range []bool{true, false} {
		cq := toolMeta("", kueue.ClusterQueueName, "cq-1", map[string]string{RecipeRunAnnotation: "run-1"})
		p := toolMeta("apps", kueue.ProfileName, "prof-1", map[string]string{RecipeRunAnnotation: "run-1"})
		fk := &fakeKueue{cq: &cq, profile: &p, rec: kueue.Record{ClusterQueueUID: "cq-1", OptIn: &kueue.OptIn{DisableKueueSet: set, ProfileUID: "prof-1", ProfileNamespace: "apps"}}}
		fk.install(t)
		created, req := Ownership(nil, testClient(), run)
		if req == nil || !req.Kueue || created[CreatedKueueSetting] != set {
			t.Errorf("setting set by the run %v: req %+v created %v", set, req, created)
		}
	}
}

// M2: publish completes only after MaaS acknowledges the model (per the
// polling reads), and Resume fails inside the step timeout when that never
// happens.
func TestMaaSPublishStep_WaitsUntilMaaSAccepts(t *testing.T) {
	r := validRecipe(t, allFeatures)
	fc := readyCluster()
	fc.published, fc.publishAcceptedAfter = false, 3
	fc.install(t)
	if res, err := runMaaSPublish(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || fc.publishReads < 3 {
		t.Fatalf("%+v %v after %d reads", res, err, fc.publishReads)
	}
	never := readyCluster()
	never.published, never.publishAcceptedAfter = false, -1
	never.install(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := runMaaSPublish(kube.NewClientWithContext(ctx, "t"), &State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || !strings.Contains(res.Message, "Resume waits again") {
		t.Fatalf("%+v %v", res, err)
	}
}

// L1/L2: objects stamped with this run's annotation belong to the run
// even if recording lagged behind creation; the resumed check picks them
// up (unannotated objects do not).
func TestCreatedParts_RecoveredOnResume(t *testing.T) {
	r := validRecipe(t, strings.Replace(allFeatures, "    workbench: {enabled: true}\n", "    workbench: {enabled: true}\n    kueue: {enabled: true}\n", 1))
	db := toolMeta("infra", "maas-db-config", "secret-1", map[string]string{RecipeRunAnnotation: "run-1"})
	reg := toolMeta(registryNS, modelregistry.Name, "reg-1", map[string]string{RecipeRunAnnotation: "run-1"})
	fc := readyCluster()
	fc.maasOurs, fc.dbMeta, fc.regMeta = true, &db, &reg
	fc.maasRec = serving.MaaSRecord{Database: "development", DBNamespace: "infra"}
	fc.registryRec = modelregistry.Record{registryNS + "/" + modelregistry.Name: {UID: "reg-1"}}
	fc.install(t)
	cq := toolMeta("", kueue.ClusterQueueName, "cq-1", map[string]string{RecipeRunAnnotation: "run-1"})
	fk := &fakeKueue{st: setUpKueue("proj", false, false, false), cq: &cq, rec: kueue.Record{ClusterQueueUID: "cq-1"}}
	fk.install(t)
	s := &State{Recipe: r, RunID: "run-1"}
	for _, check := range []func(*kube.Client, *State) (Check, error){checkMaaS, checkModelRegistry, checkKueue} {
		if chk, err := check(testClient(), s); err != nil || !chk.Done {
			t.Fatalf("%+v %v", chk, err)
		}
	}
	var parts []string
	for _, p := range s.Created {
		parts = append(parts, p.Part+"="+p.UID)
	}
	if got := strings.Join(parts, " "); got != "kueue=cq-1 maas-platform=secret-1 model-registry=reg-1" {
		t.Errorf("recovered %s", got)
	}
	other := &State{Recipe: r, RunID: "run-2"}
	for _, check := range []func(*kube.Client, *State) (Check, error){checkMaaS, checkModelRegistry, checkKueue} {
		_, _ = check(testClient(), other)
	}
	if len(other.Created) != 0 {
		t.Errorf("another run claims %+v", other.Created)
	}
}

// L6: displayed max durations reflect actual blocking work: waiting out
// workbench deletion and MaaS acceptance of a publication, each bounded by
// the step deadline.
func TestFeatureSteps_MaxCoversTheWaits(t *testing.T) {
	r := validRecipe(t, allFeatures)
	for _, id := range []string{StepWorkbench, StepMaaSPublish, StepModel} {
		if s := stepOf(t, r, id); s.Max != 15*time.Minute {
			t.Errorf("%s: max %v", id, s.Max)
		}
	}
}

// L7: when a pull request lacks published dashboard images, the Dashboard
// Dev check blocks and the plan aborts before execution starts.
func TestDashboardDevCheck_UnpublishedPRBlocks(t *testing.T) {
	r := validRecipe(t, "  dashboardDev: {deploy: pr, pr: 77}\n")
	fc := &fakeCluster{session: &types.DashboardOverride{}, prMissing: true}
	fc.install(t)
	chk, err := checkDashboardDev(nil, &State{Recipe: r})
	if err != nil || !strings.Contains(chk.Blocked, "Pull request #77 has no published dashboard images yet") || chk.Fix == nil || chk.Fix.Page != pageDashboardDev {
		t.Fatalf("%+v %v", chk, err)
	}
}

// L8: without the dashboard module (legacy path) the step still blocks on
// rollout, and resuming against an already-deployed ready PR finishes
// without a second deploy.
func TestDashboardDevStep_LegacyFlowWaitsAndIsDone(t *testing.T) {
	r := validRecipe(t, "  dashboardDev: {deploy: pr, pr: 77}\n")
	fc := &fakeCluster{legacy: &types.DashboardState{}}
	fc.install(t)
	if res, err := runDashboardDev(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || !fc.legacy.PodReady {
		t.Fatalf("%+v %v %+v", res, err, fc.legacy)
	}
	fc.ran = nil
	if chk, err := checkDashboardDev(nil, &State{Recipe: r}); err != nil || !chk.Done {
		t.Fatalf("resume over the deployed PR: %+v %v", chk, err)
	}
	// Pod exists but readiness pending: expect polling only.
	fc.legacy = &types.DashboardState{IsCustomPR: true, PRNumber: 77}
	fc.legacyReads = 0
	if res, err := runDashboardDev(testClient(), &State{Recipe: r}, func(types.UpdateStepEvent) {}); err != nil || !res.Success || len(fc.ran) != 0 {
		t.Fatalf("%+v %v deployed again: %v", res, err, fc.ran)
	}
}

// L9: a tool model reached via a path other than the recipe's counts as
// incomplete: the check blocks and reports it unchanged. The same model
// published through MaaS under this recipe counts as MaaS exposure.
func TestModelCheck_ExposureMismatchBlocks(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, model: qwen3-1-7b, exposure: default-gateway}}\n")
	fc := readyCluster()
	fc.model.Exposure = serving.ExposureInternal
	fc.install(t)
	chk, err := checkModel(nil, &State{Recipe: r})
	if err != nil || !strings.Contains(chk.Blocked, "already reached only inside the cluster, not through the cluster's default gateway") {
		t.Fatalf("%+v %v", chk, err)
	}
	pub := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {models: {enabled: true, model: qwen3-1-7b, exposure: default-gateway}, maas: {enabled: true, publish: true}}\n")
	fc.model.Exposure = serving.ExposureMaaS
	if chk, err := checkModel(nil, &State{Recipe: pub}); err != nil || chk.Blocked != "" || !chk.Done {
		t.Errorf("published by the recipe: %+v %v", chk, err)
	}
}

// L10: guidance lists every project artifact the recipe enables (playground
// and MLflow samples included); the cluster-scoped warning appears only when
// the recipe enables the setting and it is not already active.
func TestKueuePlan_WarningsFromWhatTheRecipeEnables(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    mlflow: {enabled: true, samples: true}\n    models: {enabled: true}\n    playground: {enabled: true}\n    kueue: {enabled: true}\n")
	w := kueueWarnings(r, kueueLive{})
	if len(w) != 1 || !strings.Contains(w[0].Message, "a model, a playground and the MLflow samples") {
		t.Fatalf("%+v", w)
	}
	on := validRecipe(t, "  project: {mode: existing, name: proj}\n  features: {kueue: {enabled: true, dashboardOptIn: true}}\n")
	fk := &fakeKueue{st: setUpKueue("proj", false, false, true)}
	fk.install(t)
	if w := planKueueWarnings(nil, on); len(w) != 0 {
		t.Errorf("opt-in already on: %+v", w)
	}
	fk.st = setUpKueue("proj", false, false, false)
	if w := planKueueWarnings(nil, on); len(w) != 1 || w[0].Kind != types.RecipeWarningCluster {
		t.Errorf("opt-in off: %+v", w)
	}
}
