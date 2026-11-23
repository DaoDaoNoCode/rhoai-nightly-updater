package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// Admin credentials wired into storage by putReadyManagedS3.
const oldAdminID, oldAdminSecret = "seaweedfs-key", "pass"

// putProjectS3Secrets fills both Secrets for a pipeline project with keys,
// in the same shape the tool persists them.
func putProjectS3Secrets(f *resourceFake, ns, id, secret string) {
	data := `"data":{"` + storage.S3AccessKeyField + `":"` + exampleAuth(id) + `","` + storage.S3SecretKeyField + `":"` + exampleAuth(secret) + `"}`
	f.putJSON(secretPath(ns, storage.DSPASecretName), `{"metadata":{"labels":`+toolLabelJSON+`},`+data+`}`)
	f.putJSON(secretPath(ns, storage.ConnectionSecretName), `{"metadata":{"labels":`+toolLabelJSON+`},`+data+`}`)
}

// putToolDSPAOnStorage places the tool-owned pipeline server in ns on
// seaweedfs-namespace storage, matching what setup would apply.
func putToolDSPAOnStorage(f *resourceFake, ns string) {
	putDSProject(f, ns)
	f.putJSON(dspaPath(ns, storage.DSPAName), `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(envkit.ToolFieldManager, "Apply")+`},
		"spec":{"objectStorage":{"externalStorage":{"host":"`+storage.S3Host()+`","scheme":"http","bucket":"pipelines","region":"us-east-1",
		"s3CredentialsSecret":{"secretName":"`+storage.DSPASecretName+`","accessKey":"`+storage.S3AccessKeyField+`","secretKey":"`+storage.S3SecretKeyField+`"}}}},
		"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"ObjectStoreAvailable","status":"True","observedGeneration":1}]}}`)
}

// putUnsecuredStorage lays down legacy IAM read-only storage whose admin
// key is copied into each project: team-a and team-b are recorded;
// unrecorded/dspa has the label but no record entry; dashboard runs a
// separately credentialed server.
func putUnsecuredStorage(f *resourceFake) {
	putManagedDSC(f)
	putReadyManagedS3(f)
	putS3DeploymentArgs(f, 1, unsecuredArgsJSON)
	for _, ns := range []string{"team-a", "team-b", "unrecorded"} {
		putToolDSPAOnStorage(f, ns)
		putProjectS3Secrets(f, ns, oldAdminID, oldAdminSecret)
	}
	putDSProject(f, "dashboard")
	putDSPA(f, "dashboard", "dspa", `{}`, "Mozilla", "Update", storage.S3Host(), "dashboard-dspa-secret", "", "")
	recordDSPAs(f, "team-a/dspa", "team-b/dspa")
}

func dspaAnnotations(f *resourceFake, ns string) map[string]any {
	a, _ := ensureMap(f.get(dspaPath(ns, storage.DSPAName)), "metadata")["annotations"].(map[string]any)
	return a
}

