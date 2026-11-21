package recipe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/node"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// stepDependsOn names, per step, the earlier steps whose outcome its
// check depends on: while one of them will still run, the step is checked
// when the run gets there (types.RecipeStepLater).
var stepDependsOn = map[string][]string{
	StepDSC:             {StepRHOAI},
	StepPipelinesPrereq: {StepRHOAI, StepDSC},
	StepPipelineServer:  {StepRHOAI, StepDSC, StepProject, StepPipelinesPrereq, StepS3},
	StepMLflowPrereq:    {StepRHOAI, StepDSC},
	StepMLflow:          {StepRHOAI, StepDSC, StepMLflowPrereq},
}

// buildResolveTimeout bounds resolving the recipe's build on Quay.
const buildResolveTimeout = 30 * time.Second

// Plan checks a validated recipe against the cluster, read-only: what each
// step would do (run, skip because it is set up, blocked, or checked
// later), and every reason the run would refuse to start (Problems):
//
//   - a part of the schema this version does not run yet (models, the
//     playground, MaaS, the MLflow samples, Dashboard Dev);
//   - Argo CD or an ACM policy manages the RHOAI operator;
//   - the RHOAI step would run without the one-time cluster setup (the
//     pull secret and the image mirror), or its build cannot be resolved;
//   - a step is blocked (a project that is not the recipe's, a half
//     installed operator, ...).
//
// A read that fails is a problem too (fail closed). pinned, when given (a
// resume), is the build to install instead of resolving the recipe's
// again. The State is the run's start: the recipe, the pinned build, and
// the project as found.
func Plan(c *kube.Client, r types.EnvironmentRecipe, pinned *types.RecipePinnedBuild) (*types.RecipePlan, State) {
	plan := &types.RecipePlan{Recipe: r, Steps: []types.RecipePlanStep{}, Problems: []types.RecipeProblem{}}
	var p problems
	notInThisVersion(&p, r)

	if g, err := argo.ReadOperatorGitOps(c); err != nil {
		p.add("", "Cannot check whether Argo CD manages the RHOAI operator: %v. Nothing was changed.", err)
	} else if g.Active() {
		p.add("", "%s manages the RHOAI operator on this cluster, so a recipe does not install or change it. Nothing was changed.", gitOpsManager(g))
	}

	s := State{Recipe: r, Pinned: pinned}
	resolved := false
	resolve := func() {
		if s.Pinned != nil || resolved {
			return
		}
		resolved = true
		if b, problem := resolveBuild(c, r.Spec.RHOAI.Build); problem != "" {
			p.add("spec.rhoai.build", "%s", problem)
		} else {
			s.Pinned = b
		}
	}
	// An update compares the installed build with the recipe's.
	if r.Spec.RHOAI.IfInstalled == IfInstalledUpdate {
		resolve()
	}
	willRun := map[string]bool{}
	for _, step := range Steps(r) {
		ps := types.RecipePlanStep{ID: step.ID, Title: step.Title, TypicalSeconds: int(step.Typical / time.Second), MaxSeconds: int(step.Max / time.Second)}
		later := false
		for _, dep := range stepDependsOn[step.ID] {
			later = later || willRun[dep]
		}
		if later {
			ps.Action, ps.Reason = types.RecipeStepLater, "Checked when the run gets here: it depends on a step before it."
			willRun[step.ID] = true
		} else {
			chk, err := step.Check(c, &s)
			switch {
			case err != nil:
				ps.Action, ps.Reason = types.RecipeStepBlocked, fmt.Sprintf("Cannot check it: %v.", err)
			case chk.Blocked != "":
				ps.Action, ps.Reason = types.RecipeStepBlocked, chk.Blocked
			case chk.Done:
				ps.Action, ps.Reason = types.RecipeStepSkip, chk.Reason
			default:
				ps.Action = types.RecipeStepRun
				willRun[step.ID] = true
			}
		}
		if ps.Action == types.RecipeStepBlocked {
			p.add("", "%s: %s", step.Title, ps.Reason)
		}
		if step.ID == StepRHOAI && willRun[StepRHOAI] {
			oneTimeSetup(c, &p)
			resolve()
		}
		if ps.Action == types.RecipeStepRun || ps.Action == types.RecipeStepLater {
			plan.TypicalSeconds += ps.TypicalSeconds
			plan.MaxSeconds += ps.MaxSeconds
		}
		plan.Steps = append(plan.Steps, ps)
	}
	plan.Pinned = s.Pinned
	plan.Problems = append(plan.Problems, p...)
	return plan, s
}

