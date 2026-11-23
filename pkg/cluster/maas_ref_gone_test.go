package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
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
	it, ok := maasItem(st, "proj", tModel)
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

// Removing a MaaSModelRef without going through the tool does not imply gone
// when the model still has a route on the MaaS gateway: routing is active, the
// row reports that, and Unpublish is how you remove it. Prune keeps the record
// unchanged.
func TestMaaS_ARefDeletedOutsideWithTheModelStillOnTheGatewayIsNotGone(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, tMaaSRef)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Removing) != 0 {
		t.Fatalf("removing = %+v", st.Removing)
	}
	if len(st.Items) != 1 || !st.Items[0].Partly || !strings.Contains(st.Items[0].Message, "route through") {
		t.Fatalf("items = %+v", st.Items)
	}
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); !rec.IsPublished("proj", tModel) {
		t.Fatal("the prune dropped a publication with a live route")
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
