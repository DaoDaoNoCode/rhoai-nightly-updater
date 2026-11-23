package cluster

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// H1: a user is a leftover only when its project has no pipeline server
// at all; a user without a project tag is kept.
func TestS3Keys_LeftoversOnlyWithoutAnyPipelineServer(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	// A pipeline server the dashboard made (not the tool's, not recorded).
	putDSProject(f, "dash")
	putDSPA(f, "dash", "dspa", `{}`, "Mozilla", "Update", "s3.example.com", "dashboard-dspa-secret", "", "")
	f.sw.giveScopedKey("dash")
	f.sw.giveScopedKey("gone")
	// A hashed name without its tag cannot be told apart: kept. An
	// unhashed name is its project's (L4): a leftover, tag or not.
	long := strings.Repeat("u", 60)
	f.sw.giveScopedKey(long)
	f.sw.untag(storage.ScopedUserName(long))
	f.sw.giveScopedKey("untagged")
	f.sw.untag(storage.ScopedUserName("untagged"))
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; ks == nil || strings.Join(ks.LeftoverUsers, ",") != storage.ScopedUserName("gone")+","+storage.ScopedUserName("untagged") {
		t.Fatalf("leftovers = %+v", st.S3Keys)
	}
}

// H1: Secure S3 storage deletes only the leftover users the user
// confirmed, and only those still leftovers.
func TestSecureS3_DeletesOnlyConfirmedLeftovers(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	f.sw.giveScopedKey("gone-a")
	f.sw.giveScopedKey("gone-b")
	secureEvents = nil
	resp, _ := SecureS3Storage(c, []string{storage.ScopedUserName("gone-a")}, func(e UpdateStepEvent) { secureEvents = append(secureEvents, e) })
	if !resp.Success {
		t.Fatalf("got %+v", resp)
	}
	users := f.sw.userNames()
	if slices.Contains(users, storage.ScopedUserName("gone-a")) || !slices.Contains(users, storage.ScopedUserName("gone-b")) {
		t.Errorf("users = %v, want only the confirmed one deleted", users)
	}
}

// L-h: a user whose project tag names another project is refused, not re-keyed.
func TestSetupPipelineServer_RefusesAUserOfAnotherProject(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.sw.giveScopedKey("proj")
	f.sw.retag(storage.ScopedUserName("proj"), "other")
	before := f.sw.keysOf(storage.ScopedUserName("proj"))
	resp, _ := SetupPipelineServer(c, "proj")
	if resp.Success || !strings.Contains(resp.Message, "other") {
		t.Fatalf("got %+v", resp)
	}
	if after := f.sw.keysOf(storage.ScopedUserName("proj")); !slices.Equal(before, after) {
		t.Errorf("keys changed: %v -> %v", before, after)
	}
}

// L6: no SeaweedFS request while the storage is not ready.
func TestS3KeysStatus_NoLookupsWhileNotReady(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putS3DeploymentArgs(f, 0, securedArgsJSON)
	putToolDSPAOnStorage(f, "team-a")
	recordDSPAs(f, "team-a/dspa")
	GetResourcesStatus(c)
	if n := len(f.sw.allCalls()); n != 0 {
		t.Errorf("SeaweedFS requests while not ready: %v", f.sw.allCalls())
	}
}

// L6: one failing SeaweedFS lookup ends them: one IAM call, CheckError, no
// leftovers. A Kubernetes read error skips only its server.
func TestS3KeysStatus_FirstSeaweedFSErrorEndsTheLookups(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	for _, ns := range []string{"team-a", "team-b", "team-c"} {
		putToolDSPAOnStorage(f, ns)
		id, secret := f.sw.giveScopedKey(ns)
		putProjectS3Secrets(f, ns, id, secret)
		annotateKey(f, ns, id)
	}
	recordDSPAs(f, "team-a/dspa", "team-b/dspa", "team-c/dspa")
	f.sw.giveScopedKey("gone")
	f.fail["GET "+secretPath("team-a", storage.ConnectionSecretName)] = 500
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; strings.Join(ks.OwnKey, ",") != "team-b/dspa,team-c/dspa" || ks.CheckError == "" || len(ks.LeftoverUsers) != 0 {
		t.Errorf("with a Kubernetes read error: %+v", ks)
	}
	delete(f.fail, "GET "+secretPath("team-a", storage.ConnectionSecretName))
	f.sw.resetCalls()
	f.sw.failIAM["ListAccessKeys"] = 503
	st, _ = GetResourcesStatus(c)
	if ks := st.S3Keys; ks.CheckError == "" || len(ks.LeftoverUsers) != 0 {
		t.Errorf("with SeaweedFS failing: %+v", ks)
	}
	if calls := f.sw.iamCalls(); len(calls) != 1 {
		t.Errorf("IAM calls = %v, want exactly one", calls)
	}
}

