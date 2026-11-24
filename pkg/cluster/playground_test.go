package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const testGenAIToken = "genai-secret-token"

// genAIFake is the RHOAI dashboard's gen-ai API, with the response shapes
// seen live on RHOAI 3.6: aaa/models {"data":[...]}, lsd/status
// {"data":null} or {"data":{"name","phase"}}, errors
// {"error":{"code","message"}}. Every call the dashboard passes on to Models
// as a Service with the caller's token (maas/*, aaa/models with sources,
// an install with a MaaS model, lsd/responses, genai-proxy) fails the
// test: the tool's token must never reach the MaaS gateway.
type genAIFake struct {
	t  *testing.T
	mu sync.Mutex
	// servers is each project's playground (absent: none).
	servers map[string]*features.GenAIServer
	models  map[string]string // project -> aaa/models body
	// startingReads is how many status reads a new playground reports
	// Initializing before Ready; failPhase makes it report Failed instead.
	startingReads int
	failPhase     bool
	reads         map[string]int
	// conflicts is how many installs answer 409 first.
	conflicts int
	// installFail answers every install with this status and message.
	installFail    int
	installMessage string
	// missing answers 404 to every path (an API that differs).
	missing bool
	// onInstall runs when an install creates a server (the env puts its
	// OGXServer in the fake cluster).
	onInstall func(ns string)
	// existingOnInstall answers an install with 200 and the server already
	// there (the BFF's zero-restart path) instead of creating one.
	existingOnInstall bool
	paths             []string
	installs          []map[string]any
	deletes           []string
	auth              []string
	// userToken is a signed-in user's own token: with it, and only with
	// it, maas/models (answered with maasModels) and an install with a
	// MaaS model are expected.
	userToken  string
	maasModels string
}

func newGenAIFake(t *testing.T) *genAIFake {
	return &genAIFake{t: t, servers: map[string]*features.GenAIServer{}, models: map[string]string{}, reads: map[string]int{}, startingReads: 2}
}

func (g *genAIFake) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	g.paths = append(g.paths, r.Method+" "+r.URL.Path+" "+r.URL.Query().Get("namespace"))
	ns := r.URL.Query().Get("namespace")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": http.StatusText(status), "message": msg}})
	}
	if g.missing {
		fail(http.StatusNotFound, "the requested resource could not be found")
		return
	}
	asUser := g.userToken != "" && r.Header.Get("Authorization") == "Bearer "+g.userToken
	if asUser && r.Method+" "+r.URL.Path == "GET /gen-ai/api/v1/maas/models" {
		_, _ = io.WriteString(w, kube.FirstNonEmpty(g.maasModels, `{"data":[]}`))
		return
	}
	for _, forwarding := range []string{"/gen-ai/api/v1/maas/", "/gen-ai/api/v1/lsd/responses", "/gen-ai/api/v1/genai-proxy/"} {
		if strings.HasPrefix(r.URL.Path, forwarding) {
			g.t.Errorf("%s %s: the dashboard sends the caller's token on to the MaaS gateway", r.Method, r.URL.Path)
			fail(http.StatusInternalServerError, "forwarding call")
			return
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /gen-ai/api/v1/aaa/models":
		if src := r.URL.Query()["sources"]; len(src) > 0 {
			g.t.Errorf("aaa/models with sources %v: for sources=maas the dashboard asks MaaS with the caller's token", src)
		}
		_, _ = io.WriteString(w, kube.FirstNonEmpty(g.models[ns], `{"data":[]}`))
	case "GET /gen-ai/api/v1/lsd/status":
		srv := g.servers[ns]
		if srv == nil {
			_, _ = io.WriteString(w, `{"data":null}`)
			return
		}
		g.reads[ns]++
		switch {
		case srv.Phase == "Terminating":
			delete(g.servers, ns)
		case srv.Phase == "Initializing" && g.reads[ns] > g.startingReads:
			srv.Phase = "Ready"
			if g.failPhase {
				srv.Phase = "Failed"
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"name": srv.Name, "phase": srv.Phase, "version": "v1"}})
	case "POST /gen-ai/api/v1/lsd/install":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.installs = append(g.installs, body)
		models, _ := body["models"].([]any)
		for _, m := range models {
			if mm, _ := m.(map[string]any); mm["model_source_type"] != "namespace" && (!asUser || mm["model_source_type"] != "maas") {
				g.t.Errorf("install with model %v: the dashboard lists MaaS models with the caller's token", m)
			}
		}
		if g.conflicts > 0 {
			g.conflicts--
			fail(http.StatusConflict, "previous pgvector resources are still being removed")
			return
		}
		if g.installFail != 0 {
			fail(g.installFail, g.installMessage)
			return
		}
		if g.existingOnInstall && g.servers[ns] != nil {
			_, _ = io.WriteString(w, `{"data":{"name":"lsd-genai-playground","httpStatus":"200"}}`)
			return
		}
		g.servers[ns] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Initializing"}
		if g.onInstall != nil {
			g.onInstall(ns)
		}
		_, _ = io.WriteString(w, `{"data":{"name":"lsd-genai-playground","httpStatus":"200"}}`)
	case "DELETE /gen-ai/api/v1/lsd/delete":
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.deletes = append(g.deletes, body.Name)
		if srv := g.servers[ns]; srv != nil && srv.Name == body.Name {
			srv.Phase = "Terminating"
		}
		_, _ = io.WriteString(w, `{"data":"deleted"}`)
	default:
		fail(http.StatusNotFound, "no route "+r.URL.Path)
	}
}

