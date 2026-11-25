package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// releaseClusterMutationLock releases the cluster lock and forgets the
// in-flight cluster operation. An operation that began (see
// beginOperation) becomes the last completed operation, and its marker is
// cleared in the same write that releases its lease; the lease of an
// operation that never began is released on its own.
func releaseClusterMutationLock() {
	owner := leases.stop(cluster.ClusterLockKey)
	done, bookkeeper := inflight.finish()
	switch {
	case done != nil:
		markers.complete(bookkeeper, done, owner)
	case owner != nil && bookkeeper != nil:
		leases.release(bookkeeper, cluster.ClusterLockKey, *owner)
	}
	locks.release(lockWant{})
}

// lockCluster acquires the cluster lock, this process's slot and then the
// cross-pod lease, or answers 409 Conflict with errorCode "cluster_busy"
// and the operation that holds it (also one that runs in another updater
// pod). When it returns true the caller must defer
// releaseClusterMutationLock.
func lockCluster(w http.ResponseWriter) bool {
	sw, ok := w.(*statusWriter)
	var cancelOp context.CancelCauseFunc
	if ok {
		cancelOp = sw.cancelOp
	}
	refusal := acquireRunnerLock(func() *Operation {
		if !ok {
			return nil
		}
		op := inflight.start(sw)
		sw.opID = op.ID
		return op
	}, cancelOp)
	if refusal == nil {
		return true
	}
	writeLockRefusal(w, refusal)
	return false
}

// writeLockRefusal answers a refusal of acquireRunnerLock: 409
// cluster_busy naming the operation that holds the lock (in this pod or
// another) to a cluster-admin, and nobody else's to anyone else (see
// writeClusterBusy), or 503 lock_unavailable when the lease could not be
// read or written.
func writeLockRefusal(w http.ResponseWriter, refusal *lockRefusal) {
	var viewer busyViewer
	if sw, ok := w.(*statusWriter); ok {
		viewer = busyViewer{admin: sw.admin, user: sw.user}
	}
	switch {
	case refusal.busy:
		op := inflight.get(refusal.opID)
		if op == nil {
			op = inflight.snapshot()
		}
		writeClusterBusy(w, op, refusal.reason, viewer)
		return
	case refusal.holder != nil:
		holder := refusal.holder
		slog.Info("operation refused: another updater pod runs one", "holder", holder.ID, "holderPod", holder.Pod, "type", holder.Type, "project", holder.Project)
		writeClusterBusy(w, remoteOperation(holder), refusal.reason, viewer)
		return
	}
	slog.Warn("operation refused: the cross-pod operation lock could not be read or written", "error", refusal.err)
	w.Header().Set("Retry-After", strconv.Itoa(int(lockUnavailableRetryAfter.Seconds())))
	writeError(w, lockUnavailableMessage, http.StatusServiceUnavailable, "lock_unavailable")
}

// lockRefusal is why a lock was not taken. Exactly one of busy, holder and
// err is set.
type lockRefusal struct {
	// busy: another operation of this process holds a conflicting slot:
	// opID ("" when it is not recorded yet).
	busy bool
	opID string
	// holder is the live lease of another updater process that holds a
	// conflicting lock.
	holder *types.OperationLease
	// reason is the rule that refused it (conflictCluster, ...) for busy
	// and holder.
	reason string
	// err kept the lease from being read or written (fail closed: without
	// the lease the updater cannot tell whether another pod changes the
	// cluster).
	err error
}

