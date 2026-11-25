package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// getObservabilityStatus reads the Observability tab; tests replace it.
var getObservabilityStatus = cluster.GetObservabilityStatus

// runObservability runs an Observability operation; tests replace it.
var runObservability = cluster.RunObservability

// HandleObservabilityStatus reports RHOAI's monitoring for the usage logs and
// dashboards, part by part (tier admin; the ServiceAccount's reads).
var HandleObservabilityStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	st, err := getObservabilityStatus(c)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the observability state: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, st, "observability")
})

// HandleObservabilityStream sets up or cleans up the usage logs and
// dashboards as one operation streaming its steps, with the user's token.
var HandleObservabilityStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.ObservabilityRequest
	if !decodeBody(w, r, 1024, &req, false) {
		return
	}
	if problem := cluster.ValidateObservabilityRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Action+" usage logs and dashboards")
	slog.Info("mutation", "op", "observability", "action", req.Action)
	_, runErr := streamOperation(w, "observability", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runObservability(c, req, emit)
	})
	if runErr != nil {
		slog.Error("observability operation failed", "error", runErr)
	}
})
