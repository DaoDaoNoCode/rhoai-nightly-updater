package cluster

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Change limit and Change who can use them: edits of this tool's
// MaaSSubscription (and MaaSAuthPolicy) in place, each checked by a dry run
// before it is written. API keys stay valid: they are bound to the
// subscription by name, which does not change.

// maasPublished reads what both changes need: the record, the tool's
// subscription (which must be this tool's and exist), and the publications
// it lists. It returns a refusal message and code instead.
func maasPublished(c *Client) (maasToolSub, []maasPub, string, string) {
	rec, err := readMaaSRecord(c)
	if err != nil {
		return maasToolSub{}, nil, err.Error() + " Nothing was changed.", errorCodeFromK8sErr(err)
	}
	cur, err := readMaaSToolSubscription(c)
	if err != nil {
		return cur, nil, err.Error() + " Nothing was changed.", errorCodeFromK8sErr(err)
	}
	switch {
	case !cur.found:
		return cur, nil, "No model of this tool is published, so there is no subscription to change; Publish sets it. Nothing was changed.", "prerequisites"
	case !cur.owned:
		return cur, nil, fmt.Sprintf("MaaSSubscription %s/%s was not created by this tool, so it is left alone. Nothing was changed.", maasTenantNamespace, maasSubscriptionName), "not_managed"
	}
	live, err := maasLivePubs(c, rec, maasPub{}, maasPub{})
	if err != nil {
		return cur, nil, fmt.Sprintf("Cannot check the publications: %v. Nothing was changed.", err), errorCodeFromK8sErr(err)
	}
	if len(live) == 0 {
		return cur, nil, "No model of this tool is published now. Nothing was changed.", "prerequisites"
	}
	return cur, live, "", ""
}

// changeLimit sets the token limit of the published model name in project.
// Steps: "check", "limit", "ready".
func (m *maasRun) changeLimit(project, name string, req *types.MaaSLimit) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the publication and the subscription...")
	limit, problem := parseMaaSLimit(req)
	if problem != "" {
		return m.fail("check", "Invalid limit: "+problem+". Nothing was changed.", "validation", "invalid limit")
	}
	cur, live, msg, code := maasPublished(c)
	if msg != "" {
		return m.fail("check", msg, code, "subscription check failed")
	}
	pub := maasPub{project, name}
	listed := false
	for _, p := range live {
		listed = listed || p == pub
	}
	if !listed {
		return m.fail("check", fmt.Sprintf("%s in %s is not published by this tool, so it has no limit to change. Nothing was changed.", name, project), "prerequisites", "not published")
	}
	before := cur.limitOf(pub)
	if before.equal(limit) {
		m.step("check", "success", fmt.Sprintf("%s in %s has that limit already: %s.", name, project, limit.text()))
		return m.done(fmt.Sprintf("%s in %s already has %s; nothing changed.", name, project, limit.text()))
	}
	limits := map[string]maasLimit{}
	for _, p := range live {
		limits[p.key()] = cur.limitOf(p)
	}
	limits[pub.key()] = limit
	sub, _ := maasAccessObjects(c, live, limits, cur.audience, maasOwnSubscriptionPriority(c))
	desc := "MaaSSubscription " + maasSubscriptionName
	if err := applyMaaSObject(c, desc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, true); err != nil {
		if why := unlimitedRefusal(err, limit); why != "" {
			return m.fail("check", why+" Nothing was changed.", "prerequisites", "unlimited not served")
		}
		return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", desc, err), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", fmt.Sprintf("%s in %s can change from %s to %s.", name, project, before.text(), limit.text()))
	m.step("limit", "running", "Changing the model's entry in the subscription...")
	if err := applyMaaSObject(c, desc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, false); err != nil {
		return m.fail("limit", fmt.Sprintf("Failed to update %s: %v. The limit is unchanged; run it again to retry.", desc, err), maasWriteCode(err), "apply failed")
	}
	m.step("limit", "success", fmt.Sprintf("%s lists %s in %s with %s.", desc, name, project, limit.text()))
	return m.waitAccessActive(fmt.Sprintf("%s in %s now has %s.", name, project, limit.text()), false)
}

