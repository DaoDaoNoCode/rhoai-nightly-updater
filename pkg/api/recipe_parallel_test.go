package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Concurrent recipe steps: scheduling belongs to the runner.
// Stub steps block on release channels the test owns, so overlap is
// orchestrated by the test rather than by elapsed time.

// watchRecords hooks the fake so each persisted record is visible to the test.
func watchRecords(f *recipeFake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watch = make(chan *types.RecipeRun, 1000)
}

// waitRecord blocks until a saved record satisfies ok.
func waitRecord(t *testing.T, f *recipeFake, ok func(*types.RecipeRun) bool) *types.RecipeRun {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case run := <-f.watch:
			if ok(run) {
				return run
			}
		case <-deadline:
			t.Fatalf("no record write as expected; the last is %s", stepStatuses(f.saved()))
			return nil
		}
	}
}

// statusesAre yields a matcher requiring step statuses on the record to equal want.
func statusesAre(want string) func(*types.RecipeRun) bool {
	return func(run *types.RecipeRun) bool { return stepStatuses(run) == want }
}

// started is true once s has entered Run.
func started(s *fakeRecipeStep) bool {
	select {
	case <-s.started:
		return true
	default:
		return false
	}
}

// parallel limits s's predecessors to the listed step IDs.
func parallel(s *fakeRecipeStep, after ...string) *fakeRecipeStep {
	s.after = append([]string{}, after...)
	return s
}

func endRun(t *testing.T, done chan struct{}, steps ...*fakeRecipeStep) {
	t.Helper()
	for _, s := range steps {
		close(s.release)
	}
	waitDone(t, done)
}

// Branches sharing one parent overlap: both sit inside Run, still unreleased.
// Each step blocks on its own dependency, not on the sibling lane; the
// join step waits for both branches. Active steps surface on the record,
// the in-flight operation, and GET /api/recipes/run.
func TestRecipeRun_IndependentLanesOverlap(t *testing.T) {
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	a1, b1 := parallel(f.addStep("a1"), "head"), parallel(f.addStep("b1"), "head")
	a2 := parallel(f.addStep("a2"), "a1")
	last := parallel(f.addStep("last"), "a2", "b1")
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a1)
	waitStarted(t, b1) // overlap active: a1 not released yet

	waitRecord(t, f, statusesAre("head=success a1=running b1=running a2=pending last=pending"))
	if op := inflight.snapshot(); op == nil || op.Target != "dev: 1 of 5 steps done; running: Step a1, Step b1" {
		t.Errorf("operation %+v", op)
	}
	st := recipeRunStatus(nil, nil)
	if st.Progress["a1"].Message != "working on a1" || st.Progress["b1"].Message != "working on b1" || len(st.Progress) != 2 {
		t.Errorf("progress %+v", st.Progress)
	}

	// b1 completes; a2 remains blocked on a1 and last on a2.
	close(b1.release)
	waitRecord(t, f, statusesAre("head=success a1=running b1=success a2=pending last=pending"))
	if started(a2) || started(last) {
		t.Error("a step started before the step it comes after ended")
	}
	if st := recipeRunStatus(nil, nil); len(st.Progress) != 1 || st.Progress["b1"].Message != "" {
		t.Errorf("progress of an ended step is kept: %+v", st.Progress)
	}
	close(a1.release)
	waitStarted(t, a2)
	if started(last) {
		t.Error("the last step started before a2 ended")
	}
	close(a2.release)
	waitStarted(t, last)
	endRun(t, done, last)

	run := f.saved()
	if run.State != types.RecipeRunSucceeded || stepStatuses(run) != "head=success a1=success b1=success a2=success last=success" {
		t.Errorf("final record %+v (%s)", run, stepStatuses(run))
	}
	if f.maxRunning != 2 {
		t.Errorf("at most %d steps ran at once, want 2", f.maxRunning)
	}
	assertReleased(t, rec)
}

