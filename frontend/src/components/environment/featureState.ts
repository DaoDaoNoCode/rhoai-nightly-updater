import type { ModelState, PlaygroundModel, PlaygroundState, PlaygroundStatus, ResourceState, ResourcesStatus } from "../../types";
import { envStatus, envStatusText, notManaged, type EnvStatus } from "./resourceStatus";
import type { Pending } from "./resourceRow";

/**
 * The state of each Environment feature, computed in one place: the
 * feature's tab and the Overview both render what these functions return,
 * so they cannot disagree. Readiness is health; ownership ("not managed by
 * this tool") is reported apart and only limits actions.
 */

/** What this page is doing now, for the parts it changes. */
export interface Busy {
  /** The action running from this page ("setup-minio", ...), or null. */
  resAction: string | null;
  /** The project that action is for. */
  actionProject: string | null;
  /** Migrate storage runs from this page. */
  migrating: boolean;
  /** A Models operation runs from this page: what it changes. */
  models?: ModelsOp | null;
  /** A Playground operation runs from this page: its action and project. */
  playground?: PlaygroundOp | null;
}

/** What a running Playground operation changes. */
export interface PlaygroundOp {
  action: "setup" | "teardown";
  project: string;
}

/** What a running Models operation changes. */
export interface ModelsOp {
  action: "setup" | "teardown";
  gateway: boolean;
  project: string | null;
}

export const IDLE: Busy = { resAction: null, actionProject: null, migrating: false, models: null };

/** The next thing to do about a feature. */
export type NextStep =
  | { kind: "open"; label: string; href: string }
  /** Set up the missing parts (setup); reason, when set, is why it cannot run now (a dependency). */
  | { kind: "set-up"; label: string; setup: Pending[]; reason: string | null }
  /** A part needs attention: show it. */
  | { kind: "show-issue"; part: string }
  /** Storage of an earlier version holds the data: Migrate storage moves it. */
  | { kind: "migrate" }
  /** A project-scoped feature with no project chosen. */
  | { kind: "choose-project" };

export interface FeatureState {
  /** The feature's health: the worst part that is not fine, or Ready. */
  status: EnvStatus;
  /** "Ready in juntao-test", "Ready · not managed by this tool", "Not set up in any project", ... */
  label: string;
  next: NextStep | null;
  /** The project this state is about (project-scoped features). */
  project: string | null;
}

/** One part of a feature, with its health and ownership. */
export interface PartState {
  id: string;
  status: EnvStatus;
  unmanaged: boolean;
}

const NOT_MANAGED = " · not managed by this tool";
const PROBLEM: EnvStatus[] = ["error", "needs-attention"];
const MOVING: EnvStatus[] = ["setting-up", "deleting"];

// --- Pipelines: S3 storage (cluster) and a pipeline server (project) -------------

/** The storage's health. Storage of an earlier version still holding the data needs attention. */
export function storageStatus(status: ResourcesStatus | null, busy: Busy): EnvStatus {
  const b = busy.migrating || busy.resAction === "setup-minio" ? "setting-up" : busy.resAction === "teardown-minio" ? "deleting" : null;
  return envStatus(status?.minio, { busy: b, attention: status?.legacyStorage?.stage === "migrate" });
}

/** Setup can create the storage: nothing is there, and nothing blocks it. */
export function storageCanSetUp(status: ResourcesStatus | null, busy: Busy): boolean {
  return storageStatus(status, busy) === "not-set-up" && !status?.legacyStorage && !status?.minio?.setupBlockedReason;
}

/**
 * Why a new pipeline server cannot be set up on the storage yet, or null.
 * The storage must be ready and this tool's (pipeline servers get a copy of
 * its keys), and a migration must have copied the old storage first: the
 * copy's check compares both sides, so a server writing to the new storage
 * before it would make the check fail.
 */
