package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// Cancel of a running project action (the non-admin design, sections 5.2
// and 7; POST /api/operation/cancel {"id": ...}).
//
// A cancel is a request. It only marks the action (requestCancel); the
// action checks the mark at its own safe points, between its steps and at
// each poll of a wait (record.CancelRequested), and then ends through the
// failure path it already has at that point, on its live context. So no
// request in flight is ever cut off, a long single step (the playground's
// install call) runs to its end first, and an action that reaches its end
// before its next safe point succeeds as usual. Its context still ends
// only by its deadline or the loss of its lock, as for every operation.
//
// Whoever started the action (the identity requestUser verified with
// their own token, never the X-Forwarded-User header) or a cluster-admin
// may cancel it. Anyone else gets the answer of an action that does not
// exist, so the answer tells nothing about other people's actions; the
// role is checked before the action is looked up, so a failed check is
// the same answer either way. Only the actions that declare safe points
// can be cancelled (cancellableActions: each with its tests). The cancel
// takes no lock and changes nothing in the cluster. For an action of this
// updater pod it only marks the action (its one call is the mutation
// gate's access review with the caller's own token). For an action another
// updater pod runs (one that still drains while it shuts down), it checks
// that pod's lock entry in the operation record (its starter, live, can be
// cancelled) and records the request under a key of its own there
// (cluster.RequestOperationCancel, with the ServiceAccount, as every
// record write); the holder reads it at its heartbeat (pickUpCancels).

// cancellableActions are the project actions (cluster.ProjectActionID)
// that check a cancel request at safe points, with what a cancel leaves
// (the action's own failure path at those points), for the confirmation.
// Others cannot be cancelled: their steps have no boundary where stopping
// leaves less than finishing (a delete whose record must still be
// cleared, a start or stop of one object).
var cancellableActions = map[string]string{
	"workbench.setup": "It stops before its next step. Before the storage is created nothing stays; after, the storage stays, " +
		"recorded as this tool's, and the next setup uses it. No workbench is created.",
	"playground.setup": "It stops before its next step. Before the dashboard is asked to set it up nothing stays; after, " +
		"the playground stays, recorded as this tool's, and you can delete it on the Playground tab.",
	"mlflow-samples.add":    "It stops before its next sample. The samples added by then stay; remove them on the MLflow tab.",
	"mlflow-samples.remove": "It stops before its next sample. The samples removed by then stay removed.",
}

// noSuchActionMessage answers a cancel of an action that does not run in
// this updater pod, or that the user may not cancel: the same words.
const noSuchActionMessage = "No running action with this ID that you can cancel (it may have finished). Nothing was changed."

// cancelAcceptedMessage is the answer of an accepted cancel: a request.
const cancelAcceptedMessage = "Cancel requested: the action stops before its next step. If it finishes first, it is recorded as done."

// requestCancel marks the project action id of this process as cancelled
// by user (admin: a cluster-admin). It returns false when there is no such
// action that user may cancel: not running here, not a cancellable project
// action, or another person's for a user who is not a cluster-admin. A
// second request for the same action changes nothing and returns true.
func (t *operationTracker) requestCancel(id, user string, admin bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.ops[id]
	if op == nil || op.ProjectUID == "" || !op.confirmed || op.cancelNote == "" || (!admin && (user == "" || op.User != user)) {
		return false
	}
	if op.cancelled == nil {
		op.cancelled = &cluster.ActionCancelledError{ByAdmin: user != op.User}
		// Who asked, for the pod log (cluster-admins and the updater's
		// namespace admins read it); the starter's activity entry never
		// names a cluster-admin.
		slog.Info("cancel requested for a project action", "operation", id, "type", op.Type, "by", user, "starter", user == op.User)
	}
	return true
}

// cancelRequest is the cancel requested for the operation id, nil while
// none was: what a cancellable action checks at its safe points.
func (t *operationTracker) cancelRequest(id string) *cluster.ActionCancelledError {
	t.mu.Lock()
	defer t.mu.Unlock()
	if op := t.ops[id]; op != nil {
		return op.cancelled
	}
	return nil
}

