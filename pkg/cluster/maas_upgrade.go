package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasRHCLHops bounds the channel switches one upgrade makes (a hop and
// the target, with room for one more hop).
const maasRHCLHops = 4

// upgradeRHCL upgrades Connectivity Link to the docs' minimum along the
// path OLM offers, without assuming it: it switches the Subscription to the
// target channel (rhclTargetChannel); if OLM settles at AtLatestKnown with
// an older CSV than the channel's head (it has no upgrade edge from it), it
// switches to a channel that lists the installed CSV and has a newer head
// (hopChannel), waits for that, and switches to the target again. Then it
// sets up Authorino TLS again: authorino-operator recreates Authorino's
// Service during such a step, and service-ca deletes its certificate with
// the old Service (seen live 2026-11-18, RHCL 1.4.3 -> 1.4.4 -> 1.5.0).
// Steps: "check", "upgrade", "authorino-tls", "ready".
func (m *maasRun) upgradeRHCL() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking Connectivity Link and the upgrade path its catalog offers...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	st, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "csv read failed")
	}
	if st.installed() && !rhclBelowMinimum(st.version) {
		m.step("check", "success", fmt.Sprintf("Connectivity Link %s is installed, which meets the docs' minimum (%s).", st.version, rhclDocsMinimum))
		return m.done(fmt.Sprintf("Connectivity Link %s needs no upgrade.", st.version))
	}
	if why := rhclUpgradeRefusal(st); why != "" {
		return m.fail("check", why+" Nothing was changed.", "prerequisites", "upgrade refused")
	}
	pkg, found, err := readOperatorPackage(c, rhclOperator.pkg, st.sub.Spec.Source)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the catalog's channels; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "package read failed")
	}
	if !found {
		return m.fail("check", fmt.Sprintf("Catalog %s does not offer %s. Nothing was changed.", st.sub.Spec.Source, rhclOperator.pkg), "prerequisites", "package missing")
	}
	target := rhclTargetChannel(pkg)
	if target == "" {
		return m.fail("check", fmt.Sprintf("No channel of %s in catalog %s has %s or later. Nothing was changed.", rhclOperator.pkg, st.sub.Spec.Source, rhclDocsMinimum), "prerequisites", "no target channel")
	}
	targetCh, _ := pkg.channel(target)
	if err := patchSubscription(c, st.sub, map[string]any{"channel": target}, true); err != nil {
		return m.fail("check", fmt.Sprintf("The API server rejects the channel change of Subscription %s/%s: %v. Nothing was changed.%s", st.sub.Metadata.Namespace, st.sub.Metadata.Name, err, templateHint(err)), maasWriteCode(err), "dry run failed")
	}
	ns, name := st.sub.Metadata.Namespace, st.sub.Metadata.Name
	m.step("check", "success", fmt.Sprintf("Upgrading from %s (channel %s) to channel %s (%s).", st.version, firstNonEmpty(st.sub.Spec.Channel, "not set"), target, targetCh.head()))

	want := target
	hopped := ""
	for i := 0; ; i++ {
		if i >= maasRHCLHops {
			return m.fail("upgrade", fmt.Sprintf("Stopped after %d channel switches without reaching %s. A cluster-admin checks Subscription %s/%s.", maasRHCLHops, targetCh.CurrentCSV, ns, name), "not_ready", "too many hops")
		}
		wantCh, _ := pkg.channel(want)
		sub, err := readOperatorSubscription(c, ns, name)
		if err != nil || sub == nil {
			return m.fail("upgrade", fmt.Sprintf("Cannot read Subscription %s/%s: %v.", ns, name, err), errorCodeFromK8sErr(err), "subscription read failed")
		}
		if sub.Spec.Channel != want {
			m.step("upgrade", "running", fmt.Sprintf("Switching Subscription %s/%s to channel %s...", ns, name, want))
			if err := patchSubscription(c, sub, map[string]any{"channel": want}, false); err != nil {
				return m.fail("upgrade", fmt.Sprintf("Failed to switch Subscription %s/%s to channel %s: %v. Run the upgrade again to retry.", ns, name, want, err), maasWriteCode(err), "channel patch failed")
			}
			m.logs = append(m.logs, fmt.Sprintf("OK: Subscription %s/%s channel %s", ns, name, want))
		}
		reached, p, err := m.waitForChannelHead(ns, name, wantCh.CurrentCSV)
		if err != nil {
			return m.fail("upgrade", fmt.Sprintf("Stopped waiting for OLM: %v. Channel %s is set; run the upgrade again to continue.", err, want), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "upgrade wait failed")
		}
		if p.failed != "" {
			return m.fail("upgrade", fmt.Sprintf("OLM does not upgrade Connectivity Link on channel %s: %s. A cluster-admin checks Subscription %s/%s.", want, p.failed, ns, name), "not_ready", "upgrade failed")
		}
		if reached {
			if want == target {
				m.step("upgrade", "success", fmt.Sprintf("Connectivity Link %s is installed (channel %s%s).", csvVersion(p.installedCSV), target, hopNote(hopped)))
				break
			}
			m.step("upgrade", "running", fmt.Sprintf("%s is installed; switching to channel %s now.", p.installedCSV, target))
			want = target
			continue
		}
		if !p.done {
			return m.fail("upgrade", fmt.Sprintf("OLM has not finished after %s (%s). Run the upgrade again to continue.", clockText(MaaSOperatorTimeout), firstNonEmpty(p.text, "no progress")), "not_ready", "upgrade not done")
		}
		// AtLatestKnown below the head: OLM has no edge from here on this
		// channel.
		hop := hopChannel(pkg, p.installedCSV, target)
		if want != target || hop == "" {
			return m.fail("upgrade", fmt.Sprintf("OLM stays at %s on channel %s and the catalog lists no channel that leads on from it. A cluster-admin checks the upgrade path (oc get packagemanifest %s -n %s -o yaml). Subscription %s/%s is on channel %s.", p.installedCSV, want, rhclOperator.pkg, CatalogNS, ns, name, want), "not_ready", "no upgrade path")
		}
		m.step("upgrade", "running", fmt.Sprintf("OLM has no path from %s straight to %s; going through channel %s first.", p.installedCSV, targetCh.CurrentCSV, hop))
		hopped, want = hop, hop
	}

	// Authorino's certificate, which an upgrade step may have removed.
	kq, err := readKuadrant(c)
	if err != nil || !kq.found {
		m.step("authorino-tls", "success", "No Kuadrant CR to check Authorino TLS on.")
	} else if r, err := m.applyAuthorinoTLS(&rec, kq.namespace, "authorino-tls"); r != nil || err != nil {
		return r, err
	}

	m.step("ready", "running", "Checking the operator's pods...")
	st, err = readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Cannot read the installed operators: %v. The upgrade itself is done.", err), errorCodeFromK8sErr(err), "csv read failed")
	}
	note := ""
	if action, n := rhclMemoryOffer(c, st); n != "" {
		note = " " + n
		if action != nil {
			note += " Raise the memory limit on the MaaS tab fixes it."
		}
	}
	m.step("ready", "success", fmt.Sprintf("Connectivity Link %s runs.%s", st.version, note))
	return m.done(fmt.Sprintf("Connectivity Link is upgraded to %s.%s", st.version, note))
}

