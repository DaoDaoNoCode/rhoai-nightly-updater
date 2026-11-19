package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	testGatewayPath   = "/apis/gateway.networking.k8s.io/v1/namespaces/openshift-ingress/gateways/openshift-ai-inference"
	testGatewayClass  = "/apis/gateway.networking.k8s.io/v1/gatewayclasses/openshift-ai-inference"
	testGatewayCM     = "/api/v1/namespaces/openshift-ingress/configmaps/openshift-ai-inference-options"
	testModelPath     = "/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/qwen25-05b-instruct"
	testPresetImage   = "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:978a7c7b"
	testPresetName    = "v3-6-0-kserve-config-llm-template-cpu"
	testSubscription  = "/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/subscriptions/rhods-operator"
	testModelPodsPath = "/api/v1/namespaces/proj/pods/qwen25-05b-instruct-kserve-abc"
)

// modelsCluster is a fake cluster with what a model setup needs: KServe
// with its LLM dependencies, the controller's preset prefix and the CPU
// preset, the apps domain, the default ingress certificate and a data
// science project "proj". Gateways report Programmed with an address and
// their hostname is published; a model reports Ready on its third read.
type modelsCluster struct {
	f *resourceFake
	c *Client

	mu         sync.Mutex
	modelReads int
	// readyAfter is the model read that first reports Ready (0: never).
	readyAfter int
	// route is KServe's handling of the model's HTTPRoute (newMaaSCluster).
	route *kserveRouteSim
	// calls simulates the model's answers to "Can it be called?"
	// (models_routing_test.go).
	calls *modelCallSim
}

func newModelsCluster(t *testing.T) *modelsCluster {
	t.Helper()
	capacityCache.Purge()
	planCache.Purge()
	t.Setenv(vllmImageEnv, "")
	f, c := newResourceFake(t)
	mc := &modelsCluster{f: f, c: c, readyAfter: 3}
	// KServe's workload Deployment for the model, rolled out.
	putModelDeployment(mc, 1, 1, 1)
	f.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
		"spec":{"components":{"kserve":{"managementState":"Managed"}}},
		"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"True"}]}}`)
	f.putJSON("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/llmisvc-controller-manager", `{
		"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"LLM_INFERENCE_SERVICE_CONFIG_PREFIX","value":"v3-6-0-kserve-"}]}]}}}}`)
	f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/redhat-ods-applications/llminferenceserviceconfigs/"+testPresetName, `{
		"spec":{"template":{"containers":[{"name":"main","image":"`+testPresetImage+`"}]}}}`)
	f.putJSON("/apis/config.openshift.io/v1/ingresses/cluster", `{"spec":{"domain":"apps.example.com"}}`)
	// KServe's configuration names its default gateway, as RHOAI 3.x ships it.
	f.putJSON("/api/v1/namespaces/redhat-ods-applications/configmaps/inferenceservice-config", `{"data":{"ingress":"{\"kserveIngressGateway\":\"openshift-ingress/openshift-ai-inference\"}"}}`)
	f.putJSON("/apis/operator.openshift.io/v1/namespaces/openshift-ingress-operator/ingresscontrollers/default", `{"spec":{"defaultCertificate":{"name":"custom-cert"}}}`)
	f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	f.putJSON("/apis/ingress.operator.openshift.io/v1/namespaces/openshift-ingress/dnsrecords/external", `{
		"spec":{"dnsName":"inference.apps.example.com."},
		"status":{"zones":[{"conditions":[{"type":"Published","status":"True"}]},{"conditions":[{"type":"Published","status":"True"}]}]}}`)
	f.onGet = func(k fakeKey, obj map[string]interface{}) {
		switch k.plural {
		case "gateways":
			obj["status"] = map[string]interface{}{
				"addresses":  []interface{}{map[string]interface{}{"value": "a1.elb.example.com"}},
				"conditions": []interface{}{map[string]interface{}{"type": "Accepted", "status": "True"}, map[string]interface{}{"type": "Programmed", "status": "True"}},
			}
		case "llminferenceservices":
			mc.mu.Lock()
			mc.modelReads++
			n := mc.modelReads
			mc.mu.Unlock()
			if mc.readyAfter > 0 && n >= mc.readyAfter {
				obj["status"] = map[string]interface{}{
					"url":        "https://inference.apps.example.com/" + k.ns + "/" + k.name,
					"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}},
				}
			}
		}
	}
	simulateRouting(t, mc)
	return mc
}

func runModels(t *testing.T, c *Client, req types.ModelsRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunModels(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunModels: %v", err)
	}
	return res, events
}

// finalSteps is each step's last status, in the order the steps first ran.
func finalSteps(events []UpdateStepEvent) string {
	var order []string
	last := map[string]string{}
	for _, e := range events {
		if _, ok := last[e.Step]; !ok {
			order = append(order, e.Step)
		}
		last[e.Step] = e.Status
	}
	out := make([]string, len(order))
	for i, s := range order {
		out[i] = s + "=" + last[s]
	}
	return strings.Join(out, " ")
}

func TestModelsSetup_CreatesTheGatewayThenTheModel(t *testing.T) {
	mc := newModelsCluster(t)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	if got := finalSteps(events); got != "check=success gateway=success gateway-ready=success model=success model-ready=success callable=success" {
		t.Fatalf("steps: %s", got)
	}
	if !strings.Contains(res.Message, "Qwen2.5 0.5B Instruct (CPU) is ready in proj.") {
		t.Errorf("message: %q", res.Message)
	}

	// The class is OpenShift's controller; the options ConfigMap asks
	// service-ca for the internal listener's certificate.
	class := mc.f.get(testGatewayClass)
	if class["spec"].(map[string]interface{})["controllerName"] != "openshift.io/gateway-controller/v1" {
		t.Errorf("class: %v", class)
	}
	cm := mc.f.get(testGatewayCM)
	if svc := cm["data"].(map[string]interface{})["service"].(string); !strings.Contains(svc, "service.beta.openshift.io/serving-cert-secret-name: openshift-ai-inference-service-tls") {
		t.Errorf("options: %q", svc)
	}
	// Two listeners: the apps domain with the ingress's default
	// certificate (only named), and the Service's own name.
	gw := mc.f.get(testGatewayPath)
	spec := gw["spec"].(map[string]interface{})
	listeners := spec["listeners"].([]interface{})
	l0, l1 := listeners[0].(map[string]interface{}), listeners[1].(map[string]interface{})
	cert := func(l map[string]interface{}) interface{} {
		return l["tls"].(map[string]interface{})["certificateRefs"].([]interface{})[0].(map[string]interface{})["name"]
	}
	if l0["hostname"] != "inference.apps.example.com" || cert(l0) != "custom-cert" {
		t.Errorf("external listener: %v", l0)
	}
	if l1["hostname"] != "openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local" || cert(l1) != "openshift-ai-inference-service-tls" {
		t.Errorf("internal listener: %v", l1)
	}
	if spec["infrastructure"].(map[string]interface{})["parametersRef"].(map[string]interface{})["name"] != "openshift-ai-inference-options" {
		t.Errorf("parametersRef: %v", spec["infrastructure"])
	}

	// The model: the preset found through the controller's prefix, the
	// model's name equal to the resource's (the playground resolves by it),
	// the default gateway, and no image of its own on a released cluster.
	model := mc.f.get(testModelPath)
	ms := model["spec"].(map[string]interface{})
	if ms["baseRefs"].([]interface{})[0].(map[string]interface{})["name"] != testPresetName {
		t.Errorf("baseRefs: %v", ms["baseRefs"])
	}
	if m := ms["model"].(map[string]interface{}); m["name"] != "qwen25-05b-instruct" || m["uri"] != "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775" {
		t.Errorf("model: %v", m)
	}
	main := ms["template"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})
	if _, set := main["image"]; set {
		t.Errorf("a released cluster's model must use the preset's image, got %v", main["image"])
	}
	labels := model["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if labels["opendatahub.io/dashboard"] != "true" || labels["opendatahub.io/genai-asset"] != "true" || labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("labels: %v", labels)
	}
	// Access: only the dashboard's token-authentication objects
	// (models_token_test.go checks them), nothing else.
	for _, m := range mc.f.mutations() {
		for _, kind := range []string{"serviceaccounts", "roles", "rolebindings", "secrets"} {
			if strings.Contains(m, "/"+kind) && !strings.HasPrefix(m, "POST ") {
				t.Errorf("setup wrote %s", m)
			}
		}
	}
}

// A gateway someone else made is used as it is; nothing of the gateway is
// written.
func TestModelsSetup_UsesAForeignGatewayWithoutTouchingIt(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testGatewayPath, `{"spec":{"gatewayClassName":"openshift-ai-inference","listeners":[{"name":"https","hostname":"x.example.com","allowedRoutes":{"namespaces":{"from":"All"}}}]}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	for _, m := range mc.f.mutations() {
		if strings.Contains(m, "gateway") || strings.Contains(m, "openshift-ingress") {
			t.Errorf("wrote to the gateway: %s", m)
		}
	}
	if !strings.Contains(finalSteps(events), "gateway=success") || !mc.f.has(testModelPath) {
		t.Fatalf("steps %s, model created %v", finalSteps(events), mc.f.has(testModelPath))
	}
}

