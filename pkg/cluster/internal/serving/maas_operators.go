package serving

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// The operators MaaS needs, installed from OperatorHub the way the MaaS
// install docs do it (opendatahub-io/models-as-a-service at 68b70af1,
// docs/content/install/platform-setup.md, "Red Hat OpenShift AI" tab):
//
//   - Red Hat Connectivity Link: Namespace kuadrant-system, OperatorGroup
//     kuadrant-operator-group (no spec: all namespaces), and Subscription
//     kuadrant-operator for package rhcl-operator from redhat-operators
//     (installPlanApproval Automatic), with the env the same tab gives under
//     "Rate limiting when Limitador is unavailable" (rate limits "may not be
//     enforced unless the Kuadrant operator is configured to deny traffic
//     on failure"): RATELIMIT_CHECK_SERVICE_FAILURE_MODE and
//     RATELIMIT_REPORT_SERVICE_FAILURE_MODE deny, so rate limits fail
//     closed (rhclDenyOnFailureEnv). AUTH_SERVICE_TIMEOUT, a load tuning
//     note, is not set. The channel is not the docs' "stable" (1.4.3, which
//     prerequisites.md warns about) but the one whose head meets the docs'
//     minimum for RHOAI 3.6 (rhclDocsMinimum), read from the PackageManifest.
//     The same page says RHOAI-managed clusters commonly name it
//     rhcl-operator (in rh-connectivity-link), so an existing install is
//     found by either name, in the namespace of its CSV.
//   - CloudNativePG, for this tool's database (the MaaS docs leave
//     PostgreSQL to the customer, maas-setup.md "Database Setup"): package
//     cloudnative-pg from certified-operators, its default channel, laid
//     out as Connectivity Link is: its own Namespace cnpg-system,
//     OperatorGroup cloudnative-pg for all namespaces, Subscription
//     cloudnative-pg (the package's name, as CloudNativePG's own OpenShift
//     tests name it). Not openshift-operators: platform-setup.md (Install
//     Gateway API Controller) warns that "any operator installed into the
//     openshift-operators namespace can pull an OSSM upgrade into its
//     InstallPlan (OLM v0 limitation), re-breaking the gateway". An
//     existing install is found by its CSV, in any namespace.
//
// What this tool installs it records (MaaSRecord.Operators), and never
// removes: both are shared platform pieces (RHOAI's inference gateway
// authentication, AuthPolicy openshift-ai-inference-authn, is Kuadrant's).

// rhclDocsMinimum is the Connectivity Link version the MaaS docs ask for
// with RHOAI 3.6 (prerequisites.md, "RHOAI 3.6 — use RHCL v1.5": "use RHCL
// v1.5.0+ (OLM channel stable-1.5)").
const rhclDocsMinimum = "1.5"

// rhclAuthBypassVersion is the release prerequisites.md warns about: "RHCL
// v1.4.0 contains a Wasm shim bug that silently bypasses gateway
// authentication. Management endpoints return AUTH_FAILURE; inference
// endpoints work but are unauthenticated. Upgrade to RHCL v1.4.1+."
const rhclAuthBypassVersion = "1.4.0"

var (
	RHCLOperator = envkit.OperatorSpec{ID: "rhcl", Title: "Red Hat Connectivity Link", Pkg: "rhcl-operator", SubName: "kuadrant-operator", SubNames: []string{"kuadrant-operator", "rhcl-operator"},
		Namespace: "kuadrant-system", Source: "redhat-operators", Group: "kuadrant-operator-group",
		API: KuadrantGroup, Controller: KuadrantController, Channel: rhclChannel, Env: rhclDenyOnFailureEnv}
	cnpgOperator = envkit.OperatorSpec{ID: "cnpg", Title: "CloudNativePG", Pkg: "cloudnative-pg", SubName: "cloudnative-pg", SubNames: []string{"cloudnative-pg"},
		Namespace: "cnpg-system", Source: "certified-operators", Group: "cloudnative-pg",
		API: CNPGGroup, Controller: CNPGController}
)

// rhclChannel is Connectivity Link's channel rule (RHCLTargetChannel), with
// the refusal when no channel meets the docs' minimum.
func rhclChannel(p envkit.OperatorPackage) (string, string) {
	if ch := RHCLTargetChannel(p); ch != "" {
		return ch, ""
	}
	return "", fmt.Sprintf("No channel of %s in %s has %s or later, which the MaaS docs ask for. Nothing was changed.", "rhcl-operator", "redhat-operators", rhclDocsMinimum)
}

