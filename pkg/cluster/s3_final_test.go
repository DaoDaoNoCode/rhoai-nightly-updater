package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// L1: teardown keeps a hashed user whose tag names another project.
func TestTeardownPipelineServer_KeepsAHashedUserTaggedForAnotherProject(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	long := strings.Repeat("p", 60)
	putToolDSPAOnStorage(f, long)
	recordDSPAs(f, long+"/dspa")
	f.sw.giveScopedKey(long)
	f.sw.retag(storage.ScopedUserName(long), "other")
	TeardownPipelineServer(c, long, "", nil)
	if !slices.Contains(f.sw.userNames(), storage.ScopedUserName(long)) {
		t.Error("teardown deleted a hashed user tagged for another project")
	}
}

// L2: a project whose DSPA list answers 404 keeps its user on teardown.
func TestTeardownPipelineServer_DSPAListNotFoundKeepsTheUser(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	f.sw.giveScopedKey("proj")
	f.fail["GET /apis/"+storage.DSPAAPIGroup+"/namespaces/proj/datasciencepipelinesapplications"] = 404
	TeardownPipelineServer(c, "proj", "", nil)
	if !slices.Contains(f.sw.userNames(), storage.ScopedUserName("proj")) {
		t.Error("teardown deleted the user while the DSPA list answered 404")
	}
}

// L6: "Kept SeaweedFS user" is logged only on secured storage for a user that exists.
func TestTeardownPipelineServer_KeptLogOnlyForAnExistingUser(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	putToolDSPAOnStorage(f, "proj")
	recordDSPAs(f, "proj/dspa")
	putDSPA(f, "proj", "other", `{}`, "Mozilla", "Update", storage.S3Host(), "other-secret", "", "")
	resp, _ := TeardownPipelineServer(c, "proj", "", nil)
	if strings.Contains(strings.Join(resp.Logs, "\n"), "Kept SeaweedFS user") {
		t.Errorf("logged a kept user that does not exist: %v", resp.Logs)
	}
}
