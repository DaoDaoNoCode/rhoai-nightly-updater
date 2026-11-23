package serving

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
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
//     (SECURITY.md, Models as a Service).
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
	MaaSGroup        = "maas.opendatahub.io"
	MaaSGatewayName  = "maas-default-gateway"
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
	MaaSTenantNamespace   = "models-as-a-service"
	maasAITenantNamespace = "ai-tenants"
	MaaSAuthPolicyName    = "rhoai-nightly-updater-access"
	MaaSSubscriptionName  = "rhoai-nightly-updater-subscription"
	// maasModelManager is the field manager of the gateway ref this tool
	// adds to its model: removing it reverts exactly that change.
	maasModelManager = "rhoai-nightly-updater-maas"
	// maasDSCManager is the field manager of modelsAsAService, so removal
	// restores it only while nobody has set it since.
	maasDSCManager = "rhoai-nightly-updater-maas-dsc"
	// MaaSModelFinalizer is maas-controller's on a MaaSModelRef
	// (maasmodelref_controller.go).
	MaaSModelFinalizer = "maas.opendatahub.io/model-cleanup"
	MaaSRecordKey      = "maas"
	maasRecordManager  = "rhoai-nightly-updater-maas-record"
	// The group a published model is offered to, as the docs' sample.
	maasSubjectGroup = "system:authenticated"
	CNPGGroup        = "postgresql.cnpg.io"
	maasDocsBase     = "https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/content/"
)

var (
	// maasDatabaseTimeout bounds the wait for the database to run.
	maasDatabaseTimeout = 5 * time.Minute
	// maasGatewayTimeout bounds the wait for Programmed (setup-gateway.sh: 120 s).
	maasGatewayTimeout = 2 * time.Minute
	// maasPlatformTimeout bounds the wait for maas-api and the tenant
	// (maas-setup.md waits 300 s).
	maasPlatformTimeout = 5 * time.Minute
	// maasPublishTimeout bounds the wait for a published model to be Ready.
	maasPublishTimeout = 5 * time.Minute
	// maasRouteTimeout bounds the wait for KServe to move the model's
	// HTTPRoute after its gateway refs change (live: about 6 s).
	maasRouteTimeout  = 2 * time.Minute
	maasDeleteTimeout = 2 * time.Minute
)

// authorinoGroup is the Authorino operator's API group (the Authorino CR).
const authorinoGroup = "operator.authorino.kuadrant.io"

// MaaSAPI is maas.opendatahub.io at the version the cluster serves (its
// preferred one; v1alpha1, RHOAI 3.6's, when discovery cannot tell), for
// the MaaS objects' paths and apiVersion.
func MaaSAPI(c *kube.Client) string {
	return MaaSGroup + "/" + rhoai.ServedVersions(c, MaaSGroup, []string{"v1alpha1"}).Versions[0]
}

// --- record (this tool's namespace, snapshot ConfigMap key "maas") ---------------

