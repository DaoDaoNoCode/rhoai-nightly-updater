package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// moduleDeletionGrace is how long a module CR may be deleting before
// diagnostics reports it.
var moduleDeletionGrace = 5 * time.Minute

func checkPlatformModules(c *Client) checkOutput {
	const name = "Platform modules"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	var details []string

	// Platform provisioning gates (rhods-operator rhoai-3.6
	// docs/upgrade-ordering.md "Admin ack gates", pkg/controller/provision).
	r, err := rhoai.ListServed(c, rhoai.PlatformGroup, rhoai.PlatformListFmt, nil, rhoai.PlatformFallbackVersions)
	body := r.Body
	switch {
	case kube.IsK8sNotFound(err), err == nil && r.Version == "":
		details = append(details, "no Platform API (operator older than 3.6 or not installed)")
	case kube.IsK8sError(err, http.StatusForbidden):
		details = append(details, "Platform not checked: this app cannot read platforms (RBAC)")
		out.check.Status = "warn"
	case err != nil:
		details = append(details, fmt.Sprintf("could not read the Platform: %v", err))
		out.check.Status = "warn"
	default:
		var list struct {
			Items []rhoai.DSCObject `json:"items"`
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
					evidence := []string{fmt.Sprintf("Platform %s: ProvisioningProgress=False (AdminAckRequired): %s", pl.Metadata.Name, kube.TruncateText(cond.Message, 400))}
					keys, values, keysErr := components.PendingUpgradeAckKeys(c)
					cmd := components.UpgradeAckCommand("<key>")
					fix := "Read what the gate is about, then acknowledge it in the OpenShift console or with the command below (replace <key>)."
					switch {
					case keysErr != nil:
						evidence = append(evidence, fmt.Sprintf("ConfigMap %s/odh-upgrade-acks: %v", rhoai.SubNS, keysErr))
					case len(keys) > 0:
						var cmds []string
						for _, k := range keys {
							evidence = append(evidence, fmt.Sprintf("Not acknowledged: %s=%q", k, values[k]))
							cmds = append(cmds, components.UpgradeAckCommand(k))
						}
						cmd = strings.Join(cmds, "\n")
						fix = fmt.Sprintf("Read what each gate is about (the operator's release notes name it), then acknowledge %s with the commands below.", kube.Verb(len(keys), "it", "them"))
					}
					out.problems = append(out.problems, Problem{
						ID:              "platform-admin-ack-required",
						Severity:        "critical",
						Title:           "The operator waits for an administrator to acknowledge an upgrade step",
						Description:     "Before provisioning a new version, the operator checks upgrade gates. An unacknowledged gate blocks all component provisioning until an administrator sets its key to \"true\" in ConfigMap odh-upgrade-acks. This is a deliberate manual step, not a bug.",
						Evidence:        evidence,
						AffectedObjects: []string{"ConfigMap " + rhoai.SubNS + "/odh-upgrade-acks"},
						Fix:             fix,
						TechnicalCmd:    cmd,
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
						Evidence:    []string{fmt.Sprintf("Platform %s: ProvisioningProgress=False (RunlevelTimeoutExceeded): %s", pl.Metadata.Name, kube.TruncateText(cond.Message, 400))},
						Fix:         "Fix the module named in the message (see the DataScienceCluster and RHOAI pods results).",
					})
				}
			}
			// The phase is the Platform's own state, not this check's result:
			// label it, and point to where a non-Ready phase is explained.
			phase := kube.NonEmpty(pl.Status.Phase, "unknown")
			platform := fmt.Sprintf("Platform %s phase: %s", pl.Metadata.Name, phase)
			if phase != "Ready" {
				platform += " (the DataScienceCluster check shows why)"
			}
			details = append(details, platform)
		}
	}

	// What this check is about (modules stuck in deletion) comes first.
	var moduleDetails []string
	stuck, err := stuckModuleCRs(c, time.Now())
	if err != nil {
		moduleDetails = append(moduleDetails, fmt.Sprintf("Module CRs not checked: %v", err))
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	operators := components.NewModuleOperatorFinder(c, nil)
	for _, m := range stuck {
		out.check.Status = "fail"
		out.problems = append(out.problems, stuckModuleProblem(c, operators, m))
	}
	if len(stuck) == 0 && err == nil {
		moduleDetails = append(moduleDetails, "No module is stuck in deletion")
	}
	out.check.Detail = strings.Join(append(moduleDetails, details...), "; ")
	return out
}

