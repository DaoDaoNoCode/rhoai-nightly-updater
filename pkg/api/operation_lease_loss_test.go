package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// refreshUntilStopped makes Refresh report a step and run until its
// context ends (reporting the cause) or release is closed.
func refreshUntilStopped() (started chan struct{}, cause chan error, release chan struct{}) {
	started, cause, release = make(chan struct{}), make(chan error, 1), make(chan struct{})
	runRefreshStream = func(c *cluster.Client, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		emit(cluster.UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting"})
		close(started)
		select {
		case <-c.Context().Done():
			cause <- context.Cause(c.Context())
			return &types.OperationResponse{Success: false, Message: "stopped"}, c.Context().Err()
		case <-release:
			return &types.OperationResponse{Success: true, Message: "refreshed"}, nil
		}
	}
	return started, cause, release
}

func runRefreshInBackground(t testing.TB) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleRefreshStream(httptest.NewRecorder(), postJSON(t, "/api/refresh/stream", "user:alice", "{}"))
	}()
	return done
}

// Review fix 2: a renewal that finds another holder stops the operation
// with cluster.ErrOperationLockLost, and its result says why. (The cluster
// package test TestLostLockSkipsTheRestore covers the skipped restore.)
func TestLeaseTakeoverStopsTheOperation(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	started, cause, _ := refreshUntilStopped()
	done := runRefreshInBackground(t)
	<-started
	f.setLease(otherPodLease(clk.Now())) // another pod took the lock over
	f.beat(t)
	// Blocks until the operation stops: no real-time bound (a hang fails
	// at go test's -timeout), so a loaded machine cannot fail the test.
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	<-done
	if l := f.currentLease(); l == nil || l.ID != "remote-op" {
		t.Fatalf("the other pod's lease was touched: %+v", l)
	}
	if last := inflight.lastCompleted(); last == nil || last.Success || last.Message != cluster.LockLostMessage {
		t.Fatalf("lastCompleted %+v", last)
	}
}

// Review fix 2: renewals that keep failing until the lease may have expired
// stop the operation; a single failed renewal does not.
func TestRenewalFailuresStopTheOperationOnlyPastTheThreshold(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	// Renewals every 10s; lost once they have failed for 45s-10s = 35s.
	leaseHeartbeatInterval, leaseTTL = 10*time.Second, 45*time.Second

	// One failure, then renewals succeed again: the operation goes on (a
	// stopped heartbeat would not make the second renewal).
	started, _, release := refreshUntilStopped()
	done := runRefreshInBackground(t)
	<-started
	f.mu.Lock()
	f.failNext = 1
	f.mu.Unlock()
	f.beat(t)
	f.beat(t)
	if heartbeatStopped() {
		t.Fatal("a single failed renewal stopped the operation")
	}
	close(release)
	<-done
	if last := inflight.lastCompleted(); last == nil || !last.Success {
		t.Fatalf("lastCompleted %+v", last)
	}

	// Every renewal fails: stopped once leaseTTL-interval has passed since
	// the heartbeat of the last successful write (here the acquire's).
	mutationLimiter = newRateLimiter()
	started, cause, _ := refreshUntilStopped()
	done = runRefreshInBackground(t)
	<-started
	stamped := testClock.Now()
	f.mu.Lock()
	f.failNext = 1 << 30
	f.mu.Unlock()
	// Failed renewals 10s, 20s and 30s after it: the lease still holds.
	for range 3 {
		f.beat(t)
	}
	stepTo(stamped.Add(leaseLossAfter() - time.Second))
	requireNotStopped(t)
	stepTo(stamped.Add(leaseLossAfter())) // 35s: lost, without waiting for the renewal due at 40s
	// Blocks until the operation stops: no real-time bound (a hang fails
	// at go test's -timeout), so a loaded machine cannot fail the test.
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	<-done
	if last := inflight.lastCompleted(); last == nil || last.Success || last.Message != cluster.LockLostMessage {
		t.Fatalf("lastCompleted %+v", last)
	}
}
