package cluster

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// F6 (live, 2026-11-23): a MaaSModelRef deleted outside the tool while the
// model still routes through maas-default-gateway (publish routes it there
// and takes the genai label off; only Unpublish reverts that). The prune
// drops the model from the subscription and access policy (which MaaS
// otherwise marks Failed, and every other published model with them) and
// from the gateway's projects; the record keeps only what restoring the
// model's routing writes back. The row comes from the live state, with
// "Restore direct routing".

const routingPendingText = "No MaaS reference; the model still routes through the MaaS gateway, so its direct address does not answer."

// maasPhasesFollowRefs wires the fake subscription and access policy phases to
// match maas-controller in production: both report Failed when any modelRefs entry
// points at a MaaSModelRef that is absent from the cluster.
func maasPhasesFollowRefs(mc *modelsCluster) {
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if k.plural != "maassubscriptions" && k.plural != "maasauthpolicies" {
			return
		}
		spec, _ := obj["spec"].(map[string]any)
		refs, _ := spec["modelRefs"].([]any)
		for _, r := range refs {
			ref, _ := r.(map[string]any)
			ns, _ := ref["namespace"].(string)
			name, _ := ref["name"].(string)
			if mc.f.objects[fakeKey{gv: "maas.opendatahub.io/v1alpha1", ns: ns, plural: "maasmodelrefs", name: name}] == nil {
				obj["status"] = map[string]any{"phase": "Failed"}
				return
			}
		}
	}
}

// requirePublishedAsLive checks that the fake publish left the model as it
// is live: on maas-default-gateway alone, without the genai label and
// annotation (the unit tests that hid F6 did not start from this state).
func requirePublishedAsLive(t *testing.T, mc *modelsCluster) {
	t.Helper()
	refs, _ := json.Marshal(modelGateway(t, mc)["refs"])
	if string(refs) != `[{"name":"maas-default-gateway","namespace":"openshift-ingress"}]` {
		t.Fatalf("published model refs = %s", refs)
	}
	labels, ann := modelMetaOf(mc)
	if _, ok := labels["opendatahub.io/genai-asset"]; ok {
		t.Fatalf("published model kept the genai label: %v", labels)
	}
	if _, ok := ann["opendatahub.io/genai-use-case"]; ok {
		t.Fatalf("published model kept the genai annotation: %v", ann)
	}
}

func rowOf(st *types.MaaSStatus, name string) (types.MaaSModel, bool) {
	for _, it := range st.Items {
		if it.Namespace == "proj" && it.Name == name {
			return it, true
		}
	}
	return types.MaaSModel{}, false
}

func TestMaaS_ARefDeletedOutsideWhileTheModelRoutesThroughMaaS(t *testing.T) {
	mc := maasWithModels(t, tModel, "smollm2-135m-instruct")
	maasPhasesFollowRefs(mc)
	before := modelGateway(t, mc)["refs"]
	for _, name := range []string{"smollm2-135m-instruct", tModel} {
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: name}); !res.Success {
			t.Fatalf("publish %s: %+v", name, res)
		}
	}
	requirePublishedAsLive(t, mc)
	deleteFakeObject(mc, tMaaSRef)

	// Pre-fix live F6 behavior: the companion model fails alongside it.
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if it, _ := rowOf(st, "smollm2-135m-instruct"); it.Subscription != "Failed" {
		t.Fatalf("the fake does not mirror maas-controller: %+v", it)
	}
	if it, ok := rowOf(st, tModel); !ok || !it.RoutingPending || it.Published || it.Partly || it.Removing || it.Message != routingPendingText {
		t.Fatalf("row before the prune = %+v (found %t)", it, ok)
	}

	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	rec, _ := serving.ReadMaaSRecord(mc.c)
	key := "proj/" + tModel
	if strings.Join(rec.Published, " ") != "proj/smollm2-135m-instruct" {
		t.Fatalf("published = %v", rec.Published)
	}
	if _, ok := rec.ModelRefs[key]; !ok {
		t.Fatalf("the prune dropped the refs to write back: %+v", rec.ModelRefs)
	}
	if _, ok := rec.GenAI[key]; !ok {
		t.Fatalf("the prune dropped the Gen AI values to write back: %+v", rec.GenAI)
	}
	if rec.ModelUIDs[key] == "" {
		t.Fatalf("the prune dropped the model's UID: %+v", rec.ModelUIDs)
	}
	for _, path := range []string{tMaaSSub, tMaaSAuth} {
		spec, _ := json.Marshal(mc.f.get(path)["spec"])
		if strings.Contains(string(spec), tModel) || !strings.Contains(string(spec), "smollm2-135m-instruct") {
			t.Fatalf("%s spec = %s", path, spec)
		}
	}

	st, _ = GetMaaSStatus(mc.c)
	if it, _ := rowOf(st, "smollm2-135m-instruct"); it.Subscription != "Active" || it.Access != "Active" || !it.Ready || it.Message != "Published" {
		t.Fatalf("the other published model = %+v", it)
	}
	it, ok := rowOf(st, tModel)
	if !ok || !it.RoutingPending || it.Published || it.Partly || it.Removing || it.Message != routingPendingText {
		t.Fatalf("row after the prune = %+v (found %t)", it, ok)
	}
	if len(st.Removing) != 0 {
		t.Fatalf("removing = %+v", st.Removing)
	}
	// Another background prune pass: stored values remain until restore runs.
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); rec.ModelUIDs[key] == "" || len(rec.ModelRefs) != 2 || len(rec.GenAI) != 2 {
		t.Fatalf("a second prune changed the values: %+v", rec)
	}

	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "restore-routing", Project: "proj", Model: tModel})
	if !res.Success {
		t.Fatalf("restore: %+v", res)
	}
	if after := modelGateway(t, mc)["refs"]; !reflect.DeepEqual(after, before) {
		t.Fatalf("refs after the restore = %v, before publishing %v", after, before)
	}
	labels, ann := modelMetaOf(mc)
	if labels["opendatahub.io/genai-asset"] != "true" || ann["opendatahub.io/genai-use-case"] != "Chat" {
		t.Fatalf("Gen AI metadata not restored: %v %v", labels, ann)
	}
	rec, _ = serving.ReadMaaSRecord(mc.c)
	_, refsKept := rec.ModelRefs[key]
	_, genAIKept := rec.GenAI[key]
	if refsKept || genAIKept || rec.ModelUIDs[key] != "" {
		t.Fatalf("record after the restore = %+v", rec)
	}
	st, _ = GetMaaSStatus(mc.c)
	if it, _ := rowOf(st, tModel); it.RoutingPending || it.Published || it.Message != "Not published" {
		t.Fatalf("row after the restore = %+v", it)
	}
	if it, _ := rowOf(st, "smollm2-135m-instruct"); !it.Ready || it.Subscription != "Active" {
		t.Fatalf("the other published model after the restore = %+v", it)
	}
}

