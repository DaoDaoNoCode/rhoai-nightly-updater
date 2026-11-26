package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Cross-pod operation locks.
//
// This process's lock table (operation_locks.go) only covers this process.
// The Deployment runs one replica with the Recreate strategy, so rollouts
// never overlap, but a deleted or evicted pod (node drain, cluster upgrade,
// autoscaler) is replaced at once while the old pod still drains its
// running operation for up to cluster.ShutdownDrainTimeout. Both pods would
// then delete and recreate the same Subscription, CatalogSource and CSV.
//
// So every operation that takes a slot of the table also takes its entry
// in the operation ConfigMap (the cluster lock, key "lock", next to the
// marker and lastCompleted; a project lock, key "projectLock.<uid>"; the
// existing Role already allows get, create and patch on it) and renews it
// while it runs, also during the shutdown drain. A process that finds a
// live entry of another process refuses with 409 cluster_busy and reports
// that operation as running (remote) instead of interrupted.
//
// One heartbeat per process renews every entry it holds in one JSON patch
// per interval (cluster.RenewOperationLocks), which tests each entry's
// value before replacing it: renewals do not conflict with other writes.
//
// Error policy: fail closed. An operation starts only after its entry was
// written. A live entry of another process refuses with 409 cluster_busy
// and names that operation. An entry that cannot be read or written (API
// errors, timeouts, or write conflicts that persist through every retry)
// refuses with 503 lock_unavailable and Retry-After: without it the tool
// cannot tell whether another updater pod is changing the cluster, and two
// concurrent Updates or Reinstalls would both delete and recreate the
// Subscription, CatalogSource and CSV. Nothing was changed then; the user
// retries. While the operation runs, losing its entry stops it (and only
// it: the heartbeat's other entries are not affected).
var (
	acquireOperationLock = cluster.AcquireOperationLock
	renewOperationLocks  = cluster.RenewOperationLocks
	releaseOperationLock = cluster.ReleaseOperationLock
)

// Lease timing. The holder renews the heartbeat every
// leaseHeartbeatInterval; a lease whose heartbeat is older than leaseTTL is
// free. The holder stops its operation leaseTTL-leaseHeartbeatInterval (35s)
// after its last successful heartbeat (leaseLossAfter), so three renewals in
// a row may fail. The remaining interval (10s) is the whole safety margin: it
// covers the operation stopping and the clock skew between the holder and a
// pod that takes over, since each pod compares HeartbeatAt with its own clock
// (cluster.LeaseLive). OpenShift nodes keep their clocks synchronised with
// chronyd, far inside that margin. The TTL is also the longest a crashed
// pod's operation blocks a new one. A pod restart takes longer than that
// anyway (the replacement pod is scheduled, pulls the image and passes its
// readiness probe first). The record removes expired project locks after
// the same TTL (cluster.LockTTL).
var (
	leaseHeartbeatInterval = 10 * time.Second
	leaseTTL               = cluster.LockTTL
)

// leaseWriteTimeout bounds one lease write (taking, renewing or releasing
// it), its conflict retries included. A release of an operation that never
// began runs where the marker clear would (cluster.MarkerClearTimeout), so
// it must not take longer.
const leaseWriteTimeout = 5 * time.Second

// leaseHeldElsewhere reports whether l is the live lease of another
// process. A lease of an earlier container of this same pod is not: the
// kubelet starts a container again only after the previous one exited, so
// that process is gone (the pod name is in HOSTNAME; DEV_MODE processes on
// one machine share a host name, so this shortcut is in-cluster only).
func leaseHeldElsewhere(l *types.OperationLease) bool {
	if l == nil || l.BootID == "" || l.BootID == bootID {
		return false
	}
	if pod := os.Getenv("HOSTNAME"); pod != "" && l.Pod == pod && !Dev().Mode {
		return false
	}
	return cluster.LeaseLive(l, clk.Now(), leaseTTL)
}

