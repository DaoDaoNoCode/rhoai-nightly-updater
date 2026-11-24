package cluster

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeTeardowns stubs out each teardown step that CleanUp invokes; entries in fail
// tie a step id to a response that makes that step fail. The returned slice lists
// steps in execution order.
func fakeTeardowns(t *testing.T, fail map[string]*types.OperationResponse) *[]string {
	t.Helper()
	ps, st, ml, md, gw, pg := cleanupPipelineServer, cleanupStorage, cleanupMLflow, cleanupModels, cleanupGateway, cleanupPlayground
	t.Cleanup(func() {
		cleanupPipelineServer, cleanupStorage, cleanupMLflow, cleanupModels, cleanupGateway, cleanupPlayground = ps, st, ml, md, gw, pg
	})
	var ran []string
	result := func(id, msg string) *types.OperationResponse {
		ran = append(ran, id)
		if r := fail[id]; r != nil {
			return r
		}
		return &types.OperationResponse{Success: true, Message: msg}
	}
	cleanupPipelineServer = func(_ *Client, project, _ string, _ map[string]string) (*types.OperationResponse, error) {
		return result(CleanupStepID("pipeline-server", project), "Pipeline server removed from '"+project+"'."), nil
	}
	cleanupStorage = func(_ *Client, req types.S3TeardownRequest) (*types.OperationResponse, error) {
		if req.DeleteNamespace {
			return result("storage", "S3 storage and namespace removed."), nil
		}
		return result("storage", "S3 storage removed."), nil
	}
	cleanupMLflow = func(*Client) (*types.OperationResponse, error) { return result("mlflow", "MLflow removed."), nil }
	cleanupPlayground = func(c *Client, project string) (*types.OperationResponse, error) {
		if c == nil || !c.ActsAsUser() {
			t.Errorf("playground teardown of %s does not get the user's client", project)
		}
		return result(CleanupStepID("playground", project), "Playground deleted from '"+project+"'."), nil
	}
	cleanupModels = func(_ *Client, project string) (*types.OperationResponse, error) {
		return result(CleanupStepID("model", project), "Model deleted from '"+project+"'."), nil
	}
	cleanupGateway = func(*Client, bool) (*types.OperationResponse, error) {
		return result("inference-gateway", "Inference gateway deleted."), nil
	}
	return &ran
}

// Model teardown must run ahead of the inference gateway they depend on, because
// gateway removal is blocked until every model is gone.
func TestCleanUp_DeletesTheModelsBeforeTheGateway(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{MLflow: true, Models: []string{"a", "b"}, InferenceGateway: true}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "mlflow model:a model:b inference-gateway" {
		t.Fatalf("order: %s", got)
	}
	if res.Message != "Deleted MLflow, the model in 'a', the model in 'b' and the inference gateway." {
		t.Errorf("message: %q", res.Message)
	}
}

// Playground removal precedes the models it references; the step must receive
// the user-scoped client so gen-ai calls use that user's token.
func TestCleanUp_DeletesThePlaygroundsBeforeTheModels(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	editTestClient(c, func(o *testClientOptions) { o.AsUser = true })
	res, err := CleanUp(c, types.CleanupRequest{Models: []string{"a"}, Playgrounds: []string{"a", "b"}, InferenceGateway: true}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "playground:a playground:b model:a inference-gateway" {
		t.Fatalf("order: %s", got)
	}
}

// When model deletion fails, the sequence halts and never reaches the gateway step.
func TestCleanUp_KeepsTheGatewayWhenAModelStays(t *testing.T) {
	ran := fakeTeardowns(t, map[string]*types.OperationResponse{
		"model:a": {Success: false, Message: "The model a/qwen25-05b-instruct is still being deleted after 2:00.", ErrorCode: "in_progress"},
	})
	_, c := newResourceFake(t)
	res, _ := CleanUp(c, types.CleanupRequest{Models: []string{"a"}, InferenceGateway: true}, nil)
	if res.Success || res.ErrorCode != "in_progress" || !strings.Contains(res.Message, "Not done: the inference gateway.") {
		t.Fatalf("got %+v", res)
	}
	if got := strings.Join(*ran, " "); got != "model:a" {
		t.Fatalf("ran: %s", got)
	}
}

func TestCleanUp_RunsThePartsInDependencyOrder(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	var events []UpdateStepEvent
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, Storage: true, DeleteNamespace: true, MLflow: true}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "pipeline-server:a pipeline-server:b storage mlflow" {
		t.Fatalf("order: %s", got)
	}
	if res.Message != "Deleted the pipeline server in 'a', the pipeline server in 'b', the S3 storage and MLflow." {
		t.Errorf("message: %q", res.Message)
	}
	// Progress callbacks should show a running status for each step, then that step's outcome.
	if len(events) != 8 || events[0].Status != "running" || events[5].Message != "S3 storage and namespace removed." {
		t.Errorf("events: %+v", events)
	}
}