// MaaSRecord is what this tool did to the platform that the cluster cannot
// tell by itself: whether it set modelsAsAService and what it was before,
// the models it published ("<project>/<model>"), and each published
// model's gateway refs before its publication. ReadMaaSRecord reads an
// entry of a project only (in either), written before models were named,
// as the default model's.
type MaaSRecord struct {
	DSCChanged               bool     `json:"dscChanged,omitempty"`
	PreviousModelsAsAService string   `json:"previousModelsAsAService,omitempty"`
	Published                []string `json:"published,omitempty"`
	// ModelRefs is, by publication ("<project>/<model>"), the model's
	// spec.router.gateway.refs as publish read them before routing it
	// through the MaaS gateway alone; Unpublish writes them back. A
	// publication by an earlier build has none.
	ModelRefs map[string]MaaSModelRefs `json:"modelRefs,omitempty"`
	// GenAI is, by publication, the genai label and annotation publish
	// removed from the model (maas_genai.go); Unpublish puts them back.
	GenAI map[string]maasGenAI `json:"genAI,omitempty"`
	// DBNamespace is where setup put the database and maas-db-config, so
	// removal and the status look there even when the infrastructure
	// namespace reads differently later.
	DBNamespace string `json:"dbNamespace,omitempty"`
	// Database is the database setup chose and made there ("external",
	// "development" or "cnpg"; maas_database.go), written before its
	// objects. Empty in records of earlier builds, which made only
	// CloudNativePG databases.
	Database string `json:"database,omitempty"`
	// POCCredsUID is the UID of Secret postgres-creds (the development
	// database's password) as this tool created it, in DBNamespace. The
	// tool never reads that Secret (no get or patch on it): it knows it
	// only by this record, and deletes only the object with this UID.
	POCCredsUID string `json:"pocCredsUID,omitempty"`
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

func (r *MaaSRecord) setShared(key, what string) {
	if r.Shared == nil {
		r.Shared = map[string]string{}
	}
	r.Shared[key] = what
}

func ReadMaaSRecord(c *kube.Client) (MaaSRecord, error) {
	var rec MaaSRecord
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return rec, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return rec, fmt.Errorf("read this tool's MaaS record: %w", err)
	}
	if !found {
		return rec, nil
	}
	if raw := cm.Data[MaaSRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return rec, fmt.Errorf("parse this tool's MaaS record: %w", err)
		}
	}
	var keys []string
	for _, p := range rec.Pubs() {
		keys = append(keys, p.Key())
	}
	rec.Published = keys
	if len(rec.ModelRefs) > 0 {
		def, _ := CatalogEntry("")
		refs := map[string]MaaSModelRefs{}
		for k, v := range rec.ModelRefs {
			if !strings.Contains(k, "/") {
				k = MaaSPub{k, def.Name}.Key()
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

// WriteMaaSRecord applies the record (operations hold the cluster lock).
func WriteMaaSRecord(c *kube.Client, rec MaaSRecord) error {
	rec.At, rec.By = kube.NowRFC3339(), kube.GetUser(c)
	sort.Strings(rec.Published)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, MaaSRecordKey, maasRecordManager, string(data)); err != nil {
		return fmt.Errorf("record this tool's MaaS changes: %w", err)
	}
	return nil
}

// MaaSModelRefs are a model's gateway refs as read; none is KServe's
// default gateway (spec.router.gateway {}). NoRoute: the model had no route
// before it was published: the Models tab set it up to be reached through
// MaaS only, and publishing was part of that setup (publishToMaaS), so
// Unpublish leaves it with no route at all (only inside the cluster), never
// on a gateway nobody chose.
type MaaSModelRefs struct {
	Refs    GatewayRefs `json:"refs,omitempty"`
	NoRoute bool        `json:"noRoute,omitempty"`
}

// MaaSPub is one publication: a catalog model in a project.
type MaaSPub struct{ Project, Model string }

func (p MaaSPub) Key() string { return p.Project + "/" + p.Model }

// Pubs are the recorded publications; an entry of a project only is the
// default model's.
func (r MaaSRecord) Pubs() []MaaSPub {
	def, _ := CatalogEntry("")
	var out []MaaSPub
	seen := map[string]bool{}
	for _, e := range r.Published {
		project, model, found := strings.Cut(e, "/")
		if !found {
			model = def.Name
		}
		p := MaaSPub{project, model}
		if !seen[p.Key()] {
			seen[p.Key()] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func (r MaaSRecord) IsPublished(project, model string) bool {
	return slices.Contains(r.Published, MaaSPub{project, model}.Key())
}

// ModelRefsOf are the refs publish recorded for model in project; none
// recorded for a publication by an earlier build.
func (r MaaSRecord) ModelRefsOf(project, model string) (MaaSModelRefs, bool) {
	refs, ok := r.ModelRefs[MaaSPub{project, model}.Key()]
	return refs, ok
}

func (r *MaaSRecord) setModelRefs(project, model string, refs MaaSModelRefs) {
	if r.ModelRefs == nil {
		r.ModelRefs = map[string]MaaSModelRefs{}
	}
	r.ModelRefs[MaaSPub{project, model}.Key()] = refs
}

func (r *MaaSRecord) SetPublished(project, model string, on bool) {
	key := MaaSPub{project, model}.Key()
	var out []string
	for _, p := range r.Pubs() {
		if p.Key() != key {
			out = append(out, p.Key())
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

// Projects are the projects with a recorded publication: the MaaS gateway
// admits routes from them.
func (r MaaSRecord) Projects() []string {
	var out []string
	for _, p := range r.Pubs() {
		if !slices.Contains(out, p.Project) {
			out = append(out, p.Project)
		}
	}
	return out
}

// maasEmbeddingReason is why an embedding model is not offered for MaaS.
const maasEmbeddingReason = "This tool publishes chat models only. Use the embedding model at its own address (the Models tab shows it), or add it to the Gen AI playground."

// --- reads ---------------------------------------------------------------------

// MaaSInfraNamespace is where maas-api runs and maas-db-config goes: the
// tenant's status.infraNamespace when MaaS runs, else derived from the
// applications namespace as the docs' scripts do (derive_infra_namespace in
// scripts/setup-database.sh: redhat-ods-applications ->
// redhat-ai-gateway-infra, opendatahub -> odh-ai-gateway-infra). The two
// names are that script's documented mapping, applied to the discovered
// applications namespace. err is set when neither is known; callers that
// change something refuse then.
//
// The name is derived only when the tenant definitely reports none: it does
// not exist (404: MaaS is off, or its API is not served) or its
// status.infraNamespace is empty (the operator has not reported it yet). Any
// other failure to read it (forbidden, a server error, a body that does not
// parse) is returned: the tenant may name another namespace, and a read
// failure is never replaced by a default name (rhoai_namespaces.go).
func MaaSInfraNamespace(c *kube.Client) (string, error) {
	const what = "the default tenant (MaaSTenantConfig models-as-a-service/default-tenant)"
	body, _, err := c.Get(rhoai.NamespacedPath(MaaSAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant"))
	switch {
	case err == nil:
		var tc struct {
			Status struct {
				InfraNamespace string `json:"infraNamespace"`
			} `json:"status"`
		}
		if err := json.Unmarshal(body, &tc); err != nil {
			return "", fmt.Errorf("cannot tell where maas-api and its database go: %s cannot be parsed (%w)", what, err)
		}
		if tc.Status.InfraNamespace != "" {
			return tc.Status.InfraNamespace, nil
		}
	case kube.IsK8sNotFound(err):
	default:
		return "", fmt.Errorf("cannot tell where maas-api and its database go: %s cannot be read (%w)", what, err)
	}
	apps, err := rhoai.AppsNamespace(c)
	if err != nil {
		return "", fmt.Errorf("cannot tell where maas-api and its database go: the default tenant reports no infrastructure namespace, and the applications namespace it is derived from is not known (%w)", err)
	}
	switch apps {
	case "redhat-ods-applications":
		return "redhat-ai-gateway-infra", nil
	case "opendatahub":
		return "odh-ai-gateway-infra", nil
	default:
		return apps, nil
	}
}

// maasDBNamespace is where this tool's database is: the recorded
// namespace, else MaaSInfraNamespace.
func maasDBNamespace(c *kube.Client, rec MaaSRecord) (string, error) {
	if rec.DBNamespace != "" {
		return rec.DBNamespace, nil
	}
	return MaaSInfraNamespace(c)
}

// maasDSC is the DataScienceCluster as MaaS needs it.
type maasDSC struct {
	read      rhoai.DSCRead
	name      string
	aigateway string // managementState, "" when not named
	maas      string // aigateway.modelsAsAService.managementState
	ready     string // ModelsAsAServiceReady: True, False or "" (unknown)
	readyMsg  string
	argoApp   string // the Argo CD application managing it (argo.managedBy)
	argo      argo.Tracking
}

func readMaaSDSC(c *kube.Client) (maasDSC, error) {
	r, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return maasDSC{}, err
	}
	d := maasDSC{read: r}
	if r.Object == nil {
		return d, nil
	}
	meta, _ := r.Object["metadata"].(map[string]any)
	d.name, _ = meta["name"].(string)
	d.argo = argo.TrackingOf(c, r.Object, rhoai.DSCGroup, "DataScienceCluster")
	d.argoApp = d.argo.ManagedBy()
	d.aigateway, _ = envkit.ComponentManagement(r.Object, "aigateway")
	spec, _ := r.Object["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	ag, _ := comps["aigateway"].(map[string]any)
	sub, _ := ag["modelsAsAService"].(map[string]any)
	d.maas, _ = sub["managementState"].(string)
	d.ready, d.readyMsg = DSCConditionStatus(r.Object, "ModelsAsAServiceReady")
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
func aigatewayMaaSReady(c *kube.Client) (string, string) {
	read, err := rhoai.ListServed(c, "components.platform.opendatahub.io", "/apis/components.platform.opendatahub.io/%s/aigateways", nil, []string{"v1alpha1"})
	if err != nil || read.Body == nil {
		return "", ""
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []kube.Condition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(read.Body, &list) != nil || len(list.Items) != 1 {
		return "", ""
	}
	if cond, ok := kube.FindCondition(list.Items[0].Status.Conditions, "ModelsAsAServiceReady"); ok {
		return cond.Status, kube.FirstNonEmpty(cond.Message, cond.Reason)
	}
	return "", ""
}

// DSCConditionStatus reads a DataScienceCluster condition: "True", "False"
// or "" (not reported), with its message.
func DSCConditionStatus(dsc map[string]any, typ string) (string, string) {
	status, _ := dsc["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, raw := range conds {
		cond, _ := raw.(map[string]any)
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
	Found, Ready bool
	Namespace    string
	Message      string
	Meta         kube.ObjectMeta
}

func ReadKuadrant(c *kube.Client) (kuadrantState, error) {
	// The group's versions are not the resource's: kuadrant.io serves v1
	// (AuthPolicy...) first while Kuadrant is only in v1beta1 (RHCL 1.4,
	// seen live), so list it at each served version until one serves it.
	read, err := rhoai.ListServed(c, KuadrantGroup, "/apis/kuadrant.io/%s/kuadrants", nil, []string{"v1beta1"})
	if err != nil {
		return kuadrantState{}, err
	}
	body := read.Body
	if body == nil {
		return kuadrantState{}, nil
	}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Status   struct {
				Conditions []kube.Condition `json:"conditions"`
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
	st := kuadrantState{Found: true, Namespace: k.Metadata.Namespace, Meta: k.Metadata}
	if cond, ok := kube.FindCondition(k.Status.Conditions, "Ready"); ok {
		st.Ready = cond.Status == "True"
		st.Message = kube.FirstNonEmpty(cond.Message, cond.Reason)
	}
	return st, nil
}

// rhclBelowMinimum: version is older than the docs' minimum for RHOAI 3.6
// (false when unknown).
func rhclBelowMinimum(version string) bool {
	return version != "" && kube.CompareVersions(envkit.VersionCore(version), rhclDocsMinimum) < 0
}

// RHCLWarning: the docs ask for RHCL 1.5 or later (prerequisites.md
// "RHOAI 3.6 — use RHCL v1.5"; 1.4.x can hang callers,
// Kuadrant/wasm-shim#425), and 1.4.0 bypasses gateway authentication
// (rhclAuthBypassVersion). Upgrade Connectivity Link (upgradeRHCL) finds
// OLM's path in the catalog and takes it.
func RHCLWarning(version string) string {
	switch {
	case !rhclBelowMinimum(version):
		return ""
	case envkit.VersionCore(version) == rhclAuthBypassVersion:
		return fmt.Sprintf("Red Hat Connectivity Link %s silently lets requests past the gateway's authentication (the MaaS docs' prerequisites: \"RHCL v1.4.0 contains a Wasm shim bug that silently bypasses gateway authentication\"). This tool publishes nothing until it is upgraded: Upgrade Connectivity Link does it.", version)
	}
	return fmt.Sprintf("Red Hat Connectivity Link %s is installed; the MaaS docs ask for %s or later: on 1.4.x a caller with two or more subscriptions can hang. MaaS works for a single subscription meanwhile. Upgrade Connectivity Link takes the upgrade path OLM offers, one channel at a time, and sets up Authorino TLS again afterwards.", version, rhclDocsMinimum)
}

// maasObjectPath is the path of a platform object this tool reads or
// writes, by kind; only these names.
func maasObjectPath(kind, ns string) string {
	switch kind {
	case "Gateway":
		return rhoai.NamespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "maas-default-gateway")
	case "ConfigMap":
		return rhoai.NamespacedPath("v1", "configmaps", "openshift-ingress", "maas-gateway-options")
	case "Cluster":
		return rhoai.NamespacedPath("postgresql.cnpg.io/v1", "clusters", ns, "maas-db")
	case "Secret":
		return rhoai.NamespacedPath("v1", "secrets", ns, "maas-db-config")
	case "PostgresDeployment":
		return rhoai.NamespacedPath("apps/v1", "deployments", ns, "postgres")
	case "PostgresService":
		return rhoai.NamespacedPath("v1", "services", ns, "postgres")
	}
	return ""
}

func maasCreatePath(kind, ns string) string {
	switch kind {
	case "Gateway":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", ""))
	case "ConfigMap":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "configmaps", "openshift-ingress", ""))
	case "Cluster":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("postgresql.cnpg.io/v1", "clusters", ns, ""))
	case "Secret", "PostgresSecret":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", ns, ""))
	case "PostgresDeployment":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("apps/v1", "deployments", ns, ""))
	case "PostgresService":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "services", ns, ""))
	}
	return ""
}

func maasDesc(kind, ns string) string {
	switch kind {
	case "GatewayClass":
		return "GatewayClass " + maasGatewayClass
	case "Gateway":
		return "Gateway " + inferenceGatewayNamespace + "/" + MaaSGatewayName
	case "ConfigMap":
		return "ConfigMap " + inferenceGatewayNamespace + "/" + maasGatewayOptions
	case "Cluster":
		return "CloudNativePG Cluster " + ns + "/" + maasDBCluster
	case "Secret":
		return "Secret " + ns + "/" + maasDBConfigSecret
	case "PostgresSecret":
		return "Secret " + ns + "/" + maasPOCCreds
	case "PostgresDeployment":
		return "Deployment " + ns + "/" + maasPOCName
	case "PostgresService":
		return "Service " + ns + "/" + maasPOCName
	}
	return kind
}

// maasGatewayClassPath is GatewayClass openshift-default: OpenShift's
// shared class for gateways. This tool only reads it, and creates it when
// the cluster has none (setup-gateway.sh setup_gatewayclass); it never
// changes or deletes it, also when it created it: other gateways may use
// it, and a class alone costs nothing.
func maasGatewayClassPath() string {
	return rhoai.ClusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "openshift-default")
}

// maasGatewayState is the MaaS gateway as read.
type maasGatewayState struct {
	gw          GatewayObject
	found       bool
	owned       bool
	programmed  bool
	address     string
	acceptedErr string
}

func readMaaSGateway(c *kube.Client) (maasGatewayState, error) {
	body, exists, err := c.GetOptional(maasObjectPath("Gateway", ""))
	if err != nil {
		return maasGatewayState{}, err
	}
	if !exists {
		return maasGatewayState{}, nil
	}
	st := maasGatewayState{found: true}
	if err := json.Unmarshal(body, &st.gw); err != nil {
		return st, fmt.Errorf("parse Gateway %s/%s: %w", inferenceGatewayNamespace, MaaSGatewayName, err)
	}
	st.owned = gatewayOwned(st.gw.Metadata)
	if cond, ok := kube.FindCondition(st.gw.Status.Conditions, "Accepted"); ok && cond.Status == "False" {
		st.acceptedErr = kube.FirstNonEmpty(cond.Message, cond.Reason)
	}
	if cond, ok := kube.FindCondition(st.gw.Status.Conditions, "Programmed"); ok && cond.Status == "True" {
		st.programmed = true
	}
	for _, a := range st.gw.Status.Addresses {
		st.address = kube.FirstNonEmpty(st.address, a.Value)
	}
	return st, nil
}

// MaaSNamespaceRefusal checks the namespaces maas-default-gateway always
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
func MaaSNamespaceRefusal(c *kube.Client, infra string) (string, string) {
	for _, ns := range []string{MaaSTenantNamespace, infra} {
		meta, found, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+ns)
		if err != nil {
			return fmt.Sprintf("Cannot read namespace %s, so this tool cannot tell who made it; nothing was changed: %v.%s", ns, err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
		}
		if !found {
			continue
		}
		if who := meta.Annotations[storage.ProjectRequesterAnnotation]; who != "" {
			return fmt.Sprintf("Namespace %s was made through a project request by %s (annotation %s), not by RHOAI, and MaaS takes over a namespace of that name that exists. Its admins could attach routes to the MaaS gateway and change MaaS's subscriptions and authorization policies there, so this tool does not set up or publish MaaS while it exists. A cluster-admin checks it and, if it is not RHOAI's, deletes it; RHOAI makes it again. Nothing was changed.", ns, who, storage.ProjectRequesterAnnotation), "not_managed"
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
	out := []string{infraNS, MaaSTenantNamespace}
	for _, p := range published {
		if !slices.Contains(out, p) {
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
func maasGatewayObjects(domain, certificate string, namespaces []string) map[string]map[string]any {
	values := make([]any, len(namespaces))
	for i, n := range namespaces {
		values[i] = n
	}
	labels := envkit.ToolLabels()
	labels["app.kubernetes.io/name"] = "maas"
	labels["app.kubernetes.io/instance"] = MaaSGatewayName
	labels["app.kubernetes.io/component"] = "gateway"
	labels["opendatahub.io/managed"] = "false"
	allowed := map[string]any{"namespaces": map[string]any{
		"from": "Selector",
		"selector": map[string]any{"matchExpressions": []any{map[string]any{
			"key": "kubernetes.io/metadata.name", "operator": "In", "values": values,
		}}},
	}}
	listener := func(name, host, cert string) map[string]any {
		return map[string]any{
			"name": name, "hostname": host, "port": 443, "protocol": "HTTPS", "allowedRoutes": allowed,
			"tls": map[string]any{
				"mode":            "Terminate",
				"certificateRefs": []any{map[string]any{"group": "", "kind": "Secret", "name": cert}},
			},
		}
	}
	return map[string]map[string]any{
		"ConfigMap": {
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": maasGatewayOptions, "namespace": inferenceGatewayNamespace, "labels": envkit.ToolLabels()},
			"data": map[string]any{
				"service": "metadata:\n  annotations:\n    service.beta.openshift.io/serving-cert-secret-name: " + maasGatewayServiceTLS + "\n",
			},
		},
		"GatewayClass": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "GatewayClass",
			"metadata":   map[string]any{"name": "openshift-default", "labels": envkit.ToolLabels()},
			"spec":       map[string]any{"controllerName": gatewayControllerName},
		},
		"Gateway": {
			"apiVersion": gatewayAPIVersion,
			"kind":       "Gateway",
			"metadata": map[string]any{
				"name":      "maas-default-gateway",
				"namespace": inferenceGatewayNamespace,
				"labels":    labels,
				"annotations": map[string]any{
					"opendatahub.io/managed":                          "false",
					"security.opendatahub.io/authorino-tls-bootstrap": "true",
				},
			},
			"spec": map[string]any{
				"gatewayClassName": maasGatewayClass,
				"infrastructure": map[string]any{
					"parametersRef": map[string]any{"group": "", "kind": "ConfigMap", "name": maasGatewayOptions},
				},
				"listeners": []any{
					listener("https", maasHostPrefix+domain, certificate),
					listener("https-internal", MaaSGatewayName+"-"+maasGatewayClass+"."+inferenceGatewayNamespace+".svc.cluster.local", maasGatewayServiceTLS),
				},
			},
		},
	}
}

// maasDBClusterObject is the CloudNativePG cluster: one instance, the
// smallest volume, database and owner "maas" as scripts/setup-database.sh
// names them. A development and test database.
func maasDBClusterObject(ns string) map[string]any {
	return map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "maas-db", "namespace": ns, "labels": envkit.ToolLabels()},
		"spec": map[string]any{
			"instances": 1,
			"storage":   map[string]any{"size": "1Gi"},
			"bootstrap": map[string]any{"initdb": map[string]any{"database": maasDBName, "owner": maasDBName}},
		},
	}
}

// maasDBConfigObject is Secret maas-db-config as setup-database.sh makes it
// (create_maas_db_config_secret: key DB_CONNECTION_URL, label app=maas-api).
func maasDBConfigObject(ns, connURL string) map[string]any {
	labels := envkit.ToolLabels()
	labels["app"] = "maas-api"
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "maas-db-config", "namespace": ns, "labels": labels},
		"type":       "Opaque",
		"data":       map[string]any{"DB_CONNECTION_URL": base64.StdEncoding.EncodeToString([]byte(connURL))},
	}
}

// maasConnectionURL builds DB_CONNECTION_URL from the CloudNativePG
// application Secret (keys username and password, CNPG docs "Application
// secret"), for the cluster's read-write Service, with sslmode=require
// (maas-setup.md "Connection string format"). The URL holds the password:
// it is only ever written to the Secret, never logged or returned.
func maasConnectionURL(c *kube.Client, ns string) (string, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := c.GetInto(rhoai.NamespacedPath("v1", "secrets", ns, "maas-db-app"), &s); err != nil {
		if kube.IsDecodeError(err) {
			return "", errors.New("the database's application Secret cannot be parsed")
		}
		return "", fmt.Errorf("read the database's application Secret %s/%s: %w", ns, maasDBAppSecret, err)
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
func readMaaSDBCluster(c *kube.Client, ns string) (kube.ObjectMeta, bool, bool, string, error) {
	var cl struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Instances int `json:"instances"`
		} `json:"spec"`
		Status struct {
			Phase          string `json:"phase"`
			ReadyInstances int    `json:"readyInstances"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(maasObjectPath("Cluster", ns), &cl)
	if err != nil {
		if kube.IsDecodeError(err) {
			return kube.ObjectMeta{}, true, false, "", err
		}
		return kube.ObjectMeta{}, false, false, "", err
	}
	if !found {
		return kube.ObjectMeta{}, false, false, "", nil
	}
	ready := cl.Status.ReadyInstances >= 1 && cl.Status.ReadyInstances >= cl.Spec.Instances
	return cl.Metadata, true, ready, kube.FirstNonEmpty(cl.Status.Phase, "Starting"), nil
}

// deploymentAvailable reads a Deployment's Available condition: "True",
// "False", "missing" or "" (unreadable).
func deploymentAvailable(c *kube.Client, ns, name string) string {
	var d struct {
		Status struct {
			Conditions []kube.Condition `json:"conditions"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name), &d)
	if err != nil {
		return ""
	}
	if !found {
		return "missing"
	}
	if cond, ok := kube.FindCondition(d.Status.Conditions, "Available"); ok {
		return cond.Status
	}
	return ""
}

// readyCondition reads an object's Ready condition: "True", "False",
// "missing" or "" (unreadable or not reported), with its message.
func readyCondition(c *kube.Client, path string) (string, string) {
	var obj struct {
		Status struct {
			Conditions []kube.Condition `json:"conditions"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(path, &obj)
	if err != nil {
		return "", ""
	}
	if !found {
		return "missing", ""
	}
	if cond, ok := kube.FindCondition(obj.Status.Conditions, "Ready"); ok {
		return cond.Status, kube.FirstNonEmpty(cond.Message, cond.Reason)
	}
	return "", ""
}

// MaaSGatewayAuth reads the AuthPolicy maas-controller puts on the MaaS
// gateway (maas-gateway-auth on RHOAI 3.6; the docs name others, so it is
// found by its target and maas-controller's label, never by taking the
// first policy on the gateway: a tier-era gateway-auth-policy may target
// it too): "enforced", "not-enforced", "missing" or "" (unreadable).
func MaaSGatewayAuth(c *kube.Client) string {
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

// GroupServed says whether the API server serves group: its discovery
// document answers 404 when no CRD of the group is installed. Any other
// failure is returned, so callers can refuse rather than guess.
func GroupServed(c *kube.Client, group string) (bool, error) {
	if v, ok := rhoai.APIVersionsCache.Get(c.BaseURL() + "|" + group); ok && len(v) > 0 {
		return true, nil
	}
	body, found, err := c.GetOptional("/apis/" + group)
	if err != nil {
		return false, fmt.Errorf("read the API group %s: %w", group, err)
	}
	if !found {
		return false, nil
	}
	return len(rhoai.ParseGroupVersions(body)) > 0, nil
}

// AuthorinoTLS checks what scripts/setup-authorino-tls.sh sets on
// Kuadrant's Authorino in ns: the Service's serving-cert annotation, the
// Authorino CR's TLS listener, and the Deployment's trust of the service
// CA (AuthorinoTrustsServiceCA). It
// returns "" when all are set (or cannot be read: unknown is not a
// failure), else what is missing.
//
// The certificate Secret is the one the Authorino CR's listener names
// (spec.listener.tls.certSecretRef.name; RHOAI's AI gateway may use
// another than the script's authorino-server-cert), returned for the
// commands; the Service must ask service-ca for that one.
func AuthorinoTLS(c *kube.Client, ns string) (string, bool, string) {
	var missing []string
	known := true
	secret := "authorino-server-cert"
	tlsOK := false
	// The CR at the version its group serves (v1beta1 when discovery cannot
	// tell).
	read, err := rhoai.GetServed(c, authorinoGroup, "/apis/"+authorinoGroup+"/%s/namespaces/"+ns+"/authorinos/authorino", []string{"v1beta1"})
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
			secret = kube.FirstNonEmpty(a.Spec.Listener.TLS.CertSecretRef.Name, secret)
			tlsOK = a.Spec.Listener.TLS.Enabled
		}
	}
	// Which Secret the Service must name is the CR's: without the CR it is
	// unknown, not missing.
	if known {
		meta, found, err := kube.ReadObjectMeta(c, fmt.Sprintf("/api/v1/namespaces/%s/services/%s", ns, "authorino-authorino-authorization"))
		switch {
		case err != nil:
			known = false
		case !found || meta.Annotations[CertSecretAnnotation] != secret:
			missing = append(missing, "the Service's serving certificate")
		}
	}
	if known && !tlsOK {
		missing = append(missing, "TLS on Authorino's listener")
	}
	if body, _, err := c.Get(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, "authorino")); err != nil {
		known = false
	} else if !AuthorinoTrustsServiceCA(body) {
		missing = append(missing, "trust of the service CA (SSL_CERT_FILE)")
	}
	return strings.Join(missing, ", "), known, secret
}

// AuthorinoTrustsServiceCA says whether Authorino's Deployment trusts an
// added CA bundle, either way it is done: the script's SSL_CERT_FILE or
// REQUESTS_CA_BUNDLE (it points them at service-ca.crt), or an SSL_CERT_DIR entry
// that is the mount of a ConfigMap volume, as RHOAI's AI gateway sets it
// up through the Authorino CR's volumes (live on 3.6: ConfigMap
// aigrid-service-ca, inject-cabundle, at /etc/ssl/certs/openshift-service-ca).
func AuthorinoTrustsServiceCA(deployment []byte) bool {
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
				for dir := range strings.SplitSeq(e.Value, ":") {
					if mounts[strings.TrimRight(dir, "/")] {
						return true
					}
				}
			}
		}
	}
	return false
}

// AuthorinoTLSCommands are scripts/setup-authorino-tls.sh's commands for ns,
// and deploy.sh's restart of Authorino after them. The Secret's name comes
// from Authorino's CR (whoever may edit it chooses it) and a cluster-admin
// pastes these commands into a shell, so a name that is not a Kubernetes
// object name (DNS-1123 subdomain) gets no commands: why says so. Every
// substitution is shell-quoted as well.
func AuthorinoTLSCommands(ns, secret string) (cmds, why string) {
	if len(ns) > 63 || !components.DNS1123Label.MatchString(ns) {
		return "", fmt.Sprintf("Connectivity Link's namespace %s is not a valid Kubernetes name", strconv.Quote(kube.TruncateText(ns, 300)))
	}
	if len(secret) > 253 || !components.DNS1123Subdomain.MatchString(secret) {
		return "", fmt.Sprintf("Authorino's CR names its certificate Secret %s (spec.listener.tls.certSecretRef.name), which is not a valid Kubernetes name, so no commands are shown. A cluster-admin checks Authorino authorino in %s", strconv.Quote(kube.TruncateText(secret, 300)), ns)
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"listener": map[string]any{"tls": map[string]any{
		"enabled": true, "certSecretRef": map[string]any{"name": secret}}}}})
	q := kube.ShellQuote(ns)
	return fmt.Sprintf(`oc annotate service authorino-authorino-authorization -n %[1]s \
  service.beta.openshift.io/serving-cert-secret-name=%[2]s --overwrite
oc patch authorino authorino -n %[1]s --type=merge \
  -p %[3]s
oc -n %[1]s set env deployment/authorino \
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt \
  REQUESTS_CA_BUNDLE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
oc rollout restart deployment/authorino -n %[1]s`, q, kube.ShellQuote(secret), kube.ShellQuote(string(patch))), ""
}

// --- status --------------------------------------------------------------------

// GetMaaSStatus reports the MaaS platform part by part, in setup order
// (the docs' platform setup, then their MaaS setup), what Set up would
// change now, and this tool's models with their publication.
func GetMaaSStatus(c *kube.Client) (*types.MaaSStatus, error) {
	st := &types.MaaSStatus{Subscription: MaaSSubscriptionName, DashboardURL: dashboardURLOrEmpty(c)}
	rec, err := ReadMaaSRecord(c)
	if err != nil {
		return nil, err
	}
	dsc, dscErr := readMaaSDSC(c)
	r := &maasStatusRun{c: c, st: st, rec: rec}
	r.gone, r.modelsErr = goneMaaSPublications(c, rec)
	r.infra, r.infraErr = MaaSInfraNamespace(c)
	if r.infraErr != nil {
		r.blockers = append(r.blockers, kube.SentenceStart(r.infraErr.Error())+".")
	}
	domain, _ := clusterAppsDomain(c)
	if u := "https://" + maasHostPrefix + domain; domain != "" && kube.SnippetURL(u) {
		st.URL = u
	}
	rhcl, cnpg, cnpgReadErr, kVersion, kvErr := r.statusRHCL()
	kq := r.statusKuadrant(rhcl)
	r.statusDatabase(cnpg, cnpgReadErr)
	r.statusGateway()
	r.statusAuthorinoTLS(kq)
	r.statusTierPolicies(kVersion, kvErr)
	r.statusDSC(dsc, dscErr)
	if r.denyNote != "" && len(st.SetupPlan) > 0 {
		r.plan(r.denyNote)
	}
	r.statusPlatform(dsc)
	r.statusModels()
	return st, nil
}

// maasStatusRun builds GetMaaSStatus's answer: each method reads one part
// of the platform and appends it, in setup order.
type maasStatusRun struct {
	c   *kube.Client
	st  *types.MaaSStatus
	rec MaaSRecord
	// infra is MaaSInfraNamespace; "" with infraErr when it is not known.
	infra    string
	infraErr error
	// blockers are the reasons Set up cannot run, the first one shown.
	blockers []string
	// denyNote is the Connectivity Link part's own offer, added to the
	// plan when Set up changes anything.
	denyNote string
	// gone are the recorded publications whose project or model is gone
	// (goneMaaSPublications); modelsErr says the models record cannot be
	// read.
	gone      []maasGonePub
	modelsErr error
}

// goneWhy says why the publication of name in project is gone, if it is.
func (r *maasStatusRun) goneWhy(project, name string) (string, bool) {
	for _, g := range r.gone {
		if g.pub.Project == project && g.pub.Model == name {
			return g.why, true
		}
	}
	return "", false
}

func (r *maasStatusRun) part(p types.MaaSPart) { r.st.Parts = append(r.st.Parts, p) }
func (r *maasStatusRun) plan(s string)         { r.st.SetupPlan = append(r.st.SetupPlan, s) }

// statusRHCL is Red Hat Connectivity Link's part. It returns the operators
// and Kuadrant API the later parts use.
func (r *maasStatusRun) statusRHCL() (rhcl, cnpg envkit.OperatorState, cnpgReadErr error, kVersion string, kvErr error) {
	c, rec, part, plan := r.c, r.rec, r.part, r.plan
	// Red Hat Connectivity Link (platform-setup.md "Install Gateway API
	// Controller", RHOAI tab).
	csvs, csvErr := components.ListClusterCSVs(c)
	var subs []*envkit.OLMSubscription
	if csvErr == nil {
		subs, csvErr = envkit.ListSubscriptions(c, "")
	}
	rhclErr, cnpgReadErr := csvErr, csvErr
	if csvErr == nil {
		rhcl, rhclErr = envkit.ReadOperatorIn(c, RHCLOperator, csvs, subs)
		cnpg, cnpgReadErr = envkit.ReadOperatorIn(c, cnpgOperator, csvs, subs)
	}
	kVersion, kvErr = KuadrantServedVersion(c)
	// Kuadrant's API alone is not an installed operator: its CRDs stay
	// after an uninstall (envkit.ControllerDeployed).
	kRuns, kRunsErr := kVersion != "", error(nil)
	if kvErr == nil && kRuns && !rhcl.Installed() && rhclErr == nil {
		kRuns, kRunsErr = envkit.ControllerDeployed(c, KuadrantController)
	}
	rhclPart := types.MaaSPart{ID: "rhcl", Title: "Red Hat Connectivity Link", DocURL: maasDocsBase + "install/platform-setup.md#install-gateway-api-controller"}
	_, rhclByTool := rec.Shared["rhcl"]
	switch {
	case rhclErr != nil:
		rhclPart.State, rhclPart.Message = "unknown", "Cannot read the installed operators: "+rhclErr.Error()
	case rhcl.Installed():
		rhclPart.ManagedByTool = rhclByTool && rhcl.Sub != nil && ToolOwned(rhcl.Sub.Metadata)
		rhclPart.NotManaged = !rhclPart.ManagedByTool
		where := "its Subscription is not one of the names the docs use (kuadrant-operator, rhcl-operator)"
		if rhcl.Sub != nil {
			where = fmt.Sprintf("Subscription %s/%s, channel %s", rhcl.Sub.Metadata.Namespace, rhcl.Sub.Metadata.Name, kube.FirstNonEmpty(rhcl.Sub.Spec.Channel, "not set"))
		}
		switch {
		case envkit.VersionCore(rhcl.Version) == rhclAuthBypassVersion:
			rhclPart.State, rhclPart.Message = "problem", fmt.Sprintf("Version %s (%s). ", rhcl.Version, where)+RHCLWarning(rhcl.Version)
		case rhcl.Phase != "Succeeded":
			rhclPart.State, rhclPart.Message = "waiting", fmt.Sprintf("Version %s is being installed (%s; CSV %s)", rhcl.Version, where, kube.FirstNonEmpty(rhcl.Phase, "pending"))
		default:
			rhclPart.State, rhclPart.Message = "ready", fmt.Sprintf("Version %s runs (%s)", rhcl.Version, where)
			rhclPart.Warning = RHCLWarning(rhcl.Version)
		}
		if rhclBelowMinimum(rhcl.Version) {
			action, cmds := rhclUpgradeOffer(c, rhcl)
			if action != nil {
				rhclPart.Actions = append(rhclPart.Actions, *action)
			}
			rhclPart.Commands = cmds
		}
		if action, note, cmd := rhclDenyOffer(rhcl); note != "" {
			if action != nil {
				rhclPart.Actions = append(rhclPart.Actions, *action)
			}
			rhclPart.Warning = strings.TrimSpace(rhclPart.Warning + " " + note)
			rhclPart.Commands = strings.TrimSpace(rhclPart.Commands + "\n" + cmd)
			r.denyNote = fmt.Sprintf("Not changed: Subscription %s/%s, which this tool did not create, does not deny traffic when rate limiting fails (the MaaS docs recommend it); the Connectivity Link part offers that change on its own", rhcl.Sub.Metadata.Namespace, rhcl.Sub.Metadata.Name)
		}
		if action, note := rhclMemoryOffer(c, rhcl); action != nil {
			rhclPart.Actions = append(rhclPart.Actions, *action)
			rhclPart.Warning = strings.TrimSpace(rhclPart.Warning + " " + note)
		} else if note != "" {
			rhclPart.Warning = strings.TrimSpace(rhclPart.Warning + " " + note)
		}
	case kRunsErr != nil:
		rhclPart.State, rhclPart.Message = "unknown", "Cannot tell whether Kuadrant's operator runs: "+kRunsErr.Error()
	case kvErr == nil && kRuns:
		rhclPart.State, rhclPart.NotManaged = "ready", true
		rhclPart.Message = "Kuadrant's API is served without an rhcl-operator CSV (an upstream Kuadrant, or another install); MaaS uses it as it is"
	case rhcl.Pending() != nil:
		p := rhcl.Pending()
		rhclPart.ManagedByTool, rhclPart.NotManaged = rhclByTool, !rhclByTool
		rhclPart.State, rhclPart.Message = "waiting", fmt.Sprintf("Subscription %s/%s exists; OLM is installing it (%s)", p.Metadata.Namespace, p.Metadata.Name, kube.FirstNonEmpty(p.Status.State, "no state yet"))
	default:
		rhclPart.State = "missing"
		if p, msg, _ := envkit.PlanOperatorInstall(c, RHCLOperator); msg != "" {
			rhclPart.State, rhclPart.Message = "problem", "Not installed, and Set up cannot install it: "+msg
			r.blockers = append(r.blockers, rhclPart.Message)
		} else {
			rhclPart.Message = fmt.Sprintf("Not installed. Set up installs version %s from redhat-operators (channel %s), as the MaaS docs' platform setup does.", p.Head, p.Channel)
			rhclPart.Commands = RHCLInstallCommands(p.Channel)
			plan(fmt.Sprintf("Install Red Hat Connectivity Link %s: %sSubscription %s/%s (package %s, channel %s, from redhat-operators, with spec.config.env RATELIMIT_CHECK_SERVICE_FAILURE_MODE=deny and RATELIMIT_REPORT_SERVICE_FAILURE_MODE=deny, as the MaaS docs recommend, so requests are refused rather than let through unlimited while Limitador is unavailable)", p.Head, OperatorPlanPrefix(p), RHCLOperator.Namespace, RHCLOperator.SubName, RHCLOperator.Pkg, p.Channel))
		}
	}
	part(rhclPart)
	return rhcl, cnpg, cnpgReadErr, kVersion, kvErr
}

// statusKuadrant is the Kuadrant CR's part.
func (r *maasStatusRun) statusKuadrant(rhcl envkit.OperatorState) kuadrantState {
	c, rec, part, plan := r.c, r.rec, r.part, r.plan
	// The Kuadrant CR (platform-setup.md: "Once the RHCL operator is
	// ready, create a Kuadrant instance").
	kq, kErr := ReadKuadrant(c)
	kuadrantNS := kube.FirstNonEmpty(kq.Namespace, rhcl.CSVNS, RHCLOperator.Namespace)
	kPart := types.MaaSPart{ID: "kuadrant", Title: "Connectivity Link instance (Kuadrant)", DocURL: maasDocsBase + "install/platform-setup.md#install-gateway-api-controller"}
	_, kByTool := rec.Shared["kuadrant"]
	switch {
	case kErr != nil:
		kPart.State, kPart.Message = "unknown", "Cannot read the Kuadrant CR: "+kErr.Error()
	case !kq.Found:
		kPart.State, kPart.Message = "missing", fmt.Sprintf("No Kuadrant CR. Set up creates %s/kuadrant once Connectivity Link runs, as the docs' platform setup does.", kuadrantNS)
		kPart.Commands = KuadrantCommands(kuadrantNS)
		plan(fmt.Sprintf("Create the Kuadrant CR %s/kuadrant", kuadrantNS))
	case kq.Ready:
		kPart.State, kPart.Message = "ready", kq.Namespace+"/kuadrant is ready"
	default:
		kPart.State, kPart.Message = "waiting", "Kuadrant is not Ready: "+kube.FirstNonEmpty(kq.Message, "no message")
	}
	if kq.Found {
		kPart.ManagedByTool = kByTool && ToolOwned(kq.Meta)
		kPart.NotManaged = !kPart.ManagedByTool
	}
	part(kPart)
	return kq
}

// statusDatabase is the database's parts (maas_database.go).
func (r *maasStatusRun) statusDatabase(cnpg envkit.OperatorState, cnpgReadErr error) {
	c, rec, infra, st, part := r.c, r.rec, r.infra, r.st, r.part
	// The database, as chosen (maas_database.go), and CloudNativePG while
	// it is the database.
	if rec.DBNamespace == "" && r.infraErr != nil {
		part(types.MaaSPart{ID: "database", Title: "Database", DocURL: maasDocsBase + "install/maas-setup.md#database-setup",
			State: "unknown", Message: kube.SentenceStart(r.infraErr.Error())})
		return
	}
	dbs := maasDatabaseStatus(c, rec, infra, cnpg, cnpgReadErr)
	for _, p := range dbs.parts {
		part(p)
	}
	st.SetupPlan = append(st.SetupPlan, dbs.plan...)
	r.blockers = append(r.blockers, dbs.blockers...)
	st.Database = dbs.db
}

// statusGateway is the MaaS gateway's part.
func (r *maasStatusRun) statusGateway() {
	c, rec, infra, st, part, plan := r.c, r.rec, r.infra, r.st, r.part, r.plan
	// The gateway.
	gwPart := types.MaaSPart{ID: "gateway", Title: "MaaS gateway", DocURL: maasDocsBase + "install/maas-setup.md#create-gateway"}
	gw, gwErr := readMaaSGateway(c)
	switch {
	case gwErr != nil:
		gwPart.State, gwPart.Message = "unknown", "Cannot read the gateway: "+gwErr.Error()
	case !gw.found:
		gwPart.State, gwPart.Message = "missing", "Set up creates it with an external load balancer at "+kube.FirstNonEmpty(st.URL, "maas.<apps domain>")
		plan(fmt.Sprintf("Create Gateway %s/%s with an external load balancer at %s (a cloud cost while it exists), and GatewayClass %s if the cluster has none", inferenceGatewayNamespace, MaaSGatewayName, kube.FirstNonEmpty(strings.TrimPrefix(st.URL, "https://"), "maas.<apps domain>"), maasGatewayClass))
	default:
		gwPart.ManagedByTool, gwPart.NotManaged = gw.owned, !gw.owned
		switch {
		case gw.gw.admitsAll() && gw.owned:
			gwPart.State, gwPart.Message = "problem", "A listener admits routes from every namespace, which this tool never sets: any project could route through MaaS's gateway. Set up writes back the documented list of namespaces."
			plan(fmt.Sprintf("Gateway %s/%s: admit routes only from the namespaces it lists again (the MaaS docs' setup-gateway.sh)", inferenceGatewayNamespace, MaaSGatewayName))
		case gw.gw.admitsAll():
			gwPart.State = "problem"
			gwPart.Message = "This gateway, which this tool did not create, admits routes from every namespace, so any project editor could put a route on MaaS's gateway. The MaaS docs' gateway (setup-gateway.sh) lists the namespaces instead. This tool publishes nothing through it until its owner applies the command below; it does not change a gateway it did not create."
			if r.infraErr == nil {
				projects, err := maasLiveProjects(c, rec, MaaSPub{}, MaaSPub{})
				if err != nil {
					projects = rec.Projects()
				}
				gwPart.Commands = gatewayAllowedRoutesCommand(gw.gw, maasGatewayNamespaces(infra, projects))
			}
		case gw.acceptedErr != "":
			gwPart.State, gwPart.Message = "problem", "Not accepted: "+kube.TruncateText(gw.acceptedErr, 300)
		case gw.programmed && gw.address != "":
			gwPart.State, gwPart.Message = "ready", fmt.Sprintf("%s in %s, programmed at %s", MaaSGatewayName, inferenceGatewayNamespace, gw.address)
		default:
			gwPart.State, gwPart.Message = "waiting", "Waiting for its load balancer"
		}
		if !gw.owned {
			if h := gw.gw.externalHost(); h != "" && kube.SnippetURL("https://"+h) {
				st.URL = "https://" + h
			}
		}
		gone, modelsErr := r.gone, r.modelsErr
		var warnings []string
		if len(gone) > 0 {
			w := "It still admits routes from published projects that are gone (" + goneMaaSText(gone) + "). Anyone who creates a project of such a name could attach routes to it. The background check (every few minutes) or the next MaaS change, Models change or Clean up removes them."
			if !gw.owned {
				w = "This tool's record names published projects that are gone (" + goneMaaSText(gone) + "). If this gateway, which this tool did not create, lists them, its owner removes them from its allowedRoutes: anyone who creates a project of such a name could attach routes to it."
			}
			warnings = append(warnings, w)
		}
		if modelsErr != nil && len(rec.Pubs()) > 0 {
			warnings = append(warnings, "This tool's record of its models cannot be read ("+modelsErr.Error()+"), so whether a published model was deleted and made again under its name cannot be checked: such a publication stays on the gateway until the record can be read.")
		}
		gwPart.Warning = strings.Join(warnings, " ")
	}
	part(gwPart)
}

// statusAuthorinoTLS is Authorino TLS's part, on Kuadrant's Authorino.
func (r *maasStatusRun) statusAuthorinoTLS(kq kuadrantState) {
	c, rec, part, plan := r.c, r.rec, r.part, r.plan
	// Authorino TLS (maas-setup.md "Authorino TLS",
	// scripts/setup-authorino-tls.sh), on Kuadrant's Authorino.
	tlsPart := types.MaaSPart{ID: "authorino-tls", Title: "Authorino TLS", DocURL: maasDocsBase + "configuration-and-management/tls-configuration.md"}
	_, tlsByTool := rec.Shared["authorino-tls"]
	tlsPlan := "Configure Authorino TLS as the docs' setup-authorino-tls.sh does: the serving-certificate annotation on Service authorino-authorino-authorization, TLS on the Authorino CR's listener, SSL_CERT_FILE on Deployment authorino, then restart it"
	if kq.Found {
		missing, known, secret := AuthorinoTLS(c, kq.Namespace)
		switch {
		case missing != "":
			cmds, why := AuthorinoTLSCommands(kq.Namespace, secret)
			if why != "" {
				tlsPart.State, tlsPart.Message = "problem", "Not configured: "+missing+". "+why+". Without it, API keys are refused."
				r.blockers = append(r.blockers, tlsPart.Message)
				break
			}
			tlsPart.State = "missing"
			tlsPart.Message = "Not configured: " + missing + ". Without it, API keys are refused. Set up (or Configure Authorino TLS) applies the docs' setup-authorino-tls.sh steps in " + kq.Namespace + "."
			tlsPart.Commands = cmds
			tlsPart.Actions = []types.MaaSAction{authorinoTLSAction(kq.Namespace, secret, missing)}
			plan(tlsPlan + " (in " + kq.Namespace + ")")
		case known:
			tlsPart.State, tlsPart.Message = "ready", "Authorino in "+kq.Namespace+" serves TLS and trusts the service CA"
			tlsPart.ManagedByTool, tlsPart.NotManaged = tlsByTool, !tlsByTool
		default:
			tlsPart.State, tlsPart.Message = "unknown", "Cannot read Authorino's configuration"
		}
	} else {
		tlsPart.State, tlsPart.Message = "missing", "Set up configures it once Kuadrant has made Authorino, as the docs' setup-authorino-tls.sh does."
		plan(tlsPlan)
	}
	part(tlsPart)
}

// statusTierPolicies is the part of the old gateway policies, shown only
// when there are any (or they cannot be listed).
func (r *maasStatusRun) statusTierPolicies(kVersion string, kvErr error) {
	c, part := r.c, r.part
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
				strings.Join(policyNames(foreign), ", "), kube.Verb(len(foreign), "is", "are"), MaaSGatewayName, kube.Verb(len(foreign), "is", "are"))
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
				polPart.Message += fmt.Sprintf(" %s %s not among the names the docs give for the old policies, so this tool leaves %s to a cluster-admin.", strings.Join(policyNames(other), ", "), kube.Verb(len(other), "is", "are"), kube.Verb(len(other), "it", "them"))
			}
			polPart.Commands = legacyPolicyCommands(foreign)
			part(polPart)
		}
	}
}

// statusDSC is the parts of MaaS in the DataScienceCluster, maas-api and
// the default tenant.
func (r *maasStatusRun) statusDSC(dsc maasDSC, dscErr error) {
	c, rec, infra, st, part, plan := r.c, r.rec, r.infra, r.st, r.part, r.plan
	// The DataScienceCluster: the AI gateway and MaaS in it, last.
	dscPart := types.MaaSPart{ID: "dsc", Title: "MaaS in the DataScienceCluster", DocURL: maasDocsBase + "install/maas-setup.md#configure-datasciencecluster"}
	switch {
	case dscErr != nil:
		dscPart.State, dscPart.Message = "unknown", "Cannot read the DataScienceCluster: "+dscErr.Error()
	case dsc.read.Object == nil:
		st.Blocked = &types.ComponentBlock{Component: "aigateway", Reason: "MaaS needs RHOAI's DataScienceCluster, and this cluster has none. A cluster-admin creates one on the Components page."}
		dscPart.State, dscPart.Message = "problem", "No DataScienceCluster"
	case dsc.maas == "Managed" && dsc.aigateway == "Managed":
		// This tool's while it recorded the change and still owns the field
		// (as the prerequisites' parts): someone who set it since owns it.
		owned := rec.DSCChanged && dscMaaSOwned(dsc)
		dscPart.ManagedByTool, dscPart.NotManaged = owned, !owned
		switch dsc.ready {
		case "False":
			dscPart.State, dscPart.Message = "waiting", "modelsAsAService is Managed; not ready: "+kube.TruncateText(dsc.readyMsg, 300)
		default:
			dscPart.State, dscPart.Message = "ready", "aigateway and modelsAsAService are Managed"
		}
	default:
		var fields []string
		if dsc.aigateway != "Managed" {
			fields = append(fields, fmt.Sprintf("aigateway.managementState %s to Managed", kube.FirstNonEmpty(dsc.aigateway, "not set")))
		}
		if dsc.maas != "Managed" {
			fields = append(fields, fmt.Sprintf("aigateway.modelsAsAService.managementState %s to Managed", kube.FirstNonEmpty(dsc.maas, "not set")))
		}
		dscPart.State = "missing"
		dscPart.Message = fmt.Sprintf("aigateway is %s and modelsAsAService is %s. Set up sets them to Managed, last, as the docs' DataScienceCluster step does; Remove puts back what this tool changed.", kube.FirstNonEmpty(dsc.aigateway, "not set"), kube.FirstNonEmpty(dsc.maas, "not set"))
		dscPart.Commands = maasDSCCommand(dsc.name)
		dscPart.ManagedByTool = rec.AIGatewayChanged && dsc.aigateway == "Managed" && dscAIGatewayOwned(dsc)
		if dsc.argoApp != "" {
			dscPart.State = "problem"
			dscPart.Message = argo.GitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service") + " Set " + strings.Join(fields, " and ") + " in Git." + dsc.argo.UnconfirmedNote()
			r.blockers = append(r.blockers, dscPart.Message)
		} else {
			plan(fmt.Sprintf("DataScienceCluster %s: %s (the values before are recorded, and Remove restores them)", dsc.name, strings.Join(fields, ", ")))
		}
	}

	// maas-api and the default tenant, which the operator creates.
	apiPart := types.MaaSPart{ID: "maas-api", Title: "MaaS API (maas-api)", DocURL: maasDocsBase + "install/validation.md"}
	tenantPart := types.MaaSPart{ID: "tenant", Title: "Default tenant and gateway authentication", DocURL: maasDocsBase + "install/maas-setup.md#maastenantconfig-cr"}
	if dsc.maas == "Managed" {
		state := "unknown"
		if r.infraErr == nil {
			state = deploymentAvailable(c, infra, "maas-api")
		}
		switch state {
		case "True":
			apiPart.State, apiPart.Message = "ready", "Available in "+infra
		case "missing":
			apiPart.State, apiPart.Message = "waiting", "The operator has not created it yet"
		case "False":
			apiPart.State, apiPart.Message = "waiting", "Not available yet (it restarts until the database answers)"
		default:
			apiPart.State, apiPart.Message = "unknown", "Cannot read Deployment maas-api"
			if r.infraErr != nil {
				apiPart.Message = kube.SentenceStart(r.infraErr.Error())
			}
		}
		ai, aiMsg := readyCondition(c, rhoai.NamespacedPath(MaaSAPI(c), "aitenants", "ai-tenants", "models-as-a-service"))
		tc, tcMsg := readyCondition(c, rhoai.NamespacedPath(MaaSAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant"))
		auth := MaaSGatewayAuth(c)
		switch {
		case ai == "True" && tc == "True" && auth == "enforced":
			tenantPart.State, tenantPart.Message = "ready", "Tenant models-as-a-service is Ready; the gateway's authentication is enforced"
		case ai == "False" || tc == "False":
			tenantPart.State, tenantPart.Message = "waiting", "Tenant not ready: "+kube.TruncateText(kube.FirstNonEmpty(aiMsg, tcMsg, "no message"), 300)
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
}

// statusPlatform sums the parts up for the platform as a whole.
func (r *maasStatusRun) statusPlatform(dsc maasDSC) {
	rec, infra, st := r.rec, r.infra, r.st
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
	// The DataScienceCluster counts while this tool still owns a field it
	// recorded changing (teardown restores only those).
	if (rec.DSCChanged && dscMaaSOwned(dsc)) || (rec.AIGatewayChanged && dscAIGatewayOwned(dsc)) || rec.InfraNamespace != "" {
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
	case len(r.blockers) > 0:
		p.SetupBlockedReason = r.blockers[0]
	}
	if !p.ManagedByTool {
		p.TeardownBlockedReason = "No part of the MaaS platform was set up by this tool, so there is nothing for it to remove."
	}
}

// statusModels lists this tool's models with their publication, and the
// recorded publications whose model is gone.
func (r *maasStatusRun) statusModels() {
	c, st := r.c, r.st
	// This tool's models and their publication.
	models, err := ListLLMInferenceServices(c)
	if err != nil && !errors.Is(err, ErrLLMISvcAPIMissing) {
		slog.Debug("cannot list models for MaaS", "error", err)
	}
	rec, recErr := ReadMaaSRecord(c)
	authPhase := maasPhase(c, rhoai.NamespacedPath(MaaSAPI(c), "maasauthpolicies", "models-as-a-service", "rhoai-nightly-updater-access"))
	subPhase := maasPhase(c, rhoai.NamespacedPath(MaaSAPI(c), "maassubscriptions", "models-as-a-service", "rhoai-nightly-updater-subscription"))
	// Who may use the published models, and each one's limit: the
	// subscription's (one an earlier build wrote reads as the free sample).
	st.NoUnlimited = MaaSNoUnlimited(c)
	toolSub, subErr := readMaaSToolSubscription(c)
	if subErr != nil {
		slog.Debug("cannot read this tool's MaaSSubscription", "error", subErr)
	} else if toolSub.found && toolSub.owned {
		st.Access = &types.MaaSAudienceState{MaaSAudience: toolSub.audience.api()}
		if !toolSub.audience.Everyone {
			st.Access.GroupProblems = MaaSGroupProblems(c, toolSub.audience.Groups)
		}
	}
	owned, ownedErr := ReadModelsRecord(c)
	if ownedErr != nil {
		slog.Debug("cannot read the models record for MaaS", "error", ownedErr)
	}
	v := maasModelsView{rec: rec, recErr: recErr, authPhase: authPhase, subPhase: subPhase, toolSub: toolSub, subErr: subErr}
	for _, m := range models {
		entry, _ := CatalogEntry(m.Metadata.Name)
		if _, ok := ToolModelPath(m.Metadata.Namespace, m.Metadata.Name); !ok || !owned.Owns(m.Metadata) {
			continue
		}
		st.Items = append(st.Items, r.modelRow(m, entry, v))
	}
	// A recorded publication whose project or model is gone gets no row
	// (nothing of it is on the cluster to show): the background check
	// removes what is left on its own (maas_prune.go). Only its failure
	// needs attention.
	for _, g := range r.gone {
		st.Removing = append(st.Removing, types.MaaSRemoval{Namespace: g.pub.Project, Name: g.pub.Model, Reason: g.why})
	}
	if len(r.gone) > 0 {
		st.RemovalProblem = lastPruneFailure()
	}
	sort.Slice(st.Items, func(i, j int) bool {
		if st.Items[i].Namespace != st.Items[j].Namespace {
			return st.Items[i].Namespace < st.Items[j].Namespace
		}
		return st.Items[i].Name < st.Items[j].Name
	})
}

// maasModelsView is what every row of statusModels reads.
type maasModelsView struct {
	rec                 MaaSRecord
	recErr              error
	authPhase, subPhase string
	toolSub             maasToolSub
	subErr              error
}

// modelRow is the row of one of this tool's models.
func (r *maasStatusRun) modelRow(m LLMISvcObject, entry CatalogModel, v maasModelsView) types.MaaSModel {
	c, st := r.c, r.st
	p := &st.Platform
	rec, recErr, authPhase, subPhase, toolSub, subErr := v.rec, v.recErr, v.authPhase, v.subPhase, v.toolSub, v.subErr
	item := types.MaaSModel{Namespace: m.Metadata.Namespace, Name: m.Metadata.Name, ManagedByTool: true}
	// The catalog's name: the object's annotation is the project's to change.
	item.DisplayName = entry.DisplayName
	if cond, ok := kube.FindCondition(m.Status.Conditions, "Ready"); ok && cond.Status == "True" {
		item.ModelReady = true
	}
	if entry.Type != ModelTypeChat {
		item.Message = "Not offered for MaaS"
		item.SetupBlockedReason = maasEmbeddingReason
		return item
	}
	// Its publication is gone (its project is being deleted): what is
	// left goes on its own, so the row offers nothing.
	if why, gone := r.goneWhy(m.Metadata.Namespace, m.Metadata.Name); gone {
		item.Removing = true
		item.Message = "Being removed automatically: " + why + "."
		item.SetupBlockedReason = item.Message
		return item
	}
	ref, found, _ := readMaaSModelRef(c, m.Metadata.Namespace, m.Metadata.Name)
	if found && ToolOwned(ref.Metadata) {
		item.Published, item.Deployed = true, true
		item.Phase, item.Endpoint = ref.Status.Phase, ref.Status.Endpoint
		item.Access, item.Subscription = authPhase, subPhase
		if subErr == nil && toolSub.found && toolSub.owned {
			item.Limit = toolSub.limitOf(MaaSPub{m.Metadata.Namespace, m.Metadata.Name}).api()
		}
		item.Ready = item.Phase == "Ready" && authPhase == "Active" && subPhase == "Active"
		_, recordedRefs := rec.ModelRefsOf(m.Metadata.Namespace, m.Metadata.Name)
		switch refs := m.gatewayRefNames(); {
		case ModelHasMaaSRef(m) && len(refs) > 1 && recErr == nil && recordedRefs:
			// This build published it MaaS-only (its refs are recorded):
			// someone added a gateway since, and Unpublish and Publish
			// refuse until the refs are set (refsChangedRefusal).
			item.Ready, item.Partly = false, true
			item.Message = refsAddedStatus(m.Metadata.Namespace, m.Metadata.Name, refs)
		case ModelHasMaaSRef(m) && len(refs) > 1:
			// An earlier build kept the other gateways (maas_routing.go):
			// the rate limit fails every call on them.
			item.Ready, item.Partly = false, true
			item.Message = fmt.Sprintf("Partly published: the model routes through %s, as an earlier build of this tool published it, so calls to its direct address fail with 500. Unpublish, then Publish again.", strings.Join(refs, " and "))
		case item.Ready:
			item.Message = "Published"
		default:
			item.Message = fmt.Sprintf("Published; waiting (model reference %s, access %s, subscription %s)", kube.FirstNonEmpty(item.Phase, "pending"), kube.FirstNonEmpty(authPhase, "pending"), kube.FirstNonEmpty(subPhase, "pending"))
		}
	} else if left := maasLeftovers(c, m, recErr == nil && rec.IsPublished(m.Metadata.Namespace, m.Metadata.Name)); !found && len(left) > 0 {
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
			item.SetupBlockedReason = kube.FirstNonEmpty(item.SetupBlockedReason, maasNotReadyRowReason(st))
		} else if !item.ModelReady {
			item.SetupBlockedReason = kube.FirstNonEmpty(item.SetupBlockedReason, "The model is not ready yet.")
		}
	}
	if recorded, ok := rec.RouteBefore(m.Metadata.Namespace, m.Metadata.Name, modelGatewayRefsOf(m)); item.Published && recErr == nil && ok {
		item.MaaSOnlySetup = recorded.NoRoute
	}
	return item
}

// maasLeftovers names what is left on the cluster of a publication of the
// tool's model once its MaaSModelRef is gone: the model's route through
// the MaaS gateway (its refs, or a route KServe has not moved yet, read
// only for a recorded publication). This tool's record alone is no
// leftover: it is not shown, and it settles with the next publication or
// the background check.
func maasLeftovers(c *kube.Client, m LLMISvcObject, recorded bool) []string {
	var left []string
	if ModelHasMaaSRef(m) {
		left = append(left, "the model's route through "+MaaSGatewayName)
	} else if recorded {
		parents, _, err := readModelRoute(c, m.Metadata.Namespace, m.Metadata.Name)
		if err == nil && slices.Contains(parents, inferenceGatewayNamespace+"/"+MaaSGatewayName) {
			left = append(left, "the model's route through "+MaaSGatewayName+" (KServe has not moved it yet)")
		}
	}
	return left
}

// ToolOwned: this tool created the object (its label, and a write by it).
func ToolOwned(meta kube.ObjectMeta) bool {
	return meta.HasToolLabel() && meta.HasManager(envkit.ToolManagers)
}

// dashboardURLOrEmpty is the dashboard's address, or "".
func dashboardURLOrEmpty(c *kube.Client) string {
	return dashdev.CachedURL(c)
}

// maasModelRefObject is a MaaS custom resource as read.
type maasModelRefObject struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		ModelRefs []map[string]any `json:"modelRefs"`
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
func maasModelRefPath(c *kube.Client, project, name string) (string, bool) {
	switch name {
	case "smollm2-135m-instruct":
		return rhoai.NamespacedPath(MaaSAPI(c), "maasmodelrefs", project, "smollm2-135m-instruct"), true
	case "qwen25-05b-instruct":
		return rhoai.NamespacedPath(MaaSAPI(c), "maasmodelrefs", project, "qwen25-05b-instruct"), true
	case "qwen3-1-7b":
		return rhoai.NamespacedPath(MaaSAPI(c), "maasmodelrefs", project, "qwen3-1-7b"), true
	}
	return "", false
}

// readMaaSModelRef reads the MaaSModelRef of model name in project (none
// for a model that is not publishable).
func readMaaSModelRef(c *kube.Client, project, name string) (maasModelRefObject, bool, error) {
	path, ok := maasModelRefPath(c, project, name)
	if !ok {
		return maasModelRefObject{}, false, nil
	}
	return readMaaSObject(c, path)
}

func readMaaSObject(c *kube.Client, path string) (maasModelRefObject, bool, error) {
	var obj maasModelRefObject
	found, err := c.GetIntoOptional(path, &obj)
	if err != nil {
		return obj, false, err
	}
	if !found {
		return obj, false, nil
	}
	return obj, true, nil
}

// maasPhase is a MaaS object's status.phase ("" when absent).
func maasPhase(c *kube.Client, path string) string {
	obj, found, err := readMaaSObject(c, path)
	if err != nil || !found {
		return ""
	}
	return obj.Status.Phase
}

// MaaSPublishedIn reports whether anything of a publication of this tool's
// model name in project is left on the cluster, for the Models teardown
// (never for a model that is not publishable): its MaaSModelRef, or the
// model's route through the MaaS gateway. Unpublish removes both (and the
// project from the shared subscription, authorization policy and gateway),
// so the model goes after it. This tool's record alone is not a
// publication: the tab offers no Unpublish for it.
func MaaSPublishedIn(c *kube.Client, project, name string) (bool, error) {
	refPath, ok := maasModelRefPath(c, project, name)
	if !ok {
		return false, nil
	}
	_, found, err := kube.ReadObjectMeta(c, refPath)
	if err != nil && !kube.IsK8sNotFound(err) {
		return false, err
	}
	if found {
		return true, nil
	}
	model, found, err := ReadToolModel(c, project, name)
	if err != nil {
		return false, err
	}
	if !found || !ModelHasMaaSRef(model) {
		return false, nil
	}
	return ownsModel(c, model.Metadata)
}

// maasOnlySetup reports whether this tool's model name in project is
// published and had no route before (MaaSModelRefs.NoRoute, or a setup
// through MaaS only whose publish recorded nothing: RouteBefore): the
// Models tab set it up to be reached through MaaS only, or "Only inside
// the cluster" and it was published on the MaaS tab. The Models teardown
// unpublishes such a model itself (Unpublish leaves it with no route, as
// before); any other published model is unpublished on the MaaS tab.
func maasOnlySetup(c *kube.Client, project, name string) (bool, error) {
	published, err := MaaSPublishedIn(c, project, name)
	if err != nil || !published {
		return false, err
	}
	rec, err := ReadMaaSRecord(c)
	if err != nil {
		return false, err
	}
	model, found, err := ReadToolModel(c, project, name)
	if err != nil {
		return false, err
	}
	var current GatewayRefs
	if found {
		current = modelGatewayRefsOf(model)
	}
	refs, ok := rec.RouteBefore(project, name, current)
	return ok && refs.NoRoute, nil
}

// SetUpByTool: the record says this tool set the MaaS platform up and has
// not removed it (Clean up MaaS clears each of these as it goes). Its list
// of publications does not count: a publication is its MaaSModelRef on the
// cluster, and the record keeps one whose project is gone until the
// background check drops it.
func (r MaaSRecord) SetUpByTool() bool {
	return r.DSCChanged || r.AIGatewayChanged || r.DBNamespace != "" || r.Database != "" || r.InfraNamespace != "" || r.POCCredsUID != ""
}
