package cluster

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestMLflowSamplesStatus_Unavailable(t *testing.T) {
	for _, tc := range []struct {
		name, status, want string
		deploy             bool
	}{
		{"not deployed", "", "not deployed", false},
		{"not ready", `{"url":"https://x/mlflow","conditions":[{"type":"Available","status":"False"}]}`, "not ready", true},
		{"no url", `{"conditions":[{"type":"Available","status":"True"}]}`, "no https address", true},
		{"http url", `{"url":"http://x/mlflow","conditions":[{"type":"Available","status":"True"}]}`, "no https address", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			f.putJSON("/api/v1/namespaces/"+fakeProject, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
			mlflowSamplesCache.Remove(fakeProject)
			if tc.deploy {
				putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, tc.status)
			}
			st, err := GetMLflowSamplesStatus(c, fakeProject)
			if err != nil {
				t.Fatal(err)
			}
			if st.Available || !strings.Contains(st.Reason, tc.want) || len(st.Samples) != 0 {
				t.Errorf("got %+v", st)
			}
			mlflowSamplesCache.Remove(fakeProject)
		})
	}
}

func TestMLflowSamplesStatus_AllAbsentAndHeaders(t *testing.T) {
	env := newSamplesEnv(t)
	got := env.status(t)
	for _, id := range mlflowSampleIDs {
		if got[id].State != "absent" || !reflect.DeepEqual(got[id].Actions, []string{"add"}) {
			t.Errorf("%s = %+v", id, got[id])
		}
	}
	// Only named reads: get-by-name (demo, training), the model, the MCP
	// server and the skill.
	for _, r := range env.mlflow.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("status made a write: %s", r)
		}
	}
}

// The status reads MLflow with the ServiceAccount's token, so it serves
// only Data Science projects, like the samples operations.
func TestMLflowSamplesStatus_OnlyDSProjects(t *testing.T) {
	env := newSamplesEnv(t)
	env.k8s.putJSON("/api/v1/namespaces/openshift-config", `{"metadata":{},"status":{"phase":"Active"}}`)
	mlflowSamplesCache.Remove("openshift-config")
	st, err := GetMLflowSamplesStatus(env.c, "openshift-config")
	if !errors.Is(err, ErrNotDSProject) || st != nil {
		t.Errorf("got %+v, %v; want ErrNotDSProject", st, err)
	}
	if len(env.mlflow.requests) != 0 {
		t.Errorf("MLflow was called: %v", env.mlflow.requests)
	}
}

func TestMLflowSamplesStatus_IsCached(t *testing.T) {
	env := newSamplesEnv(t)
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	n := len(env.mlflow.requests)
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	if len(env.mlflow.requests) != n {
		t.Errorf("second status read MLflow again: %v", env.mlflow.requests[n:])
	}
	InvalidateSamplesStatus(fakeProject)
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	if len(env.mlflow.requests) == n {
		t.Error("InvalidateSamplesStatus did not drop the cache")
	}
}

// A samples operation that ends while a status read is under way drops the
// cache; the read must not put its older answer back for the TTL.
func TestMLflowSamplesStatus_ReadDuringInvalidationIsNotCached(t *testing.T) {
	env := newSamplesEnv(t)
	var once sync.Once
	env.mlflow.onRequest = func(string, string) { once.Do(func() { InvalidateSamplesStatus(fakeProject) }) }
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	env.mlflow.mu.Lock()
	n := len(env.mlflow.requests)
	env.mlflow.mu.Unlock()
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	if len(env.mlflow.requests) == n {
		t.Error("the read that overlapped an invalidation was cached")
	}
	n = len(env.mlflow.requests)
	if _, err := GetMLflowSamplesStatus(env.c, fakeProject); err != nil {
		t.Fatal(err)
	}
	if len(env.mlflow.requests) != n {
		t.Error("a read without an invalidation was not cached")
	}
}

func TestMLflowSamplesStatus_SkillsAPIMissing(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.noSkills = true
	if s := env.status(t)[SampleSkill]; s.State != "unsupported" || len(s.Actions) != 0 {
		t.Errorf("skill = %+v", s)
	}
}

