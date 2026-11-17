import { useCallback, useRef, useState } from "react";
import type { OperationResponse } from "../../types";
import { errorResult } from "../../outcomes";
import type { StepState } from "./StepProgress";

/** One part of a goal's setup: an existing setup request. */
export interface GoalRunStep {
  id: string;
  title: string;
  run: () => Promise<OperationResponse>;
}

export interface GoalRun {
  title: string;
  steps: { id: string; title: string }[];
  states: Record<string, StepState>;
  running: boolean;
  /** The outcome, once the run ended. */
  result: { success: boolean; message: string; errorCode?: string } | null;
}

const seconds = (ms: number) => `${Math.max(1, Math.round(ms / 1000))} s`;

/**
 * Runs a goal's missing setups one after another (each an existing setup
 * request) and keeps their progress while its dialog is closed. It stops at
 * the first one that does not succeed: later steps need the earlier ones.
 * Starting with no steps, or while a run is going, does nothing; reset only
 * clears a finished run.
 */
export function useGoalRun(now: () => number = Date.now) {
  const [run, setRun] = useState<GoalRun | null>(null);
  const running = useRef(false);

  const start = useCallback(async (title: string, steps: GoalRunStep[]) => {
    if (steps.length === 0 || running.current) return;
    running.current = true;
    setRun({ title, steps: steps.map(({ id, title: t }) => ({ id, title: t })), states: {}, running: true, result: null });
    const setStep = (id: string, state: StepState) => setRun((r) => (r ? { ...r, states: { ...r.states, [id]: state } } : r));
    let result: GoalRun["result"] = { success: true, message: steps.length > 1 ? `All ${steps.length} parts are set up.` : "Set up." };
    for (const step of steps) {
      setStep(step.id, { status: "running", message: "Setting up..." });
      const t0 = now();
      let res: OperationResponse;
      try {
        res = await step.run();
      } catch (e) {
        res = errorResult(e, "The setup failed");
      }
      const took = ` (took ${seconds(now() - t0)})`;
      if (!res.success) {
        setStep(step.id, { status: "failed", message: res.message });
        result = { success: false, message: res.message, errorCode: res.errorCode };
        break;
      }
      setStep(step.id, { status: "success", message: `${res.message}${took}` });
    }
    running.current = false;
    setRun((r) => (r ? { ...r, running: false, result } : r));
  }, [now]);

  const reset = useCallback(() => setRun((r) => (r && !r.running ? null : r)), []);
  return { run, start, reset };
}
