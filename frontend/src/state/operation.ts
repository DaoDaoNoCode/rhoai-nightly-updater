import type { ServerOperation, UpdateStep } from "../types";
import type { StreamDetachReason } from "../services/api";
import { OPERATION_NAMES, operationKindForServerType, reconcileKindFor, type OperationKind, type ReconcileKind } from "../operationSteps";

/**
 * Where an operation's progress comes from: the SSE stream this tab opened,
 * or (for operations started elsewhere) server-side state fetched by polling.
 */
export type OperationSource = "stream" | "server";

/** Why a run stopped reporting progress without a result. */
export type OperationDetachReason = StreamDetachReason | "server_lost";

export type OperationOutcome =
  | { status: "succeeded"; message: string }
  | {
      status: "failed";
      message: string;
      errorCode?: string;
      httpStatus?: number;
      /** True when the backend refused the request, so nothing changed on the cluster. */
      rejected: boolean;
      /** For a cluster_busy rejection: the operation that holds the lock. */
      busyOperation?: ServerOperation;
    }
  | { status: "detached"; reason: OperationDetachReason; message: string };

export interface OperationRun {
  id: number;
  kind: OperationKind;
  source: OperationSource;
  startedAt: number;
  /** Latest event per step, in arrival order. */
  steps: UpdateStep[];
  endedAt?: number;
  outcome?: OperationOutcome;
  /** Server-side operation identifier (GET /api/operation `id`), once known. */
  serverId?: string;
  /** What the run targets, e.g. the FBC image of an update. */
  detail?: string;
  /** Who started it, for runs reported by the server (another tab or user). */
  user?: string;
  /** The other updater pod that runs it (the backend reports it as remote). */
  pod?: string;
  /** statusFingerprint of the operator when this tab started the run. */
  baseline?: string;
}

/**
 * The operation's own result as far as this tab knows it: "succeeded" when
 * the stream or the server (GET /api/operation lastCompleted) said so, and
 * "unknown" when the tab lost track of it (the stream detached, the page was
 * reloaded, or an operation followed from the server ended without a result).
 */
export type ReconcileResult = "succeeded" | "unknown";

export interface ReconcileState {
  active: boolean;
  startTime: number;
  kind: ReconcileKind;
  timedOut: boolean;
  /** Reconciliation reached Succeeded or Failed; keeps the result card visible. */
  finished: boolean;
  /**
   * The backend may still run the operation (the stream detached, the page
   * was reloaded). Tracking cannot finish until GET /api/operation no longer
   * reports it: until then a Succeeded CSV is the old operator's.
   */
  awaitingServer: boolean;
  /** Only status polls requested at or after this time (epoch ms) can finish tracking. */
  evidenceAfter: number;
  result: ReconcileResult;
  /** Backend id of the tracked operation, to match GET /api/operation lastCompleted. */
  serverId?: string;
  /** Operator fingerprint (statusFingerprint) before the operation started, when known. */
  baseline?: string;
  /** A poll saw a fingerprint other than the baseline, or a phase other than Succeeded. */
  sawChange: boolean;
}

export interface OperationState {
  /** The current or most recent run. */
  run: OperationRun | null;
  reconcile: ReconcileState;
}

/** The outcome GET /api/operation reports for a finished operation (lastCompleted). */
export interface ServerResult {
  success: boolean;
  message?: string;
  /** lastCompleted.type and target, to name an operation this tab has no run for (after a reload). */
  type?: string;
  target?: string;
}

