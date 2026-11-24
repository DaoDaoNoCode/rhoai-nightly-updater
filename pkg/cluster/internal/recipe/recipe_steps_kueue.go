package recipe

import (
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Kueue distributed workloads follow the Distributed workloads tab: first
// prerequisites (feature "kueue": the component, cert-manager and the Kueue
// operator), then this tool's queues in the recipe project, the project
// label, requested samples, and when requested the dashboard Kueue setting.
// These steps run after project setup for pipelines, models, and the
// workbench—Kueue only governs work submitted to a queue, so the rest stays
// active.
const (
	StepKueuePrereq = "kueue-prereq"
	StepKueue       = "kueue"
)

// Status and control hooks owned by the Distributed workloads tab; tests may
// stub them.
var (
	kueueStatus      = kueue.GetStatus
	runKueue         = kueue.RunAnnotated
	kueueRecord      = kueue.ReadRecord
	kueueCQMeta      = kueue.ReadClusterQueueMeta
	kueueProfileMeta = kueue.ReadProfileMeta
)

// kueueSteps lists the Kueue steps recipe r enables.
func kueueSteps(r types.EnvironmentRecipe) []Step {
	if !r.Spec.Features.Kueue.Enabled {
		return nil
	}
	return []Step{
		{ID: StepKueuePrereq, Title: "What distributed workloads need", Typical: 4 * time.Minute, Max: 15 * time.Minute, Check: checkKueuePrereq, Run: runKueuePrereq},
		{ID: StepKueue, Title: "Distributed workloads", Typical: time.Minute, Max: 10 * time.Minute, Check: checkKueue, Run: runKueueStep},
	}
}

func checkKueuePrereq(c *kube.Client, _ *State) (Check, error) {
	return checkPrereqFor(c, "kueue", "")
}

// runKueuePrereq performs Set up what distributed workloads need.
func runKueuePrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPrereqFor(c, s, StepKueuePrereq, "kueue", "", emit)
}

// kueueRequest builds the tab's Set up payload for the recipe.
func kueueRequest(s *State) types.KueueRequest {
	k := s.Recipe.Spec.Features.Kueue
	return types.KueueRequest{Action: "setup", Project: projectOf(s), RayCluster: k.RayCluster, RayJob: k.RayJob, RayImage: k.RayImage}
}

// checkKueue: this tool's queues for the project, the project label, the
// samples asked for and (asked for) the dashboard's setting in effect are
// there → done, and a setup that proves this run created it is recorded as
// the run's. What the tab refuses blocks (fail closed): a setup in another
// project, a LocalQueue named default, the Kueue API, OLM, labelPolicy or
// KueueReady not as RHOAI documents them, objects the tool did not create,
// and samples asked for while the ray component is off (the recipe does
// not turn it on).
func checkKueue(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	k := s.Recipe.Spec.Features.Kueue
	st, err := kueueStatus(c, c, project)
	if err != nil {
		return Check{}, fmt.Errorf("read distributed workloads in %s: %w", project, err)
	}
	if why := kueueBlocked(st, project); why != "" {
		return blockedAt(why), nil
	}
	if st.Blocked != nil {
		return Check{}, nil // its prerequisites' step turns it on
	}
	if (k.RayCluster || k.RayJob) && st.RayBlocked != nil {
		return Check{Blocked: fmt.Sprintf("The samples need the DataScienceCluster's ray component, which is off (%s). The recipe does not turn it on: turn ray on on the Components page, or leave the samples out.", strings.TrimSuffix(st.RayBlocked.Reason, ".")), Fix: fixAt(types.RecipeFixFeatureBlocked, pageComponents)}, nil
	}
	if why := kueueOutdated(st, k, project); why != "" {
		return blockedAt(why), nil
	}
	if !kueueSetUp(st, k) {
		return Check{}, nil
	}
	_ = markKueueCreated(c, s) // a read that fails marks nothing
	return Check{Done: true, Reason: fmt.Sprintf("Distributed workloads are in %s.", project)}, nil
}

// kueueOutdated says why the recipe does not count this tool's queue or a
// sample it asks for as set up: an earlier version made it with other
// sizes, and Set up keeps what is there. "" when none is.
func kueueOutdated(st *types.KueueStatus, k types.RecipeKueue, project string) string {
	what := ""
	for _, o := range st.Objects {
		if o.Present && o.ManagedByTool && o.Outdated != "" {
			what = fmt.Sprintf("%s %s (%s)", o.Kind, o.Name, o.Outdated)
		}
	}
	want := map[string]bool{kueue.SampleRayCluster: k.RayCluster, kueue.SampleRayJob: k.RayJob}
	for _, sm := range st.Samples {
		if what == "" && want[sm.Kind] && sm.Present && sm.ManagedByTool && sm.Outdated != "" {
			what = fmt.Sprintf("the sample %s (%s)", sm.Name, sm.Outdated)
		}
	}
	if what == "" {
		return ""
	}
	return fmt.Sprintf("An earlier version of this tool made %s in %s, and Set up keeps what is there, so the recipe does not count it as set up. Clean up on the Distributed workloads tab, then Resume. Nothing was changed.", what, project)
}

