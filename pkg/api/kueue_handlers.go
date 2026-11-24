package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// getKueueStatus reads the Distributed workloads feature; tests replace it.
var getKueueStatus = cluster.GetKueueStatus

// runKueue runs a Distributed workloads operation; tests replace it.
var runKueue = cluster.RunKueue

// HandleKueueStatus reports the Distributed workloads feature (?project=):
// Kueue on the cluster, this tool's queues, project label, samples and the
// dashboard opt-in (tier admin). Everything but the DataScienceCluster and
// the dashboard's address is read with the admin's own token.
var HandleKueueStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project != "" && !namespaceRegex.MatchString(project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	u := userReadClient(r.Context(), c, extractUserToken(r), resolveUsername(r))
	st, err := getKueueStatus(c, u, project)
	if err != nil {
		writeUserReadError(w, "Cannot read distributed workloads", err)
		return
	}
	writeJSON(w, st, "kueue")
})

// HandleKueueStream sets up, deletes or changes the tool's distributed
// workloads as one operation streaming its steps: setup "check", "queues",
// "project", "samples"; teardown "samples", "queues", "project", "opt-in";
// delete-sample "samples"; optin "check", "dashboard", "profile"; optout
// "opt-in".
var HandleKueueStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.KueueRequest
	if !decodeBody(w, r, 2048, &req, false) {
		return
	}
	if problem := cluster.ValidateKueueRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	target := "distributed workloads"
	if req.Project != "" {
		target += " in " + req.Project
	}
	beginOperation(w, req.Action+" "+target)
	slog.Info("mutation", "op", "kueue", "action", req.Action, "project", req.Project, "rayCluster", req.RayCluster, "rayJob", req.RayJob, "sample", req.Sample)
	_, runErr := streamOperation(w, "kueue", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runKueue(c, req, emit)
	})
	if runErr != nil {
		slog.Error("kueue operation failed", "error", runErr)
	}
})
