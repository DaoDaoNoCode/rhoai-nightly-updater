package serving

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
// Both listeners admit routes only from the namespaces they list by name,
// as RHOAI documents it for this Gateway: allowedRoutes.namespaces from
// Selector with matchExpressions kubernetes.io/metadata.name In [...]
// (odh-gitops rhai-on-openshift-chart values.yaml: "set `from` to Selector
// (recommended) or Same", whose example is that list; the RHOAI 3.4 llm-d
// guide: "Ensure your model deployment namespace is listed in the values
// array"; RHOAI 3.6's own data-science-gateway lists openshift-ingress and
// redhat-ods-applications the same way). The list holds the gateway's own
// namespace, so it is never empty (an empty In is invalid), and each
// project a model is set up in, added before the model is created. Allow
// adds a project whose model someone else deployed (the dashboard). The
// tool never removes an entry: deleting its model leaves the project
// listed, since another model there may use the gateway.
//
// Why a list: from All (the chart's default until August 2026; OpenShift's
// Gateway API docs say not to set it) lets any project editor attach an
// HTTPRoute to the gateway. One with an Exact path to another project's
// model outranks KServe's PathPrefix route (Gateway API route precedence)
// and receives that model's calls, with the callers' bearer tokens;
// odh-model-controller's AuthPolicy (<gateway>-authn) checks who calls, not
// which backend answers (confirmed live 2026-11-18). With the list, only
// routes in listed projects attach (SECURITY.md).
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
	defaultRouterCert    = "router-certs-default"
	inferenceHostPrefix  = "inference."
	gatewayNameLabel     = "gateway.networking.k8s.io/gateway-name"
	CertSecretAnnotation = "service.beta.openshift.io/serving-cert-secret-name"
	KuadrantGroup        = "kuadrant.io"
	// namespaceNameLabel is the label the API server sets on every
	// namespace to its name; the listeners list namespaces by it.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// allowedRoutesFor is the listeners' allowedRoutes: the namespaces, by
// name.
func allowedRoutesFor(namespaces []string) map[string]any {
	values := make([]any, len(namespaces))
	for i, n := range namespaces {
		values[i] = n
	}
	return map[string]any{"namespaces": map[string]any{
		"from": "Selector",
		"selector": map[string]any{"matchExpressions": []any{map[string]any{
			"key": namespaceNameLabel, "operator": "In", "values": values,
		}}},
	}}
}

// gatewayNamespaceList is the list the tool writes: the gateway's own
// namespace first, then every other name once, sorted.
func gatewayNamespaceList(names ...[]string) []string {
	seen := map[string]bool{inferenceGatewayNamespace: true}
	var rest []string
	for _, list := range names {
		for _, n := range list {
			if n != "" && !seen[n] {
				seen[n] = true
				rest = append(rest, n)
			}
		}
	}
	sort.Strings(rest)
	return append([]string{inferenceGatewayNamespace}, rest...)
}

// errGatewayChanged: the Gateway changed after a plan read its list; the
// write is planned again from a fresh read, so someone else's entry is
// kept.
var errGatewayChanged = errors.New("the gateway changed since it was read")

var (
	// inferenceGatewayReadyTimeout bounds the wait for Programmed=True and
	// an address (about 20 s live, with an AWS load balancer).
	inferenceGatewayReadyTimeout = 2 * time.Minute
	// inferenceGatewayDNSTimeout bounds the wait for the external hostname's
	// DNSRecord to be published.
	inferenceGatewayDNSTimeout = 2 * time.Minute
	// inferenceGatewayDeleteTimeout bounds the wait for the Gateway to go.
	inferenceGatewayDeleteTimeout = time.Minute
	inferenceGatewayPoll          = 5 * time.Second
	// inferenceGatewayRoutesTimeout bounds the wait, after models were
	// deleted in the same operation, for the gateway controller to stop
	// counting their routes (it updates attachedRoutes asynchronously).
	inferenceGatewayRoutesTimeout = time.Minute
)

// inferenceGatewayInternalHost is the in-cluster name of the Gateway's
// Service, the hostname of its "https" listener.
const inferenceGatewayInternalHost = "openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local"

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

