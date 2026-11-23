import type { CatalogModel, MaaSPart, MaaSRequest, MaaSStatus, ModelState, PlaygroundModel, PlaygroundState, PlaygroundStatus, ResourceState, ResourcesStatus } from "../../types";
import { envStatus, envStatusText, notManaged, type EnvStatus } from "./resourceStatus";
import type { Pending } from "./resourceRow";
import { mlflowOpenHref } from "./UseIt";

/**
 * Each Environment feature's state is derived here once; both the dedicated
 * tab and Overview display these function results, which keeps them aligned.
 * Health maps to readiness; whether something was not created by this tool
 * surfaces separately on FeatureState.unmanaged and gates actions only.
 */

/** In-flight work initiated from this page for the components it manages. */
export interface Busy {
  /** Which action this page is running ("setup-minio", ...), if any. */
  resAction: string | null;
  /** Namespace that in-flight action targets. */
  actionProject: string | null;
  /** Whether Migrate storage was kicked off here. */
  migrating: boolean;
  /** Models work in progress from this page and what it mutates. */
  models?: ModelsOp | null;
  /** Playground work in progress from this page: action plus project. */
  playground?: PlaygroundOp | null;
  /** MaaS work in progress from this page: action, project (e.g. publish). */
  maas?: MaaSOp | null;
}

/** Fields a MaaS in-flight operation is allowed to change. */
export interface MaaSOp {
  action: MaaSRequest["action"];
  project: string | null;
  /** Catalog model involved in publish or unpublish. */
  model?: string | null;
}

/** Fields a Playground in-flight operation is allowed to change. */
export interface PlaygroundOp {
  action: "setup" | "teardown";
  project: string;
}

/** Fields a Models in-flight operation is allowed to change. */
export interface ModelsOp {
  /** allow / disallow: put allowProject on or off the gateway list (gateway true); check: single model invocation. */
  action: "setup" | "teardown" | "allow" | "disallow" | "check";
  gateway: boolean;
  project: string | null;
  /** Catalog model name within the project. */
  model?: string | null;
  allowProject?: string;
}

export const IDLE: Busy = { resAction: null, actionProject: null, migrating: false, models: null };

/** Suggested user action for a feature. */
export type NextStep =
  | { kind: "open"; label: string; href: string }
  /** Provision missing pieces (setup); reason explains blocking dependencies when present. */
  | { kind: "set-up"; label: string; setup: Pending[]; reason: string | null }
  /**
   * Surface a component that needs attention. When state spans projects,
   * project pins the one with the issue.
   */
  | { kind: "show-issue"; part: string; project?: string }
  /** Legacy storage still holds data; run Migrate storage. */
  | { kind: "migrate" }
  /** Project-scoped feature with no project selected yet. */
  | { kind: "choose-project" };

export interface FeatureState {
  /** Overall health: worst non-healthy part, otherwise Ready. */
  status: EnvStatus;
  /** "Ready in juntao-test", "Ready in 3 projects", "Not set up in any project", ... */
  label: string;
  /** Present when the underlying part was not created by this tool (shown beside the label). Omitted otherwise. */
  unmanaged?: boolean;
  next: NextStep | null;
  /** Project namespace for project-scoped features. */
  project: string | null;
  /** Per-project breakdown when summarising many projects, ordered like the project list. */
  projects?: FeatureState[];
  /**
   * Underlying status has not arrived yet, so UI shows a loading Skeleton
   * rather than pretending to know the feature state.
   */
  loading?: true;
}

/** Placeholder while feature status is still being fetched. */
export const loadingState = (project: string | null = null): FeatureState => ({ status: "unknown", label: "Loading", next: null, project, loading: true });

/** Status never became available; only recovery is to fetch again. */
/** After clearing or losing the project: tab copy reflects that instead of a multi-project rollup. */
export const noProjectState = (): FeatureState => ({ status: "unknown", label: "No project chosen", next: { kind: "choose-project" }, project: null });

export const unreadableState = (project: string | null = null): FeatureState => ({ status: "unknown", label: "Unknown: cannot be read", next: null, project });

/** Single feature slice: health plus whether this tool owns it. */
export interface PartState {
  id: string;
  status: EnvStatus;
  unmanaged: boolean;
}

/** Populates FeatureState.unmanaged only when the flag is true. */
const owner = (unmanaged: boolean): { unmanaged?: true } => (unmanaged ? { unmanaged: true } : {});
const PROBLEM: EnvStatus[] = ["error", "needs-attention"];
const MOVING: EnvStatus[] = ["setting-up", "deleting"];

