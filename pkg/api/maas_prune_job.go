package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// The background check for MaaS publications that are gone
// (pkg/cluster maas_prune.go). A project deleted while its model was
// published must leave maas-default-gateway's allowedRoutes before someone
// recreates the name; waiting for a cluster-admin's next MaaS, Models or
// Clean up operation could take days. So the updater checks at start and
// every maasPruneInterval, as a cluster operation of its own: only when it
// gets the local lock and the cross-pod lease (one updater pod at a time,
// never during another operation, which then is simply skipped: the next
// tick tries again), and not while the server shuts down. The check is
// fail-closed as in an operation (what cannot be read is kept), and it
// writes an activity entry only when it changed something.
var (
	maasPruneInterval             = 5 * time.Minute
	maasPruneClock    clock.Clock = clock.Real
	// pruneGoneMaaS runs one check with the client; tests replace it.
	pruneGoneMaaS = cluster.PruneGoneMaaSPublicationsInBackground
)

// maasPruneTimeout bounds one background check.
const maasPruneTimeout = 2 * time.Minute

// maasPruneUser is who the activity log and the operation name.
const maasPruneUser = "rhoai-nightly-updater (background check)"

// StartMaaSPruneJob runs the check now and every maasPruneInterval until
// ctx ends. The channel it returns closes once the job has stopped (after
// ctx ends and a run in progress finished); it is nil when the job does
// not start: in the gitops profile (no MaaS there).
func StartMaaSPruneJob(ctx context.Context) <-chan struct{} {
	if activeProfile == ProfileGitOps {
		return nil
	}
	ticker := maasPruneClock.NewTicker(maasPruneInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		runMaaSPruneOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				runMaaSPruneOnce(ctx)
			}
		}
	}()
	return done
}

// runMaaSPruneOnce runs one check under the cluster lock and the cross-pod
// lease, and reports whether it ran. It skips quietly when the server is
// shutting down, there is no ServiceAccount token, or another operation
// (in this pod or another) holds the lock or the lease, or the lease
// cannot be read or written (fail closed).
func runMaaSPruneOnce(ctx context.Context) bool {
	token := getClusterToken()
	if token == "" {
		return false
	}
	if !mutations.begin() {
		return false
	}
	defer mutations.end()
	if !acquireClusterMutationLock() {
		slog.Debug("background MaaS check skipped: another operation runs")
		return false
	}
	opCtx, cancelOp := context.WithCancelCause(ctx)
	defer cancelOp(nil)
	runCtx, cancel := maasPruneClock.WithTimeout(opCtx, maasPruneTimeout)
	defer cancel()
	c := cluster.NewClientWithContext(runCtx, token)
	c.SetUsername(maasPruneUser)
	op := inflight.startBackground("prune-maas-publications", "Drop MaaS publications that are gone", maasPruneUser, c)
	holder, err := leases.acquire(c, op, cancelOp)
	if holder != nil || err != nil {
		inflight.finish()
		clusterMutationInProgress.Store(false)
		slog.Debug("background MaaS check skipped: the cross-pod lock is held or unavailable", "error", err)
		return false
	}
	defer releaseClusterMutationLock()
	pruneGoneMaaS(c)
	return true
}
