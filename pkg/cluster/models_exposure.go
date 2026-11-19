package cluster

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// How a model is reached (its exposure), chosen per model when it is set
// up. KServe's LLMInferenceService routing (red-hat-data-services/kserve
// rhoai-3.6, pkg/controller/v1alpha2/llmisvc router.go and
// router_preconditions_odh.go):
//   - spec.router {route: {}, gateway: {}}: KServe makes an HTTPRoute on its
//     default gateway, the one inferenceservice-config's
//     ingress.kserveIngressGateway names (openshift-ingress/
//     openshift-ai-inference on RHOAI 3.x). With that gateway missing the
//     model reports HTTPRoutesReady=False (WaitingForGateway).
//   - spec.router {route: {}, gateway: {refs: [...]}}: the HTTPRoute is on
//     those gateways, any gateway whose listeners admit the project; a ref
//     to a missing gateway is GatewaysReady=False (RefsInvalid).
//   - no spec.router.route: no HTTPRoute at all; the model answers only at
//     its cluster-internal address (status.addresses,
//     https://<name>-kserve-workload-svc.<ns>.svc.cluster.local).
//
// With token authentication on (the default unless the model carries
// security.opendatahub.io/enable-auth: "false"), KServe does not make the
// model Ready while Kuadrant's AuthPolicy API is missing
// (GatewayPreconditionNotMet, router_preconditions_odh.go:51-76), with or
// without a route. odh-model-controller adds AuthPolicy <gateway>-authn to
// KServe's default gateway and to any gateway a model references, unless the
// gateway is labelled opendatahub.io/managed=false or is owned by a
// GatewayConfig (the operator's data-science-gateway, which admits only
// openshift-ingress and redhat-ods-applications). maas-default-gateway is
// managed=false: MaaS's own controller authenticates its calls.
//
// The RHOAI operator does not create the default gateway; Red Hat's
// odh-gitops chart (rhai-on-openshift-chart) does, and so can this tool
// (inference_gateway.go), as its own explicit part: a model setup never
// creates it. The dashboard deploys with {route: {}, gateway: {}}, and its
// MaaS option writes refs [maas-default-gateway] alone
// (odh-dashboard rhoai-3.6 maasDeploymentTransformer.ts).
const (
	exposureDefault  = "default-gateway"
	exposureMaaS     = "maas"
	exposureGateway  = "gateway"
	exposureInternal = "internal"
	// dataScienceGatewayName is the RHOAI operator's gateway for its own
	// UIs (GatewayConfig default-gateway), never one for models.
	dataScienceGatewayName = "data-science-gateway"
	gatewayConfigKind      = "GatewayConfig"
	odhManagedLabel        = "opendatahub.io/managed"
)

// gatewayNameRegex is a Gateway's name: a DNS subdomain.
var gatewayNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// ValidExposure reports whether e is an exposure a setup request may name.
func ValidExposure(e string) bool {
	switch e {
	case "", exposureDefault, exposureMaaS, exposureGateway, exposureInternal:
		return true
	}
	return false
}

