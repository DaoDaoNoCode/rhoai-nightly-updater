package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// mlflowFake is a small MLflow server for the samples tests. It serves only
// the routes in mlflowFakeRules and authorizes each request like the
// kubernetes-auth plugin (mlflow-kubernetes-plugins v2.1.1): every check of
// the rule without a name first, then, when denied and the rule has a name
// parser, with the resolved name (auth/core.py:812-851). A route without a
// rule answers 404 ENDPOINT_NOT_FOUND before any check (core.py:700-714).
type mlflowFake struct {
	t  *testing.T
	mu sync.Mutex
	// allow decides an access review; nil allows everything.
	allow func(resource, verb, namespace, name string) bool
	// used records the rules that were matched.
	used map[string]bool
	// failNext maps "METHOD path" to a status to answer once.
	failNext map[string]int
	// bareNext maps "METHOD path" to a status answered once with an empty
	// body, as the gateway answers while MLflow restarts.
	bareNext map[string]int
	// noSkills: this MLflow has no Skill Registry (RHOAI 3.5).
	noSkills bool
	// noDemo: this MLflow has no demo generator.
	noDemo bool
	// onRequest, when set, runs before each request is served.
	onRequest func(method, path string)
	requests  []string

	nextID      int
	experiments map[string]*fakeExperiment // by id
	runs        map[string]*fakeRun
	models      map[string]*fakeModel
	mcp         map[string]*fakeMCP
	skills      map[string]*fakeSkill
	demo        fakeDemo
	generates   int
}

type fakeExperiment struct {
	id, name, stage string
	tags            map[string]string
}

type fakeRun struct {
	id, exp, name, stage, status string
	params, tags                 map[string]string
	metrics                      int
}

type fakeModel struct {
	tags     map[string]string
	versions []string // run ids
	aliases  map[string]string
}

type fakeMCP struct {
	tags        map[string]string
	versions    map[string]string // version -> status
	endpoints   int
	description string
	createdBy   string
}