// --- Pipelines: cluster S3 storage and per-project pipeline server -------------

/** Storage health; legacy buckets retaining data warrant attention. */
export function storageStatus(status: ResourcesStatus | null, busy: Busy): EnvStatus {
  const b = busy.migrating || busy.resAction === "setup-minio" ? "setting-up" : busy.resAction === "teardown-minio" ? "deleting" : null;
  return envStatus(status?.minio, { busy: b, attention: status?.legacyStorage?.stage === "migrate" });
}

/** True when setup may provision storage: absent and unblocked. */
/** The one label of the storage while an earlier version's storage still holds the data: the same on every tab and row. */
export const MIGRATION_NEEDED = "Migration needed";

/** A storage problem's label: the cluster's, so it names no project. */
const storageProblemLabel = (status: ResourcesStatus | null, s: EnvStatus) => (status?.legacyStorage?.stage === "migrate" ? MIGRATION_NEEDED : envStatusText(s));

export function storageCanSetUp(status: ResourcesStatus | null, busy: Busy): boolean {
  return storageStatus(status, busy) === "not-set-up" && !status?.legacyStorage && !status?.minio?.setupBlockedReason;
}

/**
 * S3 storage in isolation (dedicated tab and Overview row): cluster scope,
 * hence no project. Legacy data still resident triggers Migrate storage;
 * missing storage exposes Set up alone—the same prerequisite Set up Pipelines
 * runs first.
 */
export function storageState(status: ResourcesStatus | null, busy: Busy = IDLE): FeatureState {
  if (!status) return loadingState();
  const s = storageStatus(status, busy);
  const unmanaged = notManaged(status.minio);
  if (PROBLEM.includes(s)) {
    const migrate = status.legacyStorage?.stage === "migrate";
    return { status: s, label: migrate ? MIGRATION_NEEDED : envStatusText(s), ...owner(unmanaged), next: issueStep(status, "storage"), project: null };
  }
  if (s === "ready") {
    const href = status.minio?.uiRoute;
    return { status: s, label: envStatusText(s), ...owner(unmanaged), next: href && !unmanaged ? { kind: "open", label: "Open admin UI", href } : null, project: null };
  }
  if (s === "not-set-up" && !unmanaged) {
    const reason = status.minio?.setupBlockedReason ?? null;
    return { status: s, label: envStatusText(s), next: { kind: "set-up", label: "Set up S3 storage", setup: [{ kind: "setup-minio" }], reason }, project: null };
  }
  return { status: s, label: envStatusText(s), ...owner(unmanaged), next: null, project: null };
}

/**
 * Explanation when pipeline server setup on storage must wait, else null.
 * Storage must be ready and owned here (servers receive key material), and
 * migration must finish copying legacy data—the integrity check compares both
 * sides, so premature writes to the new bucket would fail it.
 */
export function storageNotReadyReason(minio: ResourceState | undefined, legacy: ResourcesStatus["legacyStorage"], migrating: boolean): string | null {
  if (migrating) return "Migrate storage is running. Wait until it has finished.";
  if (legacy?.stage === "migrate") return "Available once Migrate storage has moved the data of an earlier version to the new storage.";
  if (notManaged(minio)) return "Pipeline servers set up here use the S3 storage this tool sets up. Namespace seaweedfs was not created by this tool, so it is not used.";
  if (!minio?.ready) return minio?.deployed ? "Available once the S3 storage is ready." : "Available once the S3 storage is set up.";
  return null;
}

/** Pipeline server in a project: prefer this tool's record, otherwise external (health only). */
export function pipelineServerIn(status: ResourcesStatus | null, project: string): ResourceState | undefined {
  return (status?.pipelineServers ?? []).find((ps) => ps.namespace === project)
    ?? (status?.unmanagedPipelineServers ?? []).find((ps) => ps.namespace === project);
}

/** True when the namespace hosts a pipeline server outside this tool's management. */
export function pipelineServerUnmanaged(status: ResourcesStatus | null, project: string): boolean {
  return (status?.unmanagedPipelineProjects ?? []).includes(project) && !(status?.pipelineServers ?? []).some((ps) => ps.namespace === project);
}

export function pipelineServerStatus(status: ResourcesStatus | null, project: string, busy: Busy): EnvStatus {
  const mine = busy.actionProject === project;
  const b = mine && busy.resAction === "setup-pipeline-server" ? "setting-up" : mine && busy.resAction === "teardown-pipeline-server" ? "deleting" : null;
  const server = pipelineServerIn(status, project);
  // Externally managed server with no health from an older API.
  if (!server && pipelineServerUnmanaged(status, project) && !b) return "unknown";
  return envStatus(server, { busy: b });
}

