package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A project's model actions (serving models_project.go): what a project
// admin or editor runs with their own token, the cluster facts read with
// the ServiceAccount's and reduced to the project.

const (
	testAppsDeployment = "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/llmisvc-controller-manager"
	testSnapshotPath   = "/configmaps/" + record.SnapshotConfigMapName
)

// projectModelUser is the signed-in user's client of a project action in
// "proj": their token, the ServiceAccount's bookkeeping and facts.
func projectModelUser(mc *modelsCluster) *Client {
	u := userClientOf(mc.c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-proj")
	return u
}

func runProjectModelForTest(t *testing.T, c *Client, req types.ProjectModelRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunProjectModel(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunProjectModel: %v", err)
	}
	return res, events
}

// The cluster facts a project user's token may not read: the user is
// refused them, so a setup that works reads them with the facts client.
func refuseFactsToTheUser(mc *modelsCluster) {
	for _, p := range []string{
		testAppsDeployment, testKServeConfig,
		"/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters",
		"/apis/gateway.networking.k8s.io/v1/gateways",
		"/api/v1/nodes", "/api/v1/pods",
		"/apis/serving.kserve.io/v1alpha2/namespaces/redhat-ods-applications/llminferenceserviceconfigs/" + testPresetName,
	} {
		mc.f.failAs[fakeUserToken+" GET "+p] = http.StatusForbidden
	}
}

// metaOf is an object's metadata as the fake holds it.
func metaOf(obj map[string]any) kube.ObjectMeta {
	var m kube.ObjectMeta
	raw, _ := json.Marshal(obj["metadata"])
	_ = json.Unmarshal(raw, &m)
	return m
}

// writes are the changes a run sent outside this tool's own records.
func modelWritesOf(mc *modelsCluster) []string {
	var out []string
	for _, m := range mc.f.mutations() {
		if !strings.Contains(m, testSnapshotPath) {
			out = append(out, m)
		}
	}
	return out
}

// An editor deploys a model reached only inside the cluster without token
// authentication: the model is written with their token, its token
// authentication off, no Role, RoleBinding, ServiceAccount or Secret is
// made, nothing outside the project is changed, and the record entry is a
// user's, with the project's namespace UID.
func TestEditorModelWithoutTokenAllowed(t *testing.T) {
	mc := newModelsCluster(t)
	refuseFactsToTheUser(mc)
	u := projectModelUser(mc)
	res, events := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	if got := finalSteps(events); got != "check=success model=success model-ready=success callable=success" {
		t.Fatalf("steps: %s", got)
	}
	model := mc.f.get(testModelPath)
	if ann := model["metadata"].(map[string]any)["annotations"].(map[string]any); ann[serving.EnableAuthAnnot] != "false" {
		t.Errorf("token authentication must be off: %v", ann)
	}
	if model["spec"].(map[string]any)["router"] != nil {
		t.Errorf("a model inside the cluster has no router: %v", model["spec"])
	}
	for _, w := range modelWritesOf(mc) {
		if !strings.Contains(w, "/namespaces/proj/llminferenceservices") {
			t.Errorf("setup wrote %s", w)
		}
	}
	for _, call := range mc.f.calls {
		// This tool's own records (its namespace) are the ServiceAccount's.
		if call.Method != http.MethodGet && !strings.HasPrefix(call.Path, "/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps") && call.Token != fakeUserToken {
			t.Errorf("%s %s with another token than the user's", call.Method, call.Path)
		}
	}
	rec, err := serving.ReadModelsRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if e := rec[serving.ModelRecordKey("proj", "qwen25-05b-instruct")]; e.ActorRole != record.ActorRoleUser || e.ProjectUID != "uid-proj" || e.By != "alice" || e.UID == "" {
		t.Errorf("record entry: %+v", e)
	}
}

// The capacity fact is fits or may not fit: no node, pod or project name
// reaches the user.
func TestCapacityFactHasNoNames(t *testing.T) {
	mc := newModelsCluster(t)
	refuseFactsToTheUser(mc)
	u := projectModelUser(mc)
	mc.f.putJSON("/api/v1/nodes/worker-secret-node", `{"metadata":{"name":"worker-secret-node","labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"8","memory":"32Gi"}}}`)
	mc.f.putJSON("/api/v1/namespaces/other-tenant/pods/hog", `{"metadata":{"name":"hog","namespace":"other-tenant"},"spec":{"nodeName":"worker-secret-node","containers":[{"resources":{"requests":{"cpu":"7","memory":"30Gi"}}}]},"status":{"phase":"Running"}}`)
	entry, _ := serving.CatalogEntry("qwen25-05b-instruct")
	fact := serving.ModelCapacityFact(u, entry, "proj")
	if fact != serving.CapacityMayNotFit {
		t.Fatalf("fact %q", fact)
	}
	words := serving.CapacityFactWords(entry, fact)
	ex, _, _ := ProjectModelExposures(u, "proj", "qwen25-05b-instruct")
	answer, _ := json.Marshal(ex)
	for _, name := range []string{"worker-secret-node", "other-tenant", "hog"} {
		if strings.Contains(words, name) || strings.Contains(string(answer), name) {
			t.Errorf("%s reaches the user: %q %s", name, words, answer)
		}
	}
	if ex.Capacity != serving.CapacityMayNotFit {
		t.Errorf("capacity %q", ex.Capacity)
	}
}

// A user's model is reached only inside the cluster without token
// authentication: asking for the inference gateway or for token
// authentication is refused before anything is read or written, naming
// who does it; the user's exposures answer offers only the inside of the
// cluster.
func TestUserCannotReachTheGatewayOrMakeTokens(t *testing.T) {
	mc := newModelsCluster(t)
	u := projectModelUser(mc)
	for _, req := range []types.ProjectModelRequest{
		{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true},
		{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", Exposure: "default-gateway"},
		{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", Exposure: "gateway"},
	} {
		before := len(mc.f.calls)
		res, _ := runProjectModelForTest(t, u, req)
		if res.Success || res.ErrorCode != "validation" || !strings.Contains(res.Message, serving.ClusterAdminModelWords) {
			t.Fatalf("%+v: %+v", req, res)
		}
		for _, call := range mc.f.calls[before:] {
			if !strings.HasPrefix(call.Path, "/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps") {
				t.Errorf("a refused request sent %s %s", call.Method, call.Path)
			}
		}
	}
	ex, msg, _ := ProjectModelExposures(u, "proj", "qwen25-05b-instruct")
	if msg != "" || len(ex.Options) != 1 || ex.Options[0].Key != serving.ExposureInternal || ex.Default != serving.ExposureInternal {
		t.Fatalf("exposures: %q %+v", msg, ex)
	}
	if answer, _ := json.Marshal(ex); strings.Contains(string(answer), "gateway\"") {
		t.Errorf("the answer offers a gateway: %s", answer)
	}
}

// MaaS stays a cluster-admin's: a project model action cannot ask for it,
// and a model published to MaaS is neither set up again nor deleted by
// one; nothing of MaaS is written.
func TestUserCannotPublishToMaaS(t *testing.T) {
	if p := ValidateProjectModelRequest(types.ProjectModelRequest{Action: "setup", Project: "proj", Exposure: "maas"}); p == "" {
		t.Fatal("exposure maas is accepted")
	}
	mc := newModelsCluster(t)
	u := projectModelUser(mc)
	if res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	model := mc.f.get(testModelPath)
	model["spec"].(map[string]any)["router"] = map[string]any{"route": map[string]any{}, "gateway": map[string]any{"refs": []any{map[string]any{"name": serving.MaaSGatewayName, "namespace": "openshift-ingress"}}}}
	mc.f.put(testModelPath, model)
	for _, req := range []types.ProjectModelRequest{{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"}, {Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"}} {
		res, _ := runProjectModelForTest(t, u, req)
		if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "A cluster-admin") && !strings.Contains(res.Message, "a cluster-admin") {
			t.Errorf("%s of a published model: %+v", req.Action, res)
		}
	}
	for _, w := range mc.f.mutations() {
		if strings.Contains(w, "maas") {
			t.Errorf("MaaS was written: %s", w)
		}
	}
	if !mc.f.has(testModelPath) {
		t.Error("the published model was deleted")
	}
}

// A user's setup does not take over a model a cluster-admin set up with
// token authentication (it would turn it off): refused, the model left.
func TestEditorSetupAgainKeepsTokenAuth(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: "internal"}); !res.Success {
		t.Fatalf("a cluster-admin's setup: %+v", res)
	}
	before := mc.f.get(testModelPath)
	res, _ := runProjectModelForTest(t, projectModelUser(mc), types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || !strings.Contains(res.Message, "has token authentication") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("%+v", res)
	}
	if after := mc.f.get(testModelPath); after["metadata"].(map[string]any)["resourceVersion"] != before["metadata"].(map[string]any)["resourceVersion"] {
		t.Error("the model changed")
	}
}

