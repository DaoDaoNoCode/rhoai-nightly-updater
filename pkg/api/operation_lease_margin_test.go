package api

import (
	"errors"
	"testing"
	"time"

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
	inner := writeOperationLease
	writeOperationLease = func(c *cluster.Client, l *types.OperationLease, held func(*types.OperationLease) bool) (*types.OperationLease, error) {
		holder, err := inner(c, l, held)
		f.mu.Lock()
		n := f.attempts
		f.mu.Unlock()
		if n == 2 { // the first renewal succeeds after leaseWriteTimeout
			testClock.Advance(leaseWriteTimeout)
		}
		return holder, err
	}
	started, cause, _ := refreshUntilStopped()
	done := runRefreshInBackground()
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
	inner := writeOperationLease
	writeOperationLease = func(c *cluster.Client, l *types.OperationLease, held func(*types.OperationLease) bool) (*types.OperationLease, error) {
		holder, err := inner(c, l, held)
		f.mu.Lock()
		n := f.attempts
		f.mu.Unlock()
		switch n {
		case 1:
			testClock.Advance(acquireTakes)
		case 4: // the third renewal hangs until its context ends
			entered <- struct{}{}
			<-c.Context().Done()
			return nil, c.Context().Err()
		}
		return holder, err
	}
	stamped := testClock.Now()
	started, cause, _ := refreshUntilStopped()
	done := runRefreshInBackground()
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
