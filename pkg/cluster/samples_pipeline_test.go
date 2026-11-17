package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// kfpFake is the KFP v2beta1 API behind a DSPA's kube-rbac-proxy. The
// proxy checks datasciencepipelinesapplications/api on the DSPA's name with
// the verb taken from the HTTP method (POST create, GET get, DELETE delete;
// opendatahub-io/kube-rbac-proxy pkg/proxy/proxy.go:73-89) and answers 403
// "Forbidden (user=..., verb=..., ...)" as text.
type kfpFake struct {
	t        *testing.T
	mu       sync.Mutex
	allow    func(resource, verb, namespace, name string) bool
	requests []string
	nextID   int

	experiments map[string]*kfpFakeExperiment
	pipelines   map[string]*kfpFakePipeline
	runs        map[string]*kfpFakeRun
}

type kfpFakeExperiment struct{ name, description string }
type kfpFakePipeline struct {
	name, description, spec string
	versions                []string
}
type kfpFakeRun struct {
	experiment, pipeline, version, state string
	order                                int
}

// kfpFakeRoutes are the KFP routes the sample calls, each with the verb
// kube-rbac-proxy derives from its method.
var kfpFakeRoutes = []struct{ method, pattern, verb string }{
	{"GET", `^/apis/v2beta1/experiments$`, "get"},
	{"POST", `^/apis/v2beta1/experiments$`, "create"},
	{"DELETE", `^/apis/v2beta1/experiments/[^/]+$`, "delete"},
	{"GET", `^/apis/v2beta1/pipelines$`, "get"},
	{"POST", `^/apis/v2beta1/pipelines/upload$`, "create"},
	{"GET", `^/apis/v2beta1/pipelines/[^/]+/versions$`, "get"},
	{"DELETE", `^/apis/v2beta1/pipelines/[^/]+/versions/[^/]+$`, "delete"},
	{"DELETE", `^/apis/v2beta1/pipelines/[^/]+$`, "delete"},
	{"GET", `^/apis/v2beta1/runs$`, "get"},
	{"POST", `^/apis/v2beta1/runs$`, "create"},
	{"DELETE", `^/apis/v2beta1/runs/[^/]+$`, "delete"},
}

func newKFPFake(t *testing.T) *kfpFake {
	return &kfpFake{t: t, experiments: map[string]*kfpFakeExperiment{}, pipelines: map[string]*kfpFakePipeline{}, runs: map[string]*kfpFakeRun{}}
}

func (k *kfpFake) id(prefix string) string {
	k.nextID++
	return fmt.Sprintf("%s-%d", prefix, k.nextID)
}

func (k *kfpFake) grpcError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": msg, "code": 5, "message": msg})
}

func (k *kfpFake) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func filterValue(t *testing.T, r *http.Request, key string) string {
	var f struct {
		Predicates []struct {
			Key, Operation, StringValue string
		} `json:"predicates"`
	}
	raw := r.URL.Query().Get("filter")
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, "string_value", "StringValue")), &f); err != nil || len(f.Predicates) != 1 || f.Predicates[0].Key != key || f.Predicates[0].Operation != "EQUALS" {
		t.Errorf("unexpected filter %q (want %s EQUALS)", raw, key)
		return ""
	}
	return f.Predicates[0].StringValue
}

