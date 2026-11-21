package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func modelMetaOf(mc *modelsCluster) (map[string]any, map[string]any) {
	meta := mc.f.get(testModelPath)["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	return labels, ann
}

// Publishing does what the dashboard's MaaS checkbox does to the model
// (odh-dashboard rhoai-3.6 maasDeploymentTransformer.ts L27-40): the
// genai-asset label and genai-use-case annotation go, so Gen AI studio
// lists the model once, through MaaS. Setup keeps them off while it is
// published; Unpublish puts back what it recorded.
func TestMaaSPublish_TakesTheModelOutOfGenAIAssetsAndUnpublishRestores(t *testing.T) {
	mc := publishedMaaSCluster(t)
	labels, ann := modelMetaOf(mc)
	if _, ok := labels["opendatahub.io/genai-asset"]; ok {
		t.Errorf("genai-asset kept: %v", labels)
	}
	if _, ok := ann["opendatahub.io/genai-use-case"]; ok {
		t.Errorf("genai-use-case kept: %v", ann)
	}
	if labels["opendatahub.io/dashboard"] != "true" {
		t.Errorf("dashboard label lost: %v", labels)
	}
	rec, _ := serving.ReadMaaSRecord(mc.c)
	if g := rec.GenAI["proj/qwen25-05b-instruct"]; g.Asset != "true" || g.UseCase != "Chat" || !g.HadAsset || !g.HadUseCase {
		t.Errorf("record %+v", rec.GenAI)
	}
	// Setting it up again while it is published does not bring them back.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	if labels, _ := modelMetaOf(mc); labels["opendatahub.io/genai-asset"] != nil {
		t.Errorf("setup put genai-asset back while published: %v", labels)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	labels, ann = modelMetaOf(mc)
	if labels["opendatahub.io/genai-asset"] != "true" || ann["opendatahub.io/genai-use-case"] != "Chat" {
		t.Errorf("not restored: %v %v", labels, ann)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); len(rec.GenAI) != 0 {
		t.Errorf("record kept %+v", rec.GenAI)
	}
}

// The MaaSModelRef is owned by the model, as the dashboard makes it
// (odh-dashboard rhoai-3.6 maas BFF maasmodelrefs.go
// buildModelRefUnstructured: blockOwnerDeletion false; maas-model-ref.ts
// L161-231 passes the model's UID), so garbage collection
// removes it with the model.
func TestMaaSPublish_TheModelRefIsOwnedByTheModelAndCarriesItsDescription(t *testing.T) {
	mc := publishedMaaSCluster(t)
	uid := mc.f.get(testModelPath)["metadata"].(map[string]any)["uid"].(string)
	meta := mc.f.get(tMaaSRef)["metadata"].(map[string]any)
	want := `[{"apiVersion":"serving.kserve.io/v1alpha2","blockOwnerDeletion":false,"kind":"LLMInferenceService","name":"qwen25-05b-instruct","uid":"` + uid + `"}]`
	if got := jsonOf(meta["ownerReferences"]); got != want {
		t.Errorf("ownerReferences %s, want %s", got, want)
	}
	entry, _ := serving.CatalogEntry("")
	if got := meta["annotations"].(map[string]any)["openshift.io/description"]; got != entry.Description {
		t.Errorf("description %v", got)
	}
}

// Unpublish puts the model's route back on the inference gateway. When the
// gateway no longer lists the project (someone took it off while the model
// was published), Unpublish says that the direct address does not answer
// and how to allow the project; it does not change the gateway itself.
func TestMaaSUnpublish_InAProjectTheGatewayDoesNotListSaysSo(t *testing.T) {
	mc := publishedMaaSCluster(t)
	setListeners(t, mc, namesList("openshift-ingress"))
	before := len(mc.f.mutations())
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if !strings.Contains(res.Message, "direct address") || !strings.Contains(res.Message, "Allow proj") {
		t.Errorf("message %q", res.Message)
	}
	for _, w := range mc.f.mutations()[before:] {
		if strings.Contains(w, "/gateways/openshift-ai-inference") {
			t.Errorf("unpublish wrote the inference gateway: %s", w)
		}
	}
	// A listed project needs no word.
	mc2 := publishedMaaSCluster(t)
	res, _ = runMaaSOp(t, mc2.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if strings.Contains(res.Message, "direct address") {
		t.Errorf("listed project: %q", res.Message)
	}
}

// The model deleted elsewhere takes its MaaSModelRef with it (garbage
// collection): the recorded publication shows as partly published, and
// Unpublish finishes it.
func TestMaaSUnpublish_AfterGarbageCollectionTookTheModelRef(t *testing.T) {
	mc := publishedMaaSCluster(t)
	deleteFakeObject(mc, testModelPath)
	deleteFakeObject(mc, tMaaSRef)
	st, _ := GetMaaSStatus(mc.c)
	found := false
	for _, it := range st.Items {
		found = found || (it.Name == "qwen25-05b-instruct" && it.Partly)
	}
	if !found {
		t.Fatalf("no partly published row: %+v", st.Items)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); len(rec.Published) != 0 || len(rec.GenAI) != 0 || len(rec.ModelRefs) != 0 {
		t.Errorf("record kept %+v", rec)
	}
}
