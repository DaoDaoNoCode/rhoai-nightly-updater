package cluster

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	minioNSPath     = "/api/v1/namespaces/minio"
	minioDeployPath = "/apis/apps/v1/namespaces/minio/deployments/minio"
	minioPVCPath    = "/api/v1/namespaces/minio/persistentvolumeclaims/minio-pvc"
	dspaListPath    = "/apis/datasciencepipelinesapplications.opendatahub.io/v1/datasciencepipelinesapplications"
	created         = "2026-08-27T21:30:55Z"
)

// fastMinIOTimings shortens the MinIO waits and stubs bucket creation.
func fastMinIOTimings(t *testing.T) *int {
	t.Helper()
	oldReady, oldPoll, oldDel, oldDelPoll, oldBucket, oldRetry := MinIOReadyTimeout, MinIOReadyPoll, MinIODeleteTimeout, MinIODeletePoll, minioBucketCreator, minioBucketRetryDelay
	minioBucketRetryDelay = time.Millisecond
	MinIOReadyTimeout, MinIOReadyPoll = 300*time.Millisecond, 10*time.Millisecond
	MinIODeleteTimeout, MinIODeletePoll = 100*time.Millisecond, 10*time.Millisecond
	calls := 0
	minioBucketCreator = func(*Client, string) error { calls++; return nil }
	t.Cleanup(func() {
		MinIOReadyTimeout, MinIOReadyPoll, MinIODeleteTimeout, MinIODeletePoll, minioBucketCreator, minioBucketRetryDelay = oldReady, oldPoll, oldDel, oldDelPoll, oldBucket, oldRetry
	})
	t.Setenv("MINIO_IMAGE", "")
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "")
	return &calls
}

// managedFieldsJSON builds a managedFields array with one entry at the
// creation time.
func managedFieldsJSON(manager, op string) string {
	return `[{"manager":"` + manager + `","operation":"` + op + `","time":"` + created + `"}]`
}

