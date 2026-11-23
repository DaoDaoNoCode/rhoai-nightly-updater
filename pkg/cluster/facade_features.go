package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The feature setups and the Gen AI playground (internal/features), under the
// names pkg/api and this package use.

// GetPrerequisitesStatusFor is features.GetPrerequisitesStatusFor.
func GetPrerequisitesStatusFor(c *Client, modelsExposure string) (*types.PrerequisitesStatus, error) {
	return features.GetPrerequisitesStatusFor(c, modelsExposure)
}

// ValidatePrerequisitesRequest is features.ValidatePrerequisitesRequest.
func ValidatePrerequisitesRequest(req types.PrerequisitesRequest) string {
	return features.ValidatePrerequisitesRequest(req)
}

// RunPrerequisites is features.RunPrerequisites.
func RunPrerequisites(c *Client, req types.PrerequisitesRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return features.RunPrerequisites(c, req, onStep)
}

// RestorableComponentKey is features.RestorableComponentKey.
func RestorableComponentKey(key string) bool {
	return features.RestorableComponentKey(key)
}

// GetPlaygroundStatus is features.GetPlaygroundStatus.
func GetPlaygroundStatus(sa, u *Client, project string) (*types.PlaygroundStatus, error) {
	return features.GetPlaygroundStatus(sa, u, project)
}

// ValidatePlaygroundRequest is features.ValidatePlaygroundRequest.
func ValidatePlaygroundRequest(req types.PlaygroundRequest) string {
	return features.ValidatePlaygroundRequest(req)
}

// RunPlayground is features.RunPlayground.
func RunPlayground(c *Client, req types.PlaygroundRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return features.RunPlayground(c, req, onStep)
}

// SweepGoneInBackground is features.SweepGoneInBackground.
func SweepGoneInBackground(c *Client) {
	features.SweepGoneInBackground(c)
}

// SweepGoneProjectRecords is features.SweepGoneProjectRecords.
func SweepGoneProjectRecords(c *Client) {
	features.SweepGoneProjectRecords(c)
}
