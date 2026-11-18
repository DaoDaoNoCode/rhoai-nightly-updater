package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	migrateJobPathT    = "/apis/batch/v1/namespaces/seaweedfs/jobs/seaweedfs-migrate"
	migrateSourcePathT = "/api/v1/namespaces/seaweedfs/secrets/seaweedfs-migrate-source"
	migratePodPath     = "/api/v1/namespaces/seaweedfs/pods/seaweedfs-migrate-x7k2p"
	// The old storage's keys (putLegacySeaweedFS).
	legacyKeyID = "minio-abc"
	legacyKey   = "old-pass"
)

// copyOK is the Job result of a complete copy of 4 objects (4,330 bytes).
const copyOK = `{"copy":0,"check":0,"identical":4,"missing":0,"differ":0,"errors":0,"source":{"count":4,"bytes":4330,"sizeless":0},"destination":{"count":4,"bytes":4330,"sizeless":0}}`

// migrateSim plays the controllers Migrate storage waits for, acting on the
// stored objects before every request: the deployment controller (the new
// SeaweedFS is ready), the Job controller (the copy Job completes and its
// pod's termination message is result) and DSPO (ObjectStoreAvailable for
// pipeline servers on the new storage).
type migrateSim struct {
	result      string
	jobRunning  bool
	objectStore string // ObjectStoreAvailable status reported for the new storage
	mode        string // the COPY_MODE of the last copy Job seen
}

// finishJobAfterReads lets the copy Job report running for its first n reads
// (GETs of the Job by the code under test), then complete. Tests use it
// instead of flipping the state after a sleep, which raced the scheduler.
func finishJobAfterReads(f *resourceFake, sim *migrateSim, n int) {
	next := f.beforeServe
	reads := 0
	f.beforeServe = func(method, path string) {
		if method == "GET" && strings.Contains(path, "/jobs/"+migrateJobName) {
			f.mu.Lock()
			reads++
			if reads > n {
				sim.jobRunning = false
			}
			f.mu.Unlock()
		}
		next(method, path)
	}
}

// finishJobAfter lets the copy Job report running until d has passed on
// the clock (counted from now), then complete.
func finishJobAfter(f *resourceFake, sim *migrateSim, d time.Duration) {
	next := f.beforeServe
	end := clk.Now().Add(d)
	f.beforeServe = func(method, path string) {
		if method == "GET" && strings.Contains(path, "/jobs/"+migrateJobName) && !clk.Now().Before(end) {
			f.mu.Lock()
			sim.jobRunning = false
			f.mu.Unlock()
		}
		next(method, path)
	}
}

// jobCopyMode returns a stored copy Job's COPY_MODE.
func jobCopyMode(job map[string]interface{}) string {
	tmpl, _ := ensureMap(job, "spec")["template"].(map[string]interface{})
	spec, _ := tmpl["spec"].(map[string]interface{})
	ctrs, _ := spec["containers"].([]interface{})
	for _, ctr := range ctrs {
		env, _ := ctr.(map[string]interface{})["env"].([]interface{})
		for _, e := range env {
			if em := e.(map[string]interface{}); em["name"] == "COPY_MODE" {
				return fmt.Sprint(em["value"])
			}
		}
	}
	return ""
}

func simulateMigrate(f *resourceFake, sim *migrateSim) {
	f.beforeServe = func(method, path string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for k, obj := range f.objects {
			switch {
			case k.plural == "deployments" && k.ns == s3Namespace && k.name == s3Deployment:
				obj["status"] = map[string]interface{}{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1}
			case k.plural == "jobs" && k.name == migrateJobName:
				sim.mode = jobCopyMode(obj)
				ensureMap(obj, "spec")["selector"] = map[string]interface{}{"matchLabels": map[string]interface{}{"job-name": migrateJobName}}
				if !sim.jobRunning {
					obj["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Complete", "status": "True"}}}
					pod := fakeKey{gv: "v1", ns: s3Namespace, plural: "pods", name: "seaweedfs-migrate-x7k2p"}
					f.objects[pod] = map[string]interface{}{
						"metadata": map[string]interface{}{"name": pod.name, "namespace": s3Namespace, "labels": map[string]interface{}{"job-name": migrateJobName}},
						"status": map[string]interface{}{"phase": "Succeeded", "containerStatuses": []interface{}{map[string]interface{}{
							"name": migrateContainer, "state": map[string]interface{}{"terminated": map[string]interface{}{"exitCode": 0, "reason": "Completed", "message": sim.result}},
						}}},
					}
				}
			case k.plural == "datasciencepipelinesapplications":
				es, _ := ensureMap(ensureMap(obj, "spec"), "objectStorage")["externalStorage"].(map[string]interface{})
				if es != nil && es["host"] == s3Host() && sim.objectStore != "" {
					gen := ensureMap(obj, "metadata")["generation"]
					obj["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
						"type": "ObjectStoreAvailable", "status": sim.objectStore, "observedGeneration": gen, "message": "Object Store connectivity successfully verified",
					}}}
				}
			}
		}
	}
}

func fastMigrateTimings(t *testing.T) {
	t.Helper()
	fastS3Timings(t)
	oldCopy, oldPoll, oldRepoint := MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout
	MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout = 200*time.Millisecond, 5*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout = oldCopy, oldPoll, oldRepoint })
	t.Setenv("RCLONE_IMAGE", "")
}

