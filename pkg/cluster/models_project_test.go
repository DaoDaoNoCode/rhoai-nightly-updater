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

// putAdmittingGateway is the cluster's inference gateway listing the
// projects named (RHOAI's documented names list).
func putAdmittingGateway(mc *modelsCluster, projects ...string) {
	values, _ := json.Marshal(projects)
	mc.f.putJSON(testGatewayPath, `{"metadata":{"name":"openshift-ai-inference","namespace":"openshift-ingress"},"spec":{"gatewayClassName":"openshift-ai-inference","listeners":[{"name":"https","hostname":"inference.apps.example.com","allowedRoutes":{"namespaces":{"from":"Selector","selector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":`+string(values)+`}]}}}}]}}`)
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

// With token authentication, a project admin's model gets the dashboard's
// four objects, made with their token.
func TestProjectAdminModelWithToken(t *testing.T) {
	mc := newModelsCluster(t)
	refuseFactsToTheUser(mc)
	u := projectModelUser(mc)
	res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true})
	if !res.Success {
		t.Fatalf("setup failed: %+v", res)
	}
	for _, p := range []string{
		"/api/v1/namespaces/proj/serviceaccounts/qwen25-05b-instruct-sa",
		"/apis/rbac.authorization.k8s.io/v1/namespaces/proj/roles/qwen25-05b-instruct-view-role",
		"/apis/rbac.authorization.k8s.io/v1/namespaces/proj/rolebindings/qwen25-05b-instruct-view",
	} {
		if !mc.f.has(p) {
			t.Errorf("%s was not made", p)
		}
	}
	rec, _ := serving.ReadModelsRecord(mc.c)
	if rec[serving.ModelRecordKey("proj", "qwen25-05b-instruct")].TokenSecretUID == "" {
		t.Error("the token Secret is not recorded")
	}
}