/** Pipelines components for one project, ordered by dependency. */
export function pipelinesParts(status: ResourcesStatus | null, project: string, busy: Busy): PartState[] {
  return [
    { id: "storage", status: storageStatus(status, busy), unmanaged: notManaged(status?.minio) },
    { id: "pipeline-server", status: pipelineServerStatus(status, project, busy), unmanaged: pipelineServerUnmanaged(status, project) },
  ];
}

const projectsWithServers = (status: ResourcesStatus | null, projects: string[]) =>
  projects.filter((p) => !!pipelineServerIn(status, p) || pipelineServerUnmanaged(status, p));

/**
 * Initial project for the Pipelines tab: URL param if present, otherwise
 * first namespace with a pipeline server (ready wins), else none.
 */
export function defaultProject(status: ResourcesStatus | null, projects: string[], fromUrl: string | null): string | null {
  if (fromUrl) return fromUrl;
  const withServers = projectsWithServers(status, projects);
  return withServers.find((p) => pipelineServerIn(status, p)?.ready) ?? withServers[0] ?? null;
}

/** Pipelines feature state scoped to one project. */
function pipelinesIn(status: ResourcesStatus | null, project: string, busy: Busy): FeatureState {
  const parts = pipelinesParts(status, project, busy);
  const [storage, server] = parts;
  const unmanaged = parts.some((p) => p.unmanaged);
  const problem = parts.find((p) => PROBLEM.includes(p.status));
  if (problem?.id === "storage") return { status: problem.status, label: storageProblemLabel(status, problem.status), ...owner(problem.unmanaged), next: { kind: "show-issue", part: "storage" }, project };
  if (problem?.id === "storage") return { status: problem.status, label: storageProblemLabel(status, problem.status), ...owner(problem.unmanaged), next: { kind: "show-issue", part: "storage" }, project };
  if (problem) return { status: problem.status, label: `${envStatusText(problem.status)} in ${project}`, ...owner(problem.unmanaged), next: issueStep(status, problem.id), project };
  const moving = parts.find((p) => MOVING.includes(p.status));
  // Ready once the pipeline server is, atop ready storage; foreign servers store
  // artifacts elsewhere, so our storage is not a prerequisite for them.
  if (server.status === "ready" && (storage.status === "ready" || server.unmanaged)) {
    const href = pipelineServerIn(status, project)?.uiRoute;
    return { status: "ready", label: `Ready in ${project}`, ...owner(unmanaged), next: href ? { kind: "open", label: "Open pipelines", href } : null, project };
  }
  if (server.status === "unknown") return { status: "unknown", label: `Unknown in ${project}`, unmanaged: true, next: null, project };
  const setup: Pending[] = [];
  const storageSetUp = storageCanSetUp(status, busy);
  if (storageSetUp) setup.push({ kind: "setup-minio" });
  const storageReason = storageNotReadyReason(status?.minio, status?.legacyStorage, busy.migrating);
  if (server.status === "not-set-up" && !server.unmanaged && (storageReason === null || storageSetUp)) setup.push({ kind: "setup-pipeline-server", project });
  const status_: EnvStatus = moving ? "setting-up" : "not-set-up";
  const label = moving ? `Setting up in ${project}` : `Not set up in ${project}`;
  // All parts exist while one is still provisioning: no further setup steps.
  if (moving && parts.every((p) => p.status !== "not-set-up")) return { status: status_, label, next: null, project };
  let reason: string | null = null;
  if (server.status === "not-set-up" && status?.pipelinesBlocked) reason = status.pipelinesBlocked.reason;
  else if (moving) reason = `${moving.id === "storage" ? "The S3 storage" : "The pipeline server"} is still ${moving.status === "deleting" ? "being deleted" : "setting up"}. Wait until it is ready.`;
  else if (setup.length === 0) reason = storage.status === "not-set-up" ? (status?.minio?.setupBlockedReason ?? "The S3 storage cannot be set up here.") : storageReason ?? "Nothing can be set up here.";
  // Label Set up for the full feature versus the lone missing piece.
  const onlyServer = setup.length === 1 && setup[0].kind === "setup-pipeline-server";
  return {
    status: status_,
    label,
    next: { kind: "set-up", label: onlyServer ? `Set up the pipeline server in ${project}` : `Set up Pipelines in ${project}`, setup, reason },
    project,
  };
}

