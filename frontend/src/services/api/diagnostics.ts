/** Diagnostics and their fixes. */

import { OperationResponse, DiagnosticResult } from '../../types';
import { request } from './client';

export function getDiagnostics(): Promise<DiagnosticResult> {
  return request('/api/diagnostics');
}

export function fixProblem(problemId: string, acknowledged = false): Promise<OperationResponse> {
  // A fix the user acknowledged goes to its own endpoint; the plain one
  // refuses it with acknowledgement_required.
  if (acknowledged) {
    return request('/api/diagnostics/fix/acknowledged', {
      method: 'POST',
      body: JSON.stringify({ problemId, acknowledged: true }),
      acceptStatuses: [422],
    });
  }
  return request('/api/diagnostics/fix', {
    method: 'POST',
    body: JSON.stringify({ problemId }),
    acceptStatuses: [422],
  });
}
