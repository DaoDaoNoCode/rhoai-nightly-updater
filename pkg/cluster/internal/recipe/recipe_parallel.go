package recipe

import (
	"maps"
	"slices"
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
	// comes last.
	stepsLast = []string{StepKueuePrereq, StepKueue, StepDashboardDev}
)

// withAfter populates After on each step from the declared run order. Any step
// omitted from that order (currently none; a test enforces this) depends on
// every step listed before it.
func withAfter(steps []Step) []Step {
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
	head := chain(stepsFirst, nil)
	var ends []string
	for _, lane := range stepLanes {
		if end := chain(lane, head); !slices.Equal(end, head) {
			ends = append(ends, end...)
		}
	}
	if len(ends) == 0 {
		ends = head
	}
	chain(stepsLast, ends)
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
