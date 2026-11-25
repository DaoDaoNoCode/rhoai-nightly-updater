package recipe

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Kueue distributed workloads follow the Distributed workloads tab: first
// prerequisites (feature "kueue": cert-manager and the Kueue operator, set
// up early by the prerequisites step with the other features', then the
// component, here), then this tool's queues in the recipe project, the project
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
		{ID: StepKueuePrereq, Title: "What distributed workloads need", Typical: 2 * time.Minute, Max: 15 * time.Minute, Check: checkKueuePrereq, Run: runKueuePrereq},
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

// kueueLive is what the plan's Kueue notes read on the cluster before the
// run: this tool's dashboard opt-in in effect (optInLive).
type kueueLive struct {
	OptInOn bool
}

// kueueLiveOf reads kueueLive from the Distributed workloads status.
func kueueLiveOf(st *types.KueueStatus) kueueLive {
	return kueueLive{OptInOn: optInLive(st)}
}

// kueueWarnings are the plan's notes for distributed workloads, from what
// the recipe turns on and what is on the cluster (live): advice when the
// same project also gets other setups (Kueue manages only what is sent to
// a queue, so they keep running, as the tab says), and the dashboard
// setting's cluster-wide change, only when the recipe turns it on and it
// is not on already (OptInOn). The recipe creates no workbench the
// dashboard marks "Workbench bypasses Kueue queue management": with the
// setting on when it is created, distributed workloads come first
// (KueueFirst) and it gets a Queue hardware profile.
func kueueWarnings(r types.EnvironmentRecipe, live kueueLive) []types.RecipeWarning {
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
	if f.Kueue.DashboardOptIn && !live.OptInOn {
		out = append(out, types.RecipeWarning{Kind: types.RecipeWarningCluster, Message: "The dashboard's Kueue setting is turned on: in every new project on this cluster, workbenches and model deployments can only use the queue hardware profile, and a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one."})
	}
	return out
}

// planKueueWarnings are kueueWarnings with what is on the cluster now, read
// in the recipe's project (a read that fails counts as nothing on: the
// warnings show).
func planKueueWarnings(c *kube.Client, r types.EnvironmentRecipe) []types.RecipeWarning {
	var live kueueLive
	if k := r.Spec.Features.Kueue; k.Enabled && k.DashboardOptIn && r.Spec.Project != nil {
		if st, err := kueueStatus(c, c, r.Spec.Project.Name); err == nil {
			live = kueueLiveOf(st)
		}
	}
	return kueueWarnings(r, live)
}

// dashboardKueueOn reads whether the dashboard's Kueue setting is on
// (kueueManaged: disableKueue false); tests may stub it.
var dashboardKueueOn = kueueManaged

