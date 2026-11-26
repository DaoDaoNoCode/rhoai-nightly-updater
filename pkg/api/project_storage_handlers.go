package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Project S3 storage and its pipeline server (the non-admin design,
// section 4): project actions (withProjectMutationAuth) run with the
// user's own token in one of their data science projects.

// The project storage's reads and runs; tests replace them.
var (
	getProjectStorageStatus  = cluster.GetProjectStorageStatus
	runProjectStorage        = cluster.RunProjectStorage
	runProjectPipelineServer = cluster.RunProjectPipelineServer
)

// HandleProjectStorageStatus reports the S3 storage of one project
// (?project=), every object read with the user's own token (a viewer, who
// may not read Secrets, sees its Secret as unreadable). A user who is not
// a cluster-admin must name one of their own data science projects,
// checked with their token before anything else is read (a project that
// does not exist gets the same answer); a cluster-admin may name any.
var HandleProjectStorageStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if !namespaceRegex.MatchString(project) {
		writeError(w, "a valid project is required", http.StatusBadRequest, "validation")
		return
	}
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
	if !admin && !userProjectCheck(w, u, project, "Nothing was read.") {
		return
	}
	writeJSON(w, getProjectStorageStatus(u, project), "project-s3")
})

// HandleProjectStorageStream sets up, repairs or deletes the S3 storage
// of a project as one operation streaming its steps: setup and repair
// "check", "storage", "fence", "server", "ready"; delete "check" and
// "delete". Only an admin of the project (the gate's access review for
// create rolebindings there); a cluster-admin repairs and deletes, and is
// refused Set up. It holds the project's lock.
var HandleProjectStorageStream = withProjectMutationAuth(projectActionRoutes["/api/resources/project-s3/stream"], func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	var req types.ProjectStorageRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if problem := cluster.ValidateProjectStorageRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	if !beginProject(w, req.Action+" S3 storage in "+req.Project) {
		return
	}
	slog.Info("mutation", "op", "project-s3", "action", req.Action, "project", req.Project, "deleteData", req.DeleteData != "")
	_, runErr := streamOperation(w, "project-s3", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runProjectStorage(c, req, emit)
	})
	if runErr != nil {
		slog.Error("project-s3 failed", "error", runErr)
	}
})

// HandleProjectPipelineServerStream sets up or deletes the pipeline
// server of a project on the project's own S3 storage, streaming one step
// "pipeline-server". Admins and editors of the project; a cluster-admin
// deletes one and is refused Set up (their pipeline servers use the
// shared storage). It holds the project's lock.
var HandleProjectPipelineServerStream = withProjectMutationAuth(projectActionRoutes["/api/resources/project-pipeline-server/stream"], func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	var req types.ProjectPipelineServerRequest
	if err := readBody(w, r, pipelineServerBodyLimit, &req, false); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if problem := cluster.ValidateProjectPipelineServerRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	if !beginProject(w, req.Action+" pipeline server in "+req.Project) {
		return
	}
	slog.Info("mutation", "op", "project-pipeline-server", "action", req.Action, "project", req.Project, "confirmWorkbenches", len(req.ConfirmWorkbenches))
	_, runErr := streamOperation(w, "project-pipeline-server", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runProjectPipelineServer(c, req, emit)
	})
	if runErr != nil {
		slog.Error("project-pipeline-server failed", "error", runErr)
	}
})
