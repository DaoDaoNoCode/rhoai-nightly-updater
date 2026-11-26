package record

import (
	"context"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// What a project action's context carries for the updater's records and
// its own steps (pkg/api withProjectMutationAuth sets both): the project
// it acts in, and how to ask whether a cancel was requested. Both are
// context values, so every client of the operation (its user client, its
// bookkeeping copy, the post-operation context) sees them.

type actionScopeKey struct{}

type actionScope struct{ project, uid string }

// WithActionScope marks ctx as the context of a project action, a
// cluster-admin's or a user's, in project (namespace UID uid): its
// activity entry is scoped to that project (stampActivity).
func WithActionScope(ctx context.Context, project, uid string) context.Context {
	return context.WithValue(ctx, actionScopeKey{}, actionScope{project, uid})
}

// ActionScope is the project WithActionScope gave c's context, "" for a
// cluster operation.
func ActionScope(c *kube.Client) (project, uid string) {
	if c == nil || c.Context() == nil {
		return "", ""
	}
	s, _ := c.Context().Value(actionScopeKey{}).(actionScope)
	return s.project, s.uid
}

// ActionCancelledError is what a cancellable project action returns, and
// records, when it stops at one of its safe points because its starter or
// a cluster-admin asked it to (pkg/api, POST /api/operation/cancel). The
// cancel is a request: the action checks it between its own steps
// (CancelRequested), never in the middle of one, and then takes its
// existing failure path on its still-live context. ByAdmin: a
// cluster-admin who did not start it asked.
type ActionCancelledError struct {
	ByAdmin bool
}

func (e *ActionCancelledError) Error() string { return e.Reason() }

// Reason is the activity entry's reason of a cancelled action. The entry
// is the starter's: it never names the cluster-admin who cancelled it.
func (e *ActionCancelledError) Reason() string {
	if e.ByAdmin {
		return "Cancelled by a cluster-admin"
	}
	return "Cancelled by you"
}

type cancelCheckKey struct{}

// WithCancelCheck gives ctx the check of its operation's cancel request:
// check returns the request, nil while none was made.
func WithCancelCheck(ctx context.Context, check func() *ActionCancelledError) context.Context {
	return context.WithValue(ctx, cancelCheckKey{}, check)
}

// CancelRequested is the cancel requested for c's operation, nil when
// none was (or the operation cannot be cancelled). A cancellable action
// calls it at each of its safe points only.
func CancelRequested(c *kube.Client) *ActionCancelledError {
	if c == nil || c.Context() == nil {
		return nil
	}
	if check, _ := c.Context().Value(cancelCheckKey{}).(func() *ActionCancelledError); check != nil {
		return check()
	}
	return nil
}
