package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	minioNSPath = "/api/v1/namespaces/minio"
	// The MinIO an earlier version deployed.
	minioDeployPath = "/apis/apps/v1/namespaces/minio/deployments/minio"
	minioPVCPath    = "/api/v1/namespaces/minio/persistentvolumeclaims/minio-pvc"
	// The S3 storage (SeaweedFS).
	s3DeployPath = "/apis/apps/v1/namespaces/minio/deployments/seaweedfs"
	s3PVCPath    = "/api/v1/namespaces/minio/persistentvolumeclaims/seaweedfs-pvc"
	dspaListPath = "/apis/datasciencepipelinesapplications.opendatahub.io/v1/datasciencepipelinesapplications"
	created      = "2026-08-27T21:30:55Z"
)

// fastMinIOTimings shortens the MinIO waits and stubs bucket creation.
func fastMinIOTimings(t *testing.T) *int {
	t.Helper()
	oldReady, oldPoll, oldDel, oldDelPoll, oldBucket, oldRetry := MinIOReadyTimeout, MinIOReadyPoll, MinIODeleteTimeout, MinIODeletePoll, minioBucketCreator, minioBucketRetryDelay
	oldPodsGone := MinIOPodsGoneTimeout
	MinIOPodsGoneTimeout = 100 * time.Millisecond
	minioBucketRetryDelay = time.Millisecond
	MinIOReadyTimeout, MinIOReadyPoll = 300*time.Millisecond, 10*time.Millisecond
	MinIODeleteTimeout, MinIODeletePoll = 100*time.Millisecond, 10*time.Millisecond
	calls := 0
	minioBucketCreator = func(*Client, string) error { calls++; return nil }
	t.Cleanup(func() {
		MinIOReadyTimeout, MinIOReadyPoll, MinIODeleteTimeout, MinIODeletePoll, minioBucketCreator, minioBucketRetryDelay = oldReady, oldPoll, oldDel, oldDelPoll, oldBucket, oldRetry
		MinIOPodsGoneTimeout = oldPodsGone
	})
	t.Setenv("MINIO_IMAGE", "")
	t.Setenv("SEAWEEDFS_IMAGE", "")
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

// putMinIODeployment puts the MinIO Deployment of an earlier version,
// created by server-side apply with manager.
func putMinIODeployment(f *resourceFake, manager string, ready int) {
	f.putJSON(minioDeployPath, `{"metadata":{"creationTimestamp":"`+created+`","generation":1,"managedFields":`+managedFieldsJSON(manager, "Apply")+`},
		"spec":{"template":{"spec":{"containers":[{"name":"minio","image":"quay.io/hummingbird-community/minio@sha256:25268b5a"}]}}},
		"status":{"observedGeneration":1,"replicas":1,"readyReplicas":`+itoa(ready)+`,"updatedReplicas":1}}`)
}

// putS3Deployment puts the tool's SeaweedFS Deployment.
func putS3Deployment(f *resourceFake, ready int) {
	f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},
		"spec":{"template":{"spec":{"containers":[{"name":"seaweedfs","image":"`+seaweedfsDefaultImage+`"}]}}},
		"status":{"observedGeneration":1,"replicas":1,"readyReplicas":`+itoa(ready)+`,"updatedReplicas":1}}`)
}

// putS3Serving puts minio-service and Route minio-ui as setup applies them:
// they send traffic to SeaweedFS.
func putS3Serving(f *resourceFake) {
	f.putJSON(minioSvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"selector":{"app":"seaweedfs"},"clusterIP":"172.30.10.20",
		"ports":[{"name":"api","port":9000,"targetPort":8333},{"name":"ui","port":9090,"targetPort":23646}]}}`)
	f.putJSON(minioUIRoute, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"host":"minio-ui-minio.apps.example.com","to":{"kind":"Service","name":"minio-service"},"port":{"targetPort":"ui"}}}`)
}

// readyAfterApply makes the SeaweedFS deployment report a ready rollout of
// the current generation, like the deployment controller would.
func readyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "deployments" && k.name == s3Deployment {
			obj["status"] = map[string]interface{}{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1}
		}
	}
}

func notReadyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "deployments" && k.name == s3Deployment {
			obj["status"] = map[string]interface{}{"observedGeneration": 1, "replicas": 1, "readyReplicas": 0, "updatedReplicas": 1}
		}
	}
}

func putMinIOPod(f *resourceFake, name, image, reason, message string) {
	f.putJSON("/api/v1/namespaces/minio/pods/"+name, `{"metadata":{"labels":{"app":"minio"}},"spec":{"containers":[{"name":"minio","image":"`+image+`"}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],
		"containerStatuses":[{"name":"minio","state":{"waiting":{"reason":"`+reason+`","message":"`+message+`"}}}]}}`)
}

func putS3Pod(f *resourceFake, name, image, reason, message string) {
	f.putJSON("/api/v1/namespaces/minio/pods/"+name, `{"metadata":{"labels":{"app":"seaweedfs"}},"spec":{"containers":[{"name":"seaweedfs","image":"`+image+`"}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],
		"containerStatuses":[{"name":"seaweedfs","state":{"waiting":{"reason":"`+reason+`","message":"`+message+`"}}}]}}`)
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
		minioNSPath, s3PVCPath, s3DeployPath,
		"/apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/seaweedfs-ingress",
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

	if f.has(minioPVCPath) || f.has(minioDeployPath) {
		t.Error("a fresh setup must not create MinIO objects")
	}
	raw, _ := json.Marshal(f.get(s3DeployPath))
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
							Name      string          `json:"name"`
							Value     string          `json:"value"`
							ValueFrom json.RawMessage `json:"valueFrom"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(raw, &deploy)
	ctr := deploy.Spec.Template.Spec.Containers[0]
	if ctr.Image != seaweedfsDefaultImage || !strings.HasPrefix(ctr.Image, "ghcr.io/chrislusf/seaweedfs@sha256:") {
		t.Errorf("image = %q, want the digest-pinned SeaweedFS", ctr.Image)
	}
	// The admin password must not be on the command line.
	if strings.Join(ctr.Args, " ") != "mini -dir=/data -webdav=false -bucket=pipelines" {
		t.Errorf("args = %v", ctr.Args)
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
	envs := map[string]string{}
	for _, e := range ctr.Env {
		envs[e.Name] = e.Value + string(e.ValueFrom)
	}
	for name, key := range map[string]string{"AWS_ACCESS_KEY_ID": "minio_root_user", "AWS_SECRET_ACCESS_KEY": "minio_root_password", "WEED_ADMIN_PASSWORD": "minio_root_password"} {
		if !strings.Contains(envs[name], `"name":"minio-secret"`) || !strings.Contains(envs[name], `"key":"`+key+`"`) {
			t.Errorf("env %s = %s, want secret minio-secret key %s", name, envs[name], key)
		}
	}
	if envs["WEED_ADMIN_USER"] != s3AdminUser || len(envs) != 4 {
		t.Errorf("env = %v", envs)
	}
	svc, _ := json.Marshal(f.get("/api/v1/namespaces/minio/services/minio-service")["spec"])
	for _, want := range []string{`{"name":"api","port":9000,"targetPort":8333}`, `{"name":"ui","port":9090,"targetPort":23646}`, `"selector":{"app":"seaweedfs"}`} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("Service spec lacks %s: %s", want, svc)
		}
	}
	if !strings.Contains(resp.Message, "Sign in to the admin UI as 'admin'") {
		t.Errorf("message must name the admin UI user: %q", resp.Message)
	}
}

