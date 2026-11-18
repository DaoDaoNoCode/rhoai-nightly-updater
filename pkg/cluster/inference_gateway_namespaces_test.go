package cluster

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The gateway this tool creates admits routes only from the namespaces it
// lists by name, as RHOAI documents for the openshift-ai-inference Gateway
// (odh-gitops rhai-on-openshift-chart values.yaml: from Selector,
// matchExpressions kubernetes.io/metadata.name In [...]; RHOAI 3.4 llm-d
// guide: "Ensure your model deployment namespace is listed in the values
// array").

const testNameKey = "kubernetes.io/metadata.name"

// gatewayAllowed returns, for each listener, the namespaces its
// allowedRoutes lists. A listener in any other shape (from All, labels,
// more than one expression) fails the test.
func gatewayAllowed(t *testing.T, mc *modelsCluster) map[string][]string {
	t.Helper()
	raw, _ := json.Marshal(mc.f.get(testGatewayPath))
	var gw struct {
		Spec struct {
			Listeners []struct {
				Name          string `json:"name"`
				AllowedRoutes struct {
					Namespaces struct {
						From     string `json:"from"`
						Selector struct {
							MatchLabels      map[string]string `json:"matchLabels"`
							MatchExpressions []struct {
								Key      string   `json:"key"`
								Operator string   `json:"operator"`
								Values   []string `json:"values"`
							} `json:"matchExpressions"`
						} `json:"selector"`
					} `json:"namespaces"`
				} `json:"allowedRoutes"`
			} `json:"listeners"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &gw); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, l := range gw.Spec.Listeners {
		ns := l.AllowedRoutes.Namespaces
		if ns.From != "Selector" || len(ns.Selector.MatchLabels) != 0 || len(ns.Selector.MatchExpressions) != 1 ||
			ns.Selector.MatchExpressions[0].Key != testNameKey || ns.Selector.MatchExpressions[0].Operator != "In" {
			t.Fatalf("listener %s allowedRoutes is not a names list: %+v", l.Name, ns)
		}
		out[l.Name] = ns.Selector.MatchExpressions[0].Values
	}
	if len(out) != 2 {
		t.Fatalf("listeners: %v", out)
	}
	return out
}

// wantAllowed checks that both listeners list exactly want (any order).
func wantAllowed(t *testing.T, mc *modelsCluster, want ...string) {
	t.Helper()
	sort.Strings(want)
	for name, got := range gatewayAllowed(t, mc) {
		sorted := append([]string(nil), got...)
		sort.Strings(sorted)
		if strings.Join(sorted, ",") != strings.Join(want, ",") {
			t.Errorf("listener %s lists %v, want %v", name, got, want)
		}
	}
}

// setListeners gives both listeners of the stored gateway allowedRoutes
// (JSON), as an edit by someone else would: a new resourceVersion.
func setListeners(t *testing.T, mc *modelsCluster, allowedRoutes string) {
	t.Helper()
	var allowed map[string]interface{}
	if err := json.Unmarshal([]byte(allowedRoutes), &allowed); err != nil {
		t.Fatal(err)
	}
	gw := mc.f.get(testGatewayPath)
	if gw == nil {
		t.Fatal("no gateway")
	}
	for _, l := range gw["spec"].(map[string]interface{})["listeners"].([]interface{}) {
		l.(map[string]interface{})["allowedRoutes"] = allowed
	}
	mc.f.bumpRV(gw["metadata"].(map[string]interface{}))
	mc.f.put(testGatewayPath, gw)
}

func namesList(values ...string) string {
	raw, _ := json.Marshal(values)
	return `{"namespaces":{"from":"Selector","selector":{"matchExpressions":[{"key":"` + testNameKey + `","operator":"In","values":` + string(raw) + `}]}}}`
}

// someoneElsesModel adds a dashboard project with an LLMInferenceService
// this tool did not create, on KServe's default route.
func someoneElsesModel(mc *modelsCluster, project string) {
	mc.f.putJSON("/api/v1/namespaces/"+project, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/"+project+"/llminferenceservices/granite", `{"spec":{"router":{"route":{},"gateway":{}}}}`)
}

func indexWithPrefix(list []string, prefix string) int {
	for i, s := range list {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// Both listeners list the gateway's own namespace (so the list is never
// empty, as RHOAI's own data-science-gateway lists it) and the model's
// project; nothing else attaches.
func TestInferenceGateway_ListsTheModelsProjectByName(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	wantAllowed(t, mc, "openshift-ingress", "proj")

	var gw gatewayObject
	raw, _ := json.Marshal(mc.f.get(testGatewayPath))
	_ = json.Unmarshal(raw, &gw)
	if got := gw.admits("proj", nil); got != "yes" {
		t.Errorf("proj: %s", got)
	}
	if got := gw.admits("attacker", map[string]string{"opendatahub.io/dashboard": "true"}); got != "no" {
		t.Errorf("an unlisted project is admitted: %s", got)
	}

	// The gateway alone: its own namespace only.
	mc2 := newModelsCluster(t)
	if res, _ := runModels(t, mc2.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway setup: %+v", res)
	}
	wantAllowed(t, mc2, "openshift-ingress")
}

// A model set up in a project the tool's gateway does not list yet: the
// project is added (keeping what someone else added) before the model is
// created; no refusal, no command for anyone.
func TestModelsSetup_AddsTheProjectToTheGatewayBeforeTheModel(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	setListeners(t, mc, namesList("openshift-ingress", "proj", "by-hand"))
	mc.f.putJSON("/api/v1/namespaces/team", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	before := len(mc.f.mutations())
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "team"})
	if !res.Success {
		t.Fatalf("setup in team: %+v", res)
	}
	if got := finalSteps(events); got != "check=success model=success model-ready=success" {
		t.Errorf("steps: %s", got)
	}
	wantAllowed(t, mc, "by-hand", "openshift-ingress", "proj", "team")
	m := mc.f.mutations()[before:]
	gwWrite, modelWrite := indexWithPrefix(m, "PATCH "+testGatewayPath), indexWithPrefix(m, "POST /apis/serving.kserve.io/v1alpha2/namespaces/team/llminferenceservices")
	if gwWrite < 0 || modelWrite < 0 || gwWrite > modelWrite {
		t.Errorf("the gateway must list team before the model is created: %v", m)
	}
	for _, w := range m {
		if strings.Contains(w, "/gatewayclasses/") || strings.Contains(w, "/namespaces/team ") || strings.HasSuffix(w, "/namespaces/team") {
			t.Errorf("wrote %s", w)
		}
	}

	// Listed already: the gateway is not written again.
	before = len(mc.f.mutations())
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "team", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("second model: %+v", res)
	}
	for _, w := range mc.f.mutations()[before:] {
		if strings.Contains(w, "/gateways/") {
			t.Errorf("wrote the gateway again: %s", w)
		}
	}
}

// Someone changes the list between the tool's read and its write: the
// write is refused (resourceVersion), and the tool reads again, so their
// entry is kept.
func TestModelsSetup_GatewayEditedMeanwhileKeepsTheOtherEdit(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/api/v1/namespaces/team", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	// The edit lands after the check read the gateway, as the write starts.
	edited := false
	res, err := RunModels(mc.c, types.ModelsRequest{Action: "setup", Project: "team"}, func(e UpdateStepEvent) {
		if e.Step == "model" && strings.HasPrefix(e.Message, "Adding 'team'") && !edited {
			edited = true
			setListeners(t, mc, namesList("openshift-ingress", "proj", "late"))
		}
	})
	if err != nil || !res.Success {
		t.Fatalf("setup in team: %+v %v", res, err)
	}
	if !edited {
		t.Fatal("the gateway was not written")
	}
	wantAllowed(t, mc, "late", "openshift-ingress", "proj", "team")
}

// A gateway a pre-release build made (routes from every namespace, or
// the namespace label it used) becomes the names list at the next write,
// with the projects of this tool's own models that route through it (by
// the tool's record): a dummy LLMInferenceService anyone makes must not
// get its project listed, since that lets the project's editors attach
// routes to the shared gateway. Other projects with models on it are
// reported as Allow candidates. When the models or the record cannot be
// read, nothing is written.
func TestInferenceGateway_ConvertsAPreReleaseGateway(t *testing.T) {
	for name, allowed := range map[string]string{
		"from All":    `{"namespaces":{"from":"All"}}`,
		"label rule":  `{"namespaces":{"from":"Selector","selector":{"matchLabels":{"rhoai-nightly-updater/inference-routes":"true"}}}}`,
		"mixed shape": `{"namespaces":{"from":"Selector","selector":{"matchExpressions":[{"key":"` + testNameKey + `","operator":"In","values":["proj"]}],"matchLabels":{"x":"y"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			mc := newModelsCluster(t)
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			setListeners(t, mc, allowed)
			someoneElsesModel(mc, "team")
			// A model that inherits its route from a preset: only its
			// address says it is behind this gateway.
			mc.f.putJSON("/api/v1/namespaces/lab", `{"metadata":{"labels":{}}}`)
			mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/lab/llminferenceservices/opt", `{"spec":{},"status":{"url":"https://inference.apps.example.com/lab/opt"}}`)
			mc.f.putJSON("/api/v1/namespaces/dev", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)

			// The models cannot be listed: nothing is written.
			mc.f.fail["GET /apis/serving.kserve.io/v1alpha2/llminferenceservices"] = 500
			before := len(mc.f.mutations())
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "dev"}); res.Success || len(mc.f.mutations()[before:]) != 0 {
				t.Fatalf("an unknown model list must refuse before any write: %+v %v", res, mc.f.mutations()[before:])
			}
			delete(mc.f.fail, "GET /apis/serving.kserve.io/v1alpha2/llminferenceservices")
			// The tool's record cannot be read: nothing is written either.
			recPath := "GET /api/v1/namespaces/" + getActivityNamespace() + "/configmaps/" + snapshotConfigMapName
			mc.f.fail[recPath] = 500
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "dev"}); res.Success || len(mc.f.mutations()[before:]) != 0 {
				t.Fatalf("an unreadable record must refuse before any write: %+v %v", res, mc.f.mutations()[before:])
			}
			delete(mc.f.fail, recPath)

			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "dev"}); !res.Success {
				t.Fatalf("setup in dev: %+v", res)
			}
			wantAllowed(t, mc, "dev", "openshift-ingress", "proj")
			st, err := GetResourcesStatus(mc.c)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(st.InferenceGateway.NotAllowedModelProjects, ","); got != "lab,team" {
				t.Fatalf("Allow candidates = %q, want lab,team", got)
			}
		})
	}
}

