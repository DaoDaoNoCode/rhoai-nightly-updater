package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleResourcesStatus serves Environment-page state broken down by auth
// tier and role.
//
// Cluster admins receive status for the entire cluster—S3 storage, MLflow,
// the inference gateway, and pipeline servers plus models in every data
// science project—assembled via the ServiceAccount. The Pipelines tab's
// ?project= selection determines which project's Connection is included.
//
// Non-admins must supply one of their own projects (?project=) and receive
// types.ProjectStatus for that namespace using their token
// (cluster.GetProjectStatus). Omitting ?project= yields 403 admin_only
// because the cluster-wide view is restricted to admins.
var HandleResourcesStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	if !admin {
		if project == "" {
			writeError(w, "Only cluster-admins can see the whole cluster's Environment. Choose one of your projects.", http.StatusForbidden, adminOnlyCode)
			return
		}
		if !namespaceRegex.MatchString(project) {
			writeError(w, "invalid project name", http.StatusBadRequest, "validation")
			return
		}
		u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
		st, err := getProjectStatus(c, u, project)
		if errors.Is(err, cluster.ErrNotDSProject) {
			writeError(w, "Cannot read the project: "+err.Error()+".", http.StatusForbidden, cluster.ErrorCodeForbiddenForUser)
			return
		}
		if err != nil {
			writeUserReadError(w, "Cannot read the project", err)
			return
		}
		writeJSON(w, st, "project-status")
		return
	}
	// Bad project names are ignored rather than queried.
	if !namespaceRegex.MatchString(project) {
		project = ""
	}
	// The workbenches that use the selected project's Connection are read
	// with the admin's own token (the ServiceAccount may not list
	// Notebooks).
	var u *cluster.Client
	if project != "" {
		u = userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
	}
	status, err := cluster.GetResourcesStatusFor(c, u, project)
	if err != nil {
		slog.Error("resources status error", "error", err)
		writeReadError(w, "failed to get resources status", err)
		return
	}
	writeJSON(w, status, "resources-status")
})

// HandleDSProjects returns data science projects visible to the caller,
// fetched with the user's token through OpenShift's projects API (tier
// user-token; cluster.ListUserDSProjects). Admins see all projects; other
// users see only what they are allowed to access.
var HandleDSProjects = withUserRead(func(_, u *cluster.Client, w http.ResponseWriter, r *http.Request) {
	projects, err := listUserDSProjects(u)
	if err != nil {
		slog.Warn("ds-projects error", "error", err)
		writeUserReadError(w, "Cannot list your data science projects", err)
		return
	}
	writeJSON(w, map[string]any{"projects": projects}, "ds-projects")
})

// getProjectStatus loads project status for a user; tests may stub this.
var getProjectStatus = cluster.GetProjectStatus

// listUserDSProjects enumerates the caller's projects; tests may stub this.
var listUserDSProjects = cluster.ListUserDSProjects

// runPrerequisites applies prerequisite setup for a feature; tests may stub this.
var runPrerequisites = cluster.RunPrerequisites

// getPrerequisitesStatus gathers prerequisite state for all features; tests may stub this.
var getPrerequisitesStatus = cluster.GetPrerequisitesStatusFor

// HandlePrerequisitesStatus describes cluster prerequisites for each
// Environment feature (DataScienceCluster components, operators) and what
// Set up would modify.
var HandlePrerequisitesStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	exposure := r.URL.Query().Get("exposure")
	if !cluster.ValidExposure(exposure) {
		writeError(w, "exposure must be default-gateway, maas, gateway or internal", http.StatusBadRequest, "validation")
		return
	}
	st, err := getPrerequisitesStatus(c, exposure)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the prerequisites: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, st, "prerequisites")
})

