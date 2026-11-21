package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
)

// The DSC model and the component readers (internal/components), under the
// names pkg/api and this package use.

// GetDefaultDSCYAML is components.DefaultDSCYAML.
func GetDefaultDSCYAML(c *Client) (*components.DSCDefaults, error) {
	return components.DefaultDSCYAML(c)
}