// putMigrationCase is the user's cluster: the storage 2.1 to 2.3 left in
// namespace minio, the pipeline server an earlier version created
// (juntao-test/dspa, recognised by its managedFields), one this version
// created (labelled) and one made by hand, all three on the old storage.
func putMigrationCase(f *resourceFake) {
	putLegacySeaweedFS(f)
	putDSPAOn(f, "juntao-test", legacyDSPAName, `{}`, legacyPostManager, legacyDSPASecretName)
	putDSPAOn(f, "team-a", previousDSPAName, toolLabelJSON, toolFieldManager, dspaSecretName)
	f.putJSON(secretPath("team-a", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"AWS_ACCESS_KEY_ID":"`+exampleAuth(legacyKeyID)+`","AWS_SECRET_ACCESS_KEY":"`+exampleAuth(legacyKey)+`"}}`)
	putDSPAOn(f, "hand-made", legacyDSPAName, `{}`, "Mozilla", "my-secret")
}

// putDSPAOn stores a DSPA on the old storage, as earlier versions wrote it.
func putDSPAOn(f *resourceFake, ns, name, labels, manager, secret string) {
	f.putJSON(dspaPath(ns, name), `{"metadata":{"labels":`+labels+`,"generation":1,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(manager, "Update")+`},
		"spec":{"objectStorage":{"externalStorage":{"host":"`+legacyS3Host()+`","scheme":"http","bucket":"pipelines","region":"us-east-1",
		"s3CredentialsSecret":{"secretName":"`+secret+`","accessKey":"AWS_ACCESS_KEY_ID","secretKey":"AWS_SECRET_ACCESS_KEY"}}}},
		"status":{"conditions":[{"type":"ObjectStoreAvailable","status":"True","observedGeneration":1}]}}`)
}

// putNewStorageWithLegacyKeys puts a ready new storage set up with the old keys.
func putNewStorageWithLegacyKeys(f *resourceFake) {
	putReadyManagedS3(f)
	f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"AWS_ACCESS_KEY_ID":"`+exampleAuth(legacyKeyID)+`","AWS_SECRET_ACCESS_KEY":"`+exampleAuth(legacyKey)+`"}}`)
}

func runMigrate(t *testing.T, c *Client, opts MigrateOptions) (bool, string, string, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	res, err := MigrateS3Storage(c, opts, func(e UpdateStepEvent) { steps = append(steps, e) })
	if err != nil {
		t.Fatalf("MigrateS3Storage: %v", err)
	}
	return res.Success, res.Message, res.ErrorCode, steps
}

func externalStorage(t *testing.T, f *resourceFake, ns, name string) map[string]interface{} {
	t.Helper()
	obj := f.get(dspaPath(ns, name))
	if obj == nil {
		t.Fatalf("DSPA %s/%s is gone", ns, name)
	}
	return ensureMap(ensureMap(obj, "spec"), "objectStorage")["externalStorage"].(map[string]interface{})
}

func secretValue(t *testing.T, f *resourceFake, path, key string) string {
	t.Helper()
	obj := f.get(path)
	if obj == nil {
		t.Fatalf("no secret %s", path)
	}
	if sd, ok := obj["stringData"].(map[string]interface{}); ok {
		if v, ok := sd[key].(string); ok {
			return v
		}
	}
	data, _ := obj["data"].(map[string]interface{})
	v, _ := data[key].(string)
	return decodeBase64Field(v)
}

// noChangeToPipelineServers fails when any pipeline server or project
// secret was written.
func noChangeToPipelineServers(t *testing.T, f *resourceFake) {
	t.Helper()
	for _, m := range f.mutations() {
		if strings.Contains(m, "datasciencepipelinesapplications") || (strings.Contains(m, "/secrets") && !strings.Contains(m, "/namespaces/seaweedfs/")) {
			t.Errorf("unexpected pipeline server change: %s", m)
		}
	}
}

func TestMigrate_FullRunMovesDataAndToolPipelineServersOnly(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})

	ok, msg, code, steps := runMigrate(t, c, MigrateOptions{})
	if !ok {
		t.Fatalf("migrate failed: %s (%s)", msg, code)
	}
	for _, want := range []string{"4 objects (4330 bytes)", "2 pipeline servers now use it: juntao-test/dspa, team-a/nightly-dspa", "hand-made/dspa (not created by this tool)", "The old storage in namespace 'minio' is kept: delete it later from the S3 storage's menu, once nothing else uses it."} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	// 1. The new storage has fresh keys, not the old ones: whoever knew
	// the old storage's keys has no access to the new storage.
	newID, newKey := secretValue(t, f, s3SecretPath, s3AccessKeyField), secretValue(t, f, s3SecretPath, s3SecretKeyField)
	if newID == "" || newKey == "" || newID == legacyKeyID || newKey == legacyKey {
		t.Errorf("the new storage's keys are %q/%q, not fresh ones", newID, newKey)
	}
	// 2. The Job ran and is gone. It read the old keys from its own
	// Secret, owned by the Job (garbage-collected with it).
	if !hasMutation(f, "POST /apis/batch/v1/namespaces/seaweedfs/jobs") || f.has(migrateJobPathT) {
		t.Error("the copy Job was not created, or not deleted after its result was read")
	}
	if !hasMutation(f, "POST /api/v1/namespaces/seaweedfs/secrets") {
		t.Error("the copy Job's source Secret was not created")
	}
	if src := f.get(migrateSourcePathT); src != nil {
		if secretValue(t, f, migrateSourcePathT, s3AccessKeyField) != legacyKeyID || secretValue(t, f, migrateSourcePathT, s3SecretKeyField) != legacyKey {
			t.Error("the source Secret does not hold the old keys")
		}
		refs, _ := ensureMap(src, "metadata")["ownerReferences"].([]interface{})
		if len(refs) != 1 || refs[0].(map[string]interface{})["kind"] != "Job" || refs[0].(map[string]interface{})["name"] != migrateJobName {
			t.Errorf("source Secret owner = %v", refs)
		}
	}
	// 3. Both tool pipeline servers point at the new storage with the
	// standard secret; the earlier version's one is now labelled.
	for _, ps := range [][2]string{{"juntao-test", legacyDSPAName}, {"team-a", previousDSPAName}} {
		es := externalStorage(t, f, ps[0], ps[1])
		if es["host"] != s3Host() || ensureMap(es, "s3CredentialsSecret")["secretName"] != dspaSecretName || es["bucket"] != "pipelines" || es["scheme"] != "http" {
			t.Errorf("%s/%s external storage = %v", ps[0], ps[1], es)
		}
		if secretValue(t, f, secretPath(ps[0], dspaSecretName), s3AccessKeyField) != newID || secretValue(t, f, secretPath(ps[0], dspaSecretName), s3SecretKeyField) != newKey {
			t.Errorf("%s has no credentials secret with the new storage's keys", ps[0])
		}
		// Owned by its pipeline server, so whoever deletes the server (the
		// dashboard too) deletes it.
		assertOwnedByDSPA(t, f, secretPath(ps[0], dspaSecretName), ps[0], ps[1])
		// The dashboard Connection to the new storage, with its keys.
		assertConnection(t, f, ps[0], newID, newKey)
		assertOwnedByDSPA(t, f, secretPath(ps[0], connectionSecretName), ps[0], ps[1])
	}
	labels, _ := ensureMap(f.get(dspaPath("juntao-test", legacyDSPAName)), "metadata")["labels"].(map[string]interface{})
	if labels[managedByLabelKey] != managedByLabelValue {
		t.Error("the earlier version's pipeline server did not get the tool label")
	}
	// The hand-made one is untouched, and nothing old was deleted.
	if es := externalStorage(t, f, "hand-made", legacyDSPAName); es["host"] != legacyS3Host() {
		t.Error("a pipeline server this tool did not create was changed")
	}
	noWritesIn(t, f, legacyNamespace)
	noWritesIn(t, f, "hand-made")
	// Every write was dry-run first.
	dry := strings.Join(f.requests, "\n")
	for _, want := range []string{"DRYRUN POST /apis/batch/v1/namespaces/seaweedfs/jobs", "DRYRUN PATCH " + dspaPath("juntao-test", "dspa"), "DRYRUN PATCH " + dspaPath("team-a", "nightly-dspa"), "DRYRUN POST /api/v1/namespaces/juntao-test/secrets"} {
		if !strings.Contains(dry, want) {
			t.Errorf("no dry run %q", want)
		}
	}
	// Each step reported success.
	done := map[string]bool{}
	for _, s := range steps {
		if s.Status == "success" {
			done[s.Step] = true
		}
	}
	for _, s := range []string{"check", "storage", "copy", "repoint"} {
		if !done[s] {
			t.Errorf("step %s did not succeed", s)
		}
	}
}

func TestMigrateJob_RestrictedAndCredentialsOnlyFromSecretRefs(t *testing.T) {
	t.Setenv("RCLONE_IMAGE", "")
	job := migrateJob("SeaweedFS", copyOverwrite, false)
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), legacyKey) || strings.Contains(string(raw), "seaweedfs-key") {
		t.Fatal("a key value is in the Job")
	}
	// The old remote reads the old keys from the Job's source Secret, the
	// new remote the new storage's own.
	for _, want := range []string{
		`{"name":"RCLONE_CONFIG_OLD_ACCESS_KEY_ID","valueFrom":{"secretKeyRef":{"key":"AWS_ACCESS_KEY_ID","name":"seaweedfs-migrate-source"}}}`,
		`{"name":"RCLONE_CONFIG_NEW_ACCESS_KEY_ID","valueFrom":{"secretKeyRef":{"key":"AWS_ACCESS_KEY_ID","name":"seaweedfs-s3-credentials"}}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("job lacks %s", want)
		}
	}
	var j struct {
		Spec struct {
			BackoffLimit          *int `json:"backoffLimit"`
			ActiveDeadlineSeconds int  `json:"activeDeadlineSeconds"`
			Template              struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					RestartPolicy                string `json:"restartPolicy"`
					AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
					Containers                   []struct {
						Image   string   `json:"image"`
						Command []string `json:"command"`
						Env     []struct {
							Name      string          `json:"name"`
							Value     string          `json:"value"`
							ValueFrom json.RawMessage `json:"valueFrom"`
						} `json:"env"`
						Resources struct {
							Limits, Requests map[string]string
						} `json:"resources"`
						SecurityContext map[string]interface{} `json:"securityContext"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 || j.Spec.ActiveDeadlineSeconds <= 0 {
		t.Errorf("backoffLimit/activeDeadlineSeconds = %v/%d", j.Spec.BackoffLimit, j.Spec.ActiveDeadlineSeconds)
	}
	ps := j.Spec.Template.Spec
	if ps.RestartPolicy != "Never" || ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Errorf("pod spec = %+v", ps)
	}
	if j.Spec.Template.Metadata.Labels["app"] != migrateAppLabel {
		t.Errorf("pod labels = %v", j.Spec.Template.Metadata.Labels)
	}
	ctr := ps.Containers[0]
	if ctr.Image != rcloneDefaultImage || !strings.Contains(ctr.Image, "@sha256:") {
		t.Errorf("image = %s", ctr.Image)
	}
	sc := ctr.SecurityContext
	if sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false || sc["seccompProfile"].(map[string]interface{})["type"] != "RuntimeDefault" {
		t.Errorf("securityContext = %v", sc)
	}
	if drop := sc["capabilities"].(map[string]interface{})["drop"].([]interface{}); len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("capabilities = %v", sc["capabilities"])
	}
	if ctr.Resources.Limits["cpu"] == "" || ctr.Resources.Limits["memory"] == "" || ctr.Resources.Requests["cpu"] == "" || ctr.Resources.Requests["memory"] == "" {
		t.Errorf("resources = %+v", ctr.Resources)
	}
	env := map[string]string{}
	refs := 0
	for _, e := range ctr.Env {
		env[e.Name] = e.Value
		if strings.Contains(e.Name, "KEY") {
			if !strings.Contains(string(e.ValueFrom), `"secretKeyRef":{"key":"AWS_`) || e.Value != "" {
				t.Errorf("%s is not a secretKeyRef: %s", e.Name, e.ValueFrom)
			}
			refs++
		}
	}
	if refs != 4 || env["HOME"] != "/tmp" || env["RCLONE_CONFIG_OLD_ENDPOINT"] != "http://minio-service.minio.svc:9000" || env["RCLONE_CONFIG_NEW_ENDPOINT"] != "http://seaweedfs-s3.seaweedfs.svc:8333" ||
		env["RCLONE_CONFIG_OLD_PROVIDER"] != "SeaweedFS" || env["RCLONE_CONFIG_NEW_REGION"] != s3Region || env["COPY_MODE"] != copyOverwrite {
		t.Errorf("env = %v (secret refs %d)", env, refs)
	}
	if got := migrateJob("MinIO", copyIgnoreExisting, false); !strings.Contains(mustJSON(got), `"RCLONE_CONFIG_OLD_PROVIDER","value":"Minio"`) || !strings.Contains(mustJSON(got), `"COPY_MODE","value":"ignore-existing"`) {
		t.Error("a MinIO source does not use rclone's Minio provider, or the copy mode is not set")
	}
	script := strings.Join(ctr.Command, " ")
	for _, want := range []string{"overwrite) rclone copy old: new: --create-empty-src-dirs --metadata ||", "ignore-existing) rclone copy old: new: --create-empty-src-dirs --metadata --ignore-existing ||",
		"rclone lsf old: --dirs-only", "rclone lsf new: --dirs-only", `"missingBuckets":%s`, "rclone check old: new: --one-way --download", "rclone size old: --json", "/dev/termination-log"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(script, " sync ") || strings.Contains(script, "delete") {
		t.Error("the script must never delete objects")
	}
	t.Setenv("RCLONE_IMAGE", "mirror.example.com/rclone@sha256:abc")
	if rcloneImage() != "mirror.example.com/rclone@sha256:abc" {
		t.Error("RCLONE_IMAGE is not honoured")
	}
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The copy Job runs in namespace seaweedfs; the storage's policy admits S3
// from every pod in every namespace, so its pods reach it without a rule
// of their own.
func TestNetworkPolicyAdmitsTheCopyJob(t *testing.T) {
	for _, r := range s3Resources(s3Keys{AccessKey: "a", SecretKey: "b"}) {
		if r.kind != "NetworkPolicy" {
			continue
		}
		raw := mustJSON(r.obj)
		if !strings.Contains(raw, `{"from":[{"namespaceSelector":{}}],"ports":[{"port":8333,"protocol":"TCP"}]}`) {
			t.Errorf("the S3 port is not open to every pod: %s", raw)
		}
		return
	}
	t.Fatal("no NetworkPolicy")
}

func TestCopyResultProblem(t *testing.T) {
	size := func(n, b int64) *copySize { return &copySize{Count: n, Bytes: b} }
	ok := copyResult{Identical: 4, Source: size(4, 4330), Destination: size(4, 4330)}
	sample := base64.StdEncoding.EncodeToString([]byte("pipelines/a.txt\npipelines/b.txt\n"))
	cases := []struct {
		name  string
		r     copyResult
		mode  string
		want  string
		retry bool
	}{
		{"complete", ok, copyOverwrite, "", false},
		{"copy failed", copyResult{Copy: 1, Identical: 4, Source: size(4, 4330), Destination: size(4, 4330)}, copyOverwrite, "rclone copy failed", true},
		{"missing", copyResult{Check: 1, Identical: 3, Missing: 1, Source: size(4, 4330), Destination: size(3, 4000)}, copyOverwrite, "1 object missing", true},
		{"changed during an overwriting copy", copyResult{Check: 1, Identical: 3, Differ: 1, Source: size(4, 4330), Destination: size(4, 4330)}, copyOverwrite, "1 object different", true},
		{"changed after a copy that keeps existing objects", copyResult{Check: 1, Identical: 2, Differ: 2, DifferSample: sample, Source: size(4, 4330), Destination: size(4, 4330)}, copyIgnoreExisting, "2 objects different in the new storage (for example pipelines/a.txt, pipelines/b.txt)", false},
		// The sizes are for the summary only.
		{"no source size is not a problem", copyResult{Identical: 4, Destination: size(4, 4330)}, copyOverwrite, "", false},
		{"no destination size is not a problem", copyResult{Identical: 4, Source: size(4, 4330)}, copyOverwrite, "", false},
		// Objects written to the old storage between the check and the
		// size listing: the check passed, so the copy is verified.
		{"source grew after the check", copyResult{Identical: 4, Source: size(5, 4400), Destination: size(4, 4330)}, copyOverwrite, "", false},
		// The one-way check proves every source object is there; the new
		// storage may hold more, whichever mode.
		{"extra object while nothing uses the new storage", copyResult{Identical: 4, Source: size(4, 4330), Destination: size(5, 4400)}, copyOverwrite, "", false},
		{"extra object once pipeline servers write there", copyResult{Identical: 4, Source: size(4, 4330), Destination: size(5, 4400)}, copyIgnoreExisting, "", false},
	}
	for _, tc := range cases {
		p := tc.r.problem(tc.mode)
		switch {
		case tc.want == "" && p != nil:
			t.Errorf("%s: problem = %+v, want none", tc.name, *p)
		case tc.want != "" && (p == nil || !strings.Contains(p.what, tc.want) || p.retry != tc.retry || (!p.retry && p.fix == "")):
			t.Errorf("%s: problem = %+v, want %q (retry %v)", tc.name, p, tc.want, tc.retry)
		}
	}
	if got := (copyResult{Source: size(4, 4330), Destination: size(6, 5000)}).extraObjects(); got != 2 {
		t.Errorf("extraObjects = %d", got)
	}
	if got := (copyResult{Identical: 3}).summary(); got != "3 objects" {
		t.Errorf("summary without sizes = %q", got)
	}
}

// The check and the size listings run one after the other while the old
// storage may still be written to: an object added in between makes the
// size count larger than the checked count, which does not fail the run.
func TestMigrate_SourceWrittenBetweenCheckAndSizeIsNotAMismatch(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: `{"copy":0,"check":0,"identical":4,"missing":0,"differ":0,"errors":0,"source":{"count":5,"bytes":4400},"destination":{"count":4,"bytes":4330}}`, objectStore: "True"})
	ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
	if !ok || !strings.Contains(msg, "Migrated 5 objects (4400 bytes)") {
		t.Fatalf("got %v: %s", ok, msg)
	}
	if !hasStep(steps, "copy", "OK: 4 objects copied and checked byte for byte") {
		t.Error("the step does not report the checked count")
	}
}

