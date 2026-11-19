/** The Build Explorer: nightly tags, their content, and which builds contain a PR or commit. */

import { NightlyTagsResponse, FBCContentResponse, PRContainsResponse } from '../../types';
import { request } from './client';

export function getBuildExplorerTags(includeDates = false): Promise<NightlyTagsResponse> {
  return request('/api/build-explorer/tags?includeDates=' + includeDates);
}

export function getBuildExplorerContent(image: string, labels?: boolean, signal?: AbortSignal): Promise<FBCContentResponse> {
  const params = new URLSearchParams({ image });
  if (labels) params.set('labels', 'true');
  return request(`/api/build-explorer/content?${params}`, { signal });
}

/** What a contains search looks for: a merged PR or a commit, in a repository (default opendatahub-io/odh-dashboard). */
export interface ContainsQuery {
  kind: 'pr' | 'commit';
  /** The PR number or the commit SHA (7 to 40 hex). */
  value: string;
  repo?: string;
}

/**
 * Whether the given builds contain the merged PR or the commit of `query`.
 * Errors: 422 pr_not_merged, 404 pr_not_found or commit_not_found, 400
 * validation. A GitHub rate limit is not an error: the response has
 * rateLimited and unknown builds.
 */
export function getBuildContains(query: ContainsQuery, images: string[], signal?: AbortSignal): Promise<PRContainsResponse> {
  const params = new URLSearchParams({ [query.kind]: query.value });
  if (query.repo) params.set('repo', query.repo);
  for (const image of images) params.append('image', image);
  return request(`/api/build-explorer/contains?${params}`, { signal });
}
