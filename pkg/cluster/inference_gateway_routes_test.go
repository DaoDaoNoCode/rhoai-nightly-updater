package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The label a cluster-admin puts on a namespace so that its routes may
// attach to the inference gateway this tool creates.
const testRoutesLabel = "rhoai-nightly-updater/inference-routes"

// openGateway turns the tool's gateway into one an earlier version made:
// both listeners accept routes from every namespace.
func openGateway(t *testing.T, mc *modelsCluster) {
	t.Helper()
	gw := mc.f.get(testGatewayPath)
	if gw == nil {
		t.Fatal("no gateway")
	}
	for _, l := range gw["spec"].(map[string]interface{})["listeners"].([]interface{}) {
		l.(map[string]interface{})["allowedRoutes"] = map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}}
	}
	mc.f.put(testGatewayPath, gw)
}

func listenerAllowed(t *testing.T, mc *modelsCluster) []gatewayListener {
	t.Helper()
	raw, _ := json.Marshal(mc.f.get(testGatewayPath))
	var gw gatewayObject
	if err := json.Unmarshal(raw, &gw); err != nil {
		t.Fatal(err)
	}
	return gw.Spec.Listeners
}

// The gateway the tool creates admits routes only from namespaces with the
// label, on both listeners: the external one and the in-cluster one the
// playground (the dashboard's gen-ai backend) calls models through.
func TestInferenceGateway_AdmitsOnlyLabelledNamespaces(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	listeners := listenerAllowed(t, mc)
	if len(listeners) != 2 {
		t.Fatalf("listeners: %+v", listeners)
	}
	for _, l := range listeners {
		ns := l.AllowedRoutes.Namespaces
		if ns.From != "Selector" || ns.Selector == nil || len(ns.Selector.MatchLabels) != 1 || ns.Selector.MatchLabels[testRoutesLabel] != "true" || len(ns.Selector.MatchExpressions) != 0 {
			t.Errorf("listener %s allowedRoutes: %+v", l.Name, ns)
		}
		one := gatewayObject{}
		one.Metadata.Namespace = inferenceGatewayNamespace
		one.Spec.Listeners = []gatewayListener{l}
		if got := one.admits("proj", map[string]string{testRoutesLabel: "true"}); got != "yes" {
			t.Errorf("listener %s does not admit a labelled project: %s", l.Name, got)
		}
		if got := one.admits("attacker", map[string]string{"opendatahub.io/dashboard": "true"}); got != "no" {
			t.Errorf("listener %s admits an unlabelled project: %s", l.Name, got)
		}
	}
}

// A project without the label is refused before anything is written, with
// the command a cluster-admin runs; with a gateway still to make and with
// the tool's gateway already there.
func TestModelsSetup_RefusesAnUnlabelledProjectBeforeWriting(t *testing.T) {
	for _, gatewayFirst := range []bool{false, true} {
		mc := newModelsCluster(t)
		if gatewayFirst {
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
				t.Fatalf("gateway setup: %+v", res)
			}
		}
		mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
		before := len(mc.f.mutations())
		res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: !gatewayFirst, Project: "proj"})
		if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "oc label namespace proj "+testRoutesLabel+"=true") || !strings.Contains(res.Message, "cluster-admin") {
			t.Fatalf("gateway first %v: got %+v", gatewayFirst, res)
		}
		if m := mc.f.mutations()[before:]; len(m) != 0 {
			t.Errorf("gateway first %v: wrote %v", gatewayFirst, m)
		}
		if got := finalSteps(events); got != "check=failed" {
			t.Errorf("steps: %s", got)
		}
	}
}

// The status names the label and the namespaces that carry it.
func TestInferenceGatewayStatus_LabelledProjects(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/namespaces/other", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	gw := st.InferenceGateway
	if gw.RoutesLabel != testRoutesLabel || strings.Join(gw.LabelledProjects, ",") != "proj" || gw.RoutesLabelProblem != "" {
		t.Fatalf("gateway: %+v", gw)
	}

	mc.f.fail["GET /api/v1/namespaces"] = 500
	if got := getInferenceGatewayStatus(mc.c, nil, nil); got.RoutesLabelProblem == "" || len(got.LabelledProjects) != 0 {
		t.Fatalf("an unreadable label state must be said: %+v", got)
	}
}

