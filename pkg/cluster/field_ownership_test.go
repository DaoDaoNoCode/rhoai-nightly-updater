package cluster

import (
	"encoding/json"
	"strings"
	"testing"
)

const seaweedfs448 = "ghcr.io/chrislusf/seaweedfs@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"

// putOlderToolObjects turns the objects of a fresh setup into objects an
// older version of the tool created and re-applied, shaped like what 2.3.x
// left on a cluster (seen live): SeaweedFS 4.48 with its admin UI on 23646
// in the Deployment, the Service and the NetworkPolicy, and every object
// created with POST and later re-applied, so the tool's manager has two
// managedFields entries, operation Update (the create) and operation Apply
// (the re-run), that both own the container ports. Another manager's entry
// is added too, which the upgrade must leave alone.
func putOlderToolObjects(t *testing.T, f *resourceFake) {
	t.Helper()
	for _, path := range []string{s3DeployPath, s3SvcPath, s3NPPath} {
		raw, _ := json.Marshal(f.get(path))
		old := strings.ReplaceAll(string(raw), "12535", "23646")
		old = strings.ReplaceAll(old, `,"-admin.port=23646","-admin.port.legacy=0"`, "")
		old = strings.ReplaceAll(old, seaweedfsDefaultImage, seaweedfs448)
		var obj map[string]interface{}
		_ = json.Unmarshal([]byte(old), &obj)
		meta := ensureMap(obj, "metadata")
		var entries []interface{}
		for _, e := range meta["managedFields"].([]interface{}) {
			em := e.(map[string]interface{})
			if em["manager"] == toolFieldManager {
				entries = append(entries, map[string]interface{}{"manager": toolFieldManager, "operation": "Update", "apiVersion": em["apiVersion"], "time": meta["creationTimestamp"], "fieldsType": "FieldsV1", "fieldsV1": em["fieldsV1"]})
				entries = append(entries, map[string]interface{}{"manager": toolFieldManager, "operation": "Apply", "apiVersion": em["apiVersion"], "time": "2026-10-20T10:00:00Z", "fieldsType": "FieldsV1", "fieldsV1": em["fieldsV1"]})
			}
		}
		entries = append(entries, map[string]interface{}{"manager": "kube-controller-manager", "operation": "Update", "apiVersion": "apps/v1", "time": "2026-10-20T10:00:01Z", "fieldsType": "FieldsV1", "subresource": "status", "fieldsV1": map[string]interface{}{"f:status": map[string]interface{}{"f:readyReplicas": map[string]interface{}{}}}})
		meta["managedFields"] = entries
		raw, _ = json.Marshal(obj)
		f.putJSON(path, string(raw))
	}
	if ports := containerPorts(f.get(s3DeployPath))["seaweedfs"]; len(ports) != 2 || portKey(ports[1]) != `k:{"containerPort":23646,"protocol":"TCP"}` {
		t.Fatalf("fixture: ports = %v", ports)
	}
}

func deploymentAdminPorts(f *resourceFake) []string {
	var out []string
	for _, p := range containerPorts(f.get(s3DeployPath))["seaweedfs"] {
		out = append(out, portKey(p))
	}
	return out
}

// Bug seen live: Repair of a 2.3.x SeaweedFS failed with 422
// "spec.template.spec.containers[0].ports[2].name: Duplicate value: admin",
// because the tool's Update entry (its create) still owned the old admin
// port, so the apply kept it next to the new one. Setup upgrades the tool's
// field ownership to server-side apply first (csaupgrade semantics), so a
// field the tool no longer applies is removed.
func TestSetupS3Storage_RemovesAFieldTheToolNoLongerApplies(t *testing.T) {
	t.Setenv("SEAWEEDFS_IMAGE", "")
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	putOlderToolObjects(t, f)

	resp, _ := SetupS3Storage(c)
	if !resp.Success {
		t.Fatalf("repair = %s (%s)", resp.Message, resp.ErrorCode)
	}
	if got := strings.Join(deploymentAdminPorts(f), " "); got != `k:{"containerPort":8333,"protocol":"TCP"} k:{"containerPort":12535,"protocol":"TCP"}` {
		t.Errorf("container ports after the repair = %s", got)
	}
	var entries []managedFieldsEntry
	raw, _ := json.Marshal(ensureMap(f.get(s3DeployPath), "metadata")["managedFields"])
	_ = json.Unmarshal(raw, &entries)
	var ours, others []string
	for _, e := range entries {
		if e.Manager == toolFieldManager {
			ours = append(ours, e.Operation+"/"+e.Subresource)
		} else {
			others = append(others, e.Manager+"/"+e.Operation+"/"+e.Subresource)
		}
	}
	if strings.Join(ours, ",") != "Apply/" {
		t.Errorf("the tool's entries = %v, want one Apply entry", ours)
	}
	if strings.Join(others, ",") != "kube-controller-manager/Update/status" {
		t.Errorf("other managers' entries = %v, want them untouched", others)
	}
}