/** Next step when a part needs attention: migrate legacy storage if applicable, otherwise surface the part. */
function issueStep(status: ResourcesStatus | null, part: string): NextStep {
  return part === "storage" && status?.legacyStorage?.stage === "migrate" ? { kind: "migrate" } : { kind: "show-issue", part };
}

/**
 * Message when a pending set-up cannot proceed on fresh status, or null.
 * Evaluated up front so upstream pieces are not created if downstream ones
 * would fail (disabled component, missing storage).
 */
export function setUpBlocked(state: FeatureState): string | null {
  if (state.next?.kind !== "set-up") return "There is nothing left to set up here; the page shows the current state.";
  return state.next.reason;
}

/**
 * Roll up a project-scoped feature for Overview rows, prioritised for table
 * scanning: in-flight work from this page; then problems (errors beat
 * warnings even across ready namespaces); then ready; then transitional or
 * unknown. Counts include only projects that actually host the feature, not
 * every cluster namespace—e.g. "Error in 1 of 3 projects" with show-issue
 * naming the offender ("Open zeta"); multiple ready rows become "Ready in 3
 * projects" with choose-project as the step. projects mirrors per-tab detail
 * when several namespaces qualify. Null if none do.
 */
function acrossProjects(states: FeatureState[], running: FeatureState | null): FeatureState | null {
  if (running && MOVING.includes(running.status)) return running;
  const several = states.length > 1 ? { projects: states } : {};
  const problems = states.filter((s) => PROBLEM.includes(s.status));
  const ready = states.filter((s) => s.status === "ready");
  if (problems.length > 0) {
    const first = problems.find((s) => s.status === "error") ?? problems[0];
    if (states.length === 1) return first;
    const count = problems.filter((s) => s.status === first.status).length;
    const next: NextStep | null = first.next?.kind === "show-issue" && first.project ? { ...first.next, project: first.project } : first.next;
    // Tie the row and next step to the namespace carrying the problem.
    return { status: first.status, label: `${envStatusText(first.status)} in ${count} of ${states.length} projects`, next, project: first.project, ...several };
  }
  if (ready.length > 1) return { status: "ready", label: `Ready in ${ready.length} projects`, next: { kind: "choose-project" }, project: null, ...several };
  const one = ready[0] ?? states.find((s) => MOVING.includes(s.status)) ?? states.find((s) => s.status === "unknown");
  return one ? { ...one, ...several } : null;
}

/**
 * Pipelines state for a chosen project (tab) or aggregated across namespaces
 * (Overview), producing labels like "Ready in juntao-test", "Ready in 2
 * projects", "Error in 1 of 3 projects", "Not set up in any project".
 */
export function pipelinesState(status: ResourcesStatus | null, project: string | null, projects: string[], busy: Busy = IDLE): FeatureState {
  if (!status) return loadingState(project);
  if (project) return pipelinesIn(status, project, busy);
  const storage = storageStatus(status, busy);
  if (PROBLEM.includes(storage)) return { status: storage, label: storageProblemLabel(status, storage), ...owner(notManaged(status?.minio)), next: { kind: "show-issue", part: "storage" }, project: defaultProject(status, projects, null) };
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
 * Copy for the MLflow root link: with workspaces enabled, "/" lists workspaces
 * per project (opendatahub-io/mlflow server/js MlflowRouter: global route,
 * WorkspacesHomeView).
 */
export const MLFLOW_WORKSPACES = "Open MLflow experiments";

export function mlflowState(status: ResourcesStatus | null, busy: Busy = IDLE): FeatureState {
  if (!status) return loadingState();
  const mlflow = status?.mlflow;
  const s = mlflowStatus(status, busy);
  const unmanaged = notManaged(mlflow);
  switch (s) {
    case "ready":
      return { status: s, label: "Ready", ...owner(unmanaged), next: mlflow?.uiRoute ? { kind: "open", label: MLFLOW_WORKSPACES, href: mlflowOpenHref(status.dashboardUrl, mlflow.uiRoute, null) } : null, project: null };
    case "error":
    case "needs-attention":
      return { status: s, label: s === "error" ? "Error" : "Needs attention", ...owner(unmanaged), next: { kind: "show-issue", part: "mlflow" }, project: null };
    case "setting-up":
    case "deleting":
      return { status: s, label: s === "deleting" ? "Deleting" : "Setting up", next: null, project: null };
    case "not-set-up":
      return { status: s, label: "Not set up", next: { kind: "set-up", label: "Set up MLflow", setup: [{ kind: "setup-mlflow" }], reason: status?.mlflowBlocked?.reason ?? mlflow?.setupBlockedReason ?? null }, project: null };
    default:
      return { status: s, label: "Unknown", ...owner(unmanaged), next: null, project: null };
  }
}

// --- Models: cluster inference gateway and per-project model --------------------

const modelsBusy = (busy: Busy, part: "gateway" | "model", project?: string, model?: string): "setting-up" | "deleting" | null => {
  const op = busy.models;
  // Recheck is side-effect free; the model does not enter setup during it.
  if (!op || op.action === "check" || (part === "gateway" ? !op.gateway : op.project !== project || (!!model && !!op.model && op.model !== model))) return null;
  return op.action === "teardown" ? "deleting" : "setting-up";
};

/** Fallback catalog name until status loads (pkg/cluster modelCatalog). */
export const DEFAULT_MODEL = "qwen25-05b-instruct";

/** Catalog entry matching name, if any. */
export function catalogModel(status: ResourcesStatus | null | undefined, name: string): CatalogModel | undefined {
  return (status?.models?.catalog ?? []).find((c) => c.name === name);
}

/** Strip trailing "(CPU, ...)" parenthetical from display names for UI chrome. */
export function shortModelName(status: ResourcesStatus | null | undefined, name: string): string {
  const c = catalogModel(status, name);
  return c ? c.displayName.replace(/\s*\([^)]*\)\s*$/, "") : name;
}

