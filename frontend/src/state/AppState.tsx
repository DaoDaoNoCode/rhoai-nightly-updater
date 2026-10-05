import React, { createContext, useCallback, useContext, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { flushSync } from "react-dom";
import type { OperationStatusResponse, ServerOperation, StatusResponse, UpdateStep } from "../types";
import {
  ApiError,
  getOperation,
  getStatus,
  toApiError,
  type StreamDetachHandler,
  type StreamDetachReason,
  type StreamDoneHandler,
} from "../services/api";
import {
  BACKGROUND_POLL_MS,
  OPERATION_POLL_BUSY_MS,
  OPERATION_POLL_IDLE_MS,
  RECONCILE_POLL_FAST_MS,
  RECONCILE_POLL_FAST_UNTIL_MS,
  RECONCILE_POLL_MEDIUM_MS,
  RECONCILE_POLL_MEDIUM_UNTIL_MS,
  RECONCILE_POLL_SLOW_MS,
  RECONCILE_TIMEOUT_MS,
  STREAM_MAX_MS,
} from "../constants";
import { usePolling } from "../hooks/usePolling";
import { OPERATION_NAMES, STEP_SETS, operationKindForServerType, stepLabel, type OperationKind, type ReconcileKind } from "../operationSteps";
import {
  initialOperationState,
  isRunning,
  operationPhase,
  operationReducer,
  type OperationOutcome,
  type OperationPhase,
  type OperationRun,
  type OperationState,
} from "./operation";
import { useAnnounce } from "./LiveAnnouncer";

// ---------------------------------------------------------------------------
// Cluster status
// ---------------------------------------------------------------------------

export interface ClusterStatusValue {
  status: StatusResponse | null;
  loading: boolean;
  error: ApiError | null;
  lastRefreshed: Date | null;
  refresh: () => Promise<void>;
}

// ---------------------------------------------------------------------------
// Operation lifecycle
// ---------------------------------------------------------------------------

export interface StreamHandlers {
  onStep: (step: UpdateStep) => void;
  onDone: StreamDoneHandler;
  onDetach: StreamDetachHandler;
}

/** Opens the SSE stream for an operation and returns its controller. */
export type StreamOpener = (handlers: StreamHandlers) => AbortController;

/**
 * Server-side view of an operation, for tabs that did not start it (another
 * tab, a reload, a teammate). Phase 2 feeds this from the backend.
 */
export interface ServerOperationSnapshot {
  id: string;
  kind: OperationKind;
  /** Epoch milliseconds. */
  startedAt: number;
  state: "running" | "succeeded" | "failed";
  steps: UpdateStep[];
  message?: string;
  errorCode?: string;
  /** What the operation targets (image, reinstall target). */
  detail?: string;
  /** Who started it. */
  user?: string;
}

/**
 * What the backend reports about cluster operations (GET /api/operation):
 * the one holding the mutation lock, started by anyone from any tab, and an
 * operation a previous updater pod never finished.
 */
export interface ServerOperationState {
  /** At least one answer arrived. */
  loaded: boolean;
  inProgress: boolean;
  operation: ServerOperation | null;
  interrupted: OperationStatusResponse["interrupted"] | null;
}

export interface OperationContextValue {
  state: OperationState;
  /** The backend's view of the running operation (any user, any tab). */
  server: ServerOperationState;
  /** Poll GET /api/operation now (e.g. after a cluster_busy rejection). */
  refreshServerOperation: () => void;
  run: OperationRun | null;
  /** True while a run has no result yet. Mutations should stay disabled. */
  running: boolean;
  phase: OperationPhase;
  /** Start a streamed operation. Returns the run id, or null if one is already running. */
  start: (kind: OperationKind, open: StreamOpener, detail?: string) => number | null;
  /** Apply server-side operation state (null: the server reports no operation). */
  syncServerOperation: (snapshot: ServerOperationSnapshot | null) => void;
  dismissTimeout: () => void;
  dismissFinished: () => void;
}

const ClusterStatusContext = createContext<ClusterStatusValue | null>(null);
const OperationContext = createContext<OperationContextValue | null>(null);

export function useClusterStatus(): ClusterStatusValue {
  const value = useContext(ClusterStatusContext);
  if (!value) throw new Error("useClusterStatus must be used inside AppStateProvider");
  return value;
}

export function useOperation(): OperationContextValue {
  const value = useContext(OperationContext);
  if (!value) throw new Error("useOperation must be used inside AppStateProvider");
  return value;
}

// ---------------------------------------------------------------------------
// Persistence (reconcile tracking survives a page reload)
// ---------------------------------------------------------------------------

const SESSION_KEY_RECONCILING = "rhoai-reconciling";
const SESSION_KEY_RECONCILE_START = "rhoai-reconcile-start";
const SESSION_KEY_RECONCILE_KIND = "rhoai-reconcile-kind";

function restoreReconcile(): OperationState {
  try {
    const active = sessionStorage.getItem(SESSION_KEY_RECONCILING) === "true";
    const startTime = Number(sessionStorage.getItem(SESSION_KEY_RECONCILE_START) || "0");
    const storedKind = sessionStorage.getItem(SESSION_KEY_RECONCILE_KIND);
    const kind: ReconcileKind = storedKind === "refresh" || storedKind === "reinstall" ? storedKind : "update";
    if (active && startTime > 0) {
      if (Date.now() - startTime < RECONCILE_TIMEOUT_MS) {
        return initialOperationState({ active: true, startTime, kind });
      }
      persistReconcile(false, 0, kind);
    }
  } catch {
    // sessionStorage may be disabled
  }
  return initialOperationState();
}

function persistReconcile(active: boolean, startTime: number, kind: ReconcileKind): void {
  try {
    if (active) {
      sessionStorage.setItem(SESSION_KEY_RECONCILING, "true");
      sessionStorage.setItem(SESSION_KEY_RECONCILE_START, String(startTime));
      sessionStorage.setItem(SESSION_KEY_RECONCILE_KIND, kind);
    } else {
      sessionStorage.removeItem(SESSION_KEY_RECONCILING);
      sessionStorage.removeItem(SESSION_KEY_RECONCILE_START);
      sessionStorage.removeItem(SESSION_KEY_RECONCILE_KIND);
    }
  } catch {
    // sessionStorage may be disabled
  }
}

/** Adaptive reconcile poll interval based on how long we've been actively polling. */
export function reconcilePollInterval(activeTimeMs: number): number {
  if (activeTimeMs < RECONCILE_POLL_FAST_UNTIL_MS) return RECONCILE_POLL_FAST_MS;
  if (activeTimeMs < RECONCILE_POLL_MEDIUM_UNTIL_MS) return RECONCILE_POLL_MEDIUM_MS;
  return RECONCILE_POLL_SLOW_MS;
}

const DETACH_MESSAGES: Record<StreamDetachReason | "server_lost", string> = {
  aborted: "Stopped receiving live progress.",
  connection_lost: "The live progress connection was lost.",
  ended_without_result: "The live progress connection closed before the result arrived.",
  stalled: "Live progress stopped arriving.",
  server_lost: "The server no longer reports this operation.",
};

function outcomeAnnouncement(kind: OperationKind, outcome: OperationOutcome): { text: string; assertive: boolean } {
  const name = OPERATION_NAMES[kind];
  switch (outcome.status) {
    case "succeeded":
      return { text: `${name} request completed. Tracking the operator rollout.`, assertive: false };
    case "failed":
      return { text: `${name} failed: ${outcome.message}`, assertive: true };
    case "detached":
      return { text: `${outcome.message} The ${name.toLowerCase()} continues on the server; tracking it through status updates.`, assertive: false };
  }
}

interface AppStateProviderProps {
  /** Injected for tests. */
  fetchStatus?: (signal?: AbortSignal) => Promise<StatusResponse>;
  /** Injected for tests. */
  fetchOperation?: (signal?: AbortSignal) => Promise<OperationStatusResponse>;
}

/** How long after this tab's own run ended a matching server report is still that run. */
const OWN_RUN_GRACE_MS = 15_000;

/**
 * True when a server-reported operation of `kind` is most likely this tab's
 * own run that just ended (the lock is released right after the last
 * event). A run the backend rejected (409 cluster_busy, 400...) never held
 * the lock, so an operation reported after it belongs to someone else.
 */
export function isOwnRecentRun(run: OperationRun | null, kind: OperationKind | undefined, now: number): boolean {
  if (!run || run.source !== "stream" || !run.endedAt || run.kind !== kind) return false;
  if (run.outcome?.status === "failed" && run.outcome.rejected) return false;
  return now - run.endedAt < OWN_RUN_GRACE_MS;
}

/**
 * The ServerOperationSnapshot of a backend operation, for the streamed kinds
 * only. The backend reports just the latest step, and steps run in order,
 * so every earlier step of the pipeline is shown as done.
 */
export function snapshotFromServer(op: ServerOperation): ServerOperationSnapshot | null {
  const kind = operationKindForServerType(op.type, op.target);
  if (!kind) return null;
  const startedAt = Date.parse(op.startedAt);
  const steps: UpdateStep[] = [];
  if (op.step) {
    const defs = STEP_SETS[kind];
    const index = defs.findIndex((d) => d.id === op.step);
    for (let i = 0; i < index; i++) steps.push({ step: defs[i].id, status: "success", message: "", elapsedMs: 0 });
    const status = op.stepStatus === "success" || op.stepStatus === "failed" || op.stepStatus === "skipped" ? op.stepStatus : "running";
    steps.push({ step: op.step, status, message: op.message ?? "", elapsedMs: 0 });
  }
  return {
    id: op.id,
    kind,
    startedAt: Number.isFinite(startedAt) ? startedAt : Date.now(),
    state: "running",
    steps,
    detail: op.target,
    user: op.user,
  };
}

/** The running operation named in a 409 cluster_busy body, if any. */
export function busyOperationFrom(e: ApiError | undefined): ServerOperation | undefined {
  const details = e?.details as { operation?: unknown } | undefined;
  const op = details?.operation as Partial<ServerOperation> | undefined;
  if (!op || typeof op !== "object" || typeof op.id !== "string" || typeof op.type !== "string") return undefined;
  return {
    id: op.id,
    type: op.type,
    label: typeof op.label === "string" ? op.label : op.type,
    user: typeof op.user === "string" ? op.user : "",
    startedAt: typeof op.startedAt === "string" ? op.startedAt : new Date().toISOString(),
    target: typeof op.target === "string" ? op.target : undefined,
    step: typeof op.step === "string" ? op.step : undefined,
    stepStatus: typeof op.stepStatus === "string" ? op.stepStatus : undefined,
    message: typeof op.message === "string" ? op.message : undefined,
  };
}

/**
 * Owns the cluster status and the operator-operation lifecycle for the whole
 * app, so leaving the Dashboard page never aborts or forgets an operation.
 */
export const AppStateProvider: React.FC<React.PropsWithChildren<AppStateProviderProps>> = ({ children, fetchStatus = getStatus, fetchOperation = getOperation }) => {
  const announce = useAnnounce();

  // --- Status ---------------------------------------------------------------
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const statusRef = useRef<StatusResponse | null>(null);
  const latestStatusRequest = useRef(0);
  const fetchStatusRef = useRef(fetchStatus);
  const fetchOperationRef = useRef(fetchOperation);
  useEffect(() => {
    fetchStatusRef.current = fetchStatus;
    fetchOperationRef.current = fetchOperation;
  });

  // --- Operation ------------------------------------------------------------
  const [state, dispatch] = useReducer(operationReducer, undefined, restoreReconcile);
  const stateRef = useRef(state);
  stateRef.current = state;
  const nextRunId = useRef(0);
  const streamRef = useRef<{ id: number; controller: AbortController; deadline: ReturnType<typeof setTimeout> } | null>(null);
  // Visible time spent polling during reconciliation (hidden time doesn't count).
  const activePollingTime = useRef(0);
  const lastPollTimestamp = useRef(0);

  const refresh = useCallback(async () => {
    const requestId = ++latestStatusRequest.current;
    setLoading(true);
    try {
      const s = await fetchStatusRef.current();
      // An older poll must not overwrite a newer response.
      if (requestId !== latestStatusRequest.current) return;
      statusRef.current = s;
      setStatus(s);
      setError(null);
      setLastRefreshed(new Date());

      if (stateRef.current.reconcile.active) {
        const now = Date.now();
        if (lastPollTimestamp.current > 0) {
          const delta = now - lastPollTimestamp.current;
          // Only count gaps that look like normal polls, not time the tab was hidden.
          if (delta < RECONCILE_POLL_SLOW_MS * 2.5) activePollingTime.current += delta;
        }
        lastPollTimestamp.current = now;
        dispatch({ type: "statusPolled", csvPhase: s.csv.phase });
        if (activePollingTime.current >= RECONCILE_TIMEOUT_MS) dispatch({ type: "reconcileTimedOut" });
      }
    } catch (e) {
      if (requestId !== latestStatusRequest.current) return;
      setError(toApiError(e, "Failed to fetch status"));
    } finally {
      if (requestId === latestStatusRequest.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // One poller for /api/status: adaptive while reconciling, 60 s otherwise.
  // While this tab streams an operation, SSE carries the progress, so the
  // background poll is skipped (bounded by STREAM_MAX_MS).
  const poller = usePolling(
    () => {
      const run = stateRef.current.run;
      if (isRunning(run) && run.source === "stream" && Date.now() - run.startedAt < STREAM_MAX_MS) return undefined;
      return refresh();
    },
    {
      delay: () => (stateRef.current.reconcile.active ? reconcilePollInterval(activePollingTime.current) : BACKGROUND_POLL_MS),
    },
  );

  // --- Reconcile tracking side effects --------------------------------------
  const { active: reconcileActive, startTime: reconcileStart, kind: reconcileKind, finished: reconcileFinished, timedOut: reconcileTimedOut } = state.reconcile;
  useEffect(() => {
    persistReconcile(reconcileActive, reconcileStart, reconcileKind);
    if (reconcileActive) {
      activePollingTime.current = 0;
      lastPollTimestamp.current = 0;
      // Switch from the 60 s background interval to the fast reconcile interval now.
      poller.reset();
    }
  }, [reconcileActive, reconcileStart, reconcileKind, poller]);

  useEffect(() => {
    if (!reconcileFinished) return;
    const csv = statusRef.current?.csv;
    const failed = csv?.phase === "Failed";
    announce(`Operator reconciliation finished: ${csv?.name || "the operator"} is ${csv?.phase || "ready"}.`, failed ? "assertive" : "polite");
  }, [reconcileFinished, announce]);

  useEffect(() => {
    if (reconcileTimedOut) announce("Reconciliation monitoring timed out after 10 minutes. The operator may still be reconciling.", "assertive");
  }, [reconcileTimedOut, announce]);

  // --- Run side effects: one status refresh per ended run, announcements ----
  const run = state.run;
  const runId = run?.id;
  const outcome = run?.outcome;
  const runKind = run?.kind;
  useEffect(() => {
    if (runId === undefined || !outcome || !runKind) return;
    if (streamRef.current?.id === runId) {
      clearTimeout(streamRef.current.deadline);
      streamRef.current = null;
    }
    const { text, assertive } = outcomeAnnouncement(runKind, outcome);
    announce(text, assertive ? "assertive" : "polite");
    void refresh();
  }, [runId, outcome, runKind, announce, refresh]);

  const announcedSteps = useRef(new Map<string, string>());
  const steps = run?.steps;
  useEffect(() => {
    if (!steps || !runKind || runId === undefined) return;
    const total = STEP_SETS[runKind].length;
    for (const step of steps) {
      if (step.step === "operation_complete") continue;
      const key = `${runId}:${step.step}`;
      if (announcedSteps.current.get(key) === step.status) continue;
      announcedSteps.current.set(key, step.status);
      const index = STEP_SETS[runKind].findIndex((s) => s.id === step.step);
      const position = index >= 0 ? `Step ${index + 1} of ${total}, ` : "";
      if (step.status === "running") announce(`${position}${stepLabel(runKind, step.step)}: in progress`);
      else if (step.status === "skipped") announce(`${position}${stepLabel(runKind, step.step)}: skipped`);
      else if (step.status === "failed") announce(`${stepLabel(runKind, step.step)} failed: ${step.message}`, "assertive");
    }
  }, [steps, runKind, runId, announce]);

  // --- Actions ----------------------------------------------------------------
  const start = useCallback((kind: OperationKind, open: StreamOpener, detail?: string): number | null => {
    if (isRunning(stateRef.current.run)) return null;
    const id = ++nextRunId.current;
    const startAction = { type: "start", id, kind, source: "stream", now: Date.now(), detail } as const;
    // Update the ref synchronously so a double click cannot start two runs.
    stateRef.current = operationReducer(stateRef.current, startAction);
    dispatch(startAction);
    announce(`${OPERATION_NAMES[kind]} started.`);

    const end = (outcomeValue: OperationOutcome) => dispatch({ type: "end", id, outcome: outcomeValue, now: Date.now() });
    const controller = open({
      onStep: (step) => {
        // Render each step as it arrives instead of batching a burst of events.
        flushSync(() => dispatch({ type: "step", id, step }));
      },
      onDone: (success, message, apiError) => {
        if (success) {
          const terminal = stateRef.current.run?.id === id
            ? stateRef.current.run.steps.find((s) => s.step === "operation_complete")
            : undefined;
          end({ status: "succeeded", message: terminal?.message || `${OPERATION_NAMES[kind]} completed.` });
        } else {
          // HTTP rejections never started anything; a failed operation_complete did run.
          const rejected = !!apiError && apiError.status >= 400;
          end({
            status: "failed",
            message: message || apiError?.message || `${OPERATION_NAMES[kind]} failed.`,
            errorCode: apiError?.errorCode,
            httpStatus: apiError?.status,
            rejected,
            busyOperation: apiError?.errorCode === "cluster_busy" ? busyOperationFrom(apiError) : undefined,
          });
        }
      },
      onDetach: (reason) => end({ status: "detached", reason, message: DETACH_MESSAGES[reason] }),
    });
    // Backstop: the backend caps operations at 15 minutes.
    const deadline = setTimeout(() => controller.abort(), STREAM_MAX_MS);
    streamRef.current = { id, controller, deadline };
    return id;
  }, [announce]);

  const syncServerOperation = useCallback((snapshot: ServerOperationSnapshot | null) => {
    const current = stateRef.current.run;
    // This tab's own stream is the richer source while it is open.
    if (isRunning(current) && current.source === "stream") return;
    const now = Date.now();
    // The backend releases its lock just after the final event, so a poll
    // right after this tab's own run can still report that run.
    if (snapshot && isOwnRecentRun(current, snapshot.kind, now)) return;
    if (!snapshot) {
      if (isRunning(current) && current.source === "server") {
        dispatch({ type: "end", id: current.id, outcome: { status: "detached", reason: "server_lost", message: DETACH_MESSAGES.server_lost }, now });
      }
      return;
    }
    let id: number;
    if (current && current.source === "server" && current.serverId === snapshot.id) {
      id = current.id;
      if (current.outcome) return;
      for (const step of snapshot.steps) dispatch({ type: "step", id, step });
    } else {
      if (snapshot.state !== "running") return; // finished before this tab saw it
      id = ++nextRunId.current;
      dispatch({ type: "start", id, kind: snapshot.kind, source: "server", now: snapshot.startedAt, serverId: snapshot.id, steps: snapshot.steps, detail: snapshot.detail, user: snapshot.user });
    }
    if (snapshot.state === "succeeded") {
      dispatch({ type: "end", id, outcome: { status: "succeeded", message: snapshot.message || `${OPERATION_NAMES[snapshot.kind]} completed.` }, now });
    } else if (snapshot.state === "failed") {
      dispatch({ type: "end", id, outcome: { status: "failed", message: snapshot.message || `${OPERATION_NAMES[snapshot.kind]} failed.`, errorCode: snapshot.errorCode, rejected: false }, now });
    }
  }, []);

  // --- Server-side operation (GET /api/operation) ---------------------------
  const [server, setServer] = useState<ServerOperationState>({ loaded: false, inProgress: false, operation: null, interrupted: null });
  const serverRef = useRef(server);
  serverRef.current = server;

  const applyServerOperation = useCallback((res: OperationStatusResponse) => {
    const op = res.inProgress ? res.operation : null;
    const run = stateRef.current.run;
    const ownFinishedRun = !!op && isOwnRecentRun(run, snapshotFromServer(op)?.kind, Date.now());
    const next: ServerOperationState = {
      loaded: true,
      inProgress: res.inProgress && !ownFinishedRun,
      operation: ownFinishedRun ? null : op,
      interrupted: res.interrupted ?? null,
    };
    serverRef.current = next;
    setServer(next);
    syncServerOperation(op && !ownFinishedRun ? snapshotFromServer(op) : null);
  }, [syncServerOperation]);

  const pollServerOperation = useCallback(async () => {
    const current = stateRef.current.run;
    // While this tab streams its own operation, the stream is the source.
    if (isRunning(current) && current.source === "stream" && Date.now() - current.startedAt < STREAM_MAX_MS) return;
    try {
      applyServerOperation(await fetchOperationRef.current());
    } catch {
      // Keep the last answer: a failed poll says nothing about the cluster.
    }
  }, [applyServerOperation]);

  const operationPoller = usePolling(pollServerOperation, {
    delay: () => (serverRef.current.inProgress || isRunning(stateRef.current.run) ? OPERATION_POLL_BUSY_MS : OPERATION_POLL_IDLE_MS),
    runImmediately: true,
  });
  const refreshServerOperation = useCallback(() => operationPoller.reset({ runNow: true }), [operationPoller]);

  // After a run of this tab ends, look again soon: the lock is free by then,
  // or a cluster_busy rejection named the operation that holds it.
  useEffect(() => {
    if (runId === undefined || !outcome) return;
    if (outcome.status === "failed" && outcome.busyOperation) {
      applyServerOperation({ inProgress: true, operation: outcome.busyOperation });
    }
    const id = setTimeout(() => operationPoller.reset({ runNow: true }), 1_500);
    return () => clearTimeout(id);
  }, [runId, outcome, applyServerOperation, operationPoller]);

  const dismissTimeout = useCallback(() => dispatch({ type: "dismissTimeout" }), []);
  const dismissFinished = useCallback(() => dispatch({ type: "dismissFinished" }), []);

  // Stop listening when the provider goes away (page unload, tests). The
  // backend keeps running the operation either way.
  useEffect(() => () => {
    const stream = streamRef.current;
    if (stream) {
      clearTimeout(stream.deadline);
      stream.controller.abort();
    }
  }, []);

  const statusValue = useMemo<ClusterStatusValue>(
    () => ({ status, loading, error, lastRefreshed, refresh }),
    [status, loading, error, lastRefreshed, refresh],
  );
  const operationValue = useMemo<OperationContextValue>(
    () => ({
      state,
      server,
      refreshServerOperation,
      run: state.run,
      running: isRunning(state.run),
      phase: operationPhase(state),
      start,
      syncServerOperation,
      dismissTimeout,
      dismissFinished,
    }),
    [state, server, refreshServerOperation, start, syncServerOperation, dismissTimeout, dismissFinished],
  );

  return (
    <ClusterStatusContext.Provider value={statusValue}>
      <OperationContext.Provider value={operationValue}>{children}</OperationContext.Provider>
    </ClusterStatusContext.Provider>
  );
};
