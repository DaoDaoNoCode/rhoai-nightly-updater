package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	LearnMore      string   `json:"learnMore,omitempty"`
	TechnicalCmd   string   `json:"technicalCmd,omitempty"`
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

// DiagnoseCluster inspects the cluster for common RHOAI issues and returns
// a list of problems with suggested fixes and a set of health check results.
func DiagnoseCluster(c *Client) (*DiagnosticsResponse, error) {
	resp := &DiagnosticsResponse{}

	checks := []func(*Client) checkOutput{
		checkCatalogHealth,
		checkOperatorPods,
		checkSubscriptionHealth,
		checkCSVHealth,
		checkInstallPlanHealth,
		checkPullSecretHealth,
		checkImageMirror,
		checkStaleWebhooks,
		checkNodeCapacity,
	}

	for _, fn := range checks {
		out := fn(c)
		resp.Checks = append(resp.Checks, out.check)
		resp.Problems = append(resp.Problems, out.problems...)
	}

	// Deduplicate problems by ID
	seen := make(map[string]bool)
	deduped := resp.Problems[:0]
	for _, p := range resp.Problems {
		if !seen[p.ID] {
			seen[p.ID] = true
			deduped = append(deduped, p)
		}
	}
	resp.Problems = deduped

	// Sort problems by severity: critical first, then warning, then info
	severityOrder := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.Slice(resp.Problems, func(i, j int) bool {
		return severityOrder[resp.Problems[i].Severity] < severityOrder[resp.Problems[j].Severity]
	})

	return resp, nil
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

func checkOperatorPods(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: "Operator pods", Status: "pass", Detail: "All operator pods healthy"}}

	pods, err := getPodsInNamespace(c, SubNS)
	if err != nil {
		if IsK8sError(err, 404) {
			out.check = CheckResult{Name: "Operator pods", Status: "pass", Detail: "No operator namespace (operator not installed yet)"}
			return out
		}
		out.check = CheckResult{Name: "Operator pods", Status: "warn", Detail: "Could not list operator pods"}
		return out
	}

	if len(pods) == 0 {
		out.check = CheckResult{Name: "Operator pods", Status: "pass", Detail: "No operator pods (operator not installed yet)"}
		return out
	}

	for _, pod := range pods {
		for _, container := range pod.Containers {
			if container.Reason == "CrashLoopBackOff" {
				out.check = CheckResult{Name: "Operator pods", Status: "fail", Detail: fmt.Sprintf("Pod %s: CrashLoopBackOff", pod.Name)}
				out.problems = append(out.problems, Problem{
					ID:          fmt.Sprintf("operator-crashloop-%s", pod.Name),
					Severity:    "critical",
					Title:       "The operator is crashing repeatedly",
					Description: fmt.Sprintf("Pod %s has container '%s' in CrashLoopBackOff with %d restarts. The operator cannot function while crashing.", pod.Name, container.Name, container.Restarts),
					Evidence:    []string{fmt.Sprintf("Pod: %s, Container: %s, Restarts: %d, State: CrashLoopBackOff", pod.Name, container.Name, container.Restarts)},
					Fix:         "Check the operator logs for errors. This is often caused by a broken nightly build. Try rolling back to a stable version or updating to a different nightly.",
					LearnMore: "**CrashLoopBackOff:** Check operator logs: `oc logs -n redhat-ods-operator -l name=rhods-operator --tail=50`\n\n" +
						"If the operator was recently reinstalled, stale webhooks may be blocking it.",
					TechnicalCmd: fmt.Sprintf("oc logs -n %s %s -c %s --tail=50", SubNS, pod.Name, container.Name),
				})
			}
			if container.Reason == "ImagePullBackOff" || container.Reason == "ErrImagePull" {
				out.check = CheckResult{Name: "Operator pods", Status: "fail", Detail: fmt.Sprintf("Pod %s: %s", pod.Name, container.Reason)}
				out.problems = append(out.problems, Problem{
					ID:             fmt.Sprintf("operator-pod-%s", container.Reason),
					Severity:       "critical",
					Title:          fmt.Sprintf("Operator pod %s: %s", pod.Name, container.Reason),
					Description:    fmt.Sprintf("Container %s in pod %s is in %s state.", container.Name, pod.Name, container.Reason),
					Evidence:       []string{fmt.Sprintf("Pod: %s", pod.Name), fmt.Sprintf("Container: %s", container.Name), fmt.Sprintf("State: %s", container.Reason)},
					Fix:            "The pull secret may be expired or the IDMS hasn't propagated to all nodes yet (wait 2-3 minutes after setup).",
					LearnMore:      "**ImagePullBackOff:** The pull secret may be expired or the IDMS hasn't propagated to all nodes yet (wait 2-3 minutes after setup).",
					AutoFixable:    true,
					AutoFixAction:  "recreate-subscription",
					ConfirmMessage: "This will delete and recreate the operator Subscription, CSV, and clean up stale webhooks. The operator will be briefly unavailable.\n\nImportant: Do NOT manually delete DSCI or DSC resources while the operator is down.",
					TechnicalCmd:   "oc logs -n redhat-ods-operator -l name=rhods-operator --tail=50",
				})
			}
		}
		if pod.Phase == "Pending" {
			reason := getPodSchedulingReason(c, SubNS, pod.Name)
			out.check = CheckResult{Name: "Operator pods", Status: "warn", Detail: fmt.Sprintf("Pod %s is Pending", pod.Name)}
			out.problems = append(out.problems, Problem{
				ID:             fmt.Sprintf("operator-pending-%s", pod.Name),
				Severity:       "warning",
				Title:          fmt.Sprintf("Pod %s in %s cannot be scheduled", pod.Name, SubNS),
				Description:    fmt.Sprintf("Pod %s in namespace %s is stuck in Pending state. The scheduler cannot place this pod on any node.", pod.Name, SubNS),
				Evidence:       []string{fmt.Sprintf("Pod: %s, Namespace: %s, Phase: Pending", pod.Name, SubNS), fmt.Sprintf("Scheduling reason: %s", reason)},
				Fix:            "Unblock the rollout by allowing Kubernetes to replace one old pod at a time.",
				AutoFixable:    true,
				AutoFixAction:  "assist-rollout",
				ConfirmMessage: "This will patch the deployment's rollout strategy to allow terminating one old pod, freeing resources for the new pod to schedule. The deployment may be briefly unavailable (1-2 minutes) during the transition.",
				TechnicalCmd:   fmt.Sprintf("oc describe pod %s -n %s", pod.Name, SubNS),
			})
		}
	}

	return out
}