/** Human-readable model kind, e.g. "Chat", "Chat, tool calling", "Embedding (768 dimensions)". */
export function modelTypeWords(c: CatalogModel): string {
  if (c.type === "embedding") return c.embeddingDimension ? `Embedding (${c.embeddingDimension} dimensions)` : "Embedding";
  return c.toolCalling ? "Chat, tool calling" : "Chat";
}

/**
 * Reason setup is blocked for a catalog model in the project, or null when
 * capacity allows it. Redeploying an existing model is never blocked because
 * setup replaces that pod.
 */
export function modelOptionReason(status: ResourcesStatus | null, project: string | null, name: string): string | null {
  const c = catalogModel(status, name);
  if (!c?.capacityWarning) return null;
  if (project && modelIn(status, project, name)?.deployed) return null;
  return `Cannot be set up now. ${c.capacityWarning}`;
}

/**
 * State when a model—perhaps dashboard-deployed—hits this tool's gateway from
 * a namespace absent from the allow list (scoped to project when passed):
 * routing fails, so Ready never applies; Allow is the remediation.
 */
function notAllowedState(status: ResourcesStatus | null, project?: string): FeatureState | null {
  const gw = status?.inferenceGateway;
  if (!gw?.deployed || gw.managedByTool !== true) return null;
  const refs = (gw.notAllowedModels ?? []).filter((r) => !project || r.startsWith(`${project}/`));
  if (refs.length === 0) return null;
  const first = refs[0].split("/")[0];
  return {
    status: "needs-attention",
    label: `${refs[0]} can't be reached${refs.length > 1 ? `, and ${refs.length - 1} more` : ""}`,
    next: { kind: "set-up", label: `Allow ${first}`, setup: [{ kind: "allow-gateway-project", project: first }], reason: null },
    project: first,
  };
}

/** Plain-language instructions mirroring RHOAI docs for gateways we do not manage. */
export const foreignGatewayStep = (project: string) =>
  `A cluster admin adds ${project} to the projects the Gateway's listeners allow (the values array of allowedRoutes, as RHOAI documents it); this tool does not change a gateway it did not create.`;

export function gatewayStatus(status: ResourcesStatus | null, busy: Busy): EnvStatus {
  return envStatus(status?.inferenceGateway, { busy: modelsBusy(busy, "gateway") });
}

/**
 * Model record in a project by catalog name: prefer tool-managed, else any
 * match. Omit name to pick any catalog model in the namespace (tool-managed
 * first).
 */
export function modelIn(status: ResourcesStatus | null, project: string, name?: string): ModelState | undefined {
  const items = (status?.models?.items ?? []).filter((m) => m.namespace === project && (!name || m.name === name));
  return items.find((m) => m.managedByTool !== false) ?? items[0];
}

/** Model catalog names present in the project, sorted like the catalog. */
export function modelNamesIn(status: ResourcesStatus | null, project: string): string[] {
  const names = new Set((status?.models?.items ?? []).filter((m) => m.namespace === project).map((m) => m.name!));
  const order = (status?.models?.catalog ?? []).map((c) => c.name);
  return [...names].sort((a, b) => order.indexOf(a) - order.indexOf(b));
}

/** Latest callability check failed, so invocations would error (or lack auth). */
export function callCheckFailed(m: ModelState | undefined): boolean {
  const check = m?.callCheck;
  return !!check && !check.ok && check.result !== "not-checked";
}

