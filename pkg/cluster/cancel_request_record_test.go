package cluster

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A cancel request for another pod's project action is a key of its own in
// the operation ConfigMap, written only when the check allows it on the
// record as read. The action's lock entry is untouched, so its holder's
// renewal (a JSON patch testing the value it wrote last) still passes: no
// lost lock. Once the lock is gone, the next conditional write removes the
// request.
func TestCancelRequestKeyBesideTheLockAndPruned(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	live := projectLease("op-live", "boot-b", "live", "alice", now)
	live.CancelNote = "It stops before its next step."
	f.set(ProjectLockKey("live"), live)
	lockBefore := f.get(ProjectLockKey("live"))

	find := func(rec *OperationRecord) (*record.CancelRequest, error) {
		for _, l := range rec.ProjectLocks {
			if l.ID == "op-live" {
				return &record.CancelRequest{ByAdmin: true, At: "2026-11-25T10:00:00Z"}, nil
			}
		}
		return nil, record.ErrNoSuchAction
	}
	if err := RequestOperationCancel(c, "op-gone", func(*OperationRecord) (*record.CancelRequest, error) { return nil, record.ErrNoSuchAction }); !errors.Is(err, record.ErrNoSuchAction) {
		t.Fatalf("refused: %v", err)
	}
	if f.get(record.CancelRequestKey("op-gone")) != "" {
		t.Fatal("a refused request was written")
	}
	if err := RequestOperationCancel(c, "op-live", find); err != nil {
		t.Fatal(err)
	}
	if got := f.get(record.CancelRequestKey("op-live")); got != `{"byAdmin":true,"at":"2026-11-25T10:00:00Z"}` {
		t.Fatalf("request %q", got)
	}
	if f.get(ProjectLockKey("live")) != lockBefore {
		t.Fatal("the lock entry changed")
	}
	rec, err := ReadOperationRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	if r := rec.CancelRequests["op-live"]; r == nil || !r.ByAdmin {
		t.Fatalf("read back: %+v", rec.CancelRequests)
	}
	renewed := *live
	renewed.HeartbeatAt = now.UTC().Format("2006-01-02T15:04:05Z")
	lost, err := RenewOperationLocks(c, []LockRenewal{{Key: ProjectLockKey("live"), Old: lockBefore, New: leaseValue(t, &renewed)}})
	if err != nil || len(lost) != 0 {
		t.Fatalf("the holder's renewal after the request: lost %v, %v", lost, err)
	}

	// The action ended (its lock gone): the next conditional write prunes.
	f.mu.Lock()
	delete(f.data, ProjectLockKey("live"))
	f.mu.Unlock()
	if err := SaveRecipeRun(c, &types.RecipeRun{ID: "run", OperationID: "op", BootID: "boot-a"}, true); err != nil {
		t.Fatal(err)
	}
	if f.get(record.CancelRequestKey("op-live")) != "" {
		t.Fatal("the request outlived its action")
	}
}

// Releasing a lock removes the cancel request recorded for its operation.
func TestReleaseRemovesTheCancelRequest(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	mine := projectLease("op-mine", "boot-a", "mine", "alice", now)
	f.set(ProjectLockKey("mine"), mine)
	f.set(record.CancelRequestKey("op-mine"), record.CancelRequest{At: "2026-11-25T10:00:00Z"})
	if err := ReleaseOperationLock(c, ProjectLockKey("mine"), LeaseOwner{BootID: "boot-a", ID: "op-mine"}); err != nil {
		t.Fatal(err)
	}
	if f.get(ProjectLockKey("mine")) != "" || f.get(record.CancelRequestKey("op-mine")) != "" {
		t.Fatalf("left: lock %q, request %q", f.get(ProjectLockKey("mine")), f.get(record.CancelRequestKey("op-mine")))
	}
}

// The lock is released (or taken by another operation) between the cancel
// request's read and its write: the write conflicts (409), the record is
// read again, the check runs again and refuses, and no request is left.
func TestCancelRequestRecheckedAfterAConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{{"released", false}, {"taken by another operation", true}} {
		t.Run(tc.name, func(t *testing.T) {
			now := kube.Clock().Now()
			f, c := newFakeOperationConfigMap(t)
			f.set(ProjectLockKey("p"), projectLease("op-1", "boot-b", "p", "alice", now))
			changed := false
			f.beforeWrite = func(f *fakeOperationConfigMap) {
				if changed {
					return
				}
				changed = true
				delete(f.data, ProjectLockKey("p"))
				if tc.replace {
					raw, _ := json.Marshal(projectLease("op-2", "boot-b", "p", "bob", now))
					f.data[ProjectLockKey("p")] = string(raw)
				}
				f.rv++
			}
			checks := 0
			err := RequestOperationCancel(c, "op-1", func(rec *OperationRecord) (*record.CancelRequest, error) {
				checks++
				for _, l := range rec.ProjectLocks {
					if l.ID == "op-1" {
						return &record.CancelRequest{At: "2026-11-25T10:00:00Z"}, nil
					}
				}
				return nil, record.ErrNoSuchAction
			})
			if !errors.Is(err, record.ErrNoSuchAction) || checks != 2 || f.conflicts != 1 {
				t.Fatalf("err %v, checks %d, conflicts %d", err, checks, f.conflicts)
			}
			if f.get(record.CancelRequestKey("op-1")) != "" {
				t.Fatal("a request was left")
			}
		})
	}
}