// Rollback per write: a model whose record cannot be written is deleted
// again; a refusal by the records' caps writes nothing.
func TestProjectModelRollbackPerWrite(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		mc := newModelsCluster(t)
		u := projectModelUser(mc)
		mc.f.fail["PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+testSnapshotPath] = http.StatusInternalServerError
		mc.f.failMessage["PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+testSnapshotPath] = "serviceaccount words"
		res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if res.Success || !strings.Contains(res.Message, "deleted again") || strings.Contains(res.Message, "serviceaccount words") {
			t.Fatalf("%+v", res)
		}
		if mc.f.has(testModelPath) {
			t.Error("the unrecorded model is left")
		}
	})
	t.Run("records full", func(t *testing.T) {
		mc := newModelsCluster(t)
		u := projectModelUser(mc)
		full := serving.ModelsRecord{}
		for i := range record.MaxUserRecordProjectsPerUser {
			p := "alice-" + string(rune('a'+i))
			mc.f.putJSON("/api/v1/namespaces/"+p, `{"metadata":{"name":"`+p+`","uid":"uid-`+p+`"}}`)
			full[serving.ModelRecordKey(p, "qwen3-1-7b")] = serving.RecordedModel{UID: "x", By: "alice", ActorRole: record.ActorRoleUser, ProjectUID: "uid-" + p}
		}
		putModelRecord(t, mc.f, full)
		res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if res.Success || res.ErrorCode != record.ErrorCodeRecordsFull || len(modelWritesOf(mc)) != 0 {
			t.Fatalf("%+v %v", res, modelWritesOf(mc))
		}
	})
}

