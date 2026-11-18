package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The inference gateway: the Gateway that LLMInferenceServices route
// through by default. KServe's inferenceservice-config names it
// (ingress.kserveIngressGateway, "openshift-ingress/openshift-ai-inference"
// on RHOAI 3.x), and a model whose spec.router.gateway is {} attaches to it.
// RHOAI provisions it with Red Hat's rhai-on-openshift-chart
// (opendatahub-io/odh-gitops charts/rhai-on-openshift-chart): GatewayClass
// openshift-ai-inference (OpenShift's gateway controller) and the Gateway in
// openshift-ingress. Clusters installed without that chart have none.
//
// This tool creates the chart's objects with two HTTPS listeners (validated
// on ROSA HCP with RHOAI 3.6, 2026-11-16):
//   - https-external: hostname inference.<apps domain>, terminated with the
//     cluster's default ingress certificate, which the Gateway only names
//     (the Secret is never read or copied). The ingress operator publishes
//     the hostname in DNS, and KServe advertises it as the model's URL.
//   - https: hostname <gateway>-<class>.openshift-ingress.svc.cluster.local,
//     the Gateway's own Service, with a service-ca serving certificate that
//     the parametersRef ConfigMap asks for. In-cluster callers (the
//     dashboard's gen-ai proxy) use that name as SNI; a gateway with only
//     the external hostname resets them.
//
// allowedRoutes "All" is the chart's default. It is safe because
// odh-model-controller attaches a fail-closed AuthPolicy
// (<gateway>-authn: TokenReview, then a SubjectAccessReview for "get" on
// the model) to every gateway a model uses; the status warns when it is
// missing or not enforced.
const (
	gatewayAPIVersion          = "gateway.networking.k8s.io/v1"
	inferenceGatewayName       = "openshift-ai-inference"
	inferenceGatewayClass      = "openshift-ai-inference"
	inferenceGatewayNamespace  = "openshift-ingress"
	inferenceGatewayOptions    = "openshift-ai-inference-options"
	inferenceGatewayServiceTLS = "openshift-ai-inference-service-tls"
	inferenceGatewayAuthPolicy = "openshift-ai-inference-authn"
	// inferenceGatewayService is the Service OpenShift's gateway controller
	// (Istio) creates for the Gateway: <gateway>-<class>.
	inferenceGatewayService = "openshift-ai-inference-openshift-ai-inference"
	gatewayControllerName   = "openshift.io/gateway-controller/v1"
	ingressOperatorNS       = "openshift-ingress-operator"
	// defaultRouterCert is the Secret the ingress operator generates for the
	// default IngressController when it names no certificate of its own
	// (cluster-ingress-operator, "router-certs-<ingresscontroller>").
	defaultRouterCert     = "router-certs-default"
	inferenceHostPrefix   = "inference."
	gatewayNameLabel      = "gateway.networking.k8s.io/gateway-name"
	servingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"
	kuadrantGroup         = "kuadrant.io"
)

var (
	// InferenceGatewayReadyTimeout bounds the wait for Programmed=True and
	// an address (about 20 s live, with an AWS load balancer).
	InferenceGatewayReadyTimeout = 2 * time.Minute
	// InferenceGatewayDNSTimeout bounds the wait for the external hostname's
	// DNSRecord to be published.
	InferenceGatewayDNSTimeout = 2 * time.Minute
	// InferenceGatewayDeleteTimeout bounds the wait for the Gateway to go.
	InferenceGatewayDeleteTimeout = time.Minute
	InferenceGatewayPoll          = 5 * time.Second
	// InferenceGatewayRoutesTimeout bounds the wait, after models were
	// deleted in the same operation, for the gateway controller to stop
	// counting their routes (it updates attachedRoutes asynchronously).
	InferenceGatewayRoutesTimeout = time.Minute
)

// inferenceGatewayInternalHost is the in-cluster name of the Gateway's
// Service, the hostname of its "https" listener.
const inferenceGatewayInternalHost = "openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local"

type k8sCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func findCondition(conds []k8sCondition, typ string) (k8sCondition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return k8sCondition{}, false
}

type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []json.RawMessage `json:"matchExpressions"`
}

type gatewayListener struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	AllowedRoutes *struct {
		Namespaces *struct {
			From     string         `json:"from"`
			Selector *labelSelector `json:"selector"`
		} `json:"namespaces"`
	} `json:"allowedRoutes"`
}

type gatewayObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string            `json:"gatewayClassName"`
		Listeners        []gatewayListener `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Value string `json:"value"`
		} `json:"addresses"`
		Conditions []k8sCondition `json:"conditions"`
		Listeners  []struct {
			Name           string `json:"name"`
			AttachedRoutes int    `json:"attachedRoutes"`
		} `json:"listeners"`
	} `json:"status"`
}

// attachedRoutes is the number of routes the gateway controller reports
// attached to the gateway's listeners: the authoritative count of what
// uses it, including models that inherit their route from a preset
// (<prefix>config-llm-router-route sets the global gateway as parentRef)
// and routes attached by hand.
//
// A route without a sectionName attaches to every listener that admits it,
// so the count is the largest of any listener (a sum would count such a
// route once per listener): at least one per route someone attached to a
// single listener, and 0 only when none is attached.
func (g gatewayObject) attachedRoutes() int {
	n := 0
	for _, l := range g.Status.Listeners {
		n = max(n, l.AttachedRoutes)
	}
	return n
}

// externalHost is the hostname of the first listener that is not the
// Service's in-cluster name, or "".
func (g gatewayObject) externalHost() string {
	for _, l := range g.Spec.Listeners {
		if l.Hostname != "" && !strings.HasSuffix(l.Hostname, ".svc.cluster.local") && !strings.HasPrefix(l.Hostname, "*") {
			return l.Hostname
		}
	}
	return ""
}

// admits reports whether the gateway's listeners let a route from
// namespace (with labels) attach: "yes", "no" or "unknown" (a selector
// with expressions, which this tool does not evaluate).
func (g gatewayObject) admits(namespace string, labels map[string]string) string {
	result := "no"
	for _, l := range g.Spec.Listeners {
		from, sel := "Same", (*labelSelector)(nil)
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil {
			from = firstNonEmpty(l.AllowedRoutes.Namespaces.From, "Same")
			sel = l.AllowedRoutes.Namespaces.Selector
		}
		switch from {
		case "All":
			return "yes"
		case "Same":
			if namespace == g.Metadata.Namespace {
				return "yes"
			}
		case "Selector":
			if sel == nil {
				continue
			}
			match := true
			for k, v := range sel.MatchLabels {
				if labels[k] != v {
					match = false
				}
			}
			if match && len(sel.MatchExpressions) > 0 {
				result = "unknown"
				continue
			}
			if match {
				return "yes"
			}
		}
	}
	return result
}

// gatewayOwned: this tool created the object. It carries the tool label
// and the tool wrote it; a copied label alone is not enough.
func gatewayOwned(meta objectMeta) bool {
	return meta.hasToolLabel() && meta.hasManager(toolManagers)
}

// gatewayObjectPath returns the path of an inference gateway object this
// tool creates, by kind; only these names, so teardown cannot reach
// another object and the RBAC rules stay limited to them.
func gatewayObjectPath(kind string) (string, bool) {
	switch kind {
	case "GatewayClass":
		return clusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "openshift-ai-inference"), true
	case "ConfigMap":
		return namespacedPath("v1", "configmaps", "openshift-ingress", "openshift-ai-inference-options"), true
	case "Gateway":
		return namespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "openshift-ai-inference"), true
	}
	return "", false
}

// gatewayCreatePath returns the collection an inference gateway object is
// created in.
func gatewayCreatePath(kind string) (string, bool) {
	switch kind {
	case "GatewayClass":
		return withToolFieldManager(clusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "")), true
	case "ConfigMap":
		return withToolFieldManager(namespacedPath("v1", "configmaps", "openshift-ingress", "")), true
	case "Gateway":
		return withToolFieldManager(namespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "")), true
	}
	return "", false
}

// gatewayKinds are the inference gateway's objects in the order setup
// writes them; teardown deletes them in reverse.
var gatewayKinds = []string{"GatewayClass", "ConfigMap", "Gateway"}

func gatewayDesc(kind string) string {
	switch kind {
	case "GatewayClass":
		return "GatewayClass " + inferenceGatewayClass
	case "ConfigMap":
		return "ConfigMap " + inferenceGatewayNamespace + "/" + inferenceGatewayOptions
	}
	return "Gateway " + inferenceGatewayNamespace + "/" + inferenceGatewayName
}

func readGatewayObject(c *Client, kind string) (objectMeta, bool, error) {
	path, _ := gatewayObjectPath(kind)
	return readObjectMeta(c, path)
}

func readInferenceGateway(c *Client) (gatewayObject, bool, error) {
	path, _ := gatewayObjectPath("Gateway")
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return gatewayObject{}, false, nil
	}
	if err != nil {
		return gatewayObject{}, false, err
	}
	var gw gatewayObject
	if err := json.Unmarshal(body, &gw); err != nil {
		return gatewayObject{}, false, fmt.Errorf("parse Gateway %s/%s: %w", inferenceGatewayNamespace, inferenceGatewayName, err)
	}
	return gw, true, nil
}

// clusterAppsDomain is the cluster's apps domain (ingresses.config.openshift.io
// cluster, spec.domain), under which the external hostname is made.
func clusterAppsDomain(c *Client) (string, error) {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "ingresses", "cluster"))
	if err != nil {
		return "", fmt.Errorf("read the cluster's ingress configuration: %w", err)
	}
	var ing struct {
		Spec struct {
			Domain string `json:"domain"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &ing); err != nil {
		return "", fmt.Errorf("parse the cluster's ingress configuration: %w", err)
	}
	if ing.Spec.Domain == "" {
		return "", errors.New("the cluster's ingress configuration names no apps domain")
	}
	return ing.Spec.Domain, nil
}

