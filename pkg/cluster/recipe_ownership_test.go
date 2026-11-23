package cluster

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tRunID = "run-1"

// tOwnershipRecipe yields a validated recipe that enables Pipelines and
// MLflow for project recipe-dev.
func tOwnershipRecipe(t *testing.T) types.EnvironmentRecipe {
	t.Helper()
	r, problems := recipe.Validate([]byte(tRecipeBase + `{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true}}}}`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return r
}

// runOwnedStep drives a single recipe step in the ownership test fixture for run tRunID.
func runOwnedStep(t *testing.T, c *Client, id string) *types.OperationResponse {
	t.Helper()
	r := tOwnershipRecipe(t)
	s := &recipe.State{RunID: tRunID, Recipe: r, Project: &types.RecipeProjectRef{Name: "recipe-dev"}}
	res, err := recipeStep(t, r, id).Run(c, s, func(types.UpdateStepEvent) {})
	if err != nil || res == nil {
		t.Fatalf("%s: %+v %v", id, res, err)
	}
	return res
}

// runMark returns kube.RecipeRunAnnotation on the fake at path, or "" when absent.
func runMark(f *resourceFake, path string) string {
	if !f.has(path) {
		return ""
	}
	a, _ := f.get(path)["metadata"].(map[string]any)["annotations"].(map[string]any)
	v, _ := a[kube.RecipeRunAnnotation].(string)
	return v
}

// S3 Set up stamps storage with the run only for resources it created—never
// for pre-existing tool installs (including not-ready), and not when Set up
// is refused.
func TestRecipeS3Step_MarksOnlyWhatItCreated(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		readyAfterApply(f)
		if res := runOwnedStep(t, c, recipe.StepS3); !res.Success || runMark(f, s3DeployPath) != tRunID {
			t.Fatalf("%+v mark %q", res, runMark(f, s3DeployPath))
		}
	})
	t.Run("there before, not ready", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putS3Namespace(f, toolLabelJSON)
		putS3Deployment(f, 0)
		readyAfterApply(f)
		if res := runOwnedStep(t, c, recipe.StepS3); !res.Success || runMark(f, s3DeployPath) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, s3DeployPath))
		}
	})
	t.Run("refused", func(t *testing.T) {
		fastS3Timings(t)
		f, c := newResourceFake(t)
		putS3Namespace(f, `{"owner":"someone"}`)
		if res := runOwnedStep(t, c, recipe.StepS3); res.Success || runMark(f, s3DeployPath) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, s3DeployPath))
		}
	})
}

// Pipeline server Set up applies the run mark solely to servers it created,
// including when the readiness wait later times out.
func TestRecipePipelineServerStep_MarksOnlyWhatItCreated(t *testing.T) {
	defer storage.SetPipelineServerReadyTimingForTest(50*time.Millisecond, 10*time.Millisecond)()
	path := dspaPath("recipe-dev", storage.DSPAName)
	base := func(t *testing.T) (*resourceFake, *Client) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		putReadyManagedS3(f)
		putDSProject(f, "recipe-dev")
		return f, c
	}
	t.Run("created, not ready yet", func(t *testing.T) {
		f, c := base(t)
		if res := runOwnedStep(t, c, recipe.StepPipelineServer); res.Success || runMark(f, path) != tRunID {
			t.Fatalf("%+v mark %q", res, runMark(f, path))
		}
	})
	t.Run("there before, not ready", func(t *testing.T) {
		f, c := base(t)
		putDSPA(f, "recipe-dev", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
		if res := runOwnedStep(t, c, recipe.StepPipelineServer); runMark(f, path) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, path))
		}
	})
	t.Run("refused", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		putDSProject(f, "recipe-dev")
		if res := runOwnedStep(t, c, recipe.StepPipelineServer); res.Success || runMark(f, path) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, path))
		}
	})
}

// MLflow gets the run annotation only if Set up created the CR—excluding
// refused Set up and deployments that predated the run without readiness.
func TestRecipeMLflowStep_MarksOnlyWhatItCreated(t *testing.T) {
	defer mlflow.SetReadyTimingForTest(50*time.Millisecond, 10*time.Millisecond)()
	t.Run("created, not ready yet", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		if res := runOwnedStep(t, c, recipe.StepMLflow); res.Success || runMark(f, mlflowPath) != tRunID {
			t.Fatalf("%+v mark %q", res, runMark(f, mlflowPath))
		}
	})
	t.Run("refused", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		f.fail["POST /apis/"+mlflow.APIGroup+"/mlflows"] = 404
		if res := runOwnedStep(t, c, recipe.StepMLflow); res.Success || runMark(f, mlflowPath) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, mlflowPath))
		}
	})
	t.Run("there before, not ready", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		putMLflow(f, toolLabelJSON, envkit.ToolFieldManager, "Apply", "", `{}`, notAvailable)
		if res := runOwnedStep(t, c, recipe.StepMLflow); runMark(f, mlflowPath) != "" {
			t.Fatalf("%+v mark %q", res, runMark(f, mlflowPath))
		}
	})
}

