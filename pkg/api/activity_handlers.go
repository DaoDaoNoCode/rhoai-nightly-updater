package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleActivity returns the activity log (tier per-role). A
// cluster-admin gets every entry. Anyone else gets only their own entries,
// filtered here on the server: the ServiceAccount reads the log, and the
// entries whose user is the request's identity are kept. That identity is
// the one requestUser resolved from the user's own token (users/~), never
// the X-Forwarded-User header; without one, no entry is returned (fail
// closed).
var HandleActivity = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	activity, err := cluster.GetActivity(c)
	if err != nil {
		slog.Error("activity error", "error", err)
		writeReadError(w, "failed to get activity", err)
		return
	}
	if !admin {
		activity = ownActivity(activity, verifiedUser(r))
	}
	writeJSON(w, activity, "activity")
})

// verifiedUser is the request's identity as requestUser verified it with
// the user's token, "" when there is none.
func verifiedUser(r *http.Request) string {
	u, _ := r.Context().Value(identityKey{}).(string)
	return u
}

// ownActivity keeps user's entries of activity; none for no user.
func ownActivity(activity []types.ActivityEntry, user string) []types.ActivityEntry {
	out := []types.ActivityEntry{}
	if user == "" {
		return out
	}
	for _, e := range activity {
		if e.User == user {
			out = append(out, e)
		}
	}
	return out
}
