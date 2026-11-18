package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// publishedMaaSCluster is newMaaSCluster with the platform set up, the
// tool's model in proj, and that model published.
func publishedMaaSCluster(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	return mc
}

func modelGateway(t *testing.T, mc *modelsCluster) map[string]interface{} {
	t.Helper()
	spec, _ := mc.f.get(testModelPath)["spec"].(map[string]interface{})
	router, _ := spec["router"].(map[string]interface{})
	gw, ok := router["gateway"].(map[string]interface{})
	if !ok {
		t.Fatalf("model router has no gateway object: %v", router)
	}
	return gw
}

func setModelRefs(mc *modelsCluster, refs []interface{}) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	k, _ := parseFakePath(testModelPath)
	spec := ensureMap(mc.f.objects[k], "spec")
	router := ensureMap(spec, "router")
	if refs == nil {
		router["gateway"] = map[string]interface{}{}
		return
	}
	router["gateway"] = map[string]interface{}{"refs": refs}
}

func deleteFakeObject(mc *modelsCluster, path string) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	k, _ := parseFakePath(path)
	delete(mc.f.objects, k)
}

// Finding 10 (the refs list is atomic, so publish and unpublish write the
// whole list from what they read) is now TestMaaSUnpublish_RestoresTheRecordedRefs
// and TestMaaSUnpublish_RefusesWhenSomeoneAddedARefWhilePublished.