export type OperationAction =
  | { type: "start"; id: number; kind: OperationKind; source: OperationSource; now: number; serverId?: string; steps?: UpdateStep[]; detail?: string; user?: string; pod?: string; baseline?: string }
  | { type: "step"; id: number; step: UpdateStep }
  | { type: "end"; id: number; outcome: OperationOutcome; now: number }
  /** The backend id of a run this tab streams, learned while its stream held the lock. */
  | { type: "bindServerId"; id: number; serverId: string }
  /** A server-reported run turned out to be another kind (a reinstall's target arrives after it starts). */
  | { type: "retarget"; id: number; kind: OperationKind; detail?: string }
  /**
   * GET /api/operation no longer reports the tracked operation. `result` is
   * its lastCompleted entry, only when that names the same operation.
   */
  | { type: "serverSettled"; now: number; result?: ServerResult }
  | { type: "statusPolled"; csvPhase: string; fingerprint?: string; requestedAt: number }
  | { type: "reconcileTimedOut" }
  | { type: "dismissTimeout" }
  | { type: "dismissFinished" };

export const IDLE_RECONCILE: ReconcileState = {
  active: false,
  startTime: 0,
  kind: "update",
  timedOut: false,
  finished: false,
  awaitingServer: false,
  evidenceAfter: 0,
  result: "unknown",
  sawChange: false,
};

export function initialOperationState(reconcile: Partial<ReconcileState> = {}): OperationState {
  return { run: null, reconcile: { ...IDLE_RECONCILE, ...reconcile } };
}

export function isRunning(run: OperationRun | null): run is OperationRun & { outcome?: undefined } {
  return !!run && !run.outcome;
}

/**
 * What identifies the installed operator build: the CSV name, the catalog
 * image and the installed nightly digest. Nightlies of one release share the
 * CSV name (rhods-operator.3.6.0), so the image and digest tell them apart.
 */
export function statusFingerprint(
  status: { csv: { name: string }; catalogSource?: { image?: string }; nightly?: { installed?: { digest?: string } } } | null | undefined,
): string | undefined {
  if (!status) return undefined;
  return [status.csv.name, status.catalogSource?.image ?? "", status.nightly?.installed?.digest ?? ""].join("|");
}

function mergeStep(steps: UpdateStep[], step: UpdateStep): UpdateStep[] {
  const idx = steps.findIndex((s) => s.step === step.step);
  if (idx < 0) return [...steps, step];
  const next = [...steps];
  next[idx] = step;
  return next;
}

/**
 * Operation lifecycle. Every run starts once and ends once; events for other
 * or already-ended runs are ignored. A run that succeeded or detached (the
 * backend keeps going after the stream ends) hands over to reconciliation
 * tracking; a failed run does not.
 *
 * Tracking finishes on a Succeeded or Failed CSV only when that phase
 * describes the cluster after the operation: never while the backend may
 * still run it (a detached stream), and only from a status requested after
 * the end was known. Otherwise the old CSV's Succeeded would end tracking
 * before OLM even started.
 */
