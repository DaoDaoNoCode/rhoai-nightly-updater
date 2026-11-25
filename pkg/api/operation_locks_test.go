package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// otherPodProjectLease is the live lock of project uid that user's action
// holds in another updater pod.
func otherPodProjectLease(uid, user string, beat time.Time) *types.OperationLease {
	l := otherPodLease(beat)
	l.ID, l.User, l.Type, l.Label = "remote-"+uid, user, "workbench", "Workbench"
	l.Project, l.ProjectUID = "team-"+uid, uid
	return l
}

// projectHolder is a project action of this process: it takes the lock of
// project uid for user with its own cancel, and reports how it ended.
type projectHolder struct {
	uid     string
	ctx     context.Context
	cancel  context.CancelCauseFunc
	refusal *lockRefusal
}

func takeProject(t *testing.T, uid, user string) *projectHolder {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	h := &projectHolder{uid: uid, ctx: ctx, cancel: cancel}
	h.refusal = acquireProjectLock(uid, user, func() *Operation {
		return inflight.startProject("workbench", "Workbench", user, "team-"+uid, uid, c)
	}, cancel)
	return h
}

func takeCluster(t *testing.T) *lockRefusal {
	t.Helper()
	_, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	var started bool
	return acquireRunnerLock(startRunner(c, &started), cancel)
}