// L-c: a cancelled run stops before the next server.
func TestSecureS3_StopsWhenCancelled(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	ctx, cancel := context.WithCancel(context.Background())
	resp, _ := SecureS3Storage(c.WithContext(ctx), nil, func(e UpdateStepEvent) {
		if e.Step == "rollout" && e.Status == "success" {
			cancel()
		}
	})
	if resp.Success || strings.Count(resp.Message, "context canceled") > 0 || !strings.Contains(resp.Message, "cancelled") {
		t.Fatalf("got %+v", resp)
	}
}

// L-f: a rotating Repair names only servers that held the old admin key.
func TestSetupS3Storage_RotationNamesOnlyHoldersOfTheOldKey(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	putProjectS3Secrets(f, "team-b", "some-other-key", "x")
	f.deleteObject(s3DeployPath)
	readyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if !resp.Success || !strings.Contains(resp.Message, "team-a/dspa") || strings.Contains(resp.Message, "team-b/dspa") {
		t.Fatalf("got %+v", resp)
	}
}

// L-g: the NetworkPolicy's sources count too.
func TestS3Status_NetworkPolicySourcesAreChecked(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	np := f.get(s3NPPath)
	rules := np["spec"].(map[string]any)["ingress"].([]any)
	// The admin UI port opened to every namespace.
	rules[1].(map[string]any)["from"] = []any{map[string]any{"namespaceSelector": map[string]any{}}}
	f.put(s3NPPath, np)
	if st := storage.S3Status(c); !strings.Contains(st.RepairNeeded, "NetworkPolicy seaweedfs-ingress was changed") {
		t.Errorf("RepairNeeded = %q", st.RepairNeeded)
	}
}

// Checksum: keyed with a stored random value, and the admin password counts.
func TestS3Resources_ChecksumIsKeyed(t *testing.T) {
	ann := func(keys storage.S3Keys) string {
		for _, r := range storage.S3Resources(keys, true) {
			if r.Kind == "Deployment" {
				a := r.Obj["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
				return a[storage.S3KeyChecksumAnnotation].(string)
			}
		}
		return ""
	}
	base := storage.S3Keys{AccessKey: "k", SecretKey: "s", AdminPassword: "p", ChecksumKey: "r1"}
	other := base
	other.ChecksumKey = "r2"
	pw := base
	pw.AdminPassword = "p2"
	if ann(base) == ann(other) || ann(base) == ann(pw) {
		t.Error("the checksum ignores the key or the admin password")
	}
}

// The status carries no key, secret or password in any form: the admin
// keys, the admin UI password, the checksum key and a pipeline server's
// own secret key, raw, in every base64 alphabet and as hex; on secured
// storage (also with IAM failing and with a leftover user) and on storage
// that is not secured.
func TestResourcesStatus_CarriesNoCredentialInAnyForm(t *testing.T) {
	fixture := []string{"AKID-Zq9xW7", "s3-secret-Zq9x", "admin-pw-Kd7", "checksum-key-Vb3", "scoped-secret-Qm5"}
	run := func(name string, prep func(f *resourceFake)) {
		t.Run(name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putManagedDSC(f)
			putReadyManagedS3(f)
			f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"`+storage.S3AccessKeyField+`":"`+exampleAuth(fixture[0])+`","`+storage.S3SecretKeyField+`":"`+exampleAuth(fixture[1])+`","WEED_ADMIN_PASSWORD":"`+exampleAuth(fixture[2])+`","RNU_CHECKSUM_KEY":"`+exampleAuth(fixture[3])+`"}}`)
			putToolDSPAOnStorage(f, "team-a")
			recordDSPAs(f, "team-a/dspa")
			id, _ := f.sw.giveScopedKey("team-a")
			putProjectS3Secrets(f, "team-a", id, fixture[4])
			prep(f)
			st, _ := GetResourcesStatus(c)
			b, _ := json.Marshal(st)
			for _, v := range fixture {
				raw := []byte(v)
				for _, form := range []string{v, base64.StdEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw),
					base64.URLEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw), hex.EncodeToString(raw)} {
					if strings.Contains(string(b), form) {
						t.Errorf("status carries %q", form)
					}
				}
			}
		})
	}
	run("secured", func(*resourceFake) {})
	run("IAM failing", func(f *resourceFake) { f.sw.failIAM["ListAccessKeys"] = 503 })
	run("leftover user", func(f *resourceFake) { f.sw.giveScopedKey("gone") })
	run("not secured", func(f *resourceFake) { putS3DeploymentArgs(f, 1, unsecuredArgsJSON) })
}

// H1: setup paths never delete a SeaweedFS user: Repair (also what a
// recipe runs) and a pipeline server's setup leave a leftover alone.
func TestSetupPaths_NeverDeleteAUser(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.sw.giveScopedKey("gone")
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("repair: %+v", resp)
	}
	if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
		t.Fatalf("setup: %+v", resp)
	}
	if !slices.Contains(f.sw.userNames(), storage.ScopedUserName("gone")) {
		t.Error("a setup path deleted a user")
	}
	for _, call := range f.sw.iamCalls() {
		if strings.HasPrefix(call, "DeleteUser") || strings.HasPrefix(call, "DeleteAccessKey "+storage.ScopedUserName("gone")) {
			t.Errorf("setup paths called %s", call)
		}
	}
}
