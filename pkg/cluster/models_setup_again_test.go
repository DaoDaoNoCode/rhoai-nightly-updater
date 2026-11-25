package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Models tab's Set up again of the tool's model is its setup again
// (the same request): the model is updated in place to the catalog's spec,
// not made anew, and the status reports the vLLM options it runs with, so
// the card shows the live ones rather than the catalog's.
func TestModelsSetupAgain_UpdatesInPlaceAndStatusShowsLiveOptions(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	entry, _ := serving.CatalogEntry("qwen25-05b-instruct")
	liveArgs := func() *string {
		t.Helper()
		st, err := GetResourcesStatus(mc.c)
		if err != nil || len(st.Models.Items) != 1 {
			t.Fatalf("status: %+v %v", st, err)
		}
		return st.Models.Items[0].VLLMArgs
	}
	if a := liveArgs(); a == nil || *a != entry.Args {
		t.Fatalf("live options after setup = %v, want %q", a, entry.Args)
	}
	// Set up by an earlier version, without --enable-force-include-usage.
	var uid string
	editModel(mc, func(spec, meta map[string]any) {
		uid, _ = meta["uid"].(string)
		for _, e := range templateContainer(spec, "containers", "main")["env"].([]any) {
			if ev := e.(map[string]any); ev["name"] == "VLLM_ADDITIONAL_ARGS" {
				ev["value"] = "--max-model-len 4096"
			}
		}
	})
	if a := liveArgs(); a == nil || *a != "--max-model-len 4096" {
		t.Fatalf("live options of the older model = %v", a)
	}
	mc.f.requests = nil
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
		t.Fatalf("set up again: %+v", res)
	}
	// Only patches of the model itself (server-side apply): no delete, no create.
	patched := false
	for _, m := range mc.f.mutations() {
		if !strings.Contains(m, "/llminferenceservices") {
			continue
		}
		if m != "PATCH "+testModelPath {
			t.Errorf("not in place: %s", m)
		}
		patched = true
	}
	if !patched {
		t.Error("the model was not written")
	}
	if got, _ := mc.f.get(testModelPath)["metadata"].(map[string]any)["uid"].(string); got != uid {
		t.Errorf("uid %q, want the same model %q", got, uid)
	}
	if a := liveArgs(); a == nil || *a != entry.Args || !strings.Contains(*a, "--enable-force-include-usage") {
		t.Errorf("live options after set up again = %v, want %q", a, entry.Args)
	}
}