// The status lists the projects the gateway allows and the projects whose
// models route through it that it does not allow (a model deployed from
// the dashboard). Allow adds such a project; it writes nothing else.
func TestModelsAllow_AddsAProjectWhoseModelTheGatewayDoesNotList(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	someoneElsesModel(mc, "team")
	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	gw := st.InferenceGateway
	if strings.Join(gw.AllowedProjects, ",") != "openshift-ingress,proj" || strings.Join(gw.NotAllowedModelProjects, ",") != "team" || gw.AllProjectsAllowed ||
		strings.Join(gw.NotAllowedModels, ",") != "team/granite" {
		t.Fatalf("status: %+v", gw)
	}

	before := len(mc.f.mutations())
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "allow", Project: "team"})
	if !res.Success {
		t.Fatalf("allow: %+v", res)
	}
	if got := finalSteps(events); got != "check=success gateway=success" {
		t.Errorf("steps: %s", got)
	}
	wantAllowed(t, mc, "openshift-ingress", "proj", "team")
	for _, w := range mc.f.mutations()[before:] {
		if w != "PATCH "+testGatewayPath {
			t.Errorf("allow wrote %s", w)
		}
	}
	st, _ = GetResourcesStatus(mc.c)
	if len(st.InferenceGateway.NotAllowedModelProjects) != 0 || len(st.InferenceGateway.NotAllowedModels) != 0 || strings.Join(st.InferenceGateway.AllowedProjects, ",") != "openshift-ingress,proj,team" {
		t.Fatalf("status after: %+v", st.InferenceGateway)
	}

	// Allow needs a project that exists, and the tool's gateway.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "allow"}); res.Success || res.ErrorCode != "validation" {
		t.Errorf("no project: %+v", res)
	}
	mc2 := newModelsCluster(t)
	if res, _ := runModels(t, mc2.c, types.ModelsRequest{Action: "allow", Project: "proj"}); res.Success || res.ErrorCode != "prerequisites" {
		t.Errorf("no gateway: %+v", res)
	}
}

