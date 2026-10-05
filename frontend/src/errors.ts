import { toApiError, type ApiError } from "./services/api";

export interface ErrorDescription {
  title: string;
  body: string;
  variant: "danger" | "warning";
  /** The error clears on reload (session expiry). */
  reload?: boolean;
}

/**
 * User-facing title and text for an API error, chosen by HTTP status and
 * errorCode only. The backend message is kept as the body because it is
 * written for users (pkg/api writeError).
 */
const SESSION_EXPIRED: ErrorDescription = {
  title: "Session expired",
  body: "Your session has expired. Reload the page to sign in again.",
  variant: "danger",
  reload: true,
};

export function describeError(e: unknown, genericTitle = "Error"): ErrorDescription {
  const err: ApiError = toApiError(e);
  switch (err.errorCode) {
    case "unauthorized":
    case "session_expired":
      return SESSION_EXPIRED;
    case "forbidden":
      return { title: "Access denied", body: err.message, variant: "danger" };
    case "cluster_busy":
      return { title: "Cluster busy", body: "Another operation is already changing this cluster. Wait for it to finish, then try again.", variant: "warning" };
    case "rate_limited":
      return { title: "Too many requests", body: err.message, variant: "warning" };
    case "shutting_down":
      return { title: "The updater is restarting", body: err.message, variant: "warning" };
    case "authorization_unavailable":
      return { title: "Permissions could not be checked", body: err.message, variant: "warning" };
    case "network":
      return { title: "Cannot reach the server", body: err.message, variant: "danger" };
    case "timeout":
      return { title: "Request timed out", body: err.message, variant: "warning" };
  }
  if (err.status === 401) return SESSION_EXPIRED;
  if (err.status === 403) return { title: "Access denied", body: err.message, variant: "danger" };
  if (err.status === 409) return { title: "Cluster busy", body: err.message, variant: "warning" };
  return { title: genericTitle, body: err.message, variant: "danger" };
}

/** True when the error means the user may not mutate (or is signed out). */
export function isPermissionError(e: unknown): boolean {
  const err = toApiError(e);
  return err.status === 401 || err.status === 403 || err.errorCode === "unauthorized" || err.errorCode === "forbidden";
}

/**
 * Friendly text for a Quay lookup failure reported by the backend. The
 * backend has no errorCode for Quay failures yet, so this reads its own
 * fixed formats: "returned HTTP <code>" (pkg/cluster/quay.go) and Go network
 * error words. Bare digits are never matched, because image digests contain them.
 */
export function describeQuayError(e: unknown): string {
  const err = toApiError(e);
  if (err.errorCode === "network") return "Cannot reach the updater server. Check your connection and try again.";
  if (err.errorCode === "timeout") return "The request timed out. Quay may be slow; try again in a moment.";
  if (err.errorCode === "unauthorized" || err.errorCode === "session_expired") return "Your session has expired. Reload the page to sign in again.";
  return describeQuayText(err.message);
}

export function describeQuayText(raw: string): string {
  const lower = raw.toLowerCase();
  const httpStatus = /returned http (\d{3})\b/.exec(lower)?.[1];
  if (httpStatus === "401") return "Quay authentication failed. The pull secret may be expired or invalid. Update it in the cluster setup.";
  if (httpStatus === "403") return "Access denied by Quay. Check that the pull secret can read the rhoai-fbc-fragment repository.";
  if (httpStatus === "429") return "Quay rate limit reached. Wait a few minutes and try again.";
  if (lower.includes("no such host") || lower.includes("dial tcp")) return "Cannot reach the Quay registry. Check that the cluster has outbound network access to quay.io.";
  if (lower.includes("deadline exceeded") || lower.includes("timeout")) return "The request to Quay timed out. The registry may be slow or unreachable. Try again in a moment.";
  if (lower.includes("x509") || lower.includes("certificate")) return "TLS certificate error connecting to Quay. A proxy or firewall may be intercepting the connection.";
  if (lower.includes("connection reset") || lower.includes("connection refused") || /\beof\b/.test(lower)) return "Connection to Quay was interrupted. The registry may be temporarily unavailable. Try again.";
  return raw;
}
