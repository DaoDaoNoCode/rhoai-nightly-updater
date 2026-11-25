package cluster

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	mrNS         = "rhoai-model-registries"
	mrPath       = "/apis/modelregistry.opendatahub.io/v1beta1/namespaces/" + mrNS + "/modelregistries/nightly-registry"
	mrCollection = "/apis/modelregistry.opendatahub.io/v1beta1/namespaces/" + mrNS + "/modelregistries"
	mrSvcPath    = "/api/v1/namespaces/" + mrNS + "/services/nightly-registry"
	mrGroupDoc   = `{"kind":"APIGroup","name":"modelregistry.opendatahub.io","versions":[{"groupVersion":"modelregistry.opendatahub.io/v1beta1","version":"v1beta1"},{"groupVersion":"modelregistry.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"modelregistry.opendatahub.io/v1beta1","version":"v1beta1"}}`
	mrDSCV3Path  = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/default-dsc"
	dscGroupV3   = `{"kind":"APIGroup","name":"datasciencecluster.opendatahub.io","versions":[{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"},{"groupVersion":"datasciencecluster.opendatahub.io/v2","version":"v2"}],"preferredVersion":{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"}}`
)

// mrEnv models a cluster where RHOAI enables the model registry component
// (DataScienceCluster v2, modelregistry), exposes the ModelRegistry API, and
// wires the Service gateway address to a stub registry REST endpoint.
type mrEnv struct {
	f   *resourceFake
	sa  *Client
	u   *Client
	reg *fakeRegistryAPI
	// availableAfter: after this many GETs the custom resource shows Available=True (0 means
	// it never becomes available); increments on each read the test harness performs.
	availableAfter int
	gets           int
	// serviceNotOwned: the Service carries no owner reference to the CR.
	serviceNotOwned bool
	// address overrides the Service's gateway address ("" the registry API's).
	address string
}

func newMREnv(t *testing.T) *mrEnv {
	t.Helper()
	f, sa := newResourceFake(t)
	e := &mrEnv{f: f, sa: sa, availableAfter: 2}
	f.groups = map[string]string{"/apis/modelregistry.opendatahub.io": mrGroupDoc}
	putDSC(f, `{"modelregistry":{"managementState":"Managed","registriesNamespace":"`+mrNS+`"}}`)
	e.reg = newFakeRegistryAPI(t)
	t.Cleanup(mlflow.SetSamplesHTTPClientForTest(e.reg.client))
	// Simulated operator: flip Available on the CR after sufficient GETs, and ensure the
	// Service is present with a gateway address annotation.
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural != "modelregistries" {
			return
		}
		e.gets++
		status := "False"
		msg := "Deployment nightly-registry is progressing"
		if e.availableAfter > 0 && e.gets >= e.availableAfter {
			status, msg = "True", "Deployment is available"
		}
		obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": status, "message": msg}}}
	}
	e.putService(true)
	// The dashboard's address is the cluster's gateway, where the operator
	// routes the registry (/model-registry/<name>).
	f.putJSON("/apis/route.openshift.io/v1/namespaces/redhat-ods-applications/routes/rhods-dashboard", `{"spec":{"host":"`+strings.TrimPrefix(e.reg.url, "https://")+`"}}`)
	prev := f.onGet
	f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if k.plural == "services" && k.name == "nightly-registry" && !e.serviceNotOwned {
			if cr := f.objects[fakeKey{gv: "modelregistry.opendatahub.io/v1beta1", ns: mrNS, plural: "modelregistries", name: "nightly-registry"}]; cr != nil {
				uid := cr["metadata"].(map[string]any)["uid"]
				ensureMap(obj, "metadata")["ownerReferences"] = []any{map[string]any{"apiVersion": "modelregistry.opendatahub.io/v1beta1", "kind": "ModelRegistry", "name": "nightly-registry", "uid": uid, "controller": true}}
			}
		}
	}
	u := *sa
	editTestClient(&u, func(o *testClientOptions) { o.Token, o.AsUser = fakeUserToken, true })
	u.SetUsername("alice")
	u.SetBookkeeping(sa)
	u.SetFacts(sa)
	e.u = &u
	return e
}

func (e *mrEnv) putService(withAddress bool) {
	ann := map[string]any{}
	if withAddress {
		ann[modelregistry.AddressAnnotation] = kubeFirst(e.address, strings.TrimPrefix(e.reg.url, "https://")+"/model-registry/nightly-registry")
	}
	e.f.put(mrSvcPath, map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"component": "model-registry", "app": "nightly-registry"}, "annotations": ann},
		"spec":     map[string]any{"ports": []any{map[string]any{"name": "https-api", "port": 8443}}},
	})
}

