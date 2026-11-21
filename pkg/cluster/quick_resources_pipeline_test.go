package cluster

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func dspaPath(ns, name string) string {
	return "/apis/" + dspaAPIGroup + "/namespaces/" + ns + "/datasciencepipelinesapplications/" + name
}

func secretPath(ns, name string) string {
	return "/api/v1/namespaces/" + ns + "/secrets/" + name
}

const toolLabelJSON = `{"` + kube.ManagedByLabelKey + `":"` + kube.ManagedByLabelValue + `"}`

// putDSPA stores a DSPA. labels is a JSON object, manager the creator.
func putDSPA(f *resourceFake, ns, name, labels, manager, op, host, secret, extraMeta, status string) {
	if status == "" {
		status = `{}`
	}
	f.putJSON(dspaPath(ns, name), `{"metadata":{"labels":`+labels+`,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(manager, op)+extraMeta+`},
		"spec":{"objectStorage":{"externalStorage":{"host":"`+host+`","s3CredentialsSecret":{"secretName":"`+secret+`"}}}},"status":`+status+`}`)
}

func putDSProject(f *resourceFake, ns string) {
	f.putJSON("/api/v1/namespaces/"+ns, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
}

func putDSPO(f *resourceFake, ready int) {
	f.putJSON("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/data-science-pipelines-operator-controller-manager",
		`{"metadata":{"labels":{"app.kubernetes.io/name":"data-science-pipelines-operator"}},"status":{"readyReplicas":`+itoa(ready)+`}}`)
}

func fastPipelineTimings(t *testing.T) {
	t.Helper()
	oldT, oldP := PipelineServerDeleteTimeout, PipelineServerDeletePoll
	PipelineServerDeleteTimeout, PipelineServerDeletePoll = 100*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { PipelineServerDeleteTimeout, PipelineServerDeletePoll = oldT, oldP })
}

func TestGetResourcesStatus_ListsOnlyToolPipelineServers(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	// The DSPAs of earlier versions point at the storage they left in
	// namespace minio.
	putLegacySeaweedFS(f)
	for _, ns := range []string{"mine", "legacy-apply", "legacy-post", "dashboard", "dashboard-same-spec"} {
		putDSProject(f, ns)
	}
	notReady := `{"conditions":[{"type":"Ready","status":"False","reason":"MinimumReplicasAvailable","message":"Could not connect to (seaweedfs-s3.seaweedfs.svc:8333): connection refused"}]}`
	putDSPA(f, "mine", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", notReady)
	putDSPA(f, "legacy-apply", "dspa", `{}`, envkit.ToolFieldManager, "Apply", legacyS3Host(), legacyDSPASecretName, "", "")
	putDSPA(f, "legacy-post", "dspa", `{}`, envkit.LegacyPostManager, "Update", legacyS3Host(), legacyDSPASecretName, "", "")
	// Created from the RHOAI dashboard: same name, secret and even host.
	putDSPA(f, "dashboard-same-spec", "dspa", `{}`, "unknown", "Update", legacyS3Host(), legacyDSPASecretName, "", "")
	putDSPA(f, "dashboard", "dspa", `{}`, "Mozilla", "Update", "s3.amazonaws.com", legacyDSPASecretName, "", "")
	// A tool DSPA in a project the user cannot see.
	putDSPA(f, "hidden", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")

	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ps := range st.PipelineServers {
		got = append(got, ps.Namespace+"/"+ps.Name)
		if !ps.ManagedByTool {
			t.Errorf("%s listed without ManagedByTool", ps.Namespace)
		}
	}
	if strings.Join(got, ",") != "legacy-apply/dspa,legacy-post/dspa,mine/"+dspaName {
		t.Errorf("pipeline servers = %v", got)
	}
	if strings.Join(st.UnmanagedPipelineProjects, ",") != "dashboard,dashboard-same-spec" {
		t.Errorf("unmanaged = %v", st.UnmanagedPipelineProjects)
	}
	// Their health is reported too: who created a server does not decide
	// whether it works. Nothing about them invites a change.
	var foreign []string
	for _, ps := range st.UnmanagedPipelineServers {
		foreign = append(foreign, ps.Namespace+"/"+ps.Name)
		if ps.ManagedByTool || ps.TeardownBlockedReason == "" || len(ps.DataPVCs) != 0 || !ps.Deployed {
			t.Errorf("unmanaged pipeline server %s: %+v", ps.Namespace, ps)
		}
	}
	if strings.Join(foreign, ",") != "dashboard/dspa,dashboard-same-spec/dspa" {
		t.Errorf("unmanaged pipeline servers = %v", foreign)
	}
	for _, ps := range st.PipelineServers {
		if ps.Namespace == "mine" {
			if !strings.Contains(ps.Message, "connection refused") || strings.Contains(ps.Message, "MinimumReplicasAvailable") {
				t.Errorf("message should be the condition message, got %q", ps.Message)
			}
			if !ps.TerminalError {
				t.Errorf("a pipeline server not ready long after creation should be terminal: %+v", ps)
			}
			if len(ps.DataPVCs) != 1 || ps.DataPVCs[0] != "mariadb-"+dspaName {
				t.Errorf("DataPVCs = %v", ps.DataPVCs)
			}
		}
	}
	// Every live DSPA on the tool's storages blocks their teardown,
	// including the dashboard-created one and the one in a hidden project.
	if !strings.Contains(st.MinIO.TeardownBlockedReason, "5 pipeline servers use this S3 storage: dashboard-same-spec/dspa, hidden/"+dspaName+", legacy-apply/dspa, legacy-post/dspa, mine/"+dspaName+".") || !strings.Contains(st.MinIO.TeardownBlockedReason, "dashboard-same-spec/dspa") {
		t.Errorf("S3 storage teardown reason = %q", st.MinIO.TeardownBlockedReason)
	}
	// The same users, structured, for the clean-up's selection.
	if got := strings.Join(st.MinIO.UsedBy, " "); got != "dashboard-same-spec/dspa hidden/"+dspaName+" legacy-apply/dspa legacy-post/dspa mine/"+dspaName {
		t.Errorf("S3 storage users = %q", got)
	}
}

// The 5-minute "has issues" signal follows odh-dashboard: a pipeline server
// counts as timed out only while its API server is not ready
// (usePipelineNamespaceCR.ts hasServerTimedOut uses dspaLoaded, which reads
// APIServerReady, not Ready). Ready=False alone, for example while another
// component restarts, is not stuck.
func TestPipelineServerState_StuckFollowsAPIServerReady(t *testing.T) {
	old := time.Now().Add(-dspaStuckAfter - time.Minute).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	cases := []struct {
		name, created, conditions string
		wantStuck, wantReady      bool
	}{
		{"API server ready, Ready False long after creation", old,
			`[{"type":"APIServerReady","status":"True"},{"type":"Ready","status":"False","message":"Deployment ds-pipeline-ui-dspa: 0/1"}]`, false, false},
		{"API server not ready long after creation", old,
			`[{"type":"APIServerReady","status":"False","message":"Deployment ds-pipeline-dspa: 0/1"},{"type":"Ready","status":"False"}]`, true, false},
		{"no API server condition long after creation", old, `[]`, true, false},
		{"API server not ready, just created", fresh, `[{"type":"APIServerReady","status":"False"}]`, false, false},
		{"all ready", old, `[{"type":"APIServerReady","status":"True"},{"type":"Ready","status":"True"}]`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putDSProject(f, "proj")
			f.putJSON(dspaPath("proj", dspaName), `{"metadata":{"labels":`+toolLabelJSON+`,"creationTimestamp":"`+tc.created+`"},
				"spec":{"objectStorage":{"externalStorage":{"host":"`+s3Host()+`","s3CredentialsSecret":{"secretName":"`+dspaSecretName+`"}}}},
				"status":{"conditions":`+tc.conditions+`}}`)
			st, err := GetResourcesStatus(c)
			if err != nil || len(st.PipelineServers) != 1 {
				t.Fatalf("st=%+v err=%v", st, err)
			}
			ps := st.PipelineServers[0]
			if ps.TerminalError != tc.wantStuck || ps.Ready != tc.wantReady {
				t.Errorf("TerminalError=%v Ready=%v, want %v %v (%+v)", ps.TerminalError, ps.Ready, tc.wantStuck, tc.wantReady, ps)
			}
		})
	}
}

func TestGetResourcesStatus_RequestCountDoesNotGrowWithProjects(t *testing.T) {
	count := func(projects int) int {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		for i := range projects {
			ns := "p" + itoa(i)
			putDSProject(f, ns)
			putDSPA(f, ns, dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", `{"conditions":[{"type":"Ready","status":"True"}]}`)
		}
		if _, err := GetResourcesStatus(c); err != nil {
			t.Fatal(err)
		}
		return len(f.requests)
	}
	if one, twenty := count(1), count(20); one != twenty {
		t.Errorf("requests grow with projects: 1 project=%d, 20 projects=%d", one, twenty)
	}
}

func TestGetResourcesStatus_FreshClusterWithoutDSPACRD(t *testing.T) {
	f, c := newResourceFake(t)
	f.fail["GET "+dspaListPath] = 404
	st, err := GetResourcesStatus(c)
	if err != nil || len(st.PipelineServers) != 0 || st.MinIO.Deployed || st.MLflow.Deployed {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestGetResourcesStatus_DSPAListErrorFails(t *testing.T) {
	f, c := newResourceFake(t)
	f.fail["GET "+dspaListPath] = 500
	if _, err := GetResourcesStatus(c); err == nil {
		t.Fatal("a failed DSPA list must not be reported as no pipeline servers")
	}
}

// Setup names the DSPA "dspa", as the dashboard does (DEFAULT_PIPELINE_
// DEFINITION_NAME): odh-notebook-controller builds a workbench's pipeline
// config from the DSPA named "dspa" and binds Role ds-pipeline-user-access-
// dspa (kubeflow rhoai-3.6 notebook_dspa_secret.go dspaInstanceName,
// notebook_rbac.go ReconcileRoleBindings). The S3 secret keeps a name of
// the tool's own, never the dashboard's dashboard-dspa-secret.
func TestSetupPipelineServer_CreatesTheStandardName(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "new-project")

	resp, _ := SetupPipelineServer(c, "new-project")
	if !resp.Success {
		t.Fatalf("setup failed: %+v", resp)
	}
	// The project is the user's: setup never creates or changes it.
	for _, m := range f.mutations() {
		if strings.Contains(m, "/api/v1/namespaces") && !strings.Contains(m, "/api/v1/namespaces/new-project/") {
			t.Errorf("setup changed a namespace: %s", m)
		}
	}
	if dspaName != "dspa" {
		t.Fatalf("dspaName = %q, want the workbench controller's dspa", dspaName)
	}
	for _, path := range []string{dspaPath("new-project", "dspa"), secretPath("new-project", dspaSecretName)} {
		obj := f.get(path)
		if obj == nil {
			t.Fatalf("%s not created", path)
		}
		l := obj["metadata"].(map[string]any)["labels"].(map[string]any)
		if l[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
			t.Errorf("%s lacks the ownership label", path)
		}
	}
	if f.has(dspaPath("new-project", previousDSPAName)) || f.has(secretPath("new-project", legacyDSPASecretName)) {
		t.Error("setup must create only dspa and the tool's own secret name")
	}
	raw, _ := json.Marshal(f.get(dspaPath("new-project", dspaName)))
	if strings.Contains(string(raw), "instructLab") {
		t.Errorf("DSPA sends a field the 3.6 CRD does not declare: %s", raw)
	}
}

// putDSPACRD stores the DSPA CRD with the apiServer and mlflow fields of a
// DSPO release: managedPipelines.enabled (rhoai-3.6 api/v1
// ManagedPipelinesSpec.Enabled), mlflow (rhoai-3.5 and later MLflowConfig).
func putDSPACRD(f *resourceFake, managedEnabled, mlflow bool) {
	managed := `{"type":"object","nullable":true,"properties":{"image":{"type":"string"}}}`
	if managedEnabled {
		managed = `{"type":"object","nullable":true,"properties":{"enabled":{"type":"boolean"},"image":{"type":"string"}}}`
	}
	spec := `"apiServer":{"type":"object","properties":{"cacheEnabled":{"type":"boolean"},"pipelineStore":{"type":"string"},"managedPipelines":` + managed + `}},"dspVersion":{"type":"string"}`
	if mlflow {
		spec += `,"mlflow":{"type":"object","properties":{"integrationMode":{"type":"string","enum":["AUTODETECT","DISABLED"]},"injectUserEnvVars":{"type":"boolean"}}}`
	}
	f.putJSON(rhoai.ClusterPath("apiextensions.k8s.io/v1", "customresourcedefinitions", dspaCRDName),
		`{"spec":{"versions":[{"name":"v1","served":true,"storage":true,"schema":{"openAPIV3Schema":{"type":"object","properties":{"spec":{"type":"object","properties":{`+spec+`}}}}}}]}}`)
}

// The DSPA carries the settings odh-dashboard's Configure pipeline server
// sends (configurePipelinesServer/utils.ts createDSPipelineResourceSpec with
// ConfigurePipelinesServerModal.tsx FORM_DEFAULTS), each only when the
// installed CRD declares it: an older operator rejects an unknown field
// (fieldValidation=Strict), and omitting one gives the operator's default,
// which is the same value.
func TestSetupPipelineServer_DashboardSettingsWhenTheCRDHasThem(t *testing.T) {
	cases := []struct {
		name                  string
		crd                   func(f *resourceFake)
		wantManaged, wantFlow bool
	}{
		{"rhoai-3.6 CRD", func(f *resourceFake) { putDSPACRD(f, true, true) }, true, true},
		{"older CRD without managedPipelines.enabled or mlflow", func(f *resourceFake) { putDSPACRD(f, false, false) }, false, false},
		{"CRD unreadable", func(f *resourceFake) {
			f.fail["GET /apis/apiextensions.k8s.io/v1/customresourcedefinitions"] = 403
		}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putManagedDSC(f)
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			tc.crd(f)
			if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
				t.Fatalf("setup failed: %+v", resp)
			}
			spec := ensureMap(f.get(dspaPath("proj", dspaName)), "spec")
			api := ensureMap(spec, "apiServer")
			if api["pipelineStore"] != "database" || api["cacheEnabled"] != true || api["enableSamplePipeline"] != false {
				t.Errorf("apiServer = %v", api)
			}
			managed, hasManaged := api["managedPipelines"].(map[string]any)
			if hasManaged != tc.wantManaged || (hasManaged && (len(managed) != 1 || managed["enabled"] != true)) {
				t.Errorf("managedPipelines = %v, want set: %v", api["managedPipelines"], tc.wantManaged)
			}
			mlflow, hasFlow := spec["mlflow"].(map[string]any)
			if hasFlow != tc.wantFlow || (hasFlow && (len(mlflow) != 2 || mlflow["integrationMode"] != "AUTODETECT" || mlflow["injectUserEnvVars"] != false)) {
				t.Errorf("mlflow = %v, want set: %v", spec["mlflow"], tc.wantFlow)
			}
		})
	}
}

// assertOwnedByDSPA checks that the object at path has exactly one owner
// reference, to the DSPA ns/name by its current UID, without
// blockOwnerDeletion or controller: the garbage collector deletes it with
// the DSPA, and setting it needs no rights on the DSPA
// (OwnerReferencesPermissionEnforcement checks blockOwnerDeletion only).
func assertOwnedByDSPA(t *testing.T, f *resourceFake, path, ns, name string) {
	t.Helper()
	obj := f.get(path)
	if obj == nil {
		t.Fatalf("%s missing", path)
	}
	uid := ensureMap(f.get(dspaPath(ns, name)), "metadata")["uid"]
	refs, _ := ensureMap(obj, "metadata")["ownerReferences"].([]any)
	if len(refs) != 1 {
		t.Fatalf("%s ownerReferences = %v", path, refs)
	}
	ref := refs[0].(map[string]any)
	if ref["apiVersion"] != dspaAPIGroup || ref["kind"] != "DataSciencePipelinesApplication" || ref["name"] != name || ref["uid"] != uid || uid == nil {
		t.Errorf("%s owner = %v, want DSPA %s/%s uid %v", path, ref, ns, name, uid)
	}
	if _, ok := ref["blockOwnerDeletion"]; ok {
		t.Errorf("%s sets blockOwnerDeletion", path)
	}
	if _, ok := ref["controller"]; ok {
		t.Errorf("%s sets controller", path)
	}
}

// The S3 secret is owned by the DSPA, and labelled as the dashboard labels
// its own (configurePipelinesServer/utils.ts createObjectStorageSecret with
// assembleSecret: opendatahub.io/dashboard=true). The dashboard's Delete
// pipeline server deletes only the secret names it generates
// (concepts/pipelines/utils.ts deleteServer), so the owner reference is
// what removes this one.
func TestSetupPipelineServer_SecretOwnedByTheDSPA(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	for run := range 2 {
		if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
			t.Fatalf("run %d: %+v", run, resp)
		}
		assertOwnedByDSPA(t, f, secretPath("proj", dspaSecretName), "proj", dspaName)
		labels, _ := ensureMap(f.get(secretPath("proj", dspaSecretName)), "metadata")["labels"].(map[string]any)
		if labels["opendatahub.io/dashboard"] != "true" || labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
			t.Errorf("run %d: secret labels = %v", run, labels)
		}
	}
}

// A failed owner write leaves a working server and says what is left to do.
func TestSetupPipelineServer_OwnerWriteFailure(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.fail["PATCH "+secretPath("proj", dspaSecretName)] = 500
	resp, _ := SetupPipelineServer(c, "proj")
	if resp.Success || resp.ErrorCode != "partial_failure" || !strings.Contains(resp.Message, "Pipeline server created") || !strings.Contains(resp.Message, dspaSecretName) {
		t.Fatalf("got %+v", resp)
	}
	if !f.has(dspaPath("proj", dspaName)) {
		t.Error("the pipeline server must stay")
	}
}

func TestSetupPipelineServer_Refusals(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(f *resourceFake)
		wantCode string
	}{
		{"S3 storage missing", func(f *resourceFake) {}, "prerequisites"},
		{"S3 storage not created by the tool", func(f *resourceFake) {
			putS3Namespace(f, `{}`)
			putS3Deployment(f, 1)
			putS3Serving(f)
		}, "prerequisites"},
		{"only storage from an earlier version", func(f *resourceFake) {
			putLegacySeaweedFS(f)
		}, "prerequisites"},
		{"project has a dashboard pipeline server", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putDSPA(f, "proj", "dspa", `{}`, "unknown", "Update", legacyS3Host(), legacyDSPASecretName, "", "")
		}, "not_managed"},
		{"project has a pipeline server from an earlier version", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putDSPA(f, "proj", "dspa", `{}`, envkit.LegacyPostManager, "Update", legacyS3Host(), legacyDSPASecretName, "", "")
		}, "validation"},
		// A server this tool set up under its earlier name: Set up does not
		// add dspa next to it; Recreate replaces it.
		{"project has the tool's nightly-dspa", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putDSPA(f, "proj", previousDSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
		}, "validation"},
		{"secret name taken by someone else", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{}}`)
		}, "not_managed"},
		{"no DSPA CRD", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			f.fail["GET /apis/"+dspaAPIGroup+"/namespaces/proj/datasciencepipelinesapplications"] = 404
		}, "prerequisites"},
		// Only a Data Science project (made in the dashboard) gets a
		// pipeline server: setup never creates the namespace.
		{"project does not exist", func(f *resourceFake) {
			putReadyManagedS3(f)
		}, "validation"},
		{"namespace is not a Data Science project", func(f *resourceFake) {
			putReadyManagedS3(f)
			f.putJSON("/api/v1/namespaces/proj", `{"metadata":{},"status":{"phase":"Active"}}`)
		}, "validation"},
		{"project list unreadable", func(f *resourceFake) {
			putReadyManagedS3(f)
			f.fail["GET /api/v1/namespaces"] = 403
		}, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putManagedDSC(f)
			tc.setup(f)
			resp, _ := SetupPipelineServer(c, "proj")
			if resp.Success || resp.ErrorCode != tc.wantCode {
				t.Fatalf("got %+v", resp)
			}
			for _, m := range f.mutations() {
				if strings.Contains(m, "datasciencepipelinesapplications") || strings.Contains(m, "/secrets/") || strings.Contains(m, "/api/v1/namespaces") {
					t.Errorf("refusal changed %s", m)
				}
			}
		})
	}
}

