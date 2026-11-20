package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Components page's reads. Tests replace these.
var (
	getComponents       = cluster.GetComponents
	getComponentsGitOps = cluster.GetComponentsGitOps
)

// HandleComponents returns DSC component statuses and deployments. The
// route is admin-only (getRouteTiers: adminOnly runs before it), so the
// client carries the signed-in cluster-admin's own client
// (attachSignedInUser), as the plan's does (withAdminRead): the repair
// preview's removal checks list the objects of CRDs whose conversion
// webhook a component serves with it.
var HandleComponents = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	includeLabels := r.URL.Query().Get("labels") == "true"
	attachSignedInUser(c, r)
	writeComponents(w)(getComponents(c, includeLabels))
})

// HandleComponentsGitOps is HandleComponents in the gitops profile: no image
// labels, no GitHub DSC sample (cluster.GetComponentsGitOps). It runs the
// repair preview too, but its client does not carry the user's: nothing in
// that profile acts with the user's token, whose scopes (user:info
// user:check-access, docs/GITOPS.md "Sign-in in the gitops profile")
// cannot list objects anyway, and the profile has no repair. So the
// preview's conversion-webhook check refuses (fail closed).
var HandleComponentsGitOps = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	writeComponents(w)(getComponentsGitOps(c))
})

func writeComponents(w http.ResponseWriter) func(*types.ComponentsResponse, error) {
	return func(components *types.ComponentsResponse, err error) {
		if err != nil {
			slog.Error("components error", "error", err)
			code, errorCode := cluster.HTTPStatusForError(err)
			writeError(w, fmt.Sprintf("Could not read components: %v", err), code, errorCode)
			return
		}
		writeJSON(w, components, "components")
	}
}

// HandleAssistRollout detects and unblocks stuck deployment rollouts.
var HandleAssistRollout = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	// Optional body {"namespace":"...","deployment":"..."} limits the action
	// to one Deployment; an empty body checks all RHOAI Deployments.
	var req struct {
		Namespace  string `json:"namespace"`
		Deployment string `json:"deployment"`
	}
	if !decodeBody(w, r, 4096, &req, true) {
		return
	}

	target := ""
	if req.Deployment != "" {
		target = req.Namespace + "/" + req.Deployment
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "assist-rollout", "namespace", req.Namespace, "deployment", req.Deployment)

	var result *types.OperationResponse
	var err error
	if req.Deployment != "" {
		result, err = cluster.AssistRolloutFor(c, req.Namespace, req.Deployment)
	} else {
		result, err = cluster.AssistRollout(c)
	}
	if err != nil {
		slog.Error("assist-rollout failed", "error", err)
		writeOperationError(w, "assist-rollout failed", err)
		return
	}
	writeOperationResult(w, result, "assist-rollout")
})
