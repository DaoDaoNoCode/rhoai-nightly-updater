package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func validateReinstallRequest(req *types.ReinstallRequest) error {
	if req.TargetType == "" {
		req.TargetType = "stable"
	}
	req.Image = strings.TrimSpace(req.Image)
	req.Channel = strings.TrimSpace(req.Channel)
	if req.TargetType != "stable" && req.TargetType != "nightly" && req.TargetType != "custom" {
		return fmt.Errorf("invalid targetType: %s (must be stable, nightly, or custom)", req.TargetType)
	}
	if req.TargetType != "stable" {
		if req.Image == "" {
			return fmt.Errorf("image is required to install a nightly or custom build")
		}
		if req.TargetType == "custom" {
			match := customFBCImageRegex.FindStringSubmatch(req.Image)
			if match == nil || (match[1] == "" && match[2] == "") {
				return fmt.Errorf("provide a quay.io/rhoai/rhoai-fbc-fragment image with a tag or a full SHA256 digest")
			}
		} else if !imageRegex.MatchString(req.Image) || !isAllowedImage(req.Image) {
			return fmt.Errorf("provide a valid FBC image from quay.io/rhoai/ or registry.redhat.io/rhoai/")
		}
	}
	if req.Channel != "" && !channelRegex.MatchString(req.Channel) {
		return fmt.Errorf("invalid channel name: %s", req.Channel)
	}
	return nil
}

// HandleStatus returns the current cluster and operator status.
var HandleStatus = withAuth(statusHandler(cluster.GetStatus))

// HandleStatusGitOps is HandleStatus in the gitops profile: it reads no
// Secret and contacts no registry (cluster.GetStatusGitOps).
var HandleStatusGitOps = withAuth(statusHandler(cluster.GetStatusGitOps))

func statusHandler(get func(*cluster.Client) (*types.StatusResponse, error)) func(*cluster.Client, http.ResponseWriter, *http.Request) {
	return func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		status, err := get(c)
		writeStatus(w, status, err)
	}
}

func writeStatus(w http.ResponseWriter, status *types.StatusResponse, err error) {
	if err != nil {
		slog.Error("status error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to get status", code, errorCode)
		return
	}
	writeJSON(w, status, "status")
}

// HandleUpdate validates a nightly catalog update with a server-side dry
// run. Real updates go through HandleUpdateStream, which reports progress.
var HandleUpdate = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in update handler", "error", r)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.UpdateRequest
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

	if !req.DryRun {
		writeError(w, "this endpoint only runs dry runs; use POST /api/update/stream to update", http.StatusBadRequest, "validation")
		return
	}
	w.Header().Set("X-Skip-Rate-Limit", "true")
	setOperationTarget(w, req.Image)

	slog.Info("mutation", "op", "update", "image", req.Image, "dryRun", true)

	result, err := runUpdateDryRun(c, req.Image)
	if err != nil {
		slog.Error("update dry run failed", "error", err)
		writeError(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "update")
})

// HandleUpdateStream applies a nightly catalog update and streams progress via SSE.
var HandleUpdateStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in update-stream handler", "error", rec)
		}
	}()

	req, ok := decodeUpdateStreamRequest(w, r)
	if !ok {
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, req.Image)
	slog.Info("mutation", "op", "update-stream", "image", req.Image)

	result, updateErr := runUpdateStream(c, req.Image, cluster.OperationOptions{RevertDashboardDev: req.RevertDashboardDev}, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, updateErr)

	if updateErr != nil {
		slog.Error("update-stream failed", "error", updateErr)
		RecordUpdate(false)
		return
	}

	RecordUpdate(result.Success)
})

