package api

import (
	"context"
	"errors"
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
// Error policy: the lease must not make the tool unusable when the API
// server hiccups. A failed read or write (other than a conflict) is logged
// as a warning and the operation continues without the lease ("fail open");
// the heartbeat keeps trying to write it. A live lease of another process
// that was actually read always refuses ("never fail open on a read lease"),
// and so do write conflicts that persist through every retry, since they
// mean another process is writing the record right now.
var (
	writeOperationLease   = cluster.WriteOperationLease
	releaseOperationLease = cluster.ReleaseOperationLease
)

// Lease timing. The holder renews the heartbeat every
// leaseHeartbeatInterval; a lease whose heartbeat is older than leaseTTL is
// free. The TTL covers three renewals that fail in a row (each bounded by
// leaseWriteTimeout) and about 30s of clock skew between pods, and it is
// also the longest a crashed pod's operation blocks a new one. A pod
// restart takes longer than that anyway (the replacement pod is scheduled,
// pulls the image and passes its readiness probe first).
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
	if pod := os.Getenv("HOSTNAME"); pod != "" && l.Pod == pod && os.Getenv("DEV_MODE") != "true" {
		return false
	}
	return cluster.LeaseLive(l, time.Now(), leaseTTL)
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

// errLeaseContended: the lease could not be written because another
// process kept writing the record.
var errLeaseContended = cluster.ErrOperationRecordContended

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
// that holds the lock, or errLeaseContended; the caller then refuses the
// operation. Any other failure only logs a warning (see the error policy).
// On success the heartbeat runs until stop; when the lease is lost it
// stops the operation through cancelOp (see startHeartbeat).
func (k *leaseKeeper) acquire(c *cluster.Client, op *Operation, cancelOp context.CancelCauseFunc) (*types.OperationLease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWriteTimeout)
	defer cancel()
	holder, err := writeOperationLease(c.WithContext(ctx), leaseFor(op, time.Now()), leaseHeldElsewhere)
	switch {
	case holder != nil:
		return holder, nil
	case errors.Is(err, errLeaseContended):
		return nil, err
	case err != nil:
		slog.Warn("could not take the cross-pod operation lock; the operation continues without it", "operation", op.ID, "error", err)
	}
	k.startHeartbeat(c, op.ID, cancelOp)
	return nil, nil
}

// leaseLossAfter is how long renewals may fail in a row before the lease
// counts as lost: one interval before the TTL, since another pod may take
// an expired lease over.
func leaseLossAfter() time.Duration {
	return leaseTTL - leaseHeartbeatInterval
}

// startHeartbeat renews the lease of the operation with this ID, with its
// latest step, until stop. It does not depend on the request: it keeps
// running while the server drains operations at shutdown.
//
// The lease is lost when a renewal finds another process's live lease, or
// when renewals have failed for leaseLossAfter since the last success
// (another pod may then take it over). The heartbeat then stops the
// operation with cluster.ErrOperationLockLost: its context ends, and no
// cleanup or restore runs (cluster.postOperationContext), because the other
// pod may be changing the same objects. A single failed renewal does not.
func (k *leaseKeeper) startHeartbeat(c *cluster.Client, opID string, cancelOp context.CancelCauseFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	k.mu.Lock()
	k.owner = &cluster.LeaseOwner{BootID: bootID, ID: opID}
	k.cancel, k.done = cancel, done
	interval, lossAfter := leaseHeartbeatInterval, leaseLossAfter()
	k.mu.Unlock()
	lose := func(why string, attrs ...any) {
		slog.Error("operation stopped: "+why, append([]any{"operation", opID}, attrs...)...)
		inflight.update(opID, func(op *Operation) { op.lockLost = true })
		if cancelOp != nil {
			cancelOp(cluster.ErrOperationLockLost)
		}
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		lastRenewed := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			op := inflight.snapshot()
			if op == nil || op.ID != opID {
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, leaseWriteTimeout)
			holder, err := writeOperationLease(c.WithContext(wctx), leaseFor(op, time.Now()), leaseHeldElsewhere)
			wcancel()
			switch {
			case ctx.Err() != nil:
				return
			case holder != nil:
				lose("another updater pod took over the cross-pod operation lock", "holder", holder.ID, "holderPod", holder.Pod)
				return
			case err != nil && time.Since(lastRenewed) >= lossAfter:
				lose("the cross-pod operation lock could not be renewed and may have expired", "since", lastRenewed, "error", err)
				return
			case err != nil:
				slog.Warn("could not renew the cross-pod operation lock", "operation", opID, "error", err)
			default:
				lastRenewed = time.Now()
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
