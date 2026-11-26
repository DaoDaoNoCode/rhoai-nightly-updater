package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operation ConfigMap (operationConfigMapName in the updater's own
// namespace) records independent values, one data key each:
//   - operationKey: the running-operation marker, written by server-side
//     apply when an operation begins (SaveOperationMarker). A marker of a
//     process that is gone reports an interrupted operation.
//   - lastCompletedKey: the most recent finished operation.
//   - leaseKey: the cluster lock, the cross-pod lease of the running
//     cluster operation (operation_locks.go). Its format is the one every
//     earlier version writes and reads.
//   - ProjectLockKey(uid): the lock of one project, by namespace UID
//     (operation_locks.go).
//   - dscHardResetKey: the DataScienceCluster a hard reset creates,
//     recorded before it deletes the old one (SaveDSCHardResetRecord), and
//     cleared once it is created.
//   - RecipeRunKey: the last environment recipe run (recipe_run_record.go).
//
// The marker's field manager never owns the other keys, so a marker
// write keeps them (an applier only removes fields it owned:
// https://kubernetes.io/docs/reference/using-api/server-side-apply/#field-management).
// The other writes are JSON merge patches of their own keys, but for the
// lock renewals, which are JSON patches (RenewOperationLocks). The
// updater's Role allows get, create and patch on this ConfigMap, nothing
// else. This process writes it through one writer (operationWriter).
const (
	OperationKey     = "operation"
	LastCompletedKey = "lastCompleted"
	LeaseKey         = "lock"
	DSCHardResetKey  = "dscHardReset"
)

// SaveDSCHardResetRecord records the DataScienceCluster a hard reset
// creates: a JSON merge patch of its own key.
func SaveDSCHardResetRecord(c *kube.Client, rec *types.DSCHardResetRecord) error {
	value, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	err = operationWriter.do(c.Context(), func() error {
		return patchOperationRecord(c, "", map[string]any{DSCHardResetKey: string(value)})
	})
	if err != nil {
		return fmt.Errorf("save the hard reset record: %w", err)
	}
	return nil
}

// ClearDSCHardResetRecord forgets the recorded hard reset once its object
// exists (or the reset deleted nothing). A failure is logged: the record is
// only offered while no DataScienceCluster exists, and the next hard reset
// replaces it.
func ClearDSCHardResetRecord(c *kube.Client) {
	err := operationWriter.do(c.Context(), func() error {
		return patchOperationRecord(c, "", map[string]any{DSCHardResetKey: ""})
	})
	if err != nil {
		slog.Warn("could not clear the hard reset record", "error", err)
	}
}

// ReadDSCHardResetRecord returns the recorded hard reset, nil when there is
// none. A record that cannot be parsed is an error: it is never guessed at.
func ReadDSCHardResetRecord(c *kube.Client) (*types.DSCHardResetRecord, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), OperationConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse operation record: %w", err)
		}
		return nil, fmt.Errorf("get operation record: %w", err)
	}
	if !found {
		return nil, nil
	}
	raw := cm.Data[DSCHardResetKey]
	if raw == "" {
		return nil, nil
	}
	var rec types.DSCHardResetRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("parse the hard reset record: %w", err)
	}
	if rec.Name == "" || rec.Object == "" || rec.APIVersion == "" || rec.OperatorCSV == "" {
		return nil, fmt.Errorf("the hard reset record lacks its name, object, apiVersion or operator")
	}
	return &rec, nil
}

// OperationRecord is what the operation ConfigMap records. A value that is
// not recorded, or cannot be parsed, is nil.
type OperationRecord struct {
	Marker        *types.OperationMarker
	LastCompleted *types.CompletedOperation
	// Lease is the cluster lock (LeaseKey).
	Lease *types.OperationLease
	// ProjectLocks are the project locks, by namespace UID
	// (ProjectLockKey); an entry that cannot be parsed is left out.
	ProjectLocks map[string]*types.OperationLease
	// CancelRequests are the cancel requests for project actions, by
	// operation ID (CancelRequestKey).
	CancelRequests map[string]*CancelRequest

	values          map[string]string // the ConfigMap's data as read
	resourceVersion string            // "" when the ConfigMap does not exist
}

// Value is the value recorded under key, as read ("" when there is none):
// what a lock renewal tests (RenewOperationLocks).
func (r *OperationRecord) Value(key string) string { return r.values[key] }

