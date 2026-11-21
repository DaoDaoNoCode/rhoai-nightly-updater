package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The model's gateway refs while it is published: the MaaS gateway alone.
//
// maas-controller makes TokenRateLimitPolicy maas-trlp-<model> targeting
// the model's HTTPRoute (models-as-a-service maassubscription_controller.go,
// identical on main 68b70af1 and red-hat-data-services rhoai-3.6 d8df8ca1),
// with the predicate auth.identity.selected_subscription_id == "<id>"
// (ratetier.go). Kuadrant enforces a route-targeted policy on every gateway
// in the route's parentRefs (kuadrant-operator
// doc/overviews/rate-limiting.md) and has no way to scope it to one parent.
// On the inference gateway the identity comes from a Kubernetes TokenReview
// (AuthPolicy openshift-ai-inference-authn) and has no
// selected_subscription_id, so the wasm-shim fails closed: live (RHOAI 3.6,
// 2026-11-18), every direct call answered 500 while the route also named
// that gateway. So publish writes refs [maas-default-gateway], as the docs
// intend ("Only models routing through maas-default-gateway appear in the
// MaaS catalog and have policies applied", model-setup.md). The model is
// then used through MaaS with an API key and through the dashboard's
// playground (which reaches it on the MaaS gateway's internal listener),
// and its direct address answers 404 until Unpublish writes back the refs
// publish recorded (maasRecord.ModelRefs).
//
// spec.router.gateway.refs is an atomic list (KServe rhoai-3.6
// GatewaySpec.Refs has no list-map markers), so any write replaces the
// whole list: a server-side apply would take the list over. The list is
// written whole with a JSON merge patch that carries the read
// resourceVersion (a model changed in between is a conflict, never
// overwritten), as field manager rhoai-nightly-updater-maas. No refs is
// written as refs: null, which removes them, back to gateway {} (live, an
// empty server-side apply instead left gateway with no owner, so the API
// server nulled it and the CRD refused the result: 422
// "spec.router.gateway ... must be of type object: null").
//
// KServe rewrites a model's HTTPRoute only when the parentRefs it expects
// are not a prefix of the route's (router.go semanticHTTPRouteIsEqual:
// equality.Semantic.DeepDerivative compares a slice element by element up
// to the expected length; opendatahub kserve rhoai-3.6). A parent dropped
// from the end of the list therefore stays: live, refs going from
// [MaaS, inference] (a publication by an earlier build of this tool) to
// [MaaS] left the route on both gateways and the model not Ready ("bound
// to 2 parents"). Publish then first writes back the refs that publication
// replaced, waits for the route to leave the MaaS gateway, and only then
// writes the MaaS gateway alone; from there, and from the default, KServe
// rewrites the route (live, in about 6 s). Every change waits for the
// route (waitForModelRoute).

// gatewayRefs are a model's spec.router.gateway.refs as read (KServe
// UntypedObjectReference: name, namespace), kept whole.
type gatewayRefs []map[string]any

func gatewayRefName(r map[string]any) (string, string) {
	name, _ := r["name"].(string)
	ns, _ := r["namespace"].(string)
	return name, ns
}

func (g gatewayRefs) hasMaaS() bool {
	for _, r := range g {
		if isMaaSGatewayRef(gatewayRefName(r)) {
			return true
		}
	}
	return false
}

// maasOnly: the refs publish writes, the MaaS gateway alone.
func (g gatewayRefs) maasOnly() bool {
	return len(g) == 1 && g.hasMaaS()
}

// String names the refs; none is KServe's default gateway.
func (g gatewayRefs) String() string {
	if len(g) == 0 {
		return "KServe's default gateway (no refs)"
	}
	names := make([]string, 0, len(g))
	for _, r := range g {
		name, ns := gatewayRefName(r)
		names = append(names, ns+"/"+name)
	}
	return strings.Join(names, ", ")
}

