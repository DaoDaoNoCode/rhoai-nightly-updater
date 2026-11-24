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
/** How the model is reached; "" is the one the Models tab recommends when the run gets there. */
export type ExposureChoice = "" | "default-gateway" | "maas" | "internal";
export type MaaSDatabaseChoice = "development" | "cnpg";
export type DashboardDevChoice = "none" | "main" | "pr";

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
  /** Add the MLflow samples to the project (needs MLflow and a project). */
  mlflowSamples: boolean;
  models: boolean;
  /** A model of the tool's catalog; "" is the catalog's default. */
  model: string;
  exposure: ExposureChoice;
  /** Kept from an imported or earlier recipe; the editor does not show it. */
  releasedImage: boolean;
  playground: boolean;
  maas: boolean;
  maasDatabase: MaaSDatabaseChoice;
  /** Publish the model through MaaS (needs Models). */
  maasPublish: boolean;
  modelRegistry: boolean;
  /** Register the sample model in the registry. */
  registrySample: boolean;
  workbench: boolean;
  dashboardDev: DashboardDevChoice;
  /** The odh-dashboard pull request, as typed. */
  dashboardPR: string;
  /** Kept from an imported or earlier recipe; the editor does not show it. */
  dashboardFlavor: string;
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
  mlflowSamples: false,
  models: false,
  model: "",
  exposure: "",
  releasedImage: false,
  playground: false,
  maas: false,
  maasDatabase: "development",
  maasPublish: true,
  modelRegistry: false,
  registrySample: false,
  workbench: false,
  dashboardDev: "none",
  dashboardPR: "",
  dashboardFlavor: "rhoai",
};

/** Map a stored recipe into editor fields; UI-hidden values remain at defaults. */
export function formFromRecipe(r: EnvironmentRecipe): RecipeForm {
  const b = r.spec.rhoai.build;
  const project = r.spec.project;
  const f = r.spec.features;
  const d = r.spec.dashboardDev;
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
    pipelines: f.pipelines.enabled,
    mlflow: f.mlflow.enabled,
    mlflowSamples: !!f.mlflow.samples,
    models: f.models.enabled,
    model: f.models.model ?? "",
    exposure: (["default-gateway", "maas", "internal"].includes(f.models.exposure ?? "") ? f.models.exposure : "") as ExposureChoice,
    releasedImage: !!f.models.releasedImage,
    playground: f.playground.enabled,
    maas: f.maas.enabled,
    maasDatabase: f.maas.database === "cnpg" ? "cnpg" : "development",
    maasPublish: f.maas.enabled ? !!f.maas.publish : DEFAULT_RECIPE_FORM.maasPublish,
    modelRegistry: !!f.modelRegistry?.enabled,
    registrySample: !!f.modelRegistry?.sample,
    workbench: !!f.workbench?.enabled,
    dashboardDev: d.deploy === "main" || d.deploy === "pr" ? d.deploy : "none",
    dashboardPR: d.deploy === "pr" && d.pr ? String(d.pr) : "",
    dashboardFlavor: d.flavor || "rhoai",
  };
}

/** Serialize the form into the EnvironmentRecipe shape the backend expects. */
export function recipeFromForm(f: RecipeForm): EnvironmentRecipe {
  const e = effectiveForm(f);
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
        pipelines: { enabled: e.pipelines },
        mlflow: { enabled: e.mlflow, ...(e.mlflowSamples ? { samples: true } : {}) },
        models: e.models
          ? { enabled: true, ...(e.model ? { model: e.model } : {}), ...(e.exposure ? { exposure: e.exposure } : {}), ...(e.releasedImage ? { releasedImage: true } : {}) }
          : { enabled: false },
        playground: { enabled: e.playground },
        maas: e.maas ? { enabled: true, database: e.maasDatabase, ...(e.maasPublish ? { publish: true } : {}) } : { enabled: false },
        ...(e.modelRegistry ? { modelRegistry: { enabled: true, ...(e.registrySample ? { sample: true } : {}) } } : {}),
        ...(e.workbench ? { workbench: { enabled: true } } : {}),
      },
      dashboardDev: e.dashboardDev === "none" ? { deploy: "none" }
        : { deploy: e.dashboardDev, ...(e.dashboardDev === "pr" ? { pr: Number(e.dashboardPR.trim()) } : {}), flavor: e.dashboardFlavor },
    },
  };
}

/**
 * The form as it is sent: a choice another one rules out counts as off
 * (the editor shows it off, with the reason), so the recipe never asks for
 * what its own validation refuses.
 */
export function effectiveForm(f: RecipeForm): RecipeForm {
  const project = f.project !== "none";
  const models = f.models && project;
  return {
    ...f,
    pipelines: f.pipelines && project,
    mlflowSamples: f.mlflowSamples && f.mlflow && project,
    models,
    exposure: f.exposure === "maas" && !f.maas ? "" : f.exposure,
    playground: f.playground && models,
    maasPublish: f.maasPublish && f.maas && models && f.exposure !== "maas",
    registrySample: f.registrySample && f.modelRegistry,
    workbench: f.workbench && project,
  };
}

/** Invalid fields keyed to their client-side message; server validation matches. */
export type FormProblems = Partial<Record<"name" | "tag" | "image" | "newProject" | "existingProject" | "dashboardPR", string>>;

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
  if (f.dashboardDev === "pr" && !/^[1-9][0-9]{0,8}$/.test(f.dashboardPR.trim())) p.dashboardPR = "Enter the number of an odh-dashboard pull request.";
  return p;
}

