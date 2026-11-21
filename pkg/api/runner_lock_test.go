package api

import (
	"context"
	"errors"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// startRunner is the start of a holder with no request (a background job,
// a recipe run): it records a background operation with c as bookkeeper and
// reports that it ran.
func startRunner(c *cluster.Client, started *bool) func() *Operation {
	return func() *Operation {
		*started = true
		return inflight.startBackground("runner-test", "A runner", "alice", c)
	}
}

// A holder that outlives any request takes the local lock and the lease,
// and its own cancel (not a request's) is what the heartbeat stops it with
// when the lease is lost. Released, the lock and the operation are free.
func TestAcquireRunnerLock_HoldsLockAndLeaseWithItsOwnCancel(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	runCtx, cancelRun := context.WithCancelCause(context.Background())
	defer cancelRun(nil)
	c := cluster.NewClientWithContext(runCtx, "sa-token")
	var started bool
	if refusal := acquireRunnerLock(startRunner(c, &started), cancelRun); refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	op := inflight.snapshot()
	if !started || !clusterMutationInProgress.Load() || op == nil || op.Type != "runner-test" {
		t.Fatalf("not held: started %t, lock %t, op %+v", started, clusterMutationInProgress.Load(), op)
	}
	if l := f.currentLease(); l == nil || l.ID != op.ID || l.BootID != bootID || l.Type != "runner-test" {
		t.Fatalf("lease %+v", l)
	}

	// The heartbeat renews the lease while it is held ...
	f.beat(t)
	if runCtx.Err() != nil {
		t.Fatal("a renewal stopped the holder")
	}
	// ... and stops the holder through its own cancel when another pod
	// took the lease over.
	f.setLease(otherPodLease(clk.Now()))
	f.beat(t)
	<-runCtx.Done()
	if err := context.Cause(runCtx); !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}

	releaseClusterMutationLock()
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("the lock or the operation was left behind")
	}
	if l := f.currentLease(); l == nil || l.ID != "remote-op" {
		t.Fatalf("the other pod's lease was touched: %+v", l)
	}
}

// Fail closed: another operation of this process, a live lease of another
// pod, or a lease that cannot be read or written refuses. Nothing is held
// afterwards, the holder is not stopped, and a lock held by someone else
// stays held.
func TestAcquireRunnerLock_FailsClosed(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	cancelled := false
	cancel := func(error) { cancelled = true }

	clusterMutationInProgress.Store(true)
	started := false
	refusal := acquireRunnerLock(startRunner(c, &started), cancel)
	if refusal == nil || !refusal.busy || refusal.holder != nil || refusal.err != nil {
		t.Fatalf("local lock held: refusal %+v", refusal)
	}
	if started || !clusterMutationInProgress.Load() {
		t.Fatalf("started %t, or freed a lock it did not hold", started)
	}
	clusterMutationInProgress.Store(false)

	f.setLease(otherPodLease(clk.Now()))
	refusal = acquireRunnerLock(startRunner(c, &started), cancel)
	if refusal == nil || refusal.busy || refusal.holder == nil || refusal.holder.ID != "remote-op" || refusal.err != nil {
		t.Fatalf("another pod's lease: refusal %+v", refusal)
	}
	if l := f.currentLease(); l == nil || l.ID != "remote-op" {
		t.Fatalf("the other pod's lease was touched: %+v", l)
	}
	f.setLease(nil)

	f.mu.Lock()
	f.writeErr = errors.New("etcdserver: request timed out")
	f.mu.Unlock()
	refusal = acquireRunnerLock(startRunner(c, &started), cancel)
	if refusal == nil || refusal.busy || refusal.holder != nil || refusal.err == nil {
		t.Fatalf("unwritable lease: refusal %+v", refusal)
	}

	if clusterMutationInProgress.Load() || inflight.snapshot() != nil || cancelled {
		t.Fatalf("after the refusals: lock %t, op %v, cancelled %t", clusterMutationInProgress.Load(), inflight.snapshot(), cancelled)
	}
	if f.currentLease() != nil || len(f.writes) != 0 || len(f.releases) != 0 {
		t.Fatalf("lease %+v, writes %d, releases %d", f.currentLease(), len(f.writes), len(f.releases))
	}
	leases.mu.Lock()
	running := leases.done != nil
	leases.mu.Unlock()
	if running {
		t.Fatal("a heartbeat runs for a refused holder")
	}
}

// A holder whose operation began (its marker written) frees everything in
// one release: the marker is cleared, the operation recorded as the last
// completed one, and the lease released in the same write.
func TestAcquireRunnerLock_ReleaseClearsMarkerAndLease(t *testing.T) {
	setupDevMode(t)
	f := installFakeRecord(t)
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	var started bool
	if refusal := acquireRunnerLock(startRunner(c, &started), func(error) {}); refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	op := inflight.snapshot()
	var marker *types.OperationMarker
	inflight.update(op.ID, func(o *Operation) {
		o.begun = true
		marker = operationMarker(o)
	})
	markers.start(c, marker)
	inflight.recordStep(op.ID, UpdateStep{Step: "operation_complete", Status: "success", Message: "Done."})
	f.mu.Lock()
	written := f.marker
	f.mu.Unlock()
	if written == nil || written.ID != op.ID || f.currentLease() == nil {
		t.Fatalf("marker %+v, lease %+v", written, f.currentLease())
	}

	releaseClusterMutationLock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marker != nil || f.lease != nil {
		t.Fatalf("left behind: marker %+v, lease %+v", f.marker, f.lease)
	}
	if len(f.completed) != 1 || f.completed[0] == nil || f.completed[0].ID != op.ID || f.completed[0].BootID != bootID {
		t.Fatalf("completed with owners %+v", f.completed)
	}
	if f.last == nil || f.last.ID != op.ID || !f.last.Success || f.last.Message != "Done." {
		t.Fatalf("last completed %+v", f.last)
	}
	if len(f.releases) != 0 {
		t.Fatalf("the lease was released apart from the marker clear: %v", f.releases)
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("the lock or the operation was left behind")
	}
}
