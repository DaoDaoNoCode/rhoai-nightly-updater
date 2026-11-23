package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	s3NSPath     = "/api/v1/namespaces/seaweedfs"
	s3DeployPath = "/apis/apps/v1/namespaces/seaweedfs/deployments/seaweedfs"
	s3PVCPath    = "/api/v1/namespaces/seaweedfs/persistentvolumeclaims/seaweedfs-data"
	s3SecretPath = "/api/v1/namespaces/seaweedfs/secrets/seaweedfs-s3-credentials"
	s3SvcPath    = "/api/v1/namespaces/seaweedfs/services/seaweedfs-s3"
	s3RoutePath  = "/apis/route.openshift.io/v1/namespaces/seaweedfs/routes/seaweedfs-admin"
	s3NPPath     = "/apis/networking.k8s.io/v1/namespaces/seaweedfs/networkpolicies/seaweedfs-ingress"
	dspaListPath = "/apis/datasciencepipelinesapplications.opendatahub.io/v1/datasciencepipelinesapplications"
	created      = "2026-08-27T21:30:55Z"
)

// fastS3Timings shortens the storage waits, stubs bucket creation and
// clears the environment overrides.
func fastS3Timings(t *testing.T) *int {
	t.Helper()
	t.Cleanup(storage.SetBucketRetryDelayForTest(time.Millisecond))
	t.Cleanup(storage.SetS3TimingsForTest(300*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond))
	calls := 0
	t.Cleanup(storage.SetBucketCreatorForTest(func(*Client, string) error { calls++; return nil }))
	for _, env := range []string{"SEAWEEDFS_IMAGE", "SEAWEEDFS_ACCESS_KEY_ID", "SEAWEEDFS_SECRET_ACCESS_KEY", "MINIO_IMAGE", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD"} {
		t.Setenv(env, "")
	}
	return &calls
}

// ownedContentJSON is the fieldsV1 of a manager that wrote an object's
// content: labels, the spec (with a DSPA's object storage) and data.
const ownedContentJSON = `{"f:metadata":{"f:labels":{}},"f:spec":{"f:objectStorage":{"f:externalStorage":{"f:host":{},"f:s3CredentialsSecret":{"f:secretName":{}}}}},"f:data":{}}`

// managedFieldsJSON builds a managedFields array with one entry at the
// creation time, owning the object's content.
func managedFieldsJSON(manager, op string) string {
	return `[{"manager":"` + manager + `","operation":"` + op + `","time":"` + created + `","fieldsType":"FieldsV1","fieldsV1":` + ownedContentJSON + `}]`
}

func hasMutation(f *resourceFake, prefix string) bool {
	for _, m := range f.mutations() {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

func putS3Namespace(f *resourceFake, labels string) {
	f.putJSON(s3NSPath, `{"metadata":{"labels":`+labels+`,"creationTimestamp":"`+created+`"},"status":{"phase":"Active"}}`)
}

// securedArgsJSON are the args of secured storage, unsecuredArgsJSON those
// of storage an earlier version set up (IAM read-only).
const (
	unsecuredArgsJSON = `["mini","-dir=/data","-webdav=false","-bucket=pipelines","-admin.port=12535","-admin.port.legacy=0"]`
	securedArgsJSON   = `["mini","-dir=/data","-webdav=false","-bucket=pipelines","-admin.port=12535","-admin.port.legacy=0","` + storage.S3IAMWritesArg + `","` + storage.S3SelfServiceArg + `"]`
)

// putS3Deployment puts the tool's SeaweedFS Deployment, secured.
func putS3Deployment(f *resourceFake, ready int) {
	putS3DeploymentArgs(f, ready, securedArgsJSON)
}

// putS3DeploymentArgs puts the tool's SeaweedFS Deployment with args.
func putS3DeploymentArgs(f *resourceFake, ready int, args string) {
	f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},
		"spec":{"template":{"spec":{"containers":[{"name":"seaweedfs","image":"`+storage.SeaweedFSDefaultImage+`","args":`+args+`}]}}},
		"status":{"observedGeneration":1,"replicas":1,"readyReplicas":`+itoa(ready)+`,"updatedReplicas":1}}`)
}

// putS3Serving puts the Service and the admin UI Route as setup applies
// them: they send traffic to SeaweedFS.
func putS3Serving(f *resourceFake) {
	f.putJSON(s3SvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"selector":{"app":"seaweedfs"},"clusterIP":"172.30.20.30",
		"ports":[{"name":"s3","port":8333,"targetPort":8333},{"name":"ui","port":12535,"targetPort":12535}]}}`)
	f.putJSON(s3RoutePath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"host":"seaweedfs-admin-seaweedfs.apps.example.com","to":{"kind":"Service","name":"seaweedfs-s3"},"port":{"targetPort":"ui"}}}`)
}

// putS3NetworkPolicy puts NetworkPolicy seaweedfs-ingress as setup applies it.
func putS3NetworkPolicy(f *resourceFake) {
	for _, r := range storage.S3Resources(storage.S3Keys{}, true) {
		if r.Kind == "NetworkPolicy" {
			f.putJSON(s3NPPath, mustJSON(r.Obj))
		}
	}
}

// putReadyManagedS3 puts a running storage the tool set up, with its keys.
func putReadyManagedS3(f *resourceFake) {
	putS3Namespace(f, toolLabelJSON)
	putS3Deployment(f, 1)
	putS3Serving(f)
	putS3NetworkPolicy(f)
	f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"AWS_ACCESS_KEY_ID":"`+exampleAuth("seaweedfs-key")+`","AWS_SECRET_ACCESS_KEY":"`+exampleAuth("pass")+`","WEED_ADMIN_PASSWORD":"`+exampleAuth("admin-pass")+`"}}`)
}

