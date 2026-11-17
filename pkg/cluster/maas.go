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
// docs' spec.gateway.refs is not a field of LLMInferenceService), then a
// MaaSModelRef in the project, a MaaSSubscription (100 tokens a minute for
// system:authenticated, the docs' sample) and a MaaSAuthPolicy in
// models-as-a-service (docs/samples/maas-system/free/maas).

const (
	maasAPIGroup          = "maas.opendatahub.io/v1alpha1"
	maasGatewayName       = "maas-default-gateway"
	maasGatewayClass      = "openshift-default"
	maasHostPrefix        = "maas."
	maasDBCluster         = "maas-db"
	maasDBAppSecret       = "maas-db-app"
	maasDBConfigSecret    = "maas-db-config"
	maasDBName            = "maas"
	maasTenantNamespace   = "models-as-a-service"
	maasAITenantNamespace = "ai-tenants"
	maasModelRefName      = "qwen25-05b-instruct"
	maasAuthPolicyName    = "rhoai-nightly-updater-access"
	maasSubscriptionName  = "rhoai-nightly-updater-subscription"
	// maasModelManager is the field manager of the gateway ref this tool
	// adds to its model: removing it reverts exactly that change.
	maasModelManager  = "rhoai-nightly-updater-maas"
	maasRecordKey     = "maas"
	maasRecordManager = "rhoai-nightly-updater-maas-record"
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
	MaaSDeleteTimeout  = 2 * time.Minute
	MaaSPoll           = 5 * time.Second
)

// --- record (this tool's namespace, snapshot ConfigMap key "maas") ---------------