// A foreign gateway whose listeners do not admit the project: refused
// before anything is written.
func TestModelsSetup_RefusesAGatewayThatDoesNotAdmitTheProject(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testGatewayPath, `{"spec":{"listeners":[{"name":"https","allowedRoutes":{"namespaces":{"from":"Selector","selector":{"matchLabels":{"maas":"true"}}}}}]}}`)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "does not let routes from project proj attach") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations(); len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
}

func TestModelsSetup_ModelNeedsTheGateway(t *testing.T) {
	mc := newModelsCluster(t)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "Set up the inference gateway") {
		t.Fatalf("got %+v", res)
	}
}

// Every refusal changes nothing: a foreign model, a project that is not a
// data science project, KServe off, LLM dependencies missing, no preset.
func TestModelsSetup_RefusesBeforeWritingAnything(t *testing.T) {
	cases := map[string]struct {
		setup func(mc *modelsCluster)
		code  string
		want  string
	}{
		"foreign model": {func(mc *modelsCluster) {
			mc.f.putJSON(testModelPath, `{"metadata":{"labels":{}}}`)
		}, "not_managed", "that this tool did not create"},
		"not a data science project": {func(mc *modelsCluster) {
			mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{}}}`)
		}, "validation", "is not a data science project"},
		"kserve removed": {func(mc *modelsCluster) {
			mc.f.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{"spec":{"components":{"kserve":{"managementState":"Removed"}}}}`)
		}, "prerequisites", "is turned off"},
		"llm dependencies false": {func(mc *modelsCluster) {
			mc.f.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
				"spec":{"components":{"kserve":{"managementState":"Managed"}}},
				"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"False","message":"LeaderWorkerSet missing"}]}}`)
		}, "prerequisites", "KserveLLMInferenceServiceDependencies=False (LeaderWorkerSet missing)"},
		"no preset prefix": {func(mc *modelsCluster) {
			mc.f.putJSON("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/llmisvc-controller-manager", `{"spec":{"template":{"spec":{"containers":[{"name":"manager"}]}}}}`)
		}, "prerequisites", "sets no LLM_INFERENCE_SERVICE_CONFIG_PREFIX"},
		"no cpu preset": {func(mc *modelsCluster) {
			mc.f.putJSON("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/llmisvc-controller-manager", `{
				"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"LLM_INFERENCE_SERVICE_CONFIG_PREFIX","value":"v9-kserve-"}]}]}}}}`)
		}, "prerequisites", "LLMInferenceServiceConfig v9-kserve-config-llm-template-cpu does not exist"},
		"foreign options configmap": {func(mc *modelsCluster) {
			mc.f.putJSON(testGatewayCM, `{"data":{}}`)
		}, "not_managed", "ConfigMap openshift-ingress/openshift-ai-inference-options already exists"},
		"foreign class of another controller": {func(mc *modelsCluster) {
			mc.f.putJSON(testGatewayClass, `{"spec":{"controllerName":"example.com/other"}}`)
		}, "not_managed", "belongs to controller"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mc := newModelsCluster(t)
			tc.setup(mc)
			res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
			if res.Success || res.ErrorCode != tc.code || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("got %+v", res)
			}
			if m := mc.f.mutations(); len(m) != 0 {
				t.Errorf("wrote %v", m)
			}
			if got := finalSteps(events); got != "check=failed" {
				t.Errorf("steps: %s", got)
			}
		})
	}
}

// The API server's refusal of a dry run stops the setup before any write.
func TestModelsSetup_DryRunRefusalWritesNothing(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.failDryRun["POST /apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices"] = 422
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "The API server rejects LLMInferenceService proj/qwen25-05b-instruct") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations(); len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
}

// On a nightly cluster without a mirror for the preset's repository, the
// newest released image is used, pinned by digest, its version recorded.
func TestModelsSetup_NightlyClusterUsesTheNewestReleasedImage(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testSubscription, `{"spec":{"source":"`+CatalogName+`"}}`)
	prev := lookupReleasedImage
	t.Cleanup(func() { lookupReleasedImage = prev })
	var asked string
	lookupReleasedImage = func(_ context.Context, repo string) (releasedImage, error) {
		asked = repo
		return releasedImage{Ref: "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:5353", Version: "3.5.3"}, nil
	}
	mc.f.putJSON(testGatewayPath, `{"metadata":{"labels":{}},"spec":{"listeners":[{"allowedRoutes":{"namespaces":{"from":"All"}}}]}}`)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	if asked != "rhaii/vllm-cpu-rhel9" {
		t.Errorf("looked up %q", asked)
	}
	model := mc.f.get(testModelPath)
	main := model["spec"].(map[string]interface{})["template"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})
	ann := model["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if main["image"] != "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:5353" || ann[modelImageSourceAnnotation] != "released" || ann[modelImageVersionAnnotation] != "3.5.3" {
		t.Fatalf("image %v, annotations %v", main["image"], ann)
	}

	// With a mirror for registry.redhat.io/rhaii the preset's image is pulled.
	mc2 := newModelsCluster(t)
	mc2.f.putJSON(testSubscription, `{"spec":{"source":"`+CatalogName+`"}}`)
	mc2.f.putJSON("/apis/config.openshift.io/v1/imagedigestmirrorsets/stage", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhaii","mirrors":["registry.stage.redhat.io/rhaii"]}]}}`)
	plan, err := planModelImage(mc2.c, testPresetImage, false, nil, true)
	if err != nil || plan.Kind != "preset" || plan.Image != "" {
		t.Fatalf("mirrored: %+v, %v", plan, err)
	}

	// VLLM_CPU_IMAGE wins.
	t.Setenv(vllmImageEnv, "mirror.example.com/vllm-cpu:1")
	plan, err = planModelImage(mc2.c, testPresetImage, true, nil, true)
	if err != nil || plan.Kind != "override" || plan.Image != "mirror.example.com/vllm-cpu:1" {
		t.Fatalf("override: %+v, %v", plan, err)
	}
}

