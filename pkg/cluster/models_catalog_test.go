package cluster

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const testModelsBase = "/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/"

// putModelDeploymentFor stores a rolled-out workload Deployment of model
// name in proj, as KServe makes it.
func putModelDeploymentFor(mc *modelsCluster, name string) {
	mc.f.putJSON("/apis/apps/v1/namespaces/proj/deployments/"+name+"-kserve", fmt.Sprintf(`{"metadata":{"name":"%s-kserve","generation":2,
		"labels":{"app.kubernetes.io/name":%q,"app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"replicas":1},
		"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`, name, name))
}

func envOf(container map[string]interface{}) map[string]string {
	out := map[string]string{}
	list, _ := container["env"].([]interface{})
	for _, e := range list {
		m := e.(map[string]interface{})
		out[m["name"].(string)] = m["value"].(string)
	}
	return out
}

// The catalog: the four models the user chose, each pinned to a Hugging
// Face commit in KServe's hf://org/repo:<revision> form, with what it is
// for and what it needs. The default (an empty Model) stays qwen25.
func TestModelCatalog_TheFourModels(t *testing.T) {
	want := []struct {
		name, uri, typ, cpu, req, limit, threads, kv, args, useCase string
		tools                                                       bool
		dim                                                         int
	}{
		{"smollm2-135m-instruct", "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac", "chat", "2", "5Gi", "6Gi", "2", "1",
			"--max-model-len 4096", "Chat (smoke test)", false, 0},
		{"qwen25-05b-instruct", "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775", "chat", "4", "6Gi", "8Gi", "4", "2",
			"--max-model-len 4096", "Chat", false, 0},
		{"qwen3-1-7b", "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e", "chat", "4", "10Gi", "12Gi", "4", "2",
			`--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking":false}'`,
			"Chat, Tool calling, Reasoning", true, 0},
		{"granite-embedding-125m-english", "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f", "embedding", "2", "4Gi", "6Gi", "2", "1",
			"--runner pooling --max-model-len 512", "Embeddings, Semantic search", false, 768},
	}
	if len(modelCatalog) != len(want) {
		t.Fatalf("catalog has %d models", len(modelCatalog))
	}
	pinned := regexp.MustCompile(`^hf://[^/]+/[^/:]+:[0-9a-f]{40}$`)
	for i, w := range want {
		m := modelCatalog[i]
		if m.Name != w.name || m.URI != w.uri || m.Type != w.typ || m.CPU != w.cpu || m.MemoryRequest != w.req || m.Memory != w.limit ||
			m.threads != w.threads || m.kvCacheGiB != w.kv || m.Args != w.args || m.UseCase != w.useCase ||
			m.ToolCalling != w.tools || m.EmbeddingDimension != w.dim {
			t.Errorf("entry %d = %+v (threads %s, kv %s)", i, m.CatalogModel, m.threads, m.kvCacheGiB)
		}
		if !pinned.MatchString(m.URI) {
			t.Errorf("%s is not pinned to a commit: %s", m.Name, m.URI)
		}
		if m.DisplayName == "" || m.Description == "" || m.DownloadSize == "" {
			t.Errorf("%s lacks words: %+v", m.Name, m.CatalogModel)
		}
		if m.Default != (m.Name == "qwen25-05b-instruct") {
			t.Errorf("%s default = %v", m.Name, m.Default)
		}
	}
	if def, ok := catalogEntry(""); !ok || def.Name != "qwen25-05b-instruct" {
		t.Errorf("the default is %q", def.Name)
	}
	if ValidModelName("llama") || !ValidModelName("qwen3-1-7b") || !ValidModelName("") {
		t.Error("ValidModelName")
	}
}

// Every catalog model has a path, and nothing else does: the RBAC rules
// name exactly the catalog (deploy/template.yaml).
func TestToolModelPath_IsExactlyTheCatalog(t *testing.T) {
	for _, m := range modelCatalog {
		if p, ok := toolModelPath("proj", m.Name); !ok || p != testModelsBase+m.Name {
			t.Errorf("%s: %q %v", m.Name, p, ok)
		}
	}
	for _, name := range []string{"", "other", "qwen25"} {
		if _, ok := toolModelPath("proj", name); ok {
			t.Errorf("%q has a path", name)
		}
	}
}

