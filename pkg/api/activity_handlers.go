package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// HandleActivity returns the activity log.
var HandleActivity = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	activity, err := cluster.GetActivity(c)
	if err != nil {
		slog.Error("activity error", "error", err)
		writeError(w, "failed to get activity", http.StatusInternalServerError)
		return
	}
	writeJSON(w, activity, "activity")
})
