import { useCallback, useEffect, useState } from "react";
import { getModelRegistry, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { RESOURCE_SETTLE_MAX_MS } from "../../constants";
import type { ModelRegistryStatus } from "../../types";

export const MODEL_REGISTRY_POLL_MS = 5_000;

/** The registry may still change on its own: starting or being deleted. */
export function modelRegistrySettling(mr: ModelRegistryStatus | null): boolean {
  return mr?.state === "starting" || mr?.state === "terminating";
}

/**
 * The Model registry feature's status (GET /api/resources/model-registry),
 * read again when refreshKey changes and every few seconds while the
 * registry is starting or being deleted (or busy says an operation runs).
 */
export function useModelRegistryStatus(refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<ModelRegistryStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const latest = useLatestRequest();

  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const mr = await getModelRegistry(ticket.signal);
      if (!ticket.isLatest()) return;
      setData(mr);
      setError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError(toApiError(e, "Cannot read the model registry").message);
    }
  }, [enabled, latest]);

  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);

  usePolling(fetchNow, {
    enabled: enabled && (busy || modelRegistrySettling(data)),
    restartKey: pollRound,
    delay: ({ elapsedMs }) => (elapsedMs >= RESOURCE_SETTLE_MAX_MS ? null : MODEL_REGISTRY_POLL_MS),
  });

  return { data, error, reload: fetchNow };
}