// acquireRunnerLock takes the cluster lock: this process's slot, then
// records the operation start returns as the one holding it, then takes
// the cross-pod lease for that operation with its bookkeeper (the
// ServiceAccount's client) (acquireLock). It is the one way to take the
// cluster lock: lockCluster for a request, and a holder that runs without
// one or outlives it (the background MaaS check, a recipe run).
//
// cancelOp is what the lease heartbeat calls with
// cluster.ErrOperationLockLost when the lease is lost: the request's cancel
// for a handler, the holder's own for a holder that outlives the handler.
//
// start runs only once the slot is held. It may return nil (nothing
// to track, as for a response that is not a statusWriter in tests), and the
// operation may have no bookkeeper (no ServiceAccount token): then no lease
// is taken, as before.
//
// It returns nil when the lock is held; the caller must then call
// releaseClusterMutationLock when it ends, which completes the marker of a
// begun operation and releases the lease. On a refusal nothing is held: the
// operation is forgotten (it never began) and the slot is free again.
func acquireRunnerLock(start func() *Operation, cancelOp context.CancelCauseFunc) *lockRefusal {
	return acquireLock(lockWant{}, start, cancelOp)
}

// The refusal when the lease cannot be read or written (fail closed).
const (
	lockUnavailableMessage    = "Cannot verify that no other updater pod is running an operation (the operation lock could not be read or written). Nothing was changed; try again."
	lockUnavailableRetryAfter = 10 * time.Second
)

// busyViewer is who is refused: a cluster-admin (withMutationAuth checked
// it) or not, and their user name.
type busyViewer struct {
	admin bool
	user  string
}

// writeClusterBusy answers 409 cluster_busy. A cluster-admin learns op
// when it is known: who runs it, where, and the operation itself. Anyone
// else learns no other user's name and no project but their own action's
// (per-user views decision 4): see busyMessageForUser.
func writeClusterBusy(w http.ResponseWriter, op *Operation, reason string, viewer busyViewer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	body := map[string]any{"error": "Another cluster operation is in progress. Please wait.", "errorCode": "cluster_busy"}
	if !viewer.admin {
		body["error"] = busyMessageForUser(op, reason, viewer.user)
	} else if op != nil {
		body["operation"] = op
		where := ""
		if op.Project != "" {
			where = " in project " + op.Project
		}
		if op.Remote && op.Pod != "" {
			where += " on updater pod " + op.Pod
		}
		body["error"] = op.User + " is running \"" + op.Label + "\"" + where + ". Wait for it to finish."
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("response encode error", "label", "cluster-busy", "error", err)
	}
}

// busyMessageForUser is the refusal a user who is not a cluster-admin
// reads: a cluster operation by its start only, their own action by its
// label and project, another action in the project they asked for by its
// label and start; never another user's name or another project.
func busyMessageForUser(op *Operation, reason, user string) string {
	switch {
	case reason == conflictCap:
		return "Too many project actions are running at once. Try again in a few minutes."
	case op == nil:
		return "Another operation is in progress. Please wait."
	case op.ProjectUID == "":
		return "A cluster operation is running since " + op.StartedAt.UTC().Format("15:04") + " UTC. Project actions can start when it ends."
	case op.User == user:
		return "You already have an action running (" + op.Label + " in project " + op.Project + "). Wait for it to finish."
	case reason == conflictProject:
		return "An action (" + op.Label + ") is running in this project since " + op.StartedAt.UTC().Format("15:04") + " UTC. Wait for it to finish."
	}
	return "Another operation is in progress. Please wait."
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
	var bookkeeper *cluster.Client
	inflight.update(sw.opID, func(op *Operation) {
		op.Target = truncateText(target, maxOperationTargetBytes)
		if !op.begun {
			op.begun = true
			marker, bookkeeper = operationMarker(op), op.bookkeeper
		}
	})
	if marker != nil && bookkeeper != nil {
		markers.start(bookkeeper, marker)
	}
}

