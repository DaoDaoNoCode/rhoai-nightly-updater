import type { StatusResponse } from "./types";

// Argo CD (OpenShift GitOps) can manage the operator's CatalogSource and
// Subscription, and the DataScienceCluster. Git is then the source of truth:
// Argo CD undoes changes to them and recreates them when they are deleted, so
// the tool limits what it offers (pkg/cluster/gitops.go). The backend refuses
// the same actions with error code gitops_managed.

/** The Argo CD application managing the operator's Subscription or nightly catalog. */
export function operatorGitOpsApp(status: StatusResponse | null | undefined): string | undefined {
  return status?.subscription.managedBy || status?.catalogSource.managedBy || undefined;
}

export type GitOpsInstallState = "pending" | "installed" | "updating" | "installing" | "unknown";

/**
 * Under Argo CD, Git picks the nightly build: the catalog image stays as Git
 * sets it, and Update installs the build the catalog serves. Whether the
 * installed operator is that build:
 * - "pending": the catalog serves a build the operator does not run yet
 *   (Git moved on, or a same-version build OLM does not install by itself);
 * - "installed": the operator runs it;
 * - "updating": OLM is still replacing the catalog pod after Git changed
 *   the image, so what the catalog serves is not known yet;
 * - "installing": nothing is installed and OLM is working on the
 *   Subscription's InstallPlan (the first install Argo CD started);
 * - "unknown": the catalog did not say.
 */
export function gitOpsInstallState(status: StatusResponse | null | undefined): GitOpsInstallState | null {
  if (!operatorGitOpsApp(status)) return null;
  if (status?.catalogServes?.updating) return "updating";
  if (status?.catalogSource.image && (!status.csv.name || status.csv.phase === "Not Found")) {
    const phase = status.installPlan?.phase;
    return phase && phase !== "Complete" && phase !== "Failed" ? "installing" : "pending";
  }
  const installed = status?.catalogServes?.installed;
  return installed === undefined ? "unknown" : installed ? "installed" : "pending";
}

/** Why the GitOps install button is off for this state, or null. */
export function gitOpsInstallWaitReason(state: GitOpsInstallState | null): string | null {
  switch (state) {
    case "updating":
      return "OLM is replacing the catalog pod after the image changed in Git. Wait a minute, then refresh.";
    case "installing":
      return "OLM is installing the build Git set. Wait for it to finish; Diagnostics shows why if it is stuck.";
    default:
      return null;
  }
}

/** Why Re-deploy or Reinstall is off while Argo CD manages the operator, or null. */
export function gitOpsOperatorReason(status: StatusResponse | null | undefined, action: "Re-deploy" | "Reinstall"): string | null {
  const app = operatorGitOpsApp(status);
  if (!app) return null;
  return `Argo CD application "${app}" manages the operator. ${action} deletes the Subscription for minutes, and Argo CD would recreate it from Git meanwhile. Use Update, which installs the build the catalog serves.`;
}

/** Why a DataScienceCluster change is off while Argo CD manages the DSC, or null. */
export function gitOpsDSCReason(status: StatusResponse | null | undefined): string | null {
  const app = status?.dscManagedBy;
  if (!app) return null;
  return `Argo CD application "${app}" manages the DataScienceCluster and would undo this change. Change the DataScienceCluster in Git instead.`;
}

/** Why creating a DataScienceCluster by hand is off while Argo CD manages the operator, or null. */
export function gitOpsCreateDSCReason(status: StatusResponse | null | undefined): string | null {
  const app = operatorGitOpsApp(status);
  if (!app) return null;
  return `Argo CD application "${app}" manages RHOAI here and creates the DataScienceCluster from Git on its next sync. Define it there; one made by hand would be overwritten.`;
}

/** The reason a Diagnostics fix is off under Argo CD: only fixes that edit the DSC are. */
export function gitOpsFixReason(status: StatusResponse | null | undefined, action: string): string | null {
  return action.startsWith("disable-component:") ? gitOpsDSCReason(status) : null;
}
