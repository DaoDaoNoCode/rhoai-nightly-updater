package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Cluster state is authoritative: the MaaS tab must not derive rows or
// action buttons solely from this tool's persisted record; publications
// whose project or model no longer exist are pruned without manual steps.

const tModel = "qwen25-05b-instruct"

// deleteProject clears the project's namespace and all resources under it,
// mirroring what happens after a project delete completes on the cluster.
func deleteProject(mc *modelsCluster, project string) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	for k := range mc.f.objects {
		if k.ns == project || (k.plural == "namespaces" && k.name == project) {
			delete(mc.f.objects, k)
		}
	}
}

func maasItem(st *types.MaaSStatus, project, name string) (types.MaaSModel, bool) {
	for _, it := range st.Items {
		if it.Namespace == project && it.Name == name {
			return it, true
		}
	}
	return types.MaaSModel{}, false
}

// When the project is gone there is no table row and no follow-up work;
// status still reports the publication as undergoing automatic removal.
func TestMaaSStatus_AGoneProjectHasNoRow(t *testing.T) {
	mc := publishedCluster(t)
	deleteProject(mc, "proj")
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := maasItem(st, "proj", tModel); ok {
		t.Fatalf("a row for a publication whose project is gone: %+v", it)
	}
	want := []types.MaaSRemoval{{Namespace: "proj", Name: tModel, Reason: "project proj no longer exists"}}
	if len(st.Removing) != 1 || st.Removing[0] != want[0] {
		t.Fatalf("removing = %+v, want %+v", st.Removing, want)
	}
	if st.RemovalProblem != "" {
		t.Fatalf("a problem before any removal failed: %q", st.RemovalProblem)
	}
}

// A failed removal surfaces as RemovalProblem with the underlying error;
// after a successful prune, neither that field nor Removing entries remain.
func TestMaaSStatus_AFailedRemovalIsAProblemWithItsError(t *testing.T) {
	mc := publishedCluster(t)
	deleteProject(mc, "proj")
	mc.f.fail["DELETE "+tMaaSSub] = 500
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err == nil {
		t.Fatal("the prune did not fail")
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.RemovalProblem, "delete MaaSSubscription") {
		t.Fatalf("problem = %q", st.RemovalProblem)
	}
	delete(mc.f.fail, "DELETE "+tMaaSSub)
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	st, _ = GetMaaSStatus(mc.c)
	if st.RemovalProblem != "" || len(st.Removing) != 0 {
		t.Fatalf("left after a removal that succeeded: %q, %+v", st.RemovalProblem, st.Removing)
	}
}

// staleRecordCluster sets up cluster state where the model is not published
// but the on-disk record still marks it published (partial Unpublish, or
// tool re-created the model after unpublish).
func staleRecordCluster(t *testing.T) *modelsCluster {
	t.Helper()
	mc := publishedCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	rec, err := serving.ReadMaaSRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	rec.SetPublished("proj", tModel, true)
	if err := serving.WriteMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	return mc
}

// A stale record cannot produce a partly-published row or an Unpublish action;
// cluster truth wins—the UI shows not published and offers Publish.
func TestMaaSStatus_TheRecordAloneIsNotAPublication(t *testing.T) {
	mc := staleRecordCluster(t)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	it, ok := maasItem(st, "proj", tModel)
	if !ok {
		t.Fatal("no row for the tool's model")
	}
	if it.Published || it.Partly || it.Message != "Not published" {
		t.Fatalf("row = %+v", it)
	}
}

// Models-tab teardown must not block on record-only publication: it should
// not demand "Unpublish on the MaaS tab first" when there is no Unpublish
// control to use.
func TestMaaSPublishedIn_IgnoresTheRecordAlone(t *testing.T) {
	mc := staleRecordCluster(t)
	published, err := serving.MaaSPublishedIn(mc.c, "proj", tModel)
	if err != nil || published {
		t.Fatalf("published %t, %v", published, err)
	}
}

// While the project is terminating but the model still appears in the list,
// the row reflects removal in progress, exposes no actions, and requires
// no user intervention.
func TestMaaSStatus_AProjectBeingDeletedIsRemoving(t *testing.T) {
	mc := publishedCluster(t)
	mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"deletionTimestamp":"2026-11-22T10:00:00Z"}}`)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	it, ok := maasItem(st, "proj", tModel)
	if !ok {
		t.Fatal("no row for the listed model")
	}
	if !it.Removing || it.Published || it.Partly || !strings.Contains(it.Message, "project proj is being deleted") || !strings.Contains(it.Message, "automatically") {
		t.Fatalf("row = %+v", it)
	}
}

// The MaaS gateway admits routes from the projects of this tool's
// MaaSModelRefs on the cluster, not from every project its record lists: a
// publication only the record still lists does not put its project back.
func TestMaaSGateway_AdmitsOnlyProjectsWithALivePublication(t *testing.T) {
	mc := staleRecordCluster(t)
	if gatewayAdmits(t, mc) {
		t.Fatal("unpublish left proj on the gateway")
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	if gatewayAdmits(t, mc) {
		t.Fatal("the record alone put proj back on the gateway")
	}
}
