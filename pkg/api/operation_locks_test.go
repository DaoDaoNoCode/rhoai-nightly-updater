package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
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

func takeCluster(t *testing.T) (*lockRefusal, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	var started bool
	return acquireRunnerLock(startRunner(c, &started), cancel), ctx
}

// A cluster operation is refused while a project action runs: in another
// pod (its live lock in the record; the refusal names the project) or in
// this process (its slot). Nothing is held afterwards.
func TestClusterLockRefusedWhileProjectLockLive(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	f.setProject("uid-1", otherPodProjectLease("uid-1", "alice", clk.Now()))

	refusal, _ := takeCluster(t)
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
	if refusal, _ := takeCluster(t); refusal != nil {
		t.Fatalf("expired project lock refused: %+v", refusal)
	}
	releaseClusterMutationLock()

	// This process's own project action.
	f.setProject("uid-1", nil)
	h := takeProject(t, "uid-2", "carol")
	if h.refusal != nil {
		t.Fatalf("project lock refused: %+v", h.refusal)
	}
	refusal, _ = takeCluster(t)
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
	if refusal, _ := takeCluster(t); refusal != nil {
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
