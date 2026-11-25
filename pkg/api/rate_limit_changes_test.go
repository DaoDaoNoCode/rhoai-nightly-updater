package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// closedAPIServer points the clients made from now on at a local port
// nothing listens on: a write is sent and gets no answer (connection
// refused), so as far as the updater can tell it may have been applied.
func closedAPIServer(t *testing.T) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
}

// sendWrite has the operation's client send one change.
func sendWrite(c *cluster.Client) {
	_, _ = c.Delete("/api/v1/namespaces/recipe-dev/configmaps/x")
}

const (
	cleanupPath           = "/api/resources/cleanup/stream"
	cleanupUnconfirmed    = `{"recipeProjects":["recipe-dev"],"recipeProjectUIDs":{"recipe-dev":"5c1d"}}`
	cleanupConfirmed      = `{"recipeProjects":["recipe-dev"],"recipeProjectUIDs":{"recipe-dev":"5c1d"},"confirmProjectContents":{"recipe-dev":["Secret db-creds"]}}`
	confirmRefusalMessage = "Project recipe-dev still holds what this tool did not create: Secret db-creds. Deleting the project deletes them too; confirm them to go ahead. Nothing was deleted."
)

// fakeCleanUpWithClient is fakeCleanUp with the operation's client.
func fakeCleanUpWithClient(t *testing.T, run func(c *cluster.Client, req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse) {
	t.Helper()
	orig := runCleanUp
	t.Cleanup(func() { runCleanUp = orig })
	runCleanUp = func(c *cluster.Client, req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return run(c, req, onStep), nil
	}
}

// lastStep is the stream's final event.
func lastStep(t *testing.T, w *httptest.ResponseRecorder) UpdateStep {
	t.Helper()
	steps := sseSteps(t, w.Body.String())
	if w.Code != http.StatusOK || len(steps) == 0 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	return steps[len(steps)-1]
}

// N9: Clean up everything refuses with confirm_project_contents (nothing
// deleted); the user ticks "Delete them with project recipe-dev" and clicks
// Delete again a few seconds later. The refusal did not use the slot, so the
// confirmed clean-up runs; that one changed the cluster, so a third click
// right after is limited.
func TestCleanupConfirmRefusalDoesNotUseTheSlot(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	fakeCleanUpWithClient(t, func(_ *cluster.Client, req types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse {
		if len(req.ConfirmProjectContents["recipe-dev"]) == 0 {
			onStep(cluster.UpdateStepEvent{Step: cluster.CleanupStepID("recipe-project", "recipe-dev"), Status: "failed", ErrorCode: cluster.ErrorCodeConfirmProjectContents, Message: confirmRefusalMessage, Detail: "Secret db-creds"})
			return &types.OperationResponse{Success: false, Message: confirmRefusalMessage, ErrorCode: cluster.ErrorCodeConfirmProjectContents}
		}
		return &types.OperationResponse{Success: true, Message: "Deleted project recipe-dev."}
	})
	mux := http.NewServeMux()
	Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, cleanupPath, "user:alice", cleanupUnconfirmed))
	if last := lastStep(t, w); last.Status != "failed" || last.ErrorCode != cluster.ErrorCodeConfirmProjectContents {
		t.Fatalf("first answer %+v", last)
	}
	testClock.Advance(5 * time.Second)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, cleanupPath, "user:alice", cleanupConfirmed))
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("the confirmed clean-up was rate limited after a refusal that changed nothing: %s", w.Body.String())
	}
	if last := lastStep(t, w); last.Status != "success" {
		t.Fatalf("confirmed answer %+v", last)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, cleanupPath, "user:alice", cleanupConfirmed))
	if w.Code != http.StatusTooManyRequests || decodeError(t, w)["errorCode"] != "user_rate_limited" {
		t.Fatalf("a repeat right after a clean-up that ran: %d %s", w.Code, w.Body.String())
	}
}

// A stream that changed part of the cluster before it failed keeps the
// slot, whatever its errorCode: the clean-up's own teardown can refuse with
// confirm_connection_workbenches after earlier parts were deleted.
func TestStreamThatChangedPartOfTheClusterKeepsTheSlot(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	closedAPIServer(t)
	fakeCleanUpWithClient(t, func(c *cluster.Client, _ types.CleanupRequest, onStep func(cluster.UpdateStepEvent)) *types.OperationResponse {
		sendWrite(c)
		onStep(cluster.UpdateStepEvent{Step: cluster.CleanupStepID("pipeline-server", "a"), Status: "failed", ErrorCode: "confirm_connection_workbenches"})
		return &types.OperationResponse{Success: false, Message: "Deleted before it stopped: the workbench in 'a'.", ErrorCode: "confirm_connection_workbenches"}
	})
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, cleanupPath, "user:alice", `{"pipelineServers":["a"]}`))
	if last := lastStep(t, w); last.Status != "failed" {
		t.Fatalf("final event %+v", last)
	}
	if !mutationLimiter.isRateLimited("alice:" + cleanupPath) {
		t.Fatal("a stream that changed the cluster before it failed gave the slot back")
	}
}

