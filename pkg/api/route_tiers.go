package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// Who may read what (full profile; docs/research/per-user-views.md and
// SECURITY.md, "Who sees what"). Everyone RHOAI lets sign in reaches the
// API (oauth-proxy's --openshift-sar, deploy/template.yaml). Two roles:
// "admin" passes the mutation gate (mutationPermission: cluster-admin,
// checked with the user's own token), "user" is everyone else.
//
// Every GET route has one tier in getRouteTiers. A GET route that is not
// listed is admin-only (tierOf), so a route added without a decision shows
// nothing to a non-admin; TestEveryGetRouteHasATier fails until it is
// listed. Non-GET routes change the cluster and are cluster-admin only
// through withMutationAuth (TestEveryNonGetRouteIsGated).
type readTier string

const (
	// tierAdmin: cluster-admins only. Others get 403 admin_only before the
	// handler runs, so no ServiceAccount read is made for them.
	tierAdmin readTier = "admin-only"
	// tierSummary: every signed-in user; the ServiceAccount reads a
	// reviewed summary (GET /api/summary, a fixed response type) or the
	// registry (Build Explorer: build metadata, never the credential).
	tierSummary readTier = "summary-or-registry"
	// tierUserToken: every signed-in user; the cluster data is read with
	// the user's own token (withUserRead), so the API server and the
	// platform APIs decide what they see. The ServiceAccount only finds
	// where a platform API is (its address) and reads the updater's own
	// records, which are filtered to the user's projects.
	tierUserToken readTier = "user-token"
	// tierNoClusterRead: every signed-in user; no cluster read.
	tierNoClusterRead readTier = "no-cluster-read"
	// tierPerRole: every signed-in user; the handler answers per role (an
	// admin gets the admin answer, a user a reduced one, see the handler).
	tierPerRole readTier = "per-role"
)

// getRouteTiers is the tier of every GET route of routes.
var getRouteTiers = map[string]readTier{
	"GET /api/status":                      tierAdmin,
	"GET /api/activity":                    tierAdmin,
	"GET /api/components":                  tierAdmin,
	"GET /api/components/dsc":              tierAdmin,
	"GET /api/components/dsc/plan":         tierAdmin,
	"GET /api/debug":                       tierAdmin,
	"GET /api/dashboard/state":             tierAdmin,
	"GET /api/dashboard/builds":            tierAdmin,
	"GET /api/diagnostics":                 tierAdmin,
	"GET /api/setup/dsc/preview":           tierAdmin,
	"GET /api/test-pull-secret":            tierAdmin,
	"GET /api/verify-nodes":                tierAdmin,
	"GET /api/latest-nightly":              tierAdmin,
	"GET /api/nightly-tags":                tierAdmin,
	"GET /api/resources/maas":              tierAdmin,
	"GET /api/resources/maas/groups":       tierAdmin,
	"GET /api/resources/prerequisites":     tierAdmin,
	"GET /api/resources/models/exposures":  tierAdmin,
	"GET /api/resources/workbench":         tierAdmin,
	"GET /api/resources/s3/admin-password": tierAdmin,
	// The recipe run: who runs it, its recipe, project and build.
	"GET /api/recipes/run": tierAdmin,

	"GET /api/summary":                 tierSummary,
	"GET /api/build-explorer/tags":     tierSummary,
	"GET /api/build-explorer/content":  tierSummary,
	"GET /api/build-explorer/contains": tierSummary,

	"GET /api/user/permissions":         tierUserToken,
	"GET /api/resources/projects":       tierUserToken,
	"GET /api/resources/playground":     tierUserToken,
	"GET /api/resources/model-registry": tierUserToken,
	"GET /api/resources/samples/mlflow": tierUserToken,

	"GET /api/update-check":                    tierNoClusterRead,
	"GET /api/resources/samples/pipeline/spec": tierNoClusterRead,

	// The running operation: a user gets {running, label, startedAt}
	// (HandleOperation). The Environment status: a user gets one of their
	// projects, read with their token (HandleResourcesStatus).
	"GET /api/operation":        tierPerRole,
	"GET /api/resources/status": tierPerRole,
}

// tierOf is the tier of a route pattern: admin-only unless getRouteTiers
// says otherwise (fail closed).
func tierOf(pattern string) readTier {
	if t, ok := getRouteTiers[pattern]; ok {
		return t
	}
	return tierAdmin
}

// Roles of GET /api/user/permissions.
const (
	roleAdmin = "admin"
	roleUser  = "user"
)

