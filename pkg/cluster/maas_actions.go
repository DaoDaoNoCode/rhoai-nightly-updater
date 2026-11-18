package cluster

import (
	"fmt"
	"strings"

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
func operatorPlanPrefix(p *operatorInstallPlan) string {
	var out []string
	if p.nsMissing {
		out = append(out, "Namespace "+p.op.namespace)
	}
	if p.groupMissing {
		out = append(out, "OperatorGroup "+p.op.namespace+"/"+p.op.group)
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
func rhclUpgradeRefusal(st operatorState) string {
	switch {
	case !st.installed():
		return "Connectivity Link is not installed; Set up installs it."
	case st.sub == nil:
		return fmt.Sprintf("Its Subscription in %s is not named as the MaaS docs name it (kuadrant-operator, or rhcl-operator on RHOAI-managed clusters), so this tool does not change it. A cluster-admin switches its channel with the commands below.", st.csvNS)
	case st.sub.argoApp != "":
		return gitOpsRefusal("Subscription "+st.sub.Metadata.Namespace+"/"+st.sub.Metadata.Name, st.sub.argoApp, "changing its channel") + " Change the channel in Git."
	case st.sub.Spec.InstallPlanApproval == "Manual":
		return fmt.Sprintf("Subscription %s/%s approves InstallPlans by hand (Manual), and this tool does not approve someone else's plans. A cluster-admin switches its channel and approves the plans.", st.sub.Metadata.Namespace, st.sub.Metadata.Name)
	case firstNonEmpty(st.sub.Spec.SourceNamespace, CatalogNS) != CatalogNS:
		return fmt.Sprintf("Subscription %s/%s uses catalog %s/%s, outside %s, which this tool does not read.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, st.sub.Spec.SourceNamespace, st.sub.Spec.Source, CatalogNS)
	}
	return ""
}

// rhclUpgradeOffer is the Upgrade Connectivity Link action for an install
// below the docs' minimum, with the commands that do the same by hand; nil
// when this tool cannot do it (the commands, or the reason, say why).
func rhclUpgradeOffer(c *Client, st operatorState) (*types.MaaSAction, string) {
	if why := rhclUpgradeRefusal(st); why != "" {
		if st.sub == nil {
			return nil, fmt.Sprintf("# %s\noc get subscriptions -n %s", why, shellQuote(st.csvNS))
		}
		return nil, "# " + why
	}
	pkg, found, err := readOperatorPackage(c, rhclOperator.pkg, st.sub.Spec.Source)
	if err != nil || !found {
		return nil, ""
	}
	target := rhclTargetChannel(pkg)
	if target == "" {
		return nil, ""
	}
	targetCh, _ := pkg.channel(target)
	changes := []string{fmt.Sprintf("Subscription %s/%s: channel %s to %s (version %s). OLM upgrades the operator; there is no way back to %s.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, firstNonEmpty(st.sub.Spec.Channel, "not set"), target, targetCh.head(), st.version)}
	cmds := []string{rhclChannelCommand(st.sub.Metadata.Namespace, st.sub.Metadata.Name, target)}
	if hop := hopChannel(pkg, st.csv, target); hop != "" && !targetCh.has(st.csv) {
		hopCh, _ := pkg.channel(hop)
		changes = append(changes, fmt.Sprintf("If OLM has no path from %s straight to %s (it then stays at %s), channel %s first, to %s, then %s.", st.version, targetCh.head(), st.version, hop, hopCh.head(), target))
		cmds = []string{"# OLM may have no path from " + st.version + " straight to " + target + ": go through " + hop + " first", rhclChannelCommand(st.sub.Metadata.Namespace, st.sub.Metadata.Name, hop), "# once " + hopCh.CurrentCSV + " is installed:", cmds[0]}
	}
	changes = append(changes, "Afterwards, Authorino TLS is set up again where an upgrade step recreated Authorino's Service (which removes its certificate).")
	return &types.MaaSAction{
		ID: "upgrade-rhcl", Label: "Upgrade Connectivity Link",
		Summary: fmt.Sprintf("Upgrades Red Hat Connectivity Link from %s to %s or later, as the MaaS docs ask with RHOAI 3.6, one channel at a time along the path OLM offers. Gateway authentication and rate limits restart during each step; it takes a few minutes.", st.version, rhclDocsMinimum),
		Changes: changes,
	}, strings.Join(cmds, "\n")
}

// rhclMemoryOffer is the action that raises the Kuadrant operator's memory
// limit, offered only once one of its containers is seen OOMKilled (its
// last or current termination), and a note for the part's warning.
func rhclMemoryOffer(c *Client, st operatorState) (*types.MaaSAction, string) {
	if !st.installed() {
		return nil, ""
	}
	killed, err := oomKilledContainers(c, st)
	if err != nil || len(killed) == 0 {
		return nil, ""
	}
	note := fmt.Sprintf("The operator ran out of memory (OOMKilled: %s), a known issue of Connectivity Link 1.5.0 (CONNLINK-1878).", strings.Join(killed, ", "))
	if why := rhclUpgradeRefusal(st); why != "" {
		return nil, note + " " + why
	}
	containers, err := csvContainers(c, st.csvNS, st.csv)
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
		Changes: []string{fmt.Sprintf("Subscription %s/%s: spec.config.resources %s (the CSV's values, with the memory limit %s); it stays after Remove.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, resourcesText(resources), rhclMemoryLimit)},
	}, note
}

func resourcesText(res map[string]interface{}) string {
	var parts []string
	for _, kind := range []string{"limits", "requests"} {
		values, _ := res[kind].(map[string]interface{})
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
		Summary: "Applies the steps of the MaaS docs' setup-authorino-tls.sh to Connectivity Link's Authorino in " + ns + " (missing: " + missing + "). Only what is not set yet changes; Authorino restarts. It stays after Remove: RHOAI's inference gateway uses Authorino too.",
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
