package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operation marker survives a crash of the updater: it is written to a
// ConfigMap when an operation begins and cleared when it ends, in the same
// write that records the operation as lastCompleted. A marker left by
// another process (another boot ID) means that process died mid-operation
// (a graceful restart drains running operations first). Marker errors never
// block operations.
var (
	saveOperationMarker    = cluster.SaveOperationMarker
	saveCompletedOperation = cluster.SaveCompletedOperation
	readOperationState     = cluster.GetOperationState
)

// markerWriteTimeout bounds each marker write.
const markerWriteTimeout = 5 * time.Second

// defaultMarkerRetryDelays space out the retries of a failed marker clear
// (about 9 minutes in total). A GET /api/operation that finds this
// process's own marker starts another round.
var defaultMarkerRetryDelays = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
}

// markerWriter serialises the writes of the operation marker. A completion
// that could not be written stays pending and is retried in the background
// until it succeeds, a newer operation replaces the marker, or the retries
// run out.
type markerWriter struct {
	mu       sync.Mutex
	pending  *types.CompletedOperation
	client   *cluster.Client
	retrying bool
	delays   []time.Duration
	epoch    int // tests reset the writer; a retry loop of an older epoch stops
}

var markers = &markerWriter{delays: defaultMarkerRetryDelays}

// start records a newly begun operation. It replaces a pending completion:
// the new marker supersedes the old one, and lastCompleted is written when
// the new operation ends.
func (m *markerWriter) start(c *cluster.Client, marker *types.OperationMarker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = nil
	ctx, cancel := context.WithTimeout(context.Background(), markerWriteTimeout)
	defer cancel()
	if err := saveOperationMarker(c.WithContext(ctx), marker); err != nil {
		slog.Warn("could not record the running operation", "operation", marker.ID, "error", err)
	}
}

// complete clears the marker and records done as the last completed
// operation, retrying in the background when the write fails.
func (m *markerWriter) complete(c *cluster.Client, done *types.CompletedOperation) {
	if c == nil {
		return
	}
	// The whole clear, including the wait for a background retry that holds
	// the lock (its write is bounded by markerWriteTimeout), ends within
	// cluster.MarkerClearTimeout: it is part of the shutdown drain budget.
	ctx, cancel := context.WithTimeout(context.Background(), cluster.MarkerClearTimeout)
	defer cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending, m.client = done, c
	if !m.writePendingLocked(ctx) {
		m.retryLaterLocked()
	}
}

// retryPending restarts the background retries of a pending completion.
func (m *markerWriter) retryPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil {
		m.retryLaterLocked()
	}
}

// writePendingLocked writes the pending completion, if any, and reports
// whether nothing is pending anymore. m.mu must be held.
func (m *markerWriter) writePendingLocked(parent context.Context) bool {
	if m.pending == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(parent, markerWriteTimeout)
	defer cancel()
	if err := saveCompletedOperation(m.client.WithContext(ctx), m.pending); err != nil {
		slog.Warn("could not clear the running-operation marker", "operation", m.pending.ID, "error", err)
		return false
	}
	m.pending, m.client = nil, nil
	return true
}

// retryLaterLocked starts the background retries unless they already run.
// m.mu must be held.
func (m *markerWriter) retryLaterLocked() {
	if m.retrying {
		return
	}
	m.retrying = true
	delays, epoch := m.delays, m.epoch
	go func() {
		for _, d := range delays {
			time.Sleep(d)
			m.mu.Lock()
			if m.epoch != epoch {
				m.mu.Unlock()
				return
			}
			if m.writePendingLocked(context.Background()) {
				m.retrying = false
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.epoch != epoch {
			return
		}
		m.retrying = false
		if m.pending != nil {
			slog.Error("could not clear the running-operation marker; a restart will report the operation as interrupted",
				"operation", m.pending.ID)
		}
	}()
}

// FlushOperationMarker makes a last attempt, before the server exits, to
// clear the marker of an operation that finished but whose clear failed.
// Otherwise the next process reports that operation as interrupted.
func FlushOperationMarker(ctx context.Context) {
	markers.mu.Lock()
	defer markers.mu.Unlock()
	markers.writePendingLocked(ctx)
}