func (k *kfpFake) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.requests = append(k.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	verb := ""
	for _, rt := range kfpFakeRoutes {
		if rt.method == r.Method && regexp.MustCompile(rt.pattern).MatchString(r.URL.Path) {
			verb = rt.verb
		}
	}
	if verb == "" {
		k.t.Errorf("fake KFP: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if k.allow != nil && !k.allow("datasciencepipelinesapplications/api", verb, fakeProject, dspaName) {
		http.Error(w, fmt.Sprintf("Forbidden (user=system:serviceaccount:x:y, verb=%s, resource=datasciencepipelinesapplications, subresource=api)", verb), http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/v2beta1/"), "/")
	var body map[string]interface{}
	if r.Header.Get("Content-Type") == "application/json" {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	}
	str := func(key string) string { s, _ := body[key].(string); return s }
	switch {
	case r.Method == "GET" && r.URL.Path == "/apis/v2beta1/experiments":
		want := filterValue(k.t, r, "display_name")
		out := []map[string]string{}
		for id, e := range k.experiments {
			if e.name == want {
				out = append(out, map[string]string{"experiment_id": id, "display_name": e.name, "description": e.description})
			}
		}
		k.writeJSON(w, map[string]interface{}{"experiments": out})
	case r.Method == "POST" && r.URL.Path == "/apis/v2beta1/experiments":
		id := k.id("exp")
		k.experiments[id] = &kfpFakeExperiment{name: str("display_name"), description: str("description")}
		k.writeJSON(w, map[string]string{"experiment_id": id, "display_name": str("display_name")})
	case r.Method == "DELETE" && parts[0] == "experiments":
		if k.experiments[parts[1]] == nil {
			k.grpcError(w, 404, "experiment not found")
			return
		}
		for _, run := range k.runs {
			if run.experiment == parts[1] && run.pipeline != "" && k.pipelines[run.pipeline] != nil {
				k.t.Errorf("experiment deleted while sample run of a live pipeline remains")
			}
		}
		delete(k.experiments, parts[1])
		k.writeJSON(w, map[string]string{})
	case r.Method == "GET" && r.URL.Path == "/apis/v2beta1/pipelines":
		want := filterValue(k.t, r, "name")
		out := []map[string]string{}
		for id, p := range k.pipelines {
			if p.name == want {
				out = append(out, map[string]string{"pipeline_id": id, "name": p.name, "display_name": p.name, "description": p.description})
			}
		}
		k.writeJSON(w, map[string]interface{}{"pipelines": out})
	case r.Method == "POST" && r.URL.Path == "/apis/v2beta1/pipelines/upload":
		file, header, err := r.FormFile("uploadfile")
		if err != nil || !strings.HasSuffix(header.Filename, ".yaml") {
			k.grpcError(w, 400, "Failed to read pipeline from file")
			return
		}
		data, _ := io.ReadAll(file)
		id := k.id("pipe")
		k.pipelines[id] = &kfpFakePipeline{name: r.URL.Query().Get("name"), description: r.URL.Query().Get("description"), spec: string(data), versions: []string{k.id("ver")}}
		k.writeJSON(w, map[string]string{"pipeline_id": id, "name": r.URL.Query().Get("name"), "description": r.URL.Query().Get("description")})
	case r.Method == "GET" && len(parts) == 3 && parts[2] == "versions":
		p := k.pipelines[parts[1]]
		if p == nil {
			k.grpcError(w, 404, "pipeline not found")
			return
		}
		out := []map[string]string{}
		for _, v := range p.versions {
			out = append(out, map[string]string{"pipeline_version_id": v})
		}
		k.writeJSON(w, map[string]interface{}{"pipeline_versions": out})
	case r.Method == "DELETE" && len(parts) == 4:
		p := k.pipelines[parts[1]]
		if p == nil {
			k.grpcError(w, 404, "pipeline not found")
			return
		}
		var keep []string
		for _, v := range p.versions {
			if v != parts[3] {
				keep = append(keep, v)
			}
		}
		p.versions = keep
		k.writeJSON(w, map[string]string{})
	case r.Method == "DELETE" && parts[0] == "pipelines":
		p := k.pipelines[parts[1]]
		if p == nil {
			k.grpcError(w, 404, "pipeline not found")
			return
		}
		if len(p.versions) > 0 { // DeletePipeline without cascade (resource_manager.go:341-380)
			k.grpcError(w, 400, "pipeline has versions")
			return
		}
		delete(k.pipelines, parts[1])
		k.writeJSON(w, map[string]string{})
	case r.Method == "POST" && r.URL.Path == "/apis/v2beta1/runs":
		ref, _ := body["pipeline_version_reference"].(map[string]interface{})
		p := k.pipelines[fmt.Sprint(ref["pipeline_id"])]
		if k.experiments[str("experiment_id")] == nil || p == nil || len(p.versions) == 0 || p.versions[0] != fmt.Sprint(ref["pipeline_version_id"]) {
			k.grpcError(w, 400, "bad run reference")
			return
		}
		id := k.id("run")
		k.runs[id] = &kfpFakeRun{experiment: str("experiment_id"), pipeline: fmt.Sprint(ref["pipeline_id"]), version: fmt.Sprint(ref["pipeline_version_id"]), state: "PENDING", order: k.nextID}
		k.writeJSON(w, map[string]string{"run_id": id, "state": "PENDING"})
	case r.Method == "GET" && r.URL.Path == "/apis/v2beta1/runs":
		exp := r.URL.Query().Get("experiment_id")
		if r.URL.Query().Get("sort_by") != "created_at desc" {
			k.t.Errorf("runs listed without newest first")
		}
		var ids []string
		for id, run := range k.runs {
			if run.experiment == exp {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return k.runs[ids[i]].order > k.runs[ids[j]].order })
		out := []map[string]interface{}{}
		for _, id := range ids {
			run := k.runs[id]
			out = append(out, map[string]interface{}{"run_id": id, "state": run.state, "created_at": "2026-11-17T10:00:00Z", "finished_at": "1970-01-01T00:00:00Z",
				"pipeline_version_reference": map[string]string{"pipeline_id": run.pipeline, "pipeline_version_id": run.version}})
		}
		k.writeJSON(w, map[string]interface{}{"runs": out, "total_size": len(out)})
	case r.Method == "DELETE" && parts[0] == "runs":
		if k.runs[parts[1]] == nil {
			k.grpcError(w, 404, "run not found")
			return
		}
		delete(k.runs, parts[1])
		k.writeJSON(w, map[string]string{})
	default:
		k.t.Errorf("fake KFP: no handler for %s %s", r.Method, r.URL.Path)
	}
}

func (k *kfpFake) deletes(from int) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, r := range k.requests[from:] {
		if strings.HasPrefix(r, "DELETE ") {
			out = append(out, strings.SplitN(strings.TrimPrefix(r, "DELETE /apis/v2beta1/"), "/", 2)[0]+strings.Repeat("/version", strings.Count(r, "/versions/")))
		}
	}
	return out
}

const dspaFakePath = "/apis/" + dspaAPIGroup + "/namespaces/" + fakeProject + "/datasciencepipelinesapplications/" + dspaName

// newPipelineEnv is newSamplesEnv with the fake KFP and a ready, tool-created
// nightly-dspa whose in-cluster address is the fake.
func newPipelineEnv(t *testing.T) (*samplesEnv, *kfpFake) {
	t.Helper()
	env := newSamplesEnv(t)
	k := newKFPFake(t)
	k.allow = templateAllow(t)
	env.mux.HandleFunc("/apis/v2beta1/", k.serve)
	orig := runsInCluster
	runsInCluster = func() bool { return true }
	t.Cleanup(func() { runsInCluster = orig; pipelineSampleCache.Remove(fakeProject) })
	pipelineSampleCache.Remove(fakeProject)
	putSampleDSPA(env, toolLabelJSON, "True", env.srv.URL, "https://unused.example")
	env.k8s.putJSON("/apis/route.openshift.io/v1/namespaces/redhat-ods-applications/routes/rhods-dashboard", `{"metadata":{},"spec":{"host":"dash.example"}}`)
	return env, k
}

func putSampleDSPA(env *samplesEnv, labels, ready, url, externalURL string) {
	env.k8s.putJSON(dspaFakePath, `{"metadata":{"labels":`+labels+`},"spec":{},"status":{"conditions":[{"type":"Ready","status":"`+ready+`"}],
		"components":{"apiServer":{"url":"`+url+`","externalUrl":"`+externalURL+`"}}}}`)
}

func pipelineStatus(t *testing.T, env *samplesEnv) *types.PipelineSampleStatus {
	t.Helper()
	pipelineSampleCache.Remove(fakeProject)
	st, err := GetPipelineSampleStatus(env.c, fakeProject)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func runPipeline(t *testing.T, env *samplesEnv, action string) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	resp, err := RunPipelineSample(env.c, types.PipelineSampleRequest{Project: fakeProject, Action: action}, func(e UpdateStepEvent) { steps = append(steps, e) })
	if err != nil {
		t.Fatal(err)
	}
	return resp, steps
}

func TestPipelineSample_Unavailable(t *testing.T) {
	env, _ := newPipelineEnv(t)
	for _, tc := range []struct {
		name, labels, ready, url, want string
	}{
		{"not created by the tool", `{}`, "True", env.srv.URL, "not created by this tool"},
		{"not ready", toolLabelJSON, "False", env.srv.URL, "not ready"},
		{"no https address", toolLabelJSON, "True", "http://ds-pipeline-nightly-dspa.proj.svc.cluster.local:8888", "no https API address"},
	} {
		putSampleDSPA(env, tc.labels, tc.ready, tc.url, "")
		st := pipelineStatus(t, env)
		if st.Available || !strings.Contains(st.Reason, tc.want) {
			t.Errorf("%s: %+v", tc.name, st)
		}
		resp, _ := runPipeline(t, env, "run")
		if resp.Success || resp.ErrorCode != "prerequisites" {
			t.Errorf("%s: run %+v", tc.name, resp)
		}
	}
	env.k8s.mu.Lock()
	delete(env.k8s.objects, fakeKey{gv: dspaAPIGroup, ns: fakeProject, plural: "datasciencepipelinesapplications", name: dspaName})
	env.k8s.mu.Unlock()
	if st := pipelineStatus(t, env); st.Available || !strings.Contains(st.Reason, "no pipeline server created by this tool") {
		t.Errorf("no DSPA: %+v", st)
	}
}

func TestPipelineSample_RunAndStatus(t *testing.T) {
	env, k := newPipelineEnv(t)
	if st := pipelineStatus(t, env); !st.Available || st.State != "absent" || st.Image != pipelineSampleDefaultImage {
		t.Fatalf("status %+v", st)
	}
	resp, steps := runPipeline(t, env, "run")
	if !resp.Success {
		t.Fatalf("%+v\n%s", resp, stepMessages(steps))
	}
	if len(k.experiments) != 1 || len(k.pipelines) != 1 || len(k.runs) != 1 {
		t.Fatalf("objects %d %d %d", len(k.experiments), len(k.pipelines), len(k.runs))
	}
	for _, p := range k.pipelines {
		for _, want := range []string{"image: " + pipelineSampleDefaultImage, "schemaVersion: 2.1.0", "{{$.outputs.artifacts['greeting'].path}}", "producerTask: write-artifact"} {
			if !strings.Contains(p.spec, want) {
				t.Errorf("spec lacks %q", want)
			}
		}
	}
	var runID string
	for id := range k.runs {
		runID = id
	}
	st := pipelineStatus(t, env)
	if st.State != "present" || st.Runs != 1 || st.LatestRun == nil || st.LatestRun.State != "PENDING" || st.LatestRun.FinishedAt != "" ||
		st.LatestRun.Link != "https://dash.example/develop-train/pipelines/runs/proj/runs/"+runID || st.RunsLink != "https://dash.example/develop-train/pipelines/runs/proj/runs" {
		t.Errorf("status %+v run %+v", st, st.LatestRun)
	}

	if resp, _ := runPipeline(t, env, "run"); !resp.Success || len(k.experiments) != 1 || len(k.pipelines) != 1 || len(k.runs) != 2 {
		t.Errorf("second run: %+v %d runs", resp, len(k.runs))
	}
	if st := pipelineStatus(t, env); st.Runs != 2 || st.LatestRun.ID == runID {
		t.Errorf("latest run is not the newest: %+v", st.LatestRun)
	}
}

func TestPipelineSample_ImageOverride(t *testing.T) {
	env, k := newPipelineEnv(t)
	t.Setenv("SAMPLE_PIPELINE_IMAGE", "mirror.example/ubi9/ubi-minimal@sha256:abc")
	if resp, _ := runPipeline(t, env, "run"); !resp.Success {
		t.Fatal(resp.Message)
	}
	for _, p := range k.pipelines {
		if strings.Count(p.spec, "image: mirror.example/ubi9/ubi-minimal@sha256:abc") != 2 {
			t.Errorf("spec %s", p.spec)
		}
	}
	if st := pipelineStatus(t, env); st.Image != "mirror.example/ubi9/ubi-minimal@sha256:abc" {
		t.Errorf("image %q", st.Image)
	}
}

func TestPipelineSample_RemoveInOrder(t *testing.T) {
	env, k := newPipelineEnv(t)
	for i := 0; i < 2; i++ {
		if resp, _ := runPipeline(t, env, "run"); !resp.Success {
			t.Fatal(resp.Message)
		}
	}
	n := len(k.requests)
	resp, steps := runPipeline(t, env, "remove")
	if !resp.Success {
		t.Fatalf("%+v\n%s", resp, stepMessages(steps))
	}
	if got := strings.Join(k.deletes(n), ","); got != "runs,runs,pipelines/version,pipelines,experiments" {
		t.Errorf("delete order %s", got)
	}
	if len(k.experiments)+len(k.pipelines)+len(k.runs) != 0 {
		t.Error("left objects behind")
	}
	if st := pipelineStatus(t, env); st.State != "absent" {
		t.Errorf("status %+v", st)
	}
	if resp, _ := runPipeline(t, env, "remove"); !resp.Success || !strings.Contains(resp.Message, "nothing to remove") {
		t.Errorf("second remove %+v", resp)
	}
}

func TestPipelineSample_KeepsWhatTheToolDidNotCreate(t *testing.T) {
	env, k := newPipelineEnv(t)
	if resp, _ := runPipeline(t, env, "run"); !resp.Success {
		t.Fatal(resp.Message)
	}
	var expID string
	for id := range k.experiments {
		expID = id
	}
	// A user's own run in the sample experiment, of another pipeline.
	k.pipelines["user-pipe"] = &kfpFakePipeline{name: "user", versions: []string{"uv"}}
	k.runs["user-run"] = &kfpFakeRun{experiment: expID, pipeline: "user-pipe", version: "uv", state: "SUCCEEDED"}
	resp, steps := runPipeline(t, env, "remove")
	if !resp.Success || !strings.Contains(resp.Message, "kept") || k.runs["user-run"] == nil || k.experiments[expID] == nil || k.pipelines["user-pipe"] == nil {
		t.Errorf("%+v\n%s", resp, stepMessages(steps))
	}

	// An experiment of the sample's name that someone else created.
	env2, k2 := newPipelineEnv(t)
	k2.experiments["theirs"] = &kfpFakeExperiment{name: pipelineSampleName, description: "mine"}
	if st := pipelineStatus(t, env2); st.State != "foreign" {
		t.Errorf("status %+v", st)
	}
	for _, action := range []string{"run", "remove"} {
		n := len(k2.requests)
		resp, _ := runPipeline(t, env2, action)
		if resp.Success || resp.ErrorCode != "not_managed" {
			t.Errorf("%s: %+v", action, resp)
		}
		for _, r := range k2.requests[n:] {
			if !strings.HasPrefix(r, "GET ") {
				t.Errorf("%s wrote %s", action, r)
			}
		}
	}
}

func TestPipelineSample_PermissionDenied(t *testing.T) {
	env, k := newPipelineEnv(t)
	k.allow = func(resource, verb, ns, name string) bool { return verb == "get" }
	resp, steps := runPipeline(t, env, "run")
	if resp.Success || resp.ErrorCode != "forbidden" || !strings.Contains(resp.Message, "verb=create") || !strings.Contains(resp.Message, "re-apply the template") {
		t.Errorf("%+v\n%s", resp, stepMessages(steps))
	}
	k.allow = func(string, string, string, string) bool { return false }
	if st := pipelineStatus(t, env); st.State != "unknown" || !strings.Contains(st.Message, "403") {
		t.Errorf("status %+v", st)
	}
}

// DEV_MODE runs outside the cluster: the Route address is used.
func TestPipelineSample_DevModeUsesTheRoute(t *testing.T) {
	env, _ := newPipelineEnv(t)
	runsInCluster = func() bool { return false }
	putSampleDSPA(env, toolLabelJSON, "True", "https://ds-pipeline-nightly-dspa.proj.svc.cluster.local:8443", env.srv.URL)
	if resp, steps := runPipeline(t, env, "run"); !resp.Success {
		t.Errorf("%+v\n%s", resp, stepMessages(steps))
	}
}
