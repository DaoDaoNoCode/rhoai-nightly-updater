package cluster

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	wbNotebooks  = "/apis/kubeflow.org/v1/namespaces/proj/notebooks"
	wbNotebook   = wbNotebooks + "/" + workbench.Name
	wbPVCs       = "/api/v1/namespaces/proj/persistentvolumeclaims"
	wbPVC        = wbPVCs + "/" + workbench.StorageName
	wbDSCv2      = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	wbApps       = "redhat-ods-applications"
	wbStreams    = "/apis/image.openshift.io/v1/namespaces/" + wbApps + "/imagestreams/"
	wbProfiles   = "/apis/infrastructure.opendatahub.io/v1/namespaces/" + wbApps + "/hardwareprofiles/"
	wbClasses    = "/apis/storage.k8s.io/v1/storageclasses/"
	wbProjectNS  = "/api/v1/namespaces/proj"
	wbConnection = "/api/v1/namespaces/proj/secrets/rhoai-nightly-updater-s3"
)

// UsernameTranslate must align with the dashboard helper exactly—same bytes,
// including the mapping of "~" to %7f.
func TestWorkbenchUsernameTranslate(t *testing.T) {
	for in, want := range map[string]string{
		"kube:admin": "kube-3aadmin",
		"jane.doe":   "jane-2edoe",
		"a-b":        "a-2db",
		"x_y":        "x-5fy",
		"t~1":        "t-7f1",
		"Ann Lee!":   "ann-20lee-21",
		"me@x.com":   "me-40x-2ecom",
	} {
		if got := workbench.UsernameTranslate(in); got != want {
			t.Errorf("UsernameTranslate(%q) = %q, want %q", in, got, want)
		}
	}
}

func wbPlan() workbench.Plan {
	return workbench.Plan{
		Project: "proj", Username: "kube:admin",
		Image:       workbench.ImageChoice{Stream: "s2i-minimal-notebook", Namespace: wbApps, Tag: "2025.2", Repository: "image-registry.openshift-image-registry.svc:5000/redhat-ods-applications/s2i-minimal-notebook", DisplayName: "Jupyter | Minimal | CPU | Python 3.12", BuildCommit: "abc123"},
		Profile:     workbench.ProfileChoice{Name: "default-profile", Namespace: wbApps, ResourceVersion: "42", DisplayName: "Default", Resources: map[string]any{"cpu": float64(2), "memory": "4Gi"}},
		StorageSize: "20Gi", StorageClass: "gp3-csi", Connection: true, Pipelines: true, MLflow: true,
	}
}