// cancelRequestBody is the body of POST /api/operation/cancel.
type cancelRequestBody struct {
	ID string `json:"id"`
}

// HandleOperationCancel asks a running project action to stop at its next
// safe point (see above): 202 with the request recorded.
func HandleOperationCancel(w http.ResponseWriter, r *http.Request) {
	RecordRequest()
	username, r, ok := requestUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	body, err := io.ReadAll(r.Body)
	var req cancelRequestBody
	if err != nil || json.Unmarshal(body, &req) != nil || req.ID == "" {
		writeError(w, "invalid request body: an action id is required", http.StatusBadRequest, "validation")
		return
	}
	admin, err := mutationPermission(r.Context(), extractUserToken(r))
	if err != nil {
		writeAuthError(w, err, "Cannot verify your permissions. Nothing was changed.")
		return
	}
	if !inflight.requestCancel(req.ID, username, admin) {
		err := requestRemoteCancel(r.Context(), req.ID, username, admin)
		switch {
		case errors.Is(err, cluster.ErrNoSuchAction):
			writeError(w, noSuchActionMessage, http.StatusNotFound, "not_found")
			return
		case err != nil:
			slog.Warn("could not record a cancel request for another updater pod", "operation", req.ID, "problem", cluster.FactReadProblem(err))
			writeError(w, "The cancel request could not be recorded (the updater's operation record could not be read or written). Nothing was changed; try again.", http.StatusServiceUnavailable, "lock_unavailable")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]any{"cancelRequested": true, "message": cancelAcceptedMessage}); err != nil {
		slog.Error("response encode error", "label", "operation-cancel", "error", err)
	}
}

// remoteCancelTimeout bounds recording a cancel request for another pod.
const remoteCancelTimeout = 5 * time.Second

// requestRemoteCancel records a cancel request for the project action id
// that another updater pod runs, for user (admin: a cluster-admin), when
// the operation record holds its live lock entry, it can be cancelled, and
// user started it or is a cluster-admin; otherwise
// cluster.ErrNoSuchAction. The record is read, and written, with the
// ServiceAccount.
func requestRemoteCancel(ctx context.Context, id, user string, admin bool) error {
	token := getClusterToken()
	if token == "" {
		return errors.New("cluster token not available")
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCancelTimeout)
	defer cancel()
	c := cluster.NewClientWithContext(ctx, token)
	c.SetUsername(user)
	return requestOperationCancel(c, id, func(rec *cluster.OperationRecord) (*cluster.CancelRequest, error) {
		for _, l := range rec.ProjectLocks {
			if l.ID != id || !leaseHeldElsewhere(l) || l.CancelNote == "" || (!admin && (user == "" || l.User != user)) {
				continue
			}
			slog.Info("cancel requested for a project action of another updater pod", "operation", id, "type", l.Type, "pod", l.Pod, "by", user, "starter", user == l.User)
			return &cluster.CancelRequest{ByAdmin: user != l.User, At: clk.Now().UTC().Format(time.RFC3339)}, nil
		}
		return nil, cluster.ErrNoSuchAction
	})
}

// requestOperationCancel records a cancel request; tests replace it.
var requestOperationCancel = cluster.RequestOperationCancel

// markCancelled records the cancel request another pod took for the
// operation id of this process (pickUpCancels), as requestCancel would.
func (t *operationTracker) markCancelled(id string, byAdmin bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.ops[id]
	if op == nil || op.ProjectUID == "" || op.cancelNote == "" || op.cancelled != nil {
		return
	}
	op.cancelled = &cluster.ActionCancelledError{ByAdmin: byAdmin}
	slog.Info("cancel requested through another updater pod", "operation", id, "type", op.Type)
}

// awaitingCancel is whether the project action id of this process can be
// cancelled and was not yet: the heartbeat then reads the record for a
// cancel request another pod took (pickUpCancels).
func (t *operationTracker) awaitingCancel(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.ops[id]
	return op != nil && op.ProjectUID != "" && op.cancelNote != "" && op.cancelled == nil
}