// readyAfterApply makes the SeaweedFS deployment report a ready rollout of
// the current generation, like the deployment controller would.
func readyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "deployments" && k.ns == storage.S3Namespace && k.name == storage.S3Deployment {
			obj["status"] = map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1}
		}
	}
}

func notReadyAfterApply(f *resourceFake) {
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "deployments" && k.ns == storage.S3Namespace && k.name == storage.S3Deployment {
			obj["status"] = map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 0, "updatedReplicas": 1}
		}
	}
}

func putS3Pod(f *resourceFake, name, image, reason, message string) {
	f.putJSON("/api/v1/namespaces/seaweedfs/pods/"+name, `{"metadata":{"labels":{"app":"seaweedfs"}},"spec":{"containers":[{"name":"seaweedfs","image":"`+image+`"}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],
		"containerStatuses":[{"name":"seaweedfs","state":{"waiting":{"reason":"`+reason+`","message":"`+message+`"}}}]}}`)
}

// noWritesIn fails when any request changed an object in namespace ns.
func noWritesIn(t *testing.T, f *resourceFake, ns string) {
	t.Helper()
	for _, m := range f.mutations() {
		if strings.Contains(m, "/namespaces/"+ns+"/") || strings.HasSuffix(m, "/namespaces/"+ns) {
			t.Errorf("unexpected write in namespace %s: %s", ns, m)
		}
	}
}

func TestSetupS3Storage_FreshClusterUsesTheNewNamesLabelsEverythingAndWaitsForReady(t *testing.T) {
	bucketCalls := fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)

	resp, err := SetupS3Storage(c)
	if err != nil || !resp.Success {
		t.Fatalf("setup failed: %v %+v", err, resp)
	}
	if *bucketCalls != 1 {
		t.Errorf("bucket created %d times, want 1", *bucketCalls)
	}
	for _, path := range []string{s3NSPath, s3PVCPath, s3DeployPath, s3NPPath, s3SecretPath, s3SvcPath, s3RoutePath} {
		obj := f.get(path)
		if obj == nil {
			t.Fatalf("%s was not created", path)
		}
		labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
		if labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
			t.Errorf("%s lacks the ownership label: %v", path, labels)
		}
	}
	nsFields := f.get(s3NSPath)["metadata"].(map[string]any)["managedFields"].([]any)
	if nsFields[0].(map[string]any)["manager"] != envkit.ToolFieldManager {
		t.Errorf("namespace POST did not set fieldManager: %v", nsFields)
	}
	// A fresh install never touches, or names, the earlier storage.
	noWritesIn(t, f, "minio")
	for _, line := range append([]string{resp.Message}, resp.Logs...) {
		if strings.Contains(strings.ToLower(line), "minio") {
			t.Errorf("a fresh setup mentions MinIO: %q", line)
		}
	}

	raw, _ := json.Marshal(f.get(s3DeployPath))
	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext map[string]any `json:"securityContext"`
					Volumes         []struct {
						PVC struct {
							ClaimName string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
					Containers []struct {
						Image           string         `json:"image"`
						Args            []string       `json:"args"`
						SecurityContext map[string]any `json:"securityContext"`
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
	if ctr.Image != storage.SeaweedFSDefaultImage || !strings.HasPrefix(ctr.Image, "ghcr.io/chrislusf/seaweedfs@sha256:") {
		t.Errorf("image = %q, want the digest-pinned SeaweedFS", ctr.Image)
	}
	if deploy.Spec.Template.Spec.Volumes[0].PVC.ClaimName != "seaweedfs-data" {
		t.Errorf("volumes = %+v", deploy.Spec.Template.Spec.Volumes)
	}
	// The admin password must not be on the command line.
	// A first setup is secured: IAM writes on, self-service off.
	if strings.Join(ctr.Args, " ") != "mini -dir=/data -webdav=false -bucket=pipelines -admin.port=12535 -admin.port.legacy=0 -s3.iam.readOnly=false -s3.iam.selfServiceRequiresGrant=true" {
		t.Errorf("args = %v", ctr.Args)
	}
	if _, fixed := ctr.SecurityContext["runAsUser"]; fixed || deploy.Spec.Template.Spec.SecurityContext != nil {
		t.Errorf("a fixed UID would conflict with restricted-v2: %v", ctr.SecurityContext)
	}
	envs := map[string]string{}
	for _, e := range ctr.Env {
		envs[e.Name] = e.Value + string(e.ValueFrom)
	}
	for name, key := range map[string]string{"AWS_ACCESS_KEY_ID": "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY": "AWS_SECRET_ACCESS_KEY", "WEED_ADMIN_PASSWORD": "WEED_ADMIN_PASSWORD"} {
		if !strings.Contains(envs[name], `"name":"seaweedfs-s3-credentials"`) || !strings.Contains(envs[name], `"key":"`+key+`"`) {
			t.Errorf("env %s = %s, want Secret seaweedfs-s3-credentials key %s", name, envs[name], key)
		}
	}
	if envs["WEED_ADMIN_USER"] != storage.S3AdminUser || len(envs) != 4 {
		t.Errorf("env = %v", envs)
	}
	svc, _ := json.Marshal(f.get(s3SvcPath)["spec"])
	for _, want := range []string{`{"name":"s3","port":8333,"targetPort":8333}`, `{"name":"ui","port":12535,"targetPort":12535}`, `"selector":{"app":"seaweedfs"}`} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("Service spec lacks %s: %s", want, svc)
		}
	}
	route, _ := json.Marshal(f.get(s3RoutePath)["spec"])
	if !strings.Contains(string(route), `"targetPort":"ui"`) || !strings.Contains(string(route), `"name":"seaweedfs-s3"`) {
		t.Errorf("the admin Route must target the admin UI port only: %s", route)
	}
	secret, _ := json.Marshal(f.get(s3SecretPath))
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !strings.Contains(string(secret), `"`+key+`"`) {
			t.Errorf("Secret lacks key %s: %s", key, secret)
		}
	}
	if !strings.Contains(resp.Message, "Sign in to the admin UI as admin") || !strings.Contains(resp.Message, "http://seaweedfs-s3.seaweedfs.svc:8333") {
		t.Errorf("message = %q", resp.Message)
	}
}

