package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// revertDashboardOperatorTo returns the dashboard to operator management
// from any state: with or without a session, with a corrupt session, after
// RHOAI was updated, and after a partial deploy. Resuming the operator is
// enough: the dashboard module's operator applies its manifests with
// server-side apply and ForceOwnership (odh-platform-utilities pkg/deploy),
// which takes the image fields back and rolls every workload to the
// installed release. Repeating it is safe. target names the operator
// (discoverDashboardTarget). It also returns the replica count it resumed
// the operator to (0 when it could not resume it).
func revertDashboardOperatorTo(c *Client, target dashboardTarget) (*types.OperationResponse, int) {
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	var logs []string
	fail := func(message, code string) (*types.OperationResponse, int) {
		return &types.OperationResponse{Success: false, Message: message, ErrorCode: code, Logs: logs}, 0
	}
	ns, name := target.Operator.Namespace, target.Operator.Name
	cleared, failures := clearDashboardManagedAnnotations(c, ns, name)
	for _, workload := range cleared {
		logs = append(logs, workload+": removed opendatahub.io/managed so the operator manages it again")
	}
	for attempt := 0; ; attempt++ {
		// Re-read after acquiring the shared deploy/revert lock and on conflicts.
		operator, err := readDashboardOperatorAt(c, ns, name)
		if err != nil {
			code := errorCodeFromK8sErr(err)
			if code == "" {
				code = "prerequisites"
			}
			return fail("Cannot read "+name+": "+err.Error()+". Retry Revert to default.", code)
		}
		if operator == nil {
			return fail("Deployment "+ns+"/"+name+" not found. The RHOAI platform recreates it with its default replica count; retry Revert to default once it exists.", "prerequisites")
		}
		session, sessionErr := dashboardSession(operator)
		replicas := dashboardOperatorReplicas(operator)
		switch {
		case session != nil:
			replicas = session.Replicas
		case replicas <= 0:
			reason := "no Dashboard Dev session is recorded"
			if sessionErr != nil {
				reason = sessionErr.Error()
			}
			replicas = dashboardOperatorDefaultReplicas
			logs = append(logs, fmt.Sprintf("Original replica count unknown (%s); resuming with the chart default of %d.", reason, replicas))
		}
		if session != nil {
			if reasons := dashboardStaleReasons(session, operator); len(reasons) > 0 {
				logs = append(logs, "RHOAI changed during the session ("+strings.Join(reasons, "; ")+"); the operator will deploy the new release images.")
			}
		}
		// Keep a valid session while workload annotations still need cleanup
		// so the page keeps offering Revert. A corrupt one is dropped.
		var keep *dashboardDevSession
		if len(failures) > 0 {
			keep = session
		}
		now := nowRFC3339()
		detail := fmt.Sprintf("Resumed %s (%d replicas)", name, replicas)
		action := &types.DashboardDevAction{Action: "revert", By: getUser(c), At: now, Detail: detail}
		if err := patchDashboardOperator(c, operator, replicas, keep, action); err != nil {
			if isK8sConflict(err) && attempt < 2 {
				continue
			}
			return fail("Failed to resume "+name+": "+err.Error()+". Retry Revert to default.", errorCodeFromK8sErr(err))
		}
		message := fmt.Sprintf("%s restored to %d replicas. It will reconcile all dashboard images to the installed release.", name, replicas)
		code := ""
		if len(failures) > 0 {
			message += " Some workload annotations could not be restored: " + strings.Join(failures, "; ") + ". Retry revert."
			code = "partial_failure"
		}
		logs = append(logs, message)
		RecordActivity(c, types.ActivityEntry{Timestamp: now, User: getUser(c), Action: "revert-dashboard", Detail: message, Success: len(failures) == 0})
		return &types.OperationResponse{Success: len(failures) == 0, Message: message, ErrorCode: code, Logs: logs}, replicas
	}
}

// DashboardDevActive reports whether the dashboard is overridden: the
// dashboard module's operator is paused (by Dashboard Dev or otherwise), a
// session or a corrupt session annotation exists, or a Dashboard-owned
// workload carries opendatahub.io/managed. On a cluster without a
// dashboard module (RHOAI 3.4 and older) it reports the legacy flow's
// managed=false annotation, its saved original, or a PR/main build on
// rhods-dashboard. Without a DSCInitialization there is no dashboard, so
// nothing is active. When the operator cannot be identified it returns an
// error: callers refuse rather than assume either flow.
func DashboardDevActive(c *Client) (bool, error) {
	active, _, err := dashboardDevActive(c)
	return active, err
}

