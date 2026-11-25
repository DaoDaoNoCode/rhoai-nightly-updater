package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The environment recipe runner.
//
// A run is one cluster operation (type "recipe") that holds this process's
// lock and the cross-pod lease from its start until its last step ended:
// teammates get 409 cluster_busy, and the background MaaS check skips. Its
// steps are function calls (internal/recipe Steps), not requests, so the
// run uses one rate-limit slot. Holding the lock throughout, rather than
// taking it again per step, keeps another operation from changing what a
// later step relies on.
//
// A step starts once every step it comes after (its After: the recipe's
// order, internal/recipe stepsFirst, stepLanes, stepsLast) ended well;
// steps that do not wait for each other run side by side, at most
// cluster.RecipeMaxRunningSteps at once, started in plan order. Before
// steps start the runner:
//  1. starts none when Stop was asked for, or when the updater is
//     shutting down (stopped, "restart": the marker is cleared with that
//     outcome, so the run is not reported interrupted);
//  2. passes the mutation wrapper's gate again (statusWriter.gate, which
//     holds the token of the request that started the run): it checks the
//     signed-in user's token (sessionProblem: not expired
//     or revoked, user:full, not expiring before the step's deadline and
//     its restore) and that they are still a cluster-admin
//     (mutationPermission); otherwise the run stops with sign_in_again (or
//     the code sessionProblem gives) or forbidden before the steps start.
//
// Then, per step, it:
//  3. gives the step its own context, WithTimeout(run, OperationDeadline),
//     the budget every operation has, a copy of the user's client on it,
//     and a ServiceAccount bookkeeper made now (the ServiceAccount token
//     rotates);
//  4. runs the step's read-only Check, then its Run only when needed, on
//     its own copy of the run's State (merged back when it ended), and
//     shows its progress in the operation (the lease, GET
//     /api/operation) and in GET /api/recipes/run;
//  5. records the step's outcome. After a failure no step starts; the
//     running ones end, then the run ends failed, naming every failed
//     step. Nothing is retried, but for the bounded retries inside a
//     step.
//
// The lease heartbeat stops a run whose lease was lost with
// cluster.ErrOperationLockLost: every running step's context ends (its
// own rules: no restore), and the run ends failed with errorCode
// lock_lost. The run's limit (recipeRunCap) ends them the same way.
//
// The user's token stays in this process's memory for the whole run
// (never written, never logged; SECURITY.md, Environment recipes). Logging
// out does not revoke it, so it does not stop a run; Stop does: the
// running steps finish, and no other starts. After a restart nothing
// resumes by itself: the new process has no token, and the ServiceAccount
// never acts for the user. The run record
// then still says running with a dead boot ID: GET /api/recipes/run
// reports it interrupted, and Resume runs the steps again, where each
// step's check skips what is set up.
//
// The run keeps one mutation slot (drain.go) from its start to its end, so
// a shutdown waits for the running steps (within ShutdownDrainTimeout: no
// step starts once the drain began, so each ends within one operation's
// budget of its start) and for the run's last record, and the run starts
// no step once it sees the drain.

// Seams of the runner; tests replace them.
var (
	recipeSteps          = cluster.RecipeSteps
	planRecipe           = cluster.PlanRecipe
	saveRecipeRun        = cluster.SaveRecipeRun
	readRecipeRun        = cluster.ReadRecipeRun
	recordRecipeActivity = cluster.RecordRecipeActivity
	recipeOwnership      = cluster.RecipeOwnership
	recipeOwnedProjects  = cluster.RecipeOwnedProjects
	// stepResultQueued, when set, is told each step's index once its end
	// is queued for the loop.
	stepResultQueued func(i int)
)

// recipeRecordTimeout bounds one write of the run record. The last one
// runs at the same time as the marker clear (cluster.MarkerClearTimeout,
// which is longer), so a draining pod's last step and its bookkeeping
// still fit cluster.ShutdownDrainTimeout.
const recipeRecordTimeout = 5 * time.Second

// recipeRunCap bounds a whole run: the sum of its steps' budgets. A safety
// net only: each step's own deadline does the real work. Steps that run
// side by side end sooner than that sum, never later (some step always
// runs until the run ends), so the serial sum still bounds the slowest
// run: a cap from the longest chain could end a run whose steps waited
// for a free slot.
func recipeRunCap(steps int) time.Duration {
	return time.Duration(steps) * (cluster.OperationDeadline + cluster.RecoveryTimeout)
}

// recipeRunner is this process's recipe run: the one running, or the
// last one it ran.
type recipeRunner struct {
	mu      sync.Mutex
	run     *types.RecipeRun
	opID    string
	running bool
	stop    bool
	// steps is the latest progress of each running step of the run, by
	// step ID (runStep's emit; stepEnded forgets it).
	steps map[string]types.UpdateStepEvent
	// done is closed when the running run has ended (lock released).
	done chan struct{}
}

var recipes = &recipeRunner{}

// status is this process's run, nil when it ran none.
func (rr *recipeRunner) status() *types.RecipeRunStatus {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.run == nil {
		return nil
	}
	st := &types.RecipeRunStatus{Run: copyRecipeRun(rr.run), Running: rr.running, StopRequested: rr.running && rr.stop}
	if rr.running {
		st.Pod = os.Getenv("HOSTNAME")
		if op := inflight.snapshot(); op != nil && op.ID == rr.opID && op.Step != "" {
			st.Step = &types.UpdateStepEvent{Step: op.Step, Status: op.StepStatus, Message: op.Message}
		}
		if len(rr.steps) > 0 {
			st.Progress = maps.Clone(rr.steps)
		}
		st.Ending = rr.run.Ending
	}
	return st
}

