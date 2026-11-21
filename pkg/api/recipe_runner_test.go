package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// testRecipe is a valid MVP recipe.
const testRecipe = `{"apiVersion":"rhoai-nightly-updater/v1alpha1","kind":"EnvironmentRecipe","metadata":{"name":"dev"},
"spec":{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true},"models":{"enabled":false},"playground":{"enabled":false},"maas":{"enabled":false}},
"dashboardDev":{}}}`

var testPinned = types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:aaa", Digest: "sha256:aaa"}

// recipeFake is the cluster of the runner tests: the run record, the
// plan's answer, and steps that each wait for the test.
type recipeFake struct {
	mu       sync.Mutex
	record   *types.RecipeRun
	saves    int
	reads    int
	pinnedIn []*types.RecipePinnedBuild // the pinned build each plan got
	problems []types.RecipeProblem
	steps    []*fakeRecipeStep
}

// fakeRecipeStep is one step: Check reports done when done is set; Run
// closes started, then waits for release (or its context) and returns
// result. The clients it was given are kept.
type fakeRecipeStep struct {
	id       string
	done     bool
	result   *types.OperationResponse
	panics   bool
	started  chan struct{}
	release  chan struct{}
	ran      bool
	clients  []*cluster.Client
	onRun    func()
	waitsCtx bool
}

func (f *recipeFake) addStep(id string) *fakeRecipeStep {
	s := &fakeRecipeStep{id: id, result: &types.OperationResponse{Success: true, Message: id + " set up"}, started: make(chan struct{}), release: make(chan struct{})}
	f.steps = append(f.steps, s)
	return s
}

func (f *recipeFake) clusterSteps() []cluster.RecipeStep {
	var out []cluster.RecipeStep
	for _, s := range f.steps {
		out = append(out, cluster.RecipeStep{
			ID: s.id, Title: "Step " + s.id, Typical: time.Minute, Max: 15 * time.Minute,
			Check: func(c *cluster.Client, _ *cluster.RecipeState) (cluster.RecipeCheck, error) {
				f.mu.Lock()
				defer f.mu.Unlock()
				s.clients = append(s.clients, c)
				return cluster.RecipeCheck{Done: s.done, Reason: s.id + " is there"}, nil
			},
			Run: func(c *cluster.Client, _ *cluster.RecipeState, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
				f.mu.Lock()
				s.ran = true
				f.mu.Unlock()
				emit(cluster.UpdateStepEvent{Step: s.id + "-inner", Status: "running", Message: "working on " + s.id})
				if s.onRun != nil {
					s.onRun()
				}
				close(s.started)
				if s.waitsCtx {
					select {
					case <-s.release:
					case <-c.Context().Done():
						return &types.OperationResponse{Success: false, Message: "stopped"}, c.Context().Err()
					}
				} else {
					<-s.release
				}
				if s.panics {
					panic("step bug")
				}
				return s.result, nil
			},
		})
	}
	return out
}

func (f *recipeFake) saved() *types.RecipeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record == nil {
		return nil
	}
	return copyRecipeRun(f.record)
}

// installRecipeFake replaces the runner's seams, gives the test a fresh
// runner and mutation tracker, and runs in the cluster (a ServiceAccount
// token, a user:full sign-in valid for a day).
func installRecipeFake(t *testing.T) (*recipeFake, *fakeRecord) {
	t.Helper()
	inCluster(t)
	allowMutations(t)
	fakeSession(t, fullToken(sessionNow.Add(-time.Hour)), nil)
	rec := installFakeRecord(t)
	f := &recipeFake{}
	origSteps, origPlan, origSave, origRead, origActivity := recipeSteps, planRecipe, saveRecipeRun, readRecipeRun, recordRecipeActivity
	origMutations, origRecipes := mutations, recipes
	mutations, recipes = &mutationTracker{}, &recipeRunner{}
	t.Cleanup(func() {
		recipeSteps, planRecipe, saveRecipeRun, readRecipeRun, recordRecipeActivity = origSteps, origPlan, origSave, origRead, origActivity
		mutations, recipes = origMutations, origRecipes
	})
	recipeSteps = func(types.EnvironmentRecipe) []cluster.RecipeStep { return f.clusterSteps() }
	planRecipe = func(_ *cluster.Client, r types.EnvironmentRecipe, pinned *types.RecipePinnedBuild) (*types.RecipePlan, cluster.RecipeState) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pinnedIn = append(f.pinnedIn, pinned)
		if pinned == nil {
			p := testPinned
			pinned = &p
		}
		return &types.RecipePlan{Recipe: r, Problems: append([]types.RecipeProblem{}, f.problems...), Pinned: pinned},
			cluster.RecipeState{Recipe: r, Pinned: pinned, Project: &types.RecipeProjectRef{Name: "recipe-dev"}}
	}
	saveRecipeRun = func(c *cluster.Client, run *types.RecipeRun, replace bool) error {
		if c.ActsAsUser() {
			t.Error("the run record was written with the user's token")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.saves++
		if !replace && f.record != nil && f.record.ID != run.ID {
			return cluster.ErrRecipeRunReplaced
		}
		f.record = copyRecipeRun(run)
		return nil
	}
	readRecipeRun = func(*cluster.Client) (*types.RecipeRun, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads++
		if f.record == nil {
			return nil, nil
		}
		return copyRecipeRun(f.record), nil
	}
	recordRecipeActivity = func(c *cluster.Client, _, _ string, _ bool) {
		if c.ActsAsUser() {
			t.Error("the activity was written with the user's token")
		}
	}
	return f, rec
}

