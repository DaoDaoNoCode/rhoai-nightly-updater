package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The platform pieces MaaS needs besides its own: the Kuadrant CR,
// Authorino's TLS, the AI gateway's infrastructure namespace, and the
// gateway policies an earlier, tier-based MaaS left behind.

var (
	// maasKuadrantTimeout bounds the wait for the Kuadrant CR to be Ready
	// and for its Authorino to appear.
	maasKuadrantTimeout = 5 * time.Minute
)

// --- the Kuadrant CR ------------------------------------------------------------------

// kuadrantObject is the Kuadrant CR as platform-setup.md creates it (name
// kuadrant, no spec) at the version that serves kuadrants.
func kuadrantObject(version, ns string) map[string]any {
	return map[string]any{
		"apiVersion": KuadrantGroup + "/" + version,
		"kind":       "Kuadrant",
		"metadata":   map[string]any{"name": "kuadrant", "namespace": ns, "labels": envkit.ToolLabels()},
	}
}

// KuadrantServedVersion is the version of kuadrant.io that serves
// kuadrants ("" while none does: Connectivity Link's CRDs are not there).
// The group's preferred version is not it: RHCL 1.4 serves AuthPolicy in v1
// and Kuadrant only in v1beta1 (seen live).
func KuadrantServedVersion(c *kube.Client) (string, error) {
	read, err := rhoai.ListServed(c, KuadrantGroup, "/apis/kuadrant.io/%s/kuadrants", nil, []string{"v1beta1"})
	if err != nil {
		return "", err
	}
	if read.Body == nil {
		return "", nil
	}
	return read.Version, nil
}

// CreateKuadrant creates the Kuadrant CR in ns (a server-side dry run with
// dryRun). One that exists (made meanwhile) is fine.
func CreateKuadrant(c *kube.Client, version, ns string, dryRun bool) error {
	data, _ := json.Marshal(kuadrantObject(version, ns))
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	_, _, err := c.Do(http.MethodPost, envkit.WithToolFieldManager("/apis/kuadrant.io/"+version+"/namespaces/"+ns+"/kuadrants"), "application/json", data, query)
	if kube.IsK8sConflict(err) {
		return nil
	}
	return err
}

// KuadrantCRD is the CRD of the Kuadrant CR.
const KuadrantCRD = "kuadrants." + KuadrantGroup

// kuadrantAdmissionWait is how long after the Kuadrant CRD turned
// Established the Kuadrant CR waits to be created: the API server's
// admission RESTMapper is reset every 30 seconds, and 15 seconds more cover
// the reset itself and the gap between this tool's clock and the cluster's.
const kuadrantAdmissionWait = 30*time.Second + 15*time.Second