// A cluster operation is refused while a project action runs: in another
// pod (its live lock in the record; the refusal names the project) or in
// this process (its slot). Nothing is held afterwards.
func TestClusterLockRefusedWhileProjectLockLive(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	f.setProject("uid-1", otherPodProjectLease("uid-1", "alice", clk.Now()))

	refusal := takeCluster(t)
	if refusal == nil || refusal.holder == nil || refusal.holder.ProjectUID != "uid-1" || refusal.reason != conflictCluster {
		t.Fatalf("another pod's project lock: refusal %+v", refusal)
	}
	if locks.clusterHeld() || inflight.snapshot() != nil || f.currentLease() != nil {
		t.Fatal("something is held after the refusal")
	}
	// Through a route: 409 cluster_busy naming the project.
	w := httptest.NewRecorder()
	HandleUpdate(w, postJSON(t, "/api/update", "user:bob", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if body := decodeError(t, w); w.Code != 409 || body["errorCode"] != "cluster_busy" || !strings.Contains(body["error"].(string), "in project team-uid-1") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	// An expired one does not refuse.
	f.setProject("uid-1", otherPodProjectLease("uid-1", "alice", clk.Now().Add(-2*leaseTTL)))
	if refusal := takeCluster(t); refusal != nil {
		t.Fatalf("expired project lock refused: %+v", refusal)
	}
	releaseClusterMutationLock()

	// This process's own project action.
	f.setProject("uid-1", nil)
	h := takeProject(t, "uid-2", "carol")
	if h.refusal != nil {
		t.Fatalf("project lock refused: %+v", h.refusal)
	}
	refusal = takeCluster(t)
	if refusal == nil || !refusal.busy || refusal.reason != conflictCluster || refusal.opID == "" || inflight.get(refusal.opID).ProjectUID != "uid-2" {
		t.Fatalf("own project action: refusal %+v", refusal)
	}
	releaseProjectLock("uid-2")
	if f.project("uid-2") != nil {
		t.Fatal("the project lock was not released")
	}
}

// The reverse: a project action is refused while a cluster operation runs,
// in another pod or in this process.
func TestProjectLockRefusedWhileClusterLockLive(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	f.setLease(otherPodLease(clk.Now()))
	if h := takeProject(t, "uid-1", "alice"); h.refusal == nil || h.refusal.holder == nil || h.refusal.holder.ID != "remote-op" || h.refusal.reason != conflictCluster {
		t.Fatalf("another pod's cluster lock: refusal %+v", h.refusal)
	}
	if f.project("uid-1") != nil || locks.projectOp("uid-1") != "" || len(inflight.list()) != 0 {
		t.Fatal("something is held after the refusal")
	}
	f.setLease(nil)
	if refusal := takeCluster(t); refusal != nil {
		t.Fatalf("cluster lock refused: %+v", refusal)
	}
	if h := takeProject(t, "uid-1", "alice"); h.refusal == nil || !h.refusal.busy || h.refusal.reason != conflictCluster {
		t.Fatalf("own cluster operation: refusal %+v", h.refusal)
	}
	releaseClusterMutationLock()
	if h := takeProject(t, "uid-1", "alice"); h.refusal != nil {
		t.Fatalf("after the cluster operation: refusal %+v", h.refusal)
	}
	releaseProjectLock("uid-1")
}

// One running project action per user, in this process and across pods,
// and one per project.
func TestOnlyOneProjectActionPerUser(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	if h := takeProject(t, "uid-1", "alice"); h.refusal != nil {
		t.Fatalf("first: %+v", h.refusal)
	}
	if h := takeProject(t, "uid-2", "alice"); h.refusal == nil || !h.refusal.busy || h.refusal.reason != conflictUser {
		t.Fatalf("alice's second action: %+v", h.refusal)
	}
	if h := takeProject(t, "uid-1", "bob"); h.refusal == nil || !h.refusal.busy || h.refusal.reason != conflictProject {
		t.Fatalf("bob in alice's busy project: %+v", h.refusal)
	}
	if h := takeProject(t, "uid-2", "bob"); h.refusal != nil {
		t.Fatalf("bob in another project: %+v", h.refusal)
	}
	// carol runs one in another pod.
	f.setProject("uid-3", otherPodProjectLease("uid-3", "carol", clk.Now()))
	if h := takeProject(t, "uid-4", "carol"); h.refusal == nil || h.refusal.holder == nil || h.refusal.holder.User != "carol" || h.refusal.reason != conflictUser {
		t.Fatalf("carol's second action: %+v", h.refusal)
	}
	if h := takeProject(t, "uid-3", "dave"); h.refusal == nil || h.refusal.holder == nil || h.refusal.reason != conflictProject {
		t.Fatalf("dave in carol's busy project: %+v", h.refusal)
	}
	releaseProjectLock("uid-1")
	if h := takeProject(t, "uid-5", "alice"); h.refusal != nil {
		t.Fatalf("alice after her action ended: %+v", h.refusal)
	}
	releaseProjectLock("uid-2")
	releaseProjectLock("uid-5")
}

// At most maxProjectLocks project actions at once, counting this
// process's and other pods'.
func TestProjectLockCap(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	if h := takeProject(t, "uid-local", "alice"); h.refusal != nil {
		t.Fatalf("local: %+v", h.refusal)
	}
	for i := range maxProjectLocks - 1 {
		uid := "uid-" + strconv.Itoa(i)
		f.setProject(uid, otherPodProjectLease(uid, "user-"+strconv.Itoa(i), clk.Now()))
	}
	h := takeProject(t, "uid-new", "zoe")
	if h.refusal == nil || h.refusal.reason != conflictCap {
		t.Fatalf("over the cap: %+v", h.refusal)
	}
	releaseProjectLock("uid-local")
	if h := takeProject(t, "uid-new", "zoe"); h.refusal != nil {
		t.Fatalf("under the cap again: %+v", h.refusal)
	}
	releaseProjectLock("uid-new")
}

// Two project actions renewed by one heartbeat, in one write per interval:
// another pod takes one entry over, and only that action stops; the other
// is renewed in the same round and goes on.
func TestLeaseLossStopsOnlyThatOperation(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	a := takeProject(t, "uid-a", "alice")
	b := takeProject(t, "uid-b", "bob")
	if a.refusal != nil || b.refusal != nil {
		t.Fatalf("refused: %+v %+v", a.refusal, b.refusal)
	}
	f.beat(t) // one write renews both
	if pa, pb := f.project("uid-a"), f.project("uid-b"); pa.HeartbeatAt != pb.HeartbeatAt || pa.HeartbeatAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("not renewed together: %+v %+v", pa, pb)
	}
	f.setProject("uid-a", otherPodProjectLease("uid-a", "carol", clk.Now()))
	f.beat(t)
	<-a.ctx.Done()
	if err := context.Cause(a.ctx); !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("a: cause %v", err)
	}
	if b.ctx.Err() != nil {
		t.Fatal("b was stopped too")
	}
	if pb := f.project("uid-b"); pb.HeartbeatAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("b not renewed in the round that lost a: %+v", pb)
	}
	if op := inflight.get(locks.projectOp("uid-a")); op == nil || !op.lockLost {
		t.Fatalf("a not marked lost: %+v", op)
	}
	f.beat(t) // b alone now
	if b.ctx.Err() != nil || f.project("uid-a").User != "carol" {
		t.Fatal("b stopped, or the other pod's entry was touched")
	}
	releaseProjectLock("uid-a")
	releaseProjectLock("uid-b")
	if f.project("uid-a") == nil || f.project("uid-b") != nil {
		t.Fatalf("release: a %+v b %+v", f.project("uid-a"), f.project("uid-b"))
	}
	if leases.stopAll() != 0 {
		t.Fatal("entries left after both ended")
	}
}