// beginHeldOperation is beginOperation for the operation with this ID,
// which holds the lock without a request (a recipe run): it records its
// target and, the first time, writes its marker.
func beginHeldOperation(id, target string) {
	var marker *types.OperationMarker
	var bookkeeper *cluster.Client
	inflight.update(id, func(op *Operation) {
		op.Target = truncateText(target, maxOperationTargetBytes)
		if !op.begun {
			op.begun = true
			marker, bookkeeper = operationMarker(op), op.bookkeeper
		}
	})
	if marker != nil && bookkeeper != nil {
		markers.start(bookkeeper, marker)
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
	// Project and ProjectUID: the project a project action runs in (the
	// namespace and its UID); a cluster operation has neither.
	Project    string `json:"project,omitempty"`
	ProjectUID string `json:"projectUid,omitempty"`
	// Scope ("cluster" or "project") and Interrupted are set on the
	// entries of OperationStatus.Operations only.
	Scope       string `json:"scope,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`

	// bookkeeper writes the operation's lease and marker: always the
	// ServiceAccount's client, which may outlive the request.
	bookkeeper *cluster.Client
	// freshBookkeeper, when set, makes the bookkeeper again with the
	// ServiceAccount token as it is now: the lease heartbeat and the
	// marker clear use it, so an operation that holds the lock for hours
	// (a recipe run) does not keep writing with the token read at its
	// start.
	freshBookkeeper func(ctx context.Context) *cluster.Client
	sw              *statusWriter // the response, for the outcome of non-streaming operations
	begun           bool          // set by beginOperation
	// confirmed: its lock is held (confirmLock); a pending acquisition
	// is not listed in OperationStatus.Operations.
	confirmed bool
	// lockLost: the lease heartbeat stopped the operation because its lease
	// was lost; its outcome is cluster.LockLostMessage.
	lockLost bool
}

// operationTypes maps mutation endpoints to a stable type and a label.
var operationTypes = map[string][2]string{
	"/api/update":                                 {"update-dry-run", "Dry run of a nightly update"},
	"/api/update/stream":                          {"update", "Update to nightly"},
	"/api/reinstall/stream":                       {"reinstall", "Install a different build"},
	"/api/refresh/stream":                         {"refresh", "Refresh operator"},
	"/api/components/dsc/repair":                  {"repair-dsc", "Repair DataScienceCluster"},
	"/api/components/dsc/management":              {"set-component-state", "Change a DataScienceCluster component"},
	"/api/components/dsc/fix":                     {"fix-dsc", "Fix the DataScienceCluster"},
	"/api/components/dsc/reset":                   {"reset-dsc", "Reset the DataScienceCluster in place"},
	"/api/components/dsc/hard-reset/stream":       {"hard-reset-dsc", "Hard reset the DataScienceCluster"},
	"/api/components/dsc/hard-reset/finish":       {"finish-hard-reset-dsc", "Finish the hard reset of the DataScienceCluster"},
	"/api/assist-rollout":                         {"assist-rollout", "Assist stuck rollouts"},
	"/api/setup/pull-secret":                      {"create-pull-secret", "Configure pull secret"},
	"/api/setup/pull-secret/registry-copy":        {"copy-registry-pull-secret", "Copy the pull secret for the internal registry"},
	"/api/setup/pull-secret/registry-copy/remove": {"remove-registry-pull-secret", "Remove the internal registry's copy of the pull secret"},
	"/api/setup/node-pull-check":                  {"node-pull-check", "Check that the nodes can pull"},
	"/api/setup/nodes/plan":                       {"node-setup-plan", "Review a cluster setup change"},
	"/api/setup/nodes/stream":                     {"node-setup", "One-time cluster setup"},
	"/api/setup/dsc":                              {"create-dsc", "Create DataScienceCluster"},
	"/api/dashboard/deploy-pr":                    {"deploy-dashboard-pr", "Deploy dashboard PR"},
	"/api/dashboard/deploy-main":                  {"deploy-dashboard-main", "Deploy dashboard main"},
	"/api/dashboard/revert":                       {"revert-dashboard", "Revert dashboard"},
	"/api/resources/s3/setup":                     {"setup-s3", "Set up S3 storage"},
	"/api/resources/s3/teardown":                  {"teardown-s3", "Delete S3 storage"},
	"/api/resources/s3/secure/stream":             {"secure-s3", "Secure S3 storage"},
	"/api/resources/s3/leftover-keys/remove":      {"remove-leftover-s3-keys", "Remove leftover S3 keys"},
	"/api/resources/s3/migrate/stream":            {"migrate-storage", "Migrate S3 storage"},
	"/api/resources/pipeline-server/setup":        {"setup-pipeline-server", "Set up pipeline server"},
	"/api/resources/pipeline-server/teardown":     {"teardown-pipeline-server", "Delete pipeline server"},
	"/api/resources/mlflow/setup":                 {"setup-mlflow", "Set up MLflow"},
	"/api/resources/mlflow/teardown":              {"teardown-mlflow", "Delete MLflow"},
	"/api/resources/mlflow/deploy-pr":             {"deploy-mlflow-pr", "Deploy MLflow PR"},
	"/api/resources/mlflow/revert":                {"revert-mlflow", "Revert MLflow"},
	"/api/resources/models/stream":                {"models", "Models"},
	"/api/resources/playground/stream":            {"playground", "Playground"},
	"/api/resources/model-registry/stream":        {"model-registry", "Model registry"},
	"/api/resources/workbench/stream":             {"workbench", "Workbench"},
	"/api/resources/kueue/stream":                 {"kueue", "Distributed workloads"},
	"/api/resources/maas/stream":                  {"maas", "Models as a Service"},
	"/api/resources/prerequisites/stream":         {"prerequisites", "Set up a feature's prerequisites"},
	"/api/resources/cleanup/stream":               {"cleanup", "Clean up the environment"},
	"/api/resources/samples/mlflow/stream":        {"mlflow-samples", "MLflow samples"},
	"/api/diagnostics/fix":                        {"diagnostics-fix", "Apply diagnostics fix"},
	"/api/diagnostics/fix/acknowledged":           {"diagnostics-fix", "Apply diagnostics fix"},
	"/api/recipes/plan":                           {"recipe-plan", "Review an environment recipe"},
	// A run holds the lock for all its steps (recipe_runner.go) under
	// this type and label; a user who is not a cluster-admin sees only
	// the label.
	"/api/recipes/run":    {"recipe", "Environment recipe"},
	"/api/recipes/resume": {"recipe", "Environment recipe"},
	// Stop takes no lock (it asks the running run to stop after its step).
	"/api/recipes/stop": {"recipe-stop", "Stop an environment recipe"},
}

// Size bounds of what is kept and persisted about an operation.
const (
	maxOperationTargetBytes  = 512
	maxOperationMessageBytes = 1024
)

// operationTracker is this process's running operations: the one that
// holds the cluster lock, and (in time) project actions, each holding its
// project's lock.
type operationTracker struct {
	mu      sync.Mutex
	ops     map[string]*Operation     // by ID
	cluster string                    // the ID of the one holding the cluster lock ("" none)
	last    *types.CompletedOperation // the most recent begun cluster operation that finished
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

// start records the cluster operation of a request that took the
// cluster lock.
func (t *operationTracker) start(sw *statusWriter) *Operation {
	kind := operationTypes[sw.path]
	if kind[0] == "" {
		kind = [2]string{"operation", sw.path}
	}
	now := time.Now().UTC()
	op := &Operation{
		ID: newOperationID(), Type: kind[0], Label: kind[1], User: sw.user,
		StartedAt: now, UpdatedAt: now, Pod: os.Getenv("HOSTNAME"), bookkeeper: sw.bookkeeper, sw: sw, freshBookkeeper: sw.newBookkeeper,
	}
	t.add(op)
	return op
}

// startBackground records an operation the updater runs on its own (no
// request) as the cluster operation holding the lock, so the status shows
// it and the lease heartbeat renews it. The caller holds the cluster slot;
// c is the ServiceAccount's client.
func (t *operationTracker) startBackground(typ, label, user string, c *cluster.Client) *Operation {
	now := time.Now().UTC()
	op := &Operation{
		ID: newOperationID(), Type: typ, Label: label, User: user,
		StartedAt: now, UpdatedAt: now, Pod: os.Getenv("HOSTNAME"), bookkeeper: c,
	}
	t.add(op)
	return op
}

// startProject records a project action in project (namespace UID uid)
// that holds that project's slot; c is the ServiceAccount's client.
func (t *operationTracker) startProject(typ, label, user, project, uid string, c *cluster.Client) *Operation {
	now := time.Now().UTC()
	op := &Operation{
		ID: newOperationID(), Type: typ, Label: label, User: user, Project: project, ProjectUID: uid,
		StartedAt: now, UpdatedAt: now, Pod: os.Getenv("HOSTNAME"), bookkeeper: c,
	}
	t.add(op)
	return op
}

// add records op, as the cluster operation unless it is a project action.
func (t *operationTracker) add(op *Operation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ops == nil {
		t.ops = map[string]*Operation{}
	}
	t.ops[op.ID] = op
	if op.ProjectUID == "" {
		t.cluster = op.ID
	}
}

func (t *operationTracker) update(id string, fn func(*Operation)) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if op := t.ops[id]; op != nil {
		fn(op)
		op.UpdatedAt = time.Now().UTC()
	}
}

// recordStep captures a progress event of the operation with this ID.
func (t *operationTracker) recordStep(id string, step UpdateStep) {
	t.update(id, func(op *Operation) {
		op.Step, op.StepStatus, op.Message = step.Step, step.Status, step.Message
	})
}

// finish forgets the cluster operation (finishID).
func (t *operationTracker) finish() (*types.CompletedOperation, *cluster.Client) {
	t.mu.Lock()
	id := t.cluster
	t.mu.Unlock()
	return t.finishID(id)
}

// finishID forgets the operation with this ID and returns its bookkeeping
// client. When it had begun, its outcome is returned too; a cluster
// operation's becomes the last completed operation.
// It runs on the handler's goroutine after the response is written.
func (t *operationTracker) finishID(id string) (*types.CompletedOperation, *cluster.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.ops[id]
	if op == nil {
		return nil, nil
	}
	delete(t.ops, id)
	if t.cluster == id {
		t.cluster = ""
	}
	bookkeeper := op.bookkeeper
	if op.freshBookkeeper != nil {
		bookkeeper = op.freshBookkeeper(context.Background())
	}
	if !op.begun {
		return nil, bookkeeper
	}
	success, message := operationOutcome(op)
	done := &types.CompletedOperation{
		ID: op.ID, Type: op.Type, Label: op.Label, User: op.User, Target: op.Target,
		StartedAt:  op.StartedAt.Format(time.RFC3339),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
		Success:    success,
		Message:    truncateText(message, maxOperationMessageBytes),
	}
	if op.ProjectUID == "" {
		cp := *done
		t.last = &cp
	}
	return done, bookkeeper
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

// snapshot returns a copy of the cluster operation, or nil.
func (t *operationTracker) snapshot() *Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return copyOperation(t.ops[t.cluster])
}

// get returns a copy of the operation with this ID, or nil.
func (t *operationTracker) get(id string) *Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return copyOperation(t.ops[id])
}

