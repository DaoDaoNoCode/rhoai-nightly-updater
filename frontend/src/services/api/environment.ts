/** Environment page coverage: S3 storage, pipeline servers, MLflow, models, playground, MaaS, samples, prerequisites, and clean-up. */

import { OperationResponse, ProjectPipelineServerRequest, ProjectStorageRequest, ProjectStorageStatus, ProjectStatus, ResourcesStatus, UpdateStep, MLflowSamplesStatus, MLflowSampleAction, MLflowSampleId, PipelineSampleSpec, ModelsRequest, PlaygroundRequest, PlaygroundStatus, WorkbenchRequest, WorkbenchStatus, MaaSGroupProblem, MaaSRequest, MaaSStatus, ModelRegistryRequest, ModelRegistryStatus, KueueRequest, KueueStatus, ObservabilityRequest, ObservabilityStatus, ModelExposures, ModelExposureKind, PrerequisitesStatus, PrerequisitesRequest, ProjectCapabilities } from '../../types';
import { request } from './client';
import { StreamDoneHandler, StreamDetachHandler, streamSSE } from './stream';

/** Fetches one project’s status for an ordinary user (not cluster-admin), using that user’s token. */
export function getProjectStatus(project: string): Promise<ProjectStatus> {
  return request(`/api/resources/status?project=${encodeURIComponent(project)}`);
}

/** Overall Environment status; when project is set (Pipelines tab selection), the dashboard Connection is read for that project too. */
export function getResourcesStatus(project?: string | null, signal?: AbortSignal): Promise<ResourcesStatus> {
  return request(project ? `/api/resources/status?project=${encodeURIComponent(project)}` : '/api/resources/status', { signal });
}

export function getDSProjects(signal?: AbortSignal): Promise<{ projects: string[] | null }> {
  return request('/api/resources/projects', { signal });
}

export function setupMLflow(): Promise<OperationResponse> {
  return request('/api/resources/mlflow/setup', { method: 'POST', acceptStatuses: [422] });
}

export function teardownMLflow(): Promise<OperationResponse> {
  return request('/api/resources/mlflow/teardown', { method: 'POST', acceptStatuses: [422] });
}

export function deployMLflowPR(pr: number): Promise<OperationResponse> {
  return request('/api/resources/mlflow/deploy-pr', { method: 'POST', body: JSON.stringify({ pr }), acceptStatuses: [422] });
}

export function revertMLflow(): Promise<OperationResponse> {
  return request('/api/resources/mlflow/revert', { method: 'POST', acceptStatuses: [422] });
}

export function setupMinIO(): Promise<OperationResponse> {
  return request('/api/resources/s3/setup', { method: 'POST', acceptStatuses: [422] });
}

/** Removes S3 storage; deleteNamespace also drops namespace seaweedfs when this tool originally created it. */
export function teardownMinIO(deleteNamespace = false): Promise<OperationResponse> {
  return request('/api/resources/s3/teardown', { method: 'POST', body: JSON.stringify(deleteNamespace ? { deleteNamespace: true } : {}), acceptStatuses: [422] });
}

/**
 * Credentials for S3 storage: admin UI user plus its password
 * (empty string when an older setup left storage until Repair), plus S3 access
 * key and S3 secret key. The backend loads them from the storage
 * Secret for this call only (cluster-admins only, Cache-Control:
 * no-store); do not persist them in state, logs, or the DOM.
 */
export function getS3AdminLogin(): Promise<{ user: string; password: string; accessKey: string; secretKey: string }> {
  return request('/api/resources/s3/admin-password', { cache: 'no-store' });
}

/**
 * Provisions the pipeline server in project; recreate swaps nightly-dspa for dspa.
 * confirmUnrecorded: UID of a pipeline server this tool never tracked that the user acknowledged as theirs (Recreate removes it).
 */
export function setupPipelineServer(project: string, recreate = false, confirmUnrecorded = "", confirmWorkbenches: Record<string, string> = {}): Promise<OperationResponse> {
  return request('/api/resources/pipeline-server/setup', {
    method: 'POST',
    body: JSON.stringify(recreate ? { project, recreate: true, ...(confirmUnrecorded ? { confirmUnrecorded } : {}), ...(Object.keys(confirmWorkbenches).length ? { confirmWorkbenches } : {}) } : { project }),
    acceptStatuses: [422],
  });
}

/**
 * Removes the pipeline server in project; confirmUnrecorded: UID of an unrecorded server the user confirmed as theirs;
 * confirmWorkbenches: the workbenches the user confirmed removing the dashboard Connection from, by name to the UID reviewed (refused when another uses it).
 */
