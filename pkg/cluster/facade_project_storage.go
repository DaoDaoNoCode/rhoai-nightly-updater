package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Project S3 storage and its pipeline server (internal/storage), under
// the names pkg/api uses.

// ValidateProjectStorageRequest is storage.ValidateProjectStorageRequest.
func ValidateProjectStorageRequest(req types.ProjectStorageRequest) string {
	return storage.ValidateProjectStorageRequest(req)
}

// RunProjectStorage is storage.RunProjectStorage.
func RunProjectStorage(c *Client, req types.ProjectStorageRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return storage.RunProjectStorage(c, req, onStep)
}

// GetProjectStorageStatus is storage.GetProjectStorageStatus.
func GetProjectStorageStatus(c *Client, project string) *types.ProjectStorageStatus {
	return storage.GetProjectStorageStatus(c, project)
}

// ValidateProjectPipelineServerRequest is
// storage.ValidateProjectPipelineServerRequest.
func ValidateProjectPipelineServerRequest(req types.ProjectPipelineServerRequest) string {
	return storage.ValidateProjectPipelineServerRequest(req)
}

// RunProjectPipelineServer is storage.RunProjectPipelineServer.
func RunProjectPipelineServer(c *Client, req types.ProjectPipelineServerRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return storage.RunProjectPipelineServer(c, req, onStep)
}
