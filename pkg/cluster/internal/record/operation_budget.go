package record

import (
	"context"
	"errors"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The automatic restore after a failed operation runs on its own contexts,
// so it also runs when the operation was cancelled or hit its deadline. Its
// two phases have separate budgets so slow CSV deletions cannot starve the
// re-apply of the previous catalog and Subscription (the part that brings
// the operator back): removing the attempt's Subscription and CSVs gets
// restoreCSVBudget in total (each CSV wait is also capped by
// CSVDeletionTimeout), and the re-apply always gets restoreApplyTimeout.
// RecoveryTimeout is their sum: the longest a restore runs after the
// operation stopped (see ShutdownDrainTimeout).
var (
	RestoreCSVBudget    = 30 * time.Second
	RestoreApplyTimeout = 30 * time.Second
	RecoveryTimeout     = RestoreCSVBudget + RestoreApplyTimeout
)

// Time budget of one cluster operation, from start to the end of its
// bookkeeping. The pod's shutdown drain must cover all of it, and
// terminationGracePeriodSeconds (deploy/template.yaml) must cover the drain,
// the final marker flush and the HTTP server shutdown with a margin;
// budget_test.go checks the arithmetic.
//   - OperationDeadline: the context deadline pkg/api gives every mutation
//     (withMutationAuth in pkg/api/auth_handlers.go). Every step, including the
//     Dashboard Dev revert, runs inside it; the install wait ends
//     operator.InstallDeadlineReserve before it.
//   - RecoveryTimeout: the automatic restore after a failure, after the
//     operation stopped.
//   - postOperationBookkeeping: the activity entry, recording the
//     Subscription, and clearing the operation marker (pkg/api).
//
// Everything the cluster package does after the operation's deadline (the
// restore, cleanup and bookkeeping writes) runs on postOperationContext,
// which ends at the deadline plus postDeadlineWork at the latest, however
// many writes there are. Only the marker clear (MarkerClearTimeout) follows.
const (
	OperationDeadline = 15 * time.Minute
	// MarkerClearTimeout bounds clearing the operation marker when an
	// operation ends (pkg/api markerWriter.complete): waiting for a
	// background retry write that holds the lock (5s), then its own 5s write.
	MarkerClearTimeout = 10 * time.Second
	// MarkerFlushTimeout bounds main.go's last marker flush after the drain.
	MarkerFlushTimeout = 10 * time.Second
	// HTTPShutdownTimeout bounds main.go's http.Server.Shutdown after the
	// drain; no mutation is running by then.
	HTTPShutdownTimeout = 20 * time.Second
	// ShutdownMargin is kept free below terminationGracePeriodSeconds:
	// SIGKILL comes exactly at the grace period.
	ShutdownMargin = 10 * time.Second
)

// Bounds of the bookkeeping writes. Variables so tests can shorten them.
var (
	// ActivityWriteTimeout bounds one Activity call, its conflict
	// retries included.
	ActivityWriteTimeout = 5 * time.Second
	// subscriptionRecordTimeout bounds recordLiveSubscription.
	subscriptionRecordTimeout = 5 * time.Second
)

// PostDeadlineWork is the longest the cluster package works after an
// operation's deadline: the restore, then the activity entry and the
// Subscription record.
func PostDeadlineWork() time.Duration {
	return RecoveryTimeout + ActivityWriteTimeout + subscriptionRecordTimeout
}

// postOperationBookkeeping is the bookkeeping that follows the restore.
func postOperationBookkeeping() time.Duration {
	return ActivityWriteTimeout + subscriptionRecordTimeout + MarkerClearTimeout
}

// ShutdownDrainTimeout is how long a terminating pod waits for running
// operations: the longest one can still take after SIGTERM.
func ShutdownDrainTimeout() time.Duration {
	return OperationDeadline + RecoveryTimeout + postOperationBookkeeping()
}

// PostOperationContext returns a context for work that must still run when
// the operation's own context has ended (the restore, cleanup and
// bookkeeping writes). It keeps the values of c's context but not its
// cancellation, and ends after timeout or at the operation's deadline plus
// postDeadlineWork, whichever comes first. That shared end keeps all of
// this work inside ShutdownDrainTimeout however many writes follow the
// deadline. A context without a deadline gets only timeout.
//
// After the operation lost its cross-pod lock (ErrOperationLockLost) the
// context is already cancelled: another updater pod may be changing the
// same objects, so no cleanup or restore may run.
func PostOperationContext(c *kube.Client, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := BookkeepingContext(c, timeout)
	if OperationLockLost(c) {
		cancel()
		ctx, cancelCause := context.WithCancelCause(context.Background())
		cancelCause(ErrOperationLockLost)
		return ctx, func() {}
	}
	return ctx, cancel
}

// ErrOperationLockLost is the cancellation cause of an operation whose
// cross-pod lock another updater pod took over, or that could not be
// renewed for so long that it may have expired (pkg/api operation_lease.go).
var ErrOperationLockLost = errors.New("the cross-pod operation lock was lost")

// LockLostMessage is the result of an operation stopped by
// ErrOperationLockLost.
const LockLostMessage = "Another updater pod took over the lock, so this operation stopped and made no further changes (no automatic restore). " +
	"Check the cluster state and re-run the operation after the other one finishes."

// OperationLockLost reports whether c's operation was stopped because its
// cross-pod lock was lost.
func OperationLockLost(c *kube.Client) bool {
	return c != nil && c.Context() != nil && errors.Is(context.Cause(c.Context()), ErrOperationLockLost)
}

// BookkeepingContext is postOperationContext for the updater's own records
// (the activity log), which are written even after the lock was lost.
func BookkeepingContext(c *kube.Client, timeout time.Duration) (context.Context, context.CancelFunc) {
	parent := context.Background()
	end := kube.Clock().Now().Add(timeout)
	if c != nil && c.Context() != nil {
		parent = context.WithoutCancel(c.Context())
		if d, ok := c.Context().Deadline(); ok {
			if hard := d.Add(PostDeadlineWork()); hard.Before(end) {
				end = hard
			}
		}
	}
	return kube.Clock().WithDeadline(parent, end)
}
