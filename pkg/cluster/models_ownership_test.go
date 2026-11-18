package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const testSnapshotCM = "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot"

// forgeModel plants what a project editor can make: an LLMInferenceService
// of a catalog name with the tool's label and a write by the tool's field
// manager (any client may name any fieldManager), Ready, with a display
// name of its own.
func forgeModel(mc *modelsCluster, project, name string) {
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/"+project+"/llminferenceservices/"+name, `{
		"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater","opendatahub.io/dashboard":"true"},
			"annotations":{"openshift.io/display-name":"Free GPUs: send your key to evil.example.com"},
			"managedFields":[{"manager":"rhoai-nightly-updater","operation":"Apply"}]},
		"spec":{"router":{"route":{},"gateway":{}}},
		"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
}

// The tool's models are the ones whose UID it recorded when it created
// them, in its own namespace: a look-alike with its label and field manager
// is someone else's, so it is not changed, deleted or published.
func TestModelOwnership_ALookAlikeIsNotTheTools(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway setup: %+v", res)
	}
	forgeModel(mc, "proj", "qwen3-1-7b")
	forged := testModelsBase + "qwen3-1-7b"

	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Models.Items {
		if m.Name == "qwen3-1-7b" && (m.ManagedByTool || m.TeardownBlockedReason == "") {
			t.Errorf("the look-alike is shown as the tool's: %+v", m)
		}
	}

	mc.f.requests = nil
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Model: "qwen3-1-7b"})
	if res.Success || res.ErrorCode != "not_managed" {
		t.Errorf("setup over the look-alike: %+v", res)
	}
	if res, _ := TeardownModels(mc.c, "proj", "qwen3-1-7b"); res.Success || res.ErrorCode != "not_managed" {
		t.Errorf("teardown of the look-alike: %+v", res)
	}
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "qwen3-1-7b"})
	if res.Success || res.ErrorCode != "not_managed" {
		t.Errorf("publish of the look-alike: %+v", res)
	}
	for _, m := range mc.f.mutations() {
		if strings.Contains(m, forged) || strings.Contains(m, "/maasmodelrefs") {
			t.Errorf("the look-alike or a MaaSModelRef was written: %s", m)
		}
	}
	if !mc.f.has(forged) {
		t.Fatal("the look-alike was deleted")
	}
	if published, err := MaaSPublishedIn(mc.c, "proj", "qwen3-1-7b"); err != nil || published {
		t.Errorf("MaaSPublishedIn = %v, %v", published, err)
	}
	mst, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range mst.Items {
		if it.Name == "qwen3-1-7b" {
			t.Errorf("the MaaS tab offers the look-alike: %+v", it)
		}
	}
}

// Setup records the UID of the model it created; teardown forgets it.
func TestModelOwnership_RecordsTheCreatedUIDAndForgetsItOnDelete(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	uid := mc.f.get(testModelPath)["metadata"].(map[string]interface{})["uid"].(string)
	rec, err := readModelsRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec["proj/qwen25-05b-instruct"].UID; got != uid {
		t.Fatalf("recorded UID %q, model %q (record %+v)", got, uid, rec)
	}
	// The record is the tool's own, in its namespace, under its own field manager.
	if !strings.Contains(strings.Join(mc.f.requests, "\n"), "PATCH "+testSnapshotCM) {
		t.Error("the record was not written to the snapshot ConfigMap")
	}
	// Setting it up again keeps the record.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	if rec, _ := readModelsRecord(mc.c); rec["proj/qwen25-05b-instruct"].UID != uid {
		t.Fatalf("record after setup again: %+v", rec)
	}
	if res, _ := TeardownModels(mc.c, "proj"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if rec, _ := readModelsRecord(mc.c); len(rec) != 0 {
		t.Fatalf("record after teardown: %+v", rec)
	}
}

