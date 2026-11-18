package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Models as a Service (MaaS): the platform, set up as the MaaS install docs
// do it (opendatahub-io/models-as-a-service, docs/content/install at
// 68b70af1, 2026-11-13), and this tool's model published to it.
//
// The documented order (install/maas-setup.md "Installation flow"):
//  1. Database Setup: a PostgreSQL the docs call customer-provided, and
//     Secret maas-db-config (key DB_CONNECTION_URL,
//     postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require) in the
//     infrastructure namespace (maas-setup.md l.14-38). This tool runs a
//     one-instance CloudNativePG cluster in that namespace, where the
//     docs' own development database goes (scripts/setup-database.sh: user
//     and database "maas", Secret labelled app=maas-api).
//  2. Create Gateway: scripts/setup-gateway.sh in its default loadbalancer
//     mode: GatewayClass openshift-default when missing (setup_gatewayclass),
//     then deployment/base/networking/maas/maas-gateway-api.yaml (Gateway
//     maas-default-gateway in openshift-ingress, listener "https" with
//     hostname maas.<apps domain>, the default ingress certificate, the
//     annotations opendatahub.io/managed=false and
//     security.opendatahub.io/authorino-tls-bootstrap=true), with
//     allowedRoutes set as the script's ALLOWED_ROUTE_NAMESPACES does
//     (build_allowed_routes_yaml: matchExpressions
//     kubernetes.io/metadata.name In [...]). Then wait Programmed (120 s).
//  3. Authorino TLS (maas-setup.md "Authorino TLS",
//     scripts/setup-authorino-tls.sh): patches Kuadrant's own Authorino
//     objects, which Connectivity Link's operators own and reconcile. This
//     tool never edits operator-owned objects: it only checks the three
//     settings and shows the script's commands for a cluster-admin
//     (SECURITY.md, MaaS audit).
//  4. Configure DataScienceCluster: aigateway.modelsAsAService Managed; the
//     previous value is recorded so removal restores it.
//  5. Validation: maas-api Available (rollout status), the default tenant
//     (AITenant and MaasTenantConfig Ready; their phase may read Degraded
//     without observability), the gateway's AuthPolicy Enforced.
//
// Publishing follows install/model-setup.md "Update Existing Models": the
// model routes through maas-default-gateway (spec.router.gateway.refs; the
// docs' spec.gateway.refs is not a field of LLMInferenceService), and only
// through it ("Only models routing through maas-default-gateway appear in
// the MaaS catalog and have policies applied", model-setup.md; why not also
// the inference gateway: maas_routing.go), then a MaaSModelRef in the
// project, a MaaSSubscription (100 tokens a minute for system:authenticated,
// the docs' sample) and a MaaSAuthPolicy in models-as-a-service
// (docs/samples/maas-system/free/maas).

const (
	maasGroup        = "maas.opendatahub.io"
	maasGatewayName  = "maas-default-gateway"
	maasGatewayClass = "openshift-default"
	maasHostPrefix   = "maas."
	// The gateway's in-cluster listener: its Service's name
	// (<gateway>-<class>, as for the inference gateway) with a service-ca
	// certificate the options ConfigMap asks for (the docs' gateway-patterns
	// names it maas-gw-service-tls).
	maasGatewayOptions    = "maas-gateway-options"
	maasGatewayServiceTLS = "maas-gw-service-tls"
	maasDBCluster         = "maas-db"
	maasDBAppSecret       = "maas-db-app"
	maasDBConfigSecret    = "maas-db-config"
	maasDBName            = "maas"
	maasTenantNamespace   = "models-as-a-service"
	maasAITenantNamespace = "ai-tenants"
	maasAuthPolicyName    = "rhoai-nightly-updater-access"
	maasSubscriptionName  = "rhoai-nightly-updater-subscription"
	// maasModelManager is the field manager of the gateway ref this tool
	// adds to its model: removing it reverts exactly that change.
	maasModelManager = "rhoai-nightly-updater-maas"
	// maasDSCManager is the field manager of modelsAsAService, so removal
	// restores it only while nobody has set it since.
	maasDSCManager = "rhoai-nightly-updater-maas-dsc"
	// maasModelFinalizer is maas-controller's on a MaaSModelRef
	// (maasmodelref_controller.go).
	maasModelFinalizer = "maas.opendatahub.io/model-cleanup"
	maasRecordKey      = "maas"
	maasRecordManager  = "rhoai-nightly-updater-maas-record"
	// The group a published model is offered to, as the docs' sample.
	maasSubjectGroup = "system:authenticated"
	cnpgGroup        = "postgresql.cnpg.io"
	maasDocsBase     = "https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/content/"
)

var (
	// MaaSDatabaseTimeout bounds the wait for the database to run.
	MaaSDatabaseTimeout = 5 * time.Minute
	// MaaSGatewayTimeout bounds the wait for Programmed (setup-gateway.sh: 120 s).
	MaaSGatewayTimeout = 2 * time.Minute
	// MaaSPlatformTimeout bounds the wait for maas-api and the tenant
	// (maas-setup.md waits 300 s).
	MaaSPlatformTimeout = 5 * time.Minute
	// MaaSPublishTimeout bounds the wait for a published model to be Ready.
	MaaSPublishTimeout = 5 * time.Minute
	// MaaSRouteTimeout bounds the wait for KServe to move the model's
	// HTTPRoute after its gateway refs change (live: about 6 s).
	MaaSRouteTimeout  = 2 * time.Minute
	MaaSDeleteTimeout = 2 * time.Minute
	MaaSPoll          = 5 * time.Second
)

// authorinoGroup is the Authorino operator's API group (the Authorino CR).
const authorinoGroup = "operator.authorino.kuadrant.io"

// maasAPI is maas.opendatahub.io at the version the cluster serves (its
// preferred one; v1alpha1, RHOAI 3.6's, when discovery cannot tell), for
// the MaaS objects' paths and apiVersion.
func maasAPI(c *Client) string {
	return maasGroup + "/" + servedVersions(c, maasGroup, []string{"v1alpha1"}).Versions[0]
}

// --- record (this tool's namespace, snapshot ConfigMap key "maas") ---------------

// maasRecord is what this tool did to the platform that the cluster cannot
// tell by itself: whether it set modelsAsAService and what it was before,
// the models it published ("<project>/<model>"), and each published
// model's gateway refs before its publication. readMaaSRecord reads an
// entry of a project only (in either), written before models were named,
// as the default model's.
type maasRecord struct {
	DSCChanged               bool     `json:"dscChanged,omitempty"`
	PreviousModelsAsAService string   `json:"previousModelsAsAService,omitempty"`
	Published                []string `json:"published,omitempty"`
	// ModelRefs is, by publication ("<project>/<model>"), the model's
	// spec.router.gateway.refs as publish read them before routing it
	// through the MaaS gateway alone; Unpublish writes them back. A
	// publication by an earlier build has none.
	ModelRefs map[string]maasModelRefs `json:"modelRefs,omitempty"`
	// GenAI is, by publication, the genai label and annotation publish
	// removed from the model (maas_genai.go); Unpublish puts them back.
	GenAI map[string]maasGenAI `json:"genAI,omitempty"`
	// DBNamespace is where setup put the database and maas-db-config, so
	// removal and the status look there even when the infrastructure
	// namespace reads differently later.
	DBNamespace string `json:"dbNamespace,omitempty"`
	// AIGatewayChanged: setup set aigateway's managementState to Managed,
	// and PreviousAIGateway is what it was; removal restores it as it does
	// modelsAsAService.
	AIGatewayChanged  bool   `json:"aigatewayChanged,omitempty"`
	PreviousAIGateway string `json:"previousAIGateway,omitempty"`
	// InfraNamespace is the infrastructure namespace setup created ("" when
	// it existed); removal deletes it only when nothing else is in it.
	InfraNamespace string `json:"infraNamespace,omitempty"`
	// Shared are the shared platform pieces this tool installed, created or
	// configured ("rhcl", "kuadrant", "cnpg", "authorino-tls",
	// "rhcl-memory"), with what it did. Removal keeps them: other RHOAI
	// features use them.
	Shared map[string]string `json:"shared,omitempty"`
	At     string            `json:"at,omitempty"`
	By     string            `json:"by,omitempty"`
}

func (r *maasRecord) setShared(key, what string) {
	if r.Shared == nil {
		r.Shared = map[string]string{}
	}
	r.Shared[key] = what
}

func readMaaSRecord(c *Client) (maasRecord, error) {
	var rec maasRecord
	body, _, err := c.get(namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName))
	if IsK8sError(err, 404) {
		return rec, nil
	}
	if err != nil {
		return rec, fmt.Errorf("read this tool's MaaS record: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return rec, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
	}
	if raw := cm.Data[maasRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return rec, fmt.Errorf("parse this tool's MaaS record: %w", err)
		}
	}
	var keys []string
	for _, p := range rec.pubs() {
		keys = append(keys, p.key())
	}
	rec.Published = keys
	if len(rec.ModelRefs) > 0 {
		def, _ := catalogEntry("")
		refs := map[string]maasModelRefs{}
		for k, v := range rec.ModelRefs {
			if !strings.Contains(k, "/") {
				k = maasPub{k, def.Name}.key()
				if _, named := rec.ModelRefs[k]; named {
					continue
				}
			}
			refs[k] = v
		}
		rec.ModelRefs = refs
	}
	return rec, nil
}