export function operationReducer(state: OperationState, action: OperationAction): OperationState {
  switch (action.type) {
    case "start": {
      if (isRunning(state.run)) return state;
      return {
        run: {
          id: action.id,
          kind: action.kind,
          source: action.source,
          startedAt: action.now,
          steps: action.steps ?? [],
          serverId: action.serverId,
          detail: action.detail,
          user: action.user,
          pod: action.pod,
          baseline: action.baseline,
        },
        // A new run supersedes the previous run's tracking and result.
        reconcile: { ...IDLE_RECONCILE, kind: state.reconcile.kind },
      };
    }
    case "step": {
      const run = state.run;
      if (!run || run.id !== action.id || run.outcome) return state;
      return { ...state, run: { ...run, steps: mergeStep(run.steps, action.step) } };
    }
    case "bindServerId": {
      const run = state.run;
      if (!run || run.id !== action.id || run.serverId) return state;
      const r = state.reconcile;
      return { run: { ...run, serverId: action.serverId }, reconcile: r.active && !r.serverId ? { ...r, serverId: action.serverId } : r };
    }
    case "retarget": {
      const run = state.run;
      if (!run || run.id !== action.id || run.outcome) return state;
      if (run.kind === action.kind && (action.detail === undefined || run.detail === action.detail)) return state;
      return { ...state, run: { ...run, kind: action.kind, detail: action.detail ?? run.detail } };
    }
    case "end": {
      const run = state.run;
      if (!run || run.id !== action.id || run.outcome) return state;
      const ended: OperationRun = { ...run, outcome: action.outcome, endedAt: action.now };
      if (action.outcome.status === "failed") return { ...state, run: ended };
      const outcome = action.outcome;
      const detached = outcome.status === "detached";
      return {
        run: ended,
        reconcile: {
          ...IDLE_RECONCILE,
          active: true,
          startTime: action.now,
          kind: reconcileKindFor(run.kind),
          // A detached stream: the backend keeps going. server_lost: the
          // server has just said it no longer runs the operation.
          awaitingServer: outcome.status === "detached" && outcome.reason !== "server_lost",
          evidenceAfter: action.now,
          result: detached ? "unknown" : "succeeded",
          serverId: run.serverId,
          baseline: run.baseline,
        },
      };
    }
    case "serverSettled": {
      const r = state.reconcile;
      if (!r.active || !r.awaitingServer) return state;
      const run = state.run;
      if (action.result && !action.result.success) {
        // The operation failed (the backend restores the previous operator):
        // show a failed run, never a finished rollout.
        let failedRun: OperationRun | null = run;
        if (run && run.outcome?.status === "detached") {
          failedRun = { ...run, outcome: { status: "failed", message: action.result.message || `${OPERATION_NAMES[run.kind]} failed.`, rejected: false } };
        } else if (!run || run.outcome) {
          // No run of this operation in this tab (the page was reloaded while
          // it ran): show the failure from the server's record instead of
          // silently dropping the card (R7 M1).
          const kind = (action.result.type ? operationKindForServerType(action.result.type, action.result.target) : null)
            ?? (r.kind === "reinstall" ? "reinstall_nightly" : r.kind);
          const said = action.result.message || `${OPERATION_NAMES[kind]} failed.`;
          const message = `${said}${/[.!?]$/.test(said) ? "" : "."} See the activity log for details.`;
          failedRun = {
            id: -action.now, kind, source: "server", startedAt: r.startTime || action.now, steps: [], endedAt: action.now,
            serverId: r.serverId, detail: action.result.target || undefined,
            outcome: { status: "failed", message, rejected: false },
          };
        }
        return { run: failedRun, reconcile: { ...r, active: false, awaitingServer: false } };
      }
      return {
        ...state,
        reconcile: { ...r, awaitingServer: false, evidenceAfter: action.now, result: action.result?.success ? "succeeded" : r.result },
      };
    }
    case "statusPolled": {
      const r = state.reconcile;
      if (!r.active) return state;
      const changed = action.csvPhase !== "Succeeded" || (!!r.baseline && !!action.fingerprint && action.fingerprint !== r.baseline);
      const next = changed && !r.sawChange ? { ...r, sawChange: true } : r;
      const final = action.csvPhase === "Succeeded" || action.csvPhase === "Failed";
      if (!final || r.awaitingServer || action.requestedAt < r.evidenceAfter) {
        return next === r ? state : { ...state, reconcile: next };
      }
      return { ...state, reconcile: { ...next, active: false, finished: true } };
    }
    case "reconcileTimedOut":
      if (!state.reconcile.active) return state;
      return { ...state, reconcile: { ...state.reconcile, active: false, timedOut: true } };
    case "dismissTimeout":
      return { ...state, reconcile: { ...state.reconcile, timedOut: false } };
    case "dismissFinished":
      return { ...state, reconcile: { ...state.reconcile, finished: false } };
    default:
      return state;
  }
}

/** Coarse phase, for code that only needs to know what to poll. */
export type OperationPhase = "idle" | "streaming" | "reconciling" | "complete";

export function operationPhase(state: OperationState): OperationPhase {
  if (isRunning(state.run)) return "streaming";
  if (state.reconcile.active) return "reconciling";
  if (state.reconcile.finished) return "complete";
  return "idle";
}