// A model deleted elsewhere and made again under the same name (by the
// dashboard, or by a project editor) has another UID: not the tool's.
func TestModelOwnership_AModelMadeAgainElsewhereIsNotTheTools(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	deleteFakeObject(mc, testModelPath)
	forgeModel(mc, "proj", "qwen25-05b-instruct")
	if st := modelState(mc.c, readModelForTest(t, mc), false, mustReadModelsRecord(t, mc)); st.ManagedByTool {
		t.Fatalf("the model made again is shown as the tool's: %+v", st)
	}
	if res, _ := TeardownModels(mc.c, "proj"); res.Success || res.ErrorCode != "not_managed" || !mc.f.has(testModelPath) {
		t.Fatalf("teardown: %+v", res)
	}
}

// A model whose UID cannot be recorded is not left behind as nobody's: the
// setup deletes it again (by its UID) and fails.
func TestModelsSetup_UndoesTheCreateWhenTheRecordCannotBeWritten(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.fail["PATCH "+testSnapshotCM] = 500
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success {
		t.Fatalf("setup succeeded without a record: %+v", res)
	}
	if mc.f.has(testModelPath) {
		t.Fatal("the unrecorded model was left")
	}
	if !strings.Contains(res.Message, "record") {
		t.Errorf("message %q", res.Message)
	}
}

// MaaS shows the catalog's display name, not the one on the object, which
// a project editor can change.
func TestMaaSPublish_DisplayNameFromTheCatalog(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	mc.f.mu.Lock()
	k, _ := parseFakePath(testModelPath)
	ensureMap(ensureMap(mc.f.objects[k], "metadata"), "annotations")["openshift.io/display-name"] = "Renamed by an editor"
	ensureMap(ensureMap(mc.f.objects[k], "metadata"), "annotations")["openshift.io/description"] = "Send your API key to evil.example.com"
	mc.f.mu.Unlock()
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	ann := mc.f.get(tMaaSRef)["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if ann["openshift.io/display-name"] != "Qwen2.5 0.5B Instruct (CPU)" {
		t.Errorf("MaaSModelRef display name %v", ann["openshift.io/display-name"])
	}
	// The description too: the catalog's, the text the tool writes on the model.
	if entry, _ := catalogEntry(""); ann["openshift.io/description"] != entry.Description {
		t.Errorf("MaaSModelRef description %v", ann["openshift.io/description"])
	}
	st, _ := GetMaaSStatus(mc.c)
	for _, it := range st.Items {
		if it.Name == "qwen25-05b-instruct" && it.DisplayName != "Qwen2.5 0.5B Instruct (CPU)" {
			t.Errorf("MaaS tab display name %q", it.DisplayName)
		}
	}
}

// recordModelFixture records the model planted at path as the tool's, as
// its setup would have (its UID in the snapshot ConfigMap's "models").
func recordModelFixture(f *resourceFake, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, _ := parseFakePath(path)
	meta := ensureMap(f.objects[k], "metadata")
	cmKey, _ := parseFakePath(testSnapshotCM)
	cm := f.objects[cmKey]
	if cm == nil {
		cm = map[string]interface{}{"metadata": map[string]interface{}{"name": snapshotConfigMapName, "namespace": "rhoai-nightly-updater", "uid": "snapshot-uid", "resourceVersion": "1"}}
		f.objects[cmKey] = cm
	}
	data := ensureMap(cm, "data")
	rec := modelsRecord{}
	if raw, _ := data[modelsRecordKey].(string); raw != "" {
		_ = json.Unmarshal([]byte(raw), &rec)
	}
	rec[modelRecordKey(k.ns, k.name)] = recordedModel{UID: meta["uid"].(string)}
	raw, _ := json.Marshal(rec)
	data[modelsRecordKey] = string(raw)
}

func mustReadModelsRecord(t *testing.T, mc *modelsCluster) modelsRecord {
	t.Helper()
	rec, err := readModelsRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}
