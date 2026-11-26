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
// (no lock, nothing written for an action of this pod, the context
// untouched).
func TestCancelOnlyStarterOrAdmin(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	writes := 0
	requestOperationCancel = func(c *cluster.Client, _ string, check func(*cluster.OperationRecord) (*cluster.CancelRequest, error)) error {
		rec, _ := readOperationState(c)
		if _, err := check(rec); err != nil {
			return err
		}
		writes++
		return nil
	}
	defer func() {
		if writes != 0 {
			t.Errorf("%d cancel requests written for other pods", writes)
		}
	}()
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
		cancel *cluster.ActionCancelledError
		ctxErr error
		lost   error
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

// A cancel for a project action another updater pod runs: checked against
// its live lock entry in the record (its starter, or a cluster-admin; it
// can be cancelled) and recorded for that pod; anyone else, and an action
// that cannot be cancelled, get the same 404 and nothing is written. An
// admin then sees it stopping.
func TestCrossPodCancelIsRecordedForTheHolder(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	now := clk.Now()
	cancellable := otherPodProjectLease("a", "alice", now)
	cancellable.CancelNote = cancellableActions["workbench.setup"]
	plain := otherPodProjectLease("p", "alice", now)
	rec := &cluster.OperationRecord{ProjectLocks: map[string]*types.OperationLease{"a": cancellable, "p": plain}}
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) { return rec, nil }
	var written []*cluster.CancelRequest
	requestOperationCancel = func(c *cluster.Client, id string, check func(*cluster.OperationRecord) (*cluster.CancelRequest, error)) error {
		r, err := check(rec)
		if err != nil {
			return err
		}
		written = append(written, r)
		return nil
	}
	missing := postCancel(t, "user:bob", "no-such-action")
	for _, tc := range []struct{ token, id string }{{"user:bob", "remote-a"}, {"user:alice", "remote-p"}} {
		if w := postCancel(t, tc.token, tc.id); w.Code != http.StatusNotFound || w.Body.String() != missing.Body.String() {
			t.Fatalf("%s cancels %s: %d %s", tc.token, tc.id, w.Code, w.Body.String())
		}
	}
	if len(written) != 0 {
		t.Fatalf("refused requests were written: %+v", written)
	}
	if w := postCancel(t, "user:alice", "remote-a"); w.Code != http.StatusAccepted || len(written) != 1 || written[0].ByAdmin {
		t.Fatalf("the starter: %d %s %+v", w.Code, w.Body.String(), written)
	}
	if w := postCancel(t, "user:admin", "remote-a"); w.Code != http.StatusAccepted || len(written) != 2 || !written[1].ByAdmin {
		t.Fatalf("a cluster-admin: %d %+v", w.Code, written)
	}

	// With the request recorded, the action shows as stopping.
	rec.CancelRequests = map[string]*cluster.CancelRequest{"remote-a": {ByAdmin: true}}
	for _, op := range getAdminOperation(t).Operations {
		if op.ID == "remote-a" && (op.Cancellable || !op.Cancelling) {
			t.Fatalf("after the request: %+v", op)
		}
		if op.ID == "remote-p" && (op.Cancellable || op.Cancelling) {
			t.Fatalf("an action without safe points: %+v", op)
		}
	}
}

// The holder: its heartbeat reads the request another pod recorded and
// marks the action, which sees it at its next safe point. Its lock is not
// lost and its context stays live.
func TestCrossPodCancelIsPickedUpWithoutLeaseLoss(t *testing.T) {
	newProjectGateEnv(t, roleEditor)
	started, proceed := make(chan struct{}), make(chan struct{})
	seen := make(chan *cluster.ActionCancelledError, 1)
	runWorkbench = func(c *cluster.Client, _ types.WorkbenchRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		close(started)
		<-proceed
		if c.Context().Err() != nil {
			t.Error("the action's context ended")
		}
		seen <- cluster.CancelRequested(c)
		return &types.OperationResponse{Success: false, Message: "cancelled", ErrorCode: "cancelled"}, nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	}()
	<-started
	var id string
	for _, op := range inflight.list() {
		id = op.ID
	}
	reads := 0
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		reads++
		return &cluster.OperationRecord{CancelRequests: map[string]*cluster.CancelRequest{id: {ByAdmin: true}}}, nil
	}
	leases.renewOnce(context.Background())
	if reads != 1 {
		t.Fatalf("the heartbeat read the record %d times", reads)
	}
	op := inflight.get(id)
	if op == nil || !op.Cancelling || op.lockLost {
		t.Fatalf("after the heartbeat: %+v", op)
	}
	// Once marked, the heartbeat reads no more.
	leases.renewOnce(context.Background())
	if reads != 1 {
		t.Fatalf("read again after the pickup: %d", reads)
	}
	close(proceed)
	if c := <-seen; c == nil || !c.ByAdmin {
		t.Fatalf("the action saw %+v", c)
	}
	<-done
}
