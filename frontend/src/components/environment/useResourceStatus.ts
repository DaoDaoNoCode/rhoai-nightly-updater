import { useCallback, useEffect, useRef, useState } from "react";
import type { ResourcesStatus } from "../../types";
import { getDSProjects, getResourcesStatus, toApiError, type ApiError } from "../../services/api";
import { exponentialBackoff, usePolling } from "../../hooks/usePolling";
import { IDLE_REFRESH_MS, RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS, RESOURCE_SETTLE_MAX_MS } from "../../constants";
import { useLatestRequest } from "../../hooks/useLatestRequest";

/**
 * The Environment page's resource status and data science projects. They
 * load together, on every refresh and poll, and are shown together: the
 * first view never has part of the projects, and a project deleted
 * elsewhere leaves the picker with the next load. Only the latest read is
 * shown: an earlier one (a poll, another project's) that answers later
 * would show older data.
 *
 * initialPipelinesProject: the Pipelines tab's project on the first render;
 * selectedPipelinesProject keeps the one chosen since (the status also reads
 * its dashboard Connection from the Secret itself).
 */
export function useResourceStatus(initialPipelinesProject: string | null) {
  const [status, setStatus] = useState<ResourcesStatus | null>(null);
  // The latest status, for a Set up run that waits until its parts work.
  const statusRef = useRef<ResourcesStatus | null>(null);
  statusRef.current = status;
  const [projectList, setProjectList] = useState<string[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [projectsError, setProjectsError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const selectedPipelinesProject = useRef<string | null>(initialPipelinesProject);
  // The project the status shown was read for: until the selected project's
  // is in, its Connection shows as loading (never another project's).
  const [statusProject, setStatusProject] = useState<string | null>(null);
  const statusRequest = useLatestRequest();
  const fetchResources = useCallback(async () => {
    const ticket = statusRequest.start();
    const asked = selectedPipelinesProject.current;
    setLoading(true);
    const [s, p] = await Promise.allSettled([getResourcesStatus(asked, ticket.signal), getDSProjects(ticket.signal)]);
    if (!ticket.isLatest()) return;
    if (p.status === "fulfilled") {
      setProjectList(p.value.projects || []);
      setProjectsError(null);
    } else {
      // Keep the last list; until one is read, no project list is shown as empty.
      setProjectsError(toApiError(p.reason, "Could not load the data science projects"));
    }
    if (s.status === "fulfilled") {
      setStatus(s.value);
      setStatusProject(asked);
      setError(null);
      setLastRefreshed(new Date());
    } else {
      // Keep the last known state; show why it may be stale.
      setError(toApiError(s.reason, "Could not load resource status"));
    }
    setLoading(false);
  }, [statusRequest]);

  useEffect(() => { void fetchResources(); }, [fetchResources]);

  /** Reads the status for the selected project now and shows it (a Set up's preflight). */
  const readFresh = async (): Promise<ResourcesStatus> => {
    const asked = selectedPipelinesProject.current;
    const fresh = await getResourcesStatus(asked);
    setStatus(fresh);
    setStatusProject(asked);
    statusRef.current = fresh;
    return fresh;
  };

  return { status, statusRef, projectList, error, projectsError, loading, lastRefreshed, statusProject, selectedPipelinesProject, fetchResources, readFresh };
}

/**
 * While `active`, reads again: quickly after a change, backing off to 30 s,
 * stopping after 10 minutes (gaveUp); hidden tabs don't poll (A06-5).
 * restart() starts that again from the quick end.
 */
export function useSettlePolling(fetch: () => Promise<void>, active: boolean) {
  const [round, setRound] = useState(0);
  const [gaveUp, setGaveUp] = useState(false);
  usePolling(fetch, {
    enabled: active,
    restartKey: round,
    delay: ({ attempt, elapsedMs }) => {
      if (elapsedMs >= RESOURCE_SETTLE_MAX_MS) {
        setGaveUp(true);
        return null;
      }
      return exponentialBackoff(attempt, RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS);
    },
  });
  const restart = () => {
    setGaveUp(false);
    setRound((n) => n + 1);
  };
  return { gaveUp, restart };
}

/**
 * While enabled (nothing settles, so no other poller reads), reads again
 * every IDLE_REFRESH_MS while the browser tab is visible and at once when it
 * becomes visible again; the reads never overlap (createPoller is sequential).
 */
export function useIdleRefresh(fetch: () => unknown, enabled: boolean) {
  usePolling(fetch, { enabled, delay: IDLE_REFRESH_MS, pauseWhenHidden: true, refreshOnVisible: true });
}
