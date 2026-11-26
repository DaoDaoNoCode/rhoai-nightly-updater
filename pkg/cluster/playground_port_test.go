package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	noPortSvc  = "https://smollm2-135m-instruct-kserve-workload-svc.proj.svc.cluster.local"
	gatewayMdl = "https://openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local/proj/qwen25-05b-instruct"
)

// A model's address "does not connect as reported" only when the address
// that connects differs from the one KServe reports: a Service named
// without the port it listens on. An address with its port already, a
// gateway path, none, or one whose Service cannot be read is not flagged.
func TestAddressMissesPort(t *testing.T) {
	for _, c := range []struct {
		name              string
		reported, working string
		want              bool
	}{
		{"Service without its port", noPortSvc, noPortSvc + ":8000", true},
		{"port reported already", noPortSvc + ":8000", noPortSvc + ":8000", false},
		{"Service on 443", noPortSvc, noPortSvc, false},
		{"gateway path", gatewayMdl, gatewayMdl, false},
		{"no address reported", "", "", false},
		{"Service unreadable: not known", noPortSvc, "", false},
	} {
		if got := serving.AddressMissesPort(c.reported, c.working); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// putNoPortModel stores, in project proj, an LLMInferenceService named
// name with no route (KServe reports its Service without a port) and that
// Service, listening on 8000.
func (e *playgroundEnv) putNoPortModel(name string) {
	svc := name + "-kserve-workload-svc"
	e.k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/"+name,
		`{"metadata":{"name":"`+name+`","namespace":"proj"},"status":{"addresses":[{"url":"https://`+svc+`.proj.svc.cluster.local"}],"conditions":[{"type":"Ready","status":"True"}]}}`)
	e.k8s.putJSON("/api/v1/namespaces/proj/services/"+svc, `{"spec":{"ports":[{"name":"https","port":8000}]}}`)
}

// putGatewayModel stores a model on the inference gateway: its reported
// in-cluster address is the gateway's path, which connects as it is.
func (e *playgroundEnv) putGatewayModel(name string) {
	e.k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/"+name,
		`{"metadata":{"name":"`+name+`","namespace":"proj"},"spec":{"router":{"route":{},"gateway":{}}},"status":{"addresses":[{"url":"https://openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local/proj/`+name+`"}],"conditions":[{"type":"Ready","status":"True"}]}}`)
}

const testSmolModel = `{"model_name":"smollm2-135m-instruct","model_id":"smollm2-135m-instruct","status":"Running","usecase":"chat","capabilities":["text-generation"]}`

// testQwenAndSmol: the gen-ai API lists both models of the project, ready.
var testQwenAndSmol = `{"data":[` + strings.TrimSuffix(strings.TrimPrefix(testQwenModels, `{"data":[`), `]}`) + `,` + testSmolModel + `]}`

// Set up's own choice leaves out a model whose address KServe reports
// without the port, says why in the check step, and installs with the
// others; a model chosen explicitly is refused; with no other chat model
// left the setup stops and names the alternatives.
func TestPlaygroundSetupLeavesOutModelsWithoutPort(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = testQwenAndSmol
	e.putGatewayModel("qwen25-05b-instruct")
	e.putNoPortModel("smollm2-135m-instruct")

	res, steps := e.run(t, setup())
	if !res.Success || len(e.g.installs) != 1 {
		t.Fatalf("auto-pick: %+v", res)
	}
	models, _ := e.g.installs[0]["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("installed models %v", models)
	}
	if m, _ := models[0].(map[string]any); m["model_name"] != "qwen25-05b-instruct" {
		t.Errorf("installed models %v", models)
	}
	if !strings.Contains(steps[0].Message, "left out smollm2-135m-instruct ("+strings.TrimSuffix(serving.PlaygroundNoPortNote, ".")+")") {
		t.Errorf("the check does not say why the model is left out: %s", steps[0].Message)
	}

	// Chosen explicitly: refused before anything is asked.
	features.PurgePlaygroundStatus()
	delete(e.g.servers, "proj")
	installs := len(e.g.installs)
	req := setup()
	req.Models = []types.PlaygroundModelRef{{Name: "qwen25-05b-instruct", Source: "namespace"}, {Name: "smollm2-135m-instruct", Source: "namespace"}}
	res, _ = e.run(t, req)
	if res.Success || res.ErrorCode != "validation" || len(e.g.installs) != installs ||
		!strings.Contains(res.Message, "Model smollm2-135m-instruct cannot be chosen. "+serving.PlaygroundNoPortNote) || !strings.Contains(res.Message, features.PlaygroundAlternatives) {
		t.Fatalf("explicit choice: %+v", res)
	}

	// No other chat model: why, and what the playground can use instead.
	features.PurgePlaygroundStatus()
	e.g.models["proj"] = `{"data":[` + testSmolModel + `]}`
	res, _ = e.run(t, setup())
	if res.Success || res.ErrorCode != "no_model" || len(e.g.installs) != installs ||
		!strings.Contains(res.Message, "has no ready chat model the playground can use. Left out: smollm2-135m-instruct (") || !strings.Contains(res.Message, features.PlaygroundAlternatives) {
		t.Fatalf("none left: %+v", res)
	}
}

