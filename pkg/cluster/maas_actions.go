package cluster

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The fixes a MaaS platform part offers on its own, besides Set up: each is
// shown in a confirmation listing its changes (types.MaaSAction), checked
// in full before its first write, and streamed as an operation.

// maasSharedPart: a part that is a shared platform piece (other RHOAI
// features use it). Setup installs or configures it when missing; Remove
// leaves it.
func maasSharedPart(id string) bool {
	switch id {
	case "rhcl", "kuadrant", "cnpg", "authorino-tls", "tier-policies":
		return true
	}
	return false
}

// operatorPlanPrefix names the namespace and OperatorGroup an install
// creates, for the setup plan.
func operatorPlanPrefix(p *envkit.OperatorInstallPlan) string {
	var out []string
	if p.NSMissing {
		out = append(out, "Namespace "+p.Op.Namespace)
	}
	if p.GroupMissing {
		out = append(out, "OperatorGroup "+p.Op.Namespace+"/"+p.Op.Group)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ", ") + ", "
}

// rhclUpgradeRefusal says why this tool cannot upgrade Connectivity Link
// itself ("" when it can): it changes only a Subscription the docs name,
// not managed by Argo CD, with automatic InstallPlan approval (it does not
// approve someone's Manual plans).
func rhclUpgradeRefusal(st envkit.OperatorState) string {
	switch {
	case !st.Installed():
		return "Connectivity Link is not installed; Set up installs it."
	case st.Sub == nil:
		return fmt.Sprintf("Its Subscription in %s is not named as the MaaS docs name it (kuadrant-operator, or rhcl-operator on RHOAI-managed clusters), so this tool does not change it. A cluster-admin switches its channel with the commands below.", st.CSVNS)
	case st.Sub.ArgoApp != "":
		return argo.GitOpsRefusal("Subscription "+st.Sub.Metadata.Namespace+"/"+st.Sub.Metadata.Name, st.Sub.ArgoApp, "changing its channel") + " Change the channel in Git."
	case st.Sub.Spec.InstallPlanApproval == "Manual":
		return fmt.Sprintf("Subscription %s/%s approves InstallPlans by hand (Manual), and this tool does not approve someone else's plans. A cluster-admin switches its channel and approves the plans.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name)
	case kube.FirstNonEmpty(st.Sub.Spec.SourceNamespace, rhoai.CatalogNS) != rhoai.CatalogNS:
		return fmt.Sprintf("Subscription %s/%s uses catalog %s/%s, outside %s, which this tool does not read.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, st.Sub.Spec.SourceNamespace, st.Sub.Spec.Source, rhoai.CatalogNS)
	}
	return ""
}

// rhclUpgradeOffer is the Upgrade Connectivity Link action for an install
// below the docs' minimum, with the commands that do the same by hand; nil
// when this tool cannot do it (the commands, or the reason, say why).
func rhclUpgradeOffer(c *Client, st envkit.OperatorState) (*types.MaaSAction, string) {
	if why := rhclUpgradeRefusal(st); why != "" {
		if st.Sub == nil {
			return nil, fmt.Sprintf("# %s\noc get subscriptions -n %s", why, kube.ShellQuote(st.CSVNS))
		}
		return nil, "# " + why
	}
	pkg, found, err := envkit.ReadOperatorPackage(c, rhclOperator.Pkg, st.Sub.Spec.Source)
	if err != nil || !found {
		return nil, ""
	}
	target := rhclTargetChannel(pkg)
	if target == "" {
		return nil, ""
	}
	targetCh, _ := pkg.Channel(target)
	changes := []string{fmt.Sprintf("Subscription %s/%s: channel %s to %s (version %s). OLM upgrades the operator; there is no way back to %s.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, kube.FirstNonEmpty(st.Sub.Spec.Channel, "not set"), target, targetCh.Head(), st.Version)}
	cmds := []string{rhclChannelCommand(st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, target)}
	if hop := hopChannel(pkg, st.CSV, target); hop != "" && !targetCh.Has(st.CSV) {
		hopCh, _ := pkg.Channel(hop)
		changes = append(changes, fmt.Sprintf("If OLM has no path from %s straight to %s (it then stays at %s), channel %s first, to %s, then %s.", st.Version, targetCh.Head(), st.Version, hop, hopCh.Head(), target))
		cmds = []string{commentText("# OLM may have no path from " + st.Version + " straight to " + target + ": go through " + hop + " first"), rhclChannelCommand(st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, hop), commentText("# once " + hopCh.CurrentCSV + " is installed:"), cmds[0]}
	}
	changes = append(changes, "Afterwards, Authorino TLS is set up again where an upgrade step recreated Authorino's Service (which removes its certificate).")
	return &types.MaaSAction{
		ID: "upgrade-rhcl", Label: "Upgrade Connectivity Link",
		Summary: fmt.Sprintf("Upgrades Red Hat Connectivity Link from %s to %s or later, as the MaaS docs ask, one channel at a time along the path OLM offers. Gateway authentication and rate limits restart during each step; it takes a few minutes.", st.Version, rhclDocsMinimum),
		Changes: changes,
	}, strings.Join(cmds, "\n")
}

// rhclMemoryOffer is the action that raises the Kuadrant operator's memory
// limit, offered only once one of its containers is seen OOMKilled (its
// last or current termination), and a note for the part's warning.
func rhclMemoryOffer(c *Client, st envkit.OperatorState) (*types.MaaSAction, string) {
	if !st.Installed() {
		return nil, ""
	}
	killed, err := oomKilledContainers(c, st)
	if err != nil || len(killed) == 0 {
		return nil, ""
	}
	note := fmt.Sprintf("The operator ran out of memory (OOMKilled: %s), a known issue of Connectivity Link (CONNLINK-1878).", strings.Join(killed, ", "))
	if why := rhclUpgradeRefusal(st); why != "" {
		return nil, note + " " + why
	}
	containers, err := csvContainers(c, st.CSVNS, st.CSV)
	if err != nil {
		return nil, note
	}
	resources, why := rhclMemoryResources(containers)
	if why != "" {
		return nil, note + " This tool does not raise its memory limit: " + why + "."
	}
	return &types.MaaSAction{
		ID: "rhcl-memory", Label: "Raise the memory limit",
		Summary: "Gives the Connectivity Link operator a memory limit of " + rhclMemoryLimit + " through its Subscription, the way OLM lets a Subscription set an operator's resources (a change to the Deployment itself would be reverted). Its pod restarts once.",
		Changes: []string{fmt.Sprintf("Subscription %s/%s: spec.config.resources %s (the CSV's values, with the memory limit %s); it stays after Clean up.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, resourcesText(resources), rhclMemoryLimit)},
	}, note
}

// rhclDenyOffer is the Apply action for an installed Connectivity Link
// whose Subscription does not deny traffic when Limitador is unavailable
// (platform-setup.md "Rate limiting when Limitador is unavailable"), the
// note for the part, and the docs' patch. Nothing when both are set.
func rhclDenyOffer(st envkit.OperatorState) (*types.MaaSAction, string, string) {
	if !st.Installed() || st.Sub == nil {
		return nil, "", ""
	}
	missing := rhclMissingDenyEnv(st.Sub)
	if len(missing) == 0 {
		return nil, "", ""
	}
	note := fmt.Sprintf("Recommended by the MaaS docs: rate limits \"may not be enforced unless the Kuadrant operator is configured to deny traffic on failure\" when Limitador is unavailable, and Subscription %s/%s does not set %s to deny.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name, strings.Join(missing, " and "))
	cmd := "# The MaaS docs' patch (platform-setup.md, Rate limiting when Limitador is unavailable):\n" + rhclDenyEnvCommand(st.Sub)
	if why := rhclUpgradeRefusal(st); why != "" {
		return nil, note + " " + why, cmd
	}
	return &types.MaaSAction{
		ID: "rhcl-deny-on-failure", Label: "Deny traffic when rate limiting fails",
		Summary: "Sets the two variables the MaaS docs give on the Connectivity Link Subscription, so that requests are refused rather than let through without rate limits while Limitador cannot be reached. OLM rolls the operator out again; its other settings stay.",
		Changes: []string{fmt.Sprintf("Subscription %s/%s: spec.config.env RATELIMIT_CHECK_SERVICE_FAILURE_MODE=deny and RATELIMIT_REPORT_SERVICE_FAILURE_MODE=deny (other entries kept); it stays after Clean up.", st.Sub.Metadata.Namespace, st.Sub.Metadata.Name)},
	}, note, cmd
}

func resourcesText(res map[string]any) string {
	var parts []string
	for _, kind := range []string{"limits", "requests"} {
		values, _ := res[kind].(map[string]any)
		var kv []string
		for _, k := range []string{"cpu", "memory"} {
			if v, ok := values[k]; ok {
				kv = append(kv, fmt.Sprintf("%s %v", k, v))
			}
		}
		if len(kv) > 0 {
			parts = append(parts, kind+" "+strings.Join(kv, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// authorinoTLSAction is the Configure Authorino TLS action.
func authorinoTLSAction(ns, secret, missing string) types.MaaSAction {
	return types.MaaSAction{
		ID: "authorino-tls", Label: "Configure Authorino TLS",
		Summary: "Applies the steps of the MaaS docs' setup-authorino-tls.sh to Connectivity Link's Authorino in " + ns + " (missing: " + missing + "). Only what is not set yet changes; Authorino restarts. It stays after Clean up: RHOAI's inference gateway uses Authorino too.",
		Changes: []string{
			"Service " + ns + "/authorino-authorino-authorization: annotation " + servingCertAnnotation + "=" + secret + " (service-ca makes the certificate)",
			"Authorino " + ns + "/authorino: spec.listener.tls enabled, certSecretRef " + secret,
			"Deployment " + ns + "/authorino: SSL_CERT_FILE and REQUESTS_CA_BUNDLE at the service CA (" + authorinoServiceCAFile + "), unless it trusts the service CA already; then a rollout restart",
		},
	}
}

// tierPoliciesAction is the Remove the old tier policies action.
func tierPoliciesAction(legacy []gatewayPolicy) types.MaaSAction {
	var changes []string
	for _, p := range legacy {
		changes = append(changes, fmt.Sprintf("Deleted: %s %s/%s (targets %s; not managed by maas-controller)", p.Kind, inferenceGatewayNamespace, p.Name, maasGatewayName))
	}
	return types.MaaSAction{
		ID: "remove-tier-policies", Label: "Remove the old tier policies", Danger: true,
		Summary: "Deletes the gateway policies of the tier-based MaaS that RHOAI's upgrade leaves behind, as the MaaS docs' upgrade guide (Phase 4) does by hand. maas-controller's own policies stay. Clients that relied on the old tiers' rules lose them.",
		Changes: changes,
	}
}
