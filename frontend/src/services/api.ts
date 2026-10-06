import { StatusResponse, OperationResponse, LatestNightlyResponse, NightlyTagsResponse, ComponentsResponse, FBCContentResponse, UserPermissions, DashboardState, ResourcesStatus, UpdateStep, DiagnosticResult, OperationStatusResponse, VersionInfo, PRContainsResponse } from '../types';

/**
 * Error thrown by every API helper. `status` is the HTTP status (0 when no
 * response arrived), `errorCode` is the backend's machine-readable code from
 * the JSON error body (writeError in pkg/api) or a client-side code, and
 * `details` keeps the parsed body. Classify on status and errorCode, never on
 * the message text: messages can echo user input such as an image digest.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly errorCode: string;
  readonly details?: unknown;

  constructor(init: { status: number; errorCode: string; message: string; details?: unknown }) {
    super(init.message);
    this.name = 'ApiError';
    this.status = init.status;
    this.errorCode = init.errorCode;
    this.details = init.details;
  }
}

/** Error codes produced in the browser, for failures without a backend errorCode. */
export const CLIENT_ERROR_CODES = {
  network: 'network',
  timeout: 'timeout',
  aborted: 'aborted',
  sessionExpired: 'session_expired',
  operationFailed: 'operation_failed',
  unknown: 'unknown',
} as const;

const SESSION_EXPIRED_MESSAGE = 'Your OpenShift session has expired. Sign in again to continue.';

/**
 * True when the error means the user's OpenShift session is gone: the
 * backend's 401 session_expired (the API server rejected the user's token,
 * pkg/api writeAuthError) or oauth-proxy's HTML sign-in page instead of JSON.
 * Other 401s (for example the app's own ServiceAccount being rejected) are
 * not the user's session and are shown as ordinary errors.
 */
export function isSessionExpired(e: unknown): boolean {
  return isApiError(e) && e.errorCode === CLIENT_ERROR_CODES.sessionExpired;
}

type SessionListener = () => void;
const sessionListeners = new Set<SessionListener>();

/** Subscribe to "the session expired" (any API call got a 401). Returns the unsubscribe function. */
export function onSessionExpired(listener: SessionListener): () => void {
  sessionListeners.add(listener);
  return () => sessionListeners.delete(listener);
}

/** Tell every subscriber once per error that the session is gone. */
function reportIfSessionExpired(e: ApiError): ApiError {
  if (isSessionExpired(e)) sessionListeners.forEach((l) => l());
  return e;
}

/** Mirrors defaultErrorCode in pkg/api/handlers.go, for bodies without an errorCode. */
function defaultErrorCode(status: number): string {
  switch (status) {
    case 400: return 'bad_request';
    case 401: return 'unauthorized';
    case 403: return 'forbidden';
    case 404: return 'not_found';
    case 409: return 'conflict';
    case 422: return 'unprocessable';
    case 429: return 'rate_limited';
    case 503: return 'unavailable';
    default: return 'internal';
  }
}

/** True for ApiError instances, including ones created in another JS realm. */
export function isApiError(e: unknown): e is ApiError {
  if (e instanceof ApiError) return true;
  if (typeof e !== 'object' || e === null) return false;
  const candidate = e as { name?: unknown; status?: unknown; errorCode?: unknown };
  return candidate.name === 'ApiError' && typeof candidate.status === 'number' && typeof candidate.errorCode === 'string';
}

/**
 * Build an ApiError from a non-OK response. The backend always answers with
 * {"error", "errorCode"} JSON; proxies and the router may answer with text or HTML.
 */