// leaseFor is the lease that describes op now. Only a project action's
// carries its project and whether it began: the cluster lock keeps the
// format earlier versions read.
func leaseFor(op *Operation, now time.Time) *types.OperationLease {
	l := &types.OperationLease{
		ID: op.ID, BootID: bootID, Pod: op.Pod, User: op.User, Type: op.Type, Label: op.Label, Target: op.Target,
		StartedAt:   op.StartedAt.UTC().Format(time.RFC3339),
		HeartbeatAt: now.UTC().Format(time.RFC3339),
		Step:        op.Step, StepStatus: op.StepStatus,
		Message: truncateText(op.Message, maxOperationMessageBytes),
	}
	if op.ProjectUID != "" {
		l.Project, l.ProjectUID, l.Begun, l.CancelNote = op.Project, op.ProjectUID, op.begun, op.cancelNote
	}
	return l
}

// lockValue is l as it is recorded: the value a renewal tests.
func lockValue(l *types.OperationLease) string {
	raw, _ := json.Marshal(l) // a struct of strings and a bool always encodes
	return string(raw)
}

// remoteOperation describes the operation of another process's lease.
func remoteOperation(l *types.OperationLease) *Operation {
	started, _ := time.Parse(time.RFC3339, l.StartedAt)
	beat, _ := time.Parse(time.RFC3339, l.HeartbeatAt)
	return &Operation{
		ID: l.ID, Type: l.Type, Label: l.Label, User: l.User, Target: l.Target,
		StartedAt: started, UpdatedAt: beat, Step: l.Step, StepStatus: l.StepStatus, Message: l.Message,
		Pod: l.Pod, Remote: true, Project: l.Project, ProjectUID: l.ProjectUID,
		// A cancel request for it is taken here and recorded for its pod
		// (operation_cancel.go).
		Cancellable: l.CancelNote != "", CancelNote: l.CancelNote,
	}
}

// heldLease is one entry this process holds in the operation record.
type heldLease struct {
	key   string
	uid   string // the project's namespace UID, "" for the cluster lock
	opID  string
	owner cluster.LeaseOwner
	// value is the value written last: what the next renewal tests.
	value string
	// renewed is the heartbeat of the last successful write (the instant
	// other pods compare with leaseTTL).
	renewed time.Time
	// lost ends leaseLossAfter after renewed; cancelLost detaches its
	// watcher and ends it, and reports false when it had ended first.
	lost       context.Context
	cancelLost func() bool
	// stopped ends when the entry is stopped (stop): not a loss.
	stopped context.Context
	stop    context.CancelFunc
	// gone: the entry was lost; it is no longer renewed.
	gone     bool
	cancelOp context.CancelCauseFunc
	client   *cluster.Client
	// fresh, when not nil, makes the client of each renewal with the
	// ServiceAccount token as it is then (it rotates); otherwise client is
	// used.
	fresh func(context.Context) *cluster.Client
	// check takes the entry again when a renewal finds it free (see
	// renewOnce).
	check    func(*cluster.OperationRecord) error
	loseOnce sync.Once
}

// leaseKeeper holds this process's entries while their operations run and
// renews them in the background: one heartbeat for all of them.
type leaseKeeper struct {
	mu   sync.Mutex
	held map[string]*heldLease
	// The running heartbeat: running is false once it returned (or is about
	// to); cancel stops it, done is closed when it returned. done stays
	// set until the last entry is stopped.
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	// The heartbeat's clock and loss threshold, taken when it started.
	clock     clock.Clock
	lossAfter time.Duration
	// wake tells the heartbeat that an entry was lost between ticks.
	wake chan struct{}
	// round serialises the renewals of a heartbeat tick and the immediate
	// ones (renewNow): both test the value written last.
	round sync.Mutex
}

// errEntryReleased: the entry a recovery would take again was stopped
// (released, or taken by the next action) meanwhile; nothing is written.
var errEntryReleased = errors.New("the operation lock entry was released")

var leases = &leaseKeeper{}

// acquire takes the entry key for op: written when check, given the record
// as read, allows it. It returns check's refusal (a *lockConflict), or the
// error that kept it from reading or writing the record; either way the
// caller refuses the operation (see the error policy). On success the
// heartbeat renews the entry until stop; when the entry is lost it stops
// the operation through cancelOp.
func (k *leaseKeeper) acquire(c *cluster.Client, key string, op *Operation, cancelOp context.CancelCauseFunc, check func(*cluster.OperationRecord) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWriteTimeout)
	defer cancel()
	lease := leaseFor(op, clk.Now())
	value := lockValue(lease)
	if err := acquireOperationLock(c.WithContext(ctx), key, value, check); err != nil {
		return err
	}
	renewed, _ := cluster.LeaseHeartbeat(lease)
	k.add(&heldLease{
		key: key, uid: op.ProjectUID, opID: op.ID, owner: cluster.LeaseOwner{BootID: bootID, ID: op.ID}, value: value, renewed: renewed,
		cancelOp: cancelOp, client: c, fresh: op.freshBookkeeper, check: check,
	})
	return nil
}