func TestDeleteWithUIDPropagation_SendsPreconditionAndPolicy(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client(), ctx: context.Background()}
	if _, err := deleteWithUIDPropagation(c, migrateJobPath(), "job-uid", "Foreground"); err != nil {
		t.Fatal(err)
	}
	if got["propagationPolicy"] != "Foreground" || ensureMap(got, "preconditions")["uid"] != "job-uid" {
		t.Errorf("DeleteOptions = %v", got)
	}
}

// Extra objects in the new storage are reported, not a failure.
func TestMigrate_ExtraObjectsInTheNewStorageAreReported(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: `{"copy":0,"check":0,"identical":4,"missing":0,"differ":0,"errors":0,"source":{"count":4,"bytes":4330},"destination":{"count":5,"bytes":4400}}`, objectStore: "True"})
	ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
	if !ok {
		t.Fatalf("got %v: %s", ok, msg)
	}
	if !hasStep(steps, "copy", "the new storage holds 1 object more than the old one") {
		t.Error("the extra object is not reported")
	}
}

func TestMigrate_CopyMismatchChangesNothingElse(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: `{"copy":0,"check":1,"identical":3,"missing":1,"differ":0,"errors":0,"source":{"count":4,"bytes":4330},"destination":{"count":3,"bytes":4000}}`, objectStore: "True"})
	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
	if ok || code != "copy_mismatch" || !strings.Contains(msg, "1 object missing") || !strings.Contains(msg, "no pipeline server was moved") || !strings.Contains(msg, "Run Migrate storage again to retry") {
		t.Fatalf("got %v %s: %s", ok, code, msg)
	}
	noChangeToPipelineServers(t, f)
	noWritesIn(t, f, legacyNamespace)
	if f.has(migrateJobPathT) {
		t.Error("the Job was not deleted after its result was read")
	}
}

