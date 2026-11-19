package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tMaaSRefs = "/apis/maas.opendatahub.io/v1alpha1/namespaces/proj/maasmodelrefs/"

// maasWithModels is a MaaS cluster with the platform set up and the given
// catalog models set up in proj.
func maasWithModels(t *testing.T, names ...string) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for i, name := range names {
		putModelDeploymentFor(mc, name)
		if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: i == 0, Project: "proj", Model: name}); !res.Success {
			t.Fatalf("model %s: %+v", name, res)
		}
	}
	return mc
}

// The chosen chat model is published under its own name; two models of a
// project are published side by side, and unpublishing one keeps the
// other's entries and the project on the gateway.
func TestMaaSPublish_TheChosenModel(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "smollm2-135m-instruct"})
	if !res.Success {
		t.Fatalf("publish smollm2: %+v", res)
	}
	ref := mc.f.get(tMaaSRefs + "smollm2-135m-instruct")
	if ref == nil || mc.f.has(tMaaSRef) {
		t.Fatalf("refs: smollm2 %v, qwen25 %v", ref != nil, mc.f.has(tMaaSRef))
	}
	if name := ref["spec"].(map[string]any)["modelRef"].(map[string]any)["name"]; name != "smollm2-135m-instruct" {
		t.Errorf("modelRef %v", name)
	}
	if !strings.Contains(res.Message, "smollm2-135m-instruct") {
		t.Errorf("message %q", res.Message)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish the default: %+v", res)
	}
	sub, _ := json.Marshal(mc.f.get(tMaaSSub)["spec"])
	if !strings.Contains(string(sub), `"name":"qwen25-05b-instruct"`) || !strings.Contains(string(sub), `"name":"smollm2-135m-instruct"`) {
		t.Fatalf("subscription %s", sub)
	}
	rec, _ := readMaaSRecord(mc.c)
	if strings.Join(rec.Published, " ") != "proj/qwen25-05b-instruct proj/smollm2-135m-instruct" {
		t.Fatalf("record %v", rec.Published)
	}
	// Only the published model is routed through the MaaS gateway.
	if m, _, _ := readToolModel(mc.c, "proj", "smollm2-135m-instruct"); !modelHasMaaSRef(m) {
		t.Error("smollm2 is not routed through the MaaS gateway")
	}
	st, _ := GetMaaSStatus(mc.c)
	if len(st.Items) != 2 || !st.Items[0].Published || !st.Items[1].Published || st.Items[0].Name != "qwen25-05b-instruct" {
		t.Fatalf("items %+v", st.Items)
	}

	// The model of a published one cannot be deleted; the other one can.
	if res, _ := TeardownModels(mc.c, "proj", "smollm2-135m-instruct"); res.Success || res.ErrorCode != "in_use" {
		t.Fatalf("teardown while published: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
		t.Fatalf("unpublish smollm2: %+v", res)
	}
	if mc.f.has(tMaaSRefs+"smollm2-135m-instruct") || !mc.f.has(tMaaSRef) {
		t.Fatal("unpublish removed the wrong reference")
	}
	sub, _ = json.Marshal(mc.f.get(tMaaSSub)["spec"])
	if strings.Contains(string(sub), "smollm2") || !strings.Contains(string(sub), "qwen25-05b-instruct") {
		t.Fatalf("subscription after unpublish %s", sub)
	}
	gw, _ := json.Marshal(mc.f.get(tMaaSGateway))
	if !strings.Contains(string(gw), `"proj"`) {
		t.Fatalf("the gateway no longer admits proj while qwen25 is published: %s", gw)
	}
	if m, _, _ := readToolModel(mc.c, "proj", "smollm2-135m-instruct"); modelHasMaaSRef(m) {
		t.Error("smollm2 still routes through the MaaS gateway")
	}
}

// The embedding model is listed with why it is not offered, and refused.
func TestMaaS_OnlyChatModelsArePublishable(t *testing.T) {
	mc := maasWithModels(t, "granite-embedding-125m-english")
	st, _ := GetMaaSStatus(mc.c)
	if len(st.Items) != 1 || st.Items[0].Name != "granite-embedding-125m-english" || !strings.Contains(st.Items[0].SetupBlockedReason, "chat models only") {
		t.Fatalf("items %+v", st.Items)
	}
	before := len(mc.f.mutations())
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "granite-embedding-125m-english"})
	if res.Success || res.ErrorCode != "validation" {
		t.Fatalf("publish embedding: %+v", res)
	}
	if m := mc.f.mutations()[before:]; len(m) != 0 {
		t.Errorf("wrote %v", m)
	}
	// Its teardown does not look for a MaaS reference it can never have
	// (the ServiceAccount may not read one of that name).
	mc.f.fail["GET "+tMaaSRefs+"granite-embedding-125m-english"] = 403
	if res, _ := TeardownModels(mc.c, "proj", "granite-embedding-125m-english"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
}

func TestValidateMaaSRequest_Model(t *testing.T) {
	for req, want := range map[types.MaaSRequest]string{
		{Action: "publish", Project: "p", Model: "qwen3-1-7b"}:                     "",
		{Action: "unpublish", Project: "p", Model: "smollm2-135m-instruct"}:        "",
		{Action: "publish", Project: "p", Model: "llama"}:                          "unknown model",
		{Action: "publish", Project: "p", Model: "granite-embedding-125m-english"}: "only chat models are published to MaaS",
		{Action: "setup", Model: "qwen3-1-7b"}:                                     "the platform takes no model",
	} {
		if got := ValidateMaaSRequest(req); got != want {
			t.Errorf("%+v: %q, want %q", req, got, want)
		}
	}
}

// A record written before models were named lists projects only: each one
// is the default model's publication.
func TestMaaSRecord_ReadsAProjectOnlyEntryAsTheDefaultModel(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON(testRecordPath, `{"metadata":{"name":"rhoai-nightly-updater-snapshot"},"data":{"maas":"{\"published\":[\"proj\"]}"}}`)
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	rec, err := readMaaSRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.published("proj", "qwen25-05b-instruct") || rec.published("proj", "smollm2-135m-instruct") || strings.Join(rec.projects(), " ") != "proj" {
		t.Fatalf("record %+v", rec)
	}
}

// The clean-up unpublishes every publication of a project.
func TestUnpublishMaaSModel_UnpublishesEveryModelOfTheProject(t *testing.T) {
	mc := maasWithModels(t, "qwen25-05b-instruct", "qwen3-1-7b")
	for _, name := range []string{"qwen25-05b-instruct", "qwen3-1-7b"} {
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: name}); !res.Success {
			t.Fatalf("publish %s: %+v", name, res)
		}
	}
	if res, _ := UnpublishMaaSModel(mc.c, "proj"); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if mc.f.has(tMaaSRef) || mc.f.has(tMaaSRefs+"qwen3-1-7b") || mc.f.has(tMaaSSub) {
		t.Fatal("a publication is left")
	}
	if res, _ := TeardownModels(mc.c, "proj"); !res.Success {
		t.Fatalf("models teardown after the clean-up's unpublish: %+v", res)
	}
}
