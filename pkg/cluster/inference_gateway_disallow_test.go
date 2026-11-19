package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const testKServeConfig = "/api/v1/namespaces/redhat-ods-applications/configmaps/inferenceservice-config"

// listedGateway is a models cluster with this tool's gateway listing
// openshift-ingress, e2e-v300 (a project that is gone), proj and team
// (team has someone else's model on the gateway).
func listedGateway(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	setListeners(t, mc, namesList("openshift-ingress", "e2e-v300", "proj", "team"))
	someoneElsesModel(mc)
	// The tool's first write after creating the Gateway moves its field
	// ownership to server-side apply (upgradeToolFieldOwnership, a
	// metadata write of its own): done here, so the tests count only the
	// list's writes.
	meta, _, err := readGatewayObject(mc.c, "Gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgradeToolFieldOwnership(mc.c, testGatewayPath, meta); err != nil {
		t.Fatal(err)
	}
	return mc
}

// onGatewayWrite runs once, as the first write of the Gateway (not its
// server-side dry run) arrives, before the API server handles it.
func onGatewayWrite(mc *modelsCluster, do func()) *bool {
	done := false
	mc.f.beforeRequest = func(r *http.Request) {
		if done || r.Method != http.MethodPatch || r.URL.Path != testGatewayPath || r.URL.Query().Get("dryRun") != "" ||
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/apply-patch") {
			return
		}
		done = true
		do()
	}
	return &done
}

// "Remove from the list" fails closed: every read it needs that fails, a
// dry run the API server rejects, and a model that appears while the
// write is planned again each refuse it on step "check" (or "gateway" for
// the re-plan) with nothing written.
func TestModelsDisallow_FailsClosed(t *testing.T) {
	cases := map[string]func(mc *modelsCluster){
		"models cannot be listed": func(mc *modelsCluster) {
			mc.f.fail["GET /apis/serving.kserve.io/v1alpha2/llminferenceservices"] = 500
		},
		"gateway cannot be read": func(mc *modelsCluster) { mc.f.fail["GET "+testGatewayPath] = 500 },
		"KServe's configuration cannot be read": func(mc *modelsCluster) {
			mc.f.fail["GET "+testKServeConfig] = 500
		},
		"dry run rejected": func(mc *modelsCluster) {
			mc.f.failDryRun = map[string]int{"PATCH " + testGatewayPath: 422}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			mc := listedGateway(t)
			breakIt(mc)
			before := len(mc.f.mutations())
			res, events := runModels(t, mc.c, types.ModelsRequest{Action: "disallow", Project: "proj"})
			if res.Success || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("disallow: %+v", res)
			}
			if got := finalSteps(events); got != "check=failed" {
				t.Errorf("steps: %s (every check runs on step check, before the write)", got)
			}
			if m := mc.f.mutations()[before:]; len(m) != 0 {
				t.Errorf("wrote %v", m)
			}
			delete(mc.f.fail, "GET "+testGatewayPath)
			wantAllowed(t, mc, "openshift-ingress", "e2e-v300", "proj", "team")
		})
	}

	t.Run("a model appears before the write is planned again", func(t *testing.T) {
		mc := listedGateway(t)
		edited := onGatewayWrite(mc, func() {
			// Someone edits the list (the write is refused by its
			// resourceVersion) and deploys a model in proj on the gateway.
			setListeners(t, mc, namesList("openshift-ingress", "e2e-v300", "proj", "team", "late"))
			mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/granite", `{"spec":{"router":{"route":{},"gateway":{}}}}`)
		})
		before := len(mc.f.mutations())
		res, events := runModels(t, mc.c, types.ModelsRequest{Action: "disallow", Project: "proj"})
		if !*edited {
			t.Fatal("the gateway was never written")
		}
		if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "proj/granite") {
			t.Fatalf("disallow: %+v", res)
		}
		if got := finalSteps(events); got != "check=success gateway=failed" {
			t.Errorf("steps: %s", got)
		}
		// Only the write the API server refused (409).
		if m := mc.f.mutations()[before:]; len(m) != 1 || m[0] != "PATCH "+testGatewayPath {
			t.Errorf("wrote %v", m)
		}
		wantAllowed(t, mc, "openshift-ingress", "e2e-v300", "proj", "team", "late")
	})
}