// listAllGateways lists the Gateways of every namespace (read-only; the
// list is the one otherGatewaysOfClass reads).
func listAllGateways(c *Client) ([]gatewayObject, error) {
	body, _, err := c.get(clusterPath("gateway.networking.k8s.io/v1", "gateways", ""))
	if IsK8sError(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the Gateways: %w", err)
	}
	var list struct {
		Items []gatewayObject `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse the Gateways: %w", err)
	}
	return list.Items, nil
}

func gatewayKey(ns, name string) string { return ns + "/" + name }

func findGateway(gws []gatewayObject, ns, name string) (gatewayObject, bool) {
	for _, g := range gws {
		if g.Metadata.Namespace == ns && g.Metadata.Name == name {
			return g, true
		}
	}
	return gatewayObject{}, false
}

// gatewayExclusion says why a gateway is never offered for a model's refs
// ("" when it may be): MaaS's gateway (the MaaS option routes through it,
// and mixing it with another gateway on one model makes direct calls fail
// with 500, maas_routing.go), and the operator's gateway for its own UIs.
func gatewayExclusion(g gatewayObject) string {
	if g.Metadata.Namespace == inferenceGatewayNamespace && g.Metadata.Name == maasGatewayName {
		return "MaaS's gateway (choose Through MaaS)"
	}
	if g.Metadata.Name == dataScienceGatewayName {
		return "the RHOAI operator's gateway for its own pages"
	}
	for _, o := range g.Metadata.OwnerReferences {
		if o.Kind == gatewayConfigKind {
			return "managed by the RHOAI operator's GatewayConfig"
		}
	}
	return ""
}

// gatewayProblem says why a gateway cannot carry a model now ("" when it
// is accepted and programmed).
func gatewayProblem(g gatewayObject) string {
	if g.Metadata.terminating() {
		return "it is being deleted"
	}
	if cond, ok := findCondition(g.Status.Conditions, "Accepted"); ok && cond.Status == "False" {
		return "its gateway controller does not accept it (" + firstNonEmpty(truncateMessage(cond.Message), cond.Reason) + ")"
	}
	cond, ok := findCondition(g.Status.Conditions, "Programmed")
	switch {
	case !ok:
		return "it does not report Programmed yet"
	case cond.Status != "True":
		return "it is not programmed (" + firstNonEmpty(truncateMessage(cond.Message), cond.Reason, "no reason given") + ")"
	}
	return ""
}

// gatewayAddress is where a model routed through g answers: its external
// listener's hostname, then /<project>/<model> (KServe's path), or "".
func gatewayAddress(g gatewayObject, project, model string) string {
	if host := g.externalHost(); host != "" {
		return fmt.Sprintf("https://%s/%s/%s", host, project, model)
	}
	return ""
}

// internalAddress is the cluster-internal address KServe gives a model
// without a route (status.addresses).
// modelInClusterURL is the model's own in-cluster address among those
// KServe reports (status.addresses). RHOAI 3.6 lists, for a model on a
// gateway, the gateway's external and internal routing bases (".../")
// and the model's paths on each ("/<project>/<model>" and
// "/publishers/<project>/models/<model>"); a model with no route has its
// own Service. The first in-cluster address is the internal routing base,
// which is not the model's (seen live), so the address is chosen by what
// it points at: an in-cluster address of the model's own path, else the
// model's own Service; "" when KServe reports neither.
func modelInClusterURL(m llmisvcObject) string {
	ns, name := m.Metadata.Namespace, m.Metadata.Name
	service := ""
	for _, a := range m.Status.Addresses {
		if !snippetURL(a.URL) {
			continue
		}
		u, err := url.Parse(a.URL)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if !strings.HasSuffix(host, ".svc") && !strings.HasSuffix(host, ".svc.cluster.local") {
			continue
		}
		switch path := strings.Trim(u.Path, "/"); {
		case path == ns+"/"+name:
			return a.URL
		case path == "" && service == "" && strings.HasPrefix(host, name+"-") && strings.Contains(host, "."+ns+".svc"):
			service = a.URL
		}
	}
	return service
}

// withServicePort completes an in-cluster address that names a Service with
// no port. KServe reports the model's own Service as "https://<svc>.<ns>.svc
// .cluster.local" while the Service listens on another port (8000 on RHOAI
// 3.6, seen live: the address as reported does not connect), so the port is
// read from the Service: its https port (by appProtocol or name), or its
// only port. An address with a port or a path (a gateway's) is returned as
// it is. "" when the Service cannot be read or names no https port: no
// address is better than one that does not connect.
func withServicePort(c *Client, address string) string {
	u, err := url.Parse(address)
	if address == "" || err != nil || u.Port() != "" || strings.Trim(u.Path, "/") != "" {
		return address
	}
	host := strings.TrimSuffix(u.Hostname(), ".cluster.local")
	labels := strings.Split(host, ".")
	if len(labels) != 3 || labels[2] != "svc" {
		return address
	}
	body, _, err := c.get(namespacedPath("v1", "services", labels[1], labels[0]))
	if err != nil {
		return ""
	}
	var svc struct {
		Spec struct {
			Ports []struct {
				Name        string `json:"name"`
				AppProtocol string `json:"appProtocol"`
				Port        int    `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &svc) != nil {
		return ""
	}
	port := 0
	for _, p := range svc.Spec.Ports {
		if strings.EqualFold(p.AppProtocol, "https") || strings.EqualFold(p.Name, "https") {
			port = p.Port
			break
		}
	}
	if port == 0 && len(svc.Spec.Ports) == 1 {
		port = svc.Spec.Ports[0].Port
	}
	switch port {
	case 0:
		return ""
	case 443:
		return address
	}
	u.Host = fmt.Sprintf("%s:%d", u.Hostname(), port)
	return u.String()
}

func internalAddress(project, model string) string {
	return fmt.Sprintf("https://%s-kserve-workload-svc.%s.svc.cluster.local", model, project)
}

