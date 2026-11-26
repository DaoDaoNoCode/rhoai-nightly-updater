package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Workbench feature (internal/workbench), under the names pkg/api uses.

// GetWorkbenchStatus is workbench.GetStatus.
func GetWorkbenchStatus(sa, u *Client, project string) (*types.WorkbenchStatus, error) {
	return workbench.GetStatus(sa, u, project)
}

// GetWorkbenchProjectStatus is workbench.GetProjectStatus.
func GetWorkbenchProjectStatus(sa, u *Client, project string) (*types.WorkbenchStatus, error) {
	return workbench.GetProjectStatus(sa, u, project)
}

// ValidateWorkbenchRequest is workbench.Validate.
func ValidateWorkbenchRequest(req types.WorkbenchRequest) string {
	return workbench.Validate(req)
}

// RunWorkbench is workbench.Run.
func RunWorkbench(c *Client, req types.WorkbenchRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return workbench.Run(c, req, onStep)
}
