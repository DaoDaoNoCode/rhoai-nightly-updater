import { useEffect, useRef, useState } from "react";
import type { PRContainsBuild, PRContainsResponse } from "../types";
import { ApiError, getPRContains, toApiError } from "../services/api";
import { imageDigest } from "../components/buildDiff";
import type { SearchBuild } from "./useCommitSearch";

export interface PRSearchResult {
  build: SearchBuild;
  answer: PRContainsBuild;
}

export interface PRSearchState {
  running: boolean;
  checked: number;
  total: number;
  /** The PR, once GitHub answered (title, merge commit). */
  pr: Omit<PRContainsResponse, "builds"> | null;
  results: PRSearchResult[];
  /** The whole search failed (PR not merged or not found, GitHub down). */
  error: ApiError | null;
  rateLimited: boolean;
  retryAfterSeconds: number;
}

/** Builds asked about at the same time. The backend reads each build's catalog and labels, then asks GitHub once. */
export const PR_SEARCH_CONCURRENCY = 3;

const IDLE: PRSearchState = { running: false, checked: 0, total: 0, pr: null, results: [], error: null, rateLimited: false, retryAfterSeconds: 0 };

/**
 * Which builds contain merged PR `pr`. The first build is asked alone, so a
 * PR that is not merged (or does not exist) stops the search after one
 * request; the rest go a few at a time, with progress. A new search or
 * unmount cancels the previous one.
 */
export function usePRSearch(pr: number | null, builds: SearchBuild[], nonce = 0): PRSearchState {
  const [state, setState] = useState<PRSearchState>(IDLE);
  const buildsRef = useRef(builds);
  buildsRef.current = builds;
  const buildsKey = builds.map((b) => b.image).join(",");

  useEffect(() => {
    const builds = buildsRef.current;
    if (!pr || builds.length === 0) {
      setState(IDLE);
      return;
    }
    const controller = new AbortController();
    let checked = 0;
    let prInfo: PRSearchState["pr"] = null;
    let rateLimited = false;
    let retryAfterSeconds = 0;
    const results: PRSearchResult[] = [];
    setState({ ...IDLE, running: true, total: builds.length });

    const order = (b: SearchBuild) => builds.indexOf(b);
    const publish = (running: boolean, error: ApiError | null = null) => {
      if (controller.signal.aborted) return;
      setState({
        running,
        checked,
        total: builds.length,
        pr: prInfo,
        results: [...results].sort((x, y) => order(x.build) - order(y.build)),
        error,
        rateLimited,
        retryAfterSeconds,
      });
    };

    // Throws only for a request-level failure (the PR itself), which ends the search.
    const ask = async (build: SearchBuild) => {
      if (!imageDigest(build.image)) {
        // The backend answers only for digest-pinned builds: a tag moves.
        results.push({ build, answer: { image: build.image, result: "unknown", reason: "build_unreadable", message: "The build's digest is unknown." } });
        return;
      }
      try {
        const res = await getPRContains(pr, [build.image], controller.signal);
        const { builds: answers, ...info } = res;
        if (info.mergeCommit || !prInfo) prInfo = info;
        if (res.rateLimited) {
          rateLimited = true;
          retryAfterSeconds = Math.max(retryAfterSeconds, res.retryAfterSeconds ?? 0);
        }
        const answer = answers.find((a) => a.image === build.image) ?? answers[0];
        results.push({ build, answer: answer ?? { image: build.image, result: "unknown", reason: "github_error", message: "No answer for this build." } });
      } catch (e) {
        const err = toApiError(e, "Could not search the builds");
        if (["pr_not_merged", "pr_not_found", "upstream_error", "validation", "session_expired", "unauthorized", "forbidden"].includes(err.errorCode)) throw err;
        results.push({ build, answer: { image: build.image, result: "unknown", reason: "build_unreadable", message: err.message } });
      }
    };

    void (async () => {
      const queue = [...builds];
      try {
        await ask(queue.shift()!);
        checked++;
        publish(true);
        const worker = async () => {
          while (queue.length > 0 && !controller.signal.aborted) {
            await ask(queue.shift()!);
            checked++;
            publish(true);
          }
        };
        await Promise.all(Array.from({ length: Math.min(PR_SEARCH_CONCURRENCY, queue.length) }, worker));
        publish(false);
      } catch (e) {
        if (controller.signal.aborted) return;
        publish(false, toApiError(e, "Could not search the builds"));
      }
    })();
    return () => controller.abort();
  }, [pr, nonce, buildsKey]);

  return state;
}
