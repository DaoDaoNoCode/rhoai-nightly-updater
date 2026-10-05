import type { NightlyBuild, StatusResponse } from "./types";

/** Digest prefix length, the same as the backend's activity "build" field (pkg/cluster/activity.go). */
export const SHORT_DIGEST_LEN = 12;
export const SHORT_COMMIT_LEN = 7;

export interface ImageRef {
  repository: string;
  tag?: string;
  /** "sha256:<hex>" */
  digest?: string;
}

/** Split "registry/repo:tag@sha256:..." into its parts. */
export function parseImageRef(image: string): ImageRef {
  const trimmed = image.trim();
  const at = trimmed.indexOf("@");
  const digest = at >= 0 ? trimmed.slice(at + 1) : undefined;
  const name = at >= 0 ? trimmed.slice(0, at) : trimmed;
  const lastSlash = name.lastIndexOf("/");
  const colon = name.lastIndexOf(":");
  const tag = colon > lastSlash ? name.slice(colon + 1) : undefined;
  const repository = colon > lastSlash ? name.slice(0, colon) : name;
  return { repository, tag: tag || undefined, digest: digest || undefined };
}

/** "sha256:4eff06d6..." -> "4eff06d6a1b2" */
export function shortDigest(digest?: string): string | undefined {
  if (!digest) return undefined;
  const hex = digest.replace(/^sha256:/, "");
  return hex ? hex.slice(0, SHORT_DIGEST_LEN) : undefined;
}

export function shortCommit(sha?: string): string | undefined {
  return sha ? sha.slice(0, SHORT_COMMIT_LEN) : undefined;
}

/** A NightlyBuild for any image reference (tag and digest only). */
export function buildFromImage(image: string): NightlyBuild {
  const ref = parseImageRef(image);
  return { image, tag: ref.tag, digest: ref.digest };
}

/** Date and time in the user's locale, e.g. "Oct 5, 9:47 PM"; the year only when it differs. */
export function formatBuildDate(iso?: string, now = new Date()): string | undefined {
  if (!iso) return undefined;
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return undefined;
  const opts: Intl.DateTimeFormatOptions = { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" };
  if (date.getFullYear() !== now.getFullYear()) opts.year = "numeric";
  return date.toLocaleString(undefined, opts);
}

/** "github.com/org/repo" (+ ".git") and a sha -> the commit page, or undefined for anything else. */
export function commitURL(gitURL: string | undefined, sha: string | undefined): string | undefined {
  if (!gitURL || !sha || !/^[0-9a-f]{7,40}$/i.test(sha)) return undefined;
  const base = gitURL.replace(/\.git$/, "").replace(/\/$/, "");
  if (!/^https:\/\/github\.com\/[\w.-]+\/[\w.-]+$/.test(base)) return undefined;
  return `${base}/commit/${sha}`;
}

/** The nightly catalog is what the Subscription uses (not the GA catalog). */
export function isOnNightly(status: StatusResponse | null): boolean {
  return !!(status?.subscription.source && status.subscription.source !== status.stableSource);
}

/** major.minor of a "rhoai-3.6", "rhoai-3.6-ea.1" or "rhoai-3.6.1" tag. */
export function tagLine(tag?: string): [number, number] | null {
  const m = tag ? /^rhoai-(\d+)\.(\d+)/.exec(tag) : null;
  return m ? [Number(m[1]), Number(m[2])] : null;
}

/** major.minor(.patch) of a CSV version such as "3.6.0". */
export function versionParts(version?: string): number[] | null {
  const m = version ? /^v?(\d+)\.(\d+)(?:\.(\d+))?/.exec(version) : null;
  return m ? [Number(m[1]), Number(m[2]), Number(m[3] ?? 0)] : null;
}

/**
 * Compares a nightly tag's release line with the installed operator.
 * "older" only when the tag's major.minor is lower, which the backend
 * always refuses for Update (a z-stream inside the same line can go either
 * way, so the backend decides those from the bundle it would install).
 */
export function compareTagToInstalled(tag: string | undefined, installedVersion: string | undefined): "older" | "newer" | "same-line" | "unknown" {
  const line = tagLine(tag);
  const installed = versionParts(installedVersion);
  if (!line || !installed) return "unknown";
  if (line[0] !== installed[0]) return line[0] < installed[0] ? "older" : "newer";
  if (line[1] !== installed[1]) return line[1] < installed[1] ? "older" : "newer";
  return "same-line";
}

/** Compares two versions such as "3.5.1" and "3.6.0": negative when a < b. */
export function compareVersions(a?: string, b?: string): number | null {
  const pa = versionParts(a);
  const pb = versionParts(b);
  if (!pa || !pb) return null;
  for (let i = 0; i < 3; i++) if (pa[i] !== pb[i]) return pa[i] - pb[i];
  return 0;
}