// earlierBuildOriginal are the refs a publication by an earlier build of
// this tool replaced: it wrote [MaaS gateway, the model's refs], with the
// inference gateway written out when the model had none.
func (g gatewayRefs) earlierBuildOriginal() gatewayRefs {
	var others gatewayRefs
	for _, r := range g {
		if !isMaaSGatewayRef(gatewayRefName(r)) {
			others = append(others, r)
		}
	}
	if len(others) == 1 {
		if name, ns := gatewayRefName(others[0]); name == inferenceGatewayName && ns == inferenceGatewayNamespace {
			return nil
		}
	}
	return others
}

// modelGatewayRefsOf are the gateway refs of a model as read.
func modelGatewayRefsOf(m llmisvcObject) gatewayRefs {
	if m.Spec.Router == nil || m.Spec.Router.Gateway == nil {
		return nil
	}
	var out gatewayRefs
	for _, r := range m.Spec.Router.Gateway.Refs {
		ref := map[string]any{"name": r.Name}
		if r.Namespace != "" {
			ref["namespace"] = r.Namespace
		}
		out = append(out, ref)
	}
	return out
}

// specHasRoute: the model's spec (as read whole) has spec.router.route, so
// KServe makes an HTTPRoute for it; without it the model has none.
func specHasRoute(spec map[string]any) bool {
	router, _ := spec["router"].(map[string]any)
	_, ok := router["route"].(map[string]any)
	return ok
}

// routeBefore is what the tool's model name in project had before it was
// published, which Unpublish puts back: what publish recorded, or, with no
// record, no route for a model on the MaaS gateway alone whose publication
// is not recorded either. That is a model the Models tab set up through
// MaaS only whose publish stopped before it recorded anything (it is
// created on the MaaS gateway alone); a publication by an earlier build is
// always recorded as published. false: nothing is known (an earlier
// build's publication, or not published).
func (r maasRecord) routeBefore(project, name string, refs gatewayRefs) (maasModelRefs, bool) {
	if rec, ok := r.modelRefs(project, name); ok {
		return rec, true
	}
	if !r.published(project, name) && refs.maasOnly() {
		return maasModelRefs{NoRoute: true}, true
	}
	return maasModelRefs{}, false
}

func maasOnlyRefs() gatewayRefs {
	return gatewayRefs{{"name": maasGatewayName, "namespace": inferenceGatewayNamespace}}
}

// readModelGatewayRefs reads the gateway refs of the tool's model name in
// project and its resourceVersion.
func readModelGatewayRefs(c *Client, project, name string) (gatewayRefs, string, error) {
	refs, meta, _, err := readModelGatewayRefsSpec(c, project, name)
	return refs, meta.ResourceVersion, err
}

// readModelGatewayRefsSpec is readModelGatewayRefs with the model's
// metadata and its whole spec from the same read.
func readModelGatewayRefsSpec(c *Client, project, name string) (gatewayRefs, objectMeta, map[string]any, error) {
	path, _ := toolModelPath(project, name)
	body, _, err := c.Get(path)
	if err != nil {
		return nil, objectMeta{}, nil, err
	}
	var model struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Router struct {
				Gateway struct {
					Refs gatewayRefs `json:"refs"`
				} `json:"gateway"`
			} `json:"router"`
		} `json:"spec"`
	}
	var raw struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(body, &model); err != nil {
		return nil, objectMeta{}, nil, fmt.Errorf("parse %s in %s: %w", name, project, err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, objectMeta{}, nil, fmt.Errorf("parse %s in %s: %w", name, project, err)
	}
	return model.Spec.Router.Gateway.Refs, model.Metadata, raw.Spec, nil
}

// writeModelGatewayRefs writes the whole refs list of the tool's model name
// in project (none: removed, KServe's default) if the model is still at
// resourceVersion rv.
func writeModelGatewayRefs(c *Client, project, name, rv string, refs gatewayRefs) error {
	path, _ := toolModelPath(project, name)
	var list any
	if len(refs) > 0 {
		list = refs
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": rv},
		"spec":     map[string]any{"router": map[string]any{"gateway": map[string]any{"refs": list}}},
	})
	_, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, url.Values{"fieldManager": {maasModelManager}})
	return err
}

