package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A gateway whose token checks are not known (labelled
// opendatahub.io/managed=false, or outside openshift-ingress where this tool
// cannot read AuthPolicies) is offered but never recommended.
func TestModelExposures_NeverRecommendsAGatewayWithUnknownTokenChecks(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway: %+v", res)
	}
	gw := mc.f.get(testGatewayPath)
	ensureMap(ensureMap(gw, "metadata"), "labels")[odhManagedLabel] = "false"
	ex := exposures(t, mc, "")
	if a := optionByKey(t, ex, exposureDefault); !a.Available || !strings.Contains(a.Warning, "managed=false") || ex.Default != "" {
		t.Fatalf("default %s, option %+v", ex.Default, a)
	}
	// A setup that names no way is refused rather than routed there.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); res.Success || mc.f.has(testModelPath) {
		t.Fatalf("setup without a choice: %+v", res)
	}
	// Chosen explicitly, it is used.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureDefault}); !res.Success {
		t.Fatalf("explicit setup: %+v", res)
	}
}

// A gateway that answers without a token fails "Can it be called?" as
// unauthenticated (the setup fails, the model stays); an internal-only
// model is never probed (it is called without a token anyway).
func TestModelsSetup_AGatewayThatAnswersWithoutATokenFailsTheCheck(t *testing.T) {
	mc := newModelsCluster(t)
	mc.calls.open = true
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || res.ErrorCode != "not_callable" || !strings.Contains(res.Message, "answers without a token") || !strings.Contains(finalSteps(events), "callable=failed") {
		t.Fatalf("setup: %+v %s", res, finalSteps(events))
	}
	if !mc.f.has(testModelPath) {
		t.Fatal("the model must stay set up")
	}
	st, _ := GetResourcesStatus(mc.c)
	if c := st.Models.Items[0].CallCheck; c == nil || c.OK || c.Result != "unauthenticated" {
		t.Fatalf("status check: %+v", c)
	}
	// The model's own token never goes on the probe.
	for _, s := range mc.calls.sent() {
		if !strings.Contains(s, testModelToken) {
			t.Errorf("a recorded request without the token: %s", s)
		}
	}

	mc2 := newModelsCluster(t)
	if res, _ := runModels(t, mc2.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureInternal}); !res.Success || mc2.calls.probes != 0 {
		t.Fatalf("internal: %+v probes %d", res, mc2.calls.probes)
	}
}

// The ready wait can end inside a young node's pull grace window: the
// setup then fails with the pull failure and what to check, not "still
// starting".
func TestModelsSetup_ReadyWaitEndingDuringAFailingPullKeepsTheAdvice(t *testing.T) {
	defer func(d time.Duration) { ModelReadyTimeout = d }(ModelReadyTimeout)
	ModelReadyTimeout = ModelImagePullGraceYoungNode / 2
	mc := newModelsCluster(t)
	mc.readyAfter = 0
	mc.f.putJSON("/api/v1/nodes/node-a", `{"metadata":{"creationTimestamp":"`+clk.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339)+`"}}`)
	mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"nodeName":"node-a"},
		"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ImagePullBackOff","message":"unauthorized: authentication required"}}}]}}`)
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "image pull is still failing") || !strings.Contains(res.Message, "pull secret") || !strings.Contains(res.Message, "joined the cluster") {
		t.Fatalf("got %+v", res)
	}
}

// The status names the projects this tool added to the gateway's list (a
// teardown of their last model there takes them off), and the
// prerequisites answer says which way to reach a model it was planned for.
func TestModels_StatusNamesAddedProjectsAndPlannedExposure(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	setListeners(t, mc, namesList("openshift-ingress", "proj", "by-hand"))
	st, _ := GetResourcesStatus(mc.c)
	if got := strings.Join(st.InferenceGateway.AddedProjects, ","); got != "proj" {
		t.Errorf("added projects: %q", got)
	}
	for _, e := range []string{"", exposureDefault, exposureInternal} {
		p, err := GetPrerequisitesStatusFor(mc.c, e)
		if err != nil || p.ModelsExposure != e {
			t.Errorf("planned for %q: %+v %v", e, p, err)
		}
	}
}

// KServe's default gateway is used only when its configuration names it:
// unreadable or unset, the default route is not offered (and no project is
// added to the tool's gateway's list for it); the tool's gateway can still
// be chosen by name.
func TestModelExposures_UnknownKServeDefaultIsNotAssumed(t *testing.T) {
	for name, breakIt := range map[string]func(mc *modelsCluster){
		"unreadable": func(mc *modelsCluster) { mc.f.fail["GET "+testKServeConfig] = 500 },
		"unset": func(mc *modelsCluster) {
			k, _ := parseFakePath(testKServeConfig)
			delete(mc.f.objects, k)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mc := newModelsCluster(t)
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
				t.Fatalf("gateway: %+v", res)
			}
			breakIt(mc)
			ex := exposures(t, mc, "")
			if a := optionByKey(t, ex, exposureDefault); a.Available || ex.Default == exposureDefault {
				t.Fatalf("default route offered: %+v", a)
			}
			byName := optionByKey(t, ex, "gateway:openshift-ingress/openshift-ai-inference")
			if !byName.Available || len(byName.Changes) != 1 {
				t.Fatalf("by name: %+v", byName)
			}
			before := len(mc.f.mutations())
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureDefault}); res.Success || len(mc.f.mutations()[before:]) != 0 {
				t.Fatalf("default route: %+v %v", res, mc.f.mutations()[before:])
			}
			wantAllowed(t, mc, "openshift-ingress")
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureGateway, GatewayRef: "openshift-ingress/openshift-ai-inference"}); !res.Success {
				t.Fatalf("by name: %+v", res)
			}
			wantAllowed(t, mc, "openshift-ingress", "proj")
		})
	}
}
