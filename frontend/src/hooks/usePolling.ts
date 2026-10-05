import { useEffect, useMemo, useRef } from "react";

export interface PollContext {
  /** Polls completed since the poller started or was last reset. */
  attempt: number;
  /** Milliseconds since the poller started or was last reset. */
  elapsedMs: number;
}

/** Milliseconds until the next poll, or null to stop polling for good. */
export type PollDelay = number | ((ctx: PollContext) => number | null);

export interface PollerOptions {
  task: () => unknown;
  delay: PollDelay;
  /** Skip polls while document.hidden (default true). */
  pauseWhenHidden?: boolean;
  /** Poll at once when the tab becomes visible again (default true). */
  refreshOnVisible?: boolean;
  /** Run the task when the poller starts instead of after the first delay (default false). */
  runImmediately?: boolean;
  now?: () => number;
}

export interface Poller {
  start(): void;
  stop(): void;
  /** Restart the backoff and the elapsed clock. */
  reset(opts?: { runNow?: boolean }): void;
  /** True once the delay function returned null. */
  readonly finished: boolean;
}

function isHidden(): boolean {
  return typeof document !== "undefined" && document.hidden;
}

/**
 * Sequential poller: the next poll is scheduled only after the previous task
 * settles, so slow responses never overlap. While the tab is hidden, polls
 * are skipped; when it becomes visible again the poller runs at once and
 * resumes its schedule.
 */
export function createPoller(options: PollerOptions): Poller {
  const now = options.now ?? (() => Date.now());
  const pauseWhenHidden = options.pauseWhenHidden ?? true;
  const refreshOnVisible = options.refreshOnVisible ?? true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let running = false;
  let inFlight = false;
  let finished = false;
  let missedWhileHidden = false;
  let attempt = 0;
  let startedAt = 0;
  // Bumped on stop/reset so a task that settles afterwards cannot reschedule.
  let generation = 0;

  const clear = () => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };

  const schedule = () => {
    clear();
    if (!running || finished) return;
    const delay = typeof options.delay === "function"
      ? options.delay({ attempt, elapsedMs: now() - startedAt })
      : options.delay;
    if (delay === null) {
      finished = true;
      return;
    }
    timer = setTimeout(tick, Math.max(0, delay));
  };

  const run = () => {
    if (!running || finished || inFlight) return;
    clear();
    inFlight = true;
    const gen = generation;
    const settle = () => {
      if (gen !== generation) return;
      inFlight = false;
      attempt += 1;
      schedule();
    };
    try {
      Promise.resolve(options.task()).then(settle, settle);
    } catch {
      settle();
    }
  };

  const tick = () => {
    timer = undefined;
    if (pauseWhenHidden && isHidden()) {
      missedWhileHidden = true;
      return;
    }
    run();
  };

  const onVisibilityChange = () => {
    if (!running || finished || isHidden()) return;
    if (refreshOnVisible || missedWhileHidden) {
      missedWhileHidden = false;
      run();
    }
  };

  return {
    start() {
      if (running) return;
      running = true;
      finished = false;
      attempt = 0;
      startedAt = now();
      if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibilityChange);
      if (options.runImmediately && !(pauseWhenHidden && isHidden())) run();
      else if (options.runImmediately) missedWhileHidden = true;
      else schedule();
    },
    stop() {
      running = false;
      generation += 1;
      inFlight = false;
      clear();
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibilityChange);
    },
    reset(opts) {
      if (!running) return;
      generation += 1;
      inFlight = false;
      finished = false;
      attempt = 0;
      startedAt = now();
      if (opts?.runNow) run();
      else schedule();
    },
    get finished() {
      return finished;
    },
  };
}

export interface UsePollingOptions {
  delay: PollDelay;
  /** Polling runs only while enabled (default true). Toggling restarts it. */
  enabled?: boolean;
  pauseWhenHidden?: boolean;
  refreshOnVisible?: boolean;
  runImmediately?: boolean;
  /** Changing the key restarts the poller (resets backoff and elapsed time). */
  restartKey?: unknown;
}

/**
 * React wrapper around createPoller. The task and delay are read from refs,
 * so callers can pass inline functions without restarting the poller.
 */
export function usePolling(task: () => unknown, options: UsePollingOptions): Pick<Poller, "reset"> {
  const taskRef = useRef(task);
  const delayRef = useRef(options.delay);
  useEffect(() => {
    taskRef.current = task;
    delayRef.current = options.delay;
  });
  const pollerRef = useRef<Poller | null>(null);
  const { enabled = true, pauseWhenHidden, refreshOnVisible, runImmediately, restartKey } = options;

  useEffect(() => {
    if (!enabled) return;
    const poller = createPoller({
      task: () => taskRef.current(),
      delay: (ctx) => {
        const d = delayRef.current;
        return typeof d === "function" ? d(ctx) : d;
      },
      pauseWhenHidden,
      refreshOnVisible,
      runImmediately,
    });
    pollerRef.current = poller;
    poller.start();
    return () => {
      poller.stop();
      if (pollerRef.current === poller) pollerRef.current = null;
    };
  }, [enabled, pauseWhenHidden, refreshOnVisible, runImmediately, restartKey]);

  return useMemo(() => ({
    reset: (opts?: { runNow?: boolean }) => pollerRef.current?.reset(opts),
  }), []);
}

/** Exponential backoff: base, 2x base, 4x base... capped at max. */
export function exponentialBackoff(attempt: number, baseMs: number, maxMs: number): number {
  return Math.min(maxMs, baseMs * 2 ** Math.max(0, attempt));
}
