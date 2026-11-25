package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// OLM semantics relied on below (operator-framework/api pkg/operators/v1alpha1
// and operator-lifecycle-manager pkg/controller/operators/{catalog,olm}):
//   - A new Subscription without startingCSV installs the head (currentCSV)
//     of its channel; status.currentCSV names it and status.installedCSV is
//     set once that CSV exists.
//   - ResolutionFailed is set on every failed resolution and cleared by the
//     next successful one; OLM keeps retrying, so it can be transient right
//     after a catalog is recreated.
//   - BundleUnpacking is True while OLM's bundle unpack Job runs. OLM 4.23
//     and later add an image-pull hint to it when the Job's pod cannot pull
//     the bundle (operator-framework/operator-lifecycle-manager #3832); 4.22
//     and older give no message, so the wait reads the unpack pods
//     themselves (catalog.BundleUnpackPullFailures). BundleUnpackFailed and
//     InstallPlanFailed report a finished failure; OLM sets
//     BundleUnpackFailed only once the Job failed, at the latest after its
//     unpack timeout (olmBundleUnpackTimeout).
//   - InstallPlan phases end in Complete or Failed; RequiresApproval waits
//     for spec.approved when the Subscription uses Manual approval.
//   - A CSV moves Pending -> InstallReady -> Installing -> Succeeded. Failed
//     is not final: OLM moves it back to Pending when requirements change or
//     the deployment needs a reinstall, and an install that is not healthy
//     after 5 minutes goes Installing -> Failed (InstallCheckFailed).
//   - CatalogSourcesUnhealthy describes every catalog in scope, not only the
//     Subscription's, so it is reported but never treated as a failure.
var (
	// installTimeout is the longest the updater waits for OLM to install
	// the new operator: OLM's bundle unpack timeout and a minute, so that a
	// bundle pull that fails for a while (a node that has no pull secret
	// yet) can recover, or end in OLM's own verdict, BundleUnpackFailed,
	// before the wait gives up. It is a maximum, not a promise of OLM's
	// verdict: OLM's 10 minutes start when it creates the unpack Job, after
	// it resolved the Subscription. Seen on a ROSA HCP cluster: the first
	// unpack pod could not pull the bundle on a node that was being
	// replaced, and the Job's next pod, on a new node, started 8 minutes 16
	// seconds after the wait began.
	//
	// Time budget: the operation's deadline is record.OperationDeadline (15
	// minutes), and WaitForInstall ends InstallDeadlineReserve (10 s) before
	// it, so the wait lasts min(11 minutes, 15 minutes - the earlier steps -
	// 10 s). The longest waits of the earlier steps add up to 6 minutes
	// (the verification catalog and the nightly catalog each up to
	// catalog.ReadyTimeout plus PackageManifestPropagationWait, 2.5
	// minutes; the old CSV's deletion up to csvDeletionTimeout, 1 minute),
	// so even then the wait gets about 9 minutes, and ends with the
	// "operation's time budget" message. The restore after a failure does
	// not come out of that budget: it runs after the operation stopped, on
	// record.PostOperationContext with its own record.RecoveryTimeout
	// (Recovery.Restore, operator_recovery.go:247, 275 and 300), which the
	// pod's shutdown drain covers (TestShutdownBudgetFitsTerminationGracePeriod).
	installTimeout = olmBundleUnpackTimeout + time.Minute
	// resolutionFailedGrace is how long ResolutionFailed may persist before
	// the operation is treated as failed.
	resolutionFailedGrace = 60 * time.Second
	// bundlePullFailureGrace is how long the bundle unpack job may fail to
	// pull its image before the operation is treated as failed.
	bundlePullFailureGrace = 3 * time.Minute
	// csvFailedGrace is how long the new CSV may stay Failed before the
	// operation is treated as failed.
	csvFailedGrace = 90 * time.Second
	// CSVSucceededSettle is how long the new CSV must stay Succeeded before
	// the install counts as done. Right after a fresh install OLM can find
	// the CSV's webhooks or CRD conversion not available yet and move it
	// Succeeded -> Pending (NeedsReinstall) -> ... -> Succeeded again
	// (seen live: 2s after the first Succeeded). A failure in that second
	// pass must not be reported as a successful install.
	CSVSucceededSettle = 20 * time.Second
	// csvDeletionTimeout bounds the wait for a deleted CSV to disappear (OLM
	// runs its csv-cleanup finalizer first).
	csvDeletionTimeout = 60 * time.Second
)

// olmBundleUnpackTimeout is OLM's default bundle unpack timeout: the
// catalog operator's --bundle-unpack-timeout (openshift/operator-framework-olm
// release-4.22, staging/operator-lifecycle-manager/cmd/catalog/start.go:82),
// which OLM sets as the unpack Job's activeDeadlineSeconds
// (pkg/controller/bundle/bundle_unpacker.go:300-305). An OperatorGroup can
// change it with the operatorframework.io/bundle-unpack-timeout annotation
// (bundle_unpacker.go:40-44 and 307-319).
const olmBundleUnpackTimeout = 10 * time.Minute

func CSVPath(name string) string {
	return rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, name)
}

// BuildSubscription returns the Subscription for source/channel. Settings the
// user put on the previous Subscription are carried over: spec.config (env,
// resources, nodeSelector, tolerations, ...) and installPlanApproval. Only
// startingCSV is dropped, because it names a version of the previous channel.
func BuildSubscription(previous map[string]any, source, channel string) map[string]any {
	spec := map[string]any{}
	if prevSpec, ok := previous["spec"].(map[string]any); ok {
		maps.Copy(spec, prevSpec)
	}
	delete(spec, "startingCSV")
	spec["name"] = rhoai.SubName
	spec["channel"] = channel
	spec["source"] = source
	spec["sourceNamespace"] = rhoai.CatalogNS
	if approval, _ := spec["installPlanApproval"].(string); approval != "Manual" {
		spec["installPlanApproval"] = "Automatic"
	}
	return map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS},
		"spec":       spec,
	}
}

