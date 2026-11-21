package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The DSC model and the component readers (internal/components), under the
// names pkg/api and this package use.

// GetDefaultDSCYAML is components.DefaultDSCYAML.
func GetDefaultDSCYAML(c *Client) (*components.DSCDefaults, error) {
	return components.DefaultDSCYAML(c)
}

// GetDebugInfo is components.DebugInfo.
func GetDebugInfo(c *Client) (*types.DebugResponse, error) {
	return components.DebugInfo(c)
}

// AssistRollout is components.AssistRollout.
func AssistRollout(c *Client) (*types.OperationResponse, error) {
	return components.AssistRollout(c)
}

// AssistRolloutFor is components.AssistRolloutFor.
func AssistRolloutFor(c *Client, namespace, name string) (*types.OperationResponse, error) {
	return components.AssistRolloutFor(c, namespace, name)
}
