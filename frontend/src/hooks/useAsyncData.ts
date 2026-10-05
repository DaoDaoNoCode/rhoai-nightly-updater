import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, toApiError } from "../services/api";
import { usePolling } from "./usePolling";

interface AsyncDataResult<T> {
  data: T | null;
  loading: boolean;
  error: ApiError | null;
  lastRefreshed: Date | null;
  refresh: () => Promise<void>;
}

/**
 * Generic hook for fetching data on mount with optional polling.
 *
 * Handles:
 *  - Initial fetch on mount
 *  - Polling at `pollInterval` (when provided), one request at a time
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
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  // Keep fetchFn in a ref so the effect/callback identity never changes
  // when callers pass an unstable (inline) function reference.
  const fetchRef = useRef(fetchFn);
  useEffect(() => {
    fetchRef.current = fetchFn;
  });

  // Tracks whether data has loaded, without reading state inside an updater.
  const hasDataRef = useRef(false);
  // Polls can overlap with slow manual refreshes; only the newest request may
  // update state, so an older response never overwrites a newer one.
  const latestRequestRef = useRef(0);
  const mountedRef = useRef(true);

  const refresh = useCallback(async () => {
    const requestId = ++latestRequestRef.current;
    // Only show the loading spinner on the initial fetch.
    // Subsequent refreshes keep stale data visible to avoid a flash of spinner.
    if (!hasDataRef.current) setLoading(true);
    try {
      const result = await fetchRef.current();
      if (!mountedRef.current || requestId !== latestRequestRef.current) return;
      hasDataRef.current = true;
      setData(result);
      setError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      if (!mountedRef.current || requestId !== latestRequestRef.current) return;
      setError(toApiError(e));
    } finally {
      if (mountedRef.current && requestId === latestRequestRef.current) setLoading(false);
    }
  }, []);

  // Fetch on mount (runs exactly once because refresh identity is stable)
  useEffect(() => {
    mountedRef.current = true;
    refresh();
    return () => {
      mountedRef.current = false;
    };
  }, [refresh]);

  usePolling(refresh, { delay: pollInterval ?? 0, enabled: !!pollInterval });

  return { data, loading, error, lastRefreshed, refresh };
}
