package cluster

import (
	"encoding/base64"
	"strings"
	"testing"
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
	labels, _ := meta["labels"].(map[string]interface{})
	if labels["opendatahub.io/dashboard"] != "true" || labels["opendatahub.io/managed"] != "true" || labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("labels = %v", labels)
	}
	ann, _ := meta["annotations"].(map[string]interface{})
	if ann["opendatahub.io/connection-type-ref"] != "s3" || ann["opendatahub.io/connection-type"] != "s3" || ann["openshift.io/display-name"] == nil || ann["openshift.io/display-name"] == "" {
		t.Errorf("annotations = %v", ann)
	}
	if typ, ok := obj["type"]; ok && typ != "Opaque" {
		t.Errorf("type = %v", typ)
	}
	data, _ := obj["data"].(map[string]interface{})
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

func stringOf(v interface{}) string {
	s, _ := v.(string)
	return s
}

func TestSetupPipelineServer_CreatesTheDashboardConnection(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	keys, _, _ := readS3Keys(c)
	for run := 0; run < 2; run++ {
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
	putDSPA(f, "proj", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
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
