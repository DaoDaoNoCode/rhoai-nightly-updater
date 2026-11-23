import { useCallback, useEffect, useState } from "react";
import { CLIENT_ERROR_CODES, getRecipeRun, toApiError, type ApiError } from "../../services/api";
import { RECIPE_RUN_IDLE_POLL_MS, RECIPE_RUN_POLL_MS } from "../../constants";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { usePolling } from "../../hooks/usePolling";
import type { RecipeRunStatus } from "../../types";
import { sameData } from "../../utils";

export interface RecipeRunValue {
  /** The last answer; null until the first one. */
  status: RecipeRunStatus | null;
  /** The last read failed (the answer before it, if any, stays). */
  error: ApiError | null;
  loading: boolean;
  /** Read it again now (after Run, Resume, Stop or Clean up). */
  refresh: () => Promise<void>;
}

/**
 * The last recipe run (GET /api/recipes/run): read once, then every
 * RECIPE_RUN_POLL_MS while it runs and every RECIPE_RUN_IDLE_POLL_MS
 * otherwise, while the tab is visible. The run goes on on the server whether
 * or not this page follows it.
 */
export function useRecipeRun(): RecipeRunValue {
  const [status, setStatus] = useState<RecipeRunStatus | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const latest = useLatestRequest();

  const refresh = useCallback(async () => {
    const ticket = latest.start();
    try {
      const next = await getRecipeRun(ticket.signal);
      if (!ticket.isLatest()) return;
      setStatus((prev) => (sameData(prev, next) ? prev : next));
      setError(null);
    } catch (e) {
      const err = toApiError(e);
      if (!ticket.isLatest() || err.errorCode === CLIENT_ERROR_CODES.aborted) return;
      setError(err);
    } finally {
      if (ticket.isLatest()) setLoading(false);
    }
  }, [latest]);

  useEffect(() => { void refresh(); }, [refresh]);
  // One sequential poller: every RECIPE_RUN_POLL_MS while the run runs,
  // otherwise every RECIPE_RUN_IDLE_POLL_MS, so a run started or resumed
  // elsewhere shows on its own; none while the tab is hidden, at once when
  // it is shown again. A run that starts or ends restarts it at its pace.
  const running = !!status?.running;
  usePolling(refresh, { delay: () => (running ? RECIPE_RUN_POLL_MS : RECIPE_RUN_IDLE_POLL_MS), pauseWhenHidden: true, refreshOnVisible: true, restartKey: running });

  return { status, error, loading, refresh };
}