// A repair keeps the released image a model runs instead of moving it
// back to the preset's.
func TestPlanModelImage_KeepsTheReleasedImageOnRepair(t *testing.T) {
	mc := newModelsCluster(t)
	var existing llmisvcObject
	existing.Metadata.Annotations = map[string]string{modelImageSourceAnnotation: "released", modelImageVersionAnnotation: "3.5.3"}
	existing.Spec.Template = &struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	}{Containers: []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}{{Name: "main", Image: "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:5353"}}}
	plan, err := planModelImage(mc.c, testPresetImage, false, &existing, true)
	if err != nil || plan.Kind != "released" || plan.Image != "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:5353" || plan.Version != "3.5.3" {
		t.Fatalf("got %+v, %v", plan, err)
	}
	// Someone changed the image (and kept the annotation): it is not kept,
	// so setting the model up again restores what the tool decides.
	for _, img := range []string{"quay.io/someone/vllm@sha256:5353", "registry.redhat.io/rhaii/vllm-cpu-rhel9:latest"} {
		existing.Spec.Template.Containers[0].Image = img
		plan, err = planModelImage(mc.c, testPresetImage, false, &existing, true)
		if err != nil || plan.Image == img {
			t.Fatalf("%s: got %+v, %v", img, plan, err)
		}
	}
}

// The preset's image is missing from the registry: the wait stops at once
// with image_unavailable, and the status offers the released image.
func TestModelsSetup_PresetImageMissingStopsWithItsFix(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 0
	mc.f.putJSON(testGatewayPath, `{"metadata":{"labels":{}},"spec":{"listeners":[{"allowedRoutes":{"namespaces":{"from":"All"}}}]}}`)
	mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ErrImagePull","message":"reading manifest sha256:978a in registry.redhat.io/rhaii/vllm-cpu-rhel9: manifest unknown"}}}]}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"})
	if res.Success || res.ErrorCode != "image_unavailable" || !strings.Contains(res.Message, "newest released Red Hat image") {
		t.Fatalf("got %+v", res)
	}
	if got := finalSteps(events); got != "check=success model=success model-ready=failed" {
		t.Errorf("steps: %s", got)
	}

	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models.Items) != 1 || !st.Models.Items[0].PresetImageMissing || !st.Models.Items[0].TerminalError || !st.Models.Items[0].ManagedByTool {
		t.Fatalf("status: %+v", st.Models.Items)
	}
}

// A mirrored pull where one source answers "manifest unknown" and another
// refuses the credentials is waited on (it may be a node without its pull
// secret yet); once the window ends, the released image is offered again,
// as a possibility, in the setup's failure and in the status.
func TestModelsSetup_MaybeMissingPresetImageOffersItsFixAfterTheWindow(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 0
	mixed := `(Mirrors also failed: [quay.io/rhoai/vllm-cpu@sha256:a: unauthorized: access to the requested resource is not authorized]): registry.redhat.io/rhaii/vllm-cpu@sha256:a: reading manifest sha256:a in registry.redhat.io/rhaii/vllm-cpu: manifest unknown`
	mc.f.putJSON("/api/v1/nodes/node-a", `{"metadata":{"creationTimestamp":"`+clk.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339)+`"}}`)
	pod := map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"app.kubernetes.io/name": "qwen25-05b-instruct", "app.kubernetes.io/component": "llminferenceservice-workload"}},
		"spec":     map[string]interface{}{"nodeName": "node-a"},
		"status": map[string]interface{}{"containerStatuses": []interface{}{map[string]interface{}{"name": "main",
			"state": map[string]interface{}{"waiting": map[string]interface{}{"reason": "ImagePullBackOff", "message": mixed}}}}},
	}
	mc.f.put(testModelPodsPath, pod)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || res.ErrorCode != "image_unavailable" || !strings.Contains(res.Message, "for 2:00") || !strings.Contains(res.Message, "if this nightly build's runtime image is not published yet, use the newest released Red Hat image") {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(strings.Join(eventMessages(events, "model-ready"), "\n"), "Waiting up to 2:00") {
		t.Errorf("not waited on: %v", eventMessages(events, "model-ready"))
	}
	// The status: not while the model is younger than the longest window
	// (a young node may still get its pull secret), then offered.
	st, _ := GetResourcesStatus(mc.c)
	if m := st.Models.Items[0]; m.PresetImageMissing || m.PresetImageUncertain {
		t.Fatalf("young model: %+v", m)
	}
	obj := mc.f.get(testModelPath)
	ensureMap(obj, "metadata")["creationTimestamp"] = time.Now().Add(-ModelImagePullGraceYoungNode - time.Minute).UTC().Format(time.RFC3339)
	mc.f.put(testModelPath, obj)
	st, _ = GetResourcesStatus(mc.c)
	if m := st.Models.Items[0]; !m.PresetImageMissing || !m.PresetImageUncertain || m.TerminalError != true {
		t.Fatalf("old model: %+v", m)
	}
}