type stuckModule struct {
	Module     string
	Name       string
	Finalizers []string
	Since      time.Duration
	// CRD names the kind for the oc command (<plural>.<group>).
	CRD string
}

func stuckModuleCRs(c *Client, now time.Time) ([]stuckModule, error) {
	api, err := rhoai.DiscoverComponentAPI(c)
	if err != nil {
		return nil, err
	}
	type listResult struct {
		items []components.ModuleCRItem
		err   error
	}
	results := make([]listResult, len(api.Kinds))
	kube.ParallelFor(len(api.Kinds), 6, func(i int) {
		results[i].items, results[i].err = components.ListKindCRs(c, api.Kinds[i])
	})
	var out []stuckModule
	var errs []string
	for i, k := range api.Kinds {
		if results[i].err != nil {
			errs = append(errs, results[i].err.Error())
			continue
		}
		for _, it := range results[i].items {
			t, ok := components.ParseK8sTime(it.Metadata.DeletionTimestamp)
			if !ok || now.Sub(t) < moduleDeletionGrace {
				continue
			}
			out = append(out, stuckModule{Module: k.Module(), Name: it.Metadata.Name, Finalizers: it.Metadata.Finalizers, Since: now.Sub(t), CRD: k.CRD()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func stuckModuleProblem(c *Client, operators *components.ModuleOperatorFinder, m stuckModule) Problem {
	evidence := []string{fmt.Sprintf("%s %s has been deleting for %s; finalizers: %s", m.Module, m.Name, components.FormatDuration(m.Since), kube.NonEmpty(strings.Join(m.Finalizers, ", "), "none"))}
	fix := "The finalizer is removed by the module's operator; make sure it is running and read its logs."
	op, why, err := operators.Find(m.Module, m.Name)
	switch {
	case err != nil:
		evidence = append(evidence, fmt.Sprintf("Operator: could not identify it: %v", err))
	case op == nil:
		evidence = append(evidence, "Operator: not identified: "+why)
	default:
		ok, why := deploymentReady(c, op.Namespace, op.Name)
		if ok {
			evidence = append(evidence, fmt.Sprintf("Operator %s is running (%s)", op.Ref(), op.Via))
			fix = fmt.Sprintf("The operator %s is running; read its logs for why it does not finish the cleanup.", op.Name)
		} else {
			evidence = append(evidence, fmt.Sprintf("Operator (%s): %s", op.Via, why))
			fix = fmt.Sprintf("Start %s again; it removes the finalizer.", op.Ref())
		}
	}
	switch m.Module {
	case "dashboard":
		paused := "the dashboard's operator"
		if op != nil {
			paused = op.Name
		}
		fix += " If Dashboard Dev paused " + paused + " (0 replicas), use Revert on the Dashboard Dev page; the deletion then completes."
	case "mlflowoperator":
		fix = "The MLflowOperator finalizer waits until no MLflow CR exists. Delete MLflow on the Environment page, or delete the MLflow CR; the deletion then completes. " + fix
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
		TechnicalCmd:    kube.ShellCommand("oc", "get", kube.NonEmpty(m.CRD, m.Module+"s."+rhoai.ComponentAPIGroup), m.Name, "-o", "yaml"),
	}
}

func checkManagedConfig(c *Client) checkOutput {
	const name = "Operator-managed config"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	installed := dscRelease(c)
	release := installed.Version
	var details []string
	count := 0
	namespaces, nsErr := components.RolloutNamespaces(c)
	if nsErr != nil && !errors.Is(nsErr, rhoai.ErrNoDSCInitialization) {
		details = append(details, fmt.Sprintf("could not tell the applications namespace, so its Deployments were not checked: %v", nsErr))
		out.check.Status = "warn"
	}
	for _, ns := range namespaces {
		body, _, err := c.Do(http.MethodGet, rhoai.NamespacedPath("apps/v1", "deployments", ns, ""), "", nil, url.Values{})
		if kube.IsK8sNotFound(err) {
			continue
		}
		if err != nil {
			details = append(details, fmt.Sprintf("could not list deployments in %s: %v", ns, err))
			out.check.Status = "warn"
			continue
		}
		var list struct {
			Items []components.RolloutDeployment `json:"items"`
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
					evidence = append(evidence, fmt.Sprintf("It was last applied for version %s; the platform is at %s", v, kube.NonEmpty(release, "unknown")))
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
			} else if v := ann["platform.opendatahub.io/version"]; v != "" && release != "" && v != release && ann["platform.opendatahub.io/instance.name"] == "default" &&
				installed.Name != "" && ann["platform.opendatahub.io/type"] == installed.Name {
				// Only Deployments the Platform applies carry the platform's
				// version and type: the operator stamps the release it runs
				// (status.release name and version). Module operators stamp
				// their own: the MCP lifecycle module's framework defaults
				// stamp type OpenDataHub and version 0.0.1 on its manager
				// (owner MCPLifecycleOperator "default") on an OpenShift AI
				// Self-Managed 3.6.0 cluster. A type other than the installed
				// release's name is not the platform's version to compare.
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
			if raw := ann[components.AssistRolloutAnnotation]; raw != "" {
				p := Problem{
					ID:              "rollout-strategy-patched-" + d.Metadata.Name,
					Severity:        "info",
					Title:           fmt.Sprintf("%s still has the rollout strategy set by assist-rollout", d.Metadata.Name),
					Description:     "assist-rollout set maxUnavailable=1 to unblock a rollout and saved the original value. Nothing resets it automatically.",
					Evidence:        []string{fmt.Sprintf("Deployment %s: maxUnavailable=%s, saved: %s", target, kube.NonEmpty(d.CurrentMaxUnavailable(), "unset"), raw)},
					AffectedObjects: []string{"Deployment " + target},
				}
				if d.RolloutComplete() {
					p.Fix = "The rollout has finished; restore the original value."
					p.AutoFixable = true
					p.AutoFixAction = "restore-rollout-strategy:" + target
					p.ConfirmMessage = fmt.Sprintf("This patches Deployment %s: restores spec.strategy.rollingUpdate.maxUnavailable to the saved value (if it is still 1) and removes the annotation %s. No pods restart.", target, components.AssistRolloutAnnotation)
				} else {
					p.Fix = "Wait until the rollout has finished, then re-scan to restore the original value."
				}
				out.problems = append(out.problems, p)
			}
		}
	}
	if out.check.Status == "pass" {
		details = append([]string{fmt.Sprintf("%s under operator management", kube.CountNoun(count, "deployment", "deployments"))}, details...)
	} else if len(out.problems) > 0 {
		details = append([]string{kube.CountNoun(len(out.problems), "finding", "findings")}, details...)
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

// platformRelease is the release a DataScienceCluster reports in
// status.release: its name (for example "OpenShift AI Self-Managed") and
// version.
type platformRelease struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// dscRelease reads the DataScienceCluster's status.release; empty fields
// when there is no DataScienceCluster or it cannot be read.
func dscRelease(c *Client) platformRelease {
	path, err := rhoai.DataScienceClusterPath(c)
	if err != nil {
		return platformRelease{}
	}
	body, _, err := c.Get(path)
	if err != nil {
		return platformRelease{}
	}
	var dsc struct {
		Status struct {
			Release platformRelease `json:"release"`
		} `json:"status"`
	}
	_ = json.Unmarshal(body, &dsc)
	return dsc.Status.Release
}

// channelHeadBehind reports when the Subscription's channel head is older
// than the installed CSV. OLM never downgrades (it follows replaces, skips
// and skipRange forward), so such a channel can only fail to resolve.
func channelHeadBehind(c *Client, sub subscriptionState) *Problem {
	if !sub.Exists || sub.Source == "" || sub.Channel == "" || sub.InstalledCSV == "" {
		return nil
	}
	installed, ok := registry.ParseCSVVersion(sub.InstalledCSV)
	if !ok {
		return nil
	}
	channels, err := catalog.Channels(c, sub.Source)
	if err != nil {
		return nil
	}
	for _, ch := range channels {
		m, _ := ch.(map[string]any)
		if name, _ := m["name"].(string); name != sub.Channel {
			continue
		}
		head, _ := m["currentCSV"].(string)
		parsed, ok := registry.ParseCSVVersion(head)
		if !ok || registry.CompareTags(parsed, installed) >= 0 {
			return nil
		}
		return &Problem{
			ID:          "subscription-channel-behind",
			Severity:    "warning",
			Title:       fmt.Sprintf("Channel %s offers %s, older than the installed %s", sub.Channel, head, sub.InstalledCSV),
			Description: "OLM never downgrades an operator. With this channel the Subscription cannot move to the catalog's version, and an install from it would be a downgrade.",
			Evidence:    []string{fmt.Sprintf("Subscription source %s, channel %s, head %s, installed %s", sub.Source, sub.Channel, head, sub.InstalledCSV)},
			Fix:         "Update to a nightly of the installed version or newer.",
		}
	}
	return nil
}