func TestCleanUp_StopsAtTheFirstPartThatFails(t *testing.T) {
	ran := fakeTeardowns(t, map[string]*types.OperationResponse{
		"pipeline-server:b": {Success: false, Message: "Deletion of pipeline server 'nightly-dspa' was requested, but it is still terminating.", ErrorCode: "in_progress"},
	})
	_, c := newResourceFake(t)
	var last UpdateStepEvent
	res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, Storage: true, MLflow: true}, func(e UpdateStepEvent) { last = e })
	if res.Success || res.ErrorCode != "in_progress" {
		t.Fatalf("got %+v", res)
	}
	if got := strings.Join(*ran, " "); got != "pipeline-server:a pipeline-server:b" {
		t.Fatalf("the storage must not be deleted after a failed part: ran %s", got)
	}
	for _, want := range []string{"still terminating", "Deleted before it stopped: the pipeline server in 'a'.", "Not done: the S3 storage and MLflow."} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message lacks %q: %q", want, res.Message)
		}
	}
	if last.Step != "pipeline-server:b" || last.Status != "failed" {
		t.Errorf("last event: %+v", last)
	}
}

func TestCleanUp_StopsWhenTheOperationIsCancelled(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	editTestClient(c, func(o *testClientOptions) { o.Ctx = ctx })
	res, _ := CleanUp(c, types.CleanupRequest{MLflow: true}, nil)
	if res.Success || len(*ran) != 0 || !strings.Contains(res.Message, "Not done: MLflow.") {
		t.Fatalf("got %+v, ran %v", res, *ran)
	}
}

// Against a cluster with no resources from this tool installed, the live teardown
// functions still report success and indicate nothing remained to remove.
func TestCleanUp_NothingLeftSucceeds(t *testing.T) {
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"p1"}, Storage: true, MLflow: true}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// fakeRecipeProjectTeardown overrides the recipe-project problem check and teardown;
// holds lists per-project resources the tool never created. The check blocks until
// the user confirms each item (nil holds means no block), and returns the full hold
// list like recipe.ProjectTeardownProblem. Teardown appends the UID suffix after "@".
func fakeRecipeProjectTeardown(t *testing.T, ran *[]string, holds map[string][]string) {
	t.Helper()
	pr, cp, co := recipeProjectProblem, cleanupProject, cleanupComponents
	t.Cleanup(func() { recipeProjectProblem, cleanupProject, cleanupComponents = pr, cp, co })
	recipeProjectProblem = func(_ *Client, name, _ string, confirmed []string) (string, string, []string, error) {
		var unconfirmed []string
		for _, h := range holds[name] {
			if !slices.Contains(confirmed, h) {
				unconfirmed = append(unconfirmed, h)
			}
		}
		if len(unconfirmed) > 0 {
			return "Project " + name + " still holds what this tool did not create: " + strings.Join(unconfirmed, ", ") + ".", "confirm_project_contents", holds[name], nil
		}
		return "", "", holds[name], nil
	}
	cleanupProject = func(_ *Client, name, uid string, confirmed []string) (*types.OperationResponse, error) {
		id := CleanupStepID("recipe-project", name)
		if uid != "" {
			id += "@" + uid
		}
		*ran = append(*ran, id+":"+strings.Join(confirmed, "|"))
		return &types.OperationResponse{Success: true, Message: "Deleted project " + name + "."}, nil
	}
	cleanupComponents = func(*Client, []string) (*types.OperationResponse, error) {
		*ran = append(*ran, "components")
		return &types.OperationResponse{Success: true, Message: "Components restored."}, nil
	}
}

// Recipe-project deletion runs only after in-project parts (pipeline server,
// playground, model here) finish, and before components restore, carrying
// ConfirmProjectContents from the request.
func TestCleanUp_RecipeProjectAfterItsPartsBeforeTheComponents(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	fakeRecipeProjectTeardown(t, ran, nil)
	_, c := newResourceFake(t)
	res, err := CleanUp(c.SignedInUser(), types.CleanupRequest{
		PipelineServers: []string{"recipe-dev"}, Storage: true, MLflow: true, Playgrounds: []string{"recipe-dev"}, Models: []string{"recipe-dev"},
		RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": "u1"}, ConfirmProjectContents: map[string][]string{"recipe-dev": {"Notebook wb"}}, Components: []string{"aipipelines"},
	}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	want := "pipeline-server:recipe-dev storage mlflow playground:recipe-dev model:recipe-dev recipe-project:recipe-dev@u1:Notebook wb components"
	if got := strings.Join(*ran, " "); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
}

// If the recipe-project check would reject the project, CleanUp aborts upfront
// without running any teardown step or mutating cluster state.
func TestCleanUp_RecipeProjectRefusalDeletesNothing(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	fakeRecipeProjectTeardown(t, ran, map[string][]string{"recipe-dev": {"Notebook wb", "PersistentVolumeClaim wb-data"}})
	f, c := newResourceFake(t)
	var events []UpdateStepEvent
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"recipe-dev"}, RecipeProjects: []string{"recipe-dev"}}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res.Success || res.ErrorCode != "confirm_project_contents" || !strings.Contains(res.Message, "Nothing was deleted") {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(*ran) != 0 || len(f.mutations()) != 0 {
		t.Fatalf("ran %v, changed %v", *ran, f.mutations())
	}
	// The UI surfaces confirmable items from the failed step's Detail field, one entry per line.
	if len(events) != 1 || events[0].Step != "recipe-project:recipe-dev" || events[0].Status != "failed" || events[0].Detail != "Notebook wb\nPersistentVolumeClaim wb-data" {
		t.Fatalf("events %+v", events)
	}
}