// Progress comes from the pod and its newest event, in words.
func TestDescribeModelPods(t *testing.T) {
	pod := func(status string) modelPod {
		var p modelPod
		if err := json.Unmarshal([]byte(`{"metadata":{"name":"p"},"status":`+status+`}`), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ev := &modelEvent{Reason: "Pulling", Message: `Pulling image "registry.redhat.io/rhaii/vllm-cpu-rhel9"`}
	cases := []struct {
		name    string
		pods    []modelPod
		event   *modelEvent
		message string
		problem string
	}{
		{"no pod", nil, nil, "Waiting for KServe to create the model's pod...", ""},
		{"unschedulable", []modelPod{pod(`{"conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes are available: 3 Insufficient cpu."}]}`)}, nil, "Waiting for a worker with room for the model: 0/3 nodes are available: 3 Insufficient cpu.", ""},
		{"downloading", []modelPod{pod(`{"initContainerStatuses":[{"name":"storage-initializer","state":{"running":{}}}]}`)}, nil, "Downloading the model from Hugging Face (storage-initializer)...", ""},
		{"pulling", []modelPod{pod(`{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ContainerCreating"}}}]}`)}, ev, `Pulling the runtime image (several GB; a few minutes the first time on a node)... (Pulling: Pulling image "registry.redhat.io/rhaii/vllm-cpu-rhel9")`, ""},
		{"starting", []modelPod{pod(`{"containerStatuses":[{"name":"main","ready":false,"state":{"running":{}}}]}`)}, nil, "Starting vLLM and loading the model...", ""},
		{"oom restart", []modelPod{pod(`{"containerStatuses":[{"name":"main","ready":false,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}`)}, nil, "Restarted after running out of memory (OOMKilled); starting vLLM again...", ""},
		{"crash loop", []modelPod{pod(`{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}`)}, nil, "", "container main keeps crashing. Last exit: OOMKilled, code 137. It ran out of memory. Its logs say why."},
		// A failing pull is waited on (modelPullGrace); the wait fails it.
		{"pull back-off", []modelPod{pod(`{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ImagePullBackOff","message":"unauthorized"}}}]}`)}, nil, "Pulling the image of main failed (ImagePullBackOff: unauthorized); the node keeps retrying.", ""},
	}
	for _, tc := range cases {
		got := describeModelPods(tc.pods, tc.event)
		if got.message != tc.message || got.problem != tc.problem {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
}

func TestModelsTeardown_DeletesTheModelThenTheGateway(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	// A foreign model of the same name elsewhere is never touched; it
	// uses another gateway, so it does not block.
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/other/llminferenceservices/qwen25-05b-instruct", `{"spec":{"router":{"gateway":{"refs":[{"name":"maas-default-gateway","namespace":"openshift-ingress"}]}}}}`)
	before := len(mc.f.mutations())
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if got := finalSteps(events); got != "model=success gateway=success" {
		t.Errorf("steps: %s", got)
	}
	var deletes []string
	for _, m := range mc.f.mutations()[before:] {
		if strings.HasPrefix(m, "DELETE ") {
			deletes = append(deletes, strings.TrimPrefix(m, "DELETE "))
		}
	}
	// (mutations leaves ConfigMaps out: the activity log writes them.)
	// The model's token objects go first (garbage collection would take
	// them with it too), then the model, then the gateway.
	tokens := []string{"/api/v1/namespaces/proj/serviceaccounts/qwen25-05b-instruct-sa", "/apis/rbac.authorization.k8s.io/v1/namespaces/proj/roles/qwen25-05b-instruct-view-role",
		"/apis/rbac.authorization.k8s.io/v1/namespaces/proj/rolebindings/qwen25-05b-instruct-view", "/api/v1/namespaces/proj/secrets/default-name-qwen25-05b-instruct-sa"}
	if want := append(tokens, testModelPath, testGatewayPath, testGatewayClass); strings.Join(deletes, " ") != strings.Join(want, " ") {
		t.Fatalf("deleted %v, want %v", deletes, want)
	}
	if mc.f.has(testGatewayCM) {
		t.Error("the options ConfigMap is left")
	}
	if !mc.f.has("/apis/serving.kserve.io/v1alpha2/namespaces/other/llminferenceservices/qwen25-05b-instruct") {
		t.Error("deleted someone else's model")
	}
}

// The gateway stays while any model, whoever made it, routes through it
// (spec.router.gateway {} is the default gateway).
func TestGatewayTeardown_RefusesWhileAModelUsesIt(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/team/llminferenceservices/granite", `{"spec":{"router":{"route":{},"gateway":{}}}}`)
	before := len(mc.f.mutations())
	res, _ := TeardownInferenceGateway(mc.c, false)
	if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "team/granite") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
	st, _ := GetResourcesStatus(mc.c)
	if !strings.Contains(st.InferenceGateway.TeardownBlockedReason, "team/granite") || len(st.InferenceGateway.UsedBy) != 1 {
		t.Errorf("status: %+v", st.InferenceGateway)
	}

	// Models that cannot be listed: fail closed.
	mc.f.fail["GET /apis/serving.kserve.io/v1alpha2/llminferenceservices"] = 500
	res, _ = TeardownInferenceGateway(mc.c, false)
	if res.Success || !strings.Contains(res.Message, "Cannot check which models use the gateway") {
		t.Fatalf("got %+v", res)
	}
}

// A model that inherits its route from a preset (no spec.router.gateway)
// is seen through the gateway's attached routes; with none, the teardown
// proceeds.
func TestGatewayTeardown_RefusesWhileRoutesAreAttached(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/team/llminferenceservices/granite", `{"spec":{"baseRefs":[{"name":"v3-6-0-kserve-config-llm-router-route"}]}}`)
	attached := 1
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		if k.plural == "gateways" {
			obj["status"] = map[string]interface{}{"listeners": []interface{}{
				map[string]interface{}{"name": "https-external", "attachedRoutes": attached},
				map[string]interface{}{"name": "https", "attachedRoutes": 0},
			}}
		}
	}
	before := len(mc.f.mutations())
	res, _ := TeardownInferenceGateway(mc.c, false)
	if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "1 route is attached to the inference gateway") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
	st, _ := GetResourcesStatus(mc.c)
	if !strings.Contains(st.InferenceGateway.TeardownBlockedReason, "1 route is attached") {
		t.Errorf("status: %+v", st.InferenceGateway)
	}

	attached = 0
	if res, _ = TeardownInferenceGateway(mc.c, false); !res.Success || mc.f.has(testGatewayPath) {
		t.Fatalf("got %+v", res)
	}
}

func TestGatewayTeardown_NeverDeletesAForeignGateway(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testGatewayPath, `{"spec":{}}`)
	res, _ := TeardownInferenceGateway(mc.c, false)
	if res.Success || res.ErrorCode != "not_managed" || !mc.f.has(testGatewayPath) {
		t.Fatalf("got %+v", res)
	}
}

