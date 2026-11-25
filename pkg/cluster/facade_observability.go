package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/observability"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Observability: usage logs and dashboards (internal/observability), under
// the names pkg/api uses.

// GetObservabilityStatus is observability.GetStatus.
func GetObservabilityStatus(c *Client) (*types.ObservabilityStatus, error) {
	return observability.GetStatus(c)
}

// ValidateObservabilityRequest is observability.ValidateRequest.
func ValidateObservabilityRequest(req types.ObservabilityRequest) string {
	return observability.ValidateRequest(req)
}

// RunObservability is observability.Run.
func RunObservability(c *Client, req types.ObservabilityRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return observability.Run(c, req, onStep)
}
