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

func runRefreshInBackground() chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
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
	leaseHeartbeatInterval = 5 * time.Millisecond
	started, cause, _ := refreshUntilStopped()
	done := runRefreshInBackground()
	<-started
	f.setLease(otherPodLease(time.Now())) // another pod took the lock over
	select {
	case err := <-cause:
		if !errors.Is(err, cluster.ErrOperationLockLost) {
			t.Fatalf("cause %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the operation was not stopped")
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
	leaseHeartbeatInterval, leaseTTL = 5*time.Millisecond, 80*time.Millisecond

	// One failure, then renewals succeed again: the operation goes on.
	started, cause, release := refreshUntilStopped()
	done := runRefreshInBackground()
	<-started
	f.mu.Lock()
	f.failNext = 1
	f.mu.Unlock()
	select {
	case err := <-cause:
		t.Fatalf("a single failed renewal stopped the operation: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	<-done
	if last := inflight.lastCompleted(); last == nil || !last.Success {
		t.Fatalf("lastCompleted %+v", last)
	}

	// Every renewal fails: stopped once leaseTTL-interval has passed.
	mutationLimiter = newRateLimiter(30 * time.Second)
	started, cause, _ = refreshUntilStopped()
	done = runRefreshInBackground()
	<-started
	begin := time.Now()
	f.mu.Lock()
	f.failNext = 1 << 30
	f.mu.Unlock()
	select {
	case err := <-cause:
		if !errors.Is(err, cluster.ErrOperationLockLost) || time.Since(begin) < leaseTTL-3*leaseHeartbeatInterval {
			t.Fatalf("cause %v after %s", err, time.Since(begin))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the operation was not stopped")
	}
	<-done
	if last := inflight.lastCompleted(); last == nil || last.Success || last.Message != cluster.LockLostMessage {
		t.Fatalf("lastCompleted %+v", last)
	}
}
