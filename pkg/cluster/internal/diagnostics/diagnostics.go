package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/node"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/report"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// NewAckBinding binds statement to the object uid at resourceVersion.
func NewAckBinding(uid, resourceVersion, statement string) *report.AckBinding {
	sum := sha256.Sum256([]byte(statement))
	return &report.AckBinding{UID: uid, ResourceVersion: resourceVersion, Hash: hex.EncodeToString(sum[:])}
}

type CheckOutput struct {
	Problems []report.Problem
	Check    report.CheckResult
}

type diagnosticCheck struct {
	Name string
	fn   func(*kube.Client) CheckOutput
}

// Checks run independently of each other; results are reported in
// this order.
var Checks = []diagnosticCheck{
	{"Catalog health", checkCatalogHealth},
	{"Operator pods", CheckOperatorPods},
	{"Subscription health", checkSubscriptionHealth},
	{"Operator installed", checkCSVHealth},
	{"Install plan", CheckInstallPlanHealth},
	{"Pull secret", CheckPullSecretHealth},
	{"Image mirror", checkImageMirror},
	{"Stale webhooks", CheckStaleWebhooks},
	{conversionCheckName, CheckConversionFailures},
	{"Node capacity", checkNodeCapacity},
	{"DataScienceCluster", CheckDataScienceCluster},
	{"RHOAI pods", CheckRHOAIPods},
	{"Certificates", CheckCertificates},
	{"Platform modules", CheckPlatformModules},
	{components.ApplyFailuresCheckName, CheckApplyFailures},
	{components.LeftoversCheckName, CheckLeftovers},
	{"Operator-managed config", CheckManagedConfig},
}

// GitOpsChecks are Checks without "Pull secret", which
// reads a Secret and checks it with Quay: the gitops profile does neither
// (docs/GITOPS.md, section 11). It adds one check of its own: that the
// ServiceAccount cannot read Secrets (CheckServiceAccountSecretAccess). Listed again rather than filtered, so the
// gitops profile does not reference that check at all
// (gitops_offline_test.go); a test keeps the two lists in step.
var GitOpsChecks = []diagnosticCheck{
	{"Catalog health", checkCatalogHealth},
	{"Operator pods", CheckOperatorPods},
	{"Subscription health", checkSubscriptionHealth},
	{"Operator installed", checkCSVHealth},
	{"Install plan", CheckInstallPlanHealth},
	{"Image mirror", checkImageMirror},
	{"Stale webhooks", CheckStaleWebhooks},
	{conversionCheckName, CheckConversionFailures},
	{"Node capacity", checkNodeCapacity},
	{"DataScienceCluster", CheckDataScienceCluster},
	{"RHOAI pods", CheckRHOAIPods},
	{"Certificates", CheckCertificates},
	{"Platform modules", CheckPlatformModules},
	{components.ApplyFailuresCheckName, CheckApplyFailures},
	{components.LeftoversCheckName, CheckLeftoversGitOps},
	{"Operator-managed config", CheckManagedConfig},
	{SecretAccessCheckName, CheckServiceAccountSecretAccess},
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
func DiagnoseCluster(c *kube.Client) (*report.DiagnosticsResponse, error) {
	return diagnose(c, Checks)
}

// DiagnoseClusterGitOps is DiagnoseCluster in the gitops profile:
// GitOpsChecks, which read no Secret and contact no registry.
func DiagnoseClusterGitOps(c *kube.Client) (*report.DiagnosticsResponse, error) {
	return diagnose(c, GitOpsChecks)
}

func diagnose(c *kube.Client, checks []diagnosticCheck) (*report.DiagnosticsResponse, error) {
	outputs := make([]CheckOutput, len(checks))
	sem := make(chan struct{}, diagnosticsParallelism)
	var wg sync.WaitGroup
	for i, chk := range checks {
		wg.Add(1)
		go func(i int, chk diagnosticCheck) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outputs[i] = runCheck(c, chk)
		}(i, chk)
	}
	wg.Wait()

	resp := &report.DiagnosticsResponse{Problems: []report.Problem{}, Checks: []report.CheckResult{}}
	seen := make(map[string]int)
	for _, out := range outputs {
		resp.Checks = append(resp.Checks, out.Check)
		for _, p := range out.Problems {
			i, dup := seen[p.ID]
			switch {
			case !dup:
				seen[p.ID] = len(resp.Problems)
				resp.Problems = append(resp.Problems, p)
			case p.Mergeable && resp.Problems[i].Mergeable:
				MergeProblem(&resp.Problems[i], p)
			}
		}
	}
	resp.Problems = LinkProblems(resp.Problems)

	// Critical first, then warning, then info; check order within a severity.
	severityOrder := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(resp.Problems, func(i, j int) bool {
		return severityOrder[resp.Problems[i].Severity] < severityOrder[resp.Problems[j].Severity]
	})
	return resp, nil
}

func runCheck(c *kube.Client, chk diagnosticCheck) CheckOutput {
	ctx, cancel := context.WithTimeout(c.Context(), diagnosticsCheckTimeout)
	defer cancel()
	done := make(chan CheckOutput, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- CheckOutput{Check: report.CheckResult{Name: chk.Name, Status: "warn", Detail: fmt.Sprintf("Check failed: %v", r)}}
			}
		}()
		done <- chk.fn(c.WithContext(ctx))
	}()
	start := time.Now()
	select {
	case out := <-done:
		out.Check.Name = chk.Name
		slog.Debug("diagnostics check finished", "check", chk.Name, "duration", time.Since(start))
		return out
	case <-ctx.Done():
		detail := fmt.Sprintf("Check did not finish within %s", diagnosticsCheckTimeout)
		if errors.Is(c.Context().Err(), context.Canceled) {
			detail = "Check cancelled"
		}
		return CheckOutput{Check: report.CheckResult{Name: chk.Name, Status: "warn", Detail: detail}}
	}
}

