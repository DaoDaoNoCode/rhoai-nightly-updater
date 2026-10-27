package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// Profile is what the updater may do on its cluster, set by the PROFILE
// environment variable (deploy/template.yaml parameter PROFILE; the Helm
// chart's value profile). docs/GITOPS.md, "Profiles", explains both.
type Profile string

const (
	// ProfileFull is every feature: the default, for the dashboard team's
	// development clusters.
	ProfileFull Profile = "full"
	// ProfileGitOps is for clusters where Argo CD manages RHOAI: the only
	// change the updater makes is Update's Argo CD path (install the build
	// the catalog Git manages serves). Every other route answers 403
	// profile_gitops, and reads need cluster-admin. Its RBAC is
	// deploy/rbac-gitops.yaml.
	ProfileGitOps Profile = "gitops"
)

// ParseProfile reads a PROFILE value: "" and "full" are ProfileFull,
// "gitops" is ProfileGitOps. Anything else is an error, so a misspelled
// value stops the updater instead of silently running every feature.
func ParseProfile(s string) (Profile, error) {
	switch s {
	case "", string(ProfileFull):
		return ProfileFull, nil
	case string(ProfileGitOps):
		return ProfileGitOps, nil
	}
	return "", fmt.Errorf("PROFILE=%q is not a profile: use %q (every feature, the default) or %q (Argo CD manages RHOAI; see docs/GITOPS.md)", s, ProfileFull, ProfileGitOps)
}

// activeProfile is set once at startup, before Register.
var activeProfile = ProfileFull

// SetProfile sets the profile Register and /api/version use. Call it before
// Register.
func SetProfile(p Profile) {
	activeProfile = p
}

// gitOpsAdminMessage explains a 403 for a read in the gitops profile.
const gitOpsAdminMessage = "This updater runs the gitops profile, where only cluster-admins can see what it reads from the cluster."

// withGitOpsAdmin guards a read in the gitops profile: the user must pass
// the same cluster-admin check as every mutation (mutationPermission, with
// the user's own token) before the handler makes any ServiceAccount call.
// In the full profile reads only need the sign-in (oauth-proxy's
// --openshift-sar); on the clusters the gitops profile is for, many people
// can sign in, and what the ServiceAccount reads (pods, events, every
// namespace's diagnostics) is not theirs to see.
func withGitOpsAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		allowed, err := mutationPermission(r.Context(), extractUserToken(r))
		if err != nil {
			writeAuthError(w, err, "Cannot verify your permissions. Try again shortly.")
			return
		}
		if !allowed {
			writeError(w, gitOpsAdminMessage, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	}
}

// refusedInGitOpsProfile answers a route the gitops profile does not have:
// 403 profile_gitops, before authentication and before any ServiceAccount
// call.
func refusedInGitOpsProfile(feature string) http.HandlerFunc {
	msg := fmt.Sprintf("%s is off: this updater runs the gitops profile, where Argo CD manages RHOAI and the updater only installs the build the Git-managed catalog serves (docs/GITOPS.md).", feature)
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		slog.Debug("refused by the gitops profile", "path", r.URL.Path)
		writeError(w, msg, http.StatusForbidden, cluster.ErrorCodeProfileGitOps)
	}
}