// dashboardDevActive is DashboardDevActive; operator is the dashboard
// module's operator Deployment (namespace/name, "" on the legacy flow).
func dashboardDevActive(c *Client) (active bool, operator string, err error) {
	target, op, err := loadDashboardOperator(c, true)
	if errors.Is(err, errNoDSCInitialization) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if op == nil {
		if target.Namespace == "" {
			return false, "", nil
		}
		active, err := legacyDashboardDevActive(c, target.Namespace)
		return active, "", err
	}
	if dashboardOverrideFromOperator(op).Active {
		return true, op.ref(), nil
	}
	items, err := listDashboardNamespaceDeployments(c, op.Metadata.Namespace)
	if err != nil {
		return false, "", err
	}
	for _, d := range items {
		if _, ok := d.Metadata.Annotations["opendatahub.io/managed"]; ok && d.Metadata.UID != op.Metadata.UID && dashboardOwner(d) != "" {
			return true, op.ref(), nil
		}
	}
	return false, op.ref(), nil
}

// pausedOperatorText names what a Dashboard Dev session pauses, for
// messages: "Deployment ns/name is paused", or the legacy flow's patch.
func pausedOperatorText(operator string) string {
	if operator == "" {
		return dashboardDeploymentName + " runs a PR build without operator management"
	}
	return "Deployment " + operator + " is paused"
}

func legacyDashboardDevActive(c *Client, ns string) (bool, error) {
	var d dashboardDeployment
	found, err := c.GetIntoOptional(namespacedPath("apps/v1", "deployments", ns, dashboardDeploymentName), &d)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if _, saved := d.Metadata.Annotations[legacyOriginalManagedAnnotation]; saved || d.Metadata.Annotations["opendatahub.io/managed"] == "false" {
		return true, nil
	}
	for _, ct := range d.Spec.Template.Spec.Containers {
		if repo, ok := prContainerRepos[ct.Name]; ok {
			if kind, _, _ := parseDashboardBuild(repo, ct.Image); kind != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// Bounds for RevertDashboardDevForOperation; variables so tests can shorten them.
var (
	dashboardOperatorReadyTimeout = 3 * time.Minute
	dashboardOperatorPollInterval = 2 * time.Second
)

// RevertDashboardDevForOperation returns the dashboard to operator management
// before an operation that needs a working dashboard operator (RHOAI update,
// reinstall, refresh, dashboard removal, uninstall): with the operator paused,
// a Dashboard CR deletion never completes and RHOAI updates do not reach the
// dashboard. It does nothing when no override is active, otherwise reverts and
// waits, bounded by dashboardOperatorReadyTimeout and the client's context,
// until the operator is scaled up and Ready. Safe to repeat from any state.
func RevertDashboardDevForOperation(c *Client) error {
	active, err := DashboardDevActive(c)
	if err != nil {
		return fmt.Errorf("check Dashboard Dev state: %w", err)
	}
	if !active {
		return nil
	}
	target, err := discoverDashboardTarget(c)
	if err != nil {
		return err
	}
	if target.Operator == nil {
		result, err := RevertDashboardImage(c)
		if err != nil {
			return fmt.Errorf("revert legacy dashboard override: %w", err)
		}
		if !result.Success {
			return fmt.Errorf("revert legacy dashboard override: %s", result.Message)
		}
		return nil
	}
	result, replicas := revertDashboardOperatorTo(c, target)
	if !result.Success {
		return fmt.Errorf("revert Dashboard Dev: %s", result.Message)
	}
	return waitDashboardOperatorReady(c, target.Operator.Namespace, target.Operator.Name, replicas)
}

func waitDashboardOperatorReady(c *Client, ns, name string, replicas int) error {
	ctx, cancel := clk.WithTimeout(c.Context(), dashboardOperatorReadyTimeout)
	defer cancel()
	reader := c.WithContext(ctx)
	var last string
	for {
		operator, err := readDashboardOperatorAt(reader, ns, name)
		switch {
		case err != nil:
			last = err.Error()
		case operator == nil:
			last = "Deployment " + ns + "/" + name + " not found"
		case dashboardOperatorReplicas(operator) == 0:
			return fmt.Errorf("%s was paused again while waiting for it to resume", name)
		case dashboardDeploymentReady(*operator):
			if got := dashboardOperatorReplicas(operator); got != replicas {
				slog.Info("dashboard operator ready with a different replica count than restored", "deployment", ns+"/"+name, "restored", replicas, "current", got)
			}
			return nil
		default:
			last = fmt.Sprintf("%d/%d replicas ready", operator.Status.ReadyReplicas, dashboardOperatorReplicas(operator))
		}
		if err := clk.Sleep(ctx, dashboardOperatorPollInterval); err != nil {
			return fmt.Errorf("%s did not become ready (%s): %w", name, last, err)
		}
	}
}
