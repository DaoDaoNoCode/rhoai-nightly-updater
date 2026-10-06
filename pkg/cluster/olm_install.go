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

// OLM semantics relied on below (operator-framework/api pkg/operators/v1alpha1
// and operator-lifecycle-manager pkg/controller/operators/{catalog,olm}):
//   - A new Subscription without startingCSV installs the head (currentCSV)
//     of its channel; status.currentCSV names it and status.installedCSV is
//     set once that CSV exists.
//   - ResolutionFailed is set on every failed resolution and cleared by the
//     next successful one; OLM keeps retrying, so it can be transient right
//     after a catalog is recreated.
//   - BundleUnpacking stays True (with an image-pull hint) while the unpack
//     job cannot pull the bundle; BundleUnpackFailed and InstallPlanFailed
//     report a finished failure.
//   - InstallPlan phases end in Complete or Failed; RequiresApproval waits
//     for spec.approved when the Subscription uses Manual approval.
//   - A CSV moves Pending -> InstallReady -> Installing -> Succeeded. Failed
//     is not final: OLM moves it back to Pending when requirements change or
//     the deployment needs a reinstall, and an install that is not healthy
//     after 5 minutes goes Installing -> Failed (InstallCheckFailed).
//   - CatalogSourcesUnhealthy describes every catalog in scope, not only the
//     Subscription's, so it is reported but never treated as a failure.
var (
	// OperatorInstallTimeout bounds the wait for OLM to install the new
	// operator. Together with the catalog waits before it, an operation stays
	// within the 15-minute mutation deadline.
	OperatorInstallTimeout = 8 * time.Minute
	// ResolutionFailedGrace is how long ResolutionFailed may persist before
	// the operation is treated as failed.
	ResolutionFailedGrace = 60 * time.Second
	// BundlePullFailureGrace is how long the bundle unpack job may fail to
	// pull its image before the operation is treated as failed.
	BundlePullFailureGrace = 3 * time.Minute
	// CSVFailedGrace is how long the new CSV may stay Failed before the
	// operation is treated as failed.
	CSVFailedGrace = 90 * time.Second
	// CSVDeletionTimeout bounds the wait for a deleted CSV to disappear (OLM
	// runs its csv-cleanup finalizer first).
	CSVDeletionTimeout = 60 * time.Second
)

func subscriptionPath() string {
	return namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
}

func csvPath(name string) string {
	return namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, name)
}

// buildSubscription returns the Subscription for source/channel. Settings the
// user put on the previous Subscription are carried over: spec.config (env,
// resources, nodeSelector, tolerations, ...) and installPlanApproval. Only
// startingCSV is dropped, because it names a version of the previous channel.
func buildSubscription(previous map[string]interface{}, source, channel string) map[string]interface{} {
	spec := map[string]interface{}{}
	if prevSpec, ok := previous["spec"].(map[string]interface{}); ok {
		for k, v := range prevSpec {
			spec[k] = v
		}
	}
	delete(spec, "startingCSV")
	spec["name"] = SubName
	spec["channel"] = channel
	spec["source"] = source
	spec["sourceNamespace"] = CatalogNS
	if approval, _ := spec["installPlanApproval"].(string); approval != "Manual" {
		spec["installPlanApproval"] = "Automatic"
	}
	return map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec":       spec,
	}
}

