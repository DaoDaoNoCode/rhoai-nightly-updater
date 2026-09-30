import { StatusResponse, OperationResponse, LatestNightlyResponse, NightlyTagsResponse, ComponentsResponse, FBCContentResponse, UserPermissions, DashboardState, ResourcesStatus, UpdateStep, DiagnosticResult } from '../types';

interface RequestOptions extends RequestInit {
  /** HTTP status codes to treat as non-errors (e.g. 422 for validation responses) */
  acceptStatuses?: number[];
}

async function request<T>(path: string, options?: RequestOptions): Promise<T> {
  const { acceptStatuses, ...fetchOptions } = options || {};
  const headers: HeadersInit = { ...fetchOptions?.headers };
  if (fetchOptions?.body) {
    (headers as Record<string, string>)['Content-Type'] = 'application/json';
  }
  const resp = await fetch(path, {
    ...fetchOptions,
    headers,
    signal: fetchOptions?.signal ?? AbortSignal.timeout(120_000),
  });
  if (!resp.ok && !acceptStatuses?.includes(resp.status)) {
    const text = await resp.text();
    try {
      const parsed = JSON.parse(text);
      throw new Error(resp.status + ': ' + (parsed.error || parsed.message || text));
    } catch (e) {
      if (e instanceof Error && e.message.startsWith(String(resp.status) + ':')) throw e;
      throw new Error(`${resp.status}: ${text}`);
    }
  }
  const contentType = resp.headers.get('content-type') || '';
  if (!contentType.includes('application/json')) {
    throw new Error('Session expired. Please refresh the page to re-authenticate.');
  }
  return resp.json();
}

export function getStatus(): Promise<StatusResponse> {
  return request('/api/status');
}

export function updateOperator(image: string, dryRun: boolean): Promise<OperationResponse> {
  return request('/api/update', {
    method: 'POST',
    body: JSON.stringify({ image, dryRun }),
    acceptStatuses: [422],
  });
}

export function refreshOperator(): Promise<OperationResponse> {
  return request('/api/refresh', { method: 'POST', acceptStatuses: [422] });
}

export function reinstallOperator(targetType: 'stable' | 'nightly' | 'custom', image?: string, channel?: string): Promise<OperationResponse> {
  return request('/api/rollback', {
    method: 'POST',
    body: JSON.stringify({ targetType, image, channel: channel || undefined }),
    acceptStatuses: [422],
  });
}

export function testPullSecret(): Promise<OperationResponse> {
  return request('/api/test-pull-secret');
}

export function verifyNodes(): Promise<OperationResponse> {
  return request('/api/verify-nodes');
}

export function createPullSecret(auth: string): Promise<OperationResponse> {
  return request('/api/setup/pull-secret', {
    method: 'POST',
    body: JSON.stringify({ auth }),
  });
}

export function fetchLatestNightly(): Promise<LatestNightlyResponse> {
  return request('/api/latest-nightly');
}

export function fetchNightlyTags(): Promise<NightlyTagsResponse> {
  return request('/api/nightly-tags');
}

export function getComponents(): Promise<ComponentsResponse> {
  return request('/api/components');
}

export function getComponentsWithLabels(): Promise<ComponentsResponse> {
  return request('/api/components?labels=true');
}

export function getBuildExplorerTags(): Promise<NightlyTagsResponse> {
  return request('/api/build-explorer/tags');
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

export function getBuildExplorerContent(image: string, labels?: boolean): Promise<FBCContentResponse> {
  const params = new URLSearchParams({ image });
  if (labels) params.set('labels', 'true');
  return request(`/api/build-explorer/content?${params}`);
}

export function getDashboardState(): Promise<DashboardState> {
  return request('/api/dashboard/state');
}

export function deployPR(pr: number): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-pr', {
    method: 'POST',
    body: JSON.stringify({ pr }),
    acceptStatuses: [422],
  });
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

