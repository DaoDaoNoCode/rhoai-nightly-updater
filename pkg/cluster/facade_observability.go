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

// ValidateMaaSRequest is observability.ValidateMaaSRequest.
func ValidateMaaSRequest(req types.MaaSRequest) string {
	return observability.ValidateMaaSRequest(req)
}

// RunMaaS is observability.RunMaaS.
func RunMaaS(c *Client, req types.MaaSRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return observability.RunMaaS(c, req, onStep)
}

// GetMaaSStatus is observability.GetMaaSStatus.
func GetMaaSStatus(c *Client) (*types.MaaSStatus, error) {
	return observability.GetMaaSStatus(c)
}