// Cancel at the safe points: before the model's write nothing is left; at
// a poll of the ready wait the model stays, recorded as the tool's.
func TestProjectModelSetupCancelAtEverySafePoint(t *testing.T) {
	cancel := &record.ActionCancelledError{}
	t.Run("before the write", func(t *testing.T) {
		mc := newModelsCluster(t)
		u := projectModelUser(mc)
		u = u.WithContext(record.WithCancelCheck(u.Context(), func() *record.ActionCancelledError { return cancel }))
		res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if res.Success || res.ErrorCode != serving.CancelledCode || !strings.Contains(res.Message, "Nothing was changed") || len(modelWritesOf(mc)) != 0 {
			t.Fatalf("%+v %v", res, modelWritesOf(mc))
		}
	})
	t.Run("at a poll", func(t *testing.T) {
		mc := newModelsCluster(t)
		mc.readyAfter = 0
		u := projectModelUser(mc)
		asked := false
		u = u.WithContext(record.WithCancelCheck(u.Context(), func() *record.ActionCancelledError {
			if mc.f.has(testModelPath) {
				asked = true
				return cancel
			}
			return nil
		}))
		res, events := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if !asked || res.Success || res.ErrorCode != serving.CancelledCode || finalSteps(events) != "check=success model=success model-ready=failed" {
			t.Fatalf("%+v %s", res, finalSteps(events))
		}
		rec, _ := serving.ReadModelsRecord(mc.c)
		if model := mc.f.get(testModelPath); model == nil || !rec.Owns(metaOf(model)) {
			t.Error("the model is not left recorded as the tool's")
		}
	})
}