// R6-1: SeaweedFS's ports are fenced in by a tool-owned NetworkPolicy: S3
// from pods in any namespace, the admin UI only from the router, nothing
// else (master, volume, filer and worker ports stay unreachable).
func TestSetupS3Storage_CreatesIngressPolicy(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	raw, _ := json.Marshal(f.get(s3NPPath)["spec"])
	want := `{"ingress":[{"from":[{"namespaceSelector":{}}],"ports":[{"port":8333,"protocol":"TCP"}]},` +
		`{"from":[{"namespaceSelector":{"matchLabels":{"policy-group.network.openshift.io/ingress":""}}},{"namespaceSelector":{"matchLabels":{"policy-group.network.openshift.io/host-network":""}}}],"ports":[{"port":12535,"protocol":"TCP"}]}],` +
		`"podSelector":{"matchLabels":{"app":"seaweedfs"}},"policyTypes":["Ingress"]}`
	if string(raw) != want {
		t.Errorf("NetworkPolicy spec =\n%s\nwant\n%s", raw, want)
	}
}

// The Service and the Route are applied only once SeaweedFS is ready.
func TestSetupS3Storage_ServiceFollowsReadiness(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "timeout" {
		t.Fatalf("setup = %+v", resp)
	}
	if f.has(s3SvcPath) || f.has(s3RoutePath) {
		t.Error("the Service and the Route must wait for a ready SeaweedFS")
	}
}

func TestSetupS3Storage_ImageOverride(t *testing.T) {
	fastS3Timings(t)
	t.Setenv("SEAWEEDFS_IMAGE", "mirror.example.com/seaweedfs:4.54")
	f, c := newResourceFake(t)
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	raw, _ := json.Marshal(f.get(s3DeployPath))
	if !strings.Contains(string(raw), `"image":"mirror.example.com/seaweedfs:4.54"`) {
		t.Errorf("image override not applied: %s", raw)
	}
}

// Variables retired in 3.0.0 are not read.
func TestSetupS3Storage_IgnoresRetiredVariables(t *testing.T) {
	fastS3Timings(t)
	t.Setenv("MINIO_ROOT_USER", "someone")
	f, c := newResourceFake(t)
	readyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	raw, _ := json.Marshal(f.get(s3SecretPath))
	if strings.Contains(string(raw), "someone") {
		t.Errorf("a retired variable set the access key: %s", raw)
	}
}

func TestSetupS3Storage_RefusesNamespaceItDidNotCreate(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putS3Namespace(f, `{"owner":"someone"}`)
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "Nothing was changed") {
		t.Fatalf("setup = %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("refusal wrote %v", m)
	}
}

// While an earlier version's storage exists, Set up refuses and points to
// Migrate storage. That gives the new storage new keys (the old ones reach
// only the copy Job), so the message must say so and where to find them.
func TestSetupS3Storage_RefusesWhileEarlierStorageExistsAndSaysTheKeysAreNew(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "Use Migrate storage") {
		t.Fatalf("setup = %+v", resp)
	}
	for _, want := range []string{"with new keys", "moves the pipeline servers this tool set up to the new storage and its keys", "Anything else that uses the old keys", "the new keys from the Pipelines tab", "Nothing was changed"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message lacks %q: %s", want, resp.Message)
		}
	}
	if strings.Contains(resp.Message, "same keys") || strings.Contains(resp.Message, "then removes the old storage") {
		t.Errorf("message says the keys stay or that the old storage is removed: %s", resp.Message)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("refusal wrote %v", m)
	}
}