func putNamespace(f *resourceFake, labels, manager string) {
	f.putJSON(minioNSPath, `{"metadata":{"labels":`+labels+`,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(manager, "Update")+`},"status":{"phase":"Active"}}`)
}

func putMinIODeployment(f *resourceFake, manager string, ready int) {
	f.putJSON(minioDeployPath, `{"metadata":{"creationTimestamp":"`+created+`","generation":1,"managedFields":`+managedFieldsJSON(manager, "Apply")+`},"status":{"observedGeneration":1,"replicas":1,"readyReplicas":`+itoa(ready)+`,"updatedReplicas":1}}`)
}

// readyAfterApply makes the MinIO deployment report a ready rollout of the
// current generation, like the deployment controller would.
func readyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "deployments" && k.name == "minio" {
			obj["status"] = map[string]interface{}{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1}
		}
	}
}

func notReadyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "deployments" && k.name == "minio" {
			obj["status"] = map[string]interface{}{"observedGeneration": 1, "replicas": 1, "readyReplicas": 0, "updatedReplicas": 1}
		}
	}
}

func putMinIOPod(f *resourceFake, name, image, reason, message string) {
	f.putJSON("/api/v1/namespaces/minio/pods/"+name, `{"metadata":{"labels":{"app":"minio"}},"spec":{"containers":[{"name":"minio","image":"`+image+`"}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],
		"containerStatuses":[{"name":"minio","state":{"waiting":{"reason":"`+reason+`","message":"`+message+`"}}}]}}`)
}

func hasMutation(f *resourceFake, prefix string) bool {
	for _, m := range f.mutations() {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

func TestSetupMinIO_FreshClusterLabelsEverythingAndWaitsForReady(t *testing.T) {
	bucketCalls := fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)

	resp, err := SetupMinIO(c)
	if err != nil || !resp.Success {
		t.Fatalf("setup failed: %v %+v", err, resp)
	}
	if *bucketCalls != 1 {
		t.Errorf("bucket created %d times, want 1", *bucketCalls)
	}
	for _, path := range []string{
		minioNSPath, minioPVCPath, minioDeployPath,
		"/api/v1/namespaces/minio/secrets/minio-secret",
		"/api/v1/namespaces/minio/services/minio-service",
		"/apis/route.openshift.io/v1/namespaces/minio/routes/minio-ui",
	} {
		obj := f.get(path)
		if obj == nil {
			t.Fatalf("%s was not created", path)
		}
		labels, _ := obj["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
		if labels[managedByLabelKey] != managedByLabelValue {
			t.Errorf("%s lacks the ownership label: %v", path, labels)
		}
	}
	nsFields := f.get(minioNSPath)["metadata"].(map[string]interface{})["managedFields"].([]interface{})
	if nsFields[0].(map[string]interface{})["manager"] != toolFieldManager {
		t.Errorf("namespace POST did not set fieldManager: %v", nsFields)
	}

	raw, _ := json.Marshal(f.get(minioDeployPath))
	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext map[string]interface{} `json:"securityContext"`
					Containers      []struct {
						Image           string                 `json:"image"`
						Args            []string               `json:"args"`
						SecurityContext map[string]interface{} `json:"securityContext"`
						Env             []struct {
							Name string `json:"name"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(raw, &deploy)
	ctr := deploy.Spec.Template.Spec.Containers[0]
	if ctr.Image != minioDefaultImage || !strings.Contains(ctr.Image, "@sha256:") || strings.Contains(ctr.Image, "RELEASE.2019") {
		t.Errorf("image = %q, want the digest-pinned patched MinIO", ctr.Image)
	}
	if strings.Join(ctr.Args, " ") != "server /data --console-address :9090" {
		t.Errorf("args = %v; the console must have its own port", ctr.Args)
	}
	if f.has("/apis/route.openshift.io/v1/namespaces/minio/routes/minio-api") {
		t.Error("the S3 API must not be exposed through a Route")
	}
	ui, _ := json.Marshal(f.get("/apis/route.openshift.io/v1/namespaces/minio/routes/minio-ui"))
	if !strings.Contains(string(ui), `"targetPort":"ui"`) {
		t.Errorf("console Route must target the console port only: %s", ui)
	}
	if _, fixed := ctr.SecurityContext["runAsUser"]; fixed || deploy.Spec.Template.Spec.SecurityContext != nil {
		t.Errorf("a fixed UID would conflict with restricted-v2: %v", ctr.SecurityContext)
	}
	envs := map[string]bool{}
	for _, e := range ctr.Env {
		envs[e.Name] = true
	}
	if !envs["MINIO_ROOT_USER"] || !envs["MINIO_ROOT_PASSWORD"] || envs["MINIO_ACCESS_KEY"] {
		t.Errorf("env %v: want only the MINIO_ROOT_* variables", envs)
	}
}

func TestSetupMinIO_ImageOverride(t *testing.T) {
	fastMinIOTimings(t)
	t.Setenv("MINIO_IMAGE", "mirror.example.com/minio@sha256:abc")
	f, c := newResourceFake(t)
	readyAfterApply(f)
	if resp, _ := SetupMinIO(c); !resp.Success {
		t.Fatalf("setup failed: %+v", resp)
	}
	raw, _ := json.Marshal(f.get(minioDeployPath))
	if !strings.Contains(string(raw), "mirror.example.com/minio@sha256:abc") {
		t.Errorf("MINIO_IMAGE override not applied: %s", raw)
	}
}

func TestSetupMinIO_RefusesNamespaceItDidNotCreate(t *testing.T) {
	for name, manager := range map[string]string{"oc create": "kubectl-create", "console": "Mozilla"} {
		t.Run(name, func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			putNamespace(f, `{}`, manager)
			resp, _ := SetupMinIO(c)
			if resp.Success || resp.ErrorCode != "not_managed" {
				t.Fatalf("want not_managed refusal, got %+v", resp)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("refusal must not change anything, got %v", m)
			}
		})
	}
}

func TestSetupMinIO_GoClientNamespaceWithoutToolDeploymentIsNotClaimed(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	putNamespace(f, `{}`, legacyPostManager)
	putMinIODeployment(f, "kubectl-client-side-apply", 1)
	if resp, _ := SetupMinIO(c); resp.Success || resp.ErrorCode != "not_managed" {
		t.Fatalf("want not_managed, got %+v", resp)
	}
}

