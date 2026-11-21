package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The platform pieces MaaS needs besides its own: the Kuadrant CR,
// Authorino's TLS, the AI gateway's infrastructure namespace, and the
// gateway policies an earlier, tier-based MaaS left behind.

var (
	// MaaSKuadrantTimeout bounds the wait for the Kuadrant CR to be Ready
	// and for its Authorino to appear.
	MaaSKuadrantTimeout = 5 * time.Minute
)

// --- the Kuadrant CR ------------------------------------------------------------------

// kuadrantObject is the Kuadrant CR as platform-setup.md creates it (name
// kuadrant, no spec) at the version that serves kuadrants.
func kuadrantObject(version, ns string) map[string]any {
	return map[string]any{
		"apiVersion": kuadrantGroup + "/" + version,
		"kind":       "Kuadrant",
		"metadata":   map[string]any{"name": "kuadrant", "namespace": ns, "labels": toolLabels()},
	}
}

// kuadrantServedVersion is the version of kuadrant.io that serves
// kuadrants ("" while none does: Connectivity Link's CRDs are not there).
// The group's preferred version is not it: RHCL 1.4 serves AuthPolicy in v1
// and Kuadrant only in v1beta1 (seen live).
func kuadrantServedVersion(c *Client) (string, error) {
	read, err := listServed(c, kuadrantGroup, "/apis/kuadrant.io/%s/kuadrants", nil, []string{"v1beta1"})
	if err != nil {
		return "", err
	}
	if read.Body == nil {
		return "", nil
	}
	return read.Version, nil
}

// createKuadrant creates the Kuadrant CR in ns (a server-side dry run with
// dryRun). One that exists (made meanwhile) is fine.
func createKuadrant(c *Client, version, ns string, dryRun bool) error {
	data, _ := json.Marshal(kuadrantObject(version, ns))
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	_, _, err := c.do(http.MethodPost, withToolFieldManager("/apis/kuadrant.io/"+version+"/namespaces/"+ns+"/kuadrants"), "application/json", data, query)
	if isK8sConflict(err) {
		return nil
	}
	return err
}

// --- the infrastructure namespace -----------------------------------------------------

// maasInfraNamespacePath is the path of an AI gateway infrastructure
// namespace this tool may create and delete: only the two names the docs'
// scripts derive (derive_infra_namespace in scripts/setup-database.sh), so
// the RBAC delete rule names them; never the applications namespace.
func maasInfraNamespacePath(ns string) (string, bool) {
	switch ns {
	case "redhat-ai-gateway-infra":
		return "/api/v1/namespaces/redhat-ai-gateway-infra", true
	case "odh-ai-gateway-infra":
		return "/api/v1/namespaces/odh-ai-gateway-infra", true
	}
	return "", false
}

// createInfraNamespace creates the infrastructure namespace as the MaaS
// docs' fresh-install script does (scripts/setup-database.sh, "Ensure
// infrastructure namespace exists": kubectl create namespace
// "$INFRA_NAMESPACE"), before MaaS is turned on. RHOAI's AI gateway
// operator only creates it once modelsAsAService is Managed
// (ai-gateway-operator rhoai-3.6 internal/controller/aigateway/
// aigateway_infra_rbac.go, ensureInfraSecretMigrationRBAC ->
// ensureNamespace), and MaaS is turned on last, after the database that
// lives there: without this, setup could never run on a cluster that never
// had MaaS. The tool's labels mark it as the tool's.
func createInfraNamespace(c *Client, ns string, dryRun bool) error {
	if _, ok := maasInfraNamespacePath(ns); !ok {
		return fmt.Errorf("namespace %s is not one this tool creates", ns)
	}
	data, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns, "labels": toolLabels()}})
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	_, _, err := c.do(http.MethodPost, withToolFieldManager("/api/v1/namespaces"), "application/json", data, query)
	return err
}