func TestSetupS3Storage_TerminatingNamespace(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	f.putJSON(s3NSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"deletionTimestamp":"2026-10-01T00:00:00Z"},"status":{"phase":"Terminating"}}`)
	if resp, _ := SetupS3Storage(c); resp.Success || resp.ErrorCode != "terminating" {
		t.Fatalf("setup = %+v", resp)
	}
}

func TestSetupS3Storage_RefusesEveryUnlabelledObject(t *testing.T) {
	for _, path := range []string{s3PVCPath, s3SecretPath, s3DeployPath, s3SvcPath, s3RoutePath, s3NPPath} {
		t.Run(path[strings.LastIndex(path, "/")+1:], func(t *testing.T) {
			fastS3Timings(t)
			f, c := newResourceFake(t)
			putS3Namespace(f, toolLabelJSON)
			f.putJSON(path, foreignJSON(`,"data":{"k":"dXNlcg=="}`))
			resp, _ := SetupS3Storage(c)
			if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "Nothing was changed") {
				t.Fatalf("want a not_managed refusal, got %+v", resp)
			}
			if name := path[strings.LastIndex(path, "/")+1:]; !strings.Contains(resp.Message, name) {
				t.Errorf("message must name %s: %q", name, resp.Message)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("refusal must not write anything, got %v", m)
			}
			assertForeignUntouched(t, f, path, "data", "dXNlcg==")
		})
	}
}

// A foreign object created after the check and before the POST: the POST
// fails with 409, the object is re-read, and setup refuses.
func TestSetupS3Storage_ForeignObjectCreatedBetweenCheckAndCreate(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	var once sync.Once
	f.beforeServe = func(method, path string) {
		if method == http.MethodPost && path == "/api/v1/namespaces/seaweedfs/secrets" {
			once.Do(func() { f.putJSON(s3SecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
		}
	}
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "Secret seaweedfs/seaweedfs-s3-credentials") {
		t.Fatalf("want not_managed, got %+v", resp)
	}
	assertForeignUntouched(t, f, s3SecretPath, "data", "dXNlcg==")
	if hasMutation(f, "PATCH "+s3SecretPath) {
		t.Error("the foreign secret must not be patched")
	}
}

// An owned object replaced by a foreign one between the check and the
// update: the update carries the checked resourceVersion, fails with 409,
// and the re-check refuses.
func TestSetupS3Storage_ObjectReplacedBetweenCheckAndUpdate(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	putS3Namespace(f, toolLabelJSON)
	f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`,"resourceVersion":"5"},"data":{"AWS_ACCESS_KEY_ID":"`+exampleAuth("u")+`","AWS_SECRET_ACCESS_KEY":"`+exampleAuth("p")+`"}}`)
	var once sync.Once
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == s3SecretPath {
			once.Do(func() { f.putJSON(s3SecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
		}
	}
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "not_managed" {
		t.Fatalf("want not_managed, got %+v", resp)
	}
	assertForeignUntouched(t, f, s3SecretPath, "data", "dXNlcg==")
}

func TestSetupS3Storage_StopsEarlyOnImagePullBackOff(t *testing.T) {
	fastS3Timings(t)
	storage.SetS3ReadyTimeoutForTest(5 * time.Second)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	putS3Pod(f, "seaweedfs-1", storage.SeaweedFSDefaultImage, "ImagePullBackOff", "Back-off pulling image")
	start := kube.Clock().Now()
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "not_ready" || !strings.Contains(resp.Message, "ImagePullBackOff") {
		t.Fatalf("setup = %+v", resp)
	}
	if kube.Clock().Since(start) >= storage.S3ReadyTimeout {
		t.Error("setup must stop at a terminal pod reason, not wait for the timeout")
	}
}

// A pod of another image (an earlier setup) is not mistaken for this one.
func TestSetupS3Storage_IgnoresPodsOfAnotherImage(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	putS3Pod(f, "seaweedfs-old", "ghcr.io/chrislusf/seaweedfs:old", "ImagePullBackOff", "gone")
	if resp, _ := SetupS3Storage(c); resp.Success || resp.ErrorCode != "timeout" {
		t.Fatalf("setup = %+v", resp)
	}
}

func TestSetupS3Storage_ApplyFailureReportsPartialProgress(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	f.fail["POST /apis/apps/v1/namespaces/seaweedfs/deployments"] = 500
	resp, _ := SetupS3Storage(c)
	if resp.Success || !strings.Contains(resp.Message, "Already applied: PVC, Secret, NetworkPolicy") || !strings.Contains(resp.Message, "Re-run setup to retry") {
		t.Fatalf("setup = %+v", resp)
	}
}

// The Storage card's setup names itself as the way to continue (inside
// Migrate storage the same failure names Migrate storage).
func TestSetupS3Storage_BucketFailureSaysReRunSetup(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	storage.SetBucketCreatorForTest(func(*Client, string) error { return errors.New("connection refused") })
	resp, _ := SetupS3Storage(c)
	if resp.Success || !strings.Contains(resp.Message, "Re-run setup to retry, or create bucket pipelines from the admin UI") || strings.Contains(resp.Message, "Migrate") {
		t.Fatalf("setup = %+v", resp)
	}
}

// A setup that stopped part-way is finished by a re-run, which updates the
// objects it created instead of failing on them.
func TestSetupS3Storage_RecoversFromInterruptedSetup(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	if resp, _ := SetupS3Storage(c); resp.Success {
		t.Fatalf("first setup = %+v", resp)
	}
	readyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if !resp.Success || !f.has(s3SvcPath) || !f.has(s3RoutePath) {
		t.Fatalf("re-run = %+v", resp)
	}
}

func TestSetupS3Storage_ForbiddenByOldTemplateNamesTheFix(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	f.fail["POST /api/v1/namespaces"] = 403
	resp, _ := SetupS3Storage(c)
	if resp.Success || !strings.Contains(resp.Message, "make upgrade") {
		t.Fatalf("setup = %+v", resp)
	}
}

// While storage from an earlier version is in namespace minio, setup
// refuses and points at Migrate storage; nothing is created anywhere.
func TestSetupS3Storage_RefusesWhileEarlierStorageExists(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	resp, _ := SetupS3Storage(c)
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "Migrate storage") || !strings.Contains(resp.Message, "Nothing was changed") {
		t.Fatalf("setup = %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("refusal wrote %v", m)
	}
}

func TestS3Credentials(t *testing.T) {
	t.Run("random and different on each first setup", func(t *testing.T) {
		fastS3Timings(t)
		_, c1 := newResourceFake(t)
		_, c2 := newResourceFake(t)
		k1, err1 := storage.S3Credentials(c1)
		k2, err2 := storage.S3Credentials(c2)
		if err1 != nil || err2 != nil || k1 == k2 {
			t.Fatalf("keys %v %v (%v %v)", k1, k2, err1, err2)
		}
		if !strings.HasPrefix(k1.AccessKey, "seaweedfs-") || len(k1.SecretKey) != 32 || strings.Trim(k1.SecretKey, "0123456789abcdef") != "" ||
			len(k1.AdminPassword) != 32 || k1.AdminPassword == k1.SecretKey || k1.AdminPassword == k2.AdminPassword {
			t.Errorf("keys = %+v", k1)
		}
	})
	t.Run("stored keys are kept, even over the environment", func(t *testing.T) {
		fastS3Timings(t)
		t.Setenv("SEAWEEDFS_ACCESS_KEY_ID", "env-key")
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		k, err := storage.S3Credentials(c)
		if err != nil || k.AccessKey != "seaweedfs-key" || k.SecretKey != "pass" || k.AdminPassword != "admin-pass" {
			t.Fatalf("keys = %+v (%v)", k, err)
		}
	})
	t.Run("storage of an earlier version gets an admin password of its own; the S3 keys stay", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"AWS_ACCESS_KEY_ID":"`+exampleAuth("seaweedfs-key")+`","AWS_SECRET_ACCESS_KEY":"`+exampleAuth("pass")+`"}}`)
		k, err := storage.S3Credentials(c)
		if err != nil || k.AccessKey != "seaweedfs-key" || k.SecretKey != "pass" || len(k.AdminPassword) != 32 || k.AdminPassword == k.SecretKey {
			t.Fatalf("keys = %+v (%v)", k, err)
		}
	})
	t.Run("the environment sets them on a first setup", func(t *testing.T) {
		fastS3Timings(t)
		t.Setenv("SEAWEEDFS_ACCESS_KEY_ID", "env-key")
		t.Setenv("SEAWEEDFS_SECRET_ACCESS_KEY", "env-secret")
		_, c := newResourceFake(t)
		k, err := storage.S3Credentials(c)
		if err != nil || k.AccessKey != "env-key" || k.SecretKey != "env-secret" {
			t.Fatalf("keys = %+v (%v)", k, err)
		}
	})
	t.Run("a read error does not rotate them", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		f.fail["GET "+s3SecretPath] = 500
		if k, err := storage.S3Credentials(c); err == nil {
			t.Fatalf("keys = %+v, want the read error", k)
		}
	})
}

