package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The lock table: which operations may run at once.
//
// There are two kinds of lock. The cluster lock (one slot) is what every
// cluster operation takes today (lockCluster, acquireRunnerLock). A
// project lock (one slot per project, by namespace UID) is what a project
// action will take (acquireProjectLock; no route takes one yet). Each lock
// is held twice: in this process's table (locks), which settles the
// requests of this process without an API call, and as an entry of the
// operation ConfigMap (cluster.AcquireOperationLock), which settles them
// across updater pods. Both apply the same rules (lockConflictFor), so a
// request is refused, never queued:
//
//   - a cluster operation while any other lock is live;
//   - a project action while the cluster lock is live, while another
//     action runs in that project, while its user runs a project action
//     anywhere (one per user, admins included), or while maxProjectLocks
//     project actions run.
//
// A live lock is one of this process's table, or an entry of another
// process whose lease is live (leaseHeldElsewhere). An entry this process
// wrote and no longer holds (its release failed) is not live: the table
// is what this process holds.

// maxProjectLocks bounds the project actions that run at once: it bounds
// the operation ConfigMap's size (each entry under 2 KiB) and the load on
// the API server.
const maxProjectLocks = 20

// Why a lock was refused (lockConflict.reason).
const (
	// conflictCluster: a cluster operation runs (for a cluster
	// operation: any other operation runs, see the holder's scope).
	conflictCluster = "cluster"
	// conflictProject: another action runs in this project.
	conflictProject = "project"
	// conflictUser: the user runs a project action already.
	conflictUser = "user"
	// conflictCap: maxProjectLocks project actions run.
	conflictCap = "cap"
)

// lockWant is the lock an operation asks for: the cluster lock (no
// projectUID) or the lock of one project.
type lockWant struct {
	projectUID string
	user       string
}

func (w lockWant) key() string {
	if w.projectUID == "" {
		return cluster.ClusterLockKey
	}
	return cluster.ProjectLockKey(w.projectUID)
}

// lockHolder is a live lock: this process's (opID, no lease) or another
// process's (its recorded lease).
type lockHolder struct {
	projectUID string // "" for the cluster lock
	user       string
	opID       string
	lease      *types.OperationLease
}

// lockConflict is the refusal of a lock: why, and the holder that refuses
// it. It is the error a lock check returns through
// cluster.AcquireOperationLock.
type lockConflict struct {
	reason string
	holder lockHolder
}

func (e *lockConflict) Error() string {
	return "the operation lock is held (" + e.reason + ")"
}

// lockConflictFor applies the rules to want and the live locks of every
// process but want's own slot. holders are in a stable order, cluster
// first (the first conflicting holder is the one named).
func lockConflictFor(want lockWant, holders []lockHolder) *lockConflict {
	if want.projectUID == "" {
		if len(holders) > 0 {
			return &lockConflict{reason: conflictCluster, holder: holders[0]}
		}
		return nil
	}
	projects := 0
	var sameProject, sameUser *lockHolder
	for i := range holders {
		h := &holders[i]
		if h.projectUID == "" {
			return &lockConflict{reason: conflictCluster, holder: *h}
		}
		projects++
		if h.projectUID == want.projectUID && sameProject == nil {
			sameProject = h
		}
		if h.user == want.user && sameUser == nil {
			sameUser = h
		}
	}
	switch {
	case sameProject != nil:
		return &lockConflict{reason: conflictProject, holder: *sameProject}
	case sameUser != nil:
		return &lockConflict{reason: conflictUser, holder: *sameUser}
	case projects >= maxProjectLocks:
		return &lockConflict{reason: conflictCap, holder: holders[0]}
	}
	return nil
}

// lockTable is this process's part of the locks: the cluster slot and the
// project slots, by namespace UID.
type lockTable struct {
	mu       sync.Mutex
	cluster  bool
	projects map[string]*projectSlot
}

type projectSlot struct {
	user string
	opID string // "" until the operation is recorded (bindProject)
}

var locks = &lockTable{}

// holdersLocked is this process's live locks but except's (the key of the
// slot being taken), cluster first, then by project UID. l.mu must be
// held.
func (l *lockTable) holdersLocked(except string) []lockHolder {
	var out []lockHolder
	if l.cluster && except != cluster.ClusterLockKey {
		out = append(out, lockHolder{opID: inflight.clusterID()})
	}
	uids := make([]string, 0, len(l.projects))
	for uid := range l.projects {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		if cluster.ProjectLockKey(uid) == except {
			continue
		}
		s := l.projects[uid]
		out = append(out, lockHolder{projectUID: uid, user: s.user, opID: s.opID})
	}
	return out
}

// take takes want's slot when the rules allow it on this process's locks,
// and returns the conflict otherwise (nothing taken).
func (l *lockTable) take(want lockWant) *lockConflict {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := lockConflictFor(want, l.holdersLocked("")); c != nil {
		return c
	}
	if want.projectUID == "" {
		l.cluster = true
		return nil
	}
	if l.projects == nil {
		l.projects = map[string]*projectSlot{}
	}
	l.projects[want.projectUID] = &projectSlot{user: want.user}
	return nil
}

// bindProject records the operation that holds the project's slot.
func (l *lockTable) bindProject(uid, opID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.projects[uid]; s != nil {
		s.opID = opID
	}
}