// foreignGatewayAdmitReason says that someone else's gateway does not let
// project's routes attach, and the step its owner takes.
func foreignGatewayAdmitReason(g gatewayObject, project string) string {
	return fmt.Sprintf("Gateway %s, which this tool did not create, does not let routes from project %s attach (its listeners' allowedRoutes), and this tool does not change it. Its owner (a cluster admin) adds %s; for a gateway that lists projects by name, as RHOAI documents it, that is the values array of allowedRoutes.namespaces.selector (%s In [...]).",
		gatewayKey(g.Metadata.Namespace, g.Metadata.Name), project, project, namespaceNameLabel)
}

// authReadiness reads whether calls through a gateway can be authenticated:
// Connectivity Link's AuthPolicy API is served (KServe's precondition) and
// its Kuadrant CR exists (which enforces the policies). It returns a
// reason when not, in words.
func authReadiness(c *Client) (bool, string, error) {
	served, err := groupServed(c, kuadrantGroup)
	if err != nil {
		return false, "", err
	}
	if !served {
		return false, "Red Hat Connectivity Link (Kuadrant) is not installed. It authenticates the calls on a gateway, and KServe does not start a model with token authentication until its AuthPolicy API exists.", nil
	}
	k, err := readKuadrant(c)
	if err != nil {
		return false, "", err
	}
	if !k.found {
		return false, "Red Hat Connectivity Link is installed, but it has no Kuadrant CR, so the gateway's AuthPolicy is not enforced.", nil
	}
	return true, "", nil
}

// exposurePlan is the exposure a model setup writes, decided by the checks
// in planExposures.
type exposurePlan struct {
	exposure string
	gwNS     string
	gwName   string
	// toolGateway: the gateway is this tool's inference gateway: the
	// project is added to its list before the model is created.
	toolGateway bool
	// authOff: write security.opendatahub.io/enable-auth "false"
	// (internal only, and only while Connectivity Link is missing).
	authOff bool
	// needsAuth: Connectivity Link must be set up first.
	needsAuth bool
	// authUncertain: whether RHOAI checks the callers' tokens on the
	// gateway is not known: it is labelled opendatahub.io/managed=false
	// (odh-model-controller adds no AuthPolicy), or it is outside
	// openshift-ingress, where this tool cannot read AuthPolicies. Such a
	// gateway is offered, never recommended.
	authUncertain bool
}

// router is the model's spec.router for the plan (nil: no router, the
// model is internal only).
func (e exposurePlan) router() map[string]interface{} {
	switch e.exposure {
	case exposureInternal:
		return nil
	case exposureMaaS, exposureGateway:
		return map[string]interface{}{"route": map[string]interface{}{}, "gateway": map[string]interface{}{
			"refs": []interface{}{map[string]interface{}{"name": e.gwName, "namespace": e.gwNS}},
		}}
	}
	// No scheduler: one replica needs no endpoint picker.
	return map[string]interface{}{"route": map[string]interface{}{}, "gateway": map[string]interface{}{}}
}

// exposureReads are what the options are computed from.
type exposureReads struct {
	project string
	labels  map[string]string
	entry   catalogModel
	def     kserveGateway
	// defErr: KServe's configuration cannot be read (def is not known).
	defErr  error
	gws     []gatewayObject
	gwsErr  error
	auth    bool
	authWhy string
	authErr error
	// maas is GetMaaSStatus's verdict: "" ready, else why not.
	maas      string
	maasURL   string
	maasKnown bool
}

func readExposureInputs(c *Client, project string, labels map[string]string, entry catalogModel, needMaaS bool) exposureReads {
	r := exposureReads{project: project, labels: labels, entry: entry}
	// KServe's default gateway is used only when its configuration names
	// it: an unreadable or unset one is never taken to be a gateway (a
	// model routed by default could land elsewhere, and its project would
	// be added to a list it does not need).
	r.def, r.defErr = readKServeGateway(c)
	r.gws, r.gwsErr = listAllGateways(c)
	r.auth, r.authWhy, r.authErr = authReadiness(c)
	if needMaaS && entry.Type == modelTypeChat {
		r.maasKnown = true
		st, err := GetMaaSStatus(c)
		switch {
		case err != nil:
			r.maas = "The MaaS platform's state cannot be read: " + err.Error()
		case st.Blocked != nil:
			r.maas = st.Blocked.Reason
		case !st.Platform.Ready && !st.Platform.Deployed:
			r.maas = "MaaS isn't set up yet."
			for _, p := range st.Parts {
				if p.State == "missing" {
					r.maas = fmt.Sprintf("MaaS isn't set up yet (%s is missing).", p.Title)
					break
				}
			}
		case !st.Platform.Ready:
			r.maas = maasNotReadyReason(st)
		default:
			r.maasURL = st.URL
		}
	}
	return r
}