export function getDSProjects(): Promise<{ projects: string[] }> {
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
 * Shared SSE stream reader. Opens a POST SSE connection, parses UpdateStep
 * events, and invokes the provided callbacks. Returns an AbortController
 * so the caller can cancel.
 *
 * Requires an explicit operation_complete result before reporting success. If the connection drops mid-stream, invokes
 * onConnectionDrop (if provided) so the caller can fall back to polling
 * instead of treating it as a hard failure.
 */
export function streamSSE(
  url: string,
  body: unknown | null,
  onStep: (step: UpdateStep) => void,
  onDone: (success: boolean, error?: string) => void,
  onConnectionDrop?: () => void,
): AbortController {
  const controller = new AbortController();

  const fetchOptions: RequestInit = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    signal: controller.signal,
  };
  if (body !== null) {
    fetchOptions.body = JSON.stringify(body);
  }

  fetch(url, fetchOptions)
    .then(async (resp) => {
      if (!resp.ok) {
        const text = await resp.text();
        throw new Error(`${resp.status}: ${text}`);
      }

      const contentType = resp.headers.get('content-type') || '';
      if (!contentType.includes('text/event-stream')) {
        throw new Error('Session expired. Please refresh the page to re-authenticate.');
      }

      const reader = resp.body?.getReader();
      if (!reader) throw new Error('Streaming not supported');

      const decoder = new TextDecoder();
      let buffer = '';
      let terminal: UpdateStep | undefined;

      try {
        // eslint-disable-next-line no-constant-condition
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });

          const lines = buffer.split('\n');
          buffer = lines.pop() || '';

          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed || trimmed.startsWith(':')) continue;
            if (trimmed.startsWith('data: ')) {
              try {
                const step: UpdateStep = JSON.parse(trimmed.slice(6));
                if (step.step === 'operation_complete') terminal = step;
                onStep(step);
              } catch {
                // skip malformed lines
              }
            }
          }
        }

        // Flush remaining buffer
        if (buffer.trim().startsWith('data: ')) {
          try {
            const step: UpdateStep = JSON.parse(buffer.trim().slice(6));
            if (step.step === 'operation_complete') terminal = step;
            onStep(step);
          } catch {
            // skip
          }
        }

        if (terminal) {
          onDone(terminal.status === 'success', terminal.status === 'failed' ? terminal.message : undefined);
        } else if (onConnectionDrop) {
          onConnectionDrop();
        } else {
          onDone(false, 'Connection ended before the operation result. Refresh status to check progress.');
        }
      } catch {
        // reader.read() rejected - connection dropped mid-stream
        if (controller.signal.aborted) return;
        if (onConnectionDrop) {
          onConnectionDrop();
        } else {
          onDone(false, 'Connection lost during operation.');
        }
      }
    })
    .catch((err) => {
      if (controller.signal.aborted) return;
      // HTTP validation, authorization and lock errors are real failures, not
      // evidence that an operation was accepted and continues in the backend.
      onDone(false, err instanceof Error ? err.message : String(err));
    });

  return controller;
}

/**
 * Stream an update operation via SSE. Each event is an UpdateStep JSON object.
 * Returns an AbortController so the caller can cancel.
 */
export function streamUpdate(
  image: string,
  onStep: (step: UpdateStep) => void,
  onDone: (success: boolean, error?: string) => void,
  onConnectionDrop?: () => void,
): AbortController {
  return streamSSE(
    '/api/update/stream',
    { image },
    onStep,
    onDone,
    onConnectionDrop,
  );
}

/**
 * Stream a reinstall operation via SSE. Each event is an UpdateStep JSON object.
 * Returns an AbortController so the caller can cancel.
 */
export function streamReinstall(
  targetType: 'stable' | 'nightly' | 'custom',
  image: string | undefined,
  channel: string | undefined,
  onStep: (step: UpdateStep) => void,
  onDone: (success: boolean, error?: string) => void,
  onConnectionDrop?: () => void,
): AbortController {
  return streamSSE(
    '/api/rollback/stream',
    { targetType, image, channel: channel || undefined },
    onStep,
    onDone,
    onConnectionDrop,
  );
}

/**
 * Stream a refresh operation via SSE. Each event is an UpdateStep JSON object.
 * Returns an AbortController so the caller can cancel.
 */
export function streamRefresh(
  onStep: (step: UpdateStep) => void,
  onDone: (success: boolean, error?: string) => void,
  onConnectionDrop?: () => void,
): AbortController {
  return streamSSE(
    '/api/refresh/stream',
    null,
    onStep,
    onDone,
    onConnectionDrop,
  );
}

export function assistRollout(): Promise<OperationResponse> {
  return request('/api/assist-rollout', { method: 'POST' });
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

export async function getDSCPreview(): Promise<{ yaml: string; operatorVersion: string; branch: string; sourceURL: string }> {
  return request<{ yaml: string; operatorVersion: string; branch: string; sourceURL: string }>('/api/setup/dsc/preview');
}

export async function createDSC(): Promise<OperationResponse> {
  return request<OperationResponse>('/api/setup/dsc', { method: 'POST' });
}

export function repairDSC(name: string, mode: 'remove-invalid' | 'reset-defaults', expectedOperatorVersion?: string): Promise<OperationResponse> {
  return request('/api/components/dsc/repair', {
    method: 'POST',
    body: JSON.stringify({ name, mode, expectedOperatorVersion }),
    acceptStatuses: [422],
  });
}
