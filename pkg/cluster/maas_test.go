package cluster

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tMaaSDSC      = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"
	tMaaSGateway  = "/apis/gateway.networking.k8s.io/v1/namespaces/openshift-ingress/gateways/maas-default-gateway"
	tMaaSClass    = "/apis/gateway.networking.k8s.io/v1/gatewayclasses/openshift-default"
	tMaaSDB       = "/apis/postgresql.cnpg.io/v1/namespaces/redhat-ai-gateway-infra/clusters/maas-db"
	tMaaSDBConfig = "/api/v1/namespaces/redhat-ai-gateway-infra/secrets/maas-db-config"
	tMaaSRef      = "/apis/maas.opendatahub.io/v1alpha1/namespaces/proj/maasmodelrefs/qwen25-05b-instruct"
	tMaaSSub      = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/rhoai-nightly-updater-subscription"
	tMaaSAuth     = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maasauthpolicies/rhoai-nightly-updater-access"
	tMaaSPassword = "s3cr:t/p@ss"
)

// newMaaSCluster is a fake cluster as the user's: aigateway Managed,
// modelsAsAService Removed, a Ready Kuadrant whose Authorino is set up for
// TLS, CloudNativePG installed, the infrastructure namespace, and no MaaS
// objects. Controllers are simulated on read: the database becomes ready
// (and its application Secret appears), the gateway is programmed, and once
// MaaS is Managed maas-api, the tenant and the gateway's AuthPolicy are
// ready; MaaS objects report Ready/Active.
func newMaaSCluster(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newModelsCluster(t)
	f := mc.f
	f.putJSON(tMaaSDSC, `{"spec":{"components":{"kserve":{"managementState":"Managed"},"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Removed"}}}}}`)
	f.putJSON("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant", `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	// The group's discovery as RHCL 1.4 serves it (live): v1 first, where
	// AuthPolicy is, while Kuadrant is only in v1beta1.
	// ...and, as the API server does, 404 for Kuadrant at v1, which does not
	// serve it (the fake would answer an empty list).
	f.fail["GET /apis/kuadrant.io/v1/kuadrants"] = 404
	f.groups = map[string]string{"/apis/kuadrant.io": `{"kind":"APIGroup","name":"kuadrant.io","versions":[{"groupVersion":"kuadrant.io/v1","version":"v1"},{"groupVersion":"kuadrant.io/v1beta1","version":"v1beta1"},{"groupVersion":"kuadrant.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"kuadrant.io/v1","version":"v1"}}`,
		// RHOAI serves the MaaS API (its in-use checks list it).
		"/apis/maas.opendatahub.io": maasGroupDocV1alpha1,
		// CloudNativePG is installed: its API group is served.
		"/apis/postgresql.cnpg.io": `{"kind":"APIGroup","name":"postgresql.cnpg.io","versions":[{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}}`}
	// The operators' controllers run (a served API alone is not an
	// installed operator: CRDs stay after an uninstall).
	f.putJSON("/apis/apps/v1/namespaces/openshift-operators/deployments/kuadrant-operator-controller-manager", `{}`)
	f.putJSON("/apis/apps/v1/namespaces/cnpg-system/deployments/cnpg-controller-manager", `{}`)
	f.putJSON("/api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization", `{"metadata":{"annotations":{"service.beta.openshift.io/serving-cert-secret-name":"authorino-server-cert"}}}`)
	f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{"listener":{"tls":{"enabled":true}}}}`)
	f.putJSON("/apis/apps/v1/namespaces/kuadrant-system/deployments/authorino", `{"spec":{"template":{"spec":{"containers":[{"name":"authorino","env":[{"name":"SSL_CERT_FILE","value":"/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"}]}]}}}}`)
	f.putJSON("/api/v1/namespaces/redhat-ai-gateway-infra", `{}`)
	f.putJSON("/apis/ingress.operator.openshift.io/v1/namespaces/openshift-ingress/dnsrecords/maas", `{"spec":{"dnsName":"maas.apps.example.com."},"status":{"zones":[{"conditions":[{"type":"Published","status":"True"}]}]}}`)
	prev := f.onGet
	f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		switch k.plural {
		case "clusters":
			obj["status"] = map[string]any{"readyInstances": 1, "phase": "Cluster in healthy state"}
			if !f.hasLocked("/api/v1/namespaces/redhat-ai-gateway-infra/secrets/maas-db-app") {
				f.objects[fakeKey{gv: "v1", ns: "redhat-ai-gateway-infra", plural: "secrets", name: "maas-db-app"}] = map[string]any{
					"metadata": map[string]any{"name": "maas-db-app", "uid": "app-uid"},
					"data": map[string]any{"username": base64.StdEncoding.EncodeToString([]byte("maas")),
						"password": base64.StdEncoding.EncodeToString([]byte(tMaaSPassword))},
				}
			}
		case "maasmodelrefs":
			obj["status"] = map[string]any{"phase": "Ready", "endpoint": "https://maas.apps.example.com/proj/qwen25-05b-instruct"}
		case "maassubscriptions", "maasauthpolicies":
			obj["status"] = map[string]any{"phase": "Active"}
		case "llminferenceservices":
			// KServe has seen the model's current spec.
			if st, ok := obj["status"].(map[string]any); ok {
				st["observedGeneration"] = ensureMap(obj, "metadata")["generation"]
			}
		}
	}
	f.beforeServe = func(method, path string) {
		if method == "PATCH" && strings.Contains(path, "/datascienceclusters/") {
			// The operator answers MaaS Managed with maas-api and the tenant.
			f.putJSON("/apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/maas-api", `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
			f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/ai-tenants/aitenants/models-as-a-service", `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
			f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maastenantconfigs/default-tenant", `{"status":{"infraNamespace":"redhat-ai-gateway-infra","phase":"Degraded","conditions":[{"type":"Ready","status":"True"}]}}`)
			f.putJSON("/apis/kuadrant.io/v1/namespaces/openshift-ingress/authpolicies/maas-gateway-auth", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"maas-controller"}},"spec":{"targetRef":{"kind":"Gateway","name":"maas-default-gateway"}},"status":{"conditions":[{"type":"Enforced","status":"True"}]}}`)
		}
	}
	// The API server gives the fields a merge patch sets to its field
	// manager (here for the DataScienceCluster, whose modelsAsAService
	// ownership removal checks).
	f.onMergePatch = recordDSCFieldManagers
	mc.route = simulateKServeRoute(mc)
	return mc
}

// recordDSCFieldManagers is an onMergePatch that gives the fields a merge
// patch of the DataScienceCluster sets to its field manager, as the API
// server does (the manager's earlier entry is replaced).
func recordDSCFieldManagers(r *http.Request, k fakeKey, patch, next map[string]any) {
	manager := r.URL.Query().Get("fieldManager")
	if k.plural != "datascienceclusters" || manager == "" {
		return
	}
	var fields func(v any) map[string]any
	fields = func(v any) map[string]any {
		out := map[string]any{}
		if m, ok := v.(map[string]any); ok {
			for key, child := range m {
				out["f:"+key] = fields(child)
			}
		}
		return out
	}
	meta := ensureMap(next, "metadata")
	var kept []any
	entries, _ := meta["managedFields"].([]any)
	for _, e := range entries {
		if em, _ := e.(map[string]any); em["manager"] != manager {
			kept = append(kept, e)
		}
	}
	meta["managedFields"] = append(kept, map[string]any{"manager": manager, "operation": "Update", "fieldsV1": map[string]any{"f:spec": fields(patch["spec"])}})
}

// hasLocked is has for use inside onGet, which runs under the fake's lock.
func (f *resourceFake) hasLocked(path string) bool {
	k, _ := parseFakePath(path)
	return f.objects[k] != nil
}

func runMaaSOp(t *testing.T, c *Client, req types.MaaSRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunMaaS(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunMaaS: %v", err)
	}
	return res, events
}

func indexOfPrefix(list []string, prefix string) int {
	for i, s := range list {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

func TestMaaSSetup_FollowsTheDocumentedOrderAndShapes(t *testing.T) {
	mc := newMaaSCluster(t)
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	if got := finalSteps(events); got != "check=success rhcl=success kuadrant=success cnpg=success database=success gateway=success authorino-tls=success dsc=success ready=success" {
		t.Fatalf("steps = %s", got)
	}
	muts := mc.f.mutations()
	order := []string{"POST /apis/postgresql.cnpg.io/v1/namespaces/redhat-ai-gateway-infra/clusters", "POST /api/v1/namespaces/redhat-ai-gateway-infra/secrets",
		"POST /apis/gateway.networking.k8s.io/v1/gatewayclasses", "POST /apis/gateway.networking.k8s.io/v1/namespaces/openshift-ingress/gateways", "PATCH " + tMaaSDSC}
	last := -1
	for _, want := range order {
		i := indexOfPrefix(muts, want)
		if i <= last {
			t.Fatalf("%q not after the previous write in %v", want, muts)
		}
		last = i
	}
	// The connection URL: the docs' format with sslmode=require, never in
	// the logs or the message.
	sec := mc.f.get(tMaaSDBConfig)
	raw, _ := base64.StdEncoding.DecodeString(sec["data"].(map[string]any)["DB_CONNECTION_URL"].(string))
	// Assembled so that no credential-shaped URL appears in the source.
	userinfo := strings.Join([]string{"maas", "s3cr%3At%2Fp%40ss"}, ":")
	if want := "postgresql" + "://" + userinfo + "@maas-db-rw.redhat-ai-gateway-infra.svc.cluster.local:5432/maas?sslmode=require"; string(raw) != want {
		t.Fatalf("DB_CONNECTION_URL = %s", raw)
	}
	if labels := sec["metadata"].(map[string]any)["labels"].(map[string]any); labels["app"] != "maas-api" || labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("secret labels = %v", labels)
	}
	all, _ := json.Marshal(res)
	for _, e := range events {
		all = append(all, e.Message...)
	}
	if strings.Contains(string(all), "s3cr") {
		t.Fatal("the database password leaked into the output")
	}
	db := mc.f.get(tMaaSDB)
	if spec := db["spec"].(map[string]any); fmt.Sprint(spec["instances"]) != "1" || fmt.Sprint(spec["storage"]) != "map[size:1Gi]" {
		t.Fatalf("database spec = %v", spec)
	}
	// The gateway as setup-gateway.sh loadbalancer mode makes it.
	gw, _ := json.Marshal(mc.f.get(tMaaSGateway))
	for _, want := range []string{`"hostname":"maas.apps.example.com"`, `"name":"custom-cert"`, `"opendatahub.io/managed":"false"`, `"security.opendatahub.io/authorino-tls-bootstrap":"true"`,
		`"gatewayClassName":"openshift-default"`, `"values":["redhat-ai-gateway-infra","models-as-a-service"]`} {
		if !strings.Contains(string(gw), want) {
			t.Fatalf("gateway lacks %s: %s", want, gw)
		}
	}
	// The docs' external listener, plus an in-cluster one on the gateway's
	// Service name with a service-ca certificate (live: the dashboard's
	// playground proxy calls a published model there).
	for _, want := range []string{`"parametersRef":{"group":"","kind":"ConfigMap","name":"maas-gateway-options"}`,
		`"hostname":"maas-default-gateway-openshift-default.openshift-ingress.svc.cluster.local"`, `"name":"maas-gw-service-tls"`} {
		if !strings.Contains(string(gw), want) {
			t.Fatalf("gateway lacks %s: %s", want, gw)
		}
	}
	opts, _ := json.Marshal(mc.f.get("/api/v1/namespaces/openshift-ingress/configmaps/maas-gateway-options"))
	if !strings.Contains(string(opts), `service.beta.openshift.io/serving-cert-secret-name: maas-gw-service-tls`) {
		t.Fatalf("options ConfigMap: %s", opts)
	}
	dsc, _ := json.Marshal(mc.f.get(tMaaSDSC))
	if !strings.Contains(string(dsc), `"modelsAsAService":{"managementState":"Managed"}`) {
		t.Fatalf("dsc = %s", dsc)
	}
	rec, _ := readMaaSRecord(mc.c)
	if !rec.DSCChanged || rec.PreviousModelsAsAService != "Removed" {
		t.Fatalf("record = %+v", rec)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil || !st.Platform.Ready || !st.Platform.ManagedByTool {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestMaaSTeardown_ReverseOrderRestoresTheDSCAndKeepsForeignObjects(t *testing.T) {
	mc := newMaaSCluster(t)
	// A GatewayClass someone else made: used, never deleted.
	mc.f.putJSON(tMaaSClass, `{"spec":{"controllerName":"openshift.io/gateway-controller/v1"}}`)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.requests = nil
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success {
		t.Fatalf("teardown failed: %+v", res)
	}
	if got := finalSteps(events); got != "check=success unpublish=success dsc=success gateway=success database=success" {
		t.Fatalf("steps = %s", got)
	}
	muts := mc.f.mutations()
	if a, b, c, d := indexOfPrefix(muts, "PATCH "+tMaaSDSC), indexOfPrefix(muts, "DELETE "+tMaaSGateway), indexOfPrefix(muts, "DELETE "+tMaaSDBConfig), indexOfPrefix(muts, "DELETE "+tMaaSDB); a < 0 || a >= b || b >= c || c >= d {
		t.Fatalf("removal order: %v", muts)
	}
	if !mc.f.has(tMaaSClass) || indexOfPrefix(muts, "DELETE "+tMaaSClass) >= 0 {
		t.Fatal("the foreign GatewayClass was deleted")
	}
	dsc, _ := json.Marshal(mc.f.get(tMaaSDSC))
	if !strings.Contains(string(dsc), `"modelsAsAService":{"managementState":"Removed"}`) {
		t.Fatalf("dsc not restored: %s", dsc)
	}
	if mc.f.has(tMaaSGateway) || mc.f.has("/api/v1/namespaces/openshift-ingress/configmaps/maas-gateway-options") || mc.f.has(tMaaSDB) || mc.f.has(tMaaSDBConfig) {
		t.Fatal("a tool object is left")
	}
	if !mc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
		t.Fatal("the Kuadrant CR was deleted")
	}
}

func TestMaaSTeardown_KeepsAToolCreatedOpenshiftDefaultClass(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if !mc.f.has(tMaaSClass) {
		t.Fatal("setup did not create the missing GatewayClass")
	}
	mc.f.requests = nil
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("second setup: %+v", res)
	}
	if indexOfPrefix(mc.f.mutations(), "PATCH "+tMaaSClass) >= 0 {
		t.Fatal("setup changed an existing GatewayClass")
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || !strings.Contains(res.Message, "GatewayClass openshift-default") {
		t.Fatalf("teardown: %+v", res)
	}
	if !mc.f.has(tMaaSClass) || indexOfPrefix(mc.f.mutations(), "DELETE "+tMaaSClass) >= 0 {
		t.Fatal("removal deleted GatewayClass openshift-default")
	}
}

func TestMaaSSetup_UsesAForeignDatabaseSecretAsIs(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON(tMaaSDBConfig, `{"data":{"DB_CONNECTION_URL":"eA=="}}`)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if mc.f.has(tMaaSDB) || indexOfPrefix(mc.f.mutations(), "PATCH "+tMaaSDBConfig) >= 0 {
		t.Fatal("a database was created or the foreign Secret changed")
	}
}

// With Connectivity Link's API served and no Kuadrant CR, setup creates
// kuadrant-system/kuadrant as platform-setup.md does, and records it as the
// tool's.
func TestMaaSSetup_CreatesTheKuadrantCRWhenMissing(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.mu.Lock()
	delete(mc.f.objects, fakeKey{gv: "kuadrant.io/v1beta1", ns: "kuadrant-system", plural: "kuadrants", name: "kuadrant"})
	mc.f.mu.Unlock()
	st, _ := GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "kuadrant"); p.State != "missing" || !strings.Contains(strings.Join(st.SetupPlan, "\n"), "Create the Kuadrant CR kuadrant-system/kuadrant") {
		t.Fatalf("part = %+v plan = %v", p, st.SetupPlan)
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	k := mc.f.get("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant")
	if k == nil || k["kind"] != "Kuadrant" || k["spec"] != nil {
		t.Fatalf("kuadrant = %v", k)
	}
	if indexOfPrefix(mc.f.mutations(), "POST /apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants") > indexOfPrefix(mc.f.mutations(), "POST /apis/postgresql.cnpg.io") {
		t.Fatalf("the Kuadrant CR comes before the database: %v", mc.f.mutations())
	}
	st, _ = GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "kuadrant"); !p.ManagedByTool || p.NotManaged {
		t.Fatalf("part = %+v", p)
	}
}

// maasPart is the status part with id.
func maasPart(t *testing.T, st *types.MaaSStatus, id string) types.MaaSPart {
	t.Helper()
	for _, p := range st.Parts {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no part %s in %+v", id, st.Parts)
	return types.MaaSPart{}
}

func TestMaaSStatus_AuthorinoTLSIsOfferedWithTheDocsCommands(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{}}`)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	p := maasPart(t, st, "authorino-tls")
	if p.State != "missing" || !strings.Contains(p.Commands, "oc patch authorino authorino -n kuadrant-system") || len(p.Actions) != 1 || p.Actions[0].ID != "authorino-tls" {
		t.Fatalf("part = %+v", p)
	}
}