// adminOnlyCode and adminOnlyMessage answer a non-admin's read of an
// admin-only route.
const (
	adminOnlyCode    = "admin_only"
	adminOnlyMessage = "Only cluster-admins can see this."
)

// isAdmin is the request's role: true when the user passes the mutation
// gate (mutationPermission, with the user's own token). The request must
// have passed requestUser.
func isAdmin(r *http.Request) (bool, error) {
	return mutationPermission(r.Context(), extractUserToken(r))
}

// adminOnly guards an admin-only GET: the user is authenticated and must
// pass the mutation gate before next runs (and so before any
// ServiceAccount read). Others get 403 admin_only.
func adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, r, ok := requestUser(w, r)
		if !ok {
			RecordRequest()
			return
		}
		allowed, err := isAdmin(r)
		if err != nil {
			RecordRequest()
			writeAuthError(w, err, "Cannot verify your permissions. Nothing was read.")
			return
		}
		if !allowed {
			RecordRequest()
			writeError(w, adminOnlyMessage, http.StatusForbidden, adminOnlyCode)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminVerifiedKey{}, true)))
	}
}

// adminVerifiedKey marks a request whose user passed adminOnly's check
// (the mutation gate, with the request's own token), so withAdminRead
// does not ask the API server again for the same request. Only adminOnly
// sets it.
type adminVerifiedKey struct{}

// adminVerified reports whether adminOnly passed this request.
func adminVerified(r *http.Request) bool {
	v, _ := r.Context().Value(adminVerifiedKey{}).(bool)
	return v
}

// requestRole is isAdmin for a per-role handler. A 401 (the sign-in
// ended) is answered; any other error counts as "user", the smaller
// answer (fail closed), and is logged. ok is false when the handler must
// stop.
func requestRole(w http.ResponseWriter, r *http.Request) (admin, ok bool) {
	admin, err := isAdmin(r)
	if errors.Is(err, cluster.ErrUnauthenticated) {
		writeAuthError(w, err, "")
		return false, false
	}
	if err != nil {
		slog.Warn("cannot verify the user's role; answering as for a user", "path", r.URL.Path, "error", err)
		return false, true
	}
	return admin, true
}

// withUserRead wraps a read made with the signed-in user's own token: u is
// cluster.NewUserClient with that token, so the API server (and a platform
// API it is sent to: the dashboard's gen-ai API, MLflow) checks every read
// against the user's own RBAC. sa is the ServiceAccount's client, for what
// the user's token cannot find but is no user data: where a platform API
// is (the dashboard's and MLflow's addresses), and the updater's own
// records (u's bookkeeping), which a handler filters to the user's
// projects. A cache of what u reads is keyed by the token
// (cluster.tokenKey).
func withUserRead(fn func(sa, u *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return withAuth(func(sa *cluster.Client, w http.ResponseWriter, r *http.Request) {
		u := userReadClient(r.Context(), sa, extractUserToken(r), resolveUsername(r))
		fn(sa, u, w, r)
	})
}

// userReadClient is the signed-in user's client of a read: their token,
// the ServiceAccount's client for the updater's own records.
func userReadClient(ctx context.Context, sa *cluster.Client, token, username string) *cluster.Client {
	u := cluster.NewUserClient(ctx, token)
	u.SetUsername(username)
	u.SetBookkeeping(sa)
	return u
}

// writeUserReadError answers a failed read made with the user's own token.
// A 403 there is the user's own RBAC (forbidden_for_user: "you cannot see
// this"), never the ServiceAccount's (cluster.HTTPStatusForError).
//
// A failed ServiceAccount read on such a route (finding MLflow, the
// tool's records) is answered without its text, which may name objects
// the user may not see: only its kind; the error is logged.
func writeUserReadError(w http.ResponseWriter, what string, err error) {
	code, errorCode := cluster.HTTPStatusForError(err)
	var k8sErr *cluster.K8sError
	switch {
	case errorCode == cluster.ErrorCodeForbiddenForUser:
		writeError(w, what+": your OpenShift account may not see this. "+err.Error(), code, errorCode)
	case errors.As(err, &k8sErr) && !k8sErr.User:
		slog.Warn("a ServiceAccount read of a user's request failed", "what", what, "error", err)
		writeError(w, fmt.Sprintf("%s: the updater could not read what it needs (HTTP %d). Try again later, or ask a cluster-admin.", what, k8sErr.Status), code, errorCode)
	default:
		writeError(w, what+": "+err.Error(), code, errorCode)
	}
}