// --- Health check functions ---

func checkCatalogHealth(c *kube.Client) CheckOutput {
	cs, err := rhoai.GetCatalogSource(c)
	if err != nil {
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "fail", Detail: fmt.Sprintf("Failed to check catalog: %v", err)},
			Problems: []report.Problem{{
				ID:          "catalog-check-failed",
				Severity:    "warning",
				Title:       "Could not check CatalogSource health",
				Description: fmt.Sprintf("The diagnostics check could not read the CatalogSource: %v", err),
				Fix:         "Check cluster connectivity and RBAC permissions.",
			}},
		}
	}

	if !cs.Exists {
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "pass", Detail: "No nightly catalog yet (normal for fresh clusters)"},
			Problems: []report.Problem{{
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
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "pass", Detail: fmt.Sprintf("Nightly catalog is healthy (%s)", cs.Image)},
		}
	case "TRANSIENT_FAILURE":
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "fail", Detail: fmt.Sprintf("CatalogSource in TRANSIENT_FAILURE (image: %s)", cs.Image)},
			Problems: []report.Problem{{
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
				TechnicalCmd: "oc get catalogsource " + rhoai.CatalogName + " -n " + rhoai.CatalogNS + " -o jsonpath='{.status.connectionState}'",
			}},
		}
	case "CONNECTING":
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "warn", Detail: "CatalogSource is connecting..."},
			Problems: []report.Problem{{
				ID:          "catalog-connecting",
				Severity:    "info",
				Title:       "CatalogSource is connecting",
				Description: "The CatalogSource is currently connecting to the registry. This is normal during initial setup and should resolve within 1-2 minutes.",
				Fix:         "Wait a minute and re-scan. If the state persists, check the pull secret and IDMS configuration.",
			}},
		}
	default:
		return CheckOutput{
			Check: report.CheckResult{Name: "Catalog health", Status: "warn", Detail: fmt.Sprintf("CatalogSource state: %s", cs.State)},
		}
	}
}

// RHOAIPodNamespaces are the namespaces whose pods the "RHOAI pods" check
// scans, besides namespaces labelled platform.opendatahub.io/part-of (which
// the operator creates for platform modules): the applications and
// monitoring namespaces the DSCInitialization names. The operator namespace
// is covered by the "Operator pods" check. User workbench namespaces such
// as rhods-notebooks are left out on purpose. No DSCInitialization gives
// none and no error (RHOAI has none yet); a read error is returned, so the
// check cannot pass without them.
func RHOAIPodNamespaces(c *kube.Client) ([]string, error) {
	ns, err := rhoai.DiscoverNamespaces(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{ns.Applications}
	if ns.Monitoring != "" && ns.Monitoring != ns.Applications {
		out = append(out, ns.Monitoring)
	}
	return out, nil
}

func CheckOperatorPods(c *kube.Client) CheckOutput {
	const name = "Operator pods"
	issues, podCount, scanned, errs := components.ScanPods(c, []string{rhoai.SubNS}, time.Now())
	if len(errs) > 0 {
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "warn", Detail: "Could not list operator pods: " + strings.Join(errs, "; ")}}
	}
	if len(scanned) == 0 {
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: "No operator namespace (operator not installed yet)"}}
	}
	if podCount == 0 {
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: "No operator pods (operator not installed yet)"}}
	}
	return podIssuesOutput(c, name, issues, podCount, scanned, true)
}

func CheckRHOAIPods(c *kube.Client) CheckOutput {
	const name = "RHOAI pods"
	namespaces, rhoaiNSErr := RHOAIPodNamespaces(c)
	// Platform module namespaces (for example opendatahub-ogx-system).
	// A failed lookup is reported: the module namespaces could hold
	// unhealthy pods, so the check cannot pass without them.
	var nsErr error
	body, _, err := c.Do(http.MethodGet, rhoai.ClusterPath("v1", "namespaces", ""), "", nil,
		url.Values{"labelSelector": {"platform.opendatahub.io/part-of"}})
	if err != nil {
		nsErr = err
	} else {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			nsErr = fmt.Errorf("parse namespace list: %w", err)
		}
		for _, item := range list.Items {
			if item.Metadata.Name != rhoai.SubNS && !slices.Contains(namespaces, item.Metadata.Name) {
				namespaces = append(namespaces, item.Metadata.Name)
			}
		}
	}

	issues, podCount, scanned, errs := components.ScanPods(c, namespaces, time.Now())
	out := podIssuesOutput(c, name, issues, podCount, scanned, false)
	if len(errs) > 0 {
		if out.Check.Status == "pass" {
			out.Check.Status = "warn"
		}
		out.Check.Detail += "; could not list pods in " + strings.Join(errs, "; ")
	}
	if nsErr != nil {
		if out.Check.Status == "pass" {
			out.Check.Status = "warn"
		}
		out.Check.Detail += fmt.Sprintf("; could not verify the platform module namespaces (labelled platform.opendatahub.io/part-of): %v", nsErr)
	}
	if rhoaiNSErr != nil {
		if out.Check.Status == "pass" {
			out.Check.Status = "warn"
		}
		out.Check.Detail += fmt.Sprintf("; could not tell the applications and monitoring namespaces, so their pods were not checked: %v", rhoaiNSErr)
	}
	return out
}

