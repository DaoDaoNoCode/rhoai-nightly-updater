package cluster

// TODO(B4 merge): DashboardDevActive and RevertDashboardDevForOperation are
// owned by dashboard_operator.go on the B4 branch. These are build-safe
// stand-ins so the OLM pipelines can use the agreed names; delete this file
// when B4's versions are merged.

import (
	"fmt"
	"time"
)

// DashboardDevActive reports whether a Dashboard Dev session has paused
// dashboard-operator (session annotation present or zero replicas).
func DashboardDevActive(c *Client) (bool, error) {
	operator, err := readDashboardOperator(c)
	if err != nil || operator == nil {
		return false, err
	}
	if operator.Metadata.Annotations[dashboardDevAnnotation] != "" {
		return true, nil
	}
	return operator.Spec.Replicas != nil && *operator.Spec.Replicas == 0, nil
}

// RevertDashboardDevForOperation ends the Dashboard Dev session and waits
// until dashboard-operator is back at its original replicas and Ready.
func RevertDashboardDevForOperation(c *Client) error {
	operator, err := readDashboardOperator(c)
	if err != nil {
		return err
	}
	if operator == nil {
		return nil
	}
	resp, err := revertDashboardOperator(c, operator)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		operator, err := readDashboardOperator(c)
		if err == nil && operator != nil && dashboardDeploymentReady(*operator) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dashboard-operator was resumed but is not Ready after 3 minutes")
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(InstallPlanPollInterval):
		}
	}
}