type GatewayObject struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string            `json:"gatewayClassName"`
		Listeners        []gatewayListener `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Value string `json:"value"`
		} `json:"addresses"`
		Conditions []kube.Condition `json:"conditions"`
		Listeners  []struct {
			Name           string `json:"name"`
			AttachedRoutes int    `json:"attachedRoutes"`
		} `json:"listeners"`
	} `json:"status"`
}

// AttachedRoutes is the number of routes the gateway controller reports
// attached to the gateway's listeners: the authoritative count of what
// uses it, including models that inherit their route from a preset
// (<prefix>config-llm-router-route sets the global gateway as parentRef)
// and routes attached by hand.
//
// A route without a sectionName attaches to every listener that admits it,
// so the count is the largest of any listener (a sum would count such a
// route once per listener): at least one per route someone attached to a
// single listener, and 0 only when none is attached.
func (g GatewayObject) AttachedRoutes() int {
	n := 0
	for _, l := range g.Status.Listeners {
		n = max(n, l.AttachedRoutes)
	}
	return n
}

// listedNames returns the namespaces the listener lists by name (one
// kubernetes.io/metadata.name In expression and nothing else, the shape
// this tool writes), and whether it has that shape.
func (l gatewayListener) listedNames() ([]string, bool) {
	if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil {
		return nil, false
	}
	ns := l.AllowedRoutes.Namespaces
	if ns.From != "Selector" || ns.Selector == nil || len(ns.Selector.MatchLabels) > 0 || len(ns.Selector.MatchExpressions) != 1 {
		return nil, false
	}
	var e struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	}
	if json.Unmarshal(ns.Selector.MatchExpressions[0], &e) != nil || e.Key != namespaceNameLabel || e.Operator != "In" {
		return nil, false
	}
	return e.Values, true
}

// listedNamespaces returns every name the listeners list (whoever added
// it), and whether every listener has the names-list shape. A gateway an
// earlier build made (from All, or a namespace label) does not.
func (g GatewayObject) listedNamespaces() ([]string, bool) {
	var names []string
	shaped := len(g.Spec.Listeners) > 0
	for _, l := range g.Spec.Listeners {
		values, ok := l.listedNames()
		shaped = shaped && ok
		names = append(names, values...)
	}
	return names, shaped
}

// allowedProjects is what the status says the gateway allows: the
// namespaces every listener admits by name (from Same: the gateway's own;
// a missing allowedRoutes is Same), all when every listener admits every
// namespace, and known false when a listener chooses by labels.
func (g GatewayObject) allowedProjects() (names []string, all, known bool) {
	var set map[string]bool // nil: no listener narrowed it yet
	for _, l := range g.Spec.Listeners {
		from := "Same"
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil {
			from = kube.FirstNonEmpty(l.AllowedRoutes.Namespaces.From, "Same")
		}
		var these []string
		switch from {
		case "All":
			continue
		case "Same":
			these = []string{g.Metadata.Namespace}
		default:
			values, ok := l.listedNames()
			if !ok {
				return nil, false, false
			}
			these = values
		}
		next := map[string]bool{}
		for _, n := range these {
			if set == nil || set[n] {
				next[n] = true
			}
		}
		set = next
	}
	if set == nil {
		return nil, len(g.Spec.Listeners) > 0, len(g.Spec.Listeners) > 0
	}
	names = []string{}
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, false, true
}

// hosts are the listeners' hostnames.
func (g GatewayObject) hosts() map[string]bool {
	out := map[string]bool{}
	for _, l := range g.Spec.Listeners {
		if l.Hostname != "" {
			out[l.Hostname] = true
		}
	}
	return out
}

// externalHost is the hostname of the first listener that is not the
// Service's in-cluster name, or "".
func (g GatewayObject) externalHost() string {
	for _, l := range g.Spec.Listeners {
		if l.Hostname != "" && !strings.HasSuffix(l.Hostname, ".svc.cluster.local") && !strings.HasPrefix(l.Hostname, "*") {
			return l.Hostname
		}
	}
	return ""
}

// Admits reports whether the gateway's listeners let a route from
// namespace (with labels) attach: "yes", "no" or "unknown" (a selector
// with expressions, which this tool does not evaluate).
func (g GatewayObject) Admits(namespace string, labels map[string]string) string {
	result := "no"
	for _, l := range g.Spec.Listeners {
		from, sel := "Same", (*labelSelector)(nil)
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil {
			from = kube.FirstNonEmpty(l.AllowedRoutes.Namespaces.From, "Same")
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
			if match {
				switch matchExpressions(sel.MatchExpressions, namespace, labels) {
				case "yes":
					return "yes"
				case "unknown":
					result = "unknown"
				}
			}
		}
	}
	return result
}

// matchExpressions evaluates a label selector's expressions (In, NotIn,
// Exists, DoesNotExist, as metav1.LabelSelectorRequirement) against a
// namespace's labels, with kubernetes.io/metadata.name, which the API
// server sets on every namespace: "yes", "no", or "unknown" for an
// expression it cannot read.
func matchExpressions(exprs []json.RawMessage, namespace string, labels map[string]string) string {
	all := map[string]string{"kubernetes.io/metadata.name": namespace}
	maps.Copy(all, labels)
	for _, raw := range exprs {
		var e struct {
			Key      string   `json:"key"`
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		}
		if json.Unmarshal(raw, &e) != nil {
			return "unknown"
		}
		v, has := all[e.Key]
		var ok bool
		switch e.Operator {
		case "In":
			ok = has && slices.Contains(e.Values, v)
		case "NotIn":
			ok = !has || !slices.Contains(e.Values, v)
		case "Exists":
			ok = has
		case "DoesNotExist":
			ok = !has
		default:
			return "unknown"
		}
		if !ok {
			return "no"
		}
	}
	return "yes"
}

// gatewayOwned: this tool created the object. It carries the tool label
// and the tool wrote it; a copied label alone is not enough.
func gatewayOwned(meta kube.ObjectMeta) bool {
	return meta.HasToolLabel() && meta.HasManager(envkit.ToolManagers)
}

// gatewayObjectPath returns the path of an inference gateway object this
// tool creates, by kind; only these names, so teardown cannot reach
// another object and the RBAC rules stay limited to them.
func gatewayObjectPath(kind string) string {
	switch kind {
	case "GatewayClass":
		return rhoai.ClusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "openshift-ai-inference")
	case "ConfigMap":
		return rhoai.NamespacedPath("v1", "configmaps", "openshift-ingress", "openshift-ai-inference-options")
	case "Gateway":
		return rhoai.NamespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "openshift-ai-inference")
	}
	return ""
}

// gatewayCreatePath returns the collection an inference gateway object is
// created in.
func gatewayCreatePath(kind string) (string, bool) {
	switch kind {
	case "GatewayClass":
		return envkit.WithToolFieldManager(rhoai.ClusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "")), true
	case "ConfigMap":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "configmaps", "openshift-ingress", "")), true
	case "Gateway":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "")), true
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

func ReadGatewayObject(c *kube.Client, kind string) (kube.ObjectMeta, bool, error) {
	path := gatewayObjectPath(kind)
	return kube.ReadObjectMeta(c, path)
}

func readInferenceGateway(c *kube.Client) (GatewayObject, bool, error) {
	path := gatewayObjectPath("Gateway")
	var gw GatewayObject
	found, err := c.GetIntoOptional(path, &gw)
	if err != nil {
		if kube.IsDecodeError(err) {
			return GatewayObject{}, false, fmt.Errorf("parse Gateway %s/%s: %w", inferenceGatewayNamespace, inferenceGatewayName, err)
		}
		return GatewayObject{}, false, err
	}
	if !found {
		return GatewayObject{}, false, nil
	}
	return gw, true, nil
}

// clusterAppsDomain is the cluster's apps domain (ingresses.config.openshift.io
// cluster, spec.domain), under which the external hostname is made.
func clusterAppsDomain(c *kube.Client) (string, error) {
	var ing struct {
		Spec struct {
			Domain string `json:"domain"`
		} `json:"spec"`
	}
	if err := c.GetInto(rhoai.ClusterPath("config.openshift.io/v1", "ingresses", "cluster"), &ing); err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse the cluster's ingress configuration: %w", err)
		}
		return "", fmt.Errorf("read the cluster's ingress configuration: %w", err)
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
func defaultIngressCertificate(c *kube.Client) (string, error) {
	var ic struct {
		Spec struct {
			DefaultCertificate *struct {
				Name string `json:"name"`
			} `json:"defaultCertificate"`
		} `json:"spec"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("operator.openshift.io/v1", "ingresscontrollers", "openshift-ingress-operator", "default"), &ic)
	if err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse IngressController %s/default: %w", ingressOperatorNS, err)
		}
		return "", fmt.Errorf("read IngressController %s/default: %w", ingressOperatorNS, err)
	}
	if !found {
		return defaultRouterCert, nil
	}
	if ic.Spec.DefaultCertificate != nil && ic.Spec.DefaultCertificate.Name != "" {
		return ic.Spec.DefaultCertificate.Name, nil
	}
	return defaultRouterCert, nil
}

