package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleResourcesStatus returns the state of the Environment page (tier
// per-role).
//
// A cluster-admin gets the cluster-wide status (S3 storage, MLflow, the
// inference gateway, every data science project's pipeline servers and
// models), read by the ServiceAccount; ?project= is the Pipelines tab's
// selected project, whose Connection is read too.
//
// Anyone else must name one of their projects (?project=) and gets
// types.ProjectStatus: that project's objects, read with their own token
// (cluster.GetProjectStatus). Without a project the answer is 403
// admin_only: the cluster-wide status is admin-only.
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
	// An invalid name reads nothing more.
	if !namespaceRegex.MatchString(project) {
		project = ""
	}
	status, err := cluster.GetResourcesStatusFor(c, project)
	if err != nil {
		slog.Error("resources status error", "error", err)
		writeReadError(w, "failed to get resources status", err)
		return
	}
	writeJSON(w, status, "resources-status")
})

// HandleDSProjects lists the user's data science projects, read with
// their own token through the OpenShift projects API (tier user-token;
// cluster.ListUserDSProjects): a cluster-admin sees every project, anyone
// else the projects they may see.
var HandleDSProjects = withUserRead(func(_, u *cluster.Client, w http.ResponseWriter, r *http.Request) {
	projects, err := listUserDSProjects(u)
	if err != nil {
		slog.Warn("ds-projects error", "error", err)
		writeUserReadError(w, "Cannot list your data science projects", err)
		return
	}
	writeJSON(w, map[string]any{"projects": projects}, "ds-projects")
})

// getProjectStatus reads a user's project; tests replace it.
var getProjectStatus = cluster.GetProjectStatus

// listUserDSProjects lists the user's projects; tests replace it.
var listUserDSProjects = cluster.ListUserDSProjects

// runPrerequisites sets up a feature's prerequisites; tests replace it.
var runPrerequisites = cluster.RunPrerequisites

// getPrerequisitesStatus reads every feature's prerequisites; tests replace it.
var getPrerequisitesStatus = cluster.GetPrerequisitesStatusFor

// HandlePrerequisitesStatus reports what each Environment feature needs on
// the cluster (DataScienceCluster components, operators) and what Set up
// would change.
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

// HandlePrerequisitesStream sets up what one feature needs on the cluster,
// as one operation streaming its steps.
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
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in prerequisites handler", "error", rec)
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

	beginOperation(w, req.Feature+" prerequisites")
	slog.Info("mutation", "op", "prerequisites", "feature", req.Feature)
	result, runErr := runPrerequisites(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("prerequisites failed", "error", runErr)
	}
})

// runCleanUp runs a clean-up; tests replace it.
var runCleanUp = cluster.CleanUp

// maxCleanupProjects bounds the pipeline servers one clean-up deletes.
const maxCleanupProjects = 200

// HandleCleanupStream deletes what this tool created, as one operation
// (one lock, one rate-limit slot): the pipeline servers in the given
// projects, then the S3 storage, then MLflow, then the models in the given
// projects, then the inference gateway, streaming each part via SSE.
// A clean-up from the browser as separate requests would hit the per-user
// rate limit on the second pipeline server.
var HandleCleanupStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.CleanupRequest
	if !decodeBody(w, r, 16<<10, &req, false) {
		return
	}
	if len(req.PipelineServers) > maxCleanupProjects || len(req.Models) > maxCleanupProjects || len(req.Playgrounds) > maxCleanupProjects || len(req.MaaSPublished) > maxCleanupProjects {
		writeError(w, fmt.Sprintf("at most %d pipeline servers, models and playgrounds each per clean-up", maxCleanupProjects), http.StatusBadRequest, "validation")
		return
	}
	for _, list := range [][]string{req.PipelineServers, req.Models, req.Playgrounds, req.MaaSPublished} {
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
	if len(req.PipelineServers) == 0 && !req.Storage && !req.MLflow && len(req.Models) == 0 && !req.InferenceGateway && len(req.Playgrounds) == 0 && len(req.MaaSPublished) == 0 && !req.MaaSPlatform && len(req.Components) == 0 {
		writeError(w, "nothing to clean up was selected", http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in cleanup handler", "error", rec)
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
	if len(req.Components) > 0 {
		target = append(target, "DataScienceCluster "+strings.Join(req.Components, ", "))
	}
	beginOperation(w, strings.Join(target, ", "))
	slog.Info("mutation", "op", "cleanup", "pipelineServers", req.PipelineServers, "storage", req.Storage, "deleteNamespace", req.DeleteNamespace, "mlflow", req.MLflow, "models", req.Models, "inferenceGateway", req.InferenceGateway, "playgrounds", req.Playgrounds, "maasPublished", req.MaaSPublished, "maasPlatform", req.MaaSPlatform, "components", req.Components)

	result, cleanupErr := runCleanUp(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, cleanupErr)
	if cleanupErr != nil {
		slog.Error("cleanup failed", "error", cleanupErr)
	}
})
