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

func gatewayAdmits(t *testing.T, mc *modelsCluster, project string) bool {
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
	if !gatewayAdmits(t, mc, "proj") {
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
	if logs, err := pruneGoneMaaSPublications(mc.c); err != nil || len(logs) != 0 {
		t.Fatalf("an unreadable project: %v, %v", logs, err)
	}
	for _, r := range mc.f.mutations() {
		t.Errorf("written while the project could not be read: %s", r)
	}
	if rec, _ := readMaaSRecord(mc.c); len(rec.Published) != 1 || !gatewayAdmits(t, mc, "proj") {
		t.Fatalf("the publication was dropped: %+v", rec)
	}
	delete(mc.f.fail, "GET /api/v1/namespaces/proj")

	res, err := CleanUp(mc.c, types.CleanupRequest{}, "", nil)
	if err != nil || !res.Success {
		t.Fatalf("clean up: %+v, %v", res, err)
	}
	if !strings.Contains(strings.Join(res.Logs, "\n"), "project proj no longer exists") {
		t.Errorf("logs: %v", res.Logs)
	}
	if gatewayAdmits(t, mc, "proj") {
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
	if gatewayAdmits(t, mc, "proj") {
		t.Fatal("the gateway still admits proj")
	}
}

// A live publication is kept.
func TestMaaS_ALivePublicationIsKept(t *testing.T) {
	mc := publishedCluster(t)
	mc.f.requests = nil
	if logs, err := pruneGoneMaaSPublications(mc.c); err != nil || len(logs) != 0 {
		t.Fatalf("got %v, %v", logs, err)
	}
	if len(mc.f.mutations()) != 0 || !gatewayAdmits(t, mc, "proj") {
		t.Fatalf("changed: %v", mc.f.mutations())
	}
}