export function storageNotReadyReason(minio: ResourceState | undefined, legacy: ResourcesStatus["legacyStorage"], migrating: boolean): string | null {
  if (migrating) return "Migrate storage is running. Wait until it has finished.";
  if (legacy?.stage === "migrate") return "Available once Migrate storage has moved the data of an earlier version to the new storage.";
  if (notManaged(minio)) return "Pipeline servers set up here use the S3 storage this tool sets up. Namespace seaweedfs was not created by this tool, so it is not used.";
  if (!minio?.ready) return minio?.deployed ? "Available once the S3 storage is ready." : "Available once the S3 storage is set up.";
  return null;
}

/** A project's pipeline server: this tool's, else someone else's (its health only). */
export function pipelineServerIn(status: ResourcesStatus | null, project: string): ResourceState | undefined {
  return (status?.pipelineServers ?? []).find((ps) => ps.namespace === project)
    ?? (status?.unmanagedPipelineServers ?? []).find((ps) => ps.namespace === project);
}

/** The project has a pipeline server this tool did not create. */
export function pipelineServerUnmanaged(status: ResourcesStatus | null, project: string): boolean {
  return (status?.unmanagedPipelineProjects ?? []).includes(project) && !(status?.pipelineServers ?? []).some((ps) => ps.namespace === project);
}

export function pipelineServerStatus(status: ResourcesStatus | null, project: string, busy: Busy): EnvStatus {
  const mine = busy.actionProject === project;
  const b = mine && busy.resAction === "setup-pipeline-server" ? "setting-up" : mine && busy.resAction === "teardown-pipeline-server" ? "deleting" : null;
  const server = pipelineServerIn(status, project);
  // Someone else's server whose health is not reported (an older backend).
  if (!server && pipelineServerUnmanaged(status, project) && !b) return "unknown";
  return envStatus(server, { busy: b });
}

/** The parts of Pipelines in a project, in dependency order. */
export function pipelinesParts(status: ResourcesStatus | null, project: string, busy: Busy): PartState[] {
  return [
    { id: "storage", status: storageStatus(status, busy), unmanaged: notManaged(status?.minio) },
    { id: "pipeline-server", status: pipelineServerStatus(status, project, busy), unmanaged: pipelineServerUnmanaged(status, project) },
  ];
}

const projectsWithServers = (status: ResourcesStatus | null, projects: string[]) =>
  projects.filter((p) => !!pipelineServerIn(status, p) || pipelineServerUnmanaged(status, p));

/**
 * The project the Pipelines tab opens with: the URL's; else the first
 * project with a pipeline server, preferring a ready one; else none.
 */
export function defaultProject(status: ResourcesStatus | null, projects: string[], fromUrl: string | null): string | null {
  if (fromUrl) return fromUrl;
  const withServers = projectsWithServers(status, projects);
  return withServers.find((p) => pipelineServerIn(status, p)?.ready) ?? withServers[0] ?? null;
}

