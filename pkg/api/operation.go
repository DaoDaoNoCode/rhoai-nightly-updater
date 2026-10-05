package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// clusterMutationInProgress is a non-blocking mutex that prevents concurrent
// cluster-level mutation operations. When true, a cluster mutation is in
// progress and new mutation requests receive HTTP 409 Conflict instead of
// proceeding. The lock is per process; the Deployment runs one replica with
// the Recreate strategy so that only one updater pod exists at a time.
var clusterMutationInProgress atomic.Bool

// acquireClusterMutationLock tries to acquire the cluster mutation lock.
// Returns true if the lock was acquired (caller must defer releaseClusterMutationLock).
// Returns false if another operation is already in progress; the caller should
// return HTTP 409.
func acquireClusterMutationLock() bool {
	return clusterMutationInProgress.CompareAndSwap(false, true)
}

// releaseClusterMutationLock releases the cluster mutation lock and forgets
// the in-flight operation.
func releaseClusterMutationLock() {
	op := inflight.clear()
	if op != nil && op.client != nil {
		clearOperationMarker(op.client)
	}
	clusterMutationInProgress.Store(false)
}

// lockCluster acquires the cluster mutation lock or answers 409 Conflict
// with errorCode "cluster_busy" and the running operation. When it returns
// true the caller must defer releaseClusterMutationLock.
func lockCluster(w http.ResponseWriter) bool {
	if acquireClusterMutationLock() {
		if sw, ok := w.(*statusWriter); ok {
			op := inflight.start(sw)
			sw.opID = op.ID
			saveOperationMarker(op)
		}
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	body := map[string]interface{}{
		"error":     "Another cluster operation is in progress. Please wait.",
		"errorCode": "cluster_busy",
	}
	if op := inflight.snapshot(); op != nil {
		body["operation"] = op
		body["error"] = op.User + " is running \"" + op.Label + "\". Wait for it to finish."
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("response encode error", "label", "cluster-busy", "error", err)
	}
	return false
}

// setOperationTarget records what an accepted operation acts on (for example
// the catalog image), so other users can see it while it runs.
func setOperationTarget(w http.ResponseWriter, target string) {
	if sw, ok := w.(*statusWriter); ok {
		inflight.update(sw.opID, func(op *Operation) { op.Target = target })
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

	client *cluster.Client
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

type operationTracker struct {
	mu      sync.Mutex
	current *Operation
}

var inflight = &operationTracker{}

func newOperationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (t *operationTracker) start(sw *statusWriter) *Operation {
	kind := operationTypes[sw.path]
	if kind[0] == "" {
		kind = [2]string{"operation", sw.path}
	}
	now := time.Now().UTC()
	op := &Operation{
		ID: newOperationID(), Type: kind[0], Label: kind[1], User: sw.user,
		StartedAt: now, UpdatedAt: now, Pod: os.Getenv("HOSTNAME"), client: sw.client,
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

func (t *operationTracker) clear() *Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	op := t.current
	t.current = nil
	return op
}

// snapshot returns a copy of the current operation, or nil.
func (t *operationTracker) snapshot() *Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		return nil
	}
	cp := *t.current
	cp.client = nil
	return &cp
}

// The operation marker survives a crash of the updater pod: it is written
// to a ConfigMap when an operation starts and cleared when it ends. A marker
// left by another pod means that pod died mid-operation (a graceful restart
// drains running operations first). Marker errors never block operations.
var (
	saveOperationMarker = func(op *Operation) {
		if op.client == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cluster.SaveOperationMarker(op.client.WithContext(ctx), operationMarker(op)); err != nil {
			slog.Warn("could not record the running operation", "error", err)
		}
	}
	clearOperationMarker = func(c *cluster.Client) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cluster.SaveOperationMarker(c.WithContext(ctx), nil); err != nil {
			slog.Warn("could not clear the running-operation marker", "error", err)
		}
	}
	readOperationMarker = cluster.GetOperationMarker
)

func operationMarker(op *Operation) *types.OperationMarker {
	return &types.OperationMarker{
		Type: op.Type, Label: op.Label, User: op.User, Target: op.Target,
		StartedAt: op.StartedAt.Format(time.RFC3339), Pod: op.Pod,
	}
}

// interruptedMarkerMaxAge bounds how long an interrupted operation is shown.
const interruptedMarkerMaxAge = 24 * time.Hour

// OperationStatus is the GET /api/operation response.
type OperationStatus struct {
	InProgress bool       `json:"inProgress"`
	Operation  *Operation `json:"operation"`
	// Interrupted is an operation that a previous updater pod started and
	// never finished (the pod crashed or was killed). Its cluster changes
	// may be half-done.
	Interrupted *types.OperationMarker `json:"interrupted,omitempty"`
}

// HandleOperation reports the cluster operation in progress, if any.
var HandleOperation = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	status := OperationStatus{InProgress: clusterMutationInProgress.Load(), Operation: inflight.snapshot()}
	if !status.InProgress {
		if marker, err := readOperationMarker(c); err != nil {
			slog.Debug("could not read the operation marker", "error", err)
		} else if marker != nil && marker.Pod != os.Getenv("HOSTNAME") {
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
	runUpdateStream    = cluster.UpdateStream
	runReinstallStream = cluster.ReinstallStream
	runRefreshStream   = cluster.RefreshOperatorStream
)
