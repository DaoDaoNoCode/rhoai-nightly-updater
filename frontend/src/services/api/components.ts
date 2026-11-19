/** The Components page: DSC components and the DataScienceCluster manager, and rollout assistance. */

import { OperationResponse, ComponentsResponse, UpdateStep, DSCManager, DSCPlan, DSCPlanAction } from '../../types';
import { request } from './client';
import { StreamDoneHandler, StreamDetachHandler, streamSSE } from './stream';

export function getComponents(signal?: AbortSignal): Promise<ComponentsResponse> {
  return request('/api/components', { signal });
}

export function getComponentsWithLabels(signal?: AbortSignal): Promise<ComponentsResponse> {
  return request('/api/components?labels=true', { signal });
}

export interface DSCPreviewResponse {
  yaml: string;
  /** The defaults' apiVersion: the version the DSC is read at, or the newest served one without a DSC. */
  apiVersion: string;
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

/**
 * Repairs the DSC as previewed. expectedAPIVersion is the apiVersion the preview was computed at; a reset also
 * sends the management paths it listed as switched to Removed (expectedResetRemovals, [] for none). The backend
 * refuses, changing nothing, when either differs from what it would do now.
 */
export function repairDSC(name: string, mode: 'remove-invalid' | 'remove-extra-components' | 'reset-defaults', expectedAPIVersion: string | undefined, expectedOperatorVersion?: string, expectedExtraComponents?: string[], expectedResetRemovals?: string[]): Promise<OperationResponse> {
  return request('/api/components/dsc/repair', {
    method: 'POST',
    body: JSON.stringify({ name, mode, expectedAPIVersion, expectedOperatorVersion, expectedExtraComponents, expectedResetRemovals }),
  });
}

/** The DataScienceCluster's component tree, compared with the release default. */
export function getDSCManager(): Promise<DSCManager> {
  return request<DSCManager>('/api/components/dsc');
}

/** What a change would do, from a fresh read: a toggle (path, to, withParent), fix, soft-reset or hard-reset. */
export function getDSCPlan(action: DSCPlanAction, toggle?: { path: string; to: string; withParent?: boolean }, reset?: { keepEnabled: boolean }): Promise<DSCPlan> {
  const q = new URLSearchParams({ action });
  if (toggle) {
    q.set('path', toggle.path);
    q.set('to', toggle.to);
    if (toggle.withParent) q.set('withParent', 'true');
  }
  // A reset says whether it keeps the components the user turned on (required).
  if (reset) q.set('keepEnabled', String(reset.keepEnabled));
  return request<DSCPlan>(`/api/components/dsc/plan?${q.toString()}`);
}

/** Sets one managementState as the reviewed plan (uid, token); refused (422) when the DSC no longer holds `from` or the plan changed. */
export function setDSCManagementState(req: { name: string; path: string; from: string; to: string; withParent?: boolean; uid: string; token: string }): Promise<OperationResponse> {
  return request('/api/components/dsc/management', { method: 'POST', body: JSON.stringify(req), acceptStatuses: [422] });
}

/** Creates the DataScienceCluster a stopped hard reset recorded, while none exists. */
export function finishDSCHardReset(name: string, token: string): Promise<OperationResponse> {
  return request('/api/components/dsc/hard-reset/finish', { method: 'POST', body: JSON.stringify({ name, token }), acceptStatuses: [422] });
}

/** Runs the reviewed fix or soft reset (with the keepEnabled it was planned with); refused when the plan changed since. */
export function runDSCPlan(action: 'fix' | 'soft-reset', plan: DSCPlan): Promise<OperationResponse> {
  const body = action === 'fix'
    ? { name: plan.name, uid: plan.uid, token: plan.token }
    : { name: plan.name, uid: plan.uid, token: plan.token, keepEnabled: plan.keepEnabled === true };
  return request(action === 'fix' ? '/api/components/dsc/fix' : '/api/components/dsc/reset', {
    method: 'POST', body: JSON.stringify(body), acceptStatuses: [422],
  });
}

/** Hard reset, streaming "check", "delete", "wait_deleted", "create", "wait_ready". */
export function streamDSCHardReset(
  plan: DSCPlan,
  confirm: string,
  onStep: (step: UpdateStep) => void,
  onDone: StreamDoneHandler,
  onConnectionDrop?: StreamDetachHandler,
): AbortController {
  return streamSSE('/api/components/dsc/hard-reset/stream', { name: plan.name, uid: plan.uid, token: plan.token, confirm, keepEnabled: plan.keepEnabled === true }, onStep, onDone, onConnectionDrop);
}

// ---------------------------------------------------------------------------
// Secondary pages: Components, Dashboard Dev, Diagnostics.
// ---------------------------------------------------------------------------

export interface DSCPreviewResponse {
  /** "csv" (alm-examples of the installed CSV) or "github". */
  source?: string;
  sourceDescription?: string;
}

/**
 * Unblock stuck rollouts. With a target, only that Deployment is patched;
 * "nothing to do" (errorCode nothing_to_do) and refusals come back as 422
 * OperationResponse bodies with an errorCode, returned rather than thrown.
 */
export function assistRolloutFor(target?: { namespace: string; deployment: string }): Promise<OperationResponse> {
  return request('/api/assist-rollout', {
    method: 'POST',
    body: target ? JSON.stringify(target) : undefined,
    acceptStatuses: [422],
  });
}
