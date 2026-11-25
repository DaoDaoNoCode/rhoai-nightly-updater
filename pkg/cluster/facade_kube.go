package cluster

import (
	"context"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The Kubernetes API client (internal/kube), under the names pkg/api and
// this package use.
type (
	Client       = kube.Client
	K8sError     = kube.K8sError
	NetworkError = kube.NetworkError
	Changes      = kube.Changes
)

// Error codes a handler returns (see kube).
const (
	ErrorCodeForbiddenForUser = kube.ErrorCodeForbiddenForUser
	ErrorCodeGitOpsManaged    = kube.ErrorCodeGitOpsManaged
	ErrorCodeProfileGitOps    = kube.ErrorCodeProfileGitOps
)

// NewClientWithContext is kube.NewClientWithContext: a client with the
// given token (the ServiceAccount's).
func NewClientWithContext(ctx context.Context, token string) *Client {
	return kube.NewClientWithContext(ctx, token)
}

// NewUserClient is kube.NewUserClient: a client with the signed-in user's
// own token.
func NewUserClient(ctx context.Context, token string) *Client {
	return kube.NewUserClient(ctx, token)
}

// TrackChanges is kube.TrackChanges: a context whose requests record
// whether they may have changed something.
func TrackChanges(ctx context.Context) (context.Context, *Changes) {
	return kube.TrackChanges(ctx)
}

// IsK8sError is kube.IsK8sError.
func IsK8sError(err error, statusCode int) bool { return kube.IsK8sError(err, statusCode) }

// HTTPStatusForError is kube.HTTPStatusForError.
func HTTPStatusForError(err error) (int, string) { return kube.HTTPStatusForError(err) }

// SetUserAgentVersion is kube.SetUserAgentVersion.
func SetUserAgentVersion(v string) {
	kube.SetUserAgentVersion(v)
}