// modelRoutePath is the HTTPRoute KServe makes for a chat model of the
// catalog (the ones publish routes through the MaaS gateway):
// kmeta.ChildName(<model>, "-kserve-route") (router.go expectedHTTPRoute),
// which is the plain concatenation while it fits 63 characters. Only those
// names, written out so the RBAC rule (resourceNames) stays limited to them
// and is checked against them (rbac_coverage_test.go); the embedding model
// is never published, so its route is not read.
func modelRoutePath(project, name string) (string, bool) {
	switch name {
	case "smollm2-135m-instruct":
		return namespacedPath("gateway.networking.k8s.io/v1", "httproutes", project, "smollm2-135m-instruct-kserve-route"), true
	case "qwen25-05b-instruct":
		return namespacedPath("gateway.networking.k8s.io/v1", "httproutes", project, "qwen25-05b-instruct-kserve-route"), true
	case "qwen3-1-7b":
		return namespacedPath("gateway.networking.k8s.io/v1", "httproutes", project, "qwen3-1-7b-kserve-route"), true
	}
	return "", false
}

// readModelRoute reads the parents ("namespace/name") of the HTTPRoute of
// the tool's model name in project; found is false when KServe has not
// made it.
func readModelRoute(c *Client, project, name string) ([]string, bool, error) {
	path, _ := modelRoutePath(project, name)
	var route struct {
		Spec struct {
			ParentRefs []struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"parentRefs"`
		} `json:"spec"`
	}
	found, err := c.GetIntoOptional(path, &route)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, false, fmt.Errorf("parse HTTPRoute %s/%s-kserve-route: %w", project, name, err)
		}
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	parents := make([]string, 0, len(route.Spec.ParentRefs))
	for _, p := range route.Spec.ParentRefs {
		parents = append(parents, firstNonEmpty(p.Namespace, project)+"/"+p.Name)
	}
	return parents, true, nil
}

// waitForModelRoute waits, up to MaaSRouteTimeout, until KServe has moved
// the HTTPRoute of the tool's model name in project: onto the MaaS gateway alone with the model
// Ready at its generation (published), or off the MaaS gateway (not). It
// returns "" or, in words, what is still not so.
func waitForModelRoute(c *Client, project, name string, published bool) (string, error) {
	maas := inferenceGatewayNamespace + "/" + maasGatewayName
	route := fmt.Sprintf("HTTPRoute %s/%s-kserve-route", project, name)
	why, stop := "", false
	done, err := waitForGone(c, MaaSRouteTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		parents, found, err := readModelRoute(cc, project, name)
		if err != nil {
			return false, err
		}
		if !published {
			if found && slices.Contains(parents, maas) {
				why = fmt.Sprintf("the model's route (%s) still goes through %s", route, strings.Join(parents, " and "))
				return false, nil
			}
			return true, nil
		}
		switch {
		case !found:
			why = fmt.Sprintf("KServe has not made the model's route (%s)", route)
			return false, nil
		case len(parents) != 1 || parents[0] != maas:
			why = fmt.Sprintf("the model's route (%s) goes through %s, not %s alone", route, strings.Join(parents, " and "), maasGatewayName)
			return false, nil
		}
		model, found, err := readToolModel(cc, project, name)
		switch {
		case err != nil:
			return false, err
		case !found:
			why, stop = "the model was deleted", true
			return true, nil
		case model.Status.ObservedGeneration < model.Metadata.Generation:
			why = "KServe has not applied the change to the model yet"
			return false, nil
		}
		if cond, ok := findCondition(model.Status.Conditions, "Ready"); !ok || cond.Status != "True" {
			why = "the model is not Ready"
			if ok {
				why += " (" + firstNonEmpty(truncateText(cond.Message, 300), cond.Reason) + ")"
			}
			return false, nil
		}
		return true, nil
	})
	switch {
	case err != nil:
		return "", err
	case stop:
		return why, nil
	case !done:
		return fmt.Sprintf("%s after %s", why, clockText(MaaSRouteTimeout)), nil
	}
	return "", nil
}

// maasUseWords say how a published model is used.
const maasUseWords = "While it is published, the model is used through MaaS (with an API key) and the dashboard's playground; its direct address answers 404 until it is unpublished."

// routeModelThroughMaaS is publish's "model" step: the model's refs become
// the MaaS gateway alone (through original first when a publication by an
// earlier build left [MaaS, others]), then it waits for the route and the
// model. It returns a failure message and code, or "".
func (m *maasRun) routeModelThroughMaaS(project, name string, original maasModelRefs, expect modelSpecExpectation) (string, string) {
	c := m.c
	m.step("model", "running", "Routing the model through the MaaS gateway alone...")
	// Checked on the read whose resourceVersion guards the write: a spec
	// someone else writes after it makes the write fail (409).
	refs, meta, spec, err := readModelGatewayRefsSpec(c, project, name)
	if err != nil {
		return fmt.Sprintf("Cannot read %s in %s: %v. Unpublish to undo.", name, project, err), errorCodeFromK8sErr(err)
	}
	if d := expect.differences(spec); len(d) > 0 {
		return specChangedRefusal(name, project, d, "Unpublish to undo what was done."), "conflict"
	}
	rv := meta.ResourceVersion
	if refs.hasMaaS() && !refs.maasOnly() {
		m.step("model", "running", fmt.Sprintf("The model routes through %s, as an earlier build of this tool published it. KServe does not take a gateway off the end of a model's route, so the model first goes back to %s...", refs, original.Refs))
		if err := writeModelGatewayRefs(c, project, name, rv, original.Refs); err != nil {
			return fmt.Sprintf("Failed to set the model's gateways back to %s: %v. Unpublish to undo.", original.Refs, err), maasWriteCode(err)
		}
		why, err := waitForModelRoute(c, project, name, false)
		if err != nil {
			return fmt.Sprintf("Stopped waiting for the model's route: %v. Unpublish to undo.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled")
		}
		if why != "" {
			return fmt.Sprintf("Partly published: %s. The model reference, subscription and authorization policy were not made. Unpublish to undo.", why), "partial_failure"
		}
		if refs, meta, spec, err = readModelGatewayRefsSpec(c, project, name); err != nil {
			return fmt.Sprintf("Cannot read %s in %s: %v. Unpublish to undo.", name, project, err), errorCodeFromK8sErr(err)
		}
		if d := expect.differences(spec); len(d) > 0 {
			return specChangedRefusal(name, project, d, "Unpublish to undo what was done."), "conflict"
		}
		rv = meta.ResourceVersion
	}
	switch {
	case !specHasRoute(spec):
		// No route (set up "Only inside the cluster", or unpublished after
		// a setup through MaaS only): KServe makes an HTTPRoute only for a
		// spec.router.route, so the router is written whole, as the "Through
		// MaaS only" exposure writes it.
		if err := writeModelRouter(c, project, name, rv, exposurePlan{exposure: exposureMaaS, gwNS: inferenceGatewayNamespace, gwName: maasGatewayName}); err != nil {
			return fmt.Sprintf("Failed to give the model a route through the MaaS gateway: %v. Unpublish to undo.", err), maasWriteCode(err)
		}
	case !refs.maasOnly():
		if err := writeModelGatewayRefs(c, project, name, rv, maasOnlyRefs()); err != nil {
			return fmt.Sprintf("Failed to route the model through the MaaS gateway: %v. Unpublish to undo.", err), maasWriteCode(err)
		}
	}
	m.step("model", "running", "Waiting for KServe to move the model's route to the MaaS gateway...")
	why, err := waitForModelRoute(c, project, name, true)
	if err != nil {
		return fmt.Sprintf("Stopped waiting for the model's route: %v. Unpublish to undo.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled")
	}
	if why != "" {
		return fmt.Sprintf("Partly published: %s. The model reference, subscription and authorization policy were not made. Unpublish to undo.", why), "partial_failure"
	}
	m.step("model", "success", "The model routes through "+maasGatewayName+" alone. "+maasUseWords)
	return "", ""
}

// restoreModelRoute is unpublish's "model" step on the tool's model: the
// refs publish recorded are written back (KServe's default when none were
// recorded, or what a publication by an earlier build replaced; no route
// for a model set up through MaaS only), unless someone added a gateway
// meanwhile; then it waits for the route to leave the MaaS gateway. It
// returns a failure message and code, or "".
func (m *maasRun) restoreModelRoute(project, name string, recorded maasModelRefs, hasRecord bool, say func(id, status, msg string)) (string, string) {
	c := m.c
	refs, meta, spec, err := readModelGatewayRefsSpec(c, project, name)
	if err != nil {
		return fmt.Sprintf("Cannot read %s in %s: %v. Unpublish again to retry.", name, project, err), errorCodeFromK8sErr(err)
	}
	rv := meta.ResourceVersion
	target, write := refs, refs.hasMaaS()
	switch {
	case hasRecord && recorded.NoRoute && !specHasRoute(spec):
		// An earlier try already removed the route: only the wait is left.
		return m.removeModelRoute(project, name, "", say)
	case !write:
		// No MaaS gateway on it (taken off by hand, or by an earlier try):
		// its refs stay as they are.
	case hasRecord && !refs.maasOnly():
		return refsChangedRefusal(project, name, refs, recorded, "Then Unpublish again: it leaves the model's gateways as you set them."), "conflict"
	case hasRecord && recorded.NoRoute:
		return m.removeModelRoute(project, name, rv, say)
	case hasRecord:
		target = recorded.Refs
	case refs.maasOnly():
		target = nil
	default:
		target = refs.earlierBuildOriginal()
	}
	if write {
		if err := writeModelGatewayRefs(c, project, name, rv, target); err != nil {
			return fmt.Sprintf("Failed to set the model's gateways back to %s: %v. Unpublish again to retry.", target, err), maasWriteCode(err)
		}
	}
	say("model", "running", "Waiting for KServe to move the model's route off the MaaS gateway...")
	why, err := waitForModelRoute(c, project, name, false)
	if err != nil {
		return fmt.Sprintf("Stopped waiting for the model's route: %v. Unpublish again to check.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled")
	}
	if why != "" {
		return fmt.Sprintf("Partly published: %s. The model is no longer in MaaS; Unpublish again to check the route.", why), "partial_failure"
	}
	say("model", "success", fmt.Sprintf("The model routes as before publishing: %s.", target))
	return "", ""
}

// removeModelRoute is restoreModelRoute for a model the Models tab set up
// to be reached through MaaS only (its record says NoRoute): it had no
// route before publishing, so it goes back to none. The model gets the spec
// the "Only inside the cluster" exposure writes: no spec.router, and token
// authentication as that exposure decides it (internalAuthOff). Never
// KServe's default gateway, which nobody chose for it. rv is the
// resourceVersion read with the refs (MaaS gateway alone); "" when the
// route is already gone (an earlier try removed it), so nothing is
// written. It returns a failure message and code, or "".
func (m *maasRun) removeModelRoute(project, name, rv string, say func(id, status, msg string)) (string, string) {
	c := m.c
	internal := exposurePlan{exposure: exposureInternal}
	if rv != "" {
		auth, _, authErr := authReadiness(c)
		internal.authOff = internalAuthOff(auth, authErr)
		if err := writeModelRouter(c, project, name, rv, internal); err != nil {
			return fmt.Sprintf("Failed to remove the model's route: %v. Unpublish again to retry.", err), maasWriteCode(err)
		}
	}
	say("model", "running", "Waiting for KServe to remove the model's route...")
	why, err := waitForModelRoute(c, project, name, false)
	if err != nil {
		return fmt.Sprintf("Stopped waiting for the model's route: %v. Unpublish again to check.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled")
	}
	if why != "" {
		return fmt.Sprintf("Partly published: %s. The model is no longer in MaaS; Unpublish again to check the route.", why), "partial_failure"
	}
	msg := "The model has no outside route now, as before it was published: it answers only at its in-cluster address."
	if internal.authOff {
		msg += " Its token authentication is off (security.opendatahub.io/enable-auth: \"false\"), as for any model reached only inside the cluster while Red Hat Connectivity Link is not installed."
	}
	say("model", "success", msg)
	return "", ""
}

// writeModelRouter writes the spec.router of exposure exp on the tool's
// model name in project (none for "Only inside the cluster": the router is
// removed, as modelManifest leaves it out), with the enable-auth annotation
// that exposure writes, if the model is still at resourceVersion rv.
func writeModelRouter(c *Client, project, name, rv string, exp exposurePlan) error {
	path, _ := toolModelPath(project, name)
	meta := map[string]any{"resourceVersion": rv}
	if exp.authOff {
		meta["annotations"] = map[string]any{enableAuthAnnot: "false"}
	}
	var router any
	if r := exp.router(); r != nil {
		router = r
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": meta,
		"spec":     map[string]any{"router": router},
	})
	_, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, url.Values{"fieldManager": {maasModelManager}})
	return err
}

// directRouteNote says, after Unpublish put the model's route back, that
// its direct address will not answer because the inference gateway does
// not allow the project ("" when it does, or cannot tell): someone took
// the project off the gateway's list while the model was published, or a
// pre-release build's gateway chose projects otherwise. Unpublish does not
// change the inference gateway; for this tool's gateway the note points to
// Allow on the Models tab, for someone else's to RHOAI's documented step.
func directRouteNote(c *Client, project, name string) string {
	model, found, err := readToolModel(c, project, name)
	if err != nil || !found {
		return ""
	}
	def, _ := readKServeGateway(c)
	if !model.routesThroughInferenceGateway(def) {
		return ""
	}
	gw, found, err := readInferenceGateway(c)
	if err != nil || !found {
		return ""
	}
	labels, msg, _ := projectNamespace(c, project)
	if msg != "" || gw.admits(project, labels) != "no" {
		return ""
	}
	if gatewayOwned(gw.Metadata) {
		return fmt.Sprintf(" Its direct address does not answer yet: the inference gateway does not list %s among the projects it allows. Allow %s on the Models tab (a cluster-admin).", project, project)
	}
	return fmt.Sprintf(" Its direct address does not answer: Gateway %s/%s, which this tool did not create, does not let routes from %s attach; a cluster admin adds %s to the values array of its listeners' allowedRoutes, as RHOAI documents it.", inferenceGatewayNamespace, inferenceGatewayName, project, project)
}

// refsChangedRefusal is why Unpublish (or Publish again) does not write
// back the refs publish recorded: someone added a gateway to the model
// while it was published, and writing the recorded list would drop it.
// then says what the operation does once the user has set the refs.
func refsChangedRefusal(project, name string, refs gatewayRefs, recorded maasModelRefs, then string) string {
	return fmt.Sprintf("%s in %s routes through %s: someone added a gateway since it was published, which set only %s. Writing back what the model had before, %s, would drop that gateway. Nothing was changed. %s. %s",
		name, project, refs, maasGatewayName, recorded, setRefsYourself(project, name), then)
}

// String names what the model had before it was published.
func (r maasModelRefs) String() string {
	if r.NoRoute {
		return "no route"
	}
	return r.Refs.String()
}

// setRefsYourself is the remedy for a gateway someone added to a published
// model: the user sets the model's refs without the MaaS gateway, after
// which Unpublish leaves them as set (a sentence without its full stop).
func setRefsYourself(project, name string) string {
	return fmt.Sprintf("Set the gateways the model should use yourself, without %s (for KServe's default gateway: oc patch llminferenceservice %s -n %s --type=merge -p '{\"spec\":{\"router\":{\"gateway\":{\"refs\":null}}}}')",
		maasGatewayName, name, project)
}

// refsAddedStatus is the MaaS tab's word for a model this build published
// (its refs are recorded) that routes through the MaaS gateway and others
// since: someone added them, and Unpublish and Publish refuse
// (refsChangedRefusal) until the user sets the refs. The rate limit fails
// calls on the added gateways once Kuadrant applies it there (live: the
// direct address still answered 200 for about 30 s, then 500).
func refsAddedStatus(project, name string, refs []string) string {
	var added []string
	for _, r := range refs {
		if r != inferenceGatewayNamespace+"/"+maasGatewayName {
			added = append(added, r)
		}
	}
	that, it := "that gateway", "it"
	if len(added) > 1 {
		that, it = "those gateways", "them"
	}
	return fmt.Sprintf("Partly published: someone added %s since the model was published, so direct calls to the model through %s fail with 500 once the rate limit applies to %s. Unpublish and Publish refuse, since writing back the gateways the model had before would drop %s. %s, then Unpublish.",
		strings.Join(added, " and "), that, it, it, setRefsYourself(project, name))
}
