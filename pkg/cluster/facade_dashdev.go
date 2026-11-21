package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Dashboard Dev (internal/dashdev), under the names pkg/api and this
// package use.

// DashboardStateError is dashdev.StateError.
func DashboardStateError(err error) (status int, code, message string) {
	return dashdev.StateError(err)
}

// GetDashboardState is dashdev.State.
func GetDashboardState(c *Client) (*types.DashboardState, error) {
	return dashdev.State(c)
}

// DeployPRImageWithFlavor is dashdev.DeployPR.
func DeployPRImageWithFlavor(c *Client, prNumber int, flavor string) (*types.OperationResponse, error) {
	return dashdev.DeployPR(c, prNumber, flavor)
}

// RevertDashboardImage is dashdev.Revert.
func RevertDashboardImage(c *Client) (*types.OperationResponse, error) {
	return dashdev.Revert(c)
}

// ValidDashboardFlavor is dashdev.ValidFlavor.
func ValidDashboardFlavor(flavor string) bool { return dashdev.ValidFlavor(flavor) }

// DashboardOverrideSummary is dashdev.OverrideSummary.
func DashboardOverrideSummary(c *Client) (*types.DashboardOverride, error) {
	return dashdev.OverrideSummary(c)
}

// DeployDashboardMainWithFlavor is dashdev.DeployMain.
func DeployDashboardMainWithFlavor(c *Client, flavor string) (*types.OperationResponse, error) {
	return dashdev.DeployMain(c, flavor)
}

// GetDashboardBuilds is dashdev.Builds.
func GetDashboardBuilds(c *Client) (*types.DashboardBuildsResponse, error) {
	return dashdev.Builds(c)
}