func TestMLflowSamplesStatus_Foreign(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.experiments["7"] = &fakeExperiment{id: "7", name: trainingExperiment, stage: "active", tags: map[string]string{}}
	env.mlflow.mcp[mcpSampleName] = &fakeMCP{tags: map[string]string{}, versions: map[string]string{"1.0.0": "active"}, endpoints: 1}
	env.mlflow.skills[skillSampleName] = &fakeSkill{tags: map[string]string{sampleMarkerKey: "other"}, versions: 1}
	got := env.status(t)
	for _, id := range []string{SampleTraining, SampleMCPServer, SampleSkill} {
		if got[id].State != "foreign" || len(got[id].Actions) != 0 || !strings.Contains(got[id].Message, "not created by this tool") {
			t.Errorf("%s = %+v", id, got[id])
		}
	}
}

func TestMLflowSamplesStatus_Demo(t *testing.T) {
	env := newSamplesEnv(t)
	demo := &fakeExperiment{id: "1", name: demoExperiment, stage: "active", tags: map[string]string{
		demoVersionTagPrefix + "prompts": "1", demoVersionTagPrefix + "evaluation": "0", demoVersionTagPrefix + "issues": "0",
	}}
	env.mlflow.experiments["1"] = demo
	s := env.status(t)[SampleGenAIDemo]
	if s.State != "partial" || !reflect.DeepEqual(s.Missing, []string{"evaluation", "issues"}) || !reflect.DeepEqual(s.Actions, []string{"regenerate"}) {
		t.Errorf("reset features: %+v", s)
	}
	if s.Link != env.srv.URL+"/mlflow/#/experiments/1?workspace=proj" {
		t.Errorf("link = %q", s.Link)
	}
	demo.tags[demoVersionTagPrefix+"evaluation"], demo.tags[demoVersionTagPrefix+"issues"] = "2", "4"
	if s := env.status(t)[SampleGenAIDemo]; s.State != "present" || !strings.Contains(s.Message, "delete it in the MLflow UI") {
		t.Errorf("complete: %+v", s)
	}
	demo.tags = map[string]string{}
	if s := env.status(t)[SampleGenAIDemo]; s.State != "partial" {
		t.Errorf("no tags: %+v", s)
	}
	demo.stage = "deleted"
	if s := env.status(t)[SampleGenAIDemo]; s.State != "removed" || !reflect.DeepEqual(s.Actions, []string{"add"}) {
		t.Errorf("deleted: %+v", s)
	}
}

func TestMLflowSamplesStatus_PermissionDenied(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.allow = func(string, string, string, string) bool { return false }
	for id, s := range env.status(t) {
		if s.State != "unknown" || !strings.Contains(s.Message, "403 PERMISSION_DENIED") || !strings.Contains(s.Message, "re-apply the template") || len(s.Actions) != 0 {
			t.Errorf("%s = %+v", id, s)
		}
	}
}