// WaitKuadrantKindAdmissible waits, on step, until the API servers know the
// kind Kuadrant well enough to admit the objects a Kuadrant CR owns. It
// returns a failed response when it cannot tell, and nil, nil when the CR
// can be created now.
//
// Kuadrant's operator creates Authorino and Limitador with an owner
// reference to the Kuadrant CR that sets blockOwnerDeletion. The garbage
// collector's admission plugin refuses such a reference to a kind its
// RESTMapper does not know (plugin/pkg/admission/gc/gc_admission.go:141:
// "cannot set blockOwnerDeletion in this case because cannot find
// RESTMapping"), and kube-apiserver resets that mapper only every 30
// seconds (staging/src/k8s.io/apiserver/pkg/server/options/admission.go:167:
// `go utilwait.Until(discoveryRESTMapper.Reset, 30*time.Second, ...)`).
// So for up to 30 seconds after the CRD is Established the operator's first
// try fails, and it does not try again (seen live with Connectivity Link
// 1.5: Kuadrant stayed not Ready, "limitador resource not in topology",
// until its operator restarted). The wait uses time, not a probe: each API
// server replica (a hosted control plane runs several) has its own timer,
// so one request that is admitted proves nothing about the next. A CRD
// Established long ago needs no wait.
//
// The CRD is read from the cluster-wide CRD list, by name, which the
// template already grants (Set up itself reads it with the signed-in
// user's token, as every change does). Its Established time is the API
// server's, so its age is measured against the API server's time in the
// same response (the Date header), not this tool's clock, which can be
// ahead when the tool runs outside the cluster; only a response without a
// Date falls back to the local clock. A time missing, malformed or in the
// future waits the whole time. The wait itself runs on kube.Clock() and
// never lasts longer than kuadrantAdmissionWait.
func (m *Run) WaitKuadrantKindAdmissible(step string) (*types.OperationResponse, error) {
	c := m.Client
	var crds []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Conditions []struct {
				Type               string `json:"type"`
				Status             string `json:"status"`
				Reason             string `json:"reason"`
				Message            string `json:"message"`
				LastTransitionTime string `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	serverNow, err := c.ListIntoAt("/apis/apiextensions.k8s.io/v1/customresourcedefinitions?fieldSelector="+url.QueryEscape("metadata.name="+KuadrantCRD), &crds)
	if err != nil {
		return m.Fail(step, fmt.Sprintf("Cannot read CRD %s, so this tool cannot tell whether the API server accepts the new Kuadrant type yet: %v. The Kuadrant CR was not created; Set up again to retry.%s", KuadrantCRD, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "kuadrant crd read failed")
	}
	found := false
	established, status, why := "", "", "it reports no Established condition"
	for _, crd := range crds {
		if crd.Metadata.Name != KuadrantCRD {
			continue
		}
		found = true
		for _, cond := range crd.Status.Conditions {
			if cond.Type == "Established" {
				established, status = cond.LastTransitionTime, cond.Status
				why = "Established is " + kube.FirstNonEmpty(cond.Status, "not set")
				if text := kube.FirstNonEmpty(cond.Message, cond.Reason); text != "" {
					why += ": " + kube.TruncateText(text, 300)
				}
			}
		}
	}
	switch {
	case !found:
		return m.Fail(step, fmt.Sprintf("CRD %s is not there although Kuadrant's API is served. The Kuadrant CR was not created; Set up again to retry.", KuadrantCRD), "not_ready", "kuadrant crd missing")
	case status != "True":
		return m.Fail(step, fmt.Sprintf("CRD %s is not Established (%s). The Kuadrant CR was not created; Set up again to retry.", KuadrantCRD, why), "not_ready", "kuadrant crd not established")
	}
	if serverNow.IsZero() {
		serverNow = time.Now() // no Date header: the local clock is all there is
	}
	wait := kuadrantAdmissionWait
	if at, err := time.Parse(time.RFC3339, established); err == nil {
		wait = min(max(kuadrantAdmissionWait-serverNow.Sub(at), 0), kuadrantAdmissionWait)
	}
	if wait <= 0 {
		return nil, nil
	}
	m.Step(step, "running", fmt.Sprintf("Waiting for the API server to accept the new Kuadrant type (up to %d seconds)...", int((wait+time.Second-1)/time.Second)))
	if err := kube.Clock().Sleep(c.Context(), wait); err != nil {
		return m.Fail(step, fmt.Sprintf("Stopped waiting for the API server to accept the new Kuadrant type: %v. The Kuadrant CR was not created.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "kuadrant crd wait failed")
	}
	return nil, nil
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
func createInfraNamespace(c *kube.Client, ns string, dryRun bool) error {
	if _, ok := maasInfraNamespacePath(ns); !ok {
		return fmt.Errorf("namespace %s is not one this tool creates", ns)
	}
	data, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns, "labels": envkit.ToolLabels()}})
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	_, _, err := c.Do(http.MethodPost, envkit.WithToolFieldManager("/api/v1/namespaces"), "application/json", data, query)
	return err
}

// infraNamespaceLeftovers names what is still in the infrastructure
// namespace that this tool did not make, as far as it can list: pods,
// Deployments and PersistentVolumeClaims (the SA lists no other kind there),
// and a maas-db-config this tool did not write. Removal deletes the
// namespace only when this finds nothing; a read that fails is returned
// (fail closed).
func infraNamespaceLeftovers(c *kube.Client, ns string) ([]string, error) {
	var out []string
	for _, l := range []struct{ kind, path string }{
		{"pod", rhoai.NamespacedPath("v1", "pods", ns, "")},
		{"Deployment", rhoai.NamespacedPath("apps/v1", "deployments", ns, "")},
		{"PersistentVolumeClaim", rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, "")},
	} {
		var items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
		}
		if err := c.ListInto(l.path, &items); err != nil {
			if kube.IsDecodeError(err) {
				return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
			}
			return nil, fmt.Errorf("list the %ss in %s: %w", l.kind, ns, err)
		}
		for _, it := range items {
			out = append(out, l.kind+" "+it.Metadata.Name)
		}
	}
	meta, found, err := kube.ReadObjectMeta(c, maasObjectPath("Secret", ns))
	if err != nil {
		return nil, fmt.Errorf("read Secret %s/%s: %w", ns, maasDBConfigSecret, err)
	}
	if found && !ToolOwned(meta) {
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
//     trusts the service CA another way (AuthorinoTrustsServiceCA);
//  4. then, as the script's closing note and deploy.sh do, a rollout
//     restart of authorino.
//
// The secret is the one the CR names, else the script's. A dry run sends
// every patch with dryRun=All and restarts nothing. It returns the changes
// made (none when all was set).
func configureAuthorinoTLS(c *kube.Client, ns string, dryRun bool) ([]string, error) {
	read, err := rhoai.GetServed(c, authorinoGroup, "/apis/"+authorinoGroup+"/%s/namespaces/"+ns+"/authorinos/authorino", []string{"v1beta1"})
	if kube.IsK8sNotFound(err) || (err == nil && read.Body == nil) {
		return nil, fmt.Errorf("%w: %w in %s", errAuthorinoNotYet, errNoAuthorinoCR, ns)
	}
	if err != nil {
		return nil, fmt.Errorf("read Authorino %s/authorino: %w", ns, err)
	}
	var cr struct {
		Metadata kube.ObjectMeta `json:"metadata"`
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
	secret := kube.FirstNonEmpty(cr.Spec.Listener.TLS.CertSecretRef.Name, authorinoDefaultSecret)
	if len(secret) > 253 || !components.DNS1123Subdomain.MatchString(secret) {
		return nil, fmt.Errorf("authorino's CR names its certificate Secret %q, which is not a valid Kubernetes name", kube.TruncateText(secret, 300))
	}
	query := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	var changes []string
	svcPath := rhoai.NamespacedPath("v1", "services", ns, "authorino-authorino-authorization")
	svc, found, err := kube.ReadObjectMeta(c, svcPath)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read Service %s/authorino-authorino-authorization: %w", ns, err)
	case !found:
		return nil, fmt.Errorf("%w: no Service %s/authorino-authorino-authorization", errAuthorinoNotYet, ns)
	case svc.Annotations[CertSecretAnnotation] != secret:
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{CertSecretAnnotation: secret}}})
		if _, _, err := c.Do(http.MethodPatch, rhoai.NamespacedPath("v1", "services", ns, "authorino-authorino-authorization"), "application/merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("annotate Service %s/authorino-authorino-authorization: %w", ns, err)
		}
		changes = append(changes, fmt.Sprintf("Service authorino-authorino-authorization asks service-ca for Secret %s", secret))
	}
	if !cr.Spec.Listener.TLS.Enabled || cr.Spec.Listener.TLS.CertSecretRef.Name != secret {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"listener": map[string]any{"tls": map[string]any{
			"enabled": true, "certSecretRef": map[string]any{"name": secret}}}}})
		if _, _, err := c.Do(http.MethodPatch, "/apis/"+authorinoGroup+"/"+read.Version+"/namespaces/"+ns+"/authorinos/authorino", "application/merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("enable TLS on Authorino %s/authorino: %w", ns, err)
		}
		changes = append(changes, "Authorino's listener serves TLS with "+secret)
	}
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("apps/v1", "deployments", ns, "authorino"))
	if err != nil {
		return changes, fmt.Errorf("read Deployment %s/authorino: %w", ns, err)
	}
	if !exists {
		return changes, fmt.Errorf("%w: no Deployment %s/authorino", errAuthorinoNotYet, ns)
	}
	if !AuthorinoTrustsServiceCA(body) {
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
		if _, _, err := c.Do(http.MethodPatch, rhoai.NamespacedPath("apps/v1", "deployments", ns, "authorino"), "application/strategic-merge-patch+json", patch, query); err != nil {
			return changes, fmt.Errorf("set SSL_CERT_FILE on Deployment %s/authorino: %w", ns, err)
		}
		changes = append(changes, "Authorino trusts the service CA (SSL_CERT_FILE and REQUESTS_CA_BUNDLE)")
	}
	if len(changes) > 0 && !dryRun {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{components.RestartAnnotation: kube.Clock().Now().UTC().Format(time.RFC3339)},
		}}}})
		if _, _, err := c.Do(http.MethodPatch, rhoai.NamespacedPath("apps/v1", "deployments", ns, "authorino"), "application/merge-patch+json", patch, query); err != nil {
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
func maasGatewayPolicies(c *kube.Client) ([]gatewayPolicy, error) {
	var out []gatewayPolicy
	for _, l := range []struct {
		kind, path string
		fallback   []string
	}{
		{"AuthPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/authpolicies", []string{"v1"}},
		{"TokenRateLimitPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/tokenratelimitpolicies", []string{"v1alpha1"}},
		{"RateLimitPolicy", "/apis/kuadrant.io/%s/namespaces/openshift-ingress/ratelimitpolicies", []string{"v1"}},
	} {
		read, err := rhoai.ListServed(c, KuadrantGroup, l.path, nil, l.fallback)
		if err != nil {
			return nil, fmt.Errorf("list %ss in %s: %w", l.kind, inferenceGatewayNamespace, err)
		}
		if read.Body == nil {
			continue
		}
		var list struct {
			Items []struct {
				Metadata kube.ObjectMeta `json:"metadata"`
				Spec     struct {
					TargetRef struct {
						Kind string `json:"kind"`
						Name string `json:"name"`
					} `json:"targetRef"`
				} `json:"spec"`
				Status struct {
					Conditions []kube.Condition `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(read.Body, &list); err != nil {
			return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
		}
		for _, it := range list.Items {
			if it.Spec.TargetRef.Kind != "Gateway" || it.Spec.TargetRef.Name != MaaSGatewayName {
				continue
			}
			p := gatewayPolicy{Kind: l.kind, Name: it.Metadata.Name, UID: it.Metadata.UID, Version: read.Version, Managed: it.Metadata.Labels[kube.ManagedByLabelKey] == maasControllerLabel}
			if cond, ok := kube.FindCondition(it.Status.Conditions, "Enforced"); ok {
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

// RHCLInstallCommands are platform-setup.md's RHOAI install of Connectivity
// Link and its Kuadrant CR, on channel.
func RHCLInstallCommands(channel string) string {
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

// KuadrantCommands create the Kuadrant CR as platform-setup.md does.
func KuadrantCommands(ns string) string {
	return fmt.Sprintf(`oc apply -f - <<'EOF'
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: %s
EOF`, yamlString(ns))
}

// CNPGInstallCommands install CloudNativePG as this tool does.
func CNPGInstallCommands(channel string) string {
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
func rhclDenyEnvCommand(sub *envkit.OLMSubscription) string {
	ref := fmt.Sprintf("oc patch subscription %s -n %s --type='json' -p=", kube.ShellQuote(sub.Metadata.Name), kube.ShellQuote(sub.Metadata.Namespace))
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

// RHCLChannelCommand switches a Subscription's channel.
func RHCLChannelCommand(ns, name, channel string) string {
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"channel": channel}})
	return fmt.Sprintf("oc patch subscription %s -n %s --type=merge -p %s", kube.ShellQuote(name), kube.ShellQuote(ns), kube.ShellQuote(string(patch)))
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

// CommentText is s for a shown "# ..." comment line: control characters
// (a newline would end the comment) become spaces.
func CommentText(s string) string {
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
		lines = append(lines, fmt.Sprintf("oc delete %s %s -n %s", strings.ToLower(p.Kind), kube.ShellQuote(p.Name), inferenceGatewayNamespace))
	}
	return strings.Join(lines, "\n")
}

// maasDSCCommand turns on the AI gateway and MaaS in the DataScienceCluster
// (maas-setup.md "Configure DataScienceCluster").
func maasDSCCommand(dsc string) string {
	return fmt.Sprintf(`oc patch datasciencecluster %s --type=merge -p '{"spec":{"components":{"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"}}}}}'`, kube.ShellQuote(kube.FirstNonEmpty(dsc, "default-dsc")))
}

// gatewayAllowedRoutesCommand narrows a gateway someone else made from
// every namespace to the names the MaaS docs' setup-gateway.sh lists
// (build_allowed_routes_yaml: kubernetes.io/metadata.name In [...]), one
// JSON patch operation per listener that admits all.
func gatewayAllowedRoutesCommand(g GatewayObject, namespaces []string) string {
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
	return fmt.Sprintf("oc patch gateway %s -n %s --type=json -p %s", MaaSGatewayName, inferenceGatewayNamespace, kube.ShellQuote(string(data)))
}

// admitsAll: a listener of the gateway admits routes from every namespace.
func (g GatewayObject) admitsAll() bool {
	for _, l := range g.Spec.Listeners {
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From == "All" {
			return true
		}
	}
	return false
}
