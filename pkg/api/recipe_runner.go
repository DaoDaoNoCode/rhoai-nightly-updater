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
// Per step the runner:
//  1. ends the run as stopped when Stop after this step was asked for, or
//     when the updater is shutting down (stopped, "restart": the marker is
//     cleared with that outcome, so the run is not reported interrupted);
//  2. passes the mutation wrapper's gate again (statusWriter.gate, which
//     holds the token of the request that started the run): it checks the
//     signed-in user's token (sessionProblem: not expired
//     or revoked, user:full, not expiring before the step's deadline and
//     its restore) and that they are still a cluster-admin
//     (mutationPermission); otherwise the run stops with sign_in_again (or
//     the code sessionProblem gives) or forbidden before the step starts;
//  3. gives the step its own context, WithTimeout(run, OperationDeadline),
//     the budget every operation has, a copy of the user's client on it,
//     and a ServiceAccount bookkeeper made now (the ServiceAccount token
//     rotates);
//  4. runs the step's read-only Check, then its Run only when needed, and
//     shows its progress in the operation (the lease, GET
//     /api/operation);
//  5. records the step's outcome; the first failure ends the run as
//     failed. Nothing is retried, but for the bounded retries inside a
//     step.
//
// The lease heartbeat stops a run whose lease was lost with
// cluster.ErrOperationLockLost: the step's context ends (its own rules:
// no restore), and the run ends failed with errorCode lock_lost.
//
// The user's token stays in this process's memory for the whole run
// (never written, never logged; SECURITY.md, Environment recipes). Logging
// out does not revoke it, so it does not stop a run; Stop after this step
// does. After a restart nothing resumes by itself: the new process has no
// token, and the ServiceAccount never acts for the user. The run record
// then still says running with a dead boot ID: GET /api/recipes/run
// reports it interrupted, and Resume runs the steps again, where each
// step's check skips what is set up.
//
// The run keeps one mutation slot (drain.go) from its start to its end, so
// a shutdown waits for the running step (within ShutdownDrainTimeout: one
// step is one operation's budget) and for the run's last record, and the
// run then sees the drain before its next step.

// Seams of the runner; tests replace them.
var (
	recipeSteps          = cluster.RecipeSteps
	planRecipe           = cluster.PlanRecipe
	saveRecipeRun        = cluster.SaveRecipeRun
	readRecipeRun        = cluster.ReadRecipeRun
	recordRecipeActivity = cluster.RecordRecipeActivity
	recipeOwnership      = cluster.RecipeOwnership
	recipeOwnedProjects  = cluster.RecipeOwnedProjects
)

// recipeRecordTimeout bounds one write of the run record. The last one
// runs at the same time as the marker clear (cluster.MarkerClearTimeout,
// which is longer), so a draining pod's last step and its bookkeeping
// still fit cluster.ShutdownDrainTimeout.
const recipeRecordTimeout = 5 * time.Second

// recipeRunCap bounds a whole run: the sum of its steps' budgets. A safety
// net only: each step's own deadline does the real work.
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
	}
	return st
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
	beginHeldOperation(st.op.ID, recipeTarget(rcp, 0, len(steps), ""))
	recipes.mu.Lock()
	recipes.run, recipes.opID, recipes.running, recipes.stop = run, st.op.ID, true, false
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