// A gateway this tool created earlier with routes from every namespace:
// the status warns, and restricting it refuses while a model routes
// through it from a project without the label, whoever made the model.
// It never detaches a model.
func TestInferenceGatewayRestrict_WaitsForEveryModelProjectsLabel(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	openGateway(t, mc)
	// Someone else's model in another project, on the default route, and
	// one that inherits its route from a preset (only its address says it
	// is behind this gateway).
	mc.f.putJSON("/api/v1/namespaces/team", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	mc.f.putJSON("/api/v1/namespaces/lab", `{"metadata":{"labels":{}}}`)
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/team/llminferenceservices/granite", `{"spec":{"router":{"route":{},"gateway":{}}}}`)
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/lab/llminferenceservices/opt", `{"spec":{},"status":{"url":"https://inference.apps.example.com/lab/opt"}}`)

	st, _ := GetResourcesStatus(mc.c)
	gw := st.InferenceGateway
	if !gw.OpenToAllNamespaces || !gw.ManagedByTool || strings.Join(gw.UnlabelledModelProjects, ",") != "lab,team" ||
		!strings.Contains(gw.RestrictBlockedReason, "lab") || !strings.Contains(gw.RestrictBlockedReason, "team") {
		t.Fatalf("status: %+v", gw)
	}

	before := len(mc.f.mutations())
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true})
	if res.Success || res.ErrorCode != "prerequisites" ||
		!strings.Contains(res.Message, "oc label namespace lab "+testRoutesLabel+"=true") || !strings.Contains(res.Message, "oc label namespace team "+testRoutesLabel+"=true") ||
		strings.Contains(res.Message, "namespace proj ") {
		t.Fatalf("got %+v", res)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}

	// Models that cannot be listed: refused, nothing written.
	mc.f.fail["GET /apis/serving.kserve.io/v1alpha2/llminferenceservices"] = 500
	if res, _ = runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); res.Success || len(mc.f.mutations()[before:]) != 0 {
		t.Fatalf("an unknown model list must refuse: %+v", res)
	}
	delete(mc.f.fail, "GET /apis/serving.kserve.io/v1alpha2/llminferenceservices")

	// Once a cluster-admin labelled them, the selector is applied.
	for _, ns := range []string{"team", "lab"} {
		mc.f.putJSON("/api/v1/namespaces/"+ns, `{"metadata":{"labels":{"`+testRoutesLabel+`":"true"}}}`)
	}
	if res, _ = runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("restrict: %+v", res)
	}
	for _, l := range listenerAllowed(t, mc) {
		if l.AllowedRoutes.Namespaces.From != "Selector" {
			t.Errorf("listener %s still: %+v", l.Name, l.AllowedRoutes.Namespaces)
		}
	}
	st, _ = GetResourcesStatus(mc.c)
	if st.InferenceGateway.OpenToAllNamespaces || st.InferenceGateway.RestrictBlockedReason != "" || len(st.InferenceGateway.UnlabelledModelProjects) != 0 {
		t.Fatalf("status after: %+v", st.InferenceGateway)
	}
}

// A gateway someone else made is never changed; the status says that it
// accepts routes from every namespace, and a model may still use it.
func TestInferenceGatewayStatus_ForeignOpenGateway(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testGatewayPath, `{"spec":{"gatewayClassName":"openshift-ai-inference","listeners":[{"name":"https","hostname":"x.example.com","allowedRoutes":{"namespaces":{"from":"All"}}}]}}`)
	st, _ := GetResourcesStatus(mc.c)
	gw := st.InferenceGateway
	if gw.ManagedByTool || !gw.OpenToAllNamespaces || gw.RestrictBlockedReason != "" {
		t.Fatalf("status: %+v", gw)
	}
	// A project without the label may use someone else's open gateway:
	// its owner decides who attaches.
	mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, m := range mc.f.mutations() {
		if strings.Contains(m, "/gateways") || strings.Contains(m, "/namespaces/proj ") {
			t.Errorf("wrote %s", m)
		}
	}
}
