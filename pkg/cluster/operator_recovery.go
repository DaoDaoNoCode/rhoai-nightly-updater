package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"time"
)

type operatorRecovery struct {
	subscription map[string]interface{}
	catalog      map[string]interface{}
	started      bool
}

func (r *operatorRecovery) installPlanName() string {
	status, _ := r.subscription["status"].(map[string]interface{})
	ref, _ := status["installPlanRef"].(map[string]interface{})
	name, _ := ref["name"].(string)
	return name
}

func captureOperatorRecovery(c *Client) (*operatorRecovery, error) {
	r := &operatorRecovery{}
	for _, resource := range []struct {
		path   string
		target *map[string]interface{}
	}{
		{namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), &r.subscription},
		{namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName), &r.catalog},
	} {
		body, _, err := c.get(resource.path)
		if IsK8sError(err, 404) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("capture operator recovery state: %w", err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, err
		}
		if obj["spec"] != nil {
			*resource.target = obj
		}
	}
	return r, nil
}

// Restoring the old desired state lets OLM recreate the old operator after a
// failed cleanup/install. This is bounded independently of the failed context.
func (r *operatorRecovery) restore(c *Client, result *types.OperationResponse, opErr error, emit func(UpdateStepEvent)) {
	if !r.started || (result != nil && result.Success && opErr == nil) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c = c.WithContext(ctx)
	var failures []string
	if r.subscription != nil {
		// A surviving Subscription can retain installedCSV after CSV cleanup.
		// Recreate it so OLM resolves a fresh InstallPlan for the old target.
		subPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
		if _, err := c.delete(subPath); err != nil && !IsK8sError(err, 404) {
			failures = append(failures, "clear Subscription status: "+err.Error())
		}
	}
	for _, resource := range []struct {
		name, kind, path string
		obj              map[string]interface{}
	}{
		{CatalogName, "CatalogSource", namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName), r.catalog},
		{SubName, "Subscription", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), r.subscription},
	} {
		if resource.obj == nil {
			continue
		}
		obj := map[string]interface{}{"apiVersion": "operators.coreos.com/v1alpha1", "kind": resource.kind, "metadata": map[string]interface{}{"name": resource.name, "namespace": CatalogNS}, "spec": resource.obj["spec"]}
		if resource.kind == "Subscription" {
			obj["metadata"].(map[string]interface{})["namespace"] = SubNS
		}
		if _, _, err := c.apply(resource.path, obj); err != nil {
			failures = append(failures, resource.kind+": "+err.Error())
		}
	}
	message := "Previous catalog and Subscription desired state restored; monitor OLM reconciliation before retrying."
	status := "success"
	if len(failures) > 0 {
		status = "failed"
		message = fmt.Sprintf("Automatic operator recovery failed: %v. Restore the previous Subscription/catalog before retrying.", failures)
	}
	if result != nil {
		result.Logs = append(result.Logs, message)
		result.Message += " " + message
	}
	emit(UpdateStepEvent{Step: "restore_previous_operator", Status: status, Message: message})
}