func checkSubscriptionHealth(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: "Subscription health", Status: "pass", Detail: "Subscription active"}}

	sub, err := getSubscription(c)
	if err != nil {
		if IsK8sError(err, 404) {
			out.check = CheckResult{Name: "Subscription health", Status: "fail", Detail: "Subscription not found"}
			out.problems = append(out.problems, Problem{
				ID:          "subscription-missing",
				Severity:    "critical",
				Title:       "Operator Subscription not found",
				Description: "The rhods-operator Subscription does not exist. The operator cannot be installed without it.",
				Fix:         "Reinstall the operator to create a fresh Subscription.",
				LearnMore: "The Subscription tells OLM which operator to install and from which catalog. Without it, " +
					"no InstallPlan or CSV will be created.\n\nUse the Reinstall panel on the Dashboard page to create a new Subscription.",
				AutoFixable:    true,
				AutoFixAction:  "recreate-subscription",
				ConfirmMessage: "This will create a new operator Subscription pointing to the nightly catalog.\n\nImportant: Do NOT manually delete DSCI or DSC resources while the operator is down.",
			})
		} else {
			out.check = CheckResult{Name: "Subscription health", Status: "warn", Detail: "Could not read Subscription"}
		}
		return out
	}

	switch sub.State {
	case "Not Installed":
		out.check = CheckResult{Name: "Subscription health", Status: "pass", Detail: "No subscription (operator not installed yet)"}
		out.problems = append(out.problems, Problem{
			ID:          "subscription-missing",
			Severity:    "info",
			Title:       "No operator Subscription found",
			Description: "The rhods-operator Subscription does not exist. This is normal on a fresh cluster before the first nightly install.",
			Fix:         "Use the Update panel on the Dashboard to install a nightly build. This will create the Subscription automatically.",
		})
	case "AtLatestKnown":
		out.check.Detail = fmt.Sprintf("Subscription active (channel: %s, source: %s)", sub.Channel, sub.Source)
	case "UpgradePending":
		out.check = CheckResult{Name: "Subscription health", Status: "warn", Detail: "Subscription: UpgradePending"}
		out.problems = append(out.problems, Problem{
			ID:             "subscription-upgrade-pending",
			Severity:       "warning",
			Title:          "Subscription is waiting for an upgrade",
			Description:    fmt.Sprintf("The operator Subscription is in UpgradePending state (source: %s, channel: %s). OLM may be processing the InstallPlan.", sub.Source, sub.Channel),
			Fix:            "Wait a few minutes for OLM to process the InstallPlan. If the state persists, reinstall.",
			LearnMore:      "UpgradePending means OLM found a new version but hasn't completed the upgrade yet. Check that the InstallPlan was approved.",
			AutoFixable:    true,
			AutoFixAction:  "recreate-subscription",
			ConfirmMessage: "This will delete and recreate the operator Subscription. The operator will be briefly unavailable.",
			TechnicalCmd:   "oc get subscription " + SubName + " -n " + SubNS + " -o jsonpath='{.status}'",
		})
	case "UpgradeAvailable":
		out.check.Detail = fmt.Sprintf("An upgrade is available (channel: %s, source: %s)", sub.Channel, sub.Source)
	case "UpgradeFailed":
		out.check = CheckResult{Name: "Subscription health", Status: "fail", Detail: "Subscription: UpgradeFailed"}
		out.problems = append(out.problems, Problem{
			ID:             "subscription-upgrade-failed",
			Severity:       "critical",
			Title:          "Subscription upgrade failed",
			Description:    fmt.Sprintf("The operator Subscription upgrade failed (source: %s, channel: %s).", sub.Source, sub.Channel),
			Fix:            "Reinstall the operator subscription to force a fresh install.",
			AutoFixable:    true,
			AutoFixAction:  "recreate-subscription",
			ConfirmMessage: "This will delete and recreate the operator Subscription, CSV, and clean up stale webhooks.",
			TechnicalCmd:   "oc describe subscription " + SubName + " -n " + SubNS,
		})
	case "", "Unknown":
		out.check = CheckResult{Name: "Subscription health", Status: "warn", Detail: fmt.Sprintf("Subscription state: %q", sub.State)}
		out.problems = append(out.problems, Problem{
			ID:          "subscription-stuck",
			Severity:    "warning",
			Title:       fmt.Sprintf("Subscription in %q state", sub.State),
			Description: fmt.Sprintf("The operator Subscription is in %q state (source: %s, channel: %s). It may be waiting for an InstallPlan or stuck.", sub.State, sub.Source, sub.Channel),
			Fix:         "Wait a few minutes for OLM to process the InstallPlan. If the state persists, reinstall.",
			LearnMore: "**Channel mismatch:** The subscription channel may not exist in the nightly catalog. " +
				"Check available channels: `oc get packagemanifest rhods-operator -o jsonpath='{.status.channels[*].name}'`",
			AutoFixable:    true,
			AutoFixAction:  "recreate-subscription",
			ConfirmMessage: "This will delete and recreate the operator Subscription, CSV, and clean up stale webhooks.",
			TechnicalCmd:   "oc get subscription " + SubName + " -n " + SubNS + " -o jsonpath='{.status.state}'",
		})
	}

	return out
}

