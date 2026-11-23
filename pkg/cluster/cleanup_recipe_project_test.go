package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// recreateRecipeProject removes namespace recipe-dev so a recipe can
// provision it from scratch. State afterward reflects the replacement
// namespace UID; that value is what the helper returns.
func recreateRecipeProject(t *testing.T, f *resourceFake, c *Client) string {
	t.Helper()
	f.deleteObject(tRecipeNS)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	if res := ensureProject(t, c, s); !res.Success {
		t.Fatalf("create again: %+v", res)
	}
	return s.Project.UID
}

// createdRecipeProject stands up recipe-dev through a recipe and yields the fake, client, and UID.
func createdRecipeProject(t *testing.T) (*resourceFake, *Client, string) {
	t.Helper()
	f, c := projectFake(t)
	s := &recipe.State{Recipe: tRecipe(t, "create")}
	if res := ensureProject(t, c, s); !res.Success {
		t.Fatalf("ensure %+v", res)
	}
	return f, c, s.Project.UID
}

// Regression under test: user keeps a confirmation dialog open, recipe-dev
// gets torn down, then another recipe recreates it. Stored state and the live
// namespace share the fresh UID—reviewed UID is the only distinction. Cleanup
// must abort before any step executes.
func TestCleanUp_RecipeProjectMadeAgainSinceReviewedRefusesBeforeAnything(t *testing.T) {
	f, c, reviewed := createdRecipeProject(t)
	now := recreateRecipeProject(t, f, c)
	if now == reviewed {
		t.Fatal("the project was not made again")
	}
	before := len(f.mutations())
	var events []UpdateStepEvent
	res, err := CleanUp(c, types.CleanupRequest{
		MLflow: true, PipelineServers: []string{"recipe-dev"}, Storage: true,
		RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": reviewed},
	}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "not the one you reviewed") || !strings.Contains(res.Message, "Nothing was deleted") {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := f.mutations()[before:]; len(got) != 0 {
		t.Fatalf("changed the cluster: %v", got)
	}
	if len(events) != 1 || events[0].Step != "recipe-project:recipe-dev" || events[0].Status != "failed" {
		t.Fatalf("a part ran: %+v", events)
	}
	if !f.has(tRecipeNS) {
		t.Fatal("the project made again was deleted")
	}
}

// Recreation happens after cleanup's initial validation but while an earlier
// step is in flight: the recipe-project step re-validates before DELETE and
// leaves the recreated project intact.
func TestCleanUp_RecipeProjectMadeAgainDuringTheCleanUpIsKept(t *testing.T) {
	f, c, reviewed := createdRecipeProject(t)
	ml := cleanupMLflow
	t.Cleanup(func() { cleanupMLflow = ml })
	cleanupMLflow = func(c *Client) (*types.OperationResponse, error) {
		recreateRecipeProject(t, f, c)
		return &types.OperationResponse{Success: true, Message: "MLflow removed."}, nil
	}
	res, err := CleanUp(c, types.CleanupRequest{MLflow: true, RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": reviewed}}, nil)
	if err != nil || res.Success || !strings.Contains(res.Message, "not the one you reviewed") {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(f.callsTo("DELETE", tRecipeNS)) != 0 || !f.has(tRecipeNS) {
		t.Fatal("the project made again was deleted")
	}
}

// When the namespace is swapped after the step validates but before DELETE
// runs, DeleteProject must send the reviewed UID as a delete precondition (409).
func TestRecipeProject_DeleteUsesTheReviewedUIDAsPrecondition(t *testing.T) {
	f, c, reviewed := createdRecipeProject(t)
	f.beforeServe = func(method, path string) {
		if method == "DELETE" && path == tRecipeNS {
			f.putJSON(tRecipeNS, `{"metadata":{"uid":"swapped","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"rhoai-nightly-updater/recipe":"dev"}}}`)
		}
	}
	res, _ := recipe.DeleteProject(c, "recipe-dev", reviewed, nil)
	dels := f.callsTo("DELETE", tRecipeNS)
	if res.Success || len(dels) != 1 || !strings.Contains(dels[0].Body, `"uid":"`+reviewed+`"`) || !f.has(tRecipeNS) {
		t.Fatalf("got %+v, deletes %+v", res, dels)
	}
}

// Missing or mismatched reviewed UID blocks deletion; matching the live
// project's UID allows teardown to proceed.
func TestRecipeProject_TeardownNeedsTheReviewedUID(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	for _, tc := range []struct{ name, uid, want string }{
		{"no UID", "", "which project recipe-dev you reviewed"},
		{"another UID", "an-older-uid", "not the one you reviewed"},
	} {
		problem, _, _, err := recipe.ProjectTeardownProblem(c, "recipe-dev", tc.uid, nil)
		if err != nil || !strings.Contains(problem, tc.want) {
			t.Errorf("%s: %q, %v", tc.name, problem, err)
		}
		if res, _ := recipe.DeleteProject(c, "recipe-dev", tc.uid, nil); res.Success || len(f.callsTo("DELETE", tRecipeNS)) != 0 {
			t.Fatalf("%s: %+v", tc.name, res)
		}
	}
	if res, _ := recipe.DeleteProject(c, "recipe-dev", uid, nil); !res.Success || f.has(tRecipeNS) {
		t.Fatalf("the reviewed project: %+v", res)
	}
}

// Bug from review: user confirms items from one refusal, another object lands
// in the project, and the following refusal must enumerate the full set again—
// the UI should replace prior confirmation with that list, not append only the
// newcomer—or the retry drops earlier confirmations and hits refusal again.
// After confirming the complete list, cleanup removes the project.
func TestCleanUp_ConsecutiveRefusalsListEverythingInTheProject(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	f.putJSON("/apis/kubeflow.org/v1/namespaces/recipe-dev/notebooks/wb", `{}`)
	req := types.CleanupRequest{RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": uid}}
	refusal := func() UpdateStepEvent {
		t.Helper()
		var events []UpdateStepEvent
		res, err := CleanUp(c, req, func(e UpdateStepEvent) { events = append(events, e) })
		if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || len(events) != 1 {
			t.Fatalf("got %+v, %v, events %+v", res, err, events)
		}
		return events[0]
	}
	if e := refusal(); e.Detail != "Notebook wb" {
		t.Fatalf("first refusal %+v", e)
	}
	// User has confirmed; a PVC shows up in parallel.
	req.ConfirmProjectContents = map[string][]string{"recipe-dev": {"Notebook wb"}}
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/data", `{}`)
	e := refusal()
	if e.Detail != "Notebook wb\nPersistentVolumeClaim data" {
		t.Fatalf("second refusal lists %q, want the confirmed notebook too", e.Detail)
	}
	if strings.Contains(e.Message, "Notebook wb") || !strings.Contains(e.Message, "PersistentVolumeClaim data") {
		t.Errorf("the message names what is left to confirm: %q", e.Message)
	}
	req.ConfirmProjectContents = map[string][]string{"recipe-dev": strings.Split(e.Detail, "\n")}
	res, err := CleanUp(c, req, nil)
	if err != nil || !res.Success || f.has(tRecipeNS) {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// Scenario: projects a and b already confirmed; only b picks up a new object.
// Refusal must still show complete inventories for both—even when a is fully
// confirmed—so confirming that listing preserves a and lets cleanup continue.
func TestCleanUp_RefusalListsAFullyConfirmedProjectToo(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	holds := map[string][]string{"a": {"Notebook wb"}, "b": {"Pod p"}}
	fakeRecipeProjectTeardown(t, ran, holds)
	f, c := newResourceFake(t)
	req := types.CleanupRequest{RecipeProjects: []string{"a", "b"}, RecipeProjectUIDs: map[string]string{"a": "ua", "b": "ub"},
		ConfirmProjectContents: map[string][]string{"a": {"Notebook wb"}, "b": {"Pod p"}}}
	holds["b"] = []string{"PersistentVolumeClaim data", "Pod p"}
	var events []UpdateStepEvent
	res, err := CleanUp(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || len(*ran) != 0 || len(f.mutations()) != 0 {
		t.Fatalf("got %+v, %v, ran %v", res, err, *ran)
	}
	lists := map[string][]string{}
	for _, e := range events {
		if e.Status != "failed" || e.ErrorCode != recipe.ErrorCodeConfirmContents {
			t.Fatalf("event %+v", e)
		}
		lists[strings.TrimPrefix(e.Step, "recipe-project:")] = strings.Split(e.Detail, "\n")
	}
	if strings.Join(lists["a"], ",") != "Notebook wb" || strings.Join(lists["b"], ",") != "PersistentVolumeClaim data,Pod p" {
		t.Fatalf("lists %v", lists)
	}
	// Frontend overwrites ConfirmProjectContents with the refusal detail lists.
	req.ConfirmProjectContents = lists
	res, err = CleanUp(c, req, nil)
	if err != nil || !res.Success {
		t.Fatalf("retry: %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "recipe-project:a@ua:Notebook wb recipe-project:b@ub:PersistentVolumeClaim data|Pod p" {
		t.Fatalf("ran %s", got)
	}
}
