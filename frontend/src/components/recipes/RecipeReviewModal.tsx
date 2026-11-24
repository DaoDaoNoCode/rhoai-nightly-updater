import React from "react";
import { Alert, Content, List, ListItem, Skeleton, Stack, StackItem } from "@patternfly/react-core";
import { Link } from "react-router-dom";
import { NAV_ITEMS } from "../../constants";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { StepsPreview } from "../StepsPreview";
import type { PipelineStepDef } from "../../operationSteps";
import type { ApiError } from "../../services/api";
import type { RecipeFix, RecipePlan, RecipePlanStep, RecipeProblem } from "../../types";
import { shortDigest } from "../../build";
import { runTakes, usually } from "./recipeForm";

/** The problems a refusal carries (pkg/api writeRecipeRefusal), and its plan. */
export function refusalOf(e: ApiError | null): { problems: RecipeProblem[]; plan: RecipePlan | null } {
  const details = (e?.details ?? {}) as { problems?: RecipeProblem[] | null; plan?: RecipePlan | null };
  return { problems: Array.isArray(details.problems) ? details.problems : [], plan: details.plan ?? null };
}

/** The editor's names for the recipe fields a problem can name. */
const FIELD_NAMES: Record<string, string> = {
  "metadata.name": "Recipe name",
  "spec.rhoai.build": "Build",
  "spec.rhoai.build.tag": "Nightly line",
  "spec.rhoai.build.image": "Catalog image",
  "spec.project.name": "Project name",
  "spec.project.displayName": "Display name",
  "spec.features.models.model": "Model",
  "spec.features.models.exposure": "How it is reached",
  "spec.features.maas.database": "Database",
  "spec.dashboardDev.pr": "Pull request number",
};

/** The page names of the app's routes a fix can name ("/environment" is the Environment page's own route). */
const PAGE_NAMES: Record<string, string> = {
  ...Object.fromEntries(NAV_ITEMS.map((n) => [n.path, n.label])),
  "/environment": "Environment",
};

/** "Open Status": a link to the page that fixes a problem; nothing when no page of the app does (or it is one this UI does not know). */
export const RecipeFixLink: React.FC<{ fix?: RecipeFix }> = ({ fix }) => {
  const name = fix?.page ? PAGE_NAMES[fix.page] : undefined;
  return name && fix?.page ? <> <Link to={fix.page}>Open {name}</Link></> : null;
};

function stepDef(step: RecipePlanStep): PipelineStepDef {
  const time = usually(step.typicalSeconds, step.maxSeconds);
  switch (step.action) {
    case "skip":
      return { id: step.id, label: step.title, variant: "success", description: `Already set up${step.reason ? `: ${step.reason}` : "."}` };
    case "blocked":
      return { id: step.id, label: step.title, variant: "danger", description: <>Blocked: {step.reason ?? "the run would stop here."}<RecipeFixLink fix={step.fix} /></> };
    case "later":
      return { id: step.id, label: step.title, description: `Checked when the run gets there; if it runs, ${time}.` };
    default:
      return { id: step.id, label: step.title, description: `Runs: ${time}.` };
  }
}

/** The refusal's problems, each with the field it is about. */
export const RecipeProblems: React.FC<{ problems: RecipeProblem[]; title?: string }> = ({ problems, title = "The recipe cannot run on this cluster now. Nothing was changed." }) => (
  <Alert variant="danger" isInline component="p" title={title}>
    <List>
      {problems.map((p, i) => (
        <ListItem key={i}>{p.field && FIELD_NAMES[p.field] ? `${FIELD_NAMES[p.field]}: ` : ""}{p.message}<RecipeFixLink fix={p.fix} /></ListItem>
      ))}
    </List>
  </Alert>
);

/**
 * Review and run: what the run would do (POST /api/recipes/plan), step by
 * step (Run, Already set up, Blocked with the reason), how long it usually
 * takes, and that teammates cannot change the cluster until it ends. Run
 * stays off while the plan has problems.
 */
export const RecipeReviewModal: React.FC<{
  isOpen: boolean;
  name: string;
  plan: RecipePlan | null;
  /** Planning failed (a recipe the backend refuses: 400 validation with its problems). */
  planError: ApiError | null;
  /** Starting the run failed. */
  runError: ApiError | null;
  starting: boolean;
  onRun: () => void;
  onCancel: () => void;
}> = ({ isOpen, name, plan, planError, runError, starting, onRun, onCancel }) => {
  const refusal = refusalOf(runError);
  const shownPlan = refusal.plan ?? plan;
  const planProblems = refusalOf(planError).problems;
  const problems = refusal.problems.length > 0 ? refusal.problems : planProblems.length > 0 ? planProblems : shownPlan?.problems ?? [];
  const runs = shownPlan?.steps.filter((s) => s.action === "run" || s.action === "later") ?? [];
  const installs = shownPlan?.pinned && shownPlan.steps.some((s) => s.id === "rhoai" && s.action === "run") ? shownPlan.pinned : null;
  const otherError = (runError && refusal.problems.length === 0 ? runError : null) ?? (planError && planProblems.length === 0 ? planError : null);
  return (
    <ConfirmActionModal
      isOpen={isOpen}
      title={`Run recipe ${name}?`}
      changes={[]}
      summary={!shownPlan ? (planError ? <Content component="p">Nothing was changed.</Content> : <Skeleton height="6rem" screenreaderText="Checking what the run would do" />) : (
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              {runs.length === 0
                ? "Everything in this recipe is set up already: the run checks each step and changes nothing."
                : <>{runTakes(shownPlan.typicalSeconds, shownPlan.maxSeconds)}
                  {installs && <> It installs {installs.tag ? `${installs.tag} ` : ""}build <code>{shortDigest(installs.digest)}</code>, and keeps it if you resume.</>}</>}
            </Content>
          </StackItem>
          <StackItem>
            <Content component="p">
              The steps run on the updater: you can leave this page and come back. Teammates cannot change this cluster until the run ends.
            </Content>
          </StackItem>
        </Stack>
      )}
      confirmLabel="Run recipe"
      confirmDisabled={!shownPlan || problems.length > 0 || !!planError}
      isLoading={starting}
      onConfirm={onRun}
      onCancel={onCancel}
    >
      {(problems.length > 0 || otherError || shownPlan) && (
        <Stack hasGutter>
          {problems.length > 0 && <StackItem><RecipeProblems problems={problems} /></StackItem>}
          {otherError && (
            <StackItem>
              <Alert variant={otherError.errorCode === "cluster_busy" ? "warning" : "danger"} isInline component="p"
                title={otherError.errorCode === "cluster_busy" ? "Another operation is changing this cluster" : runError ? "The run did not start" : "The recipe could not be checked"}>
                {otherError.message}
              </Alert>
            </StackItem>
          )}
          {shownPlan && (
            <StackItem>
              <StepsPreview idPrefix="recipe-plan" defaultExpanded steps={shownPlan.steps.map(stepDef)} />
            </StackItem>
          )}
        </Stack>
      )}
    </ConfirmActionModal>
  );
};