// Finding 10: the model changing between the read and the write is a
// conflict, not a silent overwrite (resourceVersion precondition).
func TestMaaSUnpublish_ModelRefsWriteHasAResourceVersionPrecondition(t *testing.T) {
	mc := publishedMaaSCluster(t)
	var sent string
	prev := mc.f.beforeServe
	mc.f.beforeServe = func(method, path string) {
		prev(method, path)
		if method == "PATCH" && path == testModelPath {
			// Someone writes the model between the tool's read and its patch.
			mc.f.mu.Lock()
			k, _ := parseFakePath(testModelPath)
			meta := ensureMap(mc.f.objects[k], "metadata")
			sent = meta["resourceVersion"].(string)
			mc.f.bumpRV(meta)
			mc.f.mu.Unlock()
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if sent == "" || res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("unpublish over a concurrent change = %+v (patched: %v)", res, sent != "")
	}
}

// Finding 11: a model left only with the MaaS gateway ref (no reference, no
// record) shows "Partly published", and Unpublish then takes the ref off.
func TestMaaSUnpublish_RemovesALeftOverGatewayRef(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, tMaaSRef)
	if err := writeMaaSRecord(mc.c, maasRecord{DSCChanged: true, PreviousModelsAsAService: "Removed"}); err != nil {
		t.Fatal(err)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || !strings.Contains(st.Items[0].Message, "Partly published") {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if gw := modelGateway(t, mc); len(gw) != 0 {
		t.Fatalf("model gateway after unpublish = %v", gw)
	}
}

// Finding 12: an unpublish that could not update the gateway says so.
func TestMaaSUnpublish_SaysWhenTheGatewayWasNotUpdated(t *testing.T) {
	mc := publishedMaaSCluster(t)
	mc.f.fail["GET "+tMaaSClass] = 500
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	all := res.Message + " " + strings.Join(res.Logs, " ")
	if strings.Contains(all, "no longer admits") || !strings.Contains(all, "not updated") {
		t.Fatalf("unpublish = %+v", res)
	}
}

// Finding 2: a recorded project whose model and reference are gone is not
// put back into the shared subscription and access policy.
func TestMaaSPublish_LeavesStaleRecordsOutOfTheSubscription(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	rec, _ := readMaaSRecord(mc.c)
	rec.Published = []string{"gone"}
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	for _, path := range []string{tMaaSSub, tMaaSAuth} {
		body, _ := json.Marshal(mc.f.get(path)["spec"])
		if strings.Contains(string(body), `"gone"`) || !strings.Contains(string(body), `"proj"`) {
			t.Fatalf("%s spec = %s", path, body)
		}
	}
}

// Finding 2: a recorded publication whose model is gone still has a row, so
// Unpublish can clear it.
func TestMaaSStatus_ShowsARecordedPublicationWithoutItsModel(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	rec, _ := readMaaSRecord(mc.c)
	rec.Published = []string{"gone"}
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || st.Items[0].Namespace != "gone" || !st.Items[0].Published || !st.Items[0].Partly {
		t.Fatalf("status items = %+v, %v", st.Items, err)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "gone"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Items) != 0 {
		t.Fatalf("status items after unpublish = %+v", st.Items)
	}
}

// Finding 2: the Models teardown refuses while the record or the model's
// gateway ref still says published, not only while the reference exists.
func TestTeardownModels_RefusesWhileAPublicationIsLeft(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, tMaaSRef)
	if res, _ := TeardownModels(mc.c, "proj"); res.Success || res.ErrorCode != "in_use" {
		t.Fatalf("model teardown with a recorded publication: %+v", res)
	}
	if !mc.f.has(testModelPath) {
		t.Fatal("the model was deleted")
	}
}

// Finding 1: removal refuses, before changing anything, while MaaS objects
// someone else made exist: removing the platform would break them.
func TestMaaSTeardown_RefusesWhileOthersUseMaaS(t *testing.T) {
	for name, path := range map[string]string{
		"model reference": "/apis/maas.opendatahub.io/v1alpha1/namespaces/team/maasmodelrefs/their-model",
		"subscription":    "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/team-sub",
		"access policy":   "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maasauthpolicies/team-access",
	} {
		t.Run(name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			mc.f.putJSON(path, `{"spec":{}}`)
			mc.f.requests = nil
			res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
			if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, path[strings.LastIndex(path, "/")+1:]) {
				t.Fatalf("teardown = %+v", res)
			}
			if muts := mc.f.mutations(); len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// Finding 1: the gateway is not deleted while routes are attached to it.
func TestMaaSTeardown_KeepsTheGatewayWhileRoutesAreAttached(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		prev(k, obj)
		if k.plural == "gateways" && k.name == maasGatewayName {
			obj["status"] = map[string]interface{}{"listeners": []interface{}{map[string]interface{}{"name": "https", "attachedRoutes": 1}}}
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if res.Success || res.ErrorCode != "in_use" || !mc.f.has(tMaaSGateway) || !mc.f.has(tMaaSDB) {
		t.Fatalf("teardown = %+v (gateway kept: %v)", res, mc.f.has(tMaaSGateway))
	}
}

// Finding 8: removal restores modelsAsAService only while the tool still
// owns that field; someone who set it since keeps their value.
func TestMaaSTeardown_KeepsADSCValueSomeoneElseSetSinceSetup(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.mu.Lock()
	k, _ := parseFakePath(tMaaSDSC)
	meta := ensureMap(mc.f.objects[k], "metadata")
	meta["managedFields"] = []interface{}{map[string]interface{}{"manager": "kubectl-edit", "operation": "Update",
		"fieldsV1": map[string]interface{}{"f:spec": map[string]interface{}{"f:components": map[string]interface{}{"f:aigateway": map[string]interface{}{"f:modelsAsAService": map[string]interface{}{"f:managementState": map[string]interface{}{}}}}}}}}
	mc.f.mu.Unlock()
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	dsc, _ := json.Marshal(mc.f.get(tMaaSDSC))
	if res.Success || res.ErrorCode != "in_use" || !strings.Contains(string(dsc), `"modelsAsAService":{"managementState":"Managed"}`) ||
		!strings.Contains(res.Message, "oc patch datasciencecluster default-dsc") || !strings.Contains(res.Message, "then Remove again") {
		t.Fatalf("teardown = %+v; dsc = %s", res, dsc)
	}
	// Following the message: the user turns MaaS off, and removal finishes.
	mc.f.mu.Lock()
	k, _ = parseFakePath(tMaaSDSC)
	ensureMap(ensureMap(ensureMap(mc.f.objects[k], "spec"), "components"), "aigateway")["modelsAsAService"] = map[string]interface{}{"managementState": "Removed"}
	mc.f.mu.Unlock()
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"}); !res.Success || mc.f.has(tMaaSGateway) || mc.f.has(tMaaSDB) {
		t.Fatalf("teardown after turning MaaS off = %+v", res)
	}
	if rec, _ := readMaaSRecord(mc.c); rec.DSCChanged {
		t.Fatalf("record = %+v", rec)
	}
}

// Finding 14: the MaaS and Authorino objects are read at the versions the
// cluster serves.
func TestMaaS_UsesTheServedAPIVersions(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.groups["/apis/maas.opendatahub.io"] = `{"kind":"APIGroup","name":"maas.opendatahub.io","versions":[{"groupVersion":"maas.opendatahub.io/v1","version":"v1"},{"groupVersion":"maas.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"maas.opendatahub.io/v1","version":"v1"}}`
	mc.f.groups["/apis/operator.authorino.kuadrant.io"] = `{"kind":"APIGroup","name":"operator.authorino.kuadrant.io","versions":[{"groupVersion":"operator.authorino.kuadrant.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"operator.authorino.kuadrant.io/v1","version":"v1"}}`
	if got := maasAPI(mc.c); got != "maas.opendatahub.io/v1" {
		t.Fatalf("maasAPI = %s", got)
	}
	deleteFakeObject(mc, "/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino")
	mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{"listener":{"tls":{"enabled":true,"certSecretRef":{"name":"authorino-server-cert"}}}}}`)
	if missing, known, _ := authorinoTLS(mc.c, "kuadrant-system"); missing != "" || !known {
		t.Fatalf("authorinoTLS = %q, %v", missing, known)
	}
}

// Finding 9: with maas-controller gone (MaaS turned off by someone else),
// unpublish takes the finalizer off the tool's own terminating reference.
func TestMaaSUnpublish_FinishesWhenMaaSControllerIsGone(t *testing.T) {
	mc := publishedMaaSCluster(t)
	mc.f.mu.Lock()
	k, _ := parseFakePath(tMaaSRef)
	ensureMap(mc.f.objects[k], "metadata")["finalizers"] = []interface{}{"maas.opendatahub.io/model-cleanup"}
	dk, _ := parseFakePath(tMaaSDSC)
	ensureMap(ensureMap(ensureMap(mc.f.objects[dk], "spec"), "components"), "aigateway")["modelsAsAService"] = map[string]interface{}{"managementState": "Removed"}
	mc.f.mu.Unlock()
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success || mc.f.has(tMaaSRef) || !strings.Contains(strings.Join(res.Logs, " "), "maas-controller") {
		t.Fatalf("unpublish = %+v (ref left: %v)", res, mc.f.has(tMaaSRef))
	}
}

// Finding 6: removal deletes the database where setup made it, also when
// the tenant reports another infrastructure namespace since.
func TestMaaSTeardown_UsesTheRecordedDatabaseNamespace(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if rec, _ := readMaaSRecord(mc.c); rec.DBNamespace != "redhat-ai-gateway-infra" {
		t.Fatalf("record = %+v", rec)
	}
	mc.f.beforeServe = nil
	mc.f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maastenantconfigs/default-tenant", `{"status":{"infraNamespace":"elsewhere"}}`)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"}); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if mc.f.has(tMaaSDB) || mc.f.has(tMaaSDBConfig) {
		t.Fatal("the database setup made was left")
	}
}

// Finding 7: a connection Secret written while MaaS is already Managed
// restarts maas-api (maas-setup.md "Restarting maas-api"); the first setup
// (MaaS turned on after the Secret) does not.
func TestMaaSSetup_RestartsMaaSAPIWhenTheSecretChangesUnderARunningMaaS(t *testing.T) {
	const api = "PATCH /apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/maas-api"
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success || hasMutation(mc.f, api) {
		t.Fatalf("first setup: %+v", res)
	}
	deleteFakeObject(mc, tMaaSDBConfig)
	mc.f.requests = nil
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("second setup: %+v", res)
	}
	if !hasMutation(mc.f, api) {
		t.Fatalf("maas-api not restarted: %v", mc.f.mutations())
	}
	tmpl, _ := json.Marshal(mc.f.get("/apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/maas-api")["spec"])
	if !strings.Contains(string(tmpl), restartAnnotation) {
		t.Fatalf("maas-api spec = %s", tmpl)
	}
}

// Finding 5: a listener that selects namespaces with expressions is
// evaluated (kubernetes.io/metadata.name is on every namespace).
func TestGatewayAdmits_EvaluatesMatchExpressions(t *testing.T) {
	gw := func(exprs string) gatewayObject {
		var g gatewayObject
		_ = json.Unmarshal([]byte(`{"metadata":{"namespace":"openshift-ingress"},"spec":{"listeners":[{"name":"https","allowedRoutes":{"namespaces":{"from":"Selector","selector":{"matchExpressions":`+exprs+`}}}}]}}`), &g)
		return g
	}
	for _, tc := range []struct {
		exprs, ns string
		labels    map[string]string
		want      string
	}{
		{`[{"key":"kubernetes.io/metadata.name","operator":"In","values":["proj","b"]}]`, "proj", nil, "yes"},
		{`[{"key":"kubernetes.io/metadata.name","operator":"In","values":["a","b"]}]`, "proj", nil, "no"},
		{`[{"key":"tier","operator":"NotIn","values":["prod"]}]`, "proj", map[string]string{"tier": "dev"}, "yes"},
		{`[{"key":"tier","operator":"NotIn","values":["prod"]}]`, "proj", map[string]string{"tier": "prod"}, "no"},
		{`[{"key":"maas.opendatahub.io/gateway-access","operator":"Exists"}]`, "proj", map[string]string{"maas.opendatahub.io/gateway-access": "true"}, "yes"},
		{`[{"key":"maas.opendatahub.io/gateway-access","operator":"Exists"}]`, "proj", nil, "no"},
		{`[{"key":"tier","operator":"DoesNotExist"}]`, "proj", nil, "yes"},
		{`[{"key":"tier","operator":"Gt","values":["1"]}]`, "proj", nil, "unknown"},
	} {
		if got := gw(tc.exprs).admits(tc.ns, tc.labels); got != tc.want {
			t.Errorf("%s for %s %v: %s, want %s", tc.exprs, tc.ns, tc.labels, got, tc.want)
		}
	}
}

// Finding 4: Authorino's TLS Secret is the one its CR names, and a check
// that cannot read Authorino does not block publishing.
func TestMaaSStatus_AuthorinoTLSFollowsTheCRsSecret(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization", `{"metadata":{"annotations":{"service.beta.openshift.io/serving-cert-secret-name":"aigw-authorino-tls"}}}`)
	mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{"listener":{"tls":{"enabled":true,"certSecretRef":{"name":"aigw-authorino-tls"}}}}}`)
	st, _ := GetMaaSStatus(mc.c)
	if !st.Platform.Ready {
		t.Fatalf("platform = %+v; parts = %+v", st.Platform, st.Parts)
	}
	mc.f.putJSON("/api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization", `{"metadata":{}}`)
	st, _ = GetMaaSStatus(mc.c)
	for _, p := range st.Parts {
		if p.ID == "authorino-tls" && (p.State != "missing" || len(p.Actions) != 1 || !strings.Contains(p.Commands, "serving-cert-secret-name=aigw-authorino-tls")) {
			t.Fatalf("part = %+v", p)
		}
	}
	mc.f.fail["GET /apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino"] = 500
	if st, _ = GetMaaSStatus(mc.c); !st.Platform.Ready {
		t.Fatalf("an unreadable Authorino blocks the platform: %+v", st.Platform)
	}
}
