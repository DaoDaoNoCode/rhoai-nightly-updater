package operator

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const RestoreStepName = "restore_previous_operator"

// Recovery holds the desired state captured before an operation and
// what the operation has changed, so a failure restores exactly that.
type Recovery struct {
	Subscription map[string]any
	catalog      map[string]any

	// catalogChanged: the nightly CatalogSource was replaced or deleted
	// (NoteCatalogChanged, which also sets catalogRestore).
	catalogChanged bool
	// catalogWritten is the CatalogSource this operation applied (its UID
	// and generation; NoteCatalogWritten). The restore changes the catalog
	// only while it is still that object at that generation: one someone
	// else changed or replaced meanwhile is left as it is and reported.
	catalogWritten kube.ObjectMeta
	// catalogRestore puts the previous nightly catalog back. It is a field,
	// set only by the operations that change the catalog, so the gitops
	// profile's Update, which never does, does not reach a CatalogSource
	// write through the restore (pkg/cluster/rbac_gitops_test.go).
	catalogRestore func(r *Recovery, c *kube.Client) []string
	// SubscriptionChanged: the Subscription was deleted or replaced.
	SubscriptionChanged bool
	// subscriptionReplaced: the operation found another Subscription in
	// place of the one it was about to delete (a UID-guarded delete
	// conflicted). The restore leaves that one alone: it neither deletes
	// nor overwrites it.
	subscriptionReplaced bool
	// AttemptSubscriptionUID is the UID of the Subscription the operation
	// applied (noteAttemptSubscription). The restore deletes only that one,
	// or the previous Subscription when the operation applied none
	// (subscriptionToDelete); another object under the name is left alone.
	AttemptSubscriptionUID string
	// restoreStartingCSV, when set (Update's managed path, which leaves the
	// catalog as Git or a policy sets it), is the CSV the operation removed:
	// the restored Subscription pins it as startingCSV (unless its spec, Git's,
	// sets a pin itself), so OLM installs that
	// CSV again or reports that the catalog lacks it, and never installs
	// another build of the catalog in its place (operator-lifecycle-manager
	// pkg/controller/registry/resolver/resolver.go, getSubscriptionVariables).
	restoreStartingCSV string
	// CSVRemoved: the operation deleted the previously installed CSV
	// (removedCSVs names it), so OLM must reinstall the previous version.
	CSVRemoved  bool
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
	recorded *record.SubscriptionSnapshot
	// preCSVs maps each RHOAI CSV that existed before the operation to its
	// UID. A CSV not in it (or with another UID) was created by the attempt,
	// even when the operation deleted nothing (a first install).
	preCSVs map[string]string
}

// NoteCatalogChanged records that the operation replaced or deleted the
// nightly catalog, so a failure restores it.
func (r *Recovery) NoteCatalogChanged() {
	r.catalogChanged = true
	r.catalogRestore = (*Recovery).restoreNightlyCatalog
}

// NoteCatalogWritten records the CatalogSource the operation applied, from
// the API server's answer (body), so the restore changes only that object.
func (r *Recovery) NoteCatalogWritten(body []byte) {
	if meta, err := kube.ParseObjectMeta(body); err == nil {
		r.catalogWritten = meta
	}
}

// restoreNightlyCatalog re-applies the catalog captured before the
// operation, or deletes the one it created. When the operation applied a
// catalog, it does so only while the catalog is still that object at that
// generation (a spec change by anyone raises it; OLM's status writes do
// not): the apply carries the resourceVersion just read and the delete its
// UID, so a change made in between fails them instead of being undone. It
// returns the failures.
func (r *Recovery) restoreNightlyCatalog(c *kube.Client) []string {
	catalogPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, rhoai.CatalogName)
	metadata := map[string]any{"name": rhoai.CatalogName, "namespace": rhoai.CatalogNS}
	uid := ""
	if w := r.catalogWritten; w.UID != "" {
		now, found, err := kube.ReadObjectMeta(c, catalogPath)
		switch {
		case err != nil:
			return []string{"read the CatalogSource to restore it (it was left as it is): " + err.Error()}
		case !found || now.UID != w.UID || now.Generation != w.Generation:
			return []string{fmt.Sprintf("CatalogSource %s/%s was changed, replaced or deleted by someone else after this operation wrote it, so it was left as it is; check it (oc get catalogsource %s -n %s -o yaml)", rhoai.CatalogNS, rhoai.CatalogName, rhoai.CatalogName, rhoai.CatalogNS)}
		}
		metadata["resourceVersion"] = now.ResourceVersion
		uid = now.UID
	}
	if r.catalog != nil {
		obj := map[string]any{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource", "metadata": metadata, "spec": r.catalog["spec"]}
		if _, _, err := c.Apply(catalogPath, obj); err != nil {
			return []string{"CatalogSource: " + err.Error()}
		}
		return nil
	}
	if uid != "" {
		if _, err := kube.DeleteWithUID(c, catalogPath, uid); err != nil && !kube.IsK8sNotFound(err) {
			return []string{"remove the new CatalogSource: " + err.Error()}
		}
		return nil
	}
	if _, err := c.DeleteIfExists(catalogPath); err != nil {
		return []string{"remove the new CatalogSource: " + err.Error()}
	}
	return nil
}

