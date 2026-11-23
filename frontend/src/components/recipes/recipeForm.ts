import type { CleanupRequest } from "../../services/api";
import type { EnvironmentRecipe, RecipeRun, RecipeRunStep, ResourceState, ResourcesStatus } from "../../types";
import { isTerminating } from "../environment/resourceRow";

/** apiVersion string produced here for pkg/cluster internal/recipe. */
export const RECIPE_API_VERSION = "rhoai-nightly-updater/v1alpha1";
export const RECIPE_KIND = "EnvironmentRecipe";

/** Required catalog origin for digest-pinned recipe images (internal/recipe fbcImage). */
export const RECIPE_IMAGE_PREFIX = "quay.io/rhoai/rhoai-fbc-fragment";

/** DNS-1123 label pattern used when validating Kubernetes namespace names on the server. */
const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const DIGEST_IMAGE = /^quay\.io\/rhoai\/rhoai-fbc-fragment(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$/;

export type BuildChoice = "latest" | "tag" | "image";
export type ProjectChoice = "create" | "existing" | "none";

/** In-editor representation of the recipe MVP (environment recipes phase 1). */
export interface RecipeForm {
  name: string;
  build: BuildChoice;
  tag: string;
  image: string;
  /** keep preserves the current RHOAI install; update aligns it with the chosen build. */
  ifInstalled: "keep" | "update";
  createDSC: boolean;
  project: ProjectChoice;
  /** Identifier assigned when creating a new project. */
  newProject: string;
  displayName: string;
  /** Selected existing data science project. */
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

/** Map a stored recipe into editor fields; UI-hidden values remain at defaults. */
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

/** Serialize the form into the EnvironmentRecipe shape the backend expects. */
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

/** Invalid fields keyed to their client-side message; server validation matches. */
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

/** Blocking reason for enabling Pipelines, or null when allowed. */
export function pipelinesReason(f: RecipeForm): string | null {
  return f.project === "none" ? "Pipelines needs a project: choose one under Project." : null;
}

/** Reason the recipe must have a project, or null when omission is fine. */
export function noProjectReason(f: RecipeForm): string | null {
  return f.pipelines ? "Pipelines needs a project. Turn Pipelines off to run without one." : null;
}

/** Seconds rendered as phrases like "under a minute", "7 minutes", or "1 hour 5 minutes". */
export function formatDuration(seconds: number): string {
  if (seconds < 60) return "under a minute";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} ${minutes === 1 ? "minute" : "minutes"}`;
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  return `${h} ${h === 1 ? "hour" : "hours"}${m ? ` ${m} ${m === 1 ? "minute" : "minutes"}` : ""}`;
}

/** Full sentence comparing typical versus maximum run duration. */
export function runTakes(typicalSeconds: number, maxSeconds: number): string {
  return `The run usually takes ${formatDuration(typicalSeconds)}, at most ${formatDuration(maxSeconds)}.`;
}

/** Pending-step label: capitalized variant of usually(). */
export function usuallySentence(typicalSeconds: number, maxSeconds: number): string {
  const u = usually(typicalSeconds, maxSeconds);
  return `${u[0].toUpperCase()}${u.slice(1)}.`;
}

/** Inline phrase: typical duration and upper bound without leading capitalization. */
export function usually(typicalSeconds: number, maxSeconds: number): string {
  return `usually ${formatDuration(typicalSeconds)}, up to ${formatDuration(maxSeconds)}`;
}

/** Wall-clock display for elapsed ms: m:ss below one hour, otherwise h:mm:ss. */
export function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}

/** Resume target: earliest step neither successful nor skipped; null if the run finished cleanly. */
export function resumeStep(run: RecipeRun): RecipeRunStep | null {
  return run.steps.find((s) => s.status !== "success" && s.status !== "skipped") ?? null;
}

/** Selectable cleanup slice for a recipe run. */
export interface RecipeCleanupPart {
  /** pkg/cluster CleanupStepID from the cleanup stream, or "components". */
  id: string;
  title: string;
}

/**
 * Ordered cleanup options for a run: entries named in the backend `ready` request,
 * limited to run-owned assets—the recipe-created project (pipeline server included)
 * and DataScienceCluster components the run enabled. Shared S3 storage, MLflow, and
 * pipeline servers outside recipe-created projects are omitted (see leftInPlace).
 */
export function cleanupParts(ready: CleanupRequest | undefined): RecipeCleanupPart[] {
  const parts: RecipeCleanupPart[] = [];
  for (const p of ready?.recipeProjects ?? []) parts.push({ id: `recipe-project:${p}`, title: `Project ${p}, which the recipe created` });
  if (ready?.components?.length) parts.push({ id: "components", title: `The DataScienceCluster components the run turned on: ${ready.components.join(", ")}` });
  return parts;
}

/**
 * Build the cleanup payload for user-selected parts: start from backend `ready`,
 * drop unchecked items, and attach confirmed project contents after review.
 */
export function cleanupRequest(ready: CleanupRequest | undefined, chosen: ReadonlySet<string>, confirmed: Record<string, string[]> = {}): CleanupRequest {
  const req: CleanupRequest = {};
  const projects = (ready?.recipeProjects ?? []).filter((p) => chosen.has(`recipe-project:${p}`));
  if (projects.length > 0) {
    req.recipeProjects = projects;
    // Project UIDs captured during review; without them the backend rejects the entry.
    req.recipeProjectUIDs = Object.fromEntries(projects.map((p) => [p, ready?.recipeProjectUIDs?.[p] ?? ""]));
    const confirm = Object.fromEntries(projects.filter((p) => (confirmed[p] ?? []).length > 0).map((p) => [p, confirmed[p]]));
    if (Object.keys(confirm).length > 0) req.confirmProjectContents = confirm;
  }
  if (ready?.components?.length && chosen.has("components")) req.components = ready.components;
  return req;
}

/**
 * Resources the run provisioned but this cleanup keeps, per current cluster
 * status (GET /api/resources/status): cluster-wide S3 and MLflow, plus the
 * pipeline server in the run project when that project is not part of the delete request.
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