package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The fixes of the "Left over from an earlier release" check. Each runs
// with the user's token (POST /api/diagnostics/fix and
// /api/diagnostics/fix/acknowledged go through withMutationAuth), reads
// everything again first, and changes nothing unless every check passes:
// the failure is still reported, the object is unchanged, and the user
// acknowledged what the tool cannot check.

// applyLeftoverFix runs a leftover fix; handled is false for other IDs. ack
// is what the user acknowledged (nil without an acknowledgement).
func applyLeftoverFix(c *Client, id string, ack *AckBinding) (*types.OperationResponse, bool) {
	if target, ok := strings.CutPrefix(id, fixLeftoverFields); ok {
		return fixLeftoverStoredFields(c, target, ack), true
	}
	if target, ok := strings.CutPrefix(id, fixLeftoverRecreate); ok {
		return fixLeftoverRecreateWorkload(c, target, ack), true
	}
	if name, ok := strings.CutPrefix(id, fixLeftoverRetry); ok {
		return fixLeftoverRetryDSC(c, name), true
	}
	return nil, false
}

func leftoverRefused(msg, code string, logs []string) *types.OperationResponse {
	return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code, Logs: logs}
}

// needsAcknowledgement refuses a fix whose acknowledgement was not given.
func needsAcknowledgement(what string, reasons []string) *types.OperationResponse {
	return leftoverRefused(fmt.Sprintf("%s needs your acknowledgement first: %s. Nothing was changed; confirm it in the Fix dialog.", what, strings.Join(reasons, "; ")), "acknowledgement_required", nil)
}

// ackMismatch refuses an acknowledged fix when what the user acknowledged
// (got, sent back from the problem they reviewed) is not what the fix
// would act on now (want, computed from a fresh read): the object's UID or
// resourceVersion changed, or the acknowledgement statement differs (its
// hash). nil when they match.
func ackMismatch(what string, got AckBinding, want *AckBinding, logs []string) *types.OperationResponse {
	if want == nil {
		return leftoverRefused(fmt.Sprintf("%s: the tool cannot bind your acknowledgement to the object now. Nothing was changed. Run diagnostics again.", what), "conflict", logs)
	}
	var changed []string
	if got.UID != want.UID {
		changed = append(changed, "its UID (it was deleted and created again)")
	}
	if got.ResourceVersion != want.ResourceVersion {
		changed = append(changed, "its resourceVersion (it was changed)")
	}
	if got.Hash != want.Hash {
		changed = append(changed, "what needs your acknowledgement")
	}
	if len(changed) == 0 {
		return nil
	}
	return leftoverRefused(fmt.Sprintf("%s changed since you reviewed it (%s), so your acknowledgement does not cover it. Nothing was changed. Run diagnostics again and review it.", what, strings.Join(changed, ", ")), "conflict", logs)
}

// leftoverWriteError words the API server's refusal of a leftover change:
// a failed precondition (409) is a change since the check. A failed JSON
// Patch test gets a bare 422 that names no test (see dsc_manager.go), so
// the stored-fields fix tells it apart by reading the object again
// (storedLeftover.patchError).
func leftoverWriteError(err error, what string, dryRun bool) *types.OperationResponse {
	var k *K8sError
	msg := err.Error()
	if errors.As(err, &k) && k.K8sMessage != "" {
		msg = k.K8sMessage
	}
	if kube.IsK8sError(err, http.StatusConflict) {
		return leftoverChanged(what, msg)
	}
	stage := "The API server refused the change"
	if dryRun {
		stage = "The API server refused the change in its dry run"
	}
	return leftoverRefused(fmt.Sprintf("%s: %s. Nothing was changed.", stage, msg), kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "validation"), []string{msg})
}

// leftoverChanged refuses a leftover change because the object changed
// after it was checked.
func leftoverChanged(what, serverMsg string) *types.OperationResponse {
	return leftoverRefused(fmt.Sprintf("%s changed after it was checked, and the API server refused the change (%s). The refusal may be that change or one of its own. Nothing was changed. Run diagnostics again.", what, serverMsg), "conflict", []string{serverMsg})
}

// --- Stored field types ------------------------------------------------------

