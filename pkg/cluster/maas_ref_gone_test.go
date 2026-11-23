package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// N2: cluster-side artifacts for a publication may all be absent even when
// project and model remain (no MaaSModelRef owned by this tool, model absent
// from the MaaS gateway, no path routed via it)—in that case the publication
// is treated as gone: the UI row stays actionable ("Not published", with
// Publish), status surfaces removal (tab reload and background verification),
// and prune removes the entry from the record.
func TestMaaS_ARecordOnlyPublicationIsGoneAndPruned(t *testing.T) {
	mc := staleRecordCluster(t)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	it, ok := maasItem(st)
	if !ok || it.Removing || it.Published || it.Message != "Not published" {
		t.Fatalf("row = %+v (found %t)", it, ok)
	}
	if len(st.Removing) != 1 || !strings.Contains(st.Removing[0].Reason, "MaaSModelRef") {
		t.Fatalf("removing = %+v", st.Removing)
	}
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); rec.IsPublished("proj", tModel) {
		t.Fatalf("the record still lists it: %+v", rec.Published)
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Removing) != 0 {
		t.Fatalf("removing after the prune = %+v", st.Removing)
	}
}

// Fail closed: unreadable model route means the publication must not be
// classified as gone.
func TestMaaS_ARecordOnlyPublicationIsKeptWhileItsRouteCannotBeRead(t *testing.T) {
	mc := staleRecordCluster(t)
	path, _ := serving.ModelRoutePath("proj", tModel)
	mc.f.fail["GET "+path] = 500
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); !rec.IsPublished("proj", tModel) {
		t.Fatal("dropped although the route could not be read")
	}
}

// N6: a MaaSModelRef of the model's name that someone else made still
// stops the Models tab's Delete (it would point at nothing), but the
// refusal names it as theirs to remove: the MaaS tab leaves it alone and
// offers no Unpublish for it.
func TestModelsTeardown_AForeignMaaSModelRefIsNamedAsSomeoneElses(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	mc.f.putJSON(tMaaSRef, `{"metadata":{"name":"`+tModel+`","namespace":"proj"}}`)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: tModel})
	if res.Success || strings.Contains(res.Message, "Unpublish it on the MaaS tab") || !strings.Contains(res.Message, "did not create") || !strings.Contains(res.Message, "MaaSModelRef") {
		t.Fatalf("teardown = %+v", res)
	}
	if !mc.f.has(testModelPath) {
		t.Fatal("the model was deleted")
	}
}
