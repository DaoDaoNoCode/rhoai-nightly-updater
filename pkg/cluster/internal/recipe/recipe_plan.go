package recipe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
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
	StepDSC:            {StepRHOAI},
	StepPrerequisites:  {StepRHOAI, StepDSC},
	StepPipelineServer: {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepS3},
	StepMLflow:         {StepRHOAI, StepDSC, StepPrerequisites},
	StepMLflowSamples:  {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepMLflow},
	// The prerequisites step may install Connectivity Link and its
	// Kuadrant CR, which the MaaS platform uses.
	StepMaaS: {StepRHOAI, StepDSC, StepPrerequisites},
	// How the model is reached is resolved in the project, and depends on
	// the MaaS platform.
	StepModelsPrereq:  {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepMaaS},
	StepModel:         {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepMaaS, StepModelsPrereq},
	StepMaaSPublish:   {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepMaaS, StepModelsPrereq, StepModel},
	StepPlayground:    {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepMaaS, StepModelsPrereq, StepModel, StepMaaSPublish},
	StepModelRegistry: {StepRHOAI, StepDSC, StepPrerequisites},
	// The workbench attaches the tool's S3 Connection when the project has
	// the tool's pipeline server.
	StepWorkbench:    {StepRHOAI, StepDSC, StepProject, StepPrerequisites, StepS3, StepPipelineServer},
	StepKueuePrereq:  {StepRHOAI, StepDSC, StepPrerequisites},
	StepKueue:        {StepRHOAI, StepDSC, StepProject, StepKueuePrereq},
	StepDashboardDev: {StepRHOAI, StepDSC},
}

// buildResolveTimeout bounds resolving the recipe's build on Quay.
const buildResolveTimeout = 30 * time.Second

// Plan checks a validated recipe against the cluster, read-only: what each
// step would do (run, skip because it is set up, blocked, or checked
// later), and every reason the run would refuse to start (Problems):
//
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

	if g, err := argo.ReadOperatorGitOps(c); err != nil {
		p.addFix(checkFailed(), "", "Cannot check whether Argo CD manages the RHOAI operator: %v. Nothing was changed.", err)
	} else if g.Active() {
		p.addFix(fixAt(types.RecipeFixArgoManaged, ""), "", "%s manages the RHOAI operator on this cluster, so a recipe does not install or change it. Nothing was changed.", gitOpsManager(g))
	}

	// A pinned build comes from the run record, which the ServiceAccount
	// can write: it is checked as strictly as a recipe's own build, and
	// a pinned build that is not the recipe's refuses (fail closed).
	if pinned != nil {
		if problem := PinnedBuildProblem(r.Spec.RHOAI.Build, pinned); problem != "" {
			p.addFix(fixAt(types.RecipeFixPinnedBuildRefused, ""), "spec.rhoai.build", "%s", problem)
			pinned = nil
		}
	}
	s := State{Recipe: r, Pinned: pinned}
	resolved := false
	resolve := func() {
		if s.Pinned != nil || resolved {
			return
		}
		resolved = true
		if b, problem := resolveBuild(c, r.Spec.RHOAI.Build); problem != "" {
			p.addFix(fixAt(types.RecipeFixBuildUnresolvable, ""), "spec.rhoai.build", "%s", problem)
		} else {
			s.Pinned = b
		}
	}
	// An update compares the installed build with the recipe's.
	if r.Spec.RHOAI.IfInstalled == IfInstalledUpdate {
		resolve()
	}
	// The dashboard's Kueue setting decides the run order (KueueFirst) and
	// which hardware profiles the dashboard offers the workloads.
	f := r.Spec.Features
	settingKnown, settingOn := false, false
	if (f.Kueue.Enabled && !f.Kueue.DashboardOptIn) || f.Workbench.Enabled || f.Models.Enabled {
		on, err := dashboardKueueOn(c)
		if err != nil {
			p.addFix(checkFailed(), "", "Cannot read the dashboard's Kueue setting, which decides whether distributed workloads come before the workbench and the model, and which hardware profiles the dashboard offers them: %v. Nothing was changed.", err)
		} else {
			settingKnown, settingOn = true, on
		}
	}
	s.KueueFirst = KueueFirst(r, settingOn)
	var kueueBlocks map[string]string
	kueueChecked := false
	willRun := map[string]bool{}
	steps := Steps(r, s.KueueFirst)
	typical, most := make([]int, len(steps)), make([]int, len(steps))
	for i, step := range steps {
		ps := types.RecipePlanStep{ID: step.ID, Title: step.Title}
		ps.TypicalSeconds, ps.MaxSeconds = step.Seconds()
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
				ps.Action, ps.Reason, ps.Fix = types.RecipeStepBlocked, fmt.Sprintf("Cannot check it: %v.", err), checkFailed()
			case chk.Blocked != "":
				ps.Action, ps.Reason, ps.Fix = types.RecipeStepBlocked, chk.Blocked, chk.Fix
			case chk.Done:
				ps.Action, ps.Reason = types.RecipeStepSkip, chk.Reason
			default:
				ps.Action = types.RecipeStepRun
				willRun[step.ID] = true
			}
		}
		// RHOAI's auto-created LocalQueue default (K4): the Kueue step
		// would refuse once the project is labelled.
		if step.ID == StepKueue && (ps.Action == types.RecipeStepRun || ps.Action == types.RecipeStepLater) {
			if dsc, err := rhoai.ReadFirstDSC(c); err != nil {
				ps.Action, ps.Reason, ps.Fix = types.RecipeStepBlocked, fmt.Sprintf("Cannot check it: read the DataScienceCluster: %v.", err), checkFailed()
			} else if kueue.AutoCreatedQueue(dsc) == "default" {
				ps.Action, ps.Reason, ps.Fix = types.RecipeStepBlocked, kueue.AutoQueuesRefusal, fixAt(types.RecipeFixFeatureBlocked, pageComponents)
			}
		}
		// A workbench or model the run creates where the dashboard will
		// offer it no usable hardware profile (workloadKueueBlocks).
		if (step.ID == StepWorkbench || step.ID == StepModel) && settingKnown && (ps.Action == types.RecipeStepRun || ps.Action == types.RecipeStepLater) {
			if !kueueChecked {
				kueueChecked = true
				var err error
				if kueueBlocks, err = workloadKueueBlocks(c, &s, settingOn, willRun[StepProject]); err != nil {
					p.addFix(checkFailed(), "", "Cannot check which hardware profiles the dashboard will offer the workbench and the model: %v. Nothing was changed.", err)
				}
			}
			if why := kueueBlocks[step.ID]; why != "" {
				ps.Action, ps.Reason, ps.Fix = types.RecipeStepBlocked, why, fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)
			}
		}
		if ps.Action == types.RecipeStepBlocked {
			p.addFix(ps.Fix, "", "%s: %s", step.Title, ps.Reason)
		}
		if step.ID == StepRHOAI && willRun[StepRHOAI] {
			oneTimeSetup(c, &p)
			resolve()
		}
		if ps.Action == types.RecipeStepRun || ps.Action == types.RecipeStepLater {
			typical[i], most[i] = ps.TypicalSeconds, ps.MaxSeconds
		}
		plan.Steps = append(plan.Steps, ps)
	}
	// Steps that do not wait for each other run side by side: the run
	// takes about as long as its longest chain of steps.
	plan.TypicalSeconds, plan.MaxSeconds = RunSeconds(steps, typical), RunSeconds(steps, most)
	plan.Pinned = s.Pinned
	plan.Warnings = append(planKueueWarnings(c, r), playgroundWarnings(r)...)
	plan.Problems = append(plan.Problems, p...)
	return plan, s
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
		p.addFix(checkFailed(), "", "Cannot check the pull secret: %v. Nothing was changed.", err)
	case !ps.Exists:
		p.addFix(oneTimeSetupFix(), "", "The one-time cluster setup is not done: additional-pull-secret is missing in kube-system. Run the one-time setup on the Status page first.")
	case !ps.Valid && !strings.HasPrefix(ps.Detail, "Could not verify credentials with Quay"):
		p.addFix(oneTimeSetupFix(), "", "The pull secret cannot pull from quay.io/rhoai: %s. Fix it in the one-time setup first.", ps.Detail)
	}
	idms, err := node.ImageMirror(c)
	switch {
	case err != nil:
		p.addFix(checkFailed(), "", "Cannot check the image mirror: %v. Nothing was changed.", err)
	case !idms.Exists:
		p.addFix(oneTimeSetupFix(), "", "The one-time cluster setup is not done: no image mirror (IDMS) for %s. Run the one-time setup on the Status page first.", rhoai.IDMSSource)
	}
}