// The updater's own records (the activity log of a refusal, written with
// the bookkeeper) are not a change the user made: the slot is given back.
func TestRefusalThatOnlyWroteTheUpdatersRecordsGivesTheSlotBack(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	closedAPIServer(t)
	fakeCleanUpWithClient(t, func(c *cluster.Client, _ types.CleanupRequest, _ func(cluster.UpdateStepEvent)) *types.OperationResponse {
		_, _ = c.Bookkeeping().Delete("/api/v1/namespaces/rhoai-nightly-updater/configmaps/activity")
		_, _, _ = c.Get("/api/v1/namespaces/recipe-dev")
		return &types.OperationResponse{Success: false, Message: confirmRefusalMessage, ErrorCode: cluster.ErrorCodeConfirmProjectContents}
	})
	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, cleanupPath, "user:alice", cleanupUnconfirmed))
	if last := lastStep(t, w); last.Status != "failed" {
		t.Fatalf("final event %+v", last)
	}
	if mutationLimiter.isRateLimited("alice:" + cleanupPath) {
		t.Fatal("a refusal that wrote only the updater's records used the slot")
	}
}

// The same for an answer that is not a stream: a refusal that changed
// nothing gives the slot back (the repair can be asked again at once), one
// that failed after a write keeps it, and a repair that went through keeps
// it.
func TestNonStreamedChangeUsesTheSlotOnlyWhenItMayHaveChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		write   bool
		result  *types.OperationResponse
		status  int
		limited bool
	}{
		{"refused before any write", false, &types.OperationResponse{Success: false, Message: "Refresh and review again. Nothing was changed.", ErrorCode: "validation"}, http.StatusUnprocessableEntity, false},
		{"failed after a write", true, &types.OperationResponse{Success: false, Message: "Removed one field, then failed.", ErrorCode: "partial_failure"}, http.StatusUnprocessableEntity, true},
		{"went through", true, &types.OperationResponse{Success: true, Message: "Removed invalid DSC fields: x"}, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			closedAPIServer(t)
			prev := repairDSC
			t.Cleanup(func() { repairDSC = prev })
			repairDSC = func(c *cluster.Client, _, _, _, _ string, _, _ []string) (*types.OperationResponse, error) {
				if tc.write {
					sendWrite(c)
				}
				return tc.result, nil
			}
			mux := http.NewServeMux()
			Register(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, "/api/components/dsc/repair", "user:admin", repairBody))
			if w.Code != tc.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if limited := mutationLimiter.isRateLimited("admin:/api/components/dsc/repair"); limited != tc.limited {
				t.Fatalf("rate limited = %v, want %v", limited, tc.limited)
			}
			testClock.Advance(time.Second)
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, "/api/components/dsc/repair", "user:admin", repairBody))
			if got := w.Code == http.StatusTooManyRequests; got != tc.limited {
				t.Fatalf("repeat answered %d, want limited %v", w.Code, tc.limited)
			}
		})
	}
}

// 409 cluster_busy changes nothing, on a stream's route and on another: the
// user retries as soon as the other operation ends.
func TestClusterBusyDoesNotUseTheSlot(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{cleanupPath, cleanupUnconfirmed},
		{"/api/components/dsc/repair", repairBody},
	} {
		t.Run(tc.path, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
				return &types.OperationResponse{Success: true, Message: "Deleted project recipe-dev."}
			})
			prev := repairDSC
			t.Cleanup(func() { repairDSC = prev })
			repairDSC = func(*cluster.Client, string, string, string, string, []string, []string) (*types.OperationResponse, error) {
				return &types.OperationResponse{Success: true, Message: "Removed invalid DSC fields: x"}, nil
			}
			mux := http.NewServeMux()
			Register(mux)
			clusterMutationInProgress.Store(true)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, tc.path, "user:admin", tc.body))
			if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "cluster_busy" {
				t.Fatalf("busy: %d %s", w.Code, w.Body.String())
			}
			clusterMutationInProgress.Store(false)
			testClock.Advance(time.Second)
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, tc.path, "user:admin", tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("retry after the busy answer: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// A panic after the operation sent a write keeps the slot: how far it got is
// not known (operationPanicMessage). One before any write gives it back
// (TestStreamPanicEndsTheStreamAndFreesTheSlot).
func TestStreamPanicAfterAWriteKeepsTheSlot(t *testing.T) {
	closedAPIServer(t)
	_ = captureLog(t)
	setupDevMode(t)
	allowMutations(t)
	runUpdateStream = func(c *cluster.Client, _ string, _ cluster.OperationOptions, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		sendWrite(c)
		panic("bug after a write")
	}
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/update/stream", "user:admin", updateStreamBody))
	if last := lastStep(t, w); last.Status != "failed" || !strings.Contains(last.Message, "internal error") {
		t.Fatalf("final event %+v", last)
	}
	if !mutationLimiter.isRateLimited("admin:/api/update/stream") {
		t.Fatal("a panic after a write gave the slot back")
	}
}