func TestUpgradeToolFieldOwnership(t *testing.T) {
	fields := func(keys ...string) map[string]json.RawMessage {
		spec := map[string]interface{}{}
		for _, k := range keys {
			spec[k] = map[string]interface{}{}
		}
		raw, _ := json.Marshal(spec)
		return map[string]json.RawMessage{"f:spec": raw}
	}
	path := "/apis/apps/v1/namespaces/seaweedfs/deployments/seaweedfs"

	t.Run("folds Update into Apply and keeps other entries", func(t *testing.T) {
		f, c := newResourceFake(t)
		cur := objectMeta{ResourceVersion: "7", ManagedFields: []managedFieldsEntry{
			{Manager: toolFieldManager, Operation: "Apply", APIVersion: "apps/v1", Time: "2026-10-20T10:00:00Z", FieldsType: "FieldsV1", FieldsV1: fields("f:replicas")},
			{Manager: "kubectl-edit", Operation: "Update", APIVersion: "apps/v1", Time: "2026-10-19T10:00:00Z", FieldsType: "FieldsV1", FieldsV1: fields("f:paused")},
			{Manager: toolFieldManager, Operation: "Update", APIVersion: "apps/v1", Time: "2026-10-01T10:00:00Z", FieldsType: "FieldsV1", FieldsV1: fields("f:strategy", "f:replicas")},
			{Manager: toolFieldManager, Operation: "Update", APIVersion: "apps/v1", Time: "2026-10-01T10:00:00Z", Subresource: "scale", FieldsType: "FieldsV1", FieldsV1: fields("f:replicas")},
		}}
		raw, _ := json.Marshal(map[string]interface{}{"metadata": cur})
		f.putJSON(path, string(raw))
		got, err := upgradeToolFieldOwnership(c, path, cur)
		if err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != "8" || len(got.ManagedFields) != 3 {
			t.Fatalf("after = %+v", got)
		}
		apply := got.ManagedFields[0]
		if apply.Operation != "Apply" || apply.Time != "2026-10-20T10:00:00Z" || string(apply.FieldsV1["f:spec"]) != `{"f:replicas":{},"f:strategy":{}}` {
			t.Errorf("Apply entry = %+v (%s)", apply, apply.FieldsV1["f:spec"])
		}
		if got.ManagedFields[1].Manager != "kubectl-edit" || got.ManagedFields[2].Subresource != "scale" {
			t.Errorf("other entries changed: %+v", got.ManagedFields[1:])
		}
	})

	t.Run("converts a lone Update entry", func(t *testing.T) {
		f, c := newResourceFake(t)
		cur := objectMeta{ResourceVersion: "3", CreationTimestamp: "2026-10-01T10:00:00Z", ManagedFields: []managedFieldsEntry{
			{Manager: toolFieldManager, Operation: "Update", APIVersion: "v1", Time: "2026-10-01T10:00:00Z", FieldsType: "FieldsV1", FieldsV1: fields("f:ports")},
		}}
		raw, _ := json.Marshal(map[string]interface{}{"metadata": cur})
		f.putJSON(path, string(raw))
		got, err := upgradeToolFieldOwnership(c, path, cur)
		if err != nil || len(got.ManagedFields) != 1 || got.ManagedFields[0].Operation != "Apply" || !got.createdByToolApply() {
			t.Fatalf("after = %+v, %v", got, err)
		}
	})

	t.Run("nothing to do: no request", func(t *testing.T) {
		f, c := newResourceFake(t)
		cur := objectMeta{ResourceVersion: "3", ManagedFields: []managedFieldsEntry{
			{Manager: toolFieldManager, Operation: "Apply", APIVersion: "v1", FieldsV1: fields("f:ports")},
			{Manager: legacyPostManager, Operation: "Update", APIVersion: "v1", FieldsV1: fields("f:selector")},
		}}
		got, err := upgradeToolFieldOwnership(c, path, cur)
		if err != nil || got.ResourceVersion != "3" || len(f.requests) != 0 {
			t.Fatalf("after = %+v, %v, requests %v", got, err, f.requests)
		}
	})

	t.Run("an object changed since it was read is not touched", func(t *testing.T) {
		f, c := newResourceFake(t)
		cur := objectMeta{ResourceVersion: "3", ManagedFields: []managedFieldsEntry{
			{Manager: toolFieldManager, Operation: "Update", APIVersion: "v1", FieldsV1: fields("f:ports")},
		}}
		stored := cur
		stored.ResourceVersion = "4"
		raw, _ := json.Marshal(map[string]interface{}{"metadata": stored})
		f.putJSON(path, string(raw))
		if _, err := upgradeToolFieldOwnership(c, path, cur); !IsK8sError(err, 409) {
			t.Fatalf("err = %v, want 409", err)
		}
		if ensureMap(f.get(path), "metadata")["resourceVersion"] != "4" {
			t.Error("the object was changed")
		}
	})
}

