// Package clocktest provides a fake clock.Clock for tests.
package clocktest

import (
	"context"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock"
)

// Epoch is the time a Fake starts at. It lies far ahead of the wall clock
// because a context of the fake reports its deadline, and the network stack
// turns such a deadline into a socket deadline in wall-clock time: one in
// the past would fail every request of a test that runs longer than its
// fake timeout.
var Epoch = time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)

// Fake is a clock.Clock whose time moves only when the test advances it
// (Advance) or, for a Fake made by NewAutoFake, when the code sleeps.
// Deadlines of its contexts and ticks of its tickers fire as time passes
// them, in time order, before Advance or Sleep returns.
type Fake struct {
	mu       sync.Mutex
	now      time.Time
	auto     bool
	timers   map[*timer]struct{}
	sleepers int
	changed  chan struct{} // closed and replaced when sleepers changes
}

type timer struct {
	when   time.Time
	period time.Duration // > 0 for a ticker
	sleep  bool          // a Sleep waits on it
	fire   func()
}

var _ clock.Clock = (*Fake)(nil)

// NewFake returns a Fake at Epoch whose time moves only by Advance: Sleep
// blocks until the test advances the clock past its end.
func NewFake() *Fake {
	return &Fake{now: Epoch, timers: map[*timer]struct{}{}, changed: make(chan struct{})}
}

// NewAutoFake returns a Fake at Epoch on which Sleep advances the time to
// its own end (or to the deadline that ends its context first) and returns
// at once. Code that polls or backs off then runs its whole schedule
// without waiting, in the order the real clock would give. Goroutines that
// sleep at the same time each move the time to their own end (it never runs
// backwards), so their sleeps overlap as they would in real time.
func NewAutoFake() *Fake {
	f := NewFake()
	f.auto = true
	return f
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since is Now().Sub(t).
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Until is t.Sub(Now()).
func (f *Fake) Until(t time.Time) time.Duration { return t.Sub(f.Now()) }

// Advance moves the time forward by d, firing what falls due on the way.
func (f *Fake) Advance(d time.Duration) {
	f.advance(f.Now().Add(d), nil)
}

// Sleepers returns how many Sleep calls wait for the time to advance.
func (f *Fake) Sleepers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sleepers
}

// WaitForSleepers blocks until at least n Sleep calls wait for the time to
// advance, so a test advances only after the code under test has started
// to wait.
func (f *Fake) WaitForSleepers(n int) {
	for {
		f.mu.Lock()
		if f.sleepers >= n {
			f.mu.Unlock()
			return
		}
		ch := f.changed
		f.mu.Unlock()
		<-ch
	}
}

// Sleep implements clock.Clock.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	if f.auto {
		f.advance(f.Now().Add(d), func() bool { return ctx.Err() != nil })
		return ctx.Err()
	}
	woke := make(chan struct{})
	f.mu.Lock()
	t := &timer{when: f.now.Add(d), sleep: true, fire: func() { close(woke) }}
	f.timers[t] = struct{}{}
	f.setSleepersLocked(f.sleepers + 1)
	f.mu.Unlock()
	select {
	case <-woke:
		return nil
	case <-ctx.Done():
		f.mu.Lock()
		if _, ok := f.timers[t]; ok {
			delete(f.timers, t)
			f.setSleepersLocked(f.sleepers - 1)
		}
		f.mu.Unlock()
		return ctx.Err()
	}
}

func (f *Fake) setSleepersLocked(n int) {
	f.sleepers = n
	close(f.changed)
	f.changed = make(chan struct{})
}

