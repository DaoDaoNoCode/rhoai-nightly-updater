package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// M1: a pipeline server created in the project while Secure S3 storage
// runs keeps its user: each user's project is read again just before it
// is deleted.
func TestSecureS3_KeepsTheUserOfAPipelineServerCreatedDuringTheRun(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	f.sw.giveScopedKey("gone")
	user := storage.ScopedUserName("gone")
	SecureS3Storage(c, []string{user}, func(e UpdateStepEvent) {
		if e.Step == "check" && e.Status == "success" {
			putDSProject(f, "gone")
			putDSPA(f, "gone", "dspa", `{}`, "Mozilla", "Update", storage.S3Host(), storage.ConnectionSecretName, "", "")
		}
	})
	if !slices.Contains(f.sw.userNames(), user) {
		t.Errorf("user %s of a pipeline server created during the run was deleted", user)
	}
}

// L2: the DSPA list answering 404 deletes nothing.
func TestRemoveLeftoverKeys_DSPAListNotFoundDeletesNothing(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putToolDSPAOnStorage(f, "live")
	f.sw.giveScopedKey("live")
	user := storage.ScopedUserName("live")
	f.fail["GET /apis/"+storage.DSPAAPIGroup+"/datasciencepipelinesapplications"] = 404
	resp, _ := RemoveLeftoverS3Keys(c, []string{user})
	if resp.Success || !slices.Contains(f.sw.userNames(), user) {
		t.Errorf("got %+v; users %v", resp, f.sw.userNames())
	}
}

// M2: deleting the tool's pipeline server keeps the project's user while
// another pipeline server remains there, whatever Secret it uses.
func TestTeardownPipelineServer_KeepsTheUserWhileAnotherServerRemains(t *testing.T) {
	for _, secret := range []string{"other-secret", storage.DSPASecretName} {
		t.Run(secret, func(t *testing.T) {
			fastS3Timings(t)
			fastPipelineTimings(t)
			f, c := newResourceFake(t)
			putManagedDSC(f)
			putReadyManagedS3(f)
			putDSPO(f, 1)
			putToolDSPAOnStorage(f, "proj")
			recordDSPAs(f, "proj/dspa")
			putDSPA(f, "proj", "other", `{}`, "Mozilla", "Update", storage.S3Host(), secret, "", "")
			id, key := f.sw.giveScopedKey("proj")
			putProjectS3Secrets(f, "proj", id, key)
			TeardownPipelineServer(c, "proj", "")
			if !slices.Contains(f.sw.userNames(), storage.ScopedUserName("proj")) {
				t.Error("the user was deleted although proj/other remains")
			}
		})
	}
}

// L4: teardown never deletes a user whose tag names another project.
func TestTeardownPipelineServer_KeepsAUserOfAnotherProject(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	putToolDSPAOnStorage(f, "proj")
	recordDSPAs(f, "proj/dspa")
	f.sw.giveScopedKey("proj")
	f.sw.retag(storage.ScopedUserName("proj"), "other")
	TeardownPipelineServer(c, "proj", "")
	if !slices.Contains(f.sw.userNames(), storage.ScopedUserName("proj")) {
		t.Error("teardown deleted a user tagged for another project")
	}
}

// L1: a leftover whose project's Connection still holds its key is marked:
// workbenches using that Connection lose S3 access when it is removed.
func TestS3KeysStatus_LeftoverWithTheConnectionInUse(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "wb")
	id, secret := f.sw.giveScopedKey("wb")
	putProjectS3Secrets(f, "wb", id, secret)
	f.sw.giveScopedKey("gone")
	st, _ := GetResourcesStatus(c)
	ks := st.S3Keys
	if ks == nil || len(ks.Leftovers) != 2 {
		t.Fatalf("leftovers = %+v", ks)
	}
	for _, l := range ks.Leftovers {
		want := l.Project == "wb"
		if l.ConnectionInUse != want || l.User != storage.ScopedUserName(l.Project) {
			t.Errorf("leftover %+v", l)
		}
	}
}

// L5: a deletion that stops part-way names what it already deleted.
func TestRemoveLeftoverKeys_NamesWhatWasDeletedBeforeAFailure(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	f.sw.giveScopedKey("gone-a")
	f.sw.giveScopedKey("gone-b")
	f.sw.failDeleteUser = storage.ScopedUserName("gone-b")
	resp, _ := RemoveLeftoverS3Keys(c, []string{storage.ScopedUserName("gone-a"), storage.ScopedUserName("gone-b")})
	if resp.Success || !strings.Contains(resp.Message, storage.ScopedUserName("gone-a")) {
		t.Errorf("got %+v", resp)
	}
}

// L7: the status names each server's SeaweedFS user, also when it is hashed.
func TestS3KeysStatus_NamesEachServersUser(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putUnsecuredStorage(f)
	long := strings.Repeat("p", 60)
	putToolDSPAOnStorage(f, long)
	recordDSPAs(f, "team-a/dspa", "team-b/dspa", long+"/dspa")
	st, _ := GetResourcesStatus(c)
	if got := st.S3Keys.Users[long+"/dspa"]; got != storage.ScopedUserName(long) || !strings.Contains(got, ".") {
		t.Errorf("users = %v", st.S3Keys.Users)
	}
}
