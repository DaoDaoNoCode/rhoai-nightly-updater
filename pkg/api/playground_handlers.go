package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// runPlayground runs a Playground operation; tests replace it.
var runPlayground = cluster.RunPlayground

// getPlaygroundStatus reads the Playground feature; tests replace it.
var getPlaygroundStatus = cluster.GetPlaygroundStatus

// HandlePlaygroundStatus reports the Gen AI playground feature: what it
// needs on the cluster, the playgrounds in the user's data science
// projects, and the models the given project's playground can use, read
// with the user's own token (tier user-token; cached briefly per user).
var HandlePlaygroundStatus = withUserRead(func(sa, u *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project != "" && !namespaceRegex.MatchString(project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	st, err := getPlaygroundStatus(sa, u, project)
	if errors.Is(err, cluster.ErrNotDSProject) {
		writeError(w, "Cannot read the playground state: "+err.Error()+".", http.StatusForbidden, cluster.ErrorCodeForbiddenForUser)
		return
	}
	if err != nil {
		writeUserReadError(w, "Cannot read the playground state", err)
		return
	}
	// What the ServiceAccount read is shown in full to cluster-admins
	// only; anyone else gets fixed values (cluster.PlaygroundStatusForUser).
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	if !admin {
		st = cluster.PlaygroundStatusForUser(st)
	}
	writeJSON(w, st, "playground")
})

// HandlePlaygroundStream sets up or deletes the Gen AI playground in a
// project through the RHOAI dashboard's own gen-ai API, as one operation
// streaming its steps: setup "check", "install", "ready"; teardown
// "delete", "gone".
var HandlePlaygroundStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.PlaygroundRequest
	if !decodeBody(w, r, 16384, &req, false) {
		return
	}
	if problem := cluster.ValidatePlaygroundRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in playground handler", "error", rec)
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

	beginOperation(w, req.Action+" playground in "+req.Project)
	slog.Info("mutation", "op", "playground", "action", req.Action, "project", req.Project, "models", len(req.Models))
	result, runErr := runPlayground(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("playground failed", "error", runErr)
	}
})
