package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

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
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.CreatePullSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
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
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.NodePullCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
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
