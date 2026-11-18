package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Models: a small CPU model served by KServe's LLMInferenceService in a
// data science project, shaped as the RHOAI 3.6 dashboard's deployment
// wizard writes one (odh-dashboard packages/llmd-serving deploy.ts,
// model.ts), so the dashboard lists it and its Gen AI playground can use
// it. Nothing release-specific is in this file: the CPU preset is found
// through the controller's LLM_INFERENCE_SERVICE_CONFIG_PREFIX, its image
// through the preset, and the released image through Red Hat's catalog.
//
// No ServiceAccount, Role, RoleBinding or token is created for access:
// the gateway's AuthPolicy accepts any token whose user may "get" the
// model, so the user's own (oc whoami -t) works.
const (
	llmisvcAPIVersion      = "serving.kserve.io/v1alpha2"
	llmisvcGroup           = "serving.kserve.io"
	llmisvcControllerName  = "llmisvc-controller-manager"
	llmisvcConfigPrefixEnv = "LLM_INFERENCE_SERVICE_CONFIG_PREFIX"
	// cpuPresetSuffix follows the prefix in the CPU preset's name
	// (<prefix>config-llm-template-cpu; kserve config/overlays/odh/accelerators).
	cpuPresetSuffix = "config-llm-template-cpu"
	// The image annotations record which image a model runs when this tool
	// set one (no annotation: the preset's own).
	modelImageSourceAnnotation  = "rhoai-nightly-updater/runtime-image-source"
	modelImageVersionAnnotation = "rhoai-nightly-updater/runtime-image-version"
	llmDependenciesCondition    = "KserveLLMInferenceServiceDependencies"
	defaultApplicationsNS       = "redhat-ods-applications"
	// modelPodComponent is the label KServe puts on a model's workload pods
	// (with app.kubernetes.io/name=<model>); the dashboard selects them so.
	modelPodComponent = "llminferenceservice-workload"
)

var (
	// ModelReadyTimeout bounds the wait for a new model: 188 s measured
	// cold on m5.4xlarge (image pull, model download, vLLM start). The
	// workload's own startupProbe gives up after 10 minutes.
	ModelReadyTimeout = 10 * time.Minute
	ModelPoll         = 5 * time.Second
	// ModelDeleteTimeout bounds the wait for KServe's finalizer, which
	// deletes the model's Deployment, Service and HTTPRoute.
	ModelDeleteTimeout = 2 * time.Minute
	ModelDeletePoll    = 2 * time.Second
)

var errLLMISvcAPIMissing = errors.New("the LLMInferenceService API (serving.kserve.io) is not installed")

// catalogModel is a model the Models feature sets up: what it is, and the
// resources it needs on CPU. CatalogModel.Args is VLLM_ADDITIONAL_ARGS.
type catalogModel struct {
	types.CatalogModel
	// threads is OMP_NUM_THREADS, the CPU count.
	threads string
	// kvCacheGiB is VLLM_CPU_KVCACHE_SPACE: vLLM on CPU reserves this
	// much memory for its KV cache when it starts, whatever the load.
	kvCacheGiB string
}

// The catalog's model types.
const (
	modelTypeChat      = "chat"
	modelTypeEmbedding = "embedding"
)

// modelCatalog: small models that run on CPU, ungated on Hugging Face,
// Apache-2.0, safetensors weights and no custom code, each pinned to a
// commit in KServe's hf://<org>/<repo>:<revision> form (kserve rhoai-3.6
// python/storage/kserve_storage/kserve_storage.py _download_hf:
// model_part.partition(":") gives snapshot_download its revision). The
// order is the page's; qwen25 is the default (an empty Model).
//
// Every model's arguments go to vLLM through VLLM_ADDITIONAL_ARGS, which
// the CPU preset expands inside eval "exec vllm serve ... ${VLLM_ADDITIONAL_ARGS} ..."
// (kserve rhoai-3.6 config/llmisvcconfig/config-llm-template.yaml), so
// shell quoting in them is honoured: Qwen3's JSON is one argument.
//
// Memory (requests and limits below, in one place). Measured live only
// for qwen25 (RHOAI 3.6, vLLM CPU): 7.6 GiB with the preset's 4 GiB KV
// cache (cgroup memory.peak 7.73 GiB over four parallel 600-token chats),
// 5.8 GiB with a 2 GiB cache, which still holds over 100k tokens for it
// (24 layers x 2 KV heads x 64 x 2 x 2 bytes = 12 KiB per token). That is
// about 1 GB of weights, the cache, and some 3 GiB of runtime. The others
// are estimates from the same parts (weights + KV cache + about 3 GiB),
// to be measured live: SmolLM2 0.27 GB + 1 GiB, Qwen3 4.1 GB + 2 GiB,
// Granite embedding 0.25 GB + 1 GiB. qwen25 also fits the dashboard's
// default hardware profile (up to 4 CPUs and 8 GiB on 3.6).
//
// DownloadSize is what the storage-initializer fetches at the pinned
// commit with storageIgnorePatterns applied (Hugging Face API file sizes).
var modelCatalog = []catalogModel{
	{
		CatalogModel: types.CatalogModel{Name: "smollm2-135m-instruct", DisplayName: "SmolLM2 135M Instruct (CPU)", URI: "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac",
			CPU: "2", MemoryRequest: "5Gi", Memory: "6Gi", Type: modelTypeChat, UseCase: "Chat (smoke test)", DownloadSize: "270 MB",
			Description: "A very small chat model, for checking that model serving works. From Hugging Face (HuggingFaceTB/SmolLM2-135M-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
			Args:        "--max-model-len 4096"},
		threads: "2", kvCacheGiB: "1",
	},
	{
		CatalogModel: types.CatalogModel{Name: "qwen25-05b-instruct", DisplayName: "Qwen2.5 0.5B Instruct (CPU)", URI: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775",
			CPU: "4", MemoryRequest: "6Gi", Memory: "8Gi", Type: modelTypeChat, UseCase: "Chat", DownloadSize: "1.0 GB", Default: true,
			Description: "A small chat model. From Hugging Face (Qwen/Qwen2.5-0.5B-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
			Args:        "--max-model-len 4096"},
		threads: "4", kvCacheGiB: "2",
	},
	{
		// Thinking is off by default (the user's choice); a request turns
		// it on with chat_template_kwargs {"enable_thinking": true}.
		CatalogModel: types.CatalogModel{Name: "qwen3-1-7b", DisplayName: "Qwen3 1.7B (CPU, tool calling)", URI: "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e",
			CPU: "4", MemoryRequest: "10Gi", Memory: "12Gi", Type: modelTypeChat, ToolCalling: true, UseCase: "Chat, Tool calling, Reasoning", DownloadSize: "4.1 GB",
			Description: "A chat model that can call tools; its reasoning (thinking) is off unless a request turns it on. From Hugging Face (Qwen/Qwen3-1.7B), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
			Args:        `--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking": false}'`},
		threads: "4", kvCacheGiB: "2",
	},
	{
		// A RobertaModel, which vLLM serves as an embedding model with the
		// pooling runner (vLLM 0.24 removed --task); 512 positions.
		CatalogModel: types.CatalogModel{Name: "granite-embedding-125m-english", DisplayName: "Granite Embedding 125M English (CPU)", URI: "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f",
			CPU: "2", MemoryRequest: "4Gi", Memory: "6Gi", Type: modelTypeEmbedding, EmbeddingDimension: 768, UseCase: "Embeddings, Semantic search", DownloadSize: "250 MB",
			Description: "An English embedding model (768 dimensions) for semantic search. From Hugging Face (ibm-granite/granite-embedding-125m-english), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
			Args:        "--runner pooling --max-model-len 512"},
		threads: "2", kvCacheGiB: "1",
	},
}

