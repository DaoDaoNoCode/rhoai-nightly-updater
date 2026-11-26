package cluster

import (
	"context"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
)

// The checks made with the signed-in user's own token (internal/authz),
// under the names pkg/api and this package use.

// SessionToken is authz.SessionToken.
type SessionToken = authz.SessionToken

// The errors of internal/authz: the same values, so errors.Is matches
// either name.
var (
	ErrUnauthenticated  = authz.ErrUnauthenticated
	ErrNotAnOAuthToken  = authz.ErrNotAnOAuthToken
	ErrSessionTokenGone = authz.ErrSessionTokenGone
)

// CheckUserPermissionWithToken is authz.CheckUserPermissionWithToken.
func CheckUserPermissionWithToken(ctx context.Context, userToken, verb, resource, apiGroup, namespace string) (bool, error) {
	return authz.CheckUserPermissionWithToken(ctx, userToken, verb, resource, apiGroup, namespace)
}

// ReadSessionToken is authz.ReadSessionToken.
func ReadSessionToken(ctx context.Context, userToken string) (*SessionToken, error) {
	return authz.ReadSessionToken(ctx, userToken)
}

// LookupUserWithToken is authz.LookupUserWithToken.
func LookupUserWithToken(ctx context.Context, userToken string) (string, error) {
	return authz.LookupUserWithToken(ctx, userToken)
}

// ListUserDSProjects is authz.ListUserDSProjects.
func ListUserDSProjects(u *Client) ([]string, error) {
	return authz.ListUserDSProjects(u)
}

// ProjectUID is authz.ProjectUID.
func ProjectUID(u *Client, project string) (string, error) {
	return authz.ProjectUID(u, project)
}

// IsUserDSProject is authz.IsUserDSProject.
func IsUserDSProject(u *Client, project string) (bool, error) {
	return authz.IsUserDSProject(u, project)
}