// gatewayOption evaluates a gateway (KServe's default, or one a model
// refers to) for the project.
func (r exposureReads) gatewayOption(opt *types.ModelExposureOption, plan *exposurePlan, g gatewayObject) {
	ns, name := g.Metadata.Namespace, g.Metadata.Name
	plan.gwNS, plan.gwName = ns, name
	opt.Address = gatewayAddress(g, r.project, r.entry.Name)
	if why := gatewayProblem(g); why != "" {
		opt.Reason = fmt.Sprintf("Gateway %s cannot carry a model now: %s.", gatewayKey(ns, name), why)
		return
	}
	if ns == inferenceGatewayNamespace && name == inferenceGatewayName && gatewayOwned(g.Metadata) {
		plan.toolGateway = true
		if g.admits(r.project, r.labels) != "yes" {
			opt.Changes = append(opt.Changes, fmt.Sprintf("Gateway %s (this tool's): %s is added to the projects it allows. Anyone who can edit %s can then attach routes to this shared gateway.", gatewayKey(ns, name), r.project, r.project))
		}
	} else {
		switch g.admits(r.project, r.labels) {
		case "no":
			opt.Reason = foreignGatewayAdmitReason(g, r.project)
			return
		case "unknown":
			opt.Warning = fmt.Sprintf("Gateway %s chooses the projects it admits with expressions this tool does not evaluate: if the model does not become ready, check that it admits %s.", gatewayKey(ns, name), r.project)
		}
	}
	switch {
	case g.Metadata.Labels[odhManagedLabel] == "false":
		plan.authUncertain = true
		opt.Warning = strings.TrimSpace(opt.Warning + fmt.Sprintf(" Gateway %s is labelled %s=false, so RHOAI does not add its token authentication: whoever owns the gateway decides who may call.", gatewayKey(ns, name), odhManagedLabel))
	case ns != inferenceGatewayNamespace:
		// RHOAI adds its AuthPolicy there too, but this tool reads
		// AuthPolicies only in openshift-ingress, so it cannot tell.
		plan.authUncertain = true
	}
	opt.Available = true
	if !r.auth {
		opt.NeedsAuth, plan.needsAuth = true, true
	}
}

// planExposures computes the options for a model of the catalog in
// project, and the recommended one. needMaaS reads the MaaS platform (the
// MaaS option); without it that option is unknown.
func planExposures(c *Client, project string, labels map[string]string, entry catalogModel, needMaaS bool) (*types.ModelExposures, map[string]exposurePlan) {
	r := readExposureInputs(c, project, labels, entry, needMaaS)
	return r.options()
}

