package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Cancel requests across updater pods (the non-admin design, section 5.2).
//
// A cancel request for a project action that another updater pod runs is
// a key of its own in the operation ConfigMap, "cancelRequest.<operation
// ID>", never a change of the action's lock entry: the holder renews its
// entry with a JSON patch that tests the exact value it wrote last
// (RenewOperationLocks), so a changed entry would read as a lost lock. The
// pod that takes the request writes the key conditionally on the record it
// checked (RequestOperationCancel); the holder reads it at its heartbeat
// (pkg/api) and lets the action stop at its next safe point. A key whose
// action no longer holds a live lock is removed by the next conditional
// write (prunableLockKeys). The existing Role allows all of it: get and
// patch on the operation ConfigMap.

// CancelRequestPrefix starts the key of every cancel request.
const CancelRequestPrefix = "cancelRequest."

// CancelRequestKey is the data key of the cancel request for the
// operation with this ID.
func CancelRequestKey(opID string) string { return CancelRequestPrefix + opID }

// cancelRequestOp is the operation ID of a cancel request's key.
func cancelRequestOp(key string) (string, bool) {
	id, ok := strings.CutPrefix(key, CancelRequestPrefix)
	return id, ok && id != ""
}

// CancelRequest is a recorded cancel request: whether a cluster-admin who
// did not start the action asked (the starter's activity entry says
// "Cancelled by a cluster-admin" then), and when (RFC 3339).
type CancelRequest struct {
	ByAdmin bool   `json:"byAdmin,omitempty"`
	At      string `json:"at"`
}

// readCancelRequests is the cancel requests of the ConfigMap's data, by
// operation ID; an entry that cannot be parsed is left out.
func readCancelRequests(data map[string]string) map[string]*CancelRequest {
	var out map[string]*CancelRequest
	for key := range data {
		id, ok := cancelRequestOp(key)
		if !ok {
			continue
		}
		if r := DecodeValue[CancelRequest](data, key); r != nil {
			if out == nil {
				out = map[string]*CancelRequest{}
			}
			out[id] = r
		}
	}
	return out
}

// prunableCancelKeys are the cancel request keys of rec whose operation
// holds no live project lock: it ended, or its pod is gone.
func prunableCancelKeys(rec *OperationRecord, now time.Time) []string {
	live := map[string]bool{}
	for _, l := range rec.ProjectLocks {
		if LeaseLive(l, now, LockTTL) {
			live[l.ID] = true
		}
	}
	var keys []string
	for key := range rec.values {
		if id, ok := cancelRequestOp(key); ok && !live[id] {
			keys = append(keys, key)
		}
	}
	return keys
}

// ErrNoSuchAction: the record holds no project action the cancel may be
// asked for (RequestOperationCancel's check).
var ErrNoSuchAction = errors.New("no such running project action")

// RequestOperationCancel records a cancel request for the project action
// opID that another updater pod runs. check decides on the record as read:
// it returns the request to write, or an error (ErrNoSuchAction when there
// is no such action the caller may cancel), and nothing is written. The
// write is conditional on that read (a conflict reads again and checks
// again), so the request is written only while the lock check saw is
// still there. A request already recorded is kept.
func RequestOperationCancel(c *kube.Client, opID string, check func(*OperationRecord) (*CancelRequest, error)) error {
	var refused error
	err := operationWriter.do(c.Context(), func() error {
		return updateOperationRecord(c, func(rec *OperationRecord) (map[string]any, error) {
			req, err := check(rec)
			if err != nil {
				refused = err
				return nil, err
			}
			if rec.CancelRequests[opID] != nil {
				return nil, nil
			}
			raw, err := json.Marshal(req)
			if err != nil {
				return nil, err
			}
			return map[string]any{CancelRequestKey(opID): string(raw)}, nil
		})
	})
	switch {
	case refused != nil:
		return refused
	case err == nil, errors.Is(err, ErrOperationRecordContended):
		return err
	}
	return fmt.Errorf("write cancel request: %w", err)
}
