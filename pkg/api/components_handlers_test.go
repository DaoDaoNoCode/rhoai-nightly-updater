package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GET /api/components runs the DSC repair preview, whose removal checks
// list the objects of CRDs whose conversion webhook a component serves with
// the signed-in user's token (pkg/cluster
// TestRepairPreviewListsConversionObjectsWithTheUsersToken). In the full
// profile the handler's ServiceAccount client carries the signed-in
// cluster-admin's own client, after the route's one admin check; in the
// gitops profile it does not (that profile never acts with the user's
// token). A non-admin never reaches the handler in either.
func TestComponentsReadCarriesTheSignedInAdminsClient(t *testing.T) {
	for _, gitops := range []bool{false, true} {
		name := map[bool]string{false: "full profile", true: "gitops profile"}[gitops]
		t.Run(name, func(t *testing.T) {
			inCluster(t)
			allowMutations(t)
			prevFull, prevGitOps := getComponents, getComponentsGitOps
			t.Cleanup(func() { getComponents, getComponentsGitOps = prevFull, prevGitOps })
			var got *cluster.Client
			calls := 0
			getComponents = func(c *cluster.Client, _ bool) (*types.ComponentsResponse, error) {
				got, calls = c, calls+1
				return &types.ComponentsResponse{}, nil
			}
			getComponentsGitOps = func(c *cluster.Client) (*types.ComponentsResponse, error) {
				got, calls = c, calls+1
				return &types.ComponentsResponse{}, nil
			}
			var mux *http.ServeMux
			if gitops {
				mux = gitOpsMux(t)
			} else {
				mux = http.NewServeMux()
				Register(mux)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/components", nil)
			r.Header.Set("X-Forwarded-Access-Token", "sha256~admin-token")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK || got == nil {
				t.Fatalf("components: %d %s", w.Code, w.Body.String())
			}
			u := got.SignedInUser()
			switch {
			case got.ActsAsUser():
				t.Fatal("the read's own client acts as the user")
			case gitops && u != nil:
				// Nothing in the gitops profile acts with the user's
				// token (user:info user:check-access, which cannot list).
				t.Fatalf("gitops profile: the read carries the user's client %+v", u)
			case !gitops && (u == nil || !u.ActsAsUser() || u.Context() != got.Context()):
				t.Fatalf("signed-in user's client %+v", u)
			}

			// A user who is not a cluster-admin is refused before the
			// handler, so no client of theirs is made.
			mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden || calls != 1 {
				t.Fatalf("non-admin: %d %s, handler calls %d", w.Code, w.Body.String(), calls)
			}
		})
	}
}
