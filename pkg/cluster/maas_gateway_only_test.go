package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A published model routes through the MaaS gateway only (live, RHOAI 3.6,
// 2026-11-18): maas-controller's TokenRateLimitPolicy targets the model's
// HTTPRoute, Kuadrant enforces it on every gateway the route names, and on
// openshift-ai-inference the identity has no selected_subscription_id, so
// every direct call answered 500 while the route also named that gateway.

const (
	tModelRoute = "/apis/gateway.networking.k8s.io/v1/namespaces/proj/httproutes/qwen25-05b-instruct-kserve-route"
	tMaaSOnly   = `[{"name":"maas-default-gateway","namespace":"openshift-ingress"}]`
)

var (
	tMaaSGatewayRef      = map[string]interface{}{"name": "maas-default-gateway", "namespace": "openshift-ingress"}
	tInferenceGatewayRef = map[string]interface{}{"name": "openshift-ai-inference", "namespace": "openshift-ingress"}
	tTeamGatewayRef      = map[string]interface{}{"name": "team-gateway", "namespace": "team"}
)

// kserveRouteSim stands in for KServe's LLMInferenceService router
// (opendatahub kserve rhoai-3.6 router.go expectedHTTPRoute): a model's
// HTTPRoute <name>-kserve-route names the gateways of
// spec.router.gateway.refs, or KServe's default gateway
// (openshift-ai-inference) when there are none, and KServe rewrites an
// existing route only when those parents are not a prefix of the route's
// (semanticHTTPRouteIsEqual: equality.Semantic.DeepDerivative), so a parent
// dropped from the end stays. It reconciles whenever the route is read;
// frozen stops every rewrite (KServe not reconciling).
type kserveRouteSim struct {
	mu     sync.Mutex
	frozen bool
}

func (s *kserveRouteSim) freeze(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = on
}

func simulateKServeRoute(mc *modelsCluster) *kserveRouteSim {
	sim := &kserveRouteSim{}
	f := mc.f
	prev := f.beforeServe
	f.beforeServe = func(method, path string) {
		if prev != nil {
			prev(method, path)
		}
		if method != http.MethodGet || !strings.Contains(path, "/httproutes/") || !strings.HasSuffix(path, "-kserve-route") {
			return
		}
		sim.mu.Lock()
		frozen := sim.frozen
		sim.mu.Unlock()
		f.mu.Lock()
		defer f.mu.Unlock()
		rk, _ := parseFakePath(path)
		mk := fakeKey{gv: "serving.kserve.io/v1alpha2", ns: rk.ns, plural: "llminferenceservices", name: strings.TrimSuffix(rk.name, "-kserve-route")}
		model := f.objects[mk]
		if model == nil {
			delete(f.objects, rk)
			return
		}
		spec, _ := model["spec"].(map[string]interface{})
		router, _ := spec["router"].(map[string]interface{})
		gw, _ := router["gateway"].(map[string]interface{})
		refs, _ := gw["refs"].([]interface{})
		var want []interface{}
		for _, r := range refs {
			m, _ := r.(map[string]interface{})
			want = append(want, map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": m["name"], "namespace": m["namespace"]})
		}
		if len(want) == 0 {
			want = []interface{}{map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "openshift-ai-inference", "namespace": "openshift-ingress"}}
		}
		route := f.objects[rk]
		if route == nil {
			f.objects[rk] = map[string]interface{}{
				"metadata": map[string]interface{}{"name": rk.name, "namespace": rk.ns, "uid": "route-uid", "resourceVersion": "1"},
				"spec":     map[string]interface{}{"parentRefs": want},
			}
			return
		}
		have, _ := ensureMap(route, "spec")["parentRefs"].([]interface{})
		isPrefix := len(want) <= len(have)
		for i := 0; isPrefix && i < len(want); i++ {
			a, _ := json.Marshal(want[i])
			b, _ := json.Marshal(have[i])
			isPrefix = string(a) == string(b)
		}
		if !isPrefix && !frozen {
			ensureMap(route, "spec")["parentRefs"] = want
		}
	}
	return sim
}

// routeParents reads the model's route through the API (so the simulated
// KServe reconciles it) and returns its parents' names.
func routeParents(t *testing.T, mc *modelsCluster) string {
	t.Helper()
	body, _, err := mc.c.get(tModelRoute)
	if err != nil {
		t.Fatalf("read the model's route: %v", err)
	}
	var route struct {
		Spec struct {
			ParentRefs []struct {
				Name string `json:"name"`
			} `json:"parentRefs"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(body, &route)
	var names []string
	for _, p := range route.Spec.ParentRefs {
		names = append(names, p.Name)
	}
	return strings.Join(names, ",")
}

// modelReadyForMaaS is newMaaSCluster with the platform set up and the
// tool's model in proj, not published.
func modelReadyForMaaS(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	return mc
}

func modelRefsJSON(t *testing.T, mc *modelsCluster) string {
	t.Helper()
	refs, _ := json.Marshal(modelGateway(t, mc)["refs"])
	return string(refs)
}

// rawMaaSRecord is the record as stored (snapshot ConfigMap, key "maas").
func rawMaaSRecord(mc *modelsCluster) string {
	cm := mc.f.get("/api/v1/namespaces/" + getActivityNamespace() + "/configmaps/" + snapshotConfigMapName)
	data, _ := cm["data"].(map[string]interface{})
	s, _ := data["maas"].(string)
	return s
}

// The simulation reproduces the live finding it stands for: from
// [maas, inference], refs [maas] leave the route on both gateways.
func TestKServeRouteSim_KeepsAParentDroppedFromTheEnd(t *testing.T) {
	mc := modelReadyForMaaS(t)
	if got := routeParents(t, mc); got != "openshift-ai-inference" {
		t.Fatalf("route on the default = %s", got)
	}
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tInferenceGatewayRef})
	if got := routeParents(t, mc); got != "maas-default-gateway,openshift-ai-inference" {
		t.Fatalf("route = %s", got)
	}
	setModelRefs(mc, []interface{}{tMaaSGatewayRef})
	if got := routeParents(t, mc); got != "maas-default-gateway,openshift-ai-inference" {
		t.Fatalf("route after dropping the last parent = %s", got)
	}
	setModelRefs(mc, nil)
	if got := routeParents(t, mc); got != "openshift-ai-inference" {
		t.Fatalf("route back on the default = %s", got)
	}
}

// Publish from the default refs: the model routes through the MaaS gateway
// only, its route follows, and the refs it had (none: KServe's default) are
// recorded for Unpublish.
func TestMaaSPublish_RoutesTheModelThroughTheMaaSGatewayOnly(t *testing.T) {
	mc := modelReadyForMaaS(t)
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	if got := finalSteps(events); got != "check=success gateway=success model=success maas=success ready=success" {
		t.Fatalf("steps = %s", got)
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs = %s, want %s", got, tMaaSOnly)
	}
	if got := routeParents(t, mc); got != "maas-default-gateway" {
		t.Fatalf("route = %s", got)
	}
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"modelRefs":{"proj/qwen25-05b-instruct":{}}`) {
		t.Fatalf("record = %s", rec)
	}
	logs := strings.Join(res.Logs, " ")
	if strings.Contains(logs, "keep working") || !strings.Contains(logs, "404 until it is unpublished") {
		t.Fatalf("logs = %s", logs)
	}
}

// A publication by an earlier build ([MaaS, inference]): KServe never drops
// the trailing parent, so publish first puts the original refs back, waits
// for the route to leave the MaaS gateway, then writes the MaaS gateway
// alone.
func TestMaaSPublish_FromAnEarlierBuildsPublicationGoesThroughTheDefaultFirst(t *testing.T) {
	mc := modelReadyForMaaS(t)
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tInferenceGatewayRef})
	routeParents(t, mc)
	if got := routeParents(t, mc); got != "maas-default-gateway,openshift-ai-inference" {
		t.Fatalf("legacy route = %s", got)
	}
	rec, _ := readMaaSRecord(mc.c)
	rec.Published = []string{"proj"}
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var writes []string
	prev := mc.f.onMergePatch
	mc.f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]interface{}) {
		if prev != nil {
			prev(r, k, patch, next)
		}
		if k.plural == "llminferenceservices" {
			spec, _ := patch["spec"].(map[string]interface{})
			router, _ := spec["router"].(map[string]interface{})
			gw, _ := router["gateway"].(map[string]interface{})
			raw, _ := json.Marshal(gw["refs"])
			mu.Lock()
			writes = append(writes, string(raw))
			mu.Unlock()
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	if got := strings.Join(writes, " "); got != "null "+tMaaSOnly {
		t.Fatalf("model refs writes = %s", got)
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs = %s", got)
	}
	if got := routeParents(t, mc); got != "maas-default-gateway" {
		t.Fatalf("route = %s", got)
	}
	// Unpublish then goes back to the default.
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if gw := modelGateway(t, mc); len(gw) != 0 {
		t.Fatalf("model gateway after unpublish = %v", gw)
	}
}

// Unpublish writes back the refs publish recorded, here someone else's
// gateway, and the route follows.
func TestMaaSUnpublish_RestoresTheRecordedRefs(t *testing.T) {
	mc := modelReadyForMaaS(t)
	setModelRefs(mc, []interface{}{tTeamGatewayRef})
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs after publish = %s", got)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if got, want := modelRefsJSON(t, mc), `[{"name":"team-gateway","namespace":"team"}]`; got != want {
		t.Fatalf("refs after unpublish = %s, want %s", got, want)
	}
	if got := routeParents(t, mc); got != "team-gateway" {
		t.Fatalf("route after unpublish = %s", got)
	}
	if rec := rawMaaSRecord(mc); strings.Contains(rec, "modelRefs") {
		t.Fatalf("record after unpublish = %s", rec)
	}
}

