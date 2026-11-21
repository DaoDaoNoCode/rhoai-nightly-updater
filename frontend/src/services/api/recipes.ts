/** Environment recipes: plan, run, resume, stop and the last run (cluster-admins only; not in the gitops profile). */

import type { EnvironmentRecipe, RecipePlan, RecipeRunAccepted, RecipeRunStatus } from '../../types';
import { request } from './client';

/** The last recipe run, and whether it runs now. */
export function getRecipeRun(signal?: AbortSignal): Promise<RecipeRunStatus> {
  return request('/api/recipes/run', { signal });
}

/** What a run of the recipe would do; read-only. A recipe that is not valid fails with 400 validation (details.problems). */
export function planRecipe(recipe: EnvironmentRecipe): Promise<RecipePlan> {
  return request('/api/recipes/plan', { method: 'POST', body: JSON.stringify({ recipe }) });
}

/**
 * Starts a run: 202 with its ID. A refusal throws: 422 prerequisites
 * (details.problems and details.plan), 400 validation, 409 cluster_busy.
 */
export function runRecipe(recipe: EnvironmentRecipe): Promise<RecipeRunAccepted> {
  return request('/api/recipes/run', { method: 'POST', body: JSON.stringify({ recipe }) });
}

/** Runs the recorded run again: each step skips what is set up. reresolveBuild looks up the recipe's build again. */
export function resumeRecipe(runId: string, reresolveBuild = false): Promise<RecipeRunAccepted> {
  return request('/api/recipes/resume', { method: 'POST', body: JSON.stringify(reresolveBuild ? { runId, reresolveBuild } : { runId }) });
}

/** Stop after this step; 409 not_running when this updater process runs none. */
export function stopRecipe(): Promise<{ stopRequested: boolean }> {
  return request('/api/recipes/stop', { method: 'POST' });
}