// ReadOperationRecord reads the operation ConfigMap. A missing ConfigMap is
// an empty record. An unreadable value is logged and ignored rather than
// hiding the others.
func ReadOperationRecord(c *kube.Client) (*OperationRecord, error) {
	var cm struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), OperationConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse operation record: %w", err)
		}
		return nil, fmt.Errorf("get operation record: %w", err)
	}
	if !found {
		return &OperationRecord{}, nil
	}
	rec := &OperationRecord{
		Marker:          DecodeValue[types.OperationMarker](cm.Data, OperationKey),
		LastCompleted:   DecodeValue[types.CompletedOperation](cm.Data, LastCompletedKey),
		Lease:           DecodeValue[types.OperationLease](cm.Data, LeaseKey),
		CancelRequests:  readCancelRequests(cm.Data),
		values:          cm.Data,
		resourceVersion: cm.Metadata.ResourceVersion,
	}
	for key := range cm.Data {
		uid, ok := projectLockUID(key)
		if !ok {
			continue
		}
		if l := DecodeValue[types.OperationLease](cm.Data, key); l != nil {
			if rec.ProjectLocks == nil {
				rec.ProjectLocks = map[string]*types.OperationLease{}
			}
			rec.ProjectLocks[uid] = l
		}
	}
	return rec, nil
}

func DecodeValue[T any](data map[string]string, key string) *T {
	raw := data[key]
	if raw == "" {
		return nil
	}
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		slog.Warn("ignoring an unreadable value of the operation record", "key", key, "error", err)
		return nil
	}
	return &v
}

// SaveOperationMarker records the running operation in a ConfigMap, or
// clears the record when marker is nil.
func SaveOperationMarker(c *kube.Client, marker *types.OperationMarker) error {
	value := ""
	if marker != nil {
		data, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		value = string(data)
	}
	ns := GetActivityNamespace()
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": OperationConfigMapName, "namespace": ns},
		"data":       map[string]any{OperationKey: value},
	}
	err := operationWriter.do(c.Context(), func() error {
		_, _, err := c.Bookkeeping().Apply(rhoai.NamespacedPath("v1", "configmaps", ns, OperationConfigMapName), cm)
		return err
	})
	if err != nil {
		return fmt.Errorf("save operation marker: %w", err)
	}
	return nil
}

// LeaseOwner identifies the lease of one operation of one process.
type LeaseOwner struct {
	BootID string
	ID     string
}

func (o LeaseOwner) owns(l *types.OperationLease) bool {
	return l != nil && l.BootID == o.BootID && l.ID == o.ID
}

// SaveCompletedOperation clears the running-operation marker and records
// done as the last completed operation in a single write, so the two never
// disagree. With an owner, the same write releases owner's lease, so no
// other pod ever sees the lease gone while the marker remains (which would
// report a finished operation as interrupted). Without an owner (only an
// operation that never held a lease, which pkg/api no longer runs) it is a
// blind JSON merge patch of the marker and lastCompleted.
//
// With an owner the write is conditional on the ConfigMap's
// resourceVersion and retried on a conflict, and it touches only what is
// this process's own: the lease only while owner holds it, the marker
// unless another process wrote it (a pod that took the lease over after
// this one's heartbeat stopped; its marker describes its own operation),
// and lastCompleted only when the recorded one did not finish later (a
// delayed retry must not hide another pod's newer completion).
func SaveCompletedOperation(c *kube.Client, done *types.CompletedOperation, owner *LeaseOwner) error {
	value, err := json.Marshal(done)
	if err != nil {
		return err
	}
	if owner == nil {
		err := operationWriter.do(c.Context(), func() error {
			return patchOperationRecord(c, "", map[string]any{OperationKey: "", LastCompletedKey: string(value)})
		})
		if err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		return nil
	}
	err = operationWriter.do(c.Context(), func() error {
		return updateOperationRecord(c, func(rec *OperationRecord) (map[string]any, error) {
			data := map[string]any{}
			// A delayed retry (this pod's write failed and is repeated in the
			// background) must not replace a completion another pod recorded
			// since.
			if rec.LastCompleted == nil || !finishedAfter(rec.LastCompleted, done) {
				data[LastCompletedKey] = string(value)
			}
			if rec.Marker != nil && (rec.Marker.BootID == "" || rec.Marker.BootID == owner.BootID) {
				data[OperationKey] = ""
			}
			if owner.owns(rec.Lease) {
				data[LeaseKey] = ""
			}
			return data, nil
		})
	})
	if err != nil {
		return fmt.Errorf("save completed operation: %w", err)
	}
	return nil
}

// finishedAfter reports whether a finished strictly after b (RFC 3339
// times; an unreadable time never counts as later).
func finishedAfter(a, b *types.CompletedOperation) bool {
	ta, errA := time.Parse(time.RFC3339, a.FinishedAt)
	tb, errB := time.Parse(time.RFC3339, b.FinishedAt)
	return errA == nil && errB == nil && ta.After(tb)
}

