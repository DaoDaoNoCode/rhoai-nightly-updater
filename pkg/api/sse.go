package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// UpdateStep represents a single step in a multi-step cluster operation,
// streamed to the client as a Server-Sent Event.
type UpdateStep struct {
	Step      string `json:"step"`                // e.g., "validate_prerequisites", "apply_catalog_source"
	Status    string `json:"status"`              // "running", "success", "failed", "skipped"
	Message   string `json:"message"`             // Human-readable description
	Detail    string `json:"detail,omitempty"`    // Extra info (e.g., image URL, channel name)
	ElapsedMs int64  `json:"elapsedMs"`           // Time since step started
	ErrorCode string `json:"errorCode,omitempty"` // For error categorization
}

// SSEWriter writes Server-Sent Events to an http.ResponseWriter.
// It uses http.ResponseController (Go 1.20+) for flushing, which
// correctly handles wrapped ResponseWriters.
type SSEWriter struct {
	mu     sync.Mutex
	closed bool
	w      http.ResponseWriter
	rc     *http.ResponseController
	start  time.Time
	// onStep sees every progress event, also after the client has gone, so
	// GET /api/operation can report the step of the running operation.
	onStep func(UpdateStep)
}

// NewSSEWriter configures the ResponseWriter for SSE streaming.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// Cluster operations have their own bounded deadline and can outlast the
	// server's ordinary response timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	s := &SSEWriter{
		w:     w,
		rc:    http.NewResponseController(w),
		start: time.Now(),
	}
	if sw, ok := w.(*statusWriter); ok && sw.opID != "" {
		id := sw.opID
		s.onStep = func(step UpdateStep) { inflight.recordStep(id, step) }
	}
	return s, nil
}

// StartTime returns the time when the SSE writer was created, for computing
// elapsed durations in step events.
func (s *SSEWriter) StartTime() time.Time {
	return s.start
}

// SendStep marshals an UpdateStep to JSON and writes it as an SSE data frame.
// The format is "data: {json}\n\n" per the SSE specification.
func (s *SSEWriter) SendStep(step UpdateStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onStep != nil {
		s.onStep(step)
	}
	if s.closed {
		return nil
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	data, err := json.Marshal(step)
	if err != nil {
		slog.Error("sse: failed to marshal step", "step", step.Step, "error", err)
		return fmt.Errorf("marshal step %q: %w", step.Step, err)
	}

	_, err = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if err != nil {
		s.closed = true
		slog.Error("sse: failed to write step", "step", step.Step, "error", err)
		return fmt.Errorf("write step %q: %w", step.Step, err)
	}

	if flushErr := s.rc.Flush(); flushErr != nil {
		s.closed = true
		return flushErr
	}

	slog.Debug("sse: sent step", "step", step.Step, "status", step.Status, "elapsedMs", step.ElapsedMs)
	return nil
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

// SendHeartbeat writes an SSE comment (": heartbeat\n\n") to keep the
// connection alive through proxies with idle timeouts. Per the SSE spec,
// lines starting with ":" are comments and are ignored by EventSource clients.
func (s *SSEWriter) SendHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprint(s.w, ": heartbeat\n\n"); err != nil {
		s.closed = true
		return
	}
	if err := s.rc.Flush(); err != nil {
		s.closed = true
	}
}

// Close prevents heartbeat or progress writes after the handler returns.
func (s *SSEWriter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}