// Delete: the tool's model only (its recorded UID), with the user's
// token, its entry forgotten; someone else's model of the name is left.
func TestProjectModelTeardown(t *testing.T) {
	mc := newModelsCluster(t)
	u := projectModelUser(mc)
	if res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	before := len(mc.f.calls)
	res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success || mc.f.has(testModelPath) {
		t.Fatalf("%+v", res)
	}
	// Every change outside this tool's own records is the user's.
	for _, call := range mc.f.calls[before:] {
		if call.Method != http.MethodGet && !strings.HasPrefix(call.Path, "/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps") && call.Token != fakeUserToken {
			t.Errorf("teardown sent %s %s with another token than the user's", call.Method, call.Path)
		}
	}
	for _, m := range mc.f.mutations() {
		if strings.HasPrefix(m, "DELETE ") && (strings.Contains(m, "/roles/") || strings.Contains(m, "/rolebindings/")) {
			t.Errorf("a user's delete deleted a token object itself: %s", m)
		}
	}
	rec, _ := serving.ReadModelsRecord(mc.c)
	if _, ok := rec[serving.ModelRecordKey("proj", "qwen25-05b-instruct")]; ok {
		t.Error("the entry stays")
	}
	mc.f.putJSON(testModelPath, `{"metadata":{"name":"qwen25-05b-instruct","namespace":"proj","uid":"someone-elses"}}`)
	res, _ = runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || res.ErrorCode != "not_managed" || !mc.f.has(testModelPath) {
		t.Fatalf("someone else's model: %+v", res)
	}
}

// A cluster-admin's Models tab is unchanged: its setup still adds the
// project to this tool's gateway's list (a write the project actions
// never make).
func TestAdminModelsFlowUnchanged(t *testing.T) {
	mc := newModelsCluster(t)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	gw := mustJSON(mc.f.get(testGatewayPath))
	if !strings.Contains(gw, `"proj"`) {
		t.Errorf("the admin's setup did not list the project on its gateway: %s", gw)
	}
	rec, _ := serving.ReadModelsRecord(mc.c)
	if e := rec[serving.ModelRecordKey("proj", "qwen25-05b-instruct")]; e.ActorRole != record.ActorRoleAdmin || e.ProjectUID != "" {
		t.Errorf("an admin's entry: %+v", e)
	}
}