// infraNamespaceLeftovers names what is still in the infrastructure
// namespace that this tool did not make, as far as it can list: pods,
// Deployments and PersistentVolumeClaims (the SA lists no other kind there),
// and a maas-db-config this tool did not write. Removal deletes the
// namespace only when this finds nothing; a read that fails is returned
// (fail closed).
func infraNamespaceLeftovers(c *Client, ns string) ([]string, error) {
	var out []string
	for _, l := range []struct{ kind, path string }{
		{"pod", namespacedPath("v1", "pods", ns, "")},
		{"Deployment", namespacedPath("apps/v1", "deployments", ns, "")},
		{"PersistentVolumeClaim", namespacedPath("v1", "persistentvolumeclaims", ns, "")},
	} {
		var items []struct {
			Metadata objectMeta `json:"metadata"`
		}
		if err := c.listInto(l.path, &items); err != nil {
			if isDecodeError(err) {
				return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
			}
			return nil, fmt.Errorf("list the %ss in %s: %w", l.kind, ns, err)
		}
		for _, it := range items {
			out = append(out, l.kind+" "+it.Metadata.Name)
		}
	}
	meta, found, err := readObjectMeta(c, maasObjectPath("Secret", ns))
	if err != nil {
		return nil, fmt.Errorf("read Secret %s/%s: %w", ns, maasDBConfigSecret, err)
	}
	if found && !toolOwned(meta) {
		out = append(out, "Secret "+maasDBConfigSecret)
	}
	sort.Strings(out)
	return out, nil
}

// --- Authorino TLS --------------------------------------------------------------------

// authorinoServiceCAFile is where OpenShift projects the service CA into
// every pod (tls-configuration.md: the openshift-service-ca.crt ConfigMap
// in the service account volume); setup-authorino-tls.sh points
// SSL_CERT_FILE and REQUESTS_CA_BUNDLE at it.
const authorinoServiceCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"

// authorinoDefaultSecret is the certificate Secret setup-authorino-tls.sh
// names; an Authorino CR that already names one keeps its own.
const authorinoDefaultSecret = "authorino-server-cert"