// patchError words the API server's refusal of the fix's JSON Patch, which
// tests the object's uid and resourceVersion (patchOps). After a 422 the
// object is read again: when it is gone, or its uid or resourceVersion is
// not the one the patch tested, a test failed because it changed since the
// check; otherwise the 422 is the API server's own refusal, shown as is.
// A read that fails leaves the refusal as the API server gave it (nothing
// was changed either way).
func (st *storedLeftover) patchError(c *Client, err error, dryRun bool) *types.OperationResponse {
	if kube.IsK8sError(err, http.StatusUnprocessableEntity) {
		meta, found, rerr := kube.ReadObjectMeta(c, rhoai.ComponentListPath(st.Group, st.Version, st.Resource)+"/"+st.Name)
		if rerr == nil && (!found || meta.UID != st.UID || meta.ResourceVersion != st.ResourceVersion) {
			var k *K8sError
			msg := err.Error()
			if errors.As(err, &k) && k.K8sMessage != "" {
				msg = k.K8sMessage
			}
			return leftoverChanged(st.ref(), msg)
		}
	}
	return leftoverWriteError(err, st.ref(), dryRun)
}

func fixLeftoverStoredFields(c *Client, target string, ack *AckBinding) *types.OperationResponse {
	kind, name, ok := strings.Cut(target, "/")
	if !ok || !components.KindPattern.MatchString(kind) || !components.DNS1123Subdomain.MatchString(name) {
		return leftoverRefused(fixLeftoverFields+" needs <Kind>/<name>", "validation", nil)
	}
	s := components.ScanApplyReporters(c, components.AllApplyFailures)
	// The module CR is in one of the module API groups; a kind reported in
	// both is ambiguous and refused.
	var rf *components.ReportedApplyFailure
	for _, group := range rhoai.ModuleAPIGroups {
		if found := leftoverObject(s, group, kind, "", name, components.ApplyStoredType); found != nil {
			if rf != nil {
				return leftoverRefused(fmt.Sprintf("%s %s is reported in more than one API group (%s), so the tool cannot tell which object to change. Nothing was changed.", kind, name, strings.Join(rhoai.ModuleAPIGroups, ", ")), "prerequisites", nil)
			}
			rf = found
		}
	}
	if rf == nil {
		if len(s.Errs) > 0 {
			return leftoverRefused(fmt.Sprintf("Could not read what the operator reports (%s). Nothing was changed.", strings.Join(s.Errs, "; ")), "prerequisites", nil)
		}
		return components.NothingToDo(fmt.Sprintf("no CR reports a stored field type error about %s %s any more. Nothing was changed.", kind, name), nil)
	}
	st := analyzeStored(c, s, rf)
	logs := []string{"Reported by " + reportersText(rf)}
	if st.CRDNote != "" {
		logs = append(logs, st.CRDNote)
	}
	if st.Blocker != "" {
		return leftoverRefused(fmt.Sprintf("Not changing %s: %s. Nothing was changed.", st.ref(), st.Blocker), "prerequisites", logs)
	}
	changes := st.changes()
	if len(changes) == 0 {
		return components.NothingToDo(fmt.Sprintf("every field the error names already matches the installed CRD. If %s still reports the error after a minute, use Retry now on its problem. Nothing was changed.", reportersText(rf)), logs)
	}
	if st.needsReview() {
		if ack == nil {
			var reasons []string
			for _, lf := range changes {
				if lf.Review {
					reasons = append(reasons, fmt.Sprintf("%s: the stored %s may not mean the same as %s", lf.Path, fieldJSON(lf.Value), lf.afterText()))
				}
			}
			return needsAcknowledgement("Changing "+st.ref(), reasons)
		}
		if res := ackMismatch(st.ref(), *ack, st.problem().AcknowledgeBinding, logs); res != nil {
			return res
		}
	}

	ops := st.patchOps()
	path := rhoai.ComponentListPath(st.Group, st.Version, st.Resource) + "/" + st.Name
	if _, err := patchLeftoverObject(c, path, ops, true); err != nil {
		return st.patchError(c, err, true)
	}
	logs = append(logs, "The API server's dry run accepted the change")
	body, err := patchLeftoverObject(c, path, ops, false)
	if err != nil {
		return st.patchError(c, err, false)
	}
	var summary []string
	for _, lf := range changes {
		if lf.Change == "set-default" {
			summary = append(summary, fmt.Sprintf("%s %s -> %s", lf.Path, fieldJSON(lf.Value), fieldJSON(lf.After)))
		} else {
			summary = append(summary, fmt.Sprintf("%s %s -> removed", lf.Path, fieldJSON(lf.Value)))
		}
	}
	logs = append(logs, "Patched "+st.ref()+": "+strings.Join(summary, ", "))
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "fix-leftover-fields",
		Detail:    fmt.Sprintf("%s: %s", st.ref(), strings.Join(summary, ", ")),
		Success:   true,
	})
	var after map[string]any
	if err := json.Unmarshal(body, &after); err == nil {
		for _, lf := range changes {
			got, present := components.LookupPath(after, lf.Keys)
			if (lf.Change == "set-default" && (!present || !kube.JSONEqual(got, lf.After))) || (lf.Change == "remove" && present) {
				return &types.OperationResponse{Success: false, ErrorCode: "internal", Logs: logs,
					Message: fmt.Sprintf("The API server accepted the change, but %s is %s in the object it returned. Run diagnostics again and check %s.", lf.Path, fieldJSON(got), st.ref())}
			}
		}
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Changed %s: %s. The DataScienceCluster controller applies it again on its own; re-scan in a minute. If the error stays, the scan offers Retry now.", st.ref(), strings.Join(summary, ", ")),
		Logs:    logs,
	}
}