// Each model is written with its own resources, threads, KV cache and vLLM
// arguments, the dashboard's annotations with its use case and
// description, and a storage-initializer that skips the repository's files
// vLLM does not load.
func TestModelManifest_UsesTheEntrysSettings(t *testing.T) {
	for _, entry := range modelCatalog {
		obj := modelManifest("proj", entry, testPresetName, imagePlan{Kind: "preset"}, hardwareProfile{}, false)
		meta := obj["metadata"].(map[string]interface{})
		if meta["name"] != entry.Name {
			t.Errorf("%s: name %v", entry.Name, meta["name"])
		}
		ann := meta["annotations"].(map[string]interface{})
		if ann["openshift.io/display-name"] != entry.DisplayName || ann["openshift.io/description"] != entry.Description ||
			ann["opendatahub.io/genai-use-case"] != entry.UseCase || ann["opendatahub.io/model-type"] != "generative" {
			t.Errorf("%s: annotations %v", entry.Name, ann)
		}
		labels := meta["labels"].(map[string]interface{})
		if labels["opendatahub.io/dashboard"] != "true" || labels["opendatahub.io/genai-asset"] != "true" || labels[managedByLabelKey] != managedByLabelValue {
			t.Errorf("%s: labels %v", entry.Name, labels)
		}
		spec := obj["spec"].(map[string]interface{})
		if m := spec["model"].(map[string]interface{}); m["uri"] != entry.URI || m["name"] != entry.Name {
			t.Errorf("%s: model %v", entry.Name, m)
		}
		tmpl := spec["template"].(map[string]interface{})
		main := tmpl["containers"].([]interface{})[0].(map[string]interface{})
		env := envOf(main)
		if env["OMP_NUM_THREADS"] != entry.threads || env["VLLM_CPU_KVCACHE_SPACE"] != entry.kvCacheGiB || env["VLLM_ADDITIONAL_ARGS"] != entry.Args {
			t.Errorf("%s: env %v", entry.Name, env)
		}
		res, _ := json.Marshal(main["resources"])
		if want := fmt.Sprintf(`{"limits":{"cpu":%q,"memory":%q},"requests":{"cpu":%q,"memory":%q}}`, entry.CPU, entry.Memory, entry.CPU, entry.MemoryRequest); string(res) != want {
			t.Errorf("%s: resources %s", entry.Name, res)
		}
		inits, _ := tmpl["initContainers"].([]interface{})
		if len(inits) != 1 {
			t.Fatalf("%s: init containers %v", entry.Name, inits)
		}
		si := inits[0].(map[string]interface{})
		if si["name"] != "storage-initializer" || len(si) != 2 {
			t.Errorf("%s: storage-initializer %v (only its env may be set)", entry.Name, si)
		}
		if got := envOf(si)["STORAGE_IGNORE_PATTERNS"]; got != `["onnx/*","*.onnx","*.bin","*.pt"]` {
			t.Errorf("%s: STORAGE_IGNORE_PATTERNS %q", entry.Name, got)
		}
	}
}

// Qwen3's arguments hold a JSON value in single quotes: the CPU preset
// starts vLLM with eval "exec vllm serve ... ${VLLM_ADDITIONAL_ARGS} ..."
// (kserve rhoai-3.6 config/llmisvcconfig/config-llm-template.yaml), so the
// shell takes the quotes off and vLLM gets the JSON as one argument.
//
// The JSON has no space either: the dashboard's Edit splits
// VLLM_ADDITIONAL_ARGS on spaces into its runtime arguments and joins them
// back with one space (odh-dashboard rhoai-3.6 a9ea0d86
// packages/llmd-serving/src/deployments/model.ts extractRuntimeArgs
// L147-159, applyModelEnvVarsAndArgs L137-141), so '{"enable_thinking":
// and false}' would be listed as two arguments. Every catalog model's
// arguments are therefore words without spaces: as many as the eval gives
// back.
func TestModelCatalog_Qwen3ArgumentsSurviveThePresetsEval(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to check the preset's eval with")
	}
	eval := func(args string) []string {
		cmd := exec.Command(bash, "-c", `eval "set -- serve \
  ${VLLM_ADDITIONAL_ARGS} \
  --port 8000"; printf '%s\n' "$@"`)
		cmd.Env = []string{"VLLM_ADDITIONAL_ARGS=" + args}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	}
	entry, _ := catalogEntry("qwen3-1-7b")
	want := []string{"serve", "--max-model-len", "4096", "--enable-auto-tool-choice", "--tool-call-parser", "hermes", "--reasoning-parser", "qwen3",
		"--default-chat-template-kwargs", `{"enable_thinking":false}`, "--port", "8000"}
	if got := eval(entry.Args); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("arguments\n got %q\nwant %q", got, want)
	}
	for _, m := range modelCatalog {
		if words, parts := len(eval(m.Args))-3, len(strings.Split(m.Args, " ")); words != parts {
			t.Errorf("%s: %d shell words but %d space-separated parts, so the dashboard's Edit would split an argument: %s", m.Name, words, parts, m.Args)
		}
	}
}

