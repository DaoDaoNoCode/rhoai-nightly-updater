/** Diagnostics and their fixes. */

import { OperationResponse, DiagnosticResult, AcknowledgeBinding } from '../../types';
import { request } from './client';

export function getDiagnostics(signal?: AbortSignal): Promise<DiagnosticResult> {
  return request('/api/diagnostics', { signal });
}

/**
 * Runs a problem's fix. With binding (the problem's acknowledgeBinding the
 * user reviewed and acknowledged) it goes to the acknowledged endpoint,
 * which refuses with "conflict" when the object or the statement changed
 * since; the plain one refuses such a fix with acknowledgement_required.
 */
export function fixProblem(problemId: string, binding?: AcknowledgeBinding): Promise<OperationResponse> {
  if (binding) {
    return request('/api/diagnostics/fix/acknowledged', {
      method: 'POST',
      body: JSON.stringify({ problemId, acknowledged: true, binding }),
      acceptStatuses: [422],
    });
  }
  return request('/api/diagnostics/fix', {
    method: 'POST',
    body: JSON.stringify({ problemId }),
    acceptStatuses: [422],
  });
}
