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

const WHY = "this updater runs the gitops profile, where Argo CD or an ACM policy manages RHOAI from Git and the updater only installs the build the managed catalog serves.";

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

/** Why Update cannot run in the gitops profile when neither Argo CD nor an ACM policy manages the nightly catalog. */
export const GITOPS_PROFILE_UNMANAGED_CATALOG =
  "This updater runs the gitops profile, which only installs the build the nightly catalog (rhoai-catalog-dev) serves when Argo CD or an ACM policy manages that catalog, and neither does here. Manage the catalog with Argo CD or an ACM policy, or deploy the updater with the full profile.";

/**
 * The Status page's "Latest" row in the gitops profile, which never
 * contacts Quay or any other registry: the build comes from Git (or the ACM
 * policy), and a newer nightly arrives as a change there.
 */
export function gitOpsProfileLatestLine(acmPolicy: boolean): string {
  return acmPolicy
    ? "The ACM policy picks the build. A newer nightly arrives as a change to the policy in Git (for example a Renovate or Dependabot pull request); the gitops profile does not check Quay."
    : "Git picks the build. A newer nightly arrives as a change in Git (for example a Renovate or Dependabot pull request); the gitops profile does not check Quay.";
}

/** The help dialog's line about the updater's own releases in the gitops profile. */
export const GITOPS_PROFILE_UPDATER_RELEASES =
  "The chart pins the updater's release in Git: a newer release arrives as a change there (targetRevision and the image digest). The gitops profile does not check the registry for one.";