func TestTeardownModels_WaitsForTheFinalizerAndNamesIt(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testModelPath, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"finalizers":["serving.kserve.io/llmisvc-finalizer"],
		"managedFields":[{"manager":"rhoai-nightly-updater","operation":"Apply"}]}}`)
	recordModelFixture(mc.f, testModelPath)
	res, _ := TeardownModels(mc.c, "proj")
	if res.Success || res.ErrorCode != "in_progress" || !strings.Contains(res.Message, "serving.kserve.io/llmisvc-finalizer") {
		t.Fatalf("got %+v", res)
	}
	// The controller finishes: the next teardown sees it gone.
	mc.f.finalizeOnGet = true
	res, _ = TeardownModels(mc.c, "proj")
	if !res.Success || mc.f.has(testModelPath) {
		t.Fatalf("got %+v", res)
	}
}

// The status: an unmanaged gateway that reports nothing has unknown
// health, never a failure; a model of the catalog's name that someone
// else made is shown, not offered for deletion.
func TestModelsStatus_UnknownAndForeign(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.onGet = nil
	mc.f.putJSON(testGatewayPath, `{"spec":{"listeners":[{"hostname":"ai.example.com"}]}}`)
	mc.f.putJSON(testModelPath, `{"metadata":{"annotations":{"openshift.io/display-name":"Theirs"}},"spec":{"router":{"gateway":{}}}}`)
	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	gw := st.InferenceGateway
	if !gw.Deployed || gw.ManagedByTool || gw.Ready || !gw.HealthUnknown || gw.TerminalError || gw.ExternalURL != "https://ai.example.com" {
		t.Errorf("gateway: %+v", gw)
	}
	if len(st.Models.Items) != 1 {
		t.Fatalf("items: %+v", st.Models.Items)
	}
	m := st.Models.Items[0]
	if m.ManagedByTool || !m.HealthUnknown || m.TeardownBlockedReason == "" || m.DisplayName != "Theirs" {
		t.Errorf("model: %+v", m)
	}
	if st.Models.Preset != testPresetName || st.Models.ImagePlan != "preset" || st.Models.Dependencies != "True" || st.Models.Blocked != nil {
		t.Errorf("models: %+v", st.Models)
	}
	if len(st.Models.Catalog) != 4 || st.Models.Catalog[1].Name != "qwen25-05b-instruct" {
		t.Errorf("catalog: %+v", st.Models.Catalog)
	}
}

// The gateway's authentication is checked once a model uses it.
func TestInferenceGatewayStatus_WarnsWithoutItsAuthPolicy(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	st, _ := GetResourcesStatus(mc.c)
	gw := st.InferenceGateway
	if !gw.Ready || !gw.ManagedByTool || gw.AuthPolicy != "missing" || !strings.Contains(gw.Warning, "authentication is missing") || gw.DNS != "published" {
		t.Fatalf("gateway: %+v", gw)
	}
	mc.f.putJSON("/apis/kuadrant.io/v1/namespaces/openshift-ingress/authpolicies/openshift-ai-inference-authn", `{"status":{"conditions":[{"type":"Enforced","status":"True"}]}}`)
	mc.f.putJSON("/api/v1/namespaces/openshift-ingress/services/openshift-ai-inference-openshift-ai-inference", `{"metadata":{"labels":{"gateway.networking.k8s.io/gateway-name":"openshift-ai-inference"}}}`)
	// The status lists the models (a list is not changed by onGet).
	model := mc.f.get(testModelPath)
	model["status"] = map[string]interface{}{"url": "https://inference.apps.example.com/proj/qwen25-05b-instruct", "conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}}
	mc.f.put(testModelPath, model)
	st, _ = GetResourcesStatus(mc.c)
	if st.InferenceGateway.AuthPolicy != "enforced" || st.InferenceGateway.Warning != "" {
		t.Fatalf("gateway: %+v", st.InferenceGateway)
	}
	if len(st.Models.Items) != 1 || !st.Models.Items[0].Ready || st.Models.Items[0].URL != "https://inference.apps.example.com/proj/qwen25-05b-instruct" {
		t.Fatalf("models: %+v", st.Models.Items)
	}
}

func TestValidateModelsRequest(t *testing.T) {
	cases := map[string]types.ModelsRequest{
		"action must be setup, teardown, allow, disallow or check": {Action: "delete", Gateway: true},
		"nothing to do: name the gateway, a project, or both":      {Action: "setup"},
		"unknown model":                     {Action: "setup", Project: "p", Model: "llama"},
		"releasedImage needs a model setup": {Action: "teardown", Project: "p", ReleasedImage: true},
		"allow takes a project only":        {Action: "allow", Gateway: true, Project: "p"},
		"":                                  {Action: "setup", Gateway: true, Project: "p", ReleasedImage: true},
	}
	for want, req := range cases {
		if got := ValidateModelsRequest(req); got != want {
			t.Errorf("%+v: got %q, want %q", req, got, want)
		}
	}
}

func TestPickReleasedImage_HighestPublishedVersion(t *testing.T) {
	body := []byte(`{"data":[
		{"architecture":"amd64","creation_date":"2026-11-12","repositories":[{"registry":"registry.access.redhat.com","repository":"rhaii/vllm-cpu-rhel9","published":true,
			"manifest_list_digest":"sha256:347","tags":[{"name":"1794398071"},{"name":"3.4"},{"name":"3.4.7"},{"name":"3.4.7-1794398071"}]}]},
		{"architecture":"amd64","creation_date":"2026-11-03","repositories":[{"registry":"registry.access.redhat.com","repository":"rhaii/vllm-cpu-rhel9","published":true,
			"manifest_list_digest":"sha256:353","tags":[{"name":"3.5"},{"name":"3.5.3"}]}]},
		{"architecture":"amd64","creation_date":"2026-11-15","repositories":[{"registry":"registry.access.redhat.com","repository":"rhaii/vllm-cpu-rhel9","published":false,
			"manifest_list_digest":"sha256:360","tags":[{"name":"3.6.0"}]}]},
		{"architecture":"ppc64le","creation_date":"2026-11-16","repositories":[{"repository":"rhaii/vllm-cpu-rhel9","published":true,"manifest_list_digest":"sha256:999","tags":[{"name":"9.9"}]}]}]}`)
	img, err := pickReleasedImage(body, "rhaii/vllm-cpu-rhel9")
	if err != nil || img.Ref != "registry.redhat.io/rhaii/vllm-cpu-rhel9@sha256:353" || img.Version != "3.5.3" {
		t.Fatalf("got %+v, %v", img, err)
	}
	if _, err := pickReleasedImage([]byte(`{"data":[]}`), "rhaii/vllm-cpu-rhel9"); err == nil {
		t.Error("no image: want an error")
	}
}

// The catalog request: the repository's amd64 images, through the
// external client; an HTTP error is reported.
func TestFetchReleasedImage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":[{"architecture":"amd64","repositories":[{"repository":"rhaii/vllm-cpu-rhel9","published":true,"manifest_list_digest":"sha256:1","tags":[{"name":"3.4.1"}]}]}]}`)
	}))
	t.Cleanup(srv.Close)
	prev := redHatCatalogURL
	redHatCatalogURL = srv.URL
	t.Cleanup(func() { redHatCatalogURL = prev })
	img, err := fetchReleasedImage(context.Background(), "rhaii/vllm-cpu-rhel9")
	if err != nil || img.Version != "3.4.1" {
		t.Fatalf("got %+v, %v", img, err)
	}
	if gotPath != "/repositories/registry/registry.access.redhat.com/repository/rhaii/vllm-cpu-rhel9/images" || !strings.Contains(gotQuery, "filter=architecture%3D%3Damd64") {
		t.Errorf("request %s?%s", gotPath, gotQuery)
	}
	if _, err := fetchReleasedImage(context.Background(), "rhaii/missing"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err: %v", err)
	}
}

func TestMirrorCovers(t *testing.T) {
	repo := "registry.redhat.io/rhaii/vllm-cpu-rhel9"
	for source, want := range map[string]bool{
		"registry.redhat.io/rhaii":                true,
		"registry.redhat.io":                      true,
		"registry.redhat.io/rhaii/vllm-cpu-rhel9": true,
		"*.redhat.io":                             true,
		"registry.redhat.io/rhai":                 false,
		"registry.redhat.io/rhaii/vllm-cuda":      false,
		"quay.io":                                 false,
		"":                                        false,
	} {
		if got := mirrorCovers(source, repo); got != want {
			t.Errorf("%q: got %v", source, got)
		}
	}
	if imageRepository("registry.redhat.io/rhaii/vllm-cpu-rhel9:3.4@sha256:1") != repo || imageRepository("localhost:5000/a/b:1") != "localhost:5000/a/b" {
		t.Error("imageRepository")
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]float64{"250m": 0.25, "4": 4, "15500m": 15.5, "12Gi": 12 << 30, "64000Ki": 64000 << 10, "1G": 1e9, "1.5": 1.5} {
		if got, ok := parseQuantity(in); !ok || got != want {
			t.Errorf("%s: got %v %v", in, got, ok)
		}
	}
	if _, ok := parseQuantity("x"); ok {
		t.Error("x parsed")
	}
}

// Capacity: a warning when no amd64 worker has the requests free; none
// when one has.
func TestModelCapacityWarning(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/nodes/w1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"15500m","memory":"61Gi"}}}`)
	mc.f.putJSON("/api/v1/nodes/m1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"spec":{"taints":[{"effect":"NoSchedule"}]},"status":{"allocatable":{"cpu":"64","memory":"256Gi"}}}`)
	mc.f.putJSON("/api/v1/namespaces/a/pods/busy", `{"spec":{"nodeName":"w1","containers":[{"resources":{"requests":{"cpu":"14","memory":"8Gi"}}}]},"status":{"phase":"Running"}}`)
	w := modelCapacityWarning(mc.c, defaultEntry(), "")
	if !strings.Contains(w, "the worker with the most free CPU, w1, has 1.5 CPUs and 53.0 GiB free.") {
		t.Fatalf("warning: %q", w)
	}
	capacityCache.Purge()
	mc.f.putJSON("/api/v1/namespaces/a/pods/busy", `{"spec":{"nodeName":"w1","containers":[{"resources":{"requests":{"cpu":"2","memory":"8Gi"}}}]},"status":{"phase":"Running"}}`)
	if w := modelCapacityWarning(mc.c, defaultEntry(), ""); w != "" {
		t.Fatalf("warning: %q", w)
	}
}

// A read the ServiceAccount may not make is refused with its code and the
// hint to re-apply the template.
func TestModelsSetup_ReadErrorsKeepTheirCode(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.fail["GET "+testGatewayPath] = 403
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true})
	if res.Success || res.ErrorCode != "forbidden" || !strings.Contains(res.Message, "make upgrade") {
		t.Fatalf("got %+v", res)
	}
}