func (r *Recovery) installPlanName() string {
	status, _ := r.Subscription["status"].(map[string]any)
	ref, _ := status["installPlanRef"].(map[string]any)
	name, _ := ref["name"].(string)
	return name
}

func (r *Recovery) NoteAttemptCSV(name string) {
	if r == nil || name == "" {
		return
	}
	if slices.Contains(r.attemptCSVs, name) {
		return
	}
	r.attemptCSVs = append(r.attemptCSVs, name)
}

// NoteCSVRemoved records that the operation deleted the CSV name, which
// existed before it.
func (r *Recovery) NoteCSVRemoved(name string) {
	if r == nil {
		return
	}
	r.CSVRemoved = true
	if name != "" && !slices.Contains(r.removedCSVs, name) {
		r.removedCSVs = append(r.removedCSVs, name)
	}
}

func CaptureRecovery(c *kube.Client) (*Recovery, error) {
	r := &Recovery{}
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
		target *map[string]any
	}{
		{rhoai.SubscriptionPath(), &r.Subscription},
		{rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, rhoai.CatalogName), &r.catalog},
	} {
		var obj map[string]any
		found, err := c.GetIntoOptional(resource.path, &obj)
		if err != nil {
			if kube.IsDecodeError(err) {
				return nil, err
			}
			return nil, fmt.Errorf("capture operator recovery state: %w", err)
		}
		if !found {
			continue
		}
		if obj["spec"] != nil {
			*resource.target = obj
		}
	}
	if r.Subscription != nil {
		// Recorded so a later operation can recreate it after a crash left
		// no Subscription. Failing to record it never blocks the operation.
		if err := record.SaveSubscriptionSnapshot(c, r.Subscription); err != nil {
			slog.Warn("could not record the operator Subscription", "error", err)
		}
	} else if rec, err := record.LoadSubscriptionSnapshot(c); err != nil {
		slog.Warn("could not read the recorded operator Subscription", "error", err)
	} else {
		r.recorded = rec
	}
	return r, nil
}

// SubscriptionBase is the Subscription whose settings (spec.config, approval
// mode) a new Subscription carries over: the live one, else the recorded one.
func (r *Recovery) SubscriptionBase() map[string]any {
	if r == nil {
		return nil
	}
	if r.Subscription != nil {
		return r.Subscription
	}
	if r.recorded != nil && r.recorded.Spec != nil {
		return map[string]any{"spec": r.recorded.Spec}
	}
	return nil
}

// RecordedNote is logged when the recorded Subscription replaces a missing
// live one.
func (r *Recovery) RecordedNote() string {
	if r == nil || r.Subscription != nil || r.recorded == nil {
		return ""
	}
	return fmt.Sprintf("No operator Subscription exists; its settings (config, approval mode) are taken from the Subscription recorded at %s", r.recorded.RecordedAt)
}