// The gateway fact is a yes or a no with a fixed code for the requested
// project, and neither it nor the user's exposures answer names another
// project the gateway lists.
func TestGatewayFactIsYesNoOnly(t *testing.T) {
	mc := newModelsCluster(t)
	refuseFactsToTheUser(mc)
	u := projectModelUser(mc)
	putAdmittingGateway(mc, "openshift-ingress", "other-team-secret", "proj")
	labels := map[string]string{"opendatahub.io/dashboard": "true"}
	fact, host := serving.ProjectGatewayFact(u, "proj", labels)
	if !fact.Admits || fact.Problem != "" || host != "inference.apps.example.com" {
		t.Fatalf("fact %+v host %q", fact, host)
	}
	raw, _ := json.Marshal(fact)
	if string(raw) != `{"admits":true}` {
		t.Errorf("the fact carries more than yes or no: %s", raw)
	}
	ex, msg, _ := ProjectModelExposures(u, "proj", "qwen25-05b-instruct")
	if msg != "" {
		t.Fatal(msg)
	}
	answer, _ := json.Marshal(ex)
	if strings.Contains(string(answer), "other-team-secret") {
		t.Errorf("the answer names another project: %s", answer)
	}
	if ex.Default != serving.ExposureDefault || ex.Gateway == nil || !ex.Gateway.Admits {
		t.Errorf("exposures: %+v", ex)
	}

	putAdmittingGateway(mc, "openshift-ingress", "other-team-secret")
	fact, host = serving.ProjectGatewayFact(u, "proj", labels)
	raw, _ = json.Marshal(fact)
	if fact.Admits || host != "" || string(raw) != `{"admits":false,"problem":"not-admitted"}` {
		t.Fatalf("not admitted: %s %q", raw, host)
	}
	words := serving.GatewayFactWords("proj", fact)
	if strings.Contains(words, "other-team-secret") || !strings.Contains(words, "A cluster-admin can allow project proj on the inference gateway") {
		t.Errorf("words: %q", words)
	}
	ex, _, _ = ProjectModelExposures(u, "proj", "qwen25-05b-instruct")
	if answer, _ := json.Marshal(ex); strings.Contains(string(answer), "other-team-secret") || ex.Default != serving.ExposureInternal {
		t.Errorf("not admitted, answer: %s", answer)
	}

	// A read that fails is a no (fail closed), without the server's text.
	mc.f.fail["GET /apis/gateway.networking.k8s.io/v1/gateways"] = http.StatusInternalServerError
	mc.f.failMessage["GET /apis/gateway.networking.k8s.io/v1/gateways"] = "secret server words"
	fact, _ = serving.ProjectGatewayFact(u, "proj", labels)
	if fact.Admits || fact.Problem != serving.GatewayUnreadable || strings.Contains(serving.GatewayFactWords("proj", fact), "secret server words") {
		t.Errorf("unreadable: %+v", fact)
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

// A user never adds their project to a gateway's list: a setup through a
// gateway that does not admit the project is refused before any write,
// saying that a cluster-admin can allow it.
func TestUserCannotAddProjectToGateway(t *testing.T) {
	mc := newModelsCluster(t)
	u := projectModelUser(mc)
	putAdmittingGateway(mc, "openshift-ingress")
	res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", Exposure: "default-gateway", TokenAuth: true})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "A cluster-admin can allow project proj on the inference gateway") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("%+v", res)
	}
	if w := modelWritesOf(mc); len(w) != 0 {
		t.Errorf("a refused setup wrote %v", w)
	}
	// Through a gateway that admits it, the model routes there and the
	// gateway is not changed.
	putAdmittingGateway(mc, "openshift-ingress", "proj")
	before := mc.f.get(testGatewayPath)
	res, _ = runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", Exposure: "default-gateway", TokenAuth: true})
	if !res.Success {
		t.Fatalf("through an admitting gateway: %+v", res)
	}
	for _, w := range modelWritesOf(mc) {
		if strings.Contains(w, "gateways") || strings.Contains(w, "openshift-ingress") {
			t.Errorf("the gateway was written: %s", w)
		}
	}
	if after := mc.f.get(testGatewayPath); after["metadata"].(map[string]any)["resourceVersion"] != before["metadata"].(map[string]any)["resourceVersion"] {
		t.Error("the gateway changed")
	}
	// Without token authentication a gateway is refused by validation.
	if p := ValidateProjectModelRequest(types.ProjectModelRequest{Action: "setup", Project: "proj", Exposure: "default-gateway"}); !strings.Contains(p, "needs token authentication") {
		t.Errorf("validation: %q", p)
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

// A setup without token authentication does not take over a model that
// has it (it would turn it off): refused with the admin-role wording.
func TestEditorSetupAgainKeepsTokenAuth(t *testing.T) {
	mc := newModelsCluster(t)
	u := projectModelUser(mc)
	if res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || !strings.Contains(res.Message, "needs the project's admin role") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("%+v", res)
	}
}

// Rollback per write: a model whose record cannot be written is deleted
// again; a token object that fails leaves the model recorded (its token
// objects are owned by it); nothing is left as nobody's.
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
	t.Run("token", func(t *testing.T) {
		mc := newModelsCluster(t)
		u := projectModelUser(mc)
		mc.f.fail["POST /apis/rbac.authorization.k8s.io/v1/namespaces/proj/rolebindings"] = http.StatusForbidden
		res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true})
		if res.Success {
			t.Fatalf("%+v", res)
		}
		rec, _ := serving.ReadModelsRecord(mc.c)
		model := mc.f.get(testModelPath)
		if model == nil || !rec.Owns(metaOf(model)) {
			t.Fatal("the model is not recorded after a token failure")
		}
		role := mc.f.get("/apis/rbac.authorization.k8s.io/v1/namespaces/proj/roles/qwen25-05b-instruct-view-role")
		if role == nil || !strings.Contains(mustJSON(role["metadata"]), metaOf(model).UID) {
			t.Error("the Role made before the failure is not owned by the recorded model")
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
	if res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success || mc.f.has(testModelPath) {
		t.Fatalf("%+v", res)
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

// Token authentication asked for inside the cluster needs Connectivity
// Link: when it is not set up, or that cannot be read, the setup is
// refused before anything is written; it never turns the authentication
// off unnoticed.
func TestTokenAuthNeedsConnectivityLink(t *testing.T) {
	for _, tc := range []struct {
		name, want, code string
		breakIt          func(mc *modelsCluster)
	}{
		{"not set up", "which is not set up on this cluster", "prerequisites", func(mc *modelsCluster) {
			delete(mc.f.groups, "/apis/kuadrant.io")
			delete(mc.f.objects, fakeKey{gv: "kuadrant.io/v1beta1", ns: "kuadrant-system", plural: "kuadrants", name: "kuadrant"})
		}},
		{"unreadable", "cannot be checked now (HTTP 500)", "prerequisites_unknown", func(mc *modelsCluster) {
			delete(mc.f.groups, "/apis/kuadrant.io")
			mc.f.fail["GET /apis/kuadrant.io"] = http.StatusInternalServerError
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			tc.breakIt(mc)
			u := projectModelUser(mc)
			res, _ := runProjectModelForTest(t, u, types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", TokenAuth: true})
			if res.Success || res.ErrorCode != tc.code || !strings.Contains(res.Message, tc.want) || !strings.Contains(res.Message, "Deploy without token authentication") {
				t.Fatalf("%+v", res)
			}
			if w := modelWritesOf(mc); len(w) != 0 {
				t.Errorf("a refused setup wrote %v", w)
			}
		})
	}
}

// Through the gateway, a setup succeeds only once the gateway refused the
// same call without a token. When it answers it (no token checks): a model
// this setup created is deleted again and forgotten; a model set up
// before, deployed again, is reported and left.
func TestGatewayPolicyFailureRollsBackANewModel(t *testing.T) {
	setup := types.ProjectModelRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct", Exposure: "default-gateway", TokenAuth: true}
	t.Run("first deploy", func(t *testing.T) {
		mc := newModelsCluster(t)
		putAdmittingGateway(mc, "openshift-ingress", "proj")
		mc.calls.open = true
		res, _ := runProjectModelForTest(t, projectModelUser(mc), setup)
		if res.Success || res.ErrorCode != "not_authenticated" || !strings.Contains(res.Message, "deleted again") {
			t.Fatalf("%+v", res)
		}
		if mc.f.has(testModelPath) {
			t.Error("the unchecked model is left")
		}
		rec, _ := serving.ReadModelsRecord(mc.c)
		if _, ok := rec[serving.ModelRecordKey("proj", "qwen25-05b-instruct")]; ok {
			t.Error("its entry is left")
		}
	})
	t.Run("redeploy", func(t *testing.T) {
		mc := newModelsCluster(t)
		putAdmittingGateway(mc, "openshift-ingress", "proj")
		u := projectModelUser(mc)
		if res, _ := runProjectModelForTest(t, u, setup); !res.Success {
			t.Fatalf("first: %+v", res)
		}
		mc.calls.open = true
		res, _ := runProjectModelForTest(t, u, setup)
		if res.Success || res.ErrorCode != "not_authenticated" || !strings.Contains(res.Message, "left as it is") {
			t.Fatalf("%+v", res)
		}
		if !mc.f.has(testModelPath) {
			t.Error("a model set up before was deleted")
		}
	})
	t.Run("redeploy after the gateway stopped admitting the project", func(t *testing.T) {
		mc := newModelsCluster(t)
		putAdmittingGateway(mc, "openshift-ingress", "proj")
		u := projectModelUser(mc)
		if res, _ := runProjectModelForTest(t, u, setup); !res.Success {
			t.Fatalf("first: %+v", res)
		}
		putAdmittingGateway(mc, "openshift-ingress")
		res, _ := runProjectModelForTest(t, u, setup)
		if res.Success || !strings.Contains(res.Message, "does not admit project proj") {
			t.Fatalf("%+v", res)
		}
	})
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