// storageIgnorePatterns (STORAGE_IGNORE_PATTERNS on the storage-initializer)
// leaves out the repository files vLLM does not load: ONNX exports and
// PyTorch .bin/.pt files (SmolLM2 has 1.7 GB of onnx/, Granite a
// model.onnx and a pytorch_model.bin next to its safetensors). KServe
// rhoai-3.6 reads it as a JSON list (kserve_storage.py
// _parse_patterns_from_env) and merges a user's storage-initializer env
// into the one it generates (pkg/controller/v1alpha2/llmisvc
// workload_storage.go attachStorageInitializer; tested by "should merge
// user-provided env vars into storage-initializer for hf:// URI").
const storageIgnorePatterns = `["onnx/*","*.onnx","*.bin","*.pt"]`

// catalogEntry is the catalog model of that name; "" is the default.
func catalogEntry(name string) (catalogModel, bool) {
	for _, m := range modelCatalog {
		if m.Name == name || (name == "" && m.Default) {
			return m, true
		}
	}
	return catalogModel{}, false
}

// ValidModelName reports whether name is a catalog model ("" is the default).
func ValidModelName(name string) bool {
	_, ok := catalogEntry(name)
	return ok
}

// toolModelPath returns the path of a catalog model in project; only the
// catalog's names, written out so the RBAC rules (resourceNames) can be
// checked against them (rbac_coverage_test.go).
func toolModelPath(project, name string) (string, bool) {
	switch name {
	case "smollm2-135m-instruct":
		return namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, "smollm2-135m-instruct"), true
	case "qwen25-05b-instruct":
		return namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, "qwen25-05b-instruct"), true
	case "qwen3-1-7b":
		return namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, "qwen3-1-7b"), true
	case "granite-embedding-125m-english":
		return namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, "granite-embedding-125m-english"), true
	}
	return "", false
}

type llmisvcObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Model struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"model"`
		Router *struct {
			Gateway *struct {
				Refs []struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"refs"`
			} `json:"gateway"`
		} `json:"router"`
		Template *struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		URL                string         `json:"url"`
		ObservedGeneration int64          `json:"observedGeneration"`
		Conditions         []k8sCondition `json:"conditions"`
	} `json:"status"`
}

// routesThroughInferenceGateway: spec.router.gateway is {} and KServe's
// default gateway (def) is the inference gateway, or it refers to the
// inference gateway. With the default unknown, {} counts: teardown then
// refuses, which is the safe side.
func (m llmisvcObject) routesThroughInferenceGateway(def kserveGateway) bool {
	if m.Spec.Router == nil || m.Spec.Router.Gateway == nil {
		return false
	}
	if len(m.Spec.Router.Gateway.Refs) == 0 {
		return def.isInferenceGateway()
	}
	for _, ref := range m.Spec.Router.Gateway.Refs {
		ns := firstNonEmpty(ref.Namespace, m.Metadata.Namespace)
		if ref.Name == inferenceGatewayName && ns == inferenceGatewayNamespace {
			return true
		}
	}
	return false
}

func (m llmisvcObject) mainImage() string {
	if m.Spec.Template == nil {
		return ""
	}
	for _, c := range m.Spec.Template.Containers {
		if c.Name == "main" {
			return c.Image
		}
	}
	return ""
}

// modelOwned: this tool created the model (its label, and a write by the tool).
func modelOwned(meta objectMeta) bool {
	return meta.hasToolLabel() && meta.hasManager(toolManagers)
}

