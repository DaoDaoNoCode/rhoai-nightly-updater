/** Dashboard Dev: the dashboard's state, PR and main builds, and revert. */

import { OperationResponse, DashboardState, DashboardBuildsResponse } from '../../types';
import { request } from './client';

export function getDashboardState(signal?: AbortSignal): Promise<DashboardState> {
  return request('/api/dashboard/state', { signal });
}

export function revertDashboard(): Promise<OperationResponse> {
  return request('/api/dashboard/revert', {
    method: 'POST',
    acceptStatuses: [422],
  });
}

/** Deploy a dashboard PR build of the given flavor ("rhoai" default, or "odh"). */
export function deployDashboardPR(pr: number, flavor?: string): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-pr', {
    method: 'POST',
    body: JSON.stringify(flavor ? { pr, flavor } : { pr }),
    acceptStatuses: [422],
  });
}

/**
 * The commit of every PR or main build the dashboard runs, its distance from
 * the branch it tracks and whether a newer build exists. Slower than the
 * state (registry and GitHub reads); a GitHub rate limit is not an error.
 * Older backends answer 404.
 */
export function getDashboardBuilds(signal?: AbortSignal): Promise<DashboardBuildsResponse> {
  return request('/api/dashboard/builds', { signal });
}

/** Deploy the latest main dashboard build of the given flavor. */
export function deployDashboardLatestMain(flavor?: string): Promise<OperationResponse> {
  return request('/api/dashboard/deploy-main', {
    method: 'POST',
    body: flavor ? JSON.stringify({ flavor }) : undefined,
    acceptStatuses: [422],
  });
}