// defaultIngressCertificate is the name of the default IngressController's
// certificate Secret in openshift-ingress, which serves *.<apps domain>:
// spec.defaultCertificate.name, else the one the ingress operator
// generates. The Gateway references it; its content is never read.
func defaultIngressCertificate(c *Client) (string, error) {
	body, _, err := c.get(namespacedPath("operator.openshift.io/v1", "ingresscontrollers", "openshift-ingress-operator", "default"))
	if IsK8sError(err, 404) {
		return defaultRouterCert, nil
	}
	if err != nil {
		return "", fmt.Errorf("read IngressController %s/default: %w", ingressOperatorNS, err)
	}
	var ic struct {
		Spec struct {
			DefaultCertificate *struct {
				Name string `json:"name"`
			} `json:"defaultCertificate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &ic); err != nil {
		return "", fmt.Errorf("parse IngressController %s/default: %w", ingressOperatorNS, err)
	}
	if ic.Spec.DefaultCertificate != nil && ic.Spec.DefaultCertificate.Name != "" {
		return ic.Spec.DefaultCertificate.Name, nil
	}
	return defaultRouterCert, nil
}

// inferenceGatewayObjects are the objects setup applies, by kind.
func inferenceGatewayObjects(domain, certificate string) map[string]map[string]interface{} {
	listener := func(name, host, cert string) map[string]interface{} {
		return map[string]interface{}{
			"name":          name,
			"port":          443,
			"protocol":      "HTTPS",
			"hostname":      host,
			"allowedRoutes": map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}},
			"tls": map[string]interface{}{
				"mode":            "Terminate",
				"certificateRefs": []interface{}{map[string]interface{}{"group": "", "kind": "Secret", "name": cert}},
			},
		}
	}
	return map[string]map[string]interface{}{
		"GatewayClass": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "GatewayClass",
			"metadata":   map[string]interface{}{"name": "openshift-ai-inference", "labels": toolLabels()},
			"spec":       map[string]interface{}{"controllerName": gatewayControllerName},
		},
		// The gateway controller merges data.service into the Service it
		// creates for the Gateway (Gateway API spec.infrastructure.parametersRef,
		// as the MaaS chart's gateway options do): service-ca then issues the
		// "https" listener's certificate.
		"ConfigMap": {
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "openshift-ai-inference-options", "namespace": inferenceGatewayNamespace, "labels": toolLabels()},
			"data": map[string]interface{}{
				"service": "metadata:\n  annotations:\n    " + servingCertAnnotation + ": " + inferenceGatewayServiceTLS + "\n",
			},
		},
		"Gateway": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      "openshift-ai-inference",
				"namespace": inferenceGatewayNamespace,
				// The chart's label: the gateway runs on OpenShift's own
				// Istio revision.
				"labels": map[string]interface{}{managedByLabelKey: managedByLabelValue, "istio.io/rev": "openshift-gateway"},
			},
			"spec": map[string]interface{}{
				"gatewayClassName": inferenceGatewayClass,
				"infrastructure": map[string]interface{}{
					"parametersRef": map[string]interface{}{"group": "", "kind": "ConfigMap", "name": inferenceGatewayOptions},
				},
				"listeners": []interface{}{
					listener("https-external", inferenceHostPrefix+domain, certificate),
					listener("https", inferenceGatewayInternalHost, inferenceGatewayServiceTLS),
				},
			},
		},
	}
}

// gatewayClassController reads the controller of the GatewayClass named
// openshift-ai-inference, when it exists.
func gatewayClassController(c *Client) (string, error) {
	path, _ := gatewayObjectPath("GatewayClass")
	body, _, err := c.get(path)
	if err != nil {
		return "", err
	}
	var gc struct {
		Spec struct {
			ControllerName string `json:"controllerName"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &gc); err != nil {
		return "", err
	}
	return gc.Spec.ControllerName, nil
}

// authPolicyState reads the AuthPolicy odh-model-controller adds to the
// gateway: "enforced", "not-enforced", "missing", or "" when unknown.
func authPolicyState(c *Client) string {
	versions := servedVersions(c, kuadrantGroup, []string{"v1"})
	if len(versions.Versions) == 0 {
		return ""
	}
	body, _, err := c.get(namespacedPath("kuadrant.io/"+versions.Versions[0], "authpolicies", "openshift-ingress", "openshift-ai-inference-authn"))
	if IsK8sError(err, 404) {
		return "missing"
	}
	if err != nil {
		return ""
	}
	var ap struct {
		Status struct {
			Conditions []k8sCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &ap) != nil {
		return ""
	}
	if cond, ok := findCondition(ap.Status.Conditions, "Enforced"); ok {
		switch cond.Status {
		case "True":
			return "enforced"
		case "False":
			return "not-enforced"
		}
	}
	return ""
}

// dnsPublished reports whether the DNSRecord the ingress operator made
// for host is published in every zone: "published", "pending", or "" when
// it cannot be read. The record for the in-cluster listener is never
// published (UnmanagedDNS); it is not this host's.
func dnsPublished(c *Client, host string) string {
	body, _, err := c.get(namespacedPath("ingress.operator.openshift.io/v1", "dnsrecords", "openshift-ingress", ""))
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Spec struct {
				DNSName string `json:"dnsName"`
			} `json:"spec"`
			Status struct {
				Zones []struct {
					Conditions []k8sCondition `json:"conditions"`
				} `json:"zones"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return ""
	}
	for _, rec := range list.Items {
		if strings.TrimSuffix(rec.Spec.DNSName, ".") != host {
			continue
		}
		if len(rec.Status.Zones) == 0 {
			return "pending"
		}
		for _, z := range rec.Status.Zones {
			if cond, ok := findCondition(z.Conditions, "Published"); !ok || cond.Status != "True" {
				return "pending"
			}
		}
		return "published"
	}
	return "pending"
}

// kserveGateway is KServe's default gateway for LLMInferenceServices, read
// at run time from ConfigMap inferenceservice-config in the applications
// namespace, data key "ingress", field kserveIngressGateway: "namespace/name",
// or "name" in KServe's own namespace (rhoai-3.6 llmisvc NewConfig). A
// model whose spec.router.gateway is {} routes through it. Known is false
// when it cannot be read.
type kserveGateway struct {
	Namespace, Name string
	Known           bool
}

const kserveConfigMap = "inferenceservice-config"

func (g kserveGateway) String() string { return g.Namespace + "/" + g.Name }

// isInferenceGateway: the default is the gateway this tool manages, or
// unknown (then assumed to be, as on RHOAI 3.x: the safe side for guards).
func (g kserveGateway) isInferenceGateway() bool {
	return !g.Known || (g.Namespace == inferenceGatewayNamespace && g.Name == inferenceGatewayName)
}

// readKServeGateway reads KServe's default gateway. A missing ConfigMap or
// key is unknown, not an error; err is set when it cannot be read.
func readKServeGateway(c *Client) (kserveGateway, error) {
	ns := appsNamespace(c)
	body, _, err := c.get(namespacedPath("v1", "configmaps", ns, kserveConfigMap))
	if IsK8sError(err, 404) {
		return kserveGateway{}, nil
	}
	if err != nil {
		return kserveGateway{}, fmt.Errorf("read ConfigMap %s/%s: %w", ns, kserveConfigMap, err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	var ingress struct {
		KserveIngressGateway string `json:"kserveIngressGateway"`
	}
	if json.Unmarshal(body, &cm) != nil || json.Unmarshal([]byte(cm.Data["ingress"]), &ingress) != nil {
		return kserveGateway{}, fmt.Errorf("parse ConfigMap %s/%s (ingress)", ns, kserveConfigMap)
	}
	name := strings.TrimSpace(ingress.KserveIngressGateway)
	if name == "" {
		return kserveGateway{}, nil
	}
	if gwNS, gwName, ok := strings.Cut(name, "/"); ok {
		return kserveGateway{Namespace: gwNS, Name: gwName, Known: true}, nil
	}
	return kserveGateway{Namespace: ns, Name: name, Known: true}, nil
}

// defaultGatewayElsewhere says, in words, that KServe's default gateway is
// not the one this tool manages ("" when it is, or is unknown).
func defaultGatewayElsewhere(def kserveGateway) string {
	if def.isInferenceGateway() {
		return ""
	}
	return fmt.Sprintf("KServe's default gateway is %s (ConfigMap %s, ingress.kserveIngressGateway), not %s/%s, which this tool manages: a model on the default route goes through %s.", def, kserveConfigMap, inferenceGatewayNamespace, inferenceGatewayName, def)
}

// gatewayUsers lists the LLMInferenceServices ("namespace/name") that
// route through the inference gateway: spec.router.gateway {} while it is
// KServe's default gateway (def), or a ref to it.
func gatewayUsers(models []llmisvcObject, def kserveGateway) []string {
	var users []string
	for _, m := range models {
		if m.routesThroughInferenceGateway(def) {
			users = append(users, m.Metadata.Namespace+"/"+m.Metadata.Name)
		}
	}
	sort.Strings(users)
	return users
}

// getInferenceGatewayStatus reports the inference gateway. models are the
// cluster's LLMInferenceServices (modelsErr when they cannot be listed).
func getInferenceGatewayStatus(c *Client, models []llmisvcObject, modelsErr error) types.InferenceGatewayState {
	state := types.InferenceGatewayState{ResourceState: types.ResourceState{Namespace: inferenceGatewayNamespace, Name: inferenceGatewayName}}
	gw, found, err := readInferenceGateway(c)
	if err != nil {
		state.Message = "Cannot read the inference gateway: " + err.Error()
		state.HealthUnknown = true
		return state
	}
	def, defErr := readKServeGateway(c)
	if defErr != nil {
		slog.Debug("cannot read KServe's default gateway", "error", defErr)
	}
	if modelsErr == nil {
		state.UsedBy = gatewayUsers(models, def)
	}
	if !found {
		state.Message = "Not set up"
		if domain, err := clusterAppsDomain(c); err == nil && domain != "" {
			state.PlannedURL = "https://" + inferenceHostPrefix + domain
		}
		// Objects of an earlier setup that stopped half-way: teardown
		// removes them, setup completes them.
		for _, kind := range []string{"GatewayClass", "ConfigMap"} {
			if meta, ok, err := readGatewayObject(c, kind); err == nil && ok && gatewayOwned(meta) {
				state.ManagedByTool = true
			}
		}
		return state
	}
	state.Deployed = true
	state.ManagedByTool = gatewayOwned(gw.Metadata)
	state.Terminating = gw.Metadata.terminating()
	if host := gw.externalHost(); host != "" {
		state.ExternalURL = "https://" + host
	}
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			state.Address = a.Value
			break
		}
	}
	accepted, hasAccepted := findCondition(gw.Status.Conditions, "Accepted")
	programmed, hasProgrammed := findCondition(gw.Status.Conditions, "Programmed")
	switch {
	case state.Terminating:
		state.Message = "Terminating"
	case hasAccepted && accepted.Status == "False":
		state.Message = firstNonEmpty(truncateMessage(accepted.Message), accepted.Reason, "Not accepted by the gateway controller")
		state.WaitingReason = firstNonEmpty(accepted.Reason, "NotAccepted")
		state.TerminalError = true
	case hasProgrammed && programmed.Status == "True" && state.Address != "":
		state.Ready = true
		state.Message = "Running"
	case hasProgrammed && programmed.Status == "True":
		state.Message = "Programmed, waiting for an address"
	case hasProgrammed:
		state.Message = firstNonEmpty(truncateMessage(programmed.Message), programmed.Reason, "Not programmed yet")
	case state.ManagedByTool:
		state.Message = "Waiting for the gateway controller"
	default:
		// Someone else's gateway that reports nothing: its health is unknown.
		state.Message = "Reports no Programmed condition"
		state.HealthUnknown = true
	}
	if state.Ready {
		state.AuthPolicy = authPolicyState(c)
		if state.ManagedByTool && state.ExternalURL != "" {
			state.DNS = dnsPublished(c, strings.TrimPrefix(state.ExternalURL, "https://"))
		}
	}
	var warnings []string
	if msg := defaultGatewayElsewhere(def); msg != "" {
		warnings = append(warnings, msg)
	}
	if len(state.UsedBy) > 0 && (state.AuthPolicy == "missing" || state.AuthPolicy == "not-enforced") {
		what := map[string]string{"missing": "is missing", "not-enforced": "is not enforced"}[state.AuthPolicy]
		warnings = append(warnings, fmt.Sprintf("The gateway's authentication %s (AuthPolicy %s/%s, which odh-model-controller adds once a model uses the gateway). Check that odh-model-controller and Connectivity Link (Kuadrant) run; until then models behind it may answer without checking who calls.", what, inferenceGatewayNamespace, inferenceGatewayAuthPolicy))
	}
	if state.Ready && state.ManagedByTool {
		if msg := gatewayServiceWarning(c); msg != "" {
			warnings = append(warnings, msg)
		}
	}
	state.Warning = strings.Join(warnings, " ")
	switch {
	case !state.ManagedByTool:
		state.TeardownBlockedReason = "This gateway was not created by this tool, so the tool does not change or delete it."
	case modelsErr != nil:
		state.TeardownBlockedReason = "Cannot check which models use the gateway: " + modelsErr.Error()
	case len(state.UsedBy) > 0:
		state.TeardownBlockedReason = fmt.Sprintf("Models route through it: %s. Delete %s first.", strings.Join(state.UsedBy, ", "), verb(len(state.UsedBy), "that model", "those models"))
	case gw.attachedRoutes() > 0:
		state.TeardownBlockedReason = attachedRoutesReason(gw.attachedRoutes())
	}
	return state
}

// otherGatewaysOfClass lists the Gateways (namespace/name) other than the
// inference gateway that use its GatewayClass. Gateways are listed
// cluster-wide, read-only: another one can be in any namespace.
func otherGatewaysOfClass(c *Client) ([]string, error) {
	body, _, err := c.get(clusterPath("gateway.networking.k8s.io/v1", "gateways", ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				GatewayClassName string `json:"gatewayClassName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse the Gateways: %w", err)
	}
	var out []string
	for _, g := range list.Items {
		if g.Spec.GatewayClassName != inferenceGatewayClass || (g.Metadata.Namespace == inferenceGatewayNamespace && g.Metadata.Name == inferenceGatewayName) {
			continue
		}
		out = append(out, g.Metadata.Namespace+"/"+g.Metadata.Name)
	}
	sort.Strings(out)
	return out, nil
}

// attachedRoutesReason says why a gateway with n attached routes stays.
func attachedRoutesReason(n int) string {
	return fmt.Sprintf("At least %d %s attached to the inference gateway (models, or routes made by hand; `oc get httproute -A` lists them). Delete them first.", n, verb(n, "route is", "routes are"))
}

// gatewayServiceWarning checks that the gateway's Service has the name the
// "https" listener's hostname assumes ("" when it does, or cannot be read).
func gatewayServiceWarning(c *Client) string {
	body, _, err := c.get(namespacedPath("v1", "services", "openshift-ingress", "openshift-ai-inference-openshift-ai-inference"))
	missing := fmt.Sprintf("The gateway's Service is not named %s, so the in-cluster listener (%s) does not match it and callers inside the cluster (the dashboard's playground) may be refused. Report this with the output of `oc get svc -n %s -l %s=%s`.", inferenceGatewayService, inferenceGatewayInternalHost, inferenceGatewayNamespace, gatewayNameLabel, inferenceGatewayName)
	if IsK8sError(err, 404) {
		return missing
	}
	if err != nil {
		return ""
	}
	meta, err := parseObjectMeta(body)
	if err != nil || meta.Labels[gatewayNameLabel] == inferenceGatewayName {
		return ""
	}
	return missing
}

// gatewayPlan is what a gateway setup will do, decided before anything is
// written.
type gatewayPlan struct {
	// foreign: a Gateway openshift-ai-inference exists that this tool did
	// not create; it is used as it is.
	foreign bool
	gateway gatewayObject
	// objects to apply, by kind, with what was read of each.
	objects map[string]map[string]interface{}
	current map[string]objectMeta
	found   map[string]bool
	// classForeign: the GatewayClass exists and is not this tool's (it is
	// used, never changed).
	classForeign bool
	host         string
}

// planInferenceGateway reads everything the gateway setup writes or needs.
// A refusal is returned as a message and an error code; nothing is
// written.
func planInferenceGateway(c *Client) (*gatewayPlan, string, string) {
	gw, found, err := readInferenceGateway(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read Gateway %s/%s; nothing was changed: %v.%s", inferenceGatewayNamespace, inferenceGatewayName, err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	if found && !gatewayOwned(gw.Metadata) {
		return &gatewayPlan{foreign: true, gateway: gw}, "", ""
	}
	if found && gw.Metadata.terminating() {
		return nil, fmt.Sprintf("Gateway %s/%s is still being deleted. Wait until it is gone, then set it up again.", inferenceGatewayNamespace, inferenceGatewayName), "terminating"
	}
	plan := &gatewayPlan{current: map[string]objectMeta{}, found: map[string]bool{}}
	for _, kind := range gatewayKinds {
		meta, ok, err := readGatewayObject(c, kind)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read %s; nothing was changed: %v.%s", gatewayDesc(kind), err, templateHint(err)), errorCodeFromK8sErr(err)
		}
		plan.current[kind], plan.found[kind] = meta, ok
		if !ok || gatewayOwned(meta) {
			if ok && meta.terminating() {
				return nil, fmt.Sprintf("%s is still being deleted. Wait until it is gone, then set the gateway up again.", gatewayDesc(kind)), "terminating"
			}
			continue
		}
		if kind != "GatewayClass" {
			return nil, fmt.Sprintf("%s already exists and was not created by this tool, so it is left alone. Nothing was changed. Remove it, or ask its owner, then set up again.", gatewayDesc(kind)), "not_managed"
		}
		// Someone else's GatewayClass of this name: use it only when it
		// is OpenShift's gateway controller's.
		controller, err := gatewayClassController(c)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read GatewayClass %s; nothing was changed: %v.", inferenceGatewayClass, err), errorCodeFromK8sErr(err)
		}
		if controller != gatewayControllerName {
			return nil, fmt.Sprintf("GatewayClass %s exists, was not created by this tool, and belongs to controller %q instead of OpenShift's (%s). Nothing was changed.", inferenceGatewayClass, controller, gatewayControllerName), "not_managed"
		}
		plan.classForeign = true
	}
	domain, err := clusterAppsDomain(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's apps domain for the gateway's hostname; nothing was changed: %v.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites")
	}
	cert, err := defaultIngressCertificate(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's default ingress certificate; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	plan.objects = inferenceGatewayObjects(domain, cert)
	plan.host = inferenceHostPrefix + domain
	return plan, "", ""
}

