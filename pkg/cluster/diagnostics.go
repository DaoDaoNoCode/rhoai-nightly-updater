package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// CheckResult reports the outcome of a single diagnostic check.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "pass", "fail", "warn"
	Detail string `json:"detail"`
}

// Problem describes a detected issue with plain-English guidance.
type Problem struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"` // "critical", "warning", "info"
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Evidence       []string `json:"evidence,omitempty"`
	Fix            string   `json:"fix,omitempty"`
	AutoFixable    bool     `json:"autoFixable"`
	AutoFixAction  string   `json:"autoFixAction,omitempty"`
	ConfirmMessage string   `json:"confirmMessage,omitempty"`
	// AffectedObjects lists the cluster objects the problem is about, as
	// "<Kind> <namespace>/<name>" or "<Kind> <name>". For auto-fixable
	// problems these are exactly the objects the fix may change.
	AffectedObjects []string `json:"affectedObjects,omitempty"`
	LearnMore       string   `json:"learnMore,omitempty"`
	TechnicalCmd    string   `json:"technicalCmd,omitempty"`
}

// DiagnosticsResponse is the response for the diagnostics endpoint.
type DiagnosticsResponse struct {
	Problems []Problem     `json:"problems"`
	Checks   []CheckResult `json:"checks"`
}

type checkOutput struct {
	problems []Problem
	check    CheckResult
}

type diagnosticCheck struct {
	name string
	fn   func(*Client) checkOutput
}

// diagnosticChecks run independently of each other; results are reported in
// this order.
var diagnosticChecks = []diagnosticCheck{
	{"Catalog health", checkCatalogHealth},
	{"Operator pods", checkOperatorPods},
	{"Subscription health", checkSubscriptionHealth},
	{"Operator installed", checkCSVHealth},
	{"Install plan", checkInstallPlanHealth},
	{"Pull secret", checkPullSecretHealth},
	{"Image mirror", checkImageMirror},
	{"Stale webhooks", checkStaleWebhooks},
	{"Node capacity", checkNodeCapacity},
	{"DataScienceCluster", checkDataScienceCluster},
	{"RHOAI pods", checkRHOAIPods},
	{"Platform modules", checkPlatformModules},
	{"Operator-managed config", checkManagedConfig},
}

var (
	// diagnosticsCheckTimeout bounds each check so one slow API call cannot
	// hold up the whole report.
	diagnosticsCheckTimeout = 20 * time.Second
	// diagnosticsParallelism bounds concurrent checks (and API requests).
	diagnosticsParallelism = 6
	// operatorStuckAfter is how long a Subscription may sit in a transitional
	// state before diagnostics reports it. It matches OLM's default
	// --bundle-unpack-timeout (10m, operator-lifecycle-manager
	// cmd/catalog/start.go), after which OLM itself fails a stuck InstallPlan.
	operatorStuckAfter = 10 * time.Minute
)

// DiagnoseCluster inspects the cluster for common RHOAI issues and returns
// a list of problems with suggested fixes and a set of health check results.
// Checks run concurrently, each with its own timeout.
func DiagnoseCluster(c *Client) (*DiagnosticsResponse, error) {
	outputs := make([]checkOutput, len(diagnosticChecks))
	sem := make(chan struct{}, diagnosticsParallelism)
	var wg sync.WaitGroup
	for i, chk := range diagnosticChecks {
		wg.Add(1)
		go func(i int, chk diagnosticCheck) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outputs[i] = runCheck(c, chk)
		}(i, chk)
	}
	wg.Wait()

	resp := &DiagnosticsResponse{Problems: []Problem{}, Checks: []CheckResult{}}
	seen := make(map[string]bool)
	for _, out := range outputs {
		resp.Checks = append(resp.Checks, out.check)
		for _, p := range out.problems {
			if !seen[p.ID] {
				seen[p.ID] = true
				resp.Problems = append(resp.Problems, p)
			}
		}
	}

	// Critical first, then warning, then info; check order within a severity.
	severityOrder := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(resp.Problems, func(i, j int) bool {
		return severityOrder[resp.Problems[i].Severity] < severityOrder[resp.Problems[j].Severity]
	})
	return resp, nil
}

func runCheck(c *Client, chk diagnosticCheck) checkOutput {
	ctx, cancel := context.WithTimeout(c.ctx, diagnosticsCheckTimeout)
	defer cancel()
	done := make(chan checkOutput, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- checkOutput{check: CheckResult{Name: chk.name, Status: "warn", Detail: fmt.Sprintf("Check failed: %v", r)}}
			}
		}()
		done <- chk.fn(c.WithContext(ctx))
	}()
	select {
	case out := <-done:
		out.check.Name = chk.name
		return out
	case <-ctx.Done():
		detail := fmt.Sprintf("Check did not finish within %s", diagnosticsCheckTimeout)
		if errors.Is(c.ctx.Err(), context.Canceled) {
			detail = "Check cancelled"
		}
		return checkOutput{check: CheckResult{Name: chk.name, Status: "warn", Detail: detail}}
	}
}

// --- Health check functions ---

