package kube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
)

// storageInitializing matches what the API server returns while the watch cache
// for a resource is still coming up (k8s.io/apiserver pkg/storage/cacher/ready.go:125);
// a list observed this right after Connectivity Link went in (F9).
const storageInitializing = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"storage is (re)initializing","reason":"TooManyRequests","details":{"retryAfterSeconds":1},"code":429}`

// retryAnswer describes a single step from retryServer: HTTP status, optional
// Retry-After header ("" means absent), and response body.
type retryAnswer struct {
	status     int
	retryAfter string
	body       string
}

// retryServer walks through answers sequentially, reuses the final one after
// exhaustion, and logs each incoming request body plus Content-Type.
type retryServer struct {
	mu      sync.Mutex
	answers []retryAnswer
	bodies  []string
	types   []string
}

func (s *retryServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *retryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	a := s.answers[min(len(s.bodies), len(s.answers)-1)]
	s.bodies = append(s.bodies, string(body))
	s.types = append(s.types, r.Header.Get("Content-Type"))
	s.mu.Unlock()
	if a.retryAfter != "" {
		w.Header().Set("Retry-After", a.retryAfter)
	}
	w.WriteHeader(a.status)
	_, _ = io.WriteString(w, a.body)
}

// newRetryClient brings up a stub API server with the given answers, wires a
// Client for ctx, and routes every backoff through c (fake clock).
func newRetryClient(t *testing.T, ctx context.Context, c *clocktest.Fake, answers ...retryAnswer) (*retryServer, *Client) {
	t.Helper()
	t.Cleanup(SetClockForTest(c))
	s := &retryServer{answers: answers}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, NewTestClient(TestClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: ctx})
}

var ok200 = retryAnswer{status: http.StatusOK, body: `{"kind":"List","items":[]}`}

// status builds a Kubernetes API Status JSON payload for the given code.
func status(code int, reason string) string {
	return `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"m","reason":"` + reason + `","code":` + strconv.Itoa(code) + `}`
}

// Given 429 plus Retry-After, the client waits per the server hint and retries;
// the follow-up call succeeds (F9: "storage is (re)initializing").
func TestDoRetries429AfterRetryAfter(t *testing.T) {
	clk := clocktest.NewAutoFake()
	s, c := newRetryClient(t, context.Background(), clk,
		retryAnswer{http.StatusTooManyRequests, "3", storageInitializing}, ok200)
	start := clk.Now()
	body, code, err := c.Get("/apis/kuadrant.io/v1beta1/kuadrants")
	if err != nil || code != http.StatusOK || string(body) != ok200.body {
		t.Fatalf("got %d %q %v", code, body, err)
	}
	if n := s.calls(); n != 2 {
		t.Fatalf("%d calls, want 2", n)
	}
	if waited := clk.Since(start); waited != 3*time.Second {
		t.Fatalf("waited %s, want the advertised 3s", waited)
	}
}

// As client-go's checkWait: a 429 and a 5xx are retried only with
// Retry-After in seconds; any other 4xx is never retried.
func TestDoRetriesOnlyWhatClientGoRetries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first retryAnswer
		calls int
	}{
		{"503 with Retry-After", retryAnswer{http.StatusServiceUnavailable, "1", status(503, "ServiceUnavailable")}, 2},
		{"500 with Retry-After 0", retryAnswer{http.StatusInternalServerError, "0", status(500, "InternalError")}, 2},
		{"503 without Retry-After", retryAnswer{http.StatusServiceUnavailable, "", status(503, "ServiceUnavailable")}, 1},
		{"429 without Retry-After", retryAnswer{http.StatusTooManyRequests, "", status(429, "TooManyRequests")}, 1},
		{"429 with an HTTP-date Retry-After", retryAnswer{http.StatusTooManyRequests, "Wed, 25 Nov 2026 07:28:00 GMT", status(429, "TooManyRequests")}, 1},
		{"409 with Retry-After", retryAnswer{http.StatusConflict, "1", status(409, "Conflict")}, 1},
		{"404 with Retry-After", retryAnswer{http.StatusNotFound, "1", status(404, "NotFound")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newRetryClient(t, context.Background(), clocktest.NewAutoFake(), tc.first, ok200)
			_, code, err := c.Get("/api/v1/namespaces")
			if n := s.calls(); n != tc.calls {
				t.Fatalf("%d calls, want %d", n, tc.calls)
			}
			if retried := tc.calls > 1; retried != (err == nil && code == http.StatusOK) {
				t.Fatalf("got %d %v", code, err)
			}
			if tc.calls == 1 && !IsK8sError(err, tc.first.status) {
				t.Fatalf("got %v, want the %d answer", err, tc.first.status)
			}
		})
	}
}

// Once client-go's maxRetries (10) is exhausted, the terminal response is
// handed back and marked as throttling.
func TestDoStopsAfterTenRetries(t *testing.T) {
	clk := clocktest.NewAutoFake()
	s, c := newRetryClient(t, context.Background(), clk, retryAnswer{http.StatusTooManyRequests, "1", storageInitializing})
	start := clk.Now()
	_, code, err := c.Get("/apis/kuadrant.io/v1beta1/kuadrants")
	if n := s.calls(); n != 11 {
		t.Fatalf("%d calls, want 11 (10 retries)", n)
	}
	if waited := clk.Since(start); waited != 10*time.Second {
		t.Fatalf("waited %s, want 10s", waited)
	}
	if status, errorCode := HTTPStatusForError(err); code != http.StatusTooManyRequests || status != http.StatusServiceUnavailable || errorCode != "rate_limited" {
		t.Fatalf("got %d %v: %d %s", code, err, status, errorCode)
	}
	if !strings.Contains(err.Error(), "storage is (re)initializing") {
		t.Fatalf("error %q", err)
	}
}

// retryWaitBudget caps cumulative sleep per request: overshooting waits are
// skipped and the current response is returned unchanged.
func TestDoWaitsAtMostTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		calls      int
		waited     time.Duration
	}{
		{"one wait over the budget", "33", 1, 0},
		{"a huge value", "999999999999", 1, 0},
		{"the second wait would pass it", "20", 2, 20 * time.Second},
		{"waits that fit", "16", 3, 32 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewAutoFake()
			s, c := newRetryClient(t, context.Background(), clk, retryAnswer{http.StatusTooManyRequests, tc.retryAfter, status(429, "TooManyRequests")})
			start := clk.Now()
			_, _, err := c.Get("/api/v1/namespaces")
			if n, waited := s.calls(), clk.Since(start); n != tc.calls || waited != tc.waited {
				t.Fatalf("%d calls after %s, want %d after %s", n, waited, tc.calls, tc.waited)
			}
			if !IsK8sError(err, http.StatusTooManyRequests) {
				t.Fatalf("got %v, want the 429", err)
			}
		})
	}
}

// If Retry-After would outlive the context deadline, no sleep happens—the
// response is immediate rather than a later deadline error.
func TestDoDoesNotWaitPastTheContextDeadline(t *testing.T) {
	clk := clocktest.NewAutoFake()
	ctx, cancel := clk.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, c := newRetryClient(t, ctx, clk, retryAnswer{http.StatusTooManyRequests, "5", storageInitializing}, ok200)
	start := clk.Now()
	_, _, err := c.Get("/apis/kuadrant.io/v1beta1/kuadrants")
	if n, waited := s.calls(), clk.Since(start); n != 1 || waited != 0 {
		t.Fatalf("%d calls after %s", n, waited)
	}
	if !IsK8sError(err, http.StatusTooManyRequests) || ctx.Err() != nil {
		t.Fatalf("got %v (context: %v)", err, ctx.Err())
	}
}

// Canceling the context mid-wait ends the call immediately with the context
// error, the last answer's error in its text.
func TestDoReturnsWhenTheContextEndsDuringTheWait(t *testing.T) {
	clk := clocktest.NewFake() // manual clock: time advances only when the test steps it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, c := newRetryClient(t, ctx, clk, retryAnswer{http.StatusTooManyRequests, "30", storageInitializing}, ok200)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, code, err := c.Get("/apis/kuadrant.io/v1beta1/kuadrants")
		done <- result{code, err}
	}()
	clk.WaitForSleepers(1)
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) || IsK8sError(r.err, 0) || r.code != 0 {
		t.Fatalf("got %d %v", r.code, r.err)
	}
	if !strings.Contains(r.err.Error(), "storage is (re)initializing") {
		t.Fatalf("error %q does not name the last answer", r.err)
	}
	if n := s.calls(); n != 1 {
		t.Fatalf("%d calls, want 1", n)
	}
	if code, errorCode := HTTPStatusForError(r.err); code != http.StatusServiceUnavailable || errorCode != "canceled" {
		t.Fatalf("classified %d %s", code, errorCode)
	}
}

// A 5xx with Retry-After is retried for a read only: a write the server
// answered 5xx may have been applied. A 429 is refused before the request
// is processed, so a write is retried too (TestDoReplaysTheBody).
func TestDoRetries5xxOnlyForReads(t *testing.T) {
	for _, tc := range []struct {
		method string
		calls  int
	}{
		{http.MethodGet, 2}, {http.MethodHead, 2},
		{http.MethodPost, 1}, {http.MethodPut, 1}, {http.MethodPatch, 1}, {http.MethodDelete, 1},
	} {
		t.Run(tc.method, func(t *testing.T) {
			s, c := newRetryClient(t, context.Background(), clocktest.NewAutoFake(),
				retryAnswer{http.StatusServiceUnavailable, "1", status(503, "ServiceUnavailable")}, ok200)
			_, _, _ = c.Do(tc.method, "/api/v1/namespaces/ns/configmaps/x", "application/json", []byte(`{}`), nil)
			if n := s.calls(); n != tc.calls {
				t.Fatalf("%d calls, want %d", n, tc.calls)
			}
		})
	}
}

// The retry's log names no path: an object name can be secret (the
// digest of the user's OAuth access token in ReadSessionToken).
func TestDoRetryLogNamesNoPath(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	_, c := newRetryClient(t, context.Background(), clocktest.NewAutoFake(),
		retryAnswer{http.StatusTooManyRequests, "1", status(429, "TooManyRequests")}, ok200)
	const name = "sha256~secretdigest"
	if _, _, err := c.Get("/apis/oauth.openshift.io/v1/useroauthaccesstokens/" + name); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "asked to retry") {
		t.Fatalf("no retry logged: %q", logged)
	}
	for _, secret := range []string{name, "useroauthaccesstokens", "/apis/"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the log names %q: %q", secret, logged)
		}
	}
}

// A retried write sends the same body and content type again.
func TestDoReplaysTheBody(t *testing.T) {
	s, c := newRetryClient(t, context.Background(), clocktest.NewAutoFake(),
		retryAnswer{http.StatusTooManyRequests, "1", status(429, "TooManyRequests")},
		retryAnswer{http.StatusCreated, "", `{}`})
	const sent = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`
	if _, code, err := c.Post("/api/v1/namespaces/ns/configmaps", []byte(sent)); err != nil || code != http.StatusCreated {
		t.Fatalf("got %d %v", code, err)
	}
	if len(s.bodies) != 2 || s.bodies[0] != sent || s.bodies[1] != sent || s.types[0] != "application/json" || s.types[1] != "application/json" {
		t.Fatalf("bodies %q, content types %q", s.bodies, s.types)
	}
}

// NoRetryWhen's predicate suppresses retries for matching errors only on that
// derived client; the original client keeps retrying as usual.
func TestNoRetryWhen(t *testing.T) {
	s, c := newRetryClient(t, context.Background(), clocktest.NewAutoFake(), retryAnswer{http.StatusTooManyRequests, "1", storageInitializing}, ok200)
	skip := func(err error) bool { return strings.Contains(err.Error(), "(re)initializing") }
	if _, _, err := c.NoRetryWhen(skip).Get("/apis/kuadrant.io/v1beta1/kuadrants"); !IsK8sError(err, http.StatusTooManyRequests) || s.calls() != 1 {
		t.Fatalf("got %v after %d calls", err, s.calls())
	}
	s.mu.Lock()
	s.bodies = nil
	s.mu.Unlock()
	if _, code, err := c.Get("/apis/kuadrant.io/v1beta1/kuadrants"); err != nil || code != http.StatusOK || s.calls() != 2 {
		t.Fatalf("got %d %v after %d calls", code, err, s.calls())
	}
}
