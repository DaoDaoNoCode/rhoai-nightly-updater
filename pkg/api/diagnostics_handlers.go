package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleDebug returns debug pod information.
var HandleDebug = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	debug, err := cluster.GetDebugInfo(c)
	if err != nil {
		slog.Error("debug error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to get debug info", code, errorCode)
		return
	}
	writeJSON(w, debug, "debug")
})

// HandleDiagnostics runs cluster health checks and returns a diagnostic report.
var HandleDiagnostics = withAuth(diagnosticsHandler(cluster.RunDiagnostics))

// HandleDiagnosticsGitOps is HandleDiagnostics in the gitops profile:
// without the pull-secret check (cluster.DiagnoseClusterGitOps).
var HandleDiagnosticsGitOps = withAuth(diagnosticsHandler(cluster.DiagnoseClusterGitOps))

func diagnosticsHandler(run func(*cluster.Client) (*cluster.DiagnosticsResponse, error)) func(*cluster.Client, http.ResponseWriter, *http.Request) {
	return func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "op", "diagnostics")
		result, err := run(c)
		if err != nil {
			slog.Error("diagnostics failed", "error", err)
			writeError(w, "diagnostics failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, result, "diagnostics")
	}
}

// HandleDiagnosticsFix attempts to auto-fix a specific problem identified by diagnostics.
var HandleDiagnosticsFix = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()

	var req types.DiagnosticsFixRequest
	if err := readBody(w, r, 4096, &req, false); err != nil || req.ProblemID == "" {
		writeError(w, "problemId is required", http.StatusBadRequest, "validation")
		return
	}

	beginOperation(w, req.ProblemID)
	slog.Info("mutation", "op", "diagnostics-fix", "problemId", req.ProblemID)

	result, err := cluster.ApplyFix(c, req.ProblemID)
	if err != nil {
		slog.Error("diagnostics-fix failed", "error", err, "problemId", req.ProblemID)
		writeError(w, fmt.Sprintf("Fix failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, result, "diagnostics-fix")
})
