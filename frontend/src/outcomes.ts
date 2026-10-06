import { toApiError } from "./services/api";
import { describeError } from "./errors";
import type { OperationResponse } from "./types";

export type OutcomeVariant = "success" | "info" | "warning" | "danger";

/**
 * Alert variant for a mutation result, by errorCode (contracts of B2 and B3).
 * "Nothing to do" and "still in progress" are not failures: nothing broke and
 * the cluster is either already right or still converging.
 */
export function outcomeVariant(res: Pick<OperationResponse, "success" | "errorCode">): OutcomeVariant {
  if (res.success) return "success";
  switch (res.errorCode) {
    case "nothing_to_do":
    case "in_progress":
      return "info";
    case "not_managed":
    case "conflict":
    case "prerequisites":
    case "partial_failure":
    case "cluster_busy":
    case "lock_unavailable":
    case "terminating":
      return "warning";
    default:
      return "danger";
  }
}

/** A short title for a non-success result; the backend message is the body. */
export function outcomeTitle(res: Pick<OperationResponse, "success" | "errorCode" | "message">, failedTitle = "The action failed"): string {
  if (res.success) return res.message;
  switch (res.errorCode) {
    case "nothing_to_do": return "Nothing to do";
    case "in_progress": return "Still in progress";
    case "not_managed": return "Not managed by this tool";
    case "conflict": return "The object changed in the meantime";
    case "prerequisites": return "Blocked: prerequisites not met";
    case "partial_failure": return "Partly done";
    case "cluster_busy": return "Cluster busy";
    case "lock_lost": return "Stopped: another updater pod took over";
    case "lock_unavailable": return "Not started: try again in a few seconds";
    case "terminating": return "Still being deleted";
    case "delete_failed": return "Nothing could be deleted";
    default: return failedTitle;
  }
}

/** Turn a thrown error into an OperationResponse-shaped result, keeping its errorCode. */
export function errorResult(e: unknown, fallback: string): OperationResponse {
  const err = toApiError(e, fallback);
  // 422 operation bodies are OperationResponse JSON; keep their logs.
  const details = err.details as Partial<OperationResponse> | undefined;
  return {
    success: false,
    message: err.message,
    errorCode: err.errorCode,
    logs: Array.isArray(details?.logs) ? details.logs : [],
  };
}

export interface LoadErrorDescription {
  title: string;
  body: string;
  variant: "danger" | "warning";
}

/**
 * Title and text for a failed page load, by HTTP status and errorCode (B4
 * and B5 read handlers). Never says "not installed": that is a state the
 * backend reports explicitly (dscState, dashboard_not_deployed), not an error.
 */
export function describeLoadError(e: unknown, genericTitle: string): LoadErrorDescription {
  const err = toApiError(e);
  switch (err.errorCode) {
    case "forbidden":
      return { title: "Access denied", body: `${err.message} Ask a cluster admin for read access.`, variant: "danger" };
    case "unauthorized":
      // A 401 is the user's own session; a 502 means the cluster rejected the updater's credentials.
      if (err.status !== 401) {
        return { title: "The cluster rejected the updater's credentials", body: err.message, variant: "danger" };
      }
      break;
    case "rate_limited":
      return { title: "The cluster API is throttling requests", body: `${err.message} Try again in a minute.`, variant: "warning" };
    case "cluster_unavailable":
    case "cluster_error":
    case "upstream_error":
      return { title: "The cluster API returned an error", body: err.message, variant: "danger" };
    case "network":
      if (err.status > 0) {
        return { title: "The updater cannot reach the cluster API", body: err.message, variant: "danger" };
      }
      break;
    case "timeout":
      return { title: "The request timed out", body: `${err.message} The cluster may be slow; try again.`, variant: "warning" };
    case "registry_auth":
      return { title: "Quay rejected the pull secret", body: err.message, variant: "danger" };
    case "registry_unavailable":
      return { title: "Quay is not reachable", body: err.message, variant: "warning" };
    case "not_found":
      return { title: genericTitle, body: err.message, variant: "danger" };
  }
  const d = describeError(err, genericTitle);
  return { title: d.title, body: d.body, variant: d.variant };
}
