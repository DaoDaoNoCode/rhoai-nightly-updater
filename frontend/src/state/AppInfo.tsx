import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { DashboardOverride, DashboardState, ServerOperation, UpdateCheck, VersionInfo } from "../types";
import {
  getDashboardState,
  getUpdateCheck,
  getVersion,
  isApiError,
  toApiError,
  type ApiError,
} from "../services/api";
import { DASHBOARD_OVERRIDE_POLL_MS, GITOPS_POLICY_POLL_MS, UPDATE_CHECK_REFRESH_MS } from "../constants";
import { usePermissions, useRole } from "./Permissions";
import { usePolling } from "../hooks/usePolling";
import { useLatestRequest } from "../hooks/useLatestRequest";
import { OPERATION_NAMES, STEP_SETS, operationKindForServerType, stepLabel } from "../operationSteps";
import { isRunning } from "./operation";
import { useClusterStatus, useOperation } from "./AppState";
import { formatElapsed, isReleaseVersion, sameData } from "../utils";
import { isGitOpsProfile } from "../profile";

// Permissions, the role and the session live in PermissionsProvider (the
// outermost provider: the role decides which reads the others make); the
// hooks are re-exported here, where the pages import them from.
export {
  NO_PERMISSION_REASON,
  PERMISSION_CHECKING_REASON,
  PERMISSION_UNKNOWN_REASON,
  SESSION_EXPIRED_REASON,
  usePermissions,
  useRole,
  useSessionExpired,
  useSignedInUser,
  type PermissionStatus,
  type PermissionsValue,
  type ViewRole,
} from "./Permissions";

// ---------------------------------------------------------------------------
// Dashboard Dev override (paused dashboard module operator)
// ---------------------------------------------------------------------------

export interface DashboardOverrideValue {
  /** The latest override the backend reported; null when there is none or it is unknown. */
  override: DashboardOverride | null;
  loaded: boolean;
  refresh: () => void;
  /**
   * For a page that polls GET /api/dashboard/state itself (Dashboard Dev):
   * while it is mounted (watch() until its cleanup), the app does not poll
   * the same endpoint, and report() takes the page's answers instead. The
   * page calls beginRead() just before each read and reports its answer with
   * that number: of the page's and the app's reads, the one that started
   * last wins, whatever order the answers arrive in.
   */
  watch: () => () => void;
  beginRead: () => number;
  report: (read: number, result: DashboardState | ApiError) => void;
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
  version: VersionInfo | null;
  /** The backend's profile; null until /api/version answered (or failed: an older backend, "full"). */
  profile: "full" | "gitops" | null;
  /** Only for a running release; null until known. */
  update: UpdateCheck | null;
  dashboard: DashboardOverrideValue;
}

const AppInfoContext = createContext<AppInfoValue | null>(null);

function useAppInfo(): AppInfoValue {
  const value = useContext(AppInfoContext);
  if (!value) throw new Error("AppInfo hooks must be used inside AppInfoProvider");
  return value;
}

export function useVersion(): VersionInfo | null {
  return useAppInfo().version;
}

/**
 * The backend's profile, null while unknown. Calls that the gitops profile
 * refuses wait for "full".
 */
export function useProfile(): "full" | "gitops" | null {
  return useAppInfo().profile;
}

/** True when the backend runs the gitops profile (docs/GITOPS.md, "Profiles"). */
export function useGitOpsProfile(): boolean {
  return isGitOpsProfile(useAppInfo().version);
}

export function useUpdateCheck(): UpdateCheck | null {
  return useAppInfo().update;
}

export function useDashboardOverride(): DashboardOverrideValue {
  return useAppInfo().dashboard;
}

interface AppInfoProviderProps {
  /** Injected for tests. */
  fetchVersion?: (signal?: AbortSignal) => Promise<VersionInfo>;
  fetchUpdateCheck?: (signal?: AbortSignal) => Promise<UpdateCheck>;
  fetchDashboardState?: (signal?: AbortSignal) => Promise<DashboardState>;
}

/**
 * App-wide facts that every page needs: which build of the updater runs,
 * whether a newer release exists, and whether a Dashboard Dev session holds
 * the dashboard (read for cluster-admins only). Mount inside
 * AppStateProvider, inside PermissionsProvider.
 */
