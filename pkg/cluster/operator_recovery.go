package cluster

import (
	"context"
	"encoding/json"
	"errors"
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

// Time budget of one cluster operation, from start to the end of its
// bookkeeping. The pod's shutdown drain must cover all of it, and
// terminationGracePeriodSeconds (deploy/template.yaml) must cover the drain,
// the final marker flush and the HTTP server shutdown with a margin;
// budget_test.go checks the arithmetic.
//   - OperationDeadline: the context deadline pkg/api gives every mutation
//     (withMutationAuth in pkg/api/handlers.go). Every step, including the
//     Dashboard Dev revert, runs inside it; the install wait ends
//     installDeadlineReserve before it.
//   - RecoveryTimeout: the automatic restore after a failure, after the
//     operation stopped.
//   - postOperationBookkeeping: the activity entry, recording the
//     Subscription, and clearing the operation marker (pkg/api).
//
// Everything the cluster package does after the operation's deadline (the
// restore, cleanup and bookkeeping writes) runs on postOperationContext,
// which ends at the deadline plus postDeadlineWork at the latest, however
// many writes there are. Only the marker clear (MarkerClearTimeout) follows.
const (
	OperationDeadline = 15 * time.Minute
	// MarkerClearTimeout bounds clearing the operation marker when an
	// operation ends (pkg/api markerWriter.complete): waiting for a
	// background retry write that holds the lock (5s), then its own 5s write.
	MarkerClearTimeout = 10 * time.Second
	// MarkerFlushTimeout bounds main.go's last marker flush after the drain.
	MarkerFlushTimeout = 10 * time.Second
	// HTTPShutdownTimeout bounds main.go's http.Server.Shutdown after the
	// drain; no mutation is running by then.
	HTTPShutdownTimeout = 20 * time.Second
	// ShutdownMargin is kept free below terminationGracePeriodSeconds:
	// SIGKILL comes exactly at the grace period.
	ShutdownMargin = 10 * time.Second
)

// Bounds of the bookkeeping writes. Variables so tests can shorten them.
var (
	// activityWriteTimeout bounds one RecordActivity call, its conflict
	// retries included.
	activityWriteTimeout = 5 * time.Second
	// subscriptionRecordTimeout bounds recordLiveSubscription.
	subscriptionRecordTimeout = 5 * time.Second
)

// postDeadlineWork is the longest the cluster package works after an
// operation's deadline: the restore, then the activity entry and the
// Subscription record.
func postDeadlineWork() time.Duration {
	return RecoveryTimeout + activityWriteTimeout + subscriptionRecordTimeout
}

// postOperationBookkeeping is the bookkeeping that follows the restore.
func postOperationBookkeeping() time.Duration {
	return activityWriteTimeout + subscriptionRecordTimeout + MarkerClearTimeout
}

// ShutdownDrainTimeout is how long a terminating pod waits for running
// operations: the longest one can still take after SIGTERM.
func ShutdownDrainTimeout() time.Duration {
	return OperationDeadline + RecoveryTimeout + postOperationBookkeeping()
}

// postOperationContext returns a context for work that must still run when
// the operation's own context has ended (the restore, cleanup and
// bookkeeping writes). It keeps the values of c's context but not its
// cancellation, and ends after timeout or at the operation's deadline plus
// postDeadlineWork, whichever comes first. That shared end keeps all of
// this work inside ShutdownDrainTimeout however many writes follow the
// deadline. A context without a deadline gets only timeout.
//
// After the operation lost its cross-pod lock (ErrOperationLockLost) the
// context is already cancelled: another updater pod may be changing the
// same objects, so no cleanup or restore may run.
func postOperationContext(c *Client, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := bookkeepingContext(c, timeout)
	if operationLockLost(c) {
		cancel()
		ctx, cancelCause := context.WithCancelCause(context.Background())
		cancelCause(ErrOperationLockLost)
		return ctx, func() {}
	}
	return ctx, cancel
}

// ErrOperationLockLost is the cancellation cause of an operation whose
// cross-pod lock another updater pod took over, or that could not be
// renewed for so long that it may have expired (pkg/api operation_lease.go).
var ErrOperationLockLost = errors.New("the cross-pod operation lock was lost")

// LockLostMessage is the result of an operation stopped by
// ErrOperationLockLost.
const LockLostMessage = "Another updater pod took over the lock, so this operation stopped and made no further changes (no automatic restore). " +
	"Check the cluster state and re-run the operation after the other one finishes."

// operationLockLost reports whether c's operation was stopped because its
// cross-pod lock was lost.
func operationLockLost(c *Client) bool {
	return c != nil && c.ctx != nil && errors.Is(context.Cause(c.ctx), ErrOperationLockLost)
}

// bookkeepingContext is postOperationContext for the updater's own records
// (the activity log), which are written even after the lock was lost.
func bookkeepingContext(c *Client, timeout time.Duration) (context.Context, context.CancelFunc) {
	parent := context.Background()
	end := time.Now().Add(timeout)
	if c != nil && c.ctx != nil {
		parent = context.WithoutCancel(c.ctx)
		if d, ok := c.ctx.Deadline(); ok {
			if hard := d.Add(postDeadlineWork()); hard.Before(end) {
				end = hard
			}
		}
	}
	return context.WithDeadline(parent, end)
}

// operatorRecovery holds the desired state captured before an operation and
// what the operation has changed, so a failure restores exactly that.
type operatorRecovery struct {
	subscription map[string]interface{}
	catalog      map[string]interface{}

	// catalogChanged: the nightly CatalogSource was replaced or deleted.
	catalogChanged bool
	// subscriptionChanged: the Subscription was deleted or replaced.
	subscriptionChanged bool
	// csvRemoved: the operation deleted the previously installed CSV
	// (removedCSVs names it), so OLM must reinstall the previous version.
	csvRemoved  bool
	removedCSVs []string
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

// noteCSVRemoved records that the operation deleted the CSV name, which
// existed before it.
func (r *operatorRecovery) noteCSVRemoved(name string) {
	if r == nil {
		return
	}
	r.csvRemoved = true
	if name != "" && !containsString(r.removedCSVs, name) {
		r.removedCSVs = append(r.removedCSVs, name)
	}
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
	if operationLockLost(c) {
		// Another updater pod holds the lock and may be changing the same
		// Subscription and catalog: a restore would undo its work.
		msg := "Automatic operator recovery skipped: " + LockLostMessage
		slog.Warn("operator restore skipped: the cross-pod operation lock was lost")
		if result != nil {
			result.Logs = append(result.Logs, msg)
		}
		emit(UpdateStepEvent{Step: restoreStepName, Status: "skipped", Message: msg, ErrorCode: "lock_lost"})
		return
	}

	var failures, stillDeleting []string
	subReplaced := false
	if operatorTouched {
		delCtx, cancelDel := postOperationContext(c, restoreCSVBudget)
		cd := c.WithContext(delCtx)
		// Delete the Subscription that is there now (the attempt's), guarded
		// by the UID just read: one an administrator created in the
		// meantime is reported, and neither deleted nor overwritten.
		if err := deleteCurrentSubscription(cd); errors.Is(err, errReplacedMeanwhile) {
			subReplaced = true
			failures = append(failures, "the Subscription was replaced by someone else while the restore ran, so it was left unchanged")
		} else if err != nil {
			failures = append(failures, "delete the new Subscription: "+err.Error())
		}
		csvs, err := r.attemptCSVsToRemove(cd)
		if err != nil {
			failures = append(failures, "list CSVs: "+err.Error())
		}
		for _, csv := range csvs {
			gone, err := deleteCSVByUIDAndWait(cd, csv)
			switch {
			case errors.Is(err, errReplacedMeanwhile):
				failures = append(failures, "CSV "+csv.Name+" was replaced by another one while the restore ran, so it was left in place")
			case err != nil && delCtx.Err() != nil:
				stillDeleting = append(stillDeleting, csv.Name)
			case err != nil:
				failures = append(failures, "delete CSV "+csv.Name+": "+err.Error())
			case !gone:
				stillDeleting = append(stillDeleting, csv.Name)
			}
		}
		cancelDel()
		if len(stillDeleting) > 0 {
			slog.Warn("CSV from the failed attempt is still being deleted", "csvs", stillDeleting)
		}
	}
	applyCtx, cancelApply := postOperationContext(c, restoreApplyTimeout)
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
	if operatorTouched && r.subscription != nil && !subReplaced {
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

// errReplacedMeanwhile: a UID-guarded delete found another object with the
// same name (the API server answers 409 when a precondition UID does not
// match), so nothing was deleted.
var errReplacedMeanwhile = errors.New("replaced by another object")

// attemptCSVsToRemove lists the RHOAI CSVs the restore deletes, each with the
// UID its delete is conditioned on:
//   - a CSV that did not exist before the operation, or whose name now has
//     another UID: the attempt created it (also after a failed first
//     install, when the operation deleted nothing);
//   - a CSV the operation itself deleted that still exists with its old UID
//     (its finalizer is still running): the restore waits for it, because
//     OLM cannot reinstall a CSV with that name until it is gone.
//
// Every other CSV that existed before the operation is kept, also when the
// operation deleted the previous CSV (for example a second CSV left in
// Replacing or Pending): the restored Subscription adopts it.
func (r *operatorRecovery) attemptCSVsToRemove(c *Client) ([]rhoaiCSV, error) {
	remove := func(csv rhoaiCSV) bool {
		if csv.UID == "" {
			return false // nothing to guard the delete with
		}
		preUID, existed := r.preCSVs[csv.Name]
		switch {
		case !existed || preUID != csv.UID:
			return true
		default:
			return containsString(r.removedCSVs, csv.Name)
		}
	}
	var out []rhoaiCSV
	add := func(csv rhoaiCSV) {
		for _, o := range out {
			if o.Name == csv.Name {
				return
			}
		}
		if remove(csv) {
			out = append(out, csv)
		}
	}
	current, err := listRHOAICSVs(c)
	if err == nil {
		for _, csv := range current {
			add(csv)
		}
		return out, nil
	}
	// Fall back to the CSVs the attempt was seen installing and the ones it
	// deleted, read one by one for their UIDs.
	for _, name := range append(append([]string{}, r.attemptCSVs...), r.removedCSVs...) {
		body, _, getErr := c.get(csvPath(name))
		if IsK8sError(getErr, 404) {
			continue
		}
		if getErr != nil {
			return out, err
		}
		var obj struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &obj) != nil {
			return out, err
		}
		add(rhoaiCSV{Name: name, UID: obj.Metadata.UID})
	}
	// Still an error: a CSV the attempt was never seen with may be missed.
	return out, err
}

// deleteCSVByUIDAndWait deletes the CSV with the given UID and waits, up to
// CSVDeletionTimeout, until no CSV with that UID exists (OLM's csv-cleanup
// finalizer removes the webhooks and cluster RBAC first; a new CSV with the
// same name cannot be created until then). A CSV with the same
// name but another UID is never deleted (errReplacedMeanwhile).
func deleteCSVByUIDAndWait(c *Client, csv rhoaiCSV) (gone bool, err error) {
	path := csvPath(csv.Name)
	if _, err := deleteWithUID(c, path, csv.UID); err != nil {
		switch {
		case IsK8sError(err, 404):
			return true, nil
		case IsK8sError(err, 409):
			return false, errReplacedMeanwhile
		default:
			return false, err
		}
	}
	deadline := time.Now().Add(CSVDeletionTimeout)
	for {
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			return false, err
		}
		if !found || meta.UID != csv.UID {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-c.ctx.Done():
			return false, c.ctx.Err()
		case <-time.After(InstallPlanPollInterval):
		}
	}
}

// deleteCurrentSubscription deletes the operator Subscription that exists
// now, guarded by the UID it was read with.
func deleteCurrentSubscription(c *Client) error {
	meta, found, err := readObjectMeta(c, subscriptionPath())
	if err != nil || !found {
		return err
	}
	if meta.UID == "" {
		return fmt.Errorf("the Subscription has no UID to guard its deletion")
	}
	if _, err := deleteWithUID(c, subscriptionPath(), meta.UID); err != nil {
		switch {
		case IsK8sError(err, 404):
			return nil
		case IsK8sError(err, 409):
			return errReplacedMeanwhile
		default:
			return err
		}
	}
	return nil
}
