package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// editModel changes the tool's model in proj as someone else would.
func editModel(mc *modelsCluster, edit func(spec, meta map[string]any)) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	k, _ := parseFakePath(testModelPath)
	obj := mc.f.objects[k]
	edit(ensureMap(obj, "spec"), ensureMap(obj, "metadata"))
}

func templateContainer(spec map[string]any, list, name string) map[string]any {
	tmpl := ensureMap(spec, "template")
	items, _ := tmpl[list].([]any)
	for _, it := range items {
		if c, ok := it.(map[string]any); ok && c["name"] == name {
			return c
		}
	}
	return nil
}

// What every model has on RHOAI 3.6 (live): KServe's defaulting, field
// manager "manager" (Update, serving.kserve.io/v1alpha2), owns the main
// container's ephemeral-storage request and the storage-initializer's
// resources. That must not stop a publication.
func TestMaaSPublish_PublishesAModelKServeDefaulted(t *testing.T) {
	mc := modelReadyForMaaS(t)
	editModel(mc, func(spec, meta map[string]any) {
		main := templateContainer(spec, "containers", "main")
		req := ensureMap(ensureMap(main, "resources"), "requests")
		req["ephemeral-storage"] = "2Gi"
		initC := templateContainer(spec, "initContainers", "storage-initializer")
		initC["resources"] = map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "100Mi"}, "limits": map[string]any{"cpu": "1", "memory": "1Gi"}}
		meta["managedFields"] = []any{
			map[string]any{"manager": envkit.ToolFieldManager, "operation": "Apply", "apiVersion": "serving.kserve.io/v1alpha2", "fieldsType": "FieldsV1",
				"fieldsV1": map[string]any{"f:spec": map[string]any{"f:model": map[string]any{}, "f:template": map[string]any{}}}},
			map[string]any{"manager": "manager", "operation": "Update", "apiVersion": "serving.kserve.io/v1alpha2", "fieldsType": "FieldsV1",
				"fieldsV1": map[string]any{"f:spec": map[string]any{"f:template": map[string]any{
					"f:containers":     map[string]any{`k:{"name":"main"}`: map[string]any{"f:resources": map[string]any{"f:requests": map[string]any{"f:ephemeral-storage": map[string]any{}}}}},
					"f:initContainers": map[string]any{`k:{"name":"storage-initializer"}`: map[string]any{"f:resources": map[string]any{}}},
				}}}},
		}
	})
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
}

// Someone who may edit the project changes the tool's model before a
// cluster-admin publishes it: publish refuses before any write and names
// what differs.
func TestMaaSPublish_RefusesAChangedModelSpec(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		edit        func(spec map[string]any)
	}{
		{"main image", "spec.template.containers[main].image", func(spec map[string]any) {
			templateContainer(spec, "containers", "main")["image"] = "quay.io/someone/vllm:latest"
		}},
		{"model uri", "spec.model.uri", func(spec map[string]any) {
			ensureMap(spec, "model")["uri"] = "hf://someone/other-model"
		}},
		{"extra env", "spec.template.containers[main].env", func(spec map[string]any) {
			main := templateContainer(spec, "containers", "main")
			main["env"] = append(main["env"].([]any), map[string]any{"name": "HF_ENDPOINT", "value": "https://evil.example.com"})
		}},
		// Set up by a version before every chat model had
		// --enable-force-include-usage: MaaS would count its streamed
		// tokens as 0, so it is set up again first.
		{"no usage flag", "spec.template.containers[main].env", func(spec map[string]any) {
			for _, e := range templateContainer(spec, "containers", "main")["env"].([]any) {
				if ev := e.(map[string]any); ev["name"] == "VLLM_ADDITIONAL_ARGS" {
					ev["value"] = strings.TrimSuffix(ev["value"].(string), " --enable-force-include-usage")
				}
			}
		}},
		{"command", "spec.template.containers[main].command", func(spec map[string]any) {
			templateContainer(spec, "containers", "main")["command"] = []any{"sh", "-c", "curl evil"}
		}},
		{"extra container", "spec.template.containers[sidecar]", func(spec map[string]any) {
			tmpl := ensureMap(spec, "template")
			tmpl["containers"] = append(tmpl["containers"].([]any), map[string]any{"name": "sidecar", "image": "quay.io/x/y"})
		}},
		{"service account", "spec.template.serviceAccountName", func(spec map[string]any) {
			ensureMap(spec, "template")["serviceAccountName"] = "builder"
		}},
		{"preset", "spec.baseRefs", func(spec map[string]any) {
			spec["baseRefs"] = []any{map[string]any{"name": "my-preset"}}
		}},
		{"prefill", "spec.prefill", func(spec map[string]any) {
			spec["prefill"] = map[string]any{"template": map[string]any{}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := modelReadyForMaaS(t)
			editModel(mc, func(spec, _ map[string]any) { tc.edit(spec) })
			mc.f.requests = nil
			res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "was changed since this tool set it up: "+tc.field) ||
				!strings.Contains(res.Message, "set it up again") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("publish = %+v", res)
			}
			if muts := mc.f.mutations(); len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// The released image (a Red Hat release of the preset's repository,
// pinned by digest) and VLLM_CPU_IMAGE are what setup itself may write.
func TestModelSpecExpectation_Images(t *testing.T) {
	e := serving.ModelSpecExpectation{PresetRepo: "registry.redhat.io/rhaii/vllm-cpu-rhel9", Override: "mirror.example.com/vllm@sha256:abc"}
	for img, want := range map[string]bool{
		"": true,
		"registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:0123": true,
		"mirror.example.com/vllm@sha256:abc":                  true,
		"registry.redhat.io/rhaii/vllm-cpu-rhel9:latest":      false,
		"registry.redhat.io/rhaii/other@sha256:0123":          false,
		"quay.io/someone/vllm-cpu-rhel9@sha256:0123":          false,
	} {
		if got := e.ImageAllowed(img); got != want {
			t.Errorf("imageAllowed(%q) = %v, want %v", img, got, want)
		}
	}
}

// A chat model set up before every chat model had
// --enable-force-include-usage keeps running without it: its MaaS row warns
// (read-only; nothing is migrated) that its streamed tokens are not
// counted, and how to fix it. Set up again adds the flag and the warning
// goes.
func TestMaaSStatus_WarnsAboutAModelWithoutTheUsageFlag(t *testing.T) {
	mc := modelReadyForMaaS(t)
	row := func() types.MaaSModel {
		st, err := GetMaaSStatus(mc.c)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range st.Items {
			if i.Name == "qwen25-05b-instruct" {
				return i
			}
		}
		t.Fatalf("no row: %+v", st.Items)
		return types.MaaSModel{}
	}
	if w := row().UsageWarning; w != "" {
		t.Fatalf("warning on a model with the flag: %q", w)
	}
	editModel(mc, func(spec, _ map[string]any) {
		for _, e := range templateContainer(spec, "containers", "main")["env"].([]any) {
			if ev := e.(map[string]any); ev["name"] == "VLLM_ADDITIONAL_ARGS" {
				ev["value"] = "--max-model-len 4096"
			}
		}
	})
	w := row().UsageWarning
	for _, want := range []string{"without --enable-force-include-usage (set up by an earlier version)", "counts its streamed chats as 0 tokens", "Set it up again on the Models tab to add it (the model restarts)"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("set up again: %+v", res)
	}
	if w := row().UsageWarning; w != "" {
		t.Errorf("warning after set up again: %q", w)
	}
}
