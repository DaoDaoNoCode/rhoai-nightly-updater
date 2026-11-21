package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Each model asks for node ephemeral storage of about 1.5 times its
// download: KServe rhoai-3.6 downloads it into an emptyDir by default
// (pkg/utils/storage.go AddModelMount; StorageInitializerConfig
// ModelVolumeSource nil).
func TestModelCatalog_EphemeralStorageIsAboutOneAndAHalfTheDownload(t *testing.T) {
	for _, m := range serving.ModelCatalog {
		dl, ok1 := serving.ParseQuantity(strings.NewReplacer(" GB", "G", " MB", "M").Replace(m.DownloadSize))
		eph, ok2 := serving.ParseQuantity(m.EphemeralStorage)
		if !ok1 || !ok2 || eph < 1.4*dl || eph > 1.6*dl {
			t.Errorf("%s: ephemeral %q for a %s download", m.Name, m.EphemeralStorage, m.DownloadSize)
		}
	}
}

func mainResources(t *testing.T, mc *modelsCluster) map[string]any {
	t.Helper()
	main := mc.f.get(testModelPath)["spec"].(map[string]any)["template"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	return main["resources"].(map[string]any)
}

// With KServe's default model volume (no modelVolumeSource in
// inferenceservice-config's storageInitializer), the download lands on the
// node's ephemeral storage, so the model requests it; a cluster whose
// KServe puts models on another volume gets no such request.
func TestModelsSetup_RequestsEphemeralStorageOnlyForTheDefaultVolume(t *testing.T) {
	for _, tc := range []struct {
		name, storageInitializer string
		want                     bool
	}{
		{"no config", "", true},
		{"null", `{"memoryRequest":"100Mi","modelVolumeSource":null}`, true},
		{"absent", `{"memoryRequest":"100Mi"}`, true},
		{"a PVC", `{"modelVolumeSource":{"persistentVolumeClaim":{"claimName":"models"}}}`, false},
	} {
		mc := newModelsCluster(t)
		if tc.storageInitializer != "" {
			mc.f.putJSON("/api/v1/namespaces/redhat-ods-applications/configmaps/inferenceservice-config", `{"data":{"ingress":"{\"kserveIngressGateway\":\"openshift-ingress/openshift-ai-inference\"}","storageInitializer":`+jsonOf(tc.storageInitializer)+`}}`)
		}
		if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
			t.Fatalf("%s: setup %+v", tc.name, res)
		}
		req := mainResources(t, mc)["requests"].(map[string]any)
		if got, set := req["ephemeral-storage"]; set != tc.want || (set && got != "1.5G") {
			t.Errorf("%s: requests %v", tc.name, req)
		}
	}
}

// The project's ResourceQuota (hard minus used) is part of the room check:
// a quota the model does not fit in is said before anything is written.
func TestModelsSetup_WarnsWhenTheProjectQuotaHasNoRoom(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/nodes/w1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"16","memory":"64Gi"}}}`)
	mc.f.putJSON("/api/v1/namespaces/proj/resourcequotas/compute", `{"status":{
		"hard":{"requests.cpu":"8","requests.memory":"16Gi","limits.memory":"20Gi","pods":"10"},
		"used":{"requests.cpu":"2","requests.memory":"12Gi","limits.memory":"10Gi","pods":"3"}}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	var check string
	for _, e := range events {
		if e.Step == "check" && e.Status == "success" {
			check = e.Message
		}
	}
	if !strings.Contains(check, "ResourceQuota compute") || !strings.Contains(check, "requests.memory") || !strings.Contains(check, "4Gi") || strings.Contains(check, "requests.cpu") {
		t.Errorf("check %q", check)
	}
	if strings.Contains(check, "pending until a node is added") {
		t.Errorf("a quota is not a node: %q", check)
	}
}

// A node without the ephemeral storage the model asks for has no room.
func TestModelsRoom_CountsEphemeralStorage(t *testing.T) {
	entry, _ := serving.CatalogEntry("qwen3-1-7b")
	free := map[string]*serving.NodeRoom{"w1": {CPU: 8, Mem: 32 << 30, Eph: 5e9, EphKnown: true}}
	if w := serving.RoomWarning(entry, free); !strings.Contains(w, "ephemeral storage") {
		t.Errorf("warning %q", w)
	}
	free["w1"].Eph = 10e9
	if w := serving.RoomWarning(entry, free); w != "" {
		t.Errorf("warning %q", w)
	}
}