// A gateway someone else made is never changed. The status says which
// projects it allows; a model setup in a project it does not list, and
// Allow, are refused with RHOAI's documented step for a cluster admin.
func TestInferenceGateway_SomeoneElsesGatewayIsNeverChanged(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testGatewayPath, `{"spec":{"gatewayClassName":"openshift-ai-inference","listeners":[{"name":"https","hostname":"x.example.com","allowedRoutes":`+namesList("openshift-ingress", "other")+`}]}}`)
	someoneElsesModel(mc, "team")
	st, _ := GetResourcesStatus(mc.c)
	gw := st.InferenceGateway
	if gw.ManagedByTool || strings.Join(gw.AllowedProjects, ",") != "openshift-ingress,other" || strings.Join(gw.NotAllowedModelProjects, ",") != "team" {
		t.Fatalf("status: %+v", gw)
	}
	for _, req := range []types.ModelsRequest{{Action: "setup", Project: "proj"}, {Action: "allow", Project: "proj"}} {
		res, _ := runModels(t, mc.c, req)
		if res.Success || !strings.Contains(res.Message, "values") || !strings.Contains(res.Message, "cluster admin") {
			t.Errorf("%s: %+v", req.Action, res)
		}
	}
	if m := mc.f.mutations(); len(m) != 0 {
		t.Errorf("wrote %v", m)
	}

	// From every namespace: said, and a model may use it.
	mc2 := newModelsCluster(t)
	mc2.f.putJSON(testGatewayPath, `{"spec":{"gatewayClassName":"openshift-ai-inference","listeners":[{"name":"https","hostname":"x.example.com","allowedRoutes":{"namespaces":{"from":"All"}}}]}}`)
	st, _ = GetResourcesStatus(mc2.c)
	if !st.InferenceGateway.AllProjectsAllowed || st.InferenceGateway.AllowedProjects != nil {
		t.Fatalf("status: %+v", st.InferenceGateway)
	}
}

// Deleting the tool's model keeps its project on the list: another model
// there (the dashboard's) may use the gateway, and removing it would
// detach that model.
func TestModelsTeardown_KeepsTheProjectOnTheGateway(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj"}); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	wantAllowed(t, mc, "openshift-ingress", "proj")
}