// Restore puts back what a failed operation changed:
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
func (r *Recovery) Restore(c *kube.Client, result *types.OperationResponse, emit func(types.UpdateStepEvent)) {
	if r == nil || (result != nil && result.Success) || r.keepNewInstall {
		return
	}
	operatorTouched := r.SubscriptionChanged || r.CSVRemoved
	if !operatorTouched && !r.catalogChanged {
		return
	}
	if record.OperationLockLost(c) {
		// Another updater pod holds the lock and may be changing the same
		// Subscription and catalog: a restore would undo its work.
		msg := "Automatic operator recovery skipped: " + record.LockLostMessage
		slog.Warn("operator restore skipped: the cross-pod operation lock was lost")
		if result != nil {
			result.Logs = append(result.Logs, msg)
		}
		emit(types.UpdateStepEvent{Step: RestoreStepName, Status: "skipped", Message: msg, ErrorCode: "lock_lost"})
		return
	}

	var failures, stillDeleting []string
	subReplaced := r.subscriptionReplaced
	if subReplaced {
		failures = append(failures, "the Subscription was replaced by someone else while the operation ran, so it was left unchanged")
	}
	if operatorTouched {
		failures, stillDeleting, subReplaced = r.removeAttempt(c, failures, subReplaced)
	}
	applyCtx, cancelApply := record.PostOperationContext(c, record.RestoreApplyTimeout)
	defer cancelApply()
	c = c.WithContext(applyCtx)
	if r.catalogChanged && r.catalogRestore != nil {
		failures = append(failures, r.catalogRestore(r, c)...)
	}
	if operatorTouched && r.Subscription != nil && !subReplaced {
		failures = append(failures, r.reapplySubscription(c)...)
	}

	gitManaged := r.Subscription != nil && argo.TrackingOf(c, r.Subscription, argo.SubscriptionObject.Group, "Subscription").ManagedBy() != ""
	status, message := r.restoreMessage(failures, stillDeleting, operatorTouched, gitManaged)
	if result != nil {
		result.Logs = append(result.Logs, message)
		result.Message = strings.TrimSpace(result.Message + " " + message)
	}
	emit(types.UpdateStepEvent{Step: RestoreStepName, Status: status, Message: message})
}

// removeAttempt deletes the attempt's Subscription and the CSVs installed
// for it, within restoreCSVBudget. It returns failures with what failed
// added, the CSVs still being deleted, and whether the Subscription turned
// out to be replaced by someone else (subReplaced: already known).
func (r *Recovery) removeAttempt(c *kube.Client, failures []string, subReplaced bool) ([]string, []string, bool) {
	var stillDeleting []string
	delCtx, cancelDel := record.PostOperationContext(c, record.RestoreCSVBudget)
	cd := c.WithContext(delCtx)
	// Delete the Subscription that is there now (the attempt's), guarded
	// by the UID just read: one an administrator created in the
	// meantime is reported, and neither deleted nor overwritten. One
	// the operation already found replaced is not even read.
	replacedNow := false
	if !subReplaced {
		if err := r.deleteOwnSubscription(cd); errors.Is(err, errReplacedMeanwhile) && r.AttemptSubscriptionUID != "" {
			subReplaced, replacedNow = true, true
			failures = append(failures, "another Subscription replaced the one this operation created, so it and the CSVs installed for it were left unchanged")
		} else if errors.Is(err, errReplacedMeanwhile) {
			subReplaced = true
			failures = append(failures, "the Subscription was replaced by someone else while the restore ran, so it was left unchanged")
		} else if err != nil {
			failures = append(failures, "delete the new Subscription: "+err.Error())
		}
	}
	// A replacement found now may already have installed CSVs, which
	// the list below cannot tell from the attempt's: none is deleted.
	var csvs []rhoaiCSV
	var err error
	if !replacedNow {
		csvs, err = r.AttemptCSVsToRemove(cd)
	}
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
	return failures, stillDeleting, subReplaced
}

// reapplySubscription re-applies the previous Subscription, pinned to the
// CSV that was installed when the operation removed it; what failed, if
// anything.
func (r *Recovery) reapplySubscription(c *kube.Client) []string {
	var failures []string
	spec := r.Subscription["spec"]
	// A pin the restored spec sets itself (Git's) is kept.
	if prev, _ := spec.(map[string]any); r.CSVRemoved && r.restoreStartingCSV != "" && prev != nil && prev["startingCSV"] == nil {
		pinned := make(map[string]any, len(prev)+1)
		maps.Copy(pinned, prev)
		pinned["startingCSV"] = r.restoreStartingCSV
		spec = pinned
	}
	obj := map[string]any{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS}, "spec": spec}
	argo.KeepTracking(r.Subscription, obj)
	if _, _, err := c.Apply(rhoai.SubscriptionPath(), obj); err != nil {
		failures = append(failures, "Subscription: "+err.Error())
	}
	return failures
}