// leaseLossAfter is how long after the heartbeat of its last successful
// write the holder counts its lease as lost: one interval before the TTL,
// since another pod may take an expired lease over.
func leaseLossAfter() time.Duration {
	return leaseTTL - leaseHeartbeatInterval
}

// add holds h and starts the heartbeat unless it runs. The heartbeat's
// ticker starts here, so the first renewal of the first entry is due one
// interval after it was taken; an entry taken while the heartbeat runs is
// renewed at its next tick, sooner.
func (k *leaseKeeper) add(h *heldLease) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.running {
		if k.cancel != nil {
			k.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		k.running, k.cancel, k.done = true, cancel, done
		k.clock, k.lossAfter = clk, leaseLossAfter()
		if k.wake == nil {
			k.wake = make(chan struct{}, 1)
		}
		ticker := k.clock.NewTicker(leaseHeartbeatInterval)
		go k.heartbeat(ctx, k.wake, ticker, done)
	}
	if k.held == nil {
		k.held = map[string]*heldLease{}
	}
	h.stopped, h.stop = context.WithCancel(context.Background())
	k.held[h.key] = h
	k.watchLocked(h)
}

// watchLocked sets h's deadline: leaseLossAfter after h.renewed, on the
// heartbeat's clock. Checking only when a renewal returns would stop up to
// an interval plus a write timeout late, as late as the instant the lease
// expires. So the deadline stops the operation by itself (its watcher),
// even while a renewal's write hangs, and it also ends that write (the
// write's context derives from it). A deadline replaced after a successful
// renewal is detached from its watcher before it is cancelled, so that
// cancel is not a loss. k.mu must be held.
func (k *leaseKeeper) watchLocked(h *heldLease) {
	heartbeat := h.renewed
	lost, cancelLost := k.clock.WithDeadline(h.stopped, heartbeat.Add(k.lossAfter))
	stopped := h.stopped
	unwatch := context.AfterFunc(lost, func() {
		if stopped.Err() == nil { // not the entry's own stop
			k.expired(h, heartbeat, nil)
		}
	})
	h.lost = lost
	h.cancelLost = func() bool {
		watched := unwatch()
		cancelLost()
		return watched
	}
}

