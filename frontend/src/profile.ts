import type { VersionInfo } from "./types";

// The updater's profile (PROFILE, GET /api/version; docs/GITOPS.md,
// "Profiles"). In the gitops profile the backend answers 403 profile_gitops
// for everything but Update and the Status, Components and Diagnostics
// reads, and only cluster-admins may read: the UI hides what it refuses.

/** True when the backend runs the gitops profile. */
export function isGitOpsProfile(version: VersionInfo | null | undefined): boolean {
  return version?.profile === "gitops";
}

/** The pages that exist in the gitops profile. */
export const GITOPS_PROFILE_PAGES: ReadonlySet<string> = new Set(["/", "/components", "/diagnostics"]);

/** The backend's error code for a route the gitops profile refuses. */
export const PROFILE_GITOPS_ERROR = "profile_gitops";

const WHY = "this updater runs the gitops profile, where Argo CD manages RHOAI and the updater only installs the build the Git-managed catalog serves.";

/** Why a feature is missing in the gitops profile. */
export function gitOpsProfileReason(feature: string): string {
  return `${feature} is off: ${WHY}`;
}

/**
 * Why Update is off in the gitops profile because its admission policy is
 * not in place (the backend refuses it with 503 admission_policy_missing),
 * or null.
 */
export function gitOpsPolicyProblem(version: VersionInfo | null | undefined): string | null {
  if (!isGitOpsProfile(version)) return null;
  const state = version?.admissionPolicy;
  return state && state !== "enforced" ? state : null;
}

/** Why Update cannot run in the gitops profile when Argo CD does not manage the nightly catalog. */
export const GITOPS_PROFILE_UNMANAGED_CATALOG =
  "This updater runs the gitops profile, which only installs the build an Argo CD managed catalog serves, and Argo CD does not manage the nightly catalog (rhoai-catalog-dev) here. Manage the catalog in Git, or deploy the updater with the full profile.";