func TestMaaSPublishAndUnpublish(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	mc.f.requests = nil
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	if got := finalSteps(events); got != "check=success gateway=success model=success maas=success ready=success" {
		t.Fatalf("steps = %s", got)
	}
	sub, _ := json.Marshal(mc.f.get(tMaaSSub))
	auth, _ := json.Marshal(mc.f.get(tMaaSAuth))
	gw, _ := json.Marshal(mc.f.get(tMaaSGateway))
	model, _ := json.Marshal(mc.f.get(testModelPath))
	for what, pair := range map[string][2]string{
		"subscription": {string(sub), `"tokenRateLimits":[{"limit":100,"window":"1m"}]`},
		"subject":      {string(auth), `"groups":[{"name":"system:authenticated"}]`},
		"gateway":      {string(gw), `"values":["redhat-ai-gateway-infra","models-as-a-service","proj"]`},
		// The MaaS gateway only: Kuadrant enforces maas-controller's
		// TokenRateLimitPolicy on every gateway the model's route names, and
		// on the inference gateway it fails every call with 500 (live).
		"model": {string(model), `"refs":` + tMaaSOnly},
	} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("%s lacks %s: %s", what, pair[1], pair[0])
		}
	}
	if !mc.f.has(tMaaSRef) {
		t.Fatal("no MaaSModelRef")
	}
	// The model cannot be deleted while it is published.
	if res, _ := TeardownModels(mc.c, "proj"); res.Success || res.ErrorCode != "in_use" {
		t.Fatalf("model teardown while published: %+v", res)
	}
	res, events = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	// Back to gateway {}: the refs removed by a merge patch, the gateway
	// object kept (live: an empty apply nulled it and the API server
	// refused that with 422).
	router := mc.f.get(testModelPath)["spec"].(map[string]any)["router"].(map[string]any)
	if gw, ok := router["gateway"].(map[string]any); !ok || gw["refs"] != nil || len(gw) != 0 {
		t.Fatalf("model router after unpublish: %v", router)
	}
	if !hasMutation(mc.f, "PATCH "+testModelPath) {
		t.Fatal("unpublish did not patch the model")
	}
	if got := finalSteps(events); got != "check=success maas=success model=success gateway=success" {
		t.Fatalf("steps = %s", got)
	}
	if mc.f.has(tMaaSRef) || mc.f.has(tMaaSSub) || mc.f.has(tMaaSAuth) {
		t.Fatal("a MaaS object is left")
	}
	gw, _ = json.Marshal(mc.f.get(tMaaSGateway))
	if strings.Contains(string(gw), `"proj"`) {
		t.Fatalf("the gateway still admits proj: %s", gw)
	}
	rec, _ := readMaaSRecord(mc.c)
	if len(rec.Published) != 0 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestCleanUp_UnpublishesFirstAndRemovesThePlatformLast(t *testing.T) {
	parts := cleanupParts(nil, types.CleanupRequest{MaaSPublished: []string{"p"}, Models: []string{"p"}, InferenceGateway: true, MaaSPlatform: true})
	var ids []string
	for _, p := range parts {
		ids = append(ids, p.id)
	}
	if got := strings.Join(ids, " "); got != "maas-unpublish:p model:p inference-gateway maas-platform" {
		t.Fatalf("order = %s", got)
	}
}