export async function parseErrorResponse(resp: Response): Promise<ApiError> {
  let text = '';
  try {
    text = await resp.text();
  } catch {
    // The body is optional for classification.
  }
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = undefined;
  }
  if (parsed && typeof parsed === 'object') {
    const body = parsed as { error?: unknown; message?: unknown; errorCode?: unknown };
    const message = typeof body.error === 'string' && body.error ? body.error
      : typeof body.message === 'string' && body.message ? body.message
        : `Request failed (HTTP ${resp.status})`;
    const errorCode = typeof body.errorCode === 'string' && body.errorCode ? body.errorCode : defaultErrorCode(resp.status);
    return new ApiError({ status: resp.status, errorCode, message, details: parsed });
  }
  if (resp.status === 401 || resp.status === 403) {
    // The backend answers every error with JSON (writeError in pkg/api), so a
    // 401/403 without a JSON body comes from oauth-proxy in front of it. With
    // a missing or expired session cookie it serves its sign-in page as
    // HTTP 403 text/html (checked on the live route); that is an expired
    // session, not a permission answer.
    return new ApiError({ status: resp.status, errorCode: CLIENT_ERROR_CODES.sessionExpired, message: SESSION_EXPIRED_MESSAGE });
  }
  const contentType = resp.headers.get('content-type') || '';
  const trimmed = text.trim();
  const message = !trimmed || contentType.includes('text/html')
    ? `Request failed (HTTP ${resp.status})`
    : `${resp.status}: ${trimmed.slice(0, 300)}`;
  return new ApiError({ status: resp.status, errorCode: defaultErrorCode(resp.status), message, details: trimmed || undefined });
}

/** Normalize anything thrown by fetch or an API helper into an ApiError. */
export function toApiError(e: unknown, fallbackMessage = 'Request failed'): ApiError {
  if (isApiError(e)) return e;
  const name = typeof e === 'object' && e !== null ? (e as { name?: unknown }).name : undefined;
  const rawMessage = typeof e === 'object' && e !== null && typeof (e as { message?: unknown }).message === 'string'
    ? (e as { message: string }).message
    : typeof e === 'string' ? e : '';
  if (name === 'TimeoutError') {
    return new ApiError({ status: 0, errorCode: CLIENT_ERROR_CODES.timeout, message: 'The request timed out. The server may be busy; try again.' });
  }
  if (name === 'AbortError') {
    return new ApiError({ status: 0, errorCode: CLIENT_ERROR_CODES.aborted, message: 'The request was cancelled.' });
  }
  if (name === 'TypeError') {
    // fetch() rejects with a TypeError when no response arrives at all.
    return new ApiError({
      status: 0,
      errorCode: CLIENT_ERROR_CODES.network,
      message: 'Cannot reach the updater server. Check your connection and try again.',
      details: rawMessage || undefined,
    });
  }
  return new ApiError({ status: 0, errorCode: CLIENT_ERROR_CODES.unknown, message: rawMessage || fallbackMessage });
}

/** The message to show for any caught error. */
export function errorMessage(e: unknown, fallbackMessage = 'Request failed'): string {
  return toApiError(e, fallbackMessage).message;
}

function withTimeout(signal: AbortSignal | null | undefined, ms: number): AbortSignal {
  const timeout = AbortSignal.timeout(ms);
  if (!signal) return timeout;
  // Keep both the caller's cancellation and the timeout where supported.
  return typeof AbortSignal.any === 'function' ? AbortSignal.any([signal, timeout]) : signal;
}

interface RequestOptions extends RequestInit {
  /** HTTP status codes to treat as non-errors (e.g. 422 for validation responses) */
  acceptStatuses?: number[];
}

async function request<T>(path: string, options?: RequestOptions): Promise<T> {
  const { acceptStatuses, ...fetchOptions } = options || {};
  const headers: Record<string, string> = { ...(fetchOptions?.headers as Record<string, string> | undefined) };
  const method = (fetchOptions?.method || 'GET').toUpperCase();
  // The backend requires JSON on every state-changing request (CSRF guard),
  // including POSTs without a body.
  if (fetchOptions?.body || (method !== 'GET' && method !== 'HEAD')) {
    headers['Content-Type'] = 'application/json';
  }
  let resp: Response;
  try {
    resp = await fetch(path, {
      ...fetchOptions,
      headers,
      signal: withTimeout(fetchOptions?.signal, 120_000),
    });
  } catch (e) {
    throw toApiError(e);
  }
  if (!resp.ok && !acceptStatuses?.includes(resp.status)) {
    throw reportIfSessionExpired(await parseErrorResponse(resp));
  }
  const contentType = resp.headers.get('content-type') || '';
  if (!contentType.includes('application/json')) {
    // oauth-proxy answers an expired session with its HTML sign-in page.
    throw reportIfSessionExpired(new ApiError({ status: resp.status, errorCode: CLIENT_ERROR_CODES.sessionExpired, message: SESSION_EXPIRED_MESSAGE }));
  }
  try {
    return await resp.json();
  } catch (e) {
    throw toApiError(e, 'The server sent an unreadable response.');
  }
}

