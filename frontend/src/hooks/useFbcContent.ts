import { useEffect, useState } from "react";
import type { FBCContentResponse } from "../types";
import { ApiError, getBuildExplorerContent, toApiError } from "../services/api";

export interface FbcContentState {
  /** Content of `image`; never content of an earlier image. */
  data: FBCContentResponse | null;
  loading: boolean;
  labelsLoading: boolean;
  /** True once the labels phase finished (successfully or not). */
  labelsDone: boolean;
  error: ApiError | null;
}

const EMPTY: FbcContentState = { data: null, loading: false, labelsLoading: false, labelsDone: false, error: null };

/**
 * Two-phase FBC content fetch: the catalog first, then git labels for its
 * related images (slow on a cold cache). Each image gets its own
 * AbortController, so a slow response for a previous image is cancelled and
 * can never be shown for the current one (A06-8). Pass null to stop;
 * change refreshKey to fetch the same image again.
 */
export function useFbcContent(image: string | null, refreshKey?: unknown): FbcContentState {
  const [state, setState] = useState<FbcContentState>(EMPTY);

  useEffect(() => {
    if (!image) {
      setState(EMPTY);
      return;
    }
    const controller = new AbortController();
    const { signal } = controller;
    setState({ ...EMPTY, loading: true });
    (async () => {
      let content: FBCContentResponse;
      try {
        content = await getBuildExplorerContent(image, false, signal);
      } catch (e) {
        if (!signal.aborted) setState({ ...EMPTY, error: toApiError(e, "Failed to load catalog content") });
        return;
      }
      if (signal.aborted) return;
      const hasImages = (content.relatedImages?.length ?? 0) > 0;
      setState({ data: content, loading: false, labelsLoading: hasImages, labelsDone: !hasImages, error: null });
      if (!hasImages) return;
      try {
        const enriched = await getBuildExplorerContent(image, true, signal);
        if (!signal.aborted) setState({ data: enriched, loading: false, labelsLoading: false, labelsDone: true, error: null });
      } catch {
        // Labels are optional; keep the unlabelled content.
        if (!signal.aborted) setState((prev) => ({ ...prev, labelsLoading: false, labelsDone: true }));
      }
    })();
    return () => controller.abort();
  }, [image, refreshKey]);

  return state;
}
