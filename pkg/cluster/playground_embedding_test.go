package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// testQwenAndGranite is aaa/models with qwen25 and the Granite embedding
// model ready, as the dashboard's BFF lists an LLMInferenceService whose
// genai-use-case names embeddings.
const testQwenAndGranite = `{"data":[
	{"model_name":"qwen25-05b-instruct","model_id":"qwen25-05b-instruct","usecase":"Chat","status":"Running","display_name":"Qwen2.5 0.5B Instruct (CPU)","model_source_type":"namespace","capabilities":["text-generation"]},
	{"model_name":"granite-embedding-125m-english","model_id":"granite-embedding-125m-english","usecase":"Embeddings, Semantic search","status":"Running","display_name":"Granite Embedding 125M English (CPU)","model_source_type":"namespace","capabilities":["text-generation"]}]}`

const testGranitePath = "/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/granite-embedding-125m-english"

// putGranite stores the Granite LLMInferenceService in proj: this tool's
// (its label and a write by it), or someone else's.
func (e *playgroundEnv) putGranite(ours bool) {
	meta := `{"name":"granite-embedding-125m-english","namespace":"proj"}`
	if ours {
		meta = `{"name":"granite-embedding-125m-english","namespace":"proj","labels":{"` + kube.ManagedByLabelKey + `":"` + kube.ManagedByLabelValue + `"},
			"managedFields":[{"manager":"` + envkit.ToolFieldManager + `","operation":"Apply"}]}`
	}
	e.k8s.putJSON(testGranitePath, `{"metadata":`+meta+`}`)
	if ours {
		recordModelFixture(e.k8s, testGranitePath)
	}
}

func installedModels(t *testing.T, e *playgroundEnv) string {
	t.Helper()
	if len(e.g.installs) != 1 {
		t.Fatalf("installs: %d", len(e.g.installs))
	}
	body, _ := json.Marshal(e.g.installs[0]["models"])
	return string(body)
}

// The dashboard's install takes a model type and an embedding dimension
// per model, so this tool's ready embedding model goes in as one, with its
// 768 dimensions (the BFF's default would be 128).
func TestPlayground_SetupAddsTheToolsEmbeddingModel(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = testQwenAndGranite
	e.putGranite(true)
	res, steps := e.run(t, setup())
	if !res.Success {
		t.Fatalf("got %+v", res)
	}
	want := `[{"model_name":"qwen25-05b-instruct","model_source_type":"namespace","model_type":"llm"},{"embedding_dimension":768,"model_name":"granite-embedding-125m-english","model_source_type":"namespace","model_type":"embedding"}]`
	if got := installedModels(t, e); got != want {
		t.Errorf("models\n got %s\nwant %s", got, want)
	}
	said := false
	for _, s := range steps {
		said = said || (s.Step == "check" && strings.Contains(s.Message, "granite-embedding-125m-english (embedding, 768 dimensions)"))
	}
	if !said {
		t.Errorf("the check does not name the embedding model: %v", steps)
	}
}

// An embedding model this tool did not create is left out: its dimension
// is not known.
func TestPlayground_LeavesOutAnEmbeddingModelItDidNotCreate(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = testQwenAndGranite
	e.putGranite(false)
	if res, _ := e.run(t, setup()); !res.Success {
		t.Fatalf("got %+v", res)
	}
	if got := installedModels(t, e); strings.Contains(got, "granite") {
		t.Errorf("models %s", got)
	}
}

// The status says which embedding model Set up adds; a chosen one is used,
// but never without a chat model.
func TestPlayground_StatusAndChoiceOfTheToolsEmbeddingModel(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.models["proj"] = testQwenAndGranite
	e.putGranite(true)
	st, err := GetPlaygroundStatus(e.c, e.reader, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models) != 2 || st.Models[1].Chat || st.Models[1].EmbeddingDimension != 768 || st.Models[0].EmbeddingDimension != 0 {
		t.Fatalf("models %+v", st.Models)
	}
	req := setup()
	req.Models = []types.PlaygroundModelRef{{Name: "granite-embedding-125m-english", Source: "namespace"}}
	if res, _ := e.run(t, req); res.Success || res.ErrorCode != "no_model" {
		t.Fatalf("an embedding model alone: %+v", res)
	}
	req.Models = append(req.Models, types.PlaygroundModelRef{Name: "qwen25-05b-instruct", Source: "namespace"})
	if res, _ := e.run(t, req); !res.Success {
		t.Fatalf("chosen: %+v", res)
	}
	if got := installedModels(t, e); !strings.Contains(got, `"model_type":"embedding"`) {
		t.Errorf("models %s", got)
	}
}