func checkCatalogHealth(c *Client) checkOutput {
	cs, err := getCatalogSource(c)
	if err != nil {
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "fail", Detail: fmt.Sprintf("Failed to check catalog: %v", err)},
			problems: []Problem{{
				ID:          "catalog-check-failed",
				Severity:    "warning",
				Title:       "Could not check CatalogSource health",
				Description: fmt.Sprintf("The diagnostics check could not read the CatalogSource: %v", err),
				Fix:         "Check cluster connectivity and RBAC permissions.",
			}},
		}
	}

	if !cs.Exists {
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "pass", Detail: "No nightly catalog yet (normal for fresh clusters)"},
			problems: []Problem{{
				ID:          "catalog-missing",
				Severity:    "info",
				Title:       "Nightly CatalogSource does not exist",
				Description: "No nightly CatalogSource has been created yet. This is normal if you haven't run a nightly update.",
				Fix:         "Use the Update panel on the Dashboard to install a nightly build.",
			}},
		}
	}

	switch cs.State {
	case "READY":
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "pass", Detail: fmt.Sprintf("Nightly catalog is healthy (%s)", cs.Image)},
		}
	case "TRANSIENT_FAILURE":
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "fail", Detail: fmt.Sprintf("CatalogSource in TRANSIENT_FAILURE (image: %s)", cs.Image)},
			problems: []Problem{{
				ID:          "catalog-transient-failure",
				Severity:    "warning",
				Title:       "CatalogSource in TRANSIENT_FAILURE state",
				Description: fmt.Sprintf("CatalogSource %s is reporting TRANSIENT_FAILURE for image %s.", cs.Name, cs.Image),
				Evidence:    []string{fmt.Sprintf("State: %s", cs.State), fmt.Sprintf("Image: %s", cs.Image)},
				Fix:         "Wait 1-2 minutes — TRANSIENT_FAILURE often resolves on its own during initial FBC image pull. If it persists, verify the image tag exists on Quay and the pull secret is valid.",
				LearnMore: "TRANSIENT_FAILURE is often temporary during initial FBC image pull (1-2 minutes). If it persists:\n\n" +
					"**Common causes:** Wrong image tag, expired pull secret, image doesn't exist on registry, network issues to quay.io.\n\n" +
					"**Diagnosis commands:**\n" +
					"`oc logs -n openshift-marketplace -l olm.catalogSource=rhoai-catalog-dev`\n" +
					"`oc get pods -n openshift-marketplace -l olm.catalogSource=rhoai-catalog-dev`",
				TechnicalCmd: "oc get catalogsource " + CatalogName + " -n " + CatalogNS + " -o jsonpath='{.status.connectionState}'",
			}},
		}
	case "CONNECTING":
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "warn", Detail: "CatalogSource is connecting..."},
			problems: []Problem{{
				ID:          "catalog-connecting",
				Severity:    "info",
				Title:       "CatalogSource is connecting",
				Description: "The CatalogSource is currently connecting to the registry. This is normal during initial setup and should resolve within 1-2 minutes.",
				Fix:         "Wait a minute and re-scan. If the state persists, check the pull secret and IDMS configuration.",
			}},
		}
	default:
		return checkOutput{
			check: CheckResult{Name: "Catalog health", Status: "warn", Detail: fmt.Sprintf("CatalogSource state: %s", cs.State)},
		}
	}
}

// rhoaiPodNamespaces are the namespaces whose pods the "RHOAI pods" check
// scans, besides namespaces labelled platform.opendatahub.io/part-of (which
// the operator creates for platform modules). The operator namespace is
// covered by the "Operator pods" check. User workbench namespaces such as
// rhods-notebooks are left out on purpose.
var rhoaiPodNamespaces = []string{"redhat-ods-applications", "redhat-ods-monitoring"}

func checkOperatorPods(c *Client) checkOutput {
	const name = "Operator pods"
	issues, podCount, scanned, errs := scanPods(c, []string{SubNS}, time.Now())
	if len(errs) > 0 {
		return checkOutput{check: CheckResult{Name: name, Status: "warn", Detail: "Could not list operator pods: " + strings.Join(errs, "; ")}}
	}
	if len(scanned) == 0 {
		return checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: "No operator namespace (operator not installed yet)"}}
	}
	if podCount == 0 {
		return checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: "No operator pods (operator not installed yet)"}}
	}
	return podIssuesOutput(c, name, issues, podCount, scanned, true)
}

func checkRHOAIPods(c *Client) checkOutput {
	const name = "RHOAI pods"
	namespaces := append([]string{}, rhoaiPodNamespaces...)
	// Platform module namespaces (for example opendatahub-ogx-system).
	body, _, err := c.do(http.MethodGet, clusterPath("v1", "namespaces", ""), "", nil,
		url.Values{"labelSelector": {"platform.opendatahub.io/part-of"}})
	if err == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &list) == nil {
			for _, item := range list.Items {
				if item.Metadata.Name != SubNS && !containsString(namespaces, item.Metadata.Name) {
					namespaces = append(namespaces, item.Metadata.Name)
				}
			}
		}
	}

	issues, podCount, scanned, errs := scanPods(c, namespaces, time.Now())
	out := podIssuesOutput(c, name, issues, podCount, scanned, false)
	if len(errs) > 0 {
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
		out.check.Detail += "; could not list pods in " + strings.Join(errs, "; ")
	}
	return out
}

func podIssuesOutput(c *Client, name string, issues []podIssue, podCount int, scanned []string, critical bool) checkOutput {
	if len(issues) == 0 {
		return checkOutput{check: CheckResult{Name: name, Status: "pass",
			Detail: fmt.Sprintf("%d pod(s) healthy in %s", podCount, strings.Join(scanned, ", "))}}
	}
	out := checkOutput{}
	var summaries []string
	failing := false
	for _, g := range groupPodIssues(issues) {
		p := podGroupProblem(c, g, critical)
		out.problems = append(out.problems, p)
		summaries = append(summaries, p.Title)
		if g.Kind != issueUnschedulable && g.Kind != issueNotScheduled {
			failing = true
		}
	}
	status := "warn"
	if failing {
		status = "fail"
	}
	out.check = CheckResult{Name: name, Status: status, Detail: strings.Join(summaries, "; ")}
	return out
}

// subscriptionState is the part of the operator Subscription diagnostics uses.
type subscriptionState struct {
	Exists       bool
	Source       string
	Channel      string
	State        string
	InstallPlan  string
	InstalledCSV string
	CurrentCSV   string
	Changed      time.Time // status.lastUpdated, else creationTimestamp
	Conditions   []struct {
		Type               string `json:"type"`
		Status             string `json:"status"`
		Reason             string `json:"reason"`
		Message            string `json:"message"`
		LastTransitionTime string `json:"lastTransitionTime"`
	}
}

