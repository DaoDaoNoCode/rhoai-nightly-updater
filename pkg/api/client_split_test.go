package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// An accepted mutation has two clients: the one the operation acts with,
// handed to the handler, and a separate bookkeeping client for the lease
// and the marker, which the operation keeps after the request.
func TestMutationKeepsItsLeaseAndMarkerOnTheBookkeepingClient(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	installFakeRecord(t)
	var opClient, bookkeeper *cluster.Client
	var tracked *cluster.Client
	h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		opClient = c
		if !lockCluster(w) {
			t.Fatal("lock refused")
		}
		defer releaseClusterMutationLock()
		bookkeeper = w.(*statusWriter).bookkeeper
		beginOperation(w, "target")
		inflight.mu.Lock()
		tracked = inflight.current.bookkeeper
		inflight.mu.Unlock()
		writeJSON(w, types.OperationResponse{Success: true}, "t")
	})
	w := httptest.NewRecorder()
	h(w, postJSON(t, "/api/refresh/stream", "user:alice", "{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if opClient == nil || bookkeeper == nil || opClient == bookkeeper {
		t.Fatalf("operation client %p, bookkeeping client %p: they must be two clients", opClient, bookkeeper)
	}
	if tracked != bookkeeper {
		t.Fatal("the running operation must keep the bookkeeping client, not the one it acts with")
	}
	if snap := inflight.snapshot(); snap != nil {
		t.Fatalf("operation still tracked: %+v", snap)
	}
}