// A renewal whose answer was lost but which went through leaves this
// process's own newer value: the next renewal's test fails, and the entry
// is taken again (not lost); so is an entry removed by hand. Another
// pod's live entry is the only loss.
func TestRenewalTakesItsOwnEntryAgain(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	started, cause, release := refreshUntilStopped()
	done := runRefreshInBackground(t)
	<-started
	own := f.currentLease()
	own.HeartbeatAt = clk.Now().Add(time.Second).UTC().Format(time.RFC3339) // not what the holder last wrote
	f.setLease(own)
	f.mu.Lock()
	n := f.attempts
	f.mu.Unlock()
	testClock.Advance(leaseHeartbeatInterval)
	testClock.WaitForTicksHandled()
	f.mu.Lock()
	got := f.attempts
	f.mu.Unlock()
	if got != n+2 || heartbeatStopped() {
		t.Fatalf("%d writes (want the renewal and the acquire again), stopped %t", got-n, heartbeatStopped())
	}
	if l := f.currentLease(); l == nil || l.ID != own.ID || l.HeartbeatAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("lease %+v", l)
	}
	f.setLease(nil) // removed by hand
	testClock.Advance(leaseHeartbeatInterval)
	testClock.WaitForTicksHandled()
	if heartbeatStopped() || f.currentLease() == nil {
		t.Fatal("a removed entry stopped the operation or was not written again")
	}
	f.beat(t) // renewals go on as usual
	close(release)
	<-done
	select {
	case err := <-cause:
		t.Fatalf("stopped: %v", err)
	default:
	}
}