export function modelStatus(status: ResourcesStatus | null, project: string, busy: Busy, name: string = DEFAULT_MODEL): EnvStatus {
  const m = modelIn(status, project, name);
  const s = envStatus(m, { busy: modelsBusy(busy, "model", project, name) });
  // Callable failure downgrades Ready; the card reflects that.
  return s === "ready" && callCheckFailed(m) ? "needs-attention" : s;
}

/** Gateway plus model parts for one catalog model in a project, dependency order. */
export function modelsParts(status: ResourcesStatus | null, project: string, busy: Busy, name: string = DEFAULT_MODEL): PartState[] {
  return [
    { id: "gateway", status: gatewayStatus(status, busy), unmanaged: notManaged(status?.inferenceGateway) },
    { id: "model", status: modelStatus(status, project, busy, name), unmanaged: notManaged(modelIn(status, project, name)) },
  ];
}

/**
 * Default model for the Models tab: URL catalog name if valid, else first
 * deployed model in the project (ready preferred), else catalog default.
 */
export function defaultModelName(status: ResourcesStatus | null, project: string | null, fromUrl: string | null): string {
  const catalog = status?.models?.catalog ?? [];
  if (fromUrl && (catalog.length === 0 || catalog.some((c) => c.name === fromUrl))) return fromUrl;
  const present = project ? modelNamesIn(status, project) : [];
  return present.find((n) => modelIn(status, project!, n)?.ready) ?? present[0] ?? catalog.find((c) => c.default)?.name ?? DEFAULT_MODEL;
}

/** Why no model can be set up on this cluster, or null (unknown is not a reason). */
export function modelsBlockedReason(status: ResourcesStatus | null): string | null {
  return status?.models?.blocked?.reason ?? status?.models?.presetProblem ?? null;
}

/**
 * The dashboard's Deployments page of a project's models, not the project's
 * details page (odh-dashboard rhoai-3.6 packages/model-serving
 * src/components/global/deploymentsPaths.ts deploymentsInternalPath:
 * /ai-hub/models/deployments/internal/<ns>).
 */
export function dashboardProjectHref(dashboardUrl: string | undefined, project: string): string | undefined {
  return dashboardUrl?.startsWith("https://") ? `${dashboardUrl.replace(/\/$/, "")}/ai-hub/models/deployments/internal/${encodeURIComponent(project)}` : undefined;
}

const projectsWithModels = (status: ResourcesStatus | null, projects: string[]) => projects.filter((p) => !!modelIn(status, p));

/** The project the Models tab opens with: the URL's; else the first with a model, preferring a ready one. */
export function defaultModelsProject(status: ResourcesStatus | null, projects: string[], fromUrl: string | null): string | null {
  if (fromUrl) return fromUrl;
  const withModels = projectsWithModels(status, projects);
  return withModels.find((p) => modelIn(status, p)?.ready) ?? withModels[0] ?? null;
}

/**
 * Models in a project: for one model (the tab), or, without one, the
 * project's models together (the Overview): a problem first, then ready,
 * then anything moving.
 */
