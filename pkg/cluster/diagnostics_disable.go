package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Preconditions for setting a DSC component to Removed.
//
// Removing a component makes rhods-operator delete the module CR. The CR's
// finalizer is removed by the module's own operator, and the operator is
// kept running only while the CR exists (rhods-operator rhoai-3.6
// modules_controller_actions.go cleanupDisabledModules). If that operator is
// not running, the CR, and then the Platform finalizer, wait forever
// (RHOAI operator notes D1, D7, D8). So the tool only removes a component
// whose finalizer owner is up.

// refusedComponents are never disabled by the tool, with the reason shown.
var refusedComponents = map[string]string{
	"dashboard": "Removing the dashboard deletes the Dashboard CR, whose finalizer only dashboard-operator removes. If Dashboard Dev has paused " +
		"dashboard-operator, the deletion hangs. Change it in the OpenShift console after reverting Dashboard Dev.",
	"mlflowoperator": "The MLflowOperator CR cannot be deleted while any MLflow CR exists (its finalizer waits for them), so removing this " +
		"component hangs. Tear down MLflow first (Dashboard Dev > Resources), then change it in the OpenShift console.",
	"aipipelines": "Pipeline servers (DSPAs) keep a finalizer that only the pipelines operator removes. Removing this component while DSPAs exist " +
		"leaves them and their projects stuck in Terminating. Delete the pipeline servers first, then change it in the OpenShift console.",
}

func deploymentReady(c *Client, namespace, name string) (bool, string) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", namespace, name))
	if IsK8sError(err, http.StatusNotFound) {
		return false, fmt.Sprintf("Deployment %s/%s does not exist", namespace, name)
	}
	if err != nil {
		return false, fmt.Sprintf("could not read Deployment %s/%s: %v", namespace, name, err)
	}
	var d struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return false, fmt.Sprintf("could not parse Deployment %s/%s", namespace, name)
	}
	if d.Status.ReadyReplicas < 1 {
		return false, fmt.Sprintf("Deployment %s/%s has no ready pod", namespace, name)
	}
	return true, ""
}

// moduleCR returns the module CR of a component, or found=false. Discovery
// and list errors are returned, so a precondition is never skipped because
// the CR could not be read.
func moduleCR(c *Client, component string) (finalizers []string, deleting bool, found bool, err error) {
	items, err := listModuleCRs(c, cachedComponentAPI(c), component)
	if err != nil || len(items) == 0 {
		return nil, false, false, err
	}
	m := items[0].Metadata
	return m.Finalizers, m.DeletionTimestamp != "", true, nil
}

// disableBlockers returns the reasons the component must not be removed now.
func disableBlockers(c *Client, component string) []string {
	var blockers []string
	if ok, why := deploymentReady(c, SubNS, "rhods-operator"); !ok {
		blockers = append(blockers, "rhods-operator must be running to clean up a removed component: "+why)
	}
	finalizers, deleting, found, err := moduleCR(c, component)
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found && len(finalizers) > 0:
		op, known := moduleOperators[component]
		if !known {
			blockers = append(blockers, fmt.Sprintf("the %s module CR has finalizer %s and the tool does not know which operator removes it", component, strings.Join(finalizers, ", ")))
		} else if ok, why := deploymentReady(c, op.namespace, op.name); !ok {
			blockers = append(blockers, fmt.Sprintf("the %s module CR has finalizer %s, which %s/%s removes, but %s; fix that operator first or the removal hangs",
				component, strings.Join(finalizers, ", "), op.namespace, op.name, why))
		}
	}
	return blockers
}