// writeMaaSRecord applies the record (operations hold the cluster lock).
func writeMaaSRecord(c *Client, rec maasRecord) error {
	rec.At, rec.By = time.Now().UTC().Format(time.RFC3339), getUser(c)
	sort.Strings(rec.Published)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	cm, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": snapshotConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{maasRecordKey: string(data)},
	})
	query := url.Values{"fieldManager": {maasRecordManager}, "force": {"true"}}
	if _, _, err := c.do(http.MethodPatch, namespacedPath("v1", "configmaps", ns, snapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record this tool's MaaS changes: %w", err)
	}
	return nil
}

// maasModelRefs are a model's gateway refs as read; none is KServe's
// default gateway (spec.router.gateway {}).
type maasModelRefs struct {
	Refs gatewayRefs `json:"refs,omitempty"`
}

// maasPub is one publication: a catalog model in a project.
type maasPub struct{ project, model string }

func (p maasPub) key() string { return p.project + "/" + p.model }

// pubs are the recorded publications; an entry of a project only is the
// default model's.
func (r maasRecord) pubs() []maasPub {
	def, _ := catalogEntry("")
	var out []maasPub
	seen := map[string]bool{}
	for _, e := range r.Published {
		project, model, found := strings.Cut(e, "/")
		if !found {
			model = def.Name
		}
		p := maasPub{project, model}
		if !seen[p.key()] {
			seen[p.key()] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (r maasRecord) published(project, model string) bool {
	return containsString(r.Published, maasPub{project, model}.key())
}

// modelRefs are the refs publish recorded for model in project; none
// recorded for a publication by an earlier build.
func (r maasRecord) modelRefs(project, model string) (maasModelRefs, bool) {
	refs, ok := r.ModelRefs[maasPub{project, model}.key()]
	return refs, ok
}

func (r *maasRecord) setModelRefs(project, model string, refs maasModelRefs) {
	if r.ModelRefs == nil {
		r.ModelRefs = map[string]maasModelRefs{}
	}
	r.ModelRefs[maasPub{project, model}.key()] = refs
}

func (r *maasRecord) setPublished(project, model string, on bool) {
	key := maasPub{project, model}.key()
	var out []string
	for _, p := range r.pubs() {
		if p.key() != key {
			out = append(out, p.key())
		}
	}
	if on {
		out = append(out, key)
	} else {
		delete(r.ModelRefs, key)
		delete(r.GenAI, key)
		if len(r.GenAI) == 0 {
			r.GenAI = nil
		}
		if len(r.ModelRefs) == 0 {
			r.ModelRefs = nil
		}
	}
	sort.Strings(out)
	r.Published = out
}

// projects are the projects with a recorded publication: the MaaS gateway
// admits routes from them.
func (r maasRecord) projects() []string {
	var out []string
	for _, p := range r.pubs() {
		if !containsString(out, p.project) {
			out = append(out, p.project)
		}
	}
	return out
}

// maasEmbeddingReason is why an embedding model is not offered for MaaS.
const maasEmbeddingReason = "This tool publishes chat models only. Use the embedding model at its own address (the Models tab shows it), or add it to the Gen AI playground."

// --- reads ---------------------------------------------------------------------

// maasInfraNamespace is where maas-api runs and maas-db-config goes: the
// tenant's status.infraNamespace when MaaS runs, else derived from the
// applications namespace as the docs' scripts do (derive_infra_namespace in
// scripts/setup-database.sh: redhat-ods-applications ->
// redhat-ai-gateway-infra, opendatahub -> odh-ai-gateway-infra).
func maasInfraNamespace(c *Client) string {
	body, _, err := c.get(namespacedPath(maasAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant"))
	if err == nil {
		var tc struct {
			Status struct {
				InfraNamespace string `json:"infraNamespace"`
			} `json:"status"`
		}
		if json.Unmarshal(body, &tc) == nil && tc.Status.InfraNamespace != "" {
			return tc.Status.InfraNamespace
		}
	}
	switch apps := appsNamespace(c); apps {
	case "redhat-ods-applications":
		return "redhat-ai-gateway-infra"
	case "opendatahub":
		return "odh-ai-gateway-infra"
	default:
		return apps
	}
}

// maasDSC is the DataScienceCluster as MaaS needs it.
type maasDSC struct {
	read      dscRead
	name      string
	aigateway string // managementState, "" when not named
	maas      string // aigateway.modelsAsAService.managementState
	ready     string // ModelsAsAServiceReady: True, False or "" (unknown)
	readyMsg  string
	argoApp   string
}

func readMaaSDSC(c *Client) (maasDSC, error) {
	r, err := readFirstDSC(c)
	if err != nil {
		return maasDSC{}, err
	}
	d := maasDSC{read: r}
	if r.Object == nil {
		return d, nil
	}
	meta, _ := r.Object["metadata"].(map[string]interface{})
	d.name, _ = meta["name"].(string)
	d.argoApp = argoApplicationOf(r.Object, "DataScienceCluster")
	d.aigateway, _ = componentManagement(r.Object, "aigateway")
	spec, _ := r.Object["spec"].(map[string]interface{})
	comps, _ := spec["components"].(map[string]interface{})
	ag, _ := comps["aigateway"].(map[string]interface{})
	sub, _ := ag["modelsAsAService"].(map[string]interface{})
	d.maas, _ = sub["managementState"].(string)
	d.ready, d.readyMsg = dscConditionStatus(r.Object, "ModelsAsAServiceReady")
	// The AIGateway component reports the condition first-hand; the DSC
	// copies it, and on RHOAI 3.6 kept the copy from before MaaS was turned
	// back on (live: DSC False/Removed, component True/Ready, maas-api
	// serving). The component wins when it reports one.
	if ready, msg := aigatewayMaaSReady(c); ready != "" {
		d.ready, d.readyMsg = ready, msg
	}
	return d, nil
}

// aigatewayMaaSReady reads ModelsAsAServiceReady from the AIGateway
// component the operator projects from the DSC (components.platform.
// opendatahub.io, listed at the version that serves it): "True", "False"
// or "" when there is none or it cannot be read.
func aigatewayMaaSReady(c *Client) (string, string) {
	read, err := listServed(c, "components.platform.opendatahub.io", "/apis/components.platform.opendatahub.io/%s/aigateways", nil, []string{"v1alpha1"})
	if err != nil || read.Body == nil {
		return "", ""
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []k8sCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(read.Body, &list) != nil || len(list.Items) != 1 {
		return "", ""
	}
	if cond, ok := findCondition(list.Items[0].Status.Conditions, "ModelsAsAServiceReady"); ok {
		return cond.Status, firstNonEmpty(cond.Message, cond.Reason)
	}
	return "", ""
}

// dscConditionStatus reads a DataScienceCluster condition: "True", "False"
// or "" (not reported), with its message.
func dscConditionStatus(dsc map[string]interface{}, typ string) (string, string) {
	status, _ := dsc["status"].(map[string]interface{})
	conds, _ := status["conditions"].([]interface{})
	for _, raw := range conds {
		cond, _ := raw.(map[string]interface{})
		if t, _ := cond["type"].(string); t != typ {
			continue
		}
		s, _ := cond["status"].(string)
		msg, _ := cond["message"].(string)
		if s != "True" && s != "False" {
			s = ""
		}
		return s, msg
	}
	return "", ""
}

// kuadrantState is the cluster's Kuadrant CR (Red Hat Connectivity Link).
type kuadrantState struct {
	found, ready bool
	namespace    string
	message      string
	meta         objectMeta
}

func readKuadrant(c *Client) (kuadrantState, error) {
	// The group's versions are not the resource's: kuadrant.io serves v1
	// (AuthPolicy...) first while Kuadrant is only in v1beta1 (RHCL 1.4,
	// seen live), so list it at each served version until one serves it.
	read, err := listServed(c, kuadrantGroup, "/apis/kuadrant.io/%s/kuadrants", nil, []string{"v1beta1"})
	if err != nil {
		return kuadrantState{}, err
	}
	body := read.Body
	if body == nil {
		return kuadrantState{}, nil
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				Conditions []k8sCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return kuadrantState{}, err
	}
	if len(list.Items) == 0 {
		return kuadrantState{}, nil
	}
	k := list.Items[0]
	st := kuadrantState{found: true, namespace: k.Metadata.Namespace, meta: k.Metadata}
	if cond, ok := findCondition(k.Status.Conditions, "Ready"); ok {
		st.ready = cond.Status == "True"
		st.message = firstNonEmpty(cond.Message, cond.Reason)
	}
	return st, nil
}

// rhclBelowMinimum: version is older than the docs' minimum for RHOAI 3.6
// (false when unknown).
func rhclBelowMinimum(version string) bool {
	return version != "" && compareVersions(versionCore(version), rhclDocsMinimum) < 0
}

// rhclWarning: the docs ask for RHCL 1.5 or later (prerequisites.md
// "RHOAI 3.6 — use RHCL v1.5"; 1.4.x can hang callers,
// Kuadrant/wasm-shim#425), and 1.4.0 bypasses gateway authentication
// (rhclAuthBypassVersion). Upgrade Connectivity Link (upgradeRHCL) finds
// OLM's path in the catalog and takes it.
func rhclWarning(version string) string {
	switch {
	case !rhclBelowMinimum(version):
		return ""
	case versionCore(version) == rhclAuthBypassVersion:
		return fmt.Sprintf("Red Hat Connectivity Link %s silently lets requests past the gateway's authentication (the MaaS docs' prerequisites: \"RHCL v1.4.0 contains a Wasm shim bug that silently bypasses gateway authentication\"). This tool publishes nothing until it is upgraded: Upgrade Connectivity Link does it.", version)
	}
	return fmt.Sprintf("Red Hat Connectivity Link %s is installed; the MaaS docs ask for %s or later with RHOAI 3.6: on 1.4.x a caller with two or more subscriptions can hang. MaaS works for a single subscription meanwhile. Upgrade Connectivity Link takes the upgrade path OLM offers, one channel at a time, and sets up Authorino TLS again afterwards.", version, rhclDocsMinimum)
}

// maasObjectPath is the path of a platform object this tool reads or
// writes, by kind; only these names.
func maasObjectPath(kind, ns string) string {
	switch kind {
	case "Gateway":
		return namespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "maas-default-gateway")
	case "ConfigMap":
		return namespacedPath("v1", "configmaps", "openshift-ingress", "maas-gateway-options")
	case "Cluster":
		return namespacedPath("postgresql.cnpg.io/v1", "clusters", ns, "maas-db")
	case "Secret":
		return namespacedPath("v1", "secrets", ns, "maas-db-config")
	}
	return ""
}

func maasCreatePath(kind, ns string) string {
	switch kind {
	case "Gateway":
		return withToolFieldManager(namespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", ""))
	case "ConfigMap":
		return withToolFieldManager(namespacedPath("v1", "configmaps", "openshift-ingress", ""))
	case "Cluster":
		return withToolFieldManager(namespacedPath("postgresql.cnpg.io/v1", "clusters", ns, ""))
	case "Secret":
		return withToolFieldManager(namespacedPath("v1", "secrets", ns, ""))
	}
	return ""
}

func maasDesc(kind, ns string) string {
	switch kind {
	case "GatewayClass":
		return "GatewayClass " + maasGatewayClass
	case "Gateway":
		return "Gateway " + inferenceGatewayNamespace + "/" + maasGatewayName
	case "ConfigMap":
		return "ConfigMap " + inferenceGatewayNamespace + "/" + maasGatewayOptions
	case "Cluster":
		return "CloudNativePG Cluster " + ns + "/" + maasDBCluster
	case "Secret":
		return "Secret " + ns + "/" + maasDBConfigSecret
	}
	return kind
}

// maasGatewayClassPath is GatewayClass openshift-default: OpenShift's
// shared class for gateways. This tool only reads it, and creates it when
// the cluster has none (setup-gateway.sh setup_gatewayclass); it never
// changes or deletes it, also when it created it: other gateways may use
// it, and a class alone costs nothing.
func maasGatewayClassPath() string {
	return clusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "openshift-default")
}

// maasGatewayState is the MaaS gateway as read.
type maasGatewayState struct {
	gw          gatewayObject
	found       bool
	owned       bool
	programmed  bool
	address     string
	acceptedErr string
}

func readMaaSGateway(c *Client) (maasGatewayState, error) {
	body, _, err := c.get(maasObjectPath("Gateway", ""))
	if IsK8sError(err, 404) {
		return maasGatewayState{}, nil
	}
	if err != nil {
		return maasGatewayState{}, err
	}
	st := maasGatewayState{found: true}
	if err := json.Unmarshal(body, &st.gw); err != nil {
		return st, fmt.Errorf("parse Gateway %s/%s: %w", inferenceGatewayNamespace, maasGatewayName, err)
	}
	st.owned = gatewayOwned(st.gw.Metadata)
	if cond, ok := findCondition(st.gw.Status.Conditions, "Accepted"); ok && cond.Status == "False" {
		st.acceptedErr = firstNonEmpty(cond.Message, cond.Reason)
	}
	if cond, ok := findCondition(st.gw.Status.Conditions, "Programmed"); ok && cond.Status == "True" {
		st.programmed = true
	}
	for _, a := range st.gw.Status.Addresses {
		st.address = firstNonEmpty(st.address, a.Value)
	}
	return st, nil
}

// maasNamespaceRefusal checks the namespaces maas-default-gateway always
// admits routes from, and where this tool writes its MaaSSubscription and
// MaaSAuthPolicy: models-as-a-service and the infrastructure namespace.
// maas-controller adopts a namespace of that name that already exists
// (models-as-a-service rhoai-3.6 maas-controller/cmd/manager/main.go,
// ensureManagedNamespaceWithClient: "managed namespace already exists",
// then only its required labels are patched), so a user who made one
// first through a project request (annotation openshift.io/requester,
// which every ProjectRequest sets and RHOAI's operators do not) is an
// admin of it: they could attach routes to the MaaS gateway and change
// what this tool writes there. Such a namespace is refused, as is one that
// cannot be read (fail closed); one that does not exist yet is fine (RHOAI
// creates it). It returns a message and an error code, or "".
func maasNamespaceRefusal(c *Client, infra string) (string, string) {
	for _, ns := range []string{maasTenantNamespace, infra} {
		meta, found, err := readObjectMeta(c, "/api/v1/namespaces/"+ns)
		if err != nil {
			return fmt.Sprintf("Cannot read namespace %s, so this tool cannot tell who made it; nothing was changed: %v.%s", ns, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
		}
		if !found {
			continue
		}
		if who := meta.Annotations[projectRequesterAnnotation]; who != "" {
			return fmt.Sprintf("Namespace %s was made through a project request by %s (annotation %s), not by RHOAI, and MaaS takes over a namespace of that name that exists. Its admins could attach routes to the MaaS gateway and change MaaS's subscriptions and access policies there, so this tool does not set up or publish MaaS while it exists. A cluster-admin checks it and, if it is not RHOAI's, deletes it; RHOAI makes it again. Nothing was changed.", ns, who, projectRequesterAnnotation), "not_managed"
		}
	}
	return "", ""
}

// maasGatewayNamespaces are the namespaces the gateway admits routes from
// (setup-gateway.sh ALLOWED_ROUTE_NAMESPACES): the infrastructure namespace
// (maas-api's route), the default tenant's namespace, and each project
// whose model this tool published. A namespace label would need patch on
// every namespace; the documented name list needs nothing beyond the
// gateway this tool owns.
func maasGatewayNamespaces(infraNS string, published []string) []string {
	out := []string{infraNS, maasTenantNamespace}
	for _, p := range published {
		if !containsString(out, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out[2:])
	return out
}

// maasGatewayObjects are the gateway objects (setup-gateway.sh
// loadbalancer mode), by kind, plus an in-cluster listener.
//
// The docs' gateway has one listener, maas.<domain>. Callers inside the
// cluster that use the gateway's Service name find no listener for it:
// live, the dashboard's playground proxy called a published model at
// https://maas-default-gateway-openshift-default.openshift-ingress.svc...
// and got "upstream unreachable". As RHOAI's inference gateway does, a
// second listener answers that name with a service-ca certificate, which
// the options ConfigMap asks for (Gateway API infrastructure.parametersRef;
// the docs' ClusterIP pattern uses the same annotation and Secret name).
// The external listener stays as the docs make it, so /v1/tenants keeps
// its hostname.
func maasGatewayObjects(domain, certificate string, namespaces []string) map[string]map[string]interface{} {
	values := make([]interface{}, len(namespaces))
	for i, n := range namespaces {
		values[i] = n
	}
	labels := toolLabels()
	labels["app.kubernetes.io/name"] = "maas"
	labels["app.kubernetes.io/instance"] = maasGatewayName
	labels["app.kubernetes.io/component"] = "gateway"
	labels["opendatahub.io/managed"] = "false"
	allowed := map[string]interface{}{"namespaces": map[string]interface{}{
		"from": "Selector",
		"selector": map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{
			"key": "kubernetes.io/metadata.name", "operator": "In", "values": values,
		}}},
	}}
	listener := func(name, host, cert string) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "hostname": host, "port": 443, "protocol": "HTTPS", "allowedRoutes": allowed,
			"tls": map[string]interface{}{
				"mode":            "Terminate",
				"certificateRefs": []interface{}{map[string]interface{}{"group": "", "kind": "Secret", "name": cert}},
			},
		}
	}
	return map[string]map[string]interface{}{
		"ConfigMap": {
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": maasGatewayOptions, "namespace": inferenceGatewayNamespace, "labels": toolLabels()},
			"data": map[string]interface{}{
				"service": "metadata:\n  annotations:\n    service.beta.openshift.io/serving-cert-secret-name: " + maasGatewayServiceTLS + "\n",
			},
		},
		"GatewayClass": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "GatewayClass",
			"metadata":   map[string]interface{}{"name": "openshift-default", "labels": toolLabels()},
			"spec":       map[string]interface{}{"controllerName": gatewayControllerName},
		},
		"Gateway": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      "maas-default-gateway",
				"namespace": inferenceGatewayNamespace,
				"labels":    labels,
				"annotations": map[string]interface{}{
					"opendatahub.io/managed":                          "false",
					"security.opendatahub.io/authorino-tls-bootstrap": "true",
				},
			},
			"spec": map[string]interface{}{
				"gatewayClassName": maasGatewayClass,
				"infrastructure": map[string]interface{}{
					"parametersRef": map[string]interface{}{"group": "", "kind": "ConfigMap", "name": maasGatewayOptions},
				},
				"listeners": []interface{}{
					listener("https", maasHostPrefix+domain, certificate),
					listener("https-internal", maasGatewayName+"-"+maasGatewayClass+"."+inferenceGatewayNamespace+".svc.cluster.local", maasGatewayServiceTLS),
				},
			},
		},
	}
}