func checkCSVHealth(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: "Operator installed", Status: "pass", Detail: "CSV present and Succeeded"}}

	csv, err := getCSV(c)
	if err != nil {
		out.check = CheckResult{Name: "Operator installed", Status: "warn", Detail: "Could not query CSV"}
		return out
	}

	if csv.Phase == "Not Found" || csv.Name == "" {
		sub, subErr := getSubscription(c)
		hasSubscription := subErr == nil && sub.Name != "" && sub.State != "Not Installed"
		if !hasSubscription {
			out.check = CheckResult{Name: "Operator installed", Status: "pass", Detail: "No operator installed yet (normal for fresh clusters)"}
			return out
		}
		out.check = CheckResult{Name: "Operator installed", Status: "fail", Detail: "No CSV found — operator not installed"}
		if hasSubscription {
			out.problems = append(out.problems, Problem{
				ID:          "csv-not-found",
				Severity:    "critical",
				Title:       "Operator not installed (no CSV found)",
				Description: fmt.Sprintf("A Subscription exists (source: %s, channel: %s) but no ClusterServiceVersion was found. OLM may be unable to install the operator.", sub.Source, sub.Channel),
				Fix:         "Wait for OLM to process the InstallPlan (may take several minutes). If this persists, reinstall the operator.",
				LearnMore: "The operator CSV is created by OLM after the InstallPlan completes. If the CSV doesn't appear after 5 minutes, check:\n\n" +
					"1. Is the Subscription pointing to the correct catalog?\n" +
					"2. Is the InstallPlan created and approved?\n" +
					"3. Are there dependency resolution errors? Check subscription conditions.",
				AutoFixable:    true,
				AutoFixAction:  "recreate-subscription",
				ConfirmMessage: "This will delete and recreate the operator Subscription, CSV, and clean up stale webhooks.\n\nImportant: Do NOT manually delete DSCI or DSC resources while the operator is down.",
				TechnicalCmd:   "oc get csv -n " + SubNS,
			})
		}
		return out
	}

	switch csv.Phase {
	case "Succeeded":
		out.check.Detail = fmt.Sprintf("%s (%s)", csv.Name, csv.Phase)
	case "Failed":
		out.check = CheckResult{Name: "Operator installed", Status: "fail", Detail: fmt.Sprintf("Operator installation failed (%s)", csv.Name)}
		out.problems = append(out.problems, Problem{
			ID:             "operator-failed",
			Severity:       "critical",
			Title:          "The operator installation failed",
			Description:    fmt.Sprintf("The operator %s is in Failed state. OLM could not complete the installation.", csv.Name),
			Evidence:       []string{fmt.Sprintf("Operator: %s, Phase: Failed", csv.Name)},
			Fix:            "Run a reinstall to clear the failed state and try again. If the problem persists with nightly, try switching to stable.",
			AutoFixable:    true,
			AutoFixAction:  "recreate-subscription",
			ConfirmMessage: "This will delete the failed CSV and recreate the Subscription for a fresh install.",
			TechnicalCmd:   fmt.Sprintf("oc describe csv %s -n %s", csv.Name, SubNS),
		})
	case "Installing":
		out.check = CheckResult{Name: "Operator installed", Status: "warn", Detail: fmt.Sprintf("Operator is being installed (%s)", csv.Name)}
		out.problems = append(out.problems, Problem{
			ID:          "operator-installing",
			Severity:    "info",
			Title:       "The operator is being installed",
			Description: fmt.Sprintf("The operator %s is currently being installed. This is normal after an update or reinstall.", csv.Name),
			Evidence:    []string{fmt.Sprintf("Operator: %s, Phase: Installing", csv.Name)},
			Fix:         "Wait a few minutes for the installation to complete, then check again.",
		})
	default:
		out.check = CheckResult{Name: "Operator installed", Status: "warn", Detail: fmt.Sprintf("CSV phase: %s (%s)", csv.Phase, csv.Name)}
	}

	return out
}