// EnsureNamespaceAndGroup makes sure the operator namespace has
// exactly one OperatorGroup that RHOAI can use. RHOAI CSVs support only the
// AllNamespaces install mode, which needs a global OperatorGroup (no
// targetNamespaces, no selector); with two OperatorGroups OLM fails the CSV
// with TooManyOperatorGroups (OLM doc/design/operatorgroups.md). A missing
// namespace or OperatorGroup is created; an existing unusable one is reported
// and left alone. With dryRun nothing is created.
func EnsureNamespaceAndGroup(c *kube.Client, dryRun bool) (logs []string, problem string, errorCode string) {
	nsPath := "/api/v1/namespaces/" + rhoai.SubNS
	nsMissing, groups, problem, errorCode := ReadNamespaceAndGroups(c)
	if problem != "" {
		return logs, problem, errorCode
	}
	switch {
	case nsMissing && dryRun:
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create namespace %s", rhoai.SubNS))
	case nsMissing:
		slog.Info("creating operator namespace", "namespace", rhoai.SubNS)
		ns := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": rhoai.SubNS}}
		if _, _, err := c.Apply(nsPath, ns); err != nil {
			return logs, fmt.Sprintf("Failed to create namespace %s: %v", rhoai.SubNS, err), kube.ErrorCodeFromK8sErr(err)
		}
		logs = append(logs, fmt.Sprintf("OK: Created namespace %s", rhoai.SubNS))
	}

	switch {
	case len(groups) > 0:
		ok, problem := GroupsUsable(groups)
		if problem != "" {
			return logs, problem, "prerequisites"
		}
		logs = append(logs, ok)
	case dryRun:
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create OperatorGroup in %s", rhoai.SubNS))
	default:
		slog.Info("creating OperatorGroup", "namespace", rhoai.SubNS)
		og := map[string]any{
			"apiVersion": "operators.coreos.com/v1",
			"kind":       "OperatorGroup",
			"metadata":   map[string]any{"name": "rhods-operator", "namespace": rhoai.SubNS},
			"spec":       map[string]any{},
		}
		if _, _, err := c.Apply(rhoai.NamespacedPath("operators.coreos.com/v1", "operatorgroups", rhoai.SubNS, "rhods-operator"), og); err != nil {
			return logs, fmt.Sprintf("Failed to create OperatorGroup in %s: %v", rhoai.SubNS, err), kube.ErrorCodeFromK8sErr(err)
		}
		logs = append(logs, fmt.Sprintf("OK: Created OperatorGroup in %s", rhoai.SubNS))
	}
	return logs, "", ""
}

// rhoaiOperatorGroup is what the OperatorGroup checks read of one OperatorGroup.
type rhoaiOperatorGroup struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		TargetNamespaces []string `json:"targetNamespaces"`
		Selector         any      `json:"selector"`
	} `json:"spec"`
}

// ReadNamespaceAndGroups reads whether the operator namespace exists
// and its OperatorGroups (none when the namespace is missing). It only
// reads; problem is set when either could not be read.
func ReadNamespaceAndGroups(c *kube.Client) (nsMissing bool, groups []rhoaiOperatorGroup, problem string, errorCode string) {
	_, found, err := c.GetOptional("/api/v1/namespaces/" + rhoai.SubNS)
	switch {
	case err != nil:
		return false, nil, fmt.Sprintf("Failed to check namespace %s: %v", rhoai.SubNS, err), kube.ErrorCodeFromK8sErr(err)
	case !found:
		return true, nil, "", ""
	}
	body, found, err := c.GetOptional(rhoai.NamespacedPath("operators.coreos.com/v1", "operatorgroups", rhoai.SubNS, ""))
	if err != nil {
		return false, nil, fmt.Sprintf("Failed to check OperatorGroups in %s: %v", rhoai.SubNS, err), kube.ErrorCodeFromK8sErr(err)
	}
	if found {
		var list struct {
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return false, nil, fmt.Sprintf("Failed to parse OperatorGroups in %s: %v", rhoai.SubNS, err), "validation"
		}
		if len(list.Items) > 0 {
			if err := json.Unmarshal(list.Items, &groups); err != nil {
				return false, nil, fmt.Sprintf("Failed to parse OperatorGroups in %s: %v", rhoai.SubNS, err), "validation"
			}
		}
	}
	return false, groups, "", ""
}

// GroupsUsable checks one or more existing OperatorGroups: exactly
// one, and global. It returns the log line when they are usable, else the
// problem.
func GroupsUsable(groups []rhoaiOperatorGroup) (ok, problem string) {
	if len(groups) > 1 {
		var names []string
		for _, g := range groups {
			names = append(names, g.Metadata.Name)
		}
		return "", fmt.Sprintf("Namespace %s has %d OperatorGroups (%s). OLM fails the RHOAI CSV with TooManyOperatorGroups; delete all but one global OperatorGroup and retry. Nothing was changed.", rhoai.SubNS, len(groups), strings.Join(names, ", "))
	}
	g := groups[0]
	if len(g.Spec.TargetNamespaces) > 0 || g.Spec.Selector != nil {
		return "", fmt.Sprintf("OperatorGroup %s in %s targets specific namespaces, but RHOAI supports only the AllNamespaces install mode. Remove spec.targetNamespaces and spec.selector from it and retry. Nothing was changed.", g.Metadata.Name, rhoai.SubNS)
	}
	return fmt.Sprintf("OK: OperatorGroup %s exists in %s", g.Metadata.Name, rhoai.SubNS), ""
}

