/** The RHOAI operator: status, the running operation, updates, reinstalls and refreshes, cluster setup, and usage tracking. */

import { StatusResponse, OperationResponse, NodeSetupAction, NodeSetupPlan,LatestNightlyResponse, NightlyTagsResponse, UserPermissions, UpdateStep, OperationStatusResponse, VersionInfo, UpdateCheck, ClusterSummary, UserOperationStatus } from '../../types';
import { request } from './client';
import { StreamDoneHandler, StreamDetachHandler, streamSSE } from './stream';

export function getStatus(signal?: AbortSignal): Promise<StatusResponse> {
  return request('/api/status', { signal });
}

/**
 * The cluster operation holding the backend lock, if any (cheap; safe to
 * poll). A cluster-admin gets the full answer; any other user only whether
 * one runs, its label and its start (isUserOperation).
 */
export function getOperation(signal?: AbortSignal): Promise<OperationStatusResponse | UserOperationStatus> {
  return request('/api/operation', { signal });
}

/** True for the short answer GET /api/operation gives a user who is not a cluster-admin. */
export function isUserOperation(res: OperationStatusResponse | UserOperationStatus): res is UserOperationStatus {
  return typeof (res as Partial<OperationStatusResponse>).inProgress !== 'boolean' && typeof (res as Partial<UserOperationStatus>).running === 'boolean';
}

/** The read-only cluster summary every signed-in user may see. */
export function getSummary(signal?: AbortSignal): Promise<ClusterSummary> {
  return request('/api/summary', { signal });
}

/** Build and deployment-template information of the running updater. */
export function getVersion(signal?: AbortSignal): Promise<VersionInfo> {
  return request('/api/version', { signal });
}

/** Whether a newer updater release exists (cached by the backend for hours). */
export function getUpdateCheck(signal?: AbortSignal): Promise<UpdateCheck> {
  return request('/api/update-check', { signal });
}

export function updateOperator(image: string, dryRun: boolean): Promise<OperationResponse> {
  return request('/api/update', {
    method: 'POST',
    body: JSON.stringify({ image, dryRun }),
    acceptStatuses: [422],
  });
}

export function testPullSecret(): Promise<OperationResponse> {
  return request('/api/test-pull-secret');
}

export function verifyNodes(signal?: AbortSignal): Promise<OperationResponse> {
  return request('/api/verify-nodes', { signal });
}

/**
 * image by tag: its digest dropped, or null when it has no tag. The node
 * pull check takes only a tag: OLM pulls a digest reference only when the
 * node does not have it yet (InferImagePullPolicy), so a cached image would
 * pass without the nodes' credentials (pkg/cluster NodePullCheckImageProblem).
 */
export function imageByTag(image: string): string | null {
  const ref = image.split('@')[0];
  const lastPart = ref.slice(ref.lastIndexOf('/') + 1);
  return lastPart.includes(':') ? ref : null;
}

/** Runs a temporary catalog pod with image, by its tag, to see whether the nodes can pull it (about a minute). */
export function checkNodePull(image: string): Promise<OperationResponse> {
  const byTag = imageByTag(image);
  if (!byTag) {
    return Promise.resolve({ success: false, errorCode: 'validation', message: `The check needs the image by tag, and ${image} has none.`, logs: [] });
  }
  return request('/api/setup/node-pull-check', {
    method: 'POST',
    body: JSON.stringify({ image: byTag }),
  });
}

/** What a one-click setup action would do: read with your own token, checked by a server-side dry run; changes nothing. */
export function planNodeSetup(action: NodeSetupAction, replaceExisting = false): Promise<NodeSetupPlan> {
  return request('/api/setup/nodes/plan', {
    method: 'POST',
    body: JSON.stringify(replaceExisting ? { action, replaceExisting } : { action }),
  });
}

/**
 * Runs a reviewed setup action, streaming "check", "dry_run", "apply" and
 * "verify"; sends back the plan's object version, and replaceExisting when
 * the user acknowledged replacing an entry this tool did not add.
 */
export function streamNodeSetup(
  plan: Pick<NodeSetupPlan, 'action' | 'resourceVersion' | 'uid' | 'replacing'>,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  const body: Record<string, string | boolean> = { action: plan.action };
  if (plan.resourceVersion) body.resourceVersion = plan.resourceVersion;
  if (plan.uid) body.uid = plan.uid;
  if (plan.replacing) body.replaceExisting = true;
  return streamSSE('/api/setup/nodes/stream', body, onStep, onDone, onConnectionDrop);
}

export function createPullSecret(auth: string): Promise<OperationResponse> {
  return request('/api/setup/pull-secret', {
    method: 'POST',
    body: JSON.stringify({ auth }),
  });
}

export function fetchLatestNightly(signal?: AbortSignal): Promise<LatestNightlyResponse> {
  return request('/api/latest-nightly', { signal });
}

export function fetchNightlyTags(): Promise<NightlyTagsResponse> {
  return request('/api/nightly-tags');
}

export function trackPageView(page: string): void {
  fetch('/api/pageview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ page }),
  }).catch(() => {});
}

export function trackFeature(action: string): void {
  fetch('/api/pageview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action }),
  }).catch(() => {});
}

export function getUserPermissions(): Promise<UserPermissions> {
  return request('/api/user/permissions');
}

/**
 * Confirmations for operator operations. The backend refuses without them
 * and changes nothing (errorCode dashboard_dev_active /
 * downgrade_requires_confirmation).
 */
export interface OperatorOperationOptions {
  /** End an active Dashboard Dev session (resume the dashboard module's operator) first. */
  revertDashboardDev?: boolean;
  /** Reinstall only: accept a target older than the installed operator. */
  allowDowngrade?: boolean;
}

/**
 * Stream an update operation via SSE. Each event is an UpdateStep JSON object.
 * Returns an AbortController so the caller can stop listening.
 */
export function streamUpdate(
  image: string,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
  options: OperatorOperationOptions = {},
): AbortController {
  const body: Record<string, unknown> = { image };
  if (options.revertDashboardDev) body.revertDashboardDev = true;
  return streamSSE('/api/update/stream', body, onStep, onDone, onConnectionDrop);
}

/**
 * Stream a reinstall operation via SSE. Each event is an UpdateStep JSON object.
 * Returns an AbortController so the caller can stop listening.
 */
export function streamReinstall(
  targetType: 'stable' | 'nightly' | 'custom',
  image: string | undefined,
  channel: string | undefined,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
  options: OperatorOperationOptions = {},
): AbortController {
  const body: Record<string, unknown> = { targetType, image, channel: channel || undefined };
  if (options.allowDowngrade) body.allowDowngrade = true;
  if (options.revertDashboardDev) body.revertDashboardDev = true;
  return streamSSE('/api/reinstall/stream', body, onStep, onDone, onConnectionDrop);
}

/**
 * Stream a refresh (re-deploy the same operator version) via SSE.
 * Returns an AbortController so the caller can stop listening.
 */
export function streamRefresh(
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
  options: OperatorOperationOptions = {},
): AbortController {
  const body = options.revertDashboardDev ? { revertDashboardDev: true } : null;
  return streamSSE('/api/refresh/stream', body, onStep, onDone, onConnectionDrop);
}
