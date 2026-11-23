package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// H1: a live pipeline server whose record write failed keeps its user: Repair deletes no user.
func TestS3Keys_RepairKeepsTheUserOfAnUnrecordedLiveServer(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	putDSProject(f, "proj")
	f.fail["PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName] = 500
	resp, _ := SetupPipelineServer(c, "proj")
	t.Logf("setup: success=%v msg=%s", resp.Success, resp.Message)
	delete(f.fail, "PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName)
	user := storage.ScopedUserName("proj")
	t.Logf("users before: %v", f.sw.userNames())
	st, _ := GetResourcesStatus(c)
	if st.S3Keys != nil {
		t.Logf("status leftover=%v kept=%v", st.S3Keys.LeftoverUsers, st.S3Keys.KeepOldKey)
	}
	readyAfterApply(f)
	r2, _ := SetupS3Storage(c)
	t.Logf("repair: %s", r2.Message)
	if !slices.Contains(f.sw.userNames(), user) {
		t.Errorf("Repair deleted the user of a live pipeline server: %s", strings.Join(f.sw.userNames(), ","))
	}
}

// H2: a missing record ConfigMap makes no user a leftover.
func TestS3Keys_MissingRecordDeletesNoUser(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	putDSProject(f, "proj")
	if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
		t.Fatalf("setup: %+v", resp)
	}
	f.deleteObject("/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName)
	st, _ := GetResourcesStatus(c)
	if st.S3Keys != nil {
		t.Logf("status leftover=%v kept=%v needs=%v", st.S3Keys.LeftoverUsers, st.S3Keys.KeepOldKey, st.S3Keys.NeedsSecuring)
	}
	readyAfterApply(f)
	r2, _ := SetupS3Storage(c)
	t.Logf("repair: %s", r2.Message)
	if !slices.Contains(f.sw.userNames(), storage.ScopedUserName("proj")) {
		t.Errorf("Repair deleted the user of a live pipeline server after the record went missing")
	}
}