// A project that is not on the list: "not on the list", and nothing is
// written; the check step ends only after the checks.
func TestModelsDisallow_NotOnTheList(t *testing.T) {
	mc := listedGateway(t)
	before := len(mc.f.mutations())
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "disallow", Project: "other"})
	if !res.Success || res.Message != "other is not on the inference gateway's list; nothing was changed." {
		t.Fatalf("disallow: %+v", res)
	}
	if got := finalSteps(events); got != "check=success gateway=success" {
		t.Errorf("steps: %s", got)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
	checks := eventMessages(events, "check")
	if len(checks) < 2 || checks[len(checks)-1] != "Checked." {
		t.Errorf("check step: %v", checks)
	}
}

// Deleting a project's last model: when a model appears in the project
// while the gateway write is planned again, the project stays on the list.
func TestModelsTeardown_ReleaseChecksAgainAfterAConflict(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	edited := onGatewayWrite(mc, func() {
		setListeners(t, mc, namesList("openshift-ingress", "proj", "late"))
		mc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/granite", `{"spec":{"router":{"route":{},"gateway":{}}}}`)
	})
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj"})
	if !*edited {
		t.Fatal("the gateway was never written")
	}
	if !res.Success || strings.Contains(res.Message, "taken off") {
		t.Fatalf("teardown: %+v", res)
	}
	wantAllowed(t, mc, "late", "openshift-ingress", "proj")
	if rec, _ := readGatewayProjects(mc.c); strings.Join(rec, ",") != "proj" {
		t.Errorf("record: %v (kept while the project stays listed)", rec)
	}
}

// A project this tool added whose namespace was deleted since goes off the
// list at the next Models change that succeeds, with the same checks; a
// namespace that cannot be read is not taken for gone, and a refused
// setup changes nothing.
func TestModels_ReleasesDeletedRecordedProjects(t *testing.T) {
	setUp := func(t *testing.T) *modelsCluster {
		mc := newModelsCluster(t)
		if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		// The project is deleted with its model, behind the tool's back.
		delete(mc.f.objects, fakeKey{gv: "v1", plural: "namespaces", name: "proj"})
		delete(mc.f.objects, fakeKey{gv: "serving.kserve.io/v1alpha2", ns: "proj", plural: "llminferenceservices", name: "qwen25-05b-instruct"})
		mc.f.putJSON("/api/v1/namespaces/team", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"}}}`)
		return mc
	}

	t.Run("next setup", func(t *testing.T) {
		mc := setUp(t)
		res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "team"})
		if !res.Success || !strings.Contains(res.Message, "Project proj no longer exists, so it was taken off the projects the inference gateway allows.") {
			t.Fatalf("setup: %+v", res)
		}
		wantAllowed(t, mc, "openshift-ingress", "team")
		if rec, _ := readGatewayProjects(mc.c); strings.Join(rec, ",") != "team" {
			t.Errorf("record: %v", rec)
		}
	})

	t.Run("clean up's models part", func(t *testing.T) {
		mc := setUp(t)
		res, err := TeardownModels(mc.c, "team")
		if err != nil || !res.Success || !strings.Contains(res.Message, "Project proj no longer exists") {
			t.Fatalf("teardown: %+v %v", res, err)
		}
		wantAllowed(t, mc, "openshift-ingress")
	})

	t.Run("a namespace that cannot be read stays", func(t *testing.T) {
		mc := setUp(t)
		mc.f.fail["GET /api/v1/namespaces/proj"] = 500
		res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "team"})
		if !res.Success || strings.Contains(res.Message, "proj no longer exists") {
			t.Fatalf("setup: %+v", res)
		}
		wantAllowed(t, mc, "openshift-ingress", "proj", "team")
	})

	t.Run("a refused setup changes nothing", func(t *testing.T) {
		mc := setUp(t)
		before := len(mc.f.mutations())
		res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "nowhere"})
		if res.Success {
			t.Fatalf("setup: %+v", res)
		}
		if m := mc.f.mutations()[before:]; len(m) != 0 {
			t.Errorf("wrote %v", m)
		}
		wantAllowed(t, mc, "openshift-ingress", "proj")
	})
}
