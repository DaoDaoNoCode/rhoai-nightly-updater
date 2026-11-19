package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleComponents returns DSC component statuses and deployments.
var HandleComponents = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	includeLabels := r.URL.Query().Get("labels") == "true"
	writeComponents(w)(cluster.GetComponents(c, includeLabels))
})

// HandleComponentsGitOps is HandleComponents in the gitops profile: no image
// labels, no GitHub DSC sample (cluster.GetComponentsGitOps).
var HandleComponentsGitOps = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	writeComponents(w)(cluster.GetComponentsGitOps(c))
})

func writeComponents(w http.ResponseWriter) func(*types.ComponentsResponse, error) {
	return func(components *types.ComponentsResponse, err error) {
		if err != nil {
			slog.Error("components error", "error", err)
			code, errorCode := cluster.HTTPStatusForError(err)
			writeError(w, fmt.Sprintf("Could not read components: %v", err), code, errorCode)
			return
		}
		writeJSON(w, components, "components")
	}
}

// HandleAssistRollout detects and unblocks stuck deployment rollouts.
var HandleAssistRollout = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	// Optional body {"namespace":"...","deployment":"..."} limits the action
	// to one Deployment; an empty body checks all RHOAI Deployments.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Namespace  string `json:"namespace"`
		Deployment string `json:"deployment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}

	target := ""
	if req.Deployment != "" {
		target = req.Namespace + "/" + req.Deployment
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "assist-rollout", "namespace", req.Namespace, "deployment", req.Deployment)

	var result *types.OperationResponse
	var err error
	if req.Deployment != "" {
		result, err = cluster.AssistRolloutFor(c, req.Namespace, req.Deployment)
	} else {
		result, err = cluster.AssistRollout(c)
	}
	if err != nil {
		slog.Error("assist-rollout failed", "error", err)
		writeError(w, "assist-rollout failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "assist-rollout")
})