// markedFake wires up managed S3, a recipe-dev pipeline server, and MLflow as
// tool-owned objects, optionally annotated with run mark ("" leaves unmarked).
func markedFake(t *testing.T, mark string) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "recipe-dev")
	ann := ""
	if mark != "" {
		ann = `,"annotations":{"` + kube.RecipeRunAnnotation + `":"` + mark + `"}`
	}
	f.putJSON(s3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+ann+`},"status":{"readyReplicas":1}}`)
	putDSPA(f, "recipe-dev", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, ann, "")
	putMLflow(f, toolLabelJSON, envkit.ToolFieldManager, "Apply", ann, `{}`, `{}`)
	return f, c
}

// Ownership includes storage, pipeline server, and MLflow in cleanup only when
// they still carry this run's mark; the request records RecipeRun.
func TestRecipeOwnership_MarkedObjects(t *testing.T) {
	run := &types.RecipeRun{ID: tRunID, Project: &types.RecipeProjectRef{Name: "recipe-dev"}}
	_, c := markedFake(t, tRunID)
	created, req := recipe.Ownership(c, run)
	if req == nil || !req.Storage || !req.MLflow || len(req.PipelineServers) != 1 || req.RecipeRun != tRunID ||
		!created[recipe.StepS3] || !created[recipe.StepPipelineServer] || !created[recipe.StepMLflow] {
		t.Fatalf("created %v, cleanup %+v", created, req)
	}
	for _, mark := range []string{"", "another-run"} {
		_, c := markedFake(t, mark)
		if created, req := recipe.Ownership(c, run); req != nil || len(created) != 0 {
			t.Errorf("mark %q: created %v, cleanup %+v", mark, created, req)
		}
	}
}

// A GET/POST race where the object is swapped for one lacking the run mark
// makes run-scoped Clean up refuse and leave it intact; Clean up everything
// without RecipeRun still removes tool objects as before.
func TestRecipeCleanup_RefusesWhatIsNoLongerTheRuns(t *testing.T) {
	t.Cleanup(mlflow.SetDeleteTimingForTest(50*time.Millisecond, 10*time.Millisecond))
	fastS3Timings(t)
	fastPipelineTimings(t)
	for _, part := range []struct {
		name string
		req  types.CleanupRequest
		path string
	}{
		{"pipeline server", types.CleanupRequest{PipelineServers: []string{"recipe-dev"}}, dspaPath("recipe-dev", storage.DSPAName)},
		{"storage", types.CleanupRequest{Storage: true}, s3DeployPath},
		{"mlflow", types.CleanupRequest{MLflow: true}, mlflowPath},
	} {
		t.Run(part.name, func(t *testing.T) {
			f, c := markedFake(t, "") // stand-in object with no run annotation
			if part.req.Storage {
				dropDSPA(f) // omit pipeline server so storage is the retained resource
			}
			req := part.req
			req.RecipeRun = tRunID
			res, _ := CleanUp(c, req, nil)
			if res.Success || !strings.Contains(res.Message, "lacks the run's annotation") || hasMutation(f, "DELETE") || !f.has(part.path) {
				t.Fatalf("%+v, mutations %v", res, f.mutations())
			}
		})
	}
	t.Run("marked: deleted", func(t *testing.T) {
		f, c := markedFake(t, tRunID)
		putDSPO(f, 1)
		res, _ := CleanUp(c, types.CleanupRequest{MLflow: true, RecipeRun: tRunID}, nil)
		if !res.Success || f.has(mlflowPath) {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("clean up everything is unchanged", func(t *testing.T) {
		f, c := markedFake(t, "")
		res, _ := CleanUp(c, types.CleanupRequest{MLflow: true}, nil)
		if !res.Success || f.has(mlflowPath) {
			t.Fatalf("%+v", res)
		}
	})
}

// When S3 is replaced after the run's ownership check but before Deployment
// delete runs, no further resources are removed (fail closed).
func TestRecipeCleanup_StorageReplacedAfterTheCheck(t *testing.T) {
	fastS3Timings(t)
	f, c := markedFake(t, tRunID)
	dropDSPA(f)
	gets := 0
	f.beforeServe = func(method, path string) {
		if method != http.MethodGet || path != s3DeployPath {
			return
		}
		gets++
		if gets == 2 { // after ownership check, before delete path's GET
			f.mu.Lock()
			k, _ := parseFakePath(s3DeployPath)
			f.objects[k] = map[string]any{"metadata": map[string]any{"name": storage.S3Deployment, "uid": "someone-else", "labels": map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue}}}
			f.mu.Unlock()
		}
	}
	res, _ := CleanUp(c, types.CleanupRequest{Storage: true, RecipeRun: tRunID}, nil)
	if res.Success || !strings.Contains(res.Message, "replaced since the recipe run's check") || hasMutation(f, "DELETE") || uidOf(f, s3DeployPath) != "someone-else" {
		t.Fatalf("%+v, mutations %v", res, f.mutations())
	}
}

func dropDSPA(f *resourceFake) {
	k, _ := parseFakePath(dspaPath("recipe-dev", storage.DSPAName))
	f.mu.Lock()
	delete(f.objects, k)
	f.mu.Unlock()
}

func uidOf(f *resourceFake, path string) string {
	uid, _ := f.get(path)["metadata"].(map[string]any)["uid"].(string)
	return uid
}