// progress records a running step's latest progress, when opID's run is
// the one running.
func (rr *recipeRunner) progress(opID, stepID string, e types.UpdateStepEvent) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if !rr.running || rr.opID != opID {
		return
	}
	if rr.steps == nil {
		rr.steps = map[string]types.UpdateStepEvent{}
	}
	rr.steps[stepID] = types.UpdateStepEvent{Step: e.Step, Status: e.Status, Message: e.Message}
}

// setEnding records why opID's running run starts no other step.
func (rr *recipeRunner) setEnding(opID, why string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.running && rr.opID == opID {
		rr.run.Ending = why
	}
}

// endProgress forgets an ended step's progress.
func (rr *recipeRunner) endProgress(opID, stepID string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.opID == opID {
		delete(rr.steps, stepID)
	}
}

// requestStop asks the running run to stop after its step. It reports
// whether a run is running.
func (rr *recipeRunner) requestStop() bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if !rr.running {
		return false
	}
	rr.stop = true
	return true
}

func (rr *recipeRunner) stopRequested() bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.stop
}

// update changes the run under the lock and returns a copy.
func (rr *recipeRunner) update(fn func(run *types.RecipeRun)) *types.RecipeRun {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	fn(rr.run)
	return copyRecipeRun(rr.run)
}

// updateEnding is update for a change that can end opID's running run: fn
// changes the run and returns why the run starts no other step ("" for
// none), recorded under the same lock: no status read sees the change (a
// failed step) without why the run ends.
func (rr *recipeRunner) updateEnding(opID string, fn func(run *types.RecipeRun) string) *types.RecipeRun {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if why := fn(rr.run); why != "" && rr.running && rr.opID == opID {
		rr.run.Ending = why
	}
	return copyRecipeRun(rr.run)
}

// end changes opID's run with its outcome and, under the same lock, ends
// its running, and returns a copy.
func (rr *recipeRunner) end(opID string, fn func(run *types.RecipeRun)) *types.RecipeRun {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	fn(rr.run)
	if rr.opID == opID {
		rr.running = false
	}
	return copyRecipeRun(rr.run)
}

func copyRecipeRun(run *types.RecipeRun) *types.RecipeRun {
	cp := *run
	cp.Steps = append([]types.RecipeRunStep(nil), run.Steps...)
	cp.Components = slices.Clone(run.Components)
	cp.ComponentIDs = maps.Clone(run.ComponentIDs)
	cp.CreatedParts = slices.Clone(run.CreatedParts)
	if run.Pinned != nil {
		p := *run.Pinned
		cp.Pinned = &p
	}
	if run.Project != nil {
		p := *run.Project
		cp.Project = &p
	}
	return &cp
}

// recipeStart is a run that holds the lock and has not begun: abandon
// gives the lock back, launch begins it.
type recipeStart struct {
	runCtx    context.Context
	cancelRun context.CancelCauseFunc
	op        *Operation
	sw        *statusWriter
	// slot: the run's own mutation slot is taken; handedOff: the runner
	// owns the lock, the lease and the slot; ended: abandon ran.
	slot, handedOff, ended bool
}

// lockRecipeRunner takes the cluster lock and the lease for a recipe run,
// with the run's own cancel as the lease's (the request's ends with the
// handler), or answers the refusal as lockCluster does.
func lockRecipeRunner(w http.ResponseWriter, r *http.Request) (*recipeStart, bool) {
	sw, ok := w.(*statusWriter)
	if !ok || sw.newBookkeeper == nil || sw.gate == nil {
		writeError(w, "a recipe run needs the mutation wrapper", http.StatusInternalServerError, "internal")
		return nil, false
	}
	runCtx, cancelRun := context.WithCancelCause(context.WithoutCancel(r.Context()))
	st := &recipeStart{runCtx: runCtx, cancelRun: cancelRun, sw: sw}
	refusal := acquireRunnerLock(func() *Operation {
		st.op = inflight.startBackground("recipe", "Environment recipe", sw.user, sw.newBookkeeper(runCtx))
		inflight.update(st.op.ID, func(o *Operation) { o.freshBookkeeper = sw.newBookkeeper })
		sw.opID = st.op.ID
		return st.op
	}, cancelRun)
	if refusal != nil {
		cancelRun(nil)
		writeLockRefusal(w, refusal)
		return nil, false
	}
	return st, true
}

// abandon gives back the lock of a run that did not begin.
func (st *recipeStart) abandon() {
	if st.ended || st.handedOff {
		return
	}
	st.ended = true
	if st.slot {
		mutations.end()
	}
	releaseClusterMutationLock()
	st.cancelRun(nil)
}

// recipeRefusal answers a recipe the run refuses, with its problems (and
// the plan, when it was planned). Nothing was changed.
func writeRecipeRefusal(w http.ResponseWriter, status int, code, msg string, problems []types.RecipeProblem, plan *types.RecipePlan) {
	body := map[string]any{"error": msg, "errorCode": code, "problems": problems}
	if plan != nil {
		body["plan"] = plan
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, body, "recipe-refusal")
}