export const AppInfoProvider: React.FC<React.PropsWithChildren<AppInfoProviderProps>> = ({
  children,
  fetchVersion = getVersion,
  fetchUpdateCheck = getUpdateCheck,
  fetchDashboardState = getDashboardState,
}) => {
  const fetchers = useRef({ fetchVersion, fetchUpdateCheck, fetchDashboardState });
  useEffect(() => {
    fetchers.current = { fetchVersion, fetchUpdateCheck, fetchDashboardState };
  });
  const role = useRole();

  // --- Version (once; the gitops profile polls it, below) ---------------------
  const [version, setVersion] = useState<VersionInfo | null>(null);
  // The profile decides which routes exist, so calls the gitops profile
  // refuses wait until the version answered (or failed: an older backend).
  const [versionSettled, setVersionSettled] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    fetchers.current.fetchVersion(controller.signal)
      .then((v) => { if (!controller.signal.aborted) setVersion(v); })
      .catch(() => { /* an older backend has no /api/version */ })
      .finally(() => { if (!controller.signal.aborted) setVersionSettled(true); });
    return () => controller.abort();
  }, []);
  const gitOpsProfile = isGitOpsProfile(version);
  // In the gitops profile the version also carries the admission policy's
  // state, which changes while the page is open (Argo CD restores or
  // changes the policy): read it again, so Update turns back on, or off,
  // without a reload. A failed read keeps the last answer.
  const versionRequest = useLatestRequest();
  const refreshVersion = useCallback(async () => {
    const ticket = versionRequest.start();
    try {
      const v = await fetchers.current.fetchVersion(ticket.signal);
      if (!ticket.isLatest()) return;
      // An unchanged answer keeps the last object: nothing renders again.
      setVersion((prev) => (sameData(prev, v) ? prev : v));
    } catch {
      /* keep the last answer */
    }
  }, [versionRequest]);
  usePolling(refreshVersion, { delay: GITOPS_POLICY_POLL_MS, enabled: gitOpsProfile });

  // --- Update check (releases only; again on window focus, at most every 6 h) --
  const [update, setUpdate] = useState<UpdateCheck | null>(null);
  const runningRelease = isReleaseVersion(version?.version) ? version?.version : undefined;
  const updateCheckedAt = useRef(0);
  useEffect(() => {
    if (!runningRelease) return;
    let controller: AbortController | null = null;
    const check = () => {
      controller?.abort();
      const current = new AbortController();
      controller = current;
      updateCheckedAt.current = Date.now();
      fetchers.current.fetchUpdateCheck(current.signal)
        .then((u) => { if (!current.signal.aborted) setUpdate(u); })
        .catch(() => { /* best effort: no banner when the check fails */ });
    };
    check();
    const onFocus = () => {
      if (Date.now() - updateCheckedAt.current >= UPDATE_CHECK_REFRESH_MS) check();
    };
    window.addEventListener("focus", onFocus);
    return () => {
      window.removeEventListener("focus", onFocus);
      controller?.abort();
    };
  }, [runningRelease]);

  // --- Dashboard Dev override -------------------------------------------------
  const [override, setOverride] = useState<DashboardOverride | null>(null);
  const [overrideLoaded, setOverrideLoaded] = useState(false);
  const overrideRequest = useLatestRequest();
  const applyDashboardState = useCallback((result: DashboardState | ApiError) => {
    const next = overrideFromDashboardResponse(result);
    // undefined: the state could not be read; keep what we knew.
    if (next !== undefined) {
      setOverride((prev) => (sameData(prev, next) ? prev : next));
      setOverrideLoaded(true);
    }
  }, []);
  // Every read of the endpoint, the app's and a page's (beginRead), gets a
  // number when it starts. An answer is applied only when its read started
  // after the one last applied: a later read describes the cluster later.
  const dashboardReads = useRef({ started: 0, applied: 0, ownInFlight: 0 });
  const beginDashboardRead = useCallback(() => ++dashboardReads.current.started, []);
  const applyInOrder = useCallback((read: number, result: DashboardState | ApiError) => {
    // A read that could not tell (undefined) changes nothing, not even the order.
    if (read <= dashboardReads.current.applied || overrideFromDashboardResponse(result) === undefined) return;
    dashboardReads.current.applied = read;
    applyDashboardState(result);
  }, [applyDashboardState]);
  const loadOverride = useCallback(async () => {
    const ticket = overrideRequest.start();
    const read = beginDashboardRead();
    dashboardReads.current.ownInFlight = read;
    let result: DashboardState | ApiError;
    try {
      result = await fetchers.current.fetchDashboardState(ticket.signal);
    } catch (e) {
      result = toApiError(e);
    }
    if (!ticket.isLatest()) return;
    dashboardReads.current.ownInFlight = 0;
    applyInOrder(read, result);
  }, [overrideRequest, beginDashboardRead, applyInOrder]);
  // A page that polls the same endpoint (Dashboard Dev) is the source while it is open.
  const [pageReaders, setPageReaders] = useState(0);
  const watchDashboardState = useCallback(() => {
    setPageReaders((n) => n + 1);
    return () => setPageReaders((n) => n - 1);
  }, []);
  const reportDashboardState = useCallback((read: number, result: DashboardState | ApiError) => {
    // A read of ours in flight that started before the page's is outdated by
    // this answer; one that started after it is newer and goes on.
    const own = dashboardReads.current.ownInFlight;
    if (own && own < read) {
      overrideRequest.cancel();
      dashboardReads.current.ownInFlight = 0;
    }
    applyInOrder(read, result);
  }, [overrideRequest, applyInOrder]);
  // Dashboard Dev does not exist in the gitops profile (403 profile_gitops),
  // and only cluster-admins read it (403 admin_only for the others).
  const mayReadOverride = versionSettled && !gitOpsProfile && role === "admin";
  const overridePoller = usePolling(loadOverride, { delay: DASHBOARD_OVERRIDE_POLL_MS, runImmediately: true, enabled: mayReadOverride && pageReaders === 0 });
  const refreshOverride = useCallback(() => {
    // Asked for at once (an operation ended, a session changed): a read on
    // demand even while a page polls, as before; the poller resets otherwise.
    if (pageReaders > 0 && mayReadOverride) void loadOverride();
    else overridePoller.reset({ runNow: true });
  }, [overridePoller, pageReaders, mayReadOverride, loadOverride]);

  // An ended operator operation may have reverted the session (revertDashboardDev).
  const { run } = useOperation();
  const endedRunId = run?.outcome ? run.id : undefined;
  useEffect(() => {
    if (endedRunId !== undefined) refreshOverride();
  }, [endedRunId, refreshOverride]);

  const value = useMemo<AppInfoValue>(() => ({
    version,
    profile: !versionSettled ? null : gitOpsProfile ? "gitops" : "full",
    update,
    dashboard: { override, loaded: overrideLoaded, refresh: refreshOverride, watch: watchDashboardState, beginRead: beginDashboardRead, report: reportDashboardState },
  }), [version, versionSettled, gitOpsProfile, update, override, overrideLoaded, refreshOverride, watchDashboardState, beginDashboardRead, reportDashboardState]);

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