func (r exposureReads) options() (*types.ModelExposures, map[string]exposurePlan) {
	out := &types.ModelExposures{Project: r.project, Model: r.entry.Name, KServeDefaultGateway: gatewayKey(r.def.Namespace, r.def.Name), AuthReady: r.auth}
	plans := map[string]exposurePlan{}
	authWords := r.authWhy
	if r.authErr != nil {
		authWords = "Whether Red Hat Connectivity Link is installed cannot be read: " + r.authErr.Error()
	}

	// (a) KServe's default gateway.
	defKey := gatewayKey(r.def.Namespace, r.def.Name)
	a := types.ModelExposureOption{Key: exposureDefault, Exposure: exposureDefault,
		Title: "Through the cluster's default inference gateway"}
	ap := exposurePlan{exposure: exposureDefault}
	switch {
	case r.defErr != nil:
		a.Reason = fmt.Sprintf("KServe's default gateway cannot be read (%v), so a model cannot be routed through it. Choose another way.", r.defErr)
	case !r.def.Known:
		a.Reason = "KServe's configuration names no default gateway, so a model cannot be routed through it. Choose another way."
	case r.gwsErr != nil:
		a.Reason = fmt.Sprintf("The gateways cannot be read, so whether %s exists is unknown: %v.", defKey, r.gwsErr)
	default:
		if g, ok := findGateway(r.gws, r.def.Namespace, r.def.Name); ok {
			r.gatewayOption(&a, &ap, g)
		} else if r.def.isInferenceGateway() {
			a.Reason = "The inference gateway isn't set up yet. Set it up below, or choose another way."
			a.Fix = "inference-gateway"
		} else {
			a.Reason = fmt.Sprintf("Gateway %s, which KServe's configuration names as its default, does not exist. Its owner creates it, or choose another way.", defKey)
		}
	}
	a.Description = fmt.Sprintf("Reachable from outside the cluster at %s. Each call needs an OpenShift token that can see %s.", firstNonEmpty(a.Address, "https://inference.<apps domain>/"+r.project+"/"+r.entry.Name), r.project)
	if a.NeedsAuth {
		a.Warning = strings.TrimSpace(a.Warning + " " + authWords + " Set up installs it first.")
	}
	out.Options = append(out.Options, a)
	plans[a.Key] = ap

	// (b) MaaS only.
	b := types.ModelExposureOption{Key: exposureMaaS, Exposure: exposureMaaS, GatewayRef: gatewayKey(inferenceGatewayNamespace, maasGatewayName),
		Title:       "Through MaaS only",
		Description: "Published to Models as a Service: anyone signed in to the cluster can call it with a MaaS API key, within their subscription's token limit. The dashboard's playground lists it as a MaaS model."}
	bp := exposurePlan{exposure: exposureMaaS, gwNS: inferenceGatewayNamespace, gwName: maasGatewayName}
	switch {
	case r.entry.Type != modelTypeChat:
		b.Reason = "Only chat models are published to MaaS."
	case !r.maasKnown:
		b.Reason = "Not checked."
	case r.maas != "":
		b.Reason = r.maas
		b.Fix = "maas-tab"
	default:
		b.Available = true
		b.Changes = []string{
			fmt.Sprintf("Gateway %s: admits routes from %s.", gatewayKey(inferenceGatewayNamespace, maasGatewayName), r.project),
			fmt.Sprintf("Published to MaaS: MaaSModelRef %s in %s, and this tool's subscription and access policy list the model.", r.entry.Name, r.project),
		}
		if r.maasURL != "" {
			b.Address = r.maasURL
		}
	}
	out.Options = append(out.Options, b)
	plans[b.Key] = bp

	// (c) Another gateway that admits the project.
	var others []types.ModelExposureOption
	if r.gwsErr == nil {
		for _, g := range r.gws {
			ns, name := g.Metadata.Namespace, g.Metadata.Name
			if (r.def.Known && ns == r.def.Namespace && name == r.def.Name) || gatewayExclusion(g) != "" || gatewayProblem(g) != "" {
				continue
			}
			key := exposureGateway + ":" + gatewayKey(ns, name)
			o := types.ModelExposureOption{Key: key, Exposure: exposureGateway, GatewayRef: gatewayKey(ns, name),
				Title:       "Through another gateway: " + gatewayKey(ns, name),
				Description: "Uses a gateway someone else runs; this tool doesn't change it. RHOAI adds token checks."}
			if g.Metadata.Labels[odhManagedLabel] == "false" {
				o.Description = "Uses a gateway someone else runs; this tool doesn't change it. RHOAI adds no token checks to it."
			}
			if ns == inferenceGatewayNamespace && name == inferenceGatewayName && gatewayOwned(g.Metadata) {
				o.Title = "Through the inference gateway, by name: " + gatewayKey(ns, name)
				o.Description = "Names this tool's inference gateway on the model, because KServe's default gateway isn't known. Each call needs an OpenShift token that can see " + r.project + "."
			}
			op := exposurePlan{exposure: exposureGateway}
			r.gatewayOption(&o, &op, g)
			if o.NeedsAuth {
				o.Warning = strings.TrimSpace(o.Warning + " " + authWords + " Set up installs it first.")
			}
			others = append(others, o)
			plans[key] = op
		}
	}
	sort.SliceStable(others, func(i, j int) bool {
		if others[i].Available != others[j].Available {
			return others[i].Available
		}
		return others[i].Key < others[j].Key
	})
	out.Options = append(out.Options, others...)

	// (e) Inside the cluster only.
	e := types.ModelExposureOption{Key: exposureInternal, Exposure: exposureInternal, Available: true,
		Title:       "Only inside the cluster",
		Description: "No outside address. Programs on this cluster (for example a workbench) can call it without a token.",
		// No Address: the port of the model's Service is known only once
		// KServe has made it (withServicePort).
		Warning: "Calls to the in-cluster address are not authenticated: any pod on the cluster whose network can reach the project can use the model."}
	ep := exposurePlan{exposure: exposureInternal}
	if internalAuthOff(r.auth, r.authErr) {
		ep.authOff = true
		e.Warning += " Red Hat Connectivity Link is not installed, so the model's token authentication is turned off (security.opendatahub.io/enable-auth: \"false\"): with it on, KServe would not start the model until Connectivity Link's AuthPolicy API exists."
	}
	out.Options = append(out.Options, e)
	plans[e.Key] = ep

	// The recommendation: never a gateway whose token checks are not known
	// (labelled managed=false, or outside openshift-ingress): those are
	// offered for an explicit choice only.
	switch {
	case a.Available && !ap.authUncertain:
		out.Default = a.Key
		out.DefaultReason = "works from your laptop with an OpenShift token, the way the dashboard deploys models."
	case b.Available:
		out.Default = b.Key
		out.DefaultReason = "MaaS is set up and the inference gateway can't be used: anyone signed in calls the model with a MaaS API key."
	default:
		for _, o := range others {
			if o.Available && !plans[o.Key].authUncertain {
				out.Default = o.Key
				out.DefaultReason = fmt.Sprintf("works from your laptop with an OpenShift token, through gateway %s (KServe's default gateway can't be used).", o.GatewayRef)
				break
			}
		}
	}
	if !a.Available && r.gwsErr == nil {
		if _, found := findGateway(r.gws, inferenceGatewayNamespace, inferenceGatewayName); !found {
			out.RecommendGateway = true
			out.RecommendGatewayReason = "Set up the inference gateway to reach models from outside the cluster with an OpenShift token, as the dashboard does."
		}
	}
	return out, plans
}