// Setup acts on the chosen model: its own LLMInferenceService, resources,
// hardware profile and capacity check.
func TestModelsSetup_TheChosenModel(t *testing.T) {
	mc := newModelsCluster(t)
	putModelDeploymentFor(mc, "qwen3-1-7b")
	// The default profile holds qwen25 (6-8 GiB) but not Qwen3 (10-12 GiB).
	putProfiles(mc, hwProfile("default-profile", "4", "8Gi", ""), hwProfile("large", "8", "16Gi", ""))
	mc.f.putJSON("/api/v1/nodes/w1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"8","memory":"9Gi"}}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj", Model: "qwen3-1-7b"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	if !strings.Contains(res.Message, "Qwen3 1.7B (CPU, tool calling) is ready in proj.") {
		t.Errorf("message %q", res.Message)
	}
	if mc.f.has(testModelPath) {
		t.Error("the default model was created too")
	}
	model := mc.f.get(testModelsBase + "qwen3-1-7b")
	ann := model["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if ann[hardwareProfileNameAnnotation] != "large" {
		t.Errorf("hardware profile %v", ann[hardwareProfileNameAnnotation])
	}
	main := model["spec"].(map[string]interface{})["template"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})
	if r, _ := json.Marshal(main["resources"]); string(r) != `{"limits":{"cpu":"4","memory":"12Gi"},"requests":{"cpu":"4","memory":"10Gi"}}` {
		t.Errorf("resources %s", r)
	}
	// 9 GiB free holds qwen25's 6 GiB, not Qwen3's 10 GiB.
	warned := false
	for _, e := range events {
		warned = warned || (e.Step == "check" && e.Status == "success" && strings.Contains(e.Message, "it needs 4 CPUs and 10Gi of memory free"))
	}
	if !warned {
		t.Errorf("no capacity warning for the chosen model: %v", events)
	}
}

// A project holds several catalog models; teardown deletes only the chosen
// one, and the clean-up's teardown all of them.
func TestModels_SeveralModelsInAProject(t *testing.T) {
	mc := newModelsCluster(t)
	putModelDeploymentFor(mc, "smollm2-135m-instruct")
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup qwen25: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("setup smollm2: %+v", res)
	}
	st, _ := GetResourcesStatus(mc.c)
	var names []string
	for _, m := range st.Models.Items {
		names = append(names, m.Name)
	}
	if strings.Join(names, " ") != "qwen25-05b-instruct smollm2-135m-instruct" {
		t.Fatalf("items %v", names)
	}
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "smollm2-135m-instruct"})
	if !res.Success || finalSteps(events) != "model=success" {
		t.Fatalf("teardown: %+v %s", res, finalSteps(events))
	}
	if mc.f.has(testModelsBase+"smollm2-135m-instruct") || !mc.f.has(testModelPath) {
		t.Fatal("teardown did not delete only the chosen model")
	}
	if !strings.Contains(res.Message, "smollm2-135m-instruct") {
		t.Errorf("message %q", res.Message)
	}
	// An empty Model is the default model.
	putModelDeploymentFor(mc, "smollm2-135m-instruct")
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("setup smollm2 again: %+v", res)
	}
	if res, _ := TeardownModels(mc.c, "proj"); !res.Success {
		t.Fatalf("clean-up teardown: %+v", res)
	}
	if mc.f.has(testModelPath) || mc.f.has(testModelsBase+"smollm2-135m-instruct") {
		t.Fatal("the clean-up's teardown left a model")
	}
}

// The status offers the whole catalog, each model with its own room check,
// from one read of the nodes and pods.
func TestModelsStatus_CatalogWithEachModelsRoom(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/nodes/w1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"8","memory":"9Gi"}}}`)
	mc.f.mu.Lock()
	before := len(mc.f.requests)
	mc.f.mu.Unlock()
	st, _ := GetResourcesStatus(mc.c)
	got := map[string]string{}
	for _, m := range st.Models.Catalog {
		got[m.Name] = m.CapacityWarning
	}
	if len(st.Models.Catalog) != 4 || st.Models.Catalog[0].Name != "smollm2-135m-instruct" || st.Models.Catalog[3].Type != "embedding" {
		t.Fatalf("catalog %+v", st.Models.Catalog)
	}
	for name, room := range map[string]bool{"smollm2-135m-instruct": true, "qwen25-05b-instruct": true, "qwen3-1-7b": false, "granite-embedding-125m-english": true} {
		if (got[name] == "") != room {
			t.Errorf("%s: warning %q", name, got[name])
		}
	}
	if !strings.Contains(got["qwen3-1-7b"], "10Gi") {
		t.Errorf("qwen3 warning %q", got["qwen3-1-7b"])
	}
	pods := 0
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	for _, r := range mc.f.requests[before:] {
		if strings.HasPrefix(r, "GET /api/v1/pods") {
			pods++
		}
	}
	if pods != 1 {
		t.Errorf("the pods were listed %d times", pods)
	}
}

func TestValidateModelsRequest_UnknownModel(t *testing.T) {
	if got := ValidateModelsRequest(types.ModelsRequest{Action: "teardown", Project: "proj", Model: "llama"}); got != "unknown model" {
		t.Errorf("got %q", got)
	}
}

// defaultEntry is the catalog model an empty Model sets up.
func defaultEntry() catalogModel {
	e, _ := catalogEntry("")
	return e
}