// inferenceGatewayObjects are the objects setup applies, by kind.
func inferenceGatewayObjects(domain, certificate string, namespaces []string) map[string]map[string]any {
	listener := func(name, host, cert string) map[string]any {
		return map[string]any{
			"name":          name,
			"port":          443,
			"protocol":      "HTTPS",
			"hostname":      host,
			"allowedRoutes": allowedRoutesFor(namespaces),
			"tls": map[string]any{
				"mode":            "Terminate",
				"certificateRefs": []any{map[string]any{"group": "", "kind": "Secret", "name": cert}},
			},
		}
	}
	return map[string]map[string]any{
		"GatewayClass": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "GatewayClass",
			"metadata":   map[string]any{"name": "openshift-ai-inference", "labels": envkit.ToolLabels()},
			"spec":       map[string]any{"controllerName": gatewayControllerName},
		},
		// The gateway controller merges data.service into the Service it
		// creates for the Gateway (Gateway API spec.infrastructure.parametersRef,
		// as the MaaS chart's gateway options do): service-ca then issues the
		// "https" listener's certificate.
		"ConfigMap": {
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "openshift-ai-inference-options", "namespace": inferenceGatewayNamespace, "labels": envkit.ToolLabels()},
			"data": map[string]any{
				"service": "metadata:\n  annotations:\n    " + CertSecretAnnotation + ": " + inferenceGatewayServiceTLS + "\n",
			},
		},
		"Gateway": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "Gateway",
			"metadata": map[string]any{
				"name":      "openshift-ai-inference",
				"namespace": inferenceGatewayNamespace,
				// The chart's label: the gateway runs on OpenShift's own
				// Istio revision.
				"labels": map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue, "istio.io/rev": "openshift-gateway"},
			},
			"spec": map[string]any{
				"gatewayClassName": inferenceGatewayClass,
				"infrastructure": map[string]any{
					"parametersRef": map[string]any{"group": "", "kind": "ConfigMap", "name": inferenceGatewayOptions},
				},
				"listeners": []any{
					listener("https-external", inferenceHostPrefix+domain, certificate),
					listener("https", inferenceGatewayInternalHost, inferenceGatewayServiceTLS),
				},
			},
		},
	}
}

// gatewayClassController reads the controller of the GatewayClass named
// openshift-ai-inference, when it exists.
func gatewayClassController(c *kube.Client) (string, error) {
	path := gatewayObjectPath("GatewayClass")
	var gc struct {
		Spec struct {
			ControllerName string `json:"controllerName"`
		} `json:"spec"`
	}
	if err := c.GetInto(path, &gc); err != nil {
		return "", err
	}
	return gc.Spec.ControllerName, nil
}

// authPolicyState reads the AuthPolicy odh-model-controller adds to the
// inference gateway: "enforced", "not-enforced", "missing", or "" when
// unknown.
func authPolicyState(c *kube.Client) string {
	return authPolicyStateOf(c, inferenceGatewayNamespace, inferenceGatewayName)
}