// clusterID is the ID of the cluster operation, "" when none runs.
func (t *operationTracker) clusterID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cluster
}

// list returns copies of the running operations, oldest first.
func (t *operationTracker) list() []*Operation {
	t.mu.Lock()
	out := make([]*Operation, 0, len(t.ops))
	for _, op := range t.ops {
		out = append(out, copyOperation(op))
	}
	t.mu.Unlock()
	sortOperations(out)
	return out
}

// copyOperation is a copy of op without what only this process uses, nil
// for nil.
func copyOperation(op *Operation) *Operation {
	if op == nil {
		return nil
	}
	cp := *op
	cp.bookkeeper, cp.sw, cp.freshBookkeeper = nil, nil, nil
	return &cp
}

// sortOperations orders ops by start, then ID.
func sortOperations(ops []*Operation) {
	sort.SliceStable(ops, func(i, j int) bool {
		if !ops[i].StartedAt.Equal(ops[j].StartedAt) {
			return ops[i].StartedAt.Before(ops[j].StartedAt)
		}
		return ops[i].ID < ops[j].ID
	})
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
	// InProgress: a cluster operation runs.
	InProgress bool `json:"inProgress"`
	// Operation is the running cluster operation: this process's own, or
	// one that another updater pod runs (Operation.Remote, from its lease).
	Operation *Operation `json:"operation"`
	// Interrupted is a cluster operation that an updater process started
	// and never finished: its marker is still recorded and its process
	// holds no live lease (the pod or its container crashed or was killed,
	// or a version without leases left it). Its cluster changes may be
	// half-done.
	Interrupted *types.OperationMarker `json:"interrupted,omitempty"`
	// LastCompleted is the most recent finished cluster operation, also one
	// that finished before this process started or in another updater pod.
	LastCompleted *types.CompletedOperation `json:"lastCompleted,omitempty"`
	// Operations is every operation the locks know, oldest first: the
	// running ones (this process's and other pods', the cluster operation
	// and project actions; Scope says which) and the interrupted ones (the
	// cluster operation's marker; a project action whose lock expired after
	// it began, for interruptedMarkerMaxAge). The fields above are the
	// cluster operation's part of it, as earlier versions answered.
	Operations []*Operation `json:"operations"`
}