func TestSecureS3_GivesEachRecordedPipelineServerItsOwnKey(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)

	// Initially: unsecured; recorded DSPAs still share the legacy key;
	// excluded ones appear with explanations.
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	ks := st.S3Keys
	if ks == nil || ks.Secured || !ks.NeedsSecuring || strings.Join(ks.OldKey, ",") != "team-a/dspa,team-b/dspa" || len(ks.OwnKey) != 0 {
		t.Fatalf("before: %+v", ks)
	}
	if got := strings.Join(ks.KeepOldKey, "; "); got != "dashboard/dspa (not created by this tool); unrecorded/dspa (not recorded as this tool's)" {
		t.Errorf("kept = %q", got)
	}
	// Status reads alone must not mutate the cluster or auto-run securing.
	if m := f.mutations(); len(m) != 0 || len(f.sw.iamCalls()) != 0 {
		t.Fatalf("the status changed something: %v %v", m, f.sw.iamCalls())
	}

	resp, err := SecureS3Storage(c)
	if err != nil || !resp.Success {
		t.Fatalf("secure: %+v %v", resp, err)
	}
	// After secure: fresh admin key and deployment running secured IAM args.
	if id := secretValue(t, f, s3SecretPath, storage.S3AccessKeyField); id == oldAdminID || !strings.HasPrefix(id, "seaweedfs-") {
		t.Errorf("admin key after: %q", id)
	}
	if !f.sw.secured() {
		t.Error("the Deployment does not run with the secured args")
	}
	for _, ns := range []string{"team-a", "team-b"} {
		id := assertScopedKey(t, f, ns)
		a := dspaAnnotations(f, ns)
		if a[storage.DSPAS3UserAnnotation] != storage.ScopedUserName(ns) || a[storage.DSPAS3KeyAnnotation] != id {
			t.Errorf("%s annotations = %v", ns, a)
		}
		assertOwnedByDSPA(t, f, secretPath(ns, storage.DSPASecretName), ns, storage.DSPAName)
	}
	// unrecorded must remain on the superseded shared key and untouched otherwise.
	if got := secretValue(t, f, secretPath("unrecorded", storage.DSPASecretName), storage.S3AccessKeyField); got != oldAdminID {
		t.Errorf("the unrecorded server's Secret changed: %q", got)
	}
	if slices.Contains(f.sw.userNames(), storage.ScopedUserName("unrecorded")) {
		t.Error("a user was made for the unrecorded server")
	}
	for _, want := range []string{"team-a/dspa, team-b/dspa", "unrecorded/dspa (not recorded as this tool's)", "Restart the workbenches"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message lacks %q: %s", want, resp.Message)
		}
	}

	// When done: secured, per-recorded-DSPA keys reflected from live state.
	st, _ = GetResourcesStatus(c)
	if ks := st.S3Keys; !ks.Secured || ks.NeedsSecuring || strings.Join(ks.OwnKey, ",") != "team-a/dspa,team-b/dspa" || len(ks.OldKey) != 0 || ks.CheckError != "" {
		t.Errorf("after: %+v", ks)
	}

	// Idempotent re-invocation must leave existing scoped keys unchanged.
	before := f.sw.keysOf(storage.ScopedUserName("team-a"))
	resp, _ = SecureS3Storage(c)
	if !resp.Success || !strings.Contains(resp.Message, "Already on their own key: team-a/dspa, team-b/dspa") {
		t.Errorf("re-run: %+v", resp)
	}
	if after := f.sw.keysOf(storage.ScopedUserName("team-a")); !slices.Equal(before, after) {
		t.Errorf("a re-run changed the key: %v -> %v", before, after)
	}
}

// Preflight runs fully before writes; any failure aborts with cluster and
// SeaweedFS left exactly as before.
func TestSecureS3_PreflightFailuresChangeNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *resourceFake)
		want  string
	}{
		{"migration pending", func(f *resourceFake) { putLegacySeaweedFS(f) }, "Migrate storage"},
		{"storage not ready", func(f *resourceFake) { f.onGet = nil; putS3DeploymentArgs(f, 0, unsecuredArgsJSON) }, "not ready"},
		{"secret unreadable", func(f *resourceFake) { f.fail["GET "+secretPath("team-b", storage.ConnectionSecretName)] = 500 }, "Cannot read Secret team-b/" + storage.ConnectionSecretName},
		{"foreign secret", func(f *resourceFake) {
			f.putJSON(secretPath("team-b", storage.ConnectionSecretName), `{"metadata":{"labels":{}},"data":{}}`)
		}, "not created by this tool"},
		{"dry run rejected", func(f *resourceFake) { f.failDryRun["PATCH "+secretPath("team-b", storage.DSPASecretName)] = 422 }, "Nothing was changed"},
		{"pipeline server list fails", func(f *resourceFake) {
			f.fail["GET /apis/"+storage.DSPAAPIGroup+"/datasciencepipelinesapplications"] = 500
		}, "Cannot list the pipeline servers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastS3Timings(t)
			f, c := newResourceFake(t)
			putUnsecuredStorage(f)
			readyAfterApply(f)
			tc.setup(f)
			resp, err := SecureS3Storage(c)
			if err != nil || resp.Success || !strings.Contains(resp.Message, tc.want) || !strings.Contains(resp.Message, "Nothing was changed") {
				t.Fatalf("got %+v %v, want a refusal with %q", resp, err, tc.want)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("changes: %v", m)
			}
			if calls := f.sw.iamCalls(); len(calls) != 0 {
				t.Errorf("IAM calls: %v", calls)
			}
			if got := secretValue(t, f, s3SecretPath, storage.S3AccessKeyField); got != oldAdminID {
				t.Errorf("admin key = %q", got)
			}
		})
	}
}

