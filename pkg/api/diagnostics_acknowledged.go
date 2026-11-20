package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// applyFixAcknowledged runs the fix; tests replace it.
var applyFixAcknowledged = cluster.ApplyFixAcknowledged

// HandleDiagnosticsFixAcknowledged runs a Diagnostics fix after the user
// confirmed the problem's acknowledgement statement (a Problem with
// "acknowledge" set, for example deleting a workload whose users the tool
// cannot list). The same fixes sent to /api/diagnostics/fix are refused
// with errorCode "acknowledgement_required". The body must say
// acknowledged: true, so a client cannot reach this by accident, and
// binding: the problem's acknowledgeBinding as the user reviewed it (the
// object's UID and resourceVersion and the hash of the statement). The fix
// refuses with errorCode "conflict" when it no longer matches what it would
// act on.
var HandleDiagnosticsFixAcknowledged = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	var req struct {
		ProblemID    string              `json:"problemId"`
		Acknowledged bool                `json:"acknowledged"`
		Binding      *cluster.AckBinding `json:"binding"`
	}
	if err := readBody(w, r, 4096, &req, false); err != nil || req.ProblemID == "" {
		writeError(w, "problemId is required", http.StatusBadRequest, "validation")
		return
	}
	if !req.Acknowledged {
		writeError(w, "acknowledged must be true; use /api/diagnostics/fix for fixes without an acknowledgement", http.StatusBadRequest, "validation")
		return
	}
	if req.Binding == nil || req.Binding.Hash == "" {
		writeError(w, "binding (the problem's acknowledgeBinding: uid, resourceVersion and hash) is required", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, req.ProblemID)
	slog.Info("mutation", "op", "diagnostics-fix", "problemId", req.ProblemID, "acknowledged", true, "uid", req.Binding.UID, "resourceVersion", req.Binding.ResourceVersion)

	result, err := applyFixAcknowledged(c, req.ProblemID, *req.Binding)
	if err != nil {
		slog.Error("diagnostics-fix failed", "error", err, "problemId", req.ProblemID)
		writeError(w, fmt.Sprintf("Fix failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if encErr := json.NewEncoder(w).Encode(result); encErr != nil {
		slog.Error("response encode error", "label", "diagnostics-fix", "error", encErr)
	}
})
