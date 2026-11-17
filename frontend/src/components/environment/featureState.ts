import type { ResourceState, ResourcesStatus } from "../../types";
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
}

export const IDLE: Busy = { resAction: null, actionProject: null, migrating: false };

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
 * Pipelines: in a project (the tab), or, without one, summarised across
 * the projects (the Overview): "Ready in juntao-test", "Ready in 2 of 5
 * projects", "Not set up in any project".
 */
export function pipelinesState(status: ResourcesStatus | null, project: string | null, projects: string[], busy: Busy = IDLE): FeatureState {
  if (project) return pipelinesIn(status, project, busy);
  const storage = storageStatus(status, busy);
  if (PROBLEM.includes(storage)) return { status: storage, label: `${envStatusText(storage)}${notManaged(status?.minio) ? NOT_MANAGED : ""}`, next: issueStep(status, "storage"), project: defaultProject(status, projects, null) };
  const withServers = projectsWithServers(status, projects);
  const states = withServers.map((p) => pipelinesIn(status, p, busy));
  const ready = states.filter((s) => s.status === "ready");
  if (ready.length === 1) return ready[0];
  // Several ready projects: no jump to one of them; the tab's picker decides.
  if (ready.length > 1) {
    return { status: "ready", label: `Ready in ${ready.length} of ${projects.length} projects`, next: { kind: "choose-project" }, project: null };
  }
  const problem = states.find((s) => PROBLEM.includes(s.status)) ?? states.find((s) => MOVING.includes(s.status) || s.status === "setting-up") ?? states.find((s) => s.status === "unknown");
  if (problem) return problem;
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
