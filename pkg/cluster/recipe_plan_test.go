package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tRecipeCSV  = "/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/clusterserviceversions/rhods-operator.3.6.0"
	tRecipeDSC  = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"
	tRecipeFBC  = "quay.io/rhoai/rhoai-fbc-fragment@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tRecipeBase = `{"apiVersion":"rhoai-nightly-updater/v1alpha1","kind":"EnvironmentRecipe","metadata":{"name":"dev"},"spec":`
)

// planRecipeFor validates spec (a JSON object) as a recipe and plans it.
func planRecipeFor(t *testing.T, c *Client, spec string) (*types.RecipePlan, recipe.State) {
	t.Helper()
	r, problems := recipe.Validate([]byte(tRecipeBase + spec + `}`))
	if len(problems) > 0 {
		t.Fatalf("recipe: %v", problems)
	}
	return recipe.Plan(c, r, nil)
}

func planActions(p *types.RecipePlan) string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.ID+"="+s.Action)
	}
	return strings.Join(out, " ")
}

func problemText(p *types.RecipePlan) string {
	var out []string
	for _, pr := range p.Problems {
		out = append(out, pr.Message)
	}
	return strings.Join(out, "\n")
}

// problemFixes lists the problems' fix hints, "code@page" ("-" for none).
func problemFixes(p *types.RecipePlan) string {
	var out []string
	for _, pr := range p.Problems {
		if pr.Fix == nil {
			out = append(out, "-")
			continue
		}
		out = append(out, pr.Fix.Code+"@"+pr.Fix.Page)
	}
	return strings.Join(out, " ")
}