// internalAuthOff is the "Only inside the cluster" exposure's choice of
// token authentication: off (security.opendatahub.io/enable-auth "false")
// only while Connectivity Link is known to be missing, since KServe would
// not start the model with it on; on when that cannot be read.
func internalAuthOff(auth bool, authErr error) bool {
	return !auth && authErr == nil
}

// exposureKey is the option key of a request's exposure.
func exposureKey(exposure, ref string) string {
	if exposure == exposureGateway {
		return exposureGateway + ":" + ref
	}
	return exposure
}

// GetModelExposures reports the ways a catalog model can be reached from a
// project (GET /api/resources/models/exposures).
func GetModelExposures(c *Client, project, model string) (*types.ModelExposures, string, string) {
	entry, ok := catalogEntry(model)
	if !ok {
		return nil, "unknown model", "validation"
	}
	labels, msg, code := projectNamespace(c, project)
	if msg != "" {
		return nil, msg, code
	}
	out, _ := planExposures(c, project, labels, entry, true)
	return out, "", ""
}

// onlyConnectivityLinkMissing: KServe's dependency condition names Red Hat
// Connectivity Link (Kuadrant) as missing, and nothing else.
func onlyConnectivityLinkMissing(msg string) bool {
	missing := parseMissingDependencies(msg)
	if len(missing) == 0 {
		return false
	}
	rhcl := map[string]bool{}
	for _, n := range []string{rhclOperator.title, rhclOperator.pkg, rhclOperator.subName, "Kuadrant", "Connectivity Link"} {
		rhcl[normalizeOperatorName(n)] = true
	}
	for _, m := range missing {
		if !rhcl[normalizeOperatorName(m)] {
			return false
		}
	}
	return true
}

// authNeededRefusal is why a setup through a gateway stops while
// Connectivity Link is missing.
func authNeededRefusal(gateway, why string) string {
	return fmt.Sprintf("Calls through gateway %s are authenticated by Red Hat Connectivity Link, which is not set up: %s Set up the Models prerequisites for this choice first (the Models tab's Set up does it), or choose 'Only inside the cluster'. Nothing was changed.", gateway, strings.TrimSpace(why))
}