function modelsIn(status: ResourcesStatus | null, project: string, busy: Busy, name?: string): FeatureState {
  if (!name) {
    const names = modelNamesIn(status, project);
    const running = busy.models?.project === project && busy.models.model ? busy.models.model : null;
    if (running && !names.includes(running)) names.push(running);
    if (names.length === 0) return modelsIn(status, project, busy, defaultModelName(status, project, null));
    const states = names.map((n) => modelsIn(status, project, busy, n));
    return states.find((s) => PROBLEM.includes(s.status)) ?? states.find((s) => MOVING.includes(s.status))
      ?? states.find((s) => s.status === "ready") ?? states[0];
  }
  const short = shortModelName(status, name);
  const parts = modelsParts(status, project, busy, name);
  const [gateway, model] = parts;
  const problem = parts.find((p) => PROBLEM.includes(p.status));
  if (problem) return { status: problem.status, label: `${envStatusText(problem.status)} in ${project}`, ...owner(problem.unmanaged), next: { kind: "show-issue", part: problem.id }, project };
  const notAllowed = notAllowedState(status, project);
  if (notAllowed) return notAllowed;
  const unmanaged = parts.some((p) => p.unmanaged);
  // (A ready model whose last "Can it be called?" failed is a problem
  // above: modelStatus reads it as needs-attention.)
  if (model.status === "ready") {
    const href = dashboardProjectHref(status?.dashboardUrl, project);
    return { status: "ready", label: `Ready in ${project}`, ...owner(unmanaged), next: href ? { kind: "open", label: "Open models in the dashboard", href } : null, project };
  }
  if (model.status === "unknown") return { status: "unknown", label: `Unknown in ${project}`, ...owner(model.unmanaged), next: null, project };
  const moving = parts.find((p) => MOVING.includes(p.status));
  if (moving && parts.every((p) => p.status !== "not-set-up")) {
    return { status: moving.status === "deleting" ? "deleting" : "setting-up", label: moving.status === "deleting" ? `Deleting in ${project}` : `Setting up in ${project}`, next: null, project };
  }
  // A model setup never sets up the inference gateway: how the model is
  // reached is chosen on the tab (the gateway is its own part).
  const setup: Pending[] = [{ kind: "setup-models", gateway: false, project, model: name }];
  let reason: string | null = modelsBlockedReason(status);
  if (!reason && moving && (moving.id === "model" || gateway.status === "deleting")) reason = `The ${moving.id === "gateway" ? "inference gateway" : "model"} is still ${moving.status === "deleting" ? "being deleted" : "setting up"}. Wait until it is done.`;
  if (!reason) reason = modelOptionReason(status, project, name);
  const modelMoving = model.status === "setting-up" || model.status === "deleting";
  return {
    status: modelMoving ? "setting-up" : "not-set-up",
    label: modelMoving ? `Setting up in ${project}` : `Not set up in ${project}`,
    next: { kind: "set-up", label: `Set up ${short} in ${project}`, setup, reason },
    project,
  };
}

/**
 * Models: in a project, for one model when named (the tab), or summarised
 * across the projects (the Overview), as pipelinesState does.
 */
