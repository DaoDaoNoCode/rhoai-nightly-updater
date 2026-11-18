package cluster

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func modelMetaOf(mc *modelsCluster) (map[string]interface{}, map[string]interface{}) {
	meta := mc.f.get(testModelPath)["metadata"].(map[string]interface{})
	labels, _ := meta["labels"].(map[string]interface{})
	ann, _ := meta["annotations"].(map[string]interface{})
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
	rec, _ := readMaaSRecord(mc.c)
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
	if rec, _ := readMaaSRecord(mc.c); len(rec.GenAI) != 0 {
		t.Errorf("record kept %+v", rec.GenAI)
	}
}
