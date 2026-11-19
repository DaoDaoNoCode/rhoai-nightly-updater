package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// publishedCluster is the MaaS cluster with this tool's model published
// from project proj.
func publishedCluster(t *testing.T) *modelsCluster {
	t.Helper()
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
	return mc
}

func gatewayAdmits(t *testing.T, mc *modelsCluster) bool {
	project := "proj"
	t.Helper()
	gw, _ := json.Marshal(mc.f.get(tMaaSGateway))
	return strings.Contains(string(gw), `"`+project+`"`)
}

// dropNamespace removes project's namespace object, as its deletion does
// (the rest of the project is left: the namespace decides).
func dropNamespace(mc *modelsCluster, project string) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	delete(mc.f.objects, fakeKey{gv: "v1", plural: "namespaces", name: project})
}

// A published project that is deleted leaves the MaaS gateway at the next
// locked operation (here a Clean up with nothing selected): anyone could
// create a project of that name and attach routes otherwise. The status
// names it until then. A namespace that cannot be read keeps it (fail
// closed: nothing is changed).
func TestMaaS_APublishedProjectThatIsGoneLeavesTheGateway(t *testing.T) {
	mc := publishedCluster(t)
	if !gatewayAdmits(t, mc) {
		t.Fatal("the gateway does not admit proj after publishing")
	}
	dropNamespace(mc, "proj")

	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if w := maasPart(t, st, "gateway").Warning; !strings.Contains(w, "project proj no longer exists") {
		t.Fatalf("status warning = %q", w)
	}

	// The project cannot be read: kept, nothing written.
	mc.f.fail["GET /api/v1/namespaces/proj"] = 500
	mc.f.requests = nil
	if _, logs, err := pruneGoneMaaSPublications(mc.c); err != nil || len(logs) != 0 {
		t.Fatalf("an unreadable project: %v, %v", logs, err)
	}
	for _, r := range mc.f.mutations() {
		t.Errorf("written while the project could not be read: %s", r)
	}
	if rec, _ := readMaaSRecord(mc.c); len(rec.Published) != 1 || !gatewayAdmits(t, mc) {
		t.Fatalf("the publication was dropped: %+v", rec)
	}
	delete(mc.f.fail, "GET /api/v1/namespaces/proj")

	res, err := CleanUp(mc.c, types.CleanupRequest{}, nil)
	if err != nil || !res.Success {
		t.Fatalf("clean up: %+v, %v", res, err)
	}
	if !strings.Contains(strings.Join(res.Logs, "\n"), "project proj no longer exists") {
		t.Errorf("logs: %v", res.Logs)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the gateway still admits proj")
	}
	if rec, _ := readMaaSRecord(mc.c); len(rec.Published) != 0 {
		t.Fatalf("record = %+v", rec)
	}
	if mc.f.has(tMaaSSub) || mc.f.has(tMaaSAuth) {
		t.Fatal("the subscription or access policy still lists the gone model")
	}
	st, _ = GetMaaSStatus(mc.c)
	if w := maasPart(t, st, "gateway").Warning; w != "" {
		t.Fatalf("still warned: %q", w)
	}
}

// A model deleted and made again under its name (a recreated project, or
// an editor's look-alike) is not the published one: the publication is
// gone, at the start of any MaaS operation.
func TestMaaS_APublishedModelMadeAgainIsGone(t *testing.T) {
	mc := publishedCluster(t)
	mc.f.mu.Lock()
	k, _ := parseFakePath(testModelPath)
	ensureMap(mc.f.objects[k], "metadata")["uid"] = "someone-elses-uid"
	mc.f.mu.Unlock()
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "authorino-tls"})
	if !strings.Contains(strings.Join(res.Logs, "\n"), "is not the one this tool published") {
		t.Fatalf("logs: %v", res.Logs)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the gateway still admits proj")
	}
	// The tool's reference in the project that still exists is deleted.
	if mc.f.has(tMaaSRef) {
		t.Fatal("the tool's MaaSModelRef is left")
	}
}

// The steps are independent: the access policy and subscription cannot be
// changed, yet the gateway stops admitting the gone project; the record
// keeps the publication, so the next run finishes the rest.
func TestMaaS_PruneNarrowsTheGatewayWhenAnotherStepFails(t *testing.T) {
	mc := publishedCluster(t)
	dropNamespace(mc, "proj")
	mc.f.fail["DELETE "+tMaaSSub] = 500
	changed, _, err := pruneGoneMaaSPublications(mc.c)
	if err == nil || !changed {
		t.Fatalf("changed %t, err %v", changed, err)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the gateway still admits proj")
	}
	if rec, _ := readMaaSRecord(mc.c); len(rec.Published) != 1 {
		t.Fatalf("the record forgot a publication whose subscription is not updated: %+v", rec)
	}
	delete(mc.f.fail, "DELETE "+tMaaSSub)
	if _, _, err := pruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := readMaaSRecord(mc.c); len(rec.Published) != 0 || mc.f.has(tMaaSSub) {
		t.Fatalf("record %+v, subscription left %t", rec, mc.f.has(tMaaSSub))
	}
}