// projectOp is the ID of the operation that holds the project's slot.
func (l *lockTable) projectOp(uid string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.projects[uid]; s != nil {
		return s.opID
	}
	return ""
}

// release frees want's slot.
func (l *lockTable) release(want lockWant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if want.projectUID == "" {
		l.cluster = false
		return
	}
	delete(l.projects, want.projectUID)
}

// clusterHeld reports whether this process holds the cluster slot.
func (l *lockTable) clusterHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cluster
}

// lockCheck is the check of want against the operation record as read
// (cluster.AcquireOperationLock): the rules on this process's other locks
// and the live entries of other processes. A refusal is a *lockConflict.
func lockCheck(want lockWant) func(*cluster.OperationRecord) error {
	return func(rec *cluster.OperationRecord) error {
		seenProjectLocks.remember(rec)
		locks.mu.Lock()
		holders := locks.holdersLocked(want.key())
		locks.mu.Unlock()
		if leaseHeldElsewhere(rec.Lease) {
			holders = append(holders, lockHolder{user: rec.Lease.User, lease: rec.Lease})
		}
		uids := make([]string, 0, len(rec.ProjectLocks))
		for uid := range rec.ProjectLocks {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
		for _, uid := range uids {
			if l := rec.ProjectLocks[uid]; leaseHeldElsewhere(l) {
				holders = append(holders, lockHolder{projectUID: uid, user: l.User, lease: l})
			}
		}
		// The cluster lock first: it is the reason a project action is
		// refused when both apply.
		sort.SliceStable(holders, func(i, j int) bool { return holders[i].projectUID == "" && holders[j].projectUID != "" })
		if c := lockConflictFor(want, holders); c != nil {
			return c
		}
		return nil
	}
}

// refusalOf is the lockRefusal of a lock that was not taken: a conflict
// (with this process's operation, or another process's lease) or an error
// reading or writing the record.
func refusalOf(conflict *lockConflict, err error) *lockRefusal {
	switch {
	case conflict == nil:
		return &lockRefusal{err: err}
	case conflict.holder.lease != nil:
		return &lockRefusal{holder: conflict.holder.lease, reason: conflict.reason}
	}
	return &lockRefusal{busy: true, reason: conflict.reason, opID: conflict.holder.opID}
}

// acquireLock takes want's slot in this process's table, records the
// operation start returns as the one holding it, then takes want's entry
// in the operation record for that operation with its bookkeeper (the
// ServiceAccount's client). cancelOp is what the heartbeat stops the
// operation with when the entry is lost.
//
// start runs only once the slot is held. It may return nil (nothing to
// track, as for a response that is not a statusWriter in tests), and the
// operation may have no bookkeeper (no ServiceAccount token): then no entry
// is written, as before.
//
// It returns nil when the lock is held. On a refusal nothing is held: the
// operation is forgotten (it never began) and the slot is free again.
func acquireLock(want lockWant, start func() *Operation, cancelOp context.CancelCauseFunc) *lockRefusal {
	if c := locks.take(want); c != nil {
		return refusalOf(c, nil)
	}
	op := start()
	if op != nil && want.projectUID != "" {
		locks.bindProject(want.projectUID, op.ID)
	}
	if op == nil {
		return nil
	}
	if op.bookkeeper == nil {
		confirmLock(op.ID)
		return nil
	}
	err := leases.acquire(op.bookkeeper, want.key(), op, cancelOp, lockCheck(want))
	if err == nil {
		confirmLock(op.ID)
		return nil
	}
	inflight.finishID(op.ID)
	locks.release(want)
	var conflict *lockConflict
	if errors.As(err, &conflict) {
		return refusalOf(conflict, nil)
	}
	return refusalOf(nil, err)
}

// confirmLock marks the operation as holding its lock: from now on GET
// /api/operation lists it (a pending acquisition is not a holder).
func confirmLock(opID string) {
	inflight.update(opID, func(op *Operation) { op.confirmed = true })
}

// beginProjectAction marks the project action opID as begun, with what it
// acts on, and records Begun on its lock entry at once, so the entry
// reports the action as interrupted should this process die from then on.
// It runs before the action's first change; an error means the action
// must not change anything (its lock may be lost).
func beginProjectAction(opID, target string) error {
	var uid string
	inflight.update(opID, func(op *Operation) {
		op.Target = truncateText(target, maxOperationTargetBytes)
		op.begun = true
		uid = op.ProjectUID
	})
	if uid == "" {
		return fmt.Errorf("operation %q is not a running project action", opID)
	}
	return leases.renewNow(cluster.ProjectLockKey(uid))
}

// acquireProjectLock is acquireLock of the project with this namespace
// UID for user. The caller must call releaseProjectLock when the action
// ends.
func acquireProjectLock(uid, user string, start func() *Operation, cancelOp context.CancelCauseFunc) *lockRefusal {
	return acquireLock(lockWant{projectUID: uid, user: user}, start, cancelOp)
}

// releaseProjectLock ends the project action that holds the project's
// lock: it stops renewing its entry, forgets the operation, removes the
// entry and frees the slot.
func releaseProjectLock(uid string) {
	key := cluster.ProjectLockKey(uid)
	owner := leases.stop(key)
	_, bookkeeper := inflight.finishID(locks.projectOp(uid))
	if owner != nil && bookkeeper != nil {
		leases.release(bookkeeper, key, *owner)
	}
	locks.release(lockWant{projectUID: uid})
}
