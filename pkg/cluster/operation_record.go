package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operation ConfigMap (operationConfigMapName in the updater's own
// namespace) records three independent values, one data key each:
//   - operationKey: the running-operation marker, written by server-side
//     apply when an operation begins (SaveOperationMarker). A marker of a
//     process that is gone reports an interrupted operation.
//   - lastCompletedKey: the most recent finished operation.
//   - leaseKey: the cross-pod operation lock (WriteOperationLease).
//
// The marker's field manager never owns the other two keys, so a marker
// write keeps them (an applier only removes fields it owned:
// https://kubernetes.io/docs/reference/using-api/server-side-apply/#field-management).
// The other writes are JSON merge patches of their own keys. The updater's
// Role allows get, create and patch on this ConfigMap, nothing else.
const (
	operationKey     = "operation"
	lastCompletedKey = "lastCompleted"
	leaseKey         = "lock"
)

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
func ReadOperationRecord(c *Client) (*OperationRecord, error) {
	body, _, err := c.get(namespacedPath("v1", "configmaps", getActivityNamespace(), operationConfigMapName))
	if IsK8sError(err, 404) {
		return &OperationRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get operation record: %w", err)
	}
	var cm struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse operation record: %w", err)
	}
	return &OperationRecord{
		Marker:          decodeRecordValue[types.OperationMarker](cm.Data, operationKey),
		LastCompleted:   decodeRecordValue[types.CompletedOperation](cm.Data, lastCompletedKey),
		Lease:           decodeRecordValue[types.OperationLease](cm.Data, leaseKey),
		resourceVersion: cm.Metadata.ResourceVersion,
	}, nil
}

func decodeRecordValue[T any](data map[string]string, key string) *T {
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

// GetOperationState returns the running-operation marker and the last
// completed operation; either is nil when none is recorded.
func GetOperationState(c *Client) (*types.OperationMarker, *types.CompletedOperation, error) {
	rec, err := ReadOperationRecord(c)
	if err != nil {
		return nil, nil, err
	}
	return rec.Marker, rec.LastCompleted, nil
}

// SaveOperationMarker records the running operation in a ConfigMap, or
// clears the record when marker is nil.
func SaveOperationMarker(c *Client, marker *types.OperationMarker) error {
	value := ""
	if marker != nil {
		data, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		value = string(data)
	}
	ns := getActivityNamespace()
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": operationConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{operationKey: value},
	}
	if _, _, err := c.apply(namespacedPath("v1", "configmaps", ns, operationConfigMapName), cm); err != nil {
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
func SaveCompletedOperation(c *Client, done *types.CompletedOperation, owner *LeaseOwner) error {
	value, err := json.Marshal(done)
	if err != nil {
		return err
	}
	if owner == nil {
		data := map[string]string{operationKey: "", lastCompletedKey: string(value)}
		if err := patchOperationRecord(c, "", data); err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		return nil
	}
	for attempt := 0; attempt < maxLeaseWriteAttempts; attempt++ {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return fmt.Errorf("save completed operation: %w", err)
		}
		data := map[string]string{}
		// A delayed retry (this pod's write failed and is repeated in the
		// background) must not replace a completion another pod recorded
		// since.
		if rec.LastCompleted == nil || !finishedAfter(rec.LastCompleted, done) {
			data[lastCompletedKey] = string(value)
		}
		if rec.Marker != nil && (rec.Marker.BootID == "" || rec.Marker.BootID == owner.BootID) {
			data[operationKey] = ""
		}
		if owner.owns(rec.Lease) {
			data[leaseKey] = ""
		}
		if len(data) == 0 {
			return nil
		}
		err = writeOperationRecord(c, rec, data)
		if IsK8sError(err, 409) {
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
func writeOperationRecord(c *Client, rec *OperationRecord, data map[string]string) error {
	if rec.resourceVersion == "" {
		ns := getActivityNamespace()
		cm, err := json.Marshal(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": operationConfigMapName, "namespace": ns},
			"data":       data,
		})
		if err != nil {
			return err
		}
		_, _, err = c.post(namespacedPath("v1", "configmaps", ns, ""), cm)
		return err
	}
	return patchOperationRecord(c, rec.resourceVersion, data)
}

// patchOperationRecord merge-patches data into the operation ConfigMap. A
// resourceVersion in the patch makes the API server refuse it with 409
// Conflict when the object changed since it was read
// (https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions).
// Without one, a missing ConfigMap is created.
func patchOperationRecord(c *Client, resourceVersion string, data map[string]string) error {
	body := map[string]interface{}{"data": data}
	if resourceVersion != "" {
		body["metadata"] = map[string]interface{}{"resourceVersion": resourceVersion}
	}
	patch, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	_, _, err = c.patch(namespacedPath("v1", "configmaps", ns, operationConfigMapName), patch)
	if resourceVersion == "" && IsK8sError(err, 404) {
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
	beat, err := time.Parse(time.RFC3339, l.HeartbeatAt)
	if err != nil {
		return false
	}
	return now.Sub(beat) < ttl
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
func WriteOperationLease(c *Client, lease *types.OperationLease, heldElsewhere func(*types.OperationLease) bool) (*types.OperationLease, error) {
	value, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxLeaseWriteAttempts; attempt++ {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return nil, err
		}
		if rec.Lease != nil && heldElsewhere(rec.Lease) {
			return rec.Lease, nil
		}
		err = writeOperationRecord(c, rec, map[string]string{leaseKey: string(value)})
		if IsK8sError(err, 409) {
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
func ReleaseOperationLease(c *Client, owner LeaseOwner) error {
	for attempt := 0; attempt < maxLeaseWriteAttempts; attempt++ {
		rec, err := ReadOperationRecord(c)
		if err != nil {
			return err
		}
		if !owner.owns(rec.Lease) {
			return nil
		}
		err = writeOperationRecord(c, rec, map[string]string{leaseKey: ""})
		if IsK8sError(err, 409) {
			continue
		}
		if err != nil {
			return fmt.Errorf("release operation lease: %w", err)
		}
		return nil
	}
	return ErrOperationRecordContended
}
