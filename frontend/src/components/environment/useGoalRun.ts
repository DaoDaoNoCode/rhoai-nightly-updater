import { useCallback, useEffect, useRef, useState } from "react";
import type { OperationResponse, UpdateStep } from "../../types";
import type { StreamDetachHandler, StreamDoneHandler } from "../../services/api";
import { errorResult } from "../../outcomes";
import type { StepState } from "./StepProgress";

/** Whether a part that was just created works yet, read from the page's status. */
export type ReadyCheck =
  | { state: "ready" }
  | { state: "starting"; detail?: string }
  | { state: "failed"; reason: string };

/** One part of a run: an existing request. */
export interface GoalRunStep {
  id: string;
  title: string;
  run: () => Promise<OperationResponse>;
  /**
   * The setup request returns once the part is created; the part is set up
   * only when it works. check reads the latest status (the run's refresh
   * reads it again before each later check); the step waits until it
   * reports ready (or a reason it cannot start), up to READY_TIMEOUT_MS.
   * typical is how long it usually takes ("1 to 3 minutes").
   */
  waitReady?: { check: () => ReadyCheck; typical: string };
  /** The step's line once the part works ("The pipeline server in e2e-ui is ready."). */
  readyMessage?: string;
}

/** What a run does, for its words: a setup starts things, a deletion removes them, a change changes one. */
export type GoalRunKind = "setup" | "delete" | "change";

/** A step of a run; an optional one shows only once the operation reports it (a part created only when missing). */
export interface GoalRunStepInfo {
  id: string;
  title: string;
  optional?: boolean;
}

/** The steps a run shows: every required one, and the optional ones that reported. */
export const shownSteps = (run: GoalRun) => run.steps.filter((s) => !s.optional || !!run.states[s.id]);

export interface GoalRun {
  title: string;
  kind: GoalRunKind;
  /** When it started (epoch ms), for the elapsed time while its dialog is closed. */
  startedAt: number;
  steps: GoalRunStepInfo[];
  states: Record<string, StepState>;
  running: boolean;
  /** The outcome, once the run ended. */
  result: { success: boolean; message: string; errorCode?: string } | null;
}

/** The step a run is on now, as the alert shown while its dialog is closed names it; null before the first step starts. */
export function runStepNow(run: GoalRun): { title: string; index: number; count: number } | null {
  const steps = shownSteps(run);
  const i = steps.findIndex((s) => run.states[s.id]?.status === "running");
  return i < 0 ? null : { title: steps[i].title, index: i + 1, count: steps.length };
}

/** How long a run waits for a part to work after creating it: the page's own polling stops after as long. */
export const READY_TIMEOUT_MS = 10 * 60_000;
/** How often it looks at the status while it waits. */
export const READY_CHECK_MS = 2_000;

/** The first step of a run with a preflight: every part's prerequisites, before anything changes. */
export const CHECK_STEP = { id: "check", title: "Check what every part needs" };

const clock = (ms: number) => {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
};

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export interface StartOptions {
  kind?: GoalRunKind;
  /** The run's outcome when every step succeeds. */
  doneMessage?: string;
  /**
   * Checks every step's prerequisites on fresh data before the first step
   * runs: the reason nothing can run, or null. A run never creates part of
   * a feature when a later part cannot be set up.
   */
  preflight?: () => Promise<string | null>;
  /**
   * Reads the status again before each later ready check: the run goes on
   * when its page is left, and that page's own polling stops with it. It
   * gets the time left to wait for the part. A read it cannot do (the
   * session ended) rejects: the run stops there.
   */
  refresh?: (remainingMs: number) => Promise<void>;
}

/** A streamed operation of several parts (Clean up): it reports each step itself. */
export type StreamRun = (onStep: (step: UpdateStep) => void, onDone: StreamDoneHandler, onDetach: StreamDetachHandler) => AbortController;

/**
 * Runs an operation of the Environment page and keeps its progress while
 * its dialog is closed: a feature's missing setups one after another, a
 * single change (one step), or a streamed operation of several parts. A
 * setup step succeeds only when its part works: after the request, it
 * waits (bounded) until the status reports the part ready. It stops at the
 * first step that does not succeed: later steps need the earlier ones.
 * Starting with no steps, or while a run is going, does nothing; reset
 * only clears a finished run.
 */