// maasRecord is what this tool did to the platform that the cluster cannot
// tell by itself: whether it set modelsAsAService and what it was before,
// and the projects whose model it published.
type maasRecord struct {
	DSCChanged               bool     `json:"dscChanged,omitempty"`
	PreviousModelsAsAService string   `json:"previousModelsAsAService,omitempty"`
	Published                []string `json:"published,omitempty"`
	At                       string   `json:"at,omitempty"`
	By                       string   `json:"by,omitempty"`
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

func (r maasRecord) published(project string) bool { return containsString(r.Published, project) }

func (r *maasRecord) setPublished(project string, on bool) {
	var out []string
	for _, p := range r.Published {
		if p != project {
			out = append(out, p)
		}
	}
	if on {
		out = append(out, project)
	}
	sort.Strings(out)
	r.Published = out
}

// --- reads ---------------------------------------------------------------------

// maasInfraNamespace is where maas-api runs and maas-db-config goes: the
// tenant's status.infraNamespace when MaaS runs, else derived from the
// applications namespace as the docs' scripts do (derive_infra_namespace in
// scripts/setup-database.sh: redhat-ods-applications ->
// redhat-ai-gateway-infra, opendatahub -> odh-ai-gateway-infra).
func maasInfraNamespace(c *Client) string {
	body, _, err := c.get(namespacedPath(maasAPIGroup, "maastenantconfigs", "models-as-a-service", "default-tenant"))
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
	st := kuadrantState{found: true, namespace: k.Metadata.Namespace}
	if cond, ok := findCondition(k.Status.Conditions, "Ready"); ok {
		st.ready = cond.Status == "True"
		st.message = firstNonEmpty(cond.Message, cond.Reason)
	}
	return st, nil
}

// rhclVersion is the installed Red Hat Connectivity Link operator's version
// (its CSV), or "" when it is not found.
func rhclVersion(c *Client) string {
	csvs, err := listClusterCSVs(c)
	if err != nil {
		return ""
	}
	for _, csv := range csvs {
		if containsString(csv.packages(), "rhcl-operator") || strings.HasPrefix(csv.Name, "rhcl-operator.") {
			if i := strings.Index(csv.Name, ".v"); i >= 0 {
				return csv.Name[i+2:]
			}
		}
	}
	return ""
}

// rhclWarning: the docs ask for RHCL 1.5 or later (prerequisites.md
// "RHOAI 3.6 — use RHCL v1.5"; 1.4.x can hang callers, Kuadrant/wasm-shim#425).
func rhclWarning(version string) string {
	if version == "" || compareVersions(strings.SplitN(version, "-", 2)[0], "1.5") >= 0 {
		return ""
	}
	return fmt.Sprintf("Red Hat Connectivity Link %s is installed; the MaaS docs ask for 1.5 or later (OLM channel stable-1.5): on 1.4.x a caller with two or more subscriptions can hang. A cluster-admin upgrades it; MaaS works for a single subscription meanwhile.", version)
}

// maasObjectPath is the path of a platform object this tool reads or
// writes, by kind; only these names.
func maasObjectPath(kind, ns string) string {
	switch kind {
	case "Gateway":
		return namespacedPath("gateway.networking.k8s.io/v1", "gateways", "openshift-ingress", "maas-default-gateway")
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
// loadbalancer mode), by kind.
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
	return map[string]map[string]interface{}{
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
				"listeners": []interface{}{map[string]interface{}{
					"name":     "https",
					"hostname": maasHostPrefix + domain,
					"port":     443,
					"protocol": "HTTPS",
					"allowedRoutes": map[string]interface{}{"namespaces": map[string]interface{}{
						"from": "Selector",
						"selector": map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{
							"key": "kubernetes.io/metadata.name", "operator": "In", "values": values,
						}}},
					}},
					"tls": map[string]interface{}{
						"mode":            "Terminate",
						"certificateRefs": []interface{}{map[string]interface{}{"group": "", "kind": "Secret", "name": certificate}},
					},
				}},
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
// gateway (named maas-gateway-auth on RHOAI 3.6; the docs name others, so
// it is found by its target): "enforced", "not-enforced", "missing" or "".
func maasGatewayAuth(c *Client) string {
	versions := servedVersions(c, kuadrantGroup, []string{"v1"})
	if len(versions.Versions) == 0 {
		return ""
	}
	body, _, err := c.get(namespacedPath("kuadrant.io/"+versions.Versions[0], "authpolicies", "openshift-ingress", ""))
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Spec struct {
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
	if json.Unmarshal(body, &list) != nil {
		return ""
	}
	for _, ap := range list.Items {
		if ap.Spec.TargetRef.Kind != "Gateway" || ap.Spec.TargetRef.Name != maasGatewayName {
			continue
		}
		if cond, ok := findCondition(ap.Status.Conditions, "Enforced"); ok && cond.Status == "True" {
			return "enforced"
		}
		return "not-enforced"
	}
	return "missing"
}

// authorinoTLS checks what scripts/setup-authorino-tls.sh sets on
// Kuadrant's Authorino in ns: the Service's serving-cert annotation, the
// Authorino CR's TLS listener, and the Deployment's trust of the service
// CA (authorinoTrustsServiceCA). It
// returns "" when all are set (or cannot be read: unknown is not a
// failure), else what is missing.
func authorinoTLS(c *Client, ns string) (string, bool) {
	var missing []string
	known := true
	if meta, found, err := readObjectMeta(c, fmt.Sprintf("/api/v1/namespaces/%s/services/%s", ns, "authorino-authorino-authorization")); err != nil {
		known = false
	} else if !found || meta.Annotations[servingCertAnnotation] != "authorino-server-cert" {
		missing = append(missing, "the Service's serving certificate")
	}
	if body, _, err := c.get(namespacedPath("operator.authorino.kuadrant.io/v1beta1", "authorinos", ns, "authorino")); err != nil {
		known = false
	} else {
		var a struct {
			Spec struct {
				Listener struct {
					TLS struct {
						Enabled bool `json:"enabled"`
					} `json:"tls"`
				} `json:"listener"`
			} `json:"spec"`
		}
		if json.Unmarshal(body, &a) != nil || !a.Spec.Listener.TLS.Enabled {
			missing = append(missing, "TLS on Authorino's listener")
		}
	}
	if body, _, err := c.get(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, "authorino")); err != nil {
		known = false
	} else if !authorinoTrustsServiceCA(body) {
		missing = append(missing, "trust of the service CA (SSL_CERT_FILE)")
	}
	return strings.Join(missing, ", "), known
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
// and deploy.sh's restart of Authorino after them.
func authorinoTLSCommands(ns string) string {
	return fmt.Sprintf(`oc annotate service authorino-authorino-authorization -n %[1]s \
  service.beta.openshift.io/serving-cert-secret-name=authorino-server-cert --overwrite
oc patch authorino authorino -n %[1]s --type=merge \
  -p '{"spec":{"listener":{"tls":{"enabled":true,"certSecretRef":{"name":"authorino-server-cert"}}}}}'
oc -n %[1]s set env deployment/authorino \
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt \
  REQUESTS_CA_BUNDLE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
oc rollout restart deployment/authorino -n %[1]s`, ns)
}

// --- status --------------------------------------------------------------------

// GetMaaSStatus reports the MaaS platform part by part and this tool's
// models with their publication.
func GetMaaSStatus(c *Client) (*types.MaaSStatus, error) {
	st := &types.MaaSStatus{Subscription: maasSubscriptionName, DashboardURL: dashboardURLOrEmpty(c)}
	rec, err := readMaaSRecord(c)
	if err != nil {
		return nil, err
	}
	dsc, dscErr := readMaaSDSC(c)
	infra := maasInfraNamespace(c)
	domain, _ := clusterAppsDomain(c)
	if domain != "" {
		st.URL = "https://" + maasHostPrefix + domain
	}
	part := func(p types.MaaSPart) { st.Parts = append(st.Parts, p) }

	// The DataScienceCluster.
	dscPart := types.MaaSPart{ID: "dsc", Title: "MaaS in the DataScienceCluster", DocURL: maasDocsBase + "install/maas-setup.md#configure-datasciencecluster"}
	switch {
	case dscErr != nil:
		dscPart.State, dscPart.Message = "unknown", "Cannot read the DataScienceCluster: "+dscErr.Error()
	case dsc.read.Object == nil:
		st.Blocked = &types.ComponentBlock{Component: "aigateway", Reason: "MaaS needs RHOAI's DataScienceCluster, and this cluster has none. A cluster-admin creates one on the Components page."}
		dscPart.State, dscPart.Message = "problem", "No DataScienceCluster"
	case dsc.aigateway != "Managed":
		st.Blocked = &types.ComponentBlock{Component: "aigateway", DSC: dsc.name, State: dsc.aigateway,
			Reason: fmt.Sprintf("MaaS runs in RHOAI's AI gateway, which is %s in DataScienceCluster %s. A cluster-admin sets aigateway to Managed on the Components page (MaaS platform setup, platform-setup.md); then MaaS can be set up here.", firstNonEmpty(dsc.aigateway, "not set"), dsc.name)}
		dscPart.State, dscPart.Message = "problem", "aigateway is "+firstNonEmpty(dsc.aigateway, "not set")
	case dsc.maas == "Managed":
		dscPart.ManagedByTool, dscPart.NotManaged = rec.DSCChanged, !rec.DSCChanged
		switch dsc.ready {
		case "False":
			dscPart.State, dscPart.Message = "waiting", "modelsAsAService is Managed; not ready: "+truncateMessage(dsc.readyMsg)
		default:
			dscPart.State, dscPart.Message = "ready", "aigateway and modelsAsAService are Managed"
		}
	default:
		dscPart.State, dscPart.Message = "missing", "modelsAsAService is "+firstNonEmpty(dsc.maas, "not set")+"; setup sets it to Managed, last"
	}
	part(dscPart)

	// Connectivity Link (Kuadrant): a prerequisite, never created here.
	kq, kErr := readKuadrant(c)
	kPart := types.MaaSPart{ID: "kuadrant", Title: "Connectivity Link (Kuadrant)", DocURL: maasDocsBase + "install/platform-setup.md", NotManaged: kq.found}
	switch {
	case kErr != nil:
		kPart.State, kPart.Message = "unknown", "Cannot read the Kuadrant CR: "+kErr.Error()
	case !kq.found:
		kPart.State, kPart.Message = "manual", "No Kuadrant CR. A cluster-admin installs Red Hat Connectivity Link and creates the Kuadrant CR, as platform-setup.md shows."
	case kq.ready:
		kPart.State, kPart.Message = "ready", "Kuadrant "+kq.namespace+"/kuadrant is Ready"
	default:
		kPart.State, kPart.Message = "waiting", "Kuadrant is not Ready: "+firstNonEmpty(kq.message, "no message")
	}
	kPart.Warning = rhclWarning(rhclVersion(c))
	part(kPart)

	// The database and its connection Secret.
	dbPart := types.MaaSPart{ID: "database", Title: "Database (PostgreSQL) and maas-db-config", DocURL: maasDocsBase + "install/maas-setup.md#database-setup"}
	secMeta, secFound, secErr := readObjectMeta(c, maasObjectPath("Secret", infra))
	clMeta, clFound, clReady, clPhase, clErr := readMaaSDBCluster(c, infra)
	switch {
	case secErr != nil:
		dbPart.State, dbPart.Message = "unknown", fmt.Sprintf("Cannot read Secret %s/%s: %v", infra, maasDBConfigSecret, secErr)
	case secFound && !toolOwned(secMeta):
		dbPart.State, dbPart.NotManaged = "ready", true
		dbPart.Message = fmt.Sprintf("Secret %s/%s exists: maas-api uses the database it names", infra, maasDBConfigSecret)
	case clErr == nil && clFound && toolOwned(clMeta):
		dbPart.ManagedByTool = true
		switch {
		case clReady && secFound:
			dbPart.State, dbPart.Message = "ready", fmt.Sprintf("CloudNativePG %s/%s runs (1 instance, development and test use); Secret %s set", infra, maasDBCluster, maasDBConfigSecret)
		case clReady:
			dbPart.State, dbPart.Message = "missing", "The database runs; its connection Secret is not written yet"
		default:
			dbPart.State, dbPart.Message = "waiting", "The database is starting: "+clPhase
		}
	case servedVersionsMissing(c, cnpgGroup):
		dbPart.State = "manual"
		dbPart.Message = "The CloudNativePG operator is not installed. A cluster-admin installs it from OperatorHub (certified operators), or creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md)."
	default:
		dbPart.State, dbPart.Message = "missing", "Setup runs a one-instance CloudNativePG database (development and test use) in "+infra
	}
	part(dbPart)

	// The gateway.
	gwPart := types.MaaSPart{ID: "gateway", Title: "Gateway maas-default-gateway", DocURL: maasDocsBase + "install/maas-setup.md#create-gateway"}
	gw, gwErr := readMaaSGateway(c)
	switch {
	case gwErr != nil:
		gwPart.State, gwPart.Message = "unknown", "Cannot read the gateway: "+gwErr.Error()
	case !gw.found:
		gwPart.State, gwPart.Message = "missing", "Setup creates it with an external load balancer at "+firstNonEmpty(st.URL, "maas.<apps domain>")
	default:
		gwPart.ManagedByTool, gwPart.NotManaged = gw.owned, !gw.owned
		switch {
		case gw.acceptedErr != "":
			gwPart.State, gwPart.Message = "problem", "Not accepted: "+truncateMessage(gw.acceptedErr)
		case gw.programmed && gw.address != "":
			gwPart.State, gwPart.Message = "ready", "Programmed at "+gw.address
		default:
			gwPart.State, gwPart.Message = "waiting", "Waiting for its load balancer"
		}
		if !gw.owned {
			if h := gw.gw.externalHost(); h != "" {
				st.URL = "https://" + h
			}
		}
	}
	part(gwPart)

	// Authorino TLS: checked only.
	tlsPart := types.MaaSPart{ID: "authorino-tls", Title: "Authorino TLS", DocURL: maasDocsBase + "install/maas-setup.md#create-gateway", NotManaged: true}
	if kq.found {
		missing, known := authorinoTLS(c, kq.namespace)
		switch {
		case missing != "":
			tlsPart.State = "manual"
			tlsPart.Message = "Not configured: " + missing + ". A cluster-admin runs the docs' setup-authorino-tls.sh commands below (this tool does not change Connectivity Link's Authorino). Without it, API keys are refused."
			tlsPart.Commands = authorinoTLSCommands(kq.namespace)
		case known:
			tlsPart.State, tlsPart.Message = "ready", "Authorino in "+kq.namespace+" serves TLS and trusts the service CA"
		default:
			tlsPart.State, tlsPart.Message = "unknown", "Cannot read Authorino's configuration"
		}
	} else {
		tlsPart.State, tlsPart.Message = "unknown", "Needs Connectivity Link first"
	}
	part(tlsPart)

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
		ai, aiMsg := readyCondition(c, namespacedPath(maasAPIGroup, "aitenants", "ai-tenants", "models-as-a-service"))
		tc, tcMsg := readyCondition(c, namespacedPath(maasAPIGroup, "maastenantconfigs", "models-as-a-service", "default-tenant"))
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
	part(apiPart)
	part(tenantPart)

	// The platform as a whole.
	p := &st.Platform
	p.Namespace = infra
	p.Deployed = dsc.maas == "Managed"
	p.Ready = true
	var waiting []string
	for _, pt := range st.Parts {
		if pt.ManagedByTool {
			p.ManagedByTool = true
		}
		if pt.State != "ready" {
			p.Ready = false
			if pt.State != "missing" {
				waiting = append(waiting, pt.Title)
			}
		}
	}
	switch {
	case p.Ready:
		p.Message = "Ready"
	case !p.Deployed && !p.ManagedByTool:
		p.Message = "Not set up"
	default:
		p.Message = "Needs attention: " + strings.Join(waiting, ", ")
	}
	if st.Blocked != nil {
		p.SetupBlockedReason = st.Blocked.Reason
	} else if !kq.found && kErr == nil {
		p.SetupBlockedReason = kPart.Message
	} else if dbPart.State == "manual" {
		p.SetupBlockedReason = dbPart.Message
	}
	if !p.ManagedByTool {
		p.TeardownBlockedReason = "No part of the MaaS platform was set up by this tool, so there is nothing for it to remove."
	}

	// This tool's models and their publication.
	models, err := listLLMInferenceServices(c)
	if err != nil && !errors.Is(err, errLLMISvcAPIMissing) {
		slog.Debug("cannot list models for MaaS", "error", err)
	}
	authPhase, _ := maasPhase(c, namespacedPath(maasAPIGroup, "maasauthpolicies", "models-as-a-service", "rhoai-nightly-updater-access"))
	subPhase, _ := maasPhase(c, namespacedPath(maasAPIGroup, "maassubscriptions", "models-as-a-service", "rhoai-nightly-updater-subscription"))
	for _, m := range models {
		if !modelOwned(m.Metadata) || m.Metadata.Name != maasModelRefName {
			continue
		}
		item := types.MaaSModel{ResourceState: types.ResourceState{Namespace: m.Metadata.Namespace, Name: m.Metadata.Name, ManagedByTool: true}}
		item.DisplayName = firstNonEmpty(m.Metadata.Annotations["openshift.io/display-name"], m.Metadata.Name)
		if cond, ok := findCondition(m.Status.Conditions, "Ready"); ok && cond.Status == "True" {
			item.ModelReady = true
		}
		ref, found, _ := readMaaSModelRef(c, m.Metadata.Namespace)
		if found && toolOwned(ref.Metadata) {
			item.Published, item.Deployed = true, true
			item.Phase, item.Endpoint = ref.Status.Phase, ref.Status.Endpoint
			item.Access, item.Subscription = authPhase, subPhase
			item.Ready = item.Phase == "Ready" && authPhase == "Active" && subPhase == "Active"
			switch {
			case item.Ready:
				item.Message = "Published"
			default:
				item.Message = fmt.Sprintf("Published; waiting (model reference %s, access %s, subscription %s)", firstNonEmpty(item.Phase, "pending"), firstNonEmpty(authPhase, "pending"), firstNonEmpty(subPhase, "pending"))
			}
		} else {
			item.Message = "Not published"
			if found {
				item.Message = "A MaaSModelRef of this name exists that this tool did not create"
				item.SetupBlockedReason = item.Message + "; it is left alone."
			}
			if !p.Ready {
				item.SetupBlockedReason = firstNonEmpty(item.SetupBlockedReason, "Set up the MaaS platform first.")
			} else if !item.ModelReady {
				item.SetupBlockedReason = firstNonEmpty(item.SetupBlockedReason, "The model is not ready yet.")
			}
		}
		st.Items = append(st.Items, item)
	}
	sort.Slice(st.Items, func(i, j int) bool { return st.Items[i].Namespace < st.Items[j].Namespace })
	return st, nil
}

// servedVersionsMissing: API discovery says the group is not served
// (false when discovery cannot tell).
func servedVersionsMissing(c *Client, group string) bool {
	v := servedVersions(c, group, nil)
	return v.Discovered && len(v.Versions) == 0
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

func readMaaSModelRef(c *Client, project string) (maasModelRefObject, bool, error) {
	return readMaaSObject(c, namespacedPath(maasAPIGroup, "maasmodelrefs", project, "qwen25-05b-instruct"))
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

// MaaSPublishedIn reports whether this tool's model in project is
// published to MaaS (its MaaSModelRef exists), for the Models teardown.
func MaaSPublishedIn(c *Client, project string) (bool, error) {
	_, found, err := readObjectMeta(c, namespacedPath(maasAPIGroup, "maasmodelrefs", project, "qwen25-05b-instruct"))
	if IsK8sError(err, 404) {
		return false, nil
	}
	return found, err
}
