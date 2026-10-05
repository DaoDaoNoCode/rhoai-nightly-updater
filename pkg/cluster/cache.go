package cluster

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// lruCache is a size-bounded, concurrency-safe LRU map whose entries may also
// expire. Adding to a full cache evicts the least recently used entry only,
// so hot entries survive bursts of new keys.
type lruCache[K comparable, V any] struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[K]*list.Element
	now   func() time.Time
}

type lruEntry[K comparable, V any] struct {
	key     K
	val     V
	stored  time.Time
	expires time.Time // zero: never expires
}

func newLRU[K comparable, V any](max int) *lruCache[K, V] {
	return &lruCache[K, V]{max: max, ll: list.New(), items: make(map[K]*list.Element), now: time.Now}
}

// Get returns the value for key if present and not expired.
func (c *lruCache[K, V]) Get(key K) (V, bool) {
	v, _, ok := c.GetWithAge(key)
	return v, ok
}

// GetWithAge also reports when the entry was stored, for stale-while-revalidate callers.
func (c *lruCache[K, V]) GetWithAge(key K) (V, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, time.Time{}, false
	}
	e := el.Value.(*lruEntry[K, V])
	if !e.expires.IsZero() && !c.now().Before(e.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return zero, time.Time{}, false
	}
	c.ll.MoveToFront(el)
	return e.val, e.stored, true
}

// Add stores key. ttl <= 0 means the entry only leaves the cache through eviction.
func (c *lruCache[K, V]) Add(key K, val V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var expires time.Time
	if ttl > 0 {
		expires = now.Add(ttl)
	}
	if el, ok := c.items[key]; ok {
		e := el.Value.(*lruEntry[K, V])
		e.val, e.stored, e.expires = val, now, expires
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry[K, V]{key: key, val: val, stored: now, expires: expires})
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry[K, V]).key)
	}
}

// Remove deletes key if present.
func (c *lruCache[K, V]) Remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

// Len returns the number of stored entries, including expired ones not yet removed.
func (c *lruCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Purge removes every entry.
func (c *lruCache[K, V]) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[K]*list.Element)
}

// flightGroup collapses concurrent calls for the same key into one, like
// golang.org/x/sync/singleflight, without adding a dependency. A waiter whose
// own context ends stops waiting; if the shared call failed only because the
// leader's context ended, a waiter with a live context runs the call itself.
type flightGroup[V any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[V]
}

type flightCall[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// Do runs fn once per key at a time and returns its result to every caller.
// shared reports whether the result came from another caller's run.
func (g *flightGroup[V]) Do(ctx context.Context, key string, fn func(ctx context.Context) (V, error)) (v V, err error, shared bool) {
	for {
		g.mu.Lock()
		if g.calls == nil {
			g.calls = make(map[string]*flightCall[V])
		}
		if call, ok := g.calls[key]; ok {
			g.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				var zero V
				return zero, ctx.Err(), true
			}
			if isContextError(call.err) && ctx.Err() == nil {
				continue // the leader gave up; try again with this caller's context
			}
			return call.val, call.err, true
		}
		call := &flightCall[V]{done: make(chan struct{})}
		g.calls[key] = call
		g.mu.Unlock()

		func() {
			defer func() {
				g.mu.Lock()
				delete(g.calls, key)
				g.mu.Unlock()
				close(call.done)
			}()
			call.val, call.err = fn(ctx)
		}()
		return call.val, call.err, false
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