export function teardownPipelineServer(project: string, confirmUnrecorded = "", confirmWorkbenches: Record<string, string> = {}): Promise<OperationResponse> {
  return request('/api/resources/pipeline-server/teardown', {
    method: 'POST',
    body: JSON.stringify({ project, ...(confirmUnrecorded ? { confirmUnrecorded } : {}), ...(Object.keys(confirmWorkbenches).length ? { confirmWorkbenches } : {}) }),
    acceptStatuses: [422],
  });
}

/** Mirrors pkg/types MigrateStorageRequest: what a Migrate storage run performs. */
export interface MigrateStorageOptions {
  /** Final phase: tear down the previous storage. */
  removeOldStorage?: boolean;
  /** When removeOldStorage is set, also remove namespace minio. */
  deleteNamespace?: boolean;
  /** During Migrate only: projects where the user affirmed an unrecorded pipeline server belongs to this tool. */
  confirmProjects?: string[];
}

/**
 * Runs Migrate storage over SSE with steps "check", "storage", "copy",
 * "repoint", or with removeOldStorage "check", "copy", "remove",
 * "namespace". Yields an AbortController so the client can detach;
 * work keeps running on the server.
 */
export function streamMigrateStorage(
  options: MigrateStorageOptions,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/s3/migrate/stream', options, onStep, onDone, onConnectionDrop);
}

/**
 * Runs Secure S3 storage over SSE: steps "check", "rollout" (a new admin key
 * and per-server keys turned on), then "server:<namespace>/<name>" for each pipeline
 * server this tool recorded. Yields an AbortController so the client can
 * detach; work keeps running on the server.
 */
export function streamSecureStorage(
  removeLeftoverKeys: string[],
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/s3/secure/stream', removeLeftoverKeys.length > 0 ? { removeLeftoverKeys } : {}, onStep, onDone, onConnectionDrop);
}

/** Remove leftover keys: deletes the listed SeaweedFS users whose projects still have no pipeline server. */
export function removeLeftoverKeys(users: string[]): Promise<OperationResponse> {
  return request('/api/resources/s3/leftover-keys/remove', { method: 'POST', body: JSON.stringify({ users }), acceptStatuses: [422] });
}

/** Matches pkg/types CleanupRequest: resources clean-up removes, in sequence. */
export interface CleanupRequest {
  /** Projects from which pipeline servers are removed. */
  pipelineServers?: string[];
  /** Subset with unrecorded servers the user attributed to this tool: project mapped to that server’s UID. */
  confirmUnrecorded?: Record<string, string>;
  /** Per project of pipelineServers, the workbenches the user confirmed removing the dashboard Connection from (errorCode confirm_connection_workbenches otherwise). */
  confirmWorkbenches?: Record<string, Record<string, string>>;
  storage?: boolean;
  /** When storage is selected, additionally remove namespace seaweedfs. */
  deleteNamespace?: boolean;
  mlflow?: boolean;
  /** This tool's model registry (with its registered models and its database). */
  modelRegistry?: boolean;
  /** This tool's distributed workloads setup (samples, queues, project label, dashboard opt-in). */
  kueue?: boolean;
  /** The usage logs and dashboards this tool set up (the Observability tab's Clean up); runs before the S3 storage. */
  observability?: boolean;
  /** Projects whose models are removed (after MLflow). */
  models?: string[];
  /** Inference gateway teardown, following the models. */
  inferenceGateway?: boolean;
  /** Projects losing this tool’s playground, before models. */
  playgrounds?: string[];
  /** Projects losing this tool’s workbench (first); workbenchStorage: those whose storage goes too. */
  workbenches?: string[];
  workbenchStorage?: string[];
  /** Projects whose project S3 storage goes (after their pipeline servers); projectStorageData: those whose volume and data go too. */
  projectStorages?: string[];
  projectStorageData?: string[];
  /** Projects whose models this tool unpublishes from MaaS; runs first. */
  maasPublished?: string[];
  /** MaaS platform this tool installed; runs last. */
  maasPlatform?: boolean;
  /** One-click prerequisites: DataScienceCluster components this tool enabled and now revert when unused. */
  components?: string[];
  /**
   * A recipe's Clean up: the enablement of each of components it reviewed (the prerequisites record's ID). One
   * turned on again since is not turned back, checked under the cluster lock right before.
   */
  componentIds?: Record<string, string>;
  /** Recipe-created projects; each is removed after its contents, only while still owned by the recipe. */
  recipeProjects?: string[];
  /**
   * For each recipe project, UID from user review (RecipeOwnedProject uid): deletion happens only while that
   * UID still matches, so a recreated project survives. Mandatory per recipe project.
   */
  recipeProjectUIDs?: Record<string, string>;
  /**
   * A recipe's Clean up of the MaaS platform and the model registry its run
   * created: the run, and the UID of each object reviewed. Each is checked
   * again before its teardown and refused when it was replaced since.
   */
  recipeRunId?: string;
  maasPlatformUid?: string;
  modelRegistryUid?: string;
  /** The ClusterQueue of the distributed workloads setup the recipe's run created. */
  kueueUid?: string;
  /**
   * Per recipe project, non-tool artifacts the refusal path listed
   * (errorCode confirm_project_contents; failed step detail for the project,
   * one line each): the project is removed with them only when all are
   * confirmed for that project.
   */
  confirmProjectContents?: Record<string, string[]>;
}