// Setup applies the Service only once SeaweedFS is ready, so a MinIO being
// replaced keeps serving if SeaweedFS cannot start.
func TestSetupMinIO_ServiceFollowsReadiness(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	if resp, _ := SetupMinIO(c); resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	if f.has("/api/v1/namespaces/minio/services/minio-service") || f.has(minioUIRouteTestPath) {
		t.Error("the Service and the Route must wait for a ready SeaweedFS")
	}
}

const minioUIRouteTestPath = "/apis/route.openshift.io/v1/namespaces/minio/routes/minio-ui"

func TestSetupMinIO_ImageOverride(t *testing.T) {
	fastMinIOTimings(t)
	t.Setenv("SEAWEEDFS_IMAGE", "mirror.example.com/seaweedfs@sha256:abc")
	t.Setenv("MINIO_IMAGE", "mirror.example.com/minio@sha256:abc")
	f, c := newResourceFake(t)
	readyAfterApply(f)
	resp, _ := SetupMinIO(c)
	if !resp.Success {
		t.Fatalf("setup failed: %+v", resp)
	}
	raw, _ := json.Marshal(f.get(s3DeployPath))
	if !strings.Contains(string(raw), "mirror.example.com/seaweedfs@sha256:abc") || strings.Contains(string(raw), "mirror.example.com/minio") {
		t.Errorf("SEAWEEDFS_IMAGE override not applied, or MINIO_IMAGE used: %s", raw)
	}
	if !strings.Contains(strings.Join(resp.Logs, "\n"), "MINIO_IMAGE is set but no longer used") {
		t.Errorf("logs must say MINIO_IMAGE is ignored: %v", resp.Logs)
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
	putS3Pod(f, "seaweedfs-new", seaweedfsDefaultImage, "ImagePullBackOff", "Back-off pulling image: unauthorized: access to the requested resource is not authorized")

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
	putS3Pod(f, "seaweedfs-old", "ghcr.io/chrislusf/seaweedfs:4.47", "ImagePullBackOff", "unauthorized")
	putS3Pod(f, "seaweedfs-new", seaweedfsDefaultImage, "ContainerCreating", "")

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
	putS3Deployment(f, 0)
	putS3Pod(f, "seaweedfs-1", "ghcr.io/chrislusf/seaweedfs:4.48", "ImagePullBackOff", "Back-off pulling image ghcr.io/chrislusf/seaweedfs:4.48")
	f.putJSON(s3PVCPath, `{"metadata":{"labels":{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}}}`)

	st := getMinIOStatus(c)
	if !st.Deployed || st.Ready || !st.ManagedByTool || st.MigrationPending || st.UIUser != "admin" {
		t.Fatalf("state = %+v", st)
	}
	if st.WaitingReason != "ImagePullBackOff" || !st.TerminalError || !strings.HasPrefix(st.Message, "ImagePullBackOff: Back-off pulling image") {
		t.Errorf("waiting reason not surfaced: %+v", st)
	}
	if len(st.DataPVCs) != 1 || st.DataPVCs[0] != "seaweedfs-pvc" || len(st.KeptPVCs) != 0 {
		t.Errorf("DataPVCs = %v, KeptPVCs = %v", st.DataPVCs, st.KeptPVCs)
	}
}

// The MinIO of an earlier version reports its own pods and readiness, and
// that setup would migrate it.
func TestGetMinIOStatus_MigrationPending(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, toolLabelJSON, toolFieldManager)
	putMinIODeployment(f, toolFieldManager, 0)
	putMinIOPod(f, "minio-1", "quay.io/minio/minio:latest", "ImagePullBackOff", "Back-off pulling image quay.io/minio/minio:latest")
	f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`},"status":{"capacity":{"storage":"20Gi"}}}`)

	st := getMinIOStatus(c)
	if !st.Deployed || st.Ready || !st.MigrationPending || st.WaitingReason != "ImagePullBackOff" || st.UIUser != "" {
		t.Fatalf("state = %+v", st)
	}
	for _, want := range []string{"Re-run setup to replace it with SeaweedFS", "not copied", "return 404", "minio-pvc"} {
		if !strings.Contains(st.Warning, want) {
			t.Errorf("warning lacks %q: %q", want, st.Warning)
		}
	}
	if len(st.DataPVCs) != 1 || len(st.KeptPVCs) != 0 {
		t.Errorf("the volume in use is not kept yet: DataPVCs = %v, KeptPVCs = %v", st.DataPVCs, st.KeptPVCs)
	}
}