// HandlePrerequisitesStream performs prerequisite setup for a single feature
// as one streamed operation reporting progress step by step.
var HandlePrerequisitesStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.PrerequisitesRequest
	if !decodeBody(w, r, 1024, &req, false) {
		return
	}
	if problem := cluster.ValidatePrerequisitesRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Feature+" prerequisites")
	slog.Info("mutation", "op", "prerequisites", "feature", req.Feature)
	_, runErr := streamOperation(w, "prerequisites", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runPrerequisites(c, req, emit)
	})
	if runErr != nil {
		slog.Error("prerequisites failed", "error", runErr)
	}
})

// runCleanUp executes clean-up work; tests may stub this.
var runCleanUp = cluster.CleanUp

// maxCleanupProjects limits how many pipeline servers a single clean-up may remove.
const maxCleanupProjects = 200

// cleanupBodyLimit fits the largest valid clean-up: every list at its
// bound, maxConfirmedContents entries, and maxConfirmWorkbenches workbench
// confirmations with the longest names and UIDs (about 460 KiB).
const cleanupBodyLimit = 512 << 10

// maxConfirmedContents caps total confirmed content entries across recipe projects.
const maxConfirmedContents = 500

// maxUIDLength limits UID length in clean-up requests (Kubernetes UIDs are
// 36-character UUIDs).
const maxUIDLength = 128

