package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tTokenSA      = "/api/v1/namespaces/proj/serviceaccounts/qwen25-05b-instruct-sa"
	tTokenRole    = "/apis/rbac.authorization.k8s.io/v1/namespaces/proj/roles/qwen25-05b-instruct-view-role"
	tTokenBinding = "/apis/rbac.authorization.k8s.io/v1/namespaces/proj/rolebindings/qwen25-05b-instruct-view"
	tTokenSecret  = "/api/v1/namespaces/proj/secrets/default-name-qwen25-05b-instruct-sa"
)

func jsonOf(v interface{}) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Setup makes the dashboard's token authentication for the model, as its
// "Require token authentication" does (odh-dashboard rhoai-3.6 auth.ts,
// secrets.ts): the four objects, owned by the model, with its labels and
// annotations; enable-auth stays absent.
func TestModelsSetup_MakesTheDashboardsTokenAuthentication(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	modelUID := mc.f.get(testModelPath)["metadata"].(map[string]interface{})["uid"].(string)
	owner := `"ownerReferences":[{"apiVersion":"serving.kserve.io/v1alpha2","blockOwnerDeletion":false,"kind":"LLMInferenceService","name":"qwen25-05b-instruct","uid":"` + modelUID + `"}]`
	for path, want := range map[string][]string{
		tTokenSA:   {`"kind":"ServiceAccount"`, owner},
		tTokenRole: {`"kind":"Role"`, owner, `"opendatahub.io/dashboard":"true"`, `"rules":[{"apiGroups":["serving.kserve.io"],"resourceNames":["qwen25-05b-instruct"],"resources":["llminferenceservices"],"verbs":["get"]}]`},
		tTokenBinding: {`"kind":"RoleBinding"`, owner, `"opendatahub.io/dashboard":"true"`,
			`"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"qwen25-05b-instruct-view-role"}`,
			`"subjects":[{"kind":"ServiceAccount","name":"qwen25-05b-instruct-sa"}]`},
		tTokenSecret: {`"type":"kubernetes.io/service-account-token"`, owner, `"opendatahub.io/dashboard":"true"`,
			`"kubernetes.io/service-account.name":"qwen25-05b-instruct-sa"`, `"openshift.io/display-name":"default-name"`},
	} {
		obj := jsonOf(mc.f.get(path))
		for _, w := range want {
			if !strings.Contains(obj, w) {
				t.Errorf("%s lacks %s: %s", path, w, obj)
			}
		}
	}
	if strings.Contains(jsonOf(mc.f.get(tTokenSA)), "opendatahub.io/dashboard") {
		t.Error("the dashboard's ServiceAccount has no dashboard label")
	}
	if ann := jsonOf(mc.f.get(testModelPath)["metadata"]); strings.Contains(ann, enableAuthAnnot) {
		t.Errorf("enable-auth set: %s", ann)
	}
	secretUID := mc.f.get(tTokenSecret)["metadata"].(map[string]interface{})["uid"].(string)
	if rec := mustReadModelsRecord(t, mc); rec["proj/qwen25-05b-instruct"].TokenSecretUID != secretUID {
		t.Errorf("secret UID not recorded: %+v", rec)
	}
	// The status names the token for the Models tab.
	st, _ := GetResourcesStatus(mc.c)
	if len(st.Models.Items) != 1 || st.Models.Items[0].TokenSecret != "default-name-qwen25-05b-instruct-sa" || st.Models.Items[0].TokenServiceAccount != "qwen25-05b-instruct-sa" {
		t.Errorf("status %+v", st.Models.Items)
	}
	// The setup checks the Secret with a dry-run delete carrying its UID;
	// only "Can it be called?" reads it, by its name, for the model's own
	// token (models_call.go). No other Secret is read.
	mc.f.requests = nil
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	for _, r := range mc.f.requests {
		if strings.HasPrefix(r, "GET ") && strings.Contains(r, "/secrets/") && r != "GET /api/v1/namespaces/proj/secrets/default-name-qwen25-05b-instruct-sa" {
			t.Errorf("a Secret was read: %s", r)
		}
		if strings.HasPrefix(r, "POST ") && strings.Contains(r, "/proj/") && !strings.Contains(r, "llminferenceservices") {
			t.Errorf("setup again made a token object again: %s", r)
		}
	}
	if !strings.Contains(strings.Join(mc.f.requests, "\n"), "DRYRUN DELETE "+tTokenSecret) {
		t.Error("the recorded Secret was not checked")
	}
}

// A model set up before the token objects existed gets them on its next
// setup.
func TestModelsSetup_AddsTheTokenToAnEarlierModel(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, p := range []string{tTokenSA, tTokenRole, tTokenBinding, tTokenSecret} {
		deleteFakeObject(mc, p)
	}
	rec := mustReadModelsRecord(t, mc)
	e := rec["proj/qwen25-05b-instruct"]
	e.TokenSecretUID = ""
	rec["proj/qwen25-05b-instruct"] = e
	if err := writeModelsRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	for _, p := range []string{tTokenSA, tTokenRole, tTokenBinding, tTokenSecret} {
		if !mc.f.has(p) {
			t.Errorf("%s not made", p)
		}
	}
}

// An object of a token name that is not the model's (someone else's
// ServiceAccount, or a Secret the tool did not record) stops the setup
// before anything is written.
func TestModelsSetup_RefusesAForeignTokenObject(t *testing.T) {
	for _, path := range []string{tTokenSA, tTokenRole, tTokenBinding, tTokenSecret} {
		mc := newModelsCluster(t)
		mc.f.putJSON(path, `{"metadata":{}}`)
		res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
		if res.Success || res.ErrorCode != "not_managed" {
			t.Errorf("%s: %+v", path, res)
		}
		if mc.f.has(testModelPath) || mc.f.has(testGatewayPath) {
			t.Errorf("%s: something was created", path)
		}
	}
}

// Teardown deletes the model's own token objects; someone else's object of
// a token name (not owned by the model) is left.
func TestModelsTeardown_LeavesTokenObjectsThatAreNotTheModels(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	deleteFakeObject(mc, tTokenRole)
	mc.f.putJSON(tTokenRole, `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
	if res, _ := TeardownModels(mc.c, "proj"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if !mc.f.has(tTokenRole) {
		t.Error("someone else's Role was deleted")
	}
	for _, p := range []string{tTokenSA, tTokenBinding, tTokenSecret} {
		if mc.f.has(p) {
			t.Errorf("%s left", p)
		}
	}
}
