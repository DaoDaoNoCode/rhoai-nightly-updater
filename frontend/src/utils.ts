import type { StatusResponse } from "./types";

/** A release version (vMAJOR.MINOR.PATCH); main and commit builds are not. */
export function isReleaseVersion(version: string | undefined): version is string {
  return !!version && /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version);
}

/**
 * The gitops profile does not use additional-pull-secret: it reads no Secret
 * and contacts no registry, and the cluster's own pull secrets (which Git or
 * ACM manage) are used (pkg/cluster/gitops_profile.go,
 * gitOpsProfileNoPullSecret). The backend does not read it there, so the
 * status says nothing about it.
 */
export function pullSecretNotUsed(status: StatusResponse | null, gitOpsProfile: boolean): boolean {
  return gitOpsProfile && !!status;
}

/** The pull secret is ready, or not used (the gitops profile without one). */
export function pullSecretReady(status: StatusResponse | null, gitOpsProfile = false): boolean {
  return !!(status?.pullSecret.exists && status.pullSecret.valid) || pullSecretNotUsed(status, gitOpsProfile);
}

export function prerequisitesMet(status: StatusResponse | null, gitOpsProfile = false): boolean {
  return pullSecretReady(status, gitOpsProfile) && !!status?.imageMirror.exists;
}

export function operatorInstalled(status: StatusResponse | null): boolean {
  return !!(status?.subscription.source || (status?.csv.phase && status.csv.phase !== "Not Found"));
}

export function truncateImage(image: string, maxLen = 60): string {
  if (image.length <= maxLen) return image;
  const keep = Math.floor((maxLen - 3) / 2);
  return image.slice(0, keep) + "..." + image.slice(-keep);
}

/** Elapsed time since startMs as "42s" or "3m 5s". */
export function formatElapsed(startMs: number, now = Date.now()): string {
  const elapsed = Math.max(0, Math.floor((now - startMs) / 1000));
  if (elapsed < 60) return `${elapsed}s`;
  const min = Math.floor(elapsed / 60);
  const sec = elapsed % 60;
  return `${min}m ${sec}s`;
}

export function formatRelativeTime(dateStr: string): string {
  try {
    const date = new Date(dateStr);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    if (Number.isNaN(diffMs)) return dateStr;
    if (diffMs < 0) return "just now";

    const mins = Math.floor(diffMs / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;

    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours}h ago`;

    const days = Math.floor(hours / 24);
    if (days < 7) return `${days}d ago`;
    if (days < 30) return `${Math.floor(days / 7)}w ago`;
    if (days < 365) return `${Math.floor(days / 30)}mo ago`;
    return `${Math.floor(days / 365)}y ago`;
  } catch {
    return dateStr;
  }
}
