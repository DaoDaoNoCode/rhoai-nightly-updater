package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Models catalog meets the gateway-only publication: every chat model
// of a project is routed through the MaaS gateway alone on its own, with
// its own route and its own recorded refs.

const tModelsPath = "/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/"

func setModelRefsOf(mc *modelsCluster, name string, refs []any) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	k, _ := parseFakePath(tModelsPath + name)
	router := ensureMap(ensureMap(mc.f.objects[k], "spec"), "router")
	if refs == nil {
		router["gateway"] = map[string]any{}
		return
	}
	router["gateway"] = map[string]any{"refs": refs}
}

func modelRefsOf(mc *modelsCluster, name string) string {
	spec, _ := mc.f.get(tModelsPath + name)["spec"].(map[string]any)
	router, _ := spec["router"].(map[string]any)
	gw, _ := router["gateway"].(map[string]any)
	raw, _ := json.Marshal(gw["refs"])
	return string(raw)
}

// routeParentsOf reads the model's route through the API (so the simulated
// KServe reconciles it) and returns its parents' names.
func routeParentsOf(t *testing.T, mc *modelsCluster, name string) string {
	t.Helper()
	body, _, err := mc.c.Get("/apis/gateway.networking.k8s.io/v1/namespaces/proj/httproutes/" + name + "-kserve-route")
	if err != nil {
		t.Fatalf("read the route of %s: %v", name, err)
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

// KServe names a model's route <model>-kserve-route; the tool reads the
// routes of the three chat models (the ones it publishes), never the
// embedding model's.
func TestModelRoutePath_IsEachChatModelsOwnRoute(t *testing.T) {
	for _, m := range modelCatalog {
		path, ok := modelRoutePath("proj", m.Name)
		if m.Type != modelTypeChat {
			if ok {
				t.Errorf("%s (not published) has a route path %s", m.Name, path)
			}
			continue
		}
		if want := "/apis/gateway.networking.k8s.io/v1/namespaces/proj/httproutes/" + m.Name + "-kserve-route"; !ok || path != want {
			t.Errorf("%s: route path %q, %v; want %q", m.Name, path, ok, want)
		}
	}
}

// Two models of one project, published one after the other: each has its
// own refs recorded, routes through the MaaS gateway alone on its own
// route, and Unpublish of one restores only its refs; the other stays
// published with its record.
func TestMaaSPublish_TwoModelsOfAProjectKeepTheirOwnRefs(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
	setModelRefsOf(mc, "smollm2-135m-instruct", []any{tTeamGatewayRef})
	for _, name := range []string{"smollm2-135m-instruct", "qwen25-05b-instruct"} {
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: name}); !res.Success {
			t.Fatalf("publish %s: %+v", name, res)
		}
		if got := modelRefsOf(mc, name); got != tMaaSOnly {
			t.Fatalf("%s refs = %s", name, got)
		}
		if got := routeParentsOf(t, mc, name); got != "maas-default-gateway" {
			t.Fatalf("%s route = %s", name, got)
		}
	}
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"modelRefs":{"proj/qwen25-05b-instruct":{},"proj/smollm2-135m-instruct":{"refs":[{"name":"team-gateway","namespace":"team"}]}}`) {
		t.Fatalf("record = %s", rec)
	}

	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("unpublish smollm2: %+v", res)
	}
	if got := modelRefsOf(mc, "smollm2-135m-instruct"); got != `[{"name":"team-gateway","namespace":"team"}]` {
		t.Fatalf("smollm2 refs after unpublish = %s", got)
	}
	if got := routeParentsOf(t, mc, "smollm2-135m-instruct"); got != "team-gateway" {
		t.Fatalf("smollm2 route after unpublish = %s", got)
	}
	if got := modelRefsOf(mc, "qwen25-05b-instruct"); got != tMaaSOnly {
		t.Fatalf("qwen25 refs after unpublishing smollm2 = %s", got)
	}
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"modelRefs":{"proj/qwen25-05b-instruct":{}}`) {
		t.Fatalf("record after unpublishing smollm2 = %s", rec)
	}
	st, _ := GetMaaSStatus(mc.c)
	for _, it := range st.Items {
		if it.Partly || it.Published != (it.Name == "qwen25-05b-instruct") {
			t.Fatalf("status after unpublishing smollm2 = %+v", st.Items)
		}
	}

	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
		t.Fatalf("unpublish qwen25: %+v", res)
	}
	if got := modelRefsOf(mc, "qwen25-05b-instruct"); got != "null" {
		t.Fatalf("qwen25 refs after unpublish = %s", got)
	}
	if rec := rawMaaSRecord(mc); strings.Contains(rec, "modelRefs") {
		t.Fatalf("record after unpublishing both = %s", rec)
	}
}

// A gateway someone added to one published model refuses Unpublish of that
// model only; the other model of the project unpublishes.
func TestMaaSUnpublish_SomeoneAddedAGatewayToOneModelOnly(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "qwen3-1-7b")
	for _, name := range []string{"qwen25-05b-instruct", "qwen3-1-7b"} {
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: name}); !res.Success {
			t.Fatalf("publish %s: %+v", name, res)
		}
	}
	setModelRefsOf(mc, "qwen3-1-7b", []any{tMaaSGatewayRef, tTeamGatewayRef})
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "qwen3-1-7b"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "oc patch llminferenceservice qwen3-1-7b -n proj") {
		t.Fatalf("unpublish qwen3 = %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
		t.Fatalf("unpublish qwen25: %+v", res)
	}
	if got := modelRefsOf(mc, "qwen25-05b-instruct"); got != "null" {
		t.Fatalf("qwen25 refs = %s", got)
	}
}

// A record written before models were named (published and modelRefs by
// project only) is qwen25's: its refs are restored by Unpublish of qwen25,
// and no other model of the project takes them.
func TestMaaSRecord_AProjectOnlyEntryIsTheDefaultModels(t *testing.T) {
	mc := publishedMaaSCluster(t)
	rec, err := readMaaSRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	rec.Published = []string{"proj"}
	rec.ModelRefs = map[string]maasModelRefs{"proj": {Refs: gatewayRefs{{"name": "team-gateway", "namespace": "team"}}}}
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	if rec, _ = readMaaSRecord(mc.c); strings.Join(rec.Published, " ") != "proj/qwen25-05b-instruct" {
		t.Fatalf("published = %v", rec.Published)
	}
	if refs, ok := rec.modelRefs("proj", "qwen25-05b-instruct"); !ok || refs.Refs.String() != "team/team-gateway" {
		t.Fatalf("qwen25 refs = %v, %v", refs, ok)
	}
	if _, ok := rec.modelRefs("proj", "smollm2-135m-instruct"); ok {
		t.Fatal("smollm2 took the project's refs")
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Items) != 1 || st.Items[0].Partly || st.Items[0].Message != "Published" {
		t.Fatalf("status = %+v", st.Items)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if got := modelRefsOf(mc, "qwen25-05b-instruct"); got != `[{"name":"team-gateway","namespace":"team"}]` {
		t.Fatalf("refs after unpublish = %s", got)
	}
	if raw := rawMaaSRecord(mc); strings.Contains(raw, "modelRefs") || strings.Contains(raw, `"published"`) {
		t.Fatalf("record after unpublish = %s", raw)
	}
}