export function modelsState(status: ResourcesStatus | null, project: string | null, projects: string[], busy: Busy = IDLE, model?: string): FeatureState {
  if (!status) return loadingState(project);
  if (project) return modelsIn(status, project, busy, model);
  const gateway = gatewayStatus(status, busy);
  if (PROBLEM.includes(gateway)) {
    return { status: gateway, label: envStatusText(gateway), ...owner(notManaged(status?.inferenceGateway)), next: { kind: "show-issue", part: "gateway" }, project: defaultModelsProject(status, projects, null) };
  }
  const notAllowed = notAllowedState(status);
  if (notAllowed) return notAllowed;
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

/** The ready models Set up can add: chat models, and this tool's embedding models (their dimension is known). */
export function playgroundUsableModels(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return pg?.project === project ? (pg.models ?? []).filter((m) => m.ready && (m.chat || !!m.embeddingDimension)) : [];
}

/** Ready embedding models of the project this tool did not set up: Set up cannot add them (their dimension is unknown). */
export function playgroundOtherEmbeddingModels(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return pg?.project === project ? (pg.models ?? []).filter((m) => m.ready && !m.chat && !m.embeddingDimension && m.source === "namespace") : [];
}

/** What Set up uses by default: the project's own ready chat models, and this tool's ready embedding models (MaaS only when chosen). */
export function defaultPlaygroundModels(pg: PlaygroundStatus | null, project: string): PlaygroundModel[] {
  return playgroundUsableModels(pg, project).filter((m) => m.source === "namespace");
}

/** Models deployed for Gen AI in the project that are not ready: Set up leaves them out (the dashboard's install checks only the models it is given). */
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
  const own = owner(playground.unmanaged);
  if (PROBLEM.includes(playground.status)) return { status: playground.status, label: `${envStatusText(playground.status)} in ${project}`, ...own, next: { kind: "show-issue", part: "playground" }, project };
  if (playground.status === "ready") {
    const href = playgroundHref(status?.dashboardUrl, project);
    return { status: "ready", label: `Ready in ${project}`, ...own, next: href ? { kind: "open", label: "Open the playground", href } : null, project };
  }
  if (MOVING.includes(playground.status)) return { status: playground.status, label: playground.status === "deleting" ? `Deleting in ${project}` : `Setting up in ${project}`, next: null, project };
  if (playground.status === "unknown") return { status: "unknown", label: `Unknown in ${project}`, ...own, next: null, project };
  if (item?.deployed) return { status: "unknown", label: `Unknown in ${project}`, ...own, next: null, project };
  let reason = playgroundClusterReason(pg);
  if (!reason && models.status === "unknown") reason = pg?.modelsProblem ?? "The project's models are still loading.";
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
 * playground status (GET /api/resources/playground), null while it loads
 * (the state is loading then, as it is before status has loaded).
 */
export function playgroundState(status: ResourcesStatus | null, pg: PlaygroundStatus | null, project: string | null, busy: Busy = IDLE): FeatureState {
  if (!status || !pg) return loadingState(project);
  if (project) return playgroundInProject(status, pg, project, busy);
  const projects = [...new Set((pg.items ?? []).filter((i) => i.deployed || i.terminating).map((i) => i.namespace!))];
  const states = projects.map((p) => playgroundInProject(status, pg, p, busy));
  const running = busy.playground ? playgroundInProject(status, pg, busy.playground.project, busy) : null;
  const summary = acrossProjects(states, running);
  if (summary) return summary;
  if (pg.apiProblem) return { status: "unknown", label: "Unknown: the dashboard's gen-ai API cannot be used", next: null, project: null };
  return { status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" }, project: null };
}

// --- Models as a Service: the platform (cluster) and this tool's published models ----------

/** A platform part's state in the page's words. */
export function maasPartStatus(part: MaaSPart): EnvStatus {
  switch (part.state) {
    case "ready":
      return "ready";
    case "missing":
      return "not-set-up";
    case "waiting":
      return "setting-up";
    case "problem":
      return "error";
    case "manual":
      return "needs-attention";
    default:
      return "unknown";
  }
}

/** A part's own label: a ready part with a warning needs attention (the platform's state is maasPlatformStatus's). */
export function maasPartLabelStatus(part: MaaSPart): EnvStatus {
  const s = maasPartStatus(part);
  return s === "ready" && part.warning ? "needs-attention" : s;
}

/** The MaaS platform as a whole. */
export function maasPlatformStatus(m: MaaSStatus | null, busy: Busy): EnvStatus {
  if (!m) return "unknown";
  if (busy.maas?.action === "teardown") return "deleting";
  // Set up and a part's own fix change the platform.
  // (Publishing and the subscription's settings do not.)
  if (busy.maas && !["publish", "unpublish", "restore-routing", "change-limit", "change-access"].includes(busy.maas.action)) return "setting-up";
  const states = m.parts.map(maasPartStatus);
  if (states.every((s) => s === "ready")) return "ready";
  // Not turned on yet, with something setup makes still missing: setup
  // runs (only platform.setupBlockedReason stops it); parts that need a
  // cluster-admin or are still starting show as warnings on the tab.
  if (!m.platform.deployed && states.includes("not-set-up")) return "not-set-up";
  for (const s of ["error", "needs-attention", "setting-up", "unknown"] as EnvStatus[]) {
    if (states.includes(s) && (s !== "unknown" || m.platform.deployed)) return s;
  }
  return "not-set-up";
}

/** The dashboard's MaaS governance page (odh-dashboard rhoai-3.6 packages/maas navigation: /maas/maas-governance). */
export function maasDashboardHref(dashboardUrl: string | undefined): string | undefined {
  return dashboardUrl?.startsWith("https://") ? `${dashboardUrl.replace(/\/$/, "")}/maas/maas-governance` : undefined;
}

/**
 * Models as a Service: a cluster feature (the platform), with this tool's
 * published models. The tab and the Overview both show this.
 */
export function maasState(m: MaaSStatus | null, busy: Busy = IDLE): FeatureState {
  if (!m) return loadingState();
  const status = maasPlatformStatus(m, busy);
  const unmanaged = m.platform.deployed && !m.platform.managedByTool;
  const own = owner(unmanaged);
  const published = (m.items ?? []).filter((i) => i.published && !i.partly);
  if (status === "ready") {
    const what = published.length === 0 ? "no model of this tool published" : `${published.length} ${published.length === 1 ? "model" : "models"} published`;
    const href = maasDashboardHref(m.dashboardUrl);
    return { status, label: `Ready, ${what}`, ...own, next: href ? { kind: "open", label: "Open MaaS governance", href } : null, project: null };
  }
  if (MOVING.includes(status)) return { status, label: status === "deleting" ? "Deleting" : "Setting up", next: null, project: null };
  if (PROBLEM.includes(status)) {
    const part = m.parts.find((p) => PROBLEM.includes(maasPartStatus(p)));
    return { status, label: envStatusText(status), ...own, next: { kind: "show-issue", part: `maas-${part?.id ?? "dsc"}` }, project: null };
  }
  if (status === "unknown") return { status, label: "Unknown", ...own, next: null, project: null };
  return {
    status: "not-set-up",
    label: "Not set up",
    next: { kind: "set-up", label: "Set up MaaS", setup: [{ kind: "setup-maas" }], reason: m.platform.setupBlockedReason ?? null },
    project: null,
  };
}