// maxLockWriteAttempts bounds the read-modify-write cycles of one
// conditional write of the operation record. This process's own writes
// never conflict with each other (operationWriter); conflicts come from
// another updater pod's writes, and from up to that many project locks
// taken, renewed and released there.
const maxLockWriteAttempts = 8

// lockBackoff is the wait before attempt (1 or more) after a conflict
// (jitteredBackoff), so two pods that collided do not collide again in
// step. Tests replace it.
var lockBackoff = jitteredBackoff

// ErrOperationRecordContended: every conditional write of the operation
// record conflicted with another writer.
var ErrOperationRecordContended = errors.New("the operation record kept changing (write conflicts); another updater process is writing it")

// updateOperationRecord makes one conditional write of the operation
// record: it reads the record, asks change for the keys to write (none:
// nothing is written; a nil value removes its key), and writes them
// conditional on the resourceVersion it read (writeOperationRecord, which
// also removes expired project locks). A conflict is retried from a fresh
// read after lockBackoff, at most maxLockWriteAttempts times; then it
// returns ErrOperationRecordContended. Errors of the read and of change
// are returned as they are. The caller holds operationWriter.
func updateOperationRecord(c *kube.Client, change func(rec *OperationRecord) (map[string]any, error)) error {
	for attempt := range maxLockWriteAttempts {
		if attempt > 0 {
			if err := kube.Clock().Sleep(contextOf(c), lockBackoff(attempt)); err != nil {
				return fmt.Errorf("no time left to retry a conflicting write of the operation record: %w", err)
			}
		}
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return err
		}
		data, err := change(rec)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return nil
		}
		err = writeOperationRecord(c, rec, data)
		if kube.IsK8sConflict(err) {
			slog.Debug("operation record write conflict, retrying", "attempt", attempt+1)
			continue
		}
		return err
	}
	return ErrOperationRecordContended
}

// writeOperationRecord writes data into the record read as rec: a merge
// patch conditional on rec's resourceVersion, or a create when the
// ConfigMap did not exist. Both fail with 409 when another writer came
// first (Conflict or AlreadyExists). The patch also removes the project
// locks of rec that have expired (prunableLockKeys), unless data writes
// them.
func writeOperationRecord(c *kube.Client, rec *OperationRecord, data map[string]any) error {
	if rec.resourceVersion == "" {
		ns := GetActivityNamespace()
		values := map[string]any{}
		for k, v := range data {
			if v != nil {
				values[k] = v
			}
		}
		cm, err := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": OperationConfigMapName, "namespace": ns},
			"data":       values,
		})
		if err != nil {
			return err
		}
		_, _, err = c.Bookkeeping().Post(rhoai.NamespacedPath("v1", "configmaps", ns, ""), cm)
		return err
	}
	body := maps.Clone(data)
	now := kube.Clock().Now()
	for _, key := range append(prunableLockKeys(rec, now), prunableCancelKeys(rec, now)...) {
		if _, written := body[key]; !written {
			body[key] = nil
		}
	}
	return patchOperationRecord(c, rec.resourceVersion, body)
}

// patchOperationRecord merge-patches data into the operation ConfigMap (a
// nil value removes its key). A resourceVersion in the patch makes the API
// server refuse it with 409
// Conflict when the object changed since it was read
// (https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions).
// Without one, a missing ConfigMap is created.
func patchOperationRecord(c *kube.Client, resourceVersion string, data map[string]any) error {
	body := map[string]any{"data": data}
	if resourceVersion != "" {
		body["metadata"] = map[string]any{"resourceVersion": resourceVersion}
	}
	ns := GetActivityNamespace()
	err := c.Bookkeeping().PatchJSON(rhoai.NamespacedPath("v1", "configmaps", ns, OperationConfigMapName), body)
	if resourceVersion == "" && kube.IsK8sNotFound(err) {
		return writeOperationRecord(c, &OperationRecord{}, data)
	}
	return err
}

// LeaseLive reports whether a lease's heartbeat is within ttl of now. A
// heartbeat in the future (clock skew between pods) is live; an
// unreadable one is not.
func LeaseLive(l *types.OperationLease, now time.Time, ttl time.Duration) bool {
	if l == nil {
		return false
	}
	beat, err := LeaseHeartbeat(l)
	if err != nil {
		return false
	}
	return now.Sub(beat) < ttl
}

// LeaseHeartbeat is the heartbeat of a lease as every process reads it
// (HeartbeatAt, RFC 3339 to the second). The holder counts its own
// deadline from this same instant, not from when its write returned.
func LeaseHeartbeat(l *types.OperationLease) (time.Time, error) {
	return time.Parse(time.RFC3339, l.HeartbeatAt)
}