// patchLeftoverObject sends a JSON Patch with fieldValidation=Strict, as a
// server-side dry run when dryRun is set.
func patchLeftoverObject(c *Client, path string, ops []kube.JSONPatchOp, dryRun bool) ([]byte, error) {
	body, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {leftoverManager}, "fieldValidation": {"Strict"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	resp, _, err := c.Do(http.MethodPatch, path, "application/json-patch+json", body, q)
	return resp, err
}

// --- Retry ---------------------------------------------------------------------

// fixLeftoverRetryDSC makes the DataScienceCluster reconcile at once by
// adding and removing an annotation, when it still reports a stored-type
// error whose fields are all valid now.
func fixLeftoverRetryDSC(c *Client, name string) *types.OperationResponse {
	if !components.DNS1123Subdomain.MatchString(name) {
		return leftoverRefused(fixLeftoverRetry+" needs the DataScienceCluster's name", "validation", nil)
	}
	s := components.ScanApplyReporters(c, components.AllApplyFailures)
	var uid, waiting string
	for _, rf := range s.Found {
		if !rf.Leftover() || !rf.Has(components.ApplyStoredType) {
			continue
		}
		reported := false
		for _, r := range rf.Reporters {
			if r.Kind == "DataScienceCluster" && r.Name == name {
				reported, uid = true, r.UID
			}
		}
		if !reported {
			continue
		}
		st := analyzeStored(c, s, rf)
		if st.Blocker == "" && len(st.changes()) == 0 {
			waiting = st.ref()
			break
		}
	}
	if waiting == "" {
		return components.NothingToDo(fmt.Sprintf("DataScienceCluster %s reports no stored field error that is already fixed. Nothing was changed.", name), nil)
	}
	path, err := rhoai.DataScienceClusterPath(c)
	if err != nil {
		return leftoverRefused(fmt.Sprintf("Cannot find the DataScienceCluster: %v. Nothing was changed.", err), kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"), nil)
	}
	if !strings.HasSuffix(path, "/"+name) {
		return leftoverRefused(fmt.Sprintf("DataScienceCluster %s is not the cluster's DataScienceCluster. Nothing was changed.", name), "validation", nil)
	}
	stamp := record.NowRFC3339()
	add, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": uid, "annotations": map[string]any{leftoverRetryAnno: stamp}}})
	if _, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", add, url.Values{"fieldManager": {leftoverManager}}); err != nil {
		return leftoverWriteError(err, "DataScienceCluster "+name, false)
	}
	logs := []string{fmt.Sprintf("Added the annotation %s=%s to DataScienceCluster %s", leftoverRetryAnno, stamp, name)}
	remove, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{leftoverRetryAnno: nil}}})
	_, _, rmErr := c.Do(http.MethodPatch, path, "application/merge-patch+json", remove, url.Values{"fieldManager": {leftoverManager}})
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "fix-retry-dsc-reconcile",
		Detail:    fmt.Sprintf("annotated DataScienceCluster %s to retry %s", name, waiting),
		Success:   rmErr == nil,
	})
	if rmErr != nil {
		return &types.OperationResponse{Success: false, ErrorCode: "partial_failure", Logs: append(logs, "Error: "+rmErr.Error()),
			Message: fmt.Sprintf("The operator was asked to retry, but the annotation could not be removed again (%v). Remove it with: %s", rmErr, kube.ShellCommand("oc", "annotate", "datasciencecluster", name, leftoverRetryAnno+"-"))}
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Asked the operator to reconcile DataScienceCluster %s again (an annotation was added and removed). Re-scan in a minute.", name),
		Logs:    append(logs, "Removed the annotation again"),
	}
}