func checkInstallPlanHealth(c *Client) checkOutput {
	ipPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "")
	body, _, err := c.get(ipPath)
	if err != nil {
		if IsK8sError(err, 404) {
			_, subErr := getSubscription(c)
			if subErr == nil {
				return checkOutput{
					check: CheckResult{Name: "Install plan", Status: "warn", Detail: "No install plan found but subscription exists"},
					problems: []Problem{{
						ID:           "installplan-missing",
						Severity:     "warning",
						Title:        "OLM hasn't created an install plan yet",
						Description:  "A subscription exists but no install plan has been created. OLM may be processing the subscription or the catalog may not be ready.",
						Evidence:     []string{fmt.Sprintf("No install plans found in namespace '%s'", SubNS)},
						Fix:          "Wait a few minutes. If no install plan appears, check the catalog health and use the Reinstall panel.",
						TechnicalCmd: fmt.Sprintf("oc get installplans -n %s", SubNS),
					}},
				}
			}
			return checkOutput{
				check: CheckResult{Name: "Install plan", Status: "pass", Detail: "No install plan (no subscription active)"},
			}
		}
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "fail", Detail: fmt.Sprintf("Failed to check install plans: %v", err)},
		}
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Approved bool `json:"approved"`
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
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "fail", Detail: "Failed to parse install plans"},
		}
	}

	if len(result.Items) == 0 {
		sub, subErr := getSubscription(c)
		if subErr != nil {
			return checkOutput{
				check: CheckResult{Name: "Install plan", Status: "warn", Detail: "Could not verify subscription status"},
			}
		}
		if sub.State == "Not Installed" || sub.Name == "" {
			return checkOutput{
				check: CheckResult{Name: "Install plan", Status: "pass", Detail: "No install plans (no subscription active)"},
			}
		}
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "warn", Detail: "No install plans found but subscription exists"},
			problems: []Problem{{
				ID:           "installplan-missing",
				Severity:     "warning",
				Title:        "OLM hasn't created an install plan yet",
				Description:  "A subscription exists but no install plan has been created. OLM may be processing the subscription or the catalog may not be ready.",
				Evidence:     []string{fmt.Sprintf("No install plans found in namespace '%s'", SubNS)},
				Fix:          "Wait a few minutes. If no install plan appears, check the catalog health and use the Reinstall panel.",
				TechnicalCmd: fmt.Sprintf("oc get installplans -n %s", SubNS),
			}},
		}
	}

	latest := result.Items[0]
	for _, ip := range result.Items[1:] {
		if ip.Metadata.CreationTimestamp > latest.Metadata.CreationTimestamp {
			latest = ip
		}
	}

	switch latest.Status.Phase {
	case "Complete":
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "pass", Detail: fmt.Sprintf("Latest install plan completed (%s)", latest.Metadata.Name)},
		}
	case "Failed":
		var reason string
		for _, cond := range latest.Status.Conditions {
			if cond.Status == "True" && cond.Message != "" {
				reason = cond.Message
				break
			}
		}
		evidence := []string{fmt.Sprintf("Install plan: %s, Phase: Failed", latest.Metadata.Name)}
		if reason != "" {
			evidence = append(evidence, fmt.Sprintf("Reason: %s", reason))
		}
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "fail", Detail: fmt.Sprintf("Install plan failed: %s", latest.Metadata.Name)},
			problems: []Problem{{
				ID:             "installplan-failed",
				Severity:       "critical",
				Title:          "The install plan failed",
				Description:    fmt.Sprintf("OLM could not execute the install plan. %s", reason),
				Evidence:       evidence,
				Fix:            "Delete the failed install plans and trigger a fresh installation by running a reinstall.",
				AutoFixable:    true,
				AutoFixAction:  "delete-stale-installplans",
				ConfirmMessage: "This will delete failed install plans. Only plans with 'Failed' status are removed — active or completed plans are preserved.",
				TechnicalCmd:   fmt.Sprintf("oc describe installplan %s -n %s", latest.Metadata.Name, SubNS),
			}},
		}
	case "Installing":
		return checkOutput{
			check: CheckResult{Name: "Install plan", Status: "warn", Detail: fmt.Sprintf("Install plan is in progress (%s)", latest.Metadata.Name)},
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
			check: CheckResult{Name: "Install plan", Status: "warn", Detail: fmt.Sprintf("Install plan phase: %s (%s)", latest.Status.Phase, latest.Metadata.Name)},
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

func checkStaleWebhooks(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: "Stale webhooks", Status: "pass", Detail: "No stale webhooks found"}}

	staleCount := 0
	var staleNames []string

	vwhPath := clusterPath("admissionregistration.k8s.io/v1", "validatingwebhookconfigurations", "")
	body, _, err := c.get(vwhPath)
	if err == nil {
		cnt, names := countRHOAIWebhooksWithNames(body)
		staleCount += cnt
		staleNames = append(staleNames, names...)
	}

	mwhPath := clusterPath("admissionregistration.k8s.io/v1", "mutatingwebhookconfigurations", "")
	body, _, err = c.get(mwhPath)
	if err == nil {
		cnt, names := countRHOAIWebhooksWithNames(body)
		staleCount += cnt
		staleNames = append(staleNames, names...)
	}

	csv, csvErr := getCSV(c)
	operatorRunning := csvErr == nil && csv.Phase == "Succeeded"

	if staleCount > 0 && !operatorRunning {
		out.check = CheckResult{Name: "Stale webhooks", Status: "warn", Detail: fmt.Sprintf("%d stale webhook(s) found", staleCount)}
		out.problems = append(out.problems, Problem{
			ID:          "stale-webhooks",
			Severity:    "warning",
			Title:       fmt.Sprintf("%d stale RHOAI webhook configuration(s) detected", staleCount),
			Description: "RHOAI webhook configurations exist but the operator is not running. These can block API calls.",
			Evidence:    staleNames,
			Fix:         "Delete stale webhooks or reinstall the operator (which recreates them).",
			LearnMore: "Stale webhooks are left behind when an operator is uninstalled but its webhook configurations remain. " +
				"They can block API calls to create notebooks, inference services, or modify DSC/DSCI resources.\n\n" +
				"**Safe to delete:** Yes — the operator will recreate them when reinstalled.",
			AutoFixable:    true,
			AutoFixAction:  "delete-stale-webhooks",
			ConfirmMessage: "This will delete stale RHOAI webhook configurations. The operator will recreate them when reinstalled.",
			TechnicalCmd:   "oc get validatingwebhookconfigurations,mutatingwebhookconfigurations | grep -i opendatahub",
		})
	}

	return out
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

	pendingDetails := checkPendingPods(c)
	if len(pendingDetails) > 0 {
		evidence := make([]string, 0, len(pendingDetails))
		var podSummaries []string
		for _, pd := range pendingDetails {
			evidence = append(evidence, fmt.Sprintf("Pod %s in %s is Pending: %s", pd.name, pd.namespace, pd.reason))
			podSummaries = append(podSummaries, fmt.Sprintf("%s/%s", pd.namespace, pd.name))
		}
		out.check = CheckResult{Name: "Node capacity", Status: "warn", Detail: fmt.Sprintf("%d pod(s) stuck in Pending: %s", len(pendingDetails), strings.Join(podSummaries, ", "))}
		out.problems = append(out.problems, Problem{
			ID:             "high-cpu-allocation",
			Severity:       "warning",
			Title:          fmt.Sprintf("%d pod(s) cannot be scheduled due to insufficient resources", len(pendingDetails)),
			Description:    fmt.Sprintf("The following pods are stuck in Pending state because the cluster does not have enough resources to schedule them: %s.", strings.Join(podSummaries, ", ")),
			Evidence:       evidence,
			Fix:            "Unblock the rollout by allowing Kubernetes to replace one old pod at a time.",
			AutoFixable:    true,
			AutoFixAction:  "assist-rollout",
			ConfirmMessage: "This will patch the deployment's rollout strategy to allow terminating one old pod, freeing resources for the new pod to schedule. The deployment may be briefly unavailable (1-2 minutes) during the transition.",
		})
		return out
	}

	out.check.Detail = fmt.Sprintf("All %d nodes are healthy", totalNodes)
	return out
}