// maasDBClusterObject is the CloudNativePG cluster: one instance, the
// smallest volume, database and owner "maas" as scripts/setup-database.sh
// names them. A development and test database.
func maasDBClusterObject(ns string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]interface{}{"name": "maas-db", "namespace": ns, "labels": toolLabels()},
		"spec": map[string]interface{}{
			"instances": 1,
			"storage":   map[string]interface{}{"size": "1Gi"},
			"bootstrap": map[string]interface{}{"initdb": map[string]interface{}{"database": maasDBName, "owner": maasDBName}},
		},
	}
}

// maasDBConfigObject is Secret maas-db-config as setup-database.sh makes it
// (create_maas_db_config_secret: key DB_CONNECTION_URL, label app=maas-api).
func maasDBConfigObject(ns, connURL string) map[string]interface{} {
	labels := toolLabels()
	labels["app"] = "maas-api"
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "maas-db-config", "namespace": ns, "labels": labels},
		"type":       "Opaque",
		"data":       map[string]interface{}{"DB_CONNECTION_URL": base64.StdEncoding.EncodeToString([]byte(connURL))},
	}
}

// maasConnectionURL builds DB_CONNECTION_URL from the CloudNativePG
// application Secret (keys username and password, CNPG docs "Application
// secret"), for the cluster's read-write Service, with sslmode=require
// (maas-setup.md "Connection string format"). The URL holds the password:
// it is only ever written to the Secret, never logged or returned.
func maasConnectionURL(c *Client, ns string) (string, error) {
	body, _, err := c.get(namespacedPath("v1", "secrets", ns, "maas-db-app"))
	if err != nil {
		return "", fmt.Errorf("read the database's application Secret %s/%s: %w", ns, maasDBAppSecret, err)
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", errors.New("the database's application Secret cannot be parsed")
	}
	user, err1 := base64.StdEncoding.DecodeString(s.Data["username"])
	pass, err2 := base64.StdEncoding.DecodeString(s.Data["password"])
	if err1 != nil || err2 != nil || len(user) == 0 || len(pass) == 0 {
		return "", fmt.Errorf("the database's application Secret %s/%s has no username or password yet", ns, maasDBAppSecret)
	}
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(string(user), string(pass)),
		Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local:5432", maasDBCluster, ns), Path: "/" + maasDBName, RawQuery: "sslmode=require"}
	return u.String(), nil
}

