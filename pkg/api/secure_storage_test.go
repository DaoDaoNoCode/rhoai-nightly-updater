package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeSecure replaces Secure S3 storage for one test and counts its runs.
func fakeSecure(t *testing.T, run func(onStep func(cluster.UpdateStepEvent)) *types.OperationResponse) *int {
	t.Helper()
	orig := runSecureStorage
	t.Cleanup(func() { runSecureStorage = orig })
	calls := 0
	runSecureStorage = func(_ *cluster.Client, onStep func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		calls++
		return run(onStep), nil
	}
	return &calls
}

func TestSecureStorageStream_StreamsEachStepAsOneOperation(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeSecure(t, func(onStep func(cluster.UpdateStepEvent)) *types.OperationResponse {
		onStep(cluster.UpdateStepEvent{Step: "check", Status: "success", Message: "OK: 1 pipeline server to give a key of its own"})
		onStep(cluster.UpdateStepEvent{Step: "rollout", Status: "success", Message: "OK: new admin key"})
		onStep(cluster.UpdateStepEvent{Step: "server:dev/dspa", Status: "failed", Message: "did not get its own key", ErrorCode: "s3_keys"})
		return &types.OperationResponse{Success: false, Message: "Still on the old key: dev/dspa; run Secure S3 storage again.", ErrorCode: "s3_keys"}
	})
	w := httptest.NewRecorder()
	HandleS3StorageSecureStream(w, postJSON(t, "/api/resources/s3/secure/stream", "user:alice", `{}`))
	body := w.Body.String()
	for _, want := range []string{`"step":"check"`, `"step":"rollout"`, `"step":"server:dev/dspa","status":"failed"`, `"step":"operation_complete","status":"failed"`, "run Secure S3 storage again"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
	if *calls != 1 {
		t.Fatalf("runs = %d", *calls)
	}
	if got := operationTypes["/api/resources/s3/secure/stream"]; got != [2]string{"secure-s3", "Secure S3 storage"} {
		t.Errorf("operation label = %+v", got)
	}
}

func TestSecureStorageStream_TakesTheClusterLock(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeSecure(t, func(func(cluster.UpdateStepEvent)) *types.OperationResponse {
		return &types.OperationResponse{Success: true}
	})
	w := httptest.NewRecorder()
	HandleS3StorageSecureStream(w, postJSON(t, "/api/resources/s3/secure/stream", "user:carl", `{`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("not JSON: %d %s", w.Code, w.Body.String())
	}
	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	defer releaseClusterMutationLock()
	w = httptest.NewRecorder()
	HandleS3StorageSecureStream(w, postJSON(t, "/api/resources/s3/secure/stream", "user:bob", `{}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cluster_busy") {
		t.Fatalf("while another operation runs: %d %s", w.Code, w.Body.String())
	}
	if *calls != 0 {
		t.Fatal("it ran without the lock or with a bad body")
	}
}
