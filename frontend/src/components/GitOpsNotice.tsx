import React from "react";
import { Alert, List, ListItem, Stack } from "@patternfly/react-core";
import type { StatusResponse } from "../types";
import type { ArgoCDTracking } from "../types";
import { argoApplicationLabel, argoTrackingGroups, catalogManagedByACM, operatorGitOpsApp } from "../gitops";
import { gitOpsPolicyProblem } from "../profile";
import { useVersion } from "../state/AppInfo";

/** The application as the notice names it: with its namespace once Argo CD confirmed it. */
function appLabel(app: string, tracking: ArgoCDTracking | undefined): string {
  return tracking?.state === "managed" && tracking.application === app ? argoApplicationLabel(tracking) : `"${app}"`;
}

/**
 * What the tool still does while Argo CD manages the operator or the
 * DataScienceCluster, or an ACM policy the nightly catalog
 * (pkg/cluster/internal/argo/gitops_guard.go). Git (or the policy) is the
 * source of truth: Argo CD and ACM put back whatever differs and recreate
 * what is deleted. Shown only for an application Argo CD confirmed, or
 * could not be asked about (pkg/cluster/internal/argo/argocd.go); that case
 * says why.
 */
export const GitOpsNotice: React.FC<{ status: StatusResponse }> = ({ status }) => {
  const operatorApp = operatorGitOpsApp(status);
  const acmCatalog = catalogManagedByACM(status);
  const dscApp = status.dscManagedBy;
  const operatorTracking = status.subscription.managedBy ? status.subscription.argoCD : status.catalogSource.argoCD;
  const apps = [...new Set([operatorApp && appLabel(operatorApp, operatorTracking), dscApp && appLabel(dscApp, status.dscArgoCD)].filter(Boolean))].join(" and ");
  const unconfirmed = argoTrackingGroups(status, "unconfirmed");
  const operatorObjects = acmCatalog ? "the operator Subscription" : "the operator Subscription and nightly catalog";
  const managed = [operatorApp && operatorObjects, dscApp && "the DataScienceCluster"].filter(Boolean).join(", and ");
  const channel = status.subscription.managedBy && status.subscription.channel ? ` (${status.subscription.channel})` : "";
  const title = [managed && `Argo CD manages ${managed}`, acmCatalog && "an ACM policy manages the nightly catalog"].filter(Boolean).join(", and ");
  return (
    <Alert variant="info" isInline title={title.charAt(0).toUpperCase() + title.slice(1)}>
      {apps && (
        <p>
          Argo CD application {apps} puts back anything that differs from Git, so this tool only does what Argo CD keeps.
          {unconfirmed.map((g) => (
            <React.Fragment key={g.objects}>
              {" "}Couldn&apos;t confirm with Argo CD that application &quot;{g.tracking.application}&quot; still manages {g.objects} ({g.tracking.reason}),
              so the tool treats {g.count > 1 ? "them" : "it"} as managed.
            </React.Fragment>
          ))}
        </p>
      )}
      {acmCatalog && (
        <p>
          An ACM policy (config-policy-controller) puts CatalogSource rhoai-catalog-dev back as the policy sets it, so this tool
          leaves the catalog as it is.
        </p>
      )}
      <List>
        {operatorApp && (
          <ListItem>
            {acmCatalog ? "The ACM policy picks the nightly build: the catalog image stays as the policy sets it." : "Git picks the nightly build: the catalog image stays as Git sets it."} Update installs the build the catalog
            serves, keeps the channel Git sets{channel}, and recreates the Subscription with Argo CD&apos;s tracking. To move
            to another build, change the catalog image in {acmCatalog ? "the policy" : "Git"}.
          </ListItem>
        )}
        {!operatorApp && acmCatalog && (
          <ListItem>
            The ACM policy picks the nightly build: the catalog image stays as the policy sets it. Update installs the build the
            catalog serves. To move to another build, change the catalog image in the policy.
          </ListItem>
        )}
        {operatorApp && <ListItem>Re-deploy and Install a different build are off: they delete objects that Argo CD would recreate from Git.</ListItem>}
        {!operatorApp && acmCatalog && <ListItem>Re-deploy and Install a different build are off while the policy manages the catalog.</ListItem>}
        {dscApp && <ListItem>Changes to the DataScienceCluster are off: make them in Git.</ListItem>}
      </List>
    </Alert>
  );
};

/** The sentence for a left-over tracking annotation, by why the backend knows it is left over. */
function staleSentence(objects: string, count: number, t: ArgoCDTracking): string {
  const subject = objects.charAt(0).toUpperCase() + objects.slice(1);
  const carry = count > 1 ? "carry" : "carries";
  const them = count > 1 ? "them" : "it";
  switch (t.staleReason) {
    case "argo-cd-not-installed":
      return `${subject} ${carry} Argo CD tracking from application "${t.application}", but Argo CD is not installed on this cluster; the tool treats ${them} as not managed.`;
    case "not-in-resources":
      return `${subject} ${carry} Argo CD tracking from application ${argoApplicationLabel(t)}, which no longer lists ${them}; the tool treats ${them} as not managed.`;
    default:
      return `${subject} ${carry} Argo CD tracking left over from application "${t.application}", which no longer exists; the tool treats ${them} as not managed.`;
  }
}

/**
 * Argo CD tracking annotations that Argo CD does not back (left over after
 * an Application or Argo CD itself was removed, or copied from another
 * cluster): the tool treats the objects as not managed, and says so.
 */
export const ArgoCDLeftOverNote: React.FC<{ status: StatusResponse }> = ({ status }) => {
  const groups = argoTrackingGroups(status, "stale");
  if (groups.length === 0) return null;
  return (
    <Stack hasGutter>
      {groups.map((g) => (
        <Alert key={g.objects} variant="info" isInline isPlain title={staleSentence(g.objects, g.count, g.tracking)} />
      ))}
    </Stack>
  );
};

/**
 * What the updater does in the gitops profile (docs/GITOPS.md,
 * "Profiles"): the backend refuses everything else, so the pages and
 * actions for it are hidden; this says where they went.
 */
export const GitOpsProfileNotice: React.FC = () => {
  const policyProblem = gitOpsPolicyProblem(useVersion());
  return (
    <Stack hasGutter>
      {policyProblem && (
        <Alert variant="danger" isInline component="p" title="Update is off: the admission policy is not in place">
          {policyProblem}
        </Alert>
      )}
      <Alert variant="info" isInline title="This updater runs the gitops profile">
        <p>
          Argo CD or an ACM policy manages RHOAI on this cluster from Git, so the updater only installs the build the managed catalog serves, and shows
          Status, Components and Diagnostics to cluster-admins. Reinstall, Re-deploy, the cluster setup, Build Explorer, Dashboard Dev,
          the Environment setups, DataScienceCluster changes and Diagnostics fixes are off: make those changes in Git.
        </p>
      </Alert>
    </Stack>
  );
};
