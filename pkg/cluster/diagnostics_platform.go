package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// moduleDeletionGrace is how long a module CR may be deleting before
// diagnostics reports it.
var moduleDeletionGrace = 5 * time.Minute

// moduleOperators maps a module (lower-case kind of its
// components.platform.opendatahub.io CR) to the Deployment that removes the
// CR's finalizer (RHOAI operator notes §2.2; live names on RHOAI 3.6).
var moduleOperators = map[string]struct{ namespace, name string }{
	"dashboard":      {"redhat-ods-applications", "dashboard-operator"},
	"workbenches":    {"redhat-ods-applications", "workbenches-operator"},
	"aipipelines":    {"redhat-ods-applications", "data-science-pipelines-operator-controller-manager"},
	"kserve":         {"redhat-ods-applications", "kserve-module-controller-manager"},
	"aihub":          {"redhat-ods-applications", "aihub-controller-manager"},
	"mlflowoperator": {"redhat-ods-applications", "mlflow-operator-controller-manager"},
	"ogx":            {"redhat-ods-applications", "opendatahub-ogx-operator"},
	"ray":            {"redhat-ods-applications", "ray-module-operator-controller-manager"},
	"trainer":        {"redhat-ods-applications", "trainer-operator-controller-manager"},
	"feastoperator":  {"redhat-ods-applications", "opendatahub-feast-operator"},
	"trustyai":       {"redhat-ods-applications", "trustyai-operator-module-controller-manager"},
	"kueue":          {SubNS, "rhods-operator"},
}

