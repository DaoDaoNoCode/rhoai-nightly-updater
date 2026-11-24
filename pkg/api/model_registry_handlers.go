package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// getModelRegistryStatus reads the Model registry feature; tests replace it.
var getModelRegistryStatus = cluster.GetModelRegistryStatus

// runModelRegistry runs a Model registry operation; tests replace it.
var runModelRegistry = cluster.RunModelRegistry

// HandleModelRegistryStatus reports this tool's model registry, read with
// the user's own token (tier user-token): the registry, its Service and its
// sample model; the ServiceAccount only finds where registries live.
var HandleModelRegistryStatus = withUserRead(func(sa, u *cluster.Client, w http.ResponseWriter, r *http.Request) {
	st, err := getModelRegistryStatus(sa, u)
	if err != nil {
		writeUserReadError(w, "Cannot read the model registry", err)
		return
	}
	writeJSON(w, st, "model-registry")
})

// HandleModelRegistryStream sets up or deletes this tool's model registry
// as one operation streaming its steps: setup "check", "create", "ready",
// "samples"; teardown "check", "delete", "gone".
var HandleModelRegistryStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.ModelRegistryRequest
	if !decodeBody(w, r, 1024, &req, false) {
		return
	}
	if problem := cluster.ValidateModelRegistryRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Action+" model registry")
	slog.Info("mutation", "op", "model-registry", "action", req.Action, "samples", req.Samples)
	_, runErr := streamOperation(w, "model-registry", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runModelRegistry(c, req, emit)
	})
	if runErr != nil {
		slog.Error("model registry failed", "error", runErr)
	}
})