// notProgrammedGateway makes the gateway report what OpenShift's controller
// says while it makes the load balancer (seen live), for the first reads
// (all of them when reads is 0), then Programmed.
func notProgrammedGateway(mc *modelsCluster, reads int) {
	next := mc.f.onGet
	n := 0
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		next(k, obj)
		if k.plural != "gateways" {
			return
		}
		mc.mu.Lock()
		n++
		pending := reads == 0 || n <= reads
		mc.mu.Unlock()
		if pending {
			obj["status"] = map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": "Accepted", "status": "True"},
				map[string]interface{}{"type": "Programmed", "status": "False", "reason": "AddressNotAssigned",
					"message": `Failed to assign to any requested addresses: hostname "openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local" not found`},
			}}
		}
	}
}

// While the load balancer is made, progress is plain: the controller's
// condition reads as an error although it is the normal wait.
func TestModelsSetup_GatewayWaitSpeaksPlainly(t *testing.T) {
	mc := newModelsCluster(t)
	notProgrammedGateway(mc, 3)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	waited := false
	for _, e := range events {
		if strings.Contains(e.Message, "Failed to assign") {
			t.Errorf("progress shows the controller's condition: %q", e.Message)
		}
		waited = waited || strings.Contains(e.Message, "Waiting for OpenShift to create the gateway's load balancer")
	}
	if !waited {
		t.Error("no plain progress while the gateway was not programmed")
	}
}

// When the gateway is still not programmed at the end of the wait, the step
// names the controller's reason: then it is the clue.
func TestModelsSetup_GatewayNotProgrammedNamesTheReason(t *testing.T) {
	mc := newModelsCluster(t)
	notProgrammedGateway(mc, 0)
	_, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	for _, e := range events {
		if e.Step == "gateway-ready" && e.Status != "running" {
			if !strings.Contains(e.Message, "not programmed after") || !strings.Contains(e.Message, "The gateway controller says: Failed to assign to any requested addresses") {
				t.Errorf("gateway-ready: %q", e.Message)
			}
			return
		}
	}
	t.Fatal("no final gateway-ready step")
}

// Before set up, the status names the address set up would give the
// gateway, so the tab shows the real one instead of a placeholder.
func TestInferenceGatewayStatus_PlannedURLBeforeSetup(t *testing.T) {
	mc := newModelsCluster(t)
	if got := getInferenceGatewayStatus(mc.c, nil, nil); got.Deployed || got.PlannedURL != "https://inference.apps.example.com" || got.ExternalURL != "" {
		t.Errorf("not set up: deployed=%v planned=%q external=%q", got.Deployed, got.PlannedURL, got.ExternalURL)
	}
}

const testHardwareProfiles = "/apis/infrastructure.opendatahub.io/v1/namespaces/redhat-ods-applications/hardwareprofiles"

func hwProfile(name, cpuMax, memMax string, extra string) string {
	return `{"metadata":{"name":"` + name + `","resourceVersion":"41"` + extra + `},"spec":{"identifiers":[
		{"identifier":"cpu","minCount":1,"maxCount":` + cpuMax + `},
		{"identifier":"memory","minCount":"2Gi","maxCount":"` + memMax + `"}]}}`
}

// The model is recorded under the first enabled CPU-only profile without
// scheduling whose ranges hold its requests and limits, as the dashboard's
// wizard records it; the model's memory stays within RHOAI 3.6's
// default-profile (live: it measured 7.6 GiB with the preset's 4 GiB KV cache).
func TestModelsSetup_RecordsAFittingHardwareProfile(t *testing.T) {
	mc := newModelsCluster(t)
	putProfiles(mc,
		hwProfile("a-disabled", "8", "32Gi", `,"annotations":{"opendatahub.io/disabled":"true"}`),
		hwProfile("b-small", "4", "4Gi", ""),
		`{"metadata":{"name":"c-gpu"},"spec":{"identifiers":[{"identifier":"cpu","minCount":1,"maxCount":8},{"identifier":"memory","minCount":"2Gi","maxCount":"64Gi"},{"identifier":"nvidia.com/gpu","minCount":1,"maxCount":1}]}}`,
		`{"metadata":{"name":"d-pinned"},"spec":{"identifiers":[{"identifier":"cpu","minCount":1,"maxCount":8},{"identifier":"memory","minCount":"2Gi","maxCount":"64Gi"}],"scheduling":{"type":"Node","node":{"nodeSelector":{"pool":"x"}}}}}`,
		hwProfile("default-profile", "4", "8Gi", ""))
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	model := mc.f.get("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/qwen25-05b-instruct")
	ann := model["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if ann[hardwareProfileNameAnnotation] != "default-profile" || ann[hardwareProfileNamespaceAnnotation] != "redhat-ods-applications" || ann[hardwareProfileVersionAnnotation] != "41" {
		t.Errorf("hardware profile annotations: %v", ann)
	}
	main := model["spec"].(map[string]interface{})["template"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})
	res2, _ := json.Marshal(main["resources"])
	if string(res2) != `{"limits":{"cpu":"4","memory":"8Gi"},"requests":{"cpu":"4","ephemeral-storage":"1.5G","memory":"6Gi"}}` {
		t.Errorf("resources: %s", res2)
	}
	if env, _ := json.Marshal(main["env"]); !strings.Contains(string(env), `{"name":"VLLM_CPU_KVCACHE_SPACE","value":"2"}`) {
		t.Errorf("env: %s", env)
	}
	said := false
	for _, e := range events {
		said = said || strings.Contains(e.Message, "Hardware profile: redhat-ods-applications/default-profile")
	}
	if !said {
		t.Error("the run does not name the hardware profile")
	}
	st := modelState(mc.c, readModelForTest(t, mc), false, modelsRecord{})
	if st.HardwareProfile != "redhat-ods-applications/default-profile" {
		t.Errorf("status hardware profile = %q", st.HardwareProfile)
	}
}

// No profile fits: the model is set up without one, and the run says why.
func TestModelsSetup_NoFittingHardwareProfileSaysWhy(t *testing.T) {
	mc := newModelsCluster(t)
	putProfiles(mc, hwProfile("default-profile", "2", "8Gi", ""))
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	model := mc.f.get("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/qwen25-05b-instruct")
	if _, set := model["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})[hardwareProfileNameAnnotation]; set {
		t.Error("a profile was recorded although none fits")
	}
	for _, e := range events {
		if strings.Contains(e.Message, "Hardware profile: none") {
			if !strings.Contains(e.Message, "default-profile: cpu 4 is outside 1-2") {
				t.Errorf("reason: %q", e.Message)
			}
			// What the dashboard then shows, in its own words.
			if !strings.Contains(e.Message, `"No hardware profile"`) || !strings.Contains(e.Message, `"Hardware profile: Unknown"`) {
				t.Errorf("the run does not say what the dashboard shows: %q", e.Message)
			}
			return
		}
	}
	t.Error("the run does not say why there is no hardware profile")
}

func readModelForTest(t *testing.T, mc *modelsCluster) llmisvcObject {
	t.Helper()
	m, found, err := readToolModel(mc.c, "proj", "qwen25-05b-instruct")
	if err != nil || !found {
		t.Fatalf("read model: %v %v", found, err)
	}
	return m
}

// putProfiles stores each profile at its own path; the fake lists them.
func putProfiles(mc *modelsCluster, profiles ...string) {
	for _, p := range profiles {
		var meta struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		_ = json.Unmarshal([]byte(p), &meta)
		mc.f.putJSON(testHardwareProfiles+"/"+meta.Metadata.Name, p)
	}
}

const testModelDeployment = "/apis/apps/v1/namespaces/proj/deployments/qwen25-05b-instruct-kserve"

