package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleDashboardState returns the current state of the rhods-dashboard deployment.
var HandleDashboardState = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	state, err := cluster.GetDashboardState(c)
	if err != nil {
		status, code, message := cluster.DashboardStateError(err)
		if status != http.StatusNotFound {
			slog.Error("dashboard state error", "error", err, "errorCode", code)
			writeError(w, message, status, code)
			return
		}
		// Not deployed: still report a paused dashboard-operator so the page
		// can offer Revert (for example while a Dashboard CR deletion waits
		// for the paused operator to process its finalizer).
		body := map[string]interface{}{"error": message, "errorCode": code}
		if override, oErr := cluster.DashboardOverrideSummary(c); oErr == nil && override != nil && override.Active {
			body["override"] = override
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Error("response encode error", "label", "dashboard-state", "error", err)
		}
		return
	}
	writeJSON(w, state, "dashboard-state")
})

// HandleDashboardBuilds reports, for every dashboard container that runs a
// PR or main build, its commit, its distance from the branch it tracks and
// whether a newer build exists: GET /api/dashboard/builds. It is separate
// from the polled state route because it reads the registry and GitHub. A
// GitHub rate limit gives unknown divergences (rateLimited), never an error.
var HandleDashboardBuilds = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "dashboard-builds")
	result, err := cluster.GetDashboardBuilds(c)
	if err != nil {
		status, code, message := cluster.DashboardStateError(err)
		if status != http.StatusNotFound {
			slog.Error("dashboard builds error", "error", err, "errorCode", code)
		}
		writeError(w, message, status, code)
		return
	}
	writeJSON(w, result, "dashboard-builds")
})

// HandleDashboardDeployPR deploys a PR image to the rhods-dashboard deployment.
var HandleDashboardDeployPR = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
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
	if !cluster.ValidDashboardFlavor(req.Flavor) {
		writeError(w, "flavor must be rhoai or odh", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, fmt.Sprintf("PR #%d", req.PR))
	slog.Info("mutation", "op", "deploy-pr", "pr", req.PR, "flavor", req.Flavor)

	result, err := cluster.DeployPRImageWithFlavor(c, req.PR, req.Flavor)
	if err != nil {
		slog.Error("deploy-pr failed", "error", err)
		writeError(w, "deploy PR image failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-pr")
})

// HandleDashboardRevert reverts the dashboard to the original operator image.
var HandleDashboardRevert = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "revert-dashboard")

	result, err := cluster.RevertDashboardImage(c)
	if err != nil {
		slog.Error("revert-dashboard failed", "error", err)
		writeError(w, "revert dashboard failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "revert-dashboard")
})

var HandleDashboardDeployMain = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	// The body is optional; an empty body deploys the default (RHOAI) flavor.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.DeployMainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if !cluster.ValidDashboardFlavor(req.Flavor) {
		writeError(w, "flavor must be rhoai or odh", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Flavor)
	slog.Info("mutation", "op", "deploy-dashboard-main", "flavor", req.Flavor)
	result, err := cluster.DeployDashboardMainWithFlavor(c, req.Flavor)
	if err != nil {
		slog.Error("deploy dashboard main failed", "error", err)
		writeError(w, "deploy dashboard main failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "deploy-dashboard-main")
})
