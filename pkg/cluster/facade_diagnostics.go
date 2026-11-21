package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/diagnostics"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The diagnostics runner, its checks and fixes (internal/diagnostics), under
// the names pkg/api uses.

// RunDiagnostics is diagnostics.Run.
func RunDiagnostics(c *Client) (*DiagnosticsResponse, error) {
	return diagnostics.Run(c)
}

// DiagnoseClusterGitOps is diagnostics.DiagnoseClusterGitOps.
func DiagnoseClusterGitOps(c *Client) (*DiagnosticsResponse, error) {
	return diagnostics.DiagnoseClusterGitOps(c)
}

// ApplyFix is diagnostics.ApplyFix.
func ApplyFix(c *Client, problemID string) (*types.OperationResponse, error) {
	return diagnostics.ApplyFix(c, problemID)
}

// ApplyFixAcknowledged is diagnostics.ApplyFixAcknowledged.
func ApplyFixAcknowledged(c *Client, problemID string, binding AckBinding) (*types.OperationResponse, error) {
	return diagnostics.ApplyFixAcknowledged(c, problemID, binding)
}
