import { useCallback, useEffect, useRef, useState } from "react";
import type { ClusterSummary, NightlyProblem, SummaryBuild } from "../types";
import { getSummary, toApiError, type ApiError } from "../services/api";
import { SUMMARY_POLL_MS } from "../constants";
import { usePolling } from "./usePolling";
import { useLatestRequest } from "./useLatestRequest";
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
  const latest = useLatestRequest();

  const load = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const s = await getSummary(ticket.signal);
      if (!ticket.isLatest()) return;
      setSummary(s);
      setError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError(toApiError(e, "Could not load the cluster summary"));
    } finally {
      if (ticket.isLatest()) setLoading(false);
    }
  }, [enabled, latest]);

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

/**
 * The summary's nightly part as a user sees it: the installed build's tag
 * and build date (the summary carries no image reference, so there is
 * nothing to compare by digest), whether a newer one exists (undefined
 * when that could not be checked), and why not.
 */
export interface UserNightly {
  installed: SummaryBuild | null;
  latest: SummaryBuild | null;
  /** undefined: not known (problem says why). */
  newer: boolean | undefined;
  problem: NightlyProblem;
}

export function summaryNightly(summary: ClusterSummary | null): UserNightly | null {
  const n = summary?.nightly;
  if (!n) return null;
  return {
    installed: n.installedBuild,
    latest: n.latestBuild,
    newer: n.problem === "" ? n.newerAvailable : undefined,
    problem: n.problem,
  };
}

/** Why the newest build is not known, in words; null when it is. */
export function nightlyProblemText(problem: NightlyProblem): string | null {
  switch (problem) {
    case "catalog_unreadable":
      return "Could not read which nightly build is installed.";
    case "lookup_failed":
      return "Could not check for a newer build.";
    case "no_stream":
      return "A custom catalog image: there is no stream to compare with.";
    default:
      return null;
  }
}