// specsOf returns the objects at paths without their managedFields and
// resourceVersion, which a field-ownership upgrade may change.
func specsOf(f *resourceFake, paths ...string) string {
	var out []string
	for _, p := range paths {
		obj := deepCopyObject(f.get(p))
		meta := ensureMap(obj, "metadata")
		delete(meta, "managedFields")
		delete(meta, "resourceVersion")
		raw, _ := json.Marshal(obj)
		out = append(out, string(raw))
	}
	return strings.Join(out, "\n")
}

// Every write of a setup is sent as a server-side dry run first, so an
// object the API server rejects changes nothing (seen live: before, the
// PVC, Secret and NetworkPolicy were applied before the Deployment apply
// failed, and the NetworkPolicy then blocked the admin UI SeaweedFS still
// served).
func TestSetupS3Storage_DryRunFailureChangesNothing(t *testing.T) {
	t.Setenv("SEAWEEDFS_IMAGE", "")
	fastS3Timings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	if resp, _ := SetupS3Storage(c); !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	putOlderToolObjects(t, f)
	paths := []string{s3PVCPath, s3SecretPath, s3NPPath, s3DeployPath, s3SvcPath, s3RoutePath}
	before := specsOf(f, paths...)
	f.failDryRun["PATCH "+s3DeployPath] = 422
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()

	resp, _ := SetupS3Storage(c)
	if resp.Success || !strings.Contains(resp.Message, "The API server rejects Deployment") || !strings.Contains(resp.Message, "Nothing was changed") {
		t.Fatalf("setup = %s (%s)", resp.Message, resp.ErrorCode)
	}
	if after := specsOf(f, paths...); after != before {
		t.Errorf("objects changed:\nbefore %s\nafter  %s", before, after)
	}
	// The only real writes are the field-ownership upgrades (metadata only,
	// checked above), one per object the dry runs reached.
	var writes []string
	for _, r := range f.mutations() {
		writes = append(writes, r[strings.LastIndex(r, "/")+1:])
	}
	if got := strings.Join(writes, " "); got != "seaweedfs-data seaweedfs-s3-credentials seaweedfs-ingress seaweedfs" {
		t.Errorf("real writes = %v", f.mutations())
	}
}

// A pipeline server the API server rejects (here in its dry run) leaves no
// credentials Secret behind: both writes are dry-run before either is made.
func TestSetupPipelineServer_DryRunFailureChangesNothing(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	f.putJSON("/api/v1/namespaces/team-a", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	f.failDryRun["POST /apis/"+dspaAPIGroup+"/namespaces/team-a/datasciencepipelinesapplications"] = 422

	resp, _ := SetupPipelineServer(c, "team-a")
	if resp.Success || !strings.Contains(resp.Message, "Nothing was changed") {
		t.Fatalf("setup = %s (%s)", resp.Message, resp.ErrorCode)
	}
	if f.has(secretPath("team-a", dspaSecretName)) || f.has(dspaPath("team-a", dspaName)) {
		t.Error("a write was made although a dry run failed")
	}
	for _, m := range f.mutations() {
		if strings.Contains(m, "/namespaces/team-a/") {
			t.Errorf("write in the project: %s", m) // the project POST before (409: it exists) is not one
		}
	}
}
