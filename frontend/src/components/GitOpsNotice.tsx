import React from "react";
import { Alert, List, ListItem } from "@patternfly/react-core";
import type { StatusResponse } from "../types";
import { operatorGitOpsApp } from "../gitops";

/**
 * What the tool still does while Argo CD manages the operator or the
 * DataScienceCluster (pkg/cluster/gitops.go). Git is the source of truth:
 * Argo CD puts back whatever differs from Git and recreates what is deleted.
 */
export const GitOpsNotice: React.FC<{ status: StatusResponse }> = ({ status }) => {
  const operatorApp = operatorGitOpsApp(status);
  const dscApp = status.dscManagedBy;
  const apps = [...new Set([operatorApp, dscApp].filter(Boolean))].map((a) => `"${a}"`).join(" and ");
  const managed = [operatorApp && "the operator Subscription and nightly catalog", dscApp && "the DataScienceCluster"].filter(Boolean).join(", and ");
  const channel = status.subscription.managedBy && status.subscription.channel ? ` (${status.subscription.channel})` : "";
  return (
    <Alert variant="info" isInline title={`Argo CD manages ${managed}`}>
      <p>
        Argo CD application {apps} puts back anything that differs from Git, so this tool only does what Argo CD keeps.
      </p>
      <List>
        {operatorApp && (
          <ListItem>
            Git picks the nightly build: the catalog image stays as Git sets it. Update installs the build the catalog
            serves, keeps the channel Git sets{channel}, and recreates the Subscription with Argo CD&apos;s tracking. To move
            to another build, change the catalog image in Git.
          </ListItem>
        )}
        {operatorApp && <ListItem>Re-deploy and Reinstall are off: they delete objects that Argo CD would recreate from Git.</ListItem>}
        {dscApp && <ListItem>Changes to the DataScienceCluster are off: make them in Git.</ListItem>}
      </List>
    </Alert>
  );
};
