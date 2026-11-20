package api

import (
	"context"
	"errors"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// stubMaaSPrune replaces the check with one that reports each run (and the
// lock state it saw) on the returned channel.
func stubMaaSPrune(t *testing.T) chan bool {
	t.Helper()
	runs := make(chan bool, 4)
	orig := pruneGoneMaaS
	t.Cleanup(func() { pruneGoneMaaS = orig })
	pruneGoneMaaS = func(*cluster.Client) {
		op := inflight.snapshot()
		runs <- clusterMutationInProgress.Load() && op != nil && op.Type == "prune-maas-publications"
	}
	return runs
}

// The background check runs under the local lock and the lease, shows as
// the running operation, and frees both afterwards.
func TestMaaSPruneJob_RunsUnderTheLockAndLease(t *testing.T) {
	setupDevMode(t)
	runs := stubMaaSPrune(t)
	var released int
	releaseOperationLease = func(*cluster.Client, cluster.LeaseOwner) error { released++; return nil }
	if !runMaaSPruneOnce(context.Background()) {
		t.Fatal("did not run")
	}
	if locked := <-runs; !locked {
		t.Fatal("ran without the lock or not as the running operation")
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil || released != 1 {
		t.Fatalf("not freed: lock %t, op %v, lease releases %d", clusterMutationInProgress.Load(), inflight.snapshot(), released)
	}
}

// Another operation holds the lock (here) or the lease (another pod), or
// the lease cannot be read: skipped, nothing run, and the lock it did not
// take is left as it was.
func TestMaaSPruneJob_SkipsWhenTheLockIsHeld(t *testing.T) {
	setupDevMode(t)
	runs := stubMaaSPrune(t)

	clusterMutationInProgress.Store(true)
	if runMaaSPruneOnce(context.Background()) || !clusterMutationInProgress.Load() {
		t.Fatal("ran, or freed a lock it did not hold")
	}
	clusterMutationInProgress.Store(false)

	writeOperationLease = func(*cluster.Client, *types.OperationLease, func(*types.OperationLease) bool) (*types.OperationLease, error) {
		return &types.OperationLease{ID: "other", Pod: "other-pod"}, nil
	}
	if runMaaSPruneOnce(context.Background()) {
		t.Fatal("ran while another pod holds the lease")
	}
	writeOperationLease = func(*cluster.Client, *types.OperationLease, func(*types.OperationLease) bool) (*types.OperationLease, error) {
		return nil, errors.New("conflict")
	}
	if runMaaSPruneOnce(context.Background()) {
		t.Fatal("ran without a lease it could write")
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("the local lock or the operation was left behind")
	}
	select {
	case <-runs:
		t.Fatal("the check ran")
	default:
	}
}

// Shutdown (SIGTERM -> DrainMutations in main) waits for a check that runs
// under the lease: the check is a counted mutation, so the drain ends only
// after it finished and released the lock and the lease; no check starts
// once the drain began. Its context is not the signal's, on purpose: a
// check cut off half-way would leave the gateway half-pruned.
func TestShutdownDrainWaitsForARunningMaaSCheck(t *testing.T) {
	withFreshMutationTracker(t)
	setupDevMode(t)
	var released int
	releaseOperationLease = func(*cluster.Client, cluster.LeaseOwner) error { released++; return nil }
	orig := pruneGoneMaaS
	t.Cleanup(func() { pruneGoneMaaS = orig })
	started, finish := make(chan struct{}), make(chan struct{})
	pruneGoneMaaS = func(*cluster.Client) {
		close(started)
		<-finish
	}
	ran := make(chan bool)
	go func() { ran <- runMaaSPruneOnce(context.Background()) }()
	<-started

	wait := mutations.startDrain()
	drained := make(chan int)
	go func() { drained <- wait(context.Background()) }()
	// No new check (or change) starts while draining.
	if runMaaSPruneOnce(context.Background()) {
		t.Fatal("a check started during the drain")
	}
	select {
	case <-drained:
		t.Fatal("the drain ended while the check ran")
	default:
	}
	close(finish)
	if !<-ran {
		t.Fatal("the running check did not complete")
	}
	if n := <-drained; n != 0 {
		t.Fatalf("drain reported %d running", n)
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil || released != 1 {
		t.Fatalf("not freed: lock %t, op %v, lease releases %d", clusterMutationInProgress.Load(), inflight.snapshot(), released)
	}
}

// At start, then every interval on the clock; never in the gitops
// profile.
func TestMaaSPruneJob_RunsAtStartAndEveryInterval(t *testing.T) {
	setupDevMode(t)
	runs := stubMaaSPrune(t)
	fake := clocktest.NewFake()
	orig := maasPruneClock
	t.Cleanup(func() { maasPruneClock = orig })
	maasPruneClock = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := StartMaaSPruneJob(ctx)
	if done == nil {
		t.Fatal("not started")
	}
	<-runs
	fake.Advance(maasPruneInterval)
	<-runs
	fake.Advance(maasPruneInterval)
	<-runs
	cancel()
	<-done

	activeProfile = ProfileGitOps
	if StartMaaSPruneJob(context.Background()) != nil {
		t.Fatal("started in the gitops profile")
	}
}