// readMaaSDBCluster reads the database: found, owned, ready, a message.
func readMaaSDBCluster(c *Client, ns string) (objectMeta, bool, bool, string, error) {
	body, _, err := c.get(maasObjectPath("Cluster", ns))
	if IsK8sError(err, 404) {
		return objectMeta{}, false, false, "", nil
	}
	if err != nil {
		return objectMeta{}, false, false, "", err
	}
	var cl struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Instances int `json:"instances"`
		} `json:"spec"`
		Status struct {
			Phase          string `json:"phase"`
			ReadyInstances int    `json:"readyInstances"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &cl); err != nil {
		return objectMeta{}, true, false, "", err
	}
	ready := cl.Status.ReadyInstances >= 1 && cl.Status.ReadyInstances >= cl.Spec.Instances
	return cl.Metadata, true, ready, firstNonEmpty(cl.Status.Phase, "Starting"), nil
}

// deploymentAvailable reads a Deployment's Available condition: "True",
// "False", "missing" or "" (unreadable).
func deploymentAvailable(c *Client, ns, name string) string {
	body, _, err := c.get(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name))
	if IsK8sError(err, 404) {
		return "missing"
	}
	if err != nil {
		return ""
	}
	var d struct {
		Status struct {
			Conditions []k8sCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &d) != nil {
		return ""
	}
	if cond, ok := findCondition(d.Status.Conditions, "Available"); ok {
		return cond.Status
	}
	return ""
}

// readyCondition reads an object's Ready condition: "True", "False",
// "missing" or "" (unreadable or not reported), with its message.
func readyCondition(c *Client, path string) (string, string) {
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return "missing", ""
	}
	if err != nil {
		return "", ""
	}
	var obj struct {
		Status struct {
			Conditions []k8sCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return "", ""
	}
	if cond, ok := findCondition(obj.Status.Conditions, "Ready"); ok {
		return cond.Status, firstNonEmpty(cond.Message, cond.Reason)
	}
	return "", ""
}

// maasGatewayAuth reads the AuthPolicy maas-controller puts on the MaaS
// gateway (maas-gateway-auth on RHOAI 3.6; the docs name others, so it is
// found by its target and maas-controller's label, never by taking the
// first policy on the gateway: a tier-era gateway-auth-policy may target
// it too): "enforced", "not-enforced", "missing" or "" (unreadable).
func maasGatewayAuth(c *Client) string {
	policies, err := maasGatewayPolicies(c)
	if err != nil {
		return ""
	}
	for _, p := range policies {
		if p.Kind != "AuthPolicy" || !p.Managed {
			continue
		}
		if p.Enforced == "True" {
			return "enforced"
		}
		return "not-enforced"
	}
	return "missing"
}

// groupServed says whether the API server serves group: its discovery
// document answers 404 when no CRD of the group is installed. Any other
// failure is returned, so callers can refuse rather than guess.
func groupServed(c *Client, group string) (bool, error) {
	if v, ok := apiVersionsCache.Get(c.baseURL + "|" + group); ok && len(v) > 0 {
		return true, nil
	}
	body, _, err := c.get("/apis/" + group)
	if IsK8sError(err, 404) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the API group %s: %w", group, err)
	}
	return len(parseGroupVersions(body)) > 0, nil
}

// authorinoTLS checks what scripts/setup-authorino-tls.sh sets on
// Kuadrant's Authorino in ns: the Service's serving-cert annotation, the
// Authorino CR's TLS listener, and the Deployment's trust of the service
// CA (authorinoTrustsServiceCA). It
// returns "" when all are set (or cannot be read: unknown is not a
// failure), else what is missing.
//
// The certificate Secret is the one the Authorino CR's listener names
// (spec.listener.tls.certSecretRef.name; RHOAI's AI gateway may use
// another than the script's authorino-server-cert), returned for the
// commands; the Service must ask service-ca for that one.
func authorinoTLS(c *Client, ns string) (string, bool, string) {
	var missing []string
	known := true
	secret := "authorino-server-cert"
	tlsOK := false
	// The CR at the version its group serves (v1beta1 when discovery cannot
	// tell).
	read, err := getServed(c, authorinoGroup, "/apis/"+authorinoGroup+"/%s/namespaces/"+ns+"/authorinos/authorino", []string{"v1beta1"})
	if body := read.Body; err != nil || body == nil {
		known = false
	} else {
		var a struct {
			Spec struct {
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
		if json.Unmarshal(body, &a) == nil {
			secret = firstNonEmpty(a.Spec.Listener.TLS.CertSecretRef.Name, secret)
			tlsOK = a.Spec.Listener.TLS.Enabled
		}
	}
	// Which Secret the Service must name is the CR's: without the CR it is
	// unknown, not missing.
	if known {
		meta, found, err := readObjectMeta(c, fmt.Sprintf("/api/v1/namespaces/%s/services/%s", ns, "authorino-authorino-authorization"))
		switch {
		case err != nil:
			known = false
		case !found || meta.Annotations[servingCertAnnotation] != secret:
			missing = append(missing, "the Service's serving certificate")
		}
	}
	if known && !tlsOK {
		missing = append(missing, "TLS on Authorino's listener")
	}
	if body, _, err := c.get(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, "authorino")); err != nil {
		known = false
	} else if !authorinoTrustsServiceCA(body) {
		missing = append(missing, "trust of the service CA (SSL_CERT_FILE)")
	}
	return strings.Join(missing, ", "), known, secret
}

// authorinoTrustsServiceCA says whether Authorino's Deployment trusts an
// added CA bundle, either way it is done: the script's SSL_CERT_FILE or
// REQUESTS_CA_BUNDLE (it points them at service-ca.crt), or an SSL_CERT_DIR entry
// that is the mount of a ConfigMap volume, as RHOAI's AI gateway sets it
// up through the Authorino CR's volumes (live on 3.6: ConfigMap
// aigrid-service-ca, inject-cabundle, at /etc/ssl/certs/openshift-service-ca).
func authorinoTrustsServiceCA(deployment []byte) bool {
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name      string           `json:"name"`
						ConfigMap *json.RawMessage `json:"configMap"`
						Projected *struct {
							Sources []struct {
								ConfigMap *json.RawMessage `json:"configMap"`
							} `json:"sources"`
						} `json:"projected"`
					} `json:"volumes"`
					Containers []struct {
						Env []struct {
							Name, Value string
						} `json:"env"`
						VolumeMounts []struct {
							Name      string `json:"name"`
							MountPath string `json:"mountPath"`
						} `json:"volumeMounts"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if json.Unmarshal(deployment, &d) != nil {
		return false
	}
	configMapVolume := map[string]bool{}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			configMapVolume[v.Name] = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					configMapVolume[v.Name] = true
				}
			}
		}
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		mounts := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if configMapVolume[m.Name] {
				mounts[strings.TrimRight(m.MountPath, "/")] = true
			}
		}
		for _, e := range c.Env {
			switch e.Name {
			case "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE":
				if strings.TrimSpace(e.Value) != "" {
					return true
				}
			case "SSL_CERT_DIR":
				for _, dir := range strings.Split(e.Value, ":") {
					if mounts[strings.TrimRight(dir, "/")] {
						return true
					}
				}
			}
		}
	}
	return false
}

