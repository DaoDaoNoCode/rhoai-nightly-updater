import { useCallback, useEffect, useRef, useState } from "react";
import { toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { RESOURCE_SETTLE_MAX_MS } from "../../constants";

/** How often a sample whose state changes on its own (a pipeline run) is read again. */
export const SAMPLE_POLL_MS = 5_000;

export interface SampleStatusResult<T> {
  /** The project's status, once read (never another project's). */
  data: T | null;
  error: string | null;
  loading: boolean;
  /** Polling stopped after RESOURCE_SETTLE_MAX_MS while the state still changed. */
  gaveUp: boolean;
  reload: () => void;
}

/**
 * Reads a project's sample status when the project is chosen, again when
 * refreshKey changes (a sample operation ended), and every SAMPLE_POLL_MS
 * while active(data) says it changes on its own, for at most 10 minutes
 * like the page's own polling. A response for a project no longer chosen
 * is dropped.
 */
export function useSampleStatus<T>(
  load: (project: string) => Promise<T>,
  project: string | null,
  refreshKey: number,
  active: (data: T) => boolean = () => false,
): SampleStatusResult<T> {
  const [state, setState] = useState<{ project: string; data: T | null; error: string | null }>({ project: "", data: null, error: null });
  const [loading, setLoading] = useState(false);
  const [gaveUp, setGaveUp] = useState(false);
  const [pollRound, setPollRound] = useState(0);
  const current = useRef(project);
  current.current = project;

  const fetchNow = useCallback(async () => {
    if (!project) return;
    setLoading(true);
    try {
      const data = await load(project);
      if (current.current === project) setState({ project, data, error: null });
    } catch (e) {
      if (current.current === project) setState((s) => ({ project, data: s.project === project ? s.data : null, error: toApiError(e, "Cannot read the sample data").message }));
    } finally {
      if (current.current === project) setLoading(false);
    }
  }, [load, project]);

  useEffect(() => {
    setGaveUp(false);
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);

  const data = state.project === project ? state.data : null;
  usePolling(fetchNow, {
    enabled: !!data && active(data),
    restartKey: pollRound,
    delay: ({ elapsedMs }) => {
      if (elapsedMs >= RESOURCE_SETTLE_MAX_MS) {
        setGaveUp(true);
        return null;
      }
      return SAMPLE_POLL_MS;
    },
  });

  const reload = useCallback(() => {
    setGaveUp(false);
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow]);

  return { data, error: state.project === project ? state.error : null, loading, gaveUp, reload };
}
