package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// F3: the rolled-up cleanup message should match what happened in every phase.
// Component and recipe-project handlers choose phrasing for the case at hand—
// e.g. retained resources or a namespace that was already missing—and must not
// claim "Deleted" when no removal occurred.
func TestCleanUp_SaysWhatEachPartDid(t *testing.T) {
	for _, tc := range []struct {
		name            string
		req             types.CleanupRequest
		components      string
		project         string
		want            string
		wantInnerFailed bool
	}{
		{name: "components kept", req: types.CleanupRequest{Components: []string{"aipipelines"}},
			components: "Not turned back: aipipelines (this tool did not turn it on).",
			want:       "Not turned back: aipipelines (this tool did not turn it on)."},
		{name: "nothing to turn back", req: types.CleanupRequest{Components: []string{"aipipelines"}},
			components: "No component to turn back.",
			want:       "No component to turn back."},
		{name: "deleted and turned back", req: types.CleanupRequest{MLflow: true, Components: []string{"aipipelines"}},
			components: "aipipelines is Removed again, as before setup.",
			want:       "Deleted MLflow. aipipelines is Removed again, as before setup."},
		{name: "project already gone", req: types.CleanupRequest{RecipeProjects: []string{"recipe-dev"}},
			project: "Project recipe-dev is gone.",
			want:    "Project recipe-dev is gone."},
		{name: "project deleted", req: types.CleanupRequest{MLflow: true, RecipeProjects: []string{"recipe-dev"}},
			project: "Deleted project recipe-dev; OpenShift removes what it holds.",
			want:    "Deleted MLflow. Deleted project recipe-dev; OpenShift removes what it holds."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := fakeTeardowns(t, nil)
			fakeRecipeProjectTeardown(t, ran, nil)
			cleanupComponents = func(*Client, []string) (*types.OperationResponse, error) {
				return &types.OperationResponse{Success: true, Message: tc.components}, nil
			}
			cleanupProject = func(*Client, string, string, []string) (*types.OperationResponse, error) {
				return &types.OperationResponse{Success: true, Message: tc.project}, nil
			}
			_, c := newResourceFake(t)
			res, err := CleanUp(c, tc.req, nil)
			if err != nil || !res.Success || res.Message != tc.want {
				t.Fatalf("got %+v, %v\nwant message %q", res, err, tc.want)
			}
		})
	}
}

// When cleanup stops after a step has only emitted status, the reply identifies that step.
func TestCleanUp_StoppedNamesWhatThePartsSaid(t *testing.T) {
	ran := fakeTeardowns(t, map[string]*types.OperationResponse{"mlflow": {Success: false, Message: "MLflow is still being deleted.", ErrorCode: "in_progress"}})
	fakeRecipeProjectTeardown(t, ran, nil)
	cleanupComponents = func(*Client, []string) (*types.OperationResponse, error) {
		t.Fatal("ran after a failed part")
		return nil, nil
	}
	_, c := newResourceFake(t)
	res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a"}, MLflow: true, Components: []string{"aipipelines"}}, nil)
	if res.Success || res.Message != "MLflow is still being deleted. Deleted before it stopped: the pipeline server in 'a'. Not done: the DataScienceCluster components this tool turned on." {
		t.Fatalf("got %+v", res)
	}
}

// F4: a full cleanup can hit several recipe projects. Validation runs per project
// ahead of any deletion; confirmation is not shared across namespaces (confirming a
// workbench in one namespace does not cover the same name elsewhere). Projects that
// still lack confirmation surface as distinct steps so the UI can ask once per project.
func TestCleanUp_SeveralRecipeProjects(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	holds := map[string][]string{"a": {"Notebook wb"}, "b": {"Notebook wb", "PersistentVolumeClaim data"}}
	fakeRecipeProjectTeardown(t, ran, holds)
	f, c := newResourceFake(t)
	var events []UpdateStepEvent
	req := types.CleanupRequest{MLflow: true, RecipeProjects: []string{"a", "b"}, ConfirmProjectContents: map[string][]string{"a": {"Notebook wb"}}}
	res, err := CleanUp(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res.Success || res.ErrorCode != "confirm_project_contents" || !strings.Contains(res.Message, "Nothing was deleted") {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(*ran) != 0 || len(f.mutations()) != 0 {
		t.Fatalf("ran %v, changed %v", *ran, f.mutations())
	}
	// Each project appears complete in the event lists—including a, once fully
	// confirmed—and the browser overwrites prior confirmations from those lists.
	if len(events) != 2 || events[0].Step != "recipe-project:a" || events[0].ErrorCode != "confirm_project_contents" || events[0].Detail != "Notebook wb" ||
		events[1].Step != "recipe-project:b" || events[1].Detail != "Notebook wb\nPersistentVolumeClaim data" {
		t.Fatalf("events %+v", events)
	}
	if strings.Contains(res.Message, "Project a") {
		t.Errorf("the refusal names a, whose contents were confirmed: %q", res.Message)
	}

	// With both projects confirmed, teardown order is: in-project resources,
	// per-project confirmation stages, then component cleanup.
	events = nil
	req.ConfirmProjectContents["b"] = []string{"Notebook wb", "PersistentVolumeClaim data"}
	req.Components = []string{"aipipelines"}
	res, err = CleanUp(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	want := "mlflow recipe-project:a:Notebook wb recipe-project:b:Notebook wb|PersistentVolumeClaim data components"
	if got := strings.Join(*ran, " "); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
	if !slices.ContainsFunc(events, func(e UpdateStepEvent) bool { return e.Step == "recipe-project:a" && e.Status == "success" }) {
		t.Fatalf("events %+v", events)
	}
}