// No more than three steps run concurrently; additional steps enter in
// recipe order whenever a slot opens.
func TestRecipeRun_AtMostThreeStepsAtOnce(t *testing.T) {
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	var lanes []*fakeRecipeStep
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		lanes = append(lanes, parallel(f.addStep(id), "head"))
	}
	close(head.release)
	_, done := startRun(t)
	for _, s := range lanes[:3] {
		waitStarted(t, s)
	}
	waitRecord(t, f, statusesAre("head=success p1=running p2=running p3=running p4=pending p5=pending"))
	if started(lanes[3]) || started(lanes[4]) {
		t.Error("a fourth step started while three ran")
	}
	close(lanes[1].release)
	waitStarted(t, lanes[3])
	if started(lanes[4]) {
		t.Error("p5 started before p4, or a fourth step ran")
	}
	endRun(t, done, lanes[0], lanes[2], lanes[3], lanes[4])
	if run := f.saved(); run.State != types.RecipeRunSucceeded {
		t.Errorf("record %+v", run)
	}
	if f.maxRunning != cluster.RecipeMaxRunningSteps || cluster.RecipeMaxRunningSteps != 3 {
		t.Errorf("at most %d steps ran at once, limit %d", f.maxRunning, cluster.RecipeMaxRunningSteps)
	}
	assertReleased(t, rec)
}

// Once a step fails nothing new is scheduled; in-flight steps run to
// completion (outcomes recorded), then the run fails. StopReason cites the
// earliest failure in plan order and lists any additional failures.
func TestRecipeRun_FailureLetsRunningStepsFinish(t *testing.T) {
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	a, b, c := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head"), parallel(f.addStep("c"), "head")
	a2 := parallel(f.addStep("a2"), "c")
	a.result = &types.OperationResponse{Success: false, Message: "The S3 storage is not ready.", ErrorCode: "prerequisites"}
	b.result = &types.OperationResponse{Success: false, Message: "The model registry did not start.", ErrorCode: "timeout"}
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	waitStarted(t, c)

	// b finishes before a despite plan order—the run still attributes the
	// primary failure to a.
	close(b.release)
	waitRecord(t, f, statusesAre("head=success a=running b=failed c=running a2=pending"))
	// While a and c finish, the status says a step failed and none starts.
	if st := recipeRunStatus(nil, nil); !st.Running || st.Ending != "failed" || stepStatuses(st.Run) != "head=success a=running b=failed c=running a2=pending" {
		t.Errorf("status while the run ends: running %v, ending %q, %s", st.Running, st.Ending, stepStatuses(st.Run))
	}
	close(c.release) // c done might admit a2, but failure already doomed the run
	waitRecord(t, f, statusesAre("head=success a=running b=failed c=success a2=pending"))
	close(a.release)
	waitDone(t, done)

	run := f.saved()
	want := "Step 2 of 5 (Step a) failed: The S3 storage is not ready. Also failed: step 3 of 5 (Step b)."
	if run.State != types.RecipeRunFailed || run.ErrorCode != "prerequisites" || run.StopReason != want {
		t.Errorf("record %+v\nreason %q\nwant   %q", run, run.StopReason, want)
	}
	if stepStatuses(run) != "head=success a=failed b=failed c=success a2=pending" || a2.ran || run.Steps[2].Message != "The model registry did not start." {
		t.Errorf("steps %s, a2 ran %v", stepStatuses(run), a2.ran)
	}
	assertReleased(t, rec)
}

