/** The Environment page: S3 storage, pipeline servers, MLflow, models, playground, MaaS, samples, prerequisites and clean-up. */

import { OperationResponse, ResourcesStatus, UpdateStep, MLflowSamplesStatus, MLflowSampleAction, MLflowSampleId, PipelineSampleSpec, ModelsRequest, PlaygroundRequest, PlaygroundStatus, MaaSGroupProblem, MaaSRequest, MaaSStatus, ModelExposures, ModelExposureKind, PrerequisitesStatus, PrerequisitesRequest } from '../../types';
import { request } from './client';
import { StreamDoneHandler, StreamDetachHandler, streamSSE } from './stream';

/** The Environment status; project (the Pipelines tab's selected one) also reads its dashboard Connection. */
export function getResourcesStatus(project?: string | null): Promise<ResourcesStatus> {
  return request(project ? `/api/resources/status?project=${encodeURIComponent(project)}` : '/api/resources/status');
}

export function getDSProjects(): Promise<{ projects: string[] | null }> {
  return request('/api/resources/projects');
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

/** Tear down the S3 storage; deleteNamespace also deletes namespace seaweedfs (only when this tool created it). */
export function teardownMinIO(deleteNamespace = false): Promise<OperationResponse> {
  return request('/api/resources/s3/teardown', { method: 'POST', body: JSON.stringify(deleteNamespace ? { deleteNamespace: true } : {}), acceptStatuses: [422] });
}

/**
 * The S3 storage's credentials: the admin UI user and its own password
 * ("" on storage an earlier version set up, until Repair), the S3 access
 * key and the S3 secret key. The server reads them from the storage's
 * Secret on this request only (cluster-admins only, Cache-Control:
 * no-store); keep them out of state, logs and the DOM.
 */
export function getS3AdminLogin(): Promise<{ user: string; password: string; accessKey: string; secretKey: string }> {
  return request('/api/resources/s3/admin-password', { cache: 'no-store' });
}

/** Sets up the pipeline server in project; recreate replaces one named nightly-dspa with dspa. */
export function setupPipelineServer(project: string, recreate = false): Promise<OperationResponse> {
  return request('/api/resources/pipeline-server/setup', {
    method: 'POST',
    body: JSON.stringify(recreate ? { project, recreate: true } : { project }),
    acceptStatuses: [422],
  });
}

export function teardownPipelineServer(project: string): Promise<OperationResponse> {
  return request('/api/resources/pipeline-server/teardown', {
    method: 'POST',
    body: JSON.stringify({ project }),
    acceptStatuses: [422],
  });
}

/** What a Migrate storage run does (pkg/types MigrateStorageRequest). */
export interface MigrateStorageOptions {
  /** The last part: remove the old storage. */
  removeOldStorage?: boolean;
  /** Also delete namespace minio (only with removeOldStorage). */
  deleteNamespace?: boolean;
  /** Projects whose unrecorded pipeline server the user confirmed as this tool's (Migrate only). */
  confirmProjects?: string[];
}

/**
 * Stream Migrate storage via SSE: steps "check", "storage", "copy",
 * "repoint", or with removeOldStorage "check", "copy", "remove",
 * "namespace". Returns an AbortController so the caller can stop listening;
 * the operation continues on the server.
 */
export function streamMigrateStorage(
  options: MigrateStorageOptions,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/s3/migrate/stream', options, onStep, onDone, onConnectionDrop);
}

/** What a clean-up deletes (pkg/types CleanupRequest), in this order. */
export interface CleanupRequest {
  /** The projects whose pipeline server goes. */
  pipelineServers?: string[];
  storage?: boolean;
  /** Also delete namespace seaweedfs (needs storage). */
  deleteNamespace?: boolean;
  mlflow?: boolean;
  /** The projects whose model goes (after MLflow). */
  models?: string[];
  /** The inference gateway, after the models. */
  inferenceGateway?: boolean;
  /** The projects whose playground (this tool's) goes, before the models. */
  playgrounds?: string[];
  /** The projects whose model this tool unpublishes from MaaS, first. */
  maasPublished?: string[];
  /** The MaaS platform this tool set up, last. */
  maasPlatform?: boolean;
  /** One-click prerequisites: the DataScienceCluster components this tool turned on that go back to what they were, last (each only while nothing uses it). */
  components?: string[];
}

/**
 * Clean up: one operation on the server (one lock, one rate-limit slot)
 * that deletes the pipeline servers, then the S3 storage, then MLflow,
 * streaming one step per part ("pipeline-server:<project>", "storage",
 * "mlflow"); the first part that fails stops it.
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
 * Models: sets up the inference gateway and/or a model in a project, or
 * deletes them, as one operation streaming "check", "gateway",
 * "gateway-ready", "model", "model-ready" (teardown: "model", then
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

// --- Models routing ---

/** The ways a catalog model can be reached from a project, and the recommended one. */
export function getModelExposures(project: string, model: string): Promise<ModelExposures> {
  return request(`/api/resources/models/exposures?project=${encodeURIComponent(project)}&model=${encodeURIComponent(model)}`);
}

// --- One-click prerequisites ---

/** What each feature needs on the cluster; Models planned for how its model is reached. */
export function getPrerequisites(exposure?: ModelExposureKind | null): Promise<PrerequisitesStatus> {
  return request(`/api/resources/prerequisites${exposure ? `?exposure=${encodeURIComponent(exposure)}` : ''}`);
}

/** Sets up what a feature needs, streaming "prerequisites", "dsc", one step per operator, "kuadrant", "ready". */
export function streamPrerequisites(
  req: PrerequisitesRequest,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/resources/prerequisites/stream', req, onStep, onDone, onConnectionDrop);
}

/**
 * The Gen AI playground feature: what it needs, the playgrounds in the
 * projects, and the models a project's playground can use (cached on the
 * server for 10 s).
 */
export function getPlayground(project: string | null): Promise<PlaygroundStatus> {
  return request(`/api/resources/playground${project ? `?project=${encodeURIComponent(project)}` : ''}`);
}

/**
 * The playground: sets it up or deletes it through the RHOAI dashboard's
 * gen-ai API, streaming "check", "install", "ready" (teardown: "delete",
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

/** Models as a Service: the platform part by part, and this tool's models with their publication. */
export function getMaaS(): Promise<MaaSStatus> {
  return request('/api/resources/maas');
}

/** Which of the groups typed in a MaaS dialog have no OpenShift Group (cluster-admins only; at most 20 names). */
export function checkMaaSGroups(names: string[]): Promise<MaaSGroupProblem[]> {
  const q = new URLSearchParams();
  names.forEach((n) => q.append('name', n));
  return request(`/api/resources/maas/groups?${q.toString()}`);
}

/**
 * MaaS: sets up or removes the platform ("check", "database", "gateway",
 * "authorino-tls", "dsc", "ready"; removal "check", "unpublish", "dsc",
 * "gateway", "database"), or publishes or unpublishes this tool's model in
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

/** The MLflow samples of one project (named reads only; cached on the server for 15 s). */
export function getMLflowSamples(project: string): Promise<MLflowSamplesStatus> {
  return request(`/api/resources/samples/mlflow?project=${encodeURIComponent(project)}`);
}

/** The body of POST /api/resources/samples/mlflow/stream (pkg/types MLflowSamplesRequest). */
export interface MLflowSamplesRequest {
  project: string;
  action: MLflowSampleAction;
  samples: MLflowSampleId[];
}

/**
 * Adds, removes or regenerates MLflow samples in one project as one
 * operation (one rate-limit slot), streaming "check", then one step per
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

/** The sample pipeline's file, for the user to run with their own token (no cluster request). */
export function getPipelineSampleSpec(): Promise<PipelineSampleSpec> {
  return request('/api/resources/samples/pipeline/spec');
}