func TestSetupMinIO_AdoptsNamespaceFromEarlierVersion(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	putNamespace(f, `{"kubernetes.io/metadata.name":"minio"}`, legacyPostManager)
	putMinIODeployment(f, toolFieldManager, 0)
	readyAfterApply(f)

	resp, _ := SetupMinIO(c)
	if !resp.Success {
		t.Fatalf("legacy setup re-run failed: %+v", resp)
	}
	labels := f.get(minioNSPath)["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if labels[managedByLabelKey] != managedByLabelValue || labels["kubernetes.io/metadata.name"] != "minio" {
		t.Errorf("namespace labels after adoption: %v", labels)
	}
	for _, m := range f.mutations() {
		if m == "POST /api/v1/namespaces" {
			t.Error("an existing namespace must not be re-created")
		}
	}
}

func TestSetupMinIO_StopsEarlyOnImagePullBackOff(t *testing.T) {
	fastMinIOTimings(t)
	MinIOReadyTimeout = 5 * time.Second
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	putMinIOPod(f, "minio-new", minioDefaultImage, "ImagePullBackOff", "Back-off pulling image: unauthorized: access to the requested resource is not authorized")

	start := time.Now()
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "not_ready" {
		t.Fatalf("want not_ready failure, got %+v", resp)
	}
	if !strings.Contains(resp.Message, "ImagePullBackOff") || !strings.Contains(resp.Message, "unauthorized") {
		t.Errorf("message must carry the pull error: %q", resp.Message)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("setup waited %s instead of stopping at the terminal pull error", time.Since(start))
	}
}

func TestSetupMinIO_IgnoresPodsOfAnOlderImage(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	putMinIOPod(f, "minio-old", "quay.io/minio/minio:latest", "ImagePullBackOff", "unauthorized")
	putMinIOPod(f, "minio-new", minioDefaultImage, "ContainerCreating", "")

	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "timeout" {
		t.Fatalf("the old pod must not decide the result; got %+v", resp)
	}
	if !strings.Contains(resp.Message, "ContainerCreating") || strings.Contains(resp.Message, "unauthorized") {
		t.Errorf("message = %q", resp.Message)
	}
}

func TestSetupMinIO_NeverReportsSuccessWhileNotReady(t *testing.T) {
	bucketCalls := fastMinIOTimings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "timeout" {
		t.Fatalf("want timeout failure, got %+v", resp)
	}
	if *bucketCalls != 0 {
		t.Error("bucket creation must wait for a ready MinIO")
	}
}

func TestSetupMinIO_ApplyFailureReportsPartialProgress(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	f.fail["POST /apis/apps/v1/namespaces/minio/deployments"] = 500
	resp, _ := SetupMinIO(c)
	if resp.Success || !strings.Contains(resp.Message, "Failed to apply Deployment") || !strings.Contains(resp.Message, "PVC, Secret") {
		t.Fatalf("want a partial-progress failure, got %+v", resp)
	}
}

func TestSetupMinIO_TerminatingNamespace(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	f.putJSON(minioNSPath, `{"metadata":{"labels":{"`+managedByLabelKey+`":"`+managedByLabelValue+`"},"deletionTimestamp":"`+created+`"},"status":{"phase":"Terminating"}}`)
	if resp, _ := SetupMinIO(c); resp.Success || resp.ErrorCode != "terminating" {
		t.Fatalf("want terminating, got %+v", resp)
	}
}

func TestGetMinIOStatus_ReportsWaitingReasonAndTerminalFlag(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, `{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}`, toolFieldManager)
	putMinIODeployment(f, toolFieldManager, 0)
	putMinIOPod(f, "minio-1", "quay.io/minio/minio:latest", "ImagePullBackOff", "Back-off pulling image quay.io/minio/minio:latest")
	f.putJSON(minioPVCPath, `{"metadata":{"labels":{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}}}`)

	st := getMinIOStatus(c)
	if !st.Deployed || st.Ready || !st.ManagedByTool {
		t.Fatalf("state = %+v", st)
	}
	if st.WaitingReason != "ImagePullBackOff" || !st.TerminalError || !strings.HasPrefix(st.Message, "ImagePullBackOff: Back-off pulling image") {
		t.Errorf("waiting reason not surfaced: %+v", st)
	}
	if len(st.DataPVCs) != 1 || st.DataPVCs[0] != "minio-pvc" {
		t.Errorf("DataPVCs = %v", st.DataPVCs)
	}
}