// planForRun plans a recipe in this profile: the gitops profile never
// runs one (its routes refuse first; this is the same rule, fail closed).
func planForRun(c *cluster.Client, rcp types.EnvironmentRecipe, pinned *types.RecipePinnedBuild) (*types.RecipePlan, cluster.RecipeState) {
	plan, state := planRecipe(c, rcp, pinned)
	if activeProfile != ProfileFull {
		plan.Problems = append(plan.Problems, types.RecipeProblem{Message: "Environment recipes run only in the full profile of the updater.",
			Fix: &types.RecipeFix{Code: types.RecipeFixProfileGitOps}})
	}
	return plan, state
}

// beginRecipeRun plans the recipe under the held lock, begins the run
// (marker, run record, activity) and starts the runner; it answers 202
// with the run's ID, or refuses (422 with the plan's problems; 503 when
// the run record cannot be written or the updater is shutting down) and
// gives the lock back. prior is the run a resume continues (its ID, start
// and project are kept), nil for a new one.
func beginRecipeRun(c *cluster.Client, w http.ResponseWriter, st *recipeStart, rcp types.EnvironmentRecipe, prior *types.RecipeRun, pinned *types.RecipePinnedBuild) {
	plan, state := planForRun(c, rcp, pinned)
	if len(plan.Problems) > 0 {
		st.abandon()
		writeRecipeRefusal(w, http.StatusUnprocessableEntity, "prerequisites", "The recipe cannot run on this cluster now. Nothing was changed.", plan.Problems, plan)
		return
	}
	steps := recipeSteps(rcp)
	now := time.Now().UTC().Format(time.RFC3339)
	run := &types.RecipeRun{
		ID: newOperationID(), OperationID: st.op.ID, BootID: bootID, Pod: os.Getenv("HOSTNAME"), User: st.sw.user,
		StartedAt: now, State: types.RecipeRunRunning, Recipe: rcp, Pinned: state.Pinned, Project: state.Project,
	}
	if prior != nil {
		// The project is what the plan found now (the record of the
		// projects recipes created, and the namespace), never the
		// earlier attempt's: a project that is gone is created again.
		run.ID, run.StartedAt, run.ResumedAt = prior.ID, prior.StartedAt, now
	}
	for _, s := range steps {
		typical, most := s.Seconds()
		run.Steps = append(run.Steps, types.RecipeRunStep{ID: s.ID, Title: s.Title, Status: types.RecipeStepPending, TypicalSeconds: typical, MaxSeconds: most})
	}
	if prior != nil {
		keepComponents(run, &state, prior)
	}
	// The run's ID marks the cluster-wide objects its steps create.
	state.RunID = run.ID
	// The run's own mutation slot, before anything is recorded: a run
	// that cannot begin leaves no record of a running run.
	if !mutations.begin() {
		st.abandon()
		w.Header().Set("Retry-After", "30")
		writeError(w, "The updater is restarting. No changes were made; retry once it is back.", http.StatusServiceUnavailable, "shutting_down")
		return
	}
	st.slot = true
	ctx, cancel := context.WithTimeout(st.runCtx, recipeRecordTimeout)
	err := saveRecipeRun(st.sw.newBookkeeper(ctx), run, true)
	cancel()
	if err != nil {
		st.abandon()
		slog.Warn("recipe run refused: its record cannot be written", "error", err)
		writeError(w, "Cannot record the recipe run in the updater's operation ConfigMap ("+err.Error()+"). Nothing was changed; try again.", http.StatusServiceUnavailable, "record_unavailable")
		return
	}
	beginHeldOperation(st.op.ID, recipeTarget(rcp, 0, len(steps), nil))
	recipes.mu.Lock()
	recipes.run, recipes.opID, recipes.running, recipes.stop, recipes.steps = run, st.op.ID, true, false, nil
	done := make(chan struct{})
	recipes.done = done
	recipes.mu.Unlock()

	x := &recipeExec{
		runCtx: st.runCtx, cancelRun: st.cancelRun, opID: st.op.ID, gate: st.sw.gate, user: st.sw.user,
		client: c, newBookkeeper: st.sw.newBookkeeper, state: state, steps: steps, done: done,
	}
	verb := "Started"
	if prior != nil {
		verb = "Resumed"
	}
	slog.Info("recipe run", "verb", verb, "run", run.ID, "recipe", rcp.Metadata.Name, "steps", len(steps), "user", st.sw.user)
	st.handedOff = true
	go x.execute()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, types.RecipeRunAccepted{RunID: run.ID}, "recipe-run")
}

// keepComponents carries the DataScienceCluster components earlier
// attempts of the run turned on into the resumed one (merged, never
// reset), and the cluster-wide parts they created. Clean up offers only
// those still as the run set them. How the model is reached is kept too,
// so a resume sets it up as the first attempt resolved it; the record is
// the updater's own ConfigMap, so only an exposure a recipe may name (or
// the Models tab's own "gateway" with a namespace/name) is kept, any other
// is resolved again.
func keepComponents(run *types.RecipeRun, state *cluster.RecipeState, prior *types.RecipeRun) {
	state.AddComponents(prior.Components...)
	for key, id := range prior.ComponentIDs {
		state.AddComponentID(key, id)
	}
	run.Components = slices.Clone(state.Components)
	run.ComponentIDs = maps.Clone(state.ComponentIDs)
	for _, p := range prior.CreatedParts {
		state.AddCreated(p)
	}
	run.CreatedParts = slices.Clone(state.Created)
	// An exposure the recipe names wins: a kept one must be that one.
	if e := run.Recipe.Spec.Features.Models.Exposure; (e == "" || e == prior.Exposure) && cluster.RecipeExposureKept(prior.Exposure, prior.GatewayRef) {
		state.Exposure, state.GatewayRef = prior.Exposure, prior.GatewayRef
		run.Exposure, run.GatewayRef = prior.Exposure, prior.GatewayRef
	}
}

