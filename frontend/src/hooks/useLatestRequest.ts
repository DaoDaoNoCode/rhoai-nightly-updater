import { useEffect, useMemo, useRef } from "react";

/** One request started through useLatestRequest. */
export interface RequestTicket {
  /** Pass to the API call: aborted once a newer request starts, on cancel() and on unmount. */
  signal: AbortSignal;
  /** Still the newest request of a mounted component: only then may its answer be shown. */
  isLatest(): boolean;
}

export interface LatestRequest {
  /** Start a request: the previous one, if still in flight, is aborted and its answer dropped. */
  start(): RequestTicket;
  /** Abort the request in flight and drop its answer. */
  cancel(): void;
}

/**
 * "Latest request wins" for a read that can be started again before the last
 * one answered (a poll, a refresh, a reset, another project or a rescan):
 * an older answer that arrives after a newer request started is never shown,
 * the older request is aborted rather than left to run to the client's
 * timeout, and leaving the page aborts the one in flight.
 *
 *   const ticket = latest.start();
 *   const data = await getX(ticket.signal);
 *   if (!ticket.isLatest()) return;
 */
export function useLatestRequest(): LatestRequest {
  const state = useRef({ seq: 0, mounted: true, controller: null as AbortController | null });

  useEffect(() => {
    const s = state.current;
    s.mounted = true;
    return () => {
      s.mounted = false;
      s.seq++;
      s.controller?.abort();
      s.controller = null;
    };
  }, []);

  return useMemo<LatestRequest>(() => ({
    start() {
      const s = state.current;
      s.controller?.abort();
      const controller = new AbortController();
      s.controller = controller;
      const seq = ++s.seq;
      return {
        signal: controller.signal,
        isLatest: () => s.mounted && s.seq === seq,
      };
    },
    cancel() {
      const s = state.current;
      s.seq++;
      s.controller?.abort();
      s.controller = null;
    },
  }), []);
}