// GET /api/operation lists every operation: this process's project action,
// another pod's, an interrupted one (a begun entry that expired); not an
// expired entry that never began. The earlier fields keep describing the
// cluster operation (none). A user who is not a cluster-admin sees no
// project action in the list yet.
func TestOperationListsEveryOperation(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	f := installFakeRecord(t)
	if h := takeProject(t, "uid-local", "alice"); h.refusal != nil {
		t.Fatal(h.refusal)
	}
	f.setProject("uid-remote", otherPodProjectLease("uid-remote", "bob", clk.Now()))
	interrupted := otherPodProjectLease("uid-int", "carol", clk.Now().Add(-2*leaseTTL))
	interrupted.Begun, interrupted.StartedAt = true, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.setProject("uid-int", interrupted)
	f.setProject("uid-stale", otherPodProjectLease("uid-stale", "dave", clk.Now().Add(-2*leaseTTL)))

	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:admin"))
	var status OperationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.InProgress || status.Operation != nil || len(status.Operations) != 3 {
		t.Fatalf("status %s", w.Body.String())
	}
	byUID := map[string]*Operation{}
	for _, op := range status.Operations {
		if op.Scope != scopeProject {
			t.Errorf("scope %q", op.Scope)
		}
		byUID[op.ProjectUID] = op
	}
	if op := byUID["uid-local"]; op == nil || op.Remote || op.Interrupted || op.User != "alice" || op.Project != "team-uid-local" {
		t.Errorf("local %+v", op)
	}
	if op := byUID["uid-remote"]; op == nil || !op.Remote || op.Interrupted || op.User != "bob" {
		t.Errorf("remote %+v", op)
	}
	if op := byUID["uid-int"]; op == nil || op.Remote || !op.Interrupted || op.User != "carol" {
		t.Errorf("interrupted %+v", op)
	}

	w = httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:carol"))
	if got := strings.TrimSpace(w.Body.String()); got != `{"running":false,"label":"","startedAt":"","operations":[]}` {
		t.Fatalf("user: %s", got)
	}
	releaseProjectLock("uid-local")
}

// tick moves the fake clock one heartbeat interval on and waits until the
// heartbeat handled it.
func tick() {
	testClock.Advance(leaseHeartbeatInterval)
	testClock.WaitForTicksHandled()
}

// ownNewerValue makes the record hold this process's own entry of uid with
// another value than it wrote last (a renewal whose answer was lost): the
// next renewal's test fails, and the entry is recovered.
func ownNewerValue(f *fakeRecord, uid string) {
	l := f.project(uid)
	l.HeartbeatAt = clk.Now().Add(time.Second).UTC().Format(time.RFC3339)
	f.setProject(uid, l)
}

// beforeRecovery runs fn once, when the heartbeat is about to take key
// again (after the renewal found it changed), and then lets the recovery's
// write go on as one already under way: past the waits that the stop of
// the entry ends (its context), so only the check made where the write is
// made can refuse it.
func beforeRecovery(key string, fn func()) {
	inner := acquireOperationLock
	var ran atomic.Bool
	acquireOperationLock = func(c *cluster.Client, k, value string, check func(*cluster.OperationRecord) error) error {
		if k == key && ran.CompareAndSwap(false, true) {
			fn() // fn may take the lock itself: not under a sync.Once
			return inner(c.WithContext(context.Background()), k, value, check)
		}
		return inner(c, k, value, check)
	}
}

// The action of a project ends (its entry released) between the renewal
// that found the entry changed and its recovery's write: the recovery does not
// write the entry back. The heartbeat goes on for the other entry.
func TestRecoveryDoesNotReviveAReleasedEntry(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	a := takeProject(t, "uid-a", "alice")
	b := takeProject(t, "uid-b", "bob")
	if a.refusal != nil || b.refusal != nil {
		t.Fatal(a.refusal, b.refusal)
	}
	ownNewerValue(f, "uid-a")
	beforeRecovery(cluster.ProjectLockKey("uid-a"), func() { releaseProjectLock("uid-a") })
	tick()
	if l := f.project("uid-a"); l != nil {
		t.Fatalf("the released entry was written back: %+v", l)
	}
	if a.ctx.Err() != nil || b.ctx.Err() != nil {
		t.Fatal("an operation was stopped")
	}
	if l := f.project("uid-b"); l == nil || l.HeartbeatAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("b not renewed: %+v", l)
	}
	releaseProjectLock("uid-b")
}