// While no pipeline server uses the new storage the copy overwrites (an
// object changed in the old storage is copied again); once one does, it
// keeps what is there, and a difference then needs the user.
func TestMigrate_CopyModeFollowsThePipelineServers(t *testing.T) {
	t.Run("nothing uses the new storage", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		sim := &migrateSim{result: copyOK, objectStore: "True"}
		simulateMigrate(f, sim)
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok || sim.mode != copyOverwrite {
			t.Fatalf("got %v %s, copy mode %q", ok, msg, sim.mode)
		}
	})
	t.Run("once a copy was verified, never again overwrite", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		sim := &migrateSim{result: copyOK, objectStore: "True"}
		simulateMigrate(f, sim)
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
			t.Fatal(msg)
		}
		if ensureMap(ensureMap(f.get(s3SecretPath), "metadata"), "annotations")[migrationCopiedAnnotation] == nil {
			t.Fatal("the verified copy is not recorded on the new storage's Secret")
		}
		for _, k := range []fakeKey{
			{gv: dspaAPIGroup, ns: "juntao-test", plural: "datasciencepipelinesapplications", name: legacyDSPAName},
			{gv: dspaAPIGroup, ns: "team-a", plural: "datasciencepipelinesapplications", name: previousDSPAName},
		} {
			delete(f.objects, k)
		}
		sim.mode = ""
		runMigrate(t, c, MigrateOptions{})
		if sim.mode != copyIgnoreExisting {
			t.Errorf("a copy after a verified migration ran with COPY_MODE %q", sim.mode)
		}
	})
	t.Run("a moved pipeline server writes there", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		f.putJSON(dspaPath("juntao-test", legacyDSPAName), `{"metadata":{"labels":`+toolLabelJSON+`,"generation":2,"creationTimestamp":"`+created+`"},
			"spec":{"objectStorage":{"externalStorage":{"host":"`+s3Host()+`","scheme":"http","s3CredentialsSecret":{"secretName":"nigh..."}}}},
			"status":{"conditions":[{"type":"ObjectStoreAvailable","status":"True","observedGeneration":2}]}}`)
		sample := base64.StdEncoding.EncodeToString([]byte("pipelines/model.bin\n"))
		sim := &migrateSim{result: `{"copy":0,"check":1,"identical":3,"missing":0,"differ":1,"errors":0,"differSample":"` + sample + `","source":{"count":4,"bytes":4330},"destination":{"count":9,"bytes":9000}}`, objectStore: "True"}
		simulateMigrate(f, sim)
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
		if ok || code != "copy_mismatch" || sim.mode != copyIgnoreExisting {
			t.Fatalf("got %v %s: %s (copy mode %q)", ok, code, msg, sim.mode)
		}
		for _, want := range []string{"for example pipelines/model.bin", "a retry gives the same result", "copy the old storage's version into the new storage yourself"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message lacks %q: %s", want, msg)
			}
		}
		if strings.Contains(msg, "to retry") {
			t.Errorf("the message offers a retry that cannot help: %s", msg)
		}
	})
	t.Run("a running copy with the other mode is replaced", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		f.putJSON(dspaPath("juntao-test", legacyDSPAName), `{"metadata":{"labels":`+toolLabelJSON+`,"generation":2,"creationTimestamp":"`+created+`"},
			"spec":{"objectStorage":{"externalStorage":{"host":"`+s3Host()+`","scheme":"http","s3CredentialsSecret":{"secretName":"nigh..."}}}},
			"status":{"conditions":[{"type":"ObjectStoreAvailable","status":"True","observedGeneration":2}]}}`)
		f.putJSON(migrateJobPathT, `{"metadata":{"uid":"old-job","labels":`+toolLabelJSON+`},"spec":{"template":{"spec":{"containers":[{"name":"rclone","env":[{"name":"COPY_MODE","value":"overwrite"}]}]}}}}`)
		sim := &migrateSim{result: copyOK, objectStore: "True", jobRunning: true}
		simulateMigrate(f, sim)
		// The first request after the old Job is deleted lets the new one finish.
		next := f.beforeServe
		f.beforeServe = func(method, path string) {
			if method == "POST" && strings.HasSuffix(path, "/jobs") {
				f.mu.Lock()
				sim.jobRunning = false
				f.mu.Unlock()
			}
			next(method, path)
		}
		ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
		if !ok || sim.mode != copyIgnoreExisting || !hasStep(steps, "copy", "with other settings is still running: stopping it") {
			t.Fatalf("got %v %s (copy mode %q)", ok, msg, sim.mode)
		}
		m := strings.Join(f.mutations(), "\n")
		if del, post := strings.Index(m, "DELETE "+migrateJobPathT), strings.Index(m, "POST /apis/batch/v1/namespaces/seaweedfs/jobs"); del < 0 || post < del {
			t.Errorf("the running Job was not deleted before the new copy: %s", m)
		}
	})
}