// A MaaSModelRef someone made in the project without this tool (no MaaS
// record of it) that targets the model refuses its delete, as does a
// list of them that cannot be read; the answer names no reference. One
// that targets another model does not.
func TestProjectModelDeleteRefusedForAForeignMaaSRef(t *testing.T) {
	const refs = "/apis/maas.opendatahub.io/v1alpha1/namespaces/proj/maasmodelrefs"
	for _, tc := range []struct {
		name, code string
		set        func(mc *modelsCluster)
	}{
		{"targets the model", "in_use", func(mc *modelsCluster) {
			mc.f.putJSON(refs+"/someones-secret-ref", `{"metadata":{"name":"someones-secret-ref","namespace":"proj"},"spec":{"modelRef":{"kind":"LLMInferenceService","name":"qwen25-05b-instruct"}}}`)
		}},
		{"unreadable", "prerequisites_unknown", func(mc *modelsCluster) {
			mc.f.fail["GET "+refs] = http.StatusInternalServerError
		}},
		{"targets another model", "", func(mc *modelsCluster) {
			mc.f.putJSON(refs+"/other", `{"metadata":{"name":"other","namespace":"proj"},"spec":{"modelRef":{"kind":"LLMInferenceService","name":"qwen3-1-7b"}}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			u := projectModelUser(mc)
			if res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			tc.set(mc)
			res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
			if tc.code == "" {
				if !res.Success || mc.f.has(testModelPath) {
					t.Fatalf("%+v", res)
				}
				return
			}
			if res.Success || res.ErrorCode != tc.code || !mc.f.has(testModelPath) || strings.Contains(res.Message, "someones-secret-ref") {
				t.Fatalf("%+v", res)
			}
		})
	}
}

// eventsText is every step message and the result of a run.
func eventsText(res *types.OperationResponse, events []UpdateStepEvent) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Message + "\n")
	}
	return b.String() + res.Message
}

// What the ServiceAccount reads for a project user's setup never reaches
// their stream as its own text: a CPU preset's Ready=False condition, a
// DataScienceCluster read's error. A cluster-admin still gets the detail.
func TestUserStreamCarriesNoServiceAccountText(t *testing.T) {
	const secret = "SECRET-CONDITION-TEXT"
	t.Run("preset condition", func(t *testing.T) {
		mc := newModelsCluster(t)
		mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/redhat-ods-applications/llminferenceserviceconfigs/"+testPresetName, `{
			"spec":{"template":{"containers":[{"name":"main","image":"`+testPresetImage+`"}]}},
			"status":{"conditions":[{"type":"Ready","status":"False","message":"`+secret+`"}]}}`)
		res, events := runProjectModelForTest(t, projectModelUser(mc), types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if text := eventsText(res, events); res.Success || strings.Contains(text, secret) || !strings.Contains(res.Message, "KServe's CPU preset is not ready") {
			t.Fatalf("user: %s", text)
		}
		res, events = runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: "internal"})
		if !strings.Contains(eventsText(res, events), secret) {
			t.Errorf("a cluster-admin lost the detail: %+v", res)
		}
	})
	t.Run("kueue mode read", func(t *testing.T) {
		mc := newModelsCluster(t)
		mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
		u := projectModelUser(mc)
		// The setup's own DSC read answers; the Kueue mode's fails.
		reads := 0
		mc.f.beforeServe = func(method, p string) {
			if method == http.MethodGet && strings.HasPrefix(p, "/apis/datasciencecluster.opendatahub.io/") {
				reads++
				if reads > 1 {
					mc.f.fail["GET "+p] = http.StatusInternalServerError
					mc.f.failMessage["GET "+p] = secret
				}
			}
		}
		res, events := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
		if text := eventsText(res, events); res.Success || strings.Contains(text, secret) || !strings.Contains(text, "HTTP 500") {
			t.Fatalf("user: %s", text)
		}
	})
}

// The capacity verdicts of every project and model, at once, come from one
// read of the cluster's nodes and pods (a shared snapshot, concurrent
// refreshes coalesced); each answer is a verdict only.
func TestCapacityFactsShareOneSnapshot(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON("/api/v1/nodes/worker-1", `{"metadata":{"name":"worker-1","labels":{"kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"16","memory":"64Gi"}}}`)
	u := projectModelUser(mc)
	var wg sync.WaitGroup
	answers := make(chan string, 2*len(serving.ModelCatalog))
	for _, project := range []string{"proj", "other"} {
		for _, entry := range serving.ModelCatalog {
			wg.Go(func() { answers <- serving.ModelCapacityFact(u, entry, project) })
		}
	}
	wg.Wait()
	close(answers)
	for a := range answers {
		if a != serving.CapacityFits {
			t.Errorf("verdict %q", a)
		}
	}
	nodes, pods := 0, 0
	for _, call := range mc.f.calls {
		switch {
		case call.Method == http.MethodGet && call.Path == "/api/v1/nodes":
			nodes++
		case call.Method == http.MethodGet && call.Path == "/api/v1/pods":
			pods++
		}
	}
	if nodes != 1 || pods != 1 {
		t.Errorf("reads of the nodes %d and pods %d, want one each", nodes, pods)
	}
}

// Rollback per write of a user's setup: whichever of its writes fails (the
// model, its record), nothing is left as nobody's: the model is either
// gone or recorded as the tool's.
func TestProjectModelRollbackEveryWrite(t *testing.T) {
	for _, tc := range []struct{ name, fail string }{
		{"model", "POST /apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices"},
		{"record", "PATCH /api/v1/namespaces/" + record.GetActivityNamespace() + testSnapshotPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			mc.f.fail[tc.fail] = http.StatusForbidden
			res, _ := runProjectModelForTest(t, projectModelUser(mc), types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
			if res.Success {
				t.Fatalf("a failed write succeeded: %+v", res)
			}
			rec, _ := serving.ReadModelsRecord(mc.c)
			if model := mc.f.get(testModelPath); model != nil && !rec.Owns(metaOf(model)) {
				t.Fatal("the model is left unrecorded")
			}
		})
	}
}