// HandleCleanupStream removes resources this tool provisioned in one locked,
// rate-limited operation: pipeline servers in named projects, then S3 storage,
// MLflow, models in those projects, then the inference gateway, with each
// phase streamed over SSE. Splitting clean-up into multiple browser requests
// would exhaust the per-user rate limit before the second pipeline server.
var HandleCleanupStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.CleanupRequest
	if !decodeBody(w, r, cleanupBodyLimit, &req, false) {
		return
	}
	if len(req.PipelineServers) > maxCleanupProjects || len(req.Models) > maxCleanupProjects || len(req.Playgrounds) > maxCleanupProjects || len(req.MaaSPublished) > maxCleanupProjects || len(req.RecipeProjects) > maxCleanupProjects {
		writeError(w, fmt.Sprintf("at most %d pipeline servers, models, playgrounds and recipe projects each per clean-up", maxCleanupProjects), http.StatusBadRequest, "validation")
		return
	}
	for _, list := range [][]string{req.PipelineServers, req.Models, req.Playgrounds, req.MaaSPublished, req.RecipeProjects} {
		seen := map[string]bool{}
		for _, p := range list {
			if !namespaceRegex.MatchString(p) {
				writeError(w, "invalid project name", http.StatusBadRequest, "validation")
				return
			}
			if seen[p] {
				writeError(w, "a project is listed twice", http.StatusBadRequest, "validation")
				return
			}
			seen[p] = true
		}
	}
	// Recipe projects are removed only when the submitted UID matches what
	// the user confirmed on screen: missing UIDs and UIDs for unlisted
	// projects are rejected (fail closed—no work starts).
	for _, project := range req.RecipeProjects {
		if req.RecipeProjectUIDs[project] == "" {
			writeError(w, fmt.Sprintf("recipe project %s has no UID in recipeProjectUIDs: reload the page and try again", project), http.StatusBadRequest, "validation")
			return
		}
	}
	for project, uid := range req.RecipeProjectUIDs {
		if !slices.Contains(req.RecipeProjects, project) || len(uid) > maxUIDLength {
			writeError(w, "recipeProjectUIDs names only projects of recipeProjects, each with its UID", http.StatusBadRequest, "validation")
			return
		}
	}
	confirmed := 0
	for project, contents := range req.ConfirmProjectContents {
		confirmed += len(contents)
		if !slices.Contains(req.RecipeProjects, project) || confirmed > maxConfirmedContents {
			writeError(w, fmt.Sprintf("confirmProjectContents names only projects of recipeProjects, with at most %d entries in all", maxConfirmedContents), http.StatusBadRequest, "validation")
			return
		}
		for _, c := range contents {
			if c == "" || len(c) > 300 {
				writeError(w, "invalid confirmProjectContents entry", http.StatusBadRequest, "validation")
				return
			}
		}
	}
	workbenches := 0
	for project, confirm := range req.ConfirmWorkbenches {
		workbenches += len(confirm)
		if !slices.Contains(req.PipelineServers, project) || workbenches > maxConfirmWorkbenches || !validConfirmWorkbenches(confirm) {
			writeError(w, fmt.Sprintf("confirmWorkbenches names only projects of pipelineServers, with at most %d workbench names and their UIDs in all", maxConfirmWorkbenches), http.StatusBadRequest, "validation")
			return
		}
	}
	if req.DeleteNamespace && !req.Storage {
		writeError(w, "deleteNamespace needs storage", http.StatusBadRequest, "validation")
		return
	}
	seenComponents := map[string]bool{}
	for _, k := range req.Components {
		if !cluster.RestorableComponentKey(k) || seenComponents[k] {
			writeError(w, "components must be distinct DataScienceCluster components this tool turns on", http.StatusBadRequest, "validation")
			return
		}
		seenComponents[k] = true
	}
	if len(req.PipelineServers) == 0 && !req.Storage && !req.MLflow && !req.ModelRegistry && len(req.Models) == 0 && !req.InferenceGateway && len(req.Playgrounds) == 0 && len(req.MaaSPublished) == 0 && !req.MaaSPlatform && len(req.Components) == 0 && len(req.RecipeProjects) == 0 {
		writeError(w, "nothing to clean up was selected", http.StatusBadRequest, "validation")
		return
	}
	var target []string
	if n := len(req.MaaSPublished); n > 0 {
		target = append(target, fmt.Sprintf("%d MaaS %s", n, map[bool]string{true: "publication", false: "publications"}[n == 1]))
	}
	if n := len(req.PipelineServers); n > 0 {
		target = append(target, fmt.Sprintf("%d pipeline %s", n, map[bool]string{true: "server", false: "servers"}[n == 1]))
	}
	if req.Storage {
		target = append(target, "S3 storage")
		if req.DeleteNamespace {
			target = append(target, "namespace seaweedfs")
		}
	}
	if req.MLflow {
		target = append(target, "MLflow")
	}
	if req.ModelRegistry {
		target = append(target, "model registry")
	}
	if n := len(req.Playgrounds); n > 0 {
		target = append(target, fmt.Sprintf("%d %s", n, map[bool]string{true: "playground", false: "playgrounds"}[n == 1]))
	}
	if n := len(req.Models); n > 0 {
		target = append(target, fmt.Sprintf("%d %s", n, map[bool]string{true: "model", false: "models"}[n == 1]))
	}
	if req.InferenceGateway {
		target = append(target, "inference gateway")
	}
	if req.MaaSPlatform {
		target = append(target, "MaaS platform")
	}
	if n := len(req.RecipeProjects); n > 0 {
		target = append(target, map[bool]string{true: "recipe project ", false: "recipe projects "}[n == 1]+strings.Join(req.RecipeProjects, ", "))
	}
	if len(req.Components) > 0 {
		target = append(target, "DataScienceCluster "+strings.Join(req.Components, ", "))
	}
	beginOperation(w, strings.Join(target, ", "))
	slog.Info("mutation", "op", "cleanup", "pipelineServers", req.PipelineServers, "storage", req.Storage, "deleteNamespace", req.DeleteNamespace, "mlflow", req.MLflow, "modelRegistry", req.ModelRegistry, "models", req.Models, "inferenceGateway", req.InferenceGateway, "playgrounds", req.Playgrounds, "maasPublished", req.MaaSPublished, "maasPlatform", req.MaaSPlatform, "components", req.Components, "recipeProjects", req.RecipeProjects, "recipeProjectUIDs", req.RecipeProjectUIDs)

	_, cleanupErr := streamOperation(w, "cleanup", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runCleanUp(c, req, emit)
	})
	if cleanupErr != nil {
		slog.Error("cleanup failed", "error", cleanupErr)
	}
})