// putModelDeployment stores the model's workload Deployment as KServe makes
// it (its pods' labels) with the given replica counts.
func putModelDeployment(mc *modelsCluster, replicas, updated, available int) {
	mc.f.putJSON(testModelDeployment, fmt.Sprintf(`{"metadata":{"name":"qwen25-05b-instruct-kserve","generation":2,
		"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"replicas":1},
		"status":{"observedGeneration":2,"replicas":%d,"updatedReplicas":%d,"availableReplicas":%d}}`, replicas, updated, available))
}

// After a change, KServe reports Ready while the old pod still serves and
// the new one starts (seen live): setup waits for the rollout.
func TestModelsSetup_WaitsForTheRollout(t *testing.T) {
	mc := newModelsCluster(t)
	putModelDeployment(mc, 2, 1, 1) // the new pod starts, the old one serves
	// The model reports Ready from its third read; the rollout ends later.
	onModelRead(mc, func(n int) {
		if n == 6 {
			putModelDeployment(mc, 1, 1, 1)
		}
	})
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success || !strings.Contains(res.Message, "is ready in proj") {
		t.Fatalf("setup: %+v", res)
	}
	said := false
	for _, e := range events {
		said = said || strings.Contains(e.Message, "Rolling out the change")
	}
	if !said || mc.modelReads < 6 {
		t.Errorf("did not wait for the rollout (said=%v, model reads=%d)", said, mc.modelReads)
	}
}

// Ready for an older generation does not count: the change is not seen yet.
func TestModelsSetup_ReadyMustBeForTheAppliedGeneration(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 1
	stale := 0
	next := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		next(k, obj)
		if k.plural != "llminferenceservices" {
			return
		}
		mc.mu.Lock()
		stale++
		n := stale
		mc.mu.Unlock()
		ensureMap(obj, "metadata")["generation"] = 3.0
		st := ensureMap(obj, "status")
		st["conditions"] = []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}
		st["url"] = "https://inference.apps.example.com/proj/qwen25-05b-instruct"
		if n <= 4 {
			st["observedGeneration"] = 2.0
		} else {
			st["observedGeneration"] = 3.0
		}
	}
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success || !strings.Contains(res.Message, "is ready in proj") {
		t.Fatalf("setup: %+v", res)
	}
	if stale <= 4 {
		t.Errorf("Ready for an older generation counted: %d reads", stale)
	}
}

// putModelPod stores a pod of the model, its main container on image, with
// the given status.
func putModelPod(mc *modelsCluster, name, image, status string) {
	mc.f.putJSON("/api/v1/namespaces/proj/pods/"+name, `{"metadata":{"name":"`+name+`","labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"containers":[{"name":"main","image":"`+image+`"}]},"status":`+status+`}`)
}

// putModelDeploymentImage stores the model's Deployment with its pod
// template's main image, its generation and observed generation, and the
// given replica counts.
func putModelDeploymentImage(mc *modelsCluster, image string, generation, observed, replicas, updated, available int) {
	mc.f.putJSON(testModelDeployment, fmt.Sprintf(`{"metadata":{"name":"qwen25-05b-instruct-kserve","generation":%d,
		"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"main","image":%q}]}}},
		"status":{"observedGeneration":%d,"replicas":%d,"updatedReplicas":%d,"availableReplicas":%d}}`, generation, image, observed, replicas, updated, available))
}

const testOldPullFailure = `{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ImagePullBackOff","message":"reading manifest sha256:978a: manifest unknown"}}}]}`

// onModelRead runs fn(n) before the n-th GET of the model.
func onModelRead(mc *modelsCluster, fn func(n int)) {
	mc.f.beforeServe = func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/llminferenceservices/qwen25-05b-instruct") {
			mc.mu.Lock()
			n := mc.modelReads + 1
			mc.mu.Unlock()
			fn(n)
		}
	}
}

// Switching a failing model to another image: the old pod still fails to
// pull while the new one starts. Only the new revision's pods count.
func TestModelsSetup_OldRevisionPodDoesNotFailTheChange(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 8
	putModelDeploymentImage(mc, "quay.io/new/vllm:1", 3, 3, 2, 1, 0)
	putModelPod(mc, "qwen25-05b-instruct-kserve-old", testPresetImage, testOldPullFailure)
	putModelPod(mc, "qwen25-05b-instruct-kserve-new", "quay.io/new/vllm:1", `{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ContainerCreating"}}}]}`)
	onModelRead(mc, func(n int) {
		if n == 8 {
			putModelDeploymentImage(mc, "quay.io/new/vllm:1", 3, 3, 1, 1, 1)
		}
	})
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success || !strings.Contains(res.Message, "is ready in proj") {
		t.Fatalf("the old pod failed the change: %+v", res)
	}
}

// While the Deployment's controller has not seen the change, every pod is
// the previous revision's: their problems are not the change's.
func TestModelsSetup_PodProblemsWaitForTheObservedGeneration(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 8
	putModelDeploymentImage(mc, "quay.io/new/vllm:1", 4, 3, 1, 1, 0)
	putModelPod(mc, "qwen25-05b-instruct-kserve-old", "quay.io/new/vllm:1", testOldPullFailure)
	onModelRead(mc, func(n int) {
		if n == 8 {
			mc.f.mu.Lock()
			delete(mc.f.objects, fakeKey{gv: "v1", ns: "proj", plural: "pods", name: "qwen25-05b-instruct-kserve-old"})
			mc.f.mu.Unlock()
			putModelDeploymentImage(mc, "quay.io/new/vllm:1", 4, 4, 1, 1, 1)
		}
	})
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("a pod of the previous revision failed the change: %+v", res)
	}
	said := false
	for _, e := range events {
		said = said || strings.Contains(e.Message, "Waiting for KServe to apply the change")
	}
	if !said {
		t.Error("the wait did not say it waits for the change to be applied")
	}
}

// KServe reports Ready while the old pod serves; the new pod crash-loops:
// the rollout wait stops with the new pod's problem.
func TestModelsSetup_RolloutStopsOnAFailingNewPod(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 1
	putModelDeploymentImage(mc, "quay.io/new/vllm:1", 3, 3, 2, 1, 1)
	putModelPod(mc, "qwen25-05b-instruct-kserve-old", testPresetImage, `{"containerStatuses":[{"name":"main","ready":true,"state":{"running":{}}}]}`)
	putModelPod(mc, "qwen25-05b-instruct-kserve-new", "quay.io/new/vllm:1", `{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}`)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "keeps crashing") {
		t.Fatalf("the rollout did not stop on the new pod's crash loop: %+v", res)
	}
}

// Every pod is judged: a problem on the second pod is not hidden by the first.
func TestDescribeModelPods_ChecksEveryPod(t *testing.T) {
	var ok, bad modelPod
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"a"},"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ContainerCreating"}}}]}}`), &ok)
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"b"},"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`), &bad)
	if got := describeModelPods([]modelPod{ok, bad}, nil); !strings.Contains(got.problem, "keeps crashing") {
		t.Fatalf("got %+v", got)
	}
}

// One route attached to both listeners is one route, not two.
func TestAttachedRoutes_CountsARouteOnTwoListenersOnce(t *testing.T) {
	var g gatewayObject
	_ = json.Unmarshal([]byte(`{"status":{"listeners":[{"name":"https-external","attachedRoutes":1},{"name":"https","attachedRoutes":1}]}}`), &g)
	if n := g.attachedRoutes(); n != 1 {
		t.Fatalf("attached routes %d", n)
	}
}