func readSubscriptionState(c *Client) (subscriptionState, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName))
	if err != nil {
		if IsK8sError(err, http.StatusNotFound) {
			return subscriptionState{}, nil
		}
		return subscriptionState{}, err
	}
	var sub struct {
		Metadata struct {
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Source  string `json:"source"`
			Channel string `json:"channel"`
		} `json:"spec"`
		Status struct {
			State          string `json:"state"`
			LastUpdated    string `json:"lastUpdated"`
			InstalledCSV   string `json:"installedCSV"`
			CurrentCSV     string `json:"currentCSV"`
			InstallPlanRef *struct {
				Name string `json:"name"`
			} `json:"installPlanRef"`
			Conditions []struct {
				Type               string `json:"type"`
				Status             string `json:"status"`
				Reason             string `json:"reason"`
				Message            string `json:"message"`
				LastTransitionTime string `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &sub); err != nil {
		return subscriptionState{}, fmt.Errorf("parse subscription: %w", err)
	}
	s := subscriptionState{
		Exists: true, Source: sub.Spec.Source, Channel: sub.Spec.Channel, State: sub.Status.State,
		InstalledCSV: sub.Status.InstalledCSV, CurrentCSV: sub.Status.CurrentCSV,
	}
	s.Conditions = sub.Status.Conditions
	if sub.Status.InstallPlanRef != nil {
		s.InstallPlan = sub.Status.InstallPlanRef.Name
	}
	if t, ok := parseK8sTime(sub.Status.LastUpdated); ok {
		s.Changed = t
	} else if t, ok := parseK8sTime(sub.Metadata.CreationTimestamp); ok {
		s.Changed = t
	}
	return s, nil
}

// settled reports whether the Subscription has been in its state for at
// least operatorStuckAfter (true when the time is unknown).
func (s subscriptionState) settled(now time.Time) bool {
	return s.Changed.IsZero() || now.Sub(s.Changed) >= operatorStuckAfter
}

// recoveryGuidance is shared by the operator-level problems. These problems
// are guidance only: recovering a failed operator means deleting the CSV,
// InstallPlan and Subscription in the right order (OLM docs, "Opting into
// UnsafeFailForward upgrades": a failed CSV blocks upgrades until it is
// deleted). The Update, Refresh operator and Reinstall flows do that with a
// saved Subscription and restore on failure; a one-click fix here would not.
const recoveryGuidance = "Use Update on the Status page to move to a newer nightly (the usual fix for a broken build), or Refresh operator to reinstall the same build. Both delete and recreate the CSV, InstallPlan and Subscription and keep the DSC, DSCI and workloads."

func checkSubscriptionHealth(c *Client) checkOutput {
	const name = "Subscription health"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	now := time.Now()

	sub, err := readSubscriptionState(c)
	if err != nil {
		out.check = CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Could not read Subscription: %v", err)}
		return out
	}
	if !sub.Exists {
		csv, csvErr := getCSV(c)
		if csvErr == nil && csv.Name != "" && csv.Phase != "Not Found" {
			out.check = CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("No Subscription, but %s is installed", csv.Name)}
			out.problems = append(out.problems, Problem{
				ID:          "subscription-missing",
				Severity:    "warning",
				Title:       "The operator has no Subscription",
				Description: fmt.Sprintf("%s is installed, but the %s Subscription does not exist, so OLM will not update or repair the operator. This happens when an update or reinstall stopped part-way.", csv.Name, SubName),
				Fix:         "Use Update on the Status page to install a nightly; it recreates the Subscription. Reinstall Operator also works.",
			})
			return out
		}
		out.check.Detail = "No subscription (operator not installed yet)"
		out.problems = append(out.problems, Problem{
			ID:          "subscription-missing",
			Severity:    "info",
			Title:       "No operator Subscription found",
			Description: "The rhods-operator Subscription does not exist. This is normal on a fresh cluster before the first nightly install.",
			Fix:         "Use the Update panel on the Dashboard to install a nightly build. This will create the Subscription automatically.",
		})
		return out
	}

	// OLM reports resolution and unpack failures as Subscription conditions
	// (operator-framework/api subscription_types.go).
	failureConditions := map[string]struct {
		severity string
		terminal bool
	}{
		"ResolutionFailed":        {"critical", false},
		"BundleUnpackFailed":      {"critical", true},
		"InstallPlanFailed":       {"critical", true},
		"CatalogSourcesUnhealthy": {"warning", false},
		"InstallPlanMissing":      {"warning", false},
	}
	for _, cond := range sub.Conditions {
		fc, ok := failureConditions[cond.Type]
		if !ok || cond.Status != "True" {
			continue
		}
		if !fc.terminal {
			if t, ok := parseK8sTime(cond.LastTransitionTime); ok && now.Sub(t) < operatorStuckAfter {
				continue
			}
		}
		out.check = CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Subscription condition %s", cond.Type)}
		out.problems = append(out.problems, Problem{
			ID:           "subscription-" + strings.ToLower(cond.Type),
			Severity:     fc.severity,
			Title:        fmt.Sprintf("OLM reports %s for the operator Subscription", cond.Type),
			Description:  fmt.Sprintf("Subscription %s (source %s, channel %s) has condition %s=True.", SubName, sub.Source, sub.Channel, cond.Type),
			Evidence:     []string{fmt.Sprintf("%s (%s): %s", cond.Type, cond.Reason, truncate(cond.Message, 600))},
			Fix:          subscriptionConditionFix(cond.Type),
			TechnicalCmd: "oc get subscription " + SubName + " -n " + SubNS + " -o jsonpath='{.status.conditions}'",
		})
	}

	if p := channelHeadBehind(c, sub); p != nil {
		if out.check.Status == "pass" {
			out.check = CheckResult{Name: name, Status: "warn", Detail: p.Title}
		}
		out.problems = append(out.problems, *p)
	}

	age := formatDuration(now.Sub(sub.Changed))
	switch sub.State {
	case "AtLatestKnown":
		if out.check.Detail == "" {
			out.check.Detail = fmt.Sprintf("Subscription active (channel: %s, source: %s)", sub.Channel, sub.Source)
		}
	case "UpgradeAvailable":
		if out.check.Detail == "" {
			out.check.Detail = fmt.Sprintf("An upgrade is available (channel: %s, source: %s)", sub.Channel, sub.Source)
		}
	case "UpgradePending":
		if !sub.settled(now) {
			if out.check.Detail == "" {
				out.check = CheckResult{Name: name, Status: "pass", Detail: fmt.Sprintf("Upgrade in progress (InstallPlan %s, %s)", nonEmpty(sub.InstallPlan, "pending"), age)}
			}
			break
		}
		if out.check.Status == "pass" {
			out.check = CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Subscription UpgradePending for %s", age)}
		}
		out.problems = append(out.problems, Problem{
			ID:           "subscription-upgrade-pending",
			Severity:     "warning",
			Title:        fmt.Sprintf("The operator upgrade has been pending for %s", age),
			Description:  fmt.Sprintf("OLM created InstallPlan %s for %s but has not finished it. UpgradePending is normal for a few minutes during every update; after %s it usually means the InstallPlan or the new CSV is stuck.", nonEmpty(sub.InstallPlan, "(none)"), nonEmpty(sub.CurrentCSV, "the new version"), formatDuration(operatorStuckAfter)),
			Evidence:     []string{fmt.Sprintf("Subscription state UpgradePending since %s, InstallPlan %s, currentCSV %s", sub.Changed.UTC().Format(time.RFC3339), nonEmpty(sub.InstallPlan, "-"), nonEmpty(sub.CurrentCSV, "-"))},
			Fix:          "Check the Install plan and Operator installed results below for the cause. " + recoveryGuidance,
			TechnicalCmd: "oc get subscription " + SubName + " -n " + SubNS + " -o jsonpath='{.status}'",
		})
	case "Failed":
		out.check = CheckResult{Name: name, Status: "fail", Detail: "Subscription state Failed"}
		out.problems = append(out.problems, Problem{
			ID:           "subscription-failed",
			Severity:     "critical",
			Title:        "The operator Subscription is Failed",
			Description:  fmt.Sprintf("OLM marked the Subscription Failed because the InstallPlan or CSV for %s failed.", nonEmpty(sub.CurrentCSV, "the target version")),
			Fix:          recoveryGuidance,
			TechnicalCmd: "oc describe subscription " + SubName + " -n " + SubNS,
		})
	default: // "" or no status yet
		if !sub.settled(now) {
			if out.check.Detail == "" {
				out.check.Detail = fmt.Sprintf("OLM is resolving the Subscription (%s)", age)
			}
			break
		}
		if out.check.Status == "pass" {
			out.check = CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Subscription has no state after %s", age)}
		}
		out.problems = append(out.problems, Problem{
			ID:          "subscription-stuck",
			Severity:    "warning",
			Title:       fmt.Sprintf("OLM has not resolved the Subscription after %s", age),
			Description: fmt.Sprintf("The operator Subscription (source: %s, channel: %s) has no state. OLM normally resolves it within seconds; a missing channel or an unhealthy catalog keeps it here.", sub.Source, sub.Channel),
			Fix:         "Check the Catalog health result and that the channel exists in the catalog. " + recoveryGuidance,
			LearnMore: "**Channel mismatch:** The subscription channel may not exist in the nightly catalog. " +
				"Check available channels: `oc get packagemanifest rhods-operator -o jsonpath='{.status.channels[*].name}'`",
			TechnicalCmd: "oc get subscription " + SubName + " -n " + SubNS + " -o jsonpath='{.status}'",
		})
	}
	return out
}

func subscriptionConditionFix(condType string) string {
	fix := "Read the message above; it names the bundle, catalog or constraint that failed. " + recoveryGuidance
	if condType == "BundleUnpackFailed" {
		// OpenShift Operators guide, "Refreshing failing subscriptions".
		fix += " A failed bundle unpack is retried only after its unpack Job and ConfigMap in openshift-marketplace are deleted " +
			"(oc get job,configmap -n openshift-marketplace -o name | grep <bundle hash>); the tool does not delete them."
	}
	return fix
}

func checkCSVHealth(c *Client) checkOutput {
	const name = "Operator installed"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: "CSV present and Succeeded"}}

	csv, err := getCSV(c)
	if err != nil {
		out.check = CheckResult{Name: name, Status: "warn", Detail: "Could not query CSV"}
		return out
	}

	if csv.Phase == "Not Found" || csv.Name == "" {
		sub, subErr := readSubscriptionState(c)
		if subErr != nil || !sub.Exists {
			out.check = CheckResult{Name: name, Status: "pass", Detail: "No operator installed yet (normal for fresh clusters)"}
			return out
		}
		if !sub.settled(time.Now()) {
			out.check = CheckResult{Name: name, Status: "warn", Detail: "Waiting for OLM to install the operator"}
			return out
		}
		out.check = CheckResult{Name: name, Status: "fail", Detail: "No CSV found — operator not installed"}
		out.problems = append(out.problems, Problem{
			ID:          "csv-not-found",
			Severity:    "critical",
			Title:       "Operator not installed (no CSV found)",
			Description: fmt.Sprintf("A Subscription exists (source: %s, channel: %s, state: %q) but OLM has not created a ClusterServiceVersion for %s.", sub.Source, sub.Channel, sub.State, formatDuration(time.Since(sub.Changed))),
			Fix:         "Check the Subscription health and Install plan results for the cause. " + recoveryGuidance,
			LearnMore: "The operator CSV is created by OLM after the InstallPlan completes. If the CSV doesn't appear, check:\n\n" +
				"1. Is the Subscription pointing to the correct catalog?\n" +
				"2. Is the InstallPlan created and approved?\n" +
				"3. Are there dependency resolution errors? Check subscription conditions.",
			TechnicalCmd: "oc get csv -n " + SubNS,
		})
		return out
	}

	switch csv.Phase {
	case "Succeeded":
		out.check.Detail = fmt.Sprintf("%s (%s)", csv.Name, csv.Phase)
	case "Failed":
		evidence := []string{fmt.Sprintf("Operator: %s, Phase: Failed", csv.Name)}
		body, _, getErr := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, csv.Name))
		if getErr == nil {
			var full struct {
				Status struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"status"`
			}
			if json.Unmarshal(body, &full) == nil && (full.Status.Reason != "" || full.Status.Message != "") {
				evidence = append(evidence, fmt.Sprintf("Reason %s: %s", full.Status.Reason, truncate(full.Status.Message, 600)))
			}
		}
		out.check = CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Operator installation failed (%s)", csv.Name)}
		out.problems = append(out.problems, Problem{
			ID:           "operator-failed",
			Severity:     "critical",
			Title:        "The operator installation failed",
			Description:  fmt.Sprintf("The operator %s is in Failed state. OLM keeps re-checking a Failed CSV and reinstalls it if the cause goes away, but OLM will not upgrade away from a Failed CSV until it is deleted.", csv.Name),
			Evidence:     evidence,
			Fix:          recoveryGuidance,
			LearnMore:    "OLM documentation, \"Opting into UnsafeFailForward upgrades\": to recover from a failed CSV, the existing CSV is deleted so that a new InstallPlan can be generated.",
			TechnicalCmd: fmt.Sprintf("oc describe csv %s -n %s", csv.Name, SubNS),
		})
	case "Installing":
		out.check = CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Operator is being installed (%s)", csv.Name)}
		out.problems = append(out.problems, Problem{
			ID:          "operator-installing",
			Severity:    "info",
			Title:       "The operator is being installed",
			Description: fmt.Sprintf("The operator %s is currently being installed. This is normal after an update or reinstall.", csv.Name),
			Evidence:    []string{fmt.Sprintf("Operator: %s, Phase: Installing", csv.Name)},
			Fix:         "Wait a few minutes for the installation to complete, then check again.",
		})
	default:
		out.check = CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("CSV phase: %s (%s)", csv.Phase, csv.Name)}
	}

	return out
}