func listLLMInferenceServices(c *Client) ([]llmisvcObject, error) {
	body, _, err := c.get(clusterPath("serving.kserve.io/v1alpha2", "llminferenceservices", ""))
	if IsK8sError(err, 404) {
		return nil, errLLMISvcAPIMissing
	}
	if err != nil {
		return nil, fmt.Errorf("list LLMInferenceServices: %w", err)
	}
	var list struct {
		Items []llmisvcObject `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse LLMInferenceServices: %w", err)
	}
	return list.Items, nil
}

func readToolModel(c *Client, project, name string) (llmisvcObject, bool, error) {
	path, ok := toolModelPath(project, name)
	if !ok {
		return llmisvcObject{}, false, fmt.Errorf("%s is not a model of this tool's catalog", name)
	}
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return llmisvcObject{}, false, nil
	}
	if err != nil {
		return llmisvcObject{}, false, err
	}
	var m llmisvcObject
	if err := json.Unmarshal(body, &m); err != nil {
		return llmisvcObject{}, false, fmt.Errorf("parse LLMInferenceService %s/%s: %w", project, name, err)
	}
	return m, true, nil
}

func modelPodSelector(name string) string {
	return "app.kubernetes.io/name=" + name + ",app.kubernetes.io/component=" + modelPodComponent
}

// manifestUnknown reports an image pull that failed because the registry
// does not have the image (a nightly build's preset image is not
// published), as opposed to a credential or network problem.
func manifestUnknown(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "manifest unknown") || strings.Contains(m, "manifest_unknown")
}

// modelState reports one model. inspect reads its pods when it is not ready.
func modelState(c *Client, m llmisvcObject, inspect bool) types.ModelState {
	s := types.ModelState{ResourceState: types.ResourceState{Deployed: true, Namespace: m.Metadata.Namespace, Name: m.Metadata.Name}}
	s.ManagedByTool = modelOwned(m.Metadata)
	s.Terminating = m.Metadata.terminating()
	s.DisplayName = m.Metadata.Annotations["openshift.io/display-name"]
	s.URL = m.Status.URL
	s.MaaSPublished = modelHasMaaSRef(m)
	s.CurrentImage = m.mainImage()
	s.ImageKind = firstNonEmpty(m.Metadata.Annotations[modelImageSourceAnnotation], "preset")
	s.ImageVersion = m.Metadata.Annotations[modelImageVersionAnnotation]
	if name := m.Metadata.Annotations[hardwareProfileNameAnnotation]; name != "" {
		s.HardwareProfile = m.Metadata.Annotations[hardwareProfileNamespaceAnnotation] + "/" + name
	}
	if !s.ManagedByTool {
		s.TeardownBlockedReason = "This model was not created by this tool (for example in the dashboard), so the tool does not change or delete it."
	}
	ready, hasReady := findCondition(m.Status.Conditions, "Ready")
	presets, hasPresets := findCondition(m.Status.Conditions, "PresetsCombined")
	switch {
	case s.Terminating:
		s.Message = "Terminating"
		return s
	case hasReady && ready.Status == "True":
		s.Ready = true
		s.Message = "Running"
		return s
	case hasPresets && presets.Status == "False":
		s.Message = firstNonEmpty(truncateMessage(presets.Message), presets.Reason, "Its preset cannot be resolved")
		s.WaitingReason = firstNonEmpty(presets.Reason, "PresetsCombined")
		s.TerminalError = true
		return s
	case hasReady:
		s.Message = firstNonEmpty(truncateMessage(ready.Message), ready.Reason, "Not ready")
	case s.ManagedByTool:
		s.Message = "Starting"
	default:
		s.Message = "Reports no Ready condition"
		s.HealthUnknown = true
	}
	if inspect {
		if issue, err := inspectPods(c, m.Metadata.Namespace, modelPodSelector(m.Metadata.Name), ""); err == nil {
			applyPodIssue(&s.ResourceState, issue)
			if s.ImageKind == "preset" && (issue.Reason == "ErrImagePull" || issue.Reason == "ImagePullBackOff") && manifestUnknown(issue.Message) {
				s.PresetImageMissing = true
				s.TerminalError = true
			}
		}
	}
	return s
}

// modelItems are the catalog's models in the visible projects.
func modelItems(c *Client, models []llmisvcObject, visible map[string]bool) []types.ModelState {
	var out []types.ModelState
	for _, m := range models {
		if !visible[m.Metadata.Namespace] {
			continue
		}
		if _, ok := toolModelPath(m.Metadata.Namespace, m.Metadata.Name); !ok {
			continue
		}
		out = append(out, modelState(c, m, true))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// kserveComponent is the DataScienceCluster component that serves models.
var kserveComponent = featureComponent{feature: "Model serving (KServe)", keys: []string{"kserve"}}

// llmDependencies reads the DataScienceCluster condition that says whether
// LLMInferenceService's dependencies are met: "True", "False" or "" (not
// reported: unknown), with its message.
func llmDependencies(dsc map[string]interface{}) (string, string) {
	status, _ := dsc["status"].(map[string]interface{})
	conds, _ := status["conditions"].([]interface{})
	for _, raw := range conds {
		cond, _ := raw.(map[string]interface{})
		if t, _ := cond["type"].(string); t != llmDependenciesCondition {
			continue
		}
		s, _ := cond["status"].(string)
		msg, _ := cond["message"].(string)
		if s != "True" && s != "False" {
			s = ""
		}
		return s, msg
	}
	return "", ""
}

// modelsBlock says why no model can be set up from the DataScienceCluster:
// KServe is not turned on, or its LLMInferenceService dependencies are
// reported missing. Not reported is not a block.
func modelsBlock(dsc dscRead) (*types.ComponentBlock, string) {
	if block := componentBlock(dsc, kserveComponent); block != nil {
		return block, ""
	}
	deps, msg := llmDependencies(dsc.Object)
	if deps == "False" {
		meta, _ := dsc.Object["metadata"].(map[string]interface{})
		name, _ := meta["name"].(string)
		return &types.ComponentBlock{Component: "kserve",
			Reason: fmt.Sprintf("KServe cannot serve LLMInferenceServices yet: DataScienceCluster %s reports %s=False (%s). A cluster-admin installs what it names; the Components page shows the details.", name, llmDependenciesCondition, firstNonEmpty(msg, "no message"))}, deps
	}
	return nil, deps
}

// appsNamespace is the RHOAI applications namespace (DSCInitialization
// spec.applicationsNamespace), redhat-ods-applications when unreadable.
func appsNamespace(c *Client) string {
	return firstNonEmpty(applicationsNamespace(c), defaultApplicationsNS)
}

// cpuPreset is the CPU preset as found at run time.
type cpuPreset struct {
	name, image, problem string
}

// discoverCPUPreset finds the CPU preset: the controller's config prefix
// (Deployment llmisvc-controller-manager, env
// LLM_INFERENCE_SERVICE_CONFIG_PREFIX, "v3-6-0-kserve-" on RHOAI 3.6)
// followed by config-llm-template-cpu, in the applications namespace, and
// its main container's image.
func discoverCPUPreset(c *Client, ns string) cpuPreset {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", ns, "llmisvc-controller-manager"))
	if IsK8sError(err, 404) {
		return cpuPreset{problem: fmt.Sprintf("KServe's LLMInferenceService controller (Deployment %s in %s) is not there: LLM serving is not installed.", llmisvcControllerName, ns)}
	}
	if err != nil {
		return cpuPreset{problem: fmt.Sprintf("Cannot read Deployment %s/%s: %v.%s", ns, llmisvcControllerName, err, templateHint(err))}
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return cpuPreset{problem: fmt.Sprintf("Cannot parse Deployment %s/%s: %v.", ns, llmisvcControllerName, err)}
	}
	prefix, found := "", false
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		for _, e := range ctr.Env {
			if e.Name == llmisvcConfigPrefixEnv {
				prefix, found = e.Value, true
			}
		}
	}
	if !found {
		return cpuPreset{problem: fmt.Sprintf("Deployment %s/%s sets no %s, so the CPU preset's name is unknown on this release.", ns, llmisvcControllerName, llmisvcConfigPrefixEnv)}
	}
	name := prefix + cpuPresetSuffix
	body, _, err = c.get(namespacedPath("serving.kserve.io/v1alpha2", "llminferenceserviceconfigs", ns, name))
	if IsK8sError(err, 404) {
		return cpuPreset{name: name, problem: fmt.Sprintf("This RHOAI release has no CPU preset: LLMInferenceServiceConfig %s does not exist in %s.", name, ns)}
	}
	if err != nil {
		return cpuPreset{name: name, problem: fmt.Sprintf("Cannot read the CPU preset %s/%s: %v.%s", ns, name, err, templateHint(err))}
	}
	var cfg struct {
		Spec struct {
			Template *struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			Conditions []k8sCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return cpuPreset{name: name, problem: fmt.Sprintf("Cannot parse the CPU preset %s/%s: %v.", ns, name, err)}
	}
	if cond, ok := findCondition(cfg.Status.Conditions, "Ready"); ok && cond.Status == "False" {
		return cpuPreset{name: name, problem: fmt.Sprintf("The CPU preset %s/%s is not ready: %s.", ns, name, firstNonEmpty(cond.Message, cond.Reason))}
	}
	p := cpuPreset{name: name}
	if cfg.Spec.Template != nil {
		for _, ctr := range cfg.Spec.Template.Containers {
			if ctr.Name == "main" {
				p.image = ctr.Image
			}
		}
	}
	return p
}

// modelManifest is the LLMInferenceService setup applies.
func modelManifest(project string, entry catalogModel, preset string, img imagePlan, hw hardwareProfile) map[string]interface{} {
	annotations := map[string]interface{}{
		// The dashboard's llmd deploy wizard writes these three
		// (odh-dashboard rhoai-3.6 packages/llmd-serving deploy.ts
		// assembleLLMInferenceService: model-type "generative" on every
		// LLMInferenceService, embedding ones too). Its gen-ai BFF reads the
		// display name, description and use case (token_k8s_client.go
		// extract*FromLLMInferenceService).
		"openshift.io/display-name": entry.DisplayName,
		"openshift.io/description":  entry.Description,
		"opendatahub.io/model-type": "generative",
		// With the genai-asset label: listed under the dashboard's AI
		// assets, so its Gen AI playground offers the model (as the
		// wizard's applyModelAvailabilityData sets them).
		"opendatahub.io/genai-use-case": entry.UseCase,
	}
	main := map[string]interface{}{
		"name": "main",
		"env": []interface{}{
			map[string]interface{}{"name": "OMP_NUM_THREADS", "value": entry.threads},
			map[string]interface{}{"name": "VLLM_CPU_KVCACHE_SPACE", "value": entry.kvCacheGiB},
			map[string]interface{}{"name": "VLLM_ADDITIONAL_ARGS", "value": entry.Args},
		},
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{"cpu": entry.CPU, "memory": entry.MemoryRequest},
			"limits":   map[string]interface{}{"cpu": entry.CPU, "memory": entry.Memory},
		},
	}
	if hw.Name != "" {
		// As the dashboard's deploy wizard records the profile
		// (applyHardwareProfileConfig): its Deployments page then names it.
		annotations[hardwareProfileNameAnnotation] = hw.Name
		annotations[hardwareProfileNamespaceAnnotation] = hw.Namespace
		annotations[hardwareProfileVersionAnnotation] = hw.ResourceVersion
	}
	if img.Image != "" {
		main["image"] = img.Image
		annotations[modelImageSourceAnnotation] = img.Kind
		if img.Version != "" {
			annotations[modelImageVersionAnnotation] = img.Version
		}
	}
	return map[string]interface{}{
		"apiVersion": llmisvcAPIVersion,
		"kind":       "LLMInferenceService",
		"metadata": map[string]interface{}{
			"name":      entry.Name,
			"namespace": project,
			"labels": map[string]interface{}{
				managedByLabelKey:            managedByLabelValue,
				"opendatahub.io/dashboard":   "true",
				"opendatahub.io/genai-asset": "true",
			},
			"annotations": annotations,
		},
		"spec": map[string]interface{}{
			"baseRefs": []interface{}{map[string]interface{}{"name": preset}},
			// spec.model.name is the model's resource name: the dashboard's
			// playground proxy finds a model by it (deploy.ts), so an
			// HF-style name would make the playground answer 404.
			"model":    map[string]interface{}{"uri": entry.URI, "name": entry.Name},
			"replicas": 1,
			// The default gateway (the inference gateway); no scheduler: one
			// replica needs no endpoint picker.
			"router": map[string]interface{}{"route": map[string]interface{}{}, "gateway": map[string]interface{}{}},
			"template": map[string]interface{}{
				"containers": []interface{}{main},
				// Only env: KServe generates the rest of its storage-initializer.
				"initContainers": []interface{}{map[string]interface{}{
					"name": "storage-initializer",
					"env":  []interface{}{map[string]interface{}{"name": "STORAGE_IGNORE_PATTERNS", "value": storageIgnorePatterns}},
				}},
			},
		},
	}
}

// projectNamespace reads a project for a model: it must exist, not be
// going away, and be a data science project (the dashboard lists models
// only there). It returns a refusal, or "".
func projectNamespace(c *Client, project string) (map[string]string, string, string) {
	body, _, err := c.get("/api/v1/namespaces/" + project)
	if IsK8sError(err, 404) {
		return nil, fmt.Sprintf("Project '%s' does not exist. Choose a data science project, or create one in the dashboard.", project), "validation"
	}
	if err != nil {
		return nil, fmt.Sprintf("Cannot read project '%s': %v.", project, err), errorCodeFromK8sErr(err)
	}
	var ns struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		return nil, fmt.Sprintf("Cannot parse project '%s': %v.", project, err), "internal"
	}
	if ns.Metadata.terminating() || ns.Status.Phase == "Terminating" {
		return nil, fmt.Sprintf("Project '%s' is being deleted.", project), "terminating"
	}
	if ns.Metadata.Labels["opendatahub.io/dashboard"] != "true" {
		return nil, fmt.Sprintf("Namespace '%s' is not a data science project (it lacks the label opendatahub.io/dashboard=true), so the dashboard would not show a model there. Choose a data science project.", project), "validation"
	}
	return ns.Metadata.Labels, "", ""
}

func recordModelsActivity(c *Client, action, detail string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    action,
		Detail:    detail,
		Success:   success,
	})
}

// modelsRun is one streamed Models operation.
type modelsRun struct {
	c        *Client
	emit     func(UpdateStepEvent)
	logs     []string
	activity string
	target   string
}

func (m *modelsRun) step(id, status, msg string) {
	m.logs = append(m.logs, msg)
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (m *modelsRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	m.logs = append(m.logs, msg)
	slog.Warn("models operation stopped", "step", id, "reason", activity, "user", getUser(m.c))
	recordModelsActivity(m.c, m.activity, fmt.Sprintf("%s (%s)", m.target, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: code}, nil
}

// ValidateModelsRequest returns why req cannot run, or "".
func ValidateModelsRequest(req types.ModelsRequest) string {
	switch req.Action {
	case "setup", "teardown":
	default:
		return "action must be setup or teardown"
	}
	if !req.Gateway && req.Project == "" {
		return "nothing to do: name the gateway, a project, or both"
	}
	if req.Project != "" && !ValidModelName(req.Model) {
		return "unknown model"
	}
	if req.ReleasedImage && (req.Action != "setup" || req.Project == "") {
		return "releasedImage needs a model setup"
	}
	return ""
}

// RunModels sets up or deletes the inference gateway and a model, as one
// operation with a streamed step per part: setup "check", "gateway",
// "gateway-ready", "model", "model-ready" (the gateway steps only with
// Gateway, the model steps only with a Project); teardown "model", then
// "gateway". Setup checks everything it writes, and dry-runs every write,
// before it writes anything.
func RunModels(c *Client, req types.ModelsRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	var parts []string
	if req.Project != "" {
		entry, _ := catalogEntry(req.Model)
		parts = append(parts, fmt.Sprintf("model %s in %s", firstNonEmpty(entry.Name, req.Model), req.Project))
	}
	if req.Gateway {
		parts = append(parts, "inference gateway")
	}
	m := &modelsRun{c: c, emit: onStep, activity: req.Action + "-models", target: strings.Join(parts, ", ")}
	if problem := ValidateModelsRequest(req); problem != "" {
		return m.fail("check", problem, "validation", "invalid request")
	}
	if req.Action == "teardown" {
		return m.teardown(req)
	}
	return m.setup(req)
}

func (m *modelsRun) teardown(req types.ModelsRequest) (*types.OperationResponse, error) {
	var done []string
	if req.Project != "" {
		entry, _ := catalogEntry(req.Model)
		m.step("model", "running", fmt.Sprintf("Deleting %s in '%s'...", entry.Name, req.Project))
		res, _ := TeardownModels(m.c, req.Project, entry.Name)
		m.logs = append(m.logs, res.Logs...)
		if !res.Success {
			if m.emit != nil {
				m.emit(UpdateStepEvent{Step: "model", Status: "failed", Message: res.Message, ErrorCode: res.ErrorCode})
			}
			return &types.OperationResponse{Success: false, Message: res.Message, Logs: m.logs, ErrorCode: res.ErrorCode}, nil
		}
		m.step("model", "success", res.Message)
		done = append(done, res.Message)
	}
	if req.Gateway {
		m.step("gateway", "running", "Deleting the inference gateway...")
		res, _ := TeardownInferenceGateway(m.c, req.Project != "")
		m.logs = append(m.logs, res.Logs...)
		if !res.Success {
			if m.emit != nil {
				m.emit(UpdateStepEvent{Step: "gateway", Status: "failed", Message: res.Message, ErrorCode: res.ErrorCode})
			}
			msg := res.Message
			if len(done) > 0 {
				msg = strings.Join(done, " ") + " " + msg
			}
			return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: res.ErrorCode}, nil
		}
		m.step("gateway", "success", res.Message)
		done = append(done, res.Message)
	}
	return &types.OperationResponse{Success: true, Message: strings.Join(done, " "), Logs: m.logs}, nil
}

// modelPlan is what a model setup will write, decided before any write.
type modelPlan struct {
	entry    catalogModel
	project  string
	preset   string
	image    imagePlan
	hardware hardwareProfile
	obj      map[string]interface{}
	current  objectMeta
	found    bool
}

func (m *modelsRun) setup(req types.ModelsRequest) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking what every part needs...")
	var notes []string

	// The gateway part.
	var gw *gatewayPlan
	if req.Gateway {
		plan, msg, code := planInferenceGateway(c)
		if msg != "" {
			return m.fail("check", msg, code, "gateway check failed")
		}
		gw = plan
	}

	// The model part.
	var mp *modelPlan
	if req.Project != "" {
		plan, msg, code := m.planModel(req, gw)
		if msg != "" {
			return m.fail("check", msg, code, "model check failed")
		}
		mp = plan
		if warning := modelCapacityWarning(c, mp.entry, mp.project); warning != "" {
			notes = append(notes, warning+" It may stay pending until a node is added or other workloads stop; the setup still runs.")
		}
	}

	// Every write, as a server-side dry run, before any real one.
	if gw != nil && !gw.foreign {
		if msg, code := applyInferenceGateway(c, gw, true, func(string) {}); msg != "" {
			return m.fail("check", msg, code, "gateway dry run failed")
		}
	}
	if mp != nil {
		if msg, code := m.applyModel(mp, true); msg != "" {
			return m.fail("check", msg, code, "model dry run failed")
		}
	}
	m.step("check", "success", strings.TrimSpace("Everything the parts need is there. "+strings.Join(notes, " ")))

	var summary []string
	if gw != nil {
		if gw.foreign {
			msg := fmt.Sprintf("Gateway %s/%s already exists and was not created by this tool: models use it as it is, and this tool does not change it.", inferenceGatewayNamespace, inferenceGatewayName)
			m.step("gateway", "success", msg)
			m.step("gateway-ready", "success", "Not checked: the existing gateway's health is shown on the Models tab.")
			summary = append(summary, "The existing inference gateway is used.")
		} else {
			m.step("gateway", "running", "Creating the inference gateway...")
			if msg, code := applyInferenceGateway(c, gw, false, func(line string) { m.step("gateway", "running", line) }); msg != "" {
				return m.fail("gateway", msg, code, "gateway apply failed")
			}
			m.step("gateway", "success", fmt.Sprintf("GatewayClass, ConfigMap and Gateway %s/%s applied; external hostname %s.", inferenceGatewayNamespace, inferenceGatewayName, gw.host))
			m.step("gateway-ready", "running", "Waiting for the gateway and its load balancer...")
			ready, dns, problem, pending, err := waitInferenceGateway(c, gw.host, func(line string) { m.step("gateway-ready", "running", line) })
			switch {
			case err != nil:
				return m.fail("gateway-ready", fmt.Sprintf("Stopped waiting for the gateway: %v. It was created; the Models tab shows its state.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "gateway wait failed")
			case problem != "":
				return m.fail("gateway-ready", problem+" Its objects were created; delete the gateway to remove them.", "not_ready", "gateway not accepted")
			case !ready:
				why := ""
				if pending != "" {
					why = " The gateway controller says: " + truncateMessage(pending) + "."
				}
				m.step("gateway-ready", "success", fmt.Sprintf("Created, but not programmed after %s. It may still come up; the Models tab shows its state.%s", clockText(InferenceGatewayReadyTimeout), why))
			case dns == "published":
				m.step("gateway-ready", "success", fmt.Sprintf("Ready: https://%s is published in DNS.", gw.host))
			default:
				m.step("gateway-ready", "success", fmt.Sprintf("Ready; https://%s is not published in DNS yet (a new load balancer's name can take a few minutes to resolve).", gw.host))
			}
			summary = append(summary, "The inference gateway is set up.")
		}
	}

	if mp != nil {
		m.step("model", "running", fmt.Sprintf("Creating %s in '%s' (%s)...", mp.entry.DisplayName, mp.project, mp.image.describe()))
		if msg, code := m.applyModel(mp, false); msg != "" {
			return m.fail("model", msg, code, "model apply failed")
		}
		m.step("model", "success", fmt.Sprintf("LLMInferenceService %s/%s applied, on preset %s with %s.", mp.project, mp.entry.Name, mp.preset, mp.image.describe()))
		m.step("model-ready", "running", "Waiting for the model to start (typically 3 to 5 minutes the first time)...")
		timeout := ModelReadyTimeout
		if dl, ok := c.ctx.Deadline(); ok {
			if left := clk.Until(dl) - 30*time.Second; left < timeout {
				timeout = left
			}
		}
		res := waitModelReady(c, mp.project, mp.entry.Name, mp.image.Kind == "preset", timeout, func(line string) { m.step("model-ready", "running", line) })
		switch {
		case res.err != nil:
			return m.fail("model-ready", fmt.Sprintf("Stopped waiting for the model: %v. It was created; the Models tab shows its state.", res.err), firstNonEmpty(errorCodeFromK8sErr(res.err), "cancelled"), "model wait failed")
		case res.imageMissing:
			return m.fail("model-ready", "The model cannot start: the cluster preset's image is not in the registry (this nightly build's runtime image is not published yet). Use the newest released Red Hat image instead: the Models tab offers it, and it replaces only the model's image. Technical details: "+res.problem, "image_unavailable", "preset image missing")
		case res.problem != "":
			return m.fail("model-ready", "The model cannot start: "+res.problem+" It was created; delete it from the Models tab, or fix the cause and set it up again.", "not_ready", "model not starting")
		case !res.ready:
			m.step("model-ready", "success", fmt.Sprintf("Created, but not ready after %s. It may still start; the Models tab shows its state and the reason if it cannot.", clockText(timeout)))
			summary = append(summary, fmt.Sprintf("The model in %s is still starting.", mp.project))
		default:
			m.step("model-ready", "success", fmt.Sprintf("Ready at %s.", firstNonEmpty(res.url, "its address")))
			summary = append(summary, fmt.Sprintf("%s is ready in %s.", mp.entry.DisplayName, mp.project))
		}
	}
	slog.Info("models setup", "target", m.target, "user", getUser(c))
	recordModelsActivity(c, "setup-models", m.target, true)
	return &types.OperationResponse{Success: true, Message: strings.Join(summary, " "), Logs: m.logs}, nil
}

// planModel checks everything a model setup needs: the project, KServe and
// its LLM dependencies, the API, the CPU preset, the gateway, an existing
// model of that name, and the image. It returns a refusal, or "".
func (m *modelsRun) planModel(req types.ModelsRequest, gw *gatewayPlan) (*modelPlan, string, string) {
	c := m.c
	entry, _ := catalogEntry(req.Model)
	labels, msg, code := projectNamespace(c, req.Project)
	if msg != "" {
		return nil, msg + " Nothing was changed.", code
	}
	if dsc, err := readFirstDSC(c); err == nil {
		block, deps := modelsBlock(dsc)
		if block != nil {
			return nil, block.Reason + " Nothing was changed.", "prerequisites"
		}
		if deps == "" {
			m.step("check", "running", fmt.Sprintf("The DataScienceCluster does not report %s; continuing (unknown is not a failure).", llmDependenciesCondition))
		}
	}
	if v := servedVersions(c, llmisvcGroup, nil); v.Discovered && !containsString(v.Versions, "v1alpha2") {
		return nil, fmt.Sprintf("This cluster's KServe does not serve LLMInferenceService %s (served: %s), which is the shape this tool writes. Nothing was changed.", llmisvcAPIVersion, strings.Join(v.Versions, ", ")), "prerequisites"
	}
	ns := appsNamespace(c)
	preset := discoverCPUPreset(c, ns)
	if preset.problem != "" {
		return nil, preset.problem + " Nothing was changed.", "prerequisites"
	}

	// The model's route is KServe's default gateway (spec.router.gateway
	// {}), read at run time: it must be the gateway this tool manages.
	def, err := readKServeGateway(c)
	if err != nil {
		m.step("check", "running", fmt.Sprintf("Cannot read KServe's default gateway (%v); assuming %s/%s, as on RHOAI 3.x.", err, inferenceGatewayNamespace, inferenceGatewayName))
	}
	if msg := defaultGatewayElsewhere(def); msg != "" {
		return nil, msg + " This tool sets up models only behind its own gateway. Nothing was changed.", "prerequisites"
	}

	// The gateway the model will route through.
	switch {
	case gw != nil && gw.foreign:
		if gw.gateway.admits(req.Project, labels) == "no" {
			return nil, fmt.Sprintf("Gateway %s/%s, which this tool did not create, does not let routes from project '%s' attach (its listeners' allowedRoutes). Nothing was changed; ask its owner.", inferenceGatewayNamespace, inferenceGatewayName, req.Project), "prerequisites"
		}
	case gw == nil:
		existing, found, err := readInferenceGateway(c)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read the inference gateway: %v. Nothing was changed.%s", err, templateHint(err)), errorCodeFromK8sErr(err)
		}
		if !found {
			return nil, "The model needs the inference gateway, which is not set up. Set it up first (the Models tab's Set up does both). Nothing was changed.", "prerequisites"
		}
		if existing.Metadata.terminating() {
			return nil, "The inference gateway is being deleted. Set it up again first. Nothing was changed.", "terminating"
		}
		switch existing.admits(req.Project, labels) {
		case "no":
			return nil, fmt.Sprintf("Gateway %s/%s does not let routes from project '%s' attach (its listeners' allowedRoutes). Nothing was changed.", inferenceGatewayNamespace, inferenceGatewayName, req.Project), "prerequisites"
		case "unknown":
			m.step("check", "running", "The gateway selects namespaces with expressions this tool does not evaluate; check that it admits this project if the model does not become ready.")
		}
	}

	existing, found, err := readToolModel(c, req.Project, entry.Name)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read LLMInferenceService %s/%s: %v. Nothing was changed.%s", req.Project, entry.Name, err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	if found && !modelOwned(existing.Metadata) {
		return nil, fmt.Sprintf("Project '%s' already has an LLMInferenceService named %s that this tool did not create. It was left unchanged.", req.Project, entry.Name), "not_managed"
	}
	if found && existing.Metadata.terminating() {
		return nil, fmt.Sprintf("The model %s in '%s' is still being deleted. Wait until it is gone, then set it up again.", entry.Name, req.Project), "terminating"
	}
	var prev *llmisvcObject
	if found {
		prev = &existing
	}
	img, err := planModelImage(c, preset.image, req.ReleasedImage, prev, true)
	if err != nil {
		return nil, err.Error() + " Nothing was changed.", "prerequisites"
	}
	if img.Reason != "" {
		m.step("check", "running", "Image: "+img.Reason)
	}
	hw, why := pickHardwareProfile(c, entry)
	if hw.Name != "" {
		m.step("check", "running", fmt.Sprintf("Hardware profile: %s/%s, as the dashboard records it.", hw.Namespace, hw.Name))
	} else {
		m.step("check", "running", "Hardware profile: none; the dashboard shows the model without one, because "+why+".")
	}
	return &modelPlan{entry: entry, project: req.Project, preset: preset.name, image: img, hardware: hw,
		obj: modelManifest(req.Project, entry, preset.name, img, hw), current: existing.Metadata, found: found}, "", ""
}

// applyModel writes the model (a server-side dry run when dryRun).
func (m *modelsRun) applyModel(p *modelPlan, dryRun bool) (string, string) {
	c := m.c
	path, _ := toolModelPath(p.project, p.entry.Name)
	createPath := withToolFieldManager(namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", p.project, ""))
	project, name := p.project, p.entry.Name
	read := func() (objectMeta, bool, error) {
		obj, found, err := readToolModel(c, project, name)
		return obj.Metadata, found, err
	}
	desc := fmt.Sprintf("LLMInferenceService %s/%s", p.project, p.entry.Name)
	_, meta, found, err := ensureToolObject(c, desc, path, createPath, p.obj, p.current, p.found, read, modelOwned, dryRun)
	p.current, p.found = meta, found
	if err == nil {
		return "", ""
	}
	code := errorCodeFromK8sErr(err)
	var foreign *foreignObjectError
	switch {
	case errors.As(err, &foreign):
		code = "not_managed"
	case errors.Is(err, errObjectChanged) || IsK8sError(err, 409):
		code = "conflict"
	}
	if dryRun {
		return fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", desc, err, templateHint(err)), code
	}
	return fmt.Sprintf("Failed to apply %s: %v. Set up again to retry.%s", desc, err, templateHint(err)), code
}

// TeardownModels deletes the catalog models named (every catalog model
// when none is) that this tool created in project, and waits (bounded)
// until KServe's finalizer has removed them, so the gateway they used can
// go next. Models of the same name it did not create are left alone.
func TeardownModels(c *Client, project string, names ...string) (*types.OperationResponse, error) {
	logs := []string{}
	detail := "project=" + project
	if len(names) > 0 {
		detail += " model=" + strings.Join(names, ",")
	}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordModelsActivity(c, "teardown-model", fmt.Sprintf("%s (%s)", detail, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	var entries []catalogModel
	for _, entry := range modelCatalog {
		if len(names) == 0 || containsString(names, entry.Name) {
			entries = append(entries, entry)
		}
	}
	// A published model is unpublished first (MaaS tab): its MaaSModelRef
	// and access entries would point at nothing.
	for _, entry := range entries {
		if published, err := MaaSPublishedIn(c, project, entry.Name); err != nil {
			return fail(fmt.Sprintf("Cannot check whether %s in '%s' is published to MaaS; nothing was deleted: %v.", entry.Name, project, err), errorCodeFromK8sErr(err), "maas check failed")
		} else if published {
			return fail(fmt.Sprintf("%s in '%s' is published to Models as a Service. Unpublish it on the MaaS tab first; nothing was deleted.", entry.Name, project), "in_use", "published to maas")
		}
	}
	var deleted, kept []string
	for _, entry := range entries {
		model, found, err := readToolModel(c, project, entry.Name)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read LLMInferenceService %s/%s; nothing was deleted: %v.%s", project, entry.Name, err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
		}
		if !found {
			continue
		}
		if !modelOwned(model.Metadata) {
			kept = append(kept, entry.Name)
			continue
		}
		path, _ := toolModelPath(project, entry.Name)
		if !model.Metadata.terminating() {
			logs = append(logs, fmt.Sprintf("Deleting LLMInferenceService %s/%s...", project, entry.Name))
			if _, err := deleteWithUID(c, path, model.Metadata.UID); err != nil && !IsK8sError(err, 404) {
				code := errorCodeFromK8sErr(err)
				if IsK8sError(err, 409) {
					code = "conflict"
				}
				return fail(fmt.Sprintf("Failed to delete LLMInferenceService %s/%s: %v.%s", project, entry.Name, err, templateHint(err)), code, "delete failed")
			}
		}
		gone, err := waitForDeletion(c, path, ModelDeleteTimeout, ModelDeletePoll)
		if err != nil {
			return fail(fmt.Sprintf("The model %s/%s is being deleted, but whether it is gone cannot be checked: %v.", project, entry.Name, err), firstNonEmpty(errorCodeFromK8sErr(err), "in_progress"), "wait failed")
		}
		if !gone {
			note := ""
			if latest, ok, err := readToolModel(c, project, entry.Name); err == nil && ok && len(latest.Metadata.Finalizers) > 0 {
				note = fmt.Sprintf(" It waits on %s %s, which KServe's LLMInferenceService controller removes once it has deleted the model's Deployment, Service and route; that controller must be running.", verb(len(latest.Metadata.Finalizers), "finalizer", "finalizers"), strings.Join(latest.Metadata.Finalizers, ", "))
			}
			return fail(fmt.Sprintf("The model %s/%s is still being deleted after %s.%s Delete again to check.", project, entry.Name, clockText(ModelDeleteTimeout), note), "in_progress", "still deleting")
		}
		deleted = append(deleted, entry.Name)
		logs = append(logs, fmt.Sprintf("OK: LLMInferenceService %s/%s deleted", project, entry.Name))
	}
	if len(deleted) == 0 && len(kept) > 0 {
		return fail(fmt.Sprintf("The model %s in '%s' was not created by this tool, so it was not deleted.", strings.Join(kept, ", "), project), "not_managed", "not managed")
	}
	recordModelsActivity(c, "teardown-model", detail, true)
	if len(deleted) == 0 {
		what := "No model of this tool"
		if len(names) > 0 {
			what = "No " + strings.Join(names, ", ") + " of this tool"
		}
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("%s in '%s'; nothing to delete.", what, project), Logs: logs}, nil
	}
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Model %s deleted from '%s'; KServe removed its Deployment, Service and route. The project stays.", strings.Join(deleted, ", "), project), Logs: logs}, nil
}

// clockText is a duration as m:ss.
func clockText(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// modelPod is the part of a pod the model wait reads.
type modelPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Containers []modelContainerSpec `json:"containers"`
	} `json:"spec"`
	Status struct {
		Conditions            []k8sCondition       `json:"conditions"`
		InitContainerStatuses []modelContainerStat `json:"initContainerStatuses"`
		ContainerStatuses     []modelContainerStat `json:"containerStatuses"`
	} `json:"status"`
}

// modelContainerSpec is a container as a pod or pod template declares it.
type modelContainerSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type modelContainerStat struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Ready bool   `json:"ready"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Running *struct{} `json:"running"`
	} `json:"state"`
	LastState struct {
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminated"`
	} `json:"lastState"`
}