// The next action in the project (this process, another user) takes the
// lock between the renewal and the recovery: the recovery leaves the
// successor's entry alone.
func TestRecoveryDoesNotOverwriteASuccessor(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	a := takeProject(t, "uid-a", "alice")
	b := takeProject(t, "uid-b", "bob")
	if a.refusal != nil || b.refusal != nil {
		t.Fatal(a.refusal, b.refusal)
	}
	ownNewerValue(f, "uid-a")
	var successor *projectHolder
	beforeRecovery(cluster.ProjectLockKey("uid-a"), func() {
		releaseProjectLock("uid-a")
		successor = takeProject(t, "uid-a", "carol")
	})
	tick()
	if successor == nil || successor.refusal != nil {
		t.Fatalf("successor %+v", successor)
	}
	if l := f.project("uid-a"); l == nil || l.User != "carol" || l.ID != locks.projectOp("uid-a") {
		t.Fatalf("the successor's entry was overwritten: %+v", l)
	}
	if successor.ctx.Err() != nil {
		t.Fatal("the successor was stopped")
	}
	releaseProjectLock("uid-a")
	releaseProjectLock("uid-b")
}

// In one round, the entry whose deadline comes first is renewed (which
// ends the batch write's context) and a later one is recovered: the
// recovery writes with a context of its own and succeeds.
func TestRecoveryAfterASuccessfulRenewalHasItsOwnContext(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	a := takeProject(t, "uid-a", "alice")
	testClock.Advance(time.Second) // b's deadline comes later
	b := takeProject(t, "uid-b", "bob")
	if a.refusal != nil || b.refusal != nil {
		t.Fatal(a.refusal, b.refusal)
	}
	ownNewerValue(f, "uid-b")
	testClock.Advance(leaseHeartbeatInterval - time.Second) // a's first tick
	testClock.WaitForTicksHandled()
	now := clk.Now().UTC().Format(time.RFC3339)
	if la, lb := f.project("uid-a"), f.project("uid-b"); la.HeartbeatAt != now || lb.HeartbeatAt != now {
		t.Fatalf("a %s, b %s, want both %s", la.HeartbeatAt, lb.HeartbeatAt, now)
	}
	if a.ctx.Err() != nil || b.ctx.Err() != nil {
		t.Fatal("an operation was stopped")
	}
	releaseProjectLock("uid-a")
	releaseProjectLock("uid-b")
}

// A pending acquisition (this process's slot taken, its entry not written
// yet) is not listed: the other pod's cluster operation that holds the
// lock is the only running one.
func TestOperationListShowsConfirmedHoldersOnly(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	f := installFakeRecord(t)
	f.setLease(otherPodLease(clk.Now()))
	locks.setCluster(true)
	inflight.startBackground("update", "Update to nightly", "bob", nil)
	status, _ := getOperationAs(t, "user:admin")
	if len(status.Operations) != 1 || !status.Operations[0].Remote || status.Operations[0].ID != "remote-op" {
		t.Fatalf("operations %+v", status.Operations)
	}
}

// While this process runs a cluster operation it answers from memory (no
// cluster call), and still lists the interrupted project actions of the
// record it read last (its acquire's).
func TestOperationListKeepsInterruptionsDuringAClusterOperation(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	f := installFakeRecord(t)
	interrupted := otherPodProjectLease("uid-int", "carol", clk.Now().Add(-2*leaseTTL))
	interrupted.Begun, interrupted.StartedAt = true, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.setProject("uid-int", interrupted)
	if refusal := takeCluster(t); refusal != nil {
		t.Fatal(refusal)
	}
	defer releaseClusterMutationLock()
	inflight.rememberLast(&types.CompletedOperation{ID: "earlier", FinishedAt: time.Now().UTC().Format(time.RFC3339)})
	reads := 0
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		reads++
		return &cluster.OperationRecord{}, nil
	}
	status, _ := getOperationAs(t, "user:admin")
	if reads != 0 || len(status.Operations) != 2 {
		t.Fatalf("reads %d operations %+v", reads, status.Operations)
	}
	var sawInterrupted, sawCluster bool
	for _, op := range status.Operations {
		sawInterrupted = sawInterrupted || (op.Interrupted && op.ProjectUID == "uid-int")
		sawCluster = sawCluster || (op.Scope == scopeCluster && op.Type == "runner-test")
	}
	if !sawInterrupted || !sawCluster {
		t.Fatalf("operations %+v", status.Operations)
	}
}