func TestSetupPipelineServer_RerunIsIdempotent(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	for i := range 2 {
		if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
			t.Fatalf("run %d failed: %+v", i, resp)
		}
	}
}

func TestTeardownPipelineServer(t *testing.T) {
	withFinalizer := `,"finalizers":["` + dspaFinalizer + `"]`
	cases := []struct {
		name          string
		setup         func(f *resourceFake)
		wantOK        bool
		wantCode      string
		wantDSPAGone  string // DSPA path that must be gone
		wantKept      []string
		wantDeleted   []string
		wantNoDeletes bool
		wantMsg       string
	}{
		{
			name: "tool pipeline server",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
				f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
			},
			wantOK: true, wantDeleted: []string{dspaPath("proj", dspaName), secretPath("proj", dspaSecretName)}, wantMsg: "mariadb-" + dspaName,
		},
		{
			name: "dashboard pipeline server is never deleted",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", "dspa", `{}`, "unknown", "Update", legacyS3Host(), legacyDSPASecretName, "", "")
				f.putJSON(secretPath("proj", legacyDSPASecretName), `{"metadata":{}}`)
			},
			wantCode: "not_managed", wantNoDeletes: true,
		},
		{
			name: "legacy pipeline server and secret",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", "dspa", `{}`, envkit.LegacyPostManager, "Update", legacyS3Host(), legacyDSPASecretName, "", "")
				f.putJSON(secretPath("proj", legacyDSPASecretName), `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(envkit.ToolFieldManager, "Apply")+`}}`)
			},
			wantOK: true, wantDeleted: []string{dspaPath("proj", "dspa"), secretPath("proj", legacyDSPASecretName)},
		},
		{
			name: "legacy pipeline server whose secret someone else created",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", "dspa", `{}`, envkit.ToolFieldManager, "Apply", legacyS3Host(), legacyDSPASecretName, "", "")
				f.putJSON(secretPath("proj", legacyDSPASecretName), `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON("unknown", "Update")+`}}`)
			},
			wantOK: true, wantDeleted: []string{dspaPath("proj", "dspa")}, wantKept: []string{secretPath("proj", legacyDSPASecretName)},
		},
		{
			name: "finalizer and operator not running",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, withFinalizer, "")
				putDSPO(f, 0)
			},
			wantCode: "prerequisites", wantNoDeletes: true,
		},
		{
			name: "finalizer and operator running",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, withFinalizer, "")
				f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
				putDSPO(f, 1)
				f.finalizeOnGet = true
			},
			wantOK: true, wantDeleted: []string{dspaPath("proj", dspaName), secretPath("proj", dspaSecretName)},
		},
		{
			name: "finalizer never completes",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, withFinalizer, "")
				f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
				putDSPO(f, 1)
			},
			wantCode: "in_progress", wantDeleted: []string{secretPath("proj", dspaSecretName)}, wantKept: []string{dspaPath("proj", dspaName)},
		},
		{
			name: "secret shared with another live pipeline server",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
				putDSPA(f, "proj", "other", `{}`, "unknown", "Update", s3Host(), dspaSecretName, "", "")
				f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
			},
			wantOK: true, wantDeleted: []string{dspaPath("proj", dspaName)}, wantKept: []string{secretPath("proj", dspaSecretName), dspaPath("proj", "other")},
		},
		{
			name: "secret delete fails",
			setup: func(f *resourceFake) {
				putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
				f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
				f.fail["DELETE "+secretPath("proj", dspaSecretName)] = 500
			},
			wantCode: "partial_failure", wantDeleted: []string{dspaPath("proj", dspaName)},
		},
		{
			name:   "nothing to remove",
			setup:  func(f *resourceFake) {},
			wantOK: true, wantNoDeletes: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastPipelineTimings(t)
			f, c := newResourceFake(t)
			tc.setup(f)
			resp, err := TeardownPipelineServer(c, "proj")
			if err != nil {
				t.Fatal(err)
			}
			if resp.Success != tc.wantOK || (tc.wantCode != "" && resp.ErrorCode != tc.wantCode) {
				t.Fatalf("got %+v", resp)
			}
			for _, p := range tc.wantDeleted {
				if f.has(p) && !strings.Contains(p, "/datasciencepipelinesapplications/") {
					t.Errorf("%s should be deleted", p)
				}
				if !hasMutation(f, "DELETE "+p) {
					t.Errorf("no DELETE for %s", p)
				}
			}
			for _, p := range tc.wantKept {
				if !f.has(p) {
					t.Errorf("%s must be kept", p)
				}
			}
			if tc.wantNoDeletes {
				for _, m := range f.mutations() {
					if strings.HasPrefix(m, "DELETE") {
						t.Errorf("unexpected %s", m)
					}
				}
			}
			if tc.wantMsg != "" && !strings.Contains(resp.Message, tc.wantMsg) {
				t.Errorf("message %q lacks %q", resp.Message, tc.wantMsg)
			}
		})
	}
}