// NotebookManifest should match assembleNotebook combined with
// applyHardwareProfileConfig (odh-dashboard rhoai-3.6 api/k8s/notebooks.ts
// L92-192), augmented by this tool's label and omitting envFrom.
func TestWorkbenchNotebookEqualsTheDashboards(t *testing.T) {
	image := "image-registry.openshift-image-registry.svc:5000/redhat-ods-applications/s2i-minimal-notebook:2025.2"
	probe := `{"initialDelaySeconds":10,"periodSeconds":5,"timeoutSeconds":1,"successThreshold":1,"failureThreshold":3,"httpGet":{"scheme":"HTTP","path":"/notebook/proj/nightly-workbench/api","port":"notebook-port"}}`
	want := `{
	"apiVersion":"kubeflow.org/v1","kind":"Notebook",
	"metadata":{"name":"nightly-workbench","namespace":"proj",
		"labels":{"app":"nightly-workbench","opendatahub.io/odh-managed":"true","opendatahub.io/dashboard":"true","app.kubernetes.io/managed-by":"rhoai-nightly-updater"},
		"annotations":{
			"openshift.io/display-name":"Nightly workbench (RHOAI Nightly Updater)",
			"openshift.io/description":"",
			"notebooks.opendatahub.io/last-image-selection":"s2i-minimal-notebook:2025.2",
			"notebooks.opendatahub.io/inject-auth":"true",
			"opendatahub.io/user":"kube-3aadmin",
			"opendatahub.io/username":"kube:admin",
			"notebooks.opendatahub.io/last-image-version-git-commit-selection":"abc123",
			"opendatahub.io/connections":"proj/rhoai-nightly-updater-s3",
			"opendatahub.io/mlflow-instance":"mlflow",
			"opendatahub.io/hardware-profile-name":"default-profile",
			"opendatahub.io/hardware-profile-namespace":"redhat-ods-applications",
			"opendatahub.io/hardware-profile-resource-version":"42",
			"opendatahub.io/image-display-name":"Jupyter | Minimal | CPU | Python 3.12"}},
	"spec":{"template":{"spec":{
		"enableServiceLinks":false,
		"containers":[{
			"image":"` + image + `","imagePullPolicy":"Always","workingDir":"/opt/app-root/src","name":"nightly-workbench",
			"env":[{"name":"NOTEBOOK_ARGS","value":"--ServerApp.port=8888\n                  --ServerApp.token=''\n                  --ServerApp.password=''\n                  --ServerApp.base_url=/notebook/proj/nightly-workbench\n                  --ServerApp.quit_button=False"},{"name":"JUPYTER_IMAGE","value":"` + image + `"}],
			"volumeMounts":[{"name":"nightly-workbench-storage","mountPath":"/opt/app-root/src/"},{"name":"elyra-dsp-details","mountPath":"/opt/app-root/runtimes"},{"name":"shm","mountPath":"/dev/shm"}],
			"ports":[{"name":"notebook-port","containerPort":8888,"protocol":"TCP"}],
			"livenessProbe":` + probe + `,"readinessProbe":` + probe + `,
			"resources":{"requests":{"cpu":2,"memory":"4Gi"},"limits":{"cpu":2,"memory":"4Gi"}}}],
		"volumes":[{"name":"nightly-workbench-storage","persistentVolumeClaim":{"claimName":"nightly-workbench-storage"}},{"name":"elyra-dsp-details","secret":{"secretName":"ds-pipeline-config","optional":true}},{"name":"shm","emptyDir":{"medium":"Memory"}}]}}}}`
	assertJSONEqual(t, workbench.NotebookManifest(wbPlan()), want)
	raw, _ := json.Marshal(workbench.NotebookManifest(wbPlan()))
	if strings.Contains(string(raw), "envFrom") {
		t.Fatalf("the Notebook writes envFrom: %s", raw)
	}

	// Disabling Connection, pipelines, and MLflow leaves connections as ""
	// like the dashboard does, with no Elyra volume or MLflow link present.
	p := wbPlan()
	p.Connection, p.Pipelines, p.MLflow = false, false, false
	nb := workbench.NotebookManifest(p)
	ann := nb["metadata"].(map[string]any)["annotations"].(map[string]any)
	if v, ok := ann["opendatahub.io/connections"]; !ok || v != "" {
		t.Fatalf("connections annotation: %v %v", v, ok)
	}
	if _, ok := ann["opendatahub.io/mlflow-instance"]; ok {
		t.Fatal("mlflow-instance without MLflow")
	}
	raw, _ = json.Marshal(nb)
	if strings.Contains(string(raw), "elyra-dsp-details") || strings.Contains(string(raw), "workbench-image-namespace") {
		t.Fatalf("unexpected fields: %s", raw)
	}
}

func TestWorkbenchPVCEqualsTheDashboards(t *testing.T) {
	assertJSONEqual(t, workbench.PVCManifest(wbPlan()), `{"apiVersion":"v1","kind":"PersistentVolumeClaim",
		"metadata":{"name":"nightly-workbench-storage","namespace":"proj","labels":{"opendatahub.io/dashboard":"true","app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"openshift.io/display-name":"Nightly workbench (RHOAI Nightly Updater)"}},
		"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"20Gi"}},"volumeMode":"Filesystem","storageClassName":"gp3-csi"}}`)
	p := wbPlan()
	p.StorageClass = ""
	if _, ok := workbench.PVCManifest(p)["spec"].(map[string]any)["storageClassName"]; ok {
		t.Fatal("storageClassName without a class")
	}
}

func assertJSONEqual(t *testing.T, got any, want string) {
	t.Helper()
	raw, _ := json.Marshal(got)
	var g, w any
	_ = json.Unmarshal(raw, &g)
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		wr, _ := json.MarshalIndent(w, "", " ")
		gr, _ := json.MarshalIndent(g, "", " ")
		t.Fatalf("got\n%s\nwant\n%s", gr, wr)
	}
}

