import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { DashboardOverride, DashboardState, ServerOperation, UserPermissions, VersionInfo } from "../types";
import {
  getDashboardState,
  getUserPermissions,
  getVersion,
  isApiError,
  isSessionExpired,
  onSessionExpired,
  toApiError,
  type ApiError,
} from "../services/api";
import { DASHBOARD_OVERRIDE_POLL_MS, PERMISSION_RETRY_MS } from "../constants";
import { usePolling } from "../hooks/usePolling";
import { OPERATION_NAMES, STEP_SETS, operationKindForServerType, stepLabel } from "../operationSteps";
import { isRunning } from "./operation";
import { useClusterStatus, useOperation } from "./AppState";
import { formatElapsed } from "../utils";

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

/**
 * checking: the first answer has not arrived. allowed / denied: the backend
 * answered. unknown: it could not answer (503 authorization_unavailable, a
 * network error, an expired session); mutations stay disabled, never "allowed".
 */
export type PermissionStatus = "checking" | "allowed" | "denied" | "unknown";

export interface PermissionsValue {
  status: PermissionStatus;
  canMutate: boolean;
  /** Why mutations are disabled for this user, or null when they are allowed. */
  reason: string | null;
  error: ApiError | null;
  retry: () => void;
}

export const NO_PERMISSION_REASON = "Read-only access: changing the cluster through this tool requires the cluster-admin role.";
export const PERMISSION_CHECKING_REASON = "Checking your permissions...";
export const PERMISSION_UNKNOWN_REASON = "Your permissions could not be checked, so changes are disabled. Retry from the banner at the top of the page.";
export const SESSION_EXPIRED_REASON = "Your session has expired. Sign in again to make changes.";

function permissionReason(status: PermissionStatus, sessionExpired: boolean): string | null {
  if (sessionExpired) return SESSION_EXPIRED_REASON;
  switch (status) {
    case "allowed": return null;
    case "denied": return NO_PERMISSION_REASON;
    case "checking": return PERMISSION_CHECKING_REASON;
    case "unknown": return PERMISSION_UNKNOWN_REASON;
  }
}

// ---------------------------------------------------------------------------
// Dashboard Dev override (paused dashboard-operator)
// ---------------------------------------------------------------------------

export interface DashboardOverrideValue {
  /** The latest override the backend reported; null when there is none or it is unknown. */
  override: DashboardOverride | null;
  loaded: boolean;
  refresh: () => void;
}

/** The override in a dashboard-state response, including a 404 dashboard_not_deployed body. */
export function overrideFromDashboardResponse(result: DashboardState | ApiError): DashboardOverride | null | undefined {
  if (isApiError(result)) {
    if (result.errorCode !== "dashboard_not_deployed" && result.status !== 404) return undefined;
    const details = result.details as { override?: DashboardOverride } | undefined;
    return details?.override ?? null;
  }
  return result.override ?? null;
}

// ---------------------------------------------------------------------------
// Context
// ---------------------------------------------------------------------------

interface AppInfoValue {
  permissions: PermissionsValue;
  version: VersionInfo | null;
  dashboard: DashboardOverrideValue;
  sessionExpired: boolean;
}

const AppInfoContext = createContext<AppInfoValue | null>(null);

function useAppInfo(): AppInfoValue {
  const value = useContext(AppInfoContext);
  if (!value) throw new Error("AppInfo hooks must be used inside AppInfoProvider");
  return value;
}

export function usePermissions(): PermissionsValue {
  return useAppInfo().permissions;
}

export function useVersion(): VersionInfo | null {
  return useAppInfo().version;
}

export function useDashboardOverride(): DashboardOverrideValue {
  return useAppInfo().dashboard;
}

export function useSessionExpired(): boolean {
  return useAppInfo().sessionExpired;
}

interface AppInfoProviderProps {
  /** Injected for tests. */
  fetchPermissions?: () => Promise<UserPermissions>;
  fetchVersion?: (signal?: AbortSignal) => Promise<VersionInfo>;
  fetchDashboardState?: (signal?: AbortSignal) => Promise<DashboardState>;
}

