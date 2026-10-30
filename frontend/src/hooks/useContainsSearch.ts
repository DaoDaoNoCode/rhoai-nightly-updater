import { useEffect, useRef, useState } from "react";
import type { PRContainsBuild, PRContainsResponse } from "../types";
import { ApiError, getBuildContains, toApiError, type ContainsQuery } from "../services/api";
import { imageDigest } from "../components/buildDiff";
import type { SearchBuild } from "./useCommitSearch";

export interface ContainsSearchResult {
  build: SearchBuild;
  answer: PRContainsBuild;
}

export interface ContainsSearchState {
  running: boolean;
  checked: number;
  total: number;
  /** The PR or commit, once GitHub answered (title, merge commit or commit date). */
  info: Omit<PRContainsResponse, "builds"> | null;
  results: ContainsSearchResult[];
  /** The whole search failed (PR not merged or not found, commit unknown, GitHub down). */
  error: ApiError | null;
  rateLimited: boolean;
  retryAfterSeconds: number;
}

/** Builds asked about at the same time. The backend reads each build's catalog and labels, then asks GitHub once. */
export const CONTAINS_SEARCH_CONCURRENCY = 3;

const IDLE: ContainsSearchState = { running: false, checked: 0, total: 0, info: null, results: [], error: null, rateLimited: false, retryAfterSeconds: 0 };

/** Request-level failures that end the search: nothing can be said about any build. */
const FATAL_CODES = ["pr_not_merged", "pr_not_found", "commit_not_found", "upstream_error", "validation", "session_expired", "unauthorized", "forbidden"];

/** When the searched PR was merged, or the commit was committed: a build older than this cannot contain it. */
export function cutoffOf(info: Omit<PRContainsResponse, "builds"> | null): number | null {
  const date = info?.mergedAt || info?.commitDate;
  const t = date ? Date.parse(date) : NaN;
  return Number.isNaN(t) ? null : t;
}

/** The answer for a build built before the cutoff, given without a request. */
export function builtBeforeAnswer(kind: ContainsQuery["kind"], image: string): PRContainsBuild {
  return kind === "pr"
    ? { image, result: "not_contained", reason: "built_before_merge", message: "Built before the PR was merged." }
    : { image, result: "not_contained", reason: "built_before_commit", message: "Built before the commit." };
}

/**
 * Which builds contain the merged PR or the commit of `query`. The first
 * build is asked alone, so a PR that is not merged (or a commit GitHub does
 * not know) stops the search after one request and its answer gives the
 * merge or commit date; the remaining builds whose build date is before it
 * are answered locally, the rest go a few at a time, with progress. A new
 * search or unmount cancels the previous one.
 */
export function useContainsSearch(query: ContainsQuery | null, builds: SearchBuild[], nonce = 0): ContainsSearchState {
  const [state, setState] = useState<ContainsSearchState>(IDLE);
  // Callers may pass a new array on every render; only a different list of
  // images starts a new search.
  const buildsRef = useRef(builds);
  buildsRef.current = builds;
  const buildsKey = builds.map((b) => b.image).join(",");
  const kind = query?.kind ?? null;
  const value = query?.value ?? null;
  const repo = query?.repo ?? "";

  useEffect(() => {
    const builds = buildsRef.current;
    if (!kind || !value || builds.length === 0) {
      setState(IDLE);
      return;
    }
    const query: ContainsQuery = { kind, value, ...(repo ? { repo } : {}) };
    const controller = new AbortController();
    let checked = 0;
    let info: ContainsSearchState["info"] = null;
    let rateLimited = false;
    let retryAfterSeconds = 0;
    const results: ContainsSearchResult[] = [];
    setState({ ...IDLE, running: true, total: builds.length });

    const order = (b: SearchBuild) => builds.indexOf(b);
    const publish = (running: boolean, error: ApiError | null = null) => {
      if (controller.signal.aborted) return;
      setState({
        running,
        checked,
        total: builds.length,
        info,
        results: [...results].sort((x, y) => order(x.build) - order(y.build)),
        error,
        rateLimited,
        retryAfterSeconds,
      });
    };

    // Throws only for a request-level failure (the PR or commit itself), which ends the search.
    const ask = async (build: SearchBuild) => {
      if (!imageDigest(build.image)) {
        // The backend answers only for digest-pinned builds: a tag moves.
        results.push({ build, answer: { image: build.image, result: "unknown", reason: "build_unreadable", message: "The build's digest is unknown." } });
        return;
      }
      try {
        const res = await getBuildContains(query, [build.image], controller.signal);
        const { builds: answers, ...rest } = res;
        if (rest.mergeCommit || rest.commitDate || !info) info = rest;
        if (res.rateLimited) {
          rateLimited = true;
          retryAfterSeconds = Math.max(retryAfterSeconds, res.retryAfterSeconds ?? 0);
        }
        const answer = answers.find((a) => a.image === build.image) ?? answers[0];
        results.push({ build, answer: answer ?? { image: build.image, result: "unknown", reason: "github_error", message: "No answer for this build." } });
      } catch (e) {
        const err = toApiError(e, "Could not search the builds");
        if (FATAL_CODES.includes(err.errorCode)) throw err;
        results.push({ build, answer: { image: build.image, result: "unknown", reason: "build_unreadable", message: err.message } });
      }
    };

    void (async () => {
      const queue = [...builds];
      try {
        await ask(queue.shift()!);
        checked++;
        // Builds older than the merge (or the commit) cannot contain it: no request.
        const cutoff = cutoffOf(info);
        if (cutoff !== null) {
          for (const build of [...queue]) {
            const built = build.buildDate ? Date.parse(build.buildDate) : NaN;
            if (!Number.isNaN(built) && built < cutoff) {
              queue.splice(queue.indexOf(build), 1);
              results.push({ build, answer: builtBeforeAnswer(kind, build.image) });
              checked++;
            }
          }
        }
        publish(true);
        const worker = async () => {
          while (queue.length > 0 && !controller.signal.aborted) {
            await ask(queue.shift()!);
            checked++;
            publish(true);
          }
        };
        await Promise.all(Array.from({ length: Math.min(CONTAINS_SEARCH_CONCURRENCY, queue.length) }, worker));
        publish(false);
      } catch (e) {
        if (controller.signal.aborted) return;
        publish(false, toApiError(e, "Could not search the builds"));
      }
    })();
    return () => controller.abort();
  }, [kind, value, repo, nonce, buildsKey]);

  return state;
}