// authorinoTLSCommands are scripts/setup-authorino-tls.sh's commands for ns,
// and deploy.sh's restart of Authorino after them. The Secret's name comes
// from Authorino's CR (whoever may edit it chooses it) and a cluster-admin
// pastes these commands into a shell, so a name that is not a Kubernetes
// object name (DNS-1123 subdomain) gets no commands: why says so. Every
// substitution is shell-quoted as well.
func authorinoTLSCommands(ns, secret string) (cmds, why string) {
	if len(ns) > 63 || !dns1123Label.MatchString(ns) {
		return "", fmt.Sprintf("Connectivity Link's namespace %s is not a valid Kubernetes name", strconv.Quote(truncateMessage(ns)))
	}
	if len(secret) > 253 || !dns1123Subdomain.MatchString(secret) {
		return "", fmt.Sprintf("Authorino's CR names its certificate Secret %s (spec.listener.tls.certSecretRef.name), which is not a valid Kubernetes name, so no commands are shown. A cluster-admin checks Authorino authorino in %s", strconv.Quote(truncateMessage(secret)), ns)
	}
	patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"listener": map[string]interface{}{"tls": map[string]interface{}{
		"enabled": true, "certSecretRef": map[string]interface{}{"name": secret}}}}})
	q := shellQuote(ns)
	return fmt.Sprintf(`oc annotate service authorino-authorino-authorization -n %[1]s \
  service.beta.openshift.io/serving-cert-secret-name=%[2]s --overwrite
oc patch authorino authorino -n %[1]s --type=merge \
  -p %[3]s
oc -n %[1]s set env deployment/authorino \
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt \
  REQUESTS_CA_BUNDLE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
oc rollout restart deployment/authorino -n %[1]s`, q, shellQuote(secret), shellQuote(string(patch))), ""
}

// --- status --------------------------------------------------------------------