func checkPlatformModules(c *Client) checkOutput {
	const name = "Platform modules"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	var details []string

	// Platform provisioning gates (rhods-operator rhoai-3.6
	// docs/upgrade-ordering.md "Admin ack gates", pkg/controller/provision).
	body, _, err := c.get("/apis/config.opendatahub.io/v1alpha1/platforms")
	switch {
	case IsK8sError(err, http.StatusNotFound):
		details = append(details, "no Platform API (operator older than 3.6 or not installed)")
	case IsK8sError(err, http.StatusForbidden):
		details = append(details, "Platform not checked: this app cannot read platforms (RBAC)")
		out.check.Status = "warn"
	case err != nil:
		details = append(details, fmt.Sprintf("could not read the Platform: %v", err))
		out.check.Status = "warn"
	default:
		var list struct {
			Items []dscObject `json:"items"`
		}
		if json.Unmarshal(body, &list) == nil && len(list.Items) > 0 {
			pl := list.Items[0]
			for _, cond := range pl.Status.Conditions {
				if cond.Type != "ProvisioningProgress" || cond.Status != "False" {
					continue
				}
				switch cond.Reason {
				case "AdminAckRequired":
					out.check.Status = "fail"
					evidence := []string{fmt.Sprintf("Platform %s: ProvisioningProgress=False (AdminAckRequired): %s", pl.Metadata.Name, truncate(cond.Message, 400))}
					evidence = append(evidence, pendingUpgradeAcks(c)...)
					out.problems = append(out.problems, Problem{
						ID:              "platform-admin-ack-required",
						Severity:        "critical",
						Title:           "The operator waits for an administrator to acknowledge an upgrade step",
						Description:     "Before provisioning a new version, the operator checks upgrade gates. An unacknowledged gate blocks all component provisioning until an administrator sets its key to \"true\" in ConfigMap odh-upgrade-acks. This is a deliberate manual step, not a bug.",
						Evidence:        evidence,
						AffectedObjects: []string{"ConfigMap " + SubNS + "/odh-upgrade-acks"},
						Fix:             "Read what the gate is about, then acknowledge it in the OpenShift console or with the command below (replace <key>).",
						TechnicalCmd:    "oc patch configmap odh-upgrade-acks -n " + SubNS + " --type merge -p '{\"data\":{\"<key>\":\"true\"}}'",
					})
				case "RunlevelTimeoutExceeded":
					if out.check.Status == "pass" {
						out.check.Status = "warn"
					}
					out.problems = append(out.problems, Problem{
						ID:          "platform-runlevel-timeout",
						Severity:    "warning",
						Title:       "Some modules were provisioned before the modules they wait for became ready",
						Description: "The operator provisions modules in run levels and waits up to 10 minutes for each level. It moved on because a module of an earlier level did not become ready; modules that depend on it may not work.",
						Evidence:    []string{fmt.Sprintf("Platform %s: ProvisioningProgress=False (RunlevelTimeoutExceeded): %s", pl.Metadata.Name, truncate(cond.Message, 400))},
						Fix:         "Fix the module named in the message (see the DataScienceCluster and RHOAI pods results).",
					})
				}
			}
			details = append(details, fmt.Sprintf("Platform %s phase %s", pl.Metadata.Name, nonEmpty(pl.Status.Phase, "unknown")))
		}
	}

	stuck, err := stuckModuleCRs(c, time.Now())
	if err != nil {
		details = append(details, fmt.Sprintf("module CRs not checked: %v", err))
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	for _, m := range stuck {
		out.check.Status = "fail"
		out.problems = append(out.problems, stuckModuleProblem(c, m))
	}
	if len(stuck) == 0 && err == nil {
		details = append(details, "no module is stuck in deletion")
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

// pendingUpgradeAcks lists the keys of odh-upgrade-acks that are not "true".
func pendingUpgradeAcks(c *Client) []string {
	body, _, err := c.get(namespacedPath("v1", "configmaps", SubNS, "odh-upgrade-acks"))
	if err != nil {
		return []string{fmt.Sprintf("ConfigMap %s/odh-upgrade-acks: %v", SubNS, err)}
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(body, &cm) != nil {
		return nil
	}
	var keys []string
	for k, v := range cm.Data {
		if v != "true" {
			keys = append(keys, fmt.Sprintf("Not acknowledged: %s=%q", k, v))
		}
	}
	sort.Strings(keys)
	return keys
}

type stuckModule struct {
	Module     string
	Name       string
	Finalizers []string
	Since      time.Duration
}

func stuckModuleCRs(c *Client, now time.Time) ([]stuckModule, error) {
	api, err := discoverComponentAPI(c)
	if err != nil {
		return nil, err
	}
	type listResult struct {
		items []moduleCRItem
		err   error
	}
	discovered := func() (componentAPI, error) { return api, nil }
	results := make([]listResult, len(api.Kinds))
	parallelFor(len(api.Kinds), 6, func(i int) {
		results[i].items, results[i].err = listModuleCRs(c, discovered, strings.ToLower(api.Kinds[i].Kind))
	})
	var out []stuckModule
	var errs []string
	for i, k := range api.Kinds {
		if results[i].err != nil {
			errs = append(errs, results[i].err.Error())
			continue
		}
		for _, it := range results[i].items {
			t, ok := parseK8sTime(it.Metadata.DeletionTimestamp)
			if !ok || now.Sub(t) < moduleDeletionGrace {
				continue
			}
			out = append(out, stuckModule{Module: strings.ToLower(k.Kind), Name: it.Metadata.Name, Finalizers: it.Metadata.Finalizers, Since: now.Sub(t)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func stuckModuleProblem(c *Client, m stuckModule) Problem {
	evidence := []string{fmt.Sprintf("%s %s has been deleting for %s; finalizers: %s", m.Module, m.Name, formatDuration(m.Since), nonEmpty(strings.Join(m.Finalizers, ", "), "none"))}
	fix := "The finalizer is removed by the module's operator; make sure it is running and read its logs."
	op, known := moduleOperators[m.Module]
	if known {
		ok, why := deploymentReady(c, op.namespace, op.name)
		if ok {
			evidence = append(evidence, fmt.Sprintf("Operator %s/%s is running", op.namespace, op.name))
			fix = fmt.Sprintf("The operator %s is running; read its logs for why it does not finish the cleanup.", op.name)
		} else {
			evidence = append(evidence, "Operator: "+why)
			fix = fmt.Sprintf("Start %s/%s again; it removes the finalizer.", op.namespace, op.name)
		}
	}
	switch m.Module {
	case "dashboard":
		fix += " If Dashboard Dev paused dashboard-operator (0 replicas), use Revert on the Dashboard Dev page; the deletion then completes."
	case "mlflowoperator":
		fix = "The MLflowOperator finalizer waits until no MLflow CR exists. Tear down MLflow (Dashboard Dev > Resources) or delete the MLflow CR; the deletion then completes. " + fix
	case "aipipelines":
		fix = "Delete the pipeline servers (DSPAs) first; their finalizers need the pipelines operator. " + fix
	}
	return Problem{
		ID:              "module-stuck-deleting-" + m.Module,
		Severity:        "critical",
		Title:           fmt.Sprintf("Module %s is stuck in deletion", m.Module),
		Description:     "The operator is removing this module, but the module CR still has a finalizer. Until it is gone, the Platform cannot finish (and DSC deletion or component removal hangs). Do not remove the finalizer by hand: that skips the module's cleanup and leaves its resources behind.",
		Evidence:        evidence,
		AffectedObjects: []string{fmt.Sprintf("%s %s", m.Module, m.Name)},
		Fix:             fix,
		TechnicalCmd:    fmt.Sprintf("oc get %ss.components.platform.opendatahub.io %s -o yaml", m.Module, m.Name),
	}
}

func checkManagedConfig(c *Client) checkOutput {
	const name = "Operator-managed config"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	release := dscReleaseVersion(c)
	var details []string
	count := 0
	for _, ns := range rolloutNamespaces {
		body, _, err := c.do(http.MethodGet, namespacedPath("apps/v1", "deployments", ns, ""), "", nil, url.Values{})
		if IsK8sError(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			details = append(details, fmt.Sprintf("could not list deployments in %s: %v", ns, err))
			out.check.Status = "warn"
			continue
		}
		var list struct {
			Items []rolloutDeployment `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}
		for _, d := range list.Items {
			count++
			target := ns + "/" + d.Metadata.Name
			ann := d.Metadata.Annotations
			if ann["opendatahub.io/managed"] == "false" {
				out.check.Status = "fail"
				evidence := []string{fmt.Sprintf("Deployment %s has opendatahub.io/managed=false", target)}
				if v := ann["platform.opendatahub.io/version"]; v != "" {
					evidence = append(evidence, fmt.Sprintf("It was last applied for version %s; the platform is at %s", v, nonEmpty(release, "unknown")))
				}
				out.problems = append(out.problems, Problem{
					ID:       "deployment-unmanaged-" + d.Metadata.Name,
					Severity: "warning",
					Title:    fmt.Sprintf("%s is excluded from operator management", d.Metadata.Name),
					Description: "With opendatahub.io/managed=false the operator skips this object and removes its owner reference, so it is frozen at the version it had " +
						"and is not upgraded or cleaned up with its module (odh-platform-utilities deploy action).",
					Evidence:        evidence,
					AffectedObjects: []string{"Deployment " + target},
					Fix:             "If nobody needs the manual change any more, remove the annotation; the operator then re-applies the current version on its next reconcile. The tool does not do this automatically.",
					TechnicalCmd:    fmt.Sprintf("oc annotate deployment %s -n %s opendatahub.io/managed-", d.Metadata.Name, ns),
				})
			} else if v := ann["platform.opendatahub.io/version"]; v != "" && release != "" && v != release && ann["platform.opendatahub.io/instance.name"] == "default" {
				// Only Deployments the Platform applies carry the platform
				// version; module operators stamp their own versions.
				out.problems = append(out.problems, Problem{
					ID:              "deployment-version-drift-" + d.Metadata.Name,
					Severity:        "info",
					Title:           fmt.Sprintf("%s was last applied for version %s", d.Metadata.Name, v),
					Description:     fmt.Sprintf("The platform reports version %s. Right after an update this is normal until the operator reconciles; if it stays, the operator is not updating this Deployment.", release),
					Evidence:        []string{fmt.Sprintf("Deployment %s: platform.opendatahub.io/version=%s, DataScienceCluster release %s", target, v, release)},
					AffectedObjects: []string{"Deployment " + target},
					Fix:             "Check the RHOAI operator logs and the DataScienceCluster result.",
				})
			}
			if raw := ann[assistRolloutAnnotation]; raw != "" {
				p := Problem{
					ID:              "rollout-strategy-patched-" + d.Metadata.Name,
					Severity:        "info",
					Title:           fmt.Sprintf("%s still has the rollout strategy set by assist-rollout", d.Metadata.Name),
					Description:     "assist-rollout set maxUnavailable=1 to unblock a rollout and saved the original value. Nothing resets it automatically.",
					Evidence:        []string{fmt.Sprintf("Deployment %s: maxUnavailable=%s, saved: %s", target, nonEmpty(d.currentMaxUnavailable(), "unset"), raw)},
					AffectedObjects: []string{"Deployment " + target},
				}
				if d.rolloutComplete() {
					p.Fix = "The rollout has finished; restore the original value."
					p.AutoFixable = true
					p.AutoFixAction = "restore-rollout-strategy:" + target
					p.ConfirmMessage = fmt.Sprintf("This patches Deployment %s: restores spec.strategy.rollingUpdate.maxUnavailable to the saved value (if it is still 1) and removes the annotation %s. No pods restart.", target, assistRolloutAnnotation)
				} else {
					p.Fix = "Wait until the rollout has finished, then re-scan to restore the original value."
				}
				out.problems = append(out.problems, p)
			}
		}
	}
	if out.check.Status == "pass" {
		details = append([]string{fmt.Sprintf("%d deployment(s) under operator management", count)}, details...)
	} else if len(out.problems) > 0 {
		details = append([]string{fmt.Sprintf("%d finding(s)", len(out.problems))}, details...)
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

func dscReleaseVersion(c *Client) string {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return ""
	}
	body, _, err := c.get(path)
	if err != nil {
		return ""
	}
	var dsc struct {
		Status struct {
			Release struct {
				Version string `json:"version"`
			} `json:"release"`
		} `json:"status"`
	}
	_ = json.Unmarshal(body, &dsc)
	return dsc.Status.Release.Version
}

// channelHeadBehind reports when the Subscription's channel head is older
// than the installed CSV. OLM never downgrades (it follows replaces, skips
// and skipRange forward), so such a channel can only fail to resolve.
func channelHeadBehind(c *Client, sub subscriptionState) *Problem {
	if !sub.Exists || sub.Source == "" || sub.Channel == "" || sub.InstalledCSV == "" {
		return nil
	}
	installed, ok := parseCSVVersion(sub.InstalledCSV)
	if !ok {
		return nil
	}
	channels, err := catalogChannels(c, sub.Source)
	if err != nil {
		return nil
	}
	for _, ch := range channels {
		m, _ := ch.(map[string]interface{})
		if name, _ := m["name"].(string); name != sub.Channel {
			continue
		}
		head, _ := m["currentCSV"].(string)
		parsed, ok := parseCSVVersion(head)
		if !ok || compareTags(parsed, installed) >= 0 {
			return nil
		}
		return &Problem{
			ID:          "subscription-channel-behind",
			Severity:    "warning",
			Title:       fmt.Sprintf("Channel %s offers %s, older than the installed %s", sub.Channel, head, sub.InstalledCSV),
			Description: "OLM never downgrades an operator. With this channel the Subscription cannot move to the catalog's version, and a reinstall from it would be a downgrade.",
			Evidence:    []string{fmt.Sprintf("Subscription source %s, channel %s, head %s, installed %s", sub.Source, sub.Channel, head, sub.InstalledCSV)},
			Fix:         "Update to a nightly of the installed version or newer.",
		}
	}
	return nil
}