// When KServe reports the port (or the model is on a gateway), nothing is
// left out: the check keys on the address, not on how the model is set up.
func TestPlaygroundSetupKeepsModelsReportedWithTheirPort(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = `{"data":[` + testSmolModel + `]}`
	e.k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/smollm2-135m-instruct",
		`{"metadata":{"name":"smollm2-135m-instruct","namespace":"proj"},"status":{"addresses":[{"url":"`+noPortSvc+`:8000"}],"conditions":[{"type":"Ready","status":"True"}]}}`)
	res, steps := e.run(t, setup())
	if !res.Success || len(e.g.installs) != 1 || strings.Contains(steps[0].Message, "left out") {
		t.Fatalf("got %+v, %s", res, steps[0].Message)
	}
}

// The Playground status marks a project model the playground cannot use,
// read with the user's token; the others are not marked.
func TestPlaygroundStatusMarksModelsWithoutPort(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = testQwenAndSmol
	e.putGatewayModel("qwen25-05b-instruct")
	e.putNoPortModel("smollm2-135m-instruct")
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range st.Models {
		got[m.Name] = m.Unusable
	}
	if got["smollm2-135m-instruct"] != serving.PlaygroundNoPortNote || got["qwen25-05b-instruct"] != "" {
		t.Fatalf("models %+v", st.Models)
	}
	for _, call := range e.k8s.calls {
		if (strings.Contains(call.Path, "/llminferenceservices") || strings.Contains(call.Path, "/services/")) && strings.Contains(call.Path, "/namespaces/proj/") && call.Token != testReaderToken {
			t.Errorf("%s read with %q, not the user's token", call.Path, call.Token)
		}
	}
}

// A candidate's Service that cannot be read fails Set up closed with a
// neutral message: the API server's words (here a user name) reach
// neither the stream nor the log. The status still lists the model,
// unmarked, without a block.
func TestPlaygroundSetupFailsClosedOnAnUnreadableService(t *testing.T) {
	logs := captureSlog(t)
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = `{"data":[` + testSmolModel + `]}`
	e.putNoPortModel("smollm2-135m-instruct")
	const svc = "GET /api/v1/namespaces/proj/services/smollm2-135m-instruct-kserve-workload-svc"
	e.k8s.fail[svc] = 403
	e.k8s.failMessage[svc] = "user \"secret-person\" cannot get services"
	res, steps := e.run(t, setup())
	if res.Success || len(e.g.installs) != 0 || !strings.Contains(res.Message, "Cannot check whether smollm2-135m-instruct in proj is reachable (HTTP 403); try again.") {
		t.Fatalf("got %+v", res)
	}
	all := res.Message + strings.Join(res.Logs, "\n") + logs.String()
	for _, s := range steps {
		all += s.Message
	}
	if strings.Contains(all, "secret-person") {
		t.Errorf("the API server's words got out: %s", all)
	}
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil || len(st.Models) != 1 || st.Models[0].Unusable != "" || st.ModelsProblem != "" {
		t.Fatalf("status: %+v, %v", st, err)
	}
	if strings.Contains(logs.String(), "secret-person") {
		t.Errorf("the status logged the API server's words: %s", logs.String())
	}
}

// A user's project status gives the model's in-cluster address as it
// connects: the Service's port added to what KServe reports, the Service
// read with the user's token; a status.url KServe kept from a removed
// route is not shown (the model has no route).
func TestGetProjectStatusInternalURLHasTheServicePort(t *testing.T) {
	k8s, c := newResourceFake(t)
	k8s.putProject("proj")
	k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/smollm2-135m-instruct",
		`{"metadata":{"name":"smollm2-135m-instruct","namespace":"proj"},"status":{"url":"https://stale-route.apps.example.com/proj/smollm2-135m-instruct","addresses":[{"url":"`+noPortSvc+`"}],"conditions":[{"type":"Ready","status":"True"}]}}`)
	k8s.putJSON("/api/v1/namespaces/proj/services/smollm2-135m-instruct-kserve-workload-svc", `{"spec":{"ports":[{"name":"https","port":8000}]}}`)
	u := userClientOf(c, fakeUserToken)
	st, err := GetProjectStatus(c, u, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models) != 1 || st.Models[0].InternalURL != noPortSvc+":8000" || st.Models[0].URL != "" {
		t.Fatalf("models %+v", st.Models)
	}
	read := false
	for _, call := range k8s.calls {
		if call.Path == "/api/v1/namespaces/proj/services/smollm2-135m-instruct-kserve-workload-svc" {
			read = true
			if call.Token != fakeUserToken {
				t.Errorf("the Service was read with %q, not the user's token", call.Token)
			}
		}
	}
	if !read {
		t.Error("the Service was not read")
	}
}