// HandleOperation reports the operations in progress, the last completed
// cluster operation, and interrupted ones, if any. While this process runs
// a cluster operation it answers from memory; otherwise it reads the
// operation record, which also tells about other pods' operations.
//
// A user who is not a cluster-admin gets types.OperationSummary only:
// whether a cluster operation runs, its label and when it started; not who
// started it, its target, its pod, or an interrupted one (tier per-role);
// of the list, the running cluster operation only (project actions are
// left out until their per-project view exists). The record is read, and
// its marker retried, with the ServiceAccount for both.
var HandleOperation = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	status := operationStatus(c, admin)
	if !admin {
		writeJSON(w, operationSummary(status), "operation")
		return
	}
	writeJSON(w, status, "operation")
})

// operationSummary is what a user who is not a cluster-admin sees of the
// operation status.
func operationSummary(status OperationStatus) types.OperationSummary {
	sum := types.OperationSummary{Running: status.InProgress, Operations: []types.OperationSummaryEntry{}}
	for _, op := range status.Operations {
		if op.Scope == scopeCluster && !op.Interrupted {
			sum.Operations = append(sum.Operations, types.OperationSummaryEntry{Scope: scopeCluster, Label: op.Label, StartedAt: op.StartedAt.UTC().Format(time.RFC3339)})
		}
	}
	if !status.InProgress || status.Operation == nil {
		return sum
	}
	sum.Label, sum.StartedAt = status.Operation.Label, status.Operation.StartedAt.UTC().Format(time.RFC3339)
	return sum
}