export function getStatus(signal?: AbortSignal): Promise<StatusResponse> {
  return request('/api/status', { signal });
}

/** The cluster operation holding the backend lock, if any (cheap; safe to poll). */
export function getOperation(signal?: AbortSignal): Promise<OperationStatusResponse> {
  return request('/api/operation', { signal });
}

/** Build and deployment-template information of the running updater. */
export function getVersion(signal?: AbortSignal): Promise<VersionInfo> {
  return request('/api/version', { signal });
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

export function getComponents(signal?: AbortSignal): Promise<ComponentsResponse> {
  return request('/api/components', { signal });
}

export function getComponentsWithLabels(signal?: AbortSignal): Promise<ComponentsResponse> {
  return request('/api/components?labels=true', { signal });
}

export function getBuildExplorerTags(includeDates = false): Promise<NightlyTagsResponse> {
  return request('/api/build-explorer/tags?includeDates=' + includeDates);
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

export function getBuildExplorerContent(image: string, labels?: boolean, signal?: AbortSignal): Promise<FBCContentResponse> {
  const params = new URLSearchParams({ image });
  if (labels) params.set('labels', 'true');
  return request(`/api/build-explorer/content?${params}`, { signal });
}

export function getDashboardState(signal?: AbortSignal): Promise<DashboardState> {
  return request('/api/dashboard/state', { signal });
}

export function deployPR(pr: number): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-pr', {
    method: 'POST',
    body: JSON.stringify({ pr }),
    acceptStatuses: [422],
  });
}

export function deployDashboardMain(): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-main', { method: 'POST', acceptStatuses: [422] });
}

export function revertDashboard(): Promise<OperationResponse> {
  return request('/api/dashboard/revert', {
    method: 'POST',
    acceptStatuses: [422],
  });
}

export function getResourcesStatus(): Promise<ResourcesStatus> {
  return request('/api/resources/status');
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
  return request('/api/resources/minio/setup', { method: 'POST', acceptStatuses: [422] });
}

export function teardownMinIO(): Promise<OperationResponse> {
  return request('/api/resources/minio/teardown', { method: 'POST', acceptStatuses: [422] });
}

