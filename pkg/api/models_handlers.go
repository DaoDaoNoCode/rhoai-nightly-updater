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
	var parts []string
	if req.Gateway {
		parts = append(parts, "inference gateway")
	}
	if req.Project != "" {
		parts = append(parts, "model in "+req.Project)
	}
	beginOperation(w, req.Action+" "+strings.Join(parts, ", "))
	slog.Info("mutation", "op", "models", "action", req.Action, "gateway", req.Gateway, "project", req.Project, "releasedImage", req.ReleasedImage)
	_, runErr := streamOperation(w, "models", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runModels(c, req, emit)
	})
	if runErr != nil {
		slog.Error("models failed", "error", runErr)
	}
})

// runProjectModel runs a project's model action; tests replace it.
var runProjectModel = cluster.RunProjectModel

// HandleProjectModelsStream sets up or deletes a model of the catalog in
// one project, as one operation streaming its steps: setup "check",
// "model", "model-ready", "callable"; teardown "model". A project action
// (withProjectMutationAuth): a user who is not a cluster-admin may run it
// in one of their projects when their RBAC allows it; it holds the
// project's lock and changes nothing outside the project.
var HandleProjectModelsStream = withProjectMutationAuth(projectActionRoutes["/api/resources/models/project/stream"], func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	var req types.ProjectModelRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if problem := cluster.ValidateProjectModelRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	if !beginProject(w, req.Action+" model in "+req.Project) {
		return
	}
	slog.Info("mutation", "op", "project-model", "action", cluster.ProjectModelAction(req), "project", req.Project, "model", req.Model, "exposure", req.Exposure)
	_, runErr := streamOperation(w, "models", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runProjectModel(c, req, emit)
	})
	if runErr != nil {
		slog.Error("project model failed", "error", runErr)
	}
})

// getModelExposures reads the ways a model can be reached; tests replace it.
var getModelExposures = cluster.GetModelExposures

// getProjectModelExposures reads a user's answer; tests replace it.
var getProjectModelExposures = cluster.ProjectModelExposures

// HandleModelExposures reports the ways a catalog model can be reached from
// a project (?project=&model=), with the one this tool recommends (tier
// per-role). A cluster-admin gets every way the Models tab offers. A user
// who is not one must name one of their own data science projects,
// checked with their token before anything is read with the
// ServiceAccount (a project that does not exist gets the same answer), and
// gets the ways a project model action offers: inside the cluster, and
// the inference gateway when it already admits the project, as a yes or
// no for that project only (cluster.ProjectModelExposures).
var HandleModelExposures = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project, model := r.URL.Query().Get("project"), r.URL.Query().Get("model")
	if !namespaceRegex.MatchString(project) || !cluster.ValidModelName(model) {
		writeError(w, "a valid project and a model of the catalog are required", http.StatusBadRequest, "validation")
		return
	}
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	read := getModelExposures
	if !admin {
		u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
		if !userProjectCheck(w, u, project, "Nothing was read.") {
			return
		}
		read = func(_ *cluster.Client, project, model string) (*types.ModelExposures, string, string) {
			return getProjectModelExposures(u, project, model)
		}
	}
	ex, msg, code := read(c, project, model)
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
