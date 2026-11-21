package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Environment recipes: plan, run, resume, stop and the run's status. Every
// route is admin-only (the mutation gate; GET /api/recipes/run is tier
// admin) and refused in the gitops profile. The runner is
// recipe_runner.go.

// maxRecipeRequestBytes bounds a plan or run request: the recipe (at most
// 16 KiB as a file) as a JSON object.
const maxRecipeRequestBytes = 32 << 10

// decodeRecipe reads a request's recipe and validates it strictly
// (cluster.ValidateRecipe). It answers 400 validation with every problem
// and returns false when the handler must stop.
func decodeRecipe(w http.ResponseWriter, r *http.Request) (types.EnvironmentRecipe, bool) {
	var req types.RecipeRequest
	if !decodeBody(w, r, maxRecipeRequestBytes, &req, false) {
		return types.EnvironmentRecipe{}, false
	}
	if len(req.Recipe) == 0 {
		writeRecipeRefusal(w, http.StatusBadRequest, "validation", "The request has no recipe.", []types.RecipeProblem{{Message: "recipe is required"}}, nil)
		return types.EnvironmentRecipe{}, false
	}
	rcp, problems := cluster.ValidateRecipe(req.Recipe)
	if len(problems) > 0 {
		writeRecipeRefusal(w, http.StatusBadRequest, "validation", "The recipe is not valid. Nothing was changed.", problems, nil)
		return types.EnvironmentRecipe{}, false
	}
	return rcp, true
}

// HandleRecipePlan answers what a run of the recipe would do
// (types.RecipePlan), read-only. Like the node setup plan it takes the
// lock (what it reads must not change under it) and does not use up the
// rate-limit slot.
var HandleRecipePlan = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Skip-Rate-Limit", "true")
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	rcp, ok := decodeRecipe(w, r)
	if !ok {
		return
	}
	setOperationTarget(w, "plan of recipe "+rcp.Metadata.Name)
	plan, _ := planForRun(c, rcp, nil)
	writeJSON(w, plan, "recipe-plan")
})

// HandleRecipeRun starts a run of the recipe: 202 with its ID once it
// runs, or the refusal of the plan (422). The run holds the cluster lock
// until it ends; GET /api/recipes/run and GET /api/operation show it.
var HandleRecipeRun = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	start, ok := lockRecipeRunner(w, r)
	if !ok {
		return
	}
	rcp, ok := decodeRecipe(w, r)
	if !ok {
		start.abandon()
		return
	}
	slog.Info("mutation", "op", "recipe-run", "recipe", rcp.Metadata.Name)
	beginRecipeRun(c, w, start, rcp, nil, nil)
})

// HandleRecipeResume runs the recorded run again from the start: every
// step's check skips what is set up, so it continues where the run stopped
// (failed, stopped, or interrupted by a restart). It keeps the build the
// run pinned unless reresolveBuild asks for the recipe's build again.
var HandleRecipeResume = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	start, ok := lockRecipeRunner(w, r)
	if !ok {
		return
	}
	var req types.RecipeResumeRequest
	if !decodeBody(w, r, 1024, &req, false) {
		start.abandon()
		return
	}
	prior, err := readRecipeRun(c)
	switch {
	case err != nil:
		start.abandon()
		slog.Warn("recipe resume: the run record cannot be read", "error", err)
		writeError(w, "Cannot read the recorded recipe run: "+err.Error()+". Nothing was changed.", http.StatusServiceUnavailable, "record_unavailable")
		return
	case prior == nil:
		start.abandon()
		writeError(w, "No recipe run is recorded on this cluster. Nothing was changed.", http.StatusNotFound, "not_found")
		return
	case req.RunID == "" || prior.ID != req.RunID:
		start.abandon()
		writeError(w, "The recorded recipe run is another one than the one to resume (another run started since). Reload, then resume. Nothing was changed.", http.StatusConflict, "conflict")
		return
	}
	// The recorded recipe is checked again: the record is not trusted
	// more than a recipe a user sends.
	source, err := json.Marshal(prior.Recipe)
	if err != nil {
		start.abandon()
		writeError(w, "The recorded recipe cannot be read. Nothing was changed.", http.StatusInternalServerError, "internal")
		return
	}
	rcp, problems := cluster.ValidateRecipe(source)
	if len(problems) > 0 {
		start.abandon()
		writeRecipeRefusal(w, http.StatusUnprocessableEntity, "validation", "The recorded recipe is not valid for this updater. Nothing was changed.", problems, nil)
		return
	}
	pinned := prior.Pinned
	if req.ReresolveBuild {
		pinned = nil
	}
	slog.Info("mutation", "op", "recipe-resume", "run", prior.ID, "recipe", rcp.Metadata.Name, "reresolveBuild", req.ReresolveBuild)
	beginRecipeRun(c, w, start, rcp, prior, pinned)
})

// HandleRecipeStop asks this process's running recipe run to stop after
// its step ("Stop after this step"). It takes no lock and makes no cluster
// call: the run holds the lock, and checks the request before each step.
// Asking twice is the same as once; with no run running it answers 409
// not_running. It does not use up the rate-limit slot.
var HandleRecipeStop = withMutationAuth(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Skip-Rate-Limit", "true")
	if !recipes.requestStop() {
		writeError(w, "No recipe runs in this updater process. Nothing was changed.", http.StatusConflict, "not_running")
		return
	}
	slog.Info("mutation", "op", "recipe-stop")
	writeJSON(w, map[string]any{"stopRequested": true}, "recipe-stop")
})

// HandleRecipeRunStatus answers the last recipe run (types.RecipeRunStatus;
// tier admin): from memory while this process runs it, otherwise from the
// record, with whether another updater pod runs it or it was interrupted.
// It reads only the updater's own records, with the ServiceAccount.
var HandleRecipeRunStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, recipeRunStatus(c), "recipe-run")
})