// User Stop while two steps execute: both complete, nothing else launches,
// run becomes stopped when the slower exits. Resume retries incomplete work;
// successful steps short-circuit through their guards.
func TestRecipeRun_StopWithTwoStepsInFlightThenResume(t *testing.T) {
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	a, b := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head")
	a2 := parallel(f.addStep("a2"), "a")
	close(head.release)
	runID, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	w := httptest.NewRecorder()
	HandleRecipeStop(w, postJSON(t, "/api/recipes/stop", "user:admin", "{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	close(a.release)
	waitRecord(t, f, statusesAre("head=success a=success b=running a2=pending"))
	if started(a2) {
		t.Error("a step started after Stop")
	}
	if st := recipeRunStatus(nil, nil); !st.Running || !st.StopRequested || st.Ending != "stopped" {
		t.Errorf("status while b finishes: %+v", st)
	}
	close(b.release)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunStopped || run.ErrorCode != "stopped" || stepStatuses(run) != "head=success a=success b=success a2=pending" || a2.ran {
		t.Errorf("record %+v (%s)", run, stepStatuses(run))
	}
	if want := "Stopped after Step b, as asked. Resume continues with Step a2."; run.StopReason != want {
		t.Errorf("reason %q, want %q", run.StopReason, want)
	}
	assertReleased(t, rec)

	f.mu.Lock()
	f.steps = nil
	f.mu.Unlock()
	f.addStep("head").done = true
	parallel(f.addStep("a"), "head").done = true
	parallel(f.addStep("b"), "head").done = true
	r2 := parallel(f.addStep("a2"), "a")
	close(r2.release)
	resumeRun(t, runID)
	if run := f.saved(); run.State != types.RecipeRunSucceeded || stepStatuses(run) != "head=skipped a=skipped b=skipped a2=success" || !r2.ran {
		t.Errorf("resumed %s: %+v", stepStatuses(run), run)
	}
}

// Drain during two active steps blocks until both finish; scheduling stops
// once drain begins and the run ends stopped (restart). A fresh runner
// instance continues via Resume.
func TestRecipeRun_DrainWithStepsInFlightThenResume(t *testing.T) {
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	a, b := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head")
	a2 := parallel(f.addStep("a2"), "a")
	close(head.release)
	runID, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	wait := mutations.startDrain()
	drained := make(chan int)
	go func() { drained <- wait(context.Background()) }()
	close(a.release)
	waitRecord(t, f, statusesAre("head=success a=success b=running a2=pending"))
	select {
	case <-drained:
		t.Fatal("the drain did not wait for the running step")
	default:
	}
	close(b.release)
	if left := <-drained; left != 0 {
		t.Errorf("%d mutations left after the drain", left)
	}
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunStopped || run.ErrorCode != "restart" || a2.ran ||
		run.StopReason != "Stopped after Step b: the updater is restarting. Resume continues with Step a2 once it is back." {
		t.Errorf("record %+v (%s)", run, stepStatuses(run))
	}
	assertReleased(t, rec)

	// Stand in for a restarted process.
	mutations, recipes = &mutationTracker{}, &recipeRunner{}
	f.mu.Lock()
	f.steps = nil
	f.mu.Unlock()
	f.addStep("head").done = true
	parallel(f.addStep("a"), "head").done = true
	parallel(f.addStep("b"), "head").done = true
	r2 := parallel(f.addStep("a2"), "a")
	close(r2.release)
	resumeRun(t, runID)
	if run := f.saved(); run.State != types.RecipeRunSucceeded || stepStatuses(run) != "head=skipped a=skipped b=skipped a2=success" {
		t.Errorf("resumed %s: %+v", stepStatuses(run), run)
	}
}

// Orphaned parallel run: pod died with two steps still executing (record
// still running, BootID unknown to any live process). Status is interrupted;
// Resume re-runs each step that never reached a terminal state.
func TestRecipeResume_AnInterruptedParallelRun(t *testing.T) {
	f, rec := installRecipeFake(t)
	var rcp types.EnvironmentRecipe
	if err := json.Unmarshal([]byte(testRecipe), &rcp); err != nil {
		t.Fatal(err)
	}
	f.record = &types.RecipeRun{
		ID: "run-1", OperationID: "op-gone", BootID: "boot-gone", User: "admin", State: types.RecipeRunRunning,
		StartedAt: time.Now().UTC().Format(time.RFC3339), Recipe: rcp,
		Steps: []types.RecipeRunStep{{ID: "head", Status: types.RecipeStepSuccess}, {ID: "a", Status: types.RecipeStepRunning},
			{ID: "b", Status: types.RecipeStepRunning}, {ID: "a2", Status: types.RecipeStepPending}},
		Components: []string{"aipipelines"}, ComponentIDs: map[string]string{"aipipelines": "en-aipipelines"},
	}
	if st := recipeRunStatus(nil, nil); !st.Interrupted || st.Running {
		t.Fatalf("status %+v", st)
	}
	f.addStep("head").done = true
	a := parallel(f.addStep("a"), "head")
	a.done = true
	b := parallel(f.addStep("b"), "head")
	b.components = []string{"mlflowoperator"}
	a2 := parallel(f.addStep("a2"), "a")
	close(b.release)
	close(a2.release)
	resumeRun(t, "run-1")
	run := f.saved()
	if run.ID != "run-1" || run.State != types.RecipeRunSucceeded || stepStatuses(run) != "head=skipped a=skipped b=success a2=success" || !a2.ran {
		t.Errorf("resumed %s: %+v", stepStatuses(run), run)
	}
	if strings.Join(run.Components, ",") != "aipipelines,mlflowoperator" {
		t.Errorf("components %v", run.Components)
	}
	assertReleased(t, rec)
}

// Worker panic fails only that step (internal); the peer keeps running,
// the process survives, then the run fails and releases all held resources.
func TestRecipeRun_WorkerPanicFailsItsStep(t *testing.T) {
	logs := captureLog(t)
	f, rec := installRecipeFake(t)
	watchRecords(f)
	head := f.addStep("head")
	a, b := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head")
	a.panics = true
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	close(a.release)
	waitRecord(t, f, statusesAre("head=success a=failed b=running"))
	close(b.release)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "internal" || stepStatuses(run) != "head=success a=failed b=success" ||
		run.Steps[1].Message != operationPanicMessage || !strings.HasPrefix(run.StopReason, "Step 2 of 3 (Step a) failed: ") {
		t.Errorf("record %+v (%s)", run, stepStatuses(run))
	}
	assertReleased(t, rec)
	if !strings.Contains(logs.String(), "step bug") || !strings.Contains(logs.String(), "recipe step a") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("no panic stack in the log: %s", logs.String())
	}
}

// Lease loss cancels each active step via context; after the last one exits
// the run fails with lock_lost and no further steps are started.
func TestRecipeRun_LeaseLossCancelsEveryRunningStep(t *testing.T) {
	f, rec := installRecipeFake(t)
	head := f.addStep("head")
	a, b := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head")
	a2 := parallel(f.addStep("a2"), "a")
	a.waitsCtx, b.waitsCtx = true, true
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	rec.setLease(otherPodLease(clk.Now()))
	rec.beat(t)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "lock_lost" || run.StopReason != cluster.LockLostMessage || a2.ran {
		t.Errorf("record %+v", run)
	}
	for _, s := range run.Steps[1:3] {
		if s.Status != types.RecipeStepFailed || s.ErrorCode != "lock_lost" {
			t.Errorf("step %+v", s)
		}
	}
}

// Concurrent steps may each touch RecipeRun State; every change must appear
// on the final record. Steps mutate isolated copies (race builds prove no
// aliasing), merged back when the step completes.
func TestRecipeRun_StateOfConcurrentStepsAllLands(t *testing.T) {
	f, _ := installRecipeFake(t)
	head := f.addStep("head")
	a := parallel(f.addStep("a"), "head")
	a.components = []string{"aipipelines"}
	a.created = []types.RecipeCreatedPart{{Part: types.RecipePartMaaSPlatform, UID: "creds-1"}}
	b := parallel(f.addStep("b"), "head")
	b.components = []string{"modelregistry"}
	b.created = []types.RecipeCreatedPart{{Part: types.RecipePartModelRegistry, UID: "reg-1"}}
	c := parallel(f.addStep("c"), "head")
	c.components = []string{"mlflowoperator"}
	c.exposure = "maas"
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	waitStarted(t, c)
	endRun(t, done, c, a, b)
	run := f.saved()
	if strings.Join(run.Components, ",") != "aipipelines,mlflowoperator,modelregistry" ||
		!reflect.DeepEqual(run.ComponentIDs, map[string]string{"aipipelines": "en-aipipelines", "mlflowoperator": "en-mlflowoperator", "modelregistry": "en-modelregistry"}) {
		t.Errorf("components %v %v", run.Components, run.ComponentIDs)
	}
	if want := []types.RecipeCreatedPart{{Part: types.RecipePartMaaSPlatform, UID: "creds-1"}, {Part: types.RecipePartModelRegistry, UID: "reg-1"}}; !reflect.DeepEqual(sortedParts(run.CreatedParts), sortedParts(want)) {
		t.Errorf("created %+v", run.CreatedParts)
	}
	if run.Exposure != "maas" {
		t.Errorf("exposure %q", run.Exposure)
	}
}

func sortedParts(p []types.RecipeCreatedPart) map[string]string {
	out := map[string]string{}
	for _, x := range p {
		out[x.Part] = x.UID
	}
	return out
}

// A predecessor ID missing from the run (legacy record, disabled recipe step)
// is ignored—the successor does not block waiting for it.
func TestRecipeRun_AfterAMissingStepDoesNotWait(t *testing.T) {
	f, _ := installRecipeFake(t)
	a := parallel(f.addStep("a"), "gone")
	_, done := startRun(t)
	waitStarted(t, a)
	endRun(t, done, a)
	if run := f.saved(); run.State != types.RecipeRunSucceeded {
		t.Errorf("record %+v", run)
	}
}

// A failure and a success that are both queued before the loop takes
// either: the loop takes every queued end before it starts anything, so
// the step after the success does not start.
func TestRecipeRun_QueuedFailureStopsStarts(t *testing.T) {
	f, rec := installRecipeFake(t)
	head := f.addStep("head")
	a, b, c := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head"), parallel(f.addStep("c"), "head")
	d := parallel(f.addStep("d"), "a")
	close(d.release) // it must not start; if it does, the run still ends
	b.result = &types.OperationResponse{Success: false, Message: "b broke", ErrorCode: "timeout"}
	queued := make(chan int, 10)
	stepResultQueued = func(i int) { queued <- i }
	t.Cleanup(func() { stepResultQueued = nil })
	// The write after c's end blocks the loop until a and b both ended.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	origSave := saveRecipeRun
	saveRecipeRun = func(cl *cluster.Client, run *types.RecipeRun, replace bool) error {
		if stepStatuses(run) == "head=success a=running b=running c=success d=pending" {
			close(blocked)
			<-unblock
		}
		return origSave(cl, run, replace)
	}
	close(head.release)
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	waitStarted(t, c)
	close(c.release)
	<-blocked
	// a's success is queued first, then b's failure: taking only the
	// first would start d.
	waitQueued := func(want int) {
		for got := <-queued; got != want; got = <-queued {
		}
	}
	close(a.release)
	waitQueued(1)
	close(b.release)
	waitQueued(2)
	close(unblock)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || stepStatuses(run) != "head=success a=success b=failed c=success d=pending" || d.ran {
		t.Errorf("record %s, d ran %v: %+v", stepStatuses(run), d.ran, run)
	}
	assertReleased(t, rec)
}

// Stop accepted while the runner writes the record of a batch it is about
// to start: the batch goes back to pending and does not start.
func TestRecipeRun_StopDuringTheStartWrite(t *testing.T) {
	f, rec := installRecipeFake(t)
	head := f.addStep("head")
	a := parallel(f.addStep("a"), "head")
	close(head.release)
	origSave := saveRecipeRun
	saveRecipeRun = func(cl *cluster.Client, run *types.RecipeRun, replace bool) error {
		if stepStatuses(run) == "head=success a=running" {
			recipes.requestStop()
		}
		return origSave(cl, run, replace)
	}
	_, done := startRun(t)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunStopped || run.ErrorCode != "stopped" || stepStatuses(run) != "head=success a=pending" || a.ran || len(a.clients) != 0 ||
		run.StopReason != "Stopped after Step head, as asked. Resume continues with Step a." {
		t.Errorf("record %s: %+v", stepStatuses(run), run)
	}
	assertReleased(t, rec)
}

// A panic of the loop while it starts a batch: the running step ends and
// keeps its outcome and its State changes, the step marked running for the
// start that never happened counts as failed, and no step is left running
// in the record.
func TestRecipeRun_LoopPanicDuringAStart(t *testing.T) {
	captureLog(t)
	f, rec := installRecipeFake(t)
	head := f.addStep("head")
	a, b := parallel(f.addStep("a"), "head"), parallel(f.addStep("b"), "head")
	a.components, b.components = []string{"aipipelines"}, []string{"mlflowoperator"}
	c := parallel(f.addStep("c"), "a")
	close(head.release)
	origSave := saveRecipeRun
	panicked := false
	saveRecipeRun = func(cl *cluster.Client, run *types.RecipeRun, replace bool) error {
		if !panicked && stepStatuses(run) == "head=success a=success b=running c=running" {
			panicked = true
			close(b.release)
			panic("record bug")
		}
		return origSave(cl, run, replace)
	}
	_, done := startRun(t)
	waitStarted(t, a)
	waitStarted(t, b)
	close(a.release)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "internal" || stepStatuses(run) != "head=success a=success b=success c=failed" || c.ran {
		t.Errorf("record %s: %+v", stepStatuses(run), run)
	}
	if strings.Join(run.Components, ",") != "aipipelines,mlflowoperator" {
		t.Errorf("components %v", run.Components)
	}
	assertReleased(t, rec)
}