// waitForCSVGone waits, up to csvDeletionTimeout, until a deleted CSV no
// longer exists. It returns false on timeout and the context error if the
// operation is canceled.
func waitForCSVGone(c *kube.Client, name string) (bool, error) {
	deadline := kube.Clock().Now().Add(csvDeletionTimeout)
	for {
		_, _, err := c.Get(CSVPath(name))
		if kube.IsK8sNotFound(err) {
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

// installOutcome is OLM's verdict on a new Subscription.
type installOutcome struct {
	Succeeded bool
	csv       string
	Message   string
	ErrorCode string
	// keepNewInstall is set when the wait ended while OLM was still
	// installing (InstallPlan created, CSV not Failed): undoing that would
	// throw away a probably working install.
	keepNewInstall bool
	// note is extra information for a successful install (e.g. a pending
	// admin acknowledgement).
	note string
}

type olmCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func describeCondition(c olmCondition) string {
	switch {
	case c.Reason != "" && c.Message != "":
		return c.Reason + ": " + c.Message
	case c.Message != "":
		return c.Message
	default:
		return c.Reason
	}
}

// InstallDeadlineReserve is kept free before the operation's own deadline so
// the install wait ends with its own, explicit outcome instead of a
// cancelled request.
const InstallDeadlineReserve = 10 * time.Second

// WaitForInstall follows the Subscription until OLM has installed its
// CSV (phase Succeeded and recorded as installedCSV), reports a failure, or
// installTimeout passes (or the operation's deadline comes near,
// whichever is first). Every wait is bounded. Progress is emitted under
// step.
//
// With Manual approval an InstallPlan is approved only when it installs
// exactly expectedCSV, the version the user confirmed (Refresh: the
// installed CSV; Update and Reinstall: the target's channel head).
// Anything else OLM proposes is reported and left for an administrator:
// Manual approval is how admins hold back upgrades, and OLM cannot undo an
// approved upgrade (RHOAI operator notes §1.3, §1.4).
func WaitForInstall(c *kube.Client, emit func(types.UpdateStepEvent), logs *[]string, recovery *Recovery, expectedCSV string) installOutcome {
	// waitStart is compared with the cluster's condition times (AdminAckNote).
	waitStart := time.Now()
	deadline := kube.Clock().Now().Add(installTimeout)
	budgetLimited := false
	if opDeadline, ok := c.Context().Deadline(); ok && opDeadline.Add(-InstallDeadlineReserve).Before(deadline) {
		deadline, budgetLimited = opDeadline.Add(-InstallDeadlineReserve), true
	}
	w := &installWait{c: c, emit: emit, logs: logs}
	approved := map[string]bool{}
	progress, failed, stopped := w.progress, w.failed, w.stopped

	for {
		if err := kube.Clock().Sleep(c.Context(), InstallPlanPollInterval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return stopped("timeout", "The operation deadline passed before OLM finished installing the operator")
			}
			return stopped("cancelled", "The operation was cancelled before OLM finished installing the operator")
		}
		if kube.Clock().Now().After(deadline) {
			if budgetLimited {
				return stopped("install_timeout", "OLM did not finish installing the operator within the operation's time budget")
			}
			return stopped("install_timeout", fmt.Sprintf("OLM did not finish installing the operator within %s", installTimeout))
		}

		var sub installWaitSubscription
		found, err := c.GetIntoOptional(rhoai.SubscriptionPath(), &sub)
		if err != nil {
			if kube.IsDecodeError(err) {
				continue
			}
			slog.Debug("error polling Subscription", "error", err)
			continue
		}
		if !found {
			return failed("olm_install_failed", "The Subscription was deleted while OLM was installing the operator; another user or tool may have changed it.")
		}

		resolutionFailing, pullFailing, outcome, done := w.subscriptionConditions(sub)
		if done {
			return outcome
		}

		if name := sub.Status.InstallPlanRef.Name; name != "" {
			w.ipName = name
		} else if name := sub.Status.InstallPlan.Name; name != "" {
			w.ipName = name
		}
		if w.ipName != "" {
			if outcome, done := checkInstallPlan(c, w.ipName, expectedCSV, approved, logs, progress); done {
				outcome.csv = w.csvName
				*logs = append(*logs, outcome.Message)
				return outcome
			}
		}

		name := sub.Status.CurrentCSV
		if name == "" {
			name = sub.Status.InstalledCSV
		}
		if name == "" {
			if w.ipName == "" && !resolutionFailing && !pullFailing {
				progress(w.resolveState(sub))
			}
			continue
		}
		w.csvName = name
		recovery.NoteAttemptCSV(name)
		if outcome, done := w.checkCSV(name, sub.Status.InstalledCSV, deadline, waitStart); done {
			return outcome
		}
	}
}

// installWait is the state of one WaitForInstall, kept across its
// polls.
type installWait struct {
	c    *kube.Client
	emit func(types.UpdateStepEvent)
	logs *[]string
	// When each failing condition was first seen in a row (zero: not
	// failing).
	resolutionSince, pullSince, csvFailedSince, succeededSince time.Time
	ipName, csvName, lastState, lastProgress                   string
	sawCSVFailed, catalogsNoted                                bool
	// unpacking: the last poll found the Subscription's BundleUnpacking
	// condition True (OLM is still unpacking the bundle).
	unpacking bool
}

// installWaitSubscription is what the wait reads of the Subscription.
type installWaitSubscription struct {
	Spec struct {
		Source  string `json:"source"`
		Channel string `json:"channel"`
	} `json:"spec"`
	Status struct {
		CurrentCSV     string `json:"currentCSV"`
		InstalledCSV   string `json:"installedCSV"`
		InstallPlanRef struct {
			Name string `json:"name"`
		} `json:"installPlanRef"`
		InstallPlan struct {
			Name string `json:"name"`
		} `json:"installplan"`
		Conditions []olmCondition `json:"conditions"`
	} `json:"status"`
}

// progress emits msg once, and keeps it as the last state.
func (w *installWait) progress(msg string) {
	w.lastState = msg
	if msg != w.lastProgress {
		w.lastProgress = msg
		*w.logs = append(*w.logs, "  "+msg)
		w.emit(types.UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: msg})
	}
}

// failed is the outcome of an install OLM failed.
func (w *installWait) failed(code, msg string) installOutcome {
	*w.logs = append(*w.logs, msg)
	return installOutcome{csv: w.csvName, Message: msg, ErrorCode: code}
}

// stopped is the outcome of a wait that ended before OLM did: the new
// install is kept while OLM is still installing it.
func (w *installWait) stopped(code, why string) installOutcome {
	keep := w.ipName != "" && !w.sawCSVFailed
	state := w.lastState
	if state == "" {
		state = "OLM had not reported progress"
	}
	msg := fmt.Sprintf("%s (last state: %s).", why, state)
	if keep {
		msg += " The new Subscription was kept because OLM is still installing it; watch the operator status and re-run Update or Install a different build if it does not finish."
	}
	if w.unpacking && w.ipName == "" {
		msg += fmt.Sprintf(" OLM was still unpacking the operator bundle, and it reports no unpack failure before its unpack timeout (%s unless the OperatorGroup in %s sets operatorframework.io/bundle-unpack-timeout). "+
			"See why with: oc get pods -n %s -l %s (an image pull error usually means that a node has no pull secret for the image yet, or that the image does not exist), then run the operation again.",
			olmBundleUnpackTimeout, rhoai.SubNS, rhoai.CatalogNS, catalog.BundleUnpackRefLabel)
	}
	*w.logs = append(*w.logs, msg)
	return installOutcome{csv: w.csvName, Message: msg, ErrorCode: code, keepNewInstall: keep}
}

// subscriptionConditions reads the Subscription's conditions: whether OLM's
// resolution or the bundle pull is failing (and still retried), or the
// failed outcome when done.
func (w *installWait) subscriptionConditions(sub installWaitSubscription) (resolutionFailing, pullFailing bool, outcome installOutcome, done bool) {
	progress, failed, logs := w.progress, w.failed, w.logs
	w.unpacking = false
	for _, cond := range sub.Status.Conditions {
		if cond.Status != "True" {
			continue
		}
		switch cond.Type {
		case "InstallPlanFailed":
			return false, false, failed("installplan_failed", "OLM reports that the InstallPlan failed: "+describeCondition(cond)), true
		case "BundleUnpackFailed":
			return false, false, failed("bundle_unpack_failed", "OLM could not unpack the operator bundle: "+describeCondition(cond)+
				". If this repeats, follow the OpenShift procedure for failing subscriptions: delete the failed bundle-unpack Job and its ConfigMap in openshift-marketplace (label operatorframework.io/bundle-unpack-ref), then run the operation again."), true
		case "ResolutionFailed":
			resolutionFailing = true
			if w.resolutionSince.IsZero() {
				w.resolutionSince = kube.Clock().Now()
			}
			if kube.Clock().Since(w.resolutionSince) >= resolutionFailedGrace {
				return false, false, failed("resolution_failed", fmt.Sprintf("OLM cannot resolve %s from %s/%s: %s", rhoai.SubName, sub.Spec.Source, sub.Spec.Channel, describeCondition(cond))), true
			}
			progress("OLM resolution failed, retrying: " + describeCondition(cond))
		case "BundleUnpacking":
			w.unpacking = true
			if strings.Contains(strings.ToLower(cond.Message), "pull") {
				pullFailing = true
				if w.pullSince.IsZero() {
					w.pullSince = kube.Clock().Now()
				}
				if kube.Clock().Since(w.pullSince) >= bundlePullFailureGrace {
					return false, false, failed("bundle_image_pull", "OLM cannot pull the operator bundle image: "+cond.Message+" Check the image mirror (IDMS) for "+rhoai.IDMSSource+" and the pull secret."), true
				}
				progress("Bundle image pull is failing, OLM keeps retrying: " + cond.Message)
			}
		case "CatalogSourcesUnhealthy":
			if !w.catalogsNoted {
				w.catalogsNoted = true
				*logs = append(*logs, "  Note: OLM reports unhealthy CatalogSources: "+describeCondition(cond))
			}
		}
	}
	if !resolutionFailing {
		w.resolutionSince = time.Time{}
	}
	if !pullFailing {
		w.pullSince = time.Time{}
	}
	return resolutionFailing, pullFailing, installOutcome{}, false
}

// resolveConditionTypes are the Subscription conditions OLM sets before it
// creates an InstallPlan (operator-framework/api pkg/operators/v1alpha1
// subscription_types.go:108-126): what a wait without an InstallPlan or a
// CSV can report.
var resolveConditionTypes = []string{"CatalogSourcesUnhealthy", "ResolutionFailed", "BundleUnpacking", "BundleUnpackFailed"}

// resolveState words the wait before OLM names a CSV or an InstallPlan: the
// Subscription's resolution conditions as OLM set them and, while OLM
// unpacks the bundle, why its unpack pods wait (an image they cannot pull).
func (w *installWait) resolveState(sub installWaitSubscription) string {
	state := "Waiting for OLM to resolve the Subscription"
	if w.unpacking {
		state = "OLM is unpacking the operator bundle"
	}
	var conds []string
	for _, cond := range sub.Status.Conditions {
		if !slices.Contains(resolveConditionTypes, cond.Type) {
			continue
		}
		detail := cond.Reason
		if cond.Status == "True" && cond.Message != "" {
			detail = describeCondition(olmCondition{Reason: cond.Reason, Message: kube.TruncateText(cond.Message, 300)})
		}
		conds = append(conds, strings.TrimSpace(cond.Type+"="+cond.Status+" "+parenthesize(detail)))
	}
	if len(conds) > 0 {
		state += "; Subscription conditions: " + strings.Join(conds, ", ")
	}
	if !w.unpacking {
		return state
	}
	pulls, err := catalog.BundleUnpackPullFailures(w.c)
	switch {
	case err != nil:
		state += fmt.Sprintf(". OLM's bundle unpack pods in %s could not be read: %v", rhoai.CatalogNS, err)
	case len(pulls) > 0:
		state += ". " + strings.Join(pulls, "; ")
	}
	return state
}

// checkCSV reads CSV name, the Subscription's current one: the outcome when
// it failed for csvFailedGrace or kept Succeeded (as installedCSV) for
// CSVSucceededSettle, else done is false and the wait goes on.
func (w *installWait) checkCSV(name, installedCSV string, deadline, waitStart time.Time) (installOutcome, bool) {
	c, progress, failed, logs, ipName := w.c, w.progress, w.failed, w.logs, w.ipName
	var csv struct {
		Metadata struct {
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase   string `json:"phase"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(CSVPath(name), &csv)
	if err != nil {
		return installOutcome{}, false
	}
	if !found {
		if ipName != "" {
			progress(fmt.Sprintf("InstallPlan %s: waiting for OLM to create CSV %s", ipName, name))
		}
		return installOutcome{}, false
	}
	if csv.Metadata.DeletionTimestamp != nil {
		// The previous CSV of the same name is still being removed.
		progress(fmt.Sprintf("Waiting for the previous CSV %s to finish deleting", name))
		return installOutcome{}, false
	}
	phase := csv.Status.Phase
	if phase == "Failed" {
		w.sawCSVFailed = true
		if w.csvFailedSince.IsZero() {
			w.csvFailedSince = kube.Clock().Now()
		}
		detail := describeCondition(olmCondition{Reason: csv.Status.Reason, Message: csv.Status.Message})
		if kube.Clock().Since(w.csvFailedSince) >= csvFailedGrace {
			return failed("csv_failed", fmt.Sprintf("CSV %s failed to install: %s", name, detail)), true
		}
		progress(fmt.Sprintf("CSV %s: Failed (%s); waiting to see whether OLM retries", name, detail))
		return installOutcome{}, false
	}
	w.csvFailedSince = time.Time{}
	if phase != "Succeeded" {
		w.succeededSince = time.Time{}
	}
	if phase == "Succeeded" && installedCSV == name {
		if w.succeededSince.IsZero() {
			w.succeededSince = kube.Clock().Now()
		}
		// Stay until the CSV has kept Succeeded for CSVSucceededSettle,
		// unless the next poll would pass the deadline: a CSV that is
		// Succeeded then is reported as installed.
		if kube.Clock().Since(w.succeededSince) < CSVSucceededSettle && kube.Clock().Now().Add(InstallPlanPollInterval).Before(deadline) {
			progress(fmt.Sprintf("CSV %s: Succeeded; confirming that OLM keeps it installed", name))
			return installOutcome{}, false
		}
		*logs = append(*logs, fmt.Sprintf("OK: CSV %s is Succeeded", name))
		outcome := installOutcome{Succeeded: true, csv: name, note: AdminAckNote(c, name, waitStart)}
		if outcome.note != "" {
			*logs = append(*logs, "Warning: "+outcome.note)
		}
		return outcome, true
	}
	if phase == "" {
		phase = "created"
	}
	progress(fmt.Sprintf("CSV %s: %s", name, strings.TrimSpace(phase+" "+parenthesize(csv.Status.Reason))))
	return installOutcome{}, false
}

func parenthesize(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// checkInstallPlan inspects the Subscription's InstallPlan. It returns done
// with a failed outcome when the plan failed or needs an approval this tool
// must not give: it approves only a plan that installs exactly expectedCSV.
func checkInstallPlan(c *kube.Client, name, expectedCSV string, approved map[string]bool, logs *[]string, progress func(string)) (installOutcome, bool) {
	path := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, name)
	body, _, err := c.Get(path)
	if err != nil {
		return installOutcome{}, false
	}
	var ip struct {
		Spec struct {
			Approved bool     `json:"approved"`
			CSVNames []string `json:"clusterServiceVersionNames"`
		} `json:"spec"`
		Status struct {
			Phase      string         `json:"phase"`
			Message    string         `json:"message"`
			Conditions []olmCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &ip) != nil {
		return installOutcome{}, false
	}
	switch ip.Status.Phase {
	case "Failed":
		detail := ip.Status.Message
		for _, cond := range ip.Status.Conditions {
			if cond.Status == "False" && (cond.Reason != "" || cond.Message != "") {
				detail = describeCondition(cond)
			}
		}
		if detail == "" {
			detail = "no reason given"
		}
		return installOutcome{Message: fmt.Sprintf("OLM InstallPlan %s failed: %s", name, detail), ErrorCode: "installplan_failed"}, true
	case "RequiresApproval":
		if ip.Spec.Approved || approved[name] {
			return installOutcome{}, false
		}
		proposed := strings.Join(ip.Spec.CSVNames, ", ")
		if proposed == "" {
			proposed = "no CSV"
		}
		if expectedCSV == "" || len(ip.Spec.CSVNames) != 1 || ip.Spec.CSVNames[0] != expectedCSV {
			want := "the version to install is unknown"
			if expectedCSV != "" {
				want = "the confirmed target is " + expectedCSV
			}
			return installOutcome{
				Message: fmt.Sprintf("InstallPlan %s needs manual approval and OLM proposes to install %s, but %s, so the tool did not approve it. "+
					"The Subscription uses Manual approval, which is how upgrades are held back; approve the plan in the console (Operators > Installed Operators) only if that version is intended.", name, proposed, want),
				ErrorCode: "approval_required",
			}, true
		}
		if _, _, err := c.Patch(path, []byte(`{"spec":{"approved":true}}`)); err != nil {
			return installOutcome{Message: fmt.Sprintf("InstallPlan %s needs manual approval and approving it failed: %v", name, err), ErrorCode: kube.ErrorCodeOr(err, "approval_required")}, true
		}
		approved[name] = true
		*logs = append(*logs, fmt.Sprintf("OK: Approved InstallPlan %s for %s (the Subscription keeps Manual approval for later upgrades)", name, strings.Join(ip.Spec.CSVNames, ", ")))
		return installOutcome{}, false
	case "":
		progress(fmt.Sprintf("InstallPlan %s created", name))
	default:
		progress(fmt.Sprintf("InstallPlan %s: %s", name, ip.Status.Phase))
	}
	return installOutcome{}, false
}

// Verdict is the outcome of comparing a target operator version with
// the installed CSV.
type Verdict int

const (
	// VerdictNotInstalled: no RHOAI CSV exists, so nothing can be downgraded.
	VerdictNotInstalled Verdict = iota
	// VerdictNotOlder: the target is the same version or newer.
	VerdictNotOlder
	// VerdictOlder: the target is older than the installed CSV.
	VerdictOlder
	// VerdictUnknown: a CSV is installed, but its version or the target's
	// cannot be determined. Callers must treat this like a possible
	// downgrade (fail closed): OLM has no downgrade path, and a silently
	// allowed downgrade can leave CRDs the older operator cannot serve
	// (RHOAI_OPERATOR_NOTES §1.4).
	VerdictUnknown
)

// installedCSVVersion returns the version of the installed CSV. The CSV's
// spec.version is authoritative (OLM orders bundles by it); the
// "<package>.<version>" naming convention is the fallback.
func installedCSVVersion(installed types.CSVInfo) (registry.Tag, bool) {
	if installed.Version != "" {
		if v, ok := registry.ParseCSVVersion(rhoai.SubName + "." + installed.Version); ok {
			return v, true
		}
	}
	return registry.ParseCSVVersion(installed.Name)
}

// csvInstalled reports whether getCSV found an installed RHOAI CSV.
func csvInstalled(installed types.CSVInfo) bool {
	return installed.Name != ""
}

// CompareWithInstalled compares the bundle targetCSV with the installed CSV.
// reason explains a VerdictUnknown.
func CompareWithInstalled(installed types.CSVInfo, targetCSV string) (verdict Verdict, reason string) {
	if !csvInstalled(installed) {
		return VerdictNotInstalled, ""
	}
	current, ok := installedCSVVersion(installed)
	if !ok {
		return VerdictUnknown, fmt.Sprintf("the version of the installed CSV %s (spec.version %q) cannot be read", installed.Name, installed.Version)
	}
	target, ok := registry.ParseCSVVersion(targetCSV)
	if !ok {
		if targetCSV == "" {
			return VerdictUnknown, "the operator version the target installs is unknown"
		}
		return VerdictUnknown, fmt.Sprintf("the target bundle version %q cannot be read", targetCSV)
	}
	if registry.CompareTags(target, current) < 0 {
		return VerdictOlder, ""
	}
	return VerdictNotOlder, ""
}

// DowngradeCheck is Update's guard (the real run and the dry run). targetCSV
// is the bundle the target catalog installs (its channel head); when it is
// unknown the release tag is used, and a tag without a patch number
// ("rhoai-3.5") is treated as matching the installed patch, because a
// minor-stream build can ship any 3.5.z. Update has no downgrade
// confirmation, so a downgrade and an undeterminable version both block
// (fail closed) and point to Reinstall, which can confirm one. It returns
// the blocking message, plus a note to log.
func DowngradeCheck(installed types.CSVInfo, targetTag, targetCSV string) (blocked string, note string) {
	if !csvInstalled(installed) {
		return "", "No RHOAI operator is installed; no downgrade check needed"
	}
	const unknown = "Cannot rule out a downgrade: %s. OLM does not support downgrades, so Update refuses to continue. Nothing was changed. Use Install a different build (Recovery), which asks you to confirm."
	current, ok := installedCSVVersion(installed)
	if !ok {
		_, reason := CompareWithInstalled(installed, targetCSV)
		return fmt.Sprintf(unknown, reason), ""
	}
	if targetCSV != "" {
		switch verdict, _ := CompareWithInstalled(installed, targetCSV); verdict {
		case VerdictOlder:
			return fmt.Sprintf("Downgrade detected: the selected build installs %s, which is older than the installed %s. OLM does not support downgrades. Use Install a different build (Recovery) instead.", targetCSV, installed.Name), ""
		case VerdictNotOlder:
			return "", fmt.Sprintf("Target operator version: %s (installed: %s)", targetCSV, installed.Name)
		}
	}
	target, ok := registry.ParseTag(targetTag)
	if !ok {
		return fmt.Sprintf(unknown, fmt.Sprintf("the operator version that %q installs is unknown", targetTag)), ""
	}
	if m := registry.TagParseRegex.FindStringSubmatch(targetTag); m != nil && m[3] == "" {
		target.Patch = current.Patch
	}
	if registry.CompareTags(target, current) < 0 {
		return fmt.Sprintf("Downgrade detected: target %s is older than the installed %s. OLM does not support downgrades. Use Install a different build (Recovery) instead.", targetTag, installed.Name), ""
	}
	return "", fmt.Sprintf("Warning: the bundle version of %s is unknown; compared the release tag only", targetTag)
}

// StepTracker remembers the last pipeline step so a failure that returns
// early can mark the step that was running as failed.
type StepTracker struct {
	Emit   func(types.UpdateStepEvent)
	step   string
	status string
}

func (t *StepTracker) Send(e types.UpdateStepEvent) {
	if e.Step != RestoreStepName {
		t.step, t.status = e.Step, e.Status
	}
	t.Emit(e)
}

// Finish runs on every return path of a pipeline. It guarantees a result that
// keeps the collected logs, turns a context error into a failed result (so
// the message and restore details reach the user), marks the running step
// failed, restores the previous operator on failure, and records the
// activity once.
func (t *StepTracker) Finish(c *kube.Client, result **types.OperationResponse, opErr *error, logs []string, recovery *Recovery, recordOutcome func(ok bool, reason string)) {
	if *result == nil {
		*result = &types.OperationResponse{Success: false, Message: "Operation failed", Logs: logs}
	}
	r := *result
	if *opErr != nil {
		code := "cancelled"
		if errors.Is(*opErr, context.DeadlineExceeded) {
			code = "timeout"
		}
		r.Success = false
		if r.ErrorCode == "" {
			r.ErrorCode = code
		}
		if !strings.Contains(r.Message, (*opErr).Error()) {
			r.Message = strings.TrimSpace(r.Message + " (" + (*opErr).Error() + ")")
		}
		*opErr = nil
	}
	if record.OperationLockLost(c) {
		r.Success, r.ErrorCode = false, "lock_lost"
		if !strings.Contains(r.Message, record.LockLostMessage) {
			r.Message = record.LockLostMessage
		}
	}
	if !r.Success && t.status == "running" {
		t.Send(types.UpdateStepEvent{Step: t.step, Status: "failed", Message: r.Message, ErrorCode: r.ErrorCode})
	}
	if recovery != nil {
		recovery.Restore(c, r, t.Send)
		if r.Success {
			record.LiveSubscription(c)
		}
	}
	if recordOutcome != nil {
		reason := ""
		if !r.Success {
			reason = r.Message
		}
		recordOutcome(r.Success, reason)
	}
}

// Waiting for the upgrade-gate note. After OLM reports the CSV Succeeded,
// the new operator still has to start and reconcile before it reports a
// gate, so the note polls for at most UpgradeGateWait. The wait runs on the
// operation's own context and ends InstallDeadlineReserve before its
// deadline, so it stays inside the operation's time budget
// (operator_recovery.go). It ends at once when there is no
// DataScienceCluster, and as soon as the DSC reports the installed version.
var (
	UpgradeGateWait = 45 * time.Second
	upgradeGatePoll = 5 * time.Second
)

// upgradeGateSkew is how much older than the start of the install wait a
// condition's lastTransitionTime may be and still count as reported by the
// new operator (clock skew between the operator and this pod).
const upgradeGateSkew = 30 * time.Second

// upgradeGate is one condition that says provisioning waits on an upgrade
// gate.
type upgradeGate struct {
	where string // "Platform default", "DataScienceCluster default-dsc"
	cond  rhoai.DSCCondition
	fresh bool // reported by the newly installed operator
}

func (g upgradeGate) String() string {
	return fmt.Sprintf("%s %s=%s (%s): %s", g.where, g.cond.Type, g.cond.Status, g.cond.Reason, kube.TruncateText(g.cond.Message, 300))
}

// readUpgradeGates returns the gate conditions of the DataScienceCluster
// and of the Platform "default" (RHOAI 3.6 and later). reported is true
// once the DSC reports version as its release (the new operator has
// reconciled it); dscFound is false when there is no DSC to wait for.
// Read errors count as "nothing reported": the note is best effort.
//
// A gate is fresh (reported by the newly installed operator) when its
// condition changed after the install wait began, or when the DSC reports
// the installed version. The Platform has no release of its own and is
// left alone by operators older than 3.6, so its gates follow the same
// rule: a gate that only an earlier operator could have set is stale.
func readUpgradeGates(c *kube.Client, version string, since time.Time) (gates []upgradeGate, reported, dscFound bool) {
	type statusObject struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				rhoai.DSCCondition
				LastTransitionTime string `json:"lastTransitionTime"`
			} `json:"conditions"`
			Release struct {
				Version string `json:"version"`
			} `json:"release"`
		} `json:"status"`
	}
	collect := func(where string, obj statusObject) {
		for _, cond := range obj.Status.Conditions {
			if !components.IsUpgradeGateCondition(cond.DSCCondition) {
				continue
			}
			fresh := reported
			if t, ok := components.ParseK8sTime(cond.LastTransitionTime); ok && !t.Before(since.Add(-upgradeGateSkew)) {
				fresh = true
			}
			gates = append(gates, upgradeGate{where: where + " " + obj.Metadata.Name, cond: cond.DSCCondition, fresh: fresh})
		}
	}
	var dsc statusObject
	if path, err := rhoai.DataScienceClusterPath(c); err == nil {
		if body, _, err := c.Get(path); err == nil && json.Unmarshal(body, &dsc) == nil {
			dscFound = true
			reported = version != "" && dsc.Status.Release.Version == version
		}
	}
	if dscFound {
		collect("DataScienceCluster", dsc)
	}
	if r, err := rhoai.GetServed(c, rhoai.PlatformGroup, "/apis/config.opendatahub.io/%s/platforms/default", rhoai.PlatformFallbackVersions); err == nil {
		var pl statusObject
		if json.Unmarshal(r.Body, &pl) == nil {
			collect("Platform", pl)
		}
	}
	return gates, reported, dscFound
}

// AdminAckNote reports when the newly installed operator (CSV csvName)
// holds provisioning at an upgrade gate: an admin acknowledgement
// (AdminAckRequired) or a gate it cannot resolve, on the Platform or on
// any DataScienceCluster condition. That is a manual step, not a failed
// install. It is quiet when nothing is gated; since is when the install
// wait began. A gate that may date from before the install is reported
// with that caveat once the wait ends.
func AdminAckNote(c *kube.Client, csvName string, since time.Time) string {
	version := strings.TrimPrefix(csvName, rhoai.SubName+".")
	end := kube.Clock().Now().Add(UpgradeGateWait)
	if d, ok := c.Context().Deadline(); ok && d.Add(-InstallDeadlineReserve).Before(end) {
		end = d.Add(-InstallDeadlineReserve)
	}
	// Every read, discovery included, ends with the wait: a slow API server
	// cannot stretch it.
	ctx, cancel := kube.Clock().WithDeadline(c.Context(), end)
	defer cancel()
	c = c.WithContext(ctx)
	const staleHedge = " It was reported before this install finished, so it may clear once the new operator reconciles."
	var stale []string // from the last read that finished
	ended := func() string {
		if len(stale) > 0 {
			return upgradeGateMessage(stale, staleHedge)
		}
		return ""
	}
	for {
		gates, reported, dscFound := readUpgradeGates(c, version, since)
		if ctx.Err() != nil {
			return ended() // the wait ended during the reads
		}
		var fresh []string
		stale = nil
		for _, g := range gates {
			if g.fresh {
				fresh = append(fresh, g.String())
			} else {
				stale = append(stale, g.String())
			}
		}
		last := !dscFound || !kube.Clock().Now().Add(upgradeGatePoll).Before(end)
		switch {
		case len(fresh) > 0:
			return upgradeGateMessage(fresh, "")
		case reported:
			return ""
		case last:
			// Nothing more to wait for: no DSC, or the wait is over.
			return ended()
		}
		if kube.Clock().Sleep(ctx, upgradeGatePoll) != nil {
			return ended()
		}
	}
}

func upgradeGateMessage(gates []string, hedge string) string {
	return "The operator is installed, but it does not provision components while an upgrade gate holds it: " + strings.Join(gates, "; ") + "." + hedge +
		" Read what the gate is about: an admin acknowledgement is given by setting its key to \"true\" in ConfigMap odh-upgrade-acks in " + rhoai.SubNS +
		" (oc get configmap odh-upgrade-acks -n " + rhoai.SubNS + " shows them); a gate the operator cannot resolve usually means it is older than the resources it found. Check the Components page."
}

// ForeignSubscriptionRefusal reports another Subscription for the
// rhods-operator package in the operator namespace (for example one created
// by GitOps under a different name). The tool manages only the Subscription
// named rhods-operator; creating it next to another one for the same
// package makes OLM fail resolution, and the other one would reinstall the
// operator behind the tool's back. The ServiceAccount cannot list
// Subscriptions, so they are found through the InstallPlans OLM creates for
// them: each carries an ownerReference to its Subscription (live:
// install-wpkpm is owned by Subscription/rhods-operator). It returns "" when
// there is none; a failed lookup refuses, since nothing was changed yet.
func ForeignSubscriptionRefusal(c *kube.Client) string {
	var list struct {
		Items []struct {
			Metadata struct {
				Name            string `json:"name"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				CSVNames []string `json:"clusterServiceVersionNames"`
			} `json:"spec"`
		} `json:"items"`
	}
	exists, err := c.GetIntoOptional(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return fmt.Sprintf("Cannot check for other Subscriptions of %s (parse InstallPlans: %v). Nothing was changed.", rhoai.SubName, err)
		}
		return fmt.Sprintf("Cannot check for other Subscriptions of %s in %s (list InstallPlans: %v). Nothing was changed.", rhoai.SubName, rhoai.SubNS, err)
	}
	if !exists {
		return ""
	}
	var found []string
	for _, ip := range list.Items {
		forRHOAI := false
		for _, csv := range ip.Spec.CSVNames {
			forRHOAI = forRHOAI || strings.HasPrefix(csv, rhoai.SubName+".")
		}
		if !forRHOAI {
			continue
		}
		for _, o := range ip.Metadata.OwnerReferences {
			if o.Kind == "Subscription" && o.Name != rhoai.SubName && !slices.Contains(found, o.Name) {
				found = append(found, o.Name)
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("Another Subscription (%s) in %s installs the %s package. The tool manages only the Subscription named %s; creating it next to another one makes OLM fail to resolve, and the other one would reinstall the operator. "+
		"Nothing was changed. If the operator is managed elsewhere (for example by GitOps), make changes there, or delete that Subscription first.",
		strings.Join(found, ", "), rhoai.SubNS, rhoai.SubName, rhoai.SubName)
}
