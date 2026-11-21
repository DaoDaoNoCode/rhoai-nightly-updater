package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/gitops"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The gitops profile's update and its admission-policy check
// (internal/gitops), under the names pkg/api and main.go use.

// SetGitOpsPolicyTemplate is gitops.SetPolicyTemplate.
func SetGitOpsPolicyTemplate(data []byte) {
	gitops.SetPolicyTemplate(data)
}

// CheckGitOpsAdmissionPolicy is gitops.CheckGitOpsAdmissionPolicy.
func CheckGitOpsAdmissionPolicy(c *Client) error {
	return gitops.CheckGitOpsAdmissionPolicy(c)
}

// UpdateStreamGitOpsProfile is gitops.UpdateStream.
func UpdateStreamGitOpsProfile(c *Client, image string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return gitops.UpdateStream(c, image, emit)
}

// UpdateDryRunGitOpsProfile is gitops.UpdateDryRun.
func UpdateDryRunGitOpsProfile(c *Client, image string) (*types.OperationResponse, error) {
	return gitops.UpdateDryRun(c, image)
}