// fakeRegistryAPI stands in for registry REST (v1alpha3)—registered models,
// versions, artifacts—and does not implement DELETE.
type fakeRegistryAPI struct {
	t      *testing.T
	mu     sync.Mutex
	url    string
	client *http.Client
	calls  []registryCall
	models map[string]map[string]any // keyed by model name
	// versions and artifacts keyed by parent resource id.
	versions  map[string][]map[string]any
	artifacts map[string][]map[string]any
	next      int
}

type registryCall struct {
	Method, Path, Query, Token string
	Body                       map[string]any
}

func newFakeRegistryAPI(t *testing.T) *fakeRegistryAPI {
	r := &fakeRegistryAPI{t: t, models: map[string]map[string]any{}, versions: map[string][]map[string]any{}, artifacts: map[string][]map[string]any{}}
	srv := newFakeServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	r.url, r.client = srv.URL, srv.Client()
	return r
}

func (r *fakeRegistryAPI) id() string {
	r.next++
	return strconv.Itoa(r.next)
}

func (r *fakeRegistryAPI) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	const prefix = "/model-registry/nightly-registry/api/model_registry/v1alpha3"
	path, ok := strings.CutPrefix(req.URL.Path, prefix)
	r.calls = append(r.calls, registryCall{Method: req.Method, Path: path, Query: req.URL.RawQuery, Token: strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), Body: body})
	answer := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	if !ok {
		answer(404, map[string]any{"code": "404", "message": "no route"})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case req.Method == http.MethodGet && path == "/registered_model":
		if m, ok := r.models[req.URL.Query().Get("name")]; ok {
			answer(200, m)
			return
		}
		answer(404, map[string]any{"code": "404", "message": "registered model not found"})
	case req.Method == http.MethodPost && path == "/registered_models":
		m := map[string]any{"id": r.id(), "name": body["name"], "state": body["state"]}
		r.models[body["name"].(string)] = m
		answer(201, m)
	case req.Method == http.MethodPatch && len(parts) == 2 && parts[0] == "registered_models":
		for _, m := range r.models {
			if m["id"] == parts[1] {
				m["state"] = body["state"]
				answer(200, m)
				return
			}
		}
		answer(404, map[string]any{"message": "not found"})
	case len(parts) == 3 && parts[0] == "registered_models" && parts[2] == "versions":
		if req.Method == http.MethodGet {
			var items []any
			for _, v := range r.versions[parts[1]] {
				if n := req.URL.Query().Get("name"); n == "" || v["name"] == n {
					items = append(items, v)
				}
			}
			answer(200, map[string]any{"items": items, "size": len(items)})
			return
		}
		v := map[string]any{"id": r.id(), "name": body["name"], "state": body["state"]}
		r.versions[parts[1]] = append(r.versions[parts[1]], v)
		answer(201, v)
	case len(parts) == 2 && parts[0] == "model_versions" && req.Method == http.MethodPatch:
		for _, vs := range r.versions {
			for _, v := range vs {
				if v["id"] == parts[1] {
					v["state"] = body["state"]
					answer(200, v)
					return
				}
			}
		}
		answer(404, map[string]any{"message": "not found"})
	case len(parts) == 3 && parts[0] == "model_versions" && parts[2] == "artifacts":
		if req.Method == http.MethodGet {
			items := []any{}
			for _, a := range r.artifacts[parts[1]] {
				items = append(items, a)
			}
			answer(200, map[string]any{"items": items, "size": len(items)})
			return
		}
		a := map[string]any{"id": r.id(), "name": body["name"], "artifactType": body["artifactType"]}
		r.artifacts[parts[1]] = append(r.artifacts[parts[1]], a)
		answer(201, a)
	default:
		answer(405, map[string]any{"message": "not allowed"})
	}
}

// writes returns non-GET registry calls formatted as "METHOD path".
func (r *fakeRegistryAPI) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if c.Method != http.MethodGet {
			out = append(out, c.Method+" "+c.Path)
		}
	}
	return out
}

func (r *fakeRegistryAPI) body(method, path string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.Method == method && c.Path == path {
			return c.Body
		}
	}
	return nil
}

