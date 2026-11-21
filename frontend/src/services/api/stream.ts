/** Server-sent event streams of running operations. */

import { UpdateStep } from '../../types';
import { ApiError, CLIENT_ERROR_CODES, SESSION_EXPIRED_MESSAGE, reportIfSessionExpired, parseErrorResponse, toApiError } from './client';

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
        const apiError = reportIfSessionExpired(await parseErrorResponse(resp, controller.signal));
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