// planModelExposure decides how the model is reached: for a model that
// exists, how it is reached now (a change is refused: deleting it and
// setting it up again changes it); for a new one, the requested option, or
// the recommended one. gw is the inference gateway set up in the same
// request (Gateway with a project): the model takes KServe's default route
// through it.
func (m *modelsRun) planModelExposure(req types.ModelsRequest, labels map[string]string, entry catalogModel, gw *gatewayPlan, existing llmisvcObject, found bool) (exposurePlan, string, string) {
	c := m.c
	auth := func() (bool, string, string) {
		ok, why, err := authReadiness(c)
		if err != nil {
			return false, fmt.Sprintf("Cannot tell whether Red Hat Connectivity Link is set up, which authenticates the model's calls; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err)
		}
		return ok, why, ""
	}
	if gw != nil && !found {
		def, err := readKServeGateway(c)
		switch {
		case err != nil:
			return exposurePlan{}, fmt.Sprintf("KServe's default gateway cannot be read (%v), so the model may not route through the inference gateway this request sets up. Set the inference gateway up on its own, then choose it by name for the model. Nothing was changed.", err), errorCodeFromK8sErr(err)
		case !def.Known:
			return exposurePlan{}, "KServe's configuration names no default gateway, so the model would not route through the inference gateway this request sets up. Set the inference gateway up on its own, then choose it by name for the model. Nothing was changed.", "prerequisites"
		case !def.isInferenceGateway():
			return exposurePlan{}, fmt.Sprintf("KServe's default gateway is %s, not the inference gateway this request sets up, so the model would not route through it. Set the inference gateway up on its own, then choose 'Through gateway %s' for the model. Nothing was changed.", def, gatewayKey(inferenceGatewayNamespace, inferenceGatewayName)), "prerequisites"
		}
		if gw.foreign && gw.gateway.admits(req.Project, labels) == "no" {
			return exposurePlan{}, foreignGatewayRefusal(req.Project), "prerequisites"
		}
		ok, why, code := auth()
		if code != "" {
			return exposurePlan{}, why, code
		}
		if !ok {
			return exposurePlan{}, authNeededRefusal(gatewayKey(inferenceGatewayNamespace, inferenceGatewayName), why), "prerequisites"
		}
		return exposurePlan{exposure: exposureDefault, gwNS: inferenceGatewayNamespace, gwName: inferenceGatewayName, toolGateway: !gw.foreign}, "", ""
	}
	want := exposureKey(req.Exposure, req.GatewayRef)
	if found {
		def, _ := readKServeGateway(c)
		cur, curGW := modelExposureOf(existing, def)
		curKey := cur
		if cur == exposureGateway {
			curKey = exposureKey(cur, curGW)
		}
		if req.Exposure != "" && want != curKey {
			return exposurePlan{}, fmt.Sprintf("%s in %s is already set up to be reached %s. To reach it another way, delete it and set it up again with that choice. Nothing was changed.", entry.Name, req.Project, exposureWords(exposurePlanOf(cur, curGW))), "validation"
		}
		switch cur {
		case exposureMaaS, exposureGateway:
			// Routed as it is: its gateway is not checked again.
			p := exposurePlanOf(cur, curGW)
			p.toolGateway = curGW == gatewayKey(inferenceGatewayNamespace, inferenceGatewayName)
			return p, "", ""
		case exposureInternal:
			ok, _, code := auth()
			if code != "" {
				ok = existing.Metadata.Annotations[enableAuthAnnot] != "false"
			}
			return exposurePlan{exposure: exposureInternal, authOff: !ok}, "", ""
		}
		want = exposureDefault
	}
	r := readExposureInputs(c, req.Project, labels, entry, want == exposureMaaS || want == "")
	out, plans := r.options()
	if want == "" {
		want = out.Default
		if want == "" {
			why := strings.TrimSpace(out.Options[0].Reason + " " + out.RecommendGatewayReason)
			if why == "" {
				why = "No gateway can carry a model from " + req.Project + " now."
			}
			return exposurePlan{}, why + " Or choose 'Only inside the cluster'. Nothing was changed.", "prerequisites"
		}
		m.step("check", "running", "How it is reached: "+out.DefaultReason)
	}
	var opt *types.ModelExposureOption
	for i := range out.Options {
		if out.Options[i].Key == want {
			opt = &out.Options[i]
		}
	}
	if opt == nil {
		why := fmt.Sprintf("Gateway %s does not exist.", req.GatewayRef)
		if r.gwsErr != nil {
			why = fmt.Sprintf("The gateways cannot be read: %v.", r.gwsErr)
		} else if ns, name, _ := strings.Cut(req.GatewayRef, "/"); ns != "" {
			if g, ok := findGateway(r.gws, ns, name); ok {
				switch {
				case gatewayExclusion(g) != "":
					why = fmt.Sprintf("Gateway %s is %s, so this tool does not route models through it.", req.GatewayRef, gatewayExclusion(g))
				case gatewayProblem(g) != "":
					why = fmt.Sprintf("Gateway %s cannot carry a model now: %s.", req.GatewayRef, gatewayProblem(g))
				case ns == r.def.Namespace && name == r.def.Name:
					why = fmt.Sprintf("Gateway %s is KServe's default: choose 'Through the cluster's default inference gateway'.", req.GatewayRef)
				}
			}
		}
		return exposurePlan{}, why + " Nothing was changed.", "prerequisites"
	}
	if !opt.Available {
		return exposurePlan{}, opt.Reason + " Nothing was changed.", "prerequisites"
	}
	plan := plans[want]
	if plan.needsAuth {
		why := r.authWhy
		if r.authErr != nil {
			why = r.authErr.Error() + "."
		}
		return exposurePlan{}, authNeededRefusal(gatewayKey(plan.gwNS, plan.gwName), why), "prerequisites"
	}
	if opt.Warning != "" {
		m.step("check", "running", opt.Warning)
	}
	return plan, "", ""
}

// exposurePlanOf is the plan of an exposure as read from a model.
func exposurePlanOf(exposure, gateway string) exposurePlan {
	ns, name, _ := strings.Cut(gateway, "/")
	return exposurePlan{exposure: exposure, gwNS: ns, gwName: name}
}

// exposureWords says how a model is reached, as part of a sentence
// ("through gateway openshift-ingress/openshift-ai-inference, KServe's
// default").
func exposureWords(e exposurePlan) string {
	switch e.exposure {
	case exposureMaaS:
		return "through MaaS only (gateway " + gatewayKey(inferenceGatewayNamespace, maasGatewayName) + ")"
	case exposureGateway:
		return "through gateway " + gatewayKey(e.gwNS, e.gwName)
	case exposureInternal:
		if e.authOff {
			return "only inside the cluster (no route; token authentication off, as Connectivity Link is not installed)"
		}
		return "only inside the cluster (no route)"
	}
	if e.gwName == "" {
		return "through KServe's default gateway"
	}
	return "through gateway " + gatewayKey(e.gwNS, e.gwName) + ", KServe's default"
}

// publishToMaaS publishes a model just set up to be reached through MaaS
// only, with the MaaS tab's own publish (step "publish"): the MaaS gateway
// admits the project, then the model reference, subscription and access
// policy list it.
func (m *modelsRun) publishToMaaS(project, name string) (*types.OperationResponse, error) {
	m.step("publish", "running", "Publishing the model to MaaS (its route is on the MaaS gateway alone)...")
	pub := &maasRun{c: m.c, activity: "publish-maas", target: fmt.Sprintf("model %s in %s", name, project), newModel: true}
	pub.emit = func(e UpdateStepEvent) {
		if e.Status != "failed" {
			m.step("publish", "running", e.Message)
		}
	}
	// No audience of its own: the subscription keeps who can use the
	// published models (everyone signed in, when it is the first). The limit
	// is the Publish dialog's default, the MaaS docs' premium example
	// (100,000 tokens a day); both change on the MaaS tab.
	res, err := pub.publish(project, name, nil, &types.MaaSLimit{Preset: "premium"})
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return m.fail("publish", fmt.Sprintf("The model was created, but publishing it to MaaS stopped: %s Publish it again on the MaaS tab, or delete the model.", res.Message), firstNonEmpty(res.ErrorCode, "partial_failure"), "publish failed")
	}
	m.step("publish", "success", res.Message)
	return nil, nil
}

