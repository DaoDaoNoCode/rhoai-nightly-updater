package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A model set up on the Models tab to be reached through MaaS only is
// published as part of its setup even when its server is still starting
// when the setup's wait ends (live, 2026-11-25: a 10m21s cold start). Its
// route needs the MaaS gateway to admit the project, which only a
// publication does, so without it the model could never become Ready. The
// MaaS docs order: deploy the model, admit its namespace on the gateway,
// add the MaaSModelRef; the model does not have to be Ready first.

// startingMaaSModel is newMaaSCluster with the platform and the tool's
// inference gateway set up, and a model whose workload never rolls out and
// that never reports Ready.
func startingMaaSModel(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("inference gateway setup: %+v", res)
	}
	mc.readyAfter = 0
	putModelDeployment(mc, 1, 0, 0)
	return mc
}

// saidIn is every message of step, joined.
func saidIn(events []UpdateStepEvent, step string) string {
	return strings.Join(eventMessages(events, step), "\n")
}

func TestMaaSOnlySetup_StillStartingIsPublished(t *testing.T) {
	mc := startingMaaSModel(t)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	if !res.Success {
		t.Fatalf("setup: %+v\n%s", res, finalSteps(events))
	}
	if got := finalSteps(events); !strings.Contains(got, "model-ready=success publish=success") {
		t.Errorf("steps: %s", got)
	}
	if !mc.f.has(tMaaSRef) || !gatewayAdmits(t, mc) {
		t.Fatalf("not published: ref %v, gateway admits proj %v", mc.f.has(tMaaSRef), gatewayAdmits(t, mc))
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Errorf("refs = %s", got)
	}
	if said := saidIn(events, "model-ready"); !strings.Contains(said, "still starting after 10:00") || !strings.Contains(said, "becomes Ready once its server runs") || strings.Contains(said, "It may still start") {
		t.Errorf("model-ready said: %s", said)
	}
	if !strings.Contains(res.Message, "published to MaaS and still starting") {
		t.Errorf("message: %s", res.Message)
	}
}

// A start that failed, or an image pull still failing when the wait ends,
// is not published: nothing is admitted on the MaaS gateway for a model
// that may never run.
func TestMaaSOnlySetup_FailedStartIsNotPublished(t *testing.T) {
	mc := startingMaaSModel(t)
	mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ErrImagePull","message":"reading manifest sha256:978a in registry.redhat.io/rhaii/vllm-cpu-rhel9: manifest unknown"}}}]}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	if res.Success || res.ErrorCode != "image_unavailable" {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); strings.Contains(got, "publish=") {
		t.Errorf("steps: %s", got)
	}
	if mc.f.has(tMaaSRef) || gatewayAdmits(t, mc) {
		t.Fatalf("published: ref %v, gateway admits proj %v", mc.f.has(tMaaSRef), gatewayAdmits(t, mc))
	}
}

func TestMaaSOnlySetup_FailingPullIsNotPublished(t *testing.T) {
	defer serving.SetModelReadyTimeoutForTest(serving.ModelImagePullGraceYoungNode / 2)()
	mc := startingMaaSModel(t)
	mc.f.putJSON("/api/v1/nodes/node-a", `{"metadata":{"creationTimestamp":"`+kube.Clock().Now().Add(-3*time.Minute).UTC().Format(time.RFC3339)+`"}}`)
	mc.f.putJSON(testModelPodsPath, `{"metadata":{"labels":{"app.kubernetes.io/name":"qwen25-05b-instruct","app.kubernetes.io/component":"llminferenceservice-workload"}},
		"spec":{"nodeName":"node-a"},
		"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ImagePullBackOff","message":"unauthorized: authentication required"}}}]}}`)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "image pull is still failing") ||
		!strings.Contains(res.Message, "not published to MaaS") || !strings.Contains(res.Message, "Set up again on the Models tab") {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); strings.Contains(got, "publish=") {
		t.Errorf("steps: %s", got)
	}
	if mc.f.has(tMaaSRef) || gatewayAdmits(t, mc) {
		t.Fatalf("published: ref %v, gateway admits proj %v", mc.f.has(tMaaSRef), gatewayAdmits(t, mc))
	}
}

// The operation's deadline comes during the wait for MaaS to accept the
// model: the wait ends before it, so the setup still ends as published and
// still being accepted, not "Stopped waiting".
func TestMaaSOnlySetup_DeadlineDuringTheAcceptWait(t *testing.T) {
	mc := startingMaaSModel(t)
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if k.plural == "maasmodelrefs" {
			obj["status"] = map[string]any{"phase": "Pending"}
		}
	}
	// The setup's own wait takes 10 minutes; 2 are left for the publish.
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	res, events := runModels(t, mc.c.WithContext(ctx), types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	if !res.Success {
		t.Fatalf("setup: %+v\n%s", res, finalSteps(events))
	}
	if said := saidIn(events, "publish"); !strings.Contains(said, "is published and still being accepted") || strings.Contains(said, "Stopped waiting") || strings.Contains(said, "after 5:00") {
		t.Errorf("publish said: %s", said)
	}
	if !mc.f.has(tMaaSRef) || !gatewayAdmits(t, mc) {
		t.Fatalf("not published: ref %v, gateway admits proj %v", mc.f.has(tMaaSRef), gatewayAdmits(t, mc))
	}
}

// The recipe's model step: setup publishes the model while its server
// still starts, and the step succeeds once the model serves (here, as on
// the cluster, only after the publication admits its project).
func TestRecipeModelStep_PublishedWhileStartingThenServes(t *testing.T) {
	mc := startingMaaSModel(t)
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if !mc.f.hasLocked(tMaaSRef) {
			return
		}
		switch {
		case k.plural == "llminferenceservices":
			obj["status"] = map[string]any{
				"url":                "https://maas.apps.example.com/proj/qwen25-05b-instruct",
				"observedGeneration": ensureMap(obj, "metadata")["generation"],
				"conditions":         []any{map[string]any{"type": "Ready", "status": "True"}},
			}
		case k.plural == "deployments" && k.name == "qwen25-05b-instruct-kserve":
			obj["status"] = map[string]any{"observedGeneration": 2, "replicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
		}
	}
	r, problems := recipe.Validate([]byte(`{"apiVersion":"rhoai-nightly-updater/v1alpha1","kind":"EnvironmentRecipe","metadata":{"name":"dev"},
"spec":{"rhoai":{"build":"latest"},"project":{"mode":"existing","name":"proj"},
"features":{"models":{"enabled":true,"model":"qwen25-05b-instruct","exposure":"maas"},"maas":{"enabled":true}}}}`))
	if len(problems) > 0 {
		t.Fatalf("recipe: %v", problems)
	}
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var events []UpdateStepEvent
	res, err := recipeStep(t, r, recipe.StepModel).Run(mc.c.WithContext(ctx), &recipe.State{Recipe: r}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res == nil || !res.Success {
		t.Fatalf("model step: %+v %v\n%s", res, err, finalSteps(events))
	}
	if !mc.f.has(tMaaSRef) || !gatewayAdmits(t, mc) {
		t.Fatalf("not published: ref %v, gateway admits proj %v", mc.f.has(tMaaSRef), gatewayAdmits(t, mc))
	}
}
