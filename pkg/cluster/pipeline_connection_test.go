package cluster

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// assertConnection checks the dashboard S3 Connection in ns: the shape
// odh-dashboard's assembleConnectionSecret gives an S3 connection
// (packages/k8s-core/src/connectionTypeUtils.ts, rhoai-3.6; type s3 from
// manifests/base/connection-types/s3.yaml), with the given keys.
func assertConnection(t *testing.T, f *resourceFake, ns, accessKey, secretKey string) {
	t.Helper()
	obj := f.get(secretPath(ns, connectionSecretName))
	if obj == nil {
		t.Fatalf("connection %s/%s missing", ns, connectionSecretName)
	}
	meta := ensureMap(obj, "metadata")
	labels, _ := meta["labels"].(map[string]any)
	if labels["opendatahub.io/dashboard"] != "true" || labels["opendatahub.io/managed"] != "true" || labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
		t.Errorf("labels = %v", labels)
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann["opendatahub.io/connection-type-ref"] != "s3" || ann["opendatahub.io/connection-type"] != "s3" || ann["openshift.io/display-name"] == nil || ann["openshift.io/display-name"] == "" {
		t.Errorf("annotations = %v", ann)
	}
	if typ, ok := obj["type"]; ok && typ != "Opaque" {
		t.Errorf("type = %v", typ)
	}
	data, _ := obj["data"].(map[string]any)
	want := map[string]string{
		"AWS_ACCESS_KEY_ID": accessKey, "AWS_SECRET_ACCESS_KEY": secretKey,
		"AWS_S3_ENDPOINT": "http://seaweedfs-s3.seaweedfs.svc:8333", "AWS_DEFAULT_REGION": "us-east-1", "AWS_S3_BUCKET": "pipelines",
	}
	if len(data) != len(want) {
		t.Errorf("data keys = %v", data)
	}
	for k, v := range want {
		raw, _ := base64.StdEncoding.DecodeString(stringOf(data[k]))
		if string(raw) != v {
			t.Errorf("%s = %q, want %q", k, raw, v)
		}
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func TestSetupPipelineServer_CreatesTheDashboardConnection(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	keys, _, _ := readS3Keys(c)
	for run := range 2 {
		if resp, _ := SetupPipelineServer(c, "proj"); !resp.Success {
			t.Fatalf("run %d: %+v", run, resp)
		}
		assertConnection(t, f, "proj", keys.AccessKey, keys.SecretKey)
		assertOwnedByDSPA(t, f, secretPath("proj", connectionSecretName), "proj", dspaName)
	}
	st, err := GetResourcesStatus(c)
	if err != nil || len(st.PipelineServers) != 1 || st.PipelineServers[0].Connection != connectionSecretName {
		t.Fatalf("status = %+v err=%v", st, err)
	}
}

func TestSetupPipelineServer_ForeignConnectionNameRefused(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	f.putJSON(secretPath("proj", connectionSecretName), `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"data":{"k":"dXNlcg=="}}`)
	resp, _ := SetupPipelineServer(c, "proj")
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, connectionSecretName) {
		t.Fatalf("got %+v", resp)
	}
	for _, m := range f.mutations() {
		if strings.Contains(m, "/secrets") || strings.Contains(m, "datasciencepipelinesapplications") {
			t.Errorf("refusal changed %s", m)
		}
	}
}

func TestTeardownPipelineServer_DeletesTheConnection(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
	f.putJSON(secretPath("proj", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(secretPath("proj", connectionSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	resp, _ := TeardownPipelineServer(c, "proj")
	if !resp.Success || f.has(secretPath("proj", connectionSecretName)) {
		t.Fatalf("got %+v", resp)
	}
	// One of the same name that someone else made stays.
	f.putJSON(secretPath("proj", connectionSecretName), `{"metadata":{}}`)
	if resp, _ := TeardownPipelineServer(c, "proj"); !resp.Success || !f.has(secretPath("proj", connectionSecretName)) {
		t.Fatalf("got %+v", resp)
	}
}

// The selected project's status reads the Connection itself: the DSPA's
// annotation only says the tool created one, not that it is still there.
func TestGetResourcesStatusFor_ReadsTheConnection(t *testing.T) {
	annotated := `,"annotations":{"` + dspaConnectionAnnotation + `":"` + connectionSecretName + `"}`
	cases := []struct {
		name, want string
		setup      func(f *resourceFake)
	}{
		{"present", ConnectionPresent, func(f *resourceFake) {
			f.putJSON(secretPath("proj", connectionSecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		}},
		{"deleted in the dashboard", ConnectionMissing, func(f *resourceFake) {}},
		{"someone else's", ConnectionForeign, func(f *resourceFake) {
			f.putJSON(secretPath("proj", connectionSecretName), `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
		}},
		{"unreadable", ConnectionUnreadable, func(f *resourceFake) {
			f.fail["GET "+secretPath("proj", connectionSecretName)] = 403
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putDSProject(f, "proj")
			putDSProject(f, "other")
			putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, annotated, "")
			putDSPA(f, "other", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, annotated, "")
			tc.setup(f)
			st, err := GetResourcesStatusFor(c, "proj")
			if err != nil {
				t.Fatal(err)
			}
			for _, ps := range st.PipelineServers {
				want := ""
				if ps.Namespace == "proj" {
					want = tc.want
				}
				if ps.ConnectionState != want {
					t.Errorf("%s connection state = %q, want %q (%+v)", ps.Namespace, ps.ConnectionState, want, ps)
				}
			}
			if hasRequest(f, "GET "+secretPath("other", connectionSecretName)) {
				t.Error("the Connection of a project that is not selected was read")
			}
		})
	}
}

// Without a selected project, or for a project without the tool's server,
// the status reads no Secret.
func TestGetResourcesStatusFor_NoSecretReadOutsideTheSelectedServer(t *testing.T) {
	f, c := newResourceFake(t)
	putDSProject(f, "proj")
	putDSPA(f, "proj", dspaName, toolLabelJSON, envkit.ToolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
	for _, project := range []string{"", "elsewhere"} {
		if _, err := GetResourcesStatusFor(c, project); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range []string{"proj", "elsewhere"} {
		if hasRequest(f, "GET "+secretPath(ns, connectionSecretName)) {
			t.Errorf("secret read in %s", ns)
		}
	}
}

func hasRequest(f *resourceFake, req string) bool { return requestIndex(f, req) >= 0 }
