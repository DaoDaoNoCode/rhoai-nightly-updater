package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The model registry (internal/modelregistry), under the names pkg/api and
// this package use.

// GetModelRegistryStatus is modelregistry.ReadStatus.
func GetModelRegistryStatus(sa, u *Client) (*types.ModelRegistryStatus, error) {
	return modelregistry.ReadStatus(sa, u)
}

// ValidateModelRegistryRequest is modelregistry.ValidateRequest.
func ValidateModelRegistryRequest(req types.ModelRegistryRequest) string {
	return modelregistry.ValidateRequest(req)
}

// RunModelRegistry is modelregistry.Run.
func RunModelRegistry(c *Client, req types.ModelRegistryRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return modelregistry.Run(c, req, onStep)
}