// applyInferenceGateway writes the plan's objects (a server-side dry run
// of all of them first, when dryRun). It returns a refusal message and an
// error code, or "".
func applyInferenceGateway(c *Client, plan *gatewayPlan, dryRun bool, logf func(string)) (string, string) {
	var applied []string
	for _, kind := range gatewayKinds {
		if kind == "GatewayClass" && plan.classForeign {
			continue
		}
		path, _ := gatewayObjectPath(kind)
		createPath, _ := gatewayCreatePath(kind)
		k := kind
		read := func() (objectMeta, bool, error) { return readGatewayObject(c, k) }
		action, meta, found, err := ensureToolObject(c, gatewayDesc(kind), path, createPath, plan.objects[kind], plan.current[kind], plan.found[kind], read, gatewayOwned, dryRun)
		plan.current[kind], plan.found[kind] = meta, found
		if err != nil {
			code := errorCodeFromK8sErr(err)
			var foreign *foreignObjectError
			switch {
			case errors.As(err, &foreign):
				code = "not_managed"
			case errors.Is(err, errObjectChanged) || IsK8sError(err, 409):
				code = "conflict"
			}
			if dryRun {
				return fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", gatewayDesc(kind), err, templateHint(err)), code
			}
			done := "nothing"
			if len(applied) > 0 {
				done = strings.Join(applied, ", ")
			}
			return fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. Set up again to retry, or delete the gateway to remove it.%s", gatewayDesc(kind), err, done, templateHint(err)), code
		}
		if !dryRun {
			applied = append(applied, gatewayDesc(kind))
			logf(fmt.Sprintf("OK: %s %s", gatewayDesc(kind), action))
		}
	}
	return "", ""
}

