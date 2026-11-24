package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Distributed workloads (internal/kueue), under the names pkg/api uses.

// GetKueueStatus is kueue.GetStatus.
func GetKueueStatus(sa, u *Client, project string) (*types.KueueStatus, error) {
	return kueue.GetStatus(sa, u, project)
}

// ValidateKueueRequest is kueue.Validate.
func ValidateKueueRequest(req types.KueueRequest) string {
	return kueue.Validate(req)
}

// RunKueue is kueue.Run.
func RunKueue(c *Client, req types.KueueRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return kueue.Run(c, req, onStep)
}