// lose stops h's operation once: the entry was lost. Never called with
// k.mu held.
func (k *leaseKeeper) lose(h *heldLease, why string, attrs ...any) {
	h.loseOnce.Do(func() {
		k.mu.Lock()
		h.gone = true
		wake := k.wake
		k.mu.Unlock()
		slog.Error("operation stopped: "+why, append([]any{"operation", h.opID}, attrs...)...)
		inflight.update(h.opID, func(op *Operation) { op.lockLost = true })
		if h.cancelOp != nil {
			h.cancelOp(cluster.ErrOperationLockLost)
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
}

func (k *leaseKeeper) expired(h *heldLease, lastHeartbeat time.Time, err error) {
	k.lose(h, "the cross-pod operation lock could not be renewed and may have expired", "lastHeartbeat", lastHeartbeat, "error", err)
}

// heartbeat renews the held entries every interval until it is stopped
// (stop of the last entry) or no entry is left to renew (they were lost).
// It does not depend on any request: it keeps running while the server
// drains operations at shutdown.
func (k *leaseKeeper) heartbeat(ctx context.Context, wake <-chan struct{}, ticker clock.Ticker, done chan struct{}) {
	defer close(done)
	defer ticker.Stop()
	for {
		tick := false
		select {
		case <-ctx.Done():
		case <-wake:
		case <-ticker.C():
			tick = true
		}
		if ctx.Err() != nil || k.idle() {
			return
		}
		if tick {
			k.renewOnce(ctx)
			if ctx.Err() != nil || k.idle() {
				return
			}
		}
	}
}

// idle reports whether no held entry is left to renew, and then marks the
// heartbeat as no longer running (the caller returns).
func (k *leaseKeeper) idle() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, h := range k.held {
		if !h.gone {
			return false
		}
	}
	k.running = false
	return true
}

// renewal is one entry of a renewal round.
type renewal struct {
	h     *heldLease
	lease *types.OperationLease
	value string
}

// renewOnce renews every held entry that is not lost, with its
// operation's latest step, in one write (cluster.RenewOperationLocks).
//
// An entry is lost when the renewal finds another process's live entry in
// its place, or when leaseLossAfter has passed since its last successful
// heartbeat without another success: at that instant (its watcher),
// between renewals or during one, so the operation stops one interval
// before another pod may take the entry over. Its operation is then
// stopped with cluster.ErrOperationLockLost: its context ends, and no
// cleanup or restore runs (cluster.postOperationContext), because the
// other pod may be changing the same objects. A single failed renewal
// does not, and the other entries' operations go on.
//
// An entry the renewal finds free, or holding a lease that is not another
// process's live one (this process's own value from a renewal whose answer
// was lost, an expired lease, the key removed by hand), is taken again
// (recover), as a renewal always did before the renewals were tested JSON
// patches: it is lost only when that is refused. The results that need no
// write are applied first; each recovery then writes with a context of its
// own, bounded by its own deadline (a successful renewal ends the batch's).
//
// At most one recovery runs per round, after the round's write and
// outside k.round: a recovery is a write of its own (up to
// leaseWriteTimeout), and it must not hold up the next tick's renewals of
// healthy entries nor an immediate renewal (renewNow) of another entry.
// The other entries to recover are recovered in the next rounds (their
// renewals fail their tests again; their deadlines still apply).
func (k *leaseKeeper) renewOnce(ctx context.Context) {
	recoveries, renewedAll := k.renewBatch(ctx)
	if len(recoveries) == 0 {
		// Cancel requests of other pods are read only in a round whose
		// renewals all went through: never ahead of, nor instead of,
		// keeping the locks.
		if renewedAll && ctx.Err() == nil {
			k.pickUpCancels(ctx)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	if len(recoveries) > 1 {
		slog.Info("operation locks to take again; one per heartbeat round", "now", recoveries[0].h.key, "later", len(recoveries)-1)
	}
	k.recover(ctx, recoveries[0])
}

// pickUpCancels reads the operation record for the cancel requests another
// updater pod recorded for this process's cancellable project actions
// (operation_cancel.go), once per heartbeat while one runs and was not
// cancelled yet, and marks those actions as requestCancel does. It only
// reads, after the round's renewals, which it does not change; a failed
// read is tried again at the next tick.
func (k *leaseKeeper) pickUpCancels(ctx context.Context) {
	k.mu.Lock()
	var held []*heldLease
	for _, h := range k.held {
		if !h.gone && h.uid != "" {
			held = append(held, h)
		}
	}
	k.mu.Unlock()
	var waiting []*heldLease
	for _, h := range held {
		if inflight.awaitingCancel(h.opID) {
			waiting = append(waiting, h)
		}
	}
	if len(waiting) == 0 || ctx.Err() != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, leaseWriteTimeout)
	defer cancel()
	c := waiting[0].client.WithContext(rctx)
	if fresh := waiting[0].fresh; fresh != nil {
		c = fresh(rctx)
	}
	rec, err := readOperationState(c)
	if err != nil || rec == nil {
		slog.Debug("could not read the operation record for cancel requests", "error", err)
		return
	}
	for _, h := range waiting {
		if r := rec.CancelRequests[h.opID]; r != nil {
			inflight.markCancelled(h.opID, r.ByAdmin)
		}
	}
}

// renewBatch is renewOnce's renewal of every held entry in one write,
// under k.round; it returns the entries to recover (in key order), and
// whether every entry renewed was renewed (no error, none lost).
func (k *leaseKeeper) renewBatch(ctx context.Context) ([]renewal, bool) {
	k.round.Lock()
	defer k.round.Unlock()
	k.mu.Lock()
	keys := make([]string, 0, len(k.held))
	for key, h := range k.held {
		if !h.gone {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	held := make([]*heldLease, 0, len(keys))
	for _, key := range keys {
		held = append(held, k.held[key])
	}
	hb := k.clock
	k.mu.Unlock()

	var batch []renewal
	var first *heldLease // the entry whose deadline comes first
	for _, h := range held {
		op := inflight.get(h.opID)
		if op == nil {
			continue
		}
		l := leaseFor(op, hb.Now())
		batch = append(batch, renewal{h: h, lease: l, value: lockValue(l)})
		if first == nil || h.renewed.Before(first.renewed) {
			first = h
		}
	}
	if len(batch) == 0 {
		return nil, false
	}
	k.mu.Lock()
	writeDeadline := first.lost
	renewals := make([]cluster.LockRenewal, len(batch))
	for i, r := range batch {
		renewals[i] = cluster.LockRenewal{Key: r.h.key, Old: r.h.value, New: r.value}
	}
	k.mu.Unlock()
	wctx, wcancel := context.WithTimeout(writeDeadline, leaseWriteTimeout)
	defer wcancel()
	stopWrite := context.AfterFunc(ctx, wcancel)
	defer stopWrite()
	wc := batch[0].h.client.WithContext(wctx)
	if fresh := batch[0].h.fresh; fresh != nil {
		wc = fresh(wctx)
	}
	lostKeys, err := renewOperationLocks(wc, renewals)
	var recoveries []renewal
	renewedAll := true
	for _, r := range batch {
		if ctx.Err() != nil {
			return nil, false
		}
		k.mu.Lock()
		current := k.held[r.h.key] == r.h && !r.h.gone
		lost := r.h.lost
		k.mu.Unlock()
		if !current {
			continue
		}
		if holder, isLost := lostKeys[r.h.key]; isLost {
			renewedAll = false
			if leaseHeldElsewhere(holder) {
				k.lose(r.h, "another updater pod took over the cross-pod operation lock", "holder", holder.ID, "holderPod", holder.Pod)
				continue
			}
			recoveries = append(recoveries, r)
			continue
		}
		if err != nil || lost.Err() != nil {
			renewedAll = false
		}
		k.settle(r, lost, err)
	}
	return recoveries, renewedAll
}

// settle applies the outcome err of r's write.
func (k *leaseKeeper) settle(r renewal, lost context.Context, err error) {
	switch {
	case lost.Err() != nil:
		// The deadline passed while the renewal ran: fail closed, also
		// when the write went through.
		k.expired(r.h, r.h.renewed, err)
	case err != nil:
		slog.Warn("could not renew the cross-pod operation lock", "operation", r.h.opID, "key", r.h.key, "error", err)
	default:
		k.renewed(r)
	}
}

// recover takes r's entry again (see renewOnce) with a context of its own,
// bounded by the entry's deadline. The check runs where the write is made
// (cluster.AcquireOperationLock runs it under this process's writer, as a
// release's write is): it refuses when the entry was stopped meanwhile
// (released, or its project taken by the next action of this process), or
// when the record holds another operation of this process under the key.
// So a recovery never revives a released entry nor overwrites a
// successor's.
func (k *leaseKeeper) recover(ctx context.Context, r renewal) {
	k.mu.Lock()
	lost := r.h.lost
	k.mu.Unlock()
	rctx, cancel := context.WithTimeout(lost, leaseWriteTimeout)
	defer cancel()
	stopWrite := context.AfterFunc(ctx, cancel)
	defer stopWrite()
	c := r.h.client.WithContext(rctx)
	if r.h.fresh != nil {
		c = r.h.fresh(rctx)
	}
	err := acquireOperationLock(c, r.h.key, r.value, k.recoveryCheck(r.h))
	k.mu.Lock()
	current := k.held[r.h.key] == r.h && !r.h.gone
	k.mu.Unlock()
	var conflict *lockConflict
	switch {
	case errors.Is(err, errEntryReleased), !current:
		// Stopped meanwhile (its stop also ends this write's context):
		// the operation ended, nothing to report.
		return
	case errors.As(err, &conflict):
		attrs := []any{"reason", conflict.reason}
		if conflict.holder.lease != nil {
			attrs = append(attrs, "holder", conflict.holder.lease.ID, "holderPod", conflict.holder.lease.Pod)
		} else {
			attrs = append(attrs, "holder", conflict.holder.opID)
		}
		k.lose(r.h, "another operation took over the operation lock", attrs...)
		return
	}
	k.settle(r, lost, err)
}

// recoveryCheck is the check of a recovery of h (recover).
func (k *leaseKeeper) recoveryCheck(h *heldLease) func(*cluster.OperationRecord) error {
	return func(rec *cluster.OperationRecord) error {
		k.mu.Lock()
		held := k.held[h.key] == h && !h.gone
		k.mu.Unlock()
		if !held {
			return errEntryReleased
		}
		entry := rec.Lease
		if h.uid != "" {
			entry = rec.ProjectLocks[h.uid]
		}
		if entry != nil && entry.BootID == bootID && entry.ID != h.opID {
			return &lockConflict{reason: conflictProject, holder: lockHolder{projectUID: h.uid, user: entry.User, opID: entry.ID}}
		}
		return h.check(rec)
	}
}

// renewNow renews the entry key at once (a project action that begins
// records Begun before it changes anything). It returns an error when the
// entry is not held, was taken over, or could not be written; the
// heartbeat deals with the entry as usual afterwards.
func (k *leaseKeeper) renewNow(key string) error {
	k.round.Lock()
	defer k.round.Unlock()
	k.mu.Lock()
	h := k.held[key]
	if h == nil || h.gone {
		k.mu.Unlock()
		return fmt.Errorf("the operation lock %s is not held", key)
	}
	old, lost, hb := h.value, h.lost, k.clock
	k.mu.Unlock()
	op := inflight.get(h.opID)
	if op == nil {
		return fmt.Errorf("the operation of the lock %s has ended", key)
	}
	l := leaseFor(op, hb.Now())
	r := renewal{h: h, lease: l, value: lockValue(l)}
	wctx, cancel := context.WithTimeout(lost, leaseWriteTimeout)
	defer cancel()
	c := h.client.WithContext(wctx)
	if h.fresh != nil {
		c = h.fresh(wctx)
	}
	lostKeys, err := renewOperationLocks(c, []cluster.LockRenewal{{Key: key, Old: old, New: r.value}})
	if _, isLost := lostKeys[key]; isLost {
		return fmt.Errorf("the operation lock %s holds another value", key)
	}
	if err != nil {
		return err
	}
	if lost.Err() != nil || !k.renewed(r) {
		return fmt.Errorf("the operation lock %s may have expired", key)
	}
	return nil
}

// renewed records r's successful write: its value and heartbeat, and a
// new deadline, and reports true. When the deadline passed just now (its
// watcher stops the operation instead) it changes nothing and reports
// false.
func (k *leaseKeeper) renewed(r renewal) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !r.h.cancelLost() {
		return false
	}
	r.h.value = r.value
	r.h.renewed, _ = cluster.LeaseHeartbeat(r.lease)
	k.watchLocked(r.h)
	return true
}

// stop ends the renewals of the entry key and returns its owner, nil when
// this process does not hold it. Stopping the last entry stops the
// heartbeat (cancelling a renewal in flight) and waits until it returned.
func (k *leaseKeeper) stop(key string) *cluster.LeaseOwner {
	k.mu.Lock()
	h := k.held[key]
	if h == nil {
		k.mu.Unlock()
		return nil
	}
	delete(k.held, key)
	h.stop()
	var cancel context.CancelFunc
	var done chan struct{}
	if len(k.held) == 0 {
		cancel, done = k.cancel, k.done
		k.cancel, k.done, k.running = nil, nil, false
	}
	k.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	owner := h.owner
	return &owner
}

// stopAll stops every held entry (stop) and returns how many there were.
func (k *leaseKeeper) stopAll() int {
	k.mu.Lock()
	keys := make([]string, 0, len(k.held))
	for key := range k.held {
		keys = append(keys, key)
	}
	k.mu.Unlock()
	for _, key := range keys {
		k.stop(key)
	}
	return len(keys)
}

// release clears the entry key of an operation that never began (no
// marker write follows to release it), or of a project action.
func (k *leaseKeeper) release(c *cluster.Client, key string, owner cluster.LeaseOwner) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWriteTimeout)
	defer cancel()
	if err := releaseOperationLock(c.WithContext(ctx), key, owner); err != nil {
		slog.Warn("could not release the cross-pod operation lock; it expires on its own", "operation", owner.ID, "key", key, "ttl", leaseTTL, "error", err)
	}
}
