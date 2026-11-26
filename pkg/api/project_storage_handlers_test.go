package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The project storage's status is read with the signed-in user's own
// token (the storage's Secret holds the project's root key, which the
// ServiceAccount may not read), with the ServiceAccount's only for the
// records and the facts; a user's project is checked first.
func TestProjectStorageStatusReadsWithTheUsersToken(t *testing.T) {
	for _, role := range []string{roleProjectAdmin, roleClusterAdmin} {
		t.Run(role, func(t *testing.T) {
			newProjectGateEnv(t, role)
			var got *cluster.Client
			orig := getProjectStorageStatus
			t.Cleanup(func() { getProjectStorageStatus = orig })
			getProjectStorageStatus = func(c *cluster.Client, project string) *types.ProjectStorageStatus {
				got = c
				return &types.ProjectStorageStatus{Project: project}
			}
			mux := http.NewServeMux()
			Register(mux)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/project-s3?project=team-a", nil)
			r.Header.Set("X-Forwarded-Access-Token", "user:alice")
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK || got == nil {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if !got.ActsAsUser() || got.TestOptions().Token != "user:alice" {
				t.Fatalf("read as user %v with token %q", got.ActsAsUser(), got.TestOptions().Token)
			}
			if fc := got.Facts(); fc.ActsAsUser() || !fc.ReadOnly() || got.Bookkeeping().ActsAsUser() {
				t.Fatal("the facts or records client is the user's")
			}
		})
	}
	// Not one of the user's projects: nothing is read.
	newProjectGateEnv(t, roleNothing)
	called := false
	orig := getProjectStorageStatus
	t.Cleanup(func() { getProjectStorageStatus = orig })
	getProjectStorageStatus = func(*cluster.Client, string) *types.ProjectStorageStatus { called = true; return nil }
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/project-s3?project=team-a", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:alice")
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("%d %s, read %v", w.Code, w.Body.String(), called)
	}
}
