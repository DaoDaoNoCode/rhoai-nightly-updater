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

// RecipeSteps is recipe.Steps.
func RecipeSteps(r types.EnvironmentRecipe) []RecipeStep { return recipe.Steps(r) }

// RecordRecipeActivity is recipe.RecordActivity.
func RecordRecipeActivity(c *Client, action, detail string, success bool) {
	recipe.RecordActivity(c, action, detail, success)
}

// RecipeOwnership is recipe.Ownership.
func RecipeOwnership(c *Client, run *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
	return recipe.Ownership(c, run)
}

// RecipeOwnedProjects is recipe.OwnedProjects.
func RecipeOwnedProjects(c *Client) ([]types.RecipeOwnedProject, error) {
	return recipe.OwnedProjects(c)
}

// ForgetGoneRecipeProjects is recipe.ForgetGoneProjects.
func ForgetGoneRecipeProjects(c *Client) ([]string, error) {
	return recipe.ForgetGoneProjects(c)
}
