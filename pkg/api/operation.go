package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// clusterMutationInProgress is a non-blocking mutex that prevents concurrent
// cluster-level mutation operations. When true, a cluster mutation is in
// progress and new mutation requests receive HTTP 409 Conflict instead of
// proceeding. The lock is per process and is the first gate; lockCluster
// then takes the cross-pod lease (operation_lease.go), because a replaced
// pod can still drain an operation while its replacement serves requests.
var clusterMutationInProgress atomic.Bool

// acquireClusterMutationLock tries to acquire the cluster mutation lock.
// Returns true if the lock was acquired (caller must defer releaseClusterMutationLock).
// Returns false if another operation is already in progress; the caller should
// return HTTP 409.
func acquireClusterMutationLock() bool {
	return clusterMutationInProgress.CompareAndSwap(false, true)
}

// releaseClusterMutationLock releases the cluster mutation lock and forgets
// the in-flight operation. An operation that began (see beginOperation)
// becomes the last completed operation, and its marker is cleared in the
// same write that releases its lease; the lease of an operation that never
// began is released on its own.
func releaseClusterMutationLock() {
	owner := leases.stop()
	done, client := inflight.finish()
	switch {
	case done != nil:
		markers.complete(client, done, owner)
	case owner != nil && client != nil:
		leases.release(client, *owner)
	}
	clusterMutationInProgress.Store(false)
}

// lockCluster acquires the cluster mutation lock, then the cross-pod lease,
// or answers 409 Conflict with errorCode "cluster_busy" and the running
// operation (also one that runs in another updater pod). When it returns
// true the caller must defer releaseClusterMutationLock.
func lockCluster(w http.ResponseWriter) bool {
	if !acquireClusterMutationLock() {
		writeClusterBusy(w, inflight.snapshot(), "")
		return false
	}
	sw, ok := w.(*statusWriter)
	if !ok {
		return true
	}
	op := inflight.start(sw)
	sw.opID = op.ID
	if sw.client == nil {
		return true
	}
	holder, err := leases.acquire(sw.client, op, sw.cancelOp)
	if holder == nil && err == nil {
		return true
	}
	// Refused: forget the operation (it never began) and free the local lock.
	inflight.finish()
	clusterMutationInProgress.Store(false)
	if holder != nil {
		slog.Info("operation refused: another updater pod runs one", "holder", holder.ID, "holderPod", holder.Pod, "type", holder.Type)
		writeClusterBusy(w, remoteOperation(holder), "")
		return false
	}
	slog.Warn("operation refused: the cross-pod operation lock is contended", "error", err)
	writeClusterBusy(w, nil, "Another updater pod is writing the operation lock right now. Nothing was changed; try again in a few seconds.")
	return false
}

// writeClusterBusy answers 409 cluster_busy, naming op when it is known.
func writeClusterBusy(w http.ResponseWriter, op *Operation, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	if message == "" {
		message = "Another cluster operation is in progress. Please wait."
	}
	body := map[string]interface{}{"error": message, "errorCode": "cluster_busy"}
	if op != nil {
		body["operation"] = op
		where := ""
		if op.Remote && op.Pod != "" {
			where = " on updater pod " + op.Pod
		}
		body["error"] = op.User + " is running \"" + op.Label + "\"" + where + ". Wait for it to finish."
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("response encode error", "label", "cluster-busy", "error", err)
	}
}

// beginOperation marks a validated request as a running cluster operation:
// it records what the operation acts on (for example the catalog image) so
// other users see it, and writes the operation marker, which reports the
// operation as interrupted if this process dies before it ends. Handlers
// call it after validating the request and before changing the cluster, so
// a rejected request writes nothing. Only begun operations are reported as
// lastCompleted.
func beginOperation(w http.ResponseWriter, target string) {
	sw, ok := w.(*statusWriter)
	if !ok {
		return
	}
	var marker *types.OperationMarker
	var client *cluster.Client
	inflight.update(sw.opID, func(op *Operation) {
		op.Target = truncateText(target, maxOperationTargetBytes)
		if !op.begun {
			op.begun = true
			marker, client = operationMarker(op), op.client
		}
	})
	if marker != nil && client != nil {
		markers.start(client, marker)
	}
}