// kueueBlocked returns why Set up on the tab rejects the project, or "".
func kueueBlocked(st *types.KueueStatus, project string) string {
	switch {
	case st.SetUpIn != "" && st.SetUpIn != project:
		return fmt.Sprintf("This tool's distributed workloads are set up in project %s; it sets them up in one project only. Clean them up there first. Nothing was changed.", st.SetUpIn)
	case st.DefaultQueue:
		return fmt.Sprintf("Project %s has a LocalQueue named default: once the project is labelled, Kueue would queue every new Deployment, StatefulSet and Pod in it there. Choose another project. Nothing was changed.", project)
	case st.Blocked == nil && st.Refusal != "":
		return st.Refusal
	}
	for _, o := range st.Objects {
		if o.Foreign {
			return fmt.Sprintf("%s %s exists and was not created by this tool, so the recipe does not use or change it. Nothing was changed.", o.Kind, o.Name)
		}
	}
	for _, sm := range st.Samples {
		if sm.Foreign {
			return fmt.Sprintf("%s %s exists in %s and was not created by this tool. Nothing was changed.", sm.Kind, sm.Name, project)
		}
	}
	return ""
}

// kueueSetUp reports whether the status shows everything k asks for. The
// dashboard's setting counts only when it is in effect (optInLive), never
// from the tool's record alone.
func kueueSetUp(st *types.KueueStatus, k types.RecipeKueue) bool {
	if len(st.Objects) == 0 || st.Label == nil || !st.Label.Present {
		return false
	}
	for _, o := range st.Objects {
		if !o.Present || !o.ManagedByTool || o.Terminating {
			return false
		}
	}
	want := map[string]bool{kueue.SampleRayCluster: k.RayCluster, kueue.SampleRayJob: k.RayJob}
	for _, sm := range st.Samples {
		if want[sm.Kind] && (!sm.Present || !sm.ManagedByTool || sm.Terminating) {
			return false
		}
		delete(want, sm.Kind)
	}
	for _, asked := range want {
		if asked {
			return false
		}
	}
	return !k.DashboardOptIn || optInLive(st)
}

// optInLive reports whether the dashboard's Kueue setting is in effect as
// the tab's Turn on makes it: disableKueue false on the live dashboard
// settings, and the Queue hardware profile there and this tool's.
func optInLive(st *types.KueueStatus) bool {
	o := st.OptIn
	return o.DisableKueue != nil && !*o.DisableKueue && o.Profile.Present && o.Profile.ManagedByTool && !o.Profile.Terminating
}

// runKueueStep is the Distributed workloads tab's Set up in the project
// (with the samples asked for), then its Turn on for the dashboard's
// setting when the recipe asks for it and it is not in effect (Turn on
// handles a switch that is off and a profile that is missing). The
// cluster-wide objects it creates (the ResourceFlavor, the ClusterQueue,
// the Queue hardware profile) carry the run's annotation from their
// creation; the run records the setup as its own when its ClusterQueue
// proves it (also on a resume after a crash between create and record).
func runKueueStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	before, err := kueueStatus(c, c, project)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read distributed workloads in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	ann := runAnnotations(s)
	res, err := runKueue(c, kueueRequest(s), ann, emit)
	if err == nil && res != nil && res.Success && s.Recipe.Spec.Features.Kueue.DashboardOptIn && !optInLive(before) {
		opt, optErr := runKueue(c, types.KueueRequest{Action: "optin"}, ann, emit)
		if optErr != nil || opt == nil || !opt.Success {
			res, err = opt, optErr
		} else {
			res.Message = strings.TrimSpace(res.Message + " " + opt.Message)
			res.Logs = append(res.Logs, opt.Logs...)
		}
	}
	rc, cancel := readWithin(c)
	defer cancel()
	if markErr := markKueueCreated(rc, s); markErr != nil {
		noteUnrecorded(StepKueue, "the distributed workloads setup", markErr, res, emit)
	}
	return res, err
}

// markKueueCreated records the distributed workloads setup as the run's
// when its ClusterQueue proves the run created it: the run's annotation
// from its creation, and the tool's record owns it by its UID. It reads
// only; a read that fails is returned and marks nothing.
func markKueueCreated(c *kube.Client, s *State) error {
	if s.RunID == "" {
		return nil
	}
	meta, found, err := kueueCQMeta(c)
	if err != nil || !found || meta.Annotations[RecipeRunAnnotation] != s.RunID {
		return err
	}
	rec, err := kueueRecord(c)
	if err != nil {
		return err
	}
	if kueue.Owns(meta, rec.ClusterQueueUID) {
		s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartKueue, UID: meta.UID})
	}
	return nil
}