func TestGetMinIOStatus_ContainerCreatingIsNotTerminal(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, `{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}`, toolFieldManager)
	putMinIODeployment(f, toolFieldManager, 0)
	putMinIOPod(f, "minio-1", minioDefaultImage, "ContainerCreating", "")
	st := getMinIOStatus(c)
	if st.WaitingReason != "ContainerCreating" || st.TerminalError {
		t.Errorf("state = %+v", st)
	}
}

func TestGetMinIOStatus_UnmanagedNamespace(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, `{}`, "kubectl-create")
	putMinIODeployment(f, "kubectl-client-side-apply", 1)
	st := getMinIOStatus(c)
	if st.ManagedByTool || st.SetupBlockedReason == "" || st.TeardownBlockedReason == "" {
		t.Errorf("an unmanaged namespace must block setup and teardown: %+v", st)
	}
	if len(st.DataPVCs) != 0 {
		t.Errorf("DataPVCs must be empty for unmanaged MinIO: %v", st.DataPVCs)
	}
}

func TestGetMinIOStatus_LegacyNamespaceIsManaged(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, `{}`, legacyPostManager)
	putMinIODeployment(f, toolFieldManager, 1)
	if st := getMinIOStatus(c); !st.ManagedByTool || !st.Ready || st.TeardownBlockedReason != "" {
		t.Errorf("state = %+v", st)
	}
}

func TestTeardownMinIO(t *testing.T) {
	managedNS := func(f *resourceFake) {
		putNamespace(f, `{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}`, toolFieldManager)
		putMinIODeployment(f, toolFieldManager, 1)
		f.putJSON(minioPVCPath, `{"metadata":{"labels":{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}}}`)
	}
	dspa := func(f *resourceFake, ns, name, host string, terminating bool) {
		del := ""
		if terminating {
			del = `,"deletionTimestamp":"` + created + `","finalizers":["` + dspaFinalizer + `"]`
		}
		f.putJSON("/apis/"+dspaAPIGroup+"/namespaces/"+ns+"/datasciencepipelinesapplications/"+name,
			`{"metadata":{"creationTimestamp":"`+created+`"`+del+`},"spec":{"objectStorage":{"externalStorage":{"host":"`+host+`"}}}}`)
	}
	cases := []struct {
		name     string
		setup    func(f *resourceFake)
		wantOK   bool
		wantCode string
		wantGone bool // the tool's Deployment and PVC were deleted
		wantMsg  string
	}{
		{name: "not deployed", setup: func(*resourceFake) {}, wantOK: true},
		{name: "namespace not created by the tool", setup: func(f *resourceFake) {
			putNamespace(f, `{}`, "kubectl-create")
			putMinIODeployment(f, toolFieldManager, 1)
		}, wantCode: "not_managed"},
		{name: "managed", setup: managedNS, wantOK: true, wantGone: true, wantMsg: "oc delete project minio"},
		{name: "legacy namespace", setup: func(f *resourceFake) {
			putNamespace(f, `{}`, legacyPostManager)
			putMinIODeployment(f, toolFieldManager, 0)
			f.putJSON(minioPVCPath, `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`}}`)
		}, wantOK: true, wantGone: true, wantMsg: "minio-pvc"},
		{name: "pipeline server created by someone else still uses it", setup: func(f *resourceFake) {
			managedNS(f)
			dspa(f, "team-a", "dspa", minioS3Host(), false)
		}, wantCode: "prerequisites", wantMsg: "team-a/dspa"},
		{name: "terminating pipeline server blocks until gone", setup: func(f *resourceFake) {
			managedNS(f)
			dspa(f, "team-a", "dspa", minioS3Host(), true)
		}, wantCode: "prerequisites", wantMsg: "still being deleted: team-a/dspa"},
		{name: "pipeline server inside the minio namespace", setup: func(f *resourceFake) {
			managedNS(f)
			dspa(f, "minio", "x", "s3.amazonaws.com", false)
		}, wantCode: "prerequisites", wantMsg: "minio/x"},
		{name: "namespace being deleted by someone else", setup: func(f *resourceFake) {
			f.putJSON(minioNSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"deletionTimestamp":"`+created+`"},"status":{"phase":"Terminating",
				"conditions":[{"type":"NamespaceContentRemaining","status":"True","message":"Some resources are remaining: pods. has 1 resource instances"}]}}`)
		}, wantCode: "in_progress", wantMsg: "Some resources are remaining"},
		{name: "pipeline server on other storage does not block", setup: func(f *resourceFake) {
			managedNS(f)
			dspa(f, "team-a", "dspa", "s3.amazonaws.com", false)
		}, wantOK: true, wantGone: true},
		{name: "DSPA CRD missing", setup: func(f *resourceFake) {
			managedNS(f)
			f.fail["GET "+dspaListPath] = 404
		}, wantOK: true, wantGone: true},
		{name: "DSPA list error", setup: func(f *resourceFake) {
			managedNS(f)
			f.fail["GET "+dspaListPath] = 500
		}},
		{name: "delete error", setup: func(f *resourceFake) {
			managedNS(f)
			f.fail["DELETE "+minioPVCPath] = 500
		}, wantCode: "partial_failure", wantMsg: "PersistentVolumeClaim minio-pvc"},
		{name: "read forbidden", setup: func(f *resourceFake) {
			managedNS(f)
			f.fail["GET "+minioDeployPath] = 403
		}, wantCode: "partial_failure", wantMsg: "Deployment minio: cannot read it"},
		{name: "PVC still terminating", setup: func(f *resourceFake) {
			managedNS(f)
			f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`,"finalizers":["kubernetes.io/pvc-protection"]}}`)
		}, wantCode: "in_progress", wantMsg: "still terminating"},
		{name: "already removed, namespace left", setup: func(f *resourceFake) {
			putNamespace(f, toolLabelJSON, toolFieldManager)
		}, wantOK: true, wantMsg: "No MinIO objects created by this tool remain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			tc.setup(f)
			resp, err := TeardownMinIO(c)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Success != tc.wantOK || (tc.wantCode != "" && resp.ErrorCode != tc.wantCode) {
				t.Fatalf("got %+v", resp)
			}
			for _, m := range f.mutations() {
				if m == "DELETE "+minioNSPath {
					t.Fatal("teardown must never delete the minio namespace")
				}
				if strings.Contains(m, "customresourcedefinitions") || strings.Contains(m, "endpointslices") {
					t.Errorf("unexpected request %s", m)
				}
			}
			if gone := !f.has(minioDeployPath) && !f.has(minioPVCPath); gone != tc.wantGone && tc.wantGone {
				t.Errorf("tool objects deleted = %v, want %v (%v)", gone, tc.wantGone, f.mutations())
			}
			if tc.wantMsg != "" && !strings.Contains(resp.Message, tc.wantMsg) {
				t.Errorf("message %q lacks %q", resp.Message, tc.wantMsg)
			}
			if tc.wantOK && f.has(minioNSPath) && !strings.Contains(resp.Message, "oc delete project minio") {
				t.Errorf("message must say the namespace was kept and how to delete it: %q", resp.Message)
			}
		})
	}
}

