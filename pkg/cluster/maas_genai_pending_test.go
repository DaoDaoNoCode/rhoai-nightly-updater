package cluster

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// failGenAIRestore forces failure on the merge patch that restores the genai
// label; all other patches against the model still succeed.
func failGenAIRestore(mc *modelsCluster, on *bool) {
	mc.f.beforeRequest = func(r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != testModelPath {
			return
		}
		body, _ := io.ReadAll(r.Body)
		mc.f.mu.Lock()
		defer mc.f.mu.Unlock()
		delete(mc.f.fail, "PATCH "+testModelPath)
		if *on && strings.Contains(string(body), "genai-asset") {
			mc.f.fail["PATCH "+testModelPath] = 500
		}
	}
}

// Unpublish is incomplete when it pulled the model off the gateway but could
// not restore Gen AI studio label and annotation: MaaSModelRef is already removed
// and the model no longer rides MaaS, but GenAI fields on the record stayed unset.
// Prune retains those fields (status stays Partly published with Unpublish available);
// a second Unpublish completes the restoration.
func TestMaaS_AnUnpublishThatFailedOnGenAIKeepsWhatItMustPutBack(t *testing.T) {
	mc := publishedMaaSCluster(t)
	on := true
	failGenAIRestore(mc, &on)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "Gen AI studio") {
		t.Fatalf("unpublish = %+v", res)
	}
	if mc.f.has(tMaaSRef) {
		t.Fatal("the MaaSModelRef is still there")
	}
	if labels, _ := modelMetaOf(mc); labels["opendatahub.io/genai-asset"] != nil {
		t.Fatalf("label back although the patch failed: %v", labels)
	}

	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	rec, _ := serving.ReadMaaSRecord(mc.c)
	if _, ok := rec.GenAI["proj/"+tModel]; !ok {
		t.Fatalf("the prune dropped the values Unpublish must put back: %+v", rec)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := maasItem(st); !ok || !it.Partly || !strings.Contains(it.Message, "Gen AI studio") || !strings.Contains(it.Message, "Unpublish") {
		t.Fatalf("row = %+v (found %t)", it, ok)
	}
	if len(st.Removing) != 0 {
		t.Fatalf("removing = %+v", st.Removing)
	}

	on = false
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish again: %+v", res)
	}
	labels, ann := modelMetaOf(mc)
	if labels["opendatahub.io/genai-asset"] != "true" || ann["opendatahub.io/genai-use-case"] != "Chat" {
		t.Fatalf("not restored: %v %v", labels, ann)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); len(rec.Published) != 0 || len(rec.GenAI) != 0 {
		t.Fatalf("record kept %+v", rec)
	}
	if st, _ := GetMaaSStatus(mc.c); len(st.Items) != 1 || st.Items[0].Partly || st.Items[0].Message != "Not published" {
		t.Fatalf("items after = %+v", st.Items)
	}
}

// Prune only preserves GenAI data tied to models still owned here. Once the
// models record no longer lists the model, pending GenAI entries are dropped
// and the publication row disappears as it would without the failed restore.
func TestMaaS_PendingGenAIValuesOfAModelTheToolDoesNotOwnArePruned(t *testing.T) {
	mc := publishedMaaSCluster(t)
	on := true
	failGenAIRestore(mc, &on)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); res.Success {
		t.Fatalf("unpublish = %+v", res)
	}
	models, err := serving.ReadModelsRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	delete(models, serving.ModelRecordKey("proj", tModel))
	if err := serving.WriteModelsRecord(mc.c, models); err != nil {
		t.Fatal(err)
	}
	if _, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil {
		t.Fatal(err)
	}
	if rec, _ := serving.ReadMaaSRecord(mc.c); rec.IsPublished("proj", tModel) || len(rec.GenAI) != 0 {
		t.Fatalf("record kept %+v", rec)
	}
}