// GetMaaSStatus reports the MaaS platform part by part, in setup order
// (the docs' platform setup, then their MaaS setup), what Set up would
// change now, and this tool's models with their publication.
func GetMaaSStatus(c *Client) (*types.MaaSStatus, error) {
	st := &types.MaaSStatus{Subscription: maasSubscriptionName, DashboardURL: dashboardURLOrEmpty(c)}
	rec, err := readMaaSRecord(c)
	if err != nil {
		return nil, err
	}
	dsc, dscErr := readMaaSDSC(c)
	infra := maasInfraNamespace(c)
	domain, _ := clusterAppsDomain(c)
	if u := "https://" + maasHostPrefix + domain; domain != "" && snippetURL(u) {
		st.URL = u
	}
	part := func(p types.MaaSPart) { st.Parts = append(st.Parts, p) }
	plan := func(s string) { st.SetupPlan = append(st.SetupPlan, s) }
	var blockers []string

	// Red Hat Connectivity Link (platform-setup.md "Install Gateway API
	// Controller", RHOAI tab).
	csvs, csvErr := listClusterCSVs(c)
	var rhcl, cnpg operatorState
	rhclErr, cnpgReadErr := csvErr, csvErr
	if csvErr == nil {
		rhcl, rhclErr = readOperatorIn(c, rhclOperator, csvs)
		cnpg, cnpgReadErr = readOperatorIn(c, cnpgOperator, csvs)
	}
	kVersion, kvErr := kuadrantServedVersion(c)
	rhclPart := types.MaaSPart{ID: "rhcl", Title: "Red Hat Connectivity Link", DocURL: maasDocsBase + "install/platform-setup.md#install-gateway-api-controller"}
	_, rhclByTool := rec.Shared["rhcl"]
	switch {
	case rhclErr != nil:
		rhclPart.State, rhclPart.Message = "unknown", "Cannot read the installed operators: "+rhclErr.Error()
	case rhcl.installed():
		rhclPart.ManagedByTool = rhclByTool && rhcl.sub != nil && toolOwned(rhcl.sub.Metadata)
		rhclPart.NotManaged = !rhclPart.ManagedByTool
		where := "its Subscription is not one of the names the docs use (kuadrant-operator, rhcl-operator)"
		if rhcl.sub != nil {
			where = fmt.Sprintf("Subscription %s/%s, channel %s", rhcl.sub.Metadata.Namespace, rhcl.sub.Metadata.Name, firstNonEmpty(rhcl.sub.Spec.Channel, "not set"))
		}
		switch {
		case versionCore(rhcl.version) == rhclAuthBypassVersion:
			rhclPart.State, rhclPart.Message = "problem", fmt.Sprintf("Version %s (%s). ", rhcl.version, where)+rhclWarning(rhcl.version)
		case rhcl.phase != "Succeeded":
			rhclPart.State, rhclPart.Message = "waiting", fmt.Sprintf("Version %s is being installed (%s; CSV %s)", rhcl.version, where, firstNonEmpty(rhcl.phase, "pending"))
		default:
			rhclPart.State, rhclPart.Message = "ready", fmt.Sprintf("Version %s runs (%s)", rhcl.version, where)
			rhclPart.Warning = rhclWarning(rhcl.version)
		}
		if rhclBelowMinimum(rhcl.version) {
			action, cmds := rhclUpgradeOffer(c, rhcl)
			if action != nil {
				rhclPart.Actions = append(rhclPart.Actions, *action)
			}
			rhclPart.Commands = cmds
		}
		if action, note := rhclMemoryOffer(c, rhcl); action != nil {
			rhclPart.Actions = append(rhclPart.Actions, *action)
			rhclPart.Warning = strings.TrimSpace(rhclPart.Warning + " " + note)
		} else if note != "" {
			rhclPart.Warning = strings.TrimSpace(rhclPart.Warning + " " + note)
		}
	case kvErr == nil && kVersion != "":
		rhclPart.State, rhclPart.NotManaged = "ready", true
		rhclPart.Message = "Kuadrant's API is served without an rhcl-operator CSV (an upstream Kuadrant, or another install); MaaS uses it as it is"
	case rhcl.sub != nil:
		rhclPart.ManagedByTool, rhclPart.NotManaged = rhclByTool, !rhclByTool
		rhclPart.State, rhclPart.Message = "waiting", fmt.Sprintf("Subscription %s/%s exists; OLM is installing it (%s)", rhcl.sub.Metadata.Namespace, rhcl.sub.Metadata.Name, firstNonEmpty(rhcl.sub.Status.State, "no state yet"))
	default:
		rhclPart.State = "missing"
		if p, msg, _ := planOperatorInstall(c, rhclOperator); msg != "" {
			rhclPart.State, rhclPart.Message = "problem", "Not installed, and Set up cannot install it: "+msg
			blockers = append(blockers, rhclPart.Message)
		} else {
			rhclPart.Message = fmt.Sprintf("Not installed. Set up installs version %s from redhat-operators (channel %s), as the MaaS docs' platform setup does.", p.head, p.channel)
			rhclPart.Commands = rhclInstallCommands(p.channel)
			plan(fmt.Sprintf("Install Red Hat Connectivity Link %s: %sSubscription %s/%s (package %s, channel %s, from redhat-operators)", p.head, operatorPlanPrefix(p), rhclOperator.namespace, rhclOperator.subName, rhclOperator.pkg, p.channel))
		}
	}
	part(rhclPart)

	// The Kuadrant CR (platform-setup.md: "Once the RHCL operator is
	// ready, create a Kuadrant instance").
	kq, kErr := readKuadrant(c)
	kuadrantNS := firstNonEmpty(kq.namespace, rhcl.csvNS, rhclOperator.namespace)
	kPart := types.MaaSPart{ID: "kuadrant", Title: "Kuadrant CR", DocURL: maasDocsBase + "install/platform-setup.md#install-gateway-api-controller"}
	_, kByTool := rec.Shared["kuadrant"]
	switch {
	case kErr != nil:
		kPart.State, kPart.Message = "unknown", "Cannot read the Kuadrant CR: "+kErr.Error()
	case !kq.found:
		kPart.State, kPart.Message = "missing", fmt.Sprintf("No Kuadrant CR. Set up creates %s/kuadrant once Connectivity Link runs, as the docs' platform setup does.", kuadrantNS)
		kPart.Commands = kuadrantCommands(kuadrantNS)
		plan(fmt.Sprintf("Create the Kuadrant CR %s/kuadrant", kuadrantNS))
	case kq.ready:
		kPart.State, kPart.Message = "ready", "Kuadrant "+kq.namespace+"/kuadrant is Ready"
	default:
		kPart.State, kPart.Message = "waiting", "Kuadrant is not Ready: "+firstNonEmpty(kq.message, "no message")
	}
	if kq.found {
		kPart.ManagedByTool = kByTool && toolOwned(kq.meta)
		kPart.NotManaged = !kPart.ManagedByTool
	}
	part(kPart)

	// CloudNativePG, for this tool's database; not needed when
	// maas-db-config names someone else's.
	dbNS := firstNonEmpty(rec.DBNamespace, infra)
	secMeta, secFound, secErr := readObjectMeta(c, maasObjectPath("Secret", dbNS))
	foreignDB := secErr == nil && secFound && !toolOwned(secMeta)
	cnpgServed, cnpgErr := groupServed(c, cnpgGroup)
	if !foreignDB {
		cnpgPart := types.MaaSPart{ID: "cnpg", Title: "CloudNativePG operator", DocURL: maasDocsBase + "install/maas-setup.md#database-setup"}
		_, cnpgByTool := rec.Shared["cnpg"]
		switch {
		case cnpgErr != nil:
			cnpgPart.State, cnpgPart.Message = "unknown", cnpgErr.Error()
		case cnpgServed:
			cnpgPart.ManagedByTool = cnpgByTool && cnpg.sub != nil && toolOwned(cnpg.sub.Metadata)
			cnpgPart.NotManaged = !cnpgPart.ManagedByTool
			cnpgPart.State, cnpgPart.Message = "ready", "Its API (postgresql.cnpg.io) is served"
			if cnpg.installed() {
				cnpgPart.Message = fmt.Sprintf("Version %s runs in %s", cnpg.version, cnpg.csvNS)
			}
		case cnpgReadErr != nil:
			cnpgPart.State, cnpgPart.Message = "unknown", "Cannot read the installed operators: "+cnpgReadErr.Error()
		case cnpg.installed() || cnpg.sub != nil:
			cnpgPart.ManagedByTool, cnpgPart.NotManaged = cnpgByTool, !cnpgByTool
			cnpgPart.State, cnpgPart.Message = "waiting", "OLM is installing it"
		default:
			cnpgPart.State = "missing"
			if p, msg, _ := planOperatorInstall(c, cnpgOperator); msg != "" {
				cnpgPart.State, cnpgPart.Message = "problem", "Not installed, and Set up cannot install it: "+msg+" Or a cluster-admin creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md)."
				blockers = append(blockers, cnpgPart.Message)
			} else {
				cnpgPart.Message = fmt.Sprintf("Not installed. Set up installs version %s from certified-operators (channel %s) for this tool's database. Or a cluster-admin creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md).", p.head, p.channel)
				cnpgPart.Commands = cnpgInstallCommands(p.channel)
				plan(fmt.Sprintf("Install CloudNativePG %s: Subscription %s/%s (package %s, channel %s, from certified-operators)", p.head, cnpgOperator.namespace, cnpgOperator.subName, cnpgOperator.pkg, p.channel))
			}
		}
		part(cnpgPart)
	}

	// The database and its connection Secret, where setup put them.
	dbPart := types.MaaSPart{ID: "database", Title: "Database (PostgreSQL) and maas-db-config", DocURL: maasDocsBase + "install/maas-setup.md#database-setup"}
	clMeta, clFound, clReady, clPhase, clErr := readMaaSDBCluster(c, dbNS)
	_, nsFound, nsErr := readObjectMeta(c, "/api/v1/namespaces/"+dbNS)
	dbPlan := fmt.Sprintf("Run a one-instance CloudNativePG database %s/%s (1 GiB volume, for development and test) and write Secret %s/%s with its address", dbNS, maasDBCluster, dbNS, maasDBConfigSecret)
	switch {
	case secErr != nil:
		dbPart.State, dbPart.Message = "unknown", fmt.Sprintf("Cannot read Secret %s/%s: %v", dbNS, maasDBConfigSecret, secErr)
	case foreignDB:
		dbPart.State, dbPart.NotManaged = "ready", true
		dbPart.Message = fmt.Sprintf("Secret %s/%s exists: maas-api uses the database it names", dbNS, maasDBConfigSecret)
	case clErr == nil && clFound && toolOwned(clMeta):
		dbPart.ManagedByTool = true
		switch {
		case clReady && secFound:
			dbPart.State, dbPart.Message = "ready", fmt.Sprintf("CloudNativePG %s/%s runs (1 instance, development and test use); Secret %s set", dbNS, maasDBCluster, maasDBConfigSecret)
		case clReady:
			dbPart.State, dbPart.Message = "missing", "The database runs; its connection Secret is not written yet"
			plan(fmt.Sprintf("Write Secret %s/%s with the database's address", dbNS, maasDBConfigSecret))
		default:
			dbPart.State, dbPart.Message = "waiting", "The database is starting: "+clPhase
		}
	case clErr == nil && clFound:
		dbPart.State, dbPart.NotManaged = "problem", true
		dbPart.Message = fmt.Sprintf("%s exists and was not created by this tool, so Set up leaves it alone. A cluster-admin creates Secret %s/%s for it, or removes it.", maasDesc("Cluster", dbNS), dbNS, maasDBConfigSecret)
		blockers = append(blockers, dbPart.Message)
	case nsErr != nil:
		dbPart.State, dbPart.Message = "unknown", fmt.Sprintf("Cannot read namespace %s: %v", dbNS, nsErr)
	case !nsFound:
		if _, ok := maasInfraNamespacePath(dbNS); ok {
			dbPart.State = "missing"
			dbPart.Message = fmt.Sprintf("Set up creates namespace %s, the AI gateway's infrastructure namespace, as the docs' setup-database.sh does (RHOAI makes it only once MaaS is on, and the database must come first), then runs a one-instance CloudNativePG database there (development and test use).", dbNS)
			plan(fmt.Sprintf("Create namespace %s (the AI gateway's infrastructure namespace; recorded, and deleted by Remove when nothing else is in it)", dbNS))
			plan(dbPlan)
		} else {
			dbPart.State, dbPart.Message = "problem", fmt.Sprintf("Namespace %s, where maas-api and its database go, does not exist, and this tool creates only the AI gateway's own infrastructure namespaces. A cluster-admin creates it.", dbNS)
			blockers = append(blockers, dbPart.Message)
		}
	default:
		dbPart.State, dbPart.Message = "missing", "Set up runs a one-instance CloudNativePG database (development and test use) in "+dbNS
		plan(dbPlan)
	}
	if rec.InfraNamespace != "" && rec.InfraNamespace == dbNS {
		dbPart.ManagedByTool = true
	}
	part(dbPart)

	// The gateway.
	gwPart := types.MaaSPart{ID: "gateway", Title: "Gateway maas-default-gateway", DocURL: maasDocsBase + "install/maas-setup.md#create-gateway"}
	gw, gwErr := readMaaSGateway(c)
	switch {
	case gwErr != nil:
		gwPart.State, gwPart.Message = "unknown", "Cannot read the gateway: "+gwErr.Error()
	case !gw.found:
		gwPart.State, gwPart.Message = "missing", "Set up creates it with an external load balancer at "+firstNonEmpty(st.URL, "maas.<apps domain>")
		plan(fmt.Sprintf("Create Gateway %s/%s with an external load balancer at %s (a cloud cost while it exists), and GatewayClass %s if the cluster has none", inferenceGatewayNamespace, maasGatewayName, firstNonEmpty(strings.TrimPrefix(st.URL, "https://"), "maas.<apps domain>"), maasGatewayClass))
	default:
		gwPart.ManagedByTool, gwPart.NotManaged = gw.owned, !gw.owned
		switch {
		case gw.gw.admitsAll() && gw.owned:
			gwPart.State, gwPart.Message = "problem", "A listener admits routes from every namespace, which this tool never sets: any project could route through MaaS's gateway. Set up writes back the documented list of namespaces."
			plan(fmt.Sprintf("Gateway %s/%s: admit routes only from the namespaces it lists again (the MaaS docs' setup-gateway.sh)", inferenceGatewayNamespace, maasGatewayName))
		case gw.gw.admitsAll():
			gwPart.State = "problem"
			gwPart.Message = "This gateway, which this tool did not create, admits routes from every namespace, so any project editor could put a route on MaaS's gateway. The MaaS docs' gateway (setup-gateway.sh) lists the namespaces instead. This tool publishes nothing through it until its owner applies the command below; it does not change a gateway it did not create."
			gwPart.Commands = gatewayAllowedRoutesCommand(gw.gw, maasGatewayNamespaces(infra, rec.projects()))
		case gw.acceptedErr != "":
			gwPart.State, gwPart.Message = "problem", "Not accepted: "+truncateMessage(gw.acceptedErr)
		case gw.programmed && gw.address != "":
			gwPart.State, gwPart.Message = "ready", "Programmed at "+gw.address
		default:
			gwPart.State, gwPart.Message = "waiting", "Waiting for its load balancer"
		}
		if !gw.owned {
			if h := gw.gw.externalHost(); h != "" && snippetURL("https://"+h) {
				st.URL = "https://" + h
			}
		}
	}
	part(gwPart)

	// Authorino TLS (maas-setup.md "Authorino TLS",
	// scripts/setup-authorino-tls.sh), on Kuadrant's Authorino.
	tlsPart := types.MaaSPart{ID: "authorino-tls", Title: "Authorino TLS", DocURL: maasDocsBase + "configuration-and-management/tls-configuration.md"}
	_, tlsByTool := rec.Shared["authorino-tls"]
	tlsPlan := "Configure Authorino TLS as the docs' setup-authorino-tls.sh does: the serving-certificate annotation on Service authorino-authorino-authorization, TLS on the Authorino CR's listener, SSL_CERT_FILE on Deployment authorino, then restart it"
	if kq.found {
		missing, known, secret := authorinoTLS(c, kq.namespace)
		switch {
		case missing != "":
			cmds, why := authorinoTLSCommands(kq.namespace, secret)
			if why != "" {
				tlsPart.State, tlsPart.Message = "problem", "Not configured: "+missing+". "+why+". Without it, API keys are refused."
				blockers = append(blockers, tlsPart.Message)
				break
			}
			tlsPart.State = "missing"
			tlsPart.Message = "Not configured: " + missing + ". Without it, API keys are refused. Set up (or Configure Authorino TLS) applies the docs' setup-authorino-tls.sh steps in " + kq.namespace + "."
			tlsPart.Commands = cmds
			tlsPart.Actions = []types.MaaSAction{authorinoTLSAction(kq.namespace, secret, missing)}
			plan(tlsPlan + " (in " + kq.namespace + ")")
		case known:
			tlsPart.State, tlsPart.Message = "ready", "Authorino in "+kq.namespace+" serves TLS and trusts the service CA"
			tlsPart.ManagedByTool, tlsPart.NotManaged = tlsByTool, !tlsByTool
		default:
			tlsPart.State, tlsPart.Message = "unknown", "Cannot read Authorino's configuration"
		}
	} else {
		tlsPart.State, tlsPart.Message = "missing", "Set up configures it once Kuadrant has made Authorino, as the docs' setup-authorino-tls.sh does."
		plan(tlsPlan)
	}
	part(tlsPart)

	// Gateway policies maas-controller does not manage: a tier-based MaaS
	// left them (migration/upgrade-to-3.4.md).
	if kvErr == nil && kVersion != "" {
		policies, polErr := maasGatewayPolicies(c)
		foreign := foreignGatewayPolicies(policies)
		polPart := types.MaaSPart{ID: "tier-policies", Title: "Old gateway policies", DocURL: maasDocsBase + "migration/upgrade-to-3.4.md#phase-4-manual-cleanup-of-old-tier-resources"}
		switch {
		case polErr != nil:
			polPart.State, polPart.Message = "unknown", "Cannot list the policies on the MaaS gateway: "+polErr.Error()
			part(polPart)
		case len(foreign) > 0:
			polPart.State, polPart.NotManaged = "manual", true
			polPart.Message = fmt.Sprintf("%s %s on %s and %s not managed by maas-controller. RHOAI's upgrade from the tier-based MaaS leaves its gateway policies in place, and with old and new gateway policies \"Kuadrant behavior is undefined\" (the MaaS docs' upgrade guide), so this tool publishes nothing until they are gone.",
				strings.Join(policyNames(foreign), ", "), verb(len(foreign), "is", "are"), maasGatewayName, verb(len(foreign), "is", "are"))
			var legacy, other []gatewayPolicy
			for _, p := range foreign {
				if legacyTierPolicy(p) {
					legacy = append(legacy, p)
				} else {
					other = append(other, p)
				}
			}
			if len(legacy) > 0 {
				polPart.Actions = []types.MaaSAction{tierPoliciesAction(legacy)}
			}
			if len(other) > 0 {
				polPart.Message += fmt.Sprintf(" %s %s not among the names the docs give for the old policies, so this tool leaves %s to a cluster-admin.", strings.Join(policyNames(other), ", "), verb(len(other), "is", "are"), verb(len(other), "it", "them"))
			}
			polPart.Commands = legacyPolicyCommands(foreign)
			part(polPart)
		}
	}

	// The DataScienceCluster: the AI gateway and MaaS in it, last.
	dscPart := types.MaaSPart{ID: "dsc", Title: "MaaS in the DataScienceCluster", DocURL: maasDocsBase + "install/maas-setup.md#configure-datasciencecluster"}
	switch {
	case dscErr != nil:
		dscPart.State, dscPart.Message = "unknown", "Cannot read the DataScienceCluster: "+dscErr.Error()
	case dsc.read.Object == nil:
		st.Blocked = &types.ComponentBlock{Component: "aigateway", Reason: "MaaS needs RHOAI's DataScienceCluster, and this cluster has none. A cluster-admin creates one on the Components page."}
		dscPart.State, dscPart.Message = "problem", "No DataScienceCluster"
	case dsc.maas == "Managed" && dsc.aigateway == "Managed":
		dscPart.ManagedByTool, dscPart.NotManaged = rec.DSCChanged, !rec.DSCChanged
		switch dsc.ready {
		case "False":
			dscPart.State, dscPart.Message = "waiting", "modelsAsAService is Managed; not ready: "+truncateMessage(dsc.readyMsg)
		default:
			dscPart.State, dscPart.Message = "ready", "aigateway and modelsAsAService are Managed"
		}
	default:
		var fields []string
		if dsc.aigateway != "Managed" {
			fields = append(fields, fmt.Sprintf("aigateway.managementState %s to Managed", firstNonEmpty(dsc.aigateway, "not set")))
		}
		if dsc.maas != "Managed" {
			fields = append(fields, fmt.Sprintf("aigateway.modelsAsAService.managementState %s to Managed", firstNonEmpty(dsc.maas, "not set")))
		}
		dscPart.State = "missing"
		dscPart.Message = fmt.Sprintf("aigateway is %s and modelsAsAService is %s. Set up sets them to Managed, last, as the docs' DataScienceCluster step does; Remove puts back what this tool changed.", firstNonEmpty(dsc.aigateway, "not set"), firstNonEmpty(dsc.maas, "not set"))
		dscPart.Commands = maasDSCCommand(dsc.name)
		dscPart.ManagedByTool = rec.AIGatewayChanged
		if dsc.argoApp != "" {
			dscPart.State = "problem"
			dscPart.Message = gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service") + " Set " + strings.Join(fields, " and ") + " in Git."
			blockers = append(blockers, dscPart.Message)
		} else {
			plan(fmt.Sprintf("DataScienceCluster %s: %s (the values before are recorded, and Remove restores them)", dsc.name, strings.Join(fields, ", ")))
		}
	}

	// maas-api and the default tenant, which the operator creates.
	apiPart := types.MaaSPart{ID: "maas-api", Title: "MaaS API (maas-api)", DocURL: maasDocsBase + "install/validation.md"}
	tenantPart := types.MaaSPart{ID: "tenant", Title: "Default tenant and gateway authentication", DocURL: maasDocsBase + "install/maas-setup.md#maastenantconfig-cr"}
	if dsc.maas == "Managed" {
		switch deploymentAvailable(c, infra, "maas-api") {
		case "True":
			apiPart.State, apiPart.Message = "ready", "Available in "+infra
		case "missing":
			apiPart.State, apiPart.Message = "waiting", "The operator has not created it yet"
		case "False":
			apiPart.State, apiPart.Message = "waiting", "Not available yet (it restarts until the database answers)"
		default:
			apiPart.State, apiPart.Message = "unknown", "Cannot read Deployment maas-api"
		}
		ai, aiMsg := readyCondition(c, namespacedPath(maasAPI(c), "aitenants", "ai-tenants", "models-as-a-service"))
		tc, tcMsg := readyCondition(c, namespacedPath(maasAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant"))
		auth := maasGatewayAuth(c)
		switch {
		case ai == "True" && tc == "True" && auth == "enforced":
			tenantPart.State, tenantPart.Message = "ready", "Tenant models-as-a-service is Ready; the gateway's authentication is enforced"
		case ai == "False" || tc == "False":
			tenantPart.State, tenantPart.Message = "waiting", "Tenant not ready: "+truncateMessage(firstNonEmpty(aiMsg, tcMsg, "no message"))
		case ai == "" || tc == "" || auth == "":
			tenantPart.State, tenantPart.Message = "unknown", "Not reported yet"
		default:
			tenantPart.State, tenantPart.Message = "waiting", fmt.Sprintf("Waiting for the operator (tenant %s, configuration %s, authentication %s)", strings.ToLower(ai), strings.ToLower(tc), auth)
		}
	} else {
		apiPart.State, apiPart.Message = "missing", "The operator creates it once MaaS is turned on"
		tenantPart.State, tenantPart.Message = "missing", "The operator creates it once MaaS is turned on"
	}
	part(dscPart)
	part(apiPart)
	part(tenantPart)

	// The platform as a whole. The shared pieces (Connectivity Link, its
	// Kuadrant CR and Authorino TLS, CloudNativePG) do not make the
	// platform this tool's to remove: Remove leaves them.
	p := &st.Platform
	p.Namespace = infra
	p.Deployed = dsc.maas == "Managed"
	p.Ready = true
	var waiting []string
	for _, pt := range st.Parts {
		if pt.ManagedByTool && !maasSharedPart(pt.ID) {
			p.ManagedByTool = true
		}
		if pt.State != "ready" && (pt.ID != "authorino-tls" || pt.State != "unknown") {
			p.Ready = false
			if pt.State != "missing" {
				waiting = append(waiting, pt.Title)
			}
		}
	}
	if rec.DSCChanged || rec.AIGatewayChanged || rec.InfraNamespace != "" {
		p.ManagedByTool = true
	}
	switch {
	case p.Ready:
		p.Message = "Ready"
	case !p.Deployed && !p.ManagedByTool && len(waiting) == 0:
		p.Message = "Not set up"
	default:
		p.Message = "Needs attention: " + strings.Join(waiting, ", ")
		if len(waiting) == 0 {
			p.Message = "Not set up completely"
		}
	}
	switch {
	case st.Blocked != nil:
		p.SetupBlockedReason = st.Blocked.Reason
	case len(blockers) > 0:
		p.SetupBlockedReason = blockers[0]
	}
	if !p.ManagedByTool {
		p.TeardownBlockedReason = "No part of the MaaS platform was set up by this tool, so there is nothing for it to remove."
	}

	// This tool's models and their publication.
	models, err := listLLMInferenceServices(c)
	if err != nil && !errors.Is(err, errLLMISvcAPIMissing) {
		slog.Debug("cannot list models for MaaS", "error", err)
	}
	rec, recErr := readMaaSRecord(c)
	authPhase, _ := maasPhase(c, namespacedPath(maasAPI(c), "maasauthpolicies", "models-as-a-service", "rhoai-nightly-updater-access"))
	subPhase, _ := maasPhase(c, namespacedPath(maasAPI(c), "maassubscriptions", "models-as-a-service", "rhoai-nightly-updater-subscription"))
	owned, ownedErr := readModelsRecord(c)
	if ownedErr != nil {
		slog.Debug("cannot read the models record for MaaS", "error", ownedErr)
	}
	for _, m := range models {
		entry, _ := catalogEntry(m.Metadata.Name)
		if _, ok := toolModelPath(m.Metadata.Namespace, m.Metadata.Name); !ok || !owned.owns(m.Metadata) {
			continue
		}
		item := types.MaaSModel{ResourceState: types.ResourceState{Namespace: m.Metadata.Namespace, Name: m.Metadata.Name, ManagedByTool: true}}
		// The catalog's name: the object's annotation is the project's to change.
		item.DisplayName = entry.DisplayName
		if cond, ok := findCondition(m.Status.Conditions, "Ready"); ok && cond.Status == "True" {
			item.ModelReady = true
		}
		if entry.Type != modelTypeChat {
			item.Message = "Not offered for MaaS"
			item.SetupBlockedReason = maasEmbeddingReason
			st.Items = append(st.Items, item)
			continue
		}
		ref, found, _ := readMaaSModelRef(c, m.Metadata.Namespace, m.Metadata.Name)
		if found && toolOwned(ref.Metadata) {
			item.Published, item.Deployed = true, true
			item.Phase, item.Endpoint = ref.Status.Phase, ref.Status.Endpoint
			item.Access, item.Subscription = authPhase, subPhase
			item.Ready = item.Phase == "Ready" && authPhase == "Active" && subPhase == "Active"
			_, recordedRefs := rec.modelRefs(m.Metadata.Namespace, m.Metadata.Name)
			switch refs := m.gatewayRefNames(); {
			case modelHasMaaSRef(m) && len(refs) > 1 && recErr == nil && recordedRefs:
				// This build published it MaaS-only (its refs are recorded):
				// someone added a gateway since, and Unpublish and Publish
				// refuse until the refs are set (refsChangedRefusal).
				item.Ready, item.Partly = false, true
				item.Message = refsAddedStatus(m.Metadata.Namespace, m.Metadata.Name, refs)
			case modelHasMaaSRef(m) && len(refs) > 1:
				// An earlier build kept the other gateways (maas_routing.go):
				// the rate limit fails every call on them.
				item.Ready, item.Partly = false, true
				item.Message = fmt.Sprintf("Partly published: the model routes through %s, as an earlier build of this tool published it, so calls to its direct address fail with 500. Unpublish, then Publish again.", strings.Join(refs, " and "))
			case item.Ready:
				item.Message = "Published"
			default:
				item.Message = fmt.Sprintf("Published; waiting (model reference %s, access %s, subscription %s)", firstNonEmpty(item.Phase, "pending"), firstNonEmpty(authPhase, "pending"), firstNonEmpty(subPhase, "pending"))
			}
		} else if left := maasLeftovers(c, m, recErr == nil && rec.published(m.Metadata.Namespace, m.Metadata.Name)); !found && len(left) > 0 {
			// A publication undone half-way (live: Unpublish deleted the
			// model reference, then failed on the model): Unpublish again
			// finishes it, so the tab must still offer it.
			item.Published, item.Deployed, item.Partly = true, true, true
			item.Message = "Partly published: " + strings.Join(left, " and ") + " still in place. Unpublish to finish removing it."
		} else {
			item.Message = "Not published"
			if found {
				item.Message = "A MaaSModelRef of this name exists that this tool did not create"
				item.SetupBlockedReason = item.Message + "; it is left alone."
			}
			if !p.Ready {
				item.SetupBlockedReason = firstNonEmpty(item.SetupBlockedReason, maasNotReadyRowReason(st))
			} else if !item.ModelReady {
				item.SetupBlockedReason = firstNonEmpty(item.SetupBlockedReason, "The model is not ready yet.")
			}
		}
		st.Items = append(st.Items, item)
	}
	// A recorded publication whose model is gone (the model or its project
	// was deleted elsewhere): a row, so Unpublish can clear what is left.
	if recErr == nil {
		for _, p := range rec.pubs() {
			shown := false
			for _, it := range st.Items {
				shown = shown || (it.Namespace == p.project && it.Name == p.model)
			}
			if shown {
				continue
			}
			left := []string{"this tool's record of the publication"}
			if ref, found, _ := readMaaSModelRef(c, p.project, p.model); found && toolOwned(ref.Metadata) {
				left = append([]string{"MaaSModelRef " + p.model}, left...)
			}
			st.Items = append(st.Items, types.MaaSModel{
				ResourceState: types.ResourceState{Namespace: p.project, Name: p.model, ManagedByTool: true, Deployed: true,
					Message: "Partly published: no model of this name is in project " + p.project + " any more (it was deleted there, or the project was); " + strings.Join(left, " and ") + " still in place. Unpublish to finish removing it."},
				DisplayName: p.model,
				Published:   true,
				Partly:      true,
			})
		}
	}
	sort.Slice(st.Items, func(i, j int) bool {
		if st.Items[i].Namespace != st.Items[j].Namespace {
			return st.Items[i].Namespace < st.Items[j].Namespace
		}
		return st.Items[i].Name < st.Items[j].Name
	})
	return st, nil
}

// maasLeftovers names what is left of a publication of the tool's model
// once its MaaSModelRef is gone: the model's route through the MaaS
// gateway (its refs, or a route KServe has not moved yet, read only for a
// recorded publication), and this tool's record of the publication.
func maasLeftovers(c *Client, m llmisvcObject, recorded bool) []string {
	var left []string
	if modelHasMaaSRef(m) {
		left = append(left, "the model's route through "+maasGatewayName)
	} else if recorded {
		parents, _, err := readModelRoute(c, m.Metadata.Namespace, m.Metadata.Name)
		if err == nil && containsString(parents, inferenceGatewayNamespace+"/"+maasGatewayName) {
			left = append(left, "the model's route through "+maasGatewayName+" (KServe has not moved it yet)")
		}
	}
	if recorded {
		left = append(left, "this tool's record of the publication")
	}
	return left
}

// toolOwned: this tool created the object (its label, and a write by it).
func toolOwned(meta objectMeta) bool { return meta.hasToolLabel() && meta.hasManager(toolManagers) }

// dashboardURLOrEmpty is the dashboard's address, or "".
func dashboardURLOrEmpty(c *Client) string {
	return cachedDashboardURL(c)
}

// maasModelRefObject is a MaaS custom resource as read.
type maasModelRefObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ModelRefs []map[string]interface{} `json:"modelRefs"`
	} `json:"spec"`
	Status struct {
		Phase    string `json:"phase"`
		Endpoint string `json:"endpoint"`
	} `json:"status"`
}

