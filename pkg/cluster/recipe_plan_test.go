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
}