/**
 * Clean-up is a single server operation (one lock, one rate-limit slot)
 * removing pipeline servers, then S3 storage, then MLflow,
 * emitting one streamed step per segment ("pipeline-server:<project>", "storage",
 * "mlflow"); failure on any segment halts the rest.
 */
export function streamCleanup(
  req: CleanupRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/cleanup/stream', req, onStep, onDone, onConnectionDrop);
}

/**
 * Models flow: provision or remove the inference gateway and/or a project model in
 * one streamed operation ("check", "gateway",
 * "gateway-ready", "model", "model-ready"; teardown streams "model", then
 * "gateway").
 */
export function streamModels(
  req: ModelsRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/models/stream', req, onStep, onDone, onConnectionDrop);
}

// --- Model exposure routes ---

/** Catalog model reachability from a project, including the suggested path. */
export function getModelExposures(project: string, model: string): Promise<ModelExposures> {
  return request(`/api/resources/models/exposures?project=${encodeURIComponent(project)}&model=${encodeURIComponent(model)}`);
}

// --- One-click prerequisite setup ---

/** Cluster requirements per feature; Models also accounts for planned model exposure. */
export function getPrerequisites(exposure?: ModelExposureKind | null, signal?: AbortSignal): Promise<PrerequisitesStatus> {
  return request(`/api/resources/prerequisites${exposure ? `?exposure=${encodeURIComponent(exposure)}` : ''}`, { signal });
}

/** Installs feature prerequisites, streaming "prerequisites", "dsc", one step per operator, "kuadrant", "ready". */
export function streamPrerequisites(
  req: PrerequisitesRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/prerequisites/stream', req, onStep, onDone, onConnectionDrop);
}

/**
 * Gen AI playground: prerequisites, playgrounds per
 * project, and models available to a project playground (server-side cache
 * TTL 10 s).
 */
export function getPlayground(project: string | null, signal?: AbortSignal): Promise<PlaygroundStatus> {
  return request(`/api/resources/playground${project ? `?project=${encodeURIComponent(project)}` : ''}`, { signal });
}

/**
 * Playground install or teardown via the RHOAI dashboard
 * gen-ai API, steps "check", "install", "ready" (teardown: "delete",
 * "gone").
 */
export function streamPlayground(
  req: PlaygroundRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/playground/stream', req, onStep, onDone, onConnectionDrop);
}

/** The Workbench feature: this tool's workbenches, and a project's with what Set up would create (a user gets one of their projects only). */
export function getWorkbench(project: string | null, signal?: AbortSignal): Promise<WorkbenchStatus> {
  return request(`/api/resources/workbench${project ? `?project=${encodeURIComponent(project)}` : ''}`, { signal });
}

/** Set up, start, stop or delete a project's workbench: steps "check", "storage", "workbench" (teardown: "workbench", "storage"). */
export function streamWorkbench(
  req: WorkbenchRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/workbench/stream', req, onStep, onDone, onConnectionDrop);
}

/** A project's own S3 storage, read with the user's token. */
export function getProjectStorage(project: string, signal?: AbortSignal): Promise<ProjectStorageStatus> {
  return request(`/api/resources/project-s3?project=${encodeURIComponent(project)}`, { signal });
}

/** Set up, repair or delete a project's own S3 storage: steps "check", "storage", "fence", "server", "ready" (delete: "check", "delete"). */
export function streamProjectStorage(
  req: ProjectStorageRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/project-s3/stream', req, onStep, onDone, onConnectionDrop);
}

