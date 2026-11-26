package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The cooperative cancel of project actions (the non-admin design, B4).

// cancellableAction is localAction for a cancellable action (a workbench
// setup's note).
func cancellableAction(t *testing.T, uid, user string) string {
	t.Helper()
	id := localAction(t, uid, user)
	inflight.update(id, func(o *Operation) { o.cancelNote = cancellableActions["workbench.setup"] })
	return id
}

func postCancel(t *testing.T, token, id string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"id": id})
	mux.ServeHTTP(w, postJSON(t, "/api/operation/cancel", token, string(body)))
	return w
}

// Cancel: the action's starter or a cluster-admin. Anyone else gets the
// answer of an action that does not exist; a failed role check is the same
// answer whether it exists or not. An action that cannot be cancelled, and
// a cluster operation, answer as none. The cancel only records the request
// (no lock, no record read, the context untouched).
func TestCancelOnlyStarterOrAdmin(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		t.Error("the cancel read the operation record")
		return &cluster.OperationRecord{}, nil
	}
	id := cancellableAction(t, "a", "alice")

	missing := postCancel(t, "user:bob", "no-such-action")
	if missing.Code != http.StatusNotFound || decodeError(t, missing)["errorCode"] != "not_found" {
		t.Fatalf("an unknown ID: %d %s", missing.Code, missing.Body.String())
	}
	if others := postCancel(t, "user:bob", id); others.Code != missing.Code || others.Body.String() != missing.Body.String() {
		t.Fatalf("another person's action answers differently from none: %d %s", others.Code, others.Body.String())
	}
	mutationPermission = func(context.Context, string) (bool, error) { return false, errors.New("api down") }
	down1, down2 := postCancel(t, "user:bob", id), postCancel(t, "user:bob", "no-such-action")
	if down1.Code != http.StatusServiceUnavailable || down1.Body.String() != down2.Body.String() {
		t.Fatalf("role check failed: %d %s / %s", down1.Code, down1.Body.String(), down2.Body.String())
	}
	setAdmin(t)
	if inflight.cancelRequest(id) != nil {
		t.Fatal("cancelled by a refused request")
	}

	// Not cancellable: a project action without safe points, a cluster
	// operation; not even by its starter or an admin.
	plain := localAction(t, "p", "erin")
	if w := postCancel(t, "user:erin", plain); w.Code != http.StatusNotFound {
		t.Fatalf("an action without safe points: %d %s", w.Code, w.Body.String())
	}
	clusterOp := inflight.startBackground("update", "Update to nightly", "alice", nil)
	confirmLock(clusterOp.ID)
	if w := postCancel(t, "user:admin", clusterOp.ID); w.Code != http.StatusNotFound {
		t.Fatalf("a cluster operation: %d %s", w.Code, w.Body.String())
	}
	inflight.finishID(clusterOp.ID)

	// The starter: accepted, as a request.
	w := postCancel(t, "user:alice", id)
	var accepted struct {
		CancelRequested bool   `json:"cancelRequested"`
		Message         string `json:"message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &accepted)
	if w.Code != http.StatusAccepted || !accepted.CancelRequested || accepted.Message != cancelAcceptedMessage {
		t.Fatalf("the starter's cancel: %d %s", w.Code, w.Body.String())
	}
	if c := inflight.cancelRequest(id); c == nil || c.ByAdmin || c.Reason() != "Cancelled by you" {
		t.Fatalf("request %+v", c)
	}
	if w := postCancel(t, "user:admin", id); w.Code != http.StatusAccepted || inflight.cancelRequest(id).ByAdmin {
		t.Fatalf("a second request changed the first: %d", w.Code)
	}
	if op := inflight.get(id); op == nil || !op.Cancelling || op.Cancellable {
		t.Fatalf("after the request: %+v", op)
	}
	if locks.clusterHeld() {
		t.Fatal("the cancel took the cluster lock")
	}

	// A cluster-admin: "a cluster-admin", never their name.
	id2 := cancellableAction(t, "d", "dave")
	if w := postCancel(t, "user:admin", id2); w.Code != http.StatusAccepted {
		t.Fatalf("an admin's cancel: %d %s", w.Code, w.Body.String())
	}
	if c := inflight.cancelRequest(id2); c == nil || !c.ByAdmin || c.Reason() != "Cancelled by a cluster-admin" {
		t.Fatalf("request %+v", c)
	}
}

// Through the gate: a cancellable action sees the request through its
// context's check (cluster.CancelRequested), its context stays live, and
// the action decides when to stop. A lease loss after the request still
// ends the context with cluster.ErrOperationLockLost, as without one. A
// user's view offers Cancel with what it leaves, then shows it stopping;
// the busy refusal of their next action says "or cancel it" only while it
// can be cancelled.
func TestCancelIsCheckedThroughTheGate(t *testing.T) {
	newProjectGateEnv(t, roleEditor)
	started, proceed := make(chan struct{}), make(chan struct{})
	type seen struct {
		cancel  *cluster.ActionCancelledError
		ctxErr  error
		lost    error
		lastCtx error
	}
	result := make(chan seen, 1)
	var lose func()
	runWorkbench = func(c *cluster.Client, _ types.WorkbenchRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		close(started)
		<-proceed
		s := seen{cancel: cluster.CancelRequested(c), ctxErr: c.Context().Err()}
		lose()
		<-c.Context().Done()
		s.lost = context.Cause(c.Context())
		result <- s
		return &types.OperationResponse{Success: false, Message: "stopped", ErrorCode: "lock_lost"}, nil
	}
	lose = func() {
		leases.mu.Lock()
		var h *heldLease
		for _, held := range leases.held {
			h = held
		}
		leases.mu.Unlock()
		leases.lose(h, "test: the lock was lost")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	}()
	<-started
	stubUserProjects(t, map[string]string{"team-a": "uid-team-a"}, nil)
	sum, _ := getUserOperation(t, "user:alice")
	if len(sum.Operations) != 1 || !sum.Operations[0].Own || !sum.Operations[0].Cancellable || sum.Operations[0].CancelNote != cancellableActions["workbench.setup"] {
		t.Fatalf("the running action: %+v", sum.Operations)
	}
	id := sum.Operations[0].ID
	busy := serveProjectAction(t, "/api/resources/playground/stream", `{"action":"teardown","project":"team-a"}`)
	if msg, _ := decodeError(t, busy)["error"].(string); busy.Code != http.StatusConflict || !strings.HasSuffix(msg, "Wait for it to finish or cancel it.") {
		t.Fatalf("busy: %d %s", busy.Code, busy.Body.String())
	}
	if w := postCancel(t, "user:alice", id); w.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	sum, _ = getUserOperation(t, "user:alice")
	if len(sum.Operations) != 1 || sum.Operations[0].Cancellable || !sum.Operations[0].Cancelling {
		t.Fatalf("after the request: %+v", sum.Operations)
	}
	close(proceed)
	s := <-result
	if s.cancel == nil || s.cancel.ByAdmin || s.ctxErr != nil {
		t.Fatalf("the action saw request %+v, context %v", s.cancel, s.ctxErr)
	}
	if !errors.Is(s.lost, cluster.ErrOperationLockLost) {
		t.Fatalf("a lease loss after the request ended the context with %v", s.lost)
	}
	<-done
	if locks.projectOp("uid-team-a") != "" || len(inflight.list()) != 0 {
		t.Fatal("the action still holds its lock")
	}
}

// The busy refusal of a user's own running action: "or cancel it" only
// when it can be cancelled.
func TestBusyRefusalOffersCancelOnlyWhenCancellable(t *testing.T) {
	setupDevMode(t)
	for _, tc := range []struct {
		op   *Operation
		want string
	}{
		{&Operation{Label: "Set up the workbench", User: "alice", Project: "team-a", ProjectUID: "a", Cancellable: true}, "You already have an action running (Set up the workbench in project team-a). Wait for it to finish or cancel it."},
		{&Operation{Label: "Start the workbench", User: "alice", Project: "team-a", ProjectUID: "a"}, "You already have an action running (Start the workbench in project team-a). Wait for it to finish."},
	} {
		if got := busyMessageForUser(tc.op, conflictUser, "alice"); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