// addModelRouting adds how a model is reached to its state: the exposure
// and gateway, the authentication on that gateway (auth caches it by
// gateway), its in-cluster address, the name it serves, and the last
// "Can it be called?" check.
func addModelRouting(c *Client, s *types.ModelState, m llmisvcObject, def kserveGateway, auth map[string]string) {
	s.Exposure, s.Gateway = modelExposureOf(m, def)
	s.ServedName = firstNonEmpty(m.Spec.Model.Name, m.Metadata.Name)
	s.InternalURL = withServicePort(c, modelInClusterURL(m))
	switch s.Exposure {
	case exposureMaaS:
		s.GatewayAuth = "maas"
	case exposureDefault, exposureGateway:
		if _, done := auth[s.Gateway]; !done {
			ns, name, _ := strings.Cut(s.Gateway, "/")
			auth[s.Gateway] = authPolicyStateOf(c, ns, name)
		}
		s.GatewayAuth = auth[s.Gateway]
	}
	s.CallCheck = lastCallCheck(c, m.Metadata.Namespace, m.Metadata.Name, m.Metadata.UID)
}

// modelExposureOf reads how an existing model is reached (ModelState
// Exposure and Gateway) from its spec.router and KServe's default gateway.
func modelExposureOf(m llmisvcObject, def kserveGateway) (string, string) {
	r := m.Spec.Router
	if r == nil || r.Route == nil {
		return exposureInternal, ""
	}
	if r.Gateway == nil || len(r.Gateway.Refs) == 0 {
		if !def.Known {
			return exposureDefault, gatewayKey(inferenceGatewayNamespace, inferenceGatewayName)
		}
		return exposureDefault, def.String()
	}
	ref := r.Gateway.Refs[0]
	ns := firstNonEmpty(ref.Namespace, m.Metadata.Namespace)
	for _, x := range r.Gateway.Refs {
		if isMaaSGatewayRef(x.Name, firstNonEmpty(x.Namespace, m.Metadata.Namespace)) {
			return exposureMaaS, gatewayKey(inferenceGatewayNamespace, maasGatewayName)
		}
	}
	return exposureGateway, gatewayKey(ns, ref.Name)
}
