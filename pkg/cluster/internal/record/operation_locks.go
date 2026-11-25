package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operation locks: entries of the operation ConfigMap, one data key
// each, every value a types.OperationLease.
//
//   - LeaseKey ("lock") is the cluster lock: the running cluster operation,
//     in the format every earlier version writes and reads (a rolling
//     upgrade keeps working: both sides read the other's entry), released
//     to "" as before.
//   - ProjectLockKey(uid) ("projectLock.<namespace UID>") is the lock of
//     one project: by UID, not name, so a project deleted and made again
//     is a new lock. It carries Project, ProjectUID and Begun, and is
//     removed when released.
//
// The rules (who may take which lock while others are live) are the
// caller's: AcquireOperationLock asks its check on the record as read, and
// writes only when it allows it, conditional on that read. The holder
// renews all its entries in one JSON patch (RenewOperationLocks) that
// tests each entry's value before replacing it, without a
// resourceVersion: renewals never conflict with writes of other keys.
//
// Expired project locks are removed by every conditional write
// (prunableLockKeys): an entry whose heartbeat is LockTTL old, or, when it
// had begun (an interrupted project action), whose start is
// InterruptedLockMaxAge old, or that is not among the
// MaxInterruptedLocks newest interrupted ones. The renewals do not: they do not read the
// record, and a removal guarded by a test of another pod's entry could
// make the whole patch fail.

// ProjectLockPrefix starts the key of every project lock.
const ProjectLockPrefix = "projectLock."

// ProjectLockKey is the data key of the lock of the project with this
// namespace UID.
func ProjectLockKey(uid string) string { return ProjectLockPrefix + uid }

// projectLockUID is the namespace UID of a project lock's key.
func projectLockUID(key string) (string, bool) {
	uid, ok := strings.CutPrefix(key, ProjectLockPrefix)
	return uid, ok && uid != ""
}

// Lock aging. LockTTL is the lease TTL of every lock (pkg/api counts a
// lock live for as long, and its holder stops before). An interrupted
// project action (its entry expired after it began) is shown for
// InterruptedLockMaxAge from its start, as an interrupted cluster
// operation's marker is.
const (
	LockTTL               = 45 * time.Second
	InterruptedLockMaxAge = 24 * time.Hour
)

// MaxInterruptedLocks bounds the interrupted project actions kept (the
// newest by start): with each entry under 2 KiB (its target and message
// are truncated), they stay under 40 KiB of the ConfigMap's 1 MiB however
// many projects saw an action interrupted within InterruptedLockMaxAge.
const MaxInterruptedLocks = 20

// prunableLockKeys are the project lock keys of rec a write removes, as of
// now: unreadable entries, expired ones that had not begun, interrupted
// ones (expired after they began) older than InterruptedLockMaxAge, and
// the interrupted ones beyond the MaxInterruptedLocks newest. Sorted, so a
// write's body does not depend on the map order.
func prunableLockKeys(rec *OperationRecord, now time.Time) []string {
	var keys []string
	type kept struct {
		key     string
		started time.Time
	}
	var interrupted []kept
	for key := range rec.values {
		if _, ok := projectLockUID(key); !ok {
			continue
		}
		l := DecodeValue[types.OperationLease](rec.values, key)
		switch {
		case l == nil:
		case LeaseLive(l, now, LockTTL):
			continue
		case l.Begun:
			if started, err := time.Parse(time.RFC3339, l.StartedAt); err == nil && now.Sub(started) < InterruptedLockMaxAge {
				interrupted = append(interrupted, kept{key, started})
				continue
			}
		}
		keys = append(keys, key)
	}
	if len(interrupted) > MaxInterruptedLocks {
		sort.Slice(interrupted, func(i, j int) bool {
			if !interrupted[i].started.Equal(interrupted[j].started) {
				return interrupted[i].started.After(interrupted[j].started)
			}
			return interrupted[i].key < interrupted[j].key
		})
		for _, k := range interrupted[MaxInterruptedLocks:] {
			keys = append(keys, k.key)
		}
	}
	sort.Strings(keys)
	return keys
}

// AcquireOperationLock takes the lock entry key (LeaseKey, or a
// ProjectLockKey) by writing value (a types.OperationLease as JSON) to it,
// when check allows it on the record as read (nil). A refusal of check is
// returned as it is, and nothing is written. The write is conditional on
// the resourceVersion of that read, so two processes that both find the
// lock free cannot both take it: the second write fails with 409 and is
// retried from a fresh read (and check runs again), after a jittered
// backoff, at most maxLockWriteAttempts times; then it returns
// ErrOperationRecordContended. Other errors (the read, the write) are
// wrapped.
func AcquireOperationLock(c *kube.Client, key, value string, check func(*OperationRecord) error) error {
	var refused error
	err := operationWriter.do(c.Context(), func() error {
		return updateOperationRecord(c, func(rec *OperationRecord) (map[string]any, error) {
			if err := check(rec); err != nil {
				refused = err
				return nil, err
			}
			return map[string]any{key: value}, nil
		})
	})
	switch {
	case refused != nil:
		return refused
	case err == nil, errors.Is(err, ErrOperationRecordContended):
		return err
	}
	return fmt.Errorf("write operation lock: %w", err)
}

