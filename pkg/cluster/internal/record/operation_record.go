package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operation ConfigMap (operationConfigMapName in the updater's own
// namespace) records three independent values, one data key each:
//   - operationKey: the running-operation marker, written by server-side
//     apply when an operation begins (SaveOperationMarker). A marker of a
//     process that is gone reports an interrupted operation.
//   - lastCompletedKey: the most recent finished operation.
//   - leaseKey: the cross-pod operation lock (WriteOperationLease).
//   - dscHardResetKey: the DataScienceCluster a hard reset creates,
//     recorded before it deletes the old one (SaveDSCHardResetRecord), and
//     cleared once it is created.
//
// The marker's field manager never owns the other keys, so a marker
// write keeps them (an applier only removes fields it owned:
// https://kubernetes.io/docs/reference/using-api/server-side-apply/#field-management).
// The other writes are JSON merge patches of their own keys. The updater's
// Role allows get, create and patch on this ConfigMap, nothing else.
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
	if err := patchOperationRecord(c, "", map[string]string{DSCHardResetKey: string(value)}); err != nil {
		return fmt.Errorf("save the hard reset record: %w", err)
	}
	return nil
}

// ClearDSCHardResetRecord forgets the recorded hard reset once its object
// exists (or the reset deleted nothing). A failure is logged: the record is
// only offered while no DataScienceCluster exists, and the next hard reset
// replaces it.
func ClearDSCHardResetRecord(c *kube.Client) {
	if err := patchOperationRecord(c, "", map[string]string{DSCHardResetKey: ""}); err != nil {
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
	Lease         *types.OperationLease

	resourceVersion string // "" when the ConfigMap does not exist
}

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
	return &OperationRecord{
		Marker:          DecodeRecordValue[types.OperationMarker](cm.Data, OperationKey),
		LastCompleted:   DecodeRecordValue[types.CompletedOperation](cm.Data, LastCompletedKey),
		Lease:           DecodeRecordValue[types.OperationLease](cm.Data, LeaseKey),
		resourceVersion: cm.Metadata.ResourceVersion,
	}, nil
}

func DecodeRecordValue[T any](data map[string]string, key string) *T {
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
	if _, _, err := c.Bookkeeping().Apply(rhoai.NamespacedPath("v1", "configmaps", ns, OperationConfigMapName), cm); err != nil {
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
		data := map[string]string{OperationKey: "", LastCompletedKey: string(value)}
		if err := patchOperationRecord(c, "", data); err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		return nil
	}
	for range maxLeaseWriteAttempts {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		data := map[string]string{}
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
		if len(data) == 0 {
			return nil
		}
		err = writeOperationRecord(c, rec, data)
		if kube.IsK8sConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		return nil
	}
	return fmt.Errorf("save completed operation: %w", ErrOperationRecordContended)
}

// finishedAfter reports whether a finished strictly after b (RFC 3339
// times; an unreadable time never counts as later).
func finishedAfter(a, b *types.CompletedOperation) bool {
	ta, errA := time.Parse(time.RFC3339, a.FinishedAt)
	tb, errB := time.Parse(time.RFC3339, b.FinishedAt)
	return errA == nil && errB == nil && ta.After(tb)
}

// maxLeaseWriteAttempts bounds the read-modify-write cycles of one
// conditional write of the operation record. Conflicts come from the
// marker, completion and heartbeat writes of at most two processes, so a
// few rounds settle them.
const maxLeaseWriteAttempts = 4

// ErrOperationRecordContended: every conditional write of the operation
// record conflicted with another writer.
var ErrOperationRecordContended = errors.New("the operation record kept changing (write conflicts); another updater process is writing it")

// writeOperationRecord writes data into the record read as rec: a merge
// patch conditional on rec's resourceVersion, or a create when the
// ConfigMap did not exist. Both fail with 409 when another writer came
// first (Conflict or AlreadyExists).
func writeOperationRecord(c *kube.Client, rec *OperationRecord, data map[string]string) error {
	if rec.resourceVersion == "" {
		ns := GetActivityNamespace()
		cm, err := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": OperationConfigMapName, "namespace": ns},
			"data":       data,
		})
		if err != nil {
			return err
		}
		_, _, err = c.Bookkeeping().Post(rhoai.NamespacedPath("v1", "configmaps", ns, ""), cm)
		return err
	}
	return patchOperationRecord(c, rec.resourceVersion, data)
}

// patchOperationRecord merge-patches data into the operation ConfigMap. A
// resourceVersion in the patch makes the API server refuse it with 409
// Conflict when the object changed since it was read
// (https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions).
// Without one, a missing ConfigMap is created.
func patchOperationRecord(c *kube.Client, resourceVersion string, data map[string]string) error {
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

// WriteOperationLease records lease as the holder of the cross-pod
// operation lock: it takes the lock or renews it. heldElsewhere decides
// whether a recorded lease belongs to another process that is still alive;
// such a lease is never overwritten and is returned instead. A lease that
// is absent, expired, of a dead process or this process's own is replaced.
// The write is conditional on the ConfigMap's resourceVersion, so two
// processes that read a free lock at the same time cannot both take it: the
// second write fails with 409 and is retried from a fresh read. When every
// attempt conflicts it returns ErrOperationRecordContended.
func WriteOperationLease(c *kube.Client, lease *types.OperationLease, heldElsewhere func(*types.OperationLease) bool) (*types.OperationLease, error) {
	value, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}
	for attempt := range maxLeaseWriteAttempts {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return nil, err
		}
		if rec.Lease != nil && heldElsewhere(rec.Lease) {
			return rec.Lease, nil
		}
		err = writeOperationRecord(c, rec, map[string]string{LeaseKey: string(value)})
		if kube.IsK8sConflict(err) {
			slog.Debug("operation lease write conflict, retrying", "attempt", attempt+1)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("write operation lease: %w", err)
		}
		return nil, nil
	}
	return nil, ErrOperationRecordContended
}

// ReleaseOperationLease clears owner's lease, and nothing when the record
// holds another lease (or none). The clear is conditional like
// WriteOperationLease.
func ReleaseOperationLease(c *kube.Client, owner LeaseOwner) error {
	for range maxLeaseWriteAttempts {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return err
		}
		if !owner.owns(rec.Lease) {
			return nil
		}
		err = writeOperationRecord(c, rec, map[string]string{LeaseKey: ""})
		if kube.IsK8sConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("release operation lease: %w", err)
		}
		return nil
	}
	return ErrOperationRecordContended
}
