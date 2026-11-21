package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
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
// The gateway's AuthPolicy accepts any token whose user may "get" the
// model. Each model also gets the dashboard's token authentication (a
// ServiceAccount that may get only it, and its token; models_token.go), so
// the dashboard lists a token and its Edit keeps authentication on.
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
	// ephemeralStorage is the main container's ephemeral-storage request,
	// about 1.5 times DownloadSize: with KServe's default model volume the
	// storage-initializer downloads into an emptyDir on the node's disk
	// (models_storage.go).
	ephemeralStorage string
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
// shell quoting in them is honoured: Qwen3's JSON is one argument. No
// argument holds a space, quoted or not: the dashboard's Edit splits
// VLLM_ADDITIONAL_ARGS on spaces and writes the parts back joined by one
// (odh-dashboard rhoai-3.6 packages/llmd-serving/src/deployments/model.ts
// extractRuntimeArgs L147-159), and would list a spaced JSON as two.
//
// Memory (requests and limits below, in one place), measured live on
// RHOAI 3.6 with vLLM CPU 0.24 on 2026-11-18: the main container's memory
// (oc adm top pod --containers), idle after start with the KV cache
// already reserved. SmolLM2 3364Mi, Qwen2.5 5625Mi, Qwen3 11590Mi,
// Granite embedding 2375Mi. Each request is 0.5-0.7 GiB above that, each
// limit 1.7-2.7 GiB above it, for load. Qwen2.5 under load: 7.6 GiB with
// the preset's 4 GiB KV cache (cgroup memory.peak 7.73 GiB over four
// parallel 600-token chats), 5.8 GiB with the 2 GiB cache it has, which
// still holds over 100k tokens for it (24 layers x 2 KV heads x 64 x 2 x 2
// bytes = 12 KiB per token). SmolLM2, Qwen2.5 and Granite fit the
// dashboard's default hardware profile (up to 4 CPUs and 8 GiB on 3.6);
// Qwen3 fits none.
//
// DownloadSize is what the storage-initializer fetches at the pinned
// commit with storageIgnorePatterns applied (Hugging Face API file sizes).
var modelCatalog = []catalogModel{
	{
		Name: "smollm2-135m-instruct", DisplayName: "SmolLM2 135M Instruct (CPU)", URI: "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac",
		CPU: "2", MemoryRequest: "4Gi", Memory: "5Gi", Type: modelTypeChat, UseCase: "Chat (smoke test)", DownloadSize: "270 MB",
		Description: "A very small chat model, for checking that model serving works. From Hugging Face (HuggingFaceTB/SmolLM2-135M-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
		Args:        "--max-model-len 4096",
		threads:     "2", kvCacheGiB: "1", ephemeralStorage: "400M",
	},
	{
		Name: "qwen25-05b-instruct", DisplayName: "Qwen2.5 0.5B Instruct (CPU)", URI: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775",
		CPU: "4", MemoryRequest: "6Gi", Memory: "8Gi", Type: modelTypeChat, UseCase: "Chat", DownloadSize: "1.0 GB", Default: true,
		Description: "A small chat model. From Hugging Face (Qwen/Qwen2.5-0.5B-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
		Args:        "--max-model-len 4096",
		threads:     "4", kvCacheGiB: "2", ephemeralStorage: "1.5G",
	},
	{
		// Thinking is off by default (the user's choice); a request turns
		// it on with chat_template_kwargs {"enable_thinking": true}.
		Name: "qwen3-1-7b", DisplayName: "Qwen3 1.7B (CPU, tool calling)", URI: "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e",
		CPU: "4", MemoryRequest: "12Gi", Memory: "14Gi", Type: modelTypeChat, ToolCalling: true, UseCase: "Chat, Tool calling, Reasoning", DownloadSize: "4.1 GB",
		Description: "A chat model that can call tools; its reasoning (thinking) is off unless a request turns it on. From Hugging Face (Qwen/Qwen3-1.7B), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
		Args:        `--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking":false}'`,
		threads:     "4", kvCacheGiB: "2", ephemeralStorage: "6.2G",
	},
	{
		// A RobertaModel, which vLLM serves as an embedding model with the
		// pooling runner (vLLM 0.24 removed --task); 512 positions.
		Name: "granite-embedding-125m-english", DisplayName: "Granite Embedding 125M English (CPU)", URI: "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f",
		CPU: "2", MemoryRequest: "3Gi", Memory: "4Gi", Type: modelTypeEmbedding, EmbeddingDimension: 768, UseCase: "Embeddings, Semantic search", DownloadSize: "250 MB",
		Description: "An English embedding model (768 dimensions) for semantic search. From Hugging Face (ibm-granite/granite-embedding-125m-english), served by vLLM on CPU. Set up by rhoai-nightly-updater.",
		Args:        "--runner pooling --max-model-len 512",
		threads:     "2", kvCacheGiB: "1", ephemeralStorage: "375M",
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
			// Route is spec.router.route: present ({}), KServe makes an
			// HTTPRoute; absent, the model is cluster-internal only.
			Route   *json.RawMessage `json:"route"`
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
		URL       string `json:"url"`
		Addresses []struct {
			URL string `json:"url"`
		} `json:"addresses"`
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

func listLLMInferenceServices(c *Client) ([]llmisvcObject, error) {
	body, _, err := c.get(clusterPath("serving.kserve.io/v1alpha2", "llminferenceservices", ""))
	if isK8sNotFound(err) {
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
	if isK8sNotFound(err) {
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
// published), as opposed to a credential or network problem. A pull through
// an image mirror names every source it tried: registry.redhat.io answers
// "manifest unknown" for an image that exists only in the quay.io/rhoai
// mirror, so when any source refused the credentials the image may well
// exist and the node only lacks the pull secret (a node that just joined
// gets it a few minutes later on ROSA HCP, seen live); that is a pull
// failure to wait on, not a missing image.
func manifestUnknown(msg string) bool {
	m := strings.ToLower(msg)
	if !strings.Contains(m, "manifest unknown") && !strings.Contains(m, "manifest_unknown") {
		return false
	}
	for _, refused := range []string{"unauthorized", "not authorized", "authentication required", "denied"} {
		if strings.Contains(m, refused) {
			return false
		}
	}
	return true
}

// manifestUnknownMentioned: some source of the pull answered that the image
// does not exist, though another may have refused the credentials (which
// manifestUnknown waits on). Once that pull has failed for the whole grace
// window, the image may be missing after all, so the released image is
// offered again, worded as a possibility.
func manifestUnknownMentioned(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "manifest unknown") || strings.Contains(m, "manifest_unknown")
}

// olderThan: the object was created more than d ago (false when its
// creation time cannot be read).
func olderThan(meta objectMeta, d time.Duration) bool {
	t, err := time.Parse(time.RFC3339, meta.CreationTimestamp)
	return err == nil && time.Since(t) > d
}

// modelState reports one model. inspect reads its pods when it is not
// ready. rec is the tool's record of its models (nil: it could not be
// read, so no model counts as the tool's).
func modelState(c *Client, m llmisvcObject, inspect bool, rec modelsRecord) types.ModelState {
	s := types.ModelState{Deployed: true, Namespace: m.Metadata.Namespace, Name: m.Metadata.Name}
	s.ManagedByTool = rec.owns(m.Metadata)
	if s.ManagedByTool && rec[modelRecordKey(m.Metadata.Namespace, m.Metadata.Name)].TokenSecretUID != "" {
		s.TokenSecret, s.TokenServiceAccount = tokenSecretName(m.Metadata.Name), tokenSAName(m.Metadata.Name)
	}
	s.TokenAuthOff = m.Metadata.Annotations[enableAuthAnnot] == "false"
	s.Terminating = m.Metadata.terminating()
	s.DisplayName = m.Metadata.Annotations["openshift.io/display-name"]
	if m.Status.URL != "" {
		if snippetURL(m.Status.URL) {
			s.URL = m.Status.URL
		} else {
			s.Warning = unsafeAddressWarning("model address")
		}
	}
	s.MaaSPublished = modelHasMaaSRef(m)
	s.CurrentImage = m.mainImage()
	s.ImageKind = firstNonEmpty(m.Metadata.Annotations[modelImageSourceAnnotation], "preset")
	s.ImageVersion = m.Metadata.Annotations[modelImageVersionAnnotation]
	if name := m.Metadata.Annotations[hardwareProfileNameAnnotation]; name != "" {
		s.HardwareProfile = m.Metadata.Annotations[hardwareProfileNamespaceAnnotation] + "/" + name
	}
	switch {
	case rec == nil:
		s.TeardownBlockedReason = "This tool's record of the models it created cannot be read, so it does not change or delete this one."
	case !s.ManagedByTool:
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
		s.Message = firstNonEmpty(truncateText(presets.Message, 300), presets.Reason, "Its preset cannot be resolved")
		s.WaitingReason = firstNonEmpty(presets.Reason, "PresetsCombined")
		s.TerminalError = true
		return s
	case hasReady:
		s.Message = firstNonEmpty(truncateText(ready.Message, 300), ready.Reason, "Not ready")
	case s.ManagedByTool:
		s.Message = "Starting"
	default:
		s.Message = "Reports no Ready condition"
		s.HealthUnknown = true
	}
	if inspect {
		if issue, err := inspectPods(c, m.Metadata.Namespace, modelPodSelector(m.Metadata.Name), ""); err == nil {
			applyPodIssue(&s.ResourceState, issue)
			if s.ImageKind == "preset" && (issue.Reason == "ErrImagePull" || issue.Reason == "ImagePullBackOff") {
				switch {
				case manifestUnknown(issue.Message):
					s.PresetImageMissing = true
					s.TerminalError = true
				case manifestUnknownMentioned(issue.Message) && olderThan(m.Metadata, ModelImagePullGraceYoungNode):
					// Past the longest grace window a setup waits on.
					s.PresetImageMissing, s.PresetImageUncertain = true, true
				}
			}
		}
	}
	return s
}

// modelItems are the catalog's models in the visible projects.
func modelItems(c *Client, models []llmisvcObject, visible map[string]bool, rec modelsRecord) []types.ModelState {
	var out []types.ModelState
	def, _ := readKServeGateway(c)
	auth := map[string]string{}
	// The MaaS record, read once for the first model of this tool: which
	// models were set up through MaaS only (unreadable: none is said to be).
	var maas *maasRecord
	maasErr := false
	for _, m := range models {
		if !visible[m.Metadata.Namespace] {
			continue
		}
		if _, ok := toolModelPath(m.Metadata.Namespace, m.Metadata.Name); !ok {
			continue
		}
		s := modelState(c, m, true, rec)
		addModelRouting(c, &s, m, def, auth)
		if s.ManagedByTool {
			if maas == nil {
				r, err := readMaaSRecord(c)
				if err != nil {
					slog.Debug("cannot read the MaaS record for the models", "error", err)
					maasErr = true
				}
				maas = &r
			}
			if !maasErr {
				refs, ok := maas.routeBefore(m.Metadata.Namespace, m.Metadata.Name, modelGatewayRefsOf(m))
				s.MaaSOnlySetup = ok && refs.NoRoute
			}
		}
		out = append(out, s)
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
func llmDependencies(dsc map[string]any) (string, string) {
	status, _ := dsc["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, raw := range conds {
		cond, _ := raw.(map[string]any)
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
	// Connectivity Link alone is not a block: whether a model needs it
	// depends on how it is reached (models_exposure.go), and the setup of
	// a model that needs it says so.
	if deps == "False" && onlyConnectivityLinkMissing(msg) {
		return nil, deps
	}
	if deps == "False" {
		meta, _ := dsc.Object["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		return &types.ComponentBlock{Component: "kserve",
			Reason: fmt.Sprintf("KServe cannot serve LLMInferenceServices yet: DataScienceCluster %s reports %s=False (%s). A cluster-admin installs what it names; the Components page shows the details.", name, llmDependenciesCondition, firstNonEmpty(msg, "no message"))}, deps
	}
	return nil, deps
}

// cpuPreset is the CPU preset as found at run time.
type cpuPreset struct {
	name, image, problem string
}

// discoverCPUPreset finds the CPU preset: the controller's config prefix
// (Deployment llmisvc-controller-manager, env
// LLM_INFERENCE_SERVICE_CONFIG_PREFIX, "v3-6-0-kserve-" on RHOAI 3.6)
// followed by config-llm-template-cpu, in the applications namespace, and
// its main container's image. An unknown applications namespace is a
// problem: the preset cannot be found then.
func discoverCPUPreset(c *Client) cpuPreset {
	ns, err := appsNamespace(c)
	if err != nil {
		return cpuPreset{problem: fmt.Sprintf("Cannot tell which namespace RHOAI's applications (KServe's controller and presets) run in: %v.", err)}
	}
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", ns, "llmisvc-controller-manager"))
	if isK8sNotFound(err) {
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
	if isK8sNotFound(err) {
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
//
// published: the model is published to MaaS, which took it out of Gen AI
// studio's AI assets (maas_genai.go), so the genai label and annotation
// are left out until Unpublish puts them back.
//
// exp is how the model is reached (models_exposure.go): its spec.router,
// and token authentication turned off for an internal-only model while
// Connectivity Link is missing.
func modelManifest(project string, entry catalogModel, preset string, img imagePlan, hw hardwareProfile, published, ephemeral bool, exp exposurePlan) map[string]any {
	annotations := map[string]any{
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
	main := map[string]any{
		"name": "main",
		"env": []any{
			map[string]any{"name": "OMP_NUM_THREADS", "value": entry.threads},
			map[string]any{"name": "VLLM_CPU_KVCACHE_SPACE", "value": entry.kvCacheGiB},
			map[string]any{"name": "VLLM_ADDITIONAL_ARGS", "value": entry.Args},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": entry.CPU, "memory": entry.MemoryRequest},
			"limits":   map[string]any{"cpu": entry.CPU, "memory": entry.Memory},
		},
	}
	if ephemeral && entry.ephemeralStorage != "" {
		// The download's emptyDir is on the node's disk (models_storage.go):
		// a request, no limit, so the scheduler counts it and nothing is
		// evicted for it.
		main["resources"].(map[string]any)["requests"].(map[string]any)["ephemeral-storage"] = entry.ephemeralStorage
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
	labels := map[string]any{
		managedByLabelKey:          managedByLabelValue,
		"opendatahub.io/dashboard": "true",
		genAIAssetLabel:            "true",
	}
	if published {
		delete(labels, genAIAssetLabel)
		delete(annotations, genAIUseCaseAnnotation)
	}
	if exp.authOff {
		annotations[enableAuthAnnot] = "false"
	}
	spec := map[string]any{
		"baseRefs": []any{map[string]any{"name": preset}},
		// spec.model.name is the model's resource name: the dashboard's
		// playground proxy finds a model by it (deploy.ts), so an
		// HF-style name would make the playground answer 404. It is also
		// the name vLLM serves: a request whose "model" is anything else
		// (the display name, the Hugging Face id) answers 404 NotFoundError.
		"model":    map[string]any{"uri": entry.URI, "name": entry.Name},
		"replicas": 1,
		"template": map[string]any{
			"containers": []any{main},
			// Only env: KServe generates the rest of its storage-initializer.
			"initContainers": []any{map[string]any{
				"name": "storage-initializer",
				"env":  []any{map[string]any{"name": "STORAGE_IGNORE_PATTERNS", "value": storageIgnorePatterns}},
			}},
		},
	}
	if router := exp.router(); router != nil {
		spec["router"] = router
	}
	return map[string]any{
		"apiVersion": llmisvcAPIVersion,
		"kind":       "LLMInferenceService",
		"metadata": map[string]any{
			"name":        entry.Name,
			"namespace":   project,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": spec,
	}
}

// projectNamespace reads a project for a model: it must exist, not be
// going away, and be a data science project (the dashboard lists models
// only there). It returns a refusal, or "".
func projectNamespace(c *Client, project string) (map[string]string, string, string) {
	body, _, err := c.get("/api/v1/namespaces/" + project)
	if isK8sNotFound(err) {
		return nil, fmt.Sprintf("Project %s does not exist. Choose a data science project, or create one in the dashboard.", project), "validation"
	}
	if err != nil {
		return nil, fmt.Sprintf("Cannot read project %s: %v.", project, err), errorCodeFromK8sErr(err)
	}
	var ns struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		return nil, fmt.Sprintf("Cannot parse project %s: %v.", project, err), "internal"
	}
	if ns.Metadata.terminating() || ns.Status.Phase == "Terminating" {
		return nil, fmt.Sprintf("Project %s is being deleted.", project), "terminating"
	}
	if ns.Metadata.Labels["opendatahub.io/dashboard"] != "true" {
		return nil, fmt.Sprintf("Namespace %s is not a data science project (it lacks the label opendatahub.io/dashboard=true), so the dashboard would not show a model there. Choose a data science project.", project), "validation"
	}
	return ns.Metadata.Labels, "", ""
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

// innerSteps streams the steps of an operation run inside this one (a MaaS
// publish or unpublish) as this one's step id, without logging them: the
// inner run's own Logs, appended once it ends, hold them and its other
// lines. Its failure is not streamed; the caller fails step id with it.
func (m *modelsRun) innerSteps(id string) func(UpdateStepEvent) {
	return func(e UpdateStepEvent) {
		if e.Status != "failed" && m.emit != nil {
			m.emit(UpdateStepEvent{Step: id, Status: "running", Message: e.Message})
		}
	}
}

func (m *modelsRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	m.logs = append(m.logs, msg)
	slog.Warn("models operation stopped", "step", id, "reason", activity, "user", getUser(m.c))
	recordActivity(m.c, m.activity, fmt.Sprintf("%s (%s)", m.target, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: code}, nil
}

// ValidateModelsRequest returns why req cannot run, or "".
func ValidateModelsRequest(req types.ModelsRequest) string {
	switch req.Action {
	case "setup", "teardown":
	case "allow", "disallow":
		if req.Project == "" || req.Gateway || req.ReleasedImage || req.Exposure != "" || req.GatewayRef != "" || req.Model != "" {
			return req.Action + " takes a project only"
		}
		if !playgroundProjectRegex.MatchString(req.Project) {
			return "a valid project is required"
		}
		return ""
	case "check":
		if req.Project == "" || req.Gateway || req.ReleasedImage || req.Exposure != "" || req.GatewayRef != "" {
			return "check takes a project and a model only"
		}
		if !ValidModelName(req.Model) {
			return "unknown model"
		}
		return ""
	default:
		return "action must be setup, teardown, allow, disallow or check"
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
	if !ValidExposure(req.Exposure) {
		return "exposure must be default-gateway, maas, gateway or internal"
	}
	if req.Exposure != "" && (req.Action != "setup" || req.Project == "") {
		return "exposure needs a model setup"
	}
	if (req.Exposure == exposureGateway) != (req.GatewayRef != "") {
		return "gatewayRef goes with exposure gateway, as namespace/name"
	}
	if req.GatewayRef != "" {
		ns, name, ok := strings.Cut(req.GatewayRef, "/")
		if !ok || !playgroundProjectRegex.MatchString(ns) || !gatewayNameRegex.MatchString(name) {
			return "gatewayRef must be namespace/name"
		}
	}
	if req.Gateway && req.Exposure != "" && req.Exposure != exposureDefault {
		return "the inference gateway set up with a model is its default route: set it up on its own for another exposure"
	}
	return ""
}

// RunModels sets up or deletes the inference gateway and a model, as one
// operation with a streamed step per part: setup "check", "gateway",
// "gateway-ready", "model", "model-ready", "publish" (a model reached
// through MaaS), "callable" (the gateway steps only with Gateway, the
// model steps only with a Project); teardown "unpublish" (a model set up
// through MaaS only), "model", then "gateway";
// check "callable". Setup checks everything it writes, and dry-runs every
// write, before it writes anything.
func RunModels(c *Client, req types.ModelsRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	var parts []string
	if req.Action == "allow" || req.Action == "disallow" {
		parts = append(parts, fmt.Sprintf("project %s on the inference gateway", req.Project))
	} else if req.Project != "" {
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
	// Projects of MaaS publications that are gone leave the MaaS gateway
	// (maas_prune.go).
	m.logs = append(m.logs, releaseGoneMaaSProjects(c)...)
	switch req.Action {
	case "teardown":
		return m.teardown(req)
	case "check":
		return m.check(req)
	case "disallow":
		return m.disallow(req.Project)
	}
	var res *types.OperationResponse
	var err error
	if req.Action == "allow" {
		res, err = m.allow(req.Project)
	} else {
		res, err = m.setup(req)
	}
	// After a change that succeeded (the teardown does it itself): projects
	// this tool added to the gateway's list that were deleted since go off
	// it. Never before or after a refusal, which says nothing was changed.
	if err == nil && res != nil && res.Success {
		if note := releaseGoneGatewayProjects(c); note != "" {
			res.Message = strings.TrimSpace(res.Message + note)
		}
	}
	return res, err
}

// check is "Check again": the "Can it be called?" check of the tool's
// model, on step "callable".
func (m *modelsRun) check(req types.ModelsRequest) (*types.OperationResponse, error) {
	c := m.c
	entry, _ := catalogEntry(req.Model)
	m.step("callable", "running", fmt.Sprintf("Calling %s in %s once...", entry.Name, req.Project))
	model, found, err := readToolModel(c, req.Project, entry.Name)
	if err != nil {
		return m.fail("callable", fmt.Sprintf("Cannot read %s in %s: %v.", entry.Name, req.Project, err), errorCodeFromK8sErr(err), "model read failed")
	}
	rec, err := readModelsRecord(c)
	if err != nil {
		return m.fail("callable", err.Error(), errorCodeFromK8sErr(err), "record read failed")
	}
	switch {
	case !found:
		return m.fail("callable", fmt.Sprintf("Project %s has no %s.", req.Project, entry.Name), "validation", "no model")
	case !rec.owns(model.Metadata):
		return m.fail("callable", fmt.Sprintf("%s in %s was not created by this tool, so this tool has no token of the model's own to call it with.", entry.Name, req.Project), "not_managed", "model foreign")
	}
	if cond, ok := findCondition(model.Status.Conditions, "Ready"); !ok || cond.Status != "True" {
		return m.fail("callable", fmt.Sprintf("%s in %s is not ready yet; check once it is.", entry.Name, req.Project), "not_ready", "model not ready")
	}
	return m.callable(req.Project, entry.Name, rec)
}

// callable runs the check and reports it on step "callable": a model that
// does not answer fails the operation (it stays set up).
func (m *modelsRun) callable(project, name string, rec modelsRecord) (*types.OperationResponse, error) {
	check := checkModelCall(m.c, project, name, rec, func(line string) { m.step("callable", "running", line) })
	switch {
	case check.OK:
		m.step("callable", "success", check.Message)
	case check.Result == "not-checked":
		m.step("callable", "success", check.Message)
	default:
		return m.fail("callable", callFailureWords("The model is set up", check), "not_callable", "model not callable ("+check.Result+")")
	}
	recordActivity(m.c, m.activity, m.target, true)
	return &types.OperationResponse{Success: true, Message: check.Message, Logs: m.logs}, nil
}

// allow adds project to the projects this tool's inference gateway allows
// (steps "check", "gateway"), for a model someone else deployed there (the
// dashboard). Only the Gateway is written. Someone else's gateway is
// never changed: the refusal gives RHOAI's documented step.
func (m *modelsRun) allow(project string) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", fmt.Sprintf("Checking the inference gateway and project %s...", project))
	if _, msg, code := projectNamespace(c, project); msg != "" {
		return m.fail("check", msg+" Nothing was changed.", code, "project check failed")
	}
	gw, found, err := readInferenceGateway(c)
	switch {
	case err != nil:
		return m.fail("check", fmt.Sprintf("Cannot read the inference gateway: %v. Nothing was changed.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "gateway read failed")
	case !found:
		return m.fail("check", "The inference gateway is not set up. Setting up a model sets it up, with the model's project on its list. Nothing was changed.", "prerequisites", "no gateway")
	case !gatewayOwned(gw.Metadata):
		return m.fail("check", foreignGatewayRefusal(project), "not_managed", "foreign gateway")
	}
	plan, msg, code := planGatewayAllow(c, project)
	if msg != "" {
		return m.fail("check", msg, code, "gateway check failed")
	}
	if plan == nil {
		m.step("check", "success", "Checked.")
		m.step("gateway", "success", fmt.Sprintf("The inference gateway already allows %s.", project))
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("The inference gateway already allows %s.", project), Logs: m.logs}, nil
	}
	if plan.foreign {
		return m.fail("check", foreignGatewayRefusal(project), "not_managed", "foreign gateway")
	}
	if msg, code, _ := applyInferenceGateway(c, plan, true, func(string) {}); msg != "" {
		return m.fail("check", msg, code, "gateway dry run failed")
	}
	m.step("check", "success", "The gateway is this tool's; the project exists.")
	m.step("gateway", "running", fmt.Sprintf("Adding %s to the projects the inference gateway allows...", project))
	if err := recordGatewayProject(c, project); err != nil {
		return m.fail("gateway", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
	}
	written, msg, code := writeInferenceGateway(c, plan, func(line string) { m.step("gateway", "running", line) })
	if msg != "" {
		return m.fail("gateway", msg, code, "gateway list failed")
	}
	done := fmt.Sprintf("The inference gateway now allows %s; models there attach to it. Projects it allows: %s.", project, strings.Join(written.names, ", "))
	m.step("gateway", "success", done)
	slog.Info("models allow", "project", project, "user", getUser(c))
	recordActivity(c, m.activity, m.target, true)
	return &types.OperationResponse{Success: true, Message: done, Logs: m.logs}, nil
}

func (m *modelsRun) teardown(req types.ModelsRequest) (*types.OperationResponse, error) {
	var done []string
	if req.Project != "" {
		entry, _ := catalogEntry(req.Model)
		if res, err := m.unpublishMaaSOnly(req.Project, entry.Name); res != nil || err != nil {
			return res, err
		}
		m.step("model", "running", fmt.Sprintf("Deleting %s in %s...", entry.Name, req.Project))
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

// unpublishMaaSOnly is the teardown's step "unpublish" for a model set up
// to be reached through MaaS only, or published with no route before
// (maasOnlySetup): deleting it unpublishes it first, with the MaaS tab's
// own unpublish, in the same operation. Any other published model is left
// to TeardownModels, which refuses it. Fail closed: a check or an
// unpublish that does not succeed stops the teardown before the model is
// deleted.
func (m *modelsRun) unpublishMaaSOnly(project, name string) (*types.OperationResponse, error) {
	only, err := maasOnlySetup(m.c, project, name)
	if err != nil {
		return m.fail("unpublish", fmt.Sprintf("Cannot check whether %s in %s is published to MaaS; nothing was deleted: %v.", name, project, err), errorCodeFromK8sErr(err), "maas check failed")
	}
	if !only {
		return nil, nil
	}
	m.step("unpublish", "running", "The model is reached through MaaS only, so it is unpublished first...")
	pub := &maasRun{c: m.c, activity: "unpublish-maas", target: fmt.Sprintf("model %s in %s", name, project)}
	pub.emit = m.innerSteps("unpublish")
	res, err := pub.unpublish(project, name, true)
	if err != nil {
		return nil, err
	}
	m.logs = append(m.logs, res.Logs...)
	if !res.Success {
		return m.fail("unpublish", res.Message+" The model was not deleted.", firstNonEmpty(res.ErrorCode, "partial_failure"), "unpublish failed")
	}
	m.step("unpublish", "success", res.Message)
	return nil, nil
}

// modelPlan is what a model setup will write, decided before any write.
type modelPlan struct {
	entry    catalogModel
	project  string
	preset   string
	image    imagePlan
	hardware hardwareProfile
	obj      map[string]any
	current  objectMeta
	found    bool
	// rec is the tool's record of its models, as read by the plan.
	rec modelsRecord
	// token is which token-authentication objects are missing (models_token.go).
	token tokenPlan
	// ephemeral: the model requests ephemeral storage for its download.
	ephemeral bool
	// exp is how the model is reached (models_exposure.go); published: it
	// is published to MaaS already (or routed through its gateway); publish:
	// the setup publishes it (a model reached through MaaS with no recorded
	// publication).
	exp       exposurePlan
	published bool
	publish   bool
}

// modelsSetup is what Set up's check step planned, and the summary the
// steps after it add to.
type modelsSetup struct {
	// gw: the inference gateway part; allow: the project added to this
	// tool's existing gateway for the model.
	gw, allow *gatewayPlan
	mp        *modelPlan
	summary   []string
}

// setup sets up the inference gateway, a model, or both. Steps: "check",
// "gateway", "gateway-ready", "model", "model-ready", "callable". Each part
// returns nil, nil to go on.
func (m *modelsRun) setup(req types.ModelsRequest) (*types.OperationResponse, error) {
	c := m.c
	s := &modelsSetup{}
	if r, err := m.setupCheck(req, s); r != nil || err != nil {
		return r, err
	}
	if s.gw != nil {
		if r, err := m.setupGateway(req, s); r != nil || err != nil {
			return r, err
		}
	}
	if s.mp != nil {
		if r, err := m.setupModelApply(s); r != nil || err != nil {
			return r, err
		}
		if r, err := m.setupModelReady(s); r != nil || err != nil {
			return r, err
		}
	}
	slog.Info("models setup", "target", m.target, "user", getUser(c))
	recordActivity(c, "setup-models", m.target, true)
	return &types.OperationResponse{Success: true, Message: strings.Join(s.summary, " "), Logs: m.logs}, nil
}

// setupCheck plans the gateway and the model parts into s and dry-runs
// every write.
func (m *modelsRun) setupCheck(req types.ModelsRequest, s *modelsSetup) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking what every part needs...")
	var notes []string

	// The gateway part.
	var gw *gatewayPlan
	if req.Gateway {
		var add []string
		if req.Project != "" {
			add = []string{req.Project}
		}
		plan, msg, code := planInferenceGateway(c, add)
		if msg != "" {
			return m.fail("check", msg, code, "gateway check failed")
		}
		gw = plan
	}

	// The model part. A model behind this tool's existing gateway: its
	// project is added to the gateway's list first, when it is not on it.
	// Any other gateway is used as it is.
	var mp *modelPlan
	var allow *gatewayPlan
	if req.Project != "" {
		plan, msg, code := m.planModel(req, gw)
		if msg != "" {
			return m.fail("check", msg, code, "model check failed")
		}
		mp = plan
		if gw == nil && mp.exp.toolGateway {
			if allow, msg, code = planGatewayAllow(c, mp.project); msg != "" {
				return m.fail("check", msg, code, "gateway check failed")
			}
			if allow != nil && allow.foreign {
				allow = nil // planModel checked that it admits the project
			}
		}
		if warning := modelCapacityWarning(c, mp.entry, mp.project); warning != "" {
			notes = append(notes, warning+" It may stay pending until a node is added or other workloads stop; the setup still runs.")
		}
		if warning := quotaWarning(c, mp.project, mp.entry, mp.ephemeral); warning != "" {
			notes = append(notes, warning)
		}
	}

	// Every write, as a server-side dry run, before any real one.
	for _, plan := range []*gatewayPlan{gw, allow} {
		if plan != nil && !plan.foreign {
			if msg, code, _ := applyInferenceGateway(c, plan, true, func(string) {}); msg != "" {
				return m.fail("check", msg, code, "gateway dry run failed")
			}
		}
	}
	if mp != nil {
		if msg, code := m.applyModel(mp, true); msg != "" {
			return m.fail("check", msg, code, "model dry run failed")
		}
		if _, msg, code := applyModelToken(c, mp.project, mp.entry.Name, mp.current.UID, mp.token, true); msg != "" {
			return m.fail("check", msg+" Nothing was changed.", code, "token dry run failed")
		}
	}
	m.step("check", "success", strings.TrimSpace("Everything the parts need is there. "+strings.Join(notes, " ")))
	s.gw, s.mp, s.allow = gw, mp, allow
	return nil, nil
}

// setupGateway creates the inference gateway and waits for it, or uses one
// someone else made as it is.
func (m *modelsRun) setupGateway(req types.ModelsRequest, s *modelsSetup) (*types.OperationResponse, error) {
	c, gw := m.c, s.gw
	if gw.foreign {
		msg := fmt.Sprintf("Gateway %s/%s already exists and was not created by this tool: models use it as it is, and this tool does not change it.", inferenceGatewayNamespace, inferenceGatewayName)
		m.step("gateway", "success", msg)
		m.step("gateway-ready", "success", "Not checked: the existing gateway's health is shown on the Models tab.")
		s.summary = append(s.summary, "The existing inference gateway is used.")
	} else {
		m.step("gateway", "running", "Creating the inference gateway...")
		if listed, _ := gw.gateway.listedNamespaces(); req.Project != "" && !slices.Contains(listed, req.Project) {
			if err := recordGatewayProject(c, req.Project); err != nil {
				return m.fail("gateway", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
			}
		}
		written, msg, code := writeInferenceGateway(c, gw, func(line string) { m.step("gateway", "running", line) })
		if msg != "" {
			return m.fail("gateway", msg, code, "gateway apply failed")
		}
		m.step("gateway", "success", fmt.Sprintf("GatewayClass, ConfigMap and Gateway %s/%s applied; external hostname %s. Projects it allows: %s.", inferenceGatewayNamespace, inferenceGatewayName, gw.host, strings.Join(written.names, ", ")))
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
				why = " The gateway controller says: " + truncateText(pending, 300) + "."
			}
			m.step("gateway-ready", "success", fmt.Sprintf("Created, but not programmed after %s. It may still come up; the Models tab shows its state.%s", clockText(InferenceGatewayReadyTimeout), why))
		case dns == "published":
			m.step("gateway-ready", "success", fmt.Sprintf("Ready: https://%s is published in DNS.", gw.host))
		default:
			m.step("gateway-ready", "success", fmt.Sprintf("Ready; https://%s is not published in DNS yet (a new load balancer's name can take a few minutes to resolve).", gw.host))
		}
		s.summary = append(s.summary, "The inference gateway is set up.")
	}
	return nil, nil
}

// setupModelApply adds the model's project to this tool's gateway when it
// needs it, then applies the model and its token objects.
func (m *modelsRun) setupModelApply(s *modelsSetup) (*types.OperationResponse, error) {
	c, mp, allow := m.c, s.mp, s.allow
	if allow != nil {
		m.step("model", "running", fmt.Sprintf("Adding %s to the projects the inference gateway allows...", mp.project))
		if err := recordGatewayProject(c, mp.project); err != nil {
			return m.fail("model", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
		}
		if _, msg, code := writeInferenceGateway(c, allow, func(line string) { m.step("model", "running", line) }); msg != "" {
			return m.fail("model", msg, code, "gateway list failed")
		}
	}
	m.step("model", "running", fmt.Sprintf("Creating %s in %s (%s)...", mp.entry.DisplayName, mp.project, mp.image.describe()))
	if msg, code := m.applyModel(mp, false); msg != "" {
		return m.fail("model", msg, code, "model apply failed")
	}
	if msg, code := m.applyToken(mp); msg != "" {
		return m.fail("model", msg, code, "token apply failed")
	}
	listed := ""
	if allow != nil {
		listed = fmt.Sprintf(" %s was added to the projects the inference gateway allows.", mp.project)
	}
	m.step("model", "success", fmt.Sprintf("LLMInferenceService %s/%s applied, on preset %s with %s. Reached %s.%s%s", mp.project, mp.entry.Name, mp.preset, mp.image.describe(), exposureWords(mp.exp), tokenWords(mp.project, mp.entry.Name, mp.token), listed))
	return nil, nil
}

// setupModelReady waits for the model (publishing it first when it is
// reached through MaaS), then calls it once.
func (m *modelsRun) setupModelReady(s *modelsSetup) (*types.OperationResponse, error) {
	c, mp := m.c, s.mp
	m.step("model-ready", "running", "Waiting for the model to start (typically 3 to 5 minutes the first time)...")
	timeout := ModelReadyTimeout
	if dl, ok := c.ctx.Deadline(); ok {
		if left := clk.Until(dl) - 30*time.Second; left < timeout {
			timeout = left
		}
	}
	// A model reached through MaaS is Ready only once published (its
	// route needs the MaaS gateway to admit the project): its workload
	// is waited on, then it is published.
	publish := mp.publish
	res := waitModelReady(c, mp.project, mp.entry.Name, mp.image.Kind == "preset", publish, timeout, func(line string) { m.step("model-ready", "running", line) })
	if res.err == nil && res.problem == "" && res.ready && publish {
		m.step("model-ready", "success", "The model's server runs; publishing it to MaaS gives it its route.")
		if r, err := m.publishToMaaS(mp.project, mp.entry.Name); r != nil || err != nil {
			return r, err
		}
		res.url = ""
	}
	switch {
	case res.err != nil:
		return m.fail("model-ready", fmt.Sprintf("Stopped waiting for the model: %v. It was created; the Models tab shows its state.", res.err), firstNonEmpty(errorCodeFromK8sErr(res.err), "cancelled"), "model wait failed")
	case res.imageMissing:
		return m.fail("model-ready", "The model cannot start: the cluster preset's image is not in the registry (this nightly build's runtime image is not published yet). Use the newest released Red Hat image instead: the Models tab offers it, and it replaces only the model's image. Technical details: "+res.problem, "image_unavailable", "preset image missing")
	case res.imageMaybeMissing:
		return m.fail("model-ready", "The model cannot start: "+res.problem+" A registry answered that the image does not exist (\"manifest unknown\"): if this nightly build's runtime image is not published yet, use the newest released Red Hat image instead: the Models tab offers it, and it replaces only the model's image.", "image_unavailable", "preset image may be missing")
	case res.problem != "":
		return m.fail("model-ready", "The model cannot start: "+res.problem+" It was created; delete it from the Models tab, or fix the cause and set it up again.", "not_ready", "model not starting")
	case !res.ready && res.pulling != "":
		// The wait ended while a failing pull was still in its grace
		// window: what to check is not lost.
		return m.fail("model-ready", fmt.Sprintf("Created, but not ready after %s: its image pull is still failing. %s It stays set up; it starts once the pull succeeds.", clockText(timeout), res.pulling), "not_ready", "image pull failing")
	case !res.ready:
		m.step("model-ready", "success", fmt.Sprintf("Created, but not ready after %s. It may still start; the Models tab shows its state and the reason if it cannot.", clockText(timeout)))
		m.step("callable", "success", "Not checked: the model is not ready yet. Check again on the Models tab once it is.")
		s.summary = append(s.summary, fmt.Sprintf("The model in %s is still starting.", mp.project))
	default:
		if !publish {
			m.step("model-ready", "success", fmt.Sprintf("Ready at %s.", firstNonEmpty(res.url, "its address")))
		}
		// Ready is not enough: one request through the way it is
		// reached tells whether a caller gets an answer.
		m.step("callable", "running", "Calling the model once, as a user would...")
		// The record as written by this setup (the token's UID).
		rec, err := readModelsRecord(c)
		if err != nil {
			rec = mp.rec
		}
		check := checkModelCall(c, mp.project, mp.entry.Name, rec, func(line string) { m.step("callable", "running", line) })
		if !check.OK && check.Result != "not-checked" {
			return m.fail("callable", callFailureWords(fmt.Sprintf("%s is ready in %s", mp.entry.DisplayName, mp.project), check)+" It stays set up; check again on the Models tab after fixing the cause.", "not_callable", "model not callable ("+check.Result+")")
		}
		m.step("callable", "success", check.Message)
		s.summary = append(s.summary, fmt.Sprintf("%s is ready in %s. %s", mp.entry.DisplayName, mp.project, check.Message))
	}
	return nil, nil
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
	if v := servedVersions(c, llmisvcGroup, nil); v.Discovered && !slices.Contains(v.Versions, "v1alpha2") {
		return nil, fmt.Sprintf("This cluster's KServe does not serve LLMInferenceService %s (served: %s), which is the shape this tool writes. Nothing was changed.", llmisvcAPIVersion, strings.Join(v.Versions, ", ")), "prerequisites"
	}
	preset := discoverCPUPreset(c)
	if preset.problem != "" {
		return nil, preset.problem + " Nothing was changed.", "prerequisites"
	}

	existing, found, err := readToolModel(c, req.Project, entry.Name)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read LLMInferenceService %s/%s: %v. Nothing was changed.%s", req.Project, entry.Name, err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	rec, err := readModelsRecord(c)
	if err != nil {
		return nil, err.Error() + " Nothing was changed.", errorCodeFromK8sErr(err)
	}
	if found && !rec.owns(existing.Metadata) {
		return nil, fmt.Sprintf("Project %s already has an LLMInferenceService named %s that this tool did not create. It was left unchanged.", req.Project, entry.Name), "not_managed"
	}
	if found && existing.Metadata.terminating() {
		return nil, fmt.Sprintf("The model %s in %s is still being deleted. Wait until it is gone, then set it up again.", entry.Name, req.Project), "terminating"
	}
	exp, msg, code := m.planModelExposure(req, labels, entry, gw, existing, found)
	if msg != "" {
		return nil, msg, code
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
	modelUID := ""
	if found {
		modelUID = existing.Metadata.UID
	}
	tok, msg, code := planModelToken(c, req.Project, entry.Name, modelUID, rec[modelRecordKey(req.Project, entry.Name)].TokenSecretUID)
	if msg != "" {
		return nil, msg, code
	}
	published, publish := false, exp.exposure == exposureMaaS
	if found {
		if published, err = MaaSPublishedIn(c, req.Project, entry.Name); err != nil {
			return nil, fmt.Sprintf("Cannot check whether %s in %s is published to MaaS: %v. Nothing was changed.", entry.Name, req.Project, err), errorCodeFromK8sErr(err)
		}
		// A model reached through MaaS is published again when no
		// publication is recorded: its publish, part of its setup, stopped
		// before it recorded one (the model is on the MaaS gateway alone).
		if publish && published {
			maasRec, err := readMaaSRecord(c)
			if err != nil {
				return nil, fmt.Sprintf("Cannot check whether %s in %s is published to MaaS: %v. Nothing was changed.", entry.Name, req.Project, err), errorCodeFromK8sErr(err)
			}
			publish = !maasRec.published(req.Project, entry.Name)
		}
	}
	ephemeral := modelVolumeIsEmptyDir(c)
	hw, why := pickHardwareProfile(c, entry)
	if hw.Name != "" {
		m.step("check", "running", fmt.Sprintf("Hardware profile: %s/%s, as the dashboard records it.", hw.Namespace, hw.Name))
	} else {
		m.step("check", "running", "Hardware profile: none, because "+why+`. The dashboard's Deployments table then shows "No hardware profile" for the model, and its details "Hardware profile: Unknown"; the model runs as set up.`)
	}
	return &modelPlan{entry: entry, project: req.Project, preset: preset.name, image: img, hardware: hw,
		obj: modelManifest(req.Project, entry, preset.name, img, hw, published, ephemeral, exp), current: existing.Metadata, found: found, rec: rec, token: tok, ephemeral: ephemeral,
		exp: exp, published: published, publish: publish}, "", ""
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
	action, meta, found, err := ensureToolObject(c, desc, path, createPath, p.obj, p.current, p.found, read, p.rec.owns, dryRun)
	p.current, p.found = meta, found
	if err == nil && action == "created" && !dryRun {
		// Its UID is what makes it the tool's (models_record.go). A model
		// that cannot be recorded would be nobody's: it is deleted again.
		if recErr := recordModel(c, meta); recErr != nil {
			if _, delErr := deleteWithUID(c, path, meta.UID); delErr != nil && !isK8sNotFound(delErr) {
				return fmt.Sprintf("%s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it yourself (oc delete llminferenceservice %s -n %s), then set up again.", desc, recErr, delErr, p.entry.Name, p.project), errorCodeFromK8sErr(recErr)
			}
			return fmt.Sprintf("%s was created, but this tool could not record it as its own (%v), so it was deleted again. Set up again to retry.", desc, recErr), firstNonEmpty(errorCodeFromK8sErr(recErr), "internal")
		}
		p.rec[modelRecordKey(meta.Namespace, meta.Name)] = recordedModel{UID: meta.UID}
	}
	if err == nil {
		return "", ""
	}
	code := errorCodeFromK8sErr(err)
	var foreign *foreignObjectError
	switch {
	case errors.As(err, &foreign):
		code = "not_managed"
	case errors.Is(err, errObjectChanged) || isK8sConflict(err):
		code = "conflict"
	}
	if dryRun {
		return fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", desc, err, templateHint(err)), code
	}
	return fmt.Sprintf("Failed to apply %s: %v. Set up again to retry.%s", desc, err, templateHint(err)), code
}

// applyToken makes the model's missing token objects, owned by it, and
// records the Secret's UID; a Secret that cannot be recorded is deleted
// again, so the next setup does not find it as someone else's.
func (m *modelsRun) applyToken(p *modelPlan) (string, string) {
	c := m.c
	if p.current.UID == "" {
		return fmt.Sprintf("The UID of LLMInferenceService %s/%s is not known, so its token authentication was not made. Set up again.", p.project, p.entry.Name), "internal"
	}
	secretUID, msg, code := applyModelToken(c, p.project, p.entry.Name, p.current.UID, p.token, false)
	if msg != "" {
		return msg + " The model was set up; set up again to finish its token authentication.", code
	}
	if secretUID == "" {
		return "", ""
	}
	if err := recordTokenSecret(c, p.project, p.entry.Name, secretUID); err != nil {
		path := tokenSecretPathOf(p.project, p.entry.Name)
		if _, delErr := deleteWithUID(c, path, secretUID); delErr != nil && !isK8sNotFound(delErr) {
			slog.Warn("cannot delete the unrecorded token Secret; garbage collection removes it with the model", "error", delErr)
		}
		return fmt.Sprintf("Secret %s/%s was created, but this tool could not record it (%v), so it was deleted again. Set up again to retry.", p.project, tokenSecretName(p.entry.Name), err), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	}
	return "", ""
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
		recordActivity(c, "teardown-model", fmt.Sprintf("%s (%s)", detail, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	var entries []catalogModel
	for _, entry := range modelCatalog {
		if len(names) == 0 || slices.Contains(names, entry.Name) {
			entries = append(entries, entry)
		}
	}
	// A published model is unpublished first (MaaS tab): its MaaSModelRef
	// and access entries would point at nothing.
	for _, entry := range entries {
		if published, err := MaaSPublishedIn(c, project, entry.Name); err != nil {
			return fail(fmt.Sprintf("Cannot check whether %s in %s is published to MaaS; nothing was deleted: %v.", entry.Name, project, err), errorCodeFromK8sErr(err), "maas check failed")
		} else if published {
			return fail(fmt.Sprintf("%s in %s is published to Models as a Service. Unpublish it on the MaaS tab first; nothing was deleted.", entry.Name, project), "in_use", "published to maas")
		}
	}
	rec, err := readModelsRecord(c)
	if err != nil {
		return fail(err.Error()+" Nothing was deleted.", errorCodeFromK8sErr(err), "record read failed")
	}
	var deleted, kept, forget []string
	// The record is updated once at the end, also when a step fails: a
	// model that is gone needs no entry.
	defer func() {
		if err := forgetModels(c, forget...); err != nil {
			slog.Warn("cannot update the models record", "error", err)
		}
	}()
	for _, entry := range entries {
		model, found, err := readToolModel(c, project, entry.Name)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read LLMInferenceService %s/%s; nothing was deleted: %v.%s", project, entry.Name, err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
		}
		if !found {
			forget = append(forget, modelRecordKey(project, entry.Name))
			continue
		}
		if !rec.owns(model.Metadata) {
			kept = append(kept, entry.Name)
			continue
		}
		path, _ := toolModelPath(project, entry.Name)
		if !model.Metadata.terminating() {
			logs = append(logs, deleteModelToken(c, project, entry.Name, model.Metadata.UID, rec[modelRecordKey(project, entry.Name)].TokenSecretUID)...)
			logs = append(logs, fmt.Sprintf("Deleting LLMInferenceService %s/%s...", project, entry.Name))
			if _, err := deleteWithUID(c, path, model.Metadata.UID); err != nil && !isK8sNotFound(err) {
				code := errorCodeFromK8sErr(err)
				if isK8sConflict(err) {
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
		forget = append(forget, modelRecordKey(project, entry.Name))
		logs = append(logs, fmt.Sprintf("OK: LLMInferenceService %s/%s deleted", project, entry.Name))
	}
	if len(deleted) == 0 && len(kept) > 0 {
		return fail(fmt.Sprintf("The model %s in %s was not created by this tool, so it was not deleted.", strings.Join(kept, ", "), project), "not_managed", "not managed")
	}
	recordActivity(c, "teardown-model", detail, true)
	if len(deleted) == 0 {
		what := "No model of this tool"
		if len(names) > 0 {
			what = "No " + strings.Join(names, ", ") + " of this tool"
		}
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("%s in %s; nothing to delete.%s", what, project, releaseGoneGatewayProjects(c)), Logs: logs}, nil
	}
	// The project's last model on this tool's gateway: a project this tool
	// added goes off the gateway's list (inference_gateway_projects.go);
	// so do projects it added that were deleted since.
	note := releaseGatewayProject(c, project) + releaseGoneGatewayProjects(c)
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Model %s deleted from %s; KServe removed its Deployment, Service and route. The project stays.%s", strings.Join(deleted, ", "), project, note), Logs: logs}, nil
}

// clockText is a duration as m:ss.
func clockText(d time.Duration) string {
	s := max(int(d.Round(time.Second)/time.Second), 0)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// modelPod is the part of a pod the model wait reads.
type modelPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName   string               `json:"nodeName"`
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
	// pullFailure is an image pull that keeps failing for a reason other
	// than a missing image (credentials, network), in words; node is the
	// pod's node. The wait gives it a grace window (modelPullGrace).
	pullFailure, node string
	// mentionsMissing: a source of that pull answered "manifest unknown"
	// (manifestUnknownMentioned).
	mentionsMissing bool
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
		event = fmt.Sprintf(" (%s: %s)", newest.Reason, truncateText(newest.Message, 300))
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
		return modelProgress{message: fmt.Sprintf("Waiting for a worker with room for the model: %s", firstNonEmpty(truncateText(cond.Message, 300), cond.Reason))}
	}
	check := func(s modelContainerStat, init bool) (modelProgress, bool) {
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff":
				if manifestUnknown(w.Message) {
					return modelProgress{problem: fmt.Sprintf("%s: %s", w.Reason, truncateText(w.Message, 300)), imageMissing: true}, true
				}
				// Credentials can reach a new node a few minutes after it
				// joins (ROSA HCP, seen live): the wait gives it time.
				return modelProgress{
					message:         fmt.Sprintf("Pulling the image of %s failed (%s: %s); the node keeps retrying.", s.Name, w.Reason, truncateText(w.Message, 300)),
					pullFailure:     fmt.Sprintf("the image of container %s cannot be pulled (%s: %s)", s.Name, w.Reason, truncateText(w.Message, 300)),
					node:            p.Spec.NodeName,
					mentionsMissing: manifestUnknownMentioned(w.Message),
				}, true
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
				return modelProgress{problem: fmt.Sprintf("container %s cannot be created (%s: %s).", s.Name, w.Reason, truncateText(w.Message, 300))}, true
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
	// imageMaybeMissing: the preset's image pull failed for the whole
	// grace window, and a source answered that the image does not exist.
	imageMaybeMissing bool
	// pulling is the image pull failure the wait ended in, with what to
	// check ("" when no pull was failing when it ended).
	pulling string
	err     error
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
//
// workloadOnly: ready once the workload has rolled out, whatever the
// model's Ready condition says (a model reached through MaaS becomes Ready
// only once it is published: its route needs the MaaS gateway to admit
// the project, which publishing does).
//
// An image pull that keeps failing is waited on for modelPullGrace before
// it fails the wait: a node's pull credentials can arrive minutes after it
// joins the cluster.
func waitModelReady(c *Client, project, name string, presetImage, workloadOnly bool, timeout time.Duration, progress func(string)) modelWait {
	var res modelWait
	last := ""
	var pullSince time.Time
	var pullGrace time.Duration
	pullNote := ""
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
		if (ready || workloadOnly) && w.rolledOut && w.observed {
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
		if pr.pullFailure != "" {
			if pullSince.IsZero() {
				pullSince = clk.Now()
				pullGrace, pullNote = modelPullGrace(cc, pr.node)
			}
			if clk.Since(pullSince) >= pullGrace {
				res.problem = fmt.Sprintf("%s, for %s.%s Check that the cluster's pull secret can read it (on ROSA, the additional pull secret), that a mirror covers its registry if the cluster uses one, and that the image exists.", pr.pullFailure, clockText(pullGrace), pullNote)
				res.imageMaybeMissing = pr.mentionsMissing && presetImage
				return true, nil
			}
			res.pulling = fmt.Sprintf("%s.%s Check that the cluster's pull secret can read it (on ROSA, the additional pull secret), that a mirror covers its registry if the cluster uses one, and that the image exists.", pr.pullFailure, pullNote)
			say(fmt.Sprintf("%s%s Waiting up to %s for it to succeed...", pr.message, pullNote, clockText(pullGrace)))
			return false, nil
		}
		pullSince, res.pulling = time.Time{}, ""
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
