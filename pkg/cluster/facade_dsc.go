package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The DataScienceCluster's writes and their guards (internal/dsc), under the
// names pkg/api and this package use.

// GetDSCManager is dsc.Manager.
func GetDSCManager(c *Client) (*types.DSCManager, error) {
	return dsc.Manager(c)
}

// PlanDSCChange is dsc.PlanChange.
func PlanDSCChange(c *Client, action, path, to string, withParent, keepEnabled bool) (*types.DSCPlan, error) {
	return dsc.PlanChange(c, action, path, to, withParent, keepEnabled)
}

// SetDSCManagementState is dsc.SetManagementState.
func SetDSCManagementState(c *Client, req types.DSCToggleRequest) (*types.OperationResponse, error) {
	return dsc.SetManagementState(c, req)
}

// FixDSC is dsc.Fix.
func FixDSC(c *Client, req types.DSCPlanRequest) (*types.OperationResponse, error) {
	return dsc.Fix(c, req)
}

// SoftResetDSC is dsc.SoftReset.
func SoftResetDSC(c *Client, req types.DSCPlanRequest) (*types.OperationResponse, error) {
	return dsc.SoftReset(c, req)
}

// HardResetDSC is dsc.HardReset.
func HardResetDSC(c *Client, req types.DSCPlanRequest, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return dsc.HardReset(c, req, emit)
}

// FinishDSCHardReset is dsc.FinishHardReset.
func FinishDSCHardReset(c *Client, req types.DSCFinishRequest) (*types.OperationResponse, error) {
	return dsc.FinishHardReset(c, req)
}

// CreateDefaultDSC is dsc.CreateDefault.
func CreateDefaultDSC(c *Client) (*types.OperationResponse, error) {
	return dsc.CreateDefault(c)
}

// RepairDSC is dsc.Repair.
func RepairDSC(c *Client, name, mode, expectedOperatorVersion, expectedAPIVersion string, expectedExtraComponents, expectedResetRemovals []string) (*types.OperationResponse, error) {
	return dsc.Repair(c, name, mode, expectedOperatorVersion, expectedAPIVersion, expectedExtraComponents, expectedResetRemovals)
}