func TestSetupS3Storage_ResponseDoesNotLeakTheSecretKey(t *testing.T) {
	fastS3Timings(t)
	t.Setenv("SEAWEEDFS_SECRET_ACCESS_KEY", "do-not-print-me")
	f, c := newResourceFake(t)
	readyAfterApply(f)
	resp, _ := SetupS3Storage(c)
	if !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	for _, line := range append([]string{resp.Message}, resp.Logs...) {
		if strings.Contains(line, "do-not-print-me") {
			t.Errorf("secret key in %q", line)
		}
	}
}

func TestGetS3Status(t *testing.T) {
	t.Run("not deployed", func(t *testing.T) {
		_, c := newResourceFake(t)
		if st := storage.S3Status(c); st.Deployed || st.Message != "Not deployed" || st.Namespace != "seaweedfs" {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("running", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		f.putJSON(s3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		st := storage.S3Status(c)
		if !st.Ready || !st.ManagedByTool || st.UIRoute != "https://seaweedfs-admin-seaweedfs.apps.example.com" || st.UIUser != "admin" || st.RepairNeeded != "" || st.MigrationPending {
			t.Errorf("status = %+v", st)
		}
		if strings.Join(st.DataPVCs, ",") != "seaweedfs-data" {
			t.Errorf("DataPVCs = %v", st.DataPVCs)
		}
	})
	t.Run("terminating namespace", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.putJSON(s3NSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"deletionTimestamp":"2026-10-01T00:00:00Z"},"status":{"phase":"Terminating"}}`)
		if st := storage.S3Status(c); !st.Terminating || st.SetupBlockedReason == "" {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("namespace not created by the tool", func(t *testing.T) {
		f, c := newResourceFake(t)
		putS3Namespace(f, `{}`)
		if st := storage.S3Status(c); st.ManagedByTool || st.SetupBlockedReason == "" || st.TeardownBlockedReason == "" {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("pod waiting", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		putS3Deployment(f, 0)
		putS3Pod(f, "seaweedfs-1", storage.SeaweedFSDefaultImage, "CrashLoopBackOff", "boom")
		st := storage.S3Status(c)
		if st.Ready || st.WaitingReason != "CrashLoopBackOff" || !st.TerminalError {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("scaled to zero", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},"spec":{"replicas":0,"template":{"spec":{"containers":[{"name":"seaweedfs","image":"`+storage.SeaweedFSDefaultImage+`"}]}}},"status":{"observedGeneration":1}}`)
		st := storage.S3Status(c)
		if st.Ready || !st.TerminalError || !strings.Contains(st.RepairNeeded, "scaled to 0") {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("another image", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		t.Setenv("SEAWEEDFS_IMAGE", "mirror.example.com/seaweedfs:4.54")
		st := storage.S3Status(c)
		if !st.Ready || !strings.Contains(st.RepairNeeded, "not the image this version deploys") {
			t.Errorf("status = %+v", st)
		}
	})
	for _, tc := range []struct{ name, svc, route, want string }{
		{"Service missing", "", "ok", "Service seaweedfs-s3 is missing"},
		{"Service selects something else", `{"selector":{"app":"other"},"ports":[{"name":"s3","port":8333,"targetPort":8333},{"name":"ui","port":12535,"targetPort":12535}]}`, "ok", "selects app=other"},
		{"S3 port elsewhere", `{"selector":{"app":"seaweedfs"},"ports":[{"name":"s3","port":8333,"targetPort":9000},{"name":"ui","port":12535,"targetPort":12535}]}`, "ok", "port s3 forwards to 9000"},
		{"admin UI port elsewhere", `{"selector":{"app":"seaweedfs"},"ports":[{"name":"s3","port":8333,"targetPort":8333},{"name":"ui","port":12535,"targetPort":23646}]}`, "ok", "port ui forwards to 23646"},
		{"Route missing", "ok", "", "Route seaweedfs-admin (the admin UI) is missing"},
		{"Route elsewhere", "ok", `{"to":{"kind":"Service","name":"other"},"port":{"targetPort":"ui"}}`, "does not point at port ui"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putReadyManagedS3(f)
			switch tc.svc {
			case "":
				f.mu.Lock()
				delete(f.objects, fakeKey{"v1", "seaweedfs", "services", "seaweedfs-s3"})
				f.mu.Unlock()
			case "ok":
			default:
				f.putJSON(s3SvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":`+tc.svc+`}`)
			}
			switch tc.route {
			case "":
				f.mu.Lock()
				delete(f.objects, fakeKey{"route.openshift.io/v1", "seaweedfs", "routes", "seaweedfs-admin"})
				f.mu.Unlock()
			case "ok":
			default:
				f.putJSON(s3RoutePath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":`+tc.route+`}`)
			}
			st := storage.S3Status(c)
			if st.Ready || !st.TerminalError || !strings.Contains(st.RepairNeeded, tc.want) {
				t.Errorf("status = %+v, want repair reason with %q", st, tc.want)
			}
		})
	}
}

func TestStorageAddressAliases(t *testing.T) {
	a := storage.Address{Service: "seaweedfs-s3", Namespace: "seaweedfs", ClusterIP: "172.30.20.30", RouteHosts: []string{"seaweedfs-admin-seaweedfs.apps.example.com", ""}}
	cases := []struct {
		ns, host string
		want     bool
	}{
		{"team", "seaweedfs-s3.seaweedfs.svc:8333", true},
		{"team", "seaweedfs-s3.seaweedfs", true},
		{"team", "seaweedfs-s3.seaweedfs.svc", true},
		{"team", "seaweedfs-s3.seaweedfs.svc.cluster.local", true},
		{"team", "seaweedfs-s3.seaweedfs.svc.cluster.local.", true},
		{"team", "SEAWEEDFS-S3.SeaweedFS.SVC:8333", true},
		{"team", "http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333", true},
		{"team", "https://seaweedfs-s3.seaweedfs:8333/pipelines", true},
		{"team", "seaweedfs-s3.seaweedfs.svc:bad", true},
		{"team", "172.30.20.30:8333", true},
		{"team", "seaweedfs-admin-seaweedfs.apps.example.com:443", true},
		{"seaweedfs", "seaweedfs-s3", true},
		{"seaweedfs", "seaweedfs-s3:8333", true},
		{"team", "seaweedfs-s3", false}, // resolves to team's own service
		{"team", "seaweedfs-s3.other.svc", false},
		{"team", "minio-service.minio.svc:9000", false},
		{"team", "s3.amazonaws.com", false},
		{"team", "", false},
	}
	for _, tc := range cases {
		if got := a.UsedBy(tc.ns, tc.host); got != tc.want {
			t.Errorf("usedBy(%q, %q) = %v, want %v", tc.ns, tc.host, got, tc.want)
		}
	}
	legacy := storage.Address{Service: "minio-service", Namespace: "minio", RouteHosts: []string{"minio-api-minio.apps.example.com"}}
	dspas := []storage.DSPA{
		{Meta: kube.ObjectMeta{Namespace: "a", Name: "x"}, Host: "http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333"},
		{Meta: kube.ObjectMeta{Namespace: "b", Name: "y"}, Host: "minio-api-minio.apps.example.com"},
		{Meta: kube.ObjectMeta{Namespace: "c", Name: "z"}, Host: "s3.amazonaws.com"},
		{Meta: kube.ObjectMeta{Namespace: "d", Name: "w", DeletionTimestamp: "2026-10-01T00:00:00Z"}, Host: "minio-service.minio.svc:9000"},
	}
	got := storage.S3TeardownBlocker(dspas, a, legacy)
	if !strings.Contains(got, "2 pipeline servers use this S3 storage: a/x, b/y.") || strings.Contains(got, "c/z") || !strings.Contains(got, "still being deleted: d/w") {
		t.Errorf("blocker = %q", got)
	}
	if got := storage.S3TeardownBlocker(dspas, a); strings.Contains(got, "b/y") || strings.Contains(got, "d/w") {
		t.Errorf("blocker without the earlier storage = %q", got)
	}
}

// putDeployedS3 puts a storage the tool set up, with its data PVC.
func putDeployedS3(f *resourceFake) {
	putReadyManagedS3(f)
	f.putJSON(s3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(s3NPPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
}

func TestTeardownS3Storage(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if !resp.Success {
		t.Fatalf("teardown = %+v", resp)
	}
	for _, path := range []string{s3DeployPath, s3PVCPath, s3SecretPath, s3SvcPath, s3RoutePath, s3NPPath} {
		if f.has(path) {
			t.Errorf("%s was not deleted", path)
		}
	}
	if !f.has(s3NSPath) || !strings.Contains(resp.Message, "Namespace seaweedfs was kept") || !strings.Contains(resp.Message, "seaweedfs/seaweedfs-data") {
		t.Errorf("teardown = %+v", resp)
	}
	// Re-running finds nothing left.
	if again, _ := TeardownS3Storage(c, types.S3TeardownRequest{}); !again.Success || !strings.Contains(again.Message, "No S3 storage objects created by this tool remain") {
		t.Errorf("re-run = %+v", again)
	}
}

// putToolNamespace puts namespace seaweedfs as this tool creates it: the
// tool label, written by the tool's field manager.
func putToolNamespace(f *resourceFake) {
	f.putJSON(s3NSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(envkit.ToolFieldManager, "Apply")+`},"status":{"phase":"Active"}}`)
}

// Tear down deletes namespace seaweedfs only when asked, only once every
// object is gone, and only when this tool created it.
func TestTeardownS3Storage_DeleteNamespace(t *testing.T) {
	deleted := func(f *resourceFake) bool { return hasExactMutation(f, "DELETE "+s3NSPath) }
	t.Run("ticked, created by this tool: deleted after the objects, by UID", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putDeployedS3(f)
		putToolNamespace(f)
		resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{DeleteNamespace: true})
		if !resp.Success || !deleted(f) || f.has(s3NSPath) || !strings.Contains(resp.Message, "Namespace seaweedfs is being deleted, with everything else in it.") || strings.Contains(resp.Message, "oc delete project") {
			t.Fatalf("teardown = %+v", resp)
		}
		if f.has(s3DeployPath) || f.has(s3SecretPath) {
			t.Error("the storage's objects are left")
		}
	})
	t.Run("ticked, the namespace was recreated since it was checked: the UID precondition keeps it", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putDeployedS3(f)
		putToolNamespace(f)
		f.onGet = func(k fakeKey, obj map[string]any) {
			if k.plural == "namespaces" && k.name == storage.S3Namespace {
				obj["metadata"].(map[string]any)["uid"] = "uid-seen-by-check"
			}
		}
		resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{DeleteNamespace: true})
		if resp.Success || !f.has(s3NSPath) || !strings.Contains(resp.Message, "namespace seaweedfs") {
			t.Fatalf("teardown = %+v", resp)
		}
	})
	t.Run("not ticked: kept, with the command", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putDeployedS3(f)
		putToolNamespace(f)
		resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
		if !resp.Success || deleted(f) || !strings.Contains(resp.Message, "oc delete project seaweedfs") {
			t.Fatalf("teardown = %+v", resp)
		}
	})
	t.Run("ticked, labelled but never written by this tool: kept, with why", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putDeployedS3(f)
		putS3Namespace(f, toolLabelJSON)
		resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{DeleteNamespace: true})
		if !resp.Success || deleted(f) || !strings.Contains(resp.Message, "its managedFields show no write by this tool") {
			t.Fatalf("teardown = %+v", resp)
		}
	})
	t.Run("ticked, an object cannot be deleted: the namespace stays", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putDeployedS3(f)
		putToolNamespace(f)
		f.fail["DELETE "+s3SecretPath] = 500
		resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{DeleteNamespace: true})
		if resp.Success || deleted(f) {
			t.Fatalf("teardown = %+v", resp)
		}
	})
}