// --- Changed selectors ---------------------------------------------------------

// workloadPath is the API path of a workload kind the recreate fix handles.
func workloadPath(kind, namespace, name string) string {
	switch kind {
	case "Deployment":
		return rhoai.NamespacedPath("apps/v1", "deployments", namespace, name)
	case "StatefulSet":
		return rhoai.NamespacedPath("apps/v1", "statefulsets", namespace, name)
	case "DaemonSet":
		return rhoai.NamespacedPath("apps/v1", "daemonsets", namespace, name)
	case "Job":
		return rhoai.NamespacedPath("batch/v1", "jobs", namespace, name)
	}
	return ""
}

// readWorkloadAsUser reads any workload kind (the fix runs with the user's
// token).
func readWorkloadAsUser(c *Client, kind, namespace, name string) (workloadObject, bool, bool, error) {
	path := workloadPath(kind, namespace, name)
	if path == "" {
		return workloadObject{}, false, false, nil
	}
	body, _, err := c.Get(path)
	return decodeWorkload(body, err)
}

// deleteWorkload deletes with UID and resourceVersion preconditions and
// background propagation, as a server-side dry run when dryRun is set.
func deleteWorkload(c *Client, path, uid, resourceVersion string, dryRun bool) error {
	opts, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background",
		"preconditions": map[string]string{"uid": uid, "resourceVersion": resourceVersion},
	})
	var q url.Values
	if dryRun {
		q = url.Values{"dryRun": {"All"}}
	}
	_, _, err := c.Do(http.MethodDelete, path, "application/json", opts, q)
	return err
}