// startRun posts a run and returns its ID and the channel that closes
// when it ended.
func startRun(t *testing.T) (string, chan struct{}) {
	t.Helper()
	w := httptest.NewRecorder()
	HandleRecipeRun(w, postJSON(t, "/api/recipes/run", "user:admin", `{"recipe":`+testRecipe+`}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	var body types.RecipeRunAccepted
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.RunID == "" {
		t.Fatalf("run answer %s", w.Body.String())
	}
	recipes.mu.Lock()
	done := recipes.done
	recipes.mu.Unlock()
	return body.RunID, done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end")
	}
}

func waitStarted(t *testing.T, s *fakeRecipeStep) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(30 * time.Second):
		t.Fatalf("step %s did not start", s.id)
	}
}

func stepStatuses(run *types.RecipeRun) string {
	var out []string
	for _, s := range run.Steps {
		out = append(out, s.ID+"="+s.Status)
	}
	return strings.Join(out, " ")
}

// assertReleased: the run gave back the lock, the lease, its mutation
// slot, and its marker became the last completed operation.
func assertReleased(t *testing.T, rec *fakeRecord) {
	t.Helper()
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Error("the lock or the operation was left behind")
	}
	mutations.mu.Lock()
	active := mutations.active
	mutations.mu.Unlock()
	if active != 0 {
		t.Errorf("%d mutation slots left behind", active)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.marker != nil || rec.lease != nil {
		t.Errorf("marker %+v, lease %+v left behind", rec.marker, rec.lease)
	}
}

// The steps run in order, each with its own deadline, a copy of the
// user's client and a bookkeeper made with the ServiceAccount token as it
// is then. The lock is held from the first step to the last: a teammate's
// change gets cluster_busy and the background MaaS check skips. The
// record and the operation show the progress; the end frees everything.
func TestRecipeRun_StepsInOrderHoldingTheLock(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2, s3 := f.addStep("one"), f.addStep("two"), f.addStep("three")
	s1.onRun = func() {
		cachedTokenMu.Lock()
		cachedTokenValue = "sa-token-rotated"
		cachedTokenMu.Unlock()
	}
	runID, done := startRun(t)
	waitStarted(t, s1)

	// Busy for others, while the run holds the lock.
	w := httptest.NewRecorder()
	HandleMLflowSetup(w, postJSON(t, "/api/resources/mlflow/setup", "user:bob", "{}"))
	if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "cluster_busy" || !strings.Contains(w.Body.String(), "Environment recipe") {
		t.Errorf("a teammate's change during the run: %d %s", w.Code, w.Body.String())
	}
	if runMaaSPruneOnce(context.Background()) {
		t.Error("the background MaaS check ran during the run")
	}
	op := inflight.snapshot()
	if op == nil || op.Type != "recipe" || op.Label != "Environment recipe" || op.Target != "dev: step 1 of 3 (Step one)" || op.Step != "one-inner" {
		t.Errorf("operation %+v", op)
	}
	if st := recipeRunStatus(nil); !st.Running || st.Run.ID != runID || st.Step == nil || st.Step.Step != "one-inner" {
		t.Errorf("status during the run %+v", st)
	}
	close(s1.release)
	waitStarted(t, s2)
	if got := inflight.snapshot(); got == nil || got.ID != op.ID {
		t.Errorf("the run's operation changed between steps: %+v", got)
	}
	if run := f.saved(); stepStatuses(run) != "one=success two=running three=pending" {
		t.Errorf("record at step two: %s", stepStatuses(run))
	}
	close(s2.release)
	close(s3.release)
	waitDone(t, done)

	run := f.saved()
	if run.State != types.RecipeRunSucceeded || stepStatuses(run) != "one=success two=success three=success" || run.ID != runID || run.Pinned == nil || run.Pinned.Digest != "sha256:aaa" {
		t.Errorf("final record %+v (%s)", run, stepStatuses(run))
	}
	for i, s := range []*fakeRecipeStep{s1, s2, s3} {
		c := s.clients[0]
		dl, ok := c.Context().Deadline()
		if !c.ActsAsUser() || !ok || time.Until(dl) > cluster.OperationDeadline {
			t.Errorf("step %d: as user %v, deadline %v %v", i, c.ActsAsUser(), dl, ok)
		}
		bk := c.Bookkeeping()
		want := "sa-token"
		if i > 0 {
			want = "sa-token-rotated"
		}
		if bk.ActsAsUser() || bk.TestOptions().Token != want {
			t.Errorf("step %d: bookkeeper as user %v with token %q, want %q", i, bk.ActsAsUser(), bk.TestOptions().Token, want)
		}
	}
	if s1.clients[0].Context() == s2.clients[0].Context() {
		t.Error("two steps shared one context")
	}
	assertReleased(t, rec)
	rec.mu.Lock()
	last := rec.last
	rec.mu.Unlock()
	if last == nil || !last.Success || last.Type != "recipe" {
		t.Errorf("last completed %+v", last)
	}
	// A new change goes ahead once the run ended.
	if !runMaaSPruneOnce(context.Background()) {
		t.Error("the background MaaS check still skips after the run")
	}
}

// The first failure ends the run; later steps never start.
func TestRecipeRun_FirstFailureStops(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2, s3 := f.addStep("one"), f.addStep("two"), f.addStep("three")
	s2.result = &types.OperationResponse{Success: false, Message: "The S3 storage is not ready.", ErrorCode: "prerequisites"}
	close(s1.release)
	close(s2.release)
	_, done := startRun(t)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "prerequisites" || !strings.Contains(run.StopReason, "Step 2 of 3 (Step two) failed: The S3 storage is not ready.") {
		t.Errorf("record %+v", run)
	}
	if stepStatuses(run) != "one=success two=failed three=pending" || s3.ran || run.Steps[1].Message != "The S3 storage is not ready." {
		t.Errorf("steps %s, third ran %v", stepStatuses(run), s3.ran)
	}
	assertReleased(t, rec)
}

// Stop after this step: the running step finishes, the next does not
// start. Stop takes no lock: another stop while none runs is 409.
func TestRecipeRun_StopAfterThisStep(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2 := f.addStep("one"), f.addStep("two")
	_, done := startRun(t)
	waitStarted(t, s1)
	for range 2 { // idempotent
		w := httptest.NewRecorder()
		HandleRecipeStop(w, postJSON(t, "/api/recipes/stop", "user:admin", "{}"))
		if w.Code != http.StatusOK {
			t.Fatalf("stop: %d %s", w.Code, w.Body.String())
		}
	}
	if st := recipeRunStatus(nil); !st.StopRequested {
		t.Error("the status does not show the stop")
	}
	close(s1.release)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunStopped || run.ErrorCode != "stopped" || stepStatuses(run) != "one=success two=pending" || s2.ran {
		t.Errorf("record %+v (%s)", run, stepStatuses(run))
	}
	assertReleased(t, rec)
	w := httptest.NewRecorder()
	HandleRecipeStop(w, postJSON(t, "/api/recipes/stop", "user:admin", "{}"))
	if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "not_running" {
		t.Errorf("stop with no run: %d %s", w.Code, w.Body.String())
	}
}

// POST /api/recipes/stop is gated like every change but takes no lock and
// makes no cluster call: while another operation holds the lock it only
// answers whether a run runs here, touching neither the lock nor any
// record, and it does not use up the rate-limit slot.
func TestRecipeStopTakesNoLockAndMakesNoClusterCall(t *testing.T) {
	f, rec := installRecipeFake(t)
	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	defer clusterMutationInProgress.Store(false)
	mux := http.NewServeMux()
	Register(mux)
	for range 2 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSON(t, "/api/recipes/stop", "user:admin", "{}"))
		if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "not_running" {
			t.Fatalf("stop: %d %s", w.Code, w.Body.String())
		}
	}
	if !clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Error("stop touched the lock")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if f.saves != 0 || f.reads != 0 || rec.attempts != 0 || rec.marker != nil {
		t.Errorf("stop made cluster calls: saves %d reads %d lease writes %d", f.saves, f.reads, rec.attempts)
	}
	if mutationLimiter.isRateLimited("admin:/api/recipes/stop") {
		t.Error("stop used up the rate-limit slot")
	}
	if _, ok := lockFreeMutationRoutes["POST /api/recipes/stop"]; !ok {
		t.Error("stop is not listed as lock-free")
	}
}

// A shutdown during a step waits for the step, then the run ends as
// stopped (restart) before its next step, its marker cleared: it is not
// reported as interrupted.
func TestRecipeRun_DrainStopsBeforeTheNextStep(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2 := f.addStep("one"), f.addStep("two")
	_, done := startRun(t)
	waitStarted(t, s1)
	wait := mutations.startDrain()
	drained := make(chan int)
	go func() { drained <- wait(context.Background()) }()
	select {
	case <-drained:
		t.Fatal("the drain did not wait for the running step")
	default:
	}
	close(s1.release)
	if left := <-drained; left != 0 {
		t.Errorf("%d mutations left after the drain", left)
	}
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunStopped || run.ErrorCode != "restart" || stepStatuses(run) != "one=success two=pending" || s2.ran {
		t.Errorf("record %+v (%s)", run, stepStatuses(run))
	}
	rec.mu.Lock()
	marker := rec.marker
	rec.mu.Unlock()
	if marker != nil {
		t.Errorf("marker left: %+v", marker)
	}
	if st := operationStatus(nil); st.Interrupted != nil {
		t.Errorf("reported interrupted: %+v", st.Interrupted)
	}
	if st := recipeRunStatus(nil); st.Interrupted || st.Running {
		t.Errorf("run status %+v", st)
	}
	mutations.mu.Lock()
	mutations.draining = false
	mutations.mu.Unlock()
}

// Another pod taking the lease over stops the running step through its
// context, and the run ends failed with lock_lost.
func TestRecipeRun_LeaseLossEndsLockLost(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2 := f.addStep("one"), f.addStep("two")
	s1.waitsCtx = true
	_, done := startRun(t)
	waitStarted(t, s1)
	rec.setLease(otherPodLease(clk.Now()))
	rec.beat(t)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "lock_lost" || run.StopReason != cluster.LockLostMessage || s2.ran {
		t.Errorf("record %+v", run)
	}
	if run.Steps[0].Status != types.RecipeStepFailed || run.Steps[0].ErrorCode != "lock_lost" {
		t.Errorf("step %+v", run.Steps[0])
	}
	if l := rec.currentLease(); l == nil || l.ID != "remote-op" {
		t.Errorf("the other pod's lease was touched: %+v", l)
	}
	rec.mu.Lock()
	last := rec.last
	rec.mu.Unlock()
	if last == nil || last.Success || last.Message != cluster.LockLostMessage {
		t.Errorf("last completed %+v", last)
	}
}

// Before each step the sign-in is checked again: a token that would
// expire during the next step stops the run with sign_in_again before
// that step starts; a user who is no longer a cluster-admin with
// forbidden.
func TestRecipeRun_SignInCheckedBeforeEachStep(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func()
	}{
		{"expiring sign-in", signInAgainCode, func() {
			readSessionToken = func(context.Context, string) (*cluster.SessionToken, error) {
				return fullToken(sessionNow.Add(-24*time.Hour + 5*time.Minute)), nil
			}
		}},
		{"revoked sign-in", "session_expired", func() {
			readSessionToken = func(context.Context, string) (*cluster.SessionToken, error) {
				return nil, fmt.Errorf("x: %w", cluster.ErrSessionTokenGone)
			}
		}},
		{"no longer a cluster-admin", "forbidden", func() {
			mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rec := installRecipeFake(t)
			s1, s2 := f.addStep("one"), f.addStep("two")
			s1.onRun = tc.change
			close(s1.release)
			_, done := startRun(t)
			waitDone(t, done)
			run := f.saved()
			if run.State != types.RecipeRunStopped || run.ErrorCode != tc.code || !strings.Contains(run.StopReason, "Stopped before Step two") ||
				!strings.Contains(run.StopReason, "Resume") || s2.ran || len(s2.clients) != 0 {
				t.Errorf("record %+v, second step checked %d", run, len(s2.clients))
			}
			assertReleased(t, rec)
		})
	}
}

// Resume runs every step's check again: what is set up is skipped, the
// failed step runs again, the run keeps its ID and the build it pinned.
// With reresolveBuild the plan resolves the build again.
func TestRecipeResume_SkipsDoneStepsAndKeepsThePinnedBuild(t *testing.T) {
	f, rec := installRecipeFake(t)
	s1, s2 := f.addStep("one"), f.addStep("two")
	s2.result = &types.OperationResponse{Success: false, Message: "not ready", ErrorCode: "timeout"}
	close(s1.release)
	close(s2.release)
	runID, done := startRun(t)
	waitDone(t, done)
	if f.saved().State != types.RecipeRunFailed {
		t.Fatalf("first run %+v", f.saved())
	}
	// The next try: the first step is set up, the second now works.
	f.steps = nil
	r1, r2 := f.addStep("one"), f.addStep("two")
	r1.done = true
	close(r2.release)
	mutationLimiter = newRateLimiter()

	w := httptest.NewRecorder()
	HandleRecipeResume(w, postJSON(t, "/api/recipes/resume", "user:admin", `{"runId":"`+runID+`"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	recipes.mu.Lock()
	done = recipes.done
	recipes.mu.Unlock()
	waitDone(t, done)
	run := f.saved()
	if run.ID != runID || run.State != types.RecipeRunSucceeded || run.ResumedAt == "" || stepStatuses(run) != "one=skipped two=success" || r1.ran {
		t.Errorf("resumed run %+v (%s)", run, stepStatuses(run))
	}
	if !strings.HasPrefix(run.Steps[0].Message, "Already set up") {
		t.Errorf("skipped step %+v", run.Steps[0])
	}
	f.mu.Lock()
	pinnedIn := f.pinnedIn
	f.mu.Unlock()
	if len(pinnedIn) != 2 || pinnedIn[0] != nil || pinnedIn[1] == nil || *pinnedIn[1] != testPinned {
		t.Errorf("the plans got pinned builds %v", pinnedIn)
	}
	assertReleased(t, rec)

	// Another run ID, or re-resolving the build.
	mutationLimiter = newRateLimiter()
	w = httptest.NewRecorder()
	HandleRecipeResume(w, postJSON(t, "/api/recipes/resume", "user:admin", `{"runId":"stale"}`))
	if w.Code != http.StatusConflict || clusterMutationInProgress.Load() {
		t.Errorf("resume of another run: %d %s", w.Code, w.Body.String())
	}
	mutationLimiter = newRateLimiter()
	w = httptest.NewRecorder()
	HandleRecipeResume(w, postJSON(t, "/api/recipes/resume", "user:admin", `{"runId":"`+runID+`","reresolveBuild":true}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume re-resolving: %d %s", w.Code, w.Body.String())
	}
	recipes.mu.Lock()
	done = recipes.done
	recipes.mu.Unlock()
	waitDone(t, done)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pinnedIn) != 3 || f.pinnedIn[2] != nil {
		t.Errorf("re-resolving still passed a pinned build: %v", f.pinnedIn)
	}
}

// A panic in a step ends the run failed (internal) and frees the lock,
// the lease and the mutation slot.
func TestRecipeRun_PanicFailsTheRunAndFreesTheLock(t *testing.T) {
	logs := captureLog(t)
	f, rec := installRecipeFake(t)
	s1, s2 := f.addStep("one"), f.addStep("two")
	s1.panics = true
	close(s1.release)
	_, done := startRun(t)
	waitDone(t, done)
	run := f.saved()
	if run.State != types.RecipeRunFailed || run.ErrorCode != "internal" || run.Steps[0].Status != types.RecipeStepFailed || s2.ran {
		t.Errorf("record %+v", run)
	}
	assertReleased(t, rec)
	if !strings.Contains(logs.String(), "step bug") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("no panic stack in the log: %s", logs.String())
	}
}

// A plan with problems refuses with 422 and the plan; an invalid recipe
// with 400 and every problem. Either way nothing began: no record, the
// lock free, the slot given back.
func TestRecipeRun_RefusalsLeaveNothingBehind(t *testing.T) {
	f, rec := installRecipeFake(t)
	f.addStep("one")
	f.problems = []types.RecipeProblem{{Message: "The one-time cluster setup is not done."}}
	w := httptest.NewRecorder()
	HandleRecipeRun(w, postJSON(t, "/api/recipes/run", "user:admin", `{"recipe":`+testRecipe+`}`))
	body := decodeError(t, w)
	if w.Code != http.StatusUnprocessableEntity || body["errorCode"] != "prerequisites" || body["plan"] == nil || !strings.Contains(w.Body.String(), "one-time cluster setup") {
		t.Errorf("plan refusal: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	HandleRecipeRun(w, postJSON(t, "/api/recipes/run", "user:admin", `{"recipe":{"apiVersion":"v9","kind":"EnvironmentRecipe","metadata":{"name":"x"},"spec":{}}}`))
	if w.Code != http.StatusBadRequest || decodeError(t, w)["errorCode"] != "validation" || !strings.Contains(w.Body.String(), "apiVersion") {
		t.Errorf("invalid recipe: %d %s", w.Code, w.Body.String())
	}
	if f.saves != 0 || mutationLimiter.isRateLimited("admin:/api/recipes/run") {
		t.Errorf("saves %d, slot taken %v", f.saves, mutationLimiter.isRateLimited("admin:/api/recipes/run"))
	}
	assertReleased(t, rec)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.last != nil {
		t.Errorf("a refused run became an operation: %+v", rec.last)
	}
}

// The plan is read-only: it takes the lock but begins no operation and
// does not use up the rate-limit slot.
func TestRecipePlan_ReadOnly(t *testing.T) {
	f, rec := installRecipeFake(t)
	f.addStep("one")
	for range 2 {
		w := httptest.NewRecorder()
		HandleRecipePlan(w, postJSON(t, "/api/recipes/plan", "user:admin", `{"recipe":`+testRecipe+`}`))
		var plan types.RecipePlan
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Recipe.Metadata.Name != "dev" {
			t.Fatalf("plan: %d %s", w.Code, w.Body.String())
		}
	}
	if f.saves != 0 {
		t.Errorf("the plan wrote the run record %d times", f.saves)
	}
	assertReleased(t, rec)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.last != nil {
		t.Errorf("the plan became an operation: %+v", rec.last)
	}
}

// A record that says running, of a process that holds no live lease, is
// an interrupted run; with that process's live lease it runs in another
// pod.
func TestRecipeRunStatus_InterruptedAndRemote(t *testing.T) {
	f, rec := installRecipeFake(t)
	f.record = &types.RecipeRun{ID: "r1", BootID: "other-boot", State: types.RecipeRunRunning, Steps: []types.RecipeRunStep{{ID: "rhoai", Status: types.RecipeStepRunning}}}
	if st := recipeRunStatus(nil); !st.Interrupted || st.Running || st.Run.ID != "r1" {
		t.Errorf("no live lease: %+v", st)
	}
	rec.setLease(otherPodLease(clk.Now()))
	if st := recipeRunStatus(nil); st.Interrupted || !st.Running || !st.Remote || st.Pod != "updater-old-pod" || st.Step == nil {
		t.Errorf("live lease of its process: %+v", st)
	}
	f.record.State = types.RecipeRunFailed
	rec.setLease(nil)
	if st := recipeRunStatus(nil); st.Interrupted || st.Running {
		t.Errorf("finished run: %+v", st)
	}
	f.record = nil
	if st := recipeRunStatus(nil); st.Run != nil {
		t.Errorf("no run: %+v", st)
	}
}

// While a recipe runs, a user who is not a cluster-admin sees only that an
// operation runs, its fixed label and its start: not the recipe, project,
// build or step.
func TestRecipeRun_UsersSeeOnlyTheLabel(t *testing.T) {
	f, _ := installRecipeFake(t)
	s1 := f.addStep("one")
	_, done := startRun(t)
	waitStarted(t, s1)
	setAdmin(t)
	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:carol"))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["running"] != true || got["label"] != "Environment recipe" || got["startedAt"] == nil {
		t.Errorf("user's operation status: %s", w.Body.String())
	}
	allowMutations(t)
	close(s1.release)
	waitDone(t, done)
}

// The run record is bounded: step messages are shortened before it is
// written.
func TestRecipeRun_StepMessagesAreBounded(t *testing.T) {
	f, _ := installRecipeFake(t)
	s1 := f.addStep("one")
	s1.result = &types.OperationResponse{Success: false, Message: strings.Repeat("x", 10_000)}
	close(s1.release)
	_, done := startRun(t)
	waitDone(t, done)
	if got := len(f.saved().Steps[0].Message); got > maxOperationMessageBytes {
		t.Errorf("step message of %d bytes recorded", got)
	}
}

