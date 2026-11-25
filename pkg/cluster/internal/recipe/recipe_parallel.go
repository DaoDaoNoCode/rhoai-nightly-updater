package recipe

import (
	"maps"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// MaxRunningSteps caps concurrent step execution within a single run.
const MaxRunningSteps = 3

// Describes step ordering as data: Steps derives each step's After field from
// it. Execution begins only after all predecessors finished successfully or
// were already in place.
var (
	// stepsFirst execute serially ahead of all other work: RHOAI,
	// its DataScienceCluster, the project, and what every feature needs
	// (a single prerequisites pass; prerequisite runs do not overlap because
	// each rewrites the full record it read).
	stepsFirst = []string{StepRHOAI, StepDSC, StepProject, StepPrerequisites}
	// stepLanes proceed in parallel after stepsFirst complete; within a lane,
	// steps still run in sequence.
	//   - Pipelines: the pipeline server uses the S3 storage, and the
	//     workbench attaches the tool's S3 Connection when the project has
	//     the tool's pipeline server.
	//   - MLflow, then its samples.
	//   - The model registry.
	//   - Serving, as one lane: MaaS, the model's access, the model, its
	//     MaaS publication and the playground share Connectivity Link,
	//     Kuadrant and the gateway, and each needs the one before. The
	//     model's access runs a prerequisites run of its own; it is the
	//     only one in a lane.
	stepLanes = [][]string{
		{StepS3, StepPipelineServer, StepWorkbench},
		{StepMLflow, StepMLflowSamples},
		{StepModelRegistry},
		{StepMaaS, StepModelsPrereq, StepModel, StepMaaSPublish, StepPlayground},
	}
	// stepsKueue set up distributed workloads: Kueue's component, then the
	// queues, the project's label and (asked for) the dashboard's Kueue
	// setting with its Queue hardware profile. They run right after
	// stepsFirst or right before stepsLast (runOrder, KueueFirst).
	stepsKueue = []string{StepKueuePrereq, StepKueue}
	// stepsLast run serially after every lane finishes: a Dashboard Dev
	// build, which pauses the dashboard's operator.
	stepsLast = []string{StepDashboardDev}
)

// KueueFirst is the run order's rule for distributed workloads: whether
// recipe r sets them up before its workloads (the workbench and the
// model), as the documented path does in a project that will be Kueue's
// (opendatahub-documentation modules/kueue-workflow.adoc L8-49: queues,
// the dashboard's Kueue setting and Queue hardware profiles come before
// users create workloads). It decides from what the dashboard will offer
// those workloads (odh-dashboard rhoai-3.7-ea.1 packages/hardware-profiles
// shared/kueueUtils.ts L76-119, envkit.KueueModeFor), not from the
// recipe's opt-in alone:
//
//   - the Kueue step labels the recipe's project
//     kueue.openshift.io/managed=true, whether the run creates it or uses
//     one that is there, so with distributed workloads in the recipe the
//     project is Kueue's once that step ran;
//   - the dashboard's Kueue area is on when its setting is on now
//     (settingOn, disableKueue false; a project the run creates is then
//     labelled at creation, as the dashboard's Create project does) or the
//     recipe's dashboardOptIn turns it on (the Kueue step); the
//     DataScienceCluster's kueue is turned on by the step before it;
//   - with both, the dashboard offers a workload in the project only a
//     Queue hardware profile of a LocalQueue the project has, which the
//     Kueue step makes: so the Kueue steps run right after the project and
//     what the features need, before every lane.
//
// Otherwise (no distributed workloads, or the setting off and no opt-in)
// the order is unchanged: the workloads come first, as in a project that
// is not Kueue's, and the Kueue steps come after every lane (with the
// setting off the dashboard offers a labelled project no hardware profile
// at all, so a workload after the label would find none).
func KueueFirst(r types.EnvironmentRecipe, settingOn bool) bool {
	k := r.Spec.Features.Kueue
	return k.Enabled && (k.DashboardOptIn || settingOn)
}

// runOrder is the run's order of step IDs: stepsFirst, the lanes side by
// side, then stepsLast, with stepsKueue right after stepsFirst when
// kueueFirst (KueueFirst) and right before stepsLast otherwise.
func runOrder(kueueFirst bool) (first []string, lanes [][]string, last []string) {
	if kueueFirst {
		return slices.Concat(stepsFirst, stepsKueue), stepLanes, stepsLast
	}
	return stepsFirst, stepLanes, slices.Concat(stepsKueue, stepsLast)
}

// inRunOrder lists steps as the run takes them: with kueueFirst the Kueue
// steps move from the end of the feature steps to right after the last of
// stepsFirst, so the plan and the run record show them where they run.
func inRunOrder(steps []Step, kueueFirst bool) []Step {
	if !kueueFirst {
		return steps
	}
	var kueueSteps, rest []Step
	for _, s := range steps {
		if slices.Contains(stepsKueue, s.ID) {
			kueueSteps = append(kueueSteps, s)
		} else {
			rest = append(rest, s)
		}
	}
	at := 0
	for i, s := range rest {
		if slices.Contains(stepsFirst, s.ID) {
			at = i + 1
		}
	}
	return slices.Concat(rest[:at], kueueSteps, rest[at:])
}

// withAfter populates After on each step from the run order (runOrder).
// Any step omitted from that order (currently none; a test enforces this)
// depends on every step listed before it.
func withAfter(steps []Step, kueueFirst bool) []Step {
	present := map[string]bool{}
	for _, s := range steps {
		present[s.ID] = true
	}
	after := map[string][]string{}
	// chain wires present ids sequentially, giving the first the supplied
	// deps, and yields the final id (or deps when nothing matched).
	chain := func(ids, deps []string) []string {
		for _, id := range ids {
			if present[id] {
				after[id] = slices.Clone(deps)
				deps = []string{id}
			}
		}
		return deps
	}
	first, lanes, last := runOrder(kueueFirst)
	head := chain(first, nil)
	var ends []string
	for _, lane := range lanes {
		if end := chain(lane, head); !slices.Equal(end, head) {
			ends = append(ends, end...)
		}
	}
	if len(ends) == 0 {
		ends = head
	}
	chain(last, ends)
	var earlier []string
	for i := range steps {
		deps, listed := after[steps[i].ID]
		if !listed {
			deps = slices.Clone(earlier)
		}
		steps[i].After = deps
		earlier = append(earlier, steps[i].ID)
	}
	return steps
}

// RunSeconds returns total elapsed time when step i costs seconds[i]
// (0 means skipped), using the same scheduling the runner applies: respect
// After ordering, allow up to MaxRunningSteps in flight. When lane count
// stays within that cap, the result equals the dependency graph's longest
// path (critical path), not the arithmetic sum of durations.
func RunSeconds(steps []Step, seconds []int) int {
	index := map[string]int{}
	for i, s := range steps {
		index[s.ID] = i
	}
	n := len(steps)
	started, ended := make([]bool, n), make([]bool, n)
	ends := make([]int, n)
	now, running, left := 0, 0, n
	for left > 0 {
		for i, s := range steps {
			if started[i] || running >= MaxRunningSteps || !afterEnded(s, index, ended) {
				continue
			}
			started[i], ends[i] = true, now+seconds[i]
			running++
		}
		if running == 0 {
			// Mutually blocking steps cannot start (tests ensure the
			// ordering stays acyclic).
			return now
		}
		next := -1
		for i := range steps {
			if started[i] && !ended[i] && (next < 0 || ends[i] < next) {
				next = ends[i]
			}
		}
		now = next
		for i := range steps {
			if started[i] && !ended[i] && ends[i] == now {
				ended[i] = true
				running--
				left--
			}
		}
	}
	return now
}

// afterEnded reports whether every predecessor of s (within this run) has finished.
func afterEnded(s Step, index map[string]int, ended []bool) bool {
	for _, id := range s.After {
		if j, ok := index[id]; ok && !ended[j] {
			return false
		}
	}
	return true
}

// Clone duplicates s without sharing mutable step-owned data. Parallel steps
// each receive their own State snapshot; when one finishes the runner applies
// Merge to fold changes back, so step code needs no locks and no two steps
// mutate the same storage.
func (s State) Clone() State {
	c := s
	if s.Pinned != nil {
		p := *s.Pinned
		c.Pinned = &p
	}
	if s.Project != nil {
		p := *s.Project
		c.Project = &p
	}
	c.Components = slices.Clone(s.Components)
	c.ComponentIDs = maps.Clone(s.ComponentIDs)
	c.Created = slices.Clone(s.Created)
	return c
}

// Merge applies into s the delta a step made on its private copy: base is
// that copy at handoff (a Clone of shared State), after is the same copy when
// the step returns. Only intentional mutations merge through, preserving
// concurrent updates from other steps: components and created resources
// accumulate (steps never delete them); project, pinned build, and model
// reachability overwrite only when the step changed them. Recipe and run ID
// remain outside a step's write set.
func (s *State) Merge(base, after State) {
	if !samePtr(base.Project, after.Project) {
		s.Project = after.Clone().Project
	}
	if !samePtr(base.Pinned, after.Pinned) {
		s.Pinned = after.Clone().Pinned
	}
	if base.Exposure != after.Exposure || base.GatewayRef != after.GatewayRef {
		s.Exposure, s.GatewayRef = after.Exposure, after.GatewayRef
	}
	for _, key := range after.Components {
		if !slices.Contains(base.Components, key) {
			s.AddComponents(key)
		}
	}
	for key, id := range after.ComponentIDs {
		if base.ComponentIDs[key] != id {
			s.AddComponentID(key, id)
		}
	}
	for _, p := range after.Created {
		if !slices.Contains(base.Created, p) {
			s.AddCreated(p)
		}
	}
}

// samePtr is true when a and b are both nil or both refer to equal values.
func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
