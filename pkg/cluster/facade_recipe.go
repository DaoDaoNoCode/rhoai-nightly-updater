package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Environment recipes (internal/recipe), under the names pkg/api uses.
type (
	// RecipeStep is recipe.Step.
	RecipeStep = recipe.Step
	// RecipeState is recipe.State.
	RecipeState = recipe.State
	// RecipeCheck is recipe.Check.
	RecipeCheck = recipe.Check
)

// RecipeCreatedKueueSetting is recipe.CreatedKueueSetting.
const RecipeCreatedKueueSetting = recipe.CreatedKueueSetting

// ErrorCodeConfirmProjectContents is recipe.ErrorCodeConfirmContents.
const ErrorCodeConfirmProjectContents = recipe.ErrorCodeConfirmContents

// ValidateRecipe is recipe.Validate.
func ValidateRecipe(source []byte) (types.EnvironmentRecipe, []types.RecipeProblem) {
	return recipe.Validate(source)
}

// PlanRecipe is recipe.Plan.
func PlanRecipe(c *Client, r types.EnvironmentRecipe, pinned *types.RecipePinnedBuild) (*types.RecipePlan, RecipeState) {
	return recipe.Plan(c, r, pinned)
}

// RecipeSteps is recipe.Steps for the run order the plan chose (state is
// the plan's).
func RecipeSteps(state RecipeState) []RecipeStep {
	return recipe.Steps(state.Recipe, state.KueueFirst)
}

// RecipeExposureKept is recipe.ExposureKept.
func RecipeExposureKept(exposure, gatewayRef string) bool {
	return recipe.ExposureKept(exposure, gatewayRef)
}

// RecordRecipeActivity is recipe.RecordActivity.
func RecordRecipeActivity(c *Client, action, detail string, success bool) {
	recipe.RecordActivity(c, action, detail, success)
}

// RecipeOwnership is recipe.Ownership.
func RecipeOwnership(sa, u *Client, run *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
	return recipe.Ownership(sa, u, run)
}

// RecipeOwnedProjects is recipe.OwnedProjects.
func RecipeOwnedProjects(c *Client) ([]types.RecipeOwnedProject, error) {
	return recipe.OwnedProjects(c)
}

// ForgetGoneRecipeProjects is recipe.ForgetGoneProjects.
func ForgetGoneRecipeProjects(c *Client) ([]string, error) {
	return recipe.ForgetGoneProjects(c)
}

// RecipeMaxRunningSteps is recipe.MaxRunningSteps.
const RecipeMaxRunningSteps = recipe.MaxRunningSteps
