// Package clock is the time source of every wait in the updater: polls,
// retry backoffs, deadlines that bound them, and periodic work. Production
// code uses Real; tests use clocktest.Fake, so that no test waits in real
// time and the order of a wait and the events it races is set by the test.
package clock

import (
	"context"
	"time"
)

// Clock tells the time and waits. Time read for a wait or a deadline must
// come from the same Clock as the wait itself.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Until(t time.Time) time.Duration
	// Sleep waits for d. It returns ctx.Err() when ctx ends first, and nil
	// otherwise; it is the clock's form of
	//
	//	select {
	//	case <-ctx.Done():
	//	case <-time.After(d):
	//	}
	Sleep(ctx context.Context, d time.Duration) error
	// WithTimeout and WithDeadline are context.WithTimeout and
	// context.WithDeadline with the deadline kept by this clock.
	WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)
	WithDeadline(parent context.Context, d time.Time) (context.Context, context.CancelFunc)
	// NewTicker is time.NewTicker.
	NewTicker(d time.Duration) Ticker
}

// Ticker is the clock's time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real is the wall clock.
var Real Clock = realClock{}

type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }
func (realClock) Until(t time.Time) time.Duration { return time.Until(t) }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (realClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

func (realClock) WithDeadline(parent context.Context, d time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, d)
}

func (realClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
