package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Change limit and Change who can use them: edits of this tool's
// MaaSSubscription (and MaaSAuthPolicy) in place, each checked by a dry run
// before it is written. API keys stay valid: they are bound to the
// subscription by name, which does not change.

// maasPublished reads what both changes need: the record, the tool's
// subscription (which must be this tool's and exist), and the publications
// it lists. It returns a refusal message and code instead.
func maasPublished(c *kube.Client) (maasToolSub, []MaaSPub, string, string) {
	rec, err := ReadMaaSRecord(c)
	if err != nil {
		return maasToolSub{}, nil, err.Error() + " Nothing was changed.", kube.ErrorCodeFromK8sErr(err)
	}
	cur, err := readMaaSToolSubscription(c)
	if err != nil {
		return cur, nil, err.Error() + " Nothing was changed.", kube.ErrorCodeFromK8sErr(err)
	}
	switch {
	case !cur.found:
		return cur, nil, "No model of this tool is published, so there is no subscription to change; Publish sets it. Nothing was changed.", "prerequisites"
	case !cur.owned:
		return cur, nil, fmt.Sprintf("MaaSSubscription %s/%s was not created by this tool, so it is left alone. Nothing was changed.", MaaSTenantNamespace, MaaSSubscriptionName), "not_managed"
	}
	live, err := maasLivePubs(c, rec, MaaSPub{}, MaaSPub{})
	if err != nil {
		return cur, nil, fmt.Sprintf("Cannot check the publications: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err)
	}
	if len(live) == 0 {
		return cur, nil, "No model of this tool is published now. Nothing was changed.", "prerequisites"
	}
	return cur, live, "", ""
}

// changeLimit sets the token limit of the published model name in project.
// Steps: "check", "limit", "ready".
func (m *Run) changeLimit(project, name string, req *types.MaaSLimit) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("check", "running", "Checking the publication and the subscription")
	limit, problem := ParseMaaSLimit(req)
	if problem != "" {
		return m.Fail("check", "Invalid limit: "+problem+". Nothing was changed.", "validation", "invalid limit")
	}
	cur, live, msg, code := maasPublished(c)
	if msg != "" {
		return m.Fail("check", msg, code, "subscription check failed")
	}
	pub := MaaSPub{project, name}
	listed := false
	for _, p := range live {
		listed = listed || p == pub
	}
	if !listed {
		return m.Fail("check", fmt.Sprintf("%s in %s is not published by this tool, so it has no limit to change. Nothing was changed.", name, project), "prerequisites", "not published")
	}
	before := cur.limitOf(pub)
	if before.equal(limit) {
		m.Step("check", "success", fmt.Sprintf("%s in %s has that limit already: %s.", name, project, limit.Text()))
		return m.Done(fmt.Sprintf("%s in %s already has %s; nothing changed.", name, project, limit.Text()))
	}
	limits := map[string]MaaSLimit{}
	for _, p := range live {
		limits[p.Key()] = cur.limitOf(p)
	}
	limits[pub.Key()] = limit
	sub, _ := maasAccessObjects(c, live, limits, cur.audience)
	desc := "MaaSSubscription " + MaaSSubscriptionName
	if err := applyMaaSObject(c, desc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, true); err != nil {
		if why := UnlimitedRefusal(err, limit); why != "" {
			return m.Fail("check", why+" Nothing was changed.", "prerequisites", "unlimited not served")
		}
		return m.Fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", desc, err), MaaSWriteCode(err), "dry run failed")
	}
	m.Step("check", "success", fmt.Sprintf("%s in %s can change from %s to %s.", name, project, before.Text(), limit.Text()))
	m.Step("limit", "running", "Changing the model's entry in the subscription")
	if err := applyMaaSObject(c, desc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, false); err != nil {
		return m.Fail("limit", fmt.Sprintf("Failed to update %s: %v. The limit is unchanged; run it again to retry.", desc, err), MaaSWriteCode(err), "apply failed")
	}
	m.Step("limit", "success", fmt.Sprintf("%s lists %s in %s with %s.", desc, name, project, limit.Text()))
	return m.waitAccessActive(fmt.Sprintf("%s in %s now has %s.", name, project, limit.Text()), false)
}