// After the migration the MinIO volume is reported as kept, with its size.
func TestGetMinIOStatus_KeptMinIOVolume(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, toolLabelJSON, toolFieldManager)
	putS3Deployment(f, 1)
	putS3Serving(f)
	f.putJSON(s3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"resources":{"requests":{"storage":"10Gi"}}},"status":{"capacity":{"storage":"20Gi"}}}`)

	st := getMinIOStatus(c)
	if !st.Ready || st.MigrationPending || st.Warning != "" {
		t.Fatalf("state = %+v", st)
	}
	if len(st.KeptPVCs) != 1 || st.KeptPVCs[0].Name != "minio-pvc" || st.KeptPVCs[0].Size != "20Gi" {
		t.Errorf("KeptPVCs = %+v", st.KeptPVCs)
	}
	if strings.Join(st.DataPVCs, ",") != "seaweedfs-pvc,minio-pvc" && strings.Join(st.DataPVCs, ",") != "minio-pvc,seaweedfs-pvc" {
		t.Errorf("teardown deletes both volumes: DataPVCs = %v", st.DataPVCs)
	}
	// Someone else's PVC of that name is neither kept nor deleted by the tool.
	f.putJSON(minioPVCPath, foreignJSON(""))
	if st := getMinIOStatus(c); len(st.KeptPVCs) != 0 || len(st.DataPVCs) != 1 {
		t.Errorf("foreign minio-pvc reported: %+v", st)
	}
}

// Status comes from the serving configuration: SeaweedFS counts as Running
// only when it is ready and minio-service and Route minio-ui send traffic to
// it. Otherwise it is incomplete, says why, and asks for Repair.
func TestGetMinIOStatus_ServingConfiguration(t *testing.T) {
	const (
		svcOn = `{"metadata":{"labels":` + toolLabelJSON + `},"spec":{"selector":%s,"ports":[{"name":"api","port":9000,"targetPort":%s},{"name":"ui","port":9090,"targetPort":23646}]}}`
	)
	svc := func(selector, target string) func(f *resourceFake) {
		return func(f *resourceFake) { f.putJSON(minioSvcPath, fmt.Sprintf(svcOn, selector, target)) }
	}
	cases := []struct {
		name       string
		change     func(f *resourceFake)
		ready      int
		wantReady  bool
		wantRepair string // substring; "" means none
		wantTerm   bool
		wantMsg    string
	}{
		{name: "serving", ready: 1, wantReady: true, wantMsg: "Running"},
		{name: "Service still on MinIO", change: svc(`{"app":"minio"}`, `9000`), ready: 1, wantRepair: "selects app=minio instead of SeaweedFS", wantTerm: true},
		{name: "Service port not switched", change: svc(`{"app":"seaweedfs"}`, `9000`), ready: 1, wantRepair: "port 9000 forwards to 9000", wantTerm: true},
		{name: "Service by port name", change: svc(`{"app":"seaweedfs"}`, `"s3"`), ready: 1, wantRepair: "forwards to s3", wantTerm: true},
		{name: "Service missing (setup stopped before the switch)", change: func(f *resourceFake) {
			f.mu.Lock()
			delete(f.objects, fakeKey{gv: "v1", ns: "minio", plural: "services", name: "minio-service"})
			f.mu.Unlock()
		}, ready: 1, wantRepair: "Service minio-service is missing", wantTerm: true},
		{name: "Route missing", change: func(f *resourceFake) {
			f.mu.Lock()
			delete(f.objects, fakeKey{gv: "route.openshift.io/v1", ns: "minio", plural: "routes", name: "minio-ui"})
			f.mu.Unlock()
		}, ready: 1, wantRepair: "Route minio-ui (the admin UI) is missing", wantTerm: true},
		{name: "Route on another Service", change: func(f *resourceFake) {
			f.putJSON(minioUIRoute, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"to":{"name":"minio-old"},"port":{"targetPort":"ui"}}}`)
		}, ready: 1, wantRepair: "Route minio-ui does not point at port ui", wantTerm: true},
		{name: "still starting before the switch", change: svc(`{"app":"minio"}`, `9000`), ready: 0, wantRepair: "selects app=minio", wantMsg: "0/1 ready"},
		{name: "scaled to zero", change: func(f *resourceFake) {
			f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":2},"spec":{"replicas":0,"template":{"spec":{"containers":[{"name":"seaweedfs","image":"`+seaweedfsDefaultImage+`"}]}}},"status":{"observedGeneration":2}}`)
		}, wantRepair: "scaled to 0 replicas", wantTerm: true},
		{name: "Route unreadable", change: func(f *resourceFake) { f.fail["GET "+minioUIRoute] = 500 }, ready: 1, wantMsg: "cannot be checked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putNamespace(f, toolLabelJSON, toolFieldManager)
			putS3Deployment(f, tc.ready)
			putS3Serving(f)
			if tc.change != nil {
				tc.change(f)
			}
			st := getMinIOStatus(c)
			if !st.Deployed || st.Ready != tc.wantReady || st.TerminalError != tc.wantTerm {
				t.Fatalf("state = %+v", st)
			}
			if (tc.wantRepair == "") != (st.RepairNeeded == "") || !strings.Contains(st.RepairNeeded, tc.wantRepair) {
				t.Errorf("repairNeeded = %q, want %q", st.RepairNeeded, tc.wantRepair)
			}
			if tc.wantTerm && st.Message != st.RepairNeeded {
				t.Errorf("message = %q, want the reason", st.Message)
			}
			if tc.wantMsg != "" && !strings.Contains(st.Message, tc.wantMsg) {
				t.Errorf("message = %q, want %q", st.Message, tc.wantMsg)
			}
		})
	}
}