// maasModelRefPath is the path of the MaaSModelRef of a chat model of the
// catalog in project; only those names, written out so the RBAC rules
// (resourceNames) can be checked against them (rbac_coverage_test.go). An
// embedding model has none: it is not published.
func maasModelRefPath(c *Client, project, name string) (string, bool) {
	switch name {
	case "smollm2-135m-instruct":
		return namespacedPath(maasAPI(c), "maasmodelrefs", project, "smollm2-135m-instruct"), true
	case "qwen25-05b-instruct":
		return namespacedPath(maasAPI(c), "maasmodelrefs", project, "qwen25-05b-instruct"), true
	case "qwen3-1-7b":
		return namespacedPath(maasAPI(c), "maasmodelrefs", project, "qwen3-1-7b"), true
	}
	return "", false
}

// readMaaSModelRef reads the MaaSModelRef of model name in project (none
// for a model that is not publishable).
func readMaaSModelRef(c *Client, project, name string) (maasModelRefObject, bool, error) {
	path, ok := maasModelRefPath(c, project, name)
	if !ok {
		return maasModelRefObject{}, false, nil
	}
	return readMaaSObject(c, path)
}

func readMaaSObject(c *Client, path string) (maasModelRefObject, bool, error) {
	var obj maasModelRefObject
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return obj, false, nil
	}
	if err != nil {
		return obj, false, err
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return obj, false, err
	}
	return obj, true, nil
}

