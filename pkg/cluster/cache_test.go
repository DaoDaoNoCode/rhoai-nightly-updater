package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLRUEvictsLeastRecentlyUsedOnly(t *testing.T) {
	c := newLRU[string, int](3)
	c.Add("a", 1, 0)
	c.Add("b", 2, 0)
	c.Add("c", 3, 0)
	if _, ok := c.Get("a"); !ok { // touch a so b becomes the oldest
		t.Fatal("a missing")
	}
	c.Add("d", 4, 0)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should survive (only one entry is evicted per insert)", k)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("len %d", c.Len())
	}
	c.Add("a", 10, 0) // update keeps size
	if v, _ := c.Get("a"); v != 10 || c.Len() != 3 {
		t.Fatalf("update failed: %d len %d", v, c.Len())
	}
	c.Remove("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("removed entry still present")
	}
	c.Purge()
	if c.Len() != 0 {
		t.Fatal("purge failed")
	}
}

func TestLRUExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newLRU[string, string](10)
	c.now = func() time.Time { return now }
	c.Add("ttl", "x", time.Minute)
	c.Add("forever", "y", 0)
	now = now.Add(59 * time.Second)
	if _, stored, ok := c.GetWithAge("ttl"); !ok || !stored.Equal(time.Unix(1000, 0)) {
		t.Fatalf("entry should be live with its store time, got ok=%v stored=%v", ok, stored)
	}
	now = now.Add(time.Second)
	if _, ok := c.Get("ttl"); ok {
		t.Fatal("entry should have expired")
	}
	now = now.Add(1000 * time.Hour)
	if _, ok := c.Get("forever"); !ok {
		t.Fatal("ttl 0 must not expire")
	}
}

func TestLRUConcurrentUse(t *testing.T) {
	c := newLRU[int, int](50)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c.Add(g*1000+i, i, time.Minute)
				c.Get(g*1000 + i/2)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() > 50 {
		t.Fatalf("bound exceeded: %d", c.Len())
	}
}

func TestFlightGroupCollapsesConcurrentCalls(t *testing.T) {
	var g flightGroup[int]
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) {
				calls.Add(1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", calls.Load())
	}
	for _, v := range results {
		if v != 42 {
			t.Fatalf("result %d", v)
		}
	}
	// Later calls run again (no result caching in the group itself).
	g.Do(context.Background(), "k", func(context.Context) (int, error) { calls.Add(1); return 1, nil })
	if calls.Load() != 2 {
		t.Fatalf("expected a fresh call after completion, got %d", calls.Load())
	}
}

func TestFlightGroupWaiterRetriesWhenLeaderCanceled(t *testing.T) {
	var g flightGroup[string]
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err, _ := g.Do(leaderCtx, "k", func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", fmt.Errorf("fetch: %w", ctx.Err())
		})
		leaderDone <- err
	}()
	<-started
	waiter := make(chan string, 1)
	go func() {
		v, err, _ := g.Do(context.Background(), "k", func(context.Context) (string, error) { return "fresh", nil })
		if err != nil {
			t.Error(err)
		}
		waiter <- v
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err %v", err)
	}
	if v := <-waiter; v != "fresh" {
		t.Fatalf("waiter should rerun the call with its own context, got %q", v)
	}
}

func TestFlightGroupWaiterHonorsOwnContext(t *testing.T) {
	var g flightGroup[int]
	block := make(chan struct{})
	defer close(block)
	go g.Do(context.Background(), "k", func(context.Context) (int, error) { <-block; return 1, nil })
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err, shared := g.Do(ctx, "k", func(context.Context) (int, error) { return 2, nil })
	if !errors.Is(err, context.DeadlineExceeded) || !shared {
		t.Fatalf("expected waiter deadline, got err=%v shared=%v", err, shared)
	}
}