// When this project has no other publications left, prune also removes the
// project from maas-default-gateway and tears down the now-empty subscription
// and access policy (existing rule: admission only for projects with a live
// publication).
func TestMaaS_ARefDeletedOutsideLeavesTheGatewayAndAccess(t *testing.T) {
	mc := publishedMaaSCluster(t)
	requirePublishedAsLive(t, mc)
	deleteFakeObject(mc, tMaaSRef)
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the gateway still admits proj")
	}
	if mc.f.has(tMaaSSub) || mc.f.has(tMaaSAuth) {
		t.Fatal("the empty subscription or access policy is left")
	}
	rec, _ := serving.ReadMaaSRecord(mc.c)
	if len(rec.Published) != 0 || rec.ModelUIDs["proj/"+tModel] == "" {
		t.Fatalf("record = %+v", rec)
	}
	// Routing still flows through the MaaS gateway on the model; the prune
	// job (ServiceAccount) does not rewrite that.
	requirePublishedAsLive(t, mc)
}

// Fail-closed: publication is left unchanged while either the MaaSModelRef or
// the model itself cannot be fetched.
func TestMaaS_ARefDeletedOutsideIsKeptWhileItCannotBeRead(t *testing.T) {
	for _, path := range []string{tMaaSRef, testModelPath} {
		mc := publishedMaaSCluster(t)
		deleteFakeObject(mc, tMaaSRef)
		mc.f.fail["GET "+path] = 500
		_, _, _ = serving.PruneGoneMaaSPublications(mc.c)
		delete(mc.f.fail, "GET "+path)
		if rec, _ := serving.ReadMaaSRecord(mc.c); !rec.IsPublished("proj", tModel) {
			t.Fatalf("GET %s failing: the publication was dropped", path)
		}
	}
}

// Teardown from the Models tab works here: aside from the model's own routing
// (which deletion carries away), no publication artifacts remain; the following
// prune clears the values that were held for restore.
func TestModelsTeardown_AModelWhoseRefWasDeletedOutside(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, tMaaSRef)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: tModel})
	if !res.Success {
		t.Fatalf("teardown = %+v", res)
	}
	if mc.f.has(testModelPath) {
		t.Fatal("the model is still there")
	}
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); len(rec.Published)+len(rec.ModelRefs)+len(rec.GenAI)+len(rec.ModelUIDs) != 0 {
		t.Fatalf("record after the model went = %+v", rec)
	}
}

// Clean up MaaS deletes the gateway the model still routes through: it
// restores the model's routing first, as it unpublishes the others.
func TestMaaSTeardown_RestoresAModelWhoseRefWasDeletedOutside(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, tMaaSRef)
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"}); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if m, _, _ := serving.ReadToolModel(mc.c, "proj", tModel); serving.ModelHasMaaSRef(m) {
		t.Fatal("the model still routes through the deleted MaaS gateway")
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); len(rec.ModelRefs)+len(rec.GenAI)+len(rec.ModelUIDs) != 0 {
		t.Fatalf("record after the teardown = %+v", rec)
	}
}

// F2: during project deletion the status row reports removal regardless of
// record contents—prune clears the record within about a second of the first
// status read, and a live MaaSModelRef (Pending, deleted with the project) must
// not flip the row back to "Published; waiting" with Unpublish and Change limit.
func TestMaaSStatus_AModelInAProjectBeingDeletedIsBeingRemovedWithIt(t *testing.T) {
	mc := publishedMaaSCluster(t)
	mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"},"deletionTimestamp":"2026-11-23T05:40:20Z"}}`)
	for _, when := range []string{"before the prune", "after the prune"} {
		st, err := GetMaaSStatus(mc.c)
		if err != nil {
			t.Fatal(err)
		}
		it, ok := maasItem(st)
		if !ok || !it.Removing || it.Published || it.Partly || it.RoutingPending || it.Limit != nil || it.Message != "Being removed with its project" {
			t.Fatalf("%s: row = %+v (found %t)", when, it, ok)
		}
		if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
			t.Fatal(err)
		}
		if rec, _ := serving.ReadMaaSRecord(mc.c); rec.IsPublished("proj", tModel) {
			t.Fatal("the prune kept the publication of a project being deleted")
		}
		if !mc.f.has(tMaaSRef) {
			t.Fatal("the fake lost the live MaaSModelRef")
		}
	}
}