// A SeaweedFS that runs another image than this version deploys says so.
func TestGetMinIOStatus_ImageDrift(t *testing.T) {
	t.Setenv("SEAWEEDFS_IMAGE", "")
	f, c := newResourceFake(t)
	putNamespace(f, toolLabelJSON, toolFieldManager)
	f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"template":{"spec":{"containers":[{"name":"seaweedfs","image":"ghcr.io/chrislusf/seaweedfs:4.47"}]}}},"status":{"readyReplicas":1}}`)
	st := getMinIOStatus(c)
	if !strings.HasPrefix(st.Warning, "SeaweedFS runs ghcr.io/chrislusf/seaweedfs:4.47, not the image this version deploys") || !strings.Contains(st.Warning, "Re-run setup to update it") {
		t.Errorf("warning = %q", st.Warning)
	}
}

func TestGetMinIOStatus_ContainerCreatingIsNotTerminal(t *testing.T) {
	f, c := newResourceFake(t)
	putNamespace(f, `{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}`, toolFieldManager)
	putS3Deployment(f, 0)
	putS3Pod(f, "seaweedfs-1", seaweedfsDefaultImage, "ContainerCreating", "")
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
	if st := getMinIOStatus(c); !st.ManagedByTool || !st.Ready || st.TeardownBlockedReason != "" || !st.MigrationPending {
		t.Errorf("state = %+v", st)
	}
}

func TestTeardownMinIO(t *testing.T) {
	managedNS := func(f *resourceFake) {
		putNamespace(f, `{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}`, toolFieldManager)
		putS3Deployment(f, 1)
		f.putJSON(s3PVCPath, `{"metadata":{"labels":{"`+managedByLabelKey+`":"`+managedByLabelValue+`"}}}`)
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
		wantGone bool // the tool's Deployments and PVCs were deleted
		wantMsg  string
	}{
		{name: "not deployed", setup: func(*resourceFake) {}, wantOK: true},
		{name: "namespace not created by the tool", setup: func(f *resourceFake) {
			putNamespace(f, `{}`, "kubectl-create")
			putMinIODeployment(f, toolFieldManager, 1)
		}, wantCode: "not_managed"},
		{name: "managed", setup: managedNS, wantOK: true, wantGone: true, wantMsg: "oc delete project minio"},
		{name: "migrated, with the MinIO volume kept", setup: func(f *resourceFake) {
			managedNS(f)
			f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		}, wantOK: true, wantGone: true, wantMsg: "PVCs seaweedfs-pvc and minio-pvc and all stored objects"},
		{name: "migration pending", setup: func(f *resourceFake) {
			putNamespace(f, toolLabelJSON, toolFieldManager)
			putMinIODeployment(f, toolFieldManager, 1)
			f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		}, wantOK: true, wantGone: true, wantMsg: "PVC minio-pvc and all stored objects"},
		{name: "kept MinIO volume used by a pipeline server", setup: func(f *resourceFake) {
			putNamespace(f, toolLabelJSON, toolFieldManager)
			f.putJSON(minioPVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
			dspa(f, "team-a", "dspa", minioS3Host(), false)
		}, wantCode: "prerequisites", wantMsg: "team-a/dspa"},
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
			f.fail["DELETE "+s3PVCPath] = 500
		}, wantCode: "partial_failure", wantMsg: "PersistentVolumeClaim seaweedfs-pvc"},
		{name: "read forbidden", setup: func(f *resourceFake) {
			managedNS(f)
			f.fail["GET "+s3DeployPath] = 403
		}, wantCode: "partial_failure", wantMsg: "Deployment seaweedfs: cannot read it"},
		// The new image under an old template: the policy of the new name
		// is not readable yet. The message says how to fix it.
		{name: "RBAC from an older template", setup: func(f *resourceFake) {
			putNamespace(f, toolLabelJSON, toolFieldManager)
			putMinIODeployment(f, toolFieldManager, 1)
			f.fail["GET /apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/seaweedfs-ingress"] = 403
		}, wantCode: "partial_failure", wantMsg: "run `make upgrade`"},
		{name: "PVC still terminating", setup: func(f *resourceFake) {
			managedNS(f)
			f.putJSON(s3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`,"finalizers":["kubernetes.io/pvc-protection"]}}`)
		}, wantCode: "in_progress", wantMsg: "PVC seaweedfs-pvc is still terminating"},
		{name: "already removed, namespace left", setup: func(f *resourceFake) {
			putNamespace(f, toolLabelJSON, toolFieldManager)
		}, wantOK: true, wantMsg: "No S3 storage objects created by this tool remain"},
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
			if gone := !f.has(minioDeployPath) && !f.has(minioPVCPath) && !f.has(s3DeployPath) && !f.has(s3PVCPath); gone != tc.wantGone && tc.wantGone {
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
