package api

import (
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// runMaaS runs a MaaS operation; tests replace it.
var runMaaS = cluster.RunMaaS

// getMaaSStatus reads the MaaS feature; tests replace it.
var getMaaSStatus = cluster.GetMaaSStatus

// HandleMaaSStatus reports the Models as a Service feature: the platform
// part by part, and this tool's models with their publication.
var HandleMaaSStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	st, err := getMaaSStatus(c)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the MaaS state: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, st, "maas")
})

// checkMaaSGroups checks group names typed in a MaaS dialog; tests replace it.
var checkMaaSGroups = cluster.CheckMaaSGroups

// HandleMaaSGroups says which groups typed in a MaaS dialog
// (?name=a&name=b, at most 20) have no OpenShift Group, so the dialog warns
// before Publish. Only for a cluster-admin, who may read groups anyway:
// the check uses this tool's read of a group by its name, which other
// users may not have.
var HandleMaaSGroups = withAdminRead(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["name"]
	if len(names) == 0 || len(names) > 20 {
		writeError(w, "name 1 to 20 groups", http.StatusBadRequest, "validation")
		return
	}
	problems, why := checkMaaSGroups(c, names)
	if why != "" {
		writeError(w, why, http.StatusBadRequest, "validation")
		return
	}
	writeJSON(w, problems, "maas-groups")
})

// HandleMaaSStream sets up or removes the MaaS platform, or publishes or
// unpublishes this tool's model in a project, as one operation streaming
// its steps.
var HandleMaaSStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	// Up to 20 groups and 20 users (253 bytes each), a 2 KiB connection
	// URL and a limit: well under 32 KiB. The body is never logged: the URL
	// holds a password.
	var req types.MaaSRequest
	if !decodeBody(w, r, 32<<10, &req, false) {
		return
	}
	if problem := cluster.ValidateMaaSRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in maas handler", "error", rec)
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

	target := req.Action + " MaaS platform"
	if req.Project != "" {
		target = req.Action + " model in " + req.Project
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "maas", "action", req.Action, "project", req.Project)
	result, runErr := runMaaS(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("maas failed", "error", runErr)
	}
})