// HandleUpdateGitOps is POST /api/update (the dry run) in the gitops profile:
// the profile's own dry run (cluster.UpdateDryRunGitOpsProfile), which only
// reads. Like HandleUpdate it refuses anything but a dry run.
var HandleUpdateGitOps = withGitOpsMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if req.Image == "" || !imageRegex.MatchString(req.Image) || !isAllowedImage(req.Image) {
		writeError(w, "provide a valid FBC image from quay.io/rhoai/ or registry.redhat.io/rhoai/", http.StatusBadRequest, "validation")
		return
	}
	if !req.DryRun {
		writeError(w, "this endpoint only runs dry runs; use POST /api/update/stream to update", http.StatusBadRequest, "validation")
		return
	}
	w.Header().Set("X-Skip-Rate-Limit", "true")
	setOperationTarget(w, req.Image)
	slog.Info("mutation", "op", "update", "image", req.Image, "dryRun", true, "profile", string(ProfileGitOps))
	result, err := runUpdateDryRunGitOps(c, req.Image)
	if err != nil {
		slog.Error("update dry run failed", "error", err)
		writeError(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "update")
})

// HandleUpdateStreamGitOps is POST /api/update/stream in the gitops profile
// (gitOpsRoutes): Update's Argo CD path only
// (cluster.UpdateStreamGitOpsProfile). It takes no confirmations: ending a
// Dashboard Dev session (revertDashboardDev) is not part of the profile, so
// an active session is a refusal there. It runs only while the profile's
// admission policy is in place (refuseWithoutGitOpsPolicy).
var HandleUpdateStreamGitOps = withGitOpsMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if refuseWithoutGitOpsPolicy(c, w) {
		return
	}
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in gitops update-stream handler", "error", rec)
		}
	}()

	req, ok := decodeUpdateStreamRequest(w, r)
	if !ok {
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, req.Image)
	slog.Info("mutation", "op", "update-stream", "image", req.Image, "profile", string(ProfileGitOps))

	result, updateErr := runUpdateStreamGitOps(c, req.Image, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, updateErr)

	if updateErr != nil {
		slog.Error("update-stream failed", "error", updateErr)
		RecordUpdate(false)
		return
	}

	RecordUpdate(result.Success)
})

// decodeUpdateStreamRequest reads and validates an Update request body. It
// answers 400 and returns false when the request must not proceed.
func decodeUpdateStreamRequest(w http.ResponseWriter, r *http.Request) (types.UpdateRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return req, false
	}
	if req.Image == "" {
		writeError(w, "image is required", http.StatusBadRequest, "validation")
		return req, false
	}
	if !imageRegex.MatchString(req.Image) {
		writeError(w, fmt.Sprintf("invalid image format: %s", req.Image), http.StatusBadRequest, "validation")
		return req, false
	}
	if !isAllowedImage(req.Image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", req.Image), http.StatusBadRequest, "validation")
		return req, false
	}
	return req, true
}

// HandleReinstallStream performs the full uninstall/cleanup/reinstall flow
// and streams progress via SSE.
var HandleReinstallStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in reinstall-stream handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req types.ReinstallRequest
	// Preserve the legacy empty-body stable rollback, but reject malformed JSON.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	if err := validateReinstallRequest(&req); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest, "validation")
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	target := req.TargetType
	if req.Image != "" {
		target += " " + req.Image
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "reinstall-stream", "targetType", req.TargetType, "image", req.Image)

	opts := cluster.OperationOptions{AllowDowngrade: req.AllowDowngrade, RevertDashboardDev: req.RevertDashboardDev}
	result, reinstallErr := runReinstallStream(c, req.TargetType, req.Image, req.Channel, opts, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, reinstallErr)

	if reinstallErr != nil {
		slog.Error("reinstall-stream failed", "error", reinstallErr)
		return
	}

	if req.TargetType == "stable" {
		RecordRollback()
	} else {
		RecordReinstall()
	}
	_ = result
})

// HandleRefreshStream deletes the current CSV and Subscription to trigger OLM
// to reinstall with updated images, streaming progress via SSE.
var HandleRefreshStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in refresh-stream handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, "")
	slog.Info("mutation", "op", "refresh-stream")

	result, refreshErr := runRefreshStream(c, cluster.OperationOptions{RevertDashboardDev: req.RevertDashboardDev}, forwardSteps(sseWriter))

	sendOperationResult(sseWriter, result, refreshErr)

	if refreshErr != nil {
		slog.Error("refresh-stream failed", "error", refreshErr)
		return
	}
	_ = result
})