type installPlanItem struct {
	Metadata struct {
		Name              string `json:"name"`
		UID               string `json:"uid"`
		ResourceVersion   string `json:"resourceVersion"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Approved                   bool     `json:"approved"`
		Approval                   string   `json:"approval"`
		ClusterServiceVersionNames []string `json:"clusterServiceVersionNames"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

func listInstallPlans(c *Client) ([]installPlanItem, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []installPlanItem `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse install plans: %w", err)
	}
	return list.Items, nil
}

func installPlanFailureReason(ip installPlanItem) string {
	for _, cond := range ip.Status.Conditions {
		if cond.Message != "" && (cond.Status == "False" || cond.Reason == "InstallComponentFailed" || cond.Status == "True") {
			return cond.Message
		}
	}
	return ""
}

// installPlanDeletable decides whether a Failed InstallPlan may be deleted.
// Only phase Failed is terminal: "" is a plan OLM has not processed yet
// (InstallPlanPhaseNone) and the other phases are in flight. A plan the
// Subscription references is deleted only while the Subscription is
// UpgradePending: OLM then clears the reference and resolves again
// (operator-lifecycle-manager catalog/subscription/reconciler.go,
// InstallPlanNotFound; the OLM docs' recovery for a failed InstallPlan is
// to delete it). In any other state OLM only sets InstallPlanMissing, so
// deleting it would not trigger a new install.
func installPlanDeletable(ip installPlanItem, sub subscriptionState) (bool, string) {
	if ip.Status.Phase != "Failed" {
		return false, fmt.Sprintf("phase %q", ip.Status.Phase)
	}
	if sub.Exists && sub.InstallPlan == ip.Metadata.Name {
		if sub.State == "UpgradePending" {
			return true, "Failed; the Subscription waits on it, so OLM resolves again after it is deleted"
		}
		return false, fmt.Sprintf("Failed, but the Subscription (state %q) references it and OLM would not resolve again; use Update or Refresh operator", sub.State)
	}
	return true, "Failed and not used by the Subscription"
}

func checkInstallPlanHealth(c *Client) checkOutput {
	const name = "Install plan"
	missing := func() checkOutput {
		sub, subErr := readSubscriptionState(c)
		if subErr != nil {
			return checkOutput{check: CheckResult{Name: name, Status: "warn", Detail: "Could not verify subscription status"}}
		}
		if !sub.Exists {
			return checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: "No install plans (no subscription active)"}}
		}
		if !sub.settled(time.Now()) {
			return checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: "No install plan yet (OLM is resolving the Subscription)"}}
		}
		return checkOutput{
			check: CheckResult{Name: name, Status: "warn", Detail: "No install plans found but subscription exists"},
			problems: []Problem{{
				ID:           "installplan-missing",
				Severity:     "warning",
				Title:        "OLM hasn't created an install plan",
				Description:  "A subscription exists but no install plan has been created. OLM may be unable to resolve the subscription or the catalog may not be ready.",
				Evidence:     []string{fmt.Sprintf("No install plans found in namespace '%s'", SubNS)},
				Fix:          "Check the Catalog health and Subscription health results. " + recoveryGuidance,
				TechnicalCmd: fmt.Sprintf("oc get installplans -n %s", SubNS),
			}},
		}
	}

	plans, err := listInstallPlans(c)
	if err != nil {
		if IsK8sError(err, http.StatusNotFound) {
			return missing()
		}
		return checkOutput{check: CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Failed to check install plans: %v", err)}}
	}
	if len(plans) == 0 {
		return missing()
	}

	latest := plans[0]
	for _, ip := range plans[1:] {
		if ip.Metadata.CreationTimestamp > latest.Metadata.CreationTimestamp {
			latest = ip
		}
	}

	switch latest.Status.Phase {
	case "Complete":
		return checkOutput{check: CheckResult{Name: name, Status: "pass", Detail: fmt.Sprintf("Latest install plan completed (%s)", latest.Metadata.Name)}}
	case "Failed":
		reason := installPlanFailureReason(latest)
		sub, _ := readSubscriptionState(c)
		var deletable, affected, kept, evidence []string
		for _, ip := range plans {
			if ip.Status.Phase != "Failed" {
				continue
			}
			ok, why := installPlanDeletable(ip, sub)
			if ok {
				deletable = append(deletable, fmt.Sprintf("%s (%s)", ip.Metadata.Name, why))
				affected = append(affected, fmt.Sprintf("InstallPlan %s/%s", SubNS, ip.Metadata.Name))
			} else {
				kept = append(kept, fmt.Sprintf("%s: %s", ip.Metadata.Name, why))
			}
		}
		sort.Strings(deletable)
		sort.Strings(affected)
		evidence = append(evidence, fmt.Sprintf("Install plan: %s, Phase: Failed, CSVs: %s", latest.Metadata.Name, strings.Join(latest.Spec.ClusterServiceVersionNames, ", ")))
		if reason != "" {
			evidence = append(evidence, fmt.Sprintf("Reason: %s", truncate(reason, 600)))
		}
		for _, k := range kept {
			evidence = append(evidence, "Not deleted automatically: "+k)
		}
		p := Problem{
			ID:           "installplan-failed",
			Severity:     "critical",
			Title:        "The install plan failed",
			Description:  fmt.Sprintf("OLM could not execute install plan %s. %s", latest.Metadata.Name, truncate(reason, 300)),
			Evidence:     evidence,
			Fix:          "Update to a newer nightly or use Refresh operator. If the same build fails again, the build itself is broken.",
			LearnMore:    "OLM documentation, \"Opting into UnsafeFailForward upgrades\": to recover from a failed InstallPlan, the user deletes it and a new InstallPlan is generated.",
			TechnicalCmd: fmt.Sprintf("oc describe installplan %s -n %s", latest.Metadata.Name, SubNS),
		}
		if len(deletable) > 0 {
			p.Fix = "Delete the failed install plans so that OLM resolves the Subscription again and creates a new one. If the same build fails again, update to a newer nightly."
			p.AutoFixable = true
			p.AutoFixAction = "delete-stale-installplans"
			p.AffectedObjects = affected
			p.ConfirmMessage = fmt.Sprintf("This deletes these install plans in %s:\n- %s\n\nPlans in any other phase are kept, and each plan is checked again and deleted only if it is unchanged. "+
				"If the catalog still offers the same build, OLM's new plan may fail the same way.", SubNS, strings.Join(deletable, "\n- "))
		}
		return checkOutput{
			check:    CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Install plan failed: %s", latest.Metadata.Name)},
			problems: []Problem{p},
		}
	case "RequiresApproval":
		return checkOutput{
			check: CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan %s is waiting for approval", latest.Metadata.Name)},
			problems: []Problem{{
				ID:           "installplan-requires-approval",
				Severity:     "warning",
				Title:        "An install plan is waiting for manual approval",
				Description:  fmt.Sprintf("The Subscription uses Manual approval, so OLM waits until install plan %s (%s) is approved.", latest.Metadata.Name, strings.Join(latest.Spec.ClusterServiceVersionNames, ", ")),
				Fix:          "Approve the install plan in the OpenShift console (Operators > Installed Operators) if you want this version.",
				TechnicalCmd: fmt.Sprintf("oc patch installplan %s -n %s --type merge -p '{\"spec\":{\"approved\":true}}'", latest.Metadata.Name, SubNS),
			}},
		}
	case "Installing":
		return checkOutput{
			check: CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan is in progress (%s)", latest.Metadata.Name)},
			problems: []Problem{{
				ID:          "installplan-installing",
				Severity:    "info",
				Title:       "An install plan is in progress",
				Description: "OLM is currently executing an install plan. This is normal during an update or reinstall.",
				Evidence:    []string{fmt.Sprintf("Install plan: %s, Phase: Installing", latest.Metadata.Name)},
				Fix:         "Wait for the installation to complete. This typically takes 1-5 minutes.",
			}},
		}
	default:
		return checkOutput{
			check: CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan phase: %q (%s)", latest.Status.Phase, latest.Metadata.Name)},
		}
	}
}

func checkPullSecretHealth(c *Client) checkOutput {
	ps, err := getPullSecret(c)
	if err != nil {
		return checkOutput{
			check: CheckResult{Name: "Pull secret", Status: "fail", Detail: fmt.Sprintf("Failed to check pull secret: %v", err)},
		}
	}

	if !ps.Exists {
		return checkOutput{
			check: CheckResult{Name: "Pull secret", Status: "fail", Detail: "Pull secret is missing"},
			problems: []Problem{{
				ID:          "pull-secret-missing",
				Severity:    "critical",
				Title:       "Pull secret is not configured",
				Description: "The cluster needs registry credentials to pull nightly operator images from the private registry. Without these credentials, image pulls will fail.",
				Evidence:    []string{"Secret 'additional-pull-secret' not found in kube-system namespace"},
				Fix:         "Go to the Setup tab and configure the pull secret with your quay.io/rhoai credentials.",
				LearnMore:   "The pull secret provides authentication to pull nightly FBC images from quay.io/rhoai. Without it, CatalogSource pods will fail with ImagePullBackOff.",
			}},
		}
	}

	if !ps.Valid {
		return checkOutput{
			check: CheckResult{Name: "Pull secret", Status: "warn", Detail: fmt.Sprintf("Pull secret exists but: %s", ps.Detail)},
			problems: []Problem{{
				ID:          "pull-secret-invalid",
				Severity:    "warning",
				Title:       "Pull secret exists but may be misconfigured",
				Description: fmt.Sprintf("The pull secret was found but does not appear to have the correct format: %s", ps.Detail),
				Evidence:    []string{ps.Detail},
				Fix:         "Update the pull secret with valid quay.io/rhoai credentials in the Setup tab.",
			}},
		}
	}

	return checkOutput{
		check: CheckResult{Name: "Pull secret", Status: "pass", Detail: "Pull secret is configured and valid"},
	}
}

func checkImageMirror(c *Client) checkOutput {
	idms, err := getIDMS(c)
	if err != nil {
		return checkOutput{
			check: CheckResult{Name: "Image mirror", Status: "fail", Detail: fmt.Sprintf("Failed to check image mirror: %v", err)},
		}
	}

	if !idms.Exists {
		return checkOutput{
			check: CheckResult{Name: "Image mirror", Status: "fail", Detail: "Image mirror is not configured"},
			problems: []Problem{{
				ID:          "idms-missing",
				Severity:    "warning",
				Title:       "Image mirror is not configured",
				Description: "The image mirror redirects image pulls from the production registry (registry.redhat.io) to the nightly registry (quay.io/rhoai). Without it, the cluster will try to pull from the wrong location.",
				Evidence:    []string{fmt.Sprintf("No image mirror found for source '%s'", IDMSSource)},
				Fix:         "Configure the image mirror in the Setup tab. This is required for nightly builds to work.",
				LearnMore:   "The ImageDigestMirrorSet (IDMS) tells the cluster to redirect image pulls from registry.redhat.io/rhoai to quay.io/rhoai, so nightly images are pulled from the correct location.",
			}},
		}
	}

	return checkOutput{
		check: CheckResult{Name: "Image mirror", Status: "pass", Detail: fmt.Sprintf("Image mirror configured (%s)", idms.Name)},
	}
}

func checkNodeCapacity(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: "Node capacity", Status: "pass", Detail: "All nodes healthy"}}

	path := clusterPath("v1", "nodes", "")
	body, _, err := c.get(path)
	if err != nil {
		out.check = CheckResult{Name: "Node capacity", Status: "warn", Detail: "Could not query nodes"}
		return out
	}

	var nodeList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &nodeList); err != nil {
		out.check = CheckResult{Name: "Node capacity", Status: "warn", Detail: "Could not parse node list"}
		return out
	}

	if len(nodeList.Items) == 0 {
		out.check = CheckResult{Name: "Node capacity", Status: "warn", Detail: "No nodes found"}
		return out
	}

	totalNodes := len(nodeList.Items)
	var notReadyNodes []string
	for _, node := range nodeList.Items {
		nodeReady := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				nodeReady = true
				break
			}
		}
		if !nodeReady {
			notReadyNodes = append(notReadyNodes, node.Metadata.Name)
		}
	}

	if len(notReadyNodes) > 0 {
		out.check = CheckResult{Name: "Node capacity", Status: "warn", Detail: fmt.Sprintf("%d of %d nodes are not ready", len(notReadyNodes), totalNodes)}
		out.problems = append(out.problems, Problem{
			ID:           "nodes-not-ready",
			Severity:     "warning",
			Title:        fmt.Sprintf("%d cluster node(s) are not ready", len(notReadyNodes)),
			Description:  "Some cluster nodes are not in a Ready state. This can prevent pods from being scheduled and cause deployments to get stuck.",
			Evidence:     append([]string{fmt.Sprintf("Not-ready nodes: %s", strings.Join(notReadyNodes, ", "))}, fmt.Sprintf("Total nodes: %d", totalNodes)),
			Fix:          "Check the cluster infrastructure. Node issues are typically caused by resource exhaustion, network problems, or infrastructure failures.",
			TechnicalCmd: "oc get nodes",
		})
		return out
	}

	// Pods that cannot be scheduled are reported by the Operator pods and
	// RHOAI pods checks, with the scheduler's own reason.
	out.check.Detail = fmt.Sprintf("All %d nodes are ready", totalNodes)
	return out
}

// --- Auto-fix ---

// RunDiagnostics is the handler-facing entry point for cluster diagnostics.
func RunDiagnostics(c *Client) (*DiagnosticsResponse, error) {
	return DiagnoseCluster(c)
}

// removedFixes are fix IDs an older page may still send. They are refused
// with the reason instead of "Unknown fix action".
var removedFixes = map[string]string{
	"recreate-subscription": "This automatic fix was removed: deleting and recreating only the Subscription does not recover a failed operator, because OLM keeps the existing CSV. " + recoveryGuidance,
	"fix-maas-gateway-annotation": "This automatic fix was removed: no RHOAI 3.x operator reports the condition it was written for, and setting opendatahub.io/managed=false " +
		"on a Gateway stops the operator from managing it rather than fixing it. Nothing was changed.",
	"restart-operator": "This automatic fix was removed: diagnostics never offered it. Nothing was changed.",
}

// ApplyFix executes the automatic fix for a known problem. Every fix
// re-checks its precondition right before acting and reports what it
// actually changed; errorCode "nothing_to_do" means nothing needed changing.
func ApplyFix(c *Client, problemID string) (*types.OperationResponse, error) {
	switch problemID {
	case "delete-stale-webhooks":
		return applyFixDeleteStaleWebhooks(c)
	case "delete-stale-installplans":
		return applyFixDeleteStaleInstallPlans(c)
	case "assist-rollout":
		return AssistRollout(c)
	}
	if msg, ok := removedFixes[problemID]; ok {
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: "validation"}, nil
	}
	if target, ok := strings.CutPrefix(problemID, "assist-rollout:"); ok {
		ns, name, found := strings.Cut(target, "/")
		if !found {
			return &types.OperationResponse{Success: false, Message: "assist-rollout needs <namespace>/<deployment>", ErrorCode: "validation"}, nil
		}
		return AssistRolloutFor(c, ns, name)
	}
	if target, ok := strings.CutPrefix(problemID, "restore-rollout-strategy:"); ok {
		ns, name, found := strings.Cut(target, "/")
		if !found {
			return &types.OperationResponse{Success: false, Message: "restore-rollout-strategy needs <namespace>/<deployment>", ErrorCode: "validation"}, nil
		}
		return RestoreRolloutStrategy(c, ns, name)
	}
	if compName, ok := strings.CutPrefix(problemID, "disable-component:"); ok {
		return applyFixDisableComponent(c, compName)
	}
	return &types.OperationResponse{
		Success:   false,
		Message:   fmt.Sprintf("Unknown fix action: %s", problemID),
		ErrorCode: "validation",
	}, nil
}

func nothingToDo(msg string, logs []string) *types.OperationResponse {
	return &types.OperationResponse{Success: false, Message: "Nothing to do: " + msg, Logs: logs, ErrorCode: "nothing_to_do"}
}

func applyFixDeleteStaleInstallPlans(c *Client) (*types.OperationResponse, error) {
	plans, err := listInstallPlans(c)
	if err != nil {
		if IsK8sError(err, http.StatusNotFound) {
			return nothingToDo("there are no install plans. Nothing was deleted.", nil), nil
		}
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to list install plans: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	sub, err := readSubscriptionState(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed to read the Subscription: %v", err), ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	var logs, deleted, failed, changed []string
	for _, ip := range plans {
		if ok, why := installPlanDeletable(ip, sub); !ok {
			logs = append(logs, fmt.Sprintf("Kept install plan %s: %s", ip.Metadata.Name, why))
			continue
		}
		path := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ip.Metadata.Name)
		err := deleteExact(c, path, ip.Metadata.UID, ip.Metadata.ResourceVersion)
		switch {
		case err == nil:
			deleted = append(deleted, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Deleted install plan %s (phase Failed)", ip.Metadata.Name))
		case IsK8sError(err, http.StatusNotFound):
			logs = append(logs, fmt.Sprintf("Install plan %s was already gone", ip.Metadata.Name))
		case IsK8sError(err, http.StatusConflict):
			changed = append(changed, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Kept install plan %s: it changed after it was checked", ip.Metadata.Name))
		default:
			failed = append(failed, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Error: could not delete install plan %s: %v", ip.Metadata.Name, err))
		}
	}

	if len(deleted) > 0 {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "fix-delete-stale-installplans",
			Detail:    "deleted Failed install plans: " + strings.Join(deleted, ", "),
			Success:   len(failed) == 0,
		})
	}

	switch {
	case len(deleted) == 0 && len(failed) == 0 && len(changed) > 0:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Install plan(s) %s changed after they were checked, so nothing was deleted. Run diagnostics again.", strings.Join(changed, ", ")), Logs: logs, ErrorCode: "conflict"}, nil
	case len(deleted) == 0 && len(failed) == 0:
		return nothingToDo("no failed install plan can be deleted safely (see the log). Nothing was deleted.", logs), nil
	case len(failed) > 0 && len(deleted) == 0:
		return &types.OperationResponse{Success: false, Message: "Could not delete the failed install plans: " + strings.Join(failed, ", "), Logs: logs}, nil
	case len(failed) > 0:
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deleted %s; could not delete %s.", strings.Join(deleted, ", "), strings.Join(failed, ", ")),
			Logs:      logs,
			ErrorCode: "partial_failure",
		}, nil
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Deleted %d failed install plan(s): %s. OLM resolves the Subscription again if it was waiting on one of them.", len(deleted), strings.Join(deleted, ", ")),
		Logs:    logs,
	}, nil
}

// disableableComponents are DSC components the tool may set to Removed. All
// are top-level keys of spec.components in the v1 or v2 DataScienceCluster
// CRD (a key the CRD does not define would be pruned silently).
var disableableComponents = map[string]bool{
	"llamastackoperator": true,
	"feastoperator":      true,
	"trustyai":           true,
	"ray":                true,
	"kueue":              true,
	"sparkoperator":      true,
	"trainer":            true,
	"trainingoperator":   true,
}

func applyFixDisableComponent(c *Client, compName string) (*types.OperationResponse, error) {
	if compName == "" {
		return &types.OperationResponse{Success: false, Message: "Component name is required", ErrorCode: "validation"}, nil
	}
	if reason, refused := refusedComponents[compName]; refused {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("The tool does not remove %s. %s", compName, reason), ErrorCode: "validation"}, nil
	}
	if !disableableComponents[compName] {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Component %q cannot be disabled through this tool. Use the OpenShift Console to edit the DSC directly.", compName),
			ErrorCode: "validation",
		}, nil
	}

	dscPath, err := dataScienceClusterPath(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Cannot find the DataScienceCluster: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	// Re-check: the component must be in the DSC and not already Removed.
	body, _, err := c.get(dscPath)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot read the DataScienceCluster: %v", err), ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	state, present := componentManagementState(body, compName)
	if !present {
		return nothingToDo(fmt.Sprintf("the DataScienceCluster has no %s component. Nothing was changed.", compName), nil), nil
	}
	if state == "Removed" {
		return nothingToDo(fmt.Sprintf("%s is already Removed. Nothing was changed.", compName), nil), nil
	}
	if blockers := disableBlockers(c, compName); len(blockers) > 0 {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Not removing %s now, because it could leave the component stuck in deletion: %s. Nothing was changed.", compName, strings.Join(blockers, "; ")),
			Logs:      blockers,
			ErrorCode: "prerequisites",
		}, nil
	}

	patch := fmt.Sprintf(`{"spec":{"components":{%q:{"managementState":"Removed"}}}}`, compName)
	respBody, _, err := c.patch(dscPath, []byte(patch))
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to patch DataScienceCluster: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if after, _ := componentManagementState(respBody, compName); after != "Removed" {
		return &types.OperationResponse{
			Success: false,
			Message: fmt.Sprintf("The API accepted the patch, but %s is %q instead of Removed in the returned DataScienceCluster.", compName, after),
		}, nil
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "disable-component",
		Detail:    compName,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Set %s to Removed (was %s). The operator will clean up the component.", compName, nonEmpty(state, "unset")),
	}, nil
}

func componentManagementState(dscJSON []byte, compName string) (string, bool) {
	var dsc struct {
		Spec struct {
			Components map[string]struct {
				ManagementState string `json:"managementState"`
			} `json:"components"`
		} `json:"spec"`
	}
	if json.Unmarshal(dscJSON, &dsc) != nil {
		return "", false
	}
	comp, ok := dsc.Spec.Components[compName]
	return comp.ManagementState, ok
}

// dataScienceClusterPath returns the API path of the cluster's
// DataScienceCluster (the same one the Components page shows), using the v2
// API when it is served and v1 otherwise.
func dataScienceClusterPath(c *Client) (string, error) {
	for _, version := range []string{"v2", "v1"} {
		listPath := "/apis/datasciencecluster.opendatahub.io/" + version + "/datascienceclusters"
		body, _, err := c.get(listPath)
		if IsK8sError(err, 404) {
			continue
		}
		if err != nil {
			return "", err
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return "", fmt.Errorf("parse DataScienceCluster list: %w", err)
		}
		if len(list.Items) == 0 || list.Items[0].Metadata.Name == "" {
			return "", fmt.Errorf("no DataScienceCluster exists")
		}
		return listPath + "/" + list.Items[0].Metadata.Name, nil
	}
	return "", fmt.Errorf("the DataScienceCluster API is not installed")
}