// A project action that begins records Begun on its entry at once, before
// it changes anything; when its entry is no longer its own it cannot
// begin.
func TestBeginProjectActionRecordsBegunAtOnce(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	if h := takeProject(t, "uid-1", "alice"); h.refusal != nil {
		t.Fatal(h.refusal)
	}
	if err := beginProjectAction(locks.projectOp("uid-1"), "workbench in team-uid-1"); err != nil {
		t.Fatal(err)
	}
	if l := f.project("uid-1"); l == nil || !l.Begun || l.Target != "workbench in team-uid-1" {
		t.Fatalf("entry %+v", l)
	}
	// The next renewal tests the value the begin wrote.
	tick()
	if l := f.project("uid-1"); !l.Begun || l.HeartbeatAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("after a tick %+v", l)
	}
	if h := takeProject(t, "uid-2", "bob"); h.refusal != nil {
		t.Fatal(h.refusal)
	}
	f.setProject("uid-2", otherPodProjectLease("uid-2", "carol", clk.Now()))
	if err := beginProjectAction(locks.projectOp("uid-2"), "x"); err == nil {
		t.Fatal("began on an entry that is another pod's")
	}
	if err := beginProjectAction("no-such-op", "x"); err == nil {
		t.Fatal("began an operation that does not run")
	}
	releaseProjectLock("uid-1")
	releaseProjectLock("uid-2")
}

// A refusal names the holder to a cluster-admin only. Anyone else reads a
// cluster operation by its start, their own action by label and project,
// and never another user's name or another project.
func TestBusyRefusalNamesNobodyForUsers(t *testing.T) {
	setupDevMode(t)
	started := clk.Now().Add(-time.Minute)
	clusterOp := otherPodLease(started)
	projectOp := otherPodProjectLease("uid-1", "alice", started)
	for _, tc := range []struct {
		name    string
		viewer  busyViewer
		refusal *lockRefusal
		want    string
		hidden  []string
	}{
		{"admin, cluster", busyViewer{admin: true, user: "root"}, &lockRefusal{holder: clusterOp, reason: conflictCluster}, "alice is running", nil},
		{"user, cluster", busyViewer{user: "bob"}, &lockRefusal{holder: clusterOp, reason: conflictCluster}, "A cluster operation is running since", []string{"alice", "updater-old-pod", "rhoai-3.6"}},
		{"user, same project", busyViewer{user: "bob"}, &lockRefusal{holder: projectOp, reason: conflictProject}, "An action (Workbench) is running in this project", []string{"alice", "team-uid-1"}},
		{"user, own action", busyViewer{user: "alice"}, &lockRefusal{holder: projectOp, reason: conflictUser}, "You already have an action running (Workbench in project team-uid-1)", nil},
		{"user, cap", busyViewer{user: "bob"}, &lockRefusal{holder: projectOp, reason: conflictCap}, "Too many project actions", []string{"alice", "team-uid-1"}},
	} {
		w := httptest.NewRecorder()
		writeLockRefusal(&statusWriter{ResponseWriter: w, admin: tc.viewer.admin, user: tc.viewer.user}, tc.refusal)
		body := w.Body.String()
		if w.Code != 409 || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s", tc.name, w.Code, body)
		}
		for _, h := range tc.hidden {
			if strings.Contains(body, h) {
				t.Errorf("%s: names %q: %s", tc.name, h, body)
			}
		}
		if !tc.viewer.admin && strings.Contains(body, `"operation"`) {
			t.Errorf("%s: the operation is in the answer: %s", tc.name, body)
		}
	}
}

func getOperationAs(t *testing.T, token string) (OperationStatus, string) {
	t.Helper()
	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", token))
	var status OperationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status, w.Body.String()
}