// recipeTarget is the operation's target: "<recipe>: k of n steps done",
// and "; running: <title>, <title>" while steps run.
func recipeTarget(rcp types.EnvironmentRecipe, done, n int, running []string) string {
	t := fmt.Sprintf("%s: %d of %d steps done", rcp.Metadata.Name, done, n)
	if len(running) > 0 {
		t += "; running: " + strings.Join(running, ", ")
	}
	return t
}

// recipeExec is a running run.
type recipeExec struct {
	runCtx    context.Context
	cancelRun context.CancelCauseFunc
	opID      string
	// gate is the wrapper's check of the user's sign-in and permission
	// (statusWriter.gate); it holds the user's token.
	gate          func(ctx context.Context) (string, string)
	user          string
	client        *cluster.Client
	newBookkeeper func(context.Context) *cluster.Client
	// state is the run's State. Only the loop's goroutine reads or
	// changes it: each step runs on its own Clone, merged back when it
	// ended (stepResult).
	state cluster.RecipeState
	steps []cluster.RecipeStep
	done  chan struct{}
}

// recipeEnd is how a run ended.
type recipeEnd struct {
	state, reason, code string
}

// execute runs the steps and ends the run, also when a step panics.
func (x *recipeExec) execute() {
	defer x.cancelRun(nil)
	capped, cancel := context.WithTimeout(x.runCtx, recipeRunCap(len(x.steps)))
	defer cancel()
	x.runCtx = capped
	x.activity("recipe-start", "started", true)
	x.finish(x.loop())
}

// stepResult is how a step ended, as its worker reports it: its status,
// message and errorCode, and the State it ran on (state), with the copy
// it started from (base), for State.Merge.
type stepResult struct {
	i                 int
	status, msg, code string
	base, state       cluster.RecipeState
}

// The reasons the loop starts no more steps (the runner's notes, points 1
// and 2), besides a step's failure.
const (
	haltStopped  = "stopped"
	haltRestart  = "restart"
	haltTimeout  = "timeout"
	haltGate     = "gate"
	haltLockLost = "lock_lost"
)

// endingInternal is the ending of a run whose loop broke (recoverLoop): it
// ends failed on an internal error once its running steps ended.
const endingInternal = "internal"

// recipeHalt is why the loop starts no more steps: the kind, and for a
// gate refusal its message and code.
type recipeHalt struct {
	kind, msg, code string
}

// recipeLoop is the loop's own bookkeeping: what each step did, and what
// runs. Only the loop's goroutine uses it.
type recipeLoop struct {
	// after: the indexes of the steps each step comes after (the steps'
	// After; an ID that is not one of the run's steps is no wait).
	after [][]int
	// started: the step began; status: its outcome once it ended.
	started []bool
	status  []string
	running int
	// lastEnded is the step that ended last, -1 before any.
	lastEnded int
	halt      *recipeHalt
	failed    bool
}

// ending is why the loop starts no other step (types.RecipeRunStatus
// Ending): "failed" once a step failed, otherwise the halt's kind; ""
// while steps may start. The status shows it no later than what makes it
// true: a failure in the same change as the step's outcome, a halt before
// the loop's next write of the run.
func (l *recipeLoop) ending() string {
	switch {
	case l.failed:
		return "failed"
	case l.halt != nil:
		return l.halt.kind
	}
	return ""
}

func newRecipeLoop(steps []cluster.RecipeStep) *recipeLoop {
	index := map[string]int{}
	for i, s := range steps {
		index[s.ID] = i
	}
	l := &recipeLoop{after: make([][]int, len(steps)), started: make([]bool, len(steps)), status: make([]string, len(steps)), lastEnded: -1}
	for i, s := range steps {
		for _, id := range s.After {
			if j, ok := index[id]; ok && j != i {
				l.after[i] = append(l.after[i], j)
			}
		}
	}
	return l
}

// endedWell: step i succeeded or was found set up.
func (l *recipeLoop) endedWell(i int) bool {
	return l.status[i] == types.RecipeStepSuccess || l.status[i] == types.RecipeStepSkipped
}

