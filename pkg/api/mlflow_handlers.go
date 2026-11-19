package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleMLflowSetup creates the MLflow CR.
var HandleMLflowSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "setup-mlflow")
	result, err := cluster.SetupMLflow(c)
	if err != nil {
		slog.Error("setup-mlflow failed", "error", err)
		writeError(w, "MLflow setup failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "setup-mlflow")
})

// HandleMLflowTeardown deletes the MLflow CR.
var HandleMLflowTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "teardown-mlflow")
	result, err := cluster.TeardownMLflow(c)
	if err != nil {
		slog.Error("teardown-mlflow failed", "error", err)
		writeError(w, "MLflow teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-mlflow")
})

// HandleMLflowDeployPR patches the MLflow CR with a PR image.
var HandleMLflowDeployPR = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.DeployPRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.PR <= 0 {
		writeError(w, "pr must be a positive integer", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, fmt.Sprintf("PR #%d", req.PR))
	slog.Info("mutation", "op", "deploy-mlflow-pr", "pr", req.PR)
	result, err := cluster.DeployMLflowPR(c, req.PR)
	if err != nil {
		slog.Error("deploy-mlflow-pr failed", "error", err)
		writeError(w, "MLflow PR deploy failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-mlflow-pr")
})

// HandleMLflowRevert reverts the MLflow CR image to default.
var HandleMLflowRevert = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "revert-mlflow")
	result, err := cluster.RevertMLflowImage(c)
	if err != nil {
		slog.Error("revert-mlflow failed", "error", err)
		writeError(w, "MLflow revert failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "revert-mlflow")
})
