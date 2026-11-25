package serving

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Authorino's TLS on a gateway that odh-model-controller authenticates
// (AuthPolicy <gateway>-authn), not only on MaaS's.
//
// RHOAI documents TLS on Authorino's listener as part of authentication for
// llm-d with Connectivity Link: the serving-certificate annotation on
// Service authorino-authorino-authorization and spec.listener.tls on the
// Authorino CR (opendatahub-documentation
// modules/configuring-authentication-for-llmd.adoc, steps "Add the
// ServingCert annotation" and "Update Authorino to enable SSL", in
// assemblies/deploying-models.adoc). Kuadrant makes Authorino with that
// listener's TLS off (kuadrant-operator v1.5.0
// internal/controller/authorino_reconciler.go:160-168).
//
// odh-model-controller (rhoai-3.6 and rhoai-3.7-ea.1) points the gateway's
// authorization at Authorino over TLS with an EnvoyFilter, <gateway>-authn-ssl:
// cluster kuadrant-auth-service, port 50051, an UpstreamTlsContext
// (internal/controller/resources/template/envoyfilter_ssl.yaml), until
// Kuadrant can do it (kuadrant-operator#1531). It makes the filter when it
// reads Authorino's TLS as on, and also when it finds no Authorino or
// cannot list them (internal/controller/utils/kuadrant.go
// IsAuthorinoTLSEnabled); once it reads TLS as off it skips the filter but
// does not remove one it made before (serving/llm/gateway_controller.go
// reconcileEnvoyFilter), and it does not watch Authorino. Such a filter
// makes the gateway speak TLS to a plaintext Authorino: the gRPC call
// fails, and Kuadrant's wasm-shim, whose authorization fails closed
// (kuadrant-operator internal/wasm/utils.go AuthServiceFailureMode),
// answers 500 (wasm-shim v0.14.1 src/kuadrant/pipeline/tasks/failure_mode.rs,
// send_reply.rs SendReplyTask::default). A team's cluster showed it live
// (RHCL 1.5.0 in openshift-operators, every call through
// openshift-ai-inference 500); turning TLS on fixed it.
//
// This tool reads Authorino and says so; it does not change it on a gateway
// it does not run MaaS on (Connectivity Link is shared).

// authorinoListenerTLS reads the listener of Kuadrant's Authorino: ns is
// the Kuadrant CR's namespace (Kuadrant makes Authorino "authorino" in its
// own namespace, authorino_reconciler.go:147-149), secret the certificate
// Secret the CR names (else the docs' authorino-server-cert). known is
// false when the Kuadrant CR or the Authorino CR cannot be read. plaintext:
// spec.listener.tls.enabled is not true (unset is off, as
// odh-model-controller reads it: ptr.Deref(..., false)).
func authorinoListenerTLS(c *kube.Client) (ns, secret string, plaintext, known bool) {
	kq, err := ReadKuadrant(c)
	if err != nil || !kq.Found || kq.Namespace == "" {
		return "", "", false, false
	}
	read, err := rhoai.GetServed(c, authorinoGroup, "/apis/"+authorinoGroup+"/%s/namespaces/"+kq.Namespace+"/authorinos/authorino", []string{"v1beta1"})
	if err != nil || read.Body == nil {
		return kq.Namespace, "", false, false
	}
	var a struct {
		Spec struct {
			Listener struct {
				TLS struct {
					Enabled       *bool `json:"enabled"`
					CertSecretRef struct {
						Name string `json:"name"`
					} `json:"certSecretRef"`
				} `json:"tls"`
			} `json:"listener"`
		} `json:"spec"`
	}
	if json.Unmarshal(read.Body, &a) != nil {
		return kq.Namespace, "", false, false
	}
	tls := a.Spec.Listener.TLS
	return kq.Namespace, kube.FirstNonEmpty(tls.CertSecretRef.Name, authorinoDefaultSecret), tls.Enabled == nil || !*tls.Enabled, true
}

// llmdAuthDocTitle names RHOAI's procedure for a person to look up.
const llmdAuthDocTitle = "Configuring authentication for llm-d using Red Hat Connectivity Link"