// kueueSettingTurnedOn reports whether the tool's record says its opt-in
// set the dashboard's Kueue setting (which the teardown turns back), for
// the words of a Clean up whose setup proves it is this run's (with the
// run's Queue hardware profile: kueuePartProblem).
func kueueSettingTurnedOn(sa *kube.Client) bool {
	rec, err := kueueRecord(sa)
	return err == nil && rec.OptIn != nil && rec.OptIn.DisableKueueSet
}

// kueuePartProblem is CreatedPartProblem for the distributed workloads
// setup: its ClusterQueue carries the run's annotation, has uid and is the
// tool's (by its record), and, when this tool's dashboard opt-in is
// recorded, that was this run's too: its Queue hardware profile (in the
// namespace the opt-in recorded) carries the run's annotation and the
// recorded UID. The teardown turns the opt-in back with the rest, so an
// opt-in made outside the run, or one whose profile is missing, keeps the
// setup from being offered.
func kueuePartProblem(sa, u *kube.Client, runID, uid string, notIt func(string) string) string {
	if u == nil {
		return "Distributed workloads are read with your own sign-in, which this request does not have."
	}
	rec, err := kueueRecord(sa)
	if err != nil {
		return "Cannot read this tool's record of its distributed workloads: " + err.Error()
	}
	meta, found, err := kueueCQMeta(u)
	switch {
	case err != nil:
		return "Cannot read the ClusterQueue: " + err.Error()
	case !found || meta.UID != uid || meta.Annotations[RecipeRunAnnotation] != runID || meta.Terminating() || !kueue.Owns(meta, rec.ClusterQueueUID):
		return notIt("The distributed workloads setup")
	}
	o := rec.OptIn
	if o == nil || (!o.DisableKueueSet && o.ProfileUID == "") {
		return ""
	}
	const tabNote = " The recipe's Clean up does not remove distributed workloads, since that would turn the setting back too; use the Distributed workloads tab."
	if o.ProfileUID == "" {
		return "This tool turned the dashboard's Kueue setting on, but its Queue hardware profile was never created, so nothing shows that this run did it." + tabNote
	}
	ns := kube.FirstNonEmpty(o.ProfileNamespace, o.ConfigNamespace)
	pm, found, err := kueueProfileMeta(u, ns)
	switch {
	case err != nil:
		return "Cannot read the Queue hardware profile: " + err.Error()
	case !found:
		return fmt.Sprintf("This tool's Queue hardware profile is gone from %s, so nothing shows that this run turned the dashboard's Kueue setting on.", ns) + tabNote
	case pm.UID != o.ProfileUID || pm.Annotations[RecipeRunAnnotation] != runID || !kueue.Owns(pm, o.ProfileUID):
		return "The dashboard's Kueue setting was turned on outside this run." + tabNote
	}
	return ""
}

// kueueWarnings are the plan's notes for distributed workloads, from what
// the recipe turns on: advice when the same project also gets other
// setups (Kueue manages only what is sent to a queue, so they keep
// running, as the tab says), and the dashboard setting's cluster-wide
// change, only when the recipe turns it on and it is not on already
// (optInOn).
func kueueWarnings(r types.EnvironmentRecipe, optInOn bool) []types.RecipeWarning {
	f := r.Spec.Features
	if !f.Kueue.Enabled {
		return nil
	}
	var out []types.RecipeWarning
	var others []string
	if f.Pipelines.Enabled {
		others = append(others, "a pipeline server")
	}
	if f.Models.Enabled {
		others = append(others, "a model")
	}
	if f.Models.Enabled && f.Playground.Enabled {
		others = append(others, "a playground")
	}
	if f.MLflow.Enabled && f.MLflow.Samples {
		others = append(others, "the MLflow samples")
	}
	if f.Workbench.Enabled {
		others = append(others, "a workbench")
	}
	if len(others) > 0 {
		out = append(out, types.RecipeWarning{Kind: types.RecipeWarningAdvice, Message: fmt.Sprintf("Project %s also gets %s. Kueue manages only what is sent to a queue, so they keep running. A project of its own is still safer.", projectName(r.Spec.Project), kube.JoinTitles(others))})
	}
	if f.Kueue.DashboardOptIn && !optInOn {
		out = append(out, types.RecipeWarning{Kind: types.RecipeWarningCluster, Message: "The dashboard's Kueue setting is turned on: in every new project on this cluster, workbenches and model deployments can only use the queue hardware profile, and a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one."})
	}
	return out
}

// planKueueWarnings are kueueWarnings with whether the tool's opt-in is in
// effect now, read in the recipe's project (a read that fails counts as
// not in effect: the warning shows).
func planKueueWarnings(c *kube.Client, r types.EnvironmentRecipe) []types.RecipeWarning {
	on := false
	if k := r.Spec.Features.Kueue; k.Enabled && k.DashboardOptIn && r.Spec.Project != nil {
		if st, err := kueueStatus(c, c, r.Spec.Project.Name); err == nil {
			on = optInLive(st)
		}
	}
	return kueueWarnings(r, on)
}
