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

// checkKueue: project queues from this tool, the project label, requested
// samples, and when requested the dashboard setting must all be present →
// done. Fail closed on tab refusals: setup bound to another project, a
// LocalQueue named default, the Kueue API, OLM, labelPolicy or KueueReady
// diverging from RHOAI docs, or objects this tool did not create.
func checkKueue(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	st, err := kueueStatus(c, c, project)
	if err != nil {
		return Check{}, fmt.Errorf("read distributed workloads in %s: %w", project, err)
	}
	if why := kueueBlocked(st, project); why != "" {
		return blockedAt(why), nil
	}
	if st.Blocked != nil {
		return Check{}, nil // prerequisite step must clear blocked first
	}
	if !kueueSetUp(st, s.Recipe.Spec.Features.Kueue) {
		return Check{}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Distributed workloads are set up in %s.", project)}, nil
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

// kueueSetUp returns true when status reflects everything k requests.
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
	return !k.DashboardOptIn || st.OptIn.On
}

// runKueueStep runs the Distributed workloads tab Set up in the project
// (including requested samples), then Turn on for the dashboard setting
// when the recipe requests it. Cluster-scoped objects it creates (ResourceFlavor,
// ClusterQueue, Queue hardware profile) get the run annotation at creation; a
// ClusterQueue it created is tied to the run by UID.
func runKueueStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	before, err := kueueStatus(c, c, project)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read distributed workloads in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	var annotations map[string]string
	if s.RunID != "" {
		annotations = map[string]string{RecipeRunAnnotation: s.RunID}
	}
	cqBefore := kueueObjectPresent(before, "ClusterQueue")
	res, err := runKueue(c, kueueRequest(s), annotations, emit)
	if !cqBefore {
		markKueueCreated(c, s, res, emit)
	}
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	if s.Recipe.Spec.Features.Kueue.DashboardOptIn && !before.OptIn.On {
		opt, err := runKueue(c, types.KueueRequest{Action: "optin"}, annotations, emit)
		if err != nil || opt == nil || !opt.Success {
			return opt, err
		}
		res.Message = strings.TrimSpace(res.Message + " " + opt.Message)
		res.Logs = append(res.Logs, opt.Logs...)
	}
	return res, nil
}

func kueueObjectPresent(st *types.KueueStatus, kind string) bool {
	for _, o := range st.Objects {
		if o.Kind == kind && o.Present {
			return true
		}
	}
	return false
}

// markKueueCreated registers the ClusterQueue created by this run's setup
// (run annotation and tool ownership per its record), or reports in the step
// result when registration fails (so Clean up will not offer it).
func markKueueCreated(c *kube.Client, s *State, res *types.OperationResponse, emit func(types.UpdateStepEvent)) {
	rc, cancel := readWithin(c)
	defer cancel()
	meta, found, err := kueueCQMeta(rc)
	if err != nil {
		noteUnrecorded(StepKueue, "the distributed workloads setup", err, res, emit)
		return
	}
	rec, err := kueueRecord(rc)
	if err != nil {
		noteUnrecorded(StepKueue, "the distributed workloads setup", err, res, emit)
		return
	}
	if found && s.RunID != "" && meta.Annotations[RecipeRunAnnotation] == s.RunID && kueue.Owns(meta, rec.ClusterQueueUID) {
		s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartKueue, UID: meta.UID})
	}
}

// kueuePartProblem implements CreatedPartProblem for distributed workloads
// setup: ClusterQueue must carry the run annotation, match uid, and belong
// to the tool (per record); when dashboard opt-in from this tool is active,
// the same run must have applied it (Queue hardware profile carries the run
// annotation and recorded UID)—teardown reverts it with the rest, so opt-in
// outside the run prevents offering the part.
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
	if o := rec.OptIn; o != nil && (o.DisableKueueSet || o.ProfileUID != "") {
		pm, found, err := kueueProfileMeta(u, o.ProfileNamespace)
		switch {
		case err != nil:
			return "Cannot read the Queue hardware profile: " + err.Error()
		case !found || pm.UID != o.ProfileUID || pm.Annotations[RecipeRunAnnotation] != runID || !kueue.Owns(pm, o.ProfileUID):
			return "The dashboard's Kueue setting was turned on outside this run, and removing distributed workloads turns it back too, so the recipe's Clean up does not remove them. Use the Distributed workloads tab."
		}
	}
	return ""
}

// kueueWarnings collects plan warnings for distributed workloads: other
// recipe features in the same project (Kueue only queues submitted work,
// so those keep running, as the tab notes), and cluster-wide impact of the
// dashboard setting.
func kueueWarnings(r types.EnvironmentRecipe) []string {
	f := r.Spec.Features
	if !f.Kueue.Enabled {
		return nil
	}
	var out []string
	var others []string
	if f.Pipelines.Enabled {
		others = append(others, "a pipeline server")
	}
	if f.Models.Enabled {
		others = append(others, "a model")
	}
	if f.Workbench.Enabled {
		others = append(others, "a workbench")
	}
	if len(others) > 0 {
		out = append(out, fmt.Sprintf("Project %s also gets %s. Kueue manages only what is sent to a queue, so they keep running. A project of its own is still safer: with the dashboard's Kueue setting on, new workbenches and models there need the queue hardware profile.", projectName(r.Spec.Project), kube.JoinTitles(others)))
	}
	if f.Kueue.DashboardOptIn {
		out = append(out, "The dashboard's Kueue setting changes every new project on this cluster: workbenches and model deployments there can only use the queue hardware profile, and a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one.")
	}
	return out
}