// ensureOperatorNamespaceAndGroup makes sure the operator namespace has
// exactly one OperatorGroup that RHOAI can use. RHOAI CSVs support only the
// AllNamespaces install mode, which needs a global OperatorGroup (no
// targetNamespaces, no selector); with two OperatorGroups OLM fails the CSV
// with TooManyOperatorGroups (OLM doc/design/operatorgroups.md). A missing
// namespace or OperatorGroup is created; an existing unusable one is reported
// and left alone. With dryRun nothing is created.
func ensureOperatorNamespaceAndGroup(c *Client, dryRun bool) (logs []string, problem string, errorCode string) {
	nsPath := "/api/v1/namespaces/" + SubNS
	_, _, err := c.get(nsPath)
	nsMissing := IsK8sError(err, 404)
	switch {
	case nsMissing && dryRun:
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create namespace %s", SubNS))
	case nsMissing:
		slog.Info("creating operator namespace", "namespace", SubNS)
		ns := map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": SubNS}}
		if _, _, err := c.apply(nsPath, ns); err != nil {
			return logs, fmt.Sprintf("Failed to create namespace %s: %v", SubNS, err), errorCodeFromK8sErr(err)
		}
		logs = append(logs, fmt.Sprintf("OK: Created namespace %s", SubNS))
	case err != nil:
		return logs, fmt.Sprintf("Failed to check namespace %s: %v", SubNS, err), errorCodeFromK8sErr(err)
	}

	var groups []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			TargetNamespaces []string    `json:"targetNamespaces"`
			Selector         interface{} `json:"selector"`
		} `json:"spec"`
	}
	if !nsMissing {
		body, _, err := c.get(namespacedPath("operators.coreos.com/v1", "operatorgroups", SubNS, ""))
		if err != nil && !IsK8sError(err, 404) {
			return logs, fmt.Sprintf("Failed to check OperatorGroups in %s: %v", SubNS, err), errorCodeFromK8sErr(err)
		}
		if err == nil {
			var list struct {
				Items json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(body, &list); err != nil {
				return logs, fmt.Sprintf("Failed to parse OperatorGroups in %s: %v", SubNS, err), "validation"
			}
			if len(list.Items) > 0 {
				if err := json.Unmarshal(list.Items, &groups); err != nil {
					return logs, fmt.Sprintf("Failed to parse OperatorGroups in %s: %v", SubNS, err), "validation"
				}
			}
		}
	}

	switch {
	case len(groups) > 1:
		var names []string
		for _, g := range groups {
			names = append(names, g.Metadata.Name)
		}
		return logs, fmt.Sprintf("Namespace %s has %d OperatorGroups (%s). OLM fails the RHOAI CSV with TooManyOperatorGroups; delete all but one global OperatorGroup and retry. Nothing was changed.", SubNS, len(groups), strings.Join(names, ", ")), "prerequisites"
	case len(groups) == 1:
		g := groups[0]
		if len(g.Spec.TargetNamespaces) > 0 || g.Spec.Selector != nil {
			return logs, fmt.Sprintf("OperatorGroup %s in %s targets specific namespaces, but RHOAI supports only the AllNamespaces install mode. Remove spec.targetNamespaces and spec.selector from it and retry. Nothing was changed.", g.Metadata.Name, SubNS), "prerequisites"
		}
		logs = append(logs, fmt.Sprintf("OK: OperatorGroup %s exists in %s", g.Metadata.Name, SubNS))
	case dryRun:
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create OperatorGroup in %s", SubNS))
	default:
		slog.Info("creating OperatorGroup", "namespace", SubNS)
		og := map[string]interface{}{
			"apiVersion": "operators.coreos.com/v1",
			"kind":       "OperatorGroup",
			"metadata":   map[string]interface{}{"name": "rhods-operator", "namespace": SubNS},
			"spec":       map[string]interface{}{},
		}
		if _, _, err := c.apply(namespacedPath("operators.coreos.com/v1", "operatorgroups", SubNS, "rhods-operator"), og); err != nil {
			return logs, fmt.Sprintf("Failed to create OperatorGroup in %s: %v", SubNS, err), errorCodeFromK8sErr(err)
		}
		logs = append(logs, fmt.Sprintf("OK: Created OperatorGroup in %s", SubNS))
	}
	return logs, "", ""
}

