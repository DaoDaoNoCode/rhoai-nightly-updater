import { useCallback, useEffect, useRef, useState } from "react";

interface AsyncDataResult<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  lastRefreshed: Date | null;
  refresh: () => void;
}

/**
 * Generic hook for fetching data on mount with optional polling.
 *
 * Handles:
 *  - Initial fetch on mount
 *  - Polling at `pollInterval` (when provided)
 *  - Skipping polls while the tab is hidden (`document.hidden`)
 *  - Immediate refresh when tab becomes visible again
 *  - Loading, error, and last-refreshed state
 *  - Preserves stale data on refresh failure (only shows loading spinner on initial fetch)
 *  - Ignores responses that arrive after a newer request was started
 *
 * Note: `fetchFn` is stored in a ref, so callers do not need to memoize it.
 * Inline arrow functions are safe and will not cause infinite re-fetch loops.
 */
export function useAsyncData<T>(
  fetchFn: () => Promise<T>,
  pollInterval?: number,
): AsyncDataResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  // Keep fetchFn in a ref so the effect/callback identity never changes
  // when callers pass an unstable (inline) function reference.
  const fetchRef = useRef(fetchFn);
  fetchRef.current = fetchFn;

  // Tracks whether data has loaded, without reading state inside an updater.
  const hasDataRef = useRef(false);
  // Polls can overlap with slow requests; only the newest request may update
  // state, so an older response never overwrites a newer one.
  const latestRequestRef = useRef(0);

  const refresh = useCallback(async () => {
    const requestId = ++latestRequestRef.current;
    // Only show the loading spinner on the initial fetch.
    // Subsequent refreshes keep stale data visible to avoid a flash of spinner.
    if (!hasDataRef.current) setLoading(true);
    setError(null);
    try {
      const result = await fetchRef.current();
      if (requestId !== latestRequestRef.current) return;
      hasDataRef.current = true;
      setData(result);
      setLastRefreshed(new Date());
    } catch (e) {
      if (requestId !== latestRequestRef.current) return;
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      if (requestId === latestRequestRef.current) setLoading(false);
    }
  }, []);

  // Fetch on mount (runs exactly once because refresh identity is stable)
  useEffect(() => {
    refresh();
  }, [refresh]);

  // Optional polling + visibility-change listener
  useEffect(() => {
    if (!pollInterval) return;

    const id = setInterval(() => {
      if (document.hidden) return;
      refresh();
    }, pollInterval);

    // Refresh immediately when the user returns to the tab
    const onVisibilityChange = () => {
      if (!document.hidden) {
        refresh();
      }
    };
    document.addEventListener("visibilitychange", onVisibilityChange);

    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [pollInterval, refresh]);

  return { data, loading, error, lastRefreshed, refresh };
}