// --- Helper functions ---

func countRHOAIWebhooksWithNames(body []byte) (int, []string) {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, nil
	}
	items, _ := result["items"].([]interface{})
	count := 0
	var names []string
	for _, item := range items {
		obj, _ := item.(map[string]interface{})
		meta, _ := obj["metadata"].(map[string]interface{})
		name, _ := meta["name"].(string)
		labels, _ := meta["labels"].(map[string]interface{})
		if isRHOAIWebhook(name, labels) {
			count++
			names = append(names, name)
		}
	}
	return count, names
}

func getPodSchedulingReason(c *Client, namespace, podName string) string {
	path := namespacedPath("v1", "pods", namespace, podName)
	body, _, err := c.get(path)
	if err != nil {
		return "Could not retrieve scheduling details."
	}

	var pod struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return "Could not parse pod details."
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == "PodScheduled" && cond.Status == "False" {
			if cond.Message != "" {
				return cond.Message
			}
			if cond.Reason != "" {
				return fmt.Sprintf("Scheduling failed: %s", cond.Reason)
			}
		}
	}
	return "The scheduler has not reported a reason yet."
}

type pendingPodDetail struct {
	name      string
	namespace string
	reason    string
}

func checkPendingPods(c *Client) []pendingPodDetail {
	var details []pendingPodDetail
	for _, ns := range []string{SubNS, "redhat-ods-applications"} {
		pods, err := getPodsInNamespace(c, ns)
		if err != nil {
			continue
		}
		for _, pod := range pods {
			if pod.Phase == "Pending" {
				reason := getPodSchedulingReason(c, ns, pod.Name)
				details = append(details, pendingPodDetail{
					name:      pod.Name,
					namespace: ns,
					reason:    reason,
				})
			}
		}
	}
	return details
}

