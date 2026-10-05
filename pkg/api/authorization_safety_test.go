package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

func TestMutationAuthorizationAndPermissionsAgree(t *testing.T) {
	setupDevMode(t)
	original := mutationPermission
	t.Cleanup(func() { mutationPermission = original })
	for _, tc := range []struct {
		name    string
		allowed bool
		err     error
		status  int
	}{
		{"allowed", true, nil, 200}, {"read-only", false, nil, 403}, {"review unavailable", false, errors.New("API unavailable"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutationPermission = func(context.Context, string) (bool, error) { return tc.allowed, tc.err }
			called := false
			h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) { called = true })
			r := httptest.NewRequest("POST", "/api/auth-safety-"+strings.ReplaceAll(tc.name, " ", "-"), nil)
			r.Header.Set("X-Forwarded-Access-Token", "user-token")
			w := httptest.NewRecorder()
			h(w, r)
			if w.Code != tc.status || called != tc.allowed {
				t.Fatalf("status=%d called=%v", w.Code, called)
			}
			w = httptest.NewRecorder()
			HandleUserPermissions(w, r)
			if tc.err != nil {
				if w.Code != 503 {
					t.Fatal(w.Code)
				}
				return
			}
			want := `"canMutate":false`
			if tc.allowed {
				want = `"canMutate":true`
			}
			if !strings.Contains(w.Body.String(), want) {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestAcceptedMutationSurvivesBrowserDisconnectAndKeepsLock(t *testing.T) {
	setupDevMode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		if !acquireClusterMutationLock() {
			t.Error("lock unavailable")
			return
		}
		defer releaseClusterMutationLock()
		close(started)
		<-resume
		if err := c.Context().Err(); err != nil {
			t.Errorf("accepted operation canceled: %v", err)
		}
		if _, bounded := c.Context().Deadline(); !bounded {
			t.Error("operation has no deadline")
		}
	})
	r := httptest.NewRequest("POST", "/api/disconnect-safety", nil).WithContext(ctx)
	go func() { defer close(finished); h(httptest.NewRecorder(), r) }()
	select {
	case <-started:
	case <-finished:
		t.Fatal("handler was not called")
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	if acquireClusterMutationLock() {
		releaseClusterMutationLock()
		t.Error("lock released while work is active")
	}
	close(resume)
	<-finished
	if !acquireClusterMutationLock() {
		t.Fatal("lock retained after operation completed")
	}
	releaseClusterMutationLock()
}
