package cluster

// R6-5 / R7-L5: the bookkeeping after an operation is bounded by real
// deadlines, not only by the budget arithmetic in budget_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

type ctxKey struct{}

func TestPostOperationContext(t *testing.T) {
	work := record.PostDeadlineWork()
	cases := []struct {
		name        string
		deadline    time.Duration // relative to now; 0 = no deadline
		timeout     time.Duration
		wantExpired bool
		wantEndNear time.Duration // relative to now
	}{
		{name: "no deadline: own timeout", timeout: 5 * time.Second, wantEndNear: 5 * time.Second},
		{name: "deadline far ahead: own timeout", deadline: time.Hour, timeout: 5 * time.Second, wantEndNear: 5 * time.Second},
		{name: "deadline just passed: own timeout", deadline: -time.Second, timeout: 5 * time.Second, wantEndNear: 5 * time.Second},
		{name: "shared end comes first", deadline: -(work - 2*time.Second), timeout: 5 * time.Second, wantEndNear: 2 * time.Second},
		{name: "shared end already passed", deadline: -(work + time.Second), timeout: 5 * time.Second, wantExpired: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := context.WithValue(context.Background(), ctxKey{}, "v")
			if tc.deadline != 0 {
				var cancel context.CancelFunc
				parent, cancel = kube.Clock().WithDeadline(parent, kube.Clock().Now().Add(tc.deadline))
				// Cancelling the operation's context must not end the
				// bookkeeping.
				cancel()
			}
			c := newTestClient(testClientOptions{Ctx: parent})
			ctx, cancel := record.PostOperationContext(c, tc.timeout)
			defer cancel()
			if ctx.Value(ctxKey{}) != "v" {
				t.Error("values of the operation's context are lost")
			}
			if tc.wantExpired {
				if ctx.Err() == nil {
					t.Fatal("context still live after the shared end")
				}
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("context ended early: %v", ctx.Err())
			}
			end, ok := ctx.Deadline()
			if !ok {
				t.Fatal("no deadline")
			}
			if d := kube.Clock().Until(end); d != tc.wantEndNear {
				t.Errorf("deadline in %s, want %s", d, tc.wantEndNear)
			}
		})
	}
}

func TestRecordActivity_StalledOrConflictingAPIIsBounded(t *testing.T) {
	t.Cleanup(record.SetActivityWriteTimeoutForTest(300 * time.Millisecond))

	t.Run("stalled API server", func(t *testing.T) {
		c := stallServer(t, 0) // every request takes 5s
		start := kube.Clock().Now()
		record.RecordActivity(c, types.ActivityEntry{Action: "test"})
		if d := kube.Clock().Since(start); d != record.ActivityWriteTimeout {
			t.Fatalf("RecordActivity took %s with a %s budget", d, record.ActivityWriteTimeout)
		}
	})
	t.Run("conflict retries share the budget", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"data":{}}`))
				return
			}
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"kind":"Status","code":409}`))
		}))
		t.Cleanup(srv.Close)
		// Every call takes 150ms: unbounded, 3 attempts of GET+PUT take
		// 900ms. Bounded, the GET is answered and the budget ends the PUT.
		c := withLatency(newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()}), 150*time.Millisecond)
		start := kube.Clock().Now()
		record.RecordActivity(c, types.ActivityEntry{Action: "test"})
		if d := kube.Clock().Since(start); d != record.ActivityWriteTimeout || calls.Load() != 1 {
			t.Fatalf("RecordActivity took %s (%d calls) with a %s budget", d, calls.Load(), record.ActivityWriteTimeout)
		}
	})
	t.Run("after the shared end nothing is written", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(404)
		}))
		t.Cleanup(srv.Close)
		ctx, cancel := kube.Clock().WithDeadline(context.Background(), kube.Clock().Now().Add(-record.PostDeadlineWork()-time.Second))
		defer cancel()
		c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: ctx})
		record.RecordActivity(c, types.ActivityEntry{Action: "test"})
		if n := calls.Load(); n != 0 {
			t.Fatalf("%d requests after the operation's shared end", n)
		}
	})
}