// changeAccess sets who may use the published models: the subscription's
// owner and the authorization policy's subjects. Steps: "check", "access",
// "ready".
func (m *maasRun) changeAccess(req *types.MaaSAudience) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the subscription and the authorization policy...")
	aud, problem := parseMaaSAudience(req)
	if problem != "" {
		return m.fail("check", "Invalid audience: "+problem+". Nothing was changed.", "validation", "invalid audience")
	}
	cur, live, msg, code := maasPublished(c)
	if msg != "" {
		return m.fail("check", msg, code, "subscription check failed")
	}
	authMeta, authFound, err := readObjectMeta(c, maasAccessPath(c, "MaaSAuthPolicy"))
	switch {
	case err != nil:
		return m.fail("check", fmt.Sprintf("Cannot read MaaSAuthPolicy %s: %v. Nothing was changed.", maasAuthPolicyName, err), errorCodeFromK8sErr(err), "read failed")
	case authFound && !toolOwned(authMeta):
		return m.fail("check", fmt.Sprintf("MaaSAuthPolicy %s/%s was not created by this tool, so it is left alone. Nothing was changed.", maasTenantNamespace, maasAuthPolicyName), "not_managed", "auth foreign")
	}
	if cur.audience.equal(aud) && authFound {
		m.step("check", "success", "The published models are for "+aud.text()+" already.")
		return m.done("The published models are for " + aud.text() + " already; nothing changed.")
	}
	limits := map[string]maasLimit{}
	for _, p := range live {
		limits[p.key()] = cur.limitOf(p)
	}
	sub, auth := maasAccessObjects(c, live, limits, aud, maasOwnSubscriptionPriority(c))
	if !aud.everyone {
		for _, p := range maasGroupProblems(c, aud.groups) {
			m.step("check", "running", p.Message)
		}
	}
	subDesc, authDesc := "MaaSSubscription "+maasSubscriptionName, "MaaSAuthPolicy "+maasAuthPolicyName
	if err := applyMaaSObject(c, subDesc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, true); err != nil {
		return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", subDesc, err), maasWriteCode(err), "dry run failed")
	}
	if err := applyMaaSObject(c, authDesc, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth, true); err != nil {
		return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", authDesc, err), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", fmt.Sprintf("Who can use the published models can change from %s to %s.", cur.audience.text(), aud.text()))
	m.step("access", "running", "Changing the subscription's owners and the authorization policy's subjects...")
	if err := applyMaaSObject(c, subDesc, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub, false); err != nil {
		return m.fail("access", fmt.Sprintf("Failed to update %s: %v. Nothing was changed.", subDesc, err), maasWriteCode(err), "apply failed")
	}
	if err := applyMaaSObject(c, authDesc, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth, false); err != nil {
		return m.fail("access", fmt.Sprintf("Failed to update %s: %v. %s was changed already; run it again to finish.", authDesc, err, subDesc), maasWriteCode(err), "apply failed")
	}
	m.logs = append(m.logs, "OK: "+subDesc, "OK: "+authDesc)
	m.step("access", "success", fmt.Sprintf("The subscription and the authorization policy are for %s. API keys of anyone else stop working for these models.", aud.text()))
	return m.waitAccessActive("The published models are for "+aud.text()+" now.", true)
}

// waitAccessActive is the "ready" step: until maas-controller reports the
// subscription (and the authorization policy) Active again.
func (m *maasRun) waitAccessActive(doneMsg string, withAuth bool) (*types.OperationResponse, error) {
	c := m.c
	m.step("ready", "running", "Waiting for MaaS to apply it...")
	last := ""
	ready, err := waitForGone(c, MaaSPublishTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		s := maasPhase(cc, maasAccessPath(c, "MaaSSubscription"))
		a := "Active"
		if withAuth {
			a = maasPhase(cc, maasAccessPath(c, "MaaSAuthPolicy"))
		}
		msg := fmt.Sprintf("subscription: %s; access: %s", firstNonEmpty(s, "pending"), firstNonEmpty(a, "pending"))
		if msg != last {
			last = msg
			m.step("ready", "running", strings.ToUpper(msg[:1])+msg[1:])
		}
		return s == "Active" && a == "Active", nil
	})
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Stopped waiting: %v. The change is written; the MaaS tab shows its state.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "wait failed")
	}
	if !ready {
		m.step("ready", "success", fmt.Sprintf("Not active after %s (%s). The MaaS tab shows its state.", clockText(MaaSPublishTimeout), last))
		return m.done(doneMsg + " MaaS is still applying it.")
	}
	m.step("ready", "success", "MaaS applies it.")
	return m.done(doneMsg)
}