// installedRHOAI puts an installed operator (Subscription and a Succeeded
// CSV) and a DataScienceCluster with conditions.
func installedRHOAI(f *resourceFake) {
	f.putJSON(testSubscription, `{"spec":{"source":"rhoai-catalog-dev","channel":"beta"},"status":{"state":"AtLatestKnown","installedCSV":"rhods-operator.3.6.0"}}`)
	f.putJSON(tRecipeCSV, `{"spec":{"version":"3.6.0","displayName":"Red Hat OpenShift AI"},"status":{"phase":"Succeeded"}}`)
	f.putJSON(tRecipeDSC, `{"spec":{"components":{}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
}

// A fresh cluster (no operator, no DataScienceCluster API): the RHOAI step
// runs, the steps that depend on it are checked when the run gets there,
// and without the one-time setup the plan refuses. Nothing is changed.
func TestRecipePlan_FreshClusterWithoutTheOneTimeSetup(t *testing.T) {
	f, c := projectFake(t)
	plan, state := planRecipeFor(t, c, `{"rhoai":{"build":{"image":"`+tRecipeFBC+`"}},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}`)
	if got := planActions(plan); got != "rhoai=run dsc=later project=run prerequisites=later s3=run pipeline-server=later mlflow=later" {
		t.Fatalf("actions %s", got)
	}
	if !strings.Contains(problemText(plan), "additional-pull-secret is missing") || !strings.Contains(problemText(plan), "no image mirror") {
		t.Fatalf("problems %s", problemText(plan))
	}
	if got := problemFixes(plan); got != "one_time_setup_missing@/ one_time_setup_missing@/" {
		t.Errorf("fixes %s", got)
	}
	if state.Pinned == nil || state.Pinned.Image != tRecipeFBC || state.Pinned.Digest != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("pinned %+v", state.Pinned)
	}
	if plan.TypicalSeconds == 0 || plan.MaxSeconds < plan.TypicalSeconds {
		t.Fatalf("durations %d %d", plan.TypicalSeconds, plan.MaxSeconds)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("the plan changed %v", m)
	}
}

// A cluster where everything the recipe asks for is there: every step is
// skipped and nothing is refused.
func TestRecipePlan_SetUpClusterSkipsEveryStep(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.putJSON(tRecipeNS, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	plan, _ := planRecipeFor(t, c, `{"rhoai":{"build":"latest","ifInstalled":"keep"},"dataScienceCluster":{"create":true},"project":{"mode":"existing","name":"recipe-dev"},"features":{}}`)
	if got := planActions(plan); got != "rhoai=skip dsc=skip project=skip" || len(plan.Problems) != 0 {
		t.Fatalf("actions %s, problems %s", got, problemText(plan))
	}
	if plan.Pinned != nil {
		t.Fatalf("a kept operator needs no build, pinned %+v", plan.Pinned)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("the plan changed %v", m)
	}
}

// A project that exists and is not the recipe's blocks the plan.
func TestRecipePlan_ForeignProjectBlocks(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.putJSON(tRecipeNS, `{}`)
	plan, _ := planRecipeFor(t, c, `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},"features":{}}`)
	if got := planActions(plan); got != "rhoai=skip dsc=skip project=blocked" || !strings.Contains(problemText(plan), "was not created by a recipe") {
		t.Fatalf("actions %s, problems %s", got, problemText(plan))
	}
	if got := problemFixes(plan); got != "project_foreign@" || plan.Steps[2].Fix == nil || *plan.Steps[2].Fix != *plan.Problems[0].Fix {
		t.Errorf("fixes %s, step %+v", got, plan.Steps[2])
	}
}

// A half installed operator (a Subscription whose CSV did not succeed)
// blocks: the recipe does not guess.
func TestRecipePlan_HalfInstalledOperatorBlocks(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.putJSON(tRecipeCSV, `{"spec":{"version":"3.6.0"},"status":{"phase":"Installing"}}`)
	plan, _ := planRecipeFor(t, c, `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"features":{}}`)
	if got := planActions(plan); !strings.HasPrefix(got, "rhoai=blocked") || !strings.Contains(problemText(plan), "half installed") {
		t.Fatalf("actions %s, problems %s", got, problemText(plan))
	}
	if got := problemFixes(plan); got != "operator_half_installed@/" {
		t.Errorf("fixes %s", got)
	}
}

// tRecipeAllFeatures is a recipe with every feature on.
const tRecipeAllFeatures = `{"rhoai":{"build":{"image":"` + tRecipeFBC + `"}},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true,"samples":true},"models":{"enabled":true,"model":"qwen3-1-7b","exposure":"default-gateway"},
"playground":{"enabled":true},"maas":{"enabled":true,"database":"cnpg","publish":true},"modelRegistry":{"enabled":true,"sample":true},"workbench":{"enabled":true}},
"dashboardDev":{"deploy":"pr","pr":1234}}`

// Argo CD managing the operator refuses. The features of recipes phase 2
// (models, the playground, MaaS, the MLflow samples, the model registry,
// the workbench, Dashboard Dev) no longer do.
func TestRecipePlan_ArgoCDRefusesTheFeaturesDoNot(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.argo = argoManagingAll(argo.SubscriptionObject)
	// The prerequisites step is checked now (RHOAI and its
	// DataScienceCluster are there): Models' needs cert-manager from the
	// catalog.
	putFakePackage(f, "openshift-cert-manager-operator", certManagerFakePackage())
	plan, _ := planRecipeFor(t, c, `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"models":{"enabled":true},"playground":{"enabled":true},"mlflow":{"enabled":true,"samples":true}},"dashboardDev":{"deploy":"main"}}`)
	text := problemText(plan)
	if strings.Contains(text, "not in this version") {
		t.Errorf("a feature is still refused: %s", text)
	}
	if !strings.Contains(text, "Argo CD application") || !strings.Contains(text, "manages the RHOAI operator") {
		t.Errorf("Argo CD's management did not refuse: %s", text)
	}
	if got := problemFixes(plan); got != "argo_managed@" {
		t.Errorf("fixes %s: %s", got, text)
	}
}

// Every feature runs, in the runner's dependency order: the steps after
// the RHOAI install are checked when the run gets there, nothing of the
// schema is refused, and the plan's figures are its longest chain of steps
// (the serving lane), as the steps of other lanes run beside it (model
// serving usually 5 minutes on a CPU worker).
func TestRecipePlan_EveryFeatureInDependencyOrder(t *testing.T) {
	f, c := projectFake(t)
	plan, _ := planRecipeFor(t, c, tRecipeAllFeatures)
	want := "rhoai=run dsc=later project=run prerequisites=later s3=run pipeline-server=later mlflow=later mlflow-samples=later " +
		"maas=later model=later maas-publish=later playground=later model-registry=later workbench=later dashboard-dev=later"
	if got := planActions(plan); got != want {
		t.Fatalf("actions\n got %s\nwant %s", got, want)
	}
	for _, p := range plan.Problems {
		if p.Fix == nil || p.Fix.Code != types.RecipeFixOneTimeSetupMissing {
			t.Errorf("problem %+v", p)
		}
	}
	titles := map[string]string{}
	typical, most, sumTypical := 0, 0, 0
	critical := map[string]bool{recipe.StepRHOAI: true, recipe.StepDSC: true, recipe.StepProject: true, recipe.StepPrerequisites: true,
		recipe.StepMaaS: true, recipe.StepModelsPrereq: true, recipe.StepModel: true, recipe.StepMaaSPublish: true, recipe.StepPlayground: true,
		recipe.StepKueuePrereq: true, recipe.StepKueue: true, recipe.StepDashboardDev: true}
	for _, s := range plan.Steps {
		titles[s.ID] = s.Title
		sumTypical += s.TypicalSeconds
		if critical[s.ID] {
			typical, most = typical+s.TypicalSeconds, most+s.MaxSeconds
		}
	}
	if titles["prerequisites"] != "What the features need" || titles["model"] != "Model Qwen3 1.7B (CPU, tool calling)" || titles["dashboard-dev"] != "Dashboard Dev (PR #1234)" || titles["maas"] != "Models as a Service" {
		t.Errorf("titles %v", titles)
	}
	if plan.TypicalSeconds != typical || plan.MaxSeconds != most || plan.TypicalSeconds >= sumTypical {
		t.Errorf("plan %d/%d, critical path %d/%d, every step %d", plan.TypicalSeconds, plan.MaxSeconds, typical, most, sumTypical)
	}
	if s := recipeStep(t, plan.Recipe, recipe.StepModel); s.Typical != 5*time.Minute || s.Max != 15*time.Minute {
		t.Errorf("model step %v/%v", s.Typical, s.Max)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("the plan changed %v", m)
	}
}

// The MaaS publish step exists only when the model is not reached through
// MaaS anyway (exposure maas publishes as part of the model's setup), and
// Dashboard Dev is always last.
func TestRecipeSteps_PublishOnlyWhenNeededDashboardDevLast(t *testing.T) {
	ids := func(spec string) string {
		r, problems := recipe.Validate([]byte(tRecipeBase + spec + `}`))
		if len(problems) > 0 {
			t.Fatal(problems)
		}
		var out []string
		for _, s := range recipe.Steps(r, false) {
			out = append(out, s.ID)
		}
		return strings.Join(out, " ")
	}
	base := `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"existing","name":"p"},"dashboardDev":{"deploy":"main"},"features":`
	if got := ids(base + `{"models":{"enabled":true,"exposure":"maas"},"maas":{"enabled":true,"publish":true}}}`); got != "rhoai dsc project prerequisites maas model dashboard-dev" {
		t.Errorf("maas exposure: %s", got)
	}
	if got := ids(base + `{"models":{"enabled":true},"maas":{"enabled":true,"publish":true}}}`); got != "rhoai dsc project prerequisites maas models-prereq model maas-publish dashboard-dev" {
		t.Errorf("recommended exposure: %s", got)
	}
	if got := ids(base + `{"modelRegistry":{"enabled":true}}}`); got != "rhoai dsc project prerequisites model-registry dashboard-dev" {
		t.Errorf("registry: %s", got)
	}
	// Kueue's operators join the prerequisites step; its component stays in
	// kueue-prereq, late.
	if got := ids(base + `{"pipelines":{"enabled":true},"kueue":{"enabled":true}}}`); got != "rhoai dsc project prerequisites s3 pipeline-server kueue-prereq kueue dashboard-dev" {
		t.Errorf("kueue: %s", got)
	}
	// Nothing needs prerequisites: no step.
	if got := ids(base + `{"workbench":{"enabled":true}}}`); got != "rhoai dsc project workbench dashboard-dev" {
		t.Errorf("workbench only: %s", got)
	}
}

// A tampered run record cannot choose what Resume installs: a pinned build
// that is not the recipe's refuses the plan (it is not kept), and the RHOAI
// step refuses it too.
func TestRecipePlan_TamperedPinnedBuildRefuses(t *testing.T) {
	f, c := projectFake(t)
	evil := &types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: "quay.io/attacker/catalog:rhoai-3.6@sha256:" + strings.Repeat("e", 64), Digest: "sha256:" + strings.Repeat("e", 64)}
	r, problems := recipe.Validate([]byte(tRecipeBase + `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"features":{}}}`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	plan, state := recipe.Plan(c, r, evil)
	if !strings.Contains(problemText(plan), "is not a build of quay.io/rhoai/rhoai-fbc-fragment") || state.Pinned == evil || plan.Pinned == evil {
		t.Fatalf("problems %s, pinned %+v", problemText(plan), state.Pinned)
	}
	if got := problemFixes(plan); !strings.HasPrefix(got, "pinned_build_refused@") {
		t.Errorf("fixes %s", got)
	}
	res, err := recipeStep(t, r, recipe.StepRHOAI).Run(c, &recipe.State{Recipe: r, Pinned: evil}, func(types.UpdateStepEvent) {})
	if err != nil || res.Success || res.ErrorCode != "validation" {
		t.Fatalf("run %+v %v", res, err)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("changed %v", m)
	}
}

// The run record's step figures and the plan's come from the same
// constants (Step.Seconds).
func TestRecipePlan_StepSecondsAreTheSteps(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	plan, state := planRecipeFor(t, c, `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}`)
	steps := recipe.Steps(state.Recipe, state.KueueFirst)
	if len(steps) != len(plan.Steps) {
		t.Fatalf("%d steps, plan %d", len(steps), len(plan.Steps))
	}
	for i, s := range steps {
		typical, most := s.Seconds()
		if p := plan.Steps[i]; p.TypicalSeconds != typical || p.MaxSeconds != most || typical <= 0 || most < typical {
			t.Errorf("%s: plan %d/%d, step %d/%d", s.ID, p.TypicalSeconds, p.MaxSeconds, typical, most)
		}
	}
}