/** Pipelines in one project. */
function pipelinesIn(status: ResourcesStatus | null, project: string, busy: Busy): FeatureState {
  const parts = pipelinesParts(status, project, busy);
  const [storage, server] = parts;
  const unmanaged = parts.some((p) => p.unmanaged);
  const problem = parts.find((p) => PROBLEM.includes(p.status));
  if (problem) return { status: problem.status, label: `${envStatusText(problem.status)} in ${project}${problem.unmanaged ? NOT_MANAGED : ""}`, next: issueStep(status, problem.id), project };
  const moving = parts.find((p) => MOVING.includes(p.status));
  // Ready when the pipeline server is, on ready storage; someone else's server keeps its
  // artifacts in storage of its own, so this tool's storage is not its dependency.
  if (server.status === "ready" && (storage.status === "ready" || server.unmanaged)) {
    const href = pipelineServerIn(status, project)?.uiRoute;
    return { status: "ready", label: `Ready in ${project}${unmanaged ? NOT_MANAGED : ""}`, next: href ? { kind: "open", label: `Open pipelines in ${project}`, href } : null, project };
  }
  if (server.status === "unknown") return { status: "unknown", label: `Unknown in ${project}${NOT_MANAGED}`, next: null, project };
  const setup: Pending[] = [];
  const storageSetUp = storageCanSetUp(status, busy);
  if (storageSetUp) setup.push({ kind: "setup-minio" });
  const storageReason = storageNotReadyReason(status?.minio, status?.legacyStorage, busy.migrating);
  if (server.status === "not-set-up" && !server.unmanaged && (storageReason === null || storageSetUp)) setup.push({ kind: "setup-pipeline-server", project });
  const status_: EnvStatus = moving ? "setting-up" : "not-set-up";
  const label = moving ? `Setting up in ${project}` : `Not set up in ${project}`;
  // Every part exists and one is still starting: nothing is left to set up.
  if (moving && parts.every((p) => p.status !== "not-set-up")) return { status: status_, label, next: null, project };
  let reason: string | null = null;
  if (server.status === "not-set-up" && status?.pipelinesBlocked) reason = status.pipelinesBlocked.reason;
  else if (moving) reason = `${moving.id === "storage" ? "The S3 storage" : "The pipeline server"} is still ${moving.status === "deleting" ? "being deleted" : "setting up"}. Wait until it is ready.`;
  else if (setup.length === 0) reason = storage.status === "not-set-up" ? (status?.minio?.setupBlockedReason ?? "The S3 storage cannot be set up here.") : storageReason ?? "Nothing can be set up here.";
  // Name what Set up sets up: the whole feature, or the one part missing.
  const onlyServer = setup.length === 1 && setup[0].kind === "setup-pipeline-server";
  return {
    status: status_,
    label,
    next: { kind: "set-up", label: onlyServer ? `Set up the pipeline server in ${project}` : `Set up Pipelines in ${project}`, setup, reason },
    project,
  };
}

/** The next step for a part that needs attention: Migrate storage for storage of an earlier version, else show it. */
function issueStep(status: ResourcesStatus | null, part: string): NextStep {
  return part === "storage" && status?.legacyStorage?.stage === "migrate" ? { kind: "migrate" } : { kind: "show-issue", part };
}

/**
 * Why the set-up a run is about to start cannot run on the latest status,
 * or null: its first check, so that no part is created when a later part
 * cannot be (a component that is off, storage that is gone).
 */
export function setUpBlocked(state: FeatureState): string | null {
  if (state.next?.kind !== "set-up") return "There is nothing left to set up here; the page shows the current state.";
  return state.next.reason;
}

/**
 * A project-scoped feature summarised across the projects that have it (the
 * Overview), in the order someone scanning the table needs: the operation
 * running from this page; then problems, which ready projects never hide;
 * then ready; then anything moving or unknown. Counts are of the projects
 * that have the feature, never of every project on the cluster. Null when no
 * project has it.
 */
function acrossProjects(states: FeatureState[], running: FeatureState | null): FeatureState | null {
  if (running && MOVING.includes(running.status)) return running;
  const problems = states.filter((s) => PROBLEM.includes(s.status));
  const ready = states.filter((s) => s.status === "ready");
  if (problems.length > 0) {
    const first = problems.find((s) => s.status === "error") ?? problems[0];
    if (problems.length === 1 && ready.length === 0) return first;
    // The next step still opens the first problem's project.
    return { ...first, label: `${envStatusText(first.status)} in ${problems.length} of ${states.length} projects` };
  }
  if (ready.length === 1) return ready[0];
  if (ready.length > 1) return { status: "ready", label: `Ready in ${ready.length} projects`, next: { kind: "choose-project" }, project: null };
  return states.find((s) => MOVING.includes(s.status)) ?? states.find((s) => s.status === "unknown") ?? null;
}

/**
 * Pipelines: in a project (the tab), or, without one, summarised across
 * the projects (the Overview): "Ready in juntao-test", "Ready in 2 of 5
 * projects", "Not set up in any project".
 */