// ready lists the steps that may start, in plan order: not started, and
// every step they come after ended well.
func (l *recipeLoop) ready() []int {
	var out []int
	for i := range l.started {
		if l.started[i] {
			continue
		}
		ok := true
		for _, j := range l.after[i] {
			ok = ok && l.endedWell(j)
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// loop runs the steps (see the runner's notes) and returns how the run
// ended. Only runStep runs in other goroutines (work), at most
// cluster.RecipeMaxRunningSteps at once; this goroutine alone decides
// which steps start (beforeSteps), records starts and ends (stepStarted,
// stepEnded, save) and changes the run's State, getting each step's end
// over results. Once a step failed, or the run is to stop (Stop, a drain,
// the sign-in, the run's limit), no step starts; the running ones end
// (each within its deadline), then the run ends. A panic here ends the
// run failed once its running steps ended.
func (x *recipeExec) loop() (end recipeEnd) {
	l := newRecipeLoop(x.steps)
	results := make(chan stepResult, len(x.steps))
	defer recoverRun("recipe", func() {
		end = recipeEnd{types.RecipeRunFailed, operationPanicMessage, "internal"}
		x.recoverLoop(l, results)
	})
	unsaved := false
	ended := func(r stepResult) {
		x.takeEnd(l, r)
		unsaved = true
	}
	for {
		saved := false
		if l.halt == nil && !l.failed {
			saved = x.startSteps(l, results)
		}
		// A step's end is written with the next start, or here when none
		// starts while others run (the run's end writes the last one).
		if unsaved && !saved && l.running > 0 {
			x.save(x.snapshot())
		}
		if l.running == 0 {
			break
		}
		ended(<-results)
		// Every end already queued counts before anything starts: a
		// failure that came in beside a success must stop new starts.
		for queued := true; queued; {
			select {
			case r := <-results:
				ended(r)
			default:
				queued = false
			}
		}
	}
	return x.ended(l)
}

// takeEnd takes over a step's end; the loop alone calls it. A failure
// shows in the status together with the run's ending ("failed"), also
// when no step runs any more and the run ends right away.
func (x *recipeExec) takeEnd(l *recipeLoop, r stepResult) {
	l.running--
	x.state.Merge(r.base, r.state)
	l.status[r.i], l.lastEnded = r.status, r.i
	l.failed = l.failed || r.status == types.RecipeStepFailed
	x.stepEnded(r.i, r.status, r.msg, r.code, l.ending())
}

// recoverLoop ends a run whose loop panicked: the running steps end (each
// by its deadline) and what they changed in the State is kept; none starts.
// A step marked running for a start that never happened is pending again
// (Resume runs it). A step the record still shows running once the
// running ones ended (its end was lost in the panic) counts as failed: the
// run's bookkeeping broke. A second panic here is logged, never let
// through: the run still ends.
func (x *recipeExec) recoverLoop(l *recipeLoop, results chan stepResult) {
	defer recoverRun("recipe recovery", func() {})
	// The run ends: the status and the record say so, with only the steps
	// that run shown running, while those end, which can take their whole
	// budget. The write's own panic does not keep the steps from being
	// waited for.
	recipes.updateEnding(x.opID, func(run *types.RecipeRun) string {
		for i := range run.Steps {
			if s := &run.Steps[i]; s.Status == types.RecipeStepRunning && !l.started[i] {
				s.Status, s.StartedAt, s.FinishedAt, s.Message, s.ErrorCode = types.RecipeStepPending, "", "", "", ""
			}
		}
		return endingInternal
	})
	func() {
		defer recoverRun("recipe recovery record", func() {})
		x.save(x.snapshot())
	}()
	var drained []stepResult
	for ; l.running > 0; l.running-- {
		r := <-results
		x.state.Merge(r.base, r.state)
		drained = append(drained, r)
	}
	recipes.update(func(run *types.RecipeRun) {
		// What ended while the loop was broken is recorded as it ended.
		for _, r := range drained {
			s := &run.Steps[r.i]
			s.Status, s.Message, s.ErrorCode = r.status, truncateText(r.msg, maxOperationMessageBytes), r.code
			s.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		}
		for i := range run.Steps {
			if s := &run.Steps[i]; s.Status == types.RecipeStepRunning {
				s.Status, s.Message, s.ErrorCode = types.RecipeStepFailed, operationPanicMessage, "internal"
			}
		}
		run.Components = slices.Clone(x.state.Components)
		run.ComponentIDs = maps.Clone(x.state.ComponentIDs)
		run.CreatedParts = slices.Clone(x.state.Created)
		run.Exposure, run.GatewayRef = x.state.Exposure, x.state.GatewayRef
	})
}

// startSteps starts the steps that may start now, up to the limit: it
// passes beforeSteps once for all of them, records them running in one
// write, then starts each in a worker. It reports whether it wrote the
// record. A reason not to start sets l.halt, also when none may start yet
// but a step still waits (for a running step, or a free slot): a Stop
// then shows with the step end that came first, not only once the step
// waited for ended.
func (x *recipeExec) startSteps(l *recipeLoop, results chan<- stepResult) bool {
	batch := l.ready()
	if free := cluster.RecipeMaxRunningSteps - l.running; len(batch) > free {
		batch = batch[:max(free, 0)]
	}
	if len(batch) == 0 {
		if halt := x.halted(); halt != nil && slices.Contains(l.started, false) {
			x.halt(l, halt)
		}
		return false
	}
	if halt := x.beforeSteps(); halt != nil {
		x.halt(l, halt)
		return false
	}
	for _, i := range batch {
		x.stepStarted(i)
	}
	x.save(x.snapshot())
	// The gate and the transition write took time: a drain that began
	// meanwhile must not get a whole new step, and a Stop accepted
	// meanwhile starts nothing either. Nothing that waits lies between this
	// check and the steps' deadlines (runStep's first line), so every step
	// that starts ends by the drain's start plus OperationDeadline.
	if draining, stop := mutations.isDraining(), recipes.stopRequested(); draining || stop {
		l.halt = &recipeHalt{kind: haltStopped}
		if draining {
			l.halt.kind = haltRestart
		}
		for _, i := range batch {
			x.stepEnded(i, types.RecipeStepPending, "", "", l.ending())
		}
		if l.running > 0 {
			x.save(x.snapshot())
		}
		return true
	}
	for _, i := range batch {
		base := x.state.Clone()
		l.started[i] = true
		l.running++
		go x.work(i, base, results)
	}
	return true
}

// work runs step i on its own copy of the run's State and reports its end,
// also when the step panics (then it failed, internal): a step's panic
// never ends the process.
func (x *recipeExec) work(i int, base cluster.RecipeState, results chan<- stepResult) {
	r := stepResult{i: i, base: base, state: base.Clone()}
	queued := stepResultQueued
	defer func() {
		results <- r
		if queued != nil {
			queued(i)
		}
	}()
	defer recoverRun("recipe step "+x.steps[i].ID, func() {
		r.status, r.msg, r.code = types.RecipeStepFailed, operationPanicMessage, "internal"
	})
	r.status, r.msg, r.code = x.runStep(x.steps[i], &r.state)
}

// halt makes the loop start no other step, and shows why before the
// loop's next write (steps may still run).
func (x *recipeExec) halt(l *recipeLoop, h *recipeHalt) {
	l.halt = h
	recipes.setEnding(x.opID, l.ending())
}

// beforeSteps decides whether steps may start now (the runner's notes,
// points 1 and 2): nil when they may, otherwise why not.
func (x *recipeExec) beforeSteps() *recipeHalt {
	if h := x.halted(); h != nil {
		return h
	}
	if msg, code := x.gate(x.runCtx); msg != "" {
		return &recipeHalt{kind: haltGate, msg: msg, code: code}
	}
	return nil
}

// halted is why no step may start now that needs no request to know
// (Stop, the drain, the lease, the run's limit), nil when none.
func (x *recipeExec) halted() *recipeHalt {
	switch {
	case recipes.stopRequested():
		return &recipeHalt{kind: haltStopped}
	case mutations.isDraining():
		return &recipeHalt{kind: haltRestart}
	case errors.Is(context.Cause(x.runCtx), cluster.ErrOperationLockLost):
		return &recipeHalt{kind: haltLockLost}
	case x.runCtx.Err() != nil:
		return &recipeHalt{kind: haltTimeout}
	}
	return nil
}

// ended is how the run ended once no step runs: lock_lost when the lease
// was lost; failed when a step failed, naming the first in plan order with
// its message, and every other one that failed; stopped with why steps
// did not start; succeeded when every step ended well.
func (x *recipeExec) ended(l *recipeLoop) recipeEnd {
	n := len(x.steps)
	if errors.Is(context.Cause(x.runCtx), cluster.ErrOperationLockLost) {
		return recipeEnd{types.RecipeRunFailed, cluster.LockLostMessage, "lock_lost"}
	}
	run := x.snapshot()
	var failed []int
	for i := range x.steps {
		if l.status[i] == types.RecipeStepFailed {
			failed = append(failed, i)
		}
	}
	if len(failed) > 0 {
		first := run.Steps[failed[0]]
		reason := fmt.Sprintf("Step %d of %d (%s) failed: %s", failed[0]+1, n, first.Title, first.Message)
		if len(failed) > 1 {
			var others []string
			for _, i := range failed[1:] {
				others = append(others, fmt.Sprintf("step %d of %d (%s)", i+1, n, x.steps[i].Title))
			}
			reason += " Also failed: " + andList(others) + "."
		}
		return recipeEnd{types.RecipeRunFailed, reason, first.ErrorCode}
	}
	var next []string
	for _, i := range l.ready() {
		next = append(next, x.steps[i].Title)
	}
	where := "before its first step"
	if l.lastEnded >= 0 {
		where = "after " + x.steps[l.lastEnded].Title
	}
	if h := l.halt; h != nil {
		switch h.kind {
		case haltStopped:
			return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped %s, as asked. Resume continues with %s.", where, andList(next)), "stopped"}
		case haltRestart:
			return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped %s: the updater is restarting. Resume continues with %s once it is back.", where, andList(next)), "restart"}
		case haltGate:
			return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped before %s: %s", andList(next), h.msg), h.code}
		case haltTimeout:
			return recipeEnd{types.RecipeRunFailed, fmt.Sprintf("Stopped %s: the run took longer than its limit (%s).", where, recipeRunCap(n)), "timeout"}
		}
		// haltLockLost: the lease's loss is checked first.
	}
	for i := range x.steps {
		if !l.endedWell(i) {
			// Steps that wait for each other: a test keeps the order
			// acyclic, so this is a bug.
			return recipeEnd{types.RecipeRunFailed, fmt.Sprintf("Step %d of %d (%s) could not start: a step it comes after never ended.", i+1, n, x.steps[i].Title), "internal"}
		}
	}
	return recipeEnd{state: types.RecipeRunSucceeded, reason: fmt.Sprintf("Every step of recipe %s is set up.", x.state.Recipe.Metadata.Name)}
}

// andList joins items as "a", "a and b", "a, b and c".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// runStep runs the step on its own deadline, client, bookkeeper and facts
// client (points 3 and 4), on st (the step's own copy of the run's
// State), and returns its status, message and errorCode. It runs in a
// worker: of x it reads
// only what stays the same during the run (never x.state), and it reports
// progress to the operation and the runner, which lock.
func (x *recipeExec) runStep(step cluster.RecipeStep, st *cluster.RecipeState) (string, string, string) {
	stepCtx, cancel := context.WithTimeout(x.runCtx, cluster.OperationDeadline)
	defer cancel()
	c := x.client.WithContext(stepCtx)
	sa := x.newBookkeeper(stepCtx)
	c.SetBookkeeping(sa)
	c.SetFacts(sa)
	emit := func(e cluster.UpdateStepEvent) {
		recipes.progress(x.opID, step.ID, e)
		inflight.recordStep(x.opID, UpdateStep{Step: e.Step, Status: e.Status, Message: e.Message, Detail: e.Detail, ErrorCode: e.ErrorCode})
	}
	emit(cluster.UpdateStepEvent{Step: step.ID, Status: "running", Message: fmt.Sprintf("Checking %s", step.Title)})
	chk, err := step.Check(c, st)
	switch {
	case err != nil:
		if ctxErr := stepCtx.Err(); ctxErr != nil {
			return types.RecipeStepFailed, stepStopped(x.runCtx, ctxErr), stepStoppedCode(x.runCtx)
		}
		return types.RecipeStepFailed, fmt.Sprintf("Cannot check %s: %v.", step.Title, err), errorCodeOf(err, "prerequisites")
	case chk.Blocked != "":
		return types.RecipeStepFailed, chk.Blocked, "prerequisites"
	case chk.Done:
		return types.RecipeStepSkipped, "Already set up: " + chk.Reason, ""
	}
	res, err := step.Run(c, st, emit)
	switch {
	case err != nil:
		return types.RecipeStepFailed, stepStopped(x.runCtx, err), stepStoppedCode(x.runCtx)
	case res == nil:
		return types.RecipeStepFailed, "The step ended without reporting a result.", "internal"
	case !res.Success:
		return types.RecipeStepFailed, res.Message, res.ErrorCode
	}
	return types.RecipeStepSuccess, res.Message, ""
}

// stepStopped is the message of a step its context stopped.
func stepStopped(runCtx context.Context, err error) string {
	if errors.Is(context.Cause(runCtx), cluster.ErrOperationLockLost) {
		return cluster.LockLostMessage
	}
	return fmt.Sprintf("The step stopped: %v.", err)
}

func stepStoppedCode(runCtx context.Context) string {
	if errors.Is(context.Cause(runCtx), cluster.ErrOperationLockLost) {
		return "lock_lost"
	}
	return "timeout"
}

// errorCodeOf is the errorCode of a failed request, or fallback.
func errorCodeOf(err error, fallback string) string {
	if _, code := cluster.HTTPStatusForError(err); code != "" && code != "internal" {
		return code
	}
	return fallback
}

// snapshot is a copy of the run as it is now.
func (x *recipeExec) snapshot() *types.RecipeRun {
	return recipes.update(func(*types.RecipeRun) {})
}

// stepStarted marks step i running in memory (startSteps writes the
// record) and shows the running steps in the operation.
func (x *recipeExec) stepStarted(i int) {
	run := recipes.update(func(run *types.RecipeRun) {
		run.Steps[i].Status, run.Steps[i].StartedAt = types.RecipeStepRunning, time.Now().UTC().Format(time.RFC3339)
		run.Steps[i].Message, run.Steps[i].ErrorCode, run.Steps[i].FinishedAt = "", "", ""
	})
	x.showTarget(run)
}

// showTarget shows in the operation how far run is, and what runs.
func (x *recipeExec) showTarget(run *types.RecipeRun) {
	done := 0
	var running []string
	for _, s := range run.Steps {
		switch s.Status {
		case types.RecipeStepSuccess, types.RecipeStepSkipped:
			done++
		case types.RecipeStepRunning:
			running = append(running, s.Title)
		}
	}
	inflight.update(x.opID, func(op *Operation) {
		op.Target = truncateText(recipeTarget(x.state.Recipe, done, len(run.Steps), running), maxOperationTargetBytes)
	})
}

// stepEnded records step i's outcome in memory (the loop writes it) and
// takes over what the steps learnt (the project, components, created
// parts, the exposure: x.state, merged). ending is why the run starts no
// other step (recipeLoop.ending), recorded in the same change.
func (x *recipeExec) stepEnded(i int, status, msg, code, ending string) {
	run := recipes.updateEnding(x.opID, func(run *types.RecipeRun) string {
		s := &run.Steps[i]
		s.Status, s.Message, s.ErrorCode = status, truncateText(msg, maxOperationMessageBytes), code
		s.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		if status == types.RecipeStepPending {
			s.StartedAt, s.FinishedAt = "", ""
		}
		if x.state.Project != nil {
			p := *x.state.Project
			run.Project = &p
		}
		if x.state.Pinned != nil {
			p := *x.state.Pinned
			run.Pinned = &p
		}
		run.Components = slices.Clone(x.state.Components)
		run.ComponentIDs = maps.Clone(x.state.ComponentIDs)
		run.CreatedParts = slices.Clone(x.state.Created)
		run.Exposure, run.GatewayRef = x.state.Exposure, x.state.GatewayRef
		return ending
	})
	recipes.endProgress(x.opID, x.steps[i].ID)
	x.showTarget(run)
	inflight.recordStep(x.opID, UpdateStep{Step: x.steps[i].ID, Status: status, Message: msg, ErrorCode: code})
}

// save writes the run record with a fresh ServiceAccount client; a failed
// write is logged (the record is what other pods and a restart see; the
// run goes on).
func (x *recipeExec) save(run *types.RecipeRun) {
	ctx, cancel := context.WithTimeout(context.Background(), recipeRecordTimeout)
	defer cancel()
	if err := saveRecipeRun(x.newBookkeeper(ctx), run, false); err != nil {
		slog.Warn("could not record the recipe run", "run", run.ID, "error", err)
	}
}

// activity writes a run's activity entry with a fresh ServiceAccount
// client.
func (x *recipeExec) activity(action, detail string, success bool) {
	ctx, cancel := context.WithTimeout(context.Background(), recipeRecordTimeout)
	defer cancel()
	recordRecipeActivity(x.newBookkeeper(ctx), action, fmt.Sprintf("recipe=%s %s", x.state.Recipe.Metadata.Name, detail), success)
}

// finish ends the run: its outcome becomes the operation's, the record's
// last write and the activity entry run while the lock is released (the
// marker clear, which is longer, bounds all three), then the run's
// mutation slot is given back.
func (x *recipeExec) finish(end recipeEnd) {
	// The outcome and not running any more in one change, so no status
	// read sees a running run that ended. Before the lock is free: a run
	// started as soon as it is (also while this one's bookkeeping still
	// runs) owns the runner's state from then on, and nothing below
	// touches it.
	run := recipes.end(x.opID, func(run *types.RecipeRun) {
		run.State, run.StopReason, run.ErrorCode, run.Ending = end.state, end.reason, end.code, ""
		run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	})
	status := "failed"
	if end.state == types.RecipeRunSucceeded {
		status = "success"
	}
	inflight.recordStep(x.opID, UpdateStep{Step: "operation_complete", Status: status, Message: end.reason, ErrorCode: end.code})
	slog.Info("recipe run ended", "run", run.ID, "state", end.state, "code", end.code)
	var wg sync.WaitGroup
	wg.Go(func() { x.save(run) })
	wg.Go(func() { x.activity("recipe", end.state+": "+end.reason, end.state == types.RecipeRunSucceeded) })
	releaseClusterMutationLock()
	wg.Wait()
	mutations.end()
	close(x.done)
}

// recipeRunStatus is GET /api/recipes/run: this process's run while it
// runs, otherwise the record. A record that says running is another
// process's: running there while that process holds a live lease of the
// run's own operation (its boot ID and operation ID), otherwise
// interrupted.
//
// What a run that is not running set up is worked out from the cluster
// (cluster.RecipeOwnership): each step's created and the Clean up
// request. The record's own created flags never count.
//
// Once no run is running it also lists the projects recipes created (this
// run's and earlier runs') that the cluster has now
// (cluster.RecipeOwnedProjects), each removable by Clean up.
func recipeRunStatus(c, u *cluster.Client) types.RecipeRunStatus {
	st := recordedRecipeRunStatus(c)
	if !st.Running {
		projects, err := recipeOwnedProjects(c)
		if err != nil {
			st.ProjectsError = "Cannot read the projects recipes created: " + err.Error()
		}
		st.Projects = projects
	}
	if st.Run == nil {
		return st
	}
	created := map[string]bool{}
	if !st.Running {
		created, st.Cleanup = recipeOwnership(c, u, st.Run)
		st.CleanupKueueSetting = created[cluster.RecipeCreatedKueueSetting]
	}
	for i := range st.Run.Steps {
		st.Run.Steps[i].Created = created[st.Run.Steps[i].ID]
	}
	return st
}

func recordedRecipeRunStatus(c *cluster.Client) types.RecipeRunStatus {
	local := recipes.status()
	if local != nil && local.Running {
		return *local
	}
	run, err := readRecipeRun(c)
	if err != nil || run == nil {
		if err != nil {
			slog.Debug("could not read the recipe run record", "error", err)
		}
		if local != nil {
			return *local
		}
		return types.RecipeRunStatus{}
	}
	if run.BootID == bootID && local != nil && local.Run.ID == run.ID {
		// This process's own run: memory has its last state, also when
		// its last record write failed.
		return *local
	}
	st := types.RecipeRunStatus{Run: run}
	if run.State != types.RecipeRunRunning {
		return st
	}
	if rec, err := readOperationState(c); err == nil && rec != nil && leaseHeldElsewhere(rec.Lease) && rec.Lease.BootID == run.BootID && rec.Lease.ID == run.OperationID {
		st.Running, st.Remote, st.Pod = true, true, rec.Lease.Pod
		// The ending the pod recorded; a record from an updater version
		// that kept none still shows a failure (after one nothing starts,
		// and Resume sets the steps pending again).
		st.Ending = run.Ending
		if st.Ending == "" && slices.ContainsFunc(run.Steps, func(s types.RecipeRunStep) bool { return s.Status == types.RecipeStepFailed }) {
			st.Ending = "failed"
		}
		if rec.Lease.Step != "" {
			st.Step = &types.UpdateStepEvent{Step: rec.Lease.Step, Status: rec.Lease.StepStatus, Message: rec.Lease.Message}
		}
		return st
	} else if err != nil {
		// Unknown whether its process lives: report the record as it is.
		return st
	}
	st.Interrupted = true
	return st
}