func podIssuesOutput(c *kube.Client, name string, issues []components.PodIssue, podCount int, scanned []string, critical bool) CheckOutput {
	if len(issues) == 0 {
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass",
			Detail: fmt.Sprintf("%s healthy in %s", kube.CountNoun(podCount, "pod", "pods"), strings.Join(scanned, ", "))}}
	}
	out := CheckOutput{}
	var summaries []string
	failing := false
	for _, g := range components.GroupPodIssues(issues) {
		p := components.PodGroupProblem(c, g, critical)
		out.Problems = append(out.Problems, p)
		summaries = append(summaries, p.Title)
		if g.Kind != components.IssueUnschedulable && g.Kind != components.IssueNotScheduled {
			failing = true
		}
	}
	status := "warn"
	if failing {
		status = "fail"
	}
	out.Check = report.CheckResult{Name: name, Status: status, Detail: strings.Join(summaries, "; ")}
	return out
}

// SubscriptionState is the part of the operator Subscription diagnostics uses.
type SubscriptionState struct {
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

func readSubscriptionState(c *kube.Client) (SubscriptionState, error) {
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
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName), &sub)
	if err != nil {
		if kube.IsDecodeError(err) {
			return SubscriptionState{}, fmt.Errorf("parse subscription: %w", err)
		}
		return SubscriptionState{}, err
	}
	if !found {
		return SubscriptionState{}, nil
	}
	s := SubscriptionState{
		Exists: true, Source: sub.Spec.Source, Channel: sub.Spec.Channel, State: sub.Status.State,
		InstalledCSV: sub.Status.InstalledCSV, CurrentCSV: sub.Status.CurrentCSV,
	}
	s.Conditions = sub.Status.Conditions
	if sub.Status.InstallPlanRef != nil {
		s.InstallPlan = sub.Status.InstallPlanRef.Name
	}
	if t, ok := components.ParseK8sTime(sub.Status.LastUpdated); ok {
		s.Changed = t
	} else if t, ok := components.ParseK8sTime(sub.Metadata.CreationTimestamp); ok {
		s.Changed = t
	}
	return s, nil
}

// settled reports whether the Subscription has been in its state for at
// least operatorStuckAfter (true when the time is unknown).
func (s SubscriptionState) settled(now time.Time) bool {
	return s.Changed.IsZero() || now.Sub(s.Changed) >= operatorStuckAfter
}

// recoveryGuidance is shared by the operator-level problems. These problems
// are guidance only: recovering a failed operator means deleting the CSV,
// InstallPlan and Subscription in the right order (OLM docs, "Opting into
// UnsafeFailForward upgrades": a failed CSV blocks upgrades until it is
// deleted). The Update, Refresh operator and Reinstall flows do that with a
// saved Subscription and restore on failure; a one-click fix here would not.
const recoveryGuidance = "Use Update on the Status page to move to a newer nightly (the usual fix for a broken build), or Re-deploy operator to install the same build again. Both delete and recreate the CSV, InstallPlan and Subscription and keep the DSC, DSCI and workloads."

