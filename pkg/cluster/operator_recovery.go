package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const restoreStepName = "restore_previous_operator"

// RecoveryTimeout bounds the automatic restore after a failed operation. It
// runs on its own context so it also runs when the operation was cancelled.
var RecoveryTimeout = 2 * time.Minute

// operatorRecovery holds the desired state captured before an operation and
// what the operation has changed, so a failure restores exactly that.
type operatorRecovery struct {
	subscription map[string]interface{}
	catalog      map[string]interface{}

	// catalogChanged: the nightly CatalogSource was replaced or deleted.
	catalogChanged bool
	// subscriptionChanged: the Subscription was deleted or replaced.
	subscriptionChanged bool
	// csvRemoved: the previously installed CSV was deleted, so any RHOAI
	// CSV now in the namespace belongs to this attempt.
	csvRemoved bool
	// keepNewInstall: OLM was still installing the new target when the
	// operation stopped waiting; the new state is kept instead of undone.
	keepNewInstall bool
	attemptCSVs    []string
}

func (r *operatorRecovery) installPlanName() string {
	status, _ := r.subscription["status"].(map[string]interface{})
	ref, _ := status["installPlanRef"].(map[string]interface{})
	name, _ := ref["name"].(string)
	return name
}

func (r *operatorRecovery) noteAttemptCSV(name string) {
	if r == nil || name == "" {
		return
	}
	for _, n := range r.attemptCSVs {
		if n == name {
			return
		}
	}
	r.attemptCSVs = append(r.attemptCSVs, name)
}

func captureOperatorRecovery(c *Client) (*operatorRecovery, error) {
	r := &operatorRecovery{}
	for _, resource := range []struct {
		path   string
		target *map[string]interface{}
	}{
		{subscriptionPath(), &r.subscription},
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

// restore puts back what a failed operation changed:
//   - Only the catalog changed: the previous catalog is re-applied (or the
//     one this attempt created is deleted); the running operator is left
//     alone.
//   - The operator was touched: the attempt's Subscription and any CSV it
//     installed are deleted (a half-installed or Failed CSV would otherwise
//     be adopted by the restored Subscription), then the previous catalog
//     and Subscription are re-applied so OLM reinstalls the previous version.
//
// Deleting a Subscription never removes its CSV, and OLM itself clears the
// CRD conversion webhooks and operator webhooks of a deleted CSV, so every
// step is safe to repeat. Re-running Update or Reinstall recovers from any
// point where this restore stops.
func (r *operatorRecovery) restore(c *Client, result *types.OperationResponse, emit func(UpdateStepEvent)) {
	if r == nil || (result != nil && result.Success) || r.keepNewInstall {
		return
	}
	operatorTouched := r.subscriptionChanged || r.csvRemoved
	if !operatorTouched && !r.catalogChanged {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), RecoveryTimeout)
	defer cancel()
	c = c.WithContext(ctx)
	emit(UpdateStepEvent{Step: restoreStepName, Status: "running", Message: "Restoring the previous operator state..."})

	var failures []string
	if operatorTouched {
		if _, err := c.delete(subscriptionPath()); err != nil && !IsK8sError(err, 404) {
			failures = append(failures, "delete the new Subscription: "+err.Error())
		}
		if r.csvRemoved {
			names, err := r.attemptCSVNames(c)
			if err != nil {
				failures = append(failures, "list CSVs: "+err.Error())
			}
			for _, name := range names {
				if gone, err := deleteCSVAndWait(c, name); err != nil {
					failures = append(failures, "delete CSV "+name+": "+err.Error())
				} else if !gone {
					slog.Warn("CSV from the failed attempt is still being deleted", "csv", name)
				}
			}
		}
	}
	catalogPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	if r.catalogChanged {
		if r.catalog != nil {
			obj := map[string]interface{}{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource",
				"metadata": map[string]interface{}{"name": CatalogName, "namespace": CatalogNS}, "spec": r.catalog["spec"]}
			if _, _, err := c.apply(catalogPath, obj); err != nil {
				failures = append(failures, "CatalogSource: "+err.Error())
			}
		} else {
			if _, err := c.delete(catalogPath); err != nil && !IsK8sError(err, 404) {
				failures = append(failures, "remove the new CatalogSource: "+err.Error())
			}
		}
	}
	if operatorTouched && r.subscription != nil {
		obj := map[string]interface{}{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
			"metadata": map[string]interface{}{"name": SubName, "namespace": SubNS}, "spec": r.subscription["spec"]}
		if _, _, err := c.apply(subscriptionPath(), obj); err != nil {
			failures = append(failures, "Subscription: "+err.Error())
		}
	}

	var message string
	status := "success"
	switch {
	case len(failures) > 0:
		status = "failed"
		message = fmt.Sprintf("Automatic operator recovery failed: %s. Restore the previous Subscription/catalog before retrying, or re-run Update or Reinstall.", strings.Join(failures, "; "))
	case !operatorTouched:
		message = "The previous nightly catalog was restored; the Subscription and CSV were not changed."
	case !r.csvRemoved && r.subscription != nil:
		message = "The previous catalog and Subscription were restored; the installed operator was not changed."
	case r.subscription == nil:
		message = "There was no previous Subscription, so the Subscription and CSV created by this attempt were removed and the catalog was restored to its previous state."
	default:
		message = "The previous catalog and Subscription were restored; OLM will reinstall the previous operator version. Watch the operator status before retrying."
		if spec, _ := r.subscription["spec"].(map[string]interface{}); r.csvRemoved && spec["installPlanApproval"] == "Manual" {
			message += " The Subscription uses Manual approval: approve its InstallPlan in the console to reinstall the operator."
		}
	}
	if result != nil {
		result.Logs = append(result.Logs, message)
		result.Message = strings.TrimSpace(result.Message + " " + message)
	}
	emit(UpdateStepEvent{Step: restoreStepName, Status: status, Message: message})
}

// attemptCSVNames lists the RHOAI CSVs installed by the failed attempt: every
// non-copied rhods-operator CSV in the operator namespace (the previous one
// was already deleted when csvRemoved is set), plus those seen while waiting.
func (r *operatorRecovery) attemptCSVNames(c *Client) ([]string, error) {
	names := append([]string(nil), r.attemptCSVs...)
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if err != nil {
		return names, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return names, err
	}
	for _, item := range list.Items {
		name := item.Metadata.Name
		if !strings.HasPrefix(name, SubName+".") || item.Metadata.Labels["olm.copiedFrom"] != "" {
			continue
		}
		seen := false
		for _, n := range names {
			seen = seen || n == name
		}
		if !seen {
			names = append(names, name)
		}
	}
	return names, nil
}