/** Blocking reason for enabling Pipelines, or null when allowed. */
export function pipelinesReason(f: RecipeForm): string | null {
  return needsProjectReason(f, "Pipelines needs");
}

/** Why a feature that lives in the project cannot be on ("<what> a project: ..."), or null. */
export function needsProjectReason(f: RecipeForm, what: string): string | null {
  return f.project === "none" ? `${what} a project: choose one under Project.` : null;
}

/** Why the playground cannot be on, or null. */
export function playgroundReason(f: RecipeForm): string | null {
  return needsProjectReason(f, "The playground needs") ?? (!f.models ? "The playground needs a model: turn Models on." : null);
}

/** Why the model cannot be published through MaaS here, or null. */
export function maasPublishReason(f: RecipeForm): string | null {
  if (!f.models || f.project === "none") return "Publishing needs a model: turn Models on.";
  if (f.exposure === "maas") return "The model is reached through MaaS, which publishes it already.";
  return null;
}

/** Reason the recipe must have a project, or null when omission is fine. */
export function noProjectReason(f: RecipeForm): string | null {
  const inProject = [
    f.pipelines && "Pipelines",
    f.mlflow && f.mlflowSamples && "the MLflow samples",
    f.models && "Models",
    f.models && f.playground && "the playground",
    f.workbench && "the workbench",
  ].filter((x): x is string => !!x);
  if (inProject.length === 0) return null;
  const capital = (t: string) => `${t[0].toUpperCase()}${t.slice(1)}`;
  if (inProject.length === 1) {
    const [one] = inProject;
    return `${capital(one)} ${one === "the MLflow samples" ? "need" : "needs"} a project. Turn ${one} off to run without one.`;
  }
  const list = `${inProject.slice(0, -1).join(", ")} and ${inProject[inProject.length - 1]}`;
  return `${capital(list)} need a project. Turn them off to run without one.`;
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
  /** What it deletes besides, when that is worth saying. */
  description?: string;
  /** Ticked when the dialog opens: a cluster-wide part (the MaaS platform, the model registry) is not. */
  chosen: boolean;
}

/**
 * Ordered cleanup options for a run: entries named in the backend `ready` request,
 * limited to run-owned assets—the recipe-created project (pipeline server included)
 * and DataScienceCluster components the run enabled. Shared S3 storage, MLflow, and
 * pipeline servers outside recipe-created projects are omitted (see leftInPlace).
 */
export function cleanupParts(ready: CleanupRequest | undefined): RecipeCleanupPart[] {
  const parts: RecipeCleanupPart[] = [];
  for (const p of ready?.recipeProjects ?? []) parts.push({ id: `recipe-project:${p}`, title: `Project ${p}, which the recipe created`, chosen: true });
  // Cluster-wide parts the run created: teammates may use them since, so they are offered, not chosen.
  if (ready?.modelRegistry) {
    parts.push({ id: "model-registry", title: "The model registry the run created", description: "With its registered models and its database. Others may use it.", chosen: false });
  }
  if (ready?.maasPlatform) {
    parts.push({ id: "maas-platform", title: "The MaaS platform the run set up", description: "MaaS API keys and the MaaS database go with it. Others may use it.", chosen: false });
  }
  if (ready?.components?.length) parts.push({ id: "components", title: `The DataScienceCluster components the run turned on: ${ready.components.join(", ")}`, chosen: true });
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
  // The reviewed identities go with them: the backend checks each again before it deletes it.
  if (ready?.modelRegistry && chosen.has("model-registry")) Object.assign(req, { modelRegistry: true, modelRegistryUid: ready.modelRegistryUid ?? "" });
  if (ready?.maasPlatform && chosen.has("maas-platform")) Object.assign(req, { maasPlatform: true, maasPlatformUid: ready.maasPlatformUid ?? "" });
  if (req.modelRegistry || req.maasPlatform) req.recipeRunId = ready?.recipeRunId ?? "";
  if (ready?.components?.length && chosen.has("components")) req.components = ready.components;
  return req;
}

/**
 * Resources the run provisioned but this cleanup keeps, per current cluster
 * status (GET /api/resources/status): cluster-wide S3 and MLflow, plus the
 * pipeline server in the run project when that project is not part of the delete request,
 * the cluster-wide parts offered (offered) but not chosen, and a Dashboard Dev build.
 */
export function leftInPlace(status: ResourcesStatus, run: RecipeRun, req: CleanupRequest, offered?: CleanupRequest): string[] {
  const up = (r: ResourceState | null | undefined) => !!r?.deployed && !isTerminating(r);
  const out: string[] = [];
  const project = run.project?.name;
  if (project && !(req.recipeProjects ?? []).includes(project) && (status.pipelineServers ?? []).some((ps) => ps.namespace === project && up(ps))) {
    out.push(`The pipeline server in ${project}`);
  }
  if (up(status.minio)) out.push("The S3 storage");
  if (up(status.mlflow)) out.push("MLflow");
  if (offered?.modelRegistry && !req.modelRegistry) out.push("The model registry");
  if (offered?.maasPlatform && !req.maasPlatform) out.push("The MaaS platform");
  const dev = run.recipe.spec.dashboardDev;
  if (dev.deploy === "main" || dev.deploy === "pr") out.push("The Dashboard Dev build: revert it on the Dashboard Dev page");
  return out;
}