func fixLeftoverRecreateWorkload(c *Client, target string, ack *AckBinding) *types.OperationResponse {
	parts := strings.Split(target, "/")
	if len(parts) != 3 || workloadPath(parts[0], "x", "x") == "" || !components.DNS1123Label.MatchString(parts[1]) || !components.DNS1123Subdomain.MatchString(parts[2]) {
		return leftoverRefused(fixLeftoverRecreate+" needs <Deployment|StatefulSet|DaemonSet|Job>/<namespace>/<name>", "validation", nil)
	}
	kind, ns, name := parts[0], parts[1], parts[2]
	s := components.ScanApplyReporters(c, components.AllApplyFailures)
	rf := leftoverObject(s, components.LeftoverWorkloads[kind], kind, ns, name, components.ApplyImmutable)
	if rf == nil {
		if len(s.Errs) > 0 {
			return leftoverRefused(fmt.Sprintf("Could not read what the operator reports (%s). Nothing was deleted.", strings.Join(s.Errs, "; ")), "prerequisites", nil)
		}
		return components.NothingToDo(fmt.Sprintf("no CR reports that the selector of %s %s/%s changed any more. Nothing was deleted.", kind, ns, name), nil)
	}
	appNS := rhoai.ApplicationsNamespace(c)
	r := analyzeRecreate(c, rf, appNS, readWorkloadAsUser, dsc.ComponentUsers)
	logs := append([]string{"Reported by " + reportersText(rf)}, r.Warnings...)
	switch {
	case len(r.Blockers) > 0:
		return leftoverRefused(fmt.Sprintf("Not deleting %s: %s. Nothing was deleted.", r.ref(), strings.Join(r.Blockers, "; ")), "prerequisites", logs)
	case !r.Found:
		return components.NothingToDo(fmt.Sprintf("%s no longer exists; the operator creates it on its next reconcile. Nothing was deleted.", r.ref()), logs)
	case len(r.ackReasons()) > 0 && ack == nil:
		return needsAcknowledgement("Deleting "+r.ref(), r.ackReasons())
	case len(r.ackReasons()) > 0 && kind == "Deployment":
		// The scan read it too: the user reviewed this object and these
		// reasons.
		if res := ackMismatch(r.ref(), *ack, r.problem().AcknowledgeBinding, logs); res != nil {
			return res
		}
	case len(r.ackReasons()) > 0:
		// The scan cannot read this kind (readDeploymentForScan), so the
		// user reviewed it unread: their acknowledgement binds to the
		// reasons the scan showed, which are worked out again here the way
		// the scan does. What reading it with the user's token finds
		// beyond them (for example the webhooks its pods serve) was never
		// acknowledged, so it refuses.
		scan := analyzeRecreate(c, rf, appNS, readDeploymentForScan, dsc.ComponentUsers)
		if res := ackMismatch(r.ref(), *ack, scan.problem().AcknowledgeBinding, logs); res != nil {
			return res
		}
		shown := scan.ackReasons()
		var unseen []string
		for _, reason := range r.ackReasons() {
			if !slices.Contains(shown, reason) {
				unseen = append(unseen, reason)
			}
		}
		if len(unseen) > 0 {
			return leftoverRefused(fmt.Sprintf("Reading %s with your token found what the scan could not show, and your acknowledgement does not cover it: %s. Nothing was deleted. Check it, then delete it yourself with: %s",
				r.ref(), strings.Join(unseen, "; "), rf.OCCommand("delete")), "conflict", logs)
		}
	}
	logs = append(logs, fmt.Sprintf("%s is owned by %s; %s creates it again", r.ref(), r.Owner, r.Recreator))
	path := workloadPath(kind, ns, name)
	uid := r.Obj.Metadata.UID
	if err := deleteWorkload(c, path, uid, r.Obj.Metadata.ResourceVersion, true); err != nil {
		if kube.IsK8sError(err, http.StatusNotFound) {
			return components.NothingToDo(fmt.Sprintf("%s no longer exists. Nothing was deleted.", r.ref()), logs)
		}
		return leftoverWriteError(err, r.ref(), true)
	}
	if err := deleteWorkload(c, path, uid, r.Obj.Metadata.ResourceVersion, false); err != nil {
		if kube.IsK8sError(err, http.StatusNotFound) {
			return components.NothingToDo(fmt.Sprintf("%s no longer exists. Nothing was deleted.", r.ref()), logs)
		}
		return leftoverWriteError(err, r.ref(), false)
	}
	logs = append(logs, fmt.Sprintf("Deleted %s (UID %s, propagation Background)", r.ref(), uid))
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "fix-recreate-leftover",
		Detail:    fmt.Sprintf("deleted %s (selector changed; owned by %s) for its operator to recreate", r.ref(), r.Owner),
		Success:   true,
	})

	back, ready, waitErr := waitRecreated(c, kind, ns, name, uid)
	switch {
	case waitErr != nil:
		return &types.OperationResponse{Success: true, Logs: append(logs, "Stopped waiting: "+waitErr.Error()),
			Message: fmt.Sprintf("Deleted %s. The tool stopped waiting for it to come back (%v); re-scan to check that %s created it again.", r.ref(), waitErr, r.Recreator)}
	case ready:
		return &types.OperationResponse{Success: true, Logs: append(logs, "It is back and ready"),
			Message: fmt.Sprintf("Recreated %s: %s created it again with the current selector and its pods are ready. Re-scan to check that %s is ready.", r.ref(), r.Recreator, r.Owner)}
	case back:
		return &types.OperationResponse{Success: true, Logs: append(logs, "It is back, its pods are not ready yet"),
			Message: fmt.Sprintf("Deleted %s; %s created it again, but its pods were not ready within %s. Re-scan in a few minutes; the RHOAI pods check shows a pod that does not start.", r.ref(), r.Recreator, components.FormatDuration(leftoverRecreateWait))}
	}
	return &types.OperationResponse{Success: false, ErrorCode: "not_recreated", Logs: append(logs, "Not back yet"),
		Message: fmt.Sprintf("Deleted %s, but %s has not created it again within %s. It may still be backing off: re-scan in a few minutes, and if it stays missing restart %s.", r.ref(), r.Recreator, components.FormatDuration(leftoverRecreateWait), r.Recreator)}
}

// waitRecreated polls until the workload exists with a new UID and is
// ready, or leftoverRecreateWait passes.
func waitRecreated(c *Client, kind, ns, name, oldUID string) (back, ready bool, err error) {
	deadline := kube.Clock().Now().Add(leftoverRecreateWait)
	for {
		obj, found, _, readErr := readWorkloadAsUser(c, kind, ns, name)
		if readErr == nil && found && obj.Metadata.UID != oldUID {
			back = true
			if obj.ready(kind) {
				return true, true, nil
			}
		}
		if !kube.Clock().Now().Before(deadline) {
			return back, false, nil
		}
		if err := kube.Clock().Sleep(c.Context(), leftoverRecreatePoll); err != nil {
			return back, false, err
		}
	}
}
