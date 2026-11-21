package cluster

import "github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/report"

// The diagnostics report types (internal/report), under the names
// pkg/api and this package use.
type (
	CheckResult         = report.CheckResult
	Problem             = report.Problem
	AckBinding          = report.AckBinding
	DiagnosticsResponse = report.DiagnosticsResponse
)
