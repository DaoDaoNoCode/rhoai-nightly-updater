package clocktest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock"
)

func TestAutoFakeSleepAdvances(t *testing.T) {
	f := NewAutoFake()
	if err := f.Sleep(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := f.Since(Epoch); got != 3*time.Second {
		t.Fatalf("elapsed %s", got)
	}
	if err := f.Sleep(context.Background(), 0); err != nil || f.Since(Epoch) != 3*time.Second {
		t.Fatalf("zero sleep: %v, elapsed %s", err, f.Since(Epoch))
	}
}

// A sleep longer than the context's deadline ends at the deadline with
// DeadlineExceeded, as with the real clock.
func TestAutoFakeSleepEndsAtTheDeadline(t *testing.T) {
	f := NewAutoFake()
	ctx, cancel := f.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || !d.Equal(Epoch.Add(2*time.Second)) {
		t.Fatalf("deadline %v %v", d, ok)
	}
	err := f.Sleep(ctx, 10*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if got := f.Since(Epoch); got != 2*time.Second {
		t.Fatalf("elapsed %s", got)
	}
	if err := f.Sleep(ctx, time.Second); !errors.Is(err, context.DeadlineExceeded) || f.Since(Epoch) != 2*time.Second {
		t.Fatalf("sleep on a done context: %v, elapsed %s", err, f.Since(Epoch))
	}
}

// Contexts derived from a fake deadline context end with its error and
// cause, like the children of a context.WithDeadline.
func TestFakeDeadlineContextChildren(t *testing.T) {
	f := NewFake()
	ctx, cancel := f.WithTimeout(context.Background(), time.Second)
	defer cancel()
	child, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	grandchild, cancelGrandchild := context.WithTimeout(child, time.Hour)
	defer cancelGrandchild()
	f.Advance(999 * time.Millisecond)
	if ctx.Err() != nil || grandchild.Err() != nil {
		t.Fatalf("done early: %v %v", ctx.Err(), grandchild.Err())
	}
	f.Advance(time.Millisecond)
	for name, c := range map[string]context.Context{"ctx": ctx, "child": child, "grandchild": grandchild} {
		select {
		case <-c.Done():
		default:
			t.Fatalf("%s not done", name)
		}
		if !errors.Is(c.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(c), context.DeadlineExceeded) {
			t.Fatalf("%s: err %v cause %v", name, c.Err(), context.Cause(c))
		}
	}
}

func TestFakeDeadlineContextCancel(t *testing.T) {
	f := NewFake()
	parent, cancelParent := context.WithCancelCause(context.Background())
	ctx, cancel := f.WithTimeout(parent, time.Second)
	defer cancel()
	cause := errors.New("lock lost")
	cancelParent(cause)
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) || !errors.Is(context.Cause(ctx), cause) {
		t.Fatalf("err %v cause %v", ctx.Err(), context.Cause(ctx))
	}

	ctx2, cancel2 := f.WithTimeout(context.Background(), time.Second)
	cancel2()
	if !errors.Is(ctx2.Err(), context.Canceled) {
		t.Fatalf("cancel: %v", ctx2.Err())
	}
	f.Advance(time.Hour)
	if !errors.Is(ctx2.Err(), context.Canceled) {
		t.Fatalf("after the deadline: %v", ctx2.Err())
	}

	expired, cancel3 := f.WithDeadline(context.Background(), f.Now())
	defer cancel3()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline in the past: %v", expired.Err())
	}
}

// A parent deadline before the child's wins, as with context.WithDeadline.
func TestFakeDeadlineKeepsTheEarlierParentDeadline(t *testing.T) {
	f := NewFake()
	parent, cancel := f.WithTimeout(context.Background(), time.Second)
	defer cancel()
	child, cancelChild := f.WithTimeout(parent, time.Hour)
	defer cancelChild()
	if d, _ := child.Deadline(); !d.Equal(Epoch.Add(time.Second)) {
		t.Fatalf("deadline %v", d)
	}
	f.Advance(time.Second)
	if !errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Fatalf("child: %v", child.Err())
	}
}

func TestManualFakeSleepWaitsForAdvance(t *testing.T) {
	f := NewFake()
	done := make(chan error)
	go func() { done <- f.Sleep(context.Background(), time.Minute) }()
	f.WaitForSleepers(1)
	f.Advance(59 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("woke early: %v", err)
	default:
	}
	f.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.Sleepers() != 0 {
		t.Fatalf("sleepers %d", f.Sleepers())
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- f.Sleep(ctx, time.Minute) }()
	f.WaitForSleepers(1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep: %v", err)
	}
	if f.Sleepers() != 0 {
		t.Fatalf("sleepers after cancel %d", f.Sleepers())
	}
}

func TestFakeTicker(t *testing.T) {
	f := NewFake()
	tk := f.NewTicker(10 * time.Second)
	f.Advance(9 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("ticked early")
	default:
	}
	f.Advance(time.Minute) // one tick is held, the rest dropped
	if got := <-tk.C(); !got.Equal(Epoch.Add(10 * time.Second)) {
		t.Fatalf("tick at %v", got)
	}
	select {
	case <-tk.C():
		t.Fatal("second tick held")
	default:
	}
	f.Advance(10 * time.Second)
	<-tk.C()
	tk.Stop()
	f.Advance(time.Hour)
	select {
	case <-tk.C():
		t.Fatal("tick after Stop")
	default:
	}
}

func TestRealClock(t *testing.T) {
	c := clock.Real
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep on a cancelled context: %v", err)
	}
	if err := c.Sleep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	dctx, cancel2 := c.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if d, ok := dctx.Deadline(); !ok || c.Until(d) <= 0 {
		t.Fatalf("deadline %v", d)
	}
	tk := c.NewTicker(time.Hour)
	tk.Stop()
}