// modelEvent is the part of an event the model wait reads.
type modelEvent struct {
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Type           string `json:"type"`
	LastTimestamp  string `json:"lastTimestamp"`
	EventTime      string `json:"eventTime"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
}

func (e modelEvent) when() string { return firstNonEmpty(e.LastTimestamp, e.EventTime) }

// modelProgress describes where a starting model is, in words, from its
// pods and their newest event: waiting for a node, pulling, downloading
// the model, starting vLLM. problem is set when it cannot start without a
// change (a pull that keeps failing, a crash loop); imageMissing when the
// registry does not have the image.
type modelProgress struct {
	message, problem string
	imageMissing     bool
}

// readModelProgress reads the progress of the model's pods of the current
// revision (w.current): a pod of the previous one, still failing or still
// serving during a change, says nothing about the change.
func readModelProgress(c *Client, project, name string, w modelWorkload) (modelProgress, error) {
	body, _, err := c.get(namespacedPath("v1", "pods", project, "") + "?labelSelector=" + url.QueryEscape(modelPodSelector(name)))
	if err != nil {
		return modelProgress{}, err
	}
	var pods struct {
		Items []modelPod `json:"items"`
	}
	if err := json.Unmarshal(body, &pods); err != nil {
		return modelProgress{}, fmt.Errorf("parse pod list: %w", err)
	}
	var current []modelPod
	names := map[string]bool{}
	for _, p := range pods.Items {
		if w.current(p) {
			current = append(current, p)
			names[p.Metadata.Name] = true
		}
	}
	var newest *modelEvent
	if len(names) > 0 {
		if body, _, err := c.get(namespacedPath("v1", "events", project, "") + "?fieldSelector=" + url.QueryEscape("involvedObject.kind=Pod")); err == nil {
			var events struct {
				Items []modelEvent `json:"items"`
			}
			if json.Unmarshal(body, &events) == nil {
				for i, e := range events.Items {
					if e.InvolvedObject.Kind == "Pod" && names[e.InvolvedObject.Name] && (newest == nil || e.when() > newest.when()) {
						newest = &events.Items[i]
					}
				}
			}
		}
	}
	return describeModelPods(current, newest), nil
}

// describeModelPods turns pods and their newest event into progress. Every
// live pod is judged: the first problem found is the result, else the
// first pod's progress.
func describeModelPods(pods []modelPod, newest *modelEvent) modelProgress {
	event := ""
	if newest != nil && newest.Reason != "" {
		event = fmt.Sprintf(" (%s: %s)", newest.Reason, truncateMessage(newest.Message))
	}
	var first *modelProgress
	for _, p := range pods {
		if p.Metadata.terminating() {
			continue
		}
		pr := describeModelPod(p, event)
		if pr.problem != "" {
			return pr
		}
		if first == nil {
			first = &pr
		}
	}
	if first == nil {
		return modelProgress{message: "Waiting for KServe to create the model's pod..." + event}
	}
	return *first
}

// describeModelPod is one live pod's progress; event is its newest event, in words.
func describeModelPod(p modelPod, event string) modelProgress {
	if cond, ok := findCondition(p.Status.Conditions, "PodScheduled"); ok && cond.Status == "False" {
		return modelProgress{message: fmt.Sprintf("Waiting for a worker with room for the model: %s", firstNonEmpty(truncateMessage(cond.Message), cond.Reason))}
	}
	check := func(s modelContainerStat, init bool) (modelProgress, bool) {
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff":
				if manifestUnknown(w.Message) {
					return modelProgress{problem: fmt.Sprintf("%s: %s", w.Reason, truncateMessage(w.Message)), imageMissing: true}, true
				}
				if w.Reason == "ImagePullBackOff" {
					return modelProgress{problem: fmt.Sprintf("the image of container %s cannot be pulled (%s: %s). Check that the cluster's pull secret can read it.", s.Name, w.Reason, truncateMessage(w.Message))}, true
				}
				return modelProgress{message: fmt.Sprintf("Pulling the image of %s failed once; retrying (%s)...", s.Name, truncateMessage(w.Message))}, true
			case "CrashLoopBackOff":
				last := ""
				if t := s.LastState.Terminated; t != nil {
					last = fmt.Sprintf(" Last exit: %s, code %d.", t.Reason, t.ExitCode)
					if t.Reason == "OOMKilled" {
						last += " It ran out of memory."
					}
				}
				return modelProgress{problem: fmt.Sprintf("container %s keeps crashing.%s Its logs say why.", s.Name, last)}, true
			case "CreateContainerConfigError", "CreateContainerError", "InvalidImageName":
				return modelProgress{problem: fmt.Sprintf("container %s cannot be created (%s: %s).", s.Name, w.Reason, truncateMessage(w.Message))}, true
			case "ContainerCreating", "PodInitializing":
				if init {
					return modelProgress{}, false
				}
				return modelProgress{message: "Pulling the runtime image (several GB; a few minutes the first time on a node)..." + event}, true
			}
		}
		if init && s.State.Running != nil {
			return modelProgress{message: "Downloading the model from Hugging Face (storage-initializer)..." + event}, true
		}
		return modelProgress{}, false
	}
	for _, s := range p.Status.InitContainerStatuses {
		if pr, ok := check(s, true); ok {
			return pr
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if pr, ok := check(s, false); ok {
			return pr
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == "main" && s.State.Running != nil && !s.Ready {
			msg := "Starting vLLM and loading the model..."
			if t := s.LastState.Terminated; t != nil && t.Reason == "OOMKilled" {
				msg = "Restarted after running out of memory (OOMKilled); starting vLLM again..."
			}
			return modelProgress{message: msg}
		}
	}
	return modelProgress{message: "Starting..." + event}
}

type modelWait struct {
	ready        bool
	url          string
	problem      string
	imageMissing bool
	err          error
}

// modelWorkload is the model's workload Deployments (found by the labels
// its pods carry) as the model wait reads them.
type modelWorkload struct {
	// observed: there is one, and each one's controller saw its latest
	// spec, so its pod template is the current revision's.
	observed bool
	// images are the main container images of the current pod templates.
	images map[string]bool
	// rolledOut, as "oc rollout status" decides: observed, and every
	// replica is updated and available, with no old one left.
	rolledOut bool
	// message says, in words, what is awaited when not rolled out.
	message string
}

// current says whether a pod is of the current revision: its main image
// is a current template's. Before the change is observed, no pod is.
func (w modelWorkload) current(p modelPod) bool {
	if !w.observed {
		return false
	}
	if len(w.images) == 0 {
		return true
	}
	for _, c := range p.Spec.Containers {
		if c.Name == "main" {
			return w.images[c.Image]
		}
	}
	return false
}

// readModelWorkload reads the model's workload Deployments.
func readModelWorkload(c *Client, project, name string) (modelWorkload, error) {
	q := url.Values{"labelSelector": {modelPodSelector(name)}}
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", project, "") + "?" + q.Encode())
	if err != nil {
		return modelWorkload{}, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Replicas *int32 `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []modelContainerSpec `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				Replicas           int32 `json:"replicas"`
				UpdatedReplicas    int32 `json:"updatedReplicas"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return modelWorkload{}, fmt.Errorf("parse the model's Deployments: %w", err)
	}
	if len(list.Items) == 0 {
		return modelWorkload{message: "Waiting for KServe to create the model's Deployment..."}, nil
	}
	w := modelWorkload{observed: true, rolledOut: true, images: map[string]bool{}}
	for _, d := range list.Items {
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		st := d.Status
		if st.ObservedGeneration < d.Metadata.Generation {
			return modelWorkload{message: "Waiting for KServe to apply the change to the model's Deployment..."}, nil
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "main" && c.Image != "" {
				w.images[c.Image] = true
			}
		}
		if st.UpdatedReplicas < want || st.Replicas > st.UpdatedReplicas || st.AvailableReplicas < st.UpdatedReplicas {
			w.rolledOut = false
			w.message = "Rolling out the change: the new pod is starting, and the previous one serves until it is ready..."
		}
	}
	return w, nil
}