// notInThisVersion refuses the parts of the schema this version of the
// updater does not run yet.
func notInThisVersion(p *problems, r types.EnvironmentRecipe) {
	f := r.Spec.Features
	for _, part := range []struct {
		on    bool
		field string
		what  string
	}{
		{f.Models.Enabled, "spec.features.models.enabled", "Models"},
		{f.Playground.Enabled, "spec.features.playground.enabled", "The playground"},
		{f.MaaS.Enabled, "spec.features.maas.enabled", "Models as a Service"},
		{f.MLflow.Samples, "spec.features.mlflow.samples", "The MLflow samples"},
		{r.Spec.DashboardDev.Deploy != DashboardDevNone, "spec.dashboardDev.deploy", "Dashboard Dev"},
	} {
		if part.on {
			p.add(part.field, "%s in a recipe is not in this version of the updater; turn it off, or set it up on its own page after the run.", part.what)
		}
	}
}

// gitOpsManager names what manages the operator.
func gitOpsManager(g argo.OperatorGitOps) string {
	if app := kube.NonEmpty(g.SubscriptionApp, g.CatalogApp); app != "" {
		return fmt.Sprintf("Argo CD application %q", app)
	}
	return "An ACM policy"
}

// oneTimeSetup reports the one-time cluster setup Update needs (the pull
// secret, the image mirror) when it is missing. A recipe holds no
// credential, so it does not set them up.
func oneTimeSetup(c *kube.Client, p *problems) {
	ps, err := registry.PullSecret(c)
	switch {
	case err != nil:
		p.add("", "Cannot check the pull secret: %v. Nothing was changed.", err)
	case !ps.Exists:
		p.add("", "The one-time cluster setup is not done: additional-pull-secret is missing in kube-system. Run the one-time setup on the Status page first.")
	case !ps.Valid && !strings.HasPrefix(ps.Detail, "Could not verify credentials with Quay"):
		p.add("", "The pull secret cannot pull from quay.io/rhoai: %s. Fix it in the one-time setup first.", ps.Detail)
	}
	idms, err := node.ImageMirror(c)
	switch {
	case err != nil:
		p.add("", "Cannot check the image mirror: %v. Nothing was changed.", err)
	case !idms.Exists:
		p.add("", "The one-time cluster setup is not done: no image mirror (IDMS) for %s. Run the one-time setup on the Status page first.", rhoai.IDMSSource)
	}
}

// resolveBuild pins the recipe's build: the newest nightly (latest), the
// newest build of a line (tag), or the given image, by digest. It returns
// why it cannot.
func resolveBuild(c *kube.Client, b types.RecipeBuild) (*types.RecipePinnedBuild, string) {
	if b.Image != "" {
		_, digest, _ := strings.Cut(b.Image, "@")
		return &types.RecipePinnedBuild{Tag: registry.TagFromRef(b.Image), Image: b.Image, Digest: digest}, ""
	}
	ctx, cancel := context.WithTimeout(c.Context(), buildResolveTimeout)
	defer cancel()
	var (
		got *types.LatestNightlyResponse
		err error
	)
	if b.Latest {
		got, err = registry.FetchLatestNightly(ctx, c)
	} else {
		got, err = registry.ResolveNightlyTag(ctx, c, b.Tag)
	}
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot resolve the build on Quay: %v. Nothing was changed.", err)
	case got.Digest == "":
		return nil, fmt.Sprintf("Cannot resolve the digest of %s on Quay, so the run could not pin it. Nothing was changed; try again.", got.Image)
	}
	return &types.RecipePinnedBuild{Tag: got.Tag, Image: got.Image, Digest: got.Digest}, ""
}

// RecordActivity records an activity entry of a recipe run (action
// "recipe-start" or "recipe").
func RecordActivity(c *kube.Client, action, detail string, success bool) {
	record.Action(c, action, detail, success)
}
