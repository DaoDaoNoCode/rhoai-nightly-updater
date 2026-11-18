package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeTeardowns replaces the teardowns a clean-up runs; fail maps a step id
// to the response that fails it. It returns the steps in the order they ran.
func fakeTeardowns(t *testing.T, fail map[string]*types.OperationResponse) *[]string {
	t.Helper()
	ps, st, ml, md, gw, pg := cleanupPipelineServer, cleanupStorage, cleanupMLflow, cleanupModels, cleanupGateway, cleanupPlayground
	t.Cleanup(func() {
		cleanupPipelineServer, cleanupStorage, cleanupMLflow, cleanupModels, cleanupGateway, cleanupPlayground = ps, st, ml, md, gw, pg
	})
	var ran []string
	result := func(id, msg string) (*types.OperationResponse, error) {
		ran = append(ran, id)
		if r := fail[id]; r != nil {
			return r, nil
		}
		return &types.OperationResponse{Success: true, Message: msg}, nil
	}
	cleanupPipelineServer = func(_ *Client, project string) (*types.OperationResponse, error) {
		return result(CleanupStepID("pipeline-server", project), "Pipeline server removed from '"+project+"'.")
	}
	cleanupStorage = func(_ *Client, req types.S3TeardownRequest) (*types.OperationResponse, error) {
		if req.DeleteNamespace {
			return result("storage", "S3 storage and namespace removed.")
		}
		return result("storage", "S3 storage removed.")
	}
	cleanupMLflow = func(*Client) (*types.OperationResponse, error) { return result("mlflow", "MLflow removed.") }
	cleanupPlayground = func(_ *Client, project, token string) (*types.OperationResponse, error) {
		if token != "genai-token" {
			t.Errorf("playground teardown got token %q", token)
		}
		return result(CleanupStepID("playground", project), "Playground deleted from '"+project+"'.")
	}
	cleanupModels = func(_ *Client, project string) (*types.OperationResponse, error) {
		return result(CleanupStepID("model", project), "Model deleted from '"+project+"'.")
	}
	cleanupGateway = func(*Client, bool) (*types.OperationResponse, error) {
		return result("inference-gateway", "Inference gateway deleted.")
	}
	return &ran
}

// The models go before the inference gateway they route through, whose
// teardown refuses while one is left.
func TestCleanUp_DeletesTheModelsBeforeTheGateway(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{MLflow: true, Models: []string{"a", "b"}, InferenceGateway: true}, "", nil)
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

// The playgrounds go before the models they use, with the gen-ai token.
func TestCleanUp_DeletesThePlaygroundsBeforeTheModels(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{Models: []string{"a"}, Playgrounds: []string{"a", "b"}, InferenceGateway: true}, "genai-token", nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "playground:a playground:b model:a inference-gateway" {
		t.Fatalf("order: %s", got)
	}
}

// A model that cannot be deleted stops the run before the gateway.
func TestCleanUp_KeepsTheGatewayWhenAModelStays(t *testing.T) {
	ran := fakeTeardowns(t, map[string]*types.OperationResponse{
		"model:a": {Success: false, Message: "The model a/qwen25-05b-instruct is still being deleted after 2:00.", ErrorCode: "in_progress"},
	})
	_, c := newResourceFake(t)
	res, _ := CleanUp(c, types.CleanupRequest{Models: []string{"a"}, InferenceGateway: true}, "", nil)
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
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, Storage: true, DeleteNamespace: true, MLflow: true}, "", func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "pipeline-server:a pipeline-server:b storage mlflow" {
		t.Fatalf("order: %s", got)
	}
	if res.Message != "Deleted the pipeline server in 'a', the pipeline server in 'b', the S3 storage and MLflow." {
		t.Errorf("message: %q", res.Message)
	}
	// Each part is reported as running, then with its own result.
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
	res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, Storage: true, MLflow: true}, "", func(e UpdateStepEvent) { last = e })
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
	c.ctx = ctx
	res, _ := CleanUp(c, types.CleanupRequest{MLflow: true}, "", nil)
	if res.Success || len(*ran) != 0 || !strings.Contains(res.Message, "Not done: MLflow.") {
		t.Fatalf("got %+v, ran %v", res, *ran)
	}
}

// The real teardowns, on a cluster where nothing of this tool is left:
// each part says so and succeeds.
func TestCleanUp_NothingLeftSucceeds(t *testing.T) {
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"p1"}, Storage: true, MLflow: true}, "", nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
}