// --- Auto-fix ---

// RunDiagnostics is the handler-facing entry point for cluster diagnostics.
func RunDiagnostics(c *Client) (*DiagnosticsResponse, error) {
	return DiagnoseCluster(c)
}

// ApplyFix executes the automatic fix for a known problem.
func ApplyFix(c *Client, problemID string) (*types.OperationResponse, error) {
	switch problemID {
	case "delete-stale-webhooks":
		return applyFixDeleteStaleWebhooks(c)
	case "recreate-subscription":
		return applyFixRecreateSubscription(c)
	case "delete-stale-installplans":
		return applyFixDeleteStaleInstallPlans(c)
	case "assist-rollout":
		return AssistRollout(c)
	case "fix-maas-gateway-annotation":
		return applyFixMaaSGatewayAnnotation(c)
	case "restart-operator":
		return applyFixRestartOperator(c)
	default:
		if strings.HasPrefix(problemID, "disable-component:") {
			compName := strings.TrimPrefix(problemID, "disable-component:")
			return applyFixDisableComponent(c, compName)
		}
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Unknown fix action: %s", problemID),
			ErrorCode: "validation",
		}, nil
	}
}

func applyFixDeleteStaleWebhooks(c *Client) (*types.OperationResponse, error) {
	count, warnings := cleanupStaleWebhooks(c)
	logs := []string{fmt.Sprintf("Removed %d stale webhook configuration(s)", count)}
	for _, w := range warnings {
		logs = append(logs, fmt.Sprintf("Warning: %s", w))
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "fix-delete-stale-webhooks",
		Detail:    fmt.Sprintf("removed %d webhooks", count),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Removed %d stale webhook configuration(s). The operator will recreate the webhooks it needs when reinstalled.", count),
		Logs:    logs,
	}, nil
}

