import type { UpdateStep } from "../types";
import type { StreamDetachReason } from "../services/api";
import { reconcileKindFor, type OperationKind, type ReconcileKind } from "../operationSteps";

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
  /** Server-side operation identifier, when the backend provides one. */
  serverId?: string;
  /** What the run targets, e.g. the FBC image of an update. */
  detail?: string;
}

export interface ReconcileState {
  active: boolean;
  startTime: number;
  kind: ReconcileKind;
  timedOut: boolean;
  /** Reconciliation reached Succeeded or Failed; keeps the result card visible. */
  finished: boolean;
}

export interface OperationState {
  /** The current or most recent run. */
  run: OperationRun | null;
  reconcile: ReconcileState;
}

export type OperationAction =
  | { type: "start"; id: number; kind: OperationKind; source: OperationSource; now: number; serverId?: string; steps?: UpdateStep[]; detail?: string }
  | { type: "step"; id: number; step: UpdateStep }
  | { type: "end"; id: number; outcome: OperationOutcome; now: number }
  | { type: "statusPolled"; csvPhase: string }
  | { type: "reconcileTimedOut" }
  | { type: "dismissTimeout" }
  | { type: "dismissFinished" };

export const IDLE_RECONCILE: ReconcileState = { active: false, startTime: 0, kind: "update", timedOut: false, finished: false };

export function initialOperationState(reconcile: Partial<ReconcileState> = {}): OperationState {
  return { run: null, reconcile: { ...IDLE_RECONCILE, ...reconcile } };
}

export function isRunning(run: OperationRun | null): run is OperationRun & { outcome?: undefined } {
  return !!run && !run.outcome;
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
        },
        // A new run supersedes the previous run's reconcile result.
        reconcile: { ...state.reconcile, timedOut: false, finished: false },
      };
    }
    case "step": {
      const run = state.run;
      if (!run || run.id !== action.id || run.outcome) return state;
      return { ...state, run: { ...run, steps: mergeStep(run.steps, action.step) } };
    }
    case "end": {
      const run = state.run;
      if (!run || run.id !== action.id || run.outcome) return state;
      const ended: OperationRun = { ...run, outcome: action.outcome, endedAt: action.now };
      if (action.outcome.status === "failed") return { ...state, run: ended };
      return {
        run: ended,
        reconcile: { active: true, startTime: action.now, kind: reconcileKindFor(run.kind), timedOut: false, finished: false },
      };
    }
    case "statusPolled": {
      if (!state.reconcile.active) return state;
      if (action.csvPhase !== "Succeeded" && action.csvPhase !== "Failed") return state;
      return { ...state, reconcile: { ...state.reconcile, active: false, finished: true } };
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
