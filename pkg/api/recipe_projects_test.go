package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// F4: cleanup takes recipe projects as a list with confirmation per
// entry; malformed lists are rejected before any work starts.
func TestCleanupStream_RecipeProjectsValidation(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	calls := fakeCleanUp(t, func(types.CleanupRequest, func(cluster.UpdateStepEvent)) *types.OperationResponse {
		return &types.OperationResponse{Success: true}
	})
	for name, body := range map[string]string{
		"invalid recipe project":             `{"recipeProjects":["Bad_Name"]}`,
		"a recipe project twice":             `{"recipeProjects":["a","a"]}`,
		"too many recipe projects":           `{"recipeProjects":[` + strings.Repeat(`"p",`, maxCleanupProjects) + `"p"]}`,
		"confirmation of an unnamed project": `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":"ua"},"confirmProjectContents":{"b":["Notebook wb"]}}`,
		"empty confirmation":                 `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":"ua"},"confirmProjectContents":{"a":[""]}}`,
		"too many confirmations":             `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":"ua"},"confirmProjectContents":{"a":[` + strings.Repeat(`"Pod p",`, maxConfirmedContents) + `"Pod p"]}}`,
		// The UID of the project the user reviewed is required for each.
		"no UIDs":                     `{"recipeProjects":["a"],"mlflow":true}`,
		"a project without its UID":   `{"recipeProjects":["a","b"],"recipeProjectUIDs":{"a":"ua"}}`,
		"an empty UID":                `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":""}}`,
		"a UID of an unnamed project": `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":"ua","b":"ub"}}`,
		"a UID too long":              `{"recipeProjects":["a"],"recipeProjectUIDs":{"a":"` + strings.Repeat("u", maxUIDLength+1) + `"}}`,
	} {
		w := httptest.NewRecorder()
		HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:"+strings.ReplaceAll(name, " ", "-"), body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused request ran the clean-up: %+v", *calls)
	}

	w := httptest.NewRecorder()
	HandleCleanupStream(w, postJSON(t, "/api/resources/cleanup/stream", "user:ok", `{"recipeProjects":["a","b"],"recipeProjectUIDs":{"a":"ua","b":"ub"},"confirmProjectContents":{"b":["Notebook wb"]}}`))
	if w.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got := (*calls)[0]
	if strings.Join(got.RecipeProjects, ",") != "a,b" || strings.Join(got.ConfirmProjectContents["b"], ",") != "Notebook wb" || len(got.ConfirmProjectContents["a"]) != 0 ||
		got.RecipeProjectUIDs["a"] != "ua" || got.RecipeProjectUIDs["b"] != "ub" {
		t.Fatalf("request %+v", got)
	}
}

// F5: the background prune job also drops recipe projects that no longer
// exist, guarded by the same lock and lease.
func TestMaaSPruneJob_ForgetsGoneRecipeProjects(t *testing.T) {
	setupDevMode(t)
	runs := stubMaaSPrune(t)
	orig := forgetGoneRecipeProjects
	t.Cleanup(func() { forgetGoneRecipeProjects = orig })
	var locked []bool
	forgetGoneRecipeProjects = func(*cluster.Client) ([]string, error) {
		op := inflight.snapshot()
		locked = append(locked, clusterMutationInProgress.Load() && op != nil && op.Type == "prune-maas-publications")
		return []string{"recipe-dev2"}, nil
	}
	releaseOperationLease = func(*cluster.Client, cluster.LeaseOwner) error { return nil }
	if !runMaaSPruneOnce(context.Background()) {
		t.Fatal("did not run")
	}
	<-runs
	if len(locked) != 1 || !locked[0] {
		t.Fatalf("forget ran %v (true = under the lock)", locked)
	}
	// Forget errors are logged only; the prune run still releases the lock.
	forgetGoneRecipeProjects = func(*cluster.Client) ([]string, error) { return nil, errors.New("boom") }
	if !runMaaSPruneOnce(context.Background()) || clusterMutationInProgress.Load() {
		t.Fatal("did not run or kept the lock")
	}
	<-runs
}

// F4: when idle, GET /api/recipes/run reports recipe-owned projects still
// present on the cluster; if the list cannot be fetched, the response states
// the failure instead of fabricating data.
func TestRecipeRunStatus_ListsTheProjectsRecipesCreated(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubEndedRecipeRun(t)
	origOwnership, origOwned, origRecipes := recipeOwnership, recipeOwnedProjects, recipes
	recipes = &recipeRunner{}
	t.Cleanup(func() { recipeOwnership, recipeOwnedProjects, recipes = origOwnership, origOwned, origRecipes })
	recipeOwnership = func(_, _ *cluster.Client, _ *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
		return nil, nil
	}
	recipeOwnedProjects = func(*cluster.Client) ([]types.RecipeOwnedProject, error) {
		return []types.RecipeOwnedProject{{Name: "recipe-dev", UID: "u1", Recipe: "dev"}, {Name: "recipe-old", UID: "u2", Recipe: "old"}}, nil
	}
	st := recipeRunStatus(nil, nil)
	if len(st.Projects) != 2 || st.Projects[1].Name != "recipe-old" || st.ProjectsError != "" {
		t.Fatalf("status %+v", st)
	}
	recipeOwnedProjects = func(*cluster.Client) ([]types.RecipeOwnedProject, error) {
		return nil, errors.New("namespaces unreadable")
	}
	st = recipeRunStatus(nil, nil)
	if len(st.Projects) != 0 || !strings.Contains(st.ProjectsError, "namespaces unreadable") {
		t.Fatalf("status %+v", st)
	}
}