// waitInferenceGateway waits (bounded) until the gateway is programmed
// with an address, then until its hostname is published in DNS. A wait
// that runs out is not a failure: the gateway may still come up, and the
// status shows it. It returns a terminal problem, or "".
// While the Gateway is not programmed, progress says so in plain words:
// the controller's own condition ("Failed to assign to any requested
// addresses: hostname ... not found") is normal while the load balancer is
// made and reads as an error (seen live). That condition is returned as
// pending, for the message when the wait ends without the gateway ready.
func waitInferenceGateway(c *Client, host string, progress func(string)) (ready bool, dns string, problem string, pending string, err error) {
	last := ""
	ready, err = waitForGone(c, InferenceGatewayReadyTimeout, InferenceGatewayPoll, func(cc *Client) (bool, error) {
		gw, found, err := readInferenceGateway(cc)
		if err != nil || !found {
			return false, err
		}
		accepted, hasAccepted := findCondition(gw.Status.Conditions, "Accepted")
		if hasAccepted && accepted.Status == "False" {
			problem = fmt.Sprintf("The gateway controller does not accept the gateway: %s.", firstNonEmpty(accepted.Message, accepted.Reason))
			return true, nil
		}
		programmed, ok := findCondition(gw.Status.Conditions, "Programmed")
		addr := ""
		for _, a := range gw.Status.Addresses {
			addr = firstNonEmpty(addr, a.Value)
		}
		msg := "Waiting for the gateway controller..."
		switch {
		case ok && programmed.Status == "True" && addr != "":
			return true, nil
		case ok && programmed.Status == "True":
			msg = "Programmed; waiting for the load balancer's address..."
		case ok:
			msg = "Waiting for OpenShift to create the gateway's load balancer (usually under a minute)..."
			pending = firstNonEmpty(programmed.Message, programmed.Reason)
		}
		if msg != last {
			last = msg
			progress(msg)
		}
		return false, nil
	})
	if err != nil || problem != "" || !ready || host == "" {
		return ready && problem == "", "", problem, pending, err
	}
	progress("Programmed; waiting for " + host + " to be published in DNS...")
	published, werr := waitForGone(c, InferenceGatewayDNSTimeout, InferenceGatewayPoll, func(cc *Client) (bool, error) {
		return dnsPublished(cc, host) == "published", nil
	})
	if werr != nil {
		return true, "", "", "", werr
	}
	if published {
		return true, "published", "", "", nil
	}
	return true, "pending", "", "", nil
}