/**
 * App-wide facts that every page needs: what the user may do, which build
 * of the updater runs, whether a Dashboard Dev session holds the dashboard,
 * and whether the session expired. Mount inside AppStateProvider.
 */
export const AppInfoProvider: React.FC<React.PropsWithChildren<AppInfoProviderProps>> = ({
  children,
  fetchPermissions = getUserPermissions,
  fetchVersion = getVersion,
  fetchDashboardState = getDashboardState,
}) => {
  const fetchers = useRef({ fetchPermissions, fetchVersion, fetchDashboardState });
  useEffect(() => {
    fetchers.current = { fetchPermissions, fetchVersion, fetchDashboardState };
  });

  // --- Session ---------------------------------------------------------------
  const [sessionExpired, setSessionExpired] = useState(false);
  useEffect(() => onSessionExpired(() => setSessionExpired(true)), []);

  // --- Permissions -----------------------------------------------------------
  const [permStatus, setPermStatus] = useState<PermissionStatus>("checking");
  const [permError, setPermError] = useState<ApiError | null>(null);
  const permStatusRef = useRef(permStatus);
  permStatusRef.current = permStatus;

  const checkPermissions = useCallback(async () => {
    try {
      const p = await fetchers.current.fetchPermissions();
      setPermStatus(p.canMutate ? "allowed" : "denied");
      setPermError(null);
    } catch (e) {
      const err = toApiError(e, "Could not check your permissions");
      const expired = isSessionExpired(err);
      // A JSON 403 is an answer (read-only). An expired session (oauth-proxy's
      // HTML 403) and everything else, 503 authorization_unavailable
      // included, is "unknown": fail closed.
      setPermStatus(!expired && (err.status === 403 || err.errorCode === "forbidden") ? "denied" : "unknown");
      setPermError(err);
      if (expired) setSessionExpired(true);
    }
  }, []);
  const permPoller = usePolling(checkPermissions, {
    // Retry soon while unanswered; otherwise re-check rarely (roles change).
    delay: () => (permStatusRef.current === "unknown" ? PERMISSION_RETRY_MS : 10 * 60_000),
    runImmediately: true,
  });
  const retryPermissions = useCallback(() => {
    setPermStatus("checking");
    permPoller.reset({ runNow: true });
  }, [permPoller]);

  // --- Version (once) ---------------------------------------------------------
  const [version, setVersion] = useState<VersionInfo | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    fetchers.current.fetchVersion(controller.signal)
      .then((v) => { if (!controller.signal.aborted) setVersion(v); })
      .catch(() => { /* an older backend has no /api/version */ });
    return () => controller.abort();
  }, []);

  // --- Dashboard Dev override -------------------------------------------------
  const [override, setOverride] = useState<DashboardOverride | null>(null);
  const [overrideLoaded, setOverrideLoaded] = useState(false);
  const overrideController = useRef<AbortController | null>(null);
  const loadOverride = useCallback(async () => {
    overrideController.current?.abort();
    const controller = new AbortController();
    overrideController.current = controller;
    let result: DashboardState | ApiError;
    try {
      result = await fetchers.current.fetchDashboardState(controller.signal);
    } catch (e) {
      result = toApiError(e);
    }
    if (controller.signal.aborted) return;
    const next = overrideFromDashboardResponse(result);
    // undefined: the state could not be read; keep what we knew.
    if (next !== undefined) {
      setOverride(next);
      setOverrideLoaded(true);
    }
  }, []);
  const overridePoller = usePolling(loadOverride, { delay: DASHBOARD_OVERRIDE_POLL_MS, runImmediately: true });
  const refreshOverride = useCallback(() => overridePoller.reset({ runNow: true }), [overridePoller]);
  useEffect(() => () => overrideController.current?.abort(), []);

  // An ended operator operation may have reverted the session (revertDashboardDev).
  const { run } = useOperation();
  const endedRunId = run?.outcome ? run.id : undefined;
  useEffect(() => {
    if (endedRunId !== undefined) refreshOverride();
  }, [endedRunId, refreshOverride]);

  const value = useMemo<AppInfoValue>(() => ({
    permissions: {
      status: permStatus,
      canMutate: permStatus === "allowed" && !sessionExpired,
      reason: permissionReason(permStatus, sessionExpired),
      error: permError,
      retry: retryPermissions,
    },
    version,
    dashboard: { override, loaded: overrideLoaded, refresh: refreshOverride },
    sessionExpired,
  }), [permStatus, sessionExpired, permError, retryPermissions, version, override, overrideLoaded, refreshOverride]);

  return <AppInfoContext.Provider value={value}>{children}</AppInfoContext.Provider>;
};