func TestMigrate_CopyFailureOrNoResultStops(t *testing.T) {
	for name, result := range map[string]string{
		"check found a difference": `{"copy":0,"check":1,"identical":3,"missing":0,"differ":1,"errors":0,"source":{"count":4,"bytes":4330},"destination":{"count":4,"bytes":4330}}`,
		"no termination message":   ``,
	} {
		t.Run(name, func(t *testing.T) {
			fastMigrateTimings(t)
			f, c := newResourceFake(t)
			putMigrationCase(f)
			simulateMigrate(f, &migrateSim{result: result, objectStore: "True"})
			if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); ok {
				t.Fatalf("migrate succeeded: %s", msg)
			}
			noChangeToPipelineServers(t, f)
			noWritesIn(t, f, legacyNamespace)
		})
	}
}

func TestMigrate_PipelineServerNotReadyStops(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "False"})
	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
	if ok || code != "not_ready" || !strings.Contains(msg, "juntao-test/dspa was moved to the new storage, but after") || !strings.Contains(msg, "The old storage is kept") {
		t.Fatalf("got %v %s: %s", ok, code, msg)
	}
	// The second one was not moved; nothing old was deleted.
	if es := externalStorage(t, f, "team-a", previousDSPAName); es["host"] != legacyS3Host() {
		t.Error("the next pipeline server was moved after one failed")
	}
	noWritesIn(t, f, legacyNamespace)

	// Removing the old storage is refused while it does not reach the new one.
	ok, msg, _, _ = runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if ok || !strings.Contains(msg, "still in use") {
		t.Fatalf("remove: %v %s", ok, msg)
	}
	noWritesIn(t, f, legacyNamespace)
}

func TestMigrate_DryRunRejectionChangesNothing(t *testing.T) {
	t.Run("pipeline server", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		f.failDryRun["PATCH "+dspaPath("team-a", previousDSPAName)] = 422
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		if ok || !strings.Contains(msg, "rejects the change to pipeline server team-a/nightly-dspa") || !strings.Contains(msg, "No pipeline server was moved") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		noChangeToPipelineServers(t, f)
	})
	t.Run("copy Job", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		f.failDryRun["POST /apis/batch/v1/namespaces/seaweedfs/jobs"] = 403
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		if ok || !strings.Contains(msg, "rejects the copy Job") || !strings.Contains(msg, "make upgrade") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		if len(f.mutations()) != 0 {
			t.Errorf("writes after a rejected dry run: %v", f.mutations())
		}
	})
}