func TestValidateMaaSRequest(t *testing.T) {
	for req, want := range map[types.MaaSRequest]string{
		{Action: "setup"}:                            "",
		{Action: "setup", Project: "p"}:              "the platform takes no project",
		{Action: "publish"}:                          "a valid project is required",
		{Action: "publish", Project: "Bad_Name"}:     "a valid project is required",
		{Action: "unpublish", Project: "p"}:          "",
		{Action: "nope"}:                             "action must be setup, teardown, publish, unpublish, change-limit, change-access, change-database, upgrade-rhcl, rhcl-memory, rhcl-deny-on-failure, authorino-tls or remove-tier-policies",
		{Action: "upgrade-rhcl"}:                     "",
		{Action: "remove-tier-policies", Model: "m"}: "the platform takes no model",
	} {
		if got := ValidateMaaSRequest(req); got != want {
			t.Errorf("%+v: %q, want %q", req, got, want)
		}
	}
}

// Authorino trusts the service CA either the way setup-authorino-tls.sh
// sets it or the way RHOAI's AI gateway does (live on 3.6).
func TestAuthorinoTrustsServiceCA(t *testing.T) {
	for _, tc := range []struct {
		name, deployment string
		want             bool
	}{
		{"the script's SSL_CERT_FILE", `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"SSL_CERT_FILE","value":"/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"}]}]}}}}`, true},
		{"the AI gateway's mounted bundle in SSL_CERT_DIR", `{"spec":{"template":{"spec":{
			"volumes":[{"name":"service-ca","projected":{"sources":[{"configMap":{"name":"aigrid-service-ca"}}]}}],
			"containers":[{"env":[{"name":"SSL_CERT_DIR","value":"/etc/ssl/certs/openshift-service-ca:/etc/pki/tls/certs:/etc/ssl/certs"}],
				"volumeMounts":[{"name":"service-ca","mountPath":"/etc/ssl/certs/openshift-service-ca"}]}]}}}}`, true},
		{"SSL_CERT_DIR with only system directories", `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"SSL_CERT_DIR","value":"/etc/pki/tls/certs:/etc/ssl/certs"}]}]}}}}`, false},
		{"a mounted directory that is not in SSL_CERT_DIR", `{"spec":{"template":{"spec":{
			"volumes":[{"name":"ca","configMap":{"name":"x"}}],
			"containers":[{"volumeMounts":[{"name":"ca","mountPath":"/etc/x"}]}]}}}}`, false},
		{"nothing", `{"spec":{"template":{"spec":{"containers":[{}]}}}}`, false},
	} {
		if got := authorinoTrustsServiceCA([]byte(tc.deployment)); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Live on RHOAI 3.6, after MaaS was turned off and on again, the DSC kept
// ModelsAsAServiceReady False/Removed while its AIGateway component (which
// the DSC copies) reported True and maas-api served: the component wins.
func TestMaaSStatus_TheComponentWinsOverAStaleDSCCondition(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	stale := `{"spec":{"components":{"kserve":{"managementState":"Managed"},"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"}}}},
		"status":{"conditions":[{"type":"ModelsAsAServiceReady","status":"False","reason":"Removed","message":"modelsAsAService ManagementState is Removed"}]}}`
	mc.f.putJSON(tMaaSDSC, stale)
	if st, _ := GetMaaSStatus(mc.c); st.Platform.Ready {
		t.Fatal("without the component, the DSC's condition must still count")
	}
	mc.f.putJSON("/apis/components.platform.opendatahub.io/v1alpha1/aigateways/default-aigateway",
		`{"status":{"conditions":[{"type":"ModelsAsAServiceReady","status":"True","reason":"Ready"}]}}`)
	st, err := GetMaaSStatus(mc.c)
	if err != nil || !st.Platform.Ready {
		t.Fatalf("status = %+v, %v", st.Platform, err)
	}
}

// A publication undone half-way stays visible and Unpublish finishes it
// (live: the model reference was deleted, then the model's patch failed,
// and the tab showed "Not published" with no way to retry).
func TestMaaSUnpublish_FinishesAHalfUndonePublication(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	mc.f.mu.Lock()
	delete(mc.f.objects, fakeKey{gv: "maas.opendatahub.io/v1alpha1", ns: "proj", plural: "maasmodelrefs", name: "qwen25-05b-instruct"})
	mc.f.mu.Unlock()
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || !st.Items[0].Published || st.Items[0].Ready || !strings.Contains(st.Items[0].Message, "Partly published") {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	router := mc.f.get(testModelPath)["spec"].(map[string]any)["router"].(map[string]any)
	if gw, ok := router["gateway"].(map[string]any); !ok || len(gw) != 0 {
		t.Fatalf("model router after unpublish: %v", router)
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Items) != 1 || st.Items[0].Published || st.Items[0].Message != "Not published" {
		t.Fatalf("status after unpublish = %+v", st.Items)
	}
}
