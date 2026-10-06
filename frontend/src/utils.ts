import type { StatusResponse } from "./types";

export function prerequisitesMet(status: StatusResponse | null): boolean {
  return !!(status?.pullSecret.exists && status?.pullSecret.valid && status?.imageMirror.exists);
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