func stream(t *testing.T, js string) workbench.ImageStream {
	t.Helper()
	var s workbench.ImageStream
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ChooseImage prefers a stream whose imported tag carries default-image;
// otherwise the first stream by tier then order. Outdated or unimported tags
// are ignored; recommended beats newest when both qualify.
func TestWorkbenchChooseImage(t *testing.T) {
	minimal := stream(t, `{"metadata":{"name":"minimal","namespace":"apps","annotations":{"opendatahub.io/notebook-image-name":"Minimal","opendatahub.io/notebook-image-order":"5"}},
		"spec":{"tags":[{"name":"2024.1"},{"name":"2025.2","annotations":{"opendatahub.io/default-image":"true","opendatahub.io/notebook-build-commit":"c1"}}]},
		"status":{"dockerImageRepository":"reg/minimal","tags":[{"tag":"2024.1"},{"tag":"2025.2"}]}}`)
	datasci := stream(t, `{"metadata":{"name":"datascience","namespace":"apps","annotations":{"opendatahub.io/notebook-image-order":"1","opendatahub.io/notebook-tier":"secure"}},
		"spec":{"tags":[{"name":"2024.1"},{"name":"2025.2"},{"name":"2026.1","annotations":{"opendatahub.io/image-tag-outdated":"true"}}]},
		"status":{"dockerImageRepository":"reg/ds","tags":[{"tag":"2024.1"},{"tag":"2025.2"},{"tag":"2026.1"}]}}`)
	hidden := stream(t, `{"metadata":{"name":"hidden","annotations":{"opendatahub.io/notebook-image-hidden":"true","opendatahub.io/notebook-tier":"secure","opendatahub.io/notebook-image-order":"0"}},
		"spec":{"tags":[{"name":"1","annotations":{"opendatahub.io/default-image":"true"}}]},"status":{"tags":[{"tag":"1"}]}}`)
	got, ok := workbench.ChooseImage([]workbench.ImageStream{datasci, hidden, minimal})
	if !ok || got.Stream != "minimal" || got.Tag != "2025.2" || got.BuildCommit != "c1" || got.DisplayName != "Minimal" || got.Repository != "reg/minimal" {
		t.Fatalf("default image: %+v %v", got, ok)
	}
	// Absent default-image, pick secure tier first and the latest usable tag there.
	minimal.Spec.Tags[1].Annotations = nil
	got, _ = workbench.ChooseImage([]workbench.ImageStream{minimal, datasci})
	if got.Stream != "datascience" || got.Tag != "2025.2" || got.DisplayName != "datascience" {
		t.Fatalf("selector order: %+v", got)
	}
	// workbench-image-recommended overrides a higher version number.
	datasci.Spec.Tags[0].Annotations = map[string]string{"opendatahub.io/workbench-image-recommended": "true"}
	if got, _ = workbench.ChooseImage([]workbench.ImageStream{datasci}); got.Tag != "2024.1" {
		t.Fatalf("recommended: %+v", got)
	}
	// Tags absent from status.tags cannot be selected.
	notImported := stream(t, `{"metadata":{"name":"x"},"spec":{"tags":[{"name":"1"}]},"status":{"tags":[]}}`)
	if _, ok := workbench.ChooseImage([]workbench.ImageStream{notImported, hidden}); ok {
		t.Fatal("chose an image with no usable tag")
	}
}

func profile(t *testing.T, js string) workbench.HardwareProfile {
	t.Helper()
	var p workbench.HardwareProfile
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

const cpuMem = `"identifiers":[{"identifier":"cpu","defaultCount":2,"resourceType":"CPU"},{"identifier":"memory","defaultCount":"4Gi","resourceType":"Memory"}]`

// ChooseProfile follows dashboard defaults for a fresh workbench, including
// how Kueue-backed profiles are filtered.
func TestWorkbenchChooseProfile(t *testing.T) {
	disabled := profile(t, `{"metadata":{"name":"a-disabled","namespace":"apps","annotations":{"opendatahub.io/disabled":"true"}},"spec":{`+cpuMem+`}}`)
	servingOnly := profile(t, `{"metadata":{"name":"b-serving","namespace":"apps","annotations":{"opendatahub.io/dashboard-feature-visibility":"[\"model-serving\"]"}},"spec":{`+cpuMem+`}}`)
	invalid := profile(t, `{"metadata":{"name":"c-gpu-only","namespace":"apps"},"spec":{"identifiers":[{"identifier":"nvidia.com/gpu","defaultCount":1}]}}`)
	dra := profile(t, `{"metadata":{"name":"d-dra","namespace":"apps"},"spec":{`+cpuMem+`,"dra":{"claims":[]}}}`)
	queue := profile(t, `{"metadata":{"name":"e-queue","namespace":"apps"},"spec":{`+cpuMem+`,"scheduling":{"type":"Queue","kueue":{"localQueueName":"lq"}}}}`)
	node := profile(t, `{"metadata":{"name":"f-default","namespace":"apps","resourceVersion":"7","annotations":{"opendatahub.io/display-name":"Default"}},"spec":{`+cpuMem+`,"scheduling":{"type":"Node","node":{}}}}`)
	global := []workbench.HardwareProfile{disabled, servingOnly, invalid, dra, queue, node}

	got, why := workbench.ChooseProfile(global, nil, workbench.OnlyNonKueue, nil)
	if why != "" || got.Name != "f-default" || got.DisplayName != "Default" || got.ResourceVersion != "7" || got.Resources["cpu"] != float64(2) || got.Resources["memory"] != "4Gi" {
		t.Fatalf("non-Kueue project: %+v %q", got, why)
	}
	got, why = workbench.ChooseProfile(global, nil, workbench.OnlyKueue, []string{"lq"})
	if why != "" || got.Name != "e-queue" || got.Queue != "lq" {
		t.Fatalf("Kueue project with the queue: %+v %q", got, why)
	}
	if _, why = workbench.ChooseProfile(global, nil, workbench.OnlyKueue, []string{"other"}); !strings.Contains(why, "LocalQueue") {
		t.Fatalf("Kueue project without the queue: %q", why)
	}
	if _, why = workbench.ChooseProfile(global, nil, workbench.NoProfiles, nil); !strings.Contains(why, "Kueue is not available") {
		t.Fatalf("Kueue project, Kueue off: %q", why)
	}
	if _, why = workbench.ChooseProfile([]workbench.HardwareProfile{disabled, invalid}, nil, workbench.OnlyNonKueue, nil); !strings.Contains(why, "No enabled hardware profile") {
		t.Fatalf("none: %q", why)
	}
	// Application-namespace profiles outrank those scoped to the project.
	local := profile(t, `{"metadata":{"name":"a-local","namespace":"proj"},"spec":{`+cpuMem+`}}`)
	if got, _ = workbench.ChooseProfile([]workbench.HardwareProfile{node}, []workbench.HardwareProfile{local}, workbench.OnlyNonKueue, nil); got.Name != "f-default" {
		t.Fatalf("order: %+v", got)
	}
	if workbench.ModeFor(true, true) != workbench.OnlyKueue || workbench.ModeFor(true, false) != workbench.NoProfiles || workbench.ModeFor(false, true) != workbench.OnlyNonKueue {
		t.Fatal("ModeFor")
	}
}

func TestWorkbenchChooseStorageClass(t *testing.T) {
	sc := func(name, ann string) workbench.StorageClass {
		var s workbench.StorageClass
		_ = json.Unmarshal([]byte(`{"metadata":{"name":"`+name+`","annotations":`+ann+`}}`), &s)
		return s
	}
	clusterDefault := sc("gp3", `{"storageclass.kubernetes.io/is-default-class":"true"}`)
	admin := sc("fast", `{"opendatahub.io/sc-config":"{\"isEnabled\":true,\"isDefault\":true}"}`)
	plain := sc("slow", `{}`)
	for _, tt := range []struct {
		classes    []workbench.StorageClass
		configured string
		want       string
	}{
		{[]workbench.StorageClass{clusterDefault, admin}, "slow", "fast"},
		{[]workbench.StorageClass{clusterDefault, plain}, "slow", "gp3"},
		{[]workbench.StorageClass{plain}, "slow", "slow"},
		{[]workbench.StorageClass{plain}, "missing", ""},
		{nil, "", ""},
	} {
		if got := workbench.ChooseStorageClass(tt.classes, tt.configured); got != tt.want {
			t.Errorf("%v %q: got %q want %q", tt.classes, tt.configured, got, tt.want)
		}
	}
}

// putWorkbenchCluster primes the fake with cluster state setup consults: DSC v2
// (workbenches Managed), labelled project, image stream, hardware profile,
// StorageClass; optional dry-run Notebook responses include kube-rbac-proxy
// when mimicking odh-notebook-controller injection.
func putWorkbenchCluster(t *testing.T, f *resourceFake, sidecar bool) {
	t.Helper()
	f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Managed"},"mlflowoperator":{"managementState":"Removed"}}},"status":{"components":{"workbenches":{"managementState":"Managed"}}}}`)
	f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	f.putJSON(wbStreams+"s2i-minimal-notebook", `{"metadata":{"labels":{"opendatahub.io/notebook-image":"true"},"annotations":{"opendatahub.io/notebook-image-name":"Jupyter | Minimal | CPU | Python 3.12"}},
		"spec":{"tags":[{"name":"2025.2","annotations":{"opendatahub.io/default-image":"true"}}]},"status":{"dockerImageRepository":"reg/minimal","tags":[{"tag":"2025.2"}]}}`)
	f.putJSON(wbProfiles+"default-profile", `{"metadata":{"resourceVersion":"9"},"spec":{`+cpuMem+`}}`)
	f.putJSON(wbClasses+"gp3-csi", `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`)
	f.intercept = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method != http.MethodPost || r.URL.Path != wbNotebooks || r.URL.Query().Get("dryRun") != "All" {
			return false
		}
		var nb map[string]any
		_ = json.Unmarshal(body, &nb)
		if sidecar {
			spec := nb["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "kube-rbac-proxy"})
		}
		f.write(w, 201, nb)
		return true
	}
}

func wbSteps(t *testing.T, c *Client, req types.WorkbenchRequest) *types.OperationResponse {
	t.Helper()
	res, err := RunWorkbench(c, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// setup issues dry-run then live POSTs for PVC and Notebook on the user token,
// persists both UIDs, and leaves the Notebook manifest dashboard-shaped without envFrom.
func TestWorkbenchSetup(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("kube:admin")
	res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, p := range []string{wbNotebooks, wbPVCs} {
		calls := f.callsTo("POST", p)
		if len(calls) != 2 || calls[0].Query.Get("dryRun") != "All" || calls[1].Query.Get("dryRun") != "" {
			t.Fatalf("%s: want a dry run then the create, got %+v", p, calls)
		}
		for _, call := range calls {
			if call.Token != fakeUserToken {
				t.Fatalf("%s with the ServiceAccount's token", p)
			}
		}
	}
	nb := f.get(wbNotebook)
	raw, _ := json.Marshal(nb)
	if strings.Contains(string(raw), "envFrom") {
		t.Fatalf("envFrom written: %s", raw)
	}
	ann := nb["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["notebooks.opendatahub.io/last-image-selection"] != "s2i-minimal-notebook:2025.2" || ann["opendatahub.io/hardware-profile-name"] != "default-profile" || ann["opendatahub.io/user"] != "kube-3aadmin" || ann["opendatahub.io/connections"] != "" {
		t.Fatalf("annotations: %v", ann)
	}
	if _, ok := ann["opendatahub.io/mlflow-instance"]; ok {
		t.Fatal("MLflow linked while its component is Removed")
	}
	if sc := f.get(wbPVC)["spec"].(map[string]any)["storageClassName"]; sc != "gp3-csi" {
		t.Fatalf("storage class %v", sc)
	}
	rec, err := workbench.ReadRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := rec.Get("proj")
	if e.UID != nb["metadata"].(map[string]any)["uid"] || e.StorageUID != f.get(wbPVC)["metadata"].(map[string]any)["uid"] || e.UID == "" {
		t.Fatalf("record %+v", e)
	}

	// A second setup call is idempotent and emits no further mutations.
	before := len(f.mutations())
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success || !strings.Contains(res.Message, "already set up") {
		t.Fatalf("second setup: %+v", res)
	}
	if len(f.mutations()) != before {
		t.Fatalf("second setup wrote: %v", f.mutations()[before:])
	}

	// Workbench status fetched with the same user token should list Starting immediately after setup.
	st, err := GetWorkbenchStatus(c, u, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Items) != 1 || !st.Items[0].ManagedByTool || st.Items[0].State != workbench.StateStarting || st.Items[0].Storage == nil || !st.Items[0].Storage.ManagedByTool {
		t.Fatalf("status items: %+v", st.Items)
	}
	if st.Plan == nil || st.Plan.Image != "Jupyter | Minimal | CPU | Python 3.12" || st.Plan.HardwareProfile != "default-profile" || st.Plan.StorageSize != "20Gi" {
		t.Fatalf("plan: %+v %q", st.Plan, st.PlanProblem)
	}
}

// With DSPA-owned Connection secret present, linking happens solely through
// annotations; ds-pipeline-config supplies the Elyra mount; a Managed
// mlflowoperator component adds the mlflow-instance annotation.
func TestWorkbenchSetupConnectionPipelinesMLflow(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"status":{"components":{"workbenches":{"managementState":"Managed"},"mlflowoperator":{"managementState":"Managed"}}},"spec":{"components":{"workbenches":{"managementState":"Managed"}}}}`)
	f.putJSON(wbConnection, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"d1"}]}}`)
	f.putJSON("/api/v1/namespaces/proj/secrets/ds-pipeline-config", `{"metadata":{}}`)
	u := userClientOf(c, fakeUserToken)
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	raw, _ := json.Marshal(f.get(wbNotebook))
	for _, want := range []string{`"opendatahub.io/connections":"proj/rhoai-nightly-updater-s3"`, `"secretName":"ds-pipeline-config"`, `"opendatahub.io/mlflow-instance":"mlflow"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "envFrom") {
		t.Fatal("envFrom written")
	}
}

// Setup must reject without mutating when: only DSC v1 (RHOAI 2.x), dry-run
// lacks kube-rbac-proxy, no usable profile, or PVC not owned by this tool.
func TestWorkbenchSetupRefusals(t *testing.T) {
	cases := map[string]struct {
		change func(f *resourceFake)
		code   string
		says   string
	}{
		"rhoai 2.x": {func(f *resourceFake) {
			f.deleteObject(wbDSCv2 + "/default-dsc")
			f.fail["GET "+wbDSCv2] = 404
			f.putJSON("/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Managed"}}}}`)
		}, "unsupported", "RHOAI 2.x"},
		"no sidecar": {nil, "unsupported", "kube-rbac-proxy"},
		"component off": {func(f *resourceFake) {
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Removed"}}}}`)
		}, "prerequisites", "turned off"},
		"no profile": {func(f *resourceFake) { f.deleteObject(wbProfiles + "default-profile") }, "no_profile", "No enabled hardware profile"},
		"kueue project without its queue": {func(f *resourceFake) {
			f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}}}`)
			f.putJSON("/apis/opendatahub.io/v1alpha/namespaces/"+wbApps+"/odhdashboardconfigs/odh-dashboard-config", `{"spec":{"dashboardConfig":{"disableKueue":false}}}`)
			f.putJSON(wbProfiles+"queue", `{"spec":{`+cpuMem+`,"scheduling":{"type":"Queue","kueue":{"localQueueName":"lq"}}}}`)
		}, "no_profile_project", "LocalQueue"},
		"foreign storage": {func(f *resourceFake) { f.putJSON(wbPVC, `{"metadata":{}}`) }, "not_managed", "not created by this tool"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, name != "no sidecar")
			if tc.change != nil {
				tc.change(f)
			}
			res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "setup", Project: "proj"})
			if res.Success || res.ErrorCode != tc.code || !strings.Contains(res.Message, tc.says) {
				t.Fatalf("got %+v", res)
			}
			for _, m := range f.mutations() {
				if strings.HasPrefix(m, "POST") || strings.HasPrefix(m, "PATCH") || strings.HasPrefix(m, "DELETE") {
					t.Fatalf("wrote %s", m)
				}
			}
		})
	}
	// Status reflects the missing-profile error even when setup never ran.
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.deleteObject(wbProfiles + "default-profile")
	st, err := GetWorkbenchStatus(c, userClientOf(c, fakeUserToken), "proj")
	if err != nil || st.Plan != nil || st.PlanProblemCode != "no_profile" {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func pod(t *testing.T, js string) workbench.Pod {
	t.Helper()
	var p workbench.Pod
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// State() should agree with dashboard derivation from Notebook annotations and pod conditions.
func TestWorkbenchState(t *testing.T) {
	ready := pod(t, `{"metadata":{"name":"p"},"status":{"containerStatuses":[{"name":"nightly-workbench","ready":true,"state":{"running":{}}},{"name":"kube-rbac-proxy","ready":true,"state":{"running":{}}}]}}`)
	proxyNotReady := pod(t, `{"metadata":{"name":"p"},"status":{"containerStatuses":[{"name":"nightly-workbench","ready":true,"state":{"running":{}}},{"name":"kube-rbac-proxy","ready":false,"state":{"running":{}}}]}}`)
	pull := pod(t, `{"metadata":{"name":"p"},"status":{"containerStatuses":[{"name":"nightly-workbench","state":{"waiting":{"reason":"ImagePullBackOff","message":"no such image"}}}]}}`)
	nb := func(stoppedValue string) workbench.Notebook {
		var n workbench.Notebook
		if stoppedValue != "" {
			n.Metadata.Annotations = map[string]string{workbench.StoppedAnnotation: stoppedValue}
		}
		return n
	}
	for _, tt := range []struct {
		name  string
		nb    workbench.Notebook
		pods  []workbench.Pod
		state string
	}{
		{"running", nb(""), []workbench.Pod{ready}, workbench.StateRunning},
		{"starting: the proxy is not ready", nb(""), []workbench.Pod{proxyNotReady}, workbench.StateStarting},
		{"starting: no pod yet", nb(""), nil, workbench.StateStarting},
		{"the controller's lock is not stopped", nb("odh-notebook-controller-lock"), nil, workbench.StateStarting},
		{"stopping", nb("2026-11-24T10:00:00Z"), []workbench.Pod{ready}, workbench.StateStopping},
		{"stopped", nb("2026-11-24T10:00:00Z"), nil, workbench.StateStopped},
		{"failed", nb(""), []workbench.Pod{pull}, workbench.StateFailed},
	} {
		if got, _, _ := workbench.State(tt.nb, tt.pods); got != tt.state {
			t.Errorf("%s: got %s want %s", tt.name, got, tt.state)
		}
	}
}

func putToolWorkbench(t *testing.T, f *resourceFake, c *Client) {
	t.Helper()
	ann := `{}`
	f.putJSON(wbNotebook, `{"metadata":{"uid":"nb-1","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":`+ann+`},"spec":{"template":{"spec":{"containers":[{"name":"nightly-workbench","volumeMounts":[]}],"volumes":[]}}}}`)
	f.putJSON(wbPVC, `{"metadata":{"uid":"pvc-1","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	if err := writeWorkbenchRecord(c, "nb-1", "pvc-1"); err != nil {
		t.Fatal(err)
	}
}