// Failed key provisioning for one DSPA blocks its Secret updates; the next
// run completes peers, drops the bad IAM key, and does not rework finished servers.
func TestSecureS3_StopsOnAFailedProbeAndResumes(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	f.sw.failUserObjects[storage.ScopedUserName("team-b")] = 500

	resp, _ := SecureS3Storage(c)
	if resp.Success || resp.ErrorCode != "s3_keys" || !strings.Contains(resp.Message, "team-b/dspa did not get its own key") || !strings.Contains(resp.Message, "Already done: team-a/dspa") {
		t.Fatalf("first run: %+v", resp)
	}
	idA := assertScopedKey(t, f, "team-a")
	for _, name := range []string{storage.DSPASecretName, storage.ConnectionSecretName} {
		if got := secretValue(t, f, secretPath("team-b", name), storage.S3AccessKeyField); got != oldAdminID {
			t.Errorf("team-b's %s was written although its key failed: %q", name, got)
		}
	}
	stale := f.sw.keysOf(storage.ScopedUserName("team-b"))
	if len(stale) != 1 {
		t.Fatalf("team-b's user keys after the failure: %v", stale)
	}
	// Partial-run status should name DSPAs still on the shared key.
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; !ks.Secured || !ks.NeedsSecuring || strings.Join(ks.OldKey, ",") != "team-b/dspa" || strings.Join(ks.OwnKey, ",") != "team-a/dspa" {
		t.Errorf("status after the stop: %+v", ks)
	}

	delete(f.sw.failUserObjects, storage.ScopedUserName("team-b"))
	resp, _ = SecureS3Storage(c)
	if !resp.Success || !strings.Contains(resp.Message, "Now on a key of their own: team-b/dspa") || !strings.Contains(resp.Message, "Already on their own key: team-a/dspa") {
		t.Fatalf("resume: %+v", resp)
	}
	if got := assertScopedKey(t, f, "team-a"); got != idA {
		t.Errorf("the resume replaced team-a's working key")
	}
	if idB := assertScopedKey(t, f, "team-b"); idB == stale[0] {
		t.Errorf("the stale key %s was kept", stale[0])
	}
}

// SetupPipelineServer must reject unsecured storage so new namespaces
// cannot inherit the storage admin key.
func TestSetupPipelineServer_RefusedOnUnsecuredStorage(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	putDSProject(f, "new-proj")
	resp, _ := SetupPipelineServer(c, "new-proj")
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "Secure S3 storage") || !strings.Contains(resp.Message, "Nothing was changed") {
		t.Fatalf("got %+v", resp)
	}
	noWritesIn(t, f, "new-proj")
	if len(f.sw.iamCalls()) != 0 {
		t.Errorf("IAM calls: %v", f.sw.iamCalls())
	}
}

// SetupS3Storage repair on a legacy unsecured install preserves args and
// the existing admin key; IAM write mode and rotation require Secure S3 storage.
func TestSetupS3Storage_RepairKeepsUnsecuredStorageAsItIs(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	resp, err := SetupS3Storage(c)
	if err != nil || !resp.Success {
		t.Fatalf("repair: %+v %v", resp, err)
	}
	if f.sw.secured() {
		t.Error("Repair turned IAM writes on")
	}
	if got := secretValue(t, f, s3SecretPath, storage.S3AccessKeyField); got != oldAdminID {
		t.Errorf("Repair changed the admin key: %q", got)
	}
}

// On secured storage, setup gives each new pipeline server a dedicated key;
// teardown removes that user's access keys, then the IAM user itself.
func TestPipelineServer_OwnKeyLifecycle(t *testing.T) {
	fastS3Timings(t)
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSPO(f, 1)
	putDSProject(f, "proj")
	resp, _ := SetupPipelineServer(c, "proj")
	if !resp.Success {
		t.Fatalf("setup: %+v", resp)
	}
	id := assertScopedKey(t, f, "proj")
	if a := dspaAnnotations(f, "proj"); a[storage.DSPAS3UserAnnotation] != storage.ScopedUserName("proj") || a[storage.DSPAS3KeyAnnotation] != id {
		t.Errorf("annotations = %v", a)
	}
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; ks == nil || !ks.Secured || ks.NeedsSecuring || strings.Join(ks.OwnKey, ",") != "proj/dspa" {
		t.Errorf("status: %+v", ks)
	}

	f.finalizeOnGet = true
	down, _ := TeardownPipelineServer(c, "proj", "")
	if !down.Success {
		t.Fatalf("teardown: %+v", down)
	}
	if slices.Contains(f.sw.userNames(), storage.ScopedUserName("proj")) {
		t.Error("the user is left")
	}
	calls := f.sw.iamCalls()
	user := storage.ScopedUserName("proj")
	last := calls[len(calls)-3:]
	if want := []string{"ListAccessKeys " + user, "DeleteAccessKey " + user, "DeleteUser " + user}; !slices.Equal(last, want) {
		t.Errorf("teardown IAM calls end with %v, want %v", last, want)
	}
}