// advance moves the time to target, firing the timers due by then in time
// order. It stops early, at the time of the timer it just fired, when stop
// reports true.
func (f *Fake) advance(target time.Time, stop func() bool) {
	for {
		f.mu.Lock()
		var next *timer
		for t := range f.timers {
			if !t.when.After(target) && (next == nil || t.when.Before(next.when)) {
				next = t
			}
		}
		if next == nil {
			if target.After(f.now) {
				f.now = target
			}
			f.mu.Unlock()
			return
		}
		if next.when.After(f.now) {
			f.now = next.when
		}
		if next.period > 0 {
			// Like time.Ticker, a ticker holds one tick and drops the
			// rest: it fires once per advance however far it goes.
			for !next.when.After(target) {
				next.when = next.when.Add(next.period)
			}
		} else {
			delete(f.timers, next)
			if next.sleep {
				f.setSleepersLocked(f.sleepers - 1)
			}
		}
		f.mu.Unlock()
		next.fire()
		if stop != nil && stop() {
			return
		}
	}
}

// addTimer registers fire to run at when; one already due runs now.
func (f *Fake) addTimer(when time.Time, period time.Duration, fire func()) *timer {
	t := &timer{when: when, period: period, fire: fire}
	f.mu.Lock()
	if period == 0 && !when.After(f.now) {
		f.mu.Unlock()
		fire()
		return t
	}
	f.timers[t] = struct{}{}
	f.mu.Unlock()
	return t
}

func (f *Fake) removeTimer(t *timer) {
	f.mu.Lock()
	delete(f.timers, t)
	f.mu.Unlock()
}

// NewTicker implements clock.Clock.
func (f *Fake) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("clocktest: non-positive interval for NewTicker")
	}
	ft := &fakeTicker{f: f, c: make(chan time.Time, 1)}
	ft.t = f.addTimer(f.Now().Add(d), d, func() {
		select {
		case ft.c <- f.Now():
		default:
		}
	})
	return ft
}

type fakeTicker struct {
	f *Fake
	t *timer
	c chan time.Time
}

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               { t.f.removeTimer(t.t) }

// WithTimeout implements clock.Clock.
func (f *Fake) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return f.WithDeadline(parent, f.Now().Add(d))
}

// WithDeadline implements clock.Clock. Like context.WithDeadline, the
// context ends with context.DeadlineExceeded (also for the contexts derived
// from it) when the fake time reaches d, and with the parent's error when
// the parent ends first.
func (f *Fake) WithDeadline(parent context.Context, d time.Time) (context.Context, context.CancelFunc) {
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The parent's deadline comes first (context.WithDeadline does the same).
		return context.WithCancel(parent)
	}
	dc := &deadlineCtx{parent: parent, deadline: d, done: make(chan struct{}), funcs: map[*func()]struct{}{}}
	stopParent := context.AfterFunc(parent, func() { dc.finish(parent.Err()) })
	t := f.addTimer(d, 0, func() { dc.finish(context.DeadlineExceeded) })
	// The context handed out is a standard one: context.Cause and the
	// children's errors then follow the context package's rules.
	ctx, cancel := context.WithCancel(dc)
	return ctx, func() {
		cancel()
		f.removeTimer(t)
		stopParent()
		dc.finish(context.Canceled)
	}
}

// deadlineCtx is done when its fake deadline passes or its parent is done.
// It implements AfterFunc, which the context package uses to cancel the
// contexts derived from it, with its own error, as soon as it is done.
type deadlineCtx struct {
	parent   context.Context
	deadline time.Time
	done     chan struct{}
	mu       sync.Mutex
	err      error
	funcs    map[*func()]struct{}
}

func (c *deadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *deadlineCtx) Done() <-chan struct{}       { return c.done }
func (c *deadlineCtx) Value(key any) any           { return c.parent.Value(key) }

func (c *deadlineCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *deadlineCtx) finish(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	funcs := c.funcs
	c.funcs = nil
	c.mu.Unlock()
	for fn := range funcs {
		(*fn)()
	}
}

// AfterFunc runs fn when c is done: synchronously from the deadline or
// cancellation that ends c, or in a new goroutine when c is done already
// (the context package calls it with a lock held).
func (c *deadlineCtx) AfterFunc(fn func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		go fn()
		return func() bool { return false }
	}
	key := &fn
	c.funcs[key] = struct{}{}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.funcs[key]; !ok {
			return false
		}
		delete(c.funcs, key)
		return true
	}
}
