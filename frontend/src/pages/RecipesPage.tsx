import React, { useEffect, useState } from "react";
import { PageSection, Stack, StackItem, Title } from "@patternfly/react-core";
import { PageHeader } from "../components/PageHeader";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { RecipeEditor } from "../components/recipes/RecipeEditor";
import { RecipeReviewModal } from "../components/recipes/RecipeReviewModal";
import { RecipeRunCard } from "../components/recipes/RecipeRunCard";
import { RecipeCleanupModal } from "../components/recipes/RecipeCleanupModal";
import { useRecipeRun } from "../components/recipes/useRecipeRun";
import { DEFAULT_RECIPE_FORM, cleanupParts, formFromRecipe, recipeFromForm, type RecipeForm } from "../components/recipes/recipeForm";
import { planRecipe, runRecipe, toApiError, type ApiError } from "../services/api";
import { useClusterBusyHandler, useMutationBlocker, usePermissions } from "../state/AppInfo";
import { useOperation } from "../state/AppState";
import type { RecipePlan } from "../types";

/**
 * Recipes (cluster-admins, full profile): set up this cluster in one run,
 * an RHOAI nightly, the DataScienceCluster, a project and the Environment
 * features the recipe turns on. The run happens on the updater, which holds
 * the cluster lock until it ends; this page follows it (GET
 * /api/recipes/run), and can stop it after a step, resume it, and clean up
 * what it set up.
 */
export const RecipesPage: React.FC = () => {
  useEffect(() => {
    document.title = "Recipes — RHOAI Nightly Updater";
  }, []);
  const { status, error, loading, refresh } = useRecipeRun();
  const blocker = useMutationBlocker();
  const { reason: permissionReason } = usePermissions();
  const onClusterBusy = useClusterBusyHandler();
  const { refreshServerOperation } = useOperation();

  const [form, setForm] = useState<RecipeForm | null>(null);
  const run = status?.run ?? null;
  // The editor starts from the last run's recipe, once it is known.
  const shownForm = form ?? (run ? formFromRecipe(run.recipe) : DEFAULT_RECIPE_FORM);

  const [reviewOpen, setReviewOpen] = useState(false);
  const [plan, setPlan] = useState<RecipePlan | null>(null);
  const [planError, setPlanError] = useState<ApiError | null>(null);
  const [planning, setPlanning] = useState(false);
  const [starting, setStarting] = useState(false);
  const [runError, setRunError] = useState<ApiError | null>(null);
  const [cleanupOpen, setCleanupOpen] = useState(false);

  const review = async () => {
    setReviewOpen(true);
    setPlan(null);
    setPlanError(null);
    setRunError(null);
    setPlanning(true);
    try {
      setPlan(await planRecipe(recipeFromForm(shownForm)));
    } catch (e) {
      const err = toApiError(e);
      onClusterBusy(err);
      setPlanError(err);
    } finally {
      setPlanning(false);
    }
  };

  const start = async () => {
    setStarting(true);
    setRunError(null);
    try {
      await runRecipe(recipeFromForm(shownForm));
      setReviewOpen(false);
      setForm(null);
      // The run holds the cluster lock: the notices and every page's buttons follow at once.
      refreshServerOperation();
      await refresh();
    } catch (e) {
      const err = toApiError(e);
      onClusterBusy(err);
      setRunError(err);
    } finally {
      setStarting(false);
    }
  };

  const changed = () => {
    refreshServerOperation();
    void refresh();
  };

  const running = !!status?.running;
  const hasCleanup = !!run && !running && cleanupParts(status?.cleanup).length > 0;

  return (
    <>
      <PageHeader
        title="Recipes"
        description="Set up this cluster in one run: an RHOAI nightly, the DataScienceCluster, a project and the Environment features you choose."
      />
      {loading && !status && <PageLoading title="Loading the last recipe run" />}
      {!status && error && <PageErrorState error={error} title="Can't load the last recipe run" onRetry={() => void refresh()} />}
      {status && (
        <>
          {error && (
            <PageSection>
              <LoadErrorAlert error={error} genericTitle="Could not refresh the recipe run" onRetry={() => void refresh()} stale />
            </PageSection>
          )}
          {run && (
            <PageSection aria-label="Recipe run">
              <RecipeRunCard status={status} run={run} blocker={blocker} permissionReason={permissionReason} onChanged={changed}
                onClusterBusy={onClusterBusy} onCleanup={hasCleanup ? () => setCleanupOpen(true) : null} />
            </PageSection>
          )}
          {!running && (
            <PageSection aria-labelledby="recipe-editor-title">
              <Stack hasGutter>
                <StackItem>
                  <Title headingLevel="h2" size="xl" id="recipe-editor-title">{run ? "Run a recipe again" : "Run a recipe"}</Title>
                </StackItem>
                <StackItem>
                  <RecipeEditor form={shownForm} onChange={setForm} onReview={() => void review()} blocker={blocker} reviewing={planning} />
                </StackItem>
              </Stack>
            </PageSection>
          )}
        </>
      )}
      <RecipeReviewModal
        isOpen={reviewOpen}
        name={shownForm.name}
        plan={plan}
        planError={planError}
        runError={runError}
        starting={starting}
        onRun={() => void start()}
        onCancel={() => setReviewOpen(false)}
      />
      {run && cleanupOpen && (
        <RecipeCleanupModal run={run} ready={status?.cleanup} isOpen blocker={blocker} onClose={() => setCleanupOpen(false)} onDone={changed} onClusterBusy={onClusterBusy} />
      )}
    </>
  );
};
