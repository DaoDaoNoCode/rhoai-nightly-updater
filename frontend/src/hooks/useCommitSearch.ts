import { useEffect, useRef, useState } from "react";
import type { RelatedImage } from "../types";
import { getBuildExplorerContent, toApiError } from "../services/api";
import { imagesBuiltFrom } from "../components/buildDiff";

export interface SearchBuild {
  image: string;
  /** Display name: the tag, or "Installed". */
  label: string;
}

export interface CommitMatch {
  build: SearchBuild;
  images: RelatedImage[];
}

export interface CommitSearchState {
  running: boolean;
  checked: number;
  total: number;
  matches: CommitMatch[];
  /** Builds that could not be read, with the reason. */
  failures: { build: SearchBuild; message: string }[];
}

/** Builds read at the same time: the backend resolves labels per image, so keep this small. */
export const COMMIT_SEARCH_CONCURRENCY = 2;

const IDLE: CommitSearchState = { running: false, checked: 0, total: 0, matches: [], failures: [] };

/**
 * Find the builds with a component built from a commit (A08-5). Reads each
 * build's content with git labels (cached by the backend after the first
 * read), a few at a time, and reports progress. A new search or unmount
 * cancels the requests of the previous one.
 */
export function useCommitSearch(sha: string | null, builds: SearchBuild[], nonce = 0): CommitSearchState {
  const [state, setState] = useState<CommitSearchState>(IDLE);
  // Callers may pass a new array on every render; only a different list of
  // images starts a new search.
  const buildsRef = useRef(builds);
  buildsRef.current = builds;
  const buildsKey = builds.map((b) => b.image).join(",");

  useEffect(() => {
    const builds = buildsRef.current;
    if (!sha || builds.length === 0) {
      setState(IDLE);
      return;
    }
    const controller = new AbortController();
    const queue = [...builds];
    let checked = 0;
    const matches: CommitMatch[] = [];
    const failures: CommitSearchState["failures"] = [];
    setState({ running: true, checked: 0, total: builds.length, matches: [], failures: [] });

    const publish = (running: boolean) => {
      if (controller.signal.aborted) return;
      // Report in list order, whatever order the requests finish in.
      const order = (b: SearchBuild) => builds.indexOf(b);
      setState({
        running,
        checked,
        total: builds.length,
        matches: [...matches].sort((x, y) => order(x.build) - order(y.build)),
        failures: [...failures].sort((x, y) => order(x.build) - order(y.build)),
      });
    };

    const worker = async () => {
      while (queue.length > 0 && !controller.signal.aborted) {
        const build = queue.shift()!;
        try {
          const content = await getBuildExplorerContent(build.image, true, controller.signal);
          const images = imagesBuiltFrom(content.relatedImages ?? [], sha);
          if (images.length > 0) matches.push({ build, images });
        } catch (e) {
          if (controller.signal.aborted) return;
          failures.push({ build, message: toApiError(e, "Could not read the build").message });
        }
        checked++;
        publish(true);
      }
    };

    void Promise.all(Array.from({ length: Math.min(COMMIT_SEARCH_CONCURRENCY, builds.length) }, worker)).then(() => publish(false));
    return () => controller.abort();
  }, [sha, nonce, buildsKey]);

  return state;
}