// The status says whether Tear down may offer the namespace, and what else
// the tool sees in it.
func TestGetS3Status_NamespaceDeletion(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	putToolNamespace(f)
	f.putJSON("/apis/apps/v1/namespaces/seaweedfs/deployments/notes-app", `{"metadata":{}}`)
	f.putJSON("/api/v1/namespaces/seaweedfs/persistentvolumeclaims/scratch", `{"metadata":{}}`)
	st := storage.S3Status(c)
	if !st.NamespaceDeletable || st.NamespaceKeptReason != "" || strings.Join(st.OtherObjects, ", ") != "Deployment notes-app, PersistentVolumeClaim scratch" {
		t.Fatalf("status = %+v", st)
	}
	putS3Namespace(f, toolLabelJSON)
	if st := storage.S3Status(c); st.NamespaceDeletable || !strings.Contains(st.NamespaceKeptReason, "no write by this tool") {
		t.Fatalf("status = %+v", st)
	}
}

func TestTeardownS3Storage_KeepsForeignObjects(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	f.putJSON(s3SecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`))
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if !resp.Success || !strings.Contains(resp.Message, "Left in place because this tool did not create them: Secret seaweedfs/seaweedfs-s3-credentials") {
		t.Fatalf("teardown = %+v", resp)
	}
	assertForeignUntouched(t, f, s3SecretPath, "data", "dXNlcg==")
}

func TestTeardownS3Storage_DeleteUsesUIDPrecondition(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	// The ownership check sees a different object than the one the DELETE
	// reaches, as if the Deployment had been recreated in between.
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "deployments" && k.name == storage.S3Deployment {
			obj["metadata"].(map[string]any)["uid"] = "uid-seen-by-check"
		}
	}
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if resp.Success || !f.has(s3DeployPath) || !strings.Contains(resp.Message, "Deployment seaweedfs/seaweedfs") {
		t.Fatalf("a Deployment with a different UID must not be deleted: %+v", resp)
	}
}

func TestTeardownS3Storage_BlockedByPipelineServers(t *testing.T) {
	for _, host := range []string{"seaweedfs-s3.seaweedfs.svc:8333", "172.30.20.30:8333", "https://seaweedfs-admin-seaweedfs.apps.example.com"} {
		t.Run(host, func(t *testing.T) {
			fastS3Timings(t)
			f, c := newResourceFake(t)
			putDeployedS3(f)
			putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", host, "s", "", "")
			resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
			if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "team/dspa") {
				t.Fatalf("teardown = %+v", resp)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("a blocked teardown wrote %v", m)
			}
		})
	}
}

// When the Route host or the ClusterIP cannot be read, a pipeline server
// may depend on them: teardown refuses rather than delete the data.
func TestTeardownS3Storage_FailsClosedWhenEndpointsUnreadable(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", "s3.amazonaws.com", "s", "", "")
	f.fail["GET "+s3RoutePath] = 500
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if resp.Success || !strings.Contains(resp.Message, "nothing was deleted") {
		t.Fatalf("teardown = %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("teardown wrote %v", m)
	}
	// The status says so too.
	st, err := GetResourcesStatus(c)
	if err != nil || !strings.Contains(st.MinIO.TeardownBlockedReason, "Cannot verify which pipeline servers use the S3 storage") {
		t.Errorf("status = %+v (%v)", st.MinIO, err)
	}
}

// Without any pipeline server there is nothing to protect.
func TestTeardownS3Storage_UnreadableEndpointsWithoutPipelineServersProceeds(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	f.fail["GET "+s3RoutePath] = 500
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if resp.Success || resp.ErrorCode != "partial_failure" || f.has(s3DeployPath) {
		t.Fatalf("teardown = %+v", resp)
	}
}

func TestTeardownS3Storage_NothingDeployed(t *testing.T) {
	fastS3Timings(t)
	_, c := newResourceFake(t)
	if resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{}); !resp.Success || !strings.Contains(resp.Message, "nothing to remove") {
		t.Fatalf("teardown = %+v", resp)
	}
}

func TestWaitS3Ready_StalledAPIDoesNotOverrunDeadline(t *testing.T) {
	fastS3Timings(t)
	storage.SetS3ReadyTimeoutForTest(200 * time.Millisecond)
	c := stallServer(t, 0)
	start := kube.Clock().Now()
	_, err := storage.WaitS3Ready(c, storage.SeaweedFSDefaultImage)
	if !errors.Is(err, storage.ErrS3NotReady) {
		t.Fatalf("err = %v, want errS3NotReady", err)
	}
	if d := kube.Clock().Since(start); d != 200*time.Millisecond {
		t.Errorf("waited %s, want the 200ms deadline", d)
	}
}

// The admin UI reads its own password from the storage's Secret, never the
// S3 secret key; the pipeline projects' copies carry only the S3 keys.
func TestS3AdminPasswordIsItsOwn(t *testing.T) {
	keys := storage.S3Keys{AccessKey: "ak", SecretKey: "sk", AdminPassword: "ap"}
	for _, r := range storage.S3Resources(keys, false) {
		b, _ := json.Marshal(r.Obj)
		switch r.Kind {
		case "Secret":
			if !strings.Contains(string(b), `"WEED_ADMIN_PASSWORD":"ap"`) {
				t.Errorf("the Secret does not hold the admin password: %s", b)
			}
		case "Deployment":
			if !strings.Contains(string(b), `{"name":"WEED_ADMIN_PASSWORD","valueFrom":{"secretKeyRef":{"key":"WEED_ADMIN_PASSWORD","name":"seaweedfs-s3-credentials"}}}`) {
				t.Errorf("WEED_ADMIN_PASSWORD does not come from its own key: %s", b)
			}
		}
	}
	var old storage.S3DeploymentSpec
	_ = json.Unmarshal([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"seaweedfs","env":[{"name":"WEED_ADMIN_PASSWORD","valueFrom":{"secretKeyRef":{"name":"seaweedfs-s3-credentials","key":"AWS_SECRET_ACCESS_KEY"}}}]}]}}}}`), &old)
	if !old.AdminPasswordIsS3Key() {
		t.Error("a Deployment whose admin password is the S3 secret key is not noticed")
	}
}

