import { useCallback, useRef, useState } from "react";
import type { OperationResponse } from "../../types";
import { errorResult } from "../../outcomes";
import type { StepState } from "./StepProgress";

/** Whether a part that was just created works yet, read from the page's status. */
export type ReadyCheck =
  | { state: "ready" }
  | { state: "starting"; detail?: string }
  | { state: "failed"; reason: string };

/** One part of a feature's setup: an existing setup request. */
export interface GoalRunStep {
  id: string;
  title: string;
  run: () => Promise<OperationResponse>;
  /**
   * The setup request returns once the part is created; the part is set up
   * only when it works. check reads the status the page keeps loading; the
   * step waits until it reports ready (or a reason it cannot start), up to
   * READY_TIMEOUT_MS. typical is how long it usually takes ("1 to 3 minutes").
   */
  waitReady?: { check: () => ReadyCheck; typical: string };
  /** The step's line once the part works ("The pipeline server in e2e-ui is ready."). */
  readyMessage?: string;
}

export interface GoalRun {
  title: string;
  steps: { id: string; title: string }[];
  states: Record<string, StepState>;
  running: boolean;
  /** The outcome, once the run ended. */
  result: { success: boolean; message: string; errorCode?: string } | null;
}

/** How long a run waits for a part to work after creating it: the page's own polling stops after as long. */
export const READY_TIMEOUT_MS = 10 * 60_000;
/** How often it looks at the status while it waits. */
export const READY_CHECK_MS = 2_000;

const clock = (ms: number) => {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
};

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/**
 * Runs a feature's missing setups one after another and keeps their
 * progress while its dialog is closed. A step succeeds only when its part
 * works: after the setup request, it waits (bounded) until the status
 * reports the part ready. It stops at the first step that does not succeed:
 * later steps need the earlier ones. Starting with no steps, or while a run
 * is going, does nothing; reset only clears a finished run.
 */
export function useGoalRun(now: () => number = Date.now, sleep: (ms: number) => Promise<void> = defaultSleep) {
  const [run, setRun] = useState<GoalRun | null>(null);
  const running = useRef(false);

  const start = useCallback(async (title: string, steps: GoalRunStep[], doneMessage?: string) => {
    if (steps.length === 0 || running.current) return;
    running.current = true;
    setRun({ title, steps: steps.map(({ id, title: t }) => ({ id, title: t })), states: {}, running: true, result: null });
    const setStep = (id: string, state: StepState) => setRun((r) => (r ? { ...r, states: { ...r.states, [id]: state } } : r));
    let result: GoalRun["result"] = { success: true, message: doneMessage ?? (steps.length > 1 ? `All ${steps.length} parts are set up and working.` : "It is set up and working.") };
    for (const step of steps) {
      setStep(step.id, { status: "running", message: "Creating..." });
      const t0 = now();
      let res: OperationResponse;
      try {
        res = await step.run();
      } catch (e) {
        res = errorResult(e, "The setup failed");
      }
      if (!res.success) {
        setStep(step.id, { status: "failed", message: res.message });
        result = { success: false, message: res.message, errorCode: res.errorCode };
        break;
      }
      if (step.waitReady) {
        const t1 = now();
        let outcome: ReadyCheck = step.waitReady.check();
        while (outcome.state === "starting" && now() - t1 < READY_TIMEOUT_MS) {
          const detail = outcome.detail ? ` ${outcome.detail}` : "";
          setStep(step.id, { status: "running", message: `Created. Starting (typically ${step.waitReady.typical}), ${clock(now() - t1)}.${detail}` });
          await sleep(READY_CHECK_MS);
          outcome = step.waitReady.check();
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
      setStep(step.id, { status: "success", message: `${step.readyMessage ?? res.message} (${clock(now() - t0)})` });
    }
    running.current = false;
    setRun((r) => (r ? { ...r, running: false, result } : r));
  }, [now, sleep]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, reset };
}
