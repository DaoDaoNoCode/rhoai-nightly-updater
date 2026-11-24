package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// UpdateStepEvent represents a single step in the update pipeline
// (types.UpdateStepEvent, so the packages under pkg/cluster share it).
type UpdateStepEvent = types.UpdateStepEvent

// The operator Update, Reinstall and Refresh operations and the pull secret
// (internal/operator), under the names pkg/api uses.

// OperationOptions is operator.OperationOptions.
type OperationOptions = operator.OperationOptions

// Update is operator.Update.
func Update(c *Client, image string, dryRun bool) (*types.OperationResponse, error) {
	return operator.Update(c, image, dryRun)
}

// UpdateStreamWithOptions is operator.UpdateStreamWithOptions.
func UpdateStreamWithOptions(c *Client, image string, opts OperationOptions, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return operator.UpdateStreamWithOptions(c, image, opts, emit)
}

// ReinstallStreamWithOptions is operator.ReinstallStreamWithOptions.
func ReinstallStreamWithOptions(c *Client, targetType, image, channelOverride string, opts OperationOptions, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return operator.ReinstallStreamWithOptions(c, targetType, image, channelOverride, opts, emit)
}

// RefreshOperatorStreamWithOptions is operator.RefreshStreamWithOptions.
func RefreshOperatorStreamWithOptions(c *Client, opts OperationOptions, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return operator.RefreshStreamWithOptions(c, opts, emit)
}

// VerifyNodeReadiness is operator.VerifyNodeReadiness.
func VerifyNodeReadiness(c *Client) (*types.OperationResponse, error) {
	return operator.VerifyNodeReadiness(c)
}

// CreatePullSecret is operator.CreatePullSecret.
func CreatePullSecret(c *Client, auth string) (*types.OperationResponse, error) {
	return operator.CreatePullSecret(c, auth)
}

// CopyPullSecretForRegistry is operator.CopyPullSecretForRegistry.
func CopyPullSecretForRegistry(c *Client) (*types.OperationResponse, error) {
	return operator.CopyPullSecretForRegistry(c)
}

// RemovePullSecretRegistryCopy is operator.RemovePullSecretRegistryCopy.
func RemovePullSecretRegistryCopy(c *Client) (*types.OperationResponse, error) {
	return operator.RemovePullSecretRegistryCopy(c)
}

// TestPullSecret is operator.TestPullSecret.
func TestPullSecret(c *Client) (*types.OperationResponse, error) {
	return operator.TestPullSecret(c)
}