// restoreMessage is the restore step's status and message. gitManaged: Argo
// CD manages the restored Subscription (argoTracking.managedBy).
func (r *Recovery) restoreMessage(failures, stillDeleting []string, operatorTouched, gitManaged bool) (string, string) {
	var message string
	status := "success"
	switch {
	case len(failures) > 0:
		status = "failed"
		message = fmt.Sprintf("Automatic operator recovery failed: %s. Restore the previous Subscription/catalog before retrying, or re-run Update or Install a different build.", strings.Join(failures, "; "))
		if len(stillDeleting) > 0 {
			message += " " + stillDeletingNote(stillDeleting)
		}
	case len(stillDeleting) > 0:
		// Not a success: OLM cannot install a CSV with the same name until
		// the old one is gone, and a CSV stuck behind its finalizer needs a
		// look.
		status = "failed"
		restored := "The Subscription and catalog were restored to their previous state"
		if r.Subscription != nil {
			restored = "The previous catalog and Subscription were re-applied"
		}
		message = fmt.Sprintf("Automatic operator recovery is incomplete. %s, but %s", restored, stillDeletingNote(stillDeleting))
	case !operatorTouched:
		message = "The previous nightly catalog was restored; the Subscription and CSV were not changed."
	case !r.CSVRemoved && r.Subscription != nil:
		message = "The previous catalog and Subscription were restored; the installed operator was not changed."
	case r.Subscription == nil:
		message = "There was no previous Subscription, so the Subscription and CSV created by this attempt were removed and the catalog was restored to its previous state."
	case gitManaged:
		// Git's catalog serves one build: the restore makes OLM install it
		// again, which is the build that just failed.
		message = "The Subscription was recreated with Git's settings; OLM installs the build the catalog serves again. To go back to another build, change the catalog image in Git."
		if spec, _ := r.Subscription["spec"].(map[string]any); r.CSVRemoved && r.restoreStartingCSV != "" && spec["startingCSV"] == nil {
			message = fmt.Sprintf("The Subscription was recreated with Git's settings and startingCSV %s, the CSV that was installed: OLM installs the build the catalog serves again if the catalog has that CSV, and nothing else. To go back to another build, change the catalog image in Git.", r.restoreStartingCSV)
		}
	default:
		message = "The previous catalog and Subscription were restored; OLM will reinstall the previous operator version. Watch the operator status before retrying."
		if spec, _ := r.Subscription["spec"].(map[string]any); r.CSVRemoved && spec["installPlanApproval"] == "Manual" {
			message += " The Subscription uses Manual approval: approve its InstallPlan in the console to reinstall the operator."
		}
	}
	return status, message
}

// stillDeletingNote explains CSVs whose deletion did not finish within the
// restore budget.
func stillDeletingNote(names []string) string {
	return fmt.Sprintf("CSV %s from the failed attempt is still being deleted (OLM removes its webhooks and cluster RBAC first). "+
		"OLM cannot install a CSV with the same name until it is gone. Check it with: oc get csv -n %s %s. "+
		"If it stays, look at its finalizers and the OLM operator logs before retrying.",
		strings.Join(names, ", "), rhoai.SubNS, strings.Join(names, " "))
}

// rhoaiCSV is the identity of a non-copied RHOAI CSV in the operator
// namespace.
type rhoaiCSV struct {
	Name, UID string
}

// listRHOAICSVs lists the RHOAI CSVs (rhods-operator.*, not copied) in the
// operator namespace. A missing namespace is an empty list.
func listRHOAICSVs(c *kube.Client) ([]rhoaiCSV, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, ""), &list)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var out []rhoaiCSV
	for _, item := range list.Items {
		if strings.HasPrefix(item.Metadata.Name, rhoai.SubName+".") && item.Metadata.Labels["olm.copiedFrom"] == "" {
			out = append(out, rhoaiCSV{Name: item.Metadata.Name, UID: item.Metadata.UID})
		}
	}
	return out, nil
}

// errReplacedMeanwhile: a UID-guarded delete found another object with the
// same name (the API server answers 409 when a precondition UID does not
// match), so nothing was deleted.
var errReplacedMeanwhile = errors.New("replaced by another object")

