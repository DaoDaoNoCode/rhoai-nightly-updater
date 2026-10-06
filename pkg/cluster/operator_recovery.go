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

// The automatic restore after a failed operation runs on its own contexts,
// so it also runs when the operation was cancelled or hit its deadline. Its
// two phases have separate budgets so slow CSV deletions cannot starve the
// re-apply of the previous catalog and Subscription (the part that brings
// the operator back): removing the attempt's Subscription and CSVs gets
// restoreCSVBudget in total (each CSV wait is also capped by
// CSVDeletionTimeout), and the re-apply always gets restoreApplyTimeout.
// RecoveryTimeout is their sum: the longest a restore runs after the
// operation stopped (see ShutdownDrainTimeout).
var (
	restoreCSVBudget    = 30 * time.Second
	restoreApplyTimeout = 30 * time.Second
	RecoveryTimeout     = restoreCSVBudget + restoreApplyTimeout
)

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
	// recorded is the Subscription recorded before an earlier operation,
	// loaded only when no Subscription exists now (a crash removed it). It
	// provides the settings for the new Subscription, but the restore never
	// re-applies it: the restore puts back what existed before this
	// operation.
	recorded *subscriptionSnapshot
	// preCSVs maps each RHOAI CSV that existed before the operation to its
	// UID. A CSV not in it (or with another UID) was created by the attempt,
	// even when the operation deleted nothing (a first install).
	preCSVs map[string]string
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
	pre, err := listRHOAICSVs(c)
	if err != nil {
		return nil, fmt.Errorf("capture operator recovery state: list CSVs: %w", err)
	}
	r.preCSVs = map[string]string{}
	for _, csv := range pre {
		r.preCSVs[csv.Name] = csv.UID
	}
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
	if r.subscription != nil {
		// Recorded so a later operation can recreate it after a crash left
		// no Subscription. Failing to record it never blocks the operation.
		if err := saveSubscriptionSnapshot(c, r.subscription); err != nil {
			slog.Warn("could not record the operator Subscription", "error", err)
		}
	} else if rec, err := loadSubscriptionSnapshot(c); err != nil {
		slog.Warn("could not read the recorded operator Subscription", "error", err)
	} else {
		r.recorded = rec
	}
	return r, nil
}

// subscriptionBase is the Subscription whose settings (spec.config, approval
// mode) a new Subscription carries over: the live one, else the recorded one.
func (r *operatorRecovery) subscriptionBase() map[string]interface{} {
	if r == nil {
		return nil
	}
	if r.subscription != nil {
		return r.subscription
	}
	if r.recorded != nil && r.recorded.Spec != nil {
		return map[string]interface{}{"spec": r.recorded.Spec}
	}
	return nil
}

// recordedNote is logged when the recorded Subscription replaces a missing
// live one.
func (r *operatorRecovery) recordedNote() string {
	if r == nil || r.subscription != nil || r.recorded == nil {
		return ""
	}
	return fmt.Sprintf("No operator Subscription exists; its settings (config, approval mode) are taken from the Subscription recorded at %s", r.recorded.RecordedAt)
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

	var failures, stillDeleting []string
	if operatorTouched {
		delCtx, cancelDel := context.WithTimeout(context.Background(), restoreCSVBudget)
		cd := c.WithContext(delCtx)
		if _, err := cd.delete(subscriptionPath()); err != nil && !IsK8sError(err, 404) {
			failures = append(failures, "delete the new Subscription: "+err.Error())
		}
		names, err := r.attemptCSVNames(cd)
		if err != nil {
			failures = append(failures, "list CSVs: "+err.Error())
		}
		for _, name := range names {
			gone, err := deleteCSVAndWait(cd, name)
			switch {
			case err != nil && delCtx.Err() != nil:
				stillDeleting = append(stillDeleting, name)
			case err != nil:
				failures = append(failures, "delete CSV "+name+": "+err.Error())
			case !gone:
				stillDeleting = append(stillDeleting, name)
			}
		}
		cancelDel()
		if len(stillDeleting) > 0 {
			slog.Warn("CSV from the failed attempt is still being deleted", "csvs", stillDeleting)
		}
	}
	applyCtx, cancelApply := context.WithTimeout(context.Background(), restoreApplyTimeout)
	defer cancelApply()
	c = c.WithContext(applyCtx)
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
		if len(stillDeleting) > 0 {
			message += " " + stillDeletingNote(stillDeleting)
		}
	case len(stillDeleting) > 0:
		// Not a success: OLM cannot install a CSV with the same name until
		// the old one is gone, and a CSV stuck behind its finalizer needs a
		// look.
		status = "failed"
		restored := "The Subscription and catalog were restored to their previous state"
		if r.subscription != nil {
			restored = "The previous catalog and Subscription were re-applied"
		}
		message = fmt.Sprintf("Automatic operator recovery is incomplete. %s, but %s", restored, stillDeletingNote(stillDeleting))
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

// stillDeletingNote explains CSVs whose deletion did not finish within the
// restore budget.
func stillDeletingNote(names []string) string {
	return fmt.Sprintf("CSV %s from the failed attempt is still being deleted (OLM removes its webhooks and cluster RBAC first). "+
		"OLM cannot install a CSV with the same name until it is gone. Check it with: oc get csv -n %s %s. "+
		"If it stays, look at its finalizers and the OLM operator logs before retrying.",
		strings.Join(names, ", "), SubNS, strings.Join(names, " "))
}

// rhoaiCSV is the identity of a non-copied RHOAI CSV in the operator
// namespace.
type rhoaiCSV struct {
	Name, UID string
}

// listRHOAICSVs lists the RHOAI CSVs (rhods-operator.*, not copied) in the
// operator namespace. A missing namespace is an empty list.
func listRHOAICSVs(c *Client) ([]rhoaiCSV, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if IsK8sError(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var out []rhoaiCSV
	for _, item := range list.Items {
		if strings.HasPrefix(item.Metadata.Name, SubName+".") && item.Metadata.Labels["olm.copiedFrom"] == "" {
			out = append(out, rhoaiCSV{Name: item.Metadata.Name, UID: item.Metadata.UID})
		}
	}
	return out, nil
}

// attemptCSVNames lists the RHOAI CSVs created by the failed attempt:
//   - a CSV that did not exist before the operation (by name, or by UID
//     when the name was reused), so a failed first install is cleaned up
//     even though the operation deleted no CSV;
//   - when the operation deleted the previous CSV (csvRemoved), every RHOAI
//     CSV, since any CSV left belongs to this attempt (or is the deleted
//     one, still finishing).
//
// A CSV that existed before the operation and was not deleted by it is
// never returned: the restored Subscription adopts it.
func (r *operatorRecovery) attemptCSVNames(c *Client) ([]string, error) {
	created := func(name, uid string) bool {
		if r.csvRemoved {
			return true
		}
		preUID, existed := r.preCSVs[name]
		return !existed || (uid != "" && preUID != "" && uid != preUID)
	}
	var names []string
	add := func(name string) {
		if !containsString(names, name) {
			names = append(names, name)
		}
	}
	current, err := listRHOAICSVs(c)
	if err != nil {
		// Fall back to the CSVs the attempt was seen installing.
		for _, name := range r.attemptCSVs {
			if created(name, "") {
				add(name)
			}
		}
		return names, err
	}
	for _, csv := range current {
		if created(csv.Name, csv.UID) {
			add(csv.Name)
		}
	}
	return names, nil
}