// authPolicyStateOf reads the AuthPolicy odh-model-controller adds to a
// gateway a model routes through (<gateway>-authn, in the gateway's
// namespace): "enforced", "not-enforced", "missing", or "" when unknown.
// The inference gateway's is read by its name; another gateway's only in
// OpenShift's ingress namespace, where this app may list AuthPolicies
// (read-only). Elsewhere it is unknown.
func authPolicyStateOf(c *kube.Client, ns, gateway string) string {
	versions := rhoai.ServedVersions(c, KuadrantGroup, []string{"v1"})
	if len(versions.Versions) == 0 {
		return ""
	}
	var body []byte
	var err error
	if ns == inferenceGatewayNamespace && gateway == inferenceGatewayName {
		body, _, err = c.Get(rhoai.NamespacedPath("kuadrant.io/"+versions.Versions[0], "authpolicies", "openshift-ingress", "openshift-ai-inference-authn"))
		if kube.IsK8sNotFound(err) {
			return "missing"
		}
	} else {
		if ns != inferenceGatewayNamespace {
			return ""
		}
		var list []byte
		list, _, err = c.Get(rhoai.NamespacedPath("kuadrant.io/"+versions.Versions[0], "authpolicies", "openshift-ingress", ""))
		if err == nil {
			var items struct {
				Items []json.RawMessage `json:"items"`
			}
			if json.Unmarshal(list, &items) != nil {
				return ""
			}
			for _, raw := range items.Items {
				meta, perr := kube.ParseObjectMeta(raw)
				if perr == nil && meta.Name == gateway+"-authn" {
					body = raw
				}
			}
			if body == nil {
				return "missing"
			}
		}
	}
	if err != nil {
		return ""
	}
	var ap struct {
		Status struct {
			Conditions []kube.Condition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &ap) != nil {
		return ""
	}
	if cond, ok := kube.FindCondition(ap.Status.Conditions, "Enforced"); ok {
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
func dnsPublished(c *kube.Client, host string) string {
	var items []struct {
		Spec struct {
			DNSName string `json:"dnsName"`
		} `json:"spec"`
		Status struct {
			Zones []struct {
				Conditions []kube.Condition `json:"conditions"`
			} `json:"zones"`
		} `json:"status"`
	}
	if c.ListInto(rhoai.NamespacedPath("ingress.operator.openshift.io/v1", "dnsrecords", "openshift-ingress", ""), &items) != nil {
		return ""
	}
	for _, rec := range items {
		if strings.TrimSuffix(rec.Spec.DNSName, ".") != host {
			continue
		}
		if len(rec.Status.Zones) == 0 {
			return "pending"
		}
		for _, z := range rec.Status.Zones {
			if cond, ok := kube.FindCondition(z.Conditions, "Published"); !ok || cond.Status != "True" {
				return "pending"
			}
		}
		return "published"
	}
	return "pending"
}

// KServeGateway is KServe's default gateway for LLMInferenceServices, read
// at run time from ConfigMap inferenceservice-config in the applications
// namespace, data key "ingress", field kserveIngressGateway: "namespace/name",
// or "name" in KServe's own namespace (rhoai-3.6 llmisvc NewConfig). A
// model whose spec.router.gateway is {} routes through it. Known is false
// when it cannot be read.
type KServeGateway struct {
	Namespace, Name string
	Known           bool
}

const kserveConfigMap = "inferenceservice-config"

func (g KServeGateway) String() string { return g.Namespace + "/" + g.Name }

// isInferenceGateway: the default is the gateway this tool manages, or
// unknown (then assumed to be, as on RHOAI 3.x: the safe side for guards).
func (g KServeGateway) isInferenceGateway() bool {
	return !g.Known || (g.Namespace == inferenceGatewayNamespace && g.Name == inferenceGatewayName)
}

// readKServeGateway reads KServe's default gateway. A missing ConfigMap or
// key is unknown, not an error; err is set when it cannot be read.
func readKServeGateway(c *kube.Client) (KServeGateway, error) {
	ns, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return KServeGateway{}, nil // no RHOAI, so no ConfigMap: unknown, as when it is missing
	}
	if err != nil {
		return KServeGateway{}, fmt.Errorf("find ConfigMap %s: %w", kserveConfigMap, err)
	}
	body, found, err := c.GetOptional(rhoai.NamespacedPath("v1", "configmaps", ns, kserveConfigMap))
	if err != nil {
		return KServeGateway{}, fmt.Errorf("read ConfigMap %s/%s: %w", ns, kserveConfigMap, err)
	}
	if !found {
		return KServeGateway{}, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	var ingress struct {
		KserveIngressGateway string `json:"kserveIngressGateway"`
	}
	if json.Unmarshal(body, &cm) != nil {
		return KServeGateway{}, fmt.Errorf("parse ConfigMap %s/%s", ns, kserveConfigMap)
	}
	if strings.TrimSpace(cm.Data["ingress"]) == "" {
		return KServeGateway{}, nil
	}
	if json.Unmarshal([]byte(cm.Data["ingress"]), &ingress) != nil {
		return KServeGateway{}, fmt.Errorf("parse ConfigMap %s/%s (ingress)", ns, kserveConfigMap)
	}
	name := strings.TrimSpace(ingress.KserveIngressGateway)
	if name == "" {
		return KServeGateway{}, nil
	}
	if gwNS, gwName, ok := strings.Cut(name, "/"); ok {
		return KServeGateway{Namespace: gwNS, Name: gwName, Known: true}, nil
	}
	return KServeGateway{Namespace: ns, Name: name, Known: true}, nil
}

// gatewayUsers lists the LLMInferenceServices ("namespace/name") that
// route through the inference gateway: spec.router.gateway {} while it is
// KServe's default gateway (def), or a ref to it.
func gatewayUsers(models []LLMISvcObject, def KServeGateway) []string {
	var users []string
	for _, m := range models {
		if m.routesThroughInferenceGateway(def) {
			users = append(users, m.Metadata.Namespace+"/"+m.Metadata.Name)
		}
	}
	sort.Strings(users)
	return users
}

// routedModels lists the models that route through the gateway: those
// gatewayUsers finds, and those whose address is on one of its hostnames
// (a model that inherits its route from a preset has no
// spec.router.gateway).
func routedModels(models []LLMISvcObject, def KServeGateway, gw GatewayObject) []LLMISvcObject {
	hosts := gw.hosts()
	var out []LLMISvcObject
	for _, m := range models {
		uses := m.routesThroughInferenceGateway(def)
		if !uses && m.Status.URL != "" {
			if u, err := url.Parse(m.Status.URL); err == nil && hosts[u.Hostname()] {
				uses = true
			}
		}
		if uses {
			out = append(out, m)
		}
	}
	return out
}

// routeNamespaces lists the namespaces of routedModels, sorted, without
// repeats.
func routeNamespaces(models []LLMISvcObject, def KServeGateway, gw GatewayObject) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range routedModels(models, def, gw) {
		if !seen[m.Metadata.Namespace] {
			seen[m.Metadata.Namespace] = true
			out = append(out, m.Metadata.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// notAllowedModels lists the models that route through gw (routedModels:
// the tool's or anyone's, such as a model deployed from the dashboard)
// whose routes its listeners do not admit, as "namespace/name", and their
// namespaces. A namespace is read only when the listeners select by
// labels; one that cannot be read is left out (unknown is not "no").
func notAllowedModels(c *kube.Client, models []LLMISvcObject, def KServeGateway, gw GatewayObject) (refs, namespaces []string) {
	_, _, byName := gw.allowedProjects()
	verdict := map[string]string{}
	for _, m := range routedModels(models, def, gw) {
		ns := m.Metadata.Namespace
		if _, done := verdict[ns]; !done {
			verdict[ns] = "unknown"
			var labels map[string]string
			readable := true
			if !byName {
				meta, ok, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", ns))
				readable = err == nil && ok
				labels = meta.Labels
			}
			if readable {
				verdict[ns] = gw.Admits(ns, labels)
				if verdict[ns] == "no" {
					namespaces = append(namespaces, ns)
				}
			}
		}
		if verdict[ns] == "no" {
			refs = append(refs, ns+"/"+m.Metadata.Name)
		}
	}
	sort.Strings(refs)
	sort.Strings(namespaces)
	return refs, namespaces
}

// foreignGatewayRefusal says that someone else's gateway does not let
// project's routes attach, and RHOAI's documented step.
func foreignGatewayRefusal(project string) string {
	return fmt.Sprintf("Gateway %s/%s, which this tool did not create, does not let routes from project %s attach (its listeners' allowedRoutes), and this tool does not change it. As RHOAI documents it, a cluster admin adds %s to the projects the Gateway's listeners allow: the values array of allowedRoutes.namespaces.selector (%s In [...]). Nothing was changed.", inferenceGatewayNamespace, inferenceGatewayName, project, project, namespaceNameLabel)
}

// InferenceGatewayStatus reports the inference gateway. models are the
// cluster's LLMInferenceServices (modelsErr when they cannot be listed).
func InferenceGatewayStatus(c *kube.Client, models []LLMISvcObject, modelsErr error) types.InferenceGatewayState {
	state := types.InferenceGatewayState{Namespace: inferenceGatewayNamespace, Name: inferenceGatewayName}
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
			if meta, ok, err := ReadGatewayObject(c, kind); err == nil && ok && gatewayOwned(meta) {
				state.ManagedByTool = true
			}
		}
		return state
	}
	state.Deployed = true
	state.ManagedByTool = gatewayOwned(gw.Metadata)
	state.Terminating = gw.Metadata.Terminating()
	if host := gw.externalHost(); host != "" {
		state.ExternalURL = "https://" + host
	}
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			state.Address = a.Value
			break
		}
	}
	accepted, hasAccepted := kube.FindCondition(gw.Status.Conditions, "Accepted")
	programmed, hasProgrammed := kube.FindCondition(gw.Status.Conditions, "Programmed")
	switch {
	case state.Terminating:
		state.Message = "Terminating"
	case hasAccepted && accepted.Status == "False":
		state.Message = kube.FirstNonEmpty(kube.TruncateText(accepted.Message, 300), accepted.Reason, "Not accepted by the gateway controller")
		state.WaitingReason = kube.FirstNonEmpty(accepted.Reason, "NotAccepted")
		state.TerminalError = true
	case hasProgrammed && programmed.Status == "True" && state.Address != "":
		state.Ready = true
		state.Message = "Running"
	case hasProgrammed && programmed.Status == "True":
		state.Message = "Programmed, waiting for an address"
	case hasProgrammed:
		state.Message = kube.FirstNonEmpty(kube.TruncateText(programmed.Message, 300), programmed.Reason, "Not programmed yet")
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
	// KServe's default gateway being another one is no problem: models
	// that choose this gateway name it in their refs (models_exposure.go).
	var warnings []string
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
	if names, all, known := gw.allowedProjects(); known {
		state.AllowedProjects, state.AllProjectsAllowed = names, all
	}
	if modelsErr == nil {
		state.NotAllowedModels, state.NotAllowedModelProjects = notAllowedModels(c, models, def, gw)
		if state.ManagedByTool && !state.Terminating {
			listedProjectsState(c, &state, gw, models, def)
		}
	}
	switch {
	case !state.ManagedByTool:
		state.TeardownBlockedReason = "This gateway was not created by this tool, so the tool does not change or delete it."
	case modelsErr != nil:
		state.TeardownBlockedReason = "Cannot check which models use the gateway: " + modelsErr.Error()
	case len(state.UsedBy) > 0:
		state.TeardownBlockedReason = fmt.Sprintf("Models route through it: %s. Delete %s first.", strings.Join(state.UsedBy, ", "), kube.Verb(len(state.UsedBy), "that model", "those models"))
	case gw.AttachedRoutes() > 0:
		state.TeardownBlockedReason = attachedRoutesReason(gw.AttachedRoutes())
	}
	return state
}

// otherGatewaysOfClass lists the Gateways (namespace/name) other than the
// inference gateway that use its GatewayClass. Gateways are listed
// cluster-wide, read-only: another one can be in any namespace.
func otherGatewaysOfClass(c *kube.Client) ([]string, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			GatewayClassName string `json:"gatewayClassName"`
		} `json:"spec"`
	}
	if err := c.ListInto(rhoai.ClusterPath("gateway.networking.k8s.io/v1", "gateways", ""), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the Gateways: %w", err)
		}
		return nil, err
	}
	var out []string
	for _, g := range items {
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
	return fmt.Sprintf("At least %d %s attached to the inference gateway (models, or routes made by hand; `oc get httproute -A` lists them). Delete them first.", n, kube.Verb(n, "route is", "routes are"))
}

// gatewayServiceWarning checks that the gateway's Service has the name the
// "https" listener's hostname assumes ("" when it does, or cannot be read).
func gatewayServiceWarning(c *kube.Client) string {
	body, found, err := c.GetOptional(rhoai.NamespacedPath("v1", "services", "openshift-ingress", "openshift-ai-inference-openshift-ai-inference"))
	missing := fmt.Sprintf("The gateway's Service is not named %s, so the in-cluster listener (%s) does not match it and callers inside the cluster (the dashboard's playground) may be refused. Report this with the output of `oc get svc -n %s -l %s=%s`.", inferenceGatewayService, inferenceGatewayInternalHost, inferenceGatewayNamespace, gatewayNameLabel, inferenceGatewayName)
	if err != nil {
		return ""
	}
	if !found {
		return missing
	}
	meta, err := kube.ParseObjectMeta(body)
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
	gateway GatewayObject
	// objects to apply, by kind, with what was read of each.
	objects map[string]map[string]any
	current map[string]kube.ObjectMeta
	found   map[string]bool
	// classForeign: the GatewayClass exists and is not this tool's (it is
	// used, never changed).
	classForeign bool
	host         string
	// add are the namespaces this write adds to the list; names the full
	// list it writes. listChanges: the write changes the Gateway's list
	// (or creates the Gateway).
	add         []string
	names       []string
	listChanges bool
	// remove are the namespaces this write takes off the list (never the
	// gateway's own namespace; inference_gateway_projects.go decides
	// which may go).
	remove []string
	// gatewayOnly: write the Gateway alone (a project added to the list).
	gatewayOnly bool
}

// planInferenceGateway reads everything a gateway write needs, for a list
// with the namespaces in add. The list keeps every name the listeners
// list now, whoever added it. A gateway an earlier build made (from All,
// or its namespace label) also gets the projects of this tool's own
// models that route through it (by its record), so none of them is
// detached; other projects with models on it become Allow candidates.
// When the models or the record cannot be read, that is refused. The list and the resourceVersion that guards the write
// come from the same read. A refusal is returned as a message and an
// error code; nothing is written.
func planInferenceGateway(c *kube.Client, add []string) (*gatewayPlan, string, string) {
	return planInferenceGatewayList(c, add, nil)
}

// planInferenceGatewayList is planInferenceGateway that also takes the
// namespaces in remove off the list (the gateway's own namespace stays).
// A namespace in remove where a model (anyone's) routes through the
// gateway, or whose models cannot be checked, refuses the plan: every
// plan checks it, so a write planned again after a conflict does too.
func planInferenceGatewayList(c *kube.Client, add, remove []string) (*gatewayPlan, string, string) {
	gw, found, err := readInferenceGateway(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read Gateway %s/%s; nothing was changed: %v.%s", inferenceGatewayNamespace, inferenceGatewayName, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
	}
	if found && !gatewayOwned(gw.Metadata) {
		return &gatewayPlan{foreign: true, gateway: gw}, "", ""
	}
	if found && gw.Metadata.Terminating() {
		return nil, fmt.Sprintf("Gateway %s/%s is still being deleted. Wait until it is gone, then set it up again.", inferenceGatewayNamespace, inferenceGatewayName), "terminating"
	}
	for _, project := range remove {
		if !found || project == inferenceGatewayNamespace {
			continue
		}
		users, err := gatewayRoutedIn(c, gw, project)
		if err != nil {
			return nil, fmt.Sprintf("Cannot check which models in %s use the inference gateway, so it stays on its list: %v.%s", project, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
		}
		if len(users) > 0 {
			return nil, fmt.Sprintf("%s stays on the inference gateway's list: %s %s through it.", project, strings.Join(users, ", "), kube.Verb(len(users), "routes", "route")), "in_use"
		}
	}
	plan := &gatewayPlan{current: map[string]kube.ObjectMeta{}, found: map[string]bool{}, gateway: gw, add: add, remove: remove}
	listed, shaped := gw.listedNamespaces()
	var routed []string
	if found && !shaped {
		models, err := ListLLMInferenceServices(c)
		if err != nil && !errors.Is(err, ErrLLMISvcAPIMissing) {
			return nil, fmt.Sprintf("Cannot check which models use the inference gateway, so its list of allowed projects was not written (that could cut a model off); nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
		}
		// Only this tool's own models (by its record): anyone may make an
		// LLMInferenceService that points at the gateway, and listing its
		// project would let that project's editors attach routes to the
		// shared gateway. The others are Allow candidates in the status.
		rec, err := ReadModelsRecord(c)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read this tool's record of its models, so the inference gateway's list of allowed projects was not written (that could cut one of its models off); nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
		}
		var ours []LLMISvcObject
		for _, m := range models {
			if rec.Owns(m.Metadata) {
				ours = append(ours, m)
			}
		}
		def, _ := readKServeGateway(c)
		routed = routeNamespaces(ours, def, gw)
	}
	all := gatewayNamespaceList(listed, routed, add)
	plan.names = all[:0:0]
	for _, n := range all {
		if n == inferenceGatewayNamespace || !slices.Contains(remove, n) {
			plan.names = append(plan.names, n)
		}
	}
	plan.listChanges = !found || !shaped
	for _, n := range plan.names {
		plan.listChanges = plan.listChanges || !slices.Contains(listed, n)
	}
	for _, n := range listed {
		plan.listChanges = plan.listChanges || !slices.Contains(plan.names, n)
	}
	for _, kind := range gatewayKinds {
		meta, ok, err := gw.Metadata, found, error(nil)
		if kind != "Gateway" {
			meta, ok, err = ReadGatewayObject(c, kind)
		}
		if err != nil {
			return nil, fmt.Sprintf("Cannot read %s; nothing was changed: %v.%s", gatewayDesc(kind), err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
		}
		plan.current[kind], plan.found[kind] = meta, ok
		if !ok || gatewayOwned(meta) {
			if ok && meta.Terminating() {
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
			return nil, fmt.Sprintf("Cannot read GatewayClass %s; nothing was changed: %v.", inferenceGatewayClass, err), kube.ErrorCodeFromK8sErr(err)
		}
		if controller != gatewayControllerName {
			return nil, fmt.Sprintf("GatewayClass %s exists, was not created by this tool, and belongs to controller %q instead of OpenShift's (%s). Nothing was changed.", inferenceGatewayClass, controller, gatewayControllerName), "not_managed"
		}
		plan.classForeign = true
	}
	domain, err := clusterAppsDomain(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's apps domain for the gateway's hostname; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites")
	}
	cert, err := defaultIngressCertificate(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's default ingress certificate; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
	}
	plan.objects = inferenceGatewayObjects(domain, cert, plan.names)
	plan.host = inferenceHostPrefix + domain
	return plan, "", ""
}

// planGatewayAllow plans adding project to the list of this tool's
// gateway, writing the Gateway alone. It returns nil without a refusal
// when there is nothing to write: the gateway lists the project already,
// or does not exist. Someone else's gateway is returned as foreign.
func planGatewayAllow(c *kube.Client, project string) (*gatewayPlan, string, string) {
	plan, msg, code := planInferenceGateway(c, []string{project})
	if msg != "" || plan.foreign {
		return plan, msg, code
	}
	if !plan.found["Gateway"] || !plan.listChanges {
		return nil, "", ""
	}
	plan.gatewayOnly = true
	return plan, "", ""
}

// writeInferenceGateway applies the plan. When the Gateway changed after
// the plan read it (someone else edited its list), the write is refused
// by its resourceVersion; it is planned again from a fresh read, which
// keeps their edit, up to three times. It returns the plan it wrote, and
// a refusal message and an error code, or "".
func writeInferenceGateway(c *kube.Client, plan *gatewayPlan, logf func(string)) (*gatewayPlan, string, string) {
	for attempt := 1; ; attempt++ {
		msg, code, changed := applyInferenceGateway(c, plan, false, logf)
		if !changed {
			return plan, msg, code
		}
		if attempt == 3 {
			return plan, fmt.Sprintf("Gateway %s/%s kept changing while this ran (someone else is editing it), so its list of allowed projects was not written. Try again.", inferenceGatewayNamespace, inferenceGatewayName), "conflict"
		}
		logf("The gateway changed while this ran; reading it again.")
		next, msg, code := planInferenceGatewayList(c, plan.add, plan.remove)
		if msg != "" {
			return plan, msg, code
		}
		if next.foreign {
			return plan, fmt.Sprintf("Gateway %s/%s was replaced by one this tool did not create while this ran, so it was left alone.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed"
		}
		next.gatewayOnly = plan.gatewayOnly
		plan = next
	}
}

// applyInferenceGateway writes the plan's objects (a server-side dry run
// of all of them first, when dryRun). It returns a refusal message and an
// error code, or "", and changed when the Gateway was changed by someone
// else after the plan read it (nothing of the Gateway was written).
func applyInferenceGateway(c *kube.Client, plan *gatewayPlan, dryRun bool, logf func(string)) (string, string, bool) {
	var applied []string
	for _, kind := range gatewayKinds {
		if kind == "GatewayClass" && plan.classForeign || plan.gatewayOnly && kind != "Gateway" {
			continue
		}
		path := gatewayObjectPath(kind)
		createPath, _ := gatewayCreatePath(kind)
		k := kind
		read := func() (kube.ObjectMeta, bool, error) { return ReadGatewayObject(c, k) }
		if kind == "Gateway" {
			// The list was computed from what the plan read: a write
			// refused because the Gateway changed is planned again, never
			// retried with the old list.
			read = func() (kube.ObjectMeta, bool, error) {
				meta, ok, err := ReadGatewayObject(c, k)
				if err == nil {
					err = errGatewayChanged
				}
				return meta, ok, err
			}
		}
		action, meta, found, err := envkit.EnsureToolObject(c, gatewayDesc(kind), path, createPath, plan.objects[kind], plan.current[kind], plan.found[kind], read, gatewayOwned, dryRun)
		plan.current[kind], plan.found[kind] = meta, found
		if err != nil {
			if errors.Is(err, errGatewayChanged) && !dryRun {
				return "", "", true
			}
			code := kube.ErrorCodeFromK8sErr(err)
			var foreign *envkit.ForeignObjectError
			switch {
			case errors.As(err, &foreign):
				code = "not_managed"
			case errors.Is(err, envkit.ErrObjectChanged) || errors.Is(err, errGatewayChanged) || kube.IsK8sConflict(err):
				code = "conflict"
			}
			if dryRun {
				return fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", gatewayDesc(kind), err, kube.TemplateHint(err)), code, false
			}
			done := "nothing"
			if len(applied) > 0 {
				done = strings.Join(applied, ", ")
			}
			return fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. Set up again to retry, or delete the gateway to remove it.%s", gatewayDesc(kind), err, done, kube.TemplateHint(err)), code, false
		}
		if !dryRun {
			applied = append(applied, gatewayDesc(kind))
			logf(fmt.Sprintf("OK: %s %s", gatewayDesc(kind), action))
		}
	}
	return "", "", false
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
func waitInferenceGateway(c *kube.Client, host string, progress func(string)) (ready bool, dns string, problem string, pending string, err error) {
	last := ""
	ready, err = kube.WaitForGone(c, inferenceGatewayReadyTimeout, inferenceGatewayPoll, func(cc *kube.Client) (bool, error) {
		gw, found, err := readInferenceGateway(cc)
		if err != nil || !found {
			return false, err
		}
		accepted, hasAccepted := kube.FindCondition(gw.Status.Conditions, "Accepted")
		if hasAccepted && accepted.Status == "False" {
			problem = fmt.Sprintf("The gateway controller does not accept the gateway: %s.", kube.FirstNonEmpty(accepted.Message, accepted.Reason))
			return true, nil
		}
		programmed, ok := kube.FindCondition(gw.Status.Conditions, "Programmed")
		addr := ""
		for _, a := range gw.Status.Addresses {
			addr = kube.FirstNonEmpty(addr, a.Value)
		}
		msg := "Waiting for the gateway controller..."
		switch {
		case ok && programmed.Status == "True" && addr != "":
			return true, nil
		case ok && programmed.Status == "True":
			msg = "Programmed; waiting for the load balancer's address..."
		case ok:
			msg = "Waiting for OpenShift to create the gateway's load balancer (usually under a minute)..."
			pending = kube.FirstNonEmpty(programmed.Message, programmed.Reason)
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
	published, werr := kube.WaitForGone(c, inferenceGatewayDNSTimeout, inferenceGatewayPoll, func(cc *kube.Client) (bool, error) {
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
func TeardownInferenceGateway(c *kube.Client, modelsJustDeleted bool) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) *types.OperationResponse {
		record.Action(c, "teardown-inference-gateway", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}
	}
	type current struct {
		meta  kube.ObjectMeta
		found bool
	}
	objs := map[string]current{}
	ours := 0
	for _, kind := range gatewayKinds {
		meta, found, err := ReadGatewayObject(c, kind)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read %s; nothing was deleted: %v.%s", gatewayDesc(kind), err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "read failed"), nil
		}
		objs[kind] = current{meta, found}
		if found && gatewayOwned(meta) {
			ours++
		}
	}
	if gw := objs["Gateway"]; gw.found && !gatewayOwned(gw.meta) {
		return fail(fmt.Sprintf("Gateway %s/%s was not created by this tool, so it was not deleted.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed", "gateway not managed"), nil
	}
	if ours == 0 {
		logs = append(logs, "No inference gateway objects of this tool exist")
		return &types.OperationResponse{Success: true, Message: "The inference gateway is not set up; nothing to delete.", Logs: logs}, nil
	}
	// Never take the gateway away from a model: fail closed when the
	// models cannot be listed.
	models, err := ListLLMInferenceServices(c)
	if err != nil && !errors.Is(err, ErrLLMISvcAPIMissing) {
		return fail(fmt.Sprintf("Cannot check which models use the gateway, so it was not deleted: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "model list failed"), nil
	}
	// A model on the default route uses this gateway unless KServe's
	// default is known to be another one (unreadable counts as this one).
	def, _ := readKServeGateway(c)
	if users := gatewayUsers(models, def); len(users) > 0 {
		return fail(fmt.Sprintf("Models route through the inference gateway: %s. Delete %s first; nothing was deleted.", strings.Join(users, ", "), kube.Verb(len(users), "that model", "those models")), "in_use", "in use"), nil
	}
	// The gateway controller's count of attached routes also sees models
	// that inherit their route from a preset, and routes made by hand.
	// Fail closed when it cannot be read.
	if gw := objs["Gateway"]; gw.found {
		current, found, err := readInferenceGateway(c)
		if err == nil && found && current.AttachedRoutes() > 0 && modelsJustDeleted {
			// The controller updates the count after the routes go.
			logs = append(logs, "Waiting for the gateway controller to stop counting the deleted models' routes...")
			_, err = kube.WaitForGone(c, inferenceGatewayRoutesTimeout, inferenceGatewayPoll, func(cc *kube.Client) (bool, error) {
				g, ok, err := readInferenceGateway(cc)
				if err == nil {
					current, found = g, ok
				}
				return err == nil && (!ok || g.AttachedRoutes() == 0), err
			})
		}
		if err != nil {
			return fail(fmt.Sprintf("Cannot read the gateway's attached routes, so it was not deleted: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "gateway read failed"), nil
		}
		if found && current.AttachedRoutes() > 0 {
			return fail(attachedRoutesReason(current.AttachedRoutes())+" Nothing was deleted.", "in_use", "routes attached"), nil
		}
	}
	var deleted, kept []string
	for _, kind := range slices.Backward(gatewayKinds) {

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
				why = fmt.Sprintf("Gateway %s %s it", strings.Join(users, ", Gateway "), kube.Verb(len(users), "uses", "use"))
			}
			if why != "" {
				msg := fmt.Sprintf("GatewayClass %s was kept: %s.", inferenceGatewayClass, why)
				logs = append(logs, msg)
				kept = append(kept, msg)
				continue
			}
		}
		path := gatewayObjectPath(kind)
		logs = append(logs, fmt.Sprintf("Deleting %s...", gatewayDesc(kind)))
		if _, err := kube.DeleteWithUID(c, path, cur.meta.UID); err != nil && !kube.IsK8sNotFound(err) {
			done := ""
			if len(deleted) > 0 {
				done = " Deleted before: " + strings.Join(deleted, ", ") + "."
			}
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to delete %s: %v.%s Delete again to retry.%s", gatewayDesc(kind), err, done, kube.TemplateHint(err)), code, "delete failed"), nil
		}
		deleted = append(deleted, gatewayDesc(kind))
		logs = append(logs, fmt.Sprintf("OK: %s deleted", gatewayDesc(kind)))
		if kind == "Gateway" {
			// The class and options go after the Gateway, so its
			// controller removes the load balancer first.
			gone, werr := kube.WaitForDeletion(c, path, inferenceGatewayDeleteTimeout, inferenceGatewayPoll)
			if werr == nil && !gone {
				logs = append(logs, "The Gateway is still being deleted")
			}
		}
	}
	// The list went with the Gateway: nothing recorded is on a list any more.
	if slices.Contains(deleted, gatewayDesc("Gateway")) {
		if err := WriteGatewayProjects(c, nil); err != nil {
			slog.Warn("cannot clear the gateway projects record", "error", err)
		}
	}
	slog.Info("inference gateway teardown", "user", kube.GetUser(c))
	record.Action(c, "teardown-inference-gateway", strings.Join(deleted, ", "), true)
	return &types.OperationResponse{
		Success: true,
		Message: strings.TrimSpace("Inference gateway deleted: " + kube.JoinTitles(deleted) + ". OpenShift removes its load balancer and the ingress operator its DNS record. " + strings.Join(kept, " ")),
		Logs:    logs,
	}, nil
}