/** Set up or delete a project's pipeline server on its own S3 storage: step "pipeline-server". */
export function streamProjectPipelineServer(
  req: ProjectPipelineServerRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/project-pipeline-server/stream', req, onStep, onDone, onConnectionDrop);
}

/** Models as a Service: platform pieces and this tool’s models with publication state. */
export function getMaaS(signal?: AbortSignal): Promise<MaaSStatus> {
  return request('/api/resources/maas', { signal });
}

/** For MaaS dialog group names: which lack an OpenShift Group (cluster-admins only; cap 20 names). */
export function checkMaaSGroups(names: string[]): Promise<MaaSGroupProblem[]> {
  const q = new URLSearchParams();
  names.forEach((n) => q.append('name', n));
  return request(`/api/resources/maas/groups?${q.toString()}`);
}

/**
 * MaaS platform install or removal ("check", "database", "gateway",
 * "authorino-tls", "dsc", "ready"; removal "check", "unpublish", "dsc",
 * "gateway", "database"), or publish/unpublish this tool’s model within
 * a project.
 */
export function streamMaaS(
  req: MaaSRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/maas/stream', req, onStep, onDone, onConnectionDrop);
}

/** MLflow samples in one project (named query only; server cache 15 s). */
export function getMLflowSamples(project: string, signal?: AbortSignal): Promise<MLflowSamplesStatus> {
  return request(`/api/resources/samples/mlflow?project=${encodeURIComponent(project)}`, { signal });
}

/** Payload shape for POST /api/resources/samples/mlflow/stream (pkg/types MLflowSamplesRequest). */
export interface MLflowSamplesRequest {
  project: string;
  action: MLflowSampleAction;
  samples: MLflowSampleId[];
}

/**
 * Add, remove, or regenerate MLflow samples in one project as a
 * single rate-limited operation, streaming "check" then one step per
 * sample id.
 */
export function streamMLflowSamples(
  req: MLflowSamplesRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/samples/mlflow/stream', req, onStep, onDone, onConnectionDrop);
}

/** Sample pipeline specification for the user to execute with their own token (no cluster round-trip). */
export function getPipelineSampleSpec(): Promise<PipelineSampleSpec> {
  return request('/api/resources/samples/pipeline/spec');
}
// --- Model registry ---

/** This tool's model registry (GET /api/resources/model-registry), read with the user's own token. */
export function getModelRegistry(signal?: AbortSignal): Promise<ModelRegistryStatus> {
  return request('/api/resources/model-registry', { signal });
}

/**
 * Sets up the model registry ("check", "create", "ready", "samples") or
 * deletes it ("check", "delete", "gone") as one streamed operation.
 */
export function streamModelRegistry(
  req: ModelRegistryRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/model-registry/stream', req, onStep, onDone, onConnectionDrop);
}

// --- Distributed workloads ---

/** The Distributed workloads tab (GET /api/resources/kueue), read with the admin's own token. */
export function getKueue(project: string | null, signal?: AbortSignal): Promise<KueueStatus> {
  return request(`/api/resources/kueue${project ? `?project=${encodeURIComponent(project)}` : ''}`, { signal });
}

/**
 * Sets up ("check", "queues", "project", "samples"), deletes ("samples",
 * "queues", "project", "opt-in"), deletes a sample ("samples") or turns the
 * dashboard opt-in on ("check", "dashboard", "profile") or off ("opt-in").
 */
export function streamKueue(
  req: KueueRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/kueue/stream', req, onStep, onDone, onConnectionDrop);
}

/** The Observability tab (GET /api/resources/observability): usage logs and dashboards, part by part. */
export function getObservability(signal?: AbortSignal): Promise<ObservabilityStatus> {
  return request('/api/resources/observability', { signal });
}

/**
 * Sets up ("observability-check", the operators' steps, "usage-logs-storage",
 * "monitoring-settings", "monitoring-ready") or cleans up
 * ("observability-check", "monitoring-settings", "monitoring-gc",
 * "monitoring-leftovers", "usage-logs-storage") the usage logs and dashboards.
 */
export function streamObservability(
  req: ObservabilityRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/observability/stream', req, onStep, onDone, onConnectionDrop);
}

/**
 * What the signed-in user may do in one of their projects: per project
 * action, allowed or why not (their RBAC, read with their token, and the
 * cluster's prerequisites). Advisory: each action checks again.
 */
export function getProjectCapabilities(project: string, signal?: AbortSignal): Promise<ProjectCapabilities> {
  return request(`/api/resources/projects/capabilities?project=${encodeURIComponent(project)}`, { signal });
}