export function pipelinesState(status: ResourcesStatus | null, project: string | null, projects: string[], busy: Busy = IDLE): FeatureState {
  if (project) return pipelinesIn(status, project, busy);
  const storage = storageStatus(status, busy);
  if (PROBLEM.includes(storage)) return { status: storage, label: `${envStatusText(storage)}${notManaged(status?.minio) ? NOT_MANAGED : ""}`, next: issueStep(status, "storage"), project: defaultProject(status, projects, null) };
  const states = projectsWithServers(status, projects).map((p) => pipelinesIn(status, p, busy));
  const running = busy.actionProject ? pipelinesIn(status, busy.actionProject, busy) : null;
  const summary = acrossProjects(states, running);
  if (summary) return summary;
  if (MOVING.includes(storage)) return { status: storage, label: "Setting up", next: null, project: null };
  return { status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" }, project: null };
}

// --- MLflow ------------------------------------------------------------------------

export function mlflowStatus(status: ResourcesStatus | null, busy: Busy): EnvStatus {
  const r = busy.resAction;
  const b = r === "setup-mlflow" || r === "deploy-mlflow-pr" || r === "revert-mlflow" ? "setting-up" : r === "teardown-mlflow" ? "deleting" : null;
  return envStatus(status?.mlflow, { busy: b });
}

/**
 * The label of a link to MLflow's root: with workspaces on, its root is the
 * list of workspaces, one per project (opendatahub-io/mlflow server/js
 * MlflowRouter: "/" is a global route, WorkspacesHomeView).
 */
export const MLFLOW_WORKSPACES = "Open MLflow workspaces";

export function mlflowState(status: ResourcesStatus | null, busy: Busy = IDLE): FeatureState {
  const mlflow = status?.mlflow;
  const s = mlflowStatus(status, busy);
  const unmanaged = notManaged(mlflow);
  const suffix = unmanaged ? NOT_MANAGED : "";
  switch (s) {
    case "ready":
      return { status: s, label: `Ready${suffix}`, next: mlflow?.uiRoute ? { kind: "open", label: MLFLOW_WORKSPACES, href: mlflow.uiRoute } : null, project: null };
    case "error":
    case "needs-attention":
      return { status: s, label: `${s === "error" ? "Error" : "Needs attention"}${suffix}`, next: { kind: "show-issue", part: "mlflow" }, project: null };
    case "setting-up":
    case "deleting":
      return { status: s, label: s === "deleting" ? "Deleting" : "Setting up", next: null, project: null };
    case "not-set-up":
      return { status: s, label: "Not set up", next: { kind: "set-up", label: "Set up MLflow", setup: [{ kind: "setup-mlflow" }], reason: status?.mlflowBlocked?.reason ?? mlflow?.setupBlockedReason ?? null }, project: null };
    default:
      return { status: s, label: `Unknown${suffix}`, next: null, project: null };
  }
}

// --- Models: the inference gateway (cluster) and a model (project) --------------------

const modelsBusy = (busy: Busy, part: "gateway" | "model", project?: string): "setting-up" | "deleting" | null => {
  const op = busy.models;
  if (!op || (part === "gateway" ? !op.gateway : op.project !== project)) return null;
  return op.action === "setup" ? "setting-up" : "deleting";
};

export function gatewayStatus(status: ResourcesStatus | null, busy: Busy): EnvStatus {
  return envStatus(status?.inferenceGateway, { busy: modelsBusy(busy, "gateway") });
}

/** The project's model: this tool's, else one of the same name someone else made. */
export function modelIn(status: ResourcesStatus | null, project: string): ModelState | undefined {
  const items = (status?.models?.items ?? []).filter((m) => m.namespace === project);
  return items.find((m) => m.managedByTool !== false) ?? items[0];
}

export function modelStatus(status: ResourcesStatus | null, project: string, busy: Busy): EnvStatus {
  return envStatus(modelIn(status, project), { busy: modelsBusy(busy, "model", project) });
}

/** The parts of Models in a project, in dependency order. */
export function modelsParts(status: ResourcesStatus | null, project: string, busy: Busy): PartState[] {
  return [
    { id: "gateway", status: gatewayStatus(status, busy), unmanaged: notManaged(status?.inferenceGateway) },
    { id: "model", status: modelStatus(status, project, busy), unmanaged: notManaged(modelIn(status, project)) },
  ];
}

/** Why no model can be set up on this cluster, or null (unknown is not a reason). */
export function modelsBlockedReason(status: ResourcesStatus | null): string | null {
  return status?.models?.blocked?.reason ?? status?.models?.presetProblem ?? null;
}

/** The dashboard's page of a project's deployed models (odh-dashboard /projects/<ns>). */
export function dashboardProjectHref(dashboardUrl: string | undefined, project: string): string | undefined {
  return dashboardUrl?.startsWith("https://") ? `${dashboardUrl.replace(/\/$/, "")}/projects/${encodeURIComponent(project)}?section=model-server` : undefined;
}

const projectsWithModels = (status: ResourcesStatus | null, projects: string[]) => projects.filter((p) => !!modelIn(status, p));

/** The project the Models tab opens with: the URL's; else the first with a model, preferring a ready one. */
export function defaultModelsProject(status: ResourcesStatus | null, projects: string[], fromUrl: string | null): string | null {
  if (fromUrl) return fromUrl;
  const withModels = projectsWithModels(status, projects);
  return withModels.find((p) => modelIn(status, p)?.ready) ?? withModels[0] ?? null;
}

function modelsIn(status: ResourcesStatus | null, project: string, busy: Busy): FeatureState {
  const parts = modelsParts(status, project, busy);
  const [gateway, model] = parts;
  const problem = parts.find((p) => PROBLEM.includes(p.status));
  if (problem) return { status: problem.status, label: `${envStatusText(problem.status)} in ${project}${problem.unmanaged ? NOT_MANAGED : ""}`, next: { kind: "show-issue", part: problem.id }, project };
  const unmanaged = parts.some((p) => p.unmanaged);
  if (model.status === "ready") {
    const href = dashboardProjectHref(status?.dashboardUrl, project);
    return { status: "ready", label: `Ready in ${project}${unmanaged ? NOT_MANAGED : ""}`, next: href ? { kind: "open", label: `Open ${project} in the dashboard`, href } : null, project };
  }
  if (model.status === "unknown") return { status: "unknown", label: `Unknown in ${project}${model.unmanaged ? NOT_MANAGED : ""}`, next: null, project };
  const moving = parts.find((p) => MOVING.includes(p.status));
  if (moving && parts.every((p) => p.status !== "not-set-up")) {
    return { status: moving.status === "deleting" ? "deleting" : "setting-up", label: moving.status === "deleting" ? `Deleting in ${project}` : `Setting up in ${project}`, next: null, project };
  }
  const needGateway = gateway.status === "not-set-up";
  const setup: Pending[] = [{ kind: "setup-models", gateway: needGateway, project }];
  let reason: string | null = modelsBlockedReason(status);
  if (!reason && moving) reason = `The ${moving.id === "gateway" ? "inference gateway" : "model"} is still ${moving.status === "deleting" ? "being deleted" : "setting up"}. Wait until it is done.`;
  if (!reason && needGateway && status?.inferenceGateway?.setupBlockedReason) reason = status.inferenceGateway.setupBlockedReason;
  return {
    status: moving ? "setting-up" : "not-set-up",
    label: moving ? `Setting up in ${project}` : `Not set up in ${project}`,
    next: { kind: "set-up", label: needGateway ? `Set up Models in ${project}` : `Set up the model in ${project}`, setup, reason },
    project,
  };
}

/**
 * Models: in a project (the tab), or summarised across the projects (the
 * Overview), as pipelinesState does.
 */
export function modelsState(status: ResourcesStatus | null, project: string | null, projects: string[], busy: Busy = IDLE): FeatureState {
  if (project) return modelsIn(status, project, busy);
  const gateway = gatewayStatus(status, busy);
  if (PROBLEM.includes(gateway)) {
    return { status: gateway, label: `${envStatusText(gateway)}${notManaged(status?.inferenceGateway) ? NOT_MANAGED : ""}`, next: { kind: "show-issue", part: "gateway" }, project: defaultModelsProject(status, projects, null) };
  }
  const states = projectsWithModels(status, projects).map((p) => modelsIn(status, p, busy));
  const running = busy.models?.project ? modelsIn(status, busy.models.project, busy) : null;
  const summary = acrossProjects(states, running);
  if (summary) return summary;
  if (MOVING.includes(gateway)) return { status: gateway, label: gateway === "deleting" ? "Deleting" : "Setting up", next: null, project: null };
  return { status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" }, project: null };
}

// --- Playground: the model(s) it uses and the dashboard's playground (project) ----------

/** The project's playground, as the dashboard reports it (or this tool's record of one). */
export function playgroundIn(pg: PlaygroundStatus | null, project: string): PlaygroundState | undefined {
  return (pg?.items ?? []).find((i) => i.namespace === project);
}

/** The project's models the playground can chat with: ready, not embedding. */
export function playgroundChatModels(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return pg?.project === project ? (pg.models ?? []).filter((m) => m.ready && m.chat) : [];
}

/** What Set up uses by default, as the dashboard's dialog: the project's own ready chat models (MaaS only when chosen). */
export function defaultPlaygroundModels(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return playgroundChatModels(pg, project).filter((m) => m.source === "namespace");
}

/** Models deployed for Gen AI in the project that are not ready: the dashboard's install fails while one is. */
export function playgroundModelsNotReady(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return pg?.project === project ? (pg.models ?? []).filter((m) => m.source === "namespace" && !m.ready) : [];
}

const playgroundBusy = (busy: Busy, project: string): "setting-up" | "deleting" | null => {
  const op = busy.playground;
  if (!op || op.project !== project) return null;
  return op.action === "setup" ? "setting-up" : "deleting";
};

export function playgroundStatusOf(pg: PlaygroundStatus | null, project: string, busy: Busy): EnvStatus {
  const item = playgroundIn(pg, project);
  // A record of a playground that no longer exists is not a playground.
  return envStatus(item?.deployed || item?.terminating ? item : undefined, { busy: playgroundBusy(busy, project) });
}

/** The model part: ready when the project has a ready chat model; unknown until its models are read. */
export function playgroundModelStatus(pg: PlaygroundStatus | null, project: string): EnvStatus {
  if (pg?.project !== project || pg.modelsProblem) return "unknown";
  return playgroundChatModels(pg, project).length > 0 ? "ready" : "not-set-up";
}

/** The parts of Playground in a project, in dependency order. */
export function playgroundParts(pg: PlaygroundStatus | null, project: string, busy: Busy): PartState[] {
  const item = playgroundIn(pg, project);
  return [
    { id: "playground-models", status: playgroundModelStatus(pg, project), unmanaged: false },
    { id: "playground", status: playgroundStatusOf(pg, project, busy), unmanaged: !!item?.deployed && item.managedByTool === false },
  ];
}

/** The dashboard's playground page of a project (odh-dashboard packages/gen-ai routes: /gen-ai-studio/playground/<ns>). */
export function playgroundHref(dashboardUrl: string | undefined, project: string): string | undefined {
  return dashboardUrl?.startsWith("https://") ? `${dashboardUrl.replace(/\/$/, "")}/gen-ai-studio/playground/${encodeURIComponent(project)}` : undefined;
}

/** Why no playground can be set up now (cluster or API), or null. Unknown is not a reason. */
export function playgroundClusterReason(pg: PlaygroundStatus | null): string | null {
  if (!pg) return "The playground's state is still loading.";
  if (pg.blocked) return pg.blocked.reason;
  if (pg.genAiUi === "missing") return "The dashboard's Gen AI studio is not deployed on this cluster. A cluster-admin turns it on in the dashboard's settings.";
  if (pg.genAiUi === "unavailable") return "The dashboard's Gen AI studio is not running yet. Wait until it is.";
  if (pg.operatorReady === "False") return `The playground's operator is not ready${pg.operatorMessage ? `: ${pg.operatorMessage}` : ""}.`;
  return pg.apiProblem ?? null;
}

function playgroundInProject(status: ResourcesStatus | null, pg: PlaygroundStatus | null, project: string, busy: Busy): FeatureState {
  const parts = playgroundParts(pg, project, busy);
  const [models, playground] = parts;
  const item = playgroundIn(pg, project);
  const suffix = playground.unmanaged ? NOT_MANAGED : "";
  if (PROBLEM.includes(playground.status)) return { status: playground.status, label: `${envStatusText(playground.status)} in ${project}${suffix}`, next: { kind: "show-issue", part: "playground" }, project };
  if (playground.status === "ready") {
    const href = playgroundHref(status?.dashboardUrl, project);
    return { status: "ready", label: `Ready in ${project}${suffix}`, next: href ? { kind: "open", label: `Open the playground in ${project}`, href } : null, project };
  }
  if (MOVING.includes(playground.status)) return { status: playground.status, label: playground.status === "deleting" ? `Deleting in ${project}` : `Setting up in ${project}`, next: null, project };
  if (playground.status === "unknown") return { status: "unknown", label: `Unknown in ${project}${suffix}`, next: null, project };
  if (item?.deployed) return { status: "unknown", label: `Unknown in ${project}${suffix}`, next: null, project };
  let reason = playgroundClusterReason(pg);
  const notReady = playgroundModelsNotReady(pg, project);
  if (!reason && models.status === "unknown") reason = pg?.modelsProblem ?? "The project's models are still loading.";
  if (!reason && notReady.length > 0) reason = `Not every model in ${project} is ready (${notReady.map((m) => m.name).join(", ")}); the dashboard's install needs all of them ready.`;
  if (!reason && models.status === "not-set-up") reason = `Needs: Models. ${project} has no ready chat model; set one up on the Models tab first.`;
  return {
    status: "not-set-up",
    label: `Not set up in ${project}`,
    next: { kind: "set-up", label: `Set up the playground in ${project}`, setup: [{ kind: "setup-playground", project }], reason },
    project,
  };
}

/** The project the Playground tab opens with: the URL's; else the first with a playground, preferring a ready one. */
export function defaultPlaygroundProject(pg: PlaygroundStatus | null, fromUrl: string | null): string | null {
  if (fromUrl) return fromUrl;
  const items = (pg?.items ?? []).filter((i) => i.deployed);
  return items.find((i) => i.ready)?.namespace ?? items[0]?.namespace ?? null;
}

/**
 * Playground: in a project (the tab), or summarised across the projects
 * that have one (the Overview), as pipelinesState does. pg is the
 * playground status (GET /api/resources/playground), null while it loads.
 */
export function playgroundState(status: ResourcesStatus | null, pg: PlaygroundStatus | null, project: string | null, busy: Busy = IDLE): FeatureState {
  if (project) return playgroundInProject(status, pg, project, busy);
  if (!pg) return { status: "unknown", label: "Checking", next: null, project: null };
  const projects = [...new Set((pg.items ?? []).filter((i) => i.deployed || i.terminating).map((i) => i.namespace!))];
  const states = projects.map((p) => playgroundInProject(status, pg, p, busy));
  const running = busy.playground ? playgroundInProject(status, pg, busy.playground.project, busy) : null;
  const summary = acrossProjects(states, running);
  if (summary) return summary;
  if (pg.apiProblem) return { status: "unknown", label: "Unknown: the dashboard's gen-ai API cannot be used", next: null, project: null };
  return { status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" }, project: null };
}