// workloadKueueBlocks is why the plan refuses the workbench step and the
// model step (by step ID) before anything runs: when the run creates
// them, the dashboard will offer them no usable hardware profile, so the
// step would fail halfway. It applies the dashboard's Kueue rule
// (envkit.KueueModeFor) to the project as it will be then, and the same
// choosers the steps use (workbench.ChooseProfile,
// serving.ChooseModelProfile) to the profiles and LocalQueues as they
// will be then. What the run changes before the workloads is only:
//
//   - the label kueue.openshift.io/managed=true: the project step sets it
//     on a project it creates or completes while the dashboard's Kueue
//     setting is on (settingOn, projectStepRuns), and the Kueue step, which
//     runs before the workloads when s.KueueFirst;
//   - with s.KueueFirst, the dashboard's Kueue area (the setting, or the
//     recipe's opt-in, and the kueue component), the LocalQueue
//     nightly-kueue, and with the opt-in the tool's Queue hardware profile
//     when it is not there (Turn on keeps one that is);
//   - RHOAI's own LocalQueue in a labelled project when the
//     DataScienceCluster's kueue.autoCreateQueues is on.
//
// A workbench or model already there needs no profile. A read that fails
// is returned (the plan refuses: fail closed).
func workloadKueueBlocks(c *kube.Client, s *State, settingOn, projectStepRuns bool) (map[string]string, error) {
	r := s.Recipe
	f := r.Spec.Features
	out := map[string]string{}
	if r.Spec.Project == nil || (!f.Workbench.Enabled && !f.Models.Enabled) {
		return out, nil
	}
	project, exists := projectOf(s), s.Project != nil
	labelled := s.KueueFirst || (settingOn && projectStepRuns)
	if !labelled && exists {
		meta, found, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+project)
		if err != nil {
			return nil, fmt.Errorf("read project %s: %w", project, err)
		}
		labelled = found && meta.Labels[envkit.KueueProjectLabel] == "true"
	}
	if !labelled {
		return out, nil
	}
	var dsc rhoai.DSCRead
	dscRead := false
	readDSC := func() error {
		if dscRead {
			return nil
		}
		var err error
		if dsc, err = rhoai.ReadFirstDSC(c); err != nil {
			return fmt.Errorf("read the DataScienceCluster: %w", err)
		}
		dscRead = true
		return nil
	}
	area := s.KueueFirst
	if !area && settingOn {
		if err := readDSC(); err != nil {
			return nil, err
		}
		area = envkit.ComponentOn(dsc, "kueue")
	}
	mode := envkit.KueueModeFor(labelled, area)
	wb, model := f.Workbench.Enabled, f.Models.Enabled
	if exists && wb {
		there, err := workbench.NotebookThere(c, project)
		if err != nil {
			return nil, fmt.Errorf("read the workbench in %s: %w", project, err)
		}
		wb = !there
	}
	if exists && model {
		st, found, err := readModel(c, project, modelName(r))
		if err != nil {
			return nil, fmt.Errorf("read model %s in %s: %w", modelName(r), project, err)
		}
		model = !found || st.Terminating
	}
	if !wb && !model {
		return out, nil
	}
	block := func(why string) {
		why = strings.TrimSpace(why+" "+kueueProfileHint(r, mode)) + " Nothing was changed."
		if wb {
			out[StepWorkbench] = why
		}
		if model {
			out[StepModel] = why
		}
	}
	if mode == envkit.NoProfiles {
		block(fmt.Sprintf("Project %s has the label %s=true while Kueue is off in the dashboard (its Kueue setting, or the DataScienceCluster's kueue), so the dashboard offers no hardware profile there for a workbench or a model.", project, envkit.KueueProjectLabel))
		return out, nil
	}
	apps, err := rhoai.AppsNamespace(c)
	if err != nil {
		return nil, fmt.Errorf("find RHOAI's applications namespace: %w", err)
	}
	var queues []string
	if exists {
		if queues, err = workbench.ListLocalQueues(c, project); err != nil {
			return nil, fmt.Errorf("list the LocalQueues of %s: %w", project, err)
		}
	}
	if s.KueueFirst {
		queues = append(queues, kueue.LocalQueueName)
	}
	if err := readDSC(); err != nil {
		return nil, err
	}
	if q := kueue.AutoCreatedQueue(dsc); q != "" {
		queues = append(queues, q)
	}
	// The opt-in's Turn on creates the tool's Queue profile when it is
	// not there; this version's manifest stands for it.
	addTool := s.KueueFirst && f.Kueue.DashboardOptIn
	manifest, err := json.Marshal(kueue.ProfileManifest(apps, ""))
	if err != nil {
		return nil, err
	}
	where := fmt.Sprintf("Project %s uses Kueue when the run creates them (label %s=true, Kueue on in the dashboard), where the dashboard offers only a Queue hardware profile of a LocalQueue the project has (%s): ", project, envkit.KueueProjectLabel, kube.JoinTitles(queues))
	if len(queues) == 0 {
		where = fmt.Sprintf("Project %s uses Kueue when the run creates them (label %s=true, Kueue on in the dashboard) and has no LocalQueue, so the dashboard offers no hardware profile there: ", project, envkit.KueueProjectLabel)
	}
	if wb {
		global, err := workbench.ListProfiles(c, apps)
		if err != nil {
			return nil, fmt.Errorf("list the hardware profiles: %w", err)
		}
		var local []workbench.HardwareProfile
		if exists {
			if local, err = workbench.ListProfiles(c, project); err != nil {
				return nil, fmt.Errorf("list the hardware profiles of %s: %w", project, err)
			}
		}
		if addTool && !slices.ContainsFunc(global, func(p workbench.HardwareProfile) bool { return p.Metadata.Name == kueue.ProfileName }) {
			var p workbench.HardwareProfile
			if err := json.Unmarshal(manifest, &p); err != nil {
				return nil, err
			}
			global = append(global, p)
		}
		if _, why := workbench.ChooseProfile(global, local, mode, queues); why != "" {
			out[StepWorkbench] = strings.TrimSpace(where+why+" "+kueueProfileHint(r, mode)) + " Nothing was changed."
		}
	}
	if model {
		entry, _ := serving.CatalogEntry(f.Models.Model)
		items, err := serving.ListHardwareProfiles(c, apps)
		if err != nil {
			return nil, fmt.Errorf("list the hardware profiles: %w", err)
		}
		if addTool && !slices.ContainsFunc(items, func(p serving.HardwareProfileObject) bool { return p.Metadata.Name == kueue.ProfileName }) {
			var p serving.HardwareProfileObject
			if err := json.Unmarshal(manifest, &p); err != nil {
				return nil, err
			}
			items = append(items, p)
		}
		if _, why := serving.ChooseModelProfile(items, mode, queues, entry); why != "" {
			out[StepModel] = strings.TrimSpace(where+why+". "+kueueProfileHint(r, mode)) + " Nothing was changed."
		}
	}
	return out, nil
}

// kueueProfileHint is what recipe r can change when the dashboard offers
// its workloads no usable profile in a Kueue project (mode).
func kueueProfileHint(r types.EnvironmentRecipe, mode envkit.KueueMode) string {
	k := r.Spec.Features.Kueue
	switch {
	case mode == envkit.NoProfiles && k.Enabled:
		return "With spec.features.kueue.dashboardOptIn the run turns the setting on and sets up distributed workloads before the workbench and the model."
	case mode == envkit.NoProfiles:
		return "Turn the dashboard's Kueue setting on, or use another project."
	case !k.Enabled:
		return "Add distributed workloads (spec.features.kueue) with dashboardOptIn: the run then sets them up first, with the LocalQueue " + kueue.LocalQueueName + " and the hardware profile " + kueue.ProfileDisplayName + "."
	case !k.DashboardOptIn:
		return "With spec.features.kueue.dashboardOptIn the run also makes the hardware profile " + kueue.ProfileDisplayName + " for the LocalQueue " + kueue.LocalQueueName + "."
	}
	return "If it is this tool's hardware profile " + kueue.ProfileDisplayName + " from an earlier version, turn the dashboard's Kueue opt-in off on the Distributed workloads tab (it deletes the profile), then run the recipe again."
}
