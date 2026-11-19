/** The HTTP client every API helper uses: errors (ApiError), the session-expired signal and the JSON request. */

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

export const SESSION_EXPIRED_MESSAGE = 'Your OpenShift session has expired. Sign in again to continue.';

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
export function reportIfSessionExpired(e: ApiError): ApiError {
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

export async function request<T>(path: string, options?: RequestOptions): Promise<T> {
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
