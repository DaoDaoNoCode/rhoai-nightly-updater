package cluster

import (
	"strings"
	"testing"

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
	if got := planActions(plan); got != "rhoai=run dsc=later project=run pipelines-prereq=later s3=run pipeline-server=later mlflow-prereq=later mlflow=later" {
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

// Argo CD managing the operator refuses, and so does every part of the
// schema this version does not run yet.
func TestRecipePlan_ArgoCDAndLaterFeaturesRefuse(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.argo = argoManagingAll(argo.SubscriptionObject)
	plan, _ := planRecipeFor(t, c, `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"models":{"enabled":true},"playground":{"enabled":true},"mlflow":{"enabled":true,"samples":true}},"dashboardDev":{"deploy":"main"}}`)
	text := problemText(plan)
	for _, want := range []string{"Models in a recipe is not in this version", "The playground in a recipe", "The MLflow samples in a recipe", "Dashboard Dev in a recipe"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in %s", want, text)
		}
	}
	if !strings.Contains(text, "Argo CD application") || !strings.Contains(text, "manages the RHOAI operator") {
		t.Errorf("Argo CD's management did not refuse: %s", text)
	}
	// The parts of a later version are fixed in the recipe (Field), Argo
	// CD in Git.
	if got := problemFixes(plan); got != "- - - - argo_managed@" {
		t.Errorf("fixes %s", got)
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
	steps := recipe.Steps(state.Recipe)
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