// setOperationTarget records what an accepted request acts on without
// making it an operation that can be interrupted: dry runs change nothing,
// so they write no marker and are not reported as lastCompleted.
func setOperationTarget(w http.ResponseWriter, target string) {
	if sw, ok := w.(*statusWriter); ok {
		inflight.update(sw.opID, func(op *Operation) { op.Target = truncateText(target, maxOperationTargetBytes) })
	}
}

// Operation describes the cluster operation that holds the lock.
type Operation struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Label      string    `json:"label"`
	User       string    `json:"user"`
	Target     string    `json:"target,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Step       string    `json:"step,omitempty"`
	StepStatus string    `json:"stepStatus,omitempty"`
	Message    string    `json:"message,omitempty"`
	Pod        string    `json:"pod,omitempty"`
	// Remote: the operation runs in another updater process (Pod), known
	// from its lease; Step, StepStatus and Message are its latest progress
	// as of its last heartbeat (UpdatedAt).
	Remote bool `json:"remote,omitempty"`

	client *cluster.Client
	sw     *statusWriter // the response, for the outcome of non-streaming operations
	begun  bool          // set by beginOperation
	// lockLost: the lease heartbeat stopped the operation because its lease
	// was lost; its outcome is cluster.LockLostMessage.
	lockLost bool
}

// operationTypes maps mutation endpoints to a stable type and a label.
var operationTypes = map[string][2]string{
	"/api/update":                             {"update-dry-run", "Dry run of a nightly update"},
	"/api/update/stream":                      {"update", "Update to nightly"},
	"/api/rollback/stream":                    {"reinstall", "Reinstall operator"},
	"/api/refresh/stream":                     {"refresh", "Refresh operator"},
	"/api/components/dsc/repair":              {"repair-dsc", "Repair DataScienceCluster"},
	"/api/assist-rollout":                     {"assist-rollout", "Assist stuck rollouts"},
	"/api/setup/pull-secret":                  {"create-pull-secret", "Configure pull secret"},
	"/api/setup/dsc":                          {"create-dsc", "Create DataScienceCluster"},
	"/api/dashboard/deploy-pr":                {"deploy-dashboard-pr", "Deploy dashboard PR"},
	"/api/dashboard/deploy-main":              {"deploy-dashboard-main", "Deploy dashboard main"},
	"/api/dashboard/revert":                   {"revert-dashboard", "Revert dashboard"},
	"/api/resources/minio/setup":              {"setup-minio", "Set up MinIO"},
	"/api/resources/minio/teardown":           {"teardown-minio", "Tear down MinIO"},
	"/api/resources/pipeline-server/setup":    {"setup-pipeline-server", "Set up pipeline server"},
	"/api/resources/pipeline-server/teardown": {"teardown-pipeline-server", "Tear down pipeline server"},
	"/api/resources/mlflow/setup":             {"setup-mlflow", "Set up MLflow"},
	"/api/resources/mlflow/teardown":          {"teardown-mlflow", "Tear down MLflow"},
	"/api/resources/mlflow/deploy-pr":         {"deploy-mlflow-pr", "Deploy MLflow PR"},
	"/api/resources/mlflow/revert":            {"revert-mlflow", "Revert MLflow"},
	"/api/diagnostics/fix":                    {"diagnostics-fix", "Apply diagnostics fix"},
}

// Size bounds of what is kept and persisted about an operation.
const (
	maxOperationTargetBytes  = 512
	maxOperationMessageBytes = 1024
)

type operationTracker struct {
	mu      sync.Mutex
	current *Operation
	last    *types.CompletedOperation // the most recent begun operation that finished
}

var inflight = &operationTracker{}

func newOperationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// bootID identifies this server process in the operation marker. A marker
// with another boot ID was left by a process that died mid-operation: a new
// pod, or this pod's own container after a restart (OOM kill, failed
// liveness probe, crash), which keeps the pod name in HOSTNAME.
var bootID = newOperationID()

func (t *operationTracker) start(sw *statusWriter) *Operation {
	kind := operationTypes[sw.path]
	if kind[0] == "" {
		kind = [2]string{"operation", sw.path}
	}
	now := time.Now().UTC()
	op := &Operation{
		ID: newOperationID(), Type: kind[0], Label: kind[1], User: sw.user,
		StartedAt: now, UpdatedAt: now, Pod: os.Getenv("HOSTNAME"), client: sw.client, sw: sw,
	}
	t.mu.Lock()
	t.current = op
	t.mu.Unlock()
	return op
}

func (t *operationTracker) update(id string, fn func(*Operation)) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current != nil && t.current.ID == id {
		fn(t.current)
		t.current.UpdatedAt = time.Now().UTC()
	}
}

// recordStep captures a progress event of the operation with this ID.
func (t *operationTracker) recordStep(id string, step UpdateStep) {
	t.update(id, func(op *Operation) {
		op.Step, op.StepStatus, op.Message = step.Step, step.Status, step.Message
	})
}

// finish forgets the current operation and returns its client. When it had
// begun, it becomes the last completed operation, which is returned too.
// It runs on the handler's goroutine after the response is written.
func (t *operationTracker) finish() (*types.CompletedOperation, *cluster.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.current
	t.current = nil
	if op == nil {
		return nil, nil
	}
	if !op.begun {
		return nil, op.client
	}
	success, message := operationOutcome(op)
	t.last = &types.CompletedOperation{
		ID: op.ID, Type: op.Type, Label: op.Label, User: op.User, Target: op.Target,
		StartedAt:  op.StartedAt.Format(time.RFC3339),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
		Success:    success,
		Message:    truncateText(message, maxOperationMessageBytes),
	}
	done := *t.last
	return &done, op.client
}

// lastCompleted returns a copy of the last completed operation, or nil.
func (t *operationTracker) lastCompleted() *types.CompletedOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		return nil
	}
	cp := *t.last
	return &cp
}

// rememberLast adopts a persisted last completed operation unless the one
// this process remembers finished later, and returns the newer one (nil
// when there is neither). The persisted one may be from before a restart
// or from another updater pod; on equal finish times it wins, since it is
// what every pod reads.
func (t *operationTracker) rememberLast(persisted *types.CompletedOperation) *types.CompletedOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if persisted != nil && (t.last == nil || !finishedBefore(persisted, t.last)) {
		cp := *persisted
		t.last = &cp
	}
	if t.last == nil {
		return nil
	}
	cp := *t.last
	return &cp
}

// finishedBefore reports whether a finished strictly before b. An
// unreadable finish time counts as the oldest.
func finishedBefore(a, b *types.CompletedOperation) bool {
	ta, errA := time.Parse(time.RFC3339, a.FinishedAt)
	tb, errB := time.Parse(time.RFC3339, b.FinishedAt)
	switch {
	case errA != nil:
		return errB == nil
	case errB != nil:
		return false
	}
	return ta.Before(tb)
}

// snapshot returns a copy of the current operation, or nil.
func (t *operationTracker) snapshot() *Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		return nil
	}
	cp := *t.current
	cp.client, cp.sw = nil, nil
	return &cp
}

// operationOutcome reports whether an operation succeeded, and its message:
// the final operation_complete event of a stream, otherwise the response
// status and the "success", "message" or "error" fields of its JSON body.
func operationOutcome(op *Operation) (bool, string) {
	if op.lockLost {
		return false, cluster.LockLostMessage
	}
	if op.Step == "operation_complete" {
		return op.StepStatus == "success", op.Message
	}
	sw := op.sw
	if sw == nil || sw.status == 0 || sw.streaming {
		return false, "The operation ended without reporting a result."
	}
	var body struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(sw.body, &body) // a truncated body leaves the fields empty
	success := sw.status < http.StatusBadRequest && (body.Success == nil || *body.Success)
	message := body.Message
	if message == "" {
		message = body.Error
	}
	if message == "" {
		if success {
			message = "Completed."
		} else {
			message = fmt.Sprintf("Failed (HTTP %d).", sw.status)
		}
	}
	return success, message
}

// truncateText shortens s to at most n bytes without splitting a character.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func operationMarker(op *Operation) *types.OperationMarker {
	return &types.OperationMarker{
		ID: op.ID, Type: op.Type, Label: op.Label, User: op.User, Target: op.Target,
		StartedAt: op.StartedAt.Format(time.RFC3339), Pod: op.Pod, BootID: bootID,
	}
}

// interruptedMarkerMaxAge bounds how long an interrupted operation is shown.
const interruptedMarkerMaxAge = 24 * time.Hour

// OperationStatus is the GET /api/operation response.
type OperationStatus struct {
	InProgress bool `json:"inProgress"`
	// Operation is the running operation: this process's own, or one that
	// another updater pod runs (Operation.Remote, from its lease).
	Operation *Operation `json:"operation"`
	// Interrupted is an operation that an updater process started and
	// never finished: its marker is still recorded and its process holds
	// no live lease (the pod or its container crashed or was killed, or a
	// version without leases left it). Its cluster changes may be
	// half-done.
	Interrupted *types.OperationMarker `json:"interrupted,omitempty"`
	// LastCompleted is the most recent finished operation, also one that
	// finished before this process started or in another updater pod.
	LastCompleted *types.CompletedOperation `json:"lastCompleted,omitempty"`
}

// HandleOperation reports the cluster operation in progress, the last
// completed one, and an interrupted one, if any. While this process runs
// an operation it answers from memory; otherwise it reads the operation
// record, which also tells about another pod's operation.
var HandleOperation = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	local := clusterMutationInProgress.Load()
	status := OperationStatus{InProgress: local, Operation: inflight.snapshot()}
	last := inflight.lastCompleted()
	if local && last != nil {
		status.LastCompleted = last
		writeJSON(w, status, "operation")
		return
	}
	rec, err := readOperationState(c)
	if err != nil || rec == nil {
		slog.Debug("could not read the operation record", "error", err)
		status.LastCompleted = last
		writeJSON(w, status, "operation")
		return
	}
	status.LastCompleted = inflight.rememberLast(rec.LastCompleted)
	if local {
		writeJSON(w, status, "operation")
		return
	}
	remote := leaseHeldElsewhere(rec.Lease)
	if remote {
		status.InProgress, status.Operation = true, remoteOperation(rec.Lease)
	}
	if marker := rec.Marker; marker != nil {
		switch {
		case marker.BootID == bootID:
			// This process's own marker while no operation runs: the
			// operation finished but clearing its marker failed. Not an
			// interruption; retry the clear.
			markers.retryPending()
		case remote && marker.BootID != "" && marker.BootID == rec.Lease.BootID:
			// The operation still runs in the process that wrote it.
		default:
			if started, err := time.Parse(time.RFC3339, marker.StartedAt); err == nil && time.Since(started) < interruptedMarkerMaxAge {
				status.Interrupted = marker
			}
		}
	}
	writeJSON(w, status, "operation")
})

// Cluster operations used by the handlers; tests replace them.
var (
	runUpdateDryRun = func(c *cluster.Client, image string) (*types.OperationResponse, error) {
		return cluster.Update(c, image, true)
	}
	runUpdateStream    = cluster.UpdateStreamWithOptions
	runReinstallStream = cluster.ReinstallStreamWithOptions
	runRefreshStream   = cluster.RefreshOperatorStreamWithOptions
)