// rhclDenyOnFailureEnv is the Subscription env platform-setup.md gives so
// that rate limits are enforced (traffic denied) when Limitador cannot be
// reached: "Set RATELIMIT_CHECK_SERVICE_FAILURE_MODE and
// RATELIMIT_REPORT_SERVICE_FAILURE_MODE to deny on the operator
// Subscription (spec.config.env)".
var rhclDenyOnFailureEnv = []map[string]any{
	{"name": "RATELIMIT_CHECK_SERVICE_FAILURE_MODE", "value": "deny"},
	{"name": "RATELIMIT_REPORT_SERVICE_FAILURE_MODE", "value": "deny"},
}

// rhclMissingDenyEnv names the deny-on-failure variables a Subscription
// does not set to deny.
func rhclMissingDenyEnv(sub *envkit.OLMSubscription) []string {
	have := map[string]string{}
	env, _ := sub.Spec.Config["env"].([]any)
	for _, e := range env {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		value, _ := m["value"].(string)
		have[name] = value
	}
	var out []string
	for _, want := range rhclDenyOnFailureEnv {
		if have[want["name"].(string)] != "deny" {
			out = append(out, want["name"].(string))
		}
	}
	return out
}

// rhclDenyEnvPatch is the Subscription's spec.config.env with the
// deny-on-failure variables set and every other entry kept (a merge patch
// replaces a list whole).
func rhclDenyEnvPatch(sub *envkit.OLMSubscription) []any {
	var out []any
	set := map[string]bool{}
	env, _ := sub.Spec.Config["env"].([]any)
	for _, e := range env {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		for _, want := range rhclDenyOnFailureEnv {
			if want["name"] == name {
				m = want
				set[name] = true
			}
		}
		out = append(out, m)
	}
	for _, want := range rhclDenyOnFailureEnv {
		if !set[want["name"].(string)] {
			out = append(out, want)
		}
	}
	return out
}