// recipeTarget is the operation's target: "<recipe>: step k of n
// (<title>)".
func recipeTarget(rcp types.EnvironmentRecipe, i, n int, title string) string {
	t := fmt.Sprintf("%s: step %d of %d", rcp.Metadata.Name, i+1, n)
	if title != "" {
		t += " (" + title + ")"
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
	state         cluster.RecipeState
	steps         []cluster.RecipeStep
	done          chan struct{}
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

// loop runs the steps (see the runner's notes) and returns how the run
// ended. A panic ends it failed.
func (x *recipeExec) loop() (end recipeEnd) {
	current := -1
	defer recoverRun("recipe", func() {
		if current >= 0 {
			x.stepEnded(current, types.RecipeStepFailed, operationPanicMessage, "internal")
		}
		end = recipeEnd{types.RecipeRunFailed, operationPanicMessage, "internal"}
	})
	n := len(x.steps)
	for i, step := range x.steps {
		if end, stop := x.beforeStep(i); stop {
			return end
		}
		current = i
		x.stepStarted(i, step)
		// The gate and the transition write took time: a drain that began
		// meanwhile must not get a whole new step. Nothing that waits lies
		// between this check and the step's deadline (runStep's first
		// line), so a step that starts ends by the drain's start plus
		// OperationDeadline.
		if mutations.isDraining() {
			x.stepEnded(i, types.RecipeStepPending, "", "")
			return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped before %s: the updater is restarting. Resume continues with it once it is back.", step.Title), "restart"}
		}
		status, msg, code := x.runStep(step)
		x.stepEnded(i, status, msg, code)
		if errors.Is(context.Cause(x.runCtx), cluster.ErrOperationLockLost) {
			return recipeEnd{types.RecipeRunFailed, cluster.LockLostMessage, "lock_lost"}
		}
		if status == types.RecipeStepFailed {
			return recipeEnd{types.RecipeRunFailed, fmt.Sprintf("Step %d of %d (%s) failed: %s", i+1, n, step.Title, msg), code}
		}
		current = -1
	}
	return recipeEnd{state: types.RecipeRunSucceeded, reason: fmt.Sprintf("Every step of recipe %s is set up.", x.state.Recipe.Metadata.Name)}
}

// beforeStep decides whether step i may start (the runner's notes, points
// 1 and 2).
func (x *recipeExec) beforeStep(i int) (recipeEnd, bool) {
	title := x.steps[i].Title
	where := "before its first step"
	if i > 0 {
		where = "after " + x.steps[i-1].Title
	}
	switch {
	case recipes.stopRequested():
		return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped %s, as asked. Resume continues with %s.", where, title), "stopped"}, true
	case mutations.isDraining():
		return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped %s: the updater is restarting. Resume continues with %s once it is back.", where, title), "restart"}, true
	case errors.Is(context.Cause(x.runCtx), cluster.ErrOperationLockLost):
		return recipeEnd{types.RecipeRunFailed, cluster.LockLostMessage, "lock_lost"}, true
	case x.runCtx.Err() != nil:
		return recipeEnd{types.RecipeRunFailed, fmt.Sprintf("Stopped %s: the run took longer than its limit (%s).", where, recipeRunCap(len(x.steps))), "timeout"}, true
	}
	if msg, code := x.gate(x.runCtx); msg != "" {
		return recipeEnd{types.RecipeRunStopped, fmt.Sprintf("Stopped before %s: %s", title, msg), code}, true
	}
	return recipeEnd{}, false
}

// runStep runs the step on its own deadline, client and bookkeeper (point 3
// and 4) and returns its status, message and errorCode.
func (x *recipeExec) runStep(step cluster.RecipeStep) (string, string, string) {
	stepCtx, cancel := context.WithTimeout(x.runCtx, cluster.OperationDeadline)
	defer cancel()
	c := x.client.WithContext(stepCtx)
	c.SetBookkeeping(x.newBookkeeper(stepCtx))
	emit := func(e cluster.UpdateStepEvent) {
		inflight.recordStep(x.opID, UpdateStep{Step: e.Step, Status: e.Status, Message: e.Message, Detail: e.Detail, ErrorCode: e.ErrorCode})
	}
	emit(cluster.UpdateStepEvent{Step: step.ID, Status: "running", Message: fmt.Sprintf("Checking %s...", step.Title)})
	chk, err := step.Check(c, &x.state)
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
	res, err := step.Run(c, &x.state, emit)
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

// stepStarted marks step i running, shows it in the operation, and
// records the transition: the previous step's outcome and this start, in
// one write.
func (x *recipeExec) stepStarted(i int, step cluster.RecipeStep) {
	run := recipes.update(func(run *types.RecipeRun) {
		run.Steps[i].Status, run.Steps[i].StartedAt = types.RecipeStepRunning, time.Now().UTC().Format(time.RFC3339)
		run.Steps[i].Message, run.Steps[i].ErrorCode, run.Steps[i].FinishedAt = "", "", ""
	})
	inflight.update(x.opID, func(op *Operation) {
		op.Target = truncateText(recipeTarget(x.state.Recipe, i, len(x.steps), step.Title), maxOperationTargetBytes)
	})
	x.save(run)
}

// stepEnded records step i's outcome in memory (the next start or the
// run's end writes it) and takes over what the step learnt (the project).
func (x *recipeExec) stepEnded(i int, status, msg, code string) {
	recipes.update(func(run *types.RecipeRun) {
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
	})
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
	run := recipes.update(func(run *types.RecipeRun) {
		run.State, run.StopReason, run.ErrorCode = end.state, end.reason, end.code
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
	// Not running any more before the lock is free: a run started as soon
	// as it is (also while this one's bookkeeping still runs) owns the
	// runner's state from then on, and nothing below touches it.
	recipes.mu.Lock()
	if recipes.opID == x.opID {
		recipes.running = false
	}
	recipes.mu.Unlock()
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
