package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleTestPullSecret tests whether the pull secret is valid.
var HandleTestPullSecret = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("mutation", "op", "test-pull-secret")

	result, err := cluster.TestPullSecret(c)
	if err != nil {
		slog.Error("test-pull-secret failed", "error", err)
		writeError(w, "test failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "test-pull-secret")
})

// HandleCreatePullSecret creates or updates the quay.io pull secret.
var HandleCreatePullSecret = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.CreatePullSecretRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if req.Auth == "" {
		writeError(w, "auth is required", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, "")
	slog.Info("mutation", "op", "create-pull-secret")

	result, err := cluster.CreatePullSecret(c, req.Auth)
	if err != nil {
		slog.Error("create-pull-secret failed", "error", err)
		writeError(w, "create pull secret failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "create-pull-secret")
})

// planNodeSetup and runNodeSetup are cluster.PlanNodeSetup and
// cluster.RunNodeSetup; tests replace them.
var (
	planNodeSetup = cluster.PlanNodeSetup
	runNodeSetup  = cluster.RunNodeSetup
)

// nodeSetupVersionRegex bounds the resourceVersion and UID a stream
// request sends back from its plan.
var nodeSetupVersionRegex = regexp.MustCompile(`^[A-Za-z0-9-]{0,64}$`)

// decodeNodeSetupRequest reads and checks a one-click setup request.
func decodeNodeSetupRequest(w http.ResponseWriter, r *http.Request) (types.NodeSetupRequest, bool) {
	var req types.NodeSetupRequest
	if !decodeBody(w, r, 1024, &req, false) {
		return req, false
	}
	if !cluster.ValidNodeSetupAction(req.Action) || !nodeSetupVersionRegex.MatchString(req.ResourceVersion) || !nodeSetupVersionRegex.MatchString(req.UID) {
		writeError(w, "Give action mirror-create, mirror-remove, pull-secret-add or pull-secret-remove", http.StatusBadRequest, "validation")
		return req, false
	}
	return req, true
}

// HandleNodeSetupPlan works out what a one-click setup action (the image
// mirror, the nodes' pull secret, and their removals) would do, with the
// user's own token and a server-side dry run. It changes nothing, so it
// does not use up the user's slot and writes no operation marker; it is a
// POST through withMutationAuth because it reads openshift-config/pull-secret
// with the user's token, which the ServiceAccount may not read.
var HandleNodeSetupPlan = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	req, ok := decodeNodeSetupRequest(w, r)
	if !ok {
		return
	}
	w.Header().Set("X-Skip-Rate-Limit", "true")
	setOperationTarget(w, req.Action)
	slog.Info("mutation", "op", "node-setup-plan", "action", req.Action)
	plan, err := planNodeSetup(c, req)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not work out the change: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, plan, "node-setup-plan")
})

// HandleNodeSetupStream runs a one-click setup action the user reviewed,
// streaming its steps.
var HandleNodeSetupStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	req, ok := decodeNodeSetupRequest(w, r)
	if !ok {
		return
	}
	if cluster.NodeSetupNeedsVersion(req.Action) && req.ResourceVersion == "" {
		writeError(w, "Give the resourceVersion of the plan you reviewed", http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in node setup handler", "error", rec)
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

	beginOperation(w, req.Action)
	slog.Info("mutation", "op", "node-setup", "action", req.Action)
	result, runErr := runNodeSetup(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("node setup failed", "action", req.Action, "error", runErr)
	}
})

// HandleVerifyNodes verifies cluster node readiness and registry access.
var HandleVerifyNodes = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("mutation", "op", "verify-nodes")

	result, err := cluster.VerifyNodeReadiness(c)
	if err != nil {
		slog.Error("verify-nodes failed", "error", err)
		writeError(w, "verification failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "verify-nodes")
})

// HandleNodePullCheck checks that the nodes can pull a nightly catalog
// image (cluster.CheckNodePull). It creates and removes a temporary
// CatalogSource, so it is gated and holds the lock like any change.
var HandleNodePullCheck = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.NodePullCheckRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if req.Image == "" {
		writeError(w, "image is required", http.StatusBadRequest, "validation")
		return
	}
	if !imageRegex.MatchString(req.Image) {
		writeError(w, fmt.Sprintf("invalid image format: %s", req.Image), http.StatusBadRequest, "validation")
		return
	}
	if !isAllowedImage(req.Image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", req.Image), http.StatusBadRequest, "validation")
		return
	}
	// A check, not a change: it does not use up the user's slot. It is an
	// operation all the same (a marker while it runs), because it creates
	// a CatalogSource that an interrupted check leaves behind until the
	// next operation removes it.
	w.Header().Set("X-Skip-Rate-Limit", "true")
	beginOperation(w, req.Image)
	slog.Info("mutation", "op", "node-pull-check", "image", req.Image)

	result, err := cluster.CheckNodePull(c, req.Image)
	if err != nil {
		slog.Error("node-pull-check failed", "error", err)
		writeError(w, "node pull check failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result, "node-pull-check")
})
