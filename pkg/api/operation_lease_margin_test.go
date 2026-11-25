package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Another pod may take the lease over once its heartbeat is leaseTTL old
// (cluster.LeaseLive). The holder must stop one heartbeat interval before
// that, leaseLossAfter after the heartbeat its last successful write put
// into the lease, however long that write took and whenever its later
// renewals fail. These tests pin that instant on the fake clock.

// stepTo advances the test clock to at and waits until the heartbeat
// handled the ticks on the way.
func stepTo(at time.Time) {
	testClock.Advance(at.Sub(testClock.Now()))
	testClock.WaitForTicksHandled()
}

// heartbeatStopped reports whether the heartbeat stopped the running
// operation. lose() marks it before the heartbeat stops its ticker, so
// after WaitForTicksHandled the answer covers every tick handled so far
// (the operation's own report of the stop, on the cause channel, comes
// from another goroutine later).
func heartbeatStopped() bool {
	op := inflight.snapshot()
	return op == nil || op.lockLost
}

func requireNotStopped(t *testing.T) {
	t.Helper()
	if heartbeatStopped() {
		t.Fatalf("stopped at %s, before the threshold", testClock.Now())
	}
}

func requireHeartbeatAt(t *testing.T, l types.OperationLease, want time.Time) {
	t.Helper()
	if l.HeartbeatAt != want.UTC().Format(time.RFC3339) {
		t.Fatalf("lease heartbeatAt %s, want %s", l.HeartbeatAt, want.UTC().Format(time.RFC3339))
	}
}

