package api

import (
	"testing"
	"time"
)

// Thin wrappers and accessors that only tests use; production code calls
// the underlying functions directly.

// setDev replaces the development setup read at startup for one test.
func setDev(t *testing.T, cfg DevConfig) {
	t.Helper()
	old := devConfig.Load()
	devConfig.Store(&cfg)
	t.Cleanup(func() { devConfig.Store(old) })
}

// setDevMode turns DEV_MODE on or off for one test, keeping DEV_TOKEN and
// DEV_USER.
func setDevMode(t *testing.T, on bool) {
	t.Helper()
	cfg := Dev()
	cfg.Mode = on
	setDev(t, cfg)
}

func (rl *rateLimiter) isRateLimited(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	last, ok := rl.users[key]
	return ok && rl.now().Sub(last) < rl.window
}

func (a *apiReachability) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheck, a.lastSuccess, a.lastErr = time.Time{}, time.Time{}, nil
}

func (t *operationTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.current, t.last = nil, nil
}

// reset drops a pending completion, so a running retry loop stops at its
// next attempt without calling the (soon restored) seams again.
func (m *markerWriter) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending, m.owner, m.client = nil, nil, nil
	m.delays = defaultMarkerRetryDelays
	m.retrying = false
	m.epoch++
	if m.stopRetry != nil {
		m.stopRetry() // a retry asleep on an earlier test's fake clock
	}
}

// waitRetries waits until the background retries, if any run, have ended.
func (m *markerWriter) waitRetries() {
	m.mu.Lock()
	done := m.retryDone
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *identityCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[[32]byte]identityEntry{}
}

// EmitStep creates an UpdateStep with the elapsed time auto-calculated from
// when the SSEWriter was created, then sends it as an SSE event.
func (s *SSEWriter) EmitStep(step, status, message string) error {
	return s.SendStep(UpdateStep{
		Step:      step,
		Status:    status,
		Message:   message,
		ElapsedMs: time.Since(s.start).Milliseconds(),
	})
}

// EmitStepWithDetail is like EmitStep but also sets the Detail field for
// additional context (e.g., an image URL or channel name).
func (s *SSEWriter) EmitStepWithDetail(step, status, message, detail string) error {
	return s.SendStep(UpdateStep{
		Step:      step,
		Status:    status,
		Message:   message,
		Detail:    detail,
		ElapsedMs: time.Since(s.start).Milliseconds(),
	})
}