// waitForCSVGone waits, up to CSVDeletionTimeout, until a deleted CSV no
// longer exists. It returns false on timeout and the context error if the
// operation is canceled.
func waitForCSVGone(c *Client, name string) (bool, error) {
	deadline := time.Now().Add(CSVDeletionTimeout)
	for {
		_, _, err := c.get(csvPath(name))
		if IsK8sError(err, 404) {
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

// installOutcome is OLM's verdict on a new Subscription.
type installOutcome struct {
	succeeded bool
	csv       string
	message   string
	errorCode string
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

// installDeadlineReserve is kept free before the operation's own deadline so
// the install wait ends with its own, explicit outcome instead of a
// cancelled request.
const installDeadlineReserve = 10 * time.Second

// waitForOperatorInstall follows the Subscription until OLM has installed its
// CSV (phase Succeeded and recorded as installedCSV), reports a failure, or
// OperatorInstallTimeout passes (or the operation's deadline comes near,
// whichever is first). Every wait is bounded. Progress is emitted under
// step.
//
// With Manual approval an InstallPlan is approved only when it installs
// exactly expectedCSV, the version the user confirmed (Refresh: the
// installed CSV; Update and Reinstall: the target's channel head).
// Anything else OLM proposes is reported and left for an administrator:
// Manual approval is how admins hold back upgrades, and OLM cannot undo an
// approved upgrade (RHOAI operator notes §1.3, §1.4).
func waitForOperatorInstall(c *Client, step string, emit func(UpdateStepEvent), logs *[]string, recovery *operatorRecovery, expectedCSV string) installOutcome {
	deadline := time.Now().Add(OperatorInstallTimeout)
	budgetLimited := false
	if opDeadline, ok := c.ctx.Deadline(); ok && opDeadline.Add(-installDeadlineReserve).Before(deadline) {
		deadline, budgetLimited = opDeadline.Add(-installDeadlineReserve), true
	}
	var resolutionSince, pullSince, csvFailedSince time.Time
	var ipName, csvName, lastState, lastProgress string
	var sawCSVFailed, catalogsNoted bool
	approved := map[string]bool{}

	progress := func(msg string) {
		lastState = msg
		if msg != lastProgress {
			lastProgress = msg
			*logs = append(*logs, "  "+msg)
			emit(UpdateStepEvent{Step: step, Status: "running", Message: msg})
		}
	}
	failed := func(code, msg string) installOutcome {
		*logs = append(*logs, msg)
		return installOutcome{csv: csvName, message: msg, errorCode: code}
	}
	stopped := func(code, why string) installOutcome {
		keep := ipName != "" && !sawCSVFailed
		state := lastState
		if state == "" {
			state = "OLM had not reported progress"
		}
		msg := fmt.Sprintf("%s (last state: %s).", why, state)
		if keep {
			msg += " The new Subscription was kept because OLM is still installing it; watch the operator status and re-run Update or Reinstall if it does not finish."
		}
		*logs = append(*logs, msg)
		return installOutcome{csv: csvName, message: msg, errorCode: code, keepNewInstall: keep}
	}

	for {
		select {
		case <-c.ctx.Done():
			if errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
				return stopped("timeout", "The operation deadline passed before OLM finished installing the operator")
			}
			return stopped("cancelled", "The operation was cancelled before OLM finished installing the operator")
		case <-time.After(InstallPlanPollInterval):
		}
		if time.Now().After(deadline) {
			if budgetLimited {
				return stopped("install_timeout", "OLM did not finish installing the operator within the operation's time budget")
			}
			return stopped("install_timeout", fmt.Sprintf("OLM did not finish installing the operator within %s", OperatorInstallTimeout))
		}

		body, _, err := c.get(subscriptionPath())
		if IsK8sError(err, 404) {
			return failed("olm_install_failed", "The Subscription was deleted while OLM was installing the operator; another user or tool may have changed it.")
		}
		if err != nil {
			slog.Debug("error polling Subscription", "error", err)
			continue
		}
		var sub struct {
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
		if json.Unmarshal(body, &sub) != nil {
			continue
		}

		resolutionFailing, pullFailing := false, false
		for _, cond := range sub.Status.Conditions {
			if cond.Status != "True" {
				continue
			}
			switch cond.Type {
			case "InstallPlanFailed":
				return failed("installplan_failed", "OLM reports that the InstallPlan failed: "+describeCondition(cond))
			case "BundleUnpackFailed":
				return failed("bundle_unpack_failed", "OLM could not unpack the operator bundle: "+describeCondition(cond)+
					". If this repeats, follow the OpenShift procedure for failing subscriptions: delete the failed bundle-unpack Job and its ConfigMap in openshift-marketplace (label operatorframework.io/bundle-unpack-ref), then run the operation again.")
			case "ResolutionFailed":
				resolutionFailing = true
				if resolutionSince.IsZero() {
					resolutionSince = time.Now()
				}
				if time.Since(resolutionSince) >= ResolutionFailedGrace {
					return failed("resolution_failed", fmt.Sprintf("OLM cannot resolve %s from %s/%s: %s", SubName, sub.Spec.Source, sub.Spec.Channel, describeCondition(cond)))
				}
				progress("OLM resolution failed, retrying: " + describeCondition(cond))
			case "BundleUnpacking":
				if strings.Contains(strings.ToLower(cond.Message), "pull") {
					pullFailing = true
					if pullSince.IsZero() {
						pullSince = time.Now()
					}
					if time.Since(pullSince) >= BundlePullFailureGrace {
						return failed("bundle_image_pull", "OLM cannot pull the operator bundle image: "+cond.Message+" Check the image mirror (IDMS) for "+IDMSSource+" and the pull secret.")
					}
					progress("Bundle image pull is failing, OLM keeps retrying: " + cond.Message)
				}
			case "CatalogSourcesUnhealthy":
				if !catalogsNoted {
					catalogsNoted = true
					*logs = append(*logs, "  Note: OLM reports unhealthy CatalogSources: "+describeCondition(cond))
				}
			}
		}
		if !resolutionFailing {
			resolutionSince = time.Time{}
		}
		if !pullFailing {
			pullSince = time.Time{}
		}

		if name := sub.Status.InstallPlanRef.Name; name != "" {
			ipName = name
		} else if name := sub.Status.InstallPlan.Name; name != "" {
			ipName = name
		}
		if ipName != "" {
			if outcome, done := checkInstallPlan(c, ipName, expectedCSV, approved, logs, progress); done {
				outcome.csv = csvName
				*logs = append(*logs, outcome.message)
				return outcome
			}
		}

		name := sub.Status.CurrentCSV
		if name == "" {
			name = sub.Status.InstalledCSV
		}
		if name == "" {
			if ipName == "" && !resolutionFailing && !pullFailing {
				progress("Waiting for OLM to resolve the Subscription")
			}
			continue
		}
		csvName = name
		recovery.noteAttemptCSV(name)
		csvBody, _, err := c.get(csvPath(name))
		if IsK8sError(err, 404) {
			if ipName != "" {
				progress(fmt.Sprintf("InstallPlan %s: waiting for OLM to create CSV %s", ipName, name))
			}
			continue
		}
		if err != nil {
			continue
		}
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
		if json.Unmarshal(csvBody, &csv) != nil {
			continue
		}
		if csv.Metadata.DeletionTimestamp != nil {
			// The previous CSV of the same name is still being removed.
			progress(fmt.Sprintf("Waiting for the previous CSV %s to finish deleting", name))
			continue
		}
		phase := csv.Status.Phase
		if phase == "Failed" {
			sawCSVFailed = true
			if csvFailedSince.IsZero() {
				csvFailedSince = time.Now()
			}
			detail := describeCondition(olmCondition{Reason: csv.Status.Reason, Message: csv.Status.Message})
			if time.Since(csvFailedSince) >= CSVFailedGrace {
				return failed("csv_failed", fmt.Sprintf("CSV %s failed to install: %s", name, detail))
			}
			progress(fmt.Sprintf("CSV %s: Failed (%s); waiting to see whether OLM retries", name, detail))
			continue
		}
		csvFailedSince = time.Time{}
		if phase == "Succeeded" && sub.Status.InstalledCSV == name {
			*logs = append(*logs, fmt.Sprintf("OK: CSV %s is Succeeded", name))
			outcome := installOutcome{succeeded: true, csv: name, note: adminAckNote(c)}
			if outcome.note != "" {
				*logs = append(*logs, "Warning: "+outcome.note)
			}
			return outcome
		}
		if phase == "" {
			phase = "created"
		}
		progress(fmt.Sprintf("CSV %s: %s", name, strings.TrimSpace(phase+" "+parenthesize(csv.Status.Reason))))
	}
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
func checkInstallPlan(c *Client, name, expectedCSV string, approved map[string]bool, logs *[]string, progress func(string)) (installOutcome, bool) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, name)
	body, _, err := c.get(path)
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
		return installOutcome{message: fmt.Sprintf("OLM InstallPlan %s failed: %s", name, detail), errorCode: "installplan_failed"}, true
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
				message: fmt.Sprintf("InstallPlan %s needs manual approval and OLM proposes to install %s, but %s, so the tool did not approve it. "+
					"The Subscription uses Manual approval, which is how upgrades are held back; approve the plan in the console (Operators > Installed Operators) only if that version is intended.", name, proposed, want),
				errorCode: "approval_required",
			}, true
		}
		if _, _, err := c.patch(path, []byte(`{"spec":{"approved":true}}`)); err != nil {
			return installOutcome{message: fmt.Sprintf("InstallPlan %s needs manual approval and approving it failed: %v", name, err), errorCode: errorCodeOr(err, "approval_required")}, true
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

func errorCodeOr(err error, fallback string) string {
	if code := errorCodeFromK8sErr(err); code != "" {
		return code
	}
	return fallback
}

// versionVerdict is the outcome of comparing a target operator version with
// the installed CSV.
type versionVerdict int

const (
	// verdictNotInstalled: no RHOAI CSV exists, so nothing can be downgraded.
	verdictNotInstalled versionVerdict = iota
	// verdictNotOlder: the target is the same version or newer.
	verdictNotOlder
	// verdictOlder: the target is older than the installed CSV.
	verdictOlder
	// verdictUnknown: a CSV is installed, but its version or the target's
	// cannot be determined. Callers must treat this like a possible
	// downgrade (fail closed): OLM has no downgrade path, and a silently
	// allowed downgrade can leave CRDs the older operator cannot serve
	// (RHOAI_OPERATOR_NOTES §1.4).
	verdictUnknown
)

// installedCSVVersion returns the version of the installed CSV. The CSV's
// spec.version is authoritative (OLM orders bundles by it); the
// "<package>.<version>" naming convention is the fallback.
func installedCSVVersion(installed types.CSVInfo) (parsedTag, bool) {
	if installed.Version != "" {
		if v, ok := parseCSVVersion(SubName + "." + installed.Version); ok {
			return v, true
		}
	}
	return parseCSVVersion(installed.Name)
}

// csvInstalled reports whether getCSV found an installed RHOAI CSV.
func csvInstalled(installed types.CSVInfo) bool {
	return installed.Name != ""
}

// compareWithInstalled compares the bundle targetCSV with the installed CSV.
// reason explains a verdictUnknown.
func compareWithInstalled(installed types.CSVInfo, targetCSV string) (verdict versionVerdict, reason string) {
	if !csvInstalled(installed) {
		return verdictNotInstalled, ""
	}
	current, ok := installedCSVVersion(installed)
	if !ok {
		return verdictUnknown, fmt.Sprintf("the version of the installed CSV %s (spec.version %q) cannot be read", installed.Name, installed.Version)
	}
	target, ok := parseCSVVersion(targetCSV)
	if !ok {
		if targetCSV == "" {
			return verdictUnknown, "the operator version the target installs is unknown"
		}
		return verdictUnknown, fmt.Sprintf("the target bundle version %q cannot be read", targetCSV)
	}
	if compareTags(target, current) < 0 {
		return verdictOlder, ""
	}
	return verdictNotOlder, ""
}

// downgradeCheck is Update's guard (the real run and the dry run). targetCSV
// is the bundle the target catalog installs (its channel head); when it is
// unknown the release tag is used, and a tag without a patch number
// ("rhoai-3.5") is treated as matching the installed patch, because a
// minor-stream build can ship any 3.5.z. Update has no downgrade
// confirmation, so a downgrade and an undeterminable version both block
// (fail closed) and point to Reinstall, which can confirm one. It returns
// the blocking message, plus a note to log.
func downgradeCheck(installed types.CSVInfo, targetTag, targetCSV string) (blocked string, note string) {
	if !csvInstalled(installed) {
		return "", "No RHOAI operator is installed; no downgrade check needed"
	}
	const unknown = "Cannot rule out a downgrade: %s. OLM does not support downgrades, so Update refuses to continue. Nothing was changed. Use Reinstall, which asks you to confirm."
	current, ok := installedCSVVersion(installed)
	if !ok {
		_, reason := compareWithInstalled(installed, targetCSV)
		return fmt.Sprintf(unknown, reason), ""
	}
	if targetCSV != "" {
		switch verdict, _ := compareWithInstalled(installed, targetCSV); verdict {
		case verdictOlder:
			return fmt.Sprintf("Downgrade detected: the selected build installs %s, which is older than the installed %s. OLM does not support downgrades. Use Reinstall instead.", targetCSV, installed.Name), ""
		case verdictNotOlder:
			return "", fmt.Sprintf("Target operator version: %s (installed: %s)", targetCSV, installed.Name)
		}
	}
	target, ok := parseTag(targetTag)
	if !ok {
		return fmt.Sprintf(unknown, fmt.Sprintf("the operator version that %q installs is unknown", targetTag)), ""
	}
	if m := tagParseRegex.FindStringSubmatch(targetTag); m != nil && m[3] == "" {
		target.patch = current.patch
	}
	if compareTags(target, current) < 0 {
		return fmt.Sprintf("Downgrade detected: target %s is older than the installed %s. OLM does not support downgrades. Use Reinstall instead.", targetTag, installed.Name), ""
	}
	return "", fmt.Sprintf("Warning: the bundle version of %s is unknown; compared the release tag only", targetTag)
}

// stepTracker remembers the last pipeline step so a failure that returns
// early can mark the step that was running as failed.
type stepTracker struct {
	emit   func(UpdateStepEvent)
	step   string
	status string
}

func (t *stepTracker) send(e UpdateStepEvent) {
	if e.Step != restoreStepName {
		t.step, t.status = e.Step, e.Status
	}
	t.emit(e)
}

// finish runs on every return path of a pipeline. It guarantees a result that
// keeps the collected logs, turns a context error into a failed result (so
// the message and restore details reach the user), marks the running step
// failed, restores the previous operator on failure, and records the
// activity once.
func (t *stepTracker) finish(c *Client, result **types.OperationResponse, opErr *error, logs []string, recovery *operatorRecovery, record func(ok bool, reason string)) {
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
	if !r.Success && t.status == "running" {
		t.send(UpdateStepEvent{Step: t.step, Status: "failed", Message: r.Message, ErrorCode: r.ErrorCode})
	}
	if recovery != nil {
		recovery.restore(c, r, t.send)
		if r.Success {
			recordLiveSubscription(c)
		}
	}
	if record != nil {
		reason := ""
		if !r.Success {
			reason = r.Message
		}
		record(r.Success, reason)
	}
}

// adminAckNote reports when the newly installed operator holds provisioning
// for an admin acknowledgement (Platform "default" condition
// ProvisioningProgress=False, reason AdminAckRequired; rhods-operator
// docs/upgrade-ordering.md "Admin ack gates"). That is a manual gate, not a
// failed install. The Platform API does not exist before RHOAI 3.6 or right
// after a fresh install, which is not an error.
func adminAckNote(c *Client) string {
	body, _, err := c.get(clusterPath("config.opendatahub.io/v1alpha1", "platforms", "default"))
	if err != nil {
		return ""
	}
	var platform struct {
		Status struct {
			Conditions []olmCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &platform) != nil {
		return ""
	}
	for _, cond := range platform.Status.Conditions {
		if cond.Type == "ProvisioningProgress" && cond.Reason == "AdminAckRequired" {
			msg := "The operator is installed, but it does not provision components until an administrator acknowledges the upgrade (AdminAckRequired)"
			if cond.Message != "" {
				msg += ": " + cond.Message
			}
			return msg + ". Set the listed keys to \"true\" in ConfigMap odh-upgrade-acks in " + SubNS + "."
		}
	}
	return ""
}

// foreignSubscriptionRefusal reports another Subscription for the
// rhods-operator package in the operator namespace (for example one created
// by GitOps under a different name). The tool manages only the Subscription
// named rhods-operator; creating it next to another one for the same
// package makes OLM fail resolution, and the other one would reinstall the
// operator behind the tool's back. The ServiceAccount cannot list
// Subscriptions, so they are found through the InstallPlans OLM creates for
// them: each carries an ownerReference to its Subscription (live:
// install-wpkpm is owned by Subscription/rhods-operator). It returns "" when
// there is none; a failed lookup refuses, since nothing was changed yet.
func foreignSubscriptionRefusal(c *Client) string {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ""))
	if IsK8sError(err, 404) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("Cannot check for other Subscriptions of %s in %s (list InstallPlans: %v). Nothing was changed.", SubName, SubNS, err)
	}
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
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Sprintf("Cannot check for other Subscriptions of %s (parse InstallPlans: %v). Nothing was changed.", SubName, err)
	}
	var found []string
	for _, ip := range list.Items {
		rhoai := false
		for _, csv := range ip.Spec.CSVNames {
			rhoai = rhoai || strings.HasPrefix(csv, SubName+".")
		}
		if !rhoai {
			continue
		}
		for _, o := range ip.Metadata.OwnerReferences {
			if o.Kind == "Subscription" && o.Name != SubName && !containsString(found, o.Name) {
				found = append(found, o.Name)
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("Another Subscription (%s) in %s installs the %s package. The tool manages only the Subscription named %s; creating it next to another one makes OLM fail to resolve, and the other one would reinstall the operator. "+
		"Nothing was changed. If the operator is managed elsewhere (for example by GitOps), make changes there, or delete that Subscription first.",
		strings.Join(found, ", "), SubNS, SubName, SubName)
}