// configureAuthorinoTLS applies what scripts/setup-authorino-tls.sh does to
// Kuadrant's Authorino in ns (tls-configuration.md, "Gateway -> Authorino"
// and "Authorino -> maas-api"), only what is not already so, so running it
// again changes nothing:
//  1. Service authorino-authorino-authorization: annotation
//     service.beta.openshift.io/serving-cert-secret-name=<secret>, so
//     service-ca makes the certificate (it also makes it again after
//     authorino-operator recreated the Service, which the RHCL 1.4.4
//     upgrade does: service-ca deletes the old Service's Secret, and
//     Authorino's pods then cannot mount it, seen live 2026-11-18);
//  2. Authorino CR authorino: spec.listener.tls enabled, certSecretRef
//     <secret>;
//  3. Deployment authorino: SSL_CERT_FILE and REQUESTS_CA_BUNDLE at the
//     service CA ("set env"; the CR has no env field), unless it already
//     trusts the service CA another way (authorinoTrustsServiceCA);
//  4. then, as the script's closing note and deploy.sh do, a rollout
//     restart of authorino.
//
// The secret is the one the CR names, else the script's. A dry run sends
// every patch with dryRun=All and restarts nothing. It returns the changes
// made (none when all was set).
func configureAuthorinoTLS(c *Client, ns string, dryRun bool) ([]string, error) {
	read, err := getServed(c, authorinoGroup, "/apis/"+authorinoGroup+"/%s/namespaces/"+ns+"/authorinos/authorino", []string{"v1beta1"})
	if isK8sNotFound(err) || (err == nil && read.Body == nil) {
		return nil, fmt.Errorf("%w: no Authorino authorino in %s", errAuthorinoNotYet, ns)
	}
	if err != nil {
		return nil, fmt.Errorf("read Authorino %s/authorino: %w", ns, err)
	}
	var cr struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Listener struct {
				TLS struct {
					Enabled       bool `json:"enabled"`
					CertSecretRef struct {
						Name string `json:"name"`
					} `json:"certSecretRef"`
				} `json:"tls"`
			} `json:"listener"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(read.Body, &cr); err != nil {
		return nil, fmt.Errorf("parse Authorino %s/authorino: %w", ns, err)
	}
	secret := firstNonEmpty(cr.Spec.Listener.TLS.CertSecretRef.Name, authorinoDefaultSecret)
	if len(secret) > 253 || !dns1123Subdomain.MatchString(secret) {
		return nil, fmt.Errorf("authorino's CR names its certificate Secret %q, which is not a valid Kubernetes name", truncateText(secret, 300))
	}
	query := url.Values{"fieldManager": {toolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	var changes []string
	svcPath := namespacedPath("v1", "services", ns, "authorino-authorino-authorization")
	svc, found, err := readObjectMeta(c, svcPath)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read Service %s/authorino-authorino-authorization: %w", ns, err)
	case !found:
		return nil, fmt.Errorf("%w: no Service %s/authorino-authorino-authorization", errAuthorinoNotYet, ns)
	case svc.Annotations[servingCertAnnotation] != secret:
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{servingCertAnnotation: secret}}})
		if _, _, err := c.do(http.MethodPatch, namespacedPath("v1", "services", ns, "authorino-authorino-authorization"), "application/merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("annotate Service %s/authorino-authorino-authorization: %w", ns, err)
		}
		changes = append(changes, fmt.Sprintf("Service authorino-authorino-authorization asks service-ca for Secret %s", secret))
	}
	if !cr.Spec.Listener.TLS.Enabled || cr.Spec.Listener.TLS.CertSecretRef.Name != secret {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"listener": map[string]any{"tls": map[string]any{
			"enabled": true, "certSecretRef": map[string]any{"name": secret}}}}})
		if _, _, err := c.do(http.MethodPatch, "/apis/"+authorinoGroup+"/"+read.Version+"/namespaces/"+ns+"/authorinos/authorino", "application/merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("enable TLS on Authorino %s/authorino: %w", ns, err)
		}
		changes = append(changes, "Authorino's listener serves TLS with "+secret)
	}
	body, exists, err := c.getOptional(namespacedPath("apps/v1", "deployments", ns, "authorino"))
	if err != nil {
		return changes, fmt.Errorf("read Deployment %s/authorino: %w", ns, err)
	}
	if !exists {
		return changes, fmt.Errorf("%w: no Deployment %s/authorino", errAuthorinoNotYet, ns)
	}
	if !authorinoTrustsServiceCA(body) {
		var d struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string `json:"name"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(body, &d)
		var containers []any
		for _, ct := range d.Spec.Template.Spec.Containers {
			containers = append(containers, map[string]any{"name": ct.Name, "env": []any{
				map[string]any{"name": "SSL_CERT_FILE", "value": authorinoServiceCAFile},
				map[string]any{"name": "REQUESTS_CA_BUNDLE", "value": authorinoServiceCAFile},
			}})
		}
		if len(containers) == 0 {
			return changes, fmt.Errorf("deployment %s/authorino has no containers", ns)
		}
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}})
		if _, _, err := c.do(http.MethodPatch, namespacedPath("apps/v1", "deployments", ns, "authorino"), "application/strategic-merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("set SSL_CERT_FILE on Deployment %s/authorino: %w", ns, err)
		}
		changes = append(changes, "Authorino trusts the service CA (SSL_CERT_FILE and REQUESTS_CA_BUNDLE)")
	}
	if len(changes) > 0 && !dryRun {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{restartAnnotation: clk.Now().UTC().Format(time.RFC3339)},
		}}}})
		if _, _, err := c.do(http.MethodPatch, namespacedPath("apps/v1", "deployments", ns, "authorino"), "application/merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("restart Deployment %s/authorino: %w", ns, err)
		}
		changes = append(changes, "Authorino restarted to use it")
	}
	return changes, nil
}

// --- gateway policies of the tier-based MaaS ------------------------------------------

// maasControllerLabel marks what maas-controller made (models-as-a-service
// maas-controller: "app.kubernetes.io/managed-by": "maas-controller" on its
// gateway AuthPolicy maas-gateway-auth and its TokenRateLimitPolicy
// gateway-default-deny).
const maasControllerLabel = "maas-controller"

// gatewayPolicy is a Kuadrant policy in openshift-ingress that targets the
// MaaS gateway.
type gatewayPolicy struct {
	Kind, Name, UID, Version string
	Managed                  bool // maas-controller's
	Enforced                 string
}

// legacyTierPolicyPath is the path of a gateway policy the tier-based MaaS
// (RHOAI 3.2) made and RHOAI does not remove (migration/upgrade-to-3.4.md,
// "Phase 4: Manual Cleanup of Old Tier Resources": AuthPolicy
// gateway-auth-policy and TokenRateLimitPolicy gateway-tier-rate-limits in
// openshift-ingress); only those names, so the RBAC delete rule names them.
func legacyTierPolicyPath(kind, version string) string {
	switch kind {
	case "AuthPolicy":
		return "/apis/kuadrant.io/" + version + "/namespaces/openshift-ingress/authpolicies/gateway-auth-policy"
	case "TokenRateLimitPolicy":
		return "/apis/kuadrant.io/" + version + "/namespaces/openshift-ingress/tokenratelimitpolicies/gateway-tier-rate-limits"
	}
	return ""
}

