import { useCallback, useEffect, useRef, useState } from "react";
import type { ClusterSummary, NightlyStatus } from "../types";
import { getSummary, toApiError, type ApiError } from "../services/api";
import { SUMMARY_POLL_MS } from "../constants";
import { usePolling } from "./usePolling";
import { useOperation } from "../state/AppState";

export interface SummaryValue {
  summary: ClusterSummary | null;
  /** True until the first answer (or failure). */
  loading: boolean;
  /** The last read failed; the summary shown (if any) is from before. */
  error: ApiError | null;
  lastRefreshed: Date | null;
  refresh: () => void;
}

/**
 * The read-only cluster summary (GET /api/summary) a user who is not a
 * cluster-admin sees instead of the status: read on mount, every
 * SUMMARY_POLL_MS, and again when a running operation ends. Only an answer
 * newer than the one shown is kept.
 */
export function useSummary(enabled = true): SummaryValue {
  const [summary, setSummary] = useState<ClusterSummary | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const latest = useRef(0);

  const load = useCallback(async () => {
    if (!enabled) return;
    const id = ++latest.current;
    try {
      const s = await getSummary();
      if (id !== latest.current) return;
      setSummary(s);
      setError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      if (id !== latest.current) return;
      setError(toApiError(e, "Could not load the cluster summary"));
    } finally {
      if (id === latest.current) setLoading(false);
    }
  }, [enabled]);

  const poller = usePolling(load, { delay: SUMMARY_POLL_MS, runImmediately: true, enabled });
  const refresh = useCallback(() => poller.reset({ runNow: true }), [poller]);

  // An update that ends changes the summary: read it again at once.
  const { server } = useOperation();
  const wasRunning = useRef(false);
  useEffect(() => {
    if (wasRunning.current && !server.inProgress) refresh();
    wasRunning.current = server.inProgress;
  }, [server.inProgress, refresh]);

  return { summary, loading, error, lastRefreshed, refresh };
}

/** The summary's nightly builds in the shape the Build Explorer reads (status.nightly). */
export function summaryNightly(summary: ClusterSummary | null): NightlyStatus | undefined {
  const n = summary?.nightly;
  if (!n?.installedBuild) return undefined;
  return {
    installed: { image: n.installedBuild },
    ...(n.latestBuild ? { latest: { image: n.latestBuild } } : {}),
    updateAvailable: n.newerAvailable,
  };
}
