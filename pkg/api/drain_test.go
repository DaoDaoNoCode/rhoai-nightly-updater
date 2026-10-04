package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

func withFreshMutationTracker(t *testing.T) {
	t.Helper()
	original := mutations
	mutations = &mutationTracker{}
	t.Cleanup(func() { mutations = original })
}

func TestDrainWaitsForRunningMutation(t *testing.T) {
	withFreshMutationTracker(t)
	setupDevMode(t)
	original := mutationPermission
	mutationPermission = func(context.Context, string) (bool, error) { return true, nil }
	t.Cleanup(func() { mutationPermission = original })

	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	go func() {
		defer close(finished)
		r := httptest.NewRequest("POST", "/api/drain-running", nil)
		r.Header.Set("X-Forwarded-Access-Token", "user-token")
		h(httptest.NewRecorder(), r)
	}()
	<-started

	drained := make(chan int)
	go func() { drained <- DrainMutations(context.Background()) }()

	// New mutations are refused while draining, and readiness fails.
	deadline := time.Now().Add(2 * time.Second)
	for !mutations.isDraining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	called := false
	refused := withMutationAuth(func(*cluster.Client, http.ResponseWriter, *http.Request) { called = true })
	r := httptest.NewRequest("POST", "/api/drain-new", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user-token")
	w := httptest.NewRecorder()
	refused(w, r)
	if w.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("new mutation during drain: status=%d called=%v", w.Code, called)
	}
	ready := httptest.NewRecorder()
	HandleReady(ready, httptest.NewRequest("GET", "/api/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness during drain = %d", ready.Code)
	}

	select {
	case <-drained:
		t.Fatal("drain returned while a mutation was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-finished
	select {
	case n := <-drained:
		if n != 0 {
			t.Fatalf("drain reported %d running", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not finish after the mutation completed")
	}
}

func TestDrainTimeoutReportsRunningMutations(t *testing.T) {
	tracker := &mutationTracker{}
	if !tracker.begin() {
		t.Fatal("begin refused before drain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if n := tracker.drain(ctx); n != 1 {
		t.Fatalf("drain = %d, want 1", n)
	}
	if tracker.begin() {
		t.Fatal("begin accepted after drain started")
	}
	tracker.end()
}

func TestDrainWithNothingRunningReturnsImmediately(t *testing.T) {
	tracker := &mutationTracker{}
	if n := tracker.drain(context.Background()); n != 0 {
		t.Fatalf("drain = %d", n)
	}
}
