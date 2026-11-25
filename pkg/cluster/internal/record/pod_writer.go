package record

import (
	"context"
	"fmt"
)

// One writer per pod (per process) for each of the updater's two shared
// ConfigMaps, the operation record and the activity log. Every write of
// this process to one of them (the lease and its releases, the marker, the completion, the recipe run, the hard reset record; an
// activity entry) waits for the previous one to end, so this process never
// conflicts with itself: a resourceVersion conflict comes only from
// another updater pod (one replica with the Recreate strategy; two only
// while a replaced pod drains). Up to three recipe steps, the lease
// heartbeat and a request may otherwise write at the same instant, and
// each read-modify-write cycle would make the others start over. The lock
// renewals do not wait for the writer (RenewOperationLocks): they test
// what they replace, so they cannot overwrite a newer value.
//
// A writer is a semaphore rather than a goroutine with a queue: the write
// runs on its caller's goroutine with its caller's context, so a write
// whose caller gave up (its deadline passed while it waited) is never
// sent.
type podWriter chan struct{}

var (
	operationWriter = make(podWriter, 1)
	activityWriter  = make(podWriter, 1)
)

// do runs write once the other writes of this process ended, or returns
// the context's error when ctx ends first (nothing was written then).
func (w podWriter) do(ctx context.Context, write func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case w <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("waiting for this pod's other write of the ConfigMap: %w", context.Cause(ctx))
	}
	defer func() { <-w }()
	return write()
}