// waitModelReady waits (bounded) until the model reports Ready, reporting
// progress from its conditions, pods and events. A pull of the preset's
// image that the registry does not have stops the wait at once
// (imageMissing): retrying cannot help.
func waitModelReady(c *Client, project, name string, presetImage bool, timeout time.Duration, progress func(string)) modelWait {
	var res modelWait
	last := ""
	say := func(msg string) {
		if msg != "" && msg != last {
			last = msg
			progress(msg)
		}
	}
	ready, err := waitForGone(c, timeout, ModelPoll, func(cc *Client) (bool, error) {
		model, found, err := readToolModel(cc, project, name)
		if err != nil {
			return false, err
		}
		if !found {
			res.problem = "the model was deleted while it was starting."
			return true, nil
		}
		// Until KServe has seen the spec just applied, its conditions and
		// the pods are the previous revision's.
		if model.Status.ObservedGeneration < model.Metadata.Generation {
			say("Waiting for KServe to apply the change...")
			return false, nil
		}
		if cond, ok := findCondition(model.Status.Conditions, "PresetsCombined"); ok && cond.Status == "False" {
			res.problem = fmt.Sprintf("KServe cannot resolve its preset: %s.", firstNonEmpty(cond.Message, cond.Reason))
			return true, nil
		}
		w, err := readModelWorkload(cc, project, name)
		if err != nil {
			return false, err
		}
		// Ready counts only once the workload has rolled out: after a
		// change, KServe reports Ready while the old pod still serves and
		// the new one starts (seen live).
		cond, ok := findCondition(model.Status.Conditions, "Ready")
		ready := ok && cond.Status == "True"
		if ready && w.rolledOut {
			res.url = model.Status.URL
			return true, nil
		}
		if !w.observed {
			// Every pod is the previous revision's: their problems are not
			// this change's.
			say(w.message)
			return false, nil
		}
		// Only the current revision's pods count, in both cases: an old pod
		// still failing does not fail the change, and a new pod failing
		// while the old one keeps the model Ready does.
		pr, err := readModelProgress(cc, project, name, w)
		if err != nil {
			return false, err
		}
		if pr.problem != "" {
			res.problem = pr.problem
			res.imageMissing = pr.imageMissing && presetImage
			return true, nil
		}
		if ready {
			say(w.message)
		} else {
			say(pr.message)
		}
		return false, nil
	})
	res.err = err
	res.ready = ready && res.problem == ""
	return res
}
