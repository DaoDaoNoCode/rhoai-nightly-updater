package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Set to the lowest priority (MaaS tab) is a MaaS change like the others:
// refused with cluster_busy while another operation holds the cluster lock,
// and otherwise run under the lock with the signed-in user's own token.
func TestMaaSSubscriptionPriorityRunsUnderTheLockAsTheUser(t *testing.T) {
	inCluster(t)
	allowMutations(t)
	fakeSession(t, fullToken(sessionNow.Add(-time.Hour)), nil)
	installFakeRecord(t)
	mux := http.NewServeMux()
	Register(mux)
	prev := runMaaS
	t.Cleanup(func() { runMaaS = prev })
	var ran []types.MaaSRequest
	var asUser, locked bool
	runMaaS = func(c *cluster.Client, req types.MaaSRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		ran = append(ran, req)
		asUser, locked = c.ActsAsUser(), clusterMutationInProgress.Load()
		return &types.OperationResponse{Success: true}, nil
	}
	body := `{"action":"subscription-priority"}`

	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/resources/maas/stream", "sha256~admin-token", body))
	releaseClusterMutationLock()
	if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "cluster_busy" || len(ran) != 0 {
		t.Fatalf("while another operation runs: %d %s, ran %v", w.Code, w.Body.String(), ran)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/resources/maas/stream", "sha256~admin-token", body))
	if len(ran) != 1 || ran[0].Action != "subscription-priority" || !asUser || !locked {
		t.Fatalf("ran %v as user %v under the lock %v (%d %s)", ran, asUser, locked, w.Code, w.Body.String())
	}
	if clusterMutationInProgress.Load() {
		t.Fatal("the lock is still held")
	}
}