// A legacy namespace is recognised by its Deployment, so teardown labels it
// before deleting that Deployment; setup can then still reuse it.
func TestTeardownMinIO_LegacyNamespaceStaysManaged(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	putNamespace(f, `{}`, legacyPostManager)
	putMinIODeployment(f, toolFieldManager, 1)
	if resp, _ := TeardownMinIO(c); !resp.Success {
		t.Fatalf("teardown = %+v", resp)
	}
	if st := getMinIOStatus(c); !st.ManagedByTool || st.SetupBlockedReason != "" {
		t.Errorf("after teardown the kept namespace must stay usable by setup: %+v", st)
	}
}

func TestTeardownMinIO_DeleteUsesUIDPrecondition(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	putNamespace(f, toolLabelJSON, toolFieldManager)
	putMinIODeployment(f, toolFieldManager, 1)
	// The ownership check sees a different object than the one the DELETE
	// reaches, as if the Deployment had been recreated in between.
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "deployments" && k.name == "minio" {
			obj["metadata"].(map[string]interface{})["uid"] = "uid-seen-by-check"
		}
	}
	resp, _ := TeardownMinIO(c)
	if resp.Success || !f.has(minioDeployPath) || !strings.Contains(resp.Message, "Deployment minio") {
		t.Fatalf("a Deployment with a different UID must not be deleted: %+v", resp)
	}
}
