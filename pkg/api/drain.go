package api

import (
	"context"
	"sync"
)

// mutationTracker counts in-flight cluster mutations so that shutdown can
// wait for them. Interrupting an operation half-way (for example after the
// Subscription was deleted) would skip its recovery step.
type mutationTracker struct {
	mu       sync.Mutex
	active   int
	draining bool
	idle     chan struct{} // closed once draining and no mutation is active
}

var mutations = &mutationTracker{}

// begin registers a mutation. It returns false once shutdown has started.
func (m *mutationTracker) begin() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return false
	}
	m.active++
	return true
}

func (m *mutationTracker) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active--
	if m.draining && m.active == 0 && m.idle != nil {
		close(m.idle)
		m.idle = nil
	}
}

func (m *mutationTracker) isDraining() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draining
}

// drain rejects new mutations and waits until in-flight ones finish or ctx
// ends. It returns how many mutations are still running.
func (m *mutationTracker) drain(ctx context.Context) int {
	m.mu.Lock()
	m.draining = true
	if m.active == 0 {
		m.mu.Unlock()
		return 0
	}
	if m.idle == nil {
		m.idle = make(chan struct{})
	}
	idle := m.idle
	m.mu.Unlock()

	select {
	case <-idle:
		return 0
	case <-ctx.Done():
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.active
	}
}

// DrainMutations stops accepting new cluster mutations, marks the server not
// ready, and waits for running mutations to finish or ctx to end. It returns
// the number of mutations still running.
func DrainMutations(ctx context.Context) int {
	return mutations.drain(ctx)
}
