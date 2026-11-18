import { useCallback, useEffect, useRef, useState } from "react";
import { getPlayground, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { RESOURCE_SETTLE_MAX_MS } from "../../constants";
import type { PlaygroundStatus } from "../../types";

export const PLAYGROUND_POLL_MS = 5_000;

/** Something about a playground may still change on its own: one starting or being deleted. */
export function playgroundSettling(pg: PlaygroundStatus | null): boolean {
  return (pg?.items ?? []).some((i) => i.terminating || (i.deployed && !i.ready && !i.terminalError && !i.healthUnknown));
}

/**
 * The Playground feature's status (GET /api/resources/playground) for the
 * project, read again when refreshKey changes and every few seconds while a
 * playground is starting or being deleted (or busy says an operation runs).
 * The last answer is kept while another project's loads: its playgrounds
 * across projects still hold, and its models are only used for its project
 * (data.project says which; the tab shows the models as loading until the
 * project's answer is in). error is about the project asked for now only.
 */
export function usePlaygroundStatus(project: string | null, refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<PlaygroundStatus | null>(null);
  const [error, setError] = useState<{ project: string | null; message: string } | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const current = useRef(project);
  current.current = project;

  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    try {
      const pg = await getPlayground(project);
      if (current.current === project) {
        setData(pg);
        setError(null);
      }
    } catch (e) {
      if (current.current === project) setError({ project, message: toApiError(e, "Cannot read the playground").message });
    }
  }, [project, enabled]);

  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);

  usePolling(fetchNow, {
    enabled: enabled && (busy || playgroundSettling(data)),
    restartKey: pollRound,
    delay: ({ elapsedMs }) => (elapsedMs >= RESOURCE_SETTLE_MAX_MS ? null : PLAYGROUND_POLL_MS),
  });

  return { data, error: error?.project === project ? error.message : null, reload: fetchNow };
}