type fakeSkill struct {
	tags     map[string]string
	versions int          // the highest version number
	deleted  map[int]bool // versions deleted since
	source   map[string]interface{}
	// createdBy is the parent's creator; versions are the caller's.
	createdBy string
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fakeCaller is the user MLflow records as created_by for the tool's token.
const fakeCaller = "system:serviceaccount:rhoai-nightly-updater:rhoai-nightly-updater"

// orNull is created_by as MLflow answers it: null when unknown.
func orNull(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// fakeDemo mirrors mlflow/demo: generator versions per feature
// (generators/*.py) and, per feature, whether its data exists and whether
// that data is broken (left behind by MLflow's own demo/delete, see
// samples_demo.go).
type fakeDemo struct {
	versions map[string]int
	data     map[string]bool
	broken   map[string]bool
}

// fakeCheck is one access review a rule makes.
type fakeCheck struct{ resource, verb string }

// mlflowFakeRule is the plugin's rule for one route, as the samples call it.
type mlflowFakeRule struct {
	method, route string
	checks        []fakeCheck
	// name is how the plugin resolves the resource name ("" = no name
	// parser, so only the unnamed check counts).
	name string
	// names are the sample names the code can send to this route; the
	// fake fails a request whose resolved name is not one of them.
	names []string
	cite  string
}

var demoNames = []string{demoExperiment}
var trainingExpNames = []string{trainingExperiment}

// mlflowFakeRules lists every MLflow route the samples call, with the
// authorization of mlflow-kubernetes-plugins v2.1.1 (9694e137).
var mlflowFakeRules = []mlflowFakeRule{
	{"GET", "/api/2.0/mlflow/experiments/get-by-name", []fakeCheck{{"experiments", "get"}}, "query:experiment_name", []string{demoExperiment, trainingExperiment}, "rules_base.py:323-326"},
	{"POST", "/api/2.0/mlflow/experiments/create", []fakeCheck{{"experiments", "create"}}, "", nil, "rules_base.py:319"},
	{"POST", "/api/2.0/mlflow/experiments/restore", []fakeCheck{{"experiments", "update"}}, "body:experiment_id", []string{demoExperiment, trainingExperiment}, "rules_base.py:331-334"},
	{"POST", "/api/2.0/mlflow/experiments/delete", []fakeCheck{{"experiments", "delete"}}, "body:experiment_id", trainingExpNames, "rules_base.py:327-330"},
	{"POST", "/api/2.0/mlflow/experiments/set-experiment-tag", []fakeCheck{{"experiments", "update"}}, "body:experiment_id", demoNames, "rules_base.py:342-345"},
	{"POST", "/api/2.0/mlflow/experiments/delete-experiment-tag", []fakeCheck{{"experiments", "update"}}, "body:experiment_id", demoNames, "rules_base.py:347-350"},
	// SearchRuns: experiments list, else the experiment_ids the caller may
	// get by name (rules_base.py:653-656; collection_filters.py:123-139,
	// 161-174).
	{"POST", "/api/2.0/mlflow/runs/search", []fakeCheck{{"experiments", "list"}}, "body:experiment_ids:get", trainingExpNames, "rules_base.py:653-656"},
	{"POST", "/api/2.0/mlflow/runs/create", []fakeCheck{{"experiments", "update"}}, "body:experiment_id", trainingExpNames, "rules_base.py:403-407"},
	{"POST", "/api/2.0/mlflow/runs/log-batch", []fakeCheck{{"experiments", "update"}}, "body:run_id", trainingExpNames, "rules_base.py:428-431"},
	{"POST", "/api/2.0/mlflow/runs/update", []fakeCheck{{"experiments", "update"}}, "body:run_id", trainingExpNames, "rules_base.py:420-423"},
	{"GET", "/api/2.0/mlflow/registered-models/get", []fakeCheck{{"registeredmodels", "get"}}, "query:name", []string{trainingModel}, "rules_base.py:659-662"},
	{"POST", "/api/2.0/mlflow/registered-models/create", []fakeCheck{{"registeredmodels", "create"}}, "", nil, "rules_base.py:658"},
	{"DELETE", "/api/2.0/mlflow/registered-models/delete", []fakeCheck{{"registeredmodels", "delete"}}, "body:name", []string{trainingModel}, "rules_base.py:663-666"},
	{"GET", "/api/2.0/mlflow/model-versions/get", []fakeCheck{{"registeredmodels", "get"}}, "query:name", []string{trainingModel}, "rules_base.py:738-741"},
	{"POST", "/api/2.0/mlflow/model-versions/create", []fakeCheck{{"registeredmodels", "update"}}, "body:name", []string{trainingModel}, "rules_base.py:682-685"},
	{"POST", "/api/2.0/mlflow/registered-models/alias", []fakeCheck{{"registeredmodels", "update"}}, "body:name", []string{trainingModel}, "rules_base.py:714-717"},
	{"POST", "/ajax-api/3.0/mlflow/demo/generate", []fakeCheck{{"experiments", "create"}, {"datasets", "create"}, {"registeredmodels", "create"}}, "", nil, "rules_base.py:1178-1182"},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}", []fakeCheck{{"mcpservers", "get"}}, "path", []string{mcpSampleName}, "rules_base.py:1432-1435"},
	{"PATCH", "/api/3.0/mlflow/mcp-servers/{mcp}", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1436-1439"},
	{"DELETE", "/api/3.0/mlflow/mcp-servers/{mcp}", []fakeCheck{{"mcpservers", "delete"}}, "path", []string{mcpSampleName}, "rules_base.py:1440-1443"},
	// A version POST auto-creates a missing parent server
	// (sqlalchemy_mixin.py:272-330) and is checked as update on it.
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/versions", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1351-1354"},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}/versions", []fakeCheck{{"mcpservers", "get"}}, "path", []string{mcpSampleName}, "rules_base.py:1355-1358"},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []fakeCheck{{"mcpservers", "get"}}, "path", []string{mcpSampleName}, "rules_base.py:1359-1362"},
	{"DELETE", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1367-1373"},
	{"PATCH", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1363-1366"},
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/endpoints", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1388-1391"},
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/tags", []fakeCheck{{"mcpservers", "update"}}, "path", []string{mcpSampleName}, "rules_base.py:1411-1414"},
	{"GET", "/api/3.0/mlflow/skills/{skill}", []fakeCheck{{"skills", "get"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:111-113"},
	{"DELETE", "/api/3.0/mlflow/skills/{skill}", []fakeCheck{{"skills", "delete"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:117-119"},
	// "Like MCP, version registration uses parent update even if MLflow
	// creates the parent" (rules_skill_registry.py:30-33).
	{"POST", "/api/3.0/mlflow/skills/{skill}/versions", []fakeCheck{{"skills", "update"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:78-80"},
	{"POST", "/api/3.0/mlflow/skills/{skill}/tags", []fakeCheck{{"skills", "update"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:105-107"},
	{"GET", "/api/3.0/mlflow/skills/{skill}/versions", []fakeCheck{{"skills", "get"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:80-82"},
	{"DELETE", "/api/3.0/mlflow/skills/{skill}/versions/{v}", []fakeCheck{{"skills", "update"}}, "path", []string{skillSampleName}, "rules_skill_registry.py:89-91"},
}

func (r mlflowFakeRule) key() string { return r.method + " " + r.route }

var routeParam = regexp.MustCompile(`\{(mcp|skill|v)\}`)

func (r mlflowFakeRule) match(method, path string) (map[string]string, bool) {
	if method != r.method {
		return nil, false
	}
	var names []string
	pattern := "^"
	rest := r.route
	for {
		loc := routeParam.FindStringSubmatchIndex(rest)
		if loc == nil {
			pattern += regexp.QuoteMeta(rest)
			break
		}
		pattern += regexp.QuoteMeta(rest[:loc[0]])
		name := rest[loc[2]:loc[3]]
		names = append(names, name)
		if name == "mcp" {
			pattern += `([^/]+/[^/]+)`
		} else {
			pattern += `([^/]+)`
		}
		rest = rest[loc[1]:]
	}
	m := regexp.MustCompile(pattern + "$").FindStringSubmatch(path)
	if m == nil {
		return nil, false
	}
	params := map[string]string{}
	for i, n := range names {
		params[n] = m[i+1]
	}
	return params, true
}

const fakeProject = "proj"

func newMLflowFake(t *testing.T) *mlflowFake {
	return &mlflowFake{
		t: t, used: map[string]bool{}, failNext: map[string]int{}, bareNext: map[string]int{},
		experiments: map[string]*fakeExperiment{}, runs: map[string]*fakeRun{},
		models: map[string]*fakeModel{}, mcp: map[string]*fakeMCP{}, skills: map[string]*fakeSkill{},
		demo: fakeDemo{
			// The registered generators and their versions at rhoai-3.6
			// (generators/__init__.py; version attributes).
			versions: map[string]int{"prompts": 1, "traces": 1, "custom_view": 1, "saved_views": 1, "evaluation": 2, "judges": 1, "issues": 4, "review_queues": 1},
			data:     map[string]bool{}, broken: map[string]bool{},
		},
	}
}

func (f *mlflowFake) mlflowError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error_code": code, "message": msg})
}

func (f *mlflowFake) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *mlflowFake) expByName(name string) *fakeExperiment {
	for _, e := range f.experiments {
		if e.name == name {
			return e
		}
	}
	return nil
}

func (f *mlflowFake) newID() string {
	f.nextID++
	return strconv.Itoa(f.nextID)
}

// resolveName mirrors the plugin's resource-name parsers
// (auth/resource_names.py).
func (f *mlflowFake) resolveName(kind string, r *http.Request, body map[string]interface{}, params map[string]string) []string {
	str := func(k string) string { s, _ := body[k].(string); return s }
	switch kind {
	case "query:experiment_name":
		return []string{r.URL.Query().Get("experiment_name")}
	case "query:name":
		return []string{r.URL.Query().Get("name")}
	case "body:name":
		return []string{str("name")}
	case "body:experiment_id":
		if e := f.experiments[str("experiment_id")]; e != nil {
			return []string{e.name}
		}
	case "body:run_id":
		if run := f.runs[str("run_id")]; run != nil {
			return []string{f.experiments[run.exp].name}
		}
	case "body:experiment_ids:get":
		var out []string
		ids, _ := body["experiment_ids"].([]interface{})
		for _, id := range ids {
			if e := f.experiments[fmt.Sprint(id)]; e != nil {
				out = append(out, e.name)
			}
		}
		return out
	case "path":
		return []string{firstNonEmpty(params["mcp"], params["skill"])}
	}
	return nil
}

// authorize returns false when the plugin would deny the request.
func (f *mlflowFake) authorize(rule mlflowFakeRule, ns string, names []string) bool {
	if f.allow == nil {
		return true
	}
	for _, c := range rule.checks {
		if f.allow(c.resource, c.verb, ns, "") {
			continue
		}
		if rule.name == "" || len(names) == 0 {
			return false
		}
		verb := c.verb
		if rule.name == "body:experiment_ids:get" {
			verb = "get" // the collection filter's named read
		}
		for _, n := range names {
			if !f.allow(c.resource, verb, ns, n) {
				return false
			}
		}
	}
	return true
}

func (f *mlflowFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/mlflow")
	f.requests = append(f.requests, r.Method+" "+path)
	if f.onRequest != nil {
		f.onRequest(r.Method, path)
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		f.mlflowError(w, 401, "UNAUTHENTICATED", "no token")
		return
	}
	if status, ok := f.failNext[r.Method+" "+path]; ok {
		delete(f.failNext, r.Method+" "+path)
		f.mlflowError(w, status, "INTERNAL_ERROR", "injected failure")
		return
	}
	if status, ok := f.bareNext[r.Method+" "+path]; ok {
		delete(f.bareNext, r.Method+" "+path)
		w.WriteHeader(status)
		return
	}
	var rule mlflowFakeRule
	var params map[string]string
	found := false
	for _, ru := range mlflowFakeRules {
		if p, ok := ru.match(r.Method, path); ok {
			rule, params, found = ru, p, true
			break
		}
	}
	if !found || (f.noSkills && strings.HasPrefix(path, "/api/3.0/mlflow/skills")) || (f.noDemo && strings.HasSuffix(path, "/demo/generate")) {
		f.mlflowError(w, 404, "ENDPOINT_NOT_FOUND", "Endpoint not found.")
		return
	}
	ns := r.Header.Get(mlflowWorkspaceHeader)
	if ns == "" {
		f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "Workspace context is required for this request.")
		return
	}
	f.used[rule.key()] = true
	body := map[string]interface{}{}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		if err := json.Unmarshal(data, &body); err != nil {
			f.mlflowError(w, 400, "MALFORMED_REQUEST", err.Error())
			return
		}
	}
	names := f.resolveName(rule.name, r, body, params)
	if rule.name != "" {
		for _, n := range names {
			if !contains(rule.names, n) {
				f.t.Errorf("%s sent for name %q, which mlflowFakeRules does not list", rule.key(), n)
			}
		}
	}
	if !f.authorize(rule, ns, names) {
		f.mlflowError(w, 403, "PERMISSION_DENIED", "Permission denied for requested operation.")
		return
	}
	f.handle(w, r, rule, params, body)
}

func tagsOf(body map[string]interface{}) map[string]string {
	out := map[string]string{}
	list, _ := body["tags"].([]interface{})
	for _, t := range list {
		m, _ := t.(map[string]interface{})
		out[fmt.Sprint(m["key"])] = fmt.Sprint(m["value"])
	}
	return out
}

func tagList(tags map[string]string) []map[string]string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []map[string]string{}
	for _, k := range keys {
		out = append(out, map[string]string{"key": k, "value": tags[k]})
	}
	return out
}

func (f *mlflowFake) expJSON(e *fakeExperiment) map[string]interface{} {
	return map[string]interface{}{"experiment": map[string]interface{}{
		"experiment_id": e.id, "name": e.name, "lifecycle_stage": e.stage, "tags": tagList(e.tags),
	}}
}

func (f *mlflowFake) handle(w http.ResponseWriter, r *http.Request, rule mlflowFakeRule, params map[string]string, body map[string]interface{}) {
	str := func(k string) string { s, _ := body[k].(string); return s }
	notFound := func(what string) { f.mlflowError(w, 404, "RESOURCE_DOES_NOT_EXIST", what+" not found") }
	switch rule.key() {
	case "GET /api/2.0/mlflow/experiments/get-by-name":
		e := f.expByName(r.URL.Query().Get("experiment_name"))
		if e == nil {
			notFound("experiment")
			return
		}
		f.writeJSON(w, f.expJSON(e))
	case "POST /api/2.0/mlflow/experiments/create":
		if f.expByName(str("name")) != nil {
			f.mlflowError(w, 400, "RESOURCE_ALREADY_EXISTS", "experiment exists")
			return
		}
		e := &fakeExperiment{id: f.newID(), name: str("name"), stage: "active", tags: tagsOf(body)}
		f.experiments[e.id] = e
		f.writeJSON(w, map[string]string{"experiment_id": e.id})
	case "POST /api/2.0/mlflow/experiments/restore":
		e := f.experiments[str("experiment_id")]
		if e == nil || e.stage != "deleted" {
			notFound("deleted experiment")
			return
		}
		e.stage = "active"
		// restore_experiment marks every run active (sqlalchemy_store.py:1050-1058).
		for _, run := range f.runs {
			if run.exp == e.id {
				run.stage = "active"
			}
		}
		f.writeJSON(w, map[string]string{})
	case "POST /api/2.0/mlflow/experiments/delete":
		e := f.experiments[str("experiment_id")]
		if e == nil || e.stage != "active" {
			notFound("experiment")
			return
		}
		e.stage = "deleted"
		for _, run := range f.runs {
			if run.exp == e.id {
				run.stage = "deleted"
			}
		}
		f.writeJSON(w, map[string]string{})
	case "POST /api/2.0/mlflow/experiments/set-experiment-tag":
		e := f.experiments[str("experiment_id")]
		if e == nil {
			notFound("experiment")
			return
		}
		if e.stage != "active" { // _check_experiment_is_active (sqlalchemy_store.py:2152-2168)
			f.mlflowError(w, 400, "INVALID_STATE", "The experiment must be in the 'active' state.")
			return
		}
		e.tags[str("key")] = str("value")
		f.writeJSON(w, map[string]string{})
	case "POST /api/2.0/mlflow/experiments/delete-experiment-tag":
		e := f.experiments[str("experiment_id")]
		if e == nil || e.stage != "active" {
			notFound("experiment")
			return
		}
		if _, ok := e.tags[str("key")]; !ok {
			notFound("tag")
			return
		}
		delete(e.tags, str("key"))
		f.writeJSON(w, map[string]string{})
	case "POST /api/2.0/mlflow/runs/search":
		ids, _ := body["experiment_ids"].([]interface{})
		filter := str("filter")
		out := []interface{}{}
		var runIDs []string
		for id := range f.runs {
			runIDs = append(runIDs, id)
		}
		sort.Strings(runIDs)
		for _, id := range runIDs {
			run := f.runs[id]
			if run.stage != "active" || len(ids) == 0 || fmt.Sprint(ids[0]) != run.exp {
				continue
			}
			if filter != "" && !strings.Contains(filter, "'"+run.tags[sampleMarkerKey]+"'") {
				continue
			}
			var ps []map[string]string
			for k, v := range run.params {
				ps = append(ps, map[string]string{"key": k, "value": v})
			}
			out = append(out, map[string]interface{}{
				"info": map[string]string{"run_id": run.id, "run_name": run.name, "status": run.status, "lifecycle_stage": run.stage},
				"data": map[string]interface{}{"params": ps, "tags": tagList(run.tags)},
			})
		}
		f.writeJSON(w, map[string]interface{}{"runs": out})
	case "POST /api/2.0/mlflow/runs/create":
		e := f.experiments[str("experiment_id")]
		if e == nil || e.stage != "active" {
			notFound("experiment")
			return
		}
		run := &fakeRun{id: "run" + f.newID(), exp: e.id, name: str("run_name"), stage: "active", status: "RUNNING", params: map[string]string{}, tags: tagsOf(body)}
		f.runs[run.id] = run
		f.writeJSON(w, map[string]interface{}{"run": map[string]interface{}{"info": map[string]string{"run_id": run.id}}})
	case "POST /api/2.0/mlflow/runs/log-batch":
		run := f.runs[str("run_id")]
		if run == nil {
			notFound("run")
			return
		}
		ps, _ := body["params"].([]interface{})
		for _, p := range ps {
			m := p.(map[string]interface{})
			run.params[fmt.Sprint(m["key"])] = fmt.Sprint(m["value"])
		}
		ms, _ := body["metrics"].([]interface{})
		if len(ms) > 1000 {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "too many metrics")
			return
		}
		run.metrics += len(ms)
		f.writeJSON(w, map[string]string{})
	case "POST /api/2.0/mlflow/runs/update":
		run := f.runs[str("run_id")]
		if run == nil {
			notFound("run")
			return
		}
		run.status = str("status")
		f.writeJSON(w, map[string]string{})
	case "GET /api/2.0/mlflow/registered-models/get":
		m := f.models[r.URL.Query().Get("name")]
		if m == nil {
			notFound("model")
			return
		}
		var aliases []map[string]string
		for a, v := range m.aliases {
			aliases = append(aliases, map[string]string{"alias": a, "version": v})
		}
		f.writeJSON(w, map[string]interface{}{"registered_model": map[string]interface{}{"name": r.URL.Query().Get("name"), "tags": tagList(m.tags), "aliases": aliases}})
	case "POST /api/2.0/mlflow/registered-models/create":
		if f.models[str("name")] != nil {
			f.mlflowError(w, 400, "RESOURCE_ALREADY_EXISTS", "model exists")
			return
		}
		f.models[str("name")] = &fakeModel{tags: tagsOf(body), aliases: map[string]string{}}
		f.writeJSON(w, map[string]interface{}{"registered_model": map[string]string{"name": str("name")}})
	case "DELETE /api/2.0/mlflow/registered-models/delete":
		if f.models[str("name")] == nil {
			notFound("model")
			return
		}
		delete(f.models, str("name"))
		f.writeJSON(w, map[string]string{})
	case "GET /api/2.0/mlflow/model-versions/get":
		m := f.models[r.URL.Query().Get("name")]
		v, _ := strconv.Atoi(r.URL.Query().Get("version"))
		if m == nil || v < 1 || v > len(m.versions) {
			notFound("model version")
			return
		}
		f.writeJSON(w, map[string]interface{}{"model_version": map[string]string{"name": r.URL.Query().Get("name"), "version": strconv.Itoa(v), "run_id": m.versions[v-1]}})
	case "POST /api/2.0/mlflow/model-versions/create":
		m := f.models[str("name")]
		if m == nil {
			notFound("model")
			return
		}
		// The runs:/ URI's run id must equal run_id (handlers.py:3214-3221).
		if !strings.HasPrefix(str("source"), "runs:/"+str("run_id")+"/") {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "source run id does not match run_id")
			return
		}
		m.versions = append(m.versions, str("run_id"))
		f.writeJSON(w, map[string]interface{}{"model_version": map[string]string{"version": strconv.Itoa(len(m.versions))}})
	case "POST /api/2.0/mlflow/registered-models/alias":
		m := f.models[str("name")]
		if m == nil {
			notFound("model")
			return
		}
		v, _ := strconv.Atoi(str("version"))
		if v < 1 || v > len(m.versions) {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "no such version")
			return
		}
		m.aliases[str("alias")] = str("version")
		f.writeJSON(w, map[string]string{})
	case "POST /ajax-api/3.0/mlflow/demo/generate":
		f.generate(w)
	default:
		f.handleRegistry(w, rule, params, body)
	}
}

// generate mirrors handlers.py:7965-8012 and demo/__init__.py:20-48.
func (f *mlflowFake) generate(w http.ResponseWriter) {
	f.generates++
	features := make([]string, 0, len(f.demo.versions))
	for name := range f.demo.versions {
		features = append(features, name)
	}
	sort.Strings(features)
	e := f.expByName(demoExperiment)
	isGenerated := func(name string) bool {
		if e == nil || e.stage != "active" || !f.demo.data[name] {
			return false
		}
		stored, err := strconv.Atoi(e.tags[demoVersionTagPrefix+name])
		if err != nil || stored != f.demo.versions[name] {
			// delete_demo() of that generator
			f.demo.data[name], f.demo.broken[name] = false, false
			return false
		}
		return true
	}
	if e != nil && e.stage == "active" {
		all := true
		for _, name := range features {
			if !isGenerated(name) {
				all = false
			}
		}
		if all {
			f.writeJSON(w, map[string]interface{}{"status": "exists", "experiment_id": e.id, "features_generated": []string{}, "navigation_url": "/experiments/" + e.id})
			return
		}
	}
	if e == nil {
		e = &fakeExperiment{id: f.newID(), name: demoExperiment, stage: "active", tags: map[string]string{}}
		f.experiments[e.id] = e
	}
	e.stage = "active" // the generators restore a deleted demo experiment
	var generated []string
	for _, name := range features {
		if isGenerated(name) {
			continue
		}
		f.demo.data[name] = true
		e.tags[demoVersionTagPrefix+name] = strconv.Itoa(f.demo.versions[name])
		generated = append(generated, name)
	}
	f.writeJSON(w, map[string]interface{}{"status": "created", "experiment_id": e.id, "features_generated": generated, "navigation_url": "/experiments/" + e.id})
}

// mlflowDeleteDemo is MLflow's own demo/delete as seen live: every
// feature's data goes, except that evaluation runs and issues stay behind
// (now useless), the version tags stay, and the experiment is soft-deleted.
func (f *mlflowFake) mlflowDeleteDemo() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name := range f.demo.versions {
		f.demo.data[name] = false
	}
	for _, name := range []string{"evaluation", "issues"} {
		f.demo.data[name], f.demo.broken[name] = true, true
	}
	if e := f.expByName(demoExperiment); e != nil {
		e.stage = "deleted"
	}
}

func (f *mlflowFake) handleRegistry(w http.ResponseWriter, rule mlflowFakeRule, params map[string]string, body map[string]interface{}) {
	str := func(k string) string { s, _ := body[k].(string); return s }
	name := firstNonEmpty(params["mcp"], params["skill"])
	srv, sk := f.mcp[name], f.skills[name]
	notFound := func() { f.mlflowError(w, 404, "RESOURCE_DOES_NOT_EXIST", name+" not found") }
	switch rule.key() {
	case "GET /api/3.0/mlflow/mcp-servers/{mcp}":
		if srv == nil {
			notFound()
			return
		}
		status := "draft"
		for _, s := range srv.versions {
			if s == "active" {
				status = "active"
			} else if status != "active" {
				status = s
			}
		}
		eps := []map[string]string{}
		for i := 0; i < srv.endpoints; i++ {
			eps = append(eps, map[string]string{"id": strconv.Itoa(i)})
		}
		f.writeJSON(w, map[string]interface{}{"name": name, "status": status, "access_endpoints": eps, "tags": srv.tags, "description": srv.description, "created_by": orNull(srv.createdBy)})
	case "GET /api/3.0/mlflow/mcp-servers/{mcp}/versions":
		if srv == nil {
			notFound()
			return
		}
		list := []map[string]interface{}{}
		for _, v := range sortedKeys(srv.versions) {
			list = append(list, map[string]interface{}{"name": name, "version": v, "status": srv.versions[v], "created_by": fakeCaller})
		}
		f.writeJSON(w, map[string]interface{}{"mcp_server_versions": list})
	case "DELETE /api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}":
		if srv == nil {
			notFound()
			return
		}
		if _, ok := srv.versions[params["v"]]; !ok {
			notFound()
			return
		}
		delete(srv.versions, params["v"])
		f.writeJSON(w, map[string]string{})
	case "PATCH /api/3.0/mlflow/mcp-servers/{mcp}":
		if srv == nil {
			notFound()
			return
		}
		srv.description = str("description")
		f.writeJSON(w, map[string]string{"name": name})
	case "DELETE /api/3.0/mlflow/mcp-servers/{mcp}":
		if srv == nil {
			notFound()
			return
		}
		for _, s := range srv.versions {
			if s == "active" { // sqlalchemy_mixin.py:249-267
				f.mlflowError(w, 400, "INVALID_STATE", "cannot delete a server with an active version")
				return
			}
		}
		delete(f.mcp, name)
		f.writeJSON(w, map[string]string{})
	case "POST /api/3.0/mlflow/mcp-servers/{mcp}/versions":
		sj, _ := body["server_json"].(map[string]interface{})
		if fmt.Sprint(sj["name"]) != name {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "server_json.name does not match")
			return
		}
		remotes, _ := sj["remotes"].([]interface{})
		if len(remotes) == 0 {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "test fake expects remotes")
			return
		}
		if srv == nil {
			srv = &fakeMCP{tags: map[string]string{}, versions: map[string]string{}, createdBy: fakeCaller}
			f.mcp[name] = srv
		}
		v := fmt.Sprint(sj["version"])
		if _, dup := srv.versions[v]; dup {
			f.mlflowError(w, 400, "RESOURCE_ALREADY_EXISTS", "version exists")
			return
		}
		srv.versions[v] = firstNonEmpty(str("status"), "draft")
		f.writeJSON(w, map[string]string{"name": name, "version": v, "status": srv.versions[v], "created_by": fakeCaller})
	case "GET /api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}":
		if srv == nil {
			notFound()
			return
		}
		s, ok := srv.versions[mcpSampleVersion]
		if !ok {
			notFound()
			return
		}
		f.writeJSON(w, map[string]string{"name": name, "version": mcpSampleVersion, "status": s})
	case "PATCH /api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}":
		if srv == nil {
			notFound()
			return
		}
		cur, ok := srv.versions[mcpSampleVersion]
		next := str("status")
		allowed := map[string][]string{"draft": {"active", "deleted"}, "active": {"draft", "deprecated"}, "deprecated": {"active", "deleted"}}
		if !ok || !contains(allowed[cur], next) {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "invalid transition "+cur+" -> "+next)
			return
		}
		srv.versions[mcpSampleVersion] = next
		f.writeJSON(w, map[string]string{"status": next})
	case "POST /api/3.0/mlflow/mcp-servers/{mcp}/endpoints":
		if srv == nil {
			notFound()
			return
		}
		if str("url") == "" || str("transport_type") != "streamable-http" {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "bad endpoint")
			return
		}
		srv.endpoints++
		f.writeJSON(w, map[string]string{"id": "ep"})
	case "POST /api/3.0/mlflow/mcp-servers/{mcp}/tags":
		if srv == nil {
			notFound()
			return
		}
		srv.tags[str("key")] = str("value")
		f.writeJSON(w, map[string]string{})
	case "GET /api/3.0/mlflow/skills/{skill}":
		if sk == nil {
			notFound()
			return
		}
		var latest interface{}
		for v := sk.versions; v > 0; v-- {
			if !sk.deleted[v] {
				latest = v
				break
			}
		}
		f.writeJSON(w, map[string]interface{}{"name": name, "latest_version": latest, "tags": sk.tags, "created_by": orNull(sk.createdBy)})
	case "GET /api/3.0/mlflow/skills/{skill}/versions":
		if sk == nil {
			notFound()
			return
		}
		list := []map[string]interface{}{}
		for v := 1; v <= sk.versions; v++ {
			if !sk.deleted[v] {
				list = append(list, map[string]interface{}{"name": name, "version": v, "created_by": fakeCaller})
			}
		}
		f.writeJSON(w, map[string]interface{}{"skill_versions": list})
	case "DELETE /api/3.0/mlflow/skills/{skill}/versions/{v}":
		v, _ := strconv.Atoi(params["v"])
		if sk == nil || v < 1 || v > sk.versions || sk.deleted[v] {
			notFound()
			return
		}
		if sk.deleted == nil {
			sk.deleted = map[int]bool{}
		}
		sk.deleted[v] = true
		f.writeJSON(w, map[string]string{})
	case "DELETE /api/3.0/mlflow/skills/{skill}":
		if sk == nil {
			notFound()
			return
		}
		delete(f.skills, name)
		f.writeJSON(w, map[string]string{})
	case "POST /api/3.0/mlflow/skills/{skill}/versions":
		if str("name") != name || str("source") == "" || str("source_type") != "git" {
			f.mlflowError(w, 400, "INVALID_PARAMETER_VALUE", "bad registration")
			return
		}
		if sk == nil {
			sk = &fakeSkill{tags: map[string]string{}, createdBy: fakeCaller}
			f.skills[name] = sk
		}
		sk.versions++
		sk.source = body
		f.writeJSON(w, map[string]interface{}{"name": name, "version": sk.versions, "created_by": fakeCaller})
	case "POST /api/3.0/mlflow/skills/{skill}/tags":
		if sk == nil {
			notFound()
			return
		}
		sk.tags[str("key")] = str("value")
		f.writeJSON(w, map[string]string{})
	default:
		f.t.Errorf("fake MLflow: no handler for %s", rule.key())
		f.mlflowError(w, 500, "INTERNAL_ERROR", "no handler")
	}
}

// samplesEnv is a fake cluster with a ready MLflow and project fakeProject,
// and the fake MLflow (and, for pipeline tests, KFP) behind one TLS server.
type samplesEnv struct {
	k8s    *resourceFake
	c      *Client
	mlflow *mlflowFake
	srv    *httptest.Server
	mux    *http.ServeMux
}

func newSamplesEnv(t *testing.T) *samplesEnv {
	t.Helper()
	k8s, c := newResourceFake(t)
	env := &samplesEnv{k8s: k8s, c: c, mlflow: newMLflowFake(t), mux: http.NewServeMux()}
	// MLflow decides every access review with the template's rules.
	env.mlflow.allow = templateAllow(t)
	env.mux.HandleFunc("/mlflow/", env.mlflow.serve)
	env.srv = httptest.NewTLSServer(env.mux)
	t.Cleanup(env.srv.Close)
	orig := samplesHTTPClient
	samplesHTTPClient = env.srv.Client()
	t.Cleanup(func() { samplesHTTPClient = orig })
	t.Cleanup(func() { mlflowSamplesCache.Remove(fakeProject) })
	mlflowSamplesCache.Remove(fakeProject)
	k8s.putJSON("/api/v1/namespaces/"+fakeProject, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	putMLflow(k8s, toolLabelJSON, toolFieldManager, "Update", "", `{}`,
		`{"url":"`+env.srv.URL+`/mlflow","conditions":[{"type":"Available","status":"True"}]}`)
	return env
}

func (env *samplesEnv) status(t *testing.T) map[string]typesSample {
	t.Helper()
	mlflowSamplesCache.Remove(fakeProject)
	st, err := GetMLflowSamplesStatus(env.c, fakeProject)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available {
		t.Fatalf("not available: %s", st.Reason)
	}
	out := map[string]typesSample{}
	for _, s := range st.Samples {
		out[s.ID] = typesSample{State: s.State, Missing: s.Missing, Actions: s.Actions, Message: s.Message, Link: s.Link}
	}
	return out
}

// typesSample is the part of types.SampleStatus the tests compare.
type typesSample struct {
	State   string
	Missing []string
	Actions []string
	Message string
	Link    string
}

// run runs a samples operation and returns its result and steps.
func (env *samplesEnv) run(t *testing.T, action string, samples ...string) (bool, string, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	resp, err := RunMLflowSamples(env.c, mlflowRequest(action, samples...), func(e UpdateStepEvent) { steps = append(steps, e) })
	if err != nil {
		t.Fatal(err)
	}
	return resp.Success, resp.Message + " | " + resp.ErrorCode, steps
}

func stepMessages(steps []UpdateStepEvent) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "[%s %s] %s\n", s.Step, s.Status, s.Message)
	}
	return b.String()
}

func mlflowRequest(action string, samples ...string) types.MLflowSamplesRequest {
	return types.MLflowSamplesRequest{Project: fakeProject, Action: action, Samples: samples}
}
