package api

import "time"

// Thin wrappers and accessors that only tests use; production code calls
// the underlying functions directly.

func (rl *rateLimiter) isRateLimited(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	last, ok := rl.users[key]
	return ok && time.Since(last) < rl.window
}

func (a *apiReachability) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCheck, a.lastSuccess, a.lastErr = time.Time{}, time.Time{}, nil
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
