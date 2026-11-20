package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// getSummary reads the cluster summary; tests replace it.
var getSummary = cluster.GetSummary

// HandleSummary is the cluster summary every signed-in user sees (tier
// summary-or-registry): a fixed type (types.Summary) read by the
// ServiceAccount.
var HandleSummary = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	s, err := getSummary(c)
	if err != nil {
		slog.Error("summary error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the cluster summary.", code, errorCode)
		return
	}
	s.UpdaterVersion = Version
	writeJSON(w, s, "summary")
})
