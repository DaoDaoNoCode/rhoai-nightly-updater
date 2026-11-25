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
	// stepsLast run serially after every lane finishes: the
	// dashboard's Kueue opt-in changes how new workbenches and model
	// deployments are created, so distributed workloads come after them,
	// and a Dashboard Dev build, which pauses the dashboard's operator,
	// comes last. A recipe that turns the opt-in on creates its workbench
	// between the two instead (runOrder).
	stepsLast = []string{StepKueuePrereq, StepKueue, StepDashboardDev}
)

// workbenchAfterKueue reports whether recipe r creates its workbench after
// its distributed workloads: r sets up a workbench and turns the
// dashboard's Kueue setting on (dashboardOptIn). Once the setting is on,
// the project step labels a new project kueue.openshift.io/managed=true,
// as the dashboard's Create project does (kueueManaged), and the dashboard
// offers a workbench there only a Queue hardware profile of a LocalQueue
// the project has (envkit.KueueModeFor, workbench.ChooseProfile). The Kueue
// step makes that LocalQueue, labels the project and, with the opt-in,
// makes the tool's Queue profile; a workbench created after it gets a Queue
// profile on every run, a first one and one after a Clean up of the project
// alike. Without the opt-in the run does not turn the setting on, so the
// workbench stays in its lane: after the Kueue step's label, with the
// setting off, the dashboard would offer it no profile at all.
func workbenchAfterKueue(r types.EnvironmentRecipe) bool {
	f := r.Spec.Features
	return f.Workbench.Enabled && f.Kueue.Enabled && f.Kueue.DashboardOptIn
}

// runOrder is recipe r's stepLanes and stepsLast: as declared, or, when r
// creates its workbench after its distributed workloads
// (workbenchAfterKueue), with the workbench moved from the pipelines lane
// to right after the Kueue step. It still comes after the pipeline server
// (every lane ends before stepsLast) and before Dashboard Dev; the model
// keeps its lane, before the Kueue steps.
func runOrder(r types.EnvironmentRecipe) (lanes [][]string, last []string) {
	if !workbenchAfterKueue(r) {
		return stepLanes, stepsLast
	}
	for _, lane := range stepLanes {
		lanes = append(lanes, slices.DeleteFunc(slices.Clone(lane), func(id string) bool { return id == StepWorkbench }))
	}
	return lanes, slices.Insert(slices.Clone(stepsLast), slices.Index(stepsLast, StepKueue)+1, StepWorkbench)
}

// withAfter populates After on each step from recipe r's run order
// (runOrder). Any step omitted from that order (currently none; a test
// enforces this) depends on every step listed before it.
func withAfter(steps []Step, r types.EnvironmentRecipe) []Step {
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
	lanes, last := runOrder(r)
	head := chain(stepsFirst, nil)
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