func oneTimeSetupFix() *types.RecipeFix {
	return fixAt(types.RecipeFixOneTimeSetupMissing, pageStatus)
}

// checkFailed: a read of the cluster failed; trying again may work.
func checkFailed() *types.RecipeFix { return fixAt(types.RecipeFixCheckFailed, "") }

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

// PinnedBuildProblem says why pinned is not a build the recipe's build
// choice b may install, "" when it is: a digest-pinned image of the RHOAI
// catalog repository (the same rule as a recipe's image), whose digest is
// the pinned digest, and that is b itself (an image), the line b names (a
// tag), or a nightly line (latest). The run record that keeps a pinned
// build for Resume is the updater's own ConfigMap, which a leaked
// ServiceAccount token could write; this keeps such a write from choosing
// what the user's Resume installs from anywhere else.
func PinnedBuildProblem(b types.RecipeBuild, pinned *types.RecipePinnedBuild) string {
	refuse := func(why string) string {
		return "The build recorded for this run (" + pinned.Image + ") " + why + ", so it is not installed. Nothing was changed; resume with the build resolved again, or run the recipe again."
	}
	if !fbcImage.MatchString(pinned.Image) {
		return refuse("is not a build of " + registry.FBCImage + " pinned by digest")
	}
	_, digest, _ := strings.Cut(pinned.Image, "@")
	if digest != pinned.Digest {
		return refuse("does not carry the recorded digest " + pinned.Digest)
	}
	tag := registry.TagFromRef(pinned.Image)
	switch {
	case b.Image != "":
		if pinned.Image != b.Image {
			return refuse("is not the recipe's image " + b.Image)
		}
	case b.Tag != "":
		if tag != b.Tag || pinned.Tag != b.Tag {
			return refuse("is not of the recipe's line " + b.Tag)
		}
	default:
		if !registry.CleanTagRegex.MatchString(tag) || pinned.Tag != tag {
			return refuse("is not a nightly line's build")
		}
	}
	return ""
}

// RecordActivity records an activity entry of a recipe run (action
// "recipe-start" or "recipe").
func RecordActivity(c *kube.Client, action, detail string, success bool) {
	record.Action(c, action, detail, success)
}