func TestMigrate_ResumesAfterEachStep(t *testing.T) {
	t.Run("new storage already set up", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
		if !ok {
			t.Fatal(msg)
		}
		if hasMutation(f, "POST /apis/apps/v1/namespaces/seaweedfs/deployments") || hasMutation(f, "PATCH "+s3DeployPath) {
			t.Error("the ready storage was set up again")
		}
		if !hasStep(steps, "storage", "already set up") {
			t.Error("the step does not say it was already done")
		}
	})
	t.Run("a copy from an earlier run still running", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		f.putJSON(migrateJobPathT, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"template":{"spec":{"containers":[{"name":"rclone","env":[{"name":"COPY_MODE","value":"overwrite"}]}]}}}}`)
		sim := &migrateSim{result: copyOK, objectStore: "True", jobRunning: true}
		simulateMigrate(f, sim)
		// The earlier copy is seen running, then finishes: by reads, not a
		// timer, so a slow scheduler cannot make the run find it finished
		// first (that is the "finished copy nobody read" case below).
		finishJobAfterReads(f, sim, 2)
		ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
		if !ok {
			t.Fatal(msg)
		}
		if hasMutation(f, "POST /apis/batch/v1/namespaces/seaweedfs/jobs") {
			t.Error("a second copy was started next to the running one")
		}
		if !hasStep(steps, "copy", "still running: waiting for it") {
			t.Error("the run does not say it waits for the earlier copy")
		}
	})
	t.Run("a finished copy nobody read", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		f.putJSON(migrateJobPathT, `{"metadata":{"uid":"old-job","labels":`+toolLabelJSON+`}}`)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
			t.Fatal(msg)
		}
		m := strings.Join(f.mutations(), "\n")
		del, post := strings.Index(m, "DELETE "+migrateJobPathT), strings.Index(m, "POST /apis/batch/v1/namespaces/seaweedfs/jobs")
		if del < 0 || post < 0 || del > post {
			t.Errorf("the old Job was not deleted before a new copy: %s", m)
		}
	})
	t.Run("copy still running at the bound", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True", jobRunning: true})
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
		if ok || code != "in_progress" || !strings.Contains(msg, "Run Migrate storage again to keep waiting") {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		if !f.has(migrateJobPathT) {
			t.Error("the running copy was deleted")
		}
		noChangeToPipelineServers(t, f)
	})
	t.Run("some pipeline servers already moved", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		f.putJSON(dspaPath("juntao-test", legacyDSPAName), `{"metadata":{"labels":`+toolLabelJSON+`,"generation":2,"creationTimestamp":"`+created+`"},
			"spec":{"objectStorage":{"externalStorage":{"host":"`+s3Host()+`","scheme":"http","s3CredentialsSecret":{"secretName":"nightly-dspa-s3"}}}},
			"status":{"conditions":[{"type":"ObjectStoreAvailable","status":"True","observedGeneration":2}]}}`)
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		if !ok || !strings.Contains(msg, "1 pipeline server now uses it: team-a/nightly-dspa") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		if hasMutation(f, "PATCH "+dspaPath("juntao-test", legacyDSPAName)) {
			t.Error("a moved pipeline server was patched again")
		}
	})
	t.Run("everything done, then removed: nothing to do", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		f.put(dspaPath("hand-made", legacyDSPAName), map[string]interface{}{"metadata": map[string]interface{}{}}) // pointed elsewhere by its owner
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
			t.Fatal(msg)
		}
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true}); !ok {
			t.Fatal(msg)
		}
		before := len(f.mutations())
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		if !ok || !strings.HasPrefix(msg, "Nothing to do") || len(f.mutations()) != before {
			t.Fatalf("re-run: %v %s, writes %v", ok, msg, f.mutations()[before:])
		}
	})
}

func hasExactMutation(f *resourceFake, want string) bool {
	for _, m := range f.mutations() {
		if m == want {
			return true
		}
	}
	return false
}

func hasStep(steps []UpdateStepEvent, id, text string) bool {
	for _, s := range steps {
		if s.Step == id && strings.Contains(s.Message, text) {
			return true
		}
	}
	return false
}

func TestMigrate_Refusals(t *testing.T) {
	// A new storage this tool already set up keeps its keys (they are not
	// the old ones), and the pipeline servers get those.
	t.Run("new storage with other keys keeps them", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putReadyManagedS3(f) // other keys
		id := secretValue(t, f, s3SecretPath, s3AccessKeyField)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
		if !ok {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		if got := secretValue(t, f, s3SecretPath, s3AccessKeyField); got != id {
			t.Errorf("the new storage's keys changed: %q -> %q", id, got)
		}
		if got := secretValue(t, f, secretPath("team-a", dspaSecretName), s3AccessKeyField); got != id {
			t.Errorf("pipeline server key = %q, want %q", got, id)
		}
	})
	t.Run("old server not running", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		f.putJSON(legacyS3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},"status":{"readyReplicas":0}}`)
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
		if ok || code != "prerequisites" || !strings.Contains(msg, "SeaweedFS in namespace 'minio' is not running") {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		if len(f.mutations()) != 0 {
			t.Errorf("writes: %v", f.mutations())
		}
	})
	t.Run("someone else's Job of that name", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		f.putJSON(migrateJobPathT, `{"metadata":{}}`)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		ok, _, code, _ := runMigrate(t, c, MigrateOptions{})
		if ok || code != "not_managed" || !f.has(migrateJobPathT) {
			t.Fatalf("got %v %s", ok, code)
		}
	})
	// Inside Migrate storage a setup that stops names Migrate storage as the
	// way to continue, once: never the Storage card's "re-run setup".
	t.Run("new storage setup stops at the bucket", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		s3BucketCreator = func(*Client, string) error { return errors.New("connection refused") }
		ok, msg, _, steps := runMigrate(t, c, MigrateOptions{})
		if ok || !strings.Contains(msg, "Run Migrate storage again to retry") || strings.Count(msg, "Migrate storage again") != 1 {
			t.Fatalf("got %v: %s", ok, msg)
		}
		for _, s := range steps {
			if strings.Contains(strings.ToLower(s.Message), "re-run setup") {
				t.Errorf("step %s says re-run setup: %s", s.Step, s.Message)
			}
		}
		if !hasStep(steps, "storage", "Run Migrate storage again to retry") {
			t.Error("the failed storage step does not name Migrate storage")
		}
	})
	t.Run("a pipeline server with its own port is listed, not moved", func(t *testing.T) {
		d := dspaInfo{Meta: objectMeta{Name: previousDSPAName, Namespace: "p", Labels: map[string]string{managedByLabelKey: managedByLabelValue}}, Host: legacyS3Host(), Scheme: "http", Port: "9000"}
		if why, ok := repointable(d); ok || !strings.Contains(why, "port") {
			t.Errorf("repointable = %v %q", ok, why)
		}
	})
}

