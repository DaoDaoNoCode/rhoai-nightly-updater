package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
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
	// pruneGoneMaaS runs one check with the client: the MaaS publications
	// that are gone, then the records' entries of projects that are gone
	// (features records_sweep.go). Tests replace it.
	pruneGoneMaaS = cluster.SweepGoneInBackground
	// forgetGoneRecipeProjects drops recipe projects that are gone from
	// their record, in the same check (the record is written only under
	// the lock); tests replace it.
	forgetGoneRecipeProjects = cluster.ForgetGoneRecipeProjects
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
	opCtx, cancelOp := context.WithCancelCause(ctx)
	defer cancelOp(nil)
	var c *cluster.Client
	var cancel context.CancelFunc = func() {}
	defer func() { cancel() }()
	refusal := acquireRunnerLock(func() *Operation {
		var runCtx context.Context
		runCtx, cancel = maasPruneClock.WithTimeout(opCtx, maasPruneTimeout)
		c = cluster.NewClientWithContext(runCtx, token)
		c.SetUsername(maasPruneUser)
		op := inflight.startBackground("prune-maas-publications", "Drop MaaS publications that are gone", maasPruneUser, c)
		inflight.update(op.ID, func(o *Operation) {
			o.freshBookkeeper = func(ctx context.Context) *cluster.Client {
				fresh := getClusterToken()
				if fresh == "" {
					fresh = token
				}
				b := cluster.NewClientWithContext(ctx, fresh)
				b.SetUsername(maasPruneUser)
				return b
			}
		})
		return op
	}, cancelOp)
	if refusal != nil {
		if refusal.busy {
			slog.Debug("background MaaS check skipped: another operation runs")
		} else {
			slog.Debug("background MaaS check skipped: the cross-pod lock is held or unavailable", "error", refusal.err)
		}
		return false
	}
	defer releaseClusterMutationLock()
	pruneGoneMaaS(c)
	if forgot, err := forgetGoneRecipeProjects(c); err != nil {
		slog.Warn("background check: the record of the projects recipes created cannot be checked", "error", err)
	} else if len(forgot) > 0 {
		slog.Info("background check: forgot the recipe projects that are gone", "projects", strings.Join(forgot, ", "))
	}
	return true
}

// A MaaS status read that finds publications that are gone starts the same
// check at once (kickMaaSPrune), so the tab settles without waiting up to
// maasPruneInterval: in the background, never holding up the read, under
// the same lock and lease (runMaaSPruneOnce skips when either is taken),
// at most one at a time and one per maasPruneDebounce (the tab reads every
// few seconds while it settles), and never in the gitops profile.
var maasPruneDebounce = time.Minute

var (
	maasPruneKick struct {
		sync.Mutex
		running bool
		last    time.Time
	}
	// maasPruneKicks counts the checks kickMaaSPrune started that still run
	// (tests wait on it).
	maasPruneKicks sync.WaitGroup
)

// kickMaaSPrune starts one background check unless one runs, one started
// less than maasPruneDebounce ago, or the profile is gitops. It reports
// whether it started one.
func kickMaaSPrune() bool {
	if activeProfile == ProfileGitOps {
		return false
	}
	maasPruneKick.Lock()
	defer maasPruneKick.Unlock()
	now := maasPruneClock.Now()
	if maasPruneKick.running || (!maasPruneKick.last.IsZero() && now.Sub(maasPruneKick.last) < maasPruneDebounce) {
		return false
	}
	maasPruneKick.running, maasPruneKick.last = true, now
	maasPruneKicks.Add(1)
	go func() {
		defer maasPruneKicks.Done()
		defer func() {
			maasPruneKick.Lock()
			maasPruneKick.running = false
			maasPruneKick.Unlock()
		}()
		runMaaSPruneOnce(context.Background())
	}()
	return true
}

// resetMaaSPruneKick forgets the last kick (tests).
func resetMaaSPruneKick() {
	maasPruneKicks.Wait()
	maasPruneKick.Lock()
	defer maasPruneKick.Unlock()
	maasPruneKick.running, maasPruneKick.last = false, time.Time{}
}
