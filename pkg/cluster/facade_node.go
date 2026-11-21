package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/node"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Node setup (image mirror, the nodes' pull secret) and the node pull check
// (internal/node), under the names pkg/api uses.

// ValidNodeSetupAction is node.ValidAction.
func ValidNodeSetupAction(a string) bool { return node.ValidAction(a) }

// NodeSetupNeedsVersion is node.NeedsVersion.
func NodeSetupNeedsVersion(a string) bool { return node.NeedsVersion(a) }

// PlanNodeSetup is node.PlanSetup.
func PlanNodeSetup(c *Client, req types.NodeSetupRequest) (*types.NodeSetupPlan, error) {
	return node.PlanSetup(c, req)
}

// RunNodeSetup is node.RunSetup.
func RunNodeSetup(c *Client, req types.NodeSetupRequest, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return node.RunSetup(c, req, emit)
}

// NodePullCheckImageProblem is node.PullCheckImageProblem.
func NodePullCheckImageProblem(image string) string { return node.PullCheckImageProblem(image) }

// CheckNodePull is node.CheckPull.
func CheckNodePull(c *Client, image string) (*types.OperationResponse, error) {
	return node.CheckPull(c, image)
}