// A renewal that succeeds only after leaseWriteTimeout, then renewals that
// fail at once: the heartbeat counts from the time written into the lease
// (the start of the slow write), not from when the write returned.
func TestLeaseLostOneIntervalBeforeExpiryAfterASlowRenewal(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	leaseHeartbeatInterval, leaseTTL = 10*time.Second, 45*time.Second
	wrapLockWrites(func(c *cluster.Client, err error) error {
		f.mu.Lock()
		n := f.attempts
		f.mu.Unlock()
		if n == 2 { // the first renewal succeeds after leaseWriteTimeout
			testClock.Advance(leaseWriteTimeout)
		}
		return err
	})
	started, cause, finish := refreshUntilStoppedHeld()
	done := runRefreshInBackground(t)
	<-started
	start := testClock.Now()
	f.beat(t) // renewal stamped at start+10s, returns at start+15s
	renewed := start.Add(leaseHeartbeatInterval)
	f.mu.Lock()
	f.failNext = 1 << 30
	f.mu.Unlock()

	// Other pods may take over at renewed+leaseTTL; stop by then minus one interval.
	stopBy := renewed.Add(leaseTTL - leaseHeartbeatInterval)
	for tick := renewed.Add(leaseHeartbeatInterval); tick.Before(stopBy); tick = tick.Add(leaseHeartbeatInterval) {
		stepTo(tick) // a renewal that fails at once
		requireNotStopped(t)
	}
	stepTo(stopBy)
	// Blocks until the operation stops: no real-time bound (a hang fails
	// at go test's -timeout), so a loaded machine cannot fail the test.
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	// The heartbeat itself stops at the deadline, not the operation's end
	// (which the scheduler might run first).
	heartbeatExited(t)
	close(finish)
	<-done
	if !testClock.Now().Equal(stopBy) {
		t.Fatalf("clock moved to %s", testClock.Now())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 2 {
		t.Fatalf("%d successful lease writes, want the acquire and one renewal", len(f.writes))
	}
	requireHeartbeatAt(t, f.writes[0], start)
	requireHeartbeatAt(t, f.writes[1], renewed)
}

// The acquire's write took 3s, so the ticks come 3s after each multiple of
// the interval, and the renewal due just before the threshold hangs: the
// threshold still counts from the acquire's heartbeat, and it ends the
// renewal in flight instead of waiting for its write timeout.
func TestLeaseLostOneIntervalBeforeExpiryDuringAHangingRenewal(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	leaseHeartbeatInterval, leaseTTL = 10*time.Second, 45*time.Second
	const acquireTakes = 3 * time.Second
	entered := make(chan struct{}, 1)
	wrapLockWrites(func(c *cluster.Client, err error) error {
		f.mu.Lock()
		n := f.attempts
		f.mu.Unlock()
		switch n {
		case 1:
			testClock.Advance(acquireTakes)
		case 4: // the third renewal hangs until its context ends
			entered <- struct{}{}
			<-c.Context().Done()
			return c.Context().Err()
		}
		return err
	})
	stamped := testClock.Now()
	started, cause, finish := refreshUntilStoppedHeld()
	done := runRefreshInBackground(t)
	<-started
	f.mu.Lock()
	f.failNext = 1 << 30
	f.mu.Unlock()

	stopBy := stamped.Add(leaseTTL - leaseHeartbeatInterval) // other pods may take over at stamped+leaseTTL
	first := stamped.Add(acquireTakes + leaseHeartbeatInterval)
	stepTo(first) // fails at once
	requireNotStopped(t)
	stepTo(first.Add(leaseHeartbeatInterval)) // fails at once
	requireNotStopped(t)
	testClock.Advance(leaseHeartbeatInterval) // the third renewal starts and hangs
	<-entered
	requireNotStopped(t)
	testClock.Advance(stopBy.Sub(testClock.Now()))
	// Blocks until the operation stops (see above).
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	// The heartbeat itself stops at the deadline, not the operation's end
	// (which the scheduler might run first).
	heartbeatExited(t)
	close(finish)
	<-done
	if !testClock.Now().Equal(stopBy) {
		t.Fatalf("clock moved to %s", testClock.Now())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 1 {
		t.Fatalf("%d successful lease writes, want the acquire only", len(f.writes))
	}
	requireHeartbeatAt(t, f.writes[0], stamped)
}

// lockedBuffer is a log sink that goroutines may write while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// The deadline stops the operation by itself: a renewal write that hangs
// and ignores its context (the deadline cancels it) does not keep the
// operation running. When that write later returns, even with success, the
// lease stays lost, and the stop is reported once.
func TestLeaseLostAtTheDeadlineWhileARenewalIgnoresItsContext(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	logs := &lockedBuffer{}
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	leaseHeartbeatInterval, leaseTTL = 10*time.Second, 45*time.Second
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// The write goes through, but answers late.
	wrapLockWrites(func(c *cluster.Client, err error) error {
		f.mu.Lock()
		n := f.attempts
		f.mu.Unlock()
		if n == 2 { // the first renewal hangs until released, whatever its context does
			defer close(returned)
			close(entered)
			<-release
		}
		return err
	})
	// Refresh reports the stop, then waits to return until the test lets it,
	// so the heartbeat is not stopped by the operation's end meanwhile.
	started, cause, finish := make(chan struct{}), make(chan error, 1), make(chan struct{})
	runRefreshStream = func(c *cluster.Client, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		emit(cluster.UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting"})
		close(started)
		<-c.Context().Done()
		cause <- context.Cause(c.Context())
		<-finish
		return &types.OperationResponse{Success: false, Message: "stopped"}, c.Context().Err()
	}
	done := runRefreshInBackground(t)
	<-started
	stamped := testClock.Now()
	testClock.Advance(leaseHeartbeatInterval) // the first renewal starts and hangs
	<-entered
	testClock.Advance(stamped.Add(leaseLossAfter()).Sub(testClock.Now()))
	// Blocks until the operation stops (no real-time bound, see above);
	// release is still open, so the write has not returned.
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	if !testClock.Now().Equal(stamped.Add(leaseLossAfter())) {
		t.Fatalf("clock moved to %s", testClock.Now())
	}
	close(release)
	<-returned
	testClock.WaitForTicksHandled() // the heartbeat handled the late result and stopped its ticker
	if n := logs.count("operation stopped"); n != 1 {
		t.Fatalf("the stop was reported %d times, want once", n)
	}
	if !heartbeatStopped() {
		t.Fatal("the operation is not marked as having lost its lock")
	}
	close(finish)
	<-done
	if last := inflight.lastCompleted(); last == nil || last.Success || last.Message != cluster.LockLostMessage {
		t.Fatalf("lastCompleted %+v", last)
	}
	if leases.stopAll() != 0 {
		t.Fatal("the heartbeat is still registered after the operation ended")
	}
}

// Lease heartbeats are whole seconds (RFC 3339): one stamped at x.7s reads
// as x.0s to every pod. The holder's deadline follows that truncated
// heartbeat, so it never stops later than one interval before the other
// pods count the lease as expired (cluster.LeaseLive).
func TestLeaseDeadlineFollowsTheHeartbeatTruncatedToTheSecond(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	leaseHeartbeatInterval, leaseTTL = 10*time.Second, 45*time.Second
	testClock.Advance(700 * time.Millisecond) // every stamp below falls at .7s
	started, cause, finish := refreshUntilStoppedHeld()
	done := runRefreshInBackground(t)
	<-started
	f.beat(t) // the renewal at 10.7s succeeds: heartbeatAt 10s
	f.mu.Lock()
	f.failNext = 1 << 30
	last := f.writes[len(f.writes)-1]
	f.mu.Unlock()
	beat, err := cluster.LeaseHeartbeat(&last)
	if err != nil {
		t.Fatal(err)
	}
	if want := clocktest.Epoch.Add(10 * time.Second); !beat.Equal(want) {
		t.Fatalf("heartbeat %s, want %s (the stamp at 10.7s truncated)", beat, want)
	}
	margin := leaseTTL - leaseLossAfter()
	stopBy := beat.Add(leaseLossAfter()) // 45s, not 45.7s
	// Other pods count the lease as expired exactly margin after stopBy.
	if !cluster.LeaseLive(&last, stopBy.Add(margin-time.Nanosecond), leaseTTL) || cluster.LeaseLive(&last, stopBy.Add(margin), leaseTTL) {
		t.Fatalf("the lease does not expire at %s", stopBy.Add(margin))
	}
	for range 3 { // renewals at 20.7s, 30.7s and 40.7s fail
		f.beat(t)
		requireNotStopped(t)
	}
	stepTo(stopBy)
	// Blocks until the operation stops (see above). Stopping later, at
	// 45.7s (the untruncated stamp + 35s), would hang here.
	if err := <-cause; !errors.Is(err, cluster.ErrOperationLockLost) {
		t.Fatalf("cause %v", err)
	}
	// The heartbeat itself stops at the deadline, not the operation's end
	// (which the scheduler might run first).
	heartbeatExited(t)
	close(finish)
	<-done
	if !testClock.Now().Equal(stopBy) {
		t.Fatalf("clock moved to %s", testClock.Now())
	}
}
