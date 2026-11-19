package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// runModels runs a Models operation; tests replace it.
var runModels = cluster.RunModels

// HandleModelsStream sets up or deletes the inference gateway and a model
// in a project (Environment page, Models tab), as one operation streaming
// a step per part: setup checks every part and dry-runs every write
// first, then creates the gateway, waits for it, creates the model and
// waits until it serves; teardown deletes the model, then the gateway.
var HandleModelsStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.ModelsRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if req.Project != "" && !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	if problem := cluster.ValidateModelsRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in models handler", "error", rec)
		}
	}()
	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	defer sseWriter.Close()
	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	var parts []string
	if req.Gateway {
		parts = append(parts, "inference gateway")
	}
	if req.Project != "" {
		parts = append(parts, "model in "+req.Project)
	}
	beginOperation(w, req.Action+" "+strings.Join(parts, ", "))
	slog.Info("mutation", "op", "models", "action", req.Action, "gateway", req.Gateway, "project", req.Project, "releasedImage", req.ReleasedImage)
	result, runErr := runModels(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("models failed", "error", runErr)
	}
})

// getModelExposures reads the ways a model can be reached; tests replace it.
var getModelExposures = cluster.GetModelExposures

// HandleModelExposures reports the ways a catalog model can be reached from
// a project (?project=&model=), with the one this tool recommends.
var HandleModelExposures = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project, model := r.URL.Query().Get("project"), r.URL.Query().Get("model")
	if !namespaceRegex.MatchString(project) || !cluster.ValidModelName(model) {
		writeError(w, "a valid project and a model of the catalog are required", http.StatusBadRequest, "validation")
		return
	}
	ex, msg, code := getModelExposures(c, project, model)
	if msg != "" {
		status := http.StatusBadGateway
		switch code {
		case "validation":
			status = http.StatusBadRequest
		case "forbidden":
			status = http.StatusForbidden
		case "terminating":
			status = http.StatusConflict
		}
		writeError(w, msg, status, code)
		return
	}
	writeJSON(w, ex, "model exposures")
})