func TestTeardownPipelineServer_RerunWhileTerminatingDoesNotDeleteAgain(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName,
		`,"deletionTimestamp":"`+created+`","finalizers":["`+dspaFinalizer+`"]`, "")
	resp, _ := TeardownPipelineServer(c, "proj")
	if resp.Success || resp.ErrorCode != "in_progress" {
		t.Fatalf("got %+v", resp)
	}
	if hasMutation(f, "DELETE "+dspaPath("proj", dspaName)) {
		t.Error("a terminating DSPA must not be deleted again")
	}
}

func TestPipelineTeardown_RerunRemovesOrphanSecret(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	resp, _ := TeardownPipelineServer(c, "proj")
	if !resp.Success || f.has(secretPath("proj", dspaSecretName)) {
		t.Fatalf("got %+v", resp)
	}
	// An unlabelled secret with the same name is left alone.
	f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{}}`)
	if resp, _ := TeardownPipelineServer(c, "proj"); !resp.Success || !f.has(secretPath("proj", dspaSecretName)) {
		t.Fatalf("got %+v", resp)
	}
}

func TestCountNounAndVerbAgree(t *testing.T) {
	if got := kube.CountNoun(1, "pipeline server", "pipeline servers") + " " + kube.Verb(1, "uses", "use"); got != "1 pipeline server uses" {
		t.Errorf("singular: %q", got)
	}
	if got := kube.CountNoun(2, "pipeline server", "pipeline servers") + " " + kube.Verb(2, "uses", "use"); got != "2 pipeline servers use" {
		t.Errorf("plural: %q", got)
	}
}