func hopNote(hop string) string {
	if hop == "" {
		return ""
	}
	return ", through " + hop
}

// waitForChannelHead waits until OLM has installed head (reached), or the
// Subscription has stayed settled at another CSV for
// MaaSOperatorResolveGrace (OLM has no edge to head: AtLatestKnown), or
// failed for that long.
func (m *maasRun) waitForChannelHead(ns, name, head string) (bool, subscriptionProgress, error) {
	var last subscriptionProgress
	var settledSince, failedSince time.Time
	reached, failedLong := false, false
	_, err := waitForGone(m.c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		p, err := readSubscriptionProgress(cc, ns, name)
		if err != nil {
			return false, err
		}
		if p.text != last.text {
			m.step("upgrade", "running", p.text)
		}
		last = p
		switch {
		case p.done && p.installedCSV == head:
			reached = true
			return true, nil
		case p.failed != "":
			if failedSince.IsZero() {
				failedSince = clk.Now()
			}
			failedLong = clk.Since(failedSince) >= MaaSOperatorResolveGrace
			return failedLong, nil
		case p.done:
			failedSince = time.Time{}
			if settledSince.IsZero() {
				settledSince = clk.Now()
			}
			return clk.Since(settledSince) >= MaaSOperatorResolveGrace, nil
		}
		failedSince, settledSince = time.Time{}, time.Time{}
		return false, nil
	})
	if !failedLong {
		last.failed = "" // a passing resolution failure (a catalog refreshing)
	}
	return reached, last, err
}