// gatewayAuthorinoTLSCommands are RHOAI's steps that turn on Authorino's
// TLS (configuring-authentication-for-llmd.adoc), in Authorino's namespace
// ns, for a cluster-admin: the Service's serving-certificate annotation,
// the CR's listener TLS (a merge patch of those fields where the docs
// apply a whole CR: Kuadrant made the CR and owns the rest), the wait for
// Authorino's pods, and the restart of odh-model-controller and KServe's
// controller that the docs give for RHOAI installed before Connectivity
// Link. The restart matters here: odh-model-controller re-reads Authorino
// only when it reconciles a gateway. apps is RHOAI's applications
// namespace; without it the restart is left out and why says so. why is
// also set when a name is not one a command may carry.
func gatewayAuthorinoTLSCommands(ns, secret, apps string) (cmds, why string) {
	if len(ns) > 63 || !components.DNS1123Label.MatchString(ns) {
		return "", fmt.Sprintf("Connectivity Link's namespace %s is not a valid Kubernetes name, so no commands are shown", strconv.Quote(kube.TruncateText(ns, 300)))
	}
	if len(secret) > 253 || !components.DNS1123Subdomain.MatchString(secret) {
		return "", fmt.Sprintf("Authorino's CR names its certificate Secret %s (spec.listener.tls.certSecretRef.name), which is not a valid Kubernetes name, so no commands are shown", strconv.Quote(kube.TruncateText(secret, 300)))
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"listener": map[string]any{"tls": map[string]any{
		"enabled": true, "certSecretRef": map[string]any{"name": secret}}}}})
	q := kube.ShellQuote(ns)
	cmds = fmt.Sprintf(`oc annotate service authorino-authorino-authorization -n %[1]s \
  service.beta.openshift.io/serving-cert-secret-name=%[2]s --overwrite
oc patch authorino authorino -n %[1]s --type=merge \
  -p %[3]s
oc wait --for=condition=ready pod -l authorino-resource=authorino -n %[1]s --timeout=150s`, q, kube.ShellQuote(secret), kube.ShellQuote(string(patch)))
	if len(apps) > 63 || !components.DNS1123Label.MatchString(apps) {
		return cmds, "RHOAI's applications namespace cannot be read, so the restart of odh-model-controller the docs give is not shown"
	}
	a := kube.ShellQuote(apps)
	return cmds + fmt.Sprintf(`
oc delete pod -n %[1]s -l app=odh-model-controller
oc delete pod -n %[1]s -l control-plane=kserve-controller-manager`, a), ""
}

// gatewayAuthorinoTLSWarning is the inference gateway's warning while
// Authorino in ns serves no TLS; why is gatewayAuthorinoTLSCommands'.
func gatewayAuthorinoTLSWarning(ns, cmds, why string) string {
	w := fmt.Sprintf("Calls through the gateway can fail with HTTP 500: Authorino in %s doesn't serve TLS. RHOAI's setup of the gateway's authentication turns it on (%q), and odh-model-controller then sends the gateway's authorization to Authorino over TLS; a gateway it set up so while Authorino's TLS was on, or before Authorino existed, keeps failing every call until Authorino serves TLS. This tool doesn't change Connectivity Link's Authorino for this gateway.", ns, llmdAuthDocTitle)
	switch {
	case cmds == "":
		return w + " " + why + "."
	case why != "":
		return w + " A cluster-admin runs RHOAI's steps below; " + why + "."
	}
	return w + " A cluster-admin runs RHOAI's steps below."
}

// gatewayErrorCause adds to a "gateway-error" call check what Authorino's
// listener says about it.
func gatewayErrorCause(c *kube.Client) string {
	ns, _, plaintext, known := authorinoListenerTLS(c)
	switch {
	case known && plaintext:
		return fmt.Sprintf(" Authorino in %s doesn't serve TLS, which RHOAI's setup turns on (%q): a cluster-admin turns it on; the Inference gateway part shows the commands.", ns, llmdAuthDocTitle)
	case known:
		return fmt.Sprintf(" Authorino in %s serves TLS: check that it runs, and that odh-model-controller re-read it after its TLS was turned on (it does when a gateway or a model changes, or when it restarts, as RHOAI's steps do).", ns)
	}
	return " Check that Red Hat Connectivity Link's Authorino runs."
}
