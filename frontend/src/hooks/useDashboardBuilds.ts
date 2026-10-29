import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { DashboardContainerBuild, DashboardState } from "../types";
import { ApiError, getDashboardBuilds, toApiError } from "../services/api";
import { usePolling } from "./usePolling";
import { DASHBOARD_BUILDS_POLL_MS } from "../constants";

/** What the Dashboard containers card needs to show build commits. */
export interface DashboardBuildDetails {
  builds: DashboardContainerBuild[];
  /** A fetch is in flight; the previous builds stay shown. */
  loading: boolean;
  /** The last fetch failed (shown as a small note, never over the page). */
  error: ApiError | null;
  /** GitHub is rate limited: distances are unknown until retryAfterSeconds. */
  rateLimited: boolean;
  retryAfterSeconds?: number;
}

export interface DashboardBuildsState extends DashboardBuildDetails {
  /** False once the backend answered 404: an older backend without the route. */
  supported: boolean;
  refresh: () => void;
}

/** True for a container whose build has a commit to show. */
export function isBuildContainer(running: string | undefined): boolean {
  return running === "pr" || running === "main";
}

/**
 * The commits of the PR or main builds the dashboard runs
 * (GET /api/dashboard/builds). Fetched when at least one container runs such
 * a build, again when the set of running images changes, and every
 * DASHBOARD_BUILDS_POLL_MS while the tab is visible. Never blocks or errors
 * the page: a failure is reported in `error` for an inline note. The request
 * in flight is aborted by the next one and on unmount.
 */
export function useDashboardBuilds(state: DashboardState | null): DashboardBuildsState {
  const key = useMemo(
    () => (state?.devImages ?? []).filter((i) => isBuildContainer(i.running)).map((i) => i.currentImage).sort().join("\n"),
    [state],
  );
  const [builds, setBuilds] = useState<DashboardContainerBuild[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [rateLimited, setRateLimited] = useState(false);
  const [retryAfterSeconds, setRetryAfterSeconds] = useState<number | undefined>(undefined);
  const [supported, setSupported] = useState(true);
  const [refreshKey, setRefreshKey] = useState(0);
  const controller = useRef<AbortController | null>(null);

  const fetchBuilds = useCallback(async () => {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    setLoading(true);
    try {
      const resp = await getDashboardBuilds(current.signal);
      if (current.signal.aborted) return;
      setBuilds(resp.builds ?? []);
      setRateLimited(!!resp.rateLimited);
      setRetryAfterSeconds(resp.retryAfterSeconds);
      setError(null);
    } catch (e) {
      if (current.signal.aborted) return;
      const err = toApiError(e, "Could not load the build details");
      if (err.status === 404) {
        // An older backend, or no dashboard: nothing to show, nothing to say.
        setSupported(false);
        setBuilds([]);
        setError(null);
      } else {
        setError(err);
      }
    } finally {
      if (!current.signal.aborted) setLoading(false);
    }
  }, []);

  const enabled = !!key && supported;
  useEffect(() => {
    if (!enabled) {
      controller.current?.abort();
      setBuilds([]);
      setLoading(false);
      setError(null);
      setRateLimited(false);
      return;
    }
    void fetchBuilds();
    return () => controller.current?.abort();
  }, [enabled, key, refreshKey, fetchBuilds]);

  usePolling(fetchBuilds, { delay: DASHBOARD_BUILDS_POLL_MS, enabled });

  const refresh = useCallback(() => setRefreshKey((n) => n + 1), []);
  return { builds, loading, error, rateLimited, retryAfterSeconds, supported, refresh };
}