// The scopes of OperationStatus.Operations.
const (
	scopeCluster = "cluster"
	scopeProject = "project"
)

// operationStatus is the operation status (HandleOperation). While this
// process runs a cluster operation it makes no cluster call; a
// cluster-admin (admin) then gets the interrupted project actions of the
// record as this process last read it (seenProjectLocks).
func operationStatus(c *cluster.Client, admin bool) (status OperationStatus) {
	local := locks.clusterHeld()
	status = OperationStatus{InProgress: local, Operation: inflight.snapshot()}
	running := inflight.list()
	last := inflight.lastCompleted()
	if local && last != nil {
		// No other operation runs anywhere while this one holds the
		// cluster lock.
		status.LastCompleted = last
		var rec *cluster.OperationRecord
		if admin {
			rec = &cluster.OperationRecord{ProjectLocks: seenProjectLocks.interrupted()}
		}
		status.Operations = operationList(running, rec, nil)
		return status
	}
	rec, err := readOperationState(c)
	if err != nil || rec == nil {
		slog.Debug("could not read the operation record", "error", err)
		status.LastCompleted = last
		status.Operations = operationList(running, nil, nil)
		return status
	}
	seenProjectLocks.remember(rec)
	status.LastCompleted = inflight.rememberLast(rec.LastCompleted)
	defer func() { status.Operations = operationList(running, rec, status.Interrupted) }()
	if local {
		return status
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
	return status
}

// operationList is OperationStatus.Operations: running (this process's
// operations whose lock is held: confirmed), and, from rec when it was
// read, the live locks of other processes and the interrupted operations
// (interrupted: the cluster operation's).
func operationList(running []*Operation, rec *cluster.OperationRecord, interrupted *types.OperationMarker) []*Operation {
	out := make([]*Operation, 0, len(running))
	for _, op := range running {
		if !op.confirmed {
			continue
		}
		op.Scope = scopeOf(op)
		out = append(out, op)
	}
	if rec != nil {
		if leaseHeldElsewhere(rec.Lease) {
			op := remoteOperation(rec.Lease)
			op.Scope = scopeCluster
			out = append(out, op)
		}
		for _, l := range rec.ProjectLocks {
			switch {
			case leaseHeldElsewhere(l):
				op := remoteOperation(l)
				op.Scope = scopeProject
				out = append(out, op)
			case projectLockInterrupted(l):
				op := remoteOperation(l)
				op.Scope, op.Interrupted, op.Remote = scopeProject, true, false
				out = append(out, op)
			}
		}
	}
	if interrupted != nil {
		started, _ := time.Parse(time.RFC3339, interrupted.StartedAt)
		out = append(out, &Operation{ID: interrupted.ID, Type: interrupted.Type, Label: interrupted.Label, User: interrupted.User,
			Target: interrupted.Target, StartedAt: started, Pod: interrupted.Pod, Scope: scopeCluster, Interrupted: true})
	}
	sortOperations(out)
	return out
}

// projectLockCache is the project locks of the operation record as this
// process read it last (GET /api/operation, a lock's check). While a
// cluster operation runs here no project action can start anywhere, so
// the interrupted ones it lists change only by age (projectLockInterrupted
// is applied when they are listed).
type projectLockCache struct {
	mu    sync.Mutex
	locks map[string]*types.OperationLease
}

var seenProjectLocks = &projectLockCache{}

func (p *projectLockCache) remember(rec *cluster.OperationRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.locks = rec.ProjectLocks
}

// interrupted is the remembered locks of interrupted project actions.
func (p *projectLockCache) interrupted() map[string]*types.OperationLease {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]*types.OperationLease{}
	for uid, l := range p.locks {
		if projectLockInterrupted(l) {
			out[uid] = l
		}
	}
	return out
}

