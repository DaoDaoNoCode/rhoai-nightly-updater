package api

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Cross-pod operation lock.
//
// The local lock (clusterMutationInProgress) only covers this process. The
// Deployment runs one replica with the Recreate strategy, so rollouts never
// overlap, but a deleted or evicted pod (node drain, cluster upgrade,
// autoscaler) is replaced at once while the old pod still drains its
// running operation for up to cluster.ShutdownDrainTimeout. Both pods would
// then delete and recreate the same Subscription, CatalogSource and CSV.
//
// So every operation that takes the local lock also takes a lease in the
// operation ConfigMap (key "lock", next to the marker and lastCompleted; the
// existing Role already allows get, create and patch on it) and renews its
// heartbeat while it runs, also during the shutdown drain. A process that
// finds a live lease of another process refuses with 409 cluster_busy and
// reports that operation as running (remote) instead of interrupted.
//
// Error policy: fail closed. An operation starts only after its lease was
// written. A live lease of another process refuses with 409 cluster_busy
// and names that operation. A lease that cannot be read or written (API
// errors, timeouts, or write conflicts that persist through every retry)
// refuses with 503 lock_unavailable and Retry-After: without the lease the
// tool cannot tell whether another updater pod is changing the cluster,
// and two concurrent Updates or Reinstalls would both delete and recreate
// the Subscription, CatalogSource and CSV. Nothing was changed then; the
// user retries. While the operation runs, losing the lease stops it
// (startHeartbeat).
var (
	writeOperationLease   = cluster.WriteOperationLease
	releaseOperationLease = cluster.ReleaseOperationLease
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
// readiness probe first).
var (
	leaseHeartbeatInterval = 10 * time.Second
	leaseTTL               = 45 * time.Second
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

// leaseFor is the lease that describes op now.
func leaseFor(op *Operation, now time.Time) *types.OperationLease {
	return &types.OperationLease{
		ID: op.ID, BootID: bootID, Pod: op.Pod, User: op.User, Type: op.Type, Label: op.Label, Target: op.Target,
		StartedAt:   op.StartedAt.UTC().Format(time.RFC3339),
		HeartbeatAt: now.UTC().Format(time.RFC3339),
		Step:        op.Step, StepStatus: op.StepStatus,
		Message: truncateText(op.Message, maxOperationMessageBytes),
	}
}

// remoteOperation describes the operation of another process's lease.
func remoteOperation(l *types.OperationLease) *Operation {
	started, _ := time.Parse(time.RFC3339, l.StartedAt)
	beat, _ := time.Parse(time.RFC3339, l.HeartbeatAt)
	return &Operation{
		ID: l.ID, Type: l.Type, Label: l.Label, User: l.User, Target: l.Target,
		StartedAt: started, UpdatedAt: beat, Step: l.Step, StepStatus: l.StepStatus, Message: l.Message,
		Pod: l.Pod, Remote: true,
	}
}

// leaseKeeper holds this process's lease while an operation runs and
// renews it in the background.
type leaseKeeper struct {
	mu     sync.Mutex
	owner  *cluster.LeaseOwner
	cancel context.CancelFunc
	done   chan struct{}
}

var leases = &leaseKeeper{}

// acquire takes the lease for op. It returns the lease of another process
// that holds the lock, or the error that kept it from reading or writing
// the lease; either way the caller refuses the operation (see the error
// policy). On success the heartbeat runs until stop; when the lease is
// lost it stops the operation through cancelOp (see startHeartbeat).
func (k *leaseKeeper) acquire(c *cluster.Client, op *Operation, cancelOp context.CancelCauseFunc) (*types.OperationLease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWriteTimeout)
	defer cancel()
	lease := leaseFor(op, clk.Now())
	holder, err := writeOperationLease(c.WithContext(ctx), lease, leaseHeldElsewhere)
	if holder != nil || err != nil {
		return holder, err
	}
	renewed, _ := cluster.LeaseHeartbeat(lease)
	k.startHeartbeat(c, op.ID, renewed, cancelOp, op.freshBookkeeper)
	return nil, nil
}

// leaseLossAfter is how long after the heartbeat of its last successful
// write the holder counts its lease as lost: one interval before the TTL,
// since another pod may take an expired lease over.
func leaseLossAfter() time.Duration {
	return leaseTTL - leaseHeartbeatInterval
}

// startHeartbeat renews the lease of the operation with this ID, with its
// latest step, until stop. It does not depend on the request: it keeps
// running while the server drains operations at shutdown.
//
// The lease is lost when a renewal finds another process's live lease, or
// when leaseLossAfter has passed since renewed, the heartbeat of the last
// successful write (cluster.LeaseHeartbeat: the instant other pods compare
// with leaseTTL), without another success: at that instant, between
// renewals or during one, so the operation stops one interval before
// another pod may take the lease over. The heartbeat then stops the
// operation with cluster.ErrOperationLockLost: its context ends, and no
// cleanup or restore runs (cluster.postOperationContext), because the other
// pod may be changing the same objects. A single failed renewal does not.
//
// fresh, when not nil, makes the client of each renewal with the
// ServiceAccount token as it is then (it rotates); otherwise c is used.
func (k *leaseKeeper) startHeartbeat(c *cluster.Client, opID string, renewed time.Time, cancelOp context.CancelCauseFunc, fresh func(context.Context) *cluster.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	k.mu.Lock()
	k.owner = &cluster.LeaseOwner{BootID: bootID, ID: opID}
	k.cancel, k.done = cancel, done
	interval, lossAfter := leaseHeartbeatInterval, leaseLossAfter()
	k.mu.Unlock()
	// lose runs once: the deadline's watcher and the heartbeat may both
	// find the lease lost.
	var lostOnce sync.Once
	lose := func(why string, attrs ...any) {
		lostOnce.Do(func() {
			slog.Error("operation stopped: "+why, append([]any{"operation", opID}, attrs...)...)
			inflight.update(opID, func(op *Operation) { op.lockLost = true })
			if cancelOp != nil {
				cancelOp(cluster.ErrOperationLockLost)
			}
		})
	}
	expired := func(lastHeartbeat time.Time, err error) {
		lose("the cross-pod operation lock could not be renewed and may have expired", "lastHeartbeat", lastHeartbeat, "error", err)
	}
	// The ticker starts here, not in the goroutine, so the first renewal is
	// due one interval after the lease was taken.
	hb := clk
	ticker := hb.NewTicker(interval)
	// The deadline is on the heartbeat's clock. Checking only when a
	// renewal returns would stop up to an interval plus a write timeout
	// late, as late as the instant the lease expires. So the deadline stops
	// the operation by itself (its watcher), even while a renewal's write
	// hangs, and it also ends that write (the write's context derives from
	// it). A deadline replaced after a successful renewal is detached from
	// its watcher before it is cancelled, so that cancel is not a loss.
	deadline := func(heartbeat time.Time) (context.Context, func() bool) {
		lost, cancelLost := hb.WithDeadline(ctx, heartbeat.Add(lossAfter))
		unwatch := context.AfterFunc(lost, func() {
			if ctx.Err() == nil { // not the heartbeat's own stop
				expired(heartbeat, nil)
			}
		})
		// The cancel reports false when the deadline passed first (its
		// watcher stops the operation).
		return lost, func() bool {
			watched := unwatch()
			cancelLost()
			return watched
		}
	}
	lost, cancelLost := deadline(renewed)
	go func() {
		defer close(done)
		defer ticker.Stop()
		defer func() { cancelLost() }()
		for {
			select {
			case <-ctx.Done():
			case <-lost.Done():
			case <-ticker.C():
			}
			if ctx.Err() != nil {
				return
			}
			if lost.Err() != nil {
				expired(renewed, nil) // its watcher may be first; lose runs once
				return
			}
			op := inflight.snapshot()
			if op == nil || op.ID != opID {
				return
			}
			wctx, wcancel := context.WithTimeout(lost, leaseWriteTimeout)
			lease := leaseFor(op, hb.Now())
			wc := c.WithContext(wctx)
			if fresh != nil {
				wc = fresh(wctx)
			}
			holder, err := writeOperationLease(wc, lease, leaseHeldElsewhere)
			wcancel()
			switch {
			case ctx.Err() != nil:
				return
			case holder != nil:
				lose("another updater pod took over the cross-pod operation lock", "holder", holder.ID, "holderPod", holder.Pod)
				return
			case lost.Err() != nil:
				// The deadline passed while the renewal ran: fail closed,
				// also when the write went through.
				expired(renewed, err)
				return
			case err != nil:
				slog.Warn("could not renew the cross-pod operation lock", "operation", opID, "error", err)
			default:
				if !cancelLost() {
					return // the deadline passed just now: its watcher stops the operation
				}
				renewed, _ = cluster.LeaseHeartbeat(lease)
				lost, cancelLost = deadline(renewed)
			}
		}
	}()
}

// stop ends the heartbeat (cancelling a renewal in flight) and returns the
// held lease, or nil when none is held.
func (k *leaseKeeper) stop() *cluster.LeaseOwner {
	k.mu.Lock()
	owner, cancel, done := k.owner, k.cancel, k.done
	k.owner, k.cancel, k.done = nil, nil, nil
	k.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return owner
}

// release clears the lease of an operation that never began (no marker
// write follows to release it).
func (k *leaseKeeper) release(c *cluster.Client, owner cluster.LeaseOwner) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWriteTimeout)
	defer cancel()
	if err := releaseOperationLease(c.WithContext(ctx), owner); err != nil {
		slog.Warn("could not release the cross-pod operation lock; it expires on its own", "operation", owner.ID, "ttl", leaseTTL, "error", err)
	}
}
