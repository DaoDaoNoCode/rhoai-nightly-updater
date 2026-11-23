import type { CleanupRequest } from "../../services/api";
import type { EnvironmentRecipe, RecipeRun, RecipeRunStep, ResourceState, ResourcesStatus } from "../../types";
import { isTerminating } from "../environment/resourceRow";

/** The schema version this UI writes (pkg/cluster internal/recipe APIVersion). */
export const RECIPE_API_VERSION = "rhoai-nightly-updater/v1alpha1";
export const RECIPE_KIND = "EnvironmentRecipe";

/** The catalog repository a recipe's image must be a build of, pinned by digest (internal/recipe fbcImage). */
export const RECIPE_IMAGE_PREFIX = "quay.io/rhoai/rhoai-fbc-fragment";

/** A Kubernetes namespace name (DNS-1123 label), as the backend checks it. */
const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const DIGEST_IMAGE = /^quay\.io\/rhoai\/rhoai-fbc-fragment(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$/;

export type BuildChoice = "latest" | "tag" | "image";
export type ProjectChoice = "create" | "existing" | "none";

/** What the editor holds: the MVP part of a recipe (phase 1 of environment recipes). */
export interface RecipeForm {
  name: string;
  build: BuildChoice;
  tag: string;
  image: string;
  /** keep: an installed RHOAI stays as it is; update: it is updated to the build. */
  ifInstalled: "keep" | "update";
  createDSC: boolean;
  project: ProjectChoice;
  /** The project to create. */
  newProject: string;
  displayName: string;
  /** The data science project to use. */
  existingProject: string;
  pipelines: boolean;
  mlflow: boolean;
}

export const DEFAULT_RECIPE_FORM: RecipeForm = {
  name: "dev",
  build: "latest",
  tag: "",
  image: "",
  ifInstalled: "keep",
  createDSC: true,
  project: "create",
  newProject: "recipe-dev",
  displayName: "Recipe dev",
  existingProject: "",
  pipelines: true,
  mlflow: true,
};

/** The editor's form for a recipe (the last run's), keeping what the editor does not show at its default. */
export function formFromRecipe(r: EnvironmentRecipe): RecipeForm {
  const b = r.spec.rhoai.build;
  const project = r.spec.project;
  return {
    name: r.metadata.name,
    build: b === "latest" ? "latest" : b.image ? "image" : "tag",
    tag: b !== "latest" ? b.tag ?? "" : "",
    image: b !== "latest" ? b.image ?? "" : "",
    ifInstalled: r.spec.rhoai.ifInstalled === "update" ? "update" : "keep",
    createDSC: r.spec.dataScienceCluster.create,
    project: !project ? "none" : project.mode === "existing" ? "existing" : "create",
    newProject: project?.mode === "create" ? project.name : DEFAULT_RECIPE_FORM.newProject,
    displayName: project?.mode === "create" ? project.displayName ?? "" : DEFAULT_RECIPE_FORM.displayName,
    existingProject: project?.mode === "existing" ? project.name : "",
    pipelines: r.spec.features.pipelines.enabled,
    mlflow: r.spec.features.mlflow.enabled,
  };
}

/** The recipe the form describes, as the backend reads it. */
export function recipeFromForm(f: RecipeForm): EnvironmentRecipe {
  const build = f.build === "latest" ? "latest" as const : f.build === "tag" ? { tag: f.tag } : { image: f.image.trim() };
  const project = f.project === "create"
    ? { mode: "create", name: f.newProject.trim(), ...(f.displayName.trim() ? { displayName: f.displayName.trim() } : {}) }
    : f.project === "existing" ? { mode: "existing", name: f.existingProject } : undefined;
  return {
    apiVersion: RECIPE_API_VERSION,
    kind: RECIPE_KIND,
    metadata: { name: f.name.trim() },
    spec: {
      rhoai: { build, ifInstalled: f.ifInstalled },
      dataScienceCluster: { create: f.createDSC },
      ...(project ? { project } : {}),
      features: {
        pipelines: { enabled: f.pipelines },
        mlflow: { enabled: f.mlflow },
        models: { enabled: false },
        playground: { enabled: false },
        maas: { enabled: false },
      },
      dashboardDev: { deploy: "none" },
    },
  };
}

/** The form's fields with a problem, as the field's error text. The backend checks the same again. */
export type FormProblems = Partial<Record<"name" | "tag" | "image" | "newProject" | "existingProject", string>>;

function nameProblem(name: string, what: string): string | undefined {
  const n = name.trim();
  if (!n) return `Enter ${what}.`;
  if (!DNS1123_LABEL.test(n)) return "Use at most 63 lower-case letters, digits and hyphens, starting and ending with a letter or digit.";
  return undefined;
}

export function formProblems(f: RecipeForm): FormProblems {
  const p: FormProblems = {};
  const name = nameProblem(f.name, "a name for the recipe");
  if (name) p.name = name;
  if (f.build === "tag" && !f.tag) p.tag = "Choose a nightly line.";
  if (f.build === "image" && !DIGEST_IMAGE.test(f.image.trim())) p.image = `Enter a build of ${RECIPE_IMAGE_PREFIX} pinned by digest (@sha256:...).`;
  if (f.project === "create") {
    const project = nameProblem(f.newProject, "a project name");
    if (project) p.newProject = project;
  }
  if (f.project === "existing" && !f.existingProject) p.existingProject = "Choose a project.";
  return p;
}

/** Why Pipelines cannot be turned on, or null. */
export function pipelinesReason(f: RecipeForm): string | null {
  return f.project === "none" ? "Pipelines needs a project: choose one under Project." : null;
}

/** Why the recipe cannot go without a project, or null. */
export function noProjectReason(f: RecipeForm): string | null {
  return f.pipelines ? "Pipelines needs a project. Turn Pipelines off to run without one." : null;
}

/** "under a minute", "1 minute", "7 minutes", "1 hour 5 minutes". */
export function formatDuration(seconds: number): string {
  if (seconds < 60) return "under a minute";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} ${minutes === 1 ? "minute" : "minutes"}`;
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  return `${h} ${h === 1 ? "hour" : "hours"}${m ? ` ${m} ${m === 1 ? "minute" : "minutes"}` : ""}`;
}

/** "The run usually takes 10 minutes, at most 53 minutes." */
export function runTakes(typicalSeconds: number, maxSeconds: number): string {
  return `The run usually takes ${formatDuration(typicalSeconds)}, at most ${formatDuration(maxSeconds)}.`;
}

/** "Usually 7 minutes, up to 15 minutes.": a step still to come. */
export function usuallySentence(typicalSeconds: number, maxSeconds: number): string {
  const u = usually(typicalSeconds, maxSeconds);
  return `${u[0].toUpperCase()}${u.slice(1)}.`;
}

/** "usually 7 minutes, up to 15 minutes". */
export function usually(typicalSeconds: number, maxSeconds: number): string {
  return `usually ${formatDuration(typicalSeconds)}, up to ${formatDuration(maxSeconds)}`;
}

/** Elapsed time as m:ss ("12:05"), or h:mm:ss from an hour. */
export function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}

/** The step a resume continues at: the first one that is not set up, or null when all are. */
export function resumeStep(run: RecipeRun): RecipeRunStep | null {
  return run.steps.find((s) => s.status !== "success" && s.status !== "skipped") ?? null;
}

/** One part Clean up can delete for a recipe run. */
export interface RecipeCleanupPart {
  /** The clean-up stream's step ID (pkg/cluster CleanupStepID), or "components". */
  id: string;
  title: string;
}

/**
 * What Clean up can delete for a run, in the clean-up's own order: what the
 * backend's clean-up request `ready` names, the parts that are only the
 * run's: the project the recipe created (its pipeline server goes with it)
 * and the DataScienceCluster components it turned on. The S3 storage,
 * MLflow and a pipeline server in a project the recipe did not create are
 * never offered (leftInPlace names them).
 */
export function cleanupParts(ready: CleanupRequest | undefined): RecipeCleanupPart[] {
  const parts: RecipeCleanupPart[] = [];
  for (const p of ready?.recipeProjects ?? []) parts.push({ id: `recipe-project:${p}`, title: `Project ${p}, which the recipe created` });
  if (ready?.components?.length) parts.push({ id: "components", title: `The DataScienceCluster components the run turned on: ${ready.components.join(", ")}` });
  return parts;
}

/**
 * The clean-up request for the chosen parts: the backend's request for
 * what the run created (`ready`), without the parts the user left out,
 * and with what the project holds once the user confirmed it.
 */
export function cleanupRequest(ready: CleanupRequest | undefined, chosen: ReadonlySet<string>, confirmed: Record<string, string[]> = {}): CleanupRequest {
  const req: CleanupRequest = {};
  const projects = (ready?.recipeProjects ?? []).filter((p) => chosen.has(`recipe-project:${p}`));
  if (projects.length > 0) {
    req.recipeProjects = projects;
    const confirm = Object.fromEntries(projects.filter((p) => (confirmed[p] ?? []).length > 0).map((p) => [p, confirmed[p]]));
    if (Object.keys(confirm).length > 0) req.confirmProjectContents = confirm;
  }
  if (ready?.components?.length && chosen.has("components")) req.components = ready.components;
  return req;
}

/**
 * What the run set up that this Clean up leaves in place, as the cluster
 * shows it now (`status`, GET /api/resources/status): the S3 storage and
 * MLflow, which the whole cluster shares, and the pipeline server in the
 * run's project when that project is not deleted with this request.
 */
export function leftInPlace(status: ResourcesStatus, run: RecipeRun, req: CleanupRequest): string[] {
  const up = (r: ResourceState | null | undefined) => !!r?.deployed && !isTerminating(r);
  const out: string[] = [];
  const project = run.project?.name;
  if (project && !(req.recipeProjects ?? []).includes(project) && (status.pipelineServers ?? []).some((ps) => ps.namespace === project && up(ps))) {
    out.push(`The pipeline server in ${project}`);
  }
  if (up(status.minio)) out.push("The S3 storage");
  if (up(status.mlflow)) out.push("MLflow");
  return out;
}