// A ref someone added to the model while it was published is not dropped:
// Unpublish refuses before changing anything and says what to do.
func TestMaaSUnpublish_RefusesWhenSomeoneAddedARefWhilePublished(t *testing.T) {
	mc := publishedMaaSCluster(t)
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tTeamGatewayRef})
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "team/team-gateway") ||
		!strings.Contains(res.Message, "oc patch llminferenceservice qwen25-05b-instruct -n proj") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("unpublish = %+v", res)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if !mc.f.has(tMaaSRef) {
		t.Fatal("the model reference was deleted")
	}
	// Following the message: the user sets the refs they want; Unpublish
	// then leaves them alone and finishes.
	setModelRefs(mc, []interface{}{tTeamGatewayRef})
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish after the fix: %+v", res)
	}
	if got := modelRefsJSON(t, mc); got != `[{"name":"team-gateway","namespace":"team"}]` {
		t.Fatalf("refs = %s", got)
	}
}

// Publish again over a gateway someone added while published refuses too:
// going through the recorded refs first would drop it.
func TestMaaSPublish_RefusesWhenSomeoneAddedARefWhilePublished(t *testing.T) {
	mc := publishedMaaSCluster(t)
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tTeamGatewayRef})
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "team/team-gateway") || !strings.Contains(res.Message, "Then Publish again") {
		t.Fatalf("publish = %+v", res)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
}