// RHCLTargetChannel is the channel to install or upgrade Connectivity Link
// on: of the channels whose head meets the docs' minimum, the one with the
// lowest head (the docs name stable-1.5 for RHOAI 3.6, not the newest
// release), the package's default channel first on a tie. "" when no
// channel does.
func RHCLTargetChannel(p envkit.OperatorPackage) string {
	var best *envkit.PackageChannel
	for i := range p.Channels {
		ch := p.Channels[i]
		if ch.Head() == "" || kube.CompareVersions(ch.Head(), rhclDocsMinimum) < 0 {
			continue
		}
		if best == nil {
			best = &p.Channels[i]
			continue
		}
		switch cmp := kube.CompareVersions(ch.Head(), best.Head()); {
		case cmp < 0, cmp == 0 && ch.Name == p.DefaultChannel, cmp == 0 && best.Name != p.DefaultChannel && ch.Name < best.Name:
			best = &p.Channels[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// HopChannel is the channel OLM can upgrade installed on when a switch to
// target leaves it there: one that lists the installed CSV and whose head
// is newer, the newest such head below target's (seen live 2026-11-18: the
// RHCL catalog's stable-1.5 1.5.0 skips ">=1.4.4 <1.5.0", and 1.4.4 is only
// in stable-1.4, so 1.4.3 goes through stable-1.4 first). "" when none.
func HopChannel(p envkit.OperatorPackage, installedCSV, target string) string {
	installed := envkit.VersionCore(envkit.CSVVersion(installedCSV))
	targetCh, _ := p.Channel(target)
	var best *envkit.PackageChannel
	for i := range p.Channels {
		ch := p.Channels[i]
		if ch.Name == target || !ch.Has(installedCSV) || kube.CompareVersions(ch.Head(), installed) <= 0 {
			continue
		}
		if targetCh.Head() != "" && kube.CompareVersions(ch.Head(), targetCh.Head()) >= 0 {
			continue
		}
		if best == nil || kube.CompareVersions(ch.Head(), best.Head()) > 0 || (kube.CompareVersions(ch.Head(), best.Head()) == 0 && ch.Name < best.Name) {
			best = &p.Channels[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// --- memory of the Kuadrant operator -------------------------------------------------

// rhclMemoryLimit is the memory limit that stopped the Kuadrant operator's
// OOM kills on RHCL 1.5.0 (seen live 2026-11-18: its CSV's 500Mi limit was
// OOMKilled three times, 1Gi held; Red Hat known issue CONNLINK-1878). It is
// set only after a container is seen OOMKilled, and only on request.
const rhclMemoryLimit = "1Gi"

// operatorContainer is a container of a CSV's Deployments.
type operatorContainer struct {
	deployment, name string
	resources        map[string]map[string]string
}

// csvContainers reads the containers of a CSV's Deployments (its install
// strategy), which OLM gives a Subscription's spec.config.resources to.
func csvContainers(c *kube.Client, ns, name string) ([]operatorContainer, error) {
	var csv struct {
		Spec struct {
			Install struct {
				Spec struct {
					Deployments []struct {
						Name string `json:"name"`
						Spec struct {
							Template struct {
								Spec struct {
									Containers []struct {
										Name      string                       `json:"name"`
										Resources map[string]map[string]string `json:"resources"`
									} `json:"containers"`
								} `json:"spec"`
							} `json:"template"`
						} `json:"spec"`
					} `json:"deployments"`
				} `json:"spec"`
			} `json:"install"`
		} `json:"spec"`
	}
	if err := c.GetInto(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name), &csv); err != nil {
		return nil, err
	}
	var out []operatorContainer
	for _, d := range csv.Spec.Install.Spec.Deployments {
		for _, ct := range d.Spec.Template.Spec.Containers {
			out = append(out, operatorContainer{deployment: d.Name, name: ct.Name, resources: ct.Resources})
		}
	}
	return out, nil
}

// oomKilledContainers names the containers of the operator's pods (pods of
// its CSV's Deployments in its namespace) whose last or current
// termination was OOMKilled.
func oomKilledContainers(c *kube.Client, st envkit.OperatorState) ([]string, error) {
	if !st.Installed() || len(st.Deployments) == 0 {
		return nil, nil
	}
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
				State        struct {
					Terminated *struct {
						Reason string `json:"reason"`
					} `json:"terminated"`
				} `json:"state"`
				LastState struct {
					Terminated *struct {
						Reason string `json:"reason"`
					} `json:"terminated"`
				} `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "pods", st.CSVNS, ""), &items); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range items {
		ofOperator := false
		for _, d := range st.Deployments {
			ofOperator = ofOperator || strings.HasPrefix(p.Metadata.Name, d+"-")
		}
		if !ofOperator {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			oom := (cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason == "OOMKilled") ||
				(cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled")
			if oom {
				out = append(out, fmt.Sprintf("%s/%s (%d %s)", p.Metadata.Name, cs.Name, cs.RestartCount, kube.Verb(cs.RestartCount, "restart", "restarts")))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// rhclMemoryResources is the spec.config.resources that raises the
// operator's memory limit to rhclMemoryLimit and keeps every other value
// its CSV sets (OLM replaces a container's whole resources with
// spec.config.resources, for every container of the CSV). It returns why
// it is not offered instead: more than one container (the value would
// apply to all), or a limit already at least rhclMemoryLimit.
func rhclMemoryResources(containers []operatorContainer) (map[string]any, string) {
	if len(containers) != 1 {
		return nil, fmt.Sprintf("its CSV runs %d containers, and a Subscription's resources apply to every one of them", len(containers))
	}
	res := containers[0].resources
	limit, _ := ParseQuantity(res["limits"]["memory"])
	want, _ := ParseQuantity(rhclMemoryLimit)
	if limit >= want {
		return nil, fmt.Sprintf("its memory limit is already %s", res["limits"]["memory"])
	}
	out := map[string]any{}
	for _, kind := range []string{"limits", "requests"} {
		values := map[string]any{}
		for k, v := range res[kind] {
			values[k] = v
		}
		if kind == "limits" {
			values["memory"] = rhclMemoryLimit
		}
		if len(values) > 0 {
			out[kind] = values
		}
	}
	return out, ""
}

// The controller Deployments of Connectivity Link (Kuadrant) and
// CloudNativePG, by the names upstream gives them in OLM and non-OLM installs
// alike (seen live: openshift-operators/kuadrant-operator-controller-manager,
// cnpg-system/cnpg-controller-manager).
const (
	KuadrantController = "kuadrant-operator-controller-manager"
	CNPGController     = "cnpg-controller-manager"
)
