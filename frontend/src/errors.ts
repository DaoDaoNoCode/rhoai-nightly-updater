import { toApiError, type ApiError } from "./services/api";
import { sentence } from "./build";

export interface ErrorDescription {
  title: string;
  body: string;
  variant: "danger" | "warning";
  /** The error clears by signing in again (session expiry). */
  reload?: boolean;
  /** What the user can do next, when the title and body don't say it. */
  hint?: string;
}

/**
 * User-facing title and text for an API error, chosen by HTTP status and
 * errorCode only. The backend message is kept as the body because it is
 * written for users (pkg/api writeError).
 */
const SESSION_EXPIRED: ErrorDescription = {
  title: "Your session expired",
  body: "Your OpenShift session has expired.",
  hint: "Sign in again to continue; nothing on the cluster was changed.",
  variant: "danger",
  reload: true,
};

/** The backend's 409 body names the running operation ("alice is running ..."). */
function clusterBusyBody(err: ApiError): string {
  const details = err.details as { operation?: unknown } | undefined;
  if (details?.operation) return err.message;
  return "Another operation is already changing this cluster.";
}

export function describeError(e: unknown, genericTitle = "Error"): ErrorDescription {
  const d = describeErrorRaw(e, genericTitle);
  // Backend messages often have no final period; the hint follows as a new sentence.
  return d.hint ? { ...d, body: sentence(d.body) } : d;
}

function describeErrorRaw(e: unknown, genericTitle: string): ErrorDescription {
  const err: ApiError = toApiError(e);
  switch (err.errorCode) {
    case "session_expired":
      return SESSION_EXPIRED;
    case "unauthorized":
      // pkg/api: no token at all; behind oauth-proxy that means signed out.
      return err.status === 401 ? SESSION_EXPIRED : { title: "The cluster rejected the updater's credentials", body: err.message, variant: "danger", hint: "Check the updater's ServiceAccount (oc get sa -n <namespace>) and redeploy it." };
    case "forbidden":
      return { title: "Access denied", body: err.message, variant: "danger" };
    case "cluster_busy":
      return { title: "Cluster busy", body: clusterBusyBody(err), hint: "Nothing was changed. Wait for it to finish, then try again.", variant: "warning" };
    case "rate_limited":
      return { title: "The OpenShift API is throttling requests", body: err.message, hint: "Wait a minute, then retry.", variant: "warning" };
    case "shutting_down":
      return { title: "The updater is restarting", body: err.message, hint: "Retry in a minute.", variant: "warning" };
    case "authorization_unavailable":
      return { title: "Could not check your identity or permissions", body: err.message, hint: "The OpenShift API did not answer. Retry shortly.", variant: "warning" };
    case "network":
      return { title: err.status === 0 ? "Cannot reach the updater" : "Cannot reach the OpenShift API", body: err.message, hint: err.status === 0 ? "Check your connection or VPN. The updater pod may be restarting." : "The cluster API did not answer. Retry in a minute.", variant: "danger" };
    case "timeout":
      return { title: err.status === 0 ? "Request timed out" : "The OpenShift API timed out", body: err.message, hint: "Retry in a moment.", variant: "warning" };
    case "cluster_unavailable":
    case "cluster_error":
      return { title: "The OpenShift API returned an error", body: err.message, hint: "Retry in a minute. If it persists, check the cluster in the OpenShift console.", variant: "danger" };
    case "catalog_image_pull":
      return { title: genericTitle, body: err.message, hint: "The cluster could not pull the catalog image: check the pull secret under Cluster setup.", variant: "danger" };
    case "registry_auth":
      return { title: "Quay rejected the pull secret", body: err.message, hint: "Replace the pull secret under Cluster setup.", variant: "danger" };
    case "registry_unavailable":
      return { title: "Cannot reach Quay", body: err.message, hint: "Retry in a minute.", variant: "warning" };
    case "internal":
      if (err.status >= 500) return { title: genericTitle, body: err.message, hint: "Retry. If it persists, check the updater logs: oc logs deploy/rhoai-nightly-updater -c app", variant: "danger" };
  }
  if (err.status === 401) return SESSION_EXPIRED;
  if (err.status === 403) return { title: "Access denied", body: err.message, variant: "danger" };
  if (err.status === 409) return { title: "Cluster busy", body: clusterBusyBody(err), hint: "Nothing was changed. Wait for it to finish, then try again.", variant: "warning" };
  return { title: genericTitle, body: err.message, variant: "danger" };
}

/** describeError for a failed run's outcome (the store keeps code and status, not the ApiError). */
export function describeOutcomeError(outcome: { message: string; errorCode?: string; httpStatus?: number; busyOperation?: unknown }, genericTitle: string): ErrorDescription {
  return describeError({
    name: "ApiError",
    status: outcome.httpStatus ?? 0,
    errorCode: outcome.errorCode ?? "operation_failed",
    message: outcome.message,
    details: outcome.busyOperation ? { operation: outcome.busyOperation } : undefined,
  }, genericTitle);
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