// rhclMemory raises the Kuadrant operator's memory limit through its
// Subscription (spec.config.resources, the way OLM lets a Subscription set
// an operator's resources; its Deployment is the operator's and a patch to
// it is reverted). Only after one of its containers was seen OOMKilled,
// only on request (seen live 2026-11-18 on RHCL 1.5.0: three OOM kills at
// the CSV's 500Mi; 1Gi held; CONNLINK-1878). Steps: "check", "memory",
// "ready".
func (m *maasRun) rhclMemory() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the operator's memory...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	st, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "csv read failed")
	}
	if why := rhclUpgradeRefusal(st); why != "" {
		return m.fail("check", why+" Nothing was changed.", "prerequisites", "memory refused")
	}
	killed, err := oomKilledContainers(c, st)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the operator's pods, so this tool cannot tell whether it ran out of memory; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "pods read failed")
	}
	if len(killed) == 0 {
		return m.fail("check", "No container of the Connectivity Link operator was OOMKilled, so its memory limit stays as its CSV sets it. Nothing was changed.", "prerequisites", "not oomkilled")
	}
	containers, err := csvContainers(c, st.csvNS, st.csv)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read CSV %s/%s; nothing was changed: %v.", st.csvNS, st.csv, err), errorCodeFromK8sErr(err), "csv read failed")
	}
	resources, why := rhclMemoryResources(containers)
	if why != "" {
		return m.fail("check", "This tool does not raise the operator's memory limit: "+why+". Nothing was changed.", "prerequisites", "memory not offered")
	}
	spec := map[string]any{"config": map[string]any{"resources": resources}}
	if err := patchSubscription(c, st.sub, spec, true); err != nil {
		return m.fail("check", fmt.Sprintf("The API server rejects the change of Subscription %s/%s: %v. Nothing was changed.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, err), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", "OOMKilled: "+strings.Join(killed, ", ")+".")

	m.step("memory", "running", fmt.Sprintf("Setting spec.config.resources on Subscription %s/%s (%s)...", st.sub.Metadata.Namespace, st.sub.Metadata.Name, resourcesText(resources)))
	rec.setShared("rhcl-memory", fmt.Sprintf("Subscription %s/%s spec.config.resources (memory limit %s)", st.sub.Metadata.Namespace, st.sub.Metadata.Name, rhclMemoryLimit))
	if err := writeMaaSRecord(c, rec); err != nil {
		return m.fail("memory", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
	}
	if err := patchSubscription(c, st.sub, spec, false); err != nil {
		return m.fail("memory", fmt.Sprintf("Failed to change Subscription %s/%s: %v.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, err), maasWriteCode(err), "subscription patch failed")
	}
	m.step("memory", "success", "OLM rolls the operator out with a memory limit of "+rhclMemoryLimit+".")
	m.step("ready", "running", "Waiting for OLM...")
	p, err := waitForSubscription(c, st.sub.Metadata.Namespace, st.sub.Metadata.Name, func(s string) { m.step("ready", "running", s) })
	if err != nil || !p.done {
		m.step("ready", "success", "OLM has not reported the operator ready yet; the MaaS tab shows its state.")
		return m.done("The operator's memory limit is " + rhclMemoryLimit + "; OLM is still rolling it out.")
	}
	m.step("ready", "success", "The operator runs with a memory limit of "+rhclMemoryLimit+".")
	return m.done("The Connectivity Link operator's memory limit is " + rhclMemoryLimit + ".")
}

// rhclDenyOnFailure sets the docs' deny-on-failure env on an adopted
// Connectivity Link Subscription (one this tool creates has it already),
// keeping its other env entries; the merge patch carries the read
// resourceVersion. Steps: "check", "env", "ready".
func (m *maasRun) rhclDenyOnFailure() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking Connectivity Link's Subscription...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	st, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "csv read failed")
	}
	if why := rhclUpgradeRefusal(st); why != "" {
		return m.fail("check", why+" Nothing was changed.", "prerequisites", "env refused")
	}
	if len(rhclMissingDenyEnv(st.sub)) == 0 {
		m.step("check", "success", "The Subscription already denies traffic when rate limiting fails.")
		return m.done("Nothing to change: both variables are set to deny.")
	}
	spec := map[string]any{"config": map[string]any{"env": rhclDenyEnvPatch(st.sub)}}
	if err := patchSubscription(c, st.sub, spec, true); err != nil {
		return m.fail("check", fmt.Sprintf("The API server rejects the change of Subscription %s/%s: %v. Nothing was changed.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, err), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", fmt.Sprintf("Subscription %s/%s can be changed.", st.sub.Metadata.Namespace, st.sub.Metadata.Name))
	m.step("env", "running", "Setting RATELIMIT_CHECK_SERVICE_FAILURE_MODE and RATELIMIT_REPORT_SERVICE_FAILURE_MODE to deny...")
	rec.setShared("rhcl-deny-on-failure", fmt.Sprintf("Subscription %s/%s spec.config.env deny on failure", st.sub.Metadata.Namespace, st.sub.Metadata.Name))
	if err := writeMaaSRecord(c, rec); err != nil {
		return m.fail("env", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
	}
	if err := patchSubscription(c, st.sub, spec, false); err != nil {
		return m.fail("env", fmt.Sprintf("Failed to change Subscription %s/%s: %v.", st.sub.Metadata.Namespace, st.sub.Metadata.Name, err), maasWriteCode(err), "subscription patch failed")
	}
	m.step("env", "success", "The Subscription denies traffic when rate limiting fails.")
	m.step("ready", "running", "Waiting for OLM...")
	p, err := waitForSubscription(c, st.sub.Metadata.Namespace, st.sub.Metadata.Name, func(s string) { m.step("ready", "running", s) })
	if err != nil || !p.done {
		m.step("ready", "success", "OLM has not reported the operator ready yet; the MaaS tab shows its state.")
		return m.done("Connectivity Link denies traffic when rate limiting fails; OLM is still rolling it out.")
	}
	m.step("ready", "success", "The operator runs with the new setting.")
	return m.done("Connectivity Link denies traffic when rate limiting fails.")
}

// authorinoTLSOnly configures Authorino TLS on its own (after a Connectivity
// Link upgrade outside this tool, for example). Steps: "check",
// "authorino-tls".
func (m *maasRun) authorinoTLSOnly() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking Kuadrant's Authorino...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	kq, err := readKuadrant(c)
	switch {
	case err != nil:
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "kuadrant read failed")
	case !kq.found:
		return m.fail("check", "There is no Kuadrant CR, so there is no Authorino to configure. Set up creates it. Nothing was changed.", "prerequisites", "no kuadrant")
	}
	if _, err := configureAuthorinoTLS(c, kq.namespace, true); err != nil {
		return m.fail("check", fmt.Sprintf("Authorino TLS cannot be configured: %v. Nothing was changed.%s", err, templateHint(err)), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", "Authorino "+kq.namespace+"/authorino can be configured.")
	if r, err := m.applyAuthorinoTLS(&rec, kq.namespace, "authorino-tls"); r != nil || err != nil {
		return r, err
	}
	return m.done("Authorino TLS is configured in " + kq.namespace + ".")
}

// removeTierPolicies deletes the documented gateway policies of the
// tier-based MaaS (legacyTierPolicy), each by name with its UID as a
// precondition, after a server-side dry run of every delete; policies
// maas-controller manages, and any other, stay. Steps: "check", "delete".
func (m *maasRun) removeTierPolicies() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Listing the policies on the MaaS gateway...")
	policies, err := maasGatewayPolicies(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot list the policies on %s; nothing was deleted: %v.%s", maasGatewayName, err, templateHint(err)), errorCodeFromK8sErr(err), "policy list failed")
	}
	var legacy []gatewayPolicy
	for _, p := range policies {
		if legacyTierPolicy(p) {
			legacy = append(legacy, p)
		}
	}
	if len(legacy) == 0 {
		m.step("check", "success", "No tier-era policy is on the gateway.")
		return m.done("No old tier policy is left; nothing was deleted.")
	}
	for _, p := range legacy {
		path := legacyTierPolicyPath(p.Kind, p.Version)
		opts, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": p.UID}})
		if _, _, err := c.Do(http.MethodDelete, path, "application/json", opts, url.Values{"dryRun": {"All"}}); err != nil && !isK8sNotFound(err) {
			return m.fail("check", fmt.Sprintf("The API server rejects deleting %s %s: %v. Nothing was deleted.%s", p.Kind, p.Name, err, templateHint(err)), maasWriteCode(err), "dry run failed")
		}
	}
	m.step("check", "success", "To delete: "+strings.Join(policyNames(legacy), ", ")+".")
	m.step("delete", "running", "Deleting the old tier policies...")
	for _, p := range legacy {
		path := legacyTierPolicyPath(p.Kind, p.Version)
		if _, err := deleteWithUID(c, path, p.UID); err != nil && !isK8sNotFound(err) {
			return m.fail("delete", fmt.Sprintf("Failed to delete %s %s: %v. Run it again to retry.", p.Kind, p.Name, err), maasWriteCode(err), "delete failed")
		}
		m.logs = append(m.logs, fmt.Sprintf("OK: %s %s/%s deleted", p.Kind, inferenceGatewayNamespace, p.Name))
	}
	m.step("delete", "success", "Deleted "+strings.Join(policyNames(legacy), " and ")+"; maas-controller's own policies stay.")
	return m.done("The old tier policies are deleted: " + strings.Join(policyNames(legacy), ", ") + ".")
}
