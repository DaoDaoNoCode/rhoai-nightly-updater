package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// getWorkbenchStatus reads the Workbench feature; tests replace it.
var getWorkbenchStatus = cluster.GetWorkbenchStatus

// getWorkbenchProjectStatus reads one project's Workbench feature for a
// user who is not a cluster-admin; tests replace it.
var getWorkbenchProjectStatus = cluster.GetWorkbenchProjectStatus

// runWorkbench runs a Workbench operation; tests replace it.
var runWorkbench = cluster.RunWorkbench

// HandleWorkbenchStatus reports the Workbench feature (?project=): this
// tool's workbenches, the project's nightly-workbench and what Set up would
// create there (tier per-role). Everything about a project is read with the
// user's own token; the ServiceAccount reads only the DataScienceCluster,
// the dashboard's address and the tool's record.
//
// A user who is not a cluster-admin must name one of their own data
// science projects, checked with their token before anything is read with
// the ServiceAccount (a project that does not exist gets the same answer),
// and gets that project's workbench only: the tool's workbenches in other
// projects are left out.
var HandleWorkbenchStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project != "" && !namespaceRegex.MatchString(project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
	if !admin {
		if project == "" {
			writeError(w, "a valid project is required", http.StatusBadRequest, "validation")
			return
		}
		if !userProjectCheck(w, u, project, "Nothing was read.") {
			return
		}
	}
	read := getWorkbenchStatus
	if !admin {
		// Only their project is read: nothing in the tool's others.
		read = getWorkbenchProjectStatus
	}
	st, err := read(c, u, project)
	if err != nil {
		writeUserReadError(w, "Cannot read the workbench state", err)
		return
	}
	if !admin {
		st = workbenchStatusForUser(st, project)
	}
	writeJSON(w, st, "workbench")
})

// workbenchStatusForUser is st reduced to project: its item, its plan and
// the cluster's block; nothing about other projects.
func workbenchStatusForUser(st *types.WorkbenchStatus, project string) *types.WorkbenchStatus {
	out := *st
	out.Items = nil
	for _, it := range st.Items {
		if it.Namespace == project {
			out.Items = append(out.Items, it)
		}
	}
	out.ItemsUnreadable = nil
	out.ItemsProblem = ""
	for _, p := range st.ItemsUnreadable {
		if p == project {
			out.ItemsUnreadable = []string{project}
			out.ItemsProblem = st.PlanProblem
		}
	}
	return &out
}

// HandleWorkbenchStream sets up, starts, stops or deletes the workbench in a
// project as one operation streaming its steps: setup "check", "storage",
// "workbench"; start and stop "workbench"; teardown "workbench" and, with
// deleteStorage, "storage". A project action (withProjectMutationAuth): a
// user who is not a cluster-admin may run it in one of their projects when
// their RBAC allows it; it holds the project's lock.
var HandleWorkbenchStream = withProjectMutationAuth(projectActionRoutes["/api/resources/workbench/stream"], func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	var req types.WorkbenchRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if problem := cluster.ValidateWorkbenchRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	if !beginProject(w, req.Action+" workbench in "+req.Project) {
		return
	}
	slog.Info("mutation", "op", "workbench", "action", req.Action, "project", req.Project, "deleteStorage", req.DeleteStorage)
	_, runErr := streamOperation(w, "workbench", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runWorkbench(c, req, emit)
	})
	if runErr != nil {
		slog.Error("workbench failed", "error", runErr)
	}
})