func scopeOf(op *Operation) string {
	if op.ProjectUID != "" {
		return scopeProject
	}
	return scopeCluster
}

// projectLockInterrupted reports whether l is the lock of a project action
// that was interrupted: it began, its lease expired (its process stopped
// renewing it: the pod or container is gone), it was not this process's,
// and it started less than interruptedMarkerMaxAge ago. The next action in
// that project supersedes it (the same key).
func projectLockInterrupted(l *types.OperationLease) bool {
	if l == nil || !l.Begun || l.BootID == bootID || cluster.LeaseLive(l, clk.Now(), leaseTTL) {
		return false
	}
	started, err := time.Parse(time.RFC3339, l.StartedAt)
	return err == nil && time.Since(started) < interruptedMarkerMaxAge
}

// Cluster operations used by the handlers; tests replace them.
var (
	runUpdateDryRun = func(c *cluster.Client, image string) (*types.OperationResponse, error) {
		return cluster.Update(c, image, true)
	}
	runUpdateStream    = cluster.UpdateStreamWithOptions
	runReinstallStream = cluster.ReinstallStreamWithOptions
	runRefreshStream   = cluster.RefreshOperatorStreamWithOptions
	// Update in the gitops profile (HandleUpdateStreamGitOps).
	runUpdateStreamGitOps = cluster.UpdateStreamGitOpsProfile
	runUpdateDryRunGitOps = cluster.UpdateDryRunGitOpsProfile
)
