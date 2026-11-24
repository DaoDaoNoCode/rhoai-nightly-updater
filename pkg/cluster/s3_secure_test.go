package cluster

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
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

// secureEvents are the step events of the last secureRun.
var secureEvents []UpdateStepEvent

// secureRun runs Secure S3 storage and keeps its step events.
func secureRun(t *testing.T, c *Client) (*types.OperationResponse, error) {
	t.Helper()
	secureEvents = nil
	return SecureS3Storage(c, nil, func(e UpdateStepEvent) { secureEvents = append(secureEvents, e) })
}

// lastStatuses is each step's last status, in the order the steps began.
func lastStatuses() []string {
	var order []string
	last := map[string]string{}
	for _, e := range secureEvents {
		if _, ok := last[e.Step]; !ok {
			order = append(order, e.Step)
		}
		last[e.Step] = e.Status
	}
	out := make([]string, len(order))
	for i, id := range order {
		out[i] = id + "=" + last[id]
	}
	return out
}

func sawEvent(step, text string) bool {
	for _, e := range secureEvents {
		if e.Step == step && strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

// annotateKey writes the key annotation a finished run leaves on the DSPA.
func annotateKey(f *resourceFake, ns, id string) {
	obj := f.get(dspaPath(ns, storage.DSPAName))
	ensureMap(ensureMap(obj, "metadata"), "annotations")[storage.DSPAS3KeyAnnotation] = id
	f.put(dspaPath(ns, storage.DSPAName), obj)
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
	if got := strings.Join(ks.KeepOldKey, "; "); got != "dashboard/dspa (not set up by this tool); unrecorded/dspa (not recorded by this tool)" {
		t.Errorf("kept = %q", got)
	}
	// Status reads alone must not mutate the cluster or auto-run securing.
	if m := f.mutations(); len(m) != 0 || len(f.sw.iamCalls()) != 0 {
		t.Fatalf("the status changed something: %v %v", m, f.sw.iamCalls())
	}

	resp, err := secureRun(t, c)
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
	// The steps the user sees: the checks, the rollout, one per server.
	if got := strings.Join(lastStatuses(), " "); got != "check=success rollout=success server:team-a/dspa=success server:team-b/dspa=success" {
		t.Errorf("steps = %s", got)
	}
	for _, line := range []string{"Checking the new key against the storage", "Writing the key into Secrets", "Asking the data-science-pipelines operator"} {
		if !sawEvent("server:team-a/dspa", line) {
			t.Errorf("no %q progress for team-a", line)
		}
	}
	if !sawEvent("rollout", "Waiting up to") {
		t.Error("the rollout's progress is not streamed")
	}
	for _, want := range []string{"On a key of their own: team-a/dspa, team-b/dspa.", "Left on the old admin key, which no longer works: dashboard/dspa (not set up by this tool), unrecorded/dspa (not recorded by this tool).", "Restart the workbenches"} {
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
	resp, _ = secureRun(t, c)
	if !resp.Success || !strings.Contains(resp.Message, "On a key of their own: team-a/dspa, team-b/dspa.") || strings.Contains(resp.Message, "Restart") {
		t.Errorf("re-run: %+v", resp)
	}
	if got := strings.Join(lastStatuses(), " "); got != "check=success rollout=skipped server:team-a/dspa=success server:team-b/dspa=success" || !sawEvent("server:team-a/dspa", "already uses its own key") {
		t.Errorf("re-run steps = %s", got)
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
			resp, err := secureRun(t, c)
			if err != nil || resp.Success || !strings.Contains(resp.Message, tc.want) || !strings.Contains(resp.Message, "Nothing was changed") {
				t.Fatalf("got %+v %v, want a refusal with %q", resp, err, tc.want)
			}
			if got := strings.Join(lastStatuses(), " "); got != "check=failed" {
				t.Errorf("steps = %s, want only the checks, failed", got)
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

	resp, _ := secureRun(t, c)
	// The storage is secured; one server is left: a warning outcome
	// ("partial") that names it and says a re-run finishes it.
	if !resp.Success || resp.ErrorCode != "partial" || !strings.Contains(resp.Message, "team-b/dspa did not get its own key") ||
		!strings.Contains(resp.Message, "On a key of their own: team-a/dspa.") || !strings.Contains(resp.Message, "Run Secure S3 storage again") {
		t.Fatalf("first run: %+v", resp)
	}
	if got := strings.Join(lastStatuses(), " "); got != "check=success rollout=success server:team-a/dspa=success server:team-b/dspa=failed" {
		t.Errorf("steps = %s", got)
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
	resp, _ = secureRun(t, c)
	if !resp.Success || !strings.Contains(resp.Message, "On a key of their own: team-a/dspa, team-b/dspa.") || strings.Contains(resp.Message, "Still on the old key") {
		t.Fatalf("resume: %+v", resp)
	}
	if got := strings.Join(lastStatuses(), " "); got != "check=success rollout=skipped server:team-a/dspa=success server:team-b/dspa=success" || !sawEvent("server:team-a/dspa", "already uses its own key") {
		t.Errorf("resume steps = %s", got)
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
	down, _ := TeardownPipelineServer(c, "proj", "", nil)
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
	annotateKey(f, "own", id)
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
					if len(port) != 2 || port["protocol"] != "TCP" || port["endPort"] != nil {
						t.Errorf("a port entry must be exactly protocol TCP and port: %v", port)
					}
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

// Review 1: a run whose annotation of the DSPA failed (both Secrets hold
// the new key) is not done; the next run annotates it instead of
// skipping the server as finished.
func TestSecureS3_RetriesAFailedAnnotation(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	f.fail["PATCH "+dspaPath("team-b", storage.DSPAName)] = 500
	resp, _ := secureRun(t, c)
	if resp.ErrorCode != "partial" || !strings.Contains(resp.Message, "team-b/dspa did not get its own key") {
		t.Fatalf("first run: %+v", resp)
	}
	id := assertScopedKey(t, f, "team-b")
	// Review 2: the status shows it unfinished: the annotation does not name the key yet.
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; !ks.NeedsSecuring || strings.Join(ks.OldKey, ",") != "team-b/dspa" {
		t.Errorf("status after the failed annotation: %+v", ks)
	}
	delete(f.fail, "PATCH "+dspaPath("team-b", storage.DSPAName))
	resp, _ = secureRun(t, c)
	if !resp.Success {
		t.Fatalf("re-run: %+v", resp)
	}
	if a := dspaAnnotations(f, "team-b"); a[storage.DSPAS3KeyAnnotation] != id {
		t.Errorf("the re-run did not annotate team-b: %v", a)
	}
	if !sawEvent("server:team-b/dspa", "Asking the data-science-pipelines operator") {
		t.Error("the re-run did not say it asks the operator again")
	}
	st, _ = GetResourcesStatus(c)
	if ks := st.S3Keys; ks.NeedsSecuring || strings.Join(ks.OwnKey, ",") != "team-a/dspa,team-b/dspa" {
		t.Errorf("status after the re-run: %+v", ks)
	}
}

// Review 2: rotation done, one server's key check failed (its pipeline
// Secret still holds the dead admin key), and its Connection was deleted
// in the dashboard: the storage still needs Secure S3 storage.
func TestS3KeysStatus_MissingConnectionStillNeedsSecuring(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	f.sw.failUserObjects[storage.ScopedUserName("team-b")] = 500
	if resp, _ := secureRun(t, c); resp.ErrorCode != "partial" {
		t.Fatalf("expected team-b to fail: %+v", resp)
	}
	f.deleteObject(secretPath("team-b", storage.ConnectionSecretName))
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; !ks.Secured || !ks.NeedsSecuring || strings.Join(ks.OldKey, ",") != "team-b/dspa" {
		t.Errorf("status = %+v, want team-b still to secure", ks)
	}
}

// Review 3: a first setup that stopped after writing the credentials
// Secret but before the Deployment is retried secured, with a new key.
func TestSetupS3Storage_RetryOfAFirstSetupIsSecured(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	f.deleteObject(s3DeployPath)
	readyAfterApply(f)
	resp, err := SetupS3Storage(c)
	if err != nil || !resp.Success {
		t.Fatalf("setup: %+v %v", resp, err)
	}
	if !f.sw.secured() {
		t.Error("the retried first setup is not secured")
	}
	if got := secretValue(t, f, s3SecretPath, storage.S3AccessKeyField); got == oldAdminID {
		t.Error("a storage set up secured kept a key that was stored before its Deployment")
	}
}

// The fake checks every signature with the secret (verifySigV4), so a
// signing mistake fails the tests: a request signed with another secret
// is refused and changes nothing.
func TestSeaweedFake_RefusesAWrongSignature(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.sw.adminSecretOverride = "another-secret"
	resp, _ := SetupPipelineServer(c, "proj")
	if resp.Success || !strings.Contains(resp.Message, "SignatureDoesNotMatch") {
		t.Fatalf("got %+v", resp)
	}
	if len(f.sw.userNames()) != 0 {
		t.Errorf("users made with a bad signature: %v", f.sw.userNames())
	}
	noWritesIn(t, f, "proj")
}

// M1: once the storage is secured, one project's problem does not hold
// the others back: it is reported and the rest get their keys.
func TestSecureS3_OnSecuredStorageOneBadProjectDoesNotBlockTheOthers(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	f.sw.failUserObjects[storage.ScopedUserName("team-a")] = 500
	if resp, _ := secureRun(t, c); resp.ErrorCode != "partial" {
		t.Fatalf("first run: %+v", resp)
	}
	delete(f.sw.failUserObjects, storage.ScopedUserName("team-a"))
	// team-a's Connection now belongs to someone else.
	f.putJSON(secretPath("team-a", storage.ConnectionSecretName), `{"metadata":{"labels":{}},"data":{}}`)
	resp, _ := secureRun(t, c)
	if !strings.Contains(resp.Message, "team-a/dspa") || !strings.Contains(resp.Message, "not created by this tool") {
		t.Errorf("the outcome does not name team-a's problem: %+v", resp)
	}
	assertScopedKey(t, f, "team-b")
	if got := strings.Join(lastStatuses(), " "); got != "check=success rollout=skipped server:team-a/dspa=failed server:team-b/dspa=success" {
		t.Errorf("steps = %s", got)
	}
}

// L5: a run that has too little time left stops before a server, cleanly.
func TestSecureS3_StopsBeforeAServerWhenTimeRunsOut(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	readyAfterApply(f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secureEvents = nil
	resp, _ := SecureS3Storage(c.WithContext(ctx), nil, func(e UpdateStepEvent) { secureEvents = append(secureEvents, e) })
	if resp.Success || resp.ErrorCode != "out_of_time" || strings.Count(resp.Message, "Secure S3 storage again") != 1 {
		t.Fatalf("got %+v", resp)
	}
	if got := secretValue(t, f, secretPath("team-a", storage.DSPASecretName), storage.S3AccessKeyField); got != oldAdminID {
		t.Errorf("a server was changed without time to finish it: %q", got)
	}
}

// M2: a pipeline server deleted while SeaweedFS failed leaves its user;
// the status lists it, Repair keeps it (no side effect), and Remove
// leftover keys deletes it (keys first).
func TestS3Keys_LeftoverUserListedAndRemovedOnlyByTheAction(t *testing.T) {
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
	f.finalizeOnGet = true
	f.sw.failIAM["ListAccessKeys"] = 503
	if down, _ := TeardownPipelineServer(c, "proj", "", nil); down.Success {
		t.Fatalf("teardown should report the key it could not delete: %+v", down)
	}
	delete(f.sw.failIAM, "ListAccessKeys")
	user := storage.ScopedUserName("proj")
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; ks == nil || strings.Join(ks.LeftoverUsers, ",") != user || ks.NeedsSecuring {
		t.Fatalf("status: %+v", ks)
	}
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("repair: %+v", resp)
	}
	if !slices.Contains(f.sw.userNames(), user) {
		t.Fatal("Repair deleted a user: only the confirmed action may")
	}
	// A confirmed name that is not a leftover any more is not deleted.
	putToolDSPAOnStorage(f, "live")
	f.sw.giveScopedKey("live")
	resp, _ := RemoveLeftoverS3Keys(c, []string{user, storage.ScopedUserName("live")})
	if !resp.Success || !strings.Contains(resp.Message, user) {
		t.Fatalf("remove: %+v", resp)
	}
	if slices.Contains(f.sw.userNames(), user) || !slices.Contains(f.sw.userNames(), storage.ScopedUserName("live")) {
		t.Errorf("users after the removal: %v", f.sw.userNames())
	}
	st, _ = GetResourcesStatus(c)
	if ks := st.S3Keys; len(ks.LeftoverUsers) != 0 {
		t.Errorf("status after the removal: %+v", ks)
	}
}

// M2: a user whose tags cannot be read makes nothing a leftover (fail closed).
func TestS3Keys_NoLeftoversWhenTagsCannotBeRead(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	f.sw.giveScopedKey("gone")
	f.sw.failIAM["ListUserTags"] = 503
	st, _ := GetResourcesStatus(c)
	if ks := st.S3Keys; ks != nil && len(ks.LeftoverUsers) != 0 {
		t.Errorf("leftovers without readable tags: %+v", ks)
	}
}

// L1: a crafted project name cannot take another project's hashed user name.
func TestScopedUserName_NoCollision(t *testing.T) {
	long := strings.Repeat("a", 63)
	hashed := storage.ScopedUserName(long)
	if len(hashed) > 64 {
		t.Fatalf("%q is longer than 64", hashed)
	}
	crafted := strings.TrimPrefix(hashed, storage.ScopedUserPrefix)
	crafted = strings.ReplaceAll(crafted, ".", "-")
	if len(crafted) <= 63 && storage.ScopedUserName(crafted) == hashed {
		t.Errorf("project %q gets the user of %q: %s", crafted, long, hashed)
	}
}

// L2: a new admin key rolls the pod: the template carries a checksum of it.
func TestS3Resources_PodTemplateFollowsTheAdminKey(t *testing.T) {
	ann := func(keys storage.S3Keys) string {
		for _, r := range storage.S3Resources(keys, true) {
			if r.Kind == "Deployment" {
				tmpl := r.Obj["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
				a, _ := tmpl["annotations"].(map[string]any)
				s, _ := a[storage.S3KeyChecksumAnnotation].(string)
				return s
			}
		}
		return ""
	}
	a, b := ann(storage.S3Keys{AccessKey: "k1", SecretKey: "s"}), ann(storage.S3Keys{AccessKey: "k2", SecretKey: "s"})
	if a == "" || a == b || a != ann(storage.S3Keys{AccessKey: "k1", SecretKey: "s"}) {
		t.Errorf("checksums %q %q", a, b)
	}
	if strings.Contains(a, "k1") {
		t.Error("the annotation holds the key itself")
	}
}

// L7: a Repair that had to make a new admin key says who lost access.
func TestSetupS3Storage_RotatedKeyNamesWhoLostAccess(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putUnsecuredStorage(f)
	f.deleteObject(s3DeployPath)
	readyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if !resp.Success || !strings.Contains(resp.Message, "team-a/dspa") || !strings.Contains(resp.Message, "Secure S3 storage") {
		t.Fatalf("got %+v", resp)
	}
}

// L4: the status asks for Repair when the NetworkPolicy is missing or
// admits another port.
func TestS3Status_NetworkPolicyIsChecked(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	if st := storage.S3Status(c); st.RepairNeeded != "" {
		t.Fatalf("as set up: %q", st.RepairNeeded)
	}
	np := f.get(s3NPPath)
	rules := np["spec"].(map[string]any)["ingress"].([]any)
	rule := rules[0].(map[string]any)
	rule["ports"] = append(rule["ports"].([]any), map[string]any{"protocol": "TCP", "port": 18888})
	f.put(s3NPPath, np)
	if st := storage.S3Status(c); !strings.Contains(st.RepairNeeded, "NetworkPolicy seaweedfs-ingress was changed") {
		t.Errorf("with port 18888: %q", st.RepairNeeded)
	}
	f.deleteObject(s3NPPath)
	if st := storage.S3Status(c); !strings.Contains(st.RepairNeeded, "NetworkPolicy seaweedfs-ingress is missing") {
		t.Errorf("missing: %q", st.RepairNeeded)
	}
}