// Storage rejecting key creation must fail setup before project Secrets exist.
func TestSetupPipelineServer_KeyThatFailsChangesNoProjectObject(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.sw.failUserObjects[storage.ScopedUserName("proj")] = 403
	resp, _ := SetupPipelineServer(c, "proj")
	if resp.Success || resp.ErrorCode != "s3_keys" {
		t.Fatalf("got %+v", resp)
	}
	noWritesIn(t, f, "proj")
}

// S3Keys status is built from live Secret and IAM data, not static assumptions.
func TestS3KeysStatus_FromLiveKeys(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putToolDSPAOnStorage(f, "own")
	putToolDSPAOnStorage(f, "old")
	recordDSPAs(f, "own/dspa", "old/dspa")
	id, secret := f.sw.giveScopedKey("own")
	putProjectS3Secrets(f, "own", id, secret)
	// old: IAM user owns one key while the Secret still points elsewhere.
	f.sw.giveScopedKey("old")
	putProjectS3Secrets(f, "old", oldAdminID, oldAdminSecret)
	st, _ := GetResourcesStatus(c)
	ks := st.S3Keys
	if ks == nil || !ks.Secured || !ks.NeedsSecuring || strings.Join(ks.OwnKey, ",") != "own/dspa" || strings.Join(ks.OldKey, ",") != "old/dspa" {
		t.Fatalf("status = %+v", ks)
	}
	// With SeaweedFS down, status must surface an error instead of inferring keys.
	f.sw.failIAM["ListAccessKeys"] = 503
	st, _ = GetResourcesStatus(c)
	if ks := st.S3Keys; ks.CheckError == "" || len(ks.OwnKey)+len(ks.OldKey) != 0 || ks.NeedsSecuring {
		t.Errorf("status with SeaweedFS failing = %+v", ks)
	}
}

// Migration must finish with secured storage even if a prior migrate left
// IAM disabled and a pipeline server already on the new bucket using the
// admin key.
func TestMigrate_EndsSecuredOnUnsecuredNewStorage(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	putReadyManagedS3(f)
	putS3DeploymentArgs(f, 1, unsecuredArgsJSON)
	putToolDSPAOnStorage(f, "moved")
	putProjectS3Secrets(f, "moved", oldAdminID, oldAdminSecret)
	recordDSPAs(f, "juntao-test/"+storage.LegacyDSPAName, "team-a/"+storage.PreviousDSPAName, "moved/dspa")
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
	if !ok {
		t.Fatalf("got %v %s: %s", ok, code, msg)
	}
	if !f.sw.secured() || secretValue(t, f, s3SecretPath, storage.S3AccessKeyField) == oldAdminID {
		t.Error("the new storage is not secured with a new admin key")
	}
	for _, ns := range []string{"moved", "team-a", "juntao-test"} {
		assertScopedKey(t, f, ns)
	}
}

// NetworkPolicy for storage should expose only S3 and the admin UI ports.
// Port 18888 (filer gRPC, unauthenticated IAM) must stay blocked from the
// network.
func TestS3NetworkPolicyAllowsOnlyS3AndAdminUI(t *testing.T) {
	for _, secured := range []bool{false, true} {
		var ports []int
		for _, r := range storage.S3Resources(storage.S3Keys{AccessKey: "a", SecretKey: "b"}, secured) {
			if r.Kind != "NetworkPolicy" {
				continue
			}
			spec := r.Obj["spec"].(map[string]any)
			if types := spec["policyTypes"].([]string); !slices.Equal(types, []string{"Ingress"}) {
				t.Errorf("policyTypes = %v", types)
			}
			for _, rule := range spec["ingress"].([]map[string]any) {
				p, ok := rule["ports"].([]map[string]any)
				if !ok || len(p) == 0 {
					t.Fatalf("an ingress rule without ports admits every port: %v", rule)
				}
				for _, port := range p {
					ports = append(ports, port["port"].(int))
				}
			}
		}
		slices.Sort(ports)
		if !slices.Equal(ports, []int{8333, 12535}) {
			t.Errorf("secured=%v: the NetworkPolicy admits ports %v, want only 8333 and 12535", secured, ports)
		}
	}
}