// maasPhase is a MaaS object's status.phase ("" when absent).
func maasPhase(c *Client, path string) (string, bool) {
	obj, found, err := readMaaSObject(c, path)
	if err != nil || !found {
		return "", false
	}
	return obj.Status.Phase, true
}

// MaaSPublishedIn reports whether anything of a publication of this tool's
// model name in project is left, for the Models teardown (never for a
// model that is not publishable): its MaaSModelRef,
// this tool's record of it, or the model's route through the MaaS gateway.
// Unpublish removes all three (and the project from the shared
// subscription, access policy and gateway), so the model goes after it.
func MaaSPublishedIn(c *Client, project, name string) (bool, error) {
	refPath, ok := maasModelRefPath(c, project, name)
	if !ok {
		return false, nil
	}
	_, found, err := readObjectMeta(c, refPath)
	if err != nil && !IsK8sError(err, 404) {
		return false, err
	}
	if found {
		return true, nil
	}
	rec, err := readMaaSRecord(c)
	if err != nil {
		return false, err
	}
	if rec.published(project, name) {
		return true, nil
	}
	model, found, err := readToolModel(c, project, name)
	if err != nil {
		return false, err
	}
	if !found || !modelHasMaaSRef(model) {
		return false, nil
	}
	return ownsModel(c, model.Metadata)
}