// AttemptCSVsToRemove lists the RHOAI CSVs the restore deletes, each with the
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
func (r *Recovery) AttemptCSVsToRemove(c *kube.Client) ([]rhoaiCSV, error) {
	remove := func(csv rhoaiCSV) bool {
		if csv.UID == "" {
			return false // nothing to guard the delete with
		}
		preUID, existed := r.preCSVs[csv.Name]
		switch {
		case !existed || preUID != csv.UID:
			return true
		default:
			return slices.Contains(r.removedCSVs, csv.Name)
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
		var obj struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		found, getErr := c.GetIntoOptional(CSVPath(name), &obj)
		if getErr != nil {
			return out, err
		}
		if !found {
			continue
		}
		add(rhoaiCSV{Name: name, UID: obj.Metadata.UID})
	}
	// Still an error: a CSV the attempt was never seen with may be missed.
	return out, err
}

// deleteCSVByUIDAndWait deletes the CSV with the given UID and waits, up to
// csvDeletionTimeout, until no CSV with that UID exists (OLM's csv-cleanup
// finalizer removes the webhooks and cluster RBAC first; a new CSV with the
// same name cannot be created until then). A CSV with the same
// name but another UID is never deleted (errReplacedMeanwhile).
func deleteCSVByUIDAndWait(c *kube.Client, csv rhoaiCSV) (gone bool, err error) {
	path := CSVPath(csv.Name)
	if _, err := kube.DeleteWithUID(c, path, csv.UID); err != nil {
		switch {
		case kube.IsK8sNotFound(err):
			return true, nil
		case kube.IsK8sConflict(err):
			return false, errReplacedMeanwhile
		default:
			return false, err
		}
	}
	deadline := kube.Clock().Now().Add(csvDeletionTimeout)
	for {
		meta, found, err := kube.ReadObjectMeta(c, path)
		if err != nil {
			return false, err
		}
		if !found || meta.UID != csv.UID {
			return true, nil
		}
		if kube.Clock().Now().After(deadline) {
			return false, nil
		}
		if err := kube.Clock().Sleep(c.Context(), InstallPlanPollInterval); err != nil {
			return false, err
		}
	}
}

// noteAttemptSubscription records the UID of the operator Subscription an
// apply left. The API server answers a PATCH, server-side apply included,
// with the resulting object (kubernetes
// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/patch.go,
// PatchResource: transformResponseObject); only when that answer has no UID
// is the Subscription read once. r nil records nothing.
func (r *Recovery) noteAttemptSubscription(c *kube.Client, applied []byte) {
	if r == nil {
		return
	}
	if meta, err := kube.ParseObjectMeta(applied); err == nil && meta.UID != "" {
		r.AttemptSubscriptionUID = meta.UID
		return
	}
	if meta, found, err := kube.ReadObjectMeta(c, rhoai.SubscriptionPath()); err == nil && found {
		r.AttemptSubscriptionUID = meta.UID
	}
}

// subscriptionToDelete is the UID of the Subscription the restore may
// delete: the one the operation applied, else (it applied none) the
// previous one, which the restore then applies again.
func (r *Recovery) subscriptionToDelete() string {
	if r.AttemptSubscriptionUID != "" {
		return r.AttemptSubscriptionUID
	}
	meta, _ := r.Subscription["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	return uid
}

// deleteOwnSubscription deletes the Subscription subscriptionToDelete names,
// guarded by its UID: errReplacedMeanwhile when another object holds the
// name, nil when none does. Without a UID to delete, any Subscription found
// is someone else's.
func (r *Recovery) deleteOwnSubscription(c *kube.Client) error {
	if uid := r.subscriptionToDelete(); uid != "" {
		return deleteSubscriptionWithUID(c, uid)
	}
	_, found, err := kube.ReadObjectMeta(c, rhoai.SubscriptionPath())
	switch {
	case err != nil:
		return err
	case found:
		return errReplacedMeanwhile
	}
	return nil
}

// deleteSubscriptionWithUID deletes the operator Subscription only while it
// is the object with that UID; errReplacedMeanwhile when it is another one.
func deleteSubscriptionWithUID(c *kube.Client, uid string) error {
	if uid == "" {
		return fmt.Errorf("the Subscription has no UID to guard its deletion")
	}
	if _, err := kube.DeleteWithUID(c, rhoai.SubscriptionPath(), uid); err != nil {
		switch {
		case kube.IsK8sNotFound(err):
			return nil
		case kube.IsK8sConflict(err):
			return errReplacedMeanwhile
		default:
			return err
		}
	}
	return nil
}