// Tear down stops Migrate storage's copy Job (by UID, only the tool's): it
// must not keep copying into a storage being deleted.
func TestTeardownS3Storage_DeletesTheCopyJob(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	f.putJSON(migrateJobPathT, `{"metadata":{"uid":"copy-job","labels":`+toolLabelJSON+`}}`)
	if resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{}); !resp.Success || f.has(migrateJobPathT) {
		t.Fatalf("teardown = %+v; job left: %v", resp, f.has(migrateJobPathT))
	}
	f2, c2 := newResourceFake(t)
	putDeployedS3(f2)
	f2.putJSON(migrateJobPathT, `{"metadata":{"uid":"theirs"}}`)
	TeardownS3Storage(c2, types.S3TeardownRequest{})
	if !f2.has(migrateJobPathT) {
		t.Error("a Job of that name the tool did not create was deleted")
	}
}

// A removal past the copy is not stopped by MinIO's uncopiable volume, and
// a server whose data volume is being deleted is not offered as "start it
// again" (it cannot start).
func TestSourceGone_UncopiableAndTerminatingPVCs(t *testing.T) {
	pvc := func(name string, terminating bool) storage.PVC {
		p := storage.PVC{Meta: kube.ObjectMeta{Name: name}}
		if terminating {
			now := "2026-11-17T12:00:00Z"
			p.Meta.DeletionTimestamp = now
		}
		return p
	}
	l := &storage.Legacy{Server: "SeaweedFS", PVCs: []storage.PVC{pvc(storage.LegacyMinIOPVC, false), pvc("seaweedfs-pvc", true)}}
	if got := l.CopiablePVCsNotDeleting(); len(got) != 0 {
		t.Errorf("pending = %v, want none (minio-pvc cannot be copied)", got)
	}
	l = &storage.Legacy{Server: "SeaweedFS", PVCs: []storage.PVC{pvc("seaweedfs-pvc", true), pvc("other-data", false)}}
	msg := storage.SourceGoneMessage(l, "SeaweedFS there is not running", l.CopiablePVCsNotDeleting())
	if strings.Contains(msg, "Start the server again") || !strings.Contains(msg, "oc delete pvc other-data -n minio") {
		t.Errorf("message = %s", msg)
	}
}

// N8: once the Deployment is gone, this tool's other storage objects a
// teardown did not delete (it tries each one) are reported from the
// cluster, so Delete is offered again because they are there, not because
// the browser remembers that the last teardown failed.
func TestS3Status_NamesTheToolsObjectsATeardownLeft(t *testing.T) {
	f, c := newResourceFake(t)
	putS3Namespace(f, toolLabelJSON)
	f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON("/api/v1/namespaces/seaweedfs/services/seaweedfs-s3", `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	st := storage.S3Status(c)
	if st.Deployed || strings.Join(st.Leftovers, ",") != "Secret seaweedfs-s3-credentials,Service seaweedfs-s3" {
		t.Fatalf("status = %+v", st)
	}
	// Someone else's object of the name is not this tool's leftover.
	f.putJSON("/api/v1/namespaces/seaweedfs/services/seaweedfs-s3", `{"metadata":{}}`)
	if st := storage.S3Status(c); strings.Join(st.Leftovers, ",") != "Secret seaweedfs-s3-credentials" {
		t.Fatalf("leftovers = %v", st.Leftovers)
	}
}