// legacyTierPolicy: a documented tier-era policy this tool may remove.
func legacyTierPolicy(p gatewayPolicy) bool {
	return !p.Managed && ((p.Kind == "AuthPolicy" && p.Name == "gateway-auth-policy") || (p.Kind == "TokenRateLimitPolicy" && p.Name == "gateway-tier-rate-limits"))
}

// maasGatewayPolicies lists the AuthPolicies, TokenRateLimitPolicies and
// RateLimitPolicies in openshift-ingress that target the MaaS gateway, each
// at the version of kuadrant.io that serves it. A kind that is not served
// lists nothing; any other failure is returned.
func maasGatewayPolicies(c *Client) ([]gatewayPolicy, error) {
	var out []gatewayPolicy
	for _, l := range []struct {
		kind, path string
		fallback   []string
	}{
		{"AuthPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/authpolicies", []string{"v1"}},
		{"TokenRateLimitPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/tokenratelimitpolicies", []string{"v1alpha1"}},
		{"RateLimitPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/ratelimitpolicies", []string{"v1"}},
	} {
		read, err := listServed(c, kuadrantGroup, l.path, nil, l.fallback)
		if err != nil {
			return nil, fmt.Errorf("list %ss in %s: %w", l.kind, inferenceGatewayNamespace, err)
		}
		if read.Body == nil {
			continue
		}
		var list struct {
			Items []struct {
				Metadata objectMeta `json:"metadata"`
				Spec     struct {
					TargetRef struct {
						Kind string `json:"kind"`
						Name string `json:"name"`
					} `json:"targetRef"`
				} `json:"spec"`
				Status struct {
					Conditions []k8sCondition `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(read.Body, &list); err != nil {
			return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
		}
		for _, it := range list.Items {
			if it.Spec.TargetRef.Kind != "Gateway" || it.Spec.TargetRef.Name != maasGatewayName {
				continue
			}
			p := gatewayPolicy{Kind: l.kind, Name: it.Metadata.Name, UID: it.Metadata.UID, Version: read.Version, Managed: it.Metadata.Labels[managedByLabelKey] == maasControllerLabel}
			if cond, ok := findCondition(it.Status.Conditions, "Enforced"); ok {
				p.Enforced = cond.Status
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+"/"+out[i].Name < out[j].Kind+"/"+out[j].Name })
	return out, nil
}

// foreignGatewayPolicies are the policies on the MaaS gateway that
// maas-controller does not manage. With both old and new gateway
// policies, "Kuadrant behavior is undefined" (upgrade-to-3.4.md,
// "Duplicate gateway policies after upgrade").
func foreignGatewayPolicies(policies []gatewayPolicy) []gatewayPolicy {
	var out []gatewayPolicy
	for _, p := range policies {
		if !p.Managed {
			out = append(out, p)
		}
	}
	return out
}

func policyNames(ps []gatewayPolicy) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Kind+" "+p.Name)
	}
	return out
}

// --- commands for a cluster-admin who prefers the CLI ---------------------------------

// rhclInstallCommands are platform-setup.md's RHOAI install of Connectivity
// Link and its Kuadrant CR, on channel.
func rhclInstallCommands(channel string) string {
	return fmt.Sprintf(`oc apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: kuadrant-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kuadrant-operator
  namespace: kuadrant-system
spec:
  channel: %s
  installPlanApproval: Automatic
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  config:
    env:
    - name: RATELIMIT_CHECK_SERVICE_FAILURE_MODE
      value: deny
    - name: RATELIMIT_REPORT_SERVICE_FAILURE_MODE
      value: deny
EOF
oc wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/kuadrant-operator -n kuadrant-system --timeout=300s`, yamlString(channel))
}

// kuadrantCommands create the Kuadrant CR as platform-setup.md does.
func kuadrantCommands(ns string) string {
	return fmt.Sprintf(`oc apply -f - <<'EOF'
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: %s
EOF`, yamlString(ns))
}

// cnpgInstallCommands install CloudNativePG as this tool does.
func cnpgInstallCommands(channel string) string {
	return fmt.Sprintf(`oc apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: cnpg-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: cloudnative-pg
  namespace: cnpg-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: cloudnative-pg
  namespace: cnpg-system
spec:
  channel: %s
  installPlanApproval: Automatic
  name: cloudnative-pg
  source: certified-operators
  sourceNamespace: openshift-marketplace
EOF`, yamlString(channel))
}

// rhclDenyEnvCommand is platform-setup.md's patch that adds the
// deny-on-failure env to a Subscription, in the form that fits its shape
// (the docs give one with spec.config, and one without).
func rhclDenyEnvCommand(sub *olmSubscription) string {
	ref := fmt.Sprintf("oc patch subscription %s -n %s --type='json' -p=", shellQuote(sub.Metadata.Name), shellQuote(sub.Metadata.Namespace))
	if _, hasEnv := sub.Spec.Config["env"]; hasEnv {
		return ref + `'[
  {"op":"add","path":"/spec/config/env/-","value":{"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"}},
  {"op":"add","path":"/spec/config/env/-","value":{"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}}
]'`
	}
	if sub.Spec.Config != nil {
		return ref + `'[
  {"op":"add","path":"/spec/config/env","value":[
    {"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},
    {"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}
  ]}
]'`
	}
	return ref + `'[
  {"op":"add","path":"/spec/config","value":{"env":[
    {"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},
    {"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}
  ]}}
]'`
}

// rhclChannelCommand switches a Subscription's channel.
func rhclChannelCommand(ns, name, channel string) string {
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"channel": channel}})
	return fmt.Sprintf("oc patch subscription %s -n %s --type=merge -p %s", shellQuote(name), shellQuote(ns), shellQuote(string(patch)))
}

// yamlString is s as a YAML scalar for a shown heredoc: a JSON string,
// which YAML reads as a double-quoted scalar. Values read from the cluster
// (a catalog's channel names) go into commands a cluster-admin pastes into
// a shell: quoted and escaped, a newline cannot end the heredoc and run
// what follows.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// commentText is s for a shown "# ..." comment line: control characters
// (a newline would end the comment) become spaces.
func commentText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// legacyPolicyCommands delete the tier-era gateway policies
// (upgrade-to-3.4.md Phase 4).
func legacyPolicyCommands(ps []gatewayPolicy) string {
	var lines []string
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("oc delete %s %s -n %s", strings.ToLower(p.Kind), shellQuote(p.Name), inferenceGatewayNamespace))
	}
	return strings.Join(lines, "\n")
}