func runMR(t *testing.T, c *Client, req types.ModelRegistryRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	res, err := RunModelRegistry(c, req, func(s UpdateStepEvent) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	return res, steps
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Namespace resolution reads the DataScienceCluster—prefer status (v3 aiHub, then v2 modelregistry),
// falling back to spec when status omits it.
func TestModelRegistryNamespace(t *testing.T) {
	cases := []struct{ name, dsc, want string }{
		{"v3 status", `{"status":{"components":{"aiHub":{"instancesNamespace":"a"}}},"spec":{"components":{"aiHub":{"instancesNamespace":"b"}}}}`, "a"},
		{"v2 status", `{"status":{"components":{"modelregistry":{"registriesNamespace":"c"}}}}`, "c"},
		{"spec only", `{"spec":{"components":{"aiHub":{"managementState":"Managed","instancesNamespace":"d"}}}}`, "d"},
		{"v2 spec", `{"spec":{"components":{"modelregistry":{"registriesNamespace":"e"}}}}`, "e"},
		{"none", `{"spec":{"components":{"aiHub":{"managementState":"Removed"}}},"status":{"components":{"aiHub":{}}}}`, ""},
	}
	for _, tc := range cases {
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.dsc), &m); err != nil {
			t.Fatal(err)
		}
		if got := modelregistry.Namespace(m); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Component key selection follows the DataScienceCluster version: aiHub for v3
// (RHOAI 3.6), modelregistry for v2 and earlier.
func TestModelRegistryComponentKey(t *testing.T) {
	dsc := func(version, obj string) rhoai.DSCRead {
		var m map[string]any
		if err := json.Unmarshal([]byte(obj), &m); err != nil {
			t.Fatal(err)
		}
		return rhoai.DSCRead{Object: m, State: rhoai.DSCStatePresent, Version: version}
	}
	fc := envkit.ModelRegistryComponent
	if got := fc.KeyIn(dsc("v3", `{"spec":{"components":{"aiHub":{"managementState":"Managed"}}}}`)); got != "aiHub" {
		t.Errorf("v3: %q", got)
	}
	if got := fc.KeyIn(dsc("v2", `{"spec":{"components":{"modelregistry":{"managementState":"Removed"}}}}`)); got != "modelregistry" {
		t.Errorf("v2: %q", got)
	}
	if got := fc.KeyIn(dsc("v1", `{"spec":{"components":{}}}`)); got != "modelregistry" {
		t.Errorf("v1 without the key: %q", got)
	}
	b := envkit.ComponentBlock(dsc("v3", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"aiHub":{"managementState":"Removed"}}}}`), fc)
	if b == nil || b.Component != "aiHub" || !strings.Contains(b.Reason, "aiHub is Removed") {
		t.Errorf("v3 block: %+v", b)
	}
}

// Expect the create payload to match the dashboard's "Create model registry" form—default database
// configuration and nothing beyond the tool's managed-by label.
func TestModelRegistryBodyIsTheDashboards(t *testing.T) {
	want := map[string]any{
		"apiVersion": "modelregistry.opendatahub.io/v1beta1",
		"kind":       "ModelRegistry",
		"metadata": map[string]any{
			"name": "nightly-registry", "namespace": mrNS,
			"labels": asMap(t, envkit.ToolLabels()),
			"annotations": map[string]any{
				"openshift.io/display-name": "Nightly registry",
				"openshift.io/description":  "A model registry with the default database, for development and testing. Set up by rhoai-nightly-updater.",
			},
		},
		"spec": map[string]any{"kubeRBACProxy": map[string]any{}, "grpc": map[string]any{}, "rest": map[string]any{}, "postgres": map[string]any{"generateDeployment": true}},
	}
	if got := asMap(t, modelregistry.Body(mrNS)); !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("body:\n%s", g)
	}
}

func TestModelRegistrySetup_CreatesItAsTheDashboardAndRegistersTheSample(t *testing.T) {
	e := newMREnv(t)
	res, steps := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true})
	if !res.Success {
		t.Fatalf("setup failed: %s", res.Message)
	}
	// First POST is dry-run with strict field validation; second is the real create.
	posts := e.f.callsTo(http.MethodPost, mrCollection)
	if len(posts) != 2 || posts[0].Query.Get("dryRun") != "All" || posts[1].Query.Get("dryRun") != "" {
		t.Fatalf("posts: %+v", posts)
	}
	for _, p := range posts {
		if p.Query.Get("fieldValidation") != "Strict" || p.Token != fakeUserToken {
			t.Fatalf("post %+v: want strict validation with the user's token", p.Query)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(p.Body), &body)
		if !reflect.DeepEqual(body, asMap(t, modelregistry.Body(mrNS))) {
			t.Fatalf("posted %s", p.Body)
		}
	}
	rec, err := modelregistry.ReadRecord(e.sa)
	if err != nil {
		t.Fatal(err)
	}
	cr := e.f.get(mrPath)
	if uid := cr["metadata"].(map[string]any)["uid"]; rec[mrNS+"/nightly-registry"].UID != uid || uid == "" {
		t.Fatalf("record %+v, CR uid %v", rec, uid)
	}
	var ids []string
	for _, s := range steps {
		if s.Status == "success" {
			ids = append(ids, s.Step)
		}
	}
	if strings.Join(ids, ",") != "check,create,ready,samples" {
		t.Fatalf("steps %v", ids)
	}
	// Sample registration should mirror the dashboard's HTTP sequence, each call using the user token.
	want := []string{"POST /registered_models", "POST /registered_models/1/versions", "POST /model_versions/2/artifacts"}
	if got := e.reg.writes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry writes %v", got)
	}
	for _, c := range e.reg.calls {
		if c.Token != fakeUserToken {
			t.Fatalf("registry call %s %s with token %q", c.Method, c.Path, c.Token)
		}
	}
	model := e.reg.body(http.MethodPost, "/registered_models")
	if model["name"] != "qwen25-05b-instruct" || model["owner"] != "alice" || model["state"] != "LIVE" ||
		!reflect.DeepEqual(model["customProperties"], map[string]any{"model_type": map[string]any{"metadataType": "MetadataStringValue", "string_value": "generative"}}) {
		t.Fatalf("model body %v", model)
	}
	version := e.reg.body(http.MethodPost, "/registered_models/1/versions")
	if version["name"] != "v1" || version["author"] != "alice" || version["registeredModelId"] != "1" || version["state"] != "LIVE" {
		t.Fatalf("version body %v", version)
	}
	art := e.reg.body(http.MethodPost, "/model_versions/2/artifacts")
	if art["artifactType"] != "model-artifact" || art["name"] != "v1" || art["uri"] != modelregistry.SampleURI() || art["modelFormatName"] != "safetensors" || art["author"] != nil {
		t.Fatalf("artifact body %v", art)
	}
	if !strings.HasPrefix(modelregistry.SampleURI(), "hf://") {
		t.Fatalf("sample URI %s", modelregistry.SampleURI())
	}

	// Status fetched as the end user must report ready and include a registered sample.
	st, err := GetModelRegistryStatus(e.sa, e.u)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "ready" || !st.ManagedByTool || st.Component != "modelregistry" || st.Namespace != mrNS ||
		st.Address != e.reg.url+"/model-registry/nightly-registry" || st.Sample == nil || !st.Sample.Registered || st.Sample.State != "LIVE" {
		t.Fatalf("status %+v sample %+v", st, st.Sample)
	}
}

// When the CRD lacks default-database support (pre–RHOAI 3.4), setup aborts before
// any cluster object is created.
func TestModelRegistrySetup_RefusesWithoutTheDefaultDatabase(t *testing.T) {
	e := newMREnv(t)
	e.f.failDryRun["POST "+mrCollection] = 422
	e.f.failMessage["DRYRUN POST "+mrCollection] = `ModelRegistry.modelregistry.opendatahub.io "nightly-registry" is invalid: unknown field "spec.postgres.generateDeployment"`
	res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "no default database") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("got %+v", res)
	}
	if e.f.has(mrPath) || len(e.reg.writes()) > 0 {
		t.Fatal("something was created")
	}
}

func TestModelRegistrySetup_RefusesWhenTheComponentIsOff(t *testing.T) {
	e := newMREnv(t)
	putDSC(e.f, `{"modelregistry":{"managementState":"Removed"}}`)
	res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup"})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "modelregistry is Removed") {
		t.Fatalf("got %+v", res)
	}
	if len(e.f.callsTo(http.MethodPost, mrCollection)) > 0 {
		t.Fatal("posted")
	}
}

// A same-named registry not owned by the tool stays untouched—setup rejects, teardown skips delete.
func TestModelRegistry_ForeignRegistryIsKept(t *testing.T) {
	e := newMREnv(t)
	e.f.putJSON(mrPath, `{"metadata":{"labels":{}}}`)
	res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup"})
	if res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("setup: %+v", res)
	}
	st, err := GetModelRegistryStatus(e.sa, e.u)
	if err != nil || !st.Foreign || st.ManagedByTool || st.Sample != nil {
		t.Fatalf("status %+v %v", st, err)
	}
	// Same behavior if the object carries the tool label but bookkeeping references a different UID.
	e.f.putJSON(mrPath, `{"metadata":{"uid":"theirs","labels":`+`{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}`+`}}`)
	e.f.putJSON(testSnapshotCM, `{"data":{"modelRegistries":"{\"`+mrNS+`/nightly-registry\":{\"uid\":\"ours\"}}"}}`)
	res, _ = runMR(t, e.u, types.ModelRegistryRequest{Action: "teardown"})
	if res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("teardown: %+v", res)
	}
	if len(e.f.callsTo(http.MethodDelete, mrPath)) > 0 || !e.f.has(mrPath) {
		t.Fatal("the foreign registry was deleted")
	}
}

func TestModelRegistryTeardown_DeletesTheRecordedRegistryByUID(t *testing.T) {
	e := newMREnv(t)
	if res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup"}); !res.Success {
		t.Fatal(res.Message)
	}
	uid := e.f.get(mrPath)["metadata"].(map[string]any)["uid"]
	res, steps := runMR(t, e.u, types.ModelRegistryRequest{Action: "teardown"})
	if !res.Success || !strings.Contains(res.Message, "registered models and its database") {
		t.Fatalf("teardown %+v", res)
	}
	dels := e.f.callsTo(http.MethodDelete, mrPath)
	if len(dels) != 1 || !strings.Contains(dels[0].Body, `"uid":"`+uid.(string)+`"`) || dels[0].Token != fakeUserToken {
		t.Fatalf("deletes %+v", dels)
	}
	if e.f.has(mrPath) {
		t.Fatal("still there")
	}
	rec, _ := modelregistry.ReadRecord(e.sa)
	if len(rec) != 0 {
		t.Fatalf("record %v", rec)
	}
	if got := steps[len(steps)-1]; got.Step != "gone" || got.Status != "success" {
		t.Fatalf("last step %+v", got)
	}
}

// Re-running setup locates the archived sample by name and PATCHes it to LIVE because names remain
// reserved and the stub API offers no DELETE.
func TestModelRegistrySetup_AgainRestoresTheArchivedSample(t *testing.T) {
	e := newMREnv(t)
	if res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true}); !res.Success {
		t.Fatal(res.Message)
	}
	e.reg.mu.Lock()
	e.reg.models["qwen25-05b-instruct"]["state"] = "ARCHIVED"
	e.reg.versions["1"][0]["state"] = "ARCHIVED"
	e.reg.calls = nil
	e.reg.mu.Unlock()
	res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true})
	if !res.Success {
		t.Fatal(res.Message)
	}
	if got := e.reg.writes(); !reflect.DeepEqual(got, []string{"PATCH /registered_models/1", "PATCH /model_versions/2"}) {
		t.Fatalf("writes %v", got)
	}
	if b := e.reg.body(http.MethodPatch, "/registered_models/1"); !reflect.DeepEqual(b, map[string]any{"state": "LIVE"}) {
		t.Fatalf("patch %v", b)
	}
	if n := len(e.f.callsTo(http.MethodPost, mrCollection)); n != 2 {
		t.Fatalf("the registry was created again (%d posts)", n)
	}
}

// Missing gateway address still allows registry setup; sample registration is omitted.
func TestModelRegistrySetup_SkipsSamplesWithoutAGatewayAddress(t *testing.T) {
	e := newMREnv(t)
	e.putService(false)
	res, steps := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true})
	if !res.Success || len(e.reg.calls) > 0 {
		t.Fatalf("%+v calls %v", res, e.reg.calls)
	}
	if last := steps[len(steps)-1]; last.Step != "samples" || !strings.Contains(last.Message, "Skipped") {
		t.Fatalf("last step %+v", last)
	}
}

func TestModelRegistryStatus_States(t *testing.T) {
	e := newMREnv(t)
	if _, err := GetModelRegistryStatus(e.sa, e.sa); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("the ServiceAccount's client: %v", err)
	}
	st, err := GetModelRegistryStatus(e.sa, e.u)
	if err != nil || st.State != "absent" || st.Blocked != nil {
		t.Fatalf("absent: %+v %v", st, err)
	}
	e.availableAfter = 0
	if res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup"}); res.Success {
		t.Fatal("never available, yet set up")
	}
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if st.State != "starting" || !st.ManagedByTool || !strings.Contains(st.Message, "progressing") {
		t.Fatalf("starting: %+v", st)
	}
	e.availableAfter = 1
	e.gets = 0
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if st.State != "ready" || st.Sample == nil || st.Sample.Registered {
		t.Fatalf("ready: %+v %+v", st, st.Sample)
	}
	obj := e.f.get(mrPath)
	obj["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-24T10:00:00Z"
	e.f.put(mrPath, obj)
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if st.State != "terminating" {
		t.Fatalf("terminating: %+v", st)
	}
	// With the component Removed, status is absent and includes a blocked prerequisite.
	putDSC(e.f, `{"modelregistry":{"managementState":"Removed"}}`)
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if st.State != "absent" || st.Blocked == nil || st.Blocked.Component != "modelregistry" {
		t.Fatalf("blocked: %+v", st)
	}
}

// Under DataScienceCluster v3 (RHOAI 3.6) the feature maps to aiHub and instancesNamespace; the
// prerequisites plan should enable aiHub when it is missing.
func TestModelRegistry_V3AIHub(t *testing.T) {
	e := newMREnv(t)
	e.f.groups["/apis/datasciencecluster.opendatahub.io"] = dscGroupV3
	e.f.putJSON(mrDSCV3Path, `{"spec":{"components":{"aiHub":{"managementState":"Removed"}}},"status":{"components":{"aiHub":{}}}}`)
	ps, err := GetPrerequisitesStatusFor(e.sa, "")
	if err != nil {
		t.Fatal(err)
	}
	mr := ps.Features["model-registry"]
	if len(mr.Parts) != 1 || mr.Parts[0].ID != "dsc-aiHub" || mr.Parts[0].State != "missing" || len(mr.SetupPlan) != 1 || !strings.Contains(mr.SetupPlan[0], "set aiHub to Managed") {
		t.Fatalf("plan %+v", mr)
	}
	e.f.putJSON(mrDSCV3Path, `{"spec":{"components":{"aiHub":{"managementState":"Managed"}}},"status":{"components":{"aiHub":{"instancesNamespace":"`+mrNS+`"}}}}`)
	st, err := GetModelRegistryStatus(e.sa, e.u)
	if err != nil || st.Component != "aiHub" || st.Namespace != mrNS || st.Blocked != nil {
		t.Fatalf("status %+v %v", st, err)
	}
}

// Clean up turns the model registry component back only when this tool
// turned it on, still owns the field, and listing the registries in its
// namespace shows none left; a list that fails keeps it on.
func TestRestoreComponents_ModelRegistryComponent(t *testing.T) {
	setUp := func(t *testing.T) *prereqCluster {
		pc := newPrereqCluster(t)
		dsc := pc.f.get(tDSC)
		dsc["spec"].(map[string]any)["components"].(map[string]any)["modelregistry"] = map[string]any{"managementState": "Removed", "registriesNamespace": mrNS}
		pc.f.put(tDSC, dsc)
		if res, _ := runPrereqs(t, pc.c, "model-registry"); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		if componentState(pc.f, "modelregistry") != "Managed" {
			t.Fatal("not turned on")
		}
		if st := prereqStatus(t, pc.c, "model-registry"); len(st.Restorable) != 1 || st.Restorable[0].Component != "modelregistry" {
			t.Fatalf("restorable %+v", st.Restorable)
		}
		return pc
	}
	t.Run("no registry left: turned back", func(t *testing.T) {
		pc := setUp(t)
		res, err := features.RestoreComponents(pc.c, []string{"modelregistry"})
		if err != nil || !res.Success || !strings.Contains(res.Message, "modelregistry is Removed again") || componentState(pc.f, "modelregistry") != "Removed" {
			t.Fatalf("%+v %v", res, err)
		}
		if lists := pc.f.callsTo(http.MethodGet, mrCollection); len(lists) != 1 {
			t.Fatalf("lists %+v", lists)
		}
	})
	t.Run("another registry left: kept on", func(t *testing.T) {
		pc := setUp(t)
		pc.f.putJSON(mrCollection+"/theirs", `{"metadata":{}}`)
		res, _ := features.RestoreComponents(pc.c, []string{"modelregistry"})
		if !res.Success || !strings.Contains(res.Message, "Not turned back: modelregistry (in use: model registry "+mrNS+"/theirs)") || componentState(pc.f, "modelregistry") != "Managed" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("a registry being deleted: kept on until it is gone", func(t *testing.T) {
		pc := setUp(t)
		pc.f.putJSON(mrCollection+"/going", `{"metadata":{"deletionTimestamp":"2026-11-24T10:00:00Z","finalizers":["modelregistry.opendatahub.io/finalizer"]}}`)
		res, _ := features.RestoreComponents(pc.c, []string{"modelregistry"})
		if !res.Success || !strings.Contains(res.Message, "in use: model registry "+mrNS+"/going") || componentState(pc.f, "modelregistry") != "Managed" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("the list fails: kept on", func(t *testing.T) {
		pc := setUp(t)
		pc.f.fail["GET "+mrCollection] = 403
		res, _ := features.RestoreComponents(pc.c, []string{"modelregistry"})
		if res.Success || !strings.Contains(res.Message, "Cannot tell whether anything still uses modelregistry") || componentState(pc.f, "modelregistry") != "Managed" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("changed by someone since: kept on", func(t *testing.T) {
		pc := setUp(t)
		someoneSets(t, pc, "modelregistry", "Removed")
		someoneSets(t, pc, "modelregistry", "Managed")
		res, _ := features.RestoreComponents(pc.c, []string{"modelregistry"})
		if !res.Success || !strings.Contains(res.Message, "changed by someone since") || componentState(pc.f, "modelregistry") != "Managed" {
			t.Fatalf("%+v", res)
		}
	})
	if !RestorableComponentKey("aiHub") || !RestorableComponentKey("modelregistry") {
		t.Fatal("not restorable keys")
	}
}

func TestValidateModelRegistryRequest(t *testing.T) {
	for req, ok := range map[types.ModelRegistryRequest]bool{
		{Action: "setup"}: true, {Action: "setup", Samples: true}: true, {Action: "teardown"}: true,
		{Action: "teardown", Samples: true}: false, {Action: "delete"}: false, {}: false,
	} {
		if got := ValidateModelRegistryRequest(req) == ""; got != ok {
			t.Errorf("%+v: valid %v", req, got)
		}
	}
}

func kubeFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// C1: the user's token goes only to the cluster's own gateway, at the
// registry's own path, through a Service the recorded registry owns.
func TestModelRegistry_TokenGoesOnlyToTheClustersGateway(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *mrEnv, evil *fakeRegistryAPI)
		want  string
	}{
		{"another https host", func(e *mrEnv, evil *fakeRegistryAPI) {
			e.address = strings.TrimPrefix(evil.url, "https://") + "/model-registry/nightly-registry"
		}, "not the cluster's gateway"},
		{"another path on the gateway", func(e *mrEnv, evil *fakeRegistryAPI) {
			e.address = strings.TrimPrefix(e.reg.url, "https://") + "/model-registry/someone-else"
		}, "not the registry's own path"},
		{"a Service the registry does not own", func(e *mrEnv, evil *fakeRegistryAPI) { e.serviceNotOwned = true }, "not owned by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMREnv(t)
			evil := newFakeRegistryAPI(t)
			tc.setup(e, evil)
			e.putService(true)
			res, steps := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true})
			if res.Success || !strings.Contains(res.Message, tc.want) || !strings.Contains(res.Message, "Nothing was sent") {
				t.Fatalf("got %+v", res)
			}
			if last := steps[len(steps)-1]; last.Step != "samples" || last.Status != "failed" {
				t.Fatalf("last step %+v", last)
			}
			if len(evil.calls) > 0 || len(e.reg.calls) > 0 {
				t.Fatalf("a request was sent: %v %v", evil.calls, e.reg.calls)
			}
			st, err := GetModelRegistryStatus(e.sa, e.u)
			if err != nil || st.Address != "" || st.Sample != nil || !strings.Contains(st.AddressProblem, tc.want) {
				t.Fatalf("status %+v %v", st, err)
			}
			if len(evil.calls) > 0 || len(e.reg.calls) > 0 {
				t.Fatal("the status sent a request")
			}
		})
	}
}

// C3: the sample is complete only with its version and artifact, all live;
// Set up again completes it.
func TestModelRegistrySample_IncompleteUntilVersionAndArtifactAreThere(t *testing.T) {
	e := newMREnv(t)
	if res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup"}); !res.Success {
		t.Fatal(res.Message)
	}
	e.reg.mu.Lock()
	e.reg.models["qwen25-05b-instruct"] = map[string]any{"id": "7", "name": "qwen25-05b-instruct", "state": "LIVE"}
	e.reg.next = 7
	e.reg.mu.Unlock()
	st, _ := GetModelRegistryStatus(e.sa, e.u)
	if st.Sample == nil || !st.Sample.Registered || st.Sample.Complete {
		t.Fatalf("sample %+v", st.Sample)
	}
	if res, _ := runMR(t, e.u, types.ModelRegistryRequest{Action: "setup", Samples: true}); !res.Success {
		t.Fatal(res.Message)
	}
	if got := e.reg.writes(); !reflect.DeepEqual(got, []string{"POST /registered_models/7/versions", "POST /model_versions/8/artifacts"}) {
		t.Fatalf("writes %v", got)
	}
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if !st.Sample.Complete || st.Sample.State != "LIVE" {
		t.Fatalf("sample %+v", st.Sample)
	}
}

// C4: a registry someone else made keeps its own lifecycle state.
func TestModelRegistryStatus_ForeignKeepsItsLifecycle(t *testing.T) {
	e := newMREnv(t)
	e.availableAfter = 0
	e.f.putJSON(mrPath, `{"metadata":{"labels":{}}}`)
	st, err := GetModelRegistryStatus(e.sa, e.u)
	if err != nil || st.State != "starting" || !st.Foreign || st.ManagedByTool {
		t.Fatalf("starting: %+v %v", st, err)
	}
	e.availableAfter = 1
	st, _ = GetModelRegistryStatus(e.sa, e.u)
	if st.State != "ready" || !st.Foreign || st.Sample != nil {
		t.Fatalf("ready: %+v", st)
	}
}

// C6: Clean up deletes the registry with the user's token before the
// component; a teardown that fails keeps the component on.
func TestCleanUp_ModelRegistryThenItsComponent(t *testing.T) {
	setUp := func(t *testing.T, ours bool) (*prereqCluster, *Client) {
		pc := newPrereqCluster(t)
		pc.f.groups = map[string]string{"/apis/modelregistry.opendatahub.io": mrGroupDoc}
		dsc := pc.f.get(tDSC)
		dsc["spec"].(map[string]any)["components"].(map[string]any)["modelregistry"] = map[string]any{"managementState": "Removed", "registriesNamespace": mrNS}
		pc.f.put(tDSC, dsc)
		if res, _ := runPrereqs(t, pc.c, "model-registry"); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		pc.f.putJSON(mrPath, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		uid := pc.f.get(mrPath)["metadata"].(map[string]any)["uid"].(string)
		if !ours {
			uid = "someone-elses"
		}
		cm := pc.f.get(testSnapshotCM)
		ensureMap(cm, "data")["modelRegistries"] = `{"` + mrNS + `/nightly-registry":{"uid":"` + uid + `"}}`
		pc.f.put(testSnapshotCM, cm)
		u := *pc.c
		editTestClient(&u, func(o *testClientOptions) { o.Token, o.AsUser = fakeUserToken, true })
		u.SetBookkeeping(pc.c)
		u.SetFacts(pc.c)
		return pc, &u
	}
	req := types.CleanupRequest{ModelRegistry: true, Components: []string{"modelregistry"}}
	t.Run("in order, with the user's token", func(t *testing.T) {
		pc, u := setUp(t, true)
		var steps []string
		res, err := CleanUp(u, req, func(e UpdateStepEvent) {
			if e.Status == "success" {
				steps = append(steps, e.Step)
			}
		})
		if err != nil || !res.Success || componentState(pc.f, "modelregistry") != "Removed" || pc.f.has(mrPath) {
			t.Fatalf("%+v %v", res, err)
		}
		if strings.Join(steps, ",") != "model-registry,components" {
			t.Fatalf("steps %v", steps)
		}
		for _, call := range append(pc.f.callsTo(http.MethodDelete, mrPath), pc.f.callsTo(http.MethodGet, mrCollection)...) {
			if call.Token != fakeUserToken {
				t.Fatalf("%s %s with %q", call.Method, call.Path, call.Token)
			}
		}
	})
	t.Run("a failed teardown keeps the component on", func(t *testing.T) {
		pc, u := setUp(t, false)
		var failed []string
		res, _ := CleanUp(u, req, func(e UpdateStepEvent) {
			if e.Status == "failed" {
				failed = append(failed, e.Step)
			}
		})
		if res.Success || componentState(pc.f, "modelregistry") != "Managed" || !pc.f.has(mrPath) || strings.Join(failed, ",") != "model-registry" {
			t.Fatalf("%+v failed=%v", res, failed)
		}
	})
}
