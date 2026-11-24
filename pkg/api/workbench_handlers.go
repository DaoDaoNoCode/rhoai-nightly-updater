package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// getWorkbenchStatus reads the Workbench feature; tests replace it.
var getWorkbenchStatus = cluster.GetWorkbenchStatus

// runWorkbench runs a Workbench operation; tests replace it.
var runWorkbench = cluster.RunWorkbench

// HandleWorkbenchStatus reports the Workbench feature (?project=): this
// tool's workbenches, the project's nightly-workbench and what Set up would
// create there (tier admin). Everything about a project is read with the
// admin's own token; the ServiceAccount reads only the DataScienceCluster,
// the dashboard's address and the tool's record.
var HandleWorkbenchStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project != "" && !namespaceRegex.MatchString(project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
	st, err := getWorkbenchStatus(c, u, project)
	if err != nil {
		writeUserReadError(w, "Cannot read the workbench state", err)
		return
	}
	writeJSON(w, st, "workbench")
})

// HandleWorkbenchStream sets up, starts, stops or deletes the workbench in a
// project as one operation streaming its steps: setup "check", "storage",
// "workbench"; start and stop "workbench"; teardown "workbench" and, with
// deleteStorage, "storage".
var HandleWorkbenchStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.WorkbenchRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if problem := cluster.ValidateWorkbenchRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Action+" workbench in "+req.Project)
	slog.Info("mutation", "op", "workbench", "action", req.Action, "project", req.Project, "deleteStorage", req.DeleteStorage)
	_, runErr := streamOperation(w, "workbench", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runWorkbench(c, req, emit)
	})
	if runErr != nil {
		slog.Error("workbench failed", "error", runErr)
	}
})