// playgroundEnv is a fake cluster with what a playground needs (the OGX
// component Managed and ready, gen-ai-ui available, data science project
// "proj" with a ready chat model) and the fake gen-ai API.
type playgroundEnv struct {
	k8s *resourceFake
	c   *Client
	// reader is the signed-in user's client of a status read (pkg/api
	// withUserRead): their token, c for the updater's own records.
	reader *Client
	g      *genAIFake
}

// testReaderToken is the token of the user who reads the status.
const testReaderToken = "status-reader-token"

const (
	testQwenModels = `{"data":[{"model_name":"qwen25-05b-instruct","model_id":"qwen25-05b-instruct","serving_runtime":"Distributed inference with llm-d","api_protocol":"REST","usecase":"chat","endpoints":["internal: https://x.svc.cluster.local/"],"status":"Running","display_name":"Qwen2.5 0.5B Instruct (CPU)","model_source_type":"namespace","capabilities":["text-generation"]}]}`
	testRecordPath = "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot"
)

// putMaaSModels publishes two models to Models as a Service, as
// MaaSModelRefs in the cluster: gpt-oss-20b (Ready, offered under team-sub
// and this tool's subscription), an ExternalModel-backed one that is not
// ready, listed under its resolved alias with a display name, and one no
// subscription offers.
func (e *playgroundEnv) putMaaSModels() {
	e.k8s.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/team/maasmodelrefs/gpt-oss-20b", `{
		"metadata":{"annotations":{"opendatahub.io/model-capabilities":"[\"text-generation\"]"}},
		"spec":{"modelRef":{"kind":"LLMInferenceService","name":"gpt-oss-20b"}},"status":{"phase":"Ready"}}`)
	e.k8s.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/llm/maasmodelrefs/opt", `{
		"metadata":{"annotations":{"openshift.io/display-name":"Facebook OPT 125M (Simulated)"}},
		"spec":{"modelRef":{"kind":"ExternalModel","name":"opt-125m"}},"status":{"phase":"Pending","resolvedModelAlias":"publishers/llm/models/facebook/opt-125m"}}`)
	e.k8s.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/team-sub",
		`{"spec":{"modelRefs":[{"name":"gpt-oss-20b","namespace":"team"},{"name":"opt","namespace":"llm"}]}}`)
	// A reference no subscription lists is offered to nobody: not shown.
	e.k8s.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/team/maasmodelrefs/unoffered", `{"spec":{"modelRef":{"kind":"LLMInferenceService","name":"unoffered"}},"status":{"phase":"Ready"}}`)
	e.k8s.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/rhoai-nightly-updater-subscription",
		`{"spec":{"modelRefs":[{"name":"gpt-oss-20b","namespace":"team"},{"name":"qwen","namespace":"proj"}]}}`)
}

func newPlaygroundEnv(t *testing.T) *playgroundEnv {
	t.Helper()
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	features.PurgePlaygroundStatus()
	k8s, c := newResourceFake(t)
	// The ServiceAccount's client, as a read gets it: the gen-ai API is
	// called with its token.
	editTestClient(c, func(o *testClientOptions) { o.Token = testGenAIToken })
	g := newGenAIFake(t)
	g.models["proj"] = testQwenModels
	srv := newFakeServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	restoreGenAI := features.SetGenAIForTest(func(*Client) string { return srv.URL }, srv.Client())
	t.Cleanup(func() { restoreGenAI(); features.PurgePlaygroundStatus() })
	k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
		"spec":{"components":{"ogx":{"managementState":"Managed"}}},
		"status":{"conditions":[{"type":"OGXReady","status":"True"}]}}`)
	k8s.putJSON("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/gen-ai-ui", `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
	k8s.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	k8s.putJSON("/api/v1/namespaces/other", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	k8s.putProject("proj")
	k8s.putProject("other")
	e := &playgroundEnv{k8s: k8s, c: c, g: g, reader: userClientOf(c, testReaderToken)}
	installs := 0
	g.onInstall = func(ns string) {
		installs++
		e.putServer(ns, fmt.Sprintf("uid-installed-%d", installs), kube.Clock().Now())
	}
	return e
}

// putServer stores the playground's OGXServer in project ns, as the
// dashboard creates it.
func (e *playgroundEnv) putServer(ns, uid string, created time.Time) {
	e.k8s.putJSON("/apis/ogx.io/v1beta1/namespaces/"+ns+"/ogxservers/lsd-genai-playground",
		fmt.Sprintf(`{"metadata":{"name":"lsd-genai-playground","namespace":%q,"uid":%q,"creationTimestamp":%q}}`, ns, uid, created.UTC().Format(time.RFC3339)))
}

func (e *playgroundEnv) run(t *testing.T, req types.PlaygroundRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	res, err := RunPlayground(e.c, req, func(s UpdateStepEvent) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.Contains(s.Message, testGenAIToken) {
			t.Errorf("step %s shows the token: %s", s.Step, s.Message)
		}
	}
	if strings.Contains(res.Message, testGenAIToken) || strings.Contains(strings.Join(res.Logs, "\n"), testGenAIToken) {
		t.Errorf("the result shows the token: %+v", res)
	}
	return res, steps
}

func (e *playgroundEnv) records(t *testing.T) map[string]features.PlaygroundRecord {
	t.Helper()
	r, err := features.ReadPlaygroundRecords(e.c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func setup() types.PlaygroundRequest {
	project := "proj"
	return types.PlaygroundRequest{Action: "setup", Project: project}
}

// Setup asks the dashboard to install with the project's ready chat models
// (MaaS models are never added), records the request, and waits until the
// playground reports Ready; every call carries the token it was given.
func TestPlayground_SetupInstallsThroughTheDashboardAndWaitsForReady(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.putMaaSModels()
	res, steps := e.run(t, setup())
	if !res.Success {
		t.Fatalf("got %+v", res)
	}
	if len(e.g.installs) != 1 {
		t.Fatalf("installs: %d", len(e.g.installs))
	}
	body, _ := json.Marshal(e.g.installs[0])
	if want := `{"enable_guardrails":false,"enable_tracing":false,"models":[{"model_name":"qwen25-05b-instruct","model_source_type":"namespace","model_type":"llm"}],"vector_stores":[]}`; string(body) != want {
		t.Errorf("install body\n got %s\nwant %s", body, want)
	}
	for _, a := range e.g.auth {
		if a != "Bearer "+testGenAIToken {
			t.Fatalf("authorization %q", a)
		}
	}
	if _, ok := e.records(t)["proj"]; !ok {
		t.Error("the request is not recorded")
	}
	var ids []string
	for _, s := range steps {
		if s.Status == "success" {
			ids = append(ids, s.Step)
		}
	}
	if strings.Join(ids, " ") != "check install ready" {
		t.Errorf("steps: %v", ids)
	}
}

// A MaaS model is never installed with the ServiceAccount's token: the
// dashboard's install would list MaaS models through the MaaS gateway with
// it. The ServiceAccount's client never lists MaaS models through the
// dashboard, so the model is not offered and nothing is installed.
func TestPlayground_SetupWithTheServiceAccountNeverAddsAMaaSModel(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.putMaaSModels()
	req := setup()
	req.Models = []types.PlaygroundModelRef{{Name: "qwen25-05b-instruct", Source: "namespace"}, {Name: "gpt-oss-20b", Source: "maas"}}
	res, _ := e.run(t, req)
	if res.Success || res.ErrorCode != "validation" || !strings.Contains(res.Message, "does not offer model gpt-oss-20b") {
		t.Fatalf("got %+v", res)
	}
	if len(e.g.installs) != 0 {
		t.Fatalf("installed: %v", e.g.installs)
	}
	for _, p := range e.g.paths {
		if strings.Contains(p, "/maas/") {
			t.Fatalf("the ServiceAccount's token went to %s", p)
		}
	}
}

// userPlaygroundClient is the env's client acting with a signed-in user's
// own token, as a change's client does (NewUserClient).
func (e *playgroundEnv) userPlaygroundClient(token string) *Client {
	uc := *e.c
	editTestClient(&uc, func(o *testClientOptions) { o.Token, o.AsUser = token, true })
	e.g.userToken = token
	e.g.maasModels = `{"data":[{"id":"gpt-oss-20b","display_name":"GPT OSS 20B","ready":true,"capabilities":["text-generation"],"subscriptions":[{"name":"team-sub"}]}]}`
	return &uc
}

// Set up with the user's own token (a change) may add a Models as a
// Service model, as the user can in the dashboard: the dashboard lists the
// MaaS models that user may use, and installs one with its subscription.
// Every call carries the user's token, never the ServiceAccount's.
func TestPlayground_SetupWithTheUsersTokenAddsAMaaSModel(t *testing.T) {
	e := newPlaygroundEnv(t)
	const userToken = "sha256~user-own-token"
	uc := e.userPlaygroundClient(userToken)
	req := setup()
	req.Models = []types.PlaygroundModelRef{{Name: "qwen25-05b-instruct", Source: "namespace"}, {Name: "gpt-oss-20b", Source: "maas"}}
	var steps []UpdateStepEvent
	res, err := RunPlayground(uc, req, func(s UpdateStepEvent) { steps = append(steps, s) })
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	body, _ := json.Marshal(e.g.installs[0])
	if want := `{"enable_guardrails":false,"enable_tracing":false,"models":[{"model_name":"qwen25-05b-instruct","model_source_type":"namespace","model_type":"llm"},{"model_name":"gpt-oss-20b","model_source_type":"maas","model_type":"llm"}],"vector_stores":[]}`; string(body) != want {
		t.Errorf("install body\n got %s\nwant %s", body, want)
	}
	// The record keeps the models the request asked for, and the check
	// names the MaaS model as Models as a Service.
	if got := e.records(t)["proj"].Models; fmt.Sprint(got) != fmt.Sprint(req.Models) {
		t.Errorf("recorded models %+v", got)
	}
	if !slices.ContainsFunc(steps, func(s UpdateStepEvent) bool {
		return s.Step == "check" && strings.Contains(s.Message, "with qwen25-05b-instruct, GPT OSS 20B (Models as a Service, gpt-oss-20b).")
	}) {
		t.Errorf("check step %+v", steps)
	}
	for _, a := range e.g.auth {
		if a != "Bearer "+userToken {
			t.Fatalf("authorization %q", a)
		}
	}
	for _, s := range steps {
		if strings.Contains(s.Message, userToken) {
			t.Errorf("step %s shows the token", s.Step)
		}
	}
	// The ServiceAccount's client gets the ServiceAccount allowlist.
	if api, _, _ := features.PlaygroundAPI(e.c); api.AsUser || api.Token != testGenAIToken {
		t.Fatal("the ServiceAccount's client is treated as the user's")
	}
	// Even with the user's token, the calls that are not the playground's
	// own stay refused.
	api, _, _ := features.PlaygroundAPI(uc)
	if !api.AsUser || api.Token != userToken {
		t.Fatal("the user's client does not call with the user's token")
	}
	for _, c := range []struct{ method, path string }{{http.MethodPost, "/maas/tokens"}, {http.MethodPost, "/lsd/responses"}, {http.MethodGet, "/genai-proxy/ns/proj/v1/models"}} {
		if err := api.Call(c.method, c.path, "proj", nil, nil); !errors.Is(err, features.ErrGenAINotAllowed) {
			t.Errorf("%s %s with the user's token: %v", c.method, c.path, err)
		}
	}
	traced := map[string]any{"enable_guardrails": false, "enable_tracing": true, "vector_stores": []any{}, "models": []map[string]any{{"model_name": "m", "model_source_type": "maas"}}}
	if err := api.Call(http.MethodPost, "/lsd/install", "proj", traced, nil); !errors.Is(err, features.ErrGenAINotAllowed) {
		t.Errorf("an install with tracing: %v", err)
	}
}

// A Set up with the user's token that chose only the project's own models
// never lists MaaS models: that call sends the user's token through
// maas-default-gateway, where an editor of a published project can attach
// a route. Choosing no models (the project's ready chat models) is the same.
func TestPlayground_SetupWithProjectModelsOnlyNeverReachesMaaS(t *testing.T) {
	for name, models := range map[string][]types.PlaygroundModelRef{
		"project models chosen": {{Name: "qwen25-05b-instruct", Source: "namespace"}},
		"no models chosen":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newPlaygroundEnv(t)
			const userToken = "sha256~user-own-token"
			uc := e.userPlaygroundClient(userToken)
			req := setup()
			req.Models = models
			res, err := RunPlayground(uc, req, nil)
			if err != nil || !res.Success {
				t.Fatalf("got %+v, %v", res, err)
			}
			for _, p := range e.g.paths {
				if strings.Contains(p, "/maas/") {
					t.Errorf("called %s although no MaaS model was chosen", p)
				}
			}
		})
	}
}

// The client refuses, before sending anything, every gen-ai call that the
// dashboard passes on to Models as a Service with the caller's token: the
// check guards the token whatever calls it.
func TestPlayground_GenAIClientRefusesCallsThatReachMaaS(t *testing.T) {
	e := newPlaygroundEnv(t)
	api, problem, _ := features.PlaygroundAPI(e.c)
	if problem != "" {
		t.Fatal(problem)
	}
	ns := map[string]any{"model_name": "m", "model_source_type": "namespace"}
	// install is the body Set up sends, with over changed.
	install := func(over map[string]any) map[string]any {
		b := map[string]any{"enable_guardrails": false, "enable_tracing": false, "vector_stores": []any{}, "models": []map[string]any{ns}}
		maps.Copy(b, over)
		return b
	}
	for _, c := range []struct {
		method, path string
		in           any
	}{
		{http.MethodGet, "/maas/models", nil},
		{http.MethodPost, "/maas/tokens", map[string]string{"model": "m"}},
		{http.MethodPost, "/lsd/responses", nil},
		{http.MethodGet, "/genai-proxy/ns/proj/v1/models", nil},
		{http.MethodPost, "/lsd/install", install(map[string]any{"models": []map[string]any{ns, {"model_name": "x", "model_source_type": "maas"}}})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"models": []map[string]any{{"model_name": "x", "model_source_type": "custom_endpoint"}}})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"models": []map[string]any{}})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"enable_tracing": true})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"enable_guardrails": true})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"vector_stores": []any{map[string]any{"provider": "remote"}}})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"guardrail_models": []any{}})},
		{http.MethodPost, "/lsd/install", install(map[string]any{"models": []map[string]any{{"model_name": "m", "model_source_type": "namespace", "subscription": "s"}}})},
		{http.MethodPost, "/lsd/install", map[string]any{"models": []map[string]any{ns}}},
		{http.MethodPost, "/lsd/install", nil},
		{http.MethodGet, "/lsd/models", nil},
	} {
		if err := api.Call(c.method, c.path, "proj", c.in, nil); !errors.Is(err, features.ErrGenAINotAllowed) {
			t.Errorf("%s %s %v: got %v", c.method, c.path, c.in, err)
		}
	}
	if len(e.g.paths) != 0 {
		t.Fatalf("sent: %v", e.g.paths)
	}
	if err := api.Call(http.MethodPost, "/lsd/install", "proj", install(nil), nil); err != nil {
		t.Fatalf("an install with the project's own model: %v", err)
	}
}

// While the previous playground's database is still being removed the
// dashboard answers 409: the install is retried on the clock.
func TestPlayground_SetupRetriesWhileThePreviousDatabaseIsRemoved(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.conflicts = 2
	res, steps := e.run(t, setup())
	if !res.Success || len(e.g.installs) != 3 {
		t.Fatalf("got %+v after %d installs", res, len(e.g.installs))
	}
	found := false
	for _, s := range steps {
		found = found || strings.Contains(s.Message, "still being removed; trying again")
	}
	if !found {
		t.Error("the retry is not reported")
	}
}

// A 409 that does not clear stops the setup after the bound, and the
// request is no longer recorded.
func TestPlayground_SetupGivesUpOnAPersistentConflict(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.conflicts = 1 << 20
	res, _ := e.run(t, setup())
	if res.Success || res.ErrorCode != "in_progress" {
		t.Fatalf("got %+v", res)
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("a request that created nothing is still recorded")
	}
}

// Orphaned ConfigMaps of an earlier playground make the dashboard answer 400
// "already exists": explained, with the command that removes the one it
// names, and nothing deleted by this tool.
func TestPlayground_SetupExplainsLeftovers(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.installFail, e.g.installMessage = http.StatusBadRequest, `failed to create ConfigMap: configmaps "llama-stack-config" already exists`
	res, _ := e.run(t, setup())
	if res.Success || res.ErrorCode != "leftovers" || !strings.Contains(res.Message, "oc delete configmap -n proj llama-stack-config") {
		t.Fatalf("got %+v", res)
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("a failed install is still recorded")
	}
	for _, r := range e.k8s.requests {
		if strings.HasPrefix(r, "DELETE") {
			t.Errorf("deleted something: %s", r)
		}
	}
}

// The dashboard's install checks only the models it is asked to use
// (rhoai-3.6 generateLlamaStackConfig): another model of the project that
// is not ready is left out, not a reason to refuse.
func TestPlayground_SetupUsesTheReadyModelsOnly(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = `{"data":[` + strings.TrimSuffix(strings.TrimPrefix(testQwenModels, `{"data":[`), `]}`) + `,{"model_name":"deepseek","status":"Failed"}]}`
	res, steps := e.run(t, setup())
	if !res.Success || len(e.g.installs) != 1 {
		t.Fatalf("got %+v", res)
	}
	models, _ := e.g.installs[0]["models"].([]any)
	if m, _ := models[0].(map[string]any); len(models) != 1 || m["model_name"] != "qwen25-05b-instruct" {
		t.Errorf("models %v", models)
	}
	if !strings.Contains(steps[0].Message, "deepseek") {
		t.Errorf("the check does not name the model left out: %s", steps[0].Message)
	}
	// A chosen model that is not ready is still refused.
	features.PurgePlaygroundStatus()
	delete(e.g.servers, "proj")
	req := setup()
	req.Models = []types.PlaygroundModelRef{{Name: "deepseek", Source: "namespace"}}
	if res, _ := e.run(t, req); res.Success || res.ErrorCode != "model_not_ready" {
		t.Fatalf("chosen model not ready: %+v", res)
	}
}

// The record holds the playground's OGXServer UID once the dashboard made it.
func TestPlayground_SetupRecordsTheServerUID(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := e.records(t)["proj"].UID; got != "uid-installed-1" {
		t.Errorf("recorded UID %q", got)
	}
}

// The playground this tool made was deleted in Gen AI studio and someone
// made a new one there: the record is stale. The new one is not this
// tool's: shown as not managed, never deleted, and the record is dropped.
func TestPlayground_AStaleRecordDoesNotOwnANewPlayground(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	e.g.servers["proj"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Ready"}
	e.putServer("proj", "uid-made-in-the-dashboard", kube.Clock().Now())
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil || len(st.Items) != 1 || st.Items[0].ManagedByTool {
		t.Fatalf("status %+v, %v", st, err)
	}
	if res, _ := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("teardown: %+v", res)
	}
	if len(e.g.deletes) != 0 {
		t.Errorf("deleted %v", e.g.deletes)
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("the stale record was kept")
	}
}

// Between the check and the install someone else made a playground; the
// dashboard answers the install 200 with it. It is not this tool's.
func TestPlayground_AnInstallAnsweredWithAnExistingServerIsNotOwned(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.existingOnInstall = true
	e.k8s.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == testRecordPath {
			e.g.mu.Lock()
			if e.g.servers["proj"] == nil {
				e.g.servers["proj"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Ready"}
				e.putServer("proj", "uid-someone-else", kube.Clock().Now().Add(-time.Minute))
			}
			e.g.mu.Unlock()
		}
	}
	res, _ := e.run(t, setup())
	if res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("got %+v", res)
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("someone else's playground is recorded as this tool's")
	}
}

// The gen-ai API reports no playground as 200 {"data":null}; a 404 is a
// missing route or object, not absence: teardown fails and keeps the record.
func TestPlayground_A404IsNotAbsence(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	e.g.missing = true
	res, _ := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"})
	if res.Success || res.ErrorCode != "api_missing" {
		t.Fatalf("got %+v", res)
	}
	if _, ok := e.records(t)["proj"]; !ok {
		t.Error("the record was dropped on a 404")
	}
}

// Changing the selected project reads its models only: the playgrounds of
// every project are cached apart from it.
func TestPlayground_StatusFanOutIsSharedAcrossProjects(t *testing.T) {
	e := newPlaygroundEnv(t)
	if _, err := GetPlaygroundStatus(e.c, e.reader, "proj"); err != nil {
		t.Fatal(err)
	}
	before := len(e.g.paths)
	if _, err := GetPlaygroundStatus(e.c, e.reader, "other"); err != nil {
		t.Fatal(err)
	}
	for _, p := range e.g.paths[before:] {
		if strings.Contains(p, "/lsd/status") {
			t.Errorf("the playgrounds were read again: %v", e.g.paths[before:])
			break
		}
	}
}

// No ready chat model (an embedding model does not count): the setup
// points at the Models tab.
func TestPlayground_SetupNeedsAChatModel(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = `{"data":[{"model_name":"granite-embedding","status":"Running","usecase":"embedding","capabilities":["embedding"]}]}`
	e.putMaaSModels()
	res, _ := e.run(t, setup())
	if res.Success || res.ErrorCode != "no_model" || !strings.Contains(res.Message, "Models tab") || len(e.g.installs) != 0 {
		t.Fatalf("got %+v", res)
	}
}

// A playground this tool did not request is used as it is: setup and
// delete refuse, and nothing is sent to change it.
func TestPlayground_LeavesAPlaygroundItDidNotRequest(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.servers["proj"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Ready"}
	if res, _ := e.run(t, setup()); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("teardown: %+v", res)
	}
	if len(e.g.installs)+len(e.g.deletes) != 0 {
		t.Errorf("installs %d, deletes %v", len(e.g.installs), e.g.deletes)
	}
}

// Delete asks the dashboard to delete the server it reports, waits until it
// is gone, and clears the record.
func TestPlayground_TeardownDeletesThroughTheDashboard(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	res, steps := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"})
	if !res.Success || strings.Join(e.g.deletes, ",") != "lsd-genai-playground" {
		t.Fatalf("got %+v, deletes %v", res, e.g.deletes)
	}
	if e.g.servers["proj"] != nil {
		t.Error("the playground is still there")
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("the record is still there")
	}
	if last := steps[len(steps)-1]; last.Step != "gone" || last.Status != "success" {
		t.Errorf("last step %+v", last)
	}
	// Again: nothing left, nothing deleted.
	if res, _ := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"}); !res.Success || len(e.g.deletes) != 1 {
		t.Fatalf("second teardown: %+v", res)
	}
}

// A playground that fails to start stops the wait with the reason.
func TestPlayground_SetupReportsAFailedPlayground(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.failPhase = true
	res, _ := e.run(t, setup())
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "phase Failed") {
		t.Fatalf("got %+v", res)
	}
}

// What the setup needs on the cluster, each with its explanation.
func TestPlayground_SetupChecksTheCluster(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(e *playgroundEnv)
		code   string
	}{
		"operator off": {func(e *playgroundEnv) {
			e.k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{"spec":{"components":{"ogx":{"managementState":"Removed"}}}}`)
		}, "component_off"},
		"operator not ready": {func(e *playgroundEnv) {
			e.k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{"spec":{"components":{"ogx":{"managementState":"Managed"}}},"status":{"conditions":[{"type":"OGXReady","status":"False","message":"webhook down"}]}}`)
		}, "component_not_ready"},
		"no gen-ai-ui": {func(e *playgroundEnv) {
			e.k8s.mu.Lock()
			k, _ := parseFakePath("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/gen-ai-ui")
			delete(e.k8s.objects, k)
			e.k8s.mu.Unlock()
		}, "component_off"},
		"not a dashboard project": {func(e *playgroundEnv) {
			e.k8s.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{}},"status":{"phase":"Active"}}`)
		}, "validation"},
		"API differs": {func(e *playgroundEnv) { e.g.missing = true }, "api_missing"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPlaygroundEnv(t)
			tc.change(e)
			res, _ := e.run(t, setup())
			if res.Success || res.ErrorCode != tc.code || len(e.g.installs) != 0 {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

// Readiness the DataScienceCluster does not report is a note, not a stop.
func TestPlayground_UnknownOperatorReadinessIsANote(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{"spec":{"components":{"ogx":{"managementState":"Managed"}}}}`)
	res, steps := e.run(t, setup())
	if !res.Success || !strings.Contains(steps[0].Message, "not reported (unknown)") {
		t.Fatalf("got %+v, %+v", res, steps[0])
	}
}

func TestPlayground_NoToken(t *testing.T) {
	e := newPlaygroundEnv(t)
	editTestClient(e.c, func(o *testClientOptions) { o.Token = "" })
	res, err := RunPlayground(e.c, setup(), nil)
	if err != nil || res.Success || res.ErrorCode != "no_token" || len(e.g.auth) != 0 {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// The status: the playgrounds in the projects with their owner, and the
// selected project's models (none from MaaS while nothing is published).
func TestPlayground_Status(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	e.g.servers["other"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Initializing"}
	e.g.startingReads = 1 << 20
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocked != nil || st.OperatorReady != "True" || st.GenAIUI != "available" || st.APIProblem != "" {
		t.Fatalf("cluster parts %+v", st)
	}
	if len(st.Items) != 2 {
		t.Fatalf("items %+v", st.Items)
	}
	other, proj := st.Items[0], st.Items[1]
	if other.Namespace != "other" || other.ManagedByTool || other.Ready || !other.Deployed || other.Phase != "Initializing" {
		t.Errorf("other %+v", other)
	}
	if proj.Namespace != "proj" || !proj.ManagedByTool || !proj.Ready || proj.Name != "lsd-genai-playground" {
		t.Errorf("proj %+v", proj)
	}
	if len(st.Models) != 1 || st.Models[0].Name != "qwen25-05b-instruct" || !st.Models[0].Ready || !st.Models[0].Chat || st.Models[0].DisplayName != "Qwen2.5 0.5B Instruct (CPU)" {
		t.Errorf("models %+v", st.Models)
	}
	// Cached: a second read makes no call.
	calls := len(e.g.auth)
	if _, err := GetPlaygroundStatus(e.c, e.reader, "proj"); err != nil || len(e.g.auth) != calls {
		t.Errorf("not cached: %d calls, %v", len(e.g.auth)-calls, err)
	}
}

// An API that answers 404 is reported as one that differs, with the call.
func TestPlayground_StatusOfAnAPIThatDiffers(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.missing = true
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if st.APICode != "api_missing" || !strings.Contains(st.APIProblem, "/gen-ai/api/v1/aaa/models answered 404") {
		t.Fatalf("got %q %q", st.APICode, st.APIProblem)
	}
}

func TestPlayground_StatusWithoutToken(t *testing.T) {
	e := newPlaygroundEnv(t)
	editTestClient(e.reader, func(o *testClientOptions) { o.Token = "" })
	st, err := GetPlaygroundStatus(e.c, e.reader, "")
	if err != nil || st.APICode != "no_token" || len(e.g.auth) != 0 {
		t.Fatalf("got %+v, %v", st, err)
	}
}

// A user's status: every gen-ai call carries their token, the fan-out
// covers their projects only, and the tool's record shows only theirs; a
// project that is not theirs is refused before any gen-ai call. Another
// user's cached answer is never served.
func TestPlayground_StatusIsTheUsers(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["other"] = testQwenModels
	if res, _ := e.run(t, types.PlaygroundRequest{Action: "setup", Project: "other"}); !res.Success {
		t.Fatalf("setup in other: %+v", res)
	}
	// The reader may see proj only.
	e.k8s.deleteObject("/apis/project.openshift.io/v1/projects/other")
	features.PurgePlaygroundStatus()
	before := len(e.g.auth)
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range st.Items {
		if it.Namespace != "proj" {
			t.Errorf("another project's playground is shown: %+v", it)
		}
	}
	for i, a := range e.g.auth[before:] {
		if a != "Bearer "+testReaderToken {
			t.Errorf("gen-ai call %s with %q", e.g.paths[before+i], a)
		}
		if strings.HasSuffix(e.g.paths[before+i], " other") {
			t.Errorf("the fan-out read another project: %s", e.g.paths[before+i])
		}
	}
	for _, call := range e.k8s.calls {
		if call.Path == "/api/v1/namespaces" {
			t.Errorf("the namespaces were listed (%s)", call.Token)
		}
	}
	n := len(e.g.auth)
	if _, err := GetPlaygroundStatus(e.c, e.reader, "other"); !errors.Is(err, ErrNotDSProject) || len(e.g.auth) != n {
		t.Fatalf("another project: %v (%d gen-ai calls)", err, len(e.g.auth)-n)
	}
	// A second user with their own token reads afresh.
	second := userClientOf(e.c, "second-user-token")
	if _, err := GetPlaygroundStatus(e.c, second, "proj"); err != nil || len(e.g.auth) == n {
		t.Fatalf("second user: %v, served from the first user's cache: %v", err, len(e.g.auth) == n)
	}
}

func TestValidatePlaygroundRequest(t *testing.T) {
	for _, req := range []types.PlaygroundRequest{
		{Action: "remove", Project: "p"},
		{Action: "setup", Project: "Bad_Name"},
		{Action: "teardown", Project: "p", Models: []types.PlaygroundModelRef{{Name: "m", Source: "namespace"}}},
		{Action: "setup", Project: "p", Models: []types.PlaygroundModelRef{{Name: "m", Source: "custom"}}},
		{Action: "setup", Project: "p", Models: []types.PlaygroundModelRef{{Name: "a b", Source: "namespace"}}},
		{Action: "setup", Project: "p", Models: []types.PlaygroundModelRef{{Name: "m", Source: "namespace"}, {Name: "m", Source: "namespace"}}},
	} {
		if ValidatePlaygroundRequest(req) == "" {
			t.Errorf("accepted %+v", req)
		}
	}
	// A MaaS model is accepted: Set up runs with the user's own token, and
	// the gen-ai client still refuses it with the ServiceAccount's.
	if p := ValidatePlaygroundRequest(types.PlaygroundRequest{Action: "setup", Project: "p", Models: []types.PlaygroundModelRef{{Name: "publishers/llm/models/x", Source: "maas"}}}); p != "" {
		t.Errorf("a MaaS model: %q", p)
	}
	if p := ValidatePlaygroundRequest(types.PlaygroundRequest{Action: "setup", Project: "p", Models: []types.PlaygroundModelRef{{Name: "qwen", Source: "namespace"}}}); p != "" {
		t.Errorf("refused: %s", p)
	}
}

// The status lists the published Models as a Service from their
// MaaSModelRefs with the Kubernetes API (the gen-ai fake fails the test on
// any MaaS call), under the name maas-api lists them by, with the
// subscriptions that offer them: a chat request through the playground for
// a MaaS model names one, or the dashboard cannot mint the MaaS key and the
// playground answers 500 (seen live on RHOAI 3.6, 2026-11-18).
func TestPlayground_StatusListsMaaSModelsFromTheirModelRefs(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.putMaaSModels()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range st.Models {
		got = append(got, fmt.Sprintf("%s %s ready=%t chat=%t %q %v", m.Source, m.Name, m.Ready, m.Chat, m.DisplayName, m.Subscriptions))
	}
	want := []string{
		`namespace qwen25-05b-instruct ready=true chat=true "Qwen2.5 0.5B Instruct (CPU)" []`,
		`maas gpt-oss-20b ready=true chat=true "" [rhoai-nightly-updater-subscription team-sub]`,
		`maas publishers/llm/models/facebook/opt-125m ready=false chat=true "Facebook OPT 125M (Simulated)" [team-sub]`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("models\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range e.g.paths {
		if !strings.Contains(p, "/aaa/models") && !strings.Contains(p, "/lsd/status") {
			t.Errorf("gen-ai call %s", p)
		}
	}
}

// The status shows, for this tool's playground, the models its Set up
// asked for (from the record); a playground that is not this tool's shows
// none.
func TestPlayground_StatusShowsTheRecordedModels(t *testing.T) {
	e := newPlaygroundEnv(t)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Items) != 1 || !st.Items[0].ManagedByTool || fmt.Sprint(st.Items[0].Models) != fmt.Sprint([]types.PlaygroundModelRef{{Name: "qwen25-05b-instruct", Source: "namespace"}}) {
		t.Fatalf("items %+v", st.Items)
	}
}

// MaaSPlaygroundModelOf finds the MaaS entry of a model by the MaaSModelRef
// that names it (in the model's project), under the name the playground
// lists: not by a name built from the model's.
func TestPlayground_MaaSPlaygroundModelOf(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.putMaaSModels()
	cases := []struct {
		project, model, want string
		found                bool
	}{
		{"team", "gpt-oss-20b", "gpt-oss-20b ready=true [rhoai-nightly-updater-subscription team-sub]", true},
		{"team", "unoffered", "unoffered ready=true []", true},
		{"proj", "gpt-oss-20b", "", false},
		// An ExternalModel's reference is not an LLMInferenceService's.
		{"llm", "opt-125m", "", false},
	}
	for _, tc := range cases {
		m, found, err := features.MaaSPlaygroundModelOf(e.c, tc.project, tc.model)
		if err != nil || found != tc.found || (found && fmt.Sprintf("%s ready=%t %v", m.Name, m.Ready, m.Subscriptions) != tc.want) || (found && m.Source != "maas") {
			t.Errorf("%s/%s: %+v %v %v", tc.project, tc.model, m, found, err)
		}
	}
	// Subscriptions that cannot be read are an error, never "offered by none".
	e.k8s.fail["GET /apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions"] = http.StatusForbidden
	if _, found, err := features.MaaSPlaygroundModelOf(e.c, "team", "gpt-oss-20b"); err == nil || found || !strings.Contains(err.Error(), "list the MaaS subscriptions") {
		t.Errorf("subscriptions that cannot be read: %v %v", found, err)
	}
	// The status still lists the project's own models (MaaS is optional there).
	features.PurgePlaygroundStatus()
	if st, err := GetPlaygroundStatus(e.c, e.reader, "proj"); err != nil || len(st.Models) != 1 || st.Models[0].Source != "namespace" {
		t.Errorf("status with unreadable subscriptions: %+v %v", st, err)
	}
	e.k8s.fail["GET /apis/maas.opendatahub.io/v1alpha1/maasmodelrefs"] = http.StatusForbidden
	if _, found, err := features.MaaSPlaygroundModelOf(e.c, "team", "gpt-oss-20b"); err == nil || found {
		t.Errorf("a list that fails: %v %v", found, err)
	}
}

// MaaS models that cannot be read are none (MaaS is optional): the
// project's own models are still listed.
func TestPlayground_StatusWithoutReadableMaaSModels(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.putMaaSModels()
	e.k8s.fail["GET /apis/maas.opendatahub.io/v1alpha1/maasmodelrefs"] = http.StatusForbidden
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models) != 1 || st.Models[0].Source != "namespace" || st.ModelsProblem != "" {
		t.Fatalf("got %+v (%s)", st.Models, st.ModelsProblem)
	}
}

// N4: a record with no UID (the install never confirmed a server) claims a
// playground by the time rule only within the install's own window after
// the request. One made later in the project (an install that failed, or a
// project made again before the sweep) is not this tool's: shown as not
// managed, never deleted, and the record is dropped.
func TestPlayground_AnUnconfirmedRecordClaimsOnlyWithinTheInstallWindow(t *testing.T) {
	e := newPlaygroundEnv(t)
	requested := kube.Clock().Now().Add(-time.Hour)
	if err := features.WritePlaygroundRecords(e.c, map[string]features.PlaygroundRecord{"proj": {RequestedAt: requested.UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	e.g.servers["proj"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Ready"}
	e.putServer("proj", "uid-made-later", requested.Add(30*time.Minute))
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil || len(st.Items) != 1 || st.Items[0].ManagedByTool {
		t.Fatalf("status %+v, %v", st, err)
	}
	if res, _ := e.run(t, types.PlaygroundRequest{Action: "teardown", Project: "proj"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("teardown: %+v", res)
	}
	if len(e.g.deletes) != 0 {
		t.Errorf("deleted %v", e.g.deletes)
	}
	if _, ok := e.records(t)["proj"]; ok {
		t.Error("the unconfirmed record was kept")
	}
}

// Within the window the time rule still claims it (the install's own
// server, whose UID was not recorded yet).
func TestPlayground_AnUnconfirmedRecordClaimsTheInstallsServer(t *testing.T) {
	e := newPlaygroundEnv(t)
	requested := kube.Clock().Now().Add(-time.Hour)
	if err := features.WritePlaygroundRecords(e.c, map[string]features.PlaygroundRecord{"proj": {RequestedAt: requested.UTC().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	e.g.servers["proj"] = &features.GenAIServer{Name: "lsd-genai-playground", Phase: "Ready"}
	e.putServer("proj", "uid-installed", requested.Add(2*time.Minute))
	features.PurgePlaygroundStatus()
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil || len(st.Items) != 1 || !st.Items[0].ManagedByTool {
		t.Fatalf("status %+v, %v", st, err)
	}
}