// Another Gateway uses the class: the teardown deletes this tool's Gateway
// and options, keeps the class, and says so.
func TestGatewayTeardown_KeepsTheClassWhileAnotherGatewayUsesIt(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/apis/gateway.networking.k8s.io/v1/namespaces/team/gateways/team-gw", `{"metadata":{"name":"team-gw","namespace":"team"},"spec":{"gatewayClassName":"openshift-ai-inference"}}`)
	res, _ := TeardownInferenceGateway(mc.c, false)
	if !res.Success || mc.f.has(testGatewayPath) || mc.f.has(testGatewayCM) {
		t.Fatalf("got %+v", res)
	}
	if !mc.f.has(testGatewayClass) {
		t.Fatal("deleted the class another Gateway uses")
	}
	if !strings.Contains(res.Message, "GatewayClass openshift-ai-inference was kept: Gateway team/team-gw uses it") {
		t.Errorf("message: %s", res.Message)
	}
	// Once no other Gateway uses it, the class goes too.
	mc.f.mu.Lock()
	delete(mc.f.objects, fakeKey{gv: "gateway.networking.k8s.io/v1", ns: "team", plural: "gateways", name: "team-gw"})
	mc.f.mu.Unlock()
	if res, _ := TeardownInferenceGateway(mc.c, false); !res.Success || mc.f.has(testGatewayClass) {
		t.Fatalf("second teardown: %+v", res)
	}
}

// Deleting the models and the gateway in one operation: the controller
// updates attachedRoutes after the routes go, so the teardown waits for the
// count (bounded, on the clock) instead of refusing at once.
func TestGatewayTeardown_WaitsForRoutesOfModelsJustDeleted(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.finalizeOnGet = true
	reads := 0
	next := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		next(k, obj)
		if k.plural == "gateways" {
			reads++
			attached := 0
			if reads <= 3 {
				attached = 1
			}
			ensureMap(obj, "status")["listeners"] = []interface{}{map[string]interface{}{"name": "https-external", "attachedRoutes": attached}}
		}
	}
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Gateway: true, Project: "proj"})
	if !res.Success || mc.f.has(testGatewayPath) {
		t.Fatalf("got %+v", res)
	}
}

const testISVCConfig = "/api/v1/namespaces/redhat-ods-applications/configmaps/inferenceservice-config"

// KServe's default gateway (inferenceservice-config, ingress
// kserveIngressGateway) is read at run time. When it is not the gateway
// this tool manages, a model whose route is the default would not use it:
// setup refuses before writing anything.
func TestModelsSetup_RefusesWhenKServesDefaultGatewayIsAnother(t *testing.T) {
	for _, gw := range []string{"istio-system/kserve-gateway", "kserve-gateway"} {
		mc := newModelsCluster(t)
		mc.f.putJSON(testISVCConfig, `{"data":{"ingress":"{\"kserveIngressGateway\":\"`+gw+`\"}"}}`)
		before := len(mc.f.mutations())
		res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
		want := gw
		if !strings.Contains(gw, "/") {
			want = "redhat-ods-applications/" + gw
		}
		if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "KServe's default gateway is "+want) {
			t.Fatalf("%s: got %+v", gw, res)
		}
		if m := mc.f.mutations()[before:]; len(m) != 0 {
			t.Errorf("%s: wrote %v", gw, m)
		}
	}
	// The default named as this tool's gateway: setup runs.
	mc := newModelsCluster(t)
	mc.f.putJSON(testISVCConfig, `{"data":{"ingress":"{\"kserveIngressGateway\":\"openshift-ingress/openshift-ai-inference\"}"}}`)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("matching default: %+v", res)
	}
}

// A model on KServe's default route does not use this gateway when the
// default is another one: it does not hold the teardown. Another default
// is no warning: models choose the gateway they use (models_exposure.go).
func TestGatewayTeardown_ADefaultRouteElsewhereDoesNotBlock(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON(testISVCConfig, `{"data":{"ingress":"{\"kserveIngressGateway\":\"istio-system/kserve-gateway\"}"}}`)
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/team/llminferenceservices/granite", `{"metadata":{"name":"granite","namespace":"team"},"spec":{"router":{"gateway":{}}}}`)
	st, _ := GetResourcesStatus(mc.c)
	if len(st.InferenceGateway.UsedBy) != 0 || strings.Contains(st.InferenceGateway.Warning, "KServe's default gateway") {
		t.Errorf("status: %+v", st.InferenceGateway)
	}
	if res, _ := TeardownInferenceGateway(mc.c, false); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
}

// Setting the model up again in its project: the room its own pod holds
// is the room the new pod gets, so that pod is not counted against it.
func TestModelCapacityWarning_IgnoresTheModelsOwnPod(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/nodes/w1", `{"metadata":{"labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"15500m","memory":"61Gi"}}}`)
	mc.f.putJSON("/api/v1/namespaces/a/pods/busy", `{"metadata":{"namespace":"a"},"spec":{"nodeName":"w1","containers":[{"resources":{"requests":{"cpu":"10","memory":"8Gi"}}}]},"status":{"phase":"Running"}}`)
	mc.f.putJSON("/api/v1/namespaces/proj/pods/qwen", `{"metadata":{"namespace":"proj","labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},"spec":{"nodeName":"w1","containers":[{"resources":{"requests":{"cpu":"4","memory":"6Gi"}}}]},"status":{"phase":"Running"}}`)
	if w := modelCapacityWarning(mc.c, defaultEntry(), "proj"); w != "" {
		t.Fatalf("its own pod was counted: %q", w)
	}
	// In another project the running copy is a real occupant.
	if w := modelCapacityWarning(mc.c, defaultEntry(), "other"); w == "" {
		t.Fatal("a copy in another project was not counted")
	}
}

// A profile the dashboard shows only for workbenches is not recorded on a
// model (rhoai-3.6 isHardwareProfileVisibleIn: a list without
// "model-serving" hides it from model serving).
func TestModelsSetup_SkipsAProfileNotVisibleForModelServing(t *testing.T) {
	mc := newModelsCluster(t)
	putProfiles(mc,
		hwProfile("a-workbench", "8", "32Gi", `,"annotations":{"opendatahub.io/dashboard-feature-visibility":"[\"workbench\"]"}`),
		hwProfile("b-serving", "8", "32Gi", `,"annotations":{"opendatahub.io/dashboard-feature-visibility":"[\"workbench\",\"model-serving\"]"}`))
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	model := mc.f.get(testModelPath)
	ann := model["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if ann[hardwareProfileNameAnnotation] != "b-serving" {
		t.Errorf("hardware profile %v", ann[hardwareProfileNameAnnotation])
	}
	var p hardwareProfileObject
	_ = json.Unmarshal([]byte(hwProfile("x", "8", "32Gi", `,"annotations":{"opendatahub.io/dashboard-feature-visibility":"not json"}`)), &p)
	if why := p.fits(defaultEntry()); why != "" {
		t.Errorf("an unreadable visibility hides the profile (the dashboard shows it): %s", why)
	}
}

// The confirmation promised the newest released image: when the catalog
// cannot be read, setup refuses and names the catalog's error instead of
// quietly using the preset's image.
func TestModelsSetup_ReleasedImageLookupFailureRefuses(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testSubscription, `{"spec":{"source":"`+CatalogName+`"}}`)
	prev := lookupReleasedImage
	t.Cleanup(func() { lookupReleasedImage = prev })
	lookupReleasedImage = func(context.Context, string) (releasedImage, error) {
		return releasedImage{}, fmt.Errorf("catalog.redhat.com answered 503")
	}
	before := len(mc.f.mutations())
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "catalog.redhat.com answered 503") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
}