func TestRemoveOldStorage(t *testing.T) {
	migrated := func(t *testing.T) (*resourceFake, *Client) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		delete(f.objects, fakeKey{gv: dspaAPIGroup, ns: "hand-made", plural: "datasciencepipelinesapplications", name: legacyDSPAName})
		f.putJSON(secretPath("juntao-test", legacyDSPASecretName), `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(legacyPostManager, "Update")+`}}`)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
			t.Fatal(msg)
		}
		return f, c
	}
	ownedNS := `{"metadata":{"uid":"ns-uid","labels":` + toolLabelJSON + `,"creationTimestamp":"` + created + `","managedFields":` + managedFieldsJSON(toolFieldManager, "Update") + `},"status":{"phase":"Active"}}`
	t.Run("keeps the namespace unless asked", func(t *testing.T) {
		f, c := migrated(t)
		f.putJSON(legacyNSPath, ownedNS)
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if !ok || !strings.Contains(msg, "kept, as chosen") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		for _, p := range []string{legacyS3DeployPath, legacySvcPath, legacySecretPath, legacyUIRoutePath, legacyS3PVCPath, legacyMinIOPVCPath, legacyS3NPPath, secretPath("juntao-test", legacyDSPASecretName)} {
			if f.has(p) {
				t.Errorf("%s is left", p)
			}
		}
		if !f.has(legacyNSPath) || !f.has(s3DeployPath) {
			t.Error("the namespace or the new storage was deleted")
		}
		// A last copy and check ran first.
		m := strings.Join(f.mutations(), "\n")
		if strings.LastIndex(m, "POST /apis/batch/v1/namespaces/seaweedfs/jobs") > strings.Index(m, "DELETE "+legacyS3DeployPath) {
			t.Error("the old storage was deleted before the last copy")
		}
		if hasExactMutation(f, "DELETE "+legacyNSPath) {
			t.Error("the namespace was deleted")
		}
	})
	t.Run("deletes the namespace the tool created when asked", func(t *testing.T) {
		f, c := migrated(t)
		f.putJSON(legacyNSPath, ownedNS)
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true, DeleteNamespace: true})
		if !ok || !strings.Contains(msg, "is being deleted, with everything else in it") || f.has(legacyNSPath) {
			t.Fatalf("got %v: %s", ok, msg)
		}
	})
	t.Run("keeps a labelled namespace the tool never wrote", func(t *testing.T) {
		f, c := migrated(t)
		// The label alone (putLegacySeaweedFS has no managedFields) is not enough.
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true, DeleteNamespace: true})
		if !ok || !strings.Contains(msg, "show no write by this tool") || !f.has(legacyNSPath) {
			t.Fatalf("got %v: %s", ok, msg)
		}
		if hasExactMutation(f, "DELETE "+legacyNSPath) {
			t.Error("the namespace was deleted")
		}
	})
	t.Run("refused while a pipeline server uses the old storage", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putNewStorageWithLegacyKeys(f)
		simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || code != "prerequisites" || !strings.Contains(msg, "hand-made/dspa") {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		if len(f.mutations()) != 0 {
			t.Errorf("writes: %v", f.mutations())
		}
	})
	t.Run("a new mismatch stops before anything is deleted", func(t *testing.T) {
		f, c := migrated(t)
		simulateMigrate(f, &migrateSim{result: `{"copy":0,"check":1,"identical":4,"missing":1,"differ":0,"errors":0,"source":{"count":5,"bytes":5000},"destination":{"count":4,"bytes":4330}}`, objectStore: "True"})
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || code != "copy_mismatch" || !f.has(legacyS3DeployPath) {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
	})
}

// The live namespace minio: created by OpenShift's controllers' applies at
// creation, labelled later by an earlier version of this tool (a plain
// PATCH, so Go-http-client, at a later time).
const liveMinioNamespace = `{"metadata":{"uid":"ns-uid","creationTimestamp":"2026-08-27T21:30:55Z",
	"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater","kubernetes.io/metadata.name":"minio"},
	"managedFields":[
	  {"manager":"cluster-policy-controller","operation":"Apply","time":"2026-08-27T21:30:55Z","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{}}}},
	  {"manager":"pod-security-admission-label-synchronization-controller","operation":"Apply","time":"2026-08-27T21:30:55Z","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:labels":{}}}},
	  {"manager":"Go-http-client","operation":"Update","time":"2026-10-06T03:49:35Z","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:labels":{"f:app.kubernetes.io/managed-by":{}}}}}]},
	"status":{"phase":"Active"}}`

func TestLegacyNamespaceDeletable(t *testing.T) {
	label := map[string]string{managedByLabelKey: managedByLabelValue}
	at := func(manager, op, time string) []managedFieldsEntry {
		return []managedFieldsEntry{{Manager: manager, Operation: op, Time: time}}
	}
	cases := []struct {
		name   string
		meta   objectMeta
		want   bool
		reason string
	}{
		{"created by this version", objectMeta{CreationTimestamp: created, Labels: label, ManagedFields: at(toolFieldManager, "Update", created)}, true, ""},
		{"labelled later by an earlier version", objectMeta{CreationTimestamp: created, Labels: label, ManagedFields: at(legacyPostManager, "Update", "2026-10-06T03:49:35Z")}, true, ""},
		{"label only", objectMeta{CreationTimestamp: created, Labels: label, ManagedFields: at("kubectl-label", "Update", created)}, false, "show no write by this tool"},
		{"no label", objectMeta{CreationTimestamp: created, ManagedFields: at(toolFieldManager, "Update", created)}, false, "does not carry this tool's label"},
		{"a subresource write only", objectMeta{CreationTimestamp: created, Labels: label, ManagedFields: []managedFieldsEntry{{Manager: toolFieldManager, Operation: "Update", Subresource: "status"}}}, false, "show no write by this tool"},
	}
	for _, tc := range cases {
		got, why := legacyNamespaceDeletable(tc.meta)
		if got != tc.want || !strings.Contains(why, tc.reason) {
			t.Errorf("%s: deletable = %v, reason %q", tc.name, got, why)
		}
	}
}

// The live namespace (a later tool update, OpenShift's controllers at
// creation) is offered for deletion and deleted only after consent.
func TestRemoveOldStorage_LiveNamespaceIsDeletableAfterConsent(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	f.putJSON(legacyNSPath, liveMinioNamespace)
	delete(f.objects, fakeKey{gv: dspaAPIGroup, ns: "hand-made", plural: "datasciencepipelinesapplications", name: legacyDSPAName})
	f.putJSON("/apis/apps/v1/namespaces/minio/deployments/notes-app", `{"metadata":{"labels":{"app":"notes"}}}`)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
		t.Fatal(msg)
	}
	status, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if l := status.LegacyStorage; !l.NamespaceDeletable || strings.Join(l.OtherObjects, ",") != "Deployment notes-app" {
		t.Fatalf("legacy = %+v", l)
	}
	// Not ticked: kept.
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true}); !ok || !strings.Contains(msg, "kept, as chosen") || !f.has(legacyNSPath) {
		t.Fatalf("got %v: %s", ok, msg)
	}
}

func TestRemoveOldStorage_LiveNamespaceDeletedWhenTicked(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	f.putJSON(legacyNSPath, liveMinioNamespace)
	delete(f.objects, fakeKey{gv: dspaAPIGroup, ns: "hand-made", plural: "datasciencepipelinesapplications", name: legacyDSPAName})
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
		t.Fatal(msg)
	}
	ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true, DeleteNamespace: true})
	if !ok || !hasExactMutation(f, "DELETE "+legacyNSPath) || f.has(legacyNSPath) {
		t.Fatalf("got %v: %s", ok, msg)
	}
}

func TestRemoveOldStorage_NamespaceWithoutLabelIsKept(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	f.putJSON(legacyNSPath, strings.Replace(liveMinioNamespace, `"app.kubernetes.io/managed-by":"rhoai-nightly-updater",`, "", 1))
	delete(f.objects, fakeKey{gv: dspaAPIGroup, ns: "hand-made", plural: "datasciencepipelinesapplications", name: legacyDSPAName})
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
		t.Fatal(msg)
	}
	ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true, DeleteNamespace: true})
	if !ok || !strings.Contains(msg, "does not carry this tool's label") || hasExactMutation(f, "DELETE "+legacyNSPath) {
		t.Fatalf("got %v: %s", ok, msg)
	}
}

// DSPAs named like an earlier version's, told apart by who owns their
// object storage fields.
func TestManagedByTool_FieldOwnership(t *testing.T) {
	storage := `{"f:spec":{"f:objectStorage":{"f:externalStorage":{"f:host":{},"f:s3CredentialsSecret":{"f:secretName":{}}}}}}`
	entry := func(manager, op, time, fields string) string {
		return `{"manager":"` + manager + `","operation":"` + op + `","time":"` + time + `","fieldsType":"FieldsV1","fieldsV1":` + fields + `}`
	}
	dspa := func(entries ...string) dspaInfo {
		var meta objectMeta
		raw := `{"name":"dspa","namespace":"juntao-test","creationTimestamp":"2026-09-02T22:32:19Z","managedFields":[` + strings.Join(entries, ",") + `]}`
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		return dspaInfo{Meta: meta, Host: legacyS3Host(), Scheme: "http", SecretName: legacyDSPASecretName}
	}
	dspo := entry("manager", "Update", "2026-09-02T22:33:01Z", `{"f:metadata":{"f:finalizers":{}}}`)
	annotate := entry("kubectl-annotate", "Update", "2026-10-06T03:00:00Z", `{"f:metadata":{"f:annotations":{}}}`)
	cases := []struct {
		name string
		d    dspaInfo
		want bool
	}{
		// The live juntao-test/dspa.
		{"created by an earlier version", dspa(entry(legacyPostManager, "Update", "2026-09-02T22:32:19Z", storage), dspo, annotate), true},
		{"re-written later by the tool", dspa(entry(legacyPostManager, "Update", "2026-10-20T08:00:00Z", storage), dspo, annotate), true},
		{"created by server-side apply", dspa(entry(toolFieldManager, "Apply", "2026-11-01T08:00:00Z", storage)), true},
		// odh-dashboard: its Node backend sends no User-Agent (empty
		// manager), its Go BFF forwards the browser's ("Mozilla").
		{"created by the dashboard's Node backend", dspa(entry("", "Update", "2026-09-02T22:32:19Z", storage), dspo), false},
		{"created by the dashboard's BFF", dspa(entry("Mozilla", "Update", "2026-09-02T22:32:19Z", storage), dspo), false},
		{"the tool wrote only metadata", dspa(entry(legacyPostManager, "Update", "2026-09-02T22:32:19Z", `{"f:metadata":{"f:labels":{}}}`), entry("Mozilla", "Update", "2026-09-03T00:00:00Z", storage)), false},
	}
	for _, tc := range cases {
		if got := tc.d.managedByTool(); got != tc.want {
			t.Errorf("%s: managedByTool = %v", tc.name, got)
		}
	}
}

// A dashboard-created DSPA with the earlier version's names is listed for
// the user, never patched.
func TestMigrate_DashboardCreatedDSPAIsListedNotPatched(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	f.putJSON(dspaPath("dash-project", legacyDSPAName), `{"metadata":{"generation":1,"creationTimestamp":"`+created+`","managedFields":[{"manager":"Mozilla","operation":"Update","time":"`+created+`","fieldsType":"FieldsV1","fieldsV1":`+ownedContentJSON+`}]},
		"spec":{"objectStorage":{"externalStorage":{"host":"`+legacyS3Host()+`","scheme":"http","bucket":"pipelines","s3CredentialsSecret":{"secretName":"dashboard-dspa-secret","accessKey":"AWS_ACCESS_KEY_ID","secretKey":"AWS_SECRET_ACCESS_KEY"}}}}}`)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
	if !ok || !strings.Contains(msg, "dash-project/dspa (not created by this tool)") {
		t.Fatalf("got %v: %s", ok, msg)
	}
	noChangeToPipelineServers(t, f)
}

func TestGetResourcesStatus_MigrationPlan(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	for _, ns := range []string{"juntao-test", "team-a", "hand-made"} {
		putDSProject(f, ns)
	}
	status, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	l := status.LegacyStorage
	if l == nil || l.Stage != "migrate" || strings.Join(l.Repoint, ",") != "juntao-test/dspa,team-a/nightly-dspa" ||
		strings.Join(l.NotMoved, ",") != "hand-made/dspa (not created by this tool)" || l.NamespaceDeletable || l.NamespaceKeptReason == "" {
		t.Fatalf("legacy = %+v", l)
	}

	// After the migration: the stage is "remove", blocked while the
	// hand-made server still uses the old storage.
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
		t.Fatal(msg)
	}
	status, err = GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	l = status.LegacyStorage
	if l.Stage != "remove" || len(l.Repoint) != 0 || !strings.Contains(l.RemoveBlockedReason, "hand-made/dspa") {
		t.Fatalf("legacy = %+v", l)
	}
}

// An empty bucket has no object for the check to miss: the bucket lists
// are compared, and a missing bucket is a copy problem.
func TestCopyResult_MissingBuckets(t *testing.T) {
	ok := copyResult{Identical: 4}
	if p := ok.problem(copyOverwrite); p != nil {
		t.Fatalf("problem = %+v", p)
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{1, "1 bucket of the old storage is missing in the new storage"}, {-1, "the bucket lists of the two storages could not be read"}} {
		r := copyResult{Identical: 4, MissingBuckets: tc.n}
		if p := r.problem(copyIgnoreExisting); p == nil || p.what != tc.want || !p.retry {
			t.Errorf("%d: problem = %+v", tc.n, p)
		}
	}
}

// The removal is offered only on proof: a recorded verified copy, and every
// pipeline server of this tool on the new storage reaching it.
func TestLegacyStorageState_RemoveNeedsProof(t *testing.T) {
	l := &legacyStorage{address: storageAddress{service: legacyService, namespace: legacyNamespace}}
	moved := func(storeReady string) dspaInfo {
		return dspaInfo{
			Meta: objectMeta{Name: previousDSPAName, Namespace: "team-a", Labels: map[string]string{managedByLabelKey: managedByLabelValue}, Generation: 2},
			Host: s3Host(), ObjectStore: &dspaCondition{Status: storeReady, ObservedGeneration: 2},
		}
	}
	for _, tc := range []struct {
		name   string
		copied bool
		dspas  []dspaInfo
		want   string
	}{
		{"verified, moved server reaches it", true, []dspaInfo{moved("True")}, "remove"},
		{"no verified copy recorded", false, []dspaInfo{moved("True")}, "migrate"},
		{"a moved server does not reach the new storage", true, []dspaInfo{moved("False")}, "migrate"},
	} {
		if got := legacyStorageState(l, tc.dspas, true, tc.copied).Stage; got != tc.want {
			t.Errorf("%s: stage %q, want %q", tc.name, got, tc.want)
		}
	}
}