// changeAccess sets who may use the published models: the subscription's
// owner and the authorization policy's subjects. Steps: "check", "access",
// "ready".
func (m *Run) changeAccess(req *types.MaaSAudience) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("check", "running", "Checking the subscription and the authorization policy")
	aud, problem := ParseMaaSAudience(req)
	if problem != "" {
		return m.Fail("check", "Invalid audience: "+problem+". Nothing was changed.", "validation", "invalid audience")
	}
	cur, live, msg, code := maasPublished(c)
	if msg != "" {
		return m.Fail("check", msg, code, "subscription check failed")
	}
	authMeta, authFound, err := kube.ReadObjectMeta(c, maasAccessPath(c, "MaaSAuthPolicy"))
	switch {
	case err != nil:
		return m.Fail("check", fmt.Sprintf("Cannot read MaaSAuthPolicy %s: %v. Nothing was changed.", MaaSAuthPolicyName, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	case authFound && !ToolOwned(authMeta):
		return m.Fail("check", fmt.Sprintf("MaaSAuthPolicy %s/%s was not created by this tool, so it is left alone. Nothing was changed.", MaaSTenantNamespace, MaaSAuthPolicyName), "not_managed", "auth foreign")
	}
	if cur.audience.Equal(aud) && authFound {
		m.Step("check", "success", "The published models are for "+aud.Text()+" already.")
		return m.Done("The published models are for " + aud.Text() + " already; nothing changed.")
	}
	limits := map[string]MaaSLimit{}
	for _, p := range live {
		limits[p.Key()] = cur.limitOf(p)
	}
	sub, auth := maasAccessObjects(c, live, limits, aud)
	if !aud.Everyone {
		for _, p := range MaaSGroupProblems(c, aud.Groups) {
			m.Step("check", "running", p.Message)
		}
	}
	subDesc, authDesc := "MaaSSubscription "+MaaSSubscriptionName, "MaaSAuthPolicy "+MaaSAuthPolicyName
	if err := applyMaaSObject(c, subDesc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, true); err != nil {
		return m.Fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", subDesc, err), MaaSWriteCode(err), "dry run failed")
	}
	if err := applyMaaSObject(c, authDesc, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth, true); err != nil {
		return m.Fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", authDesc, err), MaaSWriteCode(err), "dry run failed")
	}
	m.Step("check", "success", fmt.Sprintf("Who can use the published models can change from %s to %s.", cur.audience.Text(), aud.Text()))
	m.Step("access", "running", "Changing the subscription's owners and the authorization policy's subjects")
	if err := applyMaaSObject(c, subDesc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, false); err != nil {
		return m.Fail("access", fmt.Sprintf("Failed to update %s: %v. Nothing was changed.", subDesc, err), MaaSWriteCode(err), "apply failed")
	}
	if err := applyMaaSObject(c, authDesc, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth, false); err != nil {
		return m.Fail("access", fmt.Sprintf("Failed to update %s: %v. %s was changed already; run it again to finish.", authDesc, err, subDesc), MaaSWriteCode(err), "apply failed")
	}
	m.Logs = append(m.Logs, "OK: "+subDesc, "OK: "+authDesc)
	m.Step("access", "success", fmt.Sprintf("The subscription and the authorization policy are for %s. API keys of anyone else stop working for these models.", aud.Text()))
	return m.waitAccessActive("The published models are for "+aud.Text()+" now.", true)
}

// setSubscriptionPriority sets spec.priority of this tool's MaaSSubscription
// to 0 (maasSubscriptionPriority), where an earlier version left 10 or a
// value below the other subscriptions'. Only that field changes, by a merge
// patch that carries the read resourceVersion, after a server-side dry run;
// a subscription this tool did not create is refused. Keys minted before
// keep their subscription: maas-api stores it at mint time. There is
// nothing to wait for: maas-api picks a subscription from its own cache of
// MaaSSubscriptions when a key is minted, so the new priority counts as soon
// as it is written, and maas-controller's gateway policies do not use the
// priority. Steps: "check", "priority".
func (m *Run) setSubscriptionPriority() (*types.OperationResponse, error) {
	c := m.Client
	desc := fmt.Sprintf("MaaSSubscription %s/%s", MaaSTenantNamespace, MaaSSubscriptionName)
	m.Step("check", "running", "Checking this tool's subscription")
	cur, err := readMaaSToolSubscription(c)
	switch {
	case err != nil:
		return m.Fail("check", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "subscription read failed")
	case !cur.found:
		return m.Fail("check", "No model of this tool is published, so there is no subscription to change; Publish creates it at priority 0. Nothing was changed.", "prerequisites", "no subscription")
	case !cur.owned:
		return m.Fail("check", desc+" was not created by this tool, so it is left alone. Nothing was changed.", "not_managed", "subscription foreign")
	case cur.meta.Terminating():
		return m.Fail("check", desc+" is being deleted. Nothing was changed.", "conflict", "subscription deleting")
	case cur.priority == maasSubscriptionPriority:
		m.Step("check", "success", desc+" has priority 0 already.")
		return m.Done(desc + " has priority 0 already; nothing changed.")
	}
	if err := patchMaaSSubscriptionPriority(c, cur.meta.ResourceVersion, true); err != nil {
		return m.Fail("check", fmt.Sprintf("The API server rejects the change of %s: %v. Nothing was changed.", desc, err), MaaSWriteCode(err), "dry run failed")
	}
	m.Step("check", "success", fmt.Sprintf("%s has priority %d; it can change to 0.", desc, cur.priority))
	m.Step("priority", "running", "Setting the subscription's priority to 0")
	if err := patchMaaSSubscriptionPriority(c, cur.meta.ResourceVersion, false); err != nil {
		if kube.IsK8sConflict(err) {
			return m.Fail("priority", desc+" changed since it was read, so it was not changed. Run it again.", "conflict", "subscription changed")
		}
		return m.Fail("priority", fmt.Sprintf("Failed to update %s: %v. Its priority is unchanged; run it again to retry.", desc, err), MaaSWriteCode(err), "patch failed")
	}
	m.Logs = append(m.Logs, fmt.Sprintf("OK: %s spec.priority %d to 0", desc, cur.priority))
	m.Step("priority", "success", desc+" has priority 0; maas-api uses it for the keys minted from now on. API keys made before keep the subscription they were bound to: revoke and make again those meant for another subscription.")
	return m.Done("This tool's subscription has priority 0, the lowest MaaS honours.")
}

// patchMaaSSubscriptionPriority sets spec.priority of this tool's
// MaaSSubscription to 0 by a merge patch, as field manager
// rhoai-nightly-updater, with resourceVersion as a precondition.
func patchMaaSSubscriptionPriority(c *kube.Client, resourceVersion string, dryRun bool) error {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": resourceVersion}, "spec": map[string]any{"priority": maasSubscriptionPriority}})
	if err != nil {
		return err
	}
	query := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	path := rhoai.NamespacedPath(MaaSAPI(c), "maassubscriptions", MaaSTenantNamespace, MaaSSubscriptionName)
	_, _, err = c.Do(http.MethodPatch, path, "application/merge-patch+json", data, query)
	return err
}

// waitAccessActive is the "ready" step: until maas-controller reports the
// subscription (and the authorization policy) Active again.
func (m *Run) waitAccessActive(doneMsg string, withAuth bool) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("ready", "running", "Waiting for MaaS to apply it")
	last := ""
	ready, err := kube.WaitForGone(c, maasPublishTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		s := maasPhase(cc, maasAccessPath(c, "MaaSSubscription"))
		a := "Active"
		if withAuth {
			a = maasPhase(cc, maasAccessPath(c, "MaaSAuthPolicy"))
		}
		msg := fmt.Sprintf("subscription: %s; access: %s", kube.FirstNonEmpty(s, "pending"), kube.FirstNonEmpty(a, "pending"))
		if msg != last {
			last = msg
			m.Step("ready", "running", strings.ToUpper(msg[:1])+msg[1:])
		}
		return s == "Active" && a == "Active", nil
	})
	if err != nil {
		return m.Fail("ready", fmt.Sprintf("Stopped waiting: %v. The change is written; the MaaS tab shows its state.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "wait failed")
	}
	if !ready {
		m.Step("ready", "success", fmt.Sprintf("Not active after %s (%s). The MaaS tab shows its state.", ClockText(maasPublishTimeout), last))
		return m.Done(doneMsg + " MaaS is still applying it.")
	}
	m.Step("ready", "success", "MaaS applies it.")
	return m.Done(doneMsg)
}