func writeWorkbenchRecord(c *Client, uid, storageUID string) error {
	raw, _ := json.Marshal(workbench.Record{"proj/" + workbench.Name: {UID: uid, StorageUID: storageUID}})
	return record.WriteSnapshotKey(c, workbench.RecordKey, "rhoai-nightly-updater-workbenches", string(raw))
}

func patchOps(t *testing.T, call fakeCall) []kube.JSONPatchOp {
	t.Helper()
	var ops []kube.JSONPatchOp
	if err := json.Unmarshal([]byte(call.Body), &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

// stop/start apply dashboard-equivalent JSON Patch sequences guarded by a UID test operation.
func TestWorkbenchStartStop(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	putToolWorkbench(t, f, c)
	f.putJSON("/api/v1/namespaces/proj/secrets/ds-pipeline-config", `{"metadata":{}}`)
	u := userClientOf(c, fakeUserToken)
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "stop", Project: "proj"}); !res.Success {
		t.Fatalf("stop: %+v", res)
	}
	calls := f.callsTo("PATCH", wbNotebook)
	ops := patchOps(t, calls[len(calls)-1])
	if len(ops) != 2 || ops[0].Op != "test" || string(ops[0].Value) != `"nb-1"` || ops[1].Op != "add" || ops[1].Path != "/metadata/annotations/kubeflow-resource-stopped" {
		t.Fatalf("stop ops: %+v", ops)
	}
	var at string
	_ = json.Unmarshal(ops[1].Value, &at)
	if _, err := time.Parse("2006-01-02T15:04:05Z", at); err != nil || strings.Contains(at, ".") {
		t.Fatalf("stop time %q", at)
	}
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "start", Project: "proj"}); !res.Success {
		t.Fatalf("start: %+v", res)
	}
	calls = f.callsTo("PATCH", wbNotebook)
	ops = patchOps(t, calls[len(calls)-1])
	if len(ops) != 4 || ops[1].Op != "remove" || ops[1].Path != "/metadata/annotations/kubeflow-resource-stopped" || ops[2].Path != "/spec/template/spec/volumes/-" || ops[3].Path != "/spec/template/spec/containers/0/volumeMounts/-" {
		t.Fatalf("start ops: %+v", ops)
	}
	// Matching name but mismatched UID is foreign; stop must not patch it.
	f.putJSON(wbNotebook, `{"metadata":{"uid":"other","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	n := len(f.callsTo("PATCH", wbNotebook))
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "stop", Project: "proj"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("foreign stop: %+v", res)
	}
	if len(f.callsTo("PATCH", wbNotebook)) != n {
		t.Fatal("patched someone else's workbench")
	}
}

// teardown always removes the tracked Notebook; PVC removal is gated on
// DeleteStorage and tool management labels.
func TestWorkbenchTeardown(t *testing.T) {
	t.Run("keeps the storage by default", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "teardown", Project: "proj"})
		if !res.Success || f.has(wbNotebook) || !f.has(wbPVC) || !strings.Contains(res.Message, "kept") {
			t.Fatalf("got %+v", res)
		}
		var body struct {
			Preconditions struct{ UID string } `json:"preconditions"`
		}
		_ = json.Unmarshal([]byte(f.callsTo("DELETE", wbNotebook)[0].Body), &body)
		if body.Preconditions.UID != "nb-1" {
			t.Fatalf("delete without the UID precondition: %+v", body)
		}
		rec, _ := workbench.ReadRecord(c)
		if e, _ := rec.Get("proj"); e.UID != "" || e.StorageUID != "pvc-1" {
			t.Fatalf("record %+v", e)
		}
	})
	t.Run("deletes the storage when asked", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "teardown", Project: "proj", DeleteStorage: true})
		if !res.Success || f.has(wbNotebook) || f.has(wbPVC) {
			t.Fatalf("got %+v", res)
		}
		if rec, _ := workbench.ReadRecord(c); len(rec) != 0 {
			t.Fatalf("record %+v", rec)
		}
	})
	t.Run("someone else's storage is not deleted", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		f.putJSON(wbPVC, `{"metadata":{"uid":"pvc-other","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "teardown", Project: "proj", DeleteStorage: true})
		if res.Success || res.ErrorCode != "not_managed" || !f.has(wbNotebook) || !f.has(wbPVC) {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("someone else's workbench is not deleted", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		f.putJSON(wbNotebook, `{"metadata":{"uid":"nb-x","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "teardown", Project: "proj"})
		if res.Success || res.ErrorCode != "not_managed" || !f.has(wbNotebook) || len(f.callsTo("DELETE", wbNotebook)) != 0 {
			t.Fatalf("got %+v", res)
		}
	})
}

// Cleanup part ordering places workbench actions ahead of pipeline servers,
// retains PVCs unless WorkbenchStorage names them, and skips workbench confirm maps.
func TestCleanupWorkbenchPart(t *testing.T) {
	_, c := newResourceFake(t)
	parts := cleanupParts(c, types.CleanupRequest{PipelineServers: []string{"proj"}, Workbenches: []string{"proj", "b"}, WorkbenchStorage: []string{"b"}})
	if len(parts) != 3 || parts[0].id != "workbench:proj" || parts[1].id != "workbench:b" || parts[2].id != "pipeline-server:proj" {
		t.Fatalf("order: %+v", parts)
	}
	if !strings.Contains(parts[1].title, "storage") || strings.Contains(parts[0].title, "storage") {
		t.Fatalf("titles: %q %q", parts[0].title, parts[1].title)
	}
	if err := writeWorkbenchRecord(c, "nb-1", ""); err != nil {
		t.Fatal(err)
	}
	got := confirmedWorkbenches(c, types.CleanupRequest{Workbenches: []string{"proj"}, ConfirmWorkbenches: map[string]map[string]string{"proj": {"theirs": "u9"}}}, "proj")
	if got[workbench.Name] != "nb-1" || got["theirs"] != "u9" {
		t.Fatalf("confirmed %v", got)
	}
	if got := confirmedWorkbenches(c, types.CleanupRequest{}, "proj"); got[workbench.Name] != "" {
		t.Fatalf("confirmed without the workbench part: %v", got)
	}
}

// Storage kept from a deleted workbench is deleted on its own (delete-storage
// and Clean up), only when it is the tool's and no workbench uses it.
func TestWorkbenchDeleteKeptStorage(t *testing.T) {
	t.Run("kept storage of this tool", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		f.deleteObject(wbNotebook)
		// As a teardown that kept the storage leaves the record.
		if err := writeWorkbenchRecord(c, "", "pvc-1"); err != nil {
			t.Fatal(err)
		}
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "delete-storage", Project: "proj"})
		if !res.Success || f.has(wbPVC) {
			t.Fatalf("got %+v", res)
		}
		if rec, _ := workbench.ReadRecord(c); len(rec) != 0 {
			t.Fatalf("record %+v", rec)
		}
	})
	t.Run("a workbench still uses it", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "delete-storage", Project: "proj"})
		if res.Success || !f.has(wbPVC) || len(f.callsTo("DELETE", wbPVC)) != 0 {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("someone else's", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		f.putJSON(wbPVC, `{"metadata":{"uid":"pvc-x","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "delete-storage", Project: "proj"})
		if res.Success || res.ErrorCode != "not_managed" || !f.has(wbPVC) {
			t.Fatalf("got %+v", res)
		}
	})
	_, c := newResourceFake(t)
	parts := cleanupParts(c, types.CleanupRequest{Workbenches: []string{"a"}, WorkbenchStorage: []string{"a", "b"}, PipelineServers: []string{"b"}})
	if len(parts) != 3 || parts[0].id != "workbench:a" || parts[1].id != "workbench-storage:b" || parts[2].id != "pipeline-server:b" {
		t.Fatalf("parts %+v", parts)
	}
}

// A recorded project whose workbench cannot be read is reported unreadable,
// never as absent; the shown profile is its display name, with requests.
func TestWorkbenchStatusUnreadableAndProfileName(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.putJSON(wbProfiles+"default-profile", `{"metadata":{"resourceVersion":"9","annotations":{"opendatahub.io/display-name":"Default"}},"spec":{`+cpuMem+`}}`)
	f.putJSON(wbNotebook, `{"metadata":{"uid":"nb-1","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"opendatahub.io/hardware-profile-name":"default-profile","opendatahub.io/hardware-profile-namespace":"`+wbApps+`"}},
		"spec":{"template":{"spec":{"containers":[{"name":"nightly-workbench","resources":{"requests":{"cpu":2,"memory":"4Gi"}}}]}}}}`)
	if err := writeWorkbenchRecord(c, "nb-1", ""); err != nil {
		t.Fatal(err)
	}
	u := userClientOf(c, fakeUserToken)
	st, err := GetWorkbenchStatus(c, u, "")
	if err != nil || len(st.Items) != 1 || st.Items[0].HardwareProfile != "Default" || st.Items[0].CPU != "2" || st.Items[0].Memory != "4Gi" {
		t.Fatalf("status %+v %v", st, err)
	}
	f.failAs[fakeUserToken+" GET "+wbNotebook] = 503
	st, err = GetWorkbenchStatus(c, u, "proj")
	if err != nil || len(st.Items) != 0 || len(st.ItemsUnreadable) != 1 || st.ItemsUnreadable[0] != "proj" || st.Plan != nil {
		t.Fatalf("unreadable: %+v %v", st, err)
	}
}