export function useGoalRun(now: () => number = Date.now, sleep: (ms: number) => Promise<void> = defaultSleep) {
  const [run, setRun] = useState<GoalRun | null>(null);
  const running = useRef(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);

  const setStep = useCallback((id: string, state: StepState) => setRun((r) => (r ? { ...r, states: { ...r.states, [id]: state } } : r)), []);
  const finish = useCallback((result: GoalRun["result"]) => {
    running.current = false;
    setRun((r) => (r ? { ...r, running: false, result } : r));
  }, []);

  const start = useCallback(async (title: string, steps: GoalRunStep[], options: StartOptions = {}) => {
    if (steps.length === 0 || running.current) return;
    running.current = true;
    const { kind = "setup", doneMessage, preflight, refresh } = options;
    const shown = steps.map(({ id, title: t }) => ({ id, title: t }));
    setRun({ title, kind, startedAt: now(), steps: preflight ? [CHECK_STEP, ...shown] : shown, states: {}, running: true, result: null });
    if (preflight) {
      setStep(CHECK_STEP.id, { status: "running", message: "Checking" });
      let reason: string | null;
      try {
        reason = await preflight();
      } catch (e) {
        reason = errorResult(e, "The check failed").message;
      }
      if (reason) {
        const message = `${reason} Nothing was changed.`;
        setStep(CHECK_STEP.id, { status: "failed", message });
        finish({ success: false, message, errorCode: "prerequisites" });
        return;
      }
      setStep(CHECK_STEP.id, { status: "success", message: "Everything the parts need is there." });
    }
    // Without a message of its own, a one-part run ends with what its request said ("MLflow removed.").
    let lastMessage = "";
    let result: GoalRun["result"] = { success: true, message: doneMessage ?? (steps.length > 1 ? `All ${steps.length} parts are done.` : "") };
    for (const step of steps) {
      setStep(step.id, { status: "running", message: kind === "setup" ? "Creating" : "Working" });
      const t0 = now();
      let res: OperationResponse;
      try {
        res = await step.run();
      } catch (e) {
        res = errorResult(e, "The request failed");
      }
      if (!res.success) {
        const still = res.errorCode === "in_progress";
        setStep(step.id, { status: still ? "running" : "failed", message: res.message });
        result = { success: false, message: res.message, errorCode: res.errorCode };
        break;
      }
      if (step.waitReady) {
        const t1 = now();
        let outcome: ReadyCheck = step.waitReady.check();
        let unreadable: OperationResponse | null = null;
        while (outcome.state === "starting" && now() - t1 < READY_TIMEOUT_MS) {
          const detail = outcome.detail ? ` ${outcome.detail}` : "";
          setStep(step.id, { status: "running", message: `Created. Starting (typically ${step.waitReady.typical}), ${clock(now() - t1)}.${detail}` });
          await sleep(READY_CHECK_MS);
          // The time is up while it slept: no read past it.
          if (now() - t1 >= READY_TIMEOUT_MS) break;
          if (refresh) {
            try {
              await refresh(READY_TIMEOUT_MS - (now() - t1));
            } catch (e) {
              unreadable = errorResult(e, "The status could not be read");
              break;
            }
            // The read ended past the time: a part ready only now does not start the next change.
            if (now() - t1 >= READY_TIMEOUT_MS) break;
          }
          outcome = step.waitReady.check();
        }
        if (unreadable) {
          setStep(step.id, { status: "failed", message: unreadable.message });
          result = { success: false, message: unreadable.message, errorCode: unreadable.errorCode };
          break;
        }
        if (outcome.state === "failed") {
          setStep(step.id, { status: "failed", message: outcome.reason });
          result = { success: false, message: outcome.reason };
          break;
        }
        if (outcome.state === "starting") {
          const msg = `${step.title}: created, but not ready after ${clock(READY_TIMEOUT_MS)}. It may still start; this page keeps showing its state, and its reason if it cannot start.`;
          setStep(step.id, { status: "running", message: msg });
          result = { success: false, message: msg, errorCode: "in_progress" };
          break;
        }
      }
      lastMessage = step.readyMessage ?? res.message;
      setStep(step.id, { status: "success", message: `${lastMessage} (${clock(now() - t0)})` });
    }
    if (result?.success && !result.message) result = { ...result, message: lastMessage || "Done." };
    finish(result);
  }, [now, sleep, setStep, finish]);

  /**
   * Follows a streamed operation whose steps are known up front (in their
   * order; optional ones show once reported); each event updates its step.
   */
  const follow = useCallback((title: string, steps: GoalRunStepInfo[], stream: StreamRun, kind: GoalRunKind = "delete") => {
    if (steps.length === 0 || running.current) return;
    running.current = true;
    setRun({ title, kind, startedAt: now(), steps, states: {}, running: true, result: null });
    let finalMessage = "";
    controller.current = stream(
      (step) => {
        if (step.step === "operation_complete") {
          finalMessage = step.message;
          return;
        }
        setStep(step.step, { status: step.status, message: step.message });
      },
      (success, error, apiError) => {
        controller.current = null;
        finish({ success, message: success ? finalMessage : error || finalMessage || "The operation failed.", errorCode: apiError?.errorCode });
      },
      () => {
        controller.current = null;
        finish({ success: false, message: "The connection to the server was lost. The operation continues on the server; the parts show their state when it ends.", errorCode: "detached" });
      },
    );
  }, [now, setStep, finish]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, follow, reset };
}