func applyFixRecreateSubscription(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	origSub, origSubErr := getSubscription(c)
	source, channel := "", ""
	cs, csErr := getCatalogSource(c)
	if csErr != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot read the catalog source: %v", csErr)}, nil
	}
	if cs.Exists {
		source = CatalogName
		var chErr error
		channel, chErr = detectNightlyChannel(c, cs.Image)
		if chErr != nil || channel == "" {
			return &types.OperationResponse{Success: false, Message: "Cannot determine the catalog channel; existing Subscription was retained."}, nil
		}
	} else {
		target, discoveryErr := resolveStableTarget(c)
		if discoveryErr != nil {
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot determine the stable channel: %v", discoveryErr)}, nil
		}
		source, channel = target.Source, target.Channel
	}
	origSource, origChannel := source, channel
	if origSubErr == nil && origSub.Source != "" && origSub.Channel != "" {
		origSource = origSub.Source
		origChannel = origSub.Channel
	}

	subPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	_, delErr := c.delete(subPath)
	if delErr != nil && !IsK8sError(delErr, 404) {
		logs = append(logs, fmt.Sprintf("Warning: failed to delete existing subscription: %v", delErr))
	} else {
		logs = append(logs, "Deleted existing subscription (if any)")
	}

	select {
	case <-c.ctx.Done():
		return &types.OperationResponse{Success: false, Message: "Operation cancelled.", Logs: logs}, c.ctx.Err()
	case <-time.After(3 * time.Second):
	}

	logs = append(logs, fmt.Sprintf("Recreating subscription (source: %s, channel: %s)...", source, channel))

	newSub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec": map[string]interface{}{
			"channel":             channel,
			"installPlanApproval": "Automatic",
			"name":                SubName,
			"source":              source,
			"sourceNamespace":     CatalogNS,
		},
	}

	subApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)

	var applyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, applyErr = c.apply(subApplyPath, newSub)
		if applyErr == nil {
			break
		}
		slog.Warn("Subscription apply failed, retrying", "attempt", attempt, "error", applyErr)
		logs = append(logs, fmt.Sprintf("  Attempt %d/3 failed: %v", attempt, applyErr))
		if attempt < 3 {
			select {
			case <-c.ctx.Done():
				return &types.OperationResponse{Success: false, Message: "Operation cancelled during retry.", Logs: logs}, c.ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}

	if applyErr != nil {
		logs = append(logs, "All retries exhausted. Attempting to restore original Subscription...")
		restoreSub := map[string]interface{}{
			"apiVersion": "operators.coreos.com/v1alpha1",
			"kind":       "Subscription",
			"metadata":   map[string]interface{}{"name": SubName, "namespace": SubNS},
			"spec": map[string]interface{}{
				"channel":             origChannel,
				"installPlanApproval": "Automatic",
				"name":                SubName,
				"source":              origSource,
				"sourceNamespace":     CatalogNS,
			},
		}
		_, _, restoreErr := c.apply(subApplyPath, restoreSub)
		if restoreErr != nil {
			logs = append(logs, fmt.Sprintf("  CRITICAL: failed to restore original Subscription: %v", restoreErr))
		} else {
			logs = append(logs, fmt.Sprintf("  OK: Original Subscription restored (source=%s, channel=%s)", origSource, origChannel))
		}

		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "fix-recreate-subscription",
			Detail:    fmt.Sprintf("failed after 3 attempts: %v", applyErr),
			Success:   false,
		})

		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to recreate subscription after 3 attempts: %v", applyErr),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(applyErr),
		}, nil
	}

	logs = append(logs, "Subscription recreated successfully")

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "fix-recreate-subscription",
		Detail:    fmt.Sprintf("source=%s channel=%s", source, channel),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: "Subscription recreated. OLM will begin installing the operator.",
		Logs:    logs,
	}, nil
}