// TeardownInferenceGateway deletes the inference gateway's objects this
// tool created (Gateway, its options ConfigMap, the GatewayClass), never
// while a model routes through it, and never a gateway someone else made.
// The class stays while another Gateway uses it. modelsJustDeleted: the
// same operation deleted models, so attached routes the controller still
// counts are waited for (bounded) before refusing.
func TeardownInferenceGateway(c *Client, modelsJustDeleted bool) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordModelsActivity(c, "teardown-inference-gateway", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	type current struct {
		meta  objectMeta
		found bool
	}
	objs := map[string]current{}
	ours := 0
	for _, kind := range gatewayKinds {
		meta, found, err := readGatewayObject(c, kind)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read %s; nothing was deleted: %v.%s", gatewayDesc(kind), err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
		}
		objs[kind] = current{meta, found}
		if found && gatewayOwned(meta) {
			ours++
		}
	}
	if gw := objs["Gateway"]; gw.found && !gatewayOwned(gw.meta) {
		return fail(fmt.Sprintf("Gateway %s/%s was not created by this tool, so it was not deleted.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed", "gateway not managed")
	}
	if ours == 0 {
		logs = append(logs, "No inference gateway objects of this tool exist")
		return &types.OperationResponse{Success: true, Message: "The inference gateway is not set up; nothing to delete.", Logs: logs}, nil
	}
	// Never take the gateway away from a model: fail closed when the
	// models cannot be listed.
	models, err := listLLMInferenceServices(c)
	if err != nil && !errors.Is(err, errLLMISvcAPIMissing) {
		return fail(fmt.Sprintf("Cannot check which models use the gateway, so it was not deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "model list failed")
	}
	// A model on the default route uses this gateway unless KServe's
	// default is known to be another one (unreadable counts as this one).
	def, _ := readKServeGateway(c)
	if users := gatewayUsers(models, def); len(users) > 0 {
		return fail(fmt.Sprintf("Models route through the inference gateway: %s. Delete %s first; nothing was deleted.", strings.Join(users, ", "), verb(len(users), "that model", "those models")), "in_use", "in use")
	}
	// The gateway controller's count of attached routes also sees models
	// that inherit their route from a preset, and routes made by hand.
	// Fail closed when it cannot be read.
	if gw := objs["Gateway"]; gw.found {
		current, found, err := readInferenceGateway(c)
		if err == nil && found && current.attachedRoutes() > 0 && modelsJustDeleted {
			// The controller updates the count after the routes go.
			logs = append(logs, "Waiting for the gateway controller to stop counting the deleted models' routes...")
			_, err = waitForGone(c, InferenceGatewayRoutesTimeout, InferenceGatewayPoll, func(cc *Client) (bool, error) {
				g, ok, err := readInferenceGateway(cc)
				if err == nil {
					current, found = g, ok
				}
				return err == nil && (!ok || g.attachedRoutes() == 0), err
			})
		}
		if err != nil {
			return fail(fmt.Sprintf("Cannot read the gateway's attached routes, so it was not deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "gateway read failed")
		}
		if found && current.attachedRoutes() > 0 {
			return fail(attachedRoutesReason(current.attachedRoutes())+" Nothing was deleted.", "in_use", "routes attached")
		}
	}
	var deleted, kept []string
	for i := len(gatewayKinds) - 1; i >= 0; i-- {
		kind := gatewayKinds[i]
		cur := objs[kind]
		if !cur.found || !gatewayOwned(cur.meta) {
			if cur.found {
				logs = append(logs, fmt.Sprintf("Kept %s: not created by this tool", gatewayDesc(kind)))
			}
			continue
		}
		if kind == "GatewayClass" {
			// Another Gateway on the class would lose its controller.
			why := ""
			users, err := otherGatewaysOfClass(c)
			switch {
			case err != nil:
				why = fmt.Sprintf("this tool cannot list the Gateways to check that no other one uses it (%v)", err)
			case len(users) > 0:
				why = fmt.Sprintf("Gateway %s %s it", strings.Join(users, ", Gateway "), verb(len(users), "uses", "use"))
			}
			if why != "" {
				msg := fmt.Sprintf("GatewayClass %s was kept: %s.", inferenceGatewayClass, why)
				logs = append(logs, msg)
				kept = append(kept, msg)
				continue
			}
		}
		path, _ := gatewayObjectPath(kind)
		logs = append(logs, fmt.Sprintf("Deleting %s...", gatewayDesc(kind)))
		if _, err := deleteWithUID(c, path, cur.meta.UID); err != nil && !IsK8sError(err, 404) {
			done := ""
			if len(deleted) > 0 {
				done = " Deleted before: " + strings.Join(deleted, ", ") + "."
			}
			code := errorCodeFromK8sErr(err)
			if IsK8sError(err, 409) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to delete %s: %v.%s Delete again to retry.%s", gatewayDesc(kind), err, done, templateHint(err)), code, "delete failed")
		}
		deleted = append(deleted, gatewayDesc(kind))
		logs = append(logs, fmt.Sprintf("OK: %s deleted", gatewayDesc(kind)))
		if kind == "Gateway" {
			// The class and options go after the Gateway, so its
			// controller removes the load balancer first.
			gone, werr := waitForDeletion(c, path, InferenceGatewayDeleteTimeout, InferenceGatewayPoll)
			if werr == nil && !gone {
				logs = append(logs, "The Gateway is still being deleted")
			}
		}
	}
	slog.Info("inference gateway teardown", "user", getUser(c))
	recordModelsActivity(c, "teardown-inference-gateway", strings.Join(deleted, ", "), true)
	return &types.OperationResponse{
		Success: true,
		Message: strings.TrimSpace("Inference gateway deleted: " + joinTitles(deleted) + ". OpenShift removes its load balancer and the ingress operator its DNS record. " + strings.Join(kept, " ")),
		Logs:    logs,
	}, nil
}