// maasDSCCommand turns on the AI gateway and MaaS in the DataScienceCluster
// (maas-setup.md "Configure DataScienceCluster").
func maasDSCCommand(dsc string) string {
	return fmt.Sprintf(`oc patch datasciencecluster %s --type=merge -p '{"spec":{"components":{"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"}}}}}'`, shellQuote(firstNonEmpty(dsc, "default-dsc")))
}

// gatewayAllowedRoutesCommand narrows a gateway someone else made from
// every namespace to the names the MaaS docs' setup-gateway.sh lists
// (build_allowed_routes_yaml: kubernetes.io/metadata.name In [...]), one
// JSON patch operation per listener that admits all.
func gatewayAllowedRoutesCommand(g gatewayObject, namespaces []string) string {
	var ops []any
	values := make([]any, len(namespaces))
	for i, n := range namespaces {
		values[i] = n
	}
	for i, l := range g.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From != "All" {
			continue
		}
		ops = append(ops, map[string]any{"op": "replace", "path": fmt.Sprintf("/spec/listeners/%d/allowedRoutes/namespaces", i), "value": map[string]any{
			"from": "Selector",
			"selector": map[string]any{"matchExpressions": []any{map[string]any{
				"key": "kubernetes.io/metadata.name", "operator": "In", "values": values,
			}}},
		}})
	}
	data, _ := json.Marshal(ops)
	return fmt.Sprintf("oc patch gateway %s -n %s --type=json -p %s", maasGatewayName, inferenceGatewayNamespace, shellQuote(string(data)))
}

// admitsAll: a listener of the gateway admits routes from every namespace.
func (g gatewayObject) admitsAll() bool {
	for _, l := range g.Spec.Listeners {
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From == "All" {
			return true
		}
	}
	return false
}