func applyFixDeleteStaleInstallPlans(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	ipPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "")
	body, _, err := c.get(ipPath)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to list install plans: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Failed to parse install plans",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	deleted := 0
	for _, ip := range result.Items {
		phase := ip.Status.Phase
		if phase != "Failed" && phase != "" {
			logs = append(logs, fmt.Sprintf("Skipped install plan %s (phase: %s)", ip.Metadata.Name, phase))
			continue
		}
		delPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ip.Metadata.Name)
		_, delErr := c.delete(delPath)
		if delErr != nil && !IsK8sError(delErr, 404) {
			logs = append(logs, fmt.Sprintf("Warning: failed to delete install plan %s: %v", ip.Metadata.Name, delErr))
		} else {
			deleted++
			logs = append(logs, fmt.Sprintf("Deleted install plan: %s (phase: %s)", ip.Metadata.Name, phase))
		}
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "fix-delete-stale-installplans",
		Detail:    fmt.Sprintf("deleted %d install plans", deleted),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Deleted %d failed install plan(s). OLM will create new ones if needed.", deleted),
		Logs:    logs,
	}, nil
}

func applyFixDisableComponent(c *Client, compName string) (*types.OperationResponse, error) {
	if compName == "" {
		return &types.OperationResponse{Success: false, Message: "Component name is required", ErrorCode: "validation"}, nil
	}

	allowedComponents := map[string]bool{
		"llamastackoperator": true,
		"feastoperator":      true,
		"trustyai":           true,
		"ray":                true,
		"kueue":              true,
		"sparkoperator":      true,
		"trainer":            true,
		"trainingoperator":   true,
		"modelsasservice":    true,
	}
	if !allowedComponents[compName] {
		return &types.OperationResponse{
			Success: false,
			Message: fmt.Sprintf("Component %q cannot be disabled through this tool. Use the OpenShift Console to edit the DSC directly.", compName),
		}, nil
	}

	patch := fmt.Sprintf(`{"spec":{"components":{%q:{"managementState":"Removed"}}}}`, compName)
	dscPath, err := dataScienceClusterPath(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Cannot find the DataScienceCluster: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	_, _, err = c.patch(dscPath, []byte(patch))
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to patch DataScienceCluster: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
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
		Message: fmt.Sprintf("Set %s to Removed. The operator will clean up the component.", compName),
	}, nil
}

func applyFixMaaSGatewayAnnotation(c *Client) (*types.OperationResponse, error) {
	gwPath := "/apis/gateway.networking.k8s.io/v1/namespaces/openshift-ingress/gateways/maas-default-gateway"
	patch := `{"metadata":{"annotations":{"opendatahub.io/managed":"false"}}}`
	_, _, err := c.patch(gwPath, []byte(patch))
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to patch gateway: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "fix-maas-gateway",
		Detail:    "added opendatahub.io/managed=false annotation",
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: "Gateway annotation added. MaaS prerequisites should resolve shortly.",
	}, nil
}

func applyFixRestartOperator(c *Client) (*types.OperationResponse, error) {
	depPath := namespacedPath("apps/v1", "deployments", SubNS, "rhods-operator")
	patchData, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{
						"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339),
					},
				},
			},
		},
	})

	_, _, err := c.strategicPatch(depPath, patchData)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to restart operator: %v", err),
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "restart-operator",
		Detail:    "rolling restart triggered",
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: "Operator restart triggered. The operator has multiple replicas so there will be no downtime. DSC conditions should refresh within 1-2 minutes.",
	}, nil
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