// ---------------------------------------------------------------------------
// Why mutations are blocked right now
// ---------------------------------------------------------------------------

/** "alice is running "Update to nightly"" / "You are running ..." / "... on updater pod <pod>" */
export function describeServerOperation(op: ServerOperation, currentUser?: string): string {
  const who = !op.user ? "Someone is" : op.user === currentUser ? "You are" : `${op.user} is`;
  return `${who} running "${op.label || op.type}"${remotePodSuffix(op)}`;
}

/** " on updater pod <pod>" for an operation another updater pod runs, else "". */
export function remotePodSuffix(op: Pick<ServerOperation, "remote" | "pod">): string {
  return op.remote ? ` on updater pod ${op.pod || "(unknown)"}` : "";
}

/** "Step 4 of 8: Wait for the catalog", for streamed operations that reported a step. */
export function serverOperationStep(op: ServerOperation): string | null {
  const kind = operationKindForServerType(op.type, op.target);
  if (!kind || !op.step) return null;
  const index = STEP_SETS[kind].findIndex((s) => s.id === op.step);
  const label = stepLabel(kind, op.step);
  return index >= 0 ? `step ${index + 1} of ${STEP_SETS[kind].length}: ${label}` : label;
}

/**
 * After a request the backend refused with 409 cluster_busy, ask it at once
 * which operation holds the lock, so the banner and the disabled buttons
 * appear without waiting for the next poll.
 */
export function useClusterBusyHandler(): (res: { errorCode?: string }) => void {
  const { refreshServerOperation } = useOperation();
  return useCallback((res: { errorCode?: string }) => {
    if (res.errorCode === "cluster_busy") refreshServerOperation();
  }, [refreshServerOperation]);
}

/**
 * Why a cluster-changing button must stay disabled now, or null: the one
 * source every page uses. Covers the user's permissions and session, this
 * tab's own operation, any operation the backend reports (another tab or a
 * teammate), and OLM still installing after the last operation (the backend
 * lock is already free then). `ignoreReconcile` is for repairs of what may
 * be stuck in that install (Diagnostics fixes, rollout assists).
 */
export function useMutationBlocker(options: { ignoreReconcile?: boolean } = {}): string | null {
  const { reason } = usePermissions();
  const { run, server, state } = useOperation();
  const { status } = useClusterStatus();
  if (reason) return reason;
  if (options.ignoreReconcile && !isRunning(run) && !server.inProgress) return null;
  if (isRunning(run) && run.source === "stream") {
    return `Your ${OPERATION_NAMES[run.kind].toLowerCase()} is still running. Wait for it to finish.`;
  }
  if (server.inProgress) {
    return server.operation
      ? `${describeServerOperation(server.operation, status?.cluster.user)}. Wait for it to finish.`
      : "Another operation is changing this cluster. Wait for it to finish.";
  }
  if (isRunning(run)) return `${OPERATION_NAMES[run.kind]} is still running. Wait for it to finish.`;
  if (state.reconcile.active) {
    const phase = status?.csv.phase || "installing";
    return `The operator is still being installed after the last ${state.reconcile.kind} (CSV ${phase}, ${formatElapsed(state.reconcile.startTime)}). Wait for it to finish.`;
  }
  return null;
}