export function setupPipelineServer(project: string): Promise<OperationResponse> {
  return request('/api/resources/pipeline-server/setup', {
    method: 'POST',
    body: JSON.stringify({ project }),
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

/**
 * Why a stream ended without an operation result. The backend keeps running
 * the operation after the client goes away (context.WithoutCancel in pkg/api),
 * so in every case the caller should fall back to status polling.
 * - aborted: the caller aborted the controller.
 * - connection_lost: reading the response body failed.
 * - ended_without_result: the body ended before operation_complete.
 * - stalled: no bytes (not even the 15 s heartbeat) arrived for the idle timeout.
 */
export type StreamDetachReason = 'aborted' | 'connection_lost' | 'ended_without_result' | 'stalled';

export type StreamDoneHandler = (success: boolean, error?: string, apiError?: ApiError) => void;
export type StreamDetachHandler = (reason: StreamDetachReason) => void;

export interface StreamOptions {
  /** Abort and report `stalled` when nothing arrives for this long (default 90 s). */
  idleTimeoutMs?: number;
}

/** The backend sends a heartbeat every 15 s, so 90 s of silence means the connection is gone. */
export const STREAM_IDLE_TIMEOUT_MS = 90_000;

/**
 * Shared SSE stream reader. Opens a POST SSE connection, parses UpdateStep
 * events, and invokes the provided callbacks. Returns an AbortController
 * so the caller can stop listening.
 *
 * Exactly one of onDone and onConnectionDrop is called, exactly once, for
 * every stream, including when the controller is aborted. onDone(true) needs
 * an explicit operation_complete success event. HTTP rejections (400, 403,
 * 409 cluster_busy, 429, 503) and a failed operation_complete call
 * onDone(false, message, apiError) with the backend's errorCode. Anything
 * that ends the stream without a result calls onConnectionDrop(reason); without
 * an onConnectionDrop handler it becomes onDone(false, message).
 */
export function streamSSE(
  url: string,
  body: unknown | null,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
  options: StreamOptions = {},
): AbortController {
  const controller = new AbortController();
  const idleTimeoutMs = options.idleTimeoutMs ?? STREAM_IDLE_TIMEOUT_MS;
  let settled = false;
  let detachReason: StreamDetachReason | null = null;
  let idleTimer: ReturnType<typeof setTimeout> | undefined;

  const clearIdle = () => {
    if (idleTimer !== undefined) clearTimeout(idleTimer);
    idleTimer = undefined;
  };
  const armIdle = () => {
    clearIdle();
    idleTimer = setTimeout(() => {
      if (settled) return;
      detachReason = 'stalled';
      controller.abort();
    }, idleTimeoutMs);
  };

  const finishDone = (success: boolean, error?: string, apiError?: ApiError) => {
    if (settled) return;
    settled = true;
    clearIdle();
    onDone(success, error, apiError);
  };
  const finishDetached = (reason: StreamDetachReason) => {
    if (settled) return;
    settled = true;
    clearIdle();
    if (onConnectionDrop) {
      onConnectionDrop(reason);
    } else {
      onDone(false, reason === 'ended_without_result'
        ? 'Connection ended before the operation result. Refresh status to check progress.'
        : 'Lost the connection during the operation. It continues on the server; refresh status to check progress.');
    }
  };

  // Aborting must still end the stream for the caller; otherwise the UI waits
  // for a result that never comes.
  controller.signal.addEventListener('abort', () => finishDetached(detachReason ?? 'aborted'));

  const fetchOptions: RequestInit = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    signal: controller.signal,
  };
  if (body !== null) {
    fetchOptions.body = JSON.stringify(body);
  }

  const handleLine = (line: string, setTerminal: (step: UpdateStep) => void) => {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith(':') || !trimmed.startsWith('data: ')) return;
    let step: UpdateStep;
    try {
      step = JSON.parse(trimmed.slice(6));
    } catch {
      return; // skip malformed lines
    }
    if (step.step === 'operation_complete') setTerminal(step);
    if (!settled) onStep(step);
  };

  armIdle();
  fetch(url, fetchOptions)
    .then(async (resp) => {
      if (!resp.ok) {
        const apiError = reportIfSessionExpired(await parseErrorResponse(resp));
        finishDone(false, apiError.message, apiError);
        return;
      }

      const contentType = resp.headers.get('content-type') || '';
      if (!contentType.includes('text/event-stream')) {
        const apiError = reportIfSessionExpired(new ApiError({ status: resp.status, errorCode: CLIENT_ERROR_CODES.sessionExpired, message: SESSION_EXPIRED_MESSAGE }));
        finishDone(false, apiError.message, apiError);
        return;
      }

      const reader = resp.body?.getReader();
      if (!reader) {
        const apiError = new ApiError({ status: resp.status, errorCode: CLIENT_ERROR_CODES.unknown, message: 'Streaming is not supported by this browser.' });
        finishDone(false, apiError.message, apiError);
        return;
      }

      const decoder = new TextDecoder();
      let buffer = '';
      let terminal: UpdateStep | undefined;
      const setTerminal = (step: UpdateStep) => { terminal = step; };

      try {
        while (!settled) {
          const { done, value } = await reader.read();
          if (done) break;
          armIdle();
          buffer += decoder.decode(value, { stream: true });

          const lines = buffer.split('\n');
          buffer = lines.pop() || '';
          for (const line of lines) handleLine(line, setTerminal);
        }
      } catch {
        // reader.read() rejected: the connection dropped or was aborted.
        finishDetached(controller.signal.aborted ? (detachReason ?? 'aborted') : 'connection_lost');
        return;
      }
      if (settled) return;

      // Flush the remaining buffer.
      handleLine(buffer, setTerminal);

      if (terminal) {
        const finalStep: UpdateStep = terminal;
        if (finalStep.status === 'success') {
          finishDone(true);
        } else {
          const apiError = new ApiError({
            status: resp.status,
            errorCode: finalStep.errorCode || CLIENT_ERROR_CODES.operationFailed,
            message: finalStep.message || 'The operation failed.',
            details: finalStep,
          });
          finishDone(false, apiError.message, apiError);
        }
      } else {
        finishDetached('ended_without_result');
      }
    })
    .catch((err) => {
      if (controller.signal.aborted) {
        finishDetached(detachReason ?? 'aborted');
        return;
      }
      // No response at all: the request may never have reached the backend,
      // so this is a failure, not evidence that an operation is running.
      const apiError = toApiError(err, 'Could not start the operation.');
      finishDone(false, apiError.message, apiError);
    });

  return controller;
}

/**
 * Confirmations for operator operations. The backend refuses without them
 * and changes nothing (errorCode dashboard_dev_active /
 * downgrade_requires_confirmation).
 */
export interface OperatorOperationOptions {
  /** End an active Dashboard Dev session (resume dashboard-operator) first. */
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
  return streamSSE('/api/rollback/stream', body, onStep, onDone, onConnectionDrop);
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

/**
 * Unblock stuck rollouts. With a target only that Deployment is changed.
 * "Nothing to do" is a 422 with errorCode nothing_to_do, returned as a
 * result rather than thrown.
 */
export function assistRollout(target?: { namespace: string; deployment: string }): Promise<OperationResponse> {
  return request('/api/assist-rollout', {
    method: 'POST',
    body: target ? JSON.stringify(target) : undefined,
    acceptStatuses: [422],
  });
}

export function getDiagnostics(): Promise<DiagnosticResult> {
  return request('/api/diagnostics');
}

export function fixProblem(problemId: string): Promise<OperationResponse> {
  return request('/api/diagnostics/fix', {
    method: 'POST',
    body: JSON.stringify({ problemId }),
    acceptStatuses: [422],
  });
}

export interface DSCPreviewResponse {
  yaml: string;
  operatorVersion: string;
  branch: string;
  sourceURL: string;
}

export async function getDSCPreview(): Promise<DSCPreviewResponse> {
  return request<DSCPreviewResponse>('/api/setup/dsc/preview');
}

export async function createDSC(): Promise<OperationResponse> {
  return request<OperationResponse>('/api/setup/dsc', { method: 'POST' });
}

export function repairDSC(name: string, mode: 'remove-invalid' | 'remove-extra-components' | 'reset-defaults', expectedOperatorVersion?: string, expectedExtraComponents?: string[]): Promise<OperationResponse> {
  return request('/api/components/dsc/repair', {
    method: 'POST',
    body: JSON.stringify({ name, mode, expectedOperatorVersion, expectedExtraComponents }),
  });
}

// ---------------------------------------------------------------------------
// Secondary pages (Components, Dashboard Dev, Diagnostics): additive helpers
// for the backend contracts of B2 and B4.
// ---------------------------------------------------------------------------

export interface DSCPreviewResponse {
  /** "csv" (alm-examples of the installed CSV) or "github". */
  source?: string;
  sourceDescription?: string;
}

/**
 * Assist one stuck rollout (B2). With a target, only that Deployment is
 * patched; "nothing to do" and refusals come back as 422 OperationResponse
 * bodies with an errorCode.
 */
export function assistRolloutFor(target?: { namespace: string; deployment: string }): Promise<OperationResponse> {
  return request('/api/assist-rollout', {
    method: 'POST',
    body: target ? JSON.stringify(target) : undefined,
    acceptStatuses: [422],
  });
}

/** Deploy a dashboard PR build of the given flavor ("rhoai" default, or "odh") (B4). */
export function deployDashboardPR(pr: number, flavor?: string): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-pr', {
    method: 'POST',
    body: JSON.stringify(flavor ? { pr, flavor } : { pr }),
    acceptStatuses: [422],
  });
}

/**
 * Whether the given builds contain merged PR `pr` of opendatahub-io/odh-dashboard.
 * Errors: 422 pr_not_merged, 404 pr_not_found, 400 validation. A GitHub rate
 * limit is not an error: the response has rateLimited and unknown builds.
 */
export function getPRContains(pr: number, images: string[], signal?: AbortSignal): Promise<PRContainsResponse> {
  const params = new URLSearchParams({ pr: String(pr) });
  for (const image of images) params.append('image', image);
  return request(`/api/build-explorer/contains?${params}`, { signal });
}

/** Deploy the latest main dashboard build of the given flavor (B4). */
export function deployDashboardLatestMain(flavor?: string): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-main', {
    method: 'POST',
    body: flavor ? JSON.stringify({ flavor }) : undefined,
    acceptStatuses: [422],
  });
}