// The models record cannot be read: a model made again cannot be told
// from the published one, so the status says so; the publication stays.
func TestMaaSStatus_WarnsWhenTheModelsRecordIsUnreadable(t *testing.T) {
	mc := publishedCluster(t)
	path := namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName)
	cm := mc.f.get(path)
	ensureMap(cm, "data")[modelsRecordKey] = "not json"
	mc.f.put(path, cm)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if w := maasPart(t, st, "gateway").Warning; !strings.Contains(w, "record of its models cannot be read") {
		t.Fatalf("warning = %q", w)
	}
	if _, _, err := pruneGoneMaaSPublications(mc.c); err != nil || !gatewayAdmits(t, mc) {
		t.Fatalf("the publication was dropped: %v", err)
	}
}

// The background run records an activity entry only when it changed
// something.
func TestMaaS_BackgroundPruneRecordsActivityOnlyOnChange(t *testing.T) {
	mc := publishedCluster(t)
	var entries []types.ActivityEntry
	orig := recordPruneActivity
	t.Cleanup(func() { recordPruneActivity = orig })
	recordPruneActivity = func(_ *Client, e types.ActivityEntry) { entries = append(entries, e) }
	PruneGoneMaaSPublicationsInBackground(mc.c)
	if len(entries) != 0 {
		t.Fatalf("an activity entry was recorded although nothing changed: %+v", entries)
	}
	dropNamespace(mc, "proj")
	PruneGoneMaaSPublicationsInBackground(mc.c)
	if len(entries) != 1 || entries[0].Action != "prune-maas-publications" || !entries[0].Success || !strings.Contains(entries[0].Detail, "project proj no longer exists") {
		t.Fatalf("entries = %+v", entries)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the gateway still admits proj")
	}
}

// A live publication is kept.
func TestMaaS_ALivePublicationIsKept(t *testing.T) {
	mc := publishedCluster(t)
	mc.f.requests = nil
	if _, logs, err := pruneGoneMaaSPublications(mc.c); err != nil || len(logs) != 0 {
		t.Fatalf("got %v, %v", logs, err)
	}
	if len(mc.f.mutations()) != 0 || !gatewayAdmits(t, mc) {
		t.Fatalf("changed: %v", mc.f.mutations())
	}
}

// Pruning one publication rebuilds the subscription from the rest (4.0.0):
// the remaining model keeps its own token limit and the subscription keeps
// who can use it, as publish and Change limit leave them.
func TestMaaS_PruneKeepsTheOthersLimitsAndAudience(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
	mc.f.putJSON("/apis/user.openshift.io/v1/groups/ml-team", `{}`)
	custom := &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{{Limit: 7, Window: "1m"}}}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "qwen25-05b-instruct", Limit: custom, Access: &types.MaaSAudience{Groups: []string{"ml-team"}}}); !res.Success {
		t.Fatalf("publish qwen25: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("publish smollm2: %+v", res)
	}
	// smollm2 is deleted and made again by someone else: its publication is gone.
	mc.f.mu.Lock()
	k, _ := parseFakePath(strings.Replace(testModelPath, "qwen25-05b-instruct", "smollm2-135m-instruct", 1))
	ensureMap(mc.f.objects[k], "metadata")["uid"] = "someone-elses-uid"
	mc.f.mu.Unlock()
	if changed, _, err := pruneGoneMaaSPublications(mc.c); err != nil || !changed {
		t.Fatalf("prune: changed %t, %v", changed, err)
	}
	spec, _ := subSpec(t, mc)
	refs, _ := spec["modelRefs"].([]interface{})
	if len(refs) != 1 {
		t.Fatalf("modelRefs: %s", asJSON(refs))
	}
	if got := asJSON(refs[0]); !strings.Contains(got, `"name":"qwen25-05b-instruct"`) || !strings.Contains(got, `{"limit":7,"window":"1m"}`) {
		t.Errorf("the remaining model lost its limit: %s", got)
	}
	if owner := asJSON(spec["owner"]); !strings.Contains(owner, `"ml-team"`) || strings.Contains(owner, "system:authenticated") {
		t.Errorf("the subscription lost who can use it: %s", owner)
	}
}