// LockRenewal is the renewal of one lock entry this process holds: Old is
// the value it wrote last, New the value with the new heartbeat.
type LockRenewal struct {
	Key string
	Old string
	New string
}

// RenewOperationLocks renews the entries in one JSON patch (RFC 6902) that
// first tests each entry's value against Old and then replaces it with
// New. The patch carries no resourceVersion, so it never conflicts with
// a write of other keys of the ConfigMap; it applies whole or not at all.
// It does not wait for this process's other writes (operationWriter): a
// long recipe run or hard reset record write must not hold up the
// heartbeat. That is safe because the patch tests what it replaces: a
// conditional write of this process that read the record before the
// renewal fails with 409 and reads it again.
//
// The API server answers a failed test with a generic 422 that does not
// say which test failed (k8s.io/apiserver applyJSPatch drops the
// json-patch message; the DataScienceCluster writes, dsc_manager.go, meet
// the same): after a 422 (or a 404, the ConfigMap gone) the record is read
// again, and every entry whose value is no longer Old is lost: lost maps
// its key to the lease recorded there now (nil when there is none or it
// cannot be parsed). The others are renewed again in a patch of their
// own. When no entry changed, no test failed: the 422 is the API server's
// own refusal, returned as the error (nothing was renewed). Other errors
// are returned as they are, with the entries found lost so far.
func RenewOperationLocks(c *kube.Client, renewals []LockRenewal) (lost map[string]*types.OperationLease, err error) {
	err = func() error {
		pending := renewals
		for len(pending) > 0 {
			perr := patchLockValues(c, pending)
			if perr == nil {
				return nil
			}
			if !kube.IsK8sError(perr, http.StatusUnprocessableEntity) && !kube.IsK8sNotFound(perr) {
				return fmt.Errorf("renew the operation locks: %w", perr)
			}
			rec, rerr := ReadOperationRecord(c)
			if rerr != nil {
				return fmt.Errorf("renew the operation locks: %w; reading them again: %w", perr, rerr)
			}
			var still []LockRenewal
			for _, r := range pending {
				if rec.Value(r.Key) == r.Old {
					still = append(still, r)
					continue
				}
				if lost == nil {
					lost = map[string]*types.OperationLease{}
				}
				lost[r.Key] = DecodeValue[types.OperationLease](rec.values, r.Key)
			}
			if len(still) == len(pending) {
				return fmt.Errorf("renew the operation locks: %w", perr)
			}
			pending = still
		}
		return nil
	}()
	return lost, err
}

// patchLockValues sends the JSON patch of RenewOperationLocks.
func patchLockValues(c *kube.Client, renewals []LockRenewal) error {
	ops := make([]kube.JSONPatchOp, 0, 2*len(renewals))
	for _, r := range renewals {
		path := kube.PointerOf([]string{"data", r.Key})
		oldValue, err := json.Marshal(r.Old)
		if err != nil {
			return err
		}
		newValue, err := json.Marshal(r.New)
		if err != nil {
			return err
		}
		ops = append(ops, kube.JSONPatchOp{Op: "test", Path: path, Value: oldValue}, kube.JSONPatchOp{Op: "replace", Path: path, Value: newValue})
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, _, err = c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), OperationConfigMapName), "application/json-patch+json", body, nil)
	return err
}

// ReleaseOperationLock clears the lock entry key while owner holds it, and
// nothing otherwise (another process's entry, or none): the cluster lock
// is set to "" as earlier versions do, a project lock is removed. It reads
// the entry and checks its owner rather than testing the value last
// written: a renewal whose answer was lost (a timeout) may have changed
// the value, and the release must remove it all the same. The write is
// conditional like AcquireOperationLock's.
func ReleaseOperationLock(c *kube.Client, key string, owner LeaseOwner) error {
	var cleared any
	if key == LeaseKey {
		cleared = ""
	}
	err := operationWriter.do(c.Context(), func() error {
		return updateOperationRecord(c, func(rec *OperationRecord) (map[string]any, error) {
			if !owner.owns(DecodeValue[types.OperationLease](rec.values, key)) {
				return nil, nil
			}
			return map[string]any{key: cleared}, nil
		})
	})
	switch {
	case err == nil, errors.Is(err, ErrOperationRecordContended):
		return err
	}
	return fmt.Errorf("release operation lock: %w", err)
}

// contextOf is c's context, or the background context when it has none.
func contextOf(c *kube.Client) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
