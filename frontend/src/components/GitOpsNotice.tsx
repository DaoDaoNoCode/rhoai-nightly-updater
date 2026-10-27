import React from "react";
import { Alert, List, ListItem, Stack } from "@patternfly/react-core";
import type { StatusResponse } from "../types";
import { catalogManagedByACM, operatorGitOpsApp } from "../gitops";
import { gitOpsPolicyProblem } from "../profile";
import { useVersion } from "../state/AppInfo";

/**
 * What the tool still does while Argo CD manages the operator or the
 * DataScienceCluster, or an ACM policy the nightly catalog
 * (pkg/cluster/gitops.go). Git (or the policy) is the source of truth: Argo
 * CD and ACM put back whatever differs and recreate what is deleted.
 */
export const GitOpsNotice: React.FC<{ status: StatusResponse }> = ({ status }) => {
  const operatorApp = operatorGitOpsApp(status);
  const acmCatalog = catalogManagedByACM(status);
  const dscApp = status.dscManagedBy;
  const apps = [...new Set([operatorApp, dscApp].filter(Boolean))].map((a) => `"${a}"`).join(" and ");
  const operatorObjects = acmCatalog ? "the operator Subscription" : "the operator Subscription and nightly catalog";
  const managed = [operatorApp && operatorObjects, dscApp && "the DataScienceCluster"].filter(Boolean).join(", and ");
  const channel = status.subscription.managedBy && status.subscription.channel ? ` (${status.subscription.channel})` : "";
  const title = [managed && `Argo CD manages ${managed}`, acmCatalog && "an ACM policy manages the nightly catalog"].filter(Boolean).join(", and ");
  return (
    <Alert variant="info" isInline title={title.charAt(0).toUpperCase() + title.slice(1)}>
      {apps && (
        <p>
          Argo CD application {apps} puts back anything that differs from Git, so this tool only does what Argo CD keeps.
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
        {operatorApp && <ListItem>Re-deploy and Reinstall are off: they delete objects that Argo CD would recreate from Git.</ListItem>}
        {!operatorApp && acmCatalog && <ListItem>Re-deploy and Reinstall are off while the policy manages the catalog.</ListItem>}
        {dscApp && <ListItem>Changes to the DataScienceCluster are off: make them in Git.</ListItem>}
      </List>
    </Alert>
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
          Test resources, DataScienceCluster changes and Diagnostics fixes are off: make those changes in Git.
        </p>
      </Alert>
    </Stack>
  );
};
