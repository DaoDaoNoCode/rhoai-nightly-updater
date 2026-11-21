package operator

import (
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// OperationOptions are the confirmations a caller can give for Update,
// Reinstall and Refresh. The zero value is the safe default: no downgrade and
// no change to a running Dashboard Dev session.
type OperationOptions struct {
	// AllowDowngrade confirms a Reinstall to an operator version older than
	// the installed one (OLM has no downgrade path; the user accepts that
	// OLM replaces the CRDs of the older bundle with their older versions).
	AllowDowngrade bool
	// RevertDashboardDev ends an active Dashboard Dev session first. The
	// operations refuse to run while dashboard-operator is paused: the new
	// operator keeps it at 0 replicas, and a Dashboard CR deletion would hang
	// on its finalizer (RHOAI_OPERATOR_NOTES D1).
	RevertDashboardDev bool
}

const (
	ErrorCodeDashboardDevActive = "dashboard_dev_active"
	ErrorCodeDowngrade          = "downgrade_requires_confirmation"
)

// dashboardDevGuard checks for an active Dashboard Dev session. It returns a
// refusal message when one is active and the caller did not ask to revert it,
// and whether a revert is needed before the first change.
func dashboardDevGuard(c *kube.Client, opts OperationOptions, operation string) (refusal string, revert bool, err error) {
	active, operator, err := dashdev.ActiveOperator(c)
	if err != nil {
		return "", false, fmt.Errorf("cannot check for a Dashboard Dev session: %w", err)
	}
	if !active {
		return "", false, nil
	}
	if !opts.RevertDashboardDev {
		return fmt.Sprintf("A Dashboard Dev session is active (%s). %s would keep the dashboard paused under the new operator, so it was not started and nothing was changed. Revert the dashboard first, or confirm reverting it as part of this %s.", dashdev.PausedOperatorText(operator), operation, operation), false, nil
	}
	return "", true, nil
}

// dashboardPRNote describes a dashboard PR image that this operation leaves in
// place (legacy PR deployments are unmanaged, so the operator keeps them).
func dashboardPRNote(c *kube.Client) string {
	state, err := dashdev.State(c)
	if err != nil || !state.IsCustomPR {
		return ""
	}
	return fmt.Sprintf("Note: Dashboard PR #%d is deployed. This operation does not replace it; use Revert to default on the Dashboard page to return to the release dashboard.", state.PRNumber)
}