// KServe never moves the route to the MaaS gateway alone: publish stops
// before creating the MaaS objects, says "Partly published" and why, and
// the tab shows it.
func TestMaaSPublish_RouteThatNeverBecomesMaaSOnlyIsPartlyPublished(t *testing.T) {
	mc := modelReadyForMaaS(t)
	routeParents(t, mc)
	mc.route.freeze(true)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "Partly published") || !strings.Contains(res.Message, "openshift-ai-inference") || !strings.Contains(res.Message, "Unpublish") {
		t.Fatalf("publish = %+v", res)
	}
	if mc.f.has(tMaaSRef) {
		t.Fatal("the model reference was created over a route on two gateways")
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || !st.Items[0].Partly || !strings.Contains(st.Items[0].Message, "Partly published") {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
	mc.route.freeze(false)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if got := routeParents(t, mc); got != "openshift-ai-inference" {
		t.Fatalf("route after unpublish = %s", got)
	}
}

// KServe never moves the route off the MaaS gateway: unpublish says so and
// keeps the publication's record, so the tab offers Unpublish again, which
// then finishes.
func TestMaaSUnpublish_RouteThatNeverLeavesMaaSIsPartlyPublished(t *testing.T) {
	mc := publishedMaaSCluster(t)
	mc.route.freeze(true)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "Partly published") || !strings.Contains(res.Message, "maas-default-gateway") {
		t.Fatalf("unpublish = %+v", res)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || !st.Items[0].Partly || !strings.Contains(st.Items[0].Message, "route") {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
	mc.route.freeze(false)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish again: %+v", res)
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Items) != 1 || st.Items[0].Published {
		t.Fatalf("status after unpublish = %+v", st.Items)
	}
}

// A publication by an earlier build (no refs recorded) shows as partly
// published, with what to do: its direct address answers 500 while the
// route is on both gateways.
func TestMaaSStatus_AnEarlierBuildsPublicationIsPartlyPublished(t *testing.T) {
	mc := publishedMaaSCluster(t)
	rec, _ := readMaaSRecord(mc.c)
	rec.ModelRefs = nil
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tInferenceGatewayRef})
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 || !st.Items[0].Partly || !strings.Contains(st.Items[0].Message, "earlier build") ||
		!strings.Contains(st.Items[0].Message, "Unpublish, then Publish again") {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
}

