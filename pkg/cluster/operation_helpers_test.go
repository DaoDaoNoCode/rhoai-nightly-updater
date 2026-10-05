package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func helperServer(t *testing.T, h http.HandlerFunc) (*Client, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: ctx}, cancel
}

func TestApplySubscriptionWithRetry(t *testing.T) {
	sub := map[string]interface{}{"kind": "Subscription"}
	cases := []struct {
		name          string
		failures      int32
		wantApplyErr  bool
		wantCallbacks []string
	}{
		{"first try", 0, false, nil},
		{"recovers on third attempt", 2, false, []string{"1:retry", "2:retry"}},
		{"gives up after three attempts", 5, true, []string{"1:retry", "2:retry", "3:final"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			c, _ := helperServer(t, func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) <= tc.failures {
					w.WriteHeader(500)
					return
				}
				fmt.Fprint(w, `{}`)
			})
			var got []string
			applyErr, cancelErr := applySubscriptionWithRetry(c, "/sub", sub, func(attempt int, err error, willRetry bool, backoff time.Duration) {
				state := "final"
				if willRetry {
					state = "retry"
					if backoff != SubRetryBackoffs[attempt-1] {
						t.Errorf("attempt %d backoff %s", attempt, backoff)
					}
				}
				got = append(got, fmt.Sprintf("%d:%s", attempt, state))
			})
			if cancelErr != nil || (applyErr != nil) != tc.wantApplyErr || fmt.Sprint(got) != fmt.Sprint(tc.wantCallbacks) {
				t.Fatalf("applyErr=%v cancelErr=%v callbacks=%v", applyErr, cancelErr, got)
			}
		})
	}
}

func TestApplySubscriptionWithRetry_CanceledWhileWaiting(t *testing.T) {
	c, cancel := helperServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	applyErr, cancelErr := applySubscriptionWithRetry(c, "/sub", map[string]interface{}{}, func(int, error, bool, time.Duration) { cancel() })
	if applyErr == nil || !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("applyErr=%v cancelErr=%v", applyErr, cancelErr)
	}
}

func TestWaitForNightlyCatalogReady(t *testing.T) {
	var polls int32
	c, _ := helperServer(t, func(w http.ResponseWriter, r *http.Request) {
		state := "CONNECTING"
		if atomic.AddInt32(&polls, 1) >= 2 {
			state = "READY"
		}
		fmt.Fprintf(w, `{"spec":{"image":"x"},"status":{"connectionState":{"lastObservedState":%q}}}`, state)
	})
	var logs []string
	var events []UpdateStepEvent
	ready, err := waitForNightlyCatalogReady(c, "img", func(e UpdateStepEvent) { events = append(events, e) }, &logs)
	if err != nil || !ready || len(logs) != 2 || len(events) != 2 || events[1].Detail != "READY" {
		t.Fatalf("ready=%v err=%v logs=%v events=%v", ready, err, logs, events)
	}

	c, _ = helperServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":{"connectionState":{"lastObservedState":"TRANSIENT_FAILURE"}}}`)
	})
	if ready, err := waitForNightlyCatalogReady(c, "img", func(UpdateStepEvent) {}, &logs); ready || err != nil {
		t.Fatalf("timeout: ready=%v err=%v", ready, err)
	}
}