func TestRunMLflowSamples_Refusals(t *testing.T) {
	env := newSamplesEnv(t)
	for _, tc := range []struct {
		action  string
		samples []string
		want    string
	}{
		{"delete", []string{SampleTraining}, "action must be"},
		{"add", nil, "samples is required"},
		{"add", []string{"nope"}, "unknown sample"},
		{"add", []string{SampleSkill, SampleSkill}, "listed twice"},
		{"regenerate", []string{SampleTraining}, "only genai-demo"},
		{"remove", []string{SampleGenAIDemo}, "cannot be removed by the updater"},
	} {
		ok, msg, steps := env.run(t, tc.action, tc.samples...)
		if ok || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "validation") {
			t.Errorf("%s %v: %s\n%s", tc.action, tc.samples, msg, stepMessages(steps))
		}
	}
	if len(env.mlflow.requests) != 0 {
		t.Errorf("a refused request reached MLflow: %v", env.mlflow.requests)
	}

	// Not a Data Science project, and MLflow not ready: nothing is sent.
	resp, _ := RunMLflowSamples(env.c, types.MLflowSamplesRequest{Project: "other", Action: "add", Samples: []string{SampleTraining}}, nil)
	if resp.Success || resp.ErrorCode != "validation" || !strings.Contains(resp.Message, "not a Data Science project") {
		t.Errorf("other project: %+v", resp)
	}
	putMLflow(env.k8s, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{"url":"https://x/mlflow","conditions":[{"type":"Available","status":"False"}]}`)
	resp, _ = RunMLflowSamples(env.c, mlflowRequest("add", SampleTraining), nil)
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "not ready") {
		t.Errorf("not ready: %+v", resp)
	}
	if len(env.mlflow.requests) != 0 {
		t.Errorf("requests: %v", env.mlflow.requests)
	}
}

func TestSamplesTLSConfig(t *testing.T) {
	read := func(files map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if cfg := samplesTLSConfig(env(map[string]string{"DEV_INSECURE_TLS": "true"}), read(nil)); cfg.InsecureSkipVerify {
		t.Error("DEV_INSECURE_TLS without DEV_MODE skipped verification")
	}
	if cfg := samplesTLSConfig(env(map[string]string{"DEV_MODE": "true", "DEV_INSECURE_TLS": "true"}), read(nil)); !cfg.InsecureSkipVerify {
		t.Error("DEV_MODE with DEV_INSECURE_TLS must skip verification, as for the API server")
	}
	if cfg := samplesTLSConfig(env(nil), read(nil)); cfg.RootCAs == nil || cfg.MinVersion == 0 {
		t.Error("no root pool")
	}
}

func TestParseSampleAPIError(t *testing.T) {
	for _, tc := range []struct {
		body       string
		code, msg  string
		missing    bool
		notFound   bool
		alreadyErr bool
	}{
		{`{"error_code":"RESOURCE_DOES_NOT_EXIST","message":"no"}`, "RESOURCE_DOES_NOT_EXIST", "no", false, true, false},
		{`{"error_code":"ENDPOINT_NOT_FOUND","message":"Endpoint not found."}`, "ENDPOINT_NOT_FOUND", "Endpoint not found.", true, false, false},
		{`{"detail":"Not Found"}`, "", "Not Found", true, false, false},
		// Neither: a 404 without MLflow's code or FastAPI's exact body says
		// nothing about the route or the object (the gateway's empty 404
		// while MLflow restarts).
		{`{"error":"x","code":5,"message":"run not found"}`, "", "run not found", false, false, false},
		{"Forbidden (user=system:serviceaccount:a:b, verb=get)", "", "Forbidden (user=system:serviceaccount:a:b, verb=get)", false, false, false},
		{"", "", "", false, false, false},
		{`{"detail":"Item missing"}`, "", "Item missing", false, false, false},
		{`{"error_code":"INVALID_PARAMETER_VALUE","message":"Not Found"}`, "INVALID_PARAMETER_VALUE", "Not Found", false, false, false},
	} {
		e := parseSampleAPIError("MLflow", 404, []byte(tc.body))
		if e.Code != tc.code || e.Message != tc.msg || isEndpointMissing(e) != tc.missing || isNotFound(e) != tc.notFound {
			t.Errorf("%s: %+v missing=%v notFound=%v", tc.body, e, isEndpointMissing(e), isNotFound(e))
		}
	}
	long := parseSampleAPIError("MLflow", 500, []byte(strings.Repeat("x", 1000)))
	if len(long.Message) > maxSampleErrorMessage+3 {
		t.Errorf("message not shortened: %d", len(long.Message))
	}
	if !isAlreadyExists(parseSampleAPIError("KFP", 409, nil)) || !isDenied(parseSampleAPIError("KFP", 403, nil)) {
		t.Error("409/403 not classified")
	}
	if sampleErrorCode(errors.New("dial")) != "network" || sampleErrorCode(parseSampleAPIError("MLflow", 403, nil)) != "forbidden" {
		t.Error("error codes")
	}
}