func checkSubscriptionHealth(c *kube.Client) CheckOutput {
	const name = "Subscription health"
	out := CheckOutput{Check: report.CheckResult{Name: name, Status: "pass"}}
	now := time.Now()

	sub, err := readSubscriptionState(c)
	if err != nil {
		out.Check = report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Could not read Subscription: %v", err)}
		return out
	}
	if !sub.Exists {
		csv, csvErr := rhoai.GetCSV(c)
		if csvErr == nil && csv.Name != "" && csv.Phase != "Not Found" {
			out.Check = report.CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("No Subscription, but %s is installed", csv.Name)}
			out.Problems = append(out.Problems, report.Problem{
				ID:          "subscription-missing",
				Severity:    "warning",
				Title:       "The operator has no Subscription",
				Description: fmt.Sprintf("%s is installed, but the %s Subscription does not exist, so OLM will not update or repair the operator. This happens when an update or an install of a different build stopped part-way.", csv.Name, rhoai.SubName),
				Fix:         "Use Update on the Status page to install a nightly; it recreates the Subscription. Install a different build (Recovery on the Status page) also works.",
			})
			return out
		}
		out.Check.Detail = "No subscription (operator not installed yet)"
		out.Problems = append(out.Problems, report.Problem{
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
			if t, ok := components.ParseK8sTime(cond.LastTransitionTime); ok && now.Sub(t) < operatorStuckAfter {
				continue
			}
		}
		out.Check = report.CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Subscription condition %s", cond.Type)}
		out.Problems = append(out.Problems, report.Problem{
			ID:           "subscription-" + strings.ToLower(cond.Type),
			Severity:     fc.severity,
			Title:        fmt.Sprintf("OLM reports %s for the operator Subscription", cond.Type),
			Description:  fmt.Sprintf("Subscription %s (source %s, channel %s) has condition %s=True.", rhoai.SubName, sub.Source, sub.Channel, cond.Type),
			Evidence:     []string{fmt.Sprintf("%s (%s): %s", cond.Type, cond.Reason, kube.TruncateText(cond.Message, 600))},
			Fix:          subscriptionConditionFix(cond.Type),
			TechnicalCmd: "oc get subscription " + rhoai.SubName + " -n " + rhoai.SubNS + " -o jsonpath='{.status.conditions}'",
		})
	}

	if p := ChannelHeadBehind(c, sub); p != nil {
		if out.Check.Status == "pass" {
			out.Check = report.CheckResult{Name: name, Status: "warn", Detail: p.Title}
		}
		out.Problems = append(out.Problems, *p)
	}

	age := components.FormatDuration(now.Sub(sub.Changed))
	switch sub.State {
	case "AtLatestKnown":
		if out.Check.Detail == "" {
			out.Check.Detail = fmt.Sprintf("Subscription active (channel: %s, source: %s)", sub.Channel, sub.Source)
		}
	case "UpgradeAvailable":
		if out.Check.Detail == "" {
			out.Check.Detail = fmt.Sprintf("An upgrade is available (channel: %s, source: %s)", sub.Channel, sub.Source)
		}
	case "UpgradePending":
		if !sub.settled(now) {
			if out.Check.Detail == "" {
				out.Check = report.CheckResult{Name: name, Status: "pass", Detail: fmt.Sprintf("Upgrade in progress (InstallPlan %s, %s)", kube.NonEmpty(sub.InstallPlan, "pending"), age)}
			}
			break
		}
		if out.Check.Status == "pass" {
			out.Check = report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Subscription UpgradePending for %s", age)}
		}
		out.Problems = append(out.Problems, report.Problem{
			ID:           "subscription-upgrade-pending",
			Severity:     "warning",
			Title:        fmt.Sprintf("The operator upgrade has been pending for %s", age),
			Description:  fmt.Sprintf("OLM created InstallPlan %s for %s but has not finished it. UpgradePending is normal for a few minutes during every update; after %s it usually means the InstallPlan or the new CSV is stuck.", kube.NonEmpty(sub.InstallPlan, "(none)"), kube.NonEmpty(sub.CurrentCSV, "the new version"), components.FormatDuration(operatorStuckAfter)),
			Evidence:     []string{fmt.Sprintf("Subscription state UpgradePending since %s, InstallPlan %s, currentCSV %s", sub.Changed.UTC().Format(time.RFC3339), kube.NonEmpty(sub.InstallPlan, "-"), kube.NonEmpty(sub.CurrentCSV, "-"))},
			Fix:          "Check the Install plan and Operator installed results below for the cause. " + recoveryGuidance,
			TechnicalCmd: "oc get subscription " + rhoai.SubName + " -n " + rhoai.SubNS + " -o jsonpath='{.status}'",
		})
	case "Failed":
		out.Check = report.CheckResult{Name: name, Status: "fail", Detail: "Subscription state Failed"}
		out.Problems = append(out.Problems, report.Problem{
			ID:           "subscription-failed",
			Severity:     "critical",
			Title:        "The operator Subscription is Failed",
			Description:  fmt.Sprintf("OLM marked the Subscription Failed because the InstallPlan or CSV for %s failed.", kube.NonEmpty(sub.CurrentCSV, "the target version")),
			Fix:          recoveryGuidance,
			TechnicalCmd: "oc describe subscription " + rhoai.SubName + " -n " + rhoai.SubNS,
		})
	default: // "" or no status yet
		if !sub.settled(now) {
			if out.Check.Detail == "" {
				out.Check.Detail = fmt.Sprintf("OLM is resolving the Subscription (%s)", age)
			}
			break
		}
		if out.Check.Status == "pass" {
			out.Check = report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Subscription has no state after %s", age)}
		}
		out.Problems = append(out.Problems, report.Problem{
			ID:          "subscription-stuck",
			Severity:    "warning",
			Title:       fmt.Sprintf("OLM has not resolved the Subscription after %s", age),
			Description: fmt.Sprintf("The operator Subscription (source: %s, channel: %s) has no state. OLM normally resolves it within seconds; a missing channel or an unhealthy catalog keeps it here.", sub.Source, sub.Channel),
			Fix:         "Check the Catalog health result and that the channel exists in the catalog. " + recoveryGuidance,
			LearnMore: "**Channel mismatch:** The subscription channel may not exist in the nightly catalog. " +
				"Check available channels: `oc get packagemanifest rhods-operator -o jsonpath='{.status.channels[*].name}'`",
			TechnicalCmd: "oc get subscription " + rhoai.SubName + " -n " + rhoai.SubNS + " -o jsonpath='{.status}'",
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

func checkCSVHealth(c *kube.Client) CheckOutput {
	const name = "Operator installed"
	out := CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: "CSV present and Succeeded"}}

	csv, err := rhoai.GetCSV(c)
	if err != nil {
		out.Check = report.CheckResult{Name: name, Status: "warn", Detail: "Could not query CSV"}
		return out
	}

	if csv.Phase == "Not Found" || csv.Name == "" {
		sub, subErr := readSubscriptionState(c)
		if subErr != nil || !sub.Exists {
			out.Check = report.CheckResult{Name: name, Status: "pass", Detail: "No operator installed yet (normal for fresh clusters)"}
			return out
		}
		if !sub.settled(time.Now()) {
			out.Check = report.CheckResult{Name: name, Status: "warn", Detail: "Waiting for OLM to install the operator"}
			return out
		}
		out.Check = report.CheckResult{Name: name, Status: "fail", Detail: "No CSV found — operator not installed"}
		out.Problems = append(out.Problems, report.Problem{
			ID:          "csv-not-found",
			Severity:    "critical",
			Title:       "Operator not installed (no CSV found)",
			Description: fmt.Sprintf("A Subscription exists (source: %s, channel: %s, state: %q) but OLM has not created a ClusterServiceVersion for %s.", sub.Source, sub.Channel, sub.State, components.FormatDuration(time.Since(sub.Changed))),
			Fix:         "Check the Subscription health and Install plan results for the cause. " + recoveryGuidance,
			LearnMore: "The operator CSV is created by OLM after the InstallPlan completes. If the CSV doesn't appear, check:\n\n" +
				"1. Is the Subscription pointing to the correct catalog?\n" +
				"2. Is the InstallPlan created and approved?\n" +
				"3. Are there dependency resolution errors? Check subscription conditions.",
			TechnicalCmd: "oc get csv -n " + rhoai.SubNS,
		})
		return out
	}

	switch csv.Phase {
	case "Succeeded":
		out.Check.Detail = fmt.Sprintf("%s (%s)", csv.Name, csv.Phase)
	case "Failed":
		evidence := []string{fmt.Sprintf("Operator: %s, Phase: Failed", csv.Name)}
		body, _, getErr := c.Get(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, csv.Name))
		if getErr == nil {
			var full struct {
				Status struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"status"`
			}
			if json.Unmarshal(body, &full) == nil && (full.Status.Reason != "" || full.Status.Message != "") {
				evidence = append(evidence, fmt.Sprintf("Reason %s: %s", full.Status.Reason, kube.TruncateText(full.Status.Message, 600)))
			}
		}
		out.Check = report.CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Operator installation failed (%s)", csv.Name)}
		out.Problems = append(out.Problems, report.Problem{
			ID:           "operator-failed",
			Severity:     "critical",
			Title:        "The operator installation failed",
			Description:  fmt.Sprintf("The operator %s is in Failed state. OLM keeps re-checking a Failed CSV and reinstalls it if the cause goes away, but OLM will not upgrade away from a Failed CSV until it is deleted.", csv.Name),
			Evidence:     evidence,
			Fix:          recoveryGuidance,
			LearnMore:    "OLM documentation, \"Opting into UnsafeFailForward upgrades\": to recover from a failed CSV, the existing CSV is deleted so that a new InstallPlan can be generated.",
			TechnicalCmd: fmt.Sprintf("oc describe csv %s -n %s", csv.Name, rhoai.SubNS),
		})
	case "Installing":
		out.Check = report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Operator is being installed (%s)", csv.Name)}
		out.Problems = append(out.Problems, report.Problem{
			ID:          "operator-installing",
			Severity:    "info",
			Title:       "The operator is being installed",
			Description: fmt.Sprintf("The operator %s is currently being installed. This is normal after an update or an install of a different build.", csv.Name),
			Evidence:    []string{fmt.Sprintf("Operator: %s, Phase: Installing", csv.Name)},
			Fix:         "Wait a few minutes for the installation to complete, then check again.",
		})
	default:
		out.Check = report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("CSV phase: %s (%s)", csv.Phase, csv.Name)}
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

func listInstallPlans(c *kube.Client) ([]installPlanItem, error) {
	var items []installPlanItem
	if err := c.ListInto(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, ""), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse install plans: %w", err)
		}
		return nil, err
	}
	return items, nil
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
func installPlanDeletable(ip installPlanItem, sub SubscriptionState) (bool, string) {
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

func CheckInstallPlanHealth(c *kube.Client) CheckOutput {
	const name = "Install plan"
	missing := func() CheckOutput {
		sub, subErr := readSubscriptionState(c)
		if subErr != nil {
			return CheckOutput{Check: report.CheckResult{Name: name, Status: "warn", Detail: "Could not verify subscription status"}}
		}
		if !sub.Exists {
			return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: "No install plans (no subscription active)"}}
		}
		if !sub.settled(time.Now()) {
			return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: "No install plan yet (OLM is resolving the Subscription)"}}
		}
		return CheckOutput{
			Check: report.CheckResult{Name: name, Status: "warn", Detail: "No install plans found but subscription exists"},
			Problems: []report.Problem{{
				ID:           "installplan-missing",
				Severity:     "warning",
				Title:        "OLM hasn't created an install plan",
				Description:  "A subscription exists but no install plan has been created. OLM may be unable to resolve the subscription or the catalog may not be ready.",
				Evidence:     []string{fmt.Sprintf("No install plans found in namespace '%s'", rhoai.SubNS)},
				Fix:          "Check the Catalog health and Subscription health results. " + recoveryGuidance,
				TechnicalCmd: fmt.Sprintf("oc get installplans -n %s", rhoai.SubNS),
			}},
		}
	}

	plans, err := listInstallPlans(c)
	if err != nil {
		if kube.IsK8sNotFound(err) {
			return missing()
		}
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Failed to check install plans: %v", err)}}
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
		return CheckOutput{Check: report.CheckResult{Name: name, Status: "pass", Detail: fmt.Sprintf("Latest install plan completed (%s)", latest.Metadata.Name)}}
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
				affected = append(affected, fmt.Sprintf("InstallPlan %s/%s", rhoai.SubNS, ip.Metadata.Name))
			} else {
				kept = append(kept, fmt.Sprintf("%s: %s", ip.Metadata.Name, why))
			}
		}
		sort.Strings(deletable)
		sort.Strings(affected)
		evidence = append(evidence, fmt.Sprintf("Install plan: %s, Phase: Failed, CSVs: %s", latest.Metadata.Name, strings.Join(latest.Spec.ClusterServiceVersionNames, ", ")))
		if reason != "" {
			evidence = append(evidence, fmt.Sprintf("Reason: %s", kube.TruncateText(reason, 600)))
		}
		for _, k := range kept {
			evidence = append(evidence, "Not deleted automatically: "+k)
		}
		p := report.Problem{
			ID:           "installplan-failed",
			Severity:     "critical",
			Title:        "The install plan failed",
			Description:  fmt.Sprintf("OLM could not execute install plan %s. %s", latest.Metadata.Name, kube.TruncateText(reason, 300)),
			Evidence:     evidence,
			Fix:          "Update to a newer nightly or use Refresh operator. If the same build fails again, the build itself is broken.",
			LearnMore:    "OLM documentation, \"Opting into UnsafeFailForward upgrades\": to recover from a failed InstallPlan, the user deletes it and a new InstallPlan is generated.",
			TechnicalCmd: fmt.Sprintf("oc describe installplan %s -n %s", latest.Metadata.Name, rhoai.SubNS),
		}
		if len(deletable) > 0 {
			p.Fix = "Delete the failed install plans so that OLM resolves the Subscription again and creates a new one. If the same build fails again, update to a newer nightly."
			p.AutoFixable = true
			p.AutoFixAction = "delete-stale-installplans"
			p.AffectedObjects = affected
			p.ConfirmMessage = fmt.Sprintf("This deletes these install plans in %s:\n- %s\n\nPlans in any other phase are kept, and each plan is checked again and deleted only if it is unchanged. "+
				"If the catalog still offers the same build, OLM's new plan may fail the same way.", rhoai.SubNS, strings.Join(deletable, "\n- "))
		}
		return CheckOutput{
			Check:    report.CheckResult{Name: name, Status: "fail", Detail: fmt.Sprintf("Install plan failed: %s", latest.Metadata.Name)},
			Problems: []report.Problem{p},
		}
	case "RequiresApproval":
		return CheckOutput{
			Check: report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan %s is waiting for approval", latest.Metadata.Name)},
			Problems: []report.Problem{{
				ID:           "installplan-requires-approval",
				Severity:     "warning",
				Title:        "An install plan is waiting for manual approval",
				Description:  fmt.Sprintf("The Subscription uses Manual approval, so OLM waits until install plan %s (%s) is approved.", latest.Metadata.Name, strings.Join(latest.Spec.ClusterServiceVersionNames, ", ")),
				Fix:          "Approve the install plan in the OpenShift console (Operators > Installed Operators) if you want this version.",
				TechnicalCmd: fmt.Sprintf("oc patch installplan %s -n %s --type merge -p '{\"spec\":{\"approved\":true}}'", latest.Metadata.Name, rhoai.SubNS),
			}},
		}
	case "Installing":
		return CheckOutput{
			Check: report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan is in progress (%s)", latest.Metadata.Name)},
			Problems: []report.Problem{{
				ID:          "installplan-installing",
				Severity:    "info",
				Title:       "An install plan is in progress",
				Description: "OLM is currently executing an install plan. This is normal during an update or an install of a different build.",
				Evidence:    []string{fmt.Sprintf("Install plan: %s, Phase: Installing", latest.Metadata.Name)},
				Fix:         "Wait for the installation to complete. This typically takes 1-5 minutes.",
			}},
		}
	default:
		return CheckOutput{
			Check: report.CheckResult{Name: name, Status: "warn", Detail: fmt.Sprintf("Install plan phase: %q (%s)", latest.Status.Phase, latest.Metadata.Name)},
		}
	}
}

func CheckPullSecretHealth(c *kube.Client) CheckOutput {
	ps, err := registry.PullSecret(c)
	if err != nil {
		return CheckOutput{
			Check: report.CheckResult{Name: "Pull secret", Status: "fail", Detail: fmt.Sprintf("Failed to check pull secret: %v", err)},
		}
	}

	if !ps.Exists {
		return CheckOutput{
			Check: report.CheckResult{Name: "Pull secret", Status: "fail", Detail: "Pull secret is missing"},
			Problems: []report.Problem{{
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
		return CheckOutput{
			Check: report.CheckResult{Name: "Pull secret", Status: "warn", Detail: fmt.Sprintf("Pull secret exists but: %s", ps.Detail)},
			Problems: []report.Problem{{
				ID:          "pull-secret-invalid",
				Severity:    "warning",
				Title:       "Pull secret exists but may be misconfigured",
				Description: fmt.Sprintf("The pull secret was found but does not appear to have the correct format: %s", ps.Detail),
				Evidence:    []string{ps.Detail},
				Fix:         "Update the pull secret with valid quay.io/rhoai credentials in the Setup tab.",
			}},
		}
	}

	return nodePullSecretHealth(c, rhoai.CachedControlPlane(c))
}

// nodePullSecretHealth is the "Pull secret" check once additional-pull-secret
// is valid. On a hosted control plane the nodes use it, so it passes. A
// self-managed cluster's nodes pull with openshift-config/pull-secret, which
// the updater cannot read; a READY nightly catalog on its quay.io/rhoai image
// shows that they can pull, otherwise the check says what to run
// (NodePullSecretCommand). An unknown topology is treated as possibly
// self-managed.
func nodePullSecretHealth(c *kube.Client, controlPlane string) CheckOutput {
	if controlPlane == rhoai.ControlPlaneHosted {
		return CheckOutput{
			Check: report.CheckResult{Name: "Pull secret", Status: "pass", Detail: "Pull secret is configured and valid"},
		}
	}
	cs, csErr := rhoai.GetCatalogSource(c)
	if csErr == nil && rhoai.CatalogPulledFromQuay(cs) {
		return CheckOutput{
			Check: report.CheckResult{Name: "Pull secret", Status: "pass", Detail: "additional-pull-secret is valid, and the nodes can pull from quay.io/rhoai: the nightly catalog runs its quay.io/rhoai image"},
		}
	}
	catalogSrc := "no nightly catalog runs yet that would show it"
	switch {
	case csErr != nil:
		catalogSrc = "the nightly catalog could not be read: " + csErr.Error()
	case cs.Exists:
		catalogSrc = fmt.Sprintf("CatalogSource %s is %s on %s", rhoai.CatalogName, cs.State, cs.Image)
	}
	topology := "Infrastructure cluster: controlPlaneTopology is not External (self-managed)"
	title := "The nodes may not have the quay.io/rhoai credentials"
	description := "This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with kube-system/additional-pull-secret, which the updater uses for its own Quay lookups. "
	fix := "Click Add to the nodes' pull secret under Pull secret in the cluster setup on the Status page: it copies the token saved there into openshift-config/pull-secret with your own permissions. Or a cluster-admin runs the command below in bash or zsh (jq 1.6 or later): it asks for the quay.io/rhoai token from the setup and adds only that entry. " +
		"Then click Check that the nodes can pull there."
	switch controlPlane {
	case rhoai.ControlPlaneSelfManaged:
	case rhoai.ControlPlaneManaged:
		topology = "Infrastructure cluster: controlPlaneTopology is not External, and Red Hat SRE's managed-cluster webhooks (*.managed.openshift.io) are installed (ROSA Classic or OpenShift Dedicated)"
		description = "Red Hat runs this cluster: its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with kube-system/additional-pull-secret, which the updater uses for its own Quay lookups. Red Hat creates that secret and documents no procedure for changing it on these services, so the updater does not change it. "
		fix = "If you choose to, a cluster-admin runs the command below in bash or zsh (jq 1.6 or later): it asks for the quay.io/rhoai token from the setup and adds only that entry. Then click Check that the nodes can pull under Pull secret in the cluster setup on the Status page."
	default:
		topology = "Infrastructure cluster or the validating webhook configurations could not be read, so the platform is unknown"
		title = "The nodes may not have the quay.io/rhoai credentials (if this cluster is self-managed)"
		description = "The updater could not read whether this cluster has a hosted control plane. On a self-managed cluster the nodes pull with the cluster pull secret, openshift-config/pull-secret, not with kube-system/additional-pull-secret, which the updater uses for its own Quay lookups. "
	}
	return CheckOutput{
		Check: report.CheckResult{Name: "Pull secret", Status: "warn", Detail: "additional-pull-secret is valid; whether the nodes can pull from quay.io/rhoai is not known"},
		Problems: []report.Problem{{
			ID:       "node-pull-secret-unchecked",
			Severity: "warning",
			Title:    title,
			Description: description + "The updater's ServiceAccount has no access to openshift-config/pull-secret, which holds every registry credential of the cluster, so this check cannot tell whether the quay.io/rhoai entry is there. " +
				"Without it, Update stops at the catalog check with ErrImagePull and changes nothing.",
			Evidence:     []string{topology, catalogSrc},
			Fix:          fix,
			LearnMore:    "The nodes pick up the change within a few minutes, without a reboot; oc get mcp shows UPDATED True for every pool when they all have it.",
			TechnicalCmd: rhoai.NodePullSecretCommand,
		}},
	}
}

func checkImageMirror(c *kube.Client) CheckOutput {
	idms, err := node.ImageMirror(c)
	if err != nil {
		return CheckOutput{
			Check: report.CheckResult{Name: "Image mirror", Status: "fail", Detail: fmt.Sprintf("Failed to check image mirror: %v", err)},
		}
	}

	if !idms.Exists {
		return CheckOutput{
			Check: report.CheckResult{Name: "Image mirror", Status: "fail", Detail: "Image mirror is not configured"},
			Problems: []report.Problem{{
				ID:          "idms-missing",
				Severity:    "warning",
				Title:       "Image mirror is not configured",
				Description: "The image mirror redirects image pulls from the production registry (registry.redhat.io) to the nightly registry (quay.io/rhoai). Without it, the cluster will try to pull from the wrong location.",
				Evidence:    []string{fmt.Sprintf("No image mirror found for source '%s'", rhoai.IDMSSource)},
				Fix:         "Configure the image mirror in the Setup tab. This is required for nightly builds to work.",
				LearnMore:   "The ImageDigestMirrorSet (IDMS) tells the cluster to redirect image pulls from registry.redhat.io/rhoai to quay.io/rhoai, so nightly images are pulled from the correct location.",
			}},
		}
	}

	return CheckOutput{
		Check: report.CheckResult{Name: "Image mirror", Status: "pass", Detail: fmt.Sprintf("Image mirror configured (%s)", idms.Name)},
	}
}

func checkNodeCapacity(c *kube.Client) CheckOutput {
	out := CheckOutput{Check: report.CheckResult{Name: "Node capacity", Status: "pass", Detail: "All nodes healthy"}}

	path := rhoai.ClusterPath("v1", "nodes", "")
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			out.Check = report.CheckResult{Name: "Node capacity", Status: "warn", Detail: "Could not parse node list"}
			return out
		}
		out.Check = report.CheckResult{Name: "Node capacity", Status: "warn", Detail: "Could not query nodes"}
		return out
	}

	if len(items) == 0 {
		out.Check = report.CheckResult{Name: "Node capacity", Status: "warn", Detail: "No nodes found"}
		return out
	}

	totalNodes := len(items)
	var notReadyNodes []string
	for _, node := range items {
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
		out.Check = report.CheckResult{Name: "Node capacity", Status: "warn", Detail: fmt.Sprintf("%d of %d nodes are not ready", len(notReadyNodes), totalNodes)}
		out.Problems = append(out.Problems, report.Problem{
			ID:           "nodes-not-ready",
			Severity:     "warning",
			Title:        fmt.Sprintf("%s %s not ready", kube.CountNoun(len(notReadyNodes), "cluster node", "cluster nodes"), kube.Verb(len(notReadyNodes), "is", "are")),
			Description:  "Some cluster nodes are not in a Ready state. This can prevent pods from being scheduled and cause deployments to get stuck.",
			Evidence:     append([]string{fmt.Sprintf("Not-ready nodes: %s", strings.Join(notReadyNodes, ", "))}, fmt.Sprintf("Total nodes: %d", totalNodes)),
			Fix:          "Check the cluster infrastructure. Node issues are typically caused by resource exhaustion, network problems, or infrastructure failures.",
			TechnicalCmd: "oc get nodes",
		})
		return out
	}

	// Pods that cannot be scheduled are reported by the Operator pods and
	// RHOAI pods checks, with the scheduler's own reason.
	out.Check.Detail = fmt.Sprintf("All %d nodes are ready", totalNodes)
	return out
}

// --- Auto-fix ---

// Run is the handler-facing entry point for cluster diagnostics.
func Run(c *kube.Client) (*report.DiagnosticsResponse, error) {
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
func ApplyFix(c *kube.Client, problemID string) (*types.OperationResponse, error) {
	return applyFix(c, problemID, nil)
}

// ApplyFixAcknowledged is ApplyFix after the user confirmed the problem's
// Acknowledge statement (POST /api/diagnostics/fix/acknowledged), with the
// problem's AcknowledgeBinding they reviewed. A fix that needs the
// acknowledgement refuses ("conflict") when the binding does not match
// what it would act on now. Fixes that need none run as with ApplyFix.
func ApplyFixAcknowledged(c *kube.Client, problemID string, binding report.AckBinding) (*types.OperationResponse, error) {
	return applyFix(c, problemID, &binding)
}

func applyFix(c *kube.Client, problemID string, ack *report.AckBinding) (*types.OperationResponse, error) {
	if res, ok := applyLeftoverFix(c, problemID, ack); ok {
		return res, nil
	}
	switch problemID {
	case "delete-stale-webhooks":
		return applyFixDeleteStaleWebhooks(c)
	case "delete-stale-installplans":
		return applyFixDeleteStaleInstallPlans(c)
	case "assist-rollout":
		return components.AssistRollout(c)
	}
	if module, ok := strings.CutPrefix(problemID, "restart-module-operator:"); ok {
		return components.ApplyFixRestartModuleOperator(c, module)
	}
	if msg, ok := removedFixes[problemID]; ok {
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: "validation"}, nil
	}
	if target, ok := strings.CutPrefix(problemID, "assist-rollout:"); ok {
		ns, name, found := strings.Cut(target, "/")
		if !found {
			return &types.OperationResponse{Success: false, Message: "assist-rollout needs <namespace>/<deployment>", ErrorCode: "validation"}, nil
		}
		return components.AssistRolloutFor(c, ns, name)
	}
	if target, ok := strings.CutPrefix(problemID, "restore-rollout-strategy:"); ok {
		ns, name, found := strings.Cut(target, "/")
		if !found {
			return &types.OperationResponse{Success: false, Message: "restore-rollout-strategy needs <namespace>/<deployment>", ErrorCode: "validation"}, nil
		}
		return components.RestoreRolloutStrategy(c, ns, name)
	}
	if compName, ok := strings.CutPrefix(problemID, "disable-component:"); ok {
		return dsc.DisableComponent(c, compName)
	}
	return &types.OperationResponse{
		Success:   false,
		Message:   fmt.Sprintf("Unknown fix action: %s", problemID),
		ErrorCode: "validation",
	}, nil
}

func applyFixDeleteStaleInstallPlans(c *kube.Client) (*types.OperationResponse, error) {
	plans, err := listInstallPlans(c)
	if err != nil {
		if kube.IsK8sNotFound(err) {
			return components.NothingToDo("there are no install plans. Nothing was deleted.", nil), nil
		}
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to list install plans: %v", err),
			ErrorCode: kube.ErrorCodeFromK8sErr(err),
		}, nil
	}

	sub, err := readSubscriptionState(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed to read the Subscription: %v", err), ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	}
	var logs, deleted, failed, changed []string
	for _, ip := range plans {
		if ok, why := installPlanDeletable(ip, sub); !ok {
			logs = append(logs, fmt.Sprintf("Kept install plan %s: %s", ip.Metadata.Name, why))
			continue
		}
		path := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, ip.Metadata.Name)
		err := kube.DeleteExact(c, path, ip.Metadata.UID, ip.Metadata.ResourceVersion)
		switch {
		case err == nil:
			deleted = append(deleted, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Deleted install plan %s (phase Failed)", ip.Metadata.Name))
		case kube.IsK8sNotFound(err):
			logs = append(logs, fmt.Sprintf("Install plan %s was already gone", ip.Metadata.Name))
		case kube.IsK8sConflict(err):
			changed = append(changed, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Kept install plan %s: it changed after it was checked", ip.Metadata.Name))
		default:
			failed = append(failed, ip.Metadata.Name)
			logs = append(logs, fmt.Sprintf("Error: could not delete install plan %s: %v", ip.Metadata.Name, err))
		}
	}

	if len(deleted) > 0 {
		record.RecordActivity(c, types.ActivityEntry{
			Timestamp: record.NowRFC3339(),
			User:      kube.GetUser(c),
			Action:    "fix-delete-stale-installplans",
			Detail:    "deleted Failed install plans: " + strings.Join(deleted, ", "),
			Success:   len(failed) == 0,
		})
	}

	switch {
	case len(deleted) == 0 && len(failed) == 0 && len(changed) > 0:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s %s changed after %s checked, so nothing was deleted. Run diagnostics again.", kube.Verb(len(changed), "Install plan", "Install plans"), strings.Join(changed, ", "), kube.Verb(len(changed), "it was", "they were")), Logs: logs, ErrorCode: "conflict"}, nil
	case len(deleted) == 0 && len(failed) == 0:
		return components.NothingToDo("no failed install plan can be deleted safely (see the log). Nothing was deleted.", logs), nil
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
		Message: fmt.Sprintf("Deleted %s: %s. OLM resolves the Subscription again if it was waiting on one of them.", kube.CountNoun(len(deleted), "failed install plan", "failed install plans"), strings.Join(deleted, ", ")),
		Logs:    logs,
	}, nil
}