// A gateway someone added to a model this build published (its refs are
// recorded) is not an earlier build's publication: the status says so and
// gives the remedy Unpublish and Publish give when they refuse
// (refsChangedRefusal), not "Unpublish, then Publish again", which they
// refuse (live, 2026-11-18). Right after the change the direct address
// still answers until Kuadrant applies the policy (about 30 s, live), so
// the 500 is "once the rate limit applies".
func TestMaaSStatus_SomeoneAddedAGatewayWhilePublished(t *testing.T) {
	mc := publishedMaaSCluster(t)
	setModelRefs(mc, []interface{}{tMaaSGatewayRef, tInferenceGatewayRef})
	st, err := GetMaaSStatus(mc.c)
	if err != nil || len(st.Items) != 1 {
		t.Fatalf("status = %+v, %v", st.Items, err)
	}
	it := st.Items[0]
	for _, want := range []string{"Partly published", "someone added openshift-ingress/openshift-ai-inference", "fail with 500 once the rate limit applies",
		"without maas-default-gateway", "oc patch llminferenceservice qwen25-05b-instruct -n proj", "then Unpublish"} {
		if !strings.Contains(it.Message, want) {
			t.Errorf("message lacks %q: %s", want, it.Message)
		}
	}
	if !it.Partly || it.Ready || strings.Contains(it.Message, "earlier build") || strings.Contains(it.Message, "Publish again") {
		t.Fatalf("status = %+v", it)
	}
	// Following it: Unpublish refuses with the same remedy, and after the
	// user sets the refs, finishes.
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "oc patch llminferenceservice qwen25-05b-instruct -n proj") {
		t.Fatalf("unpublish = %+v", res)
	}
	setModelRefs(mc, []interface{}{tInferenceGatewayRef})
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish after setting the refs: %+v", res)
	}
}

// The same for the second chat model of a project: the status reads that
// model's recorded refs, not the project's other model's.
func TestMaaSStatus_SomeoneAddedAGatewayToTheSecondModel(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	setModelRefsOf(mc, "smollm2-135m-instruct", []interface{}{tMaaSGatewayRef, tTeamGatewayRef})
	st, _ := GetMaaSStatus(mc.c)
	for _, it := range st.Items {
		if it.Name != "smollm2-135m-instruct" {
			continue
		}
		if !it.Partly || !strings.Contains(it.Message, "someone added team/team-gateway") || !strings.Contains(it.Message, "oc patch llminferenceservice smollm2-135m-instruct -n proj") {
			t.Fatalf("status = %+v", it)
		}
		return
	}
	t.Fatalf("no smollm2 row: %+v", st.Items)
}

// The Models tab knows the model is published, so "Use it" shows the MaaS
// commands instead of the direct address, which answers 404 meanwhile.
func TestModelState_SaysWhenTheModelRoutesThroughMaaS(t *testing.T) {
	mc := modelReadyForMaaS(t)
	model, _, _ := readToolModel(mc.c, "proj", "qwen25-05b-instruct")
	if raw, _ := json.Marshal(modelState(mc.c, model, false)); strings.Contains(string(raw), "maasPublished") {
		t.Fatalf("unpublished model state = %s", raw)
	}
	setModelRefs(mc, []interface{}{tMaaSGatewayRef})
	model, _, _ = readToolModel(mc.c, "proj", "qwen25-05b-instruct")
	if raw, _ := json.Marshal(modelState(mc.c, model, false)); !strings.Contains(string(raw), `"maasPublished":true`) {
		t.Fatalf("published model state = %s", raw)
	}
}
