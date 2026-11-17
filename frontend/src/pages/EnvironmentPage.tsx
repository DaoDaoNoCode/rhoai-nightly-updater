import React, { useEffect } from "react";
import { PageHeader } from "../components/PageHeader";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { useClusterBusyHandler, useMutationBlocker, usePermissions } from "../state/AppInfo";
import { useClusterStatus } from "../state/AppState";

/**
 * Environment: one-click setups for developing and reproducing issues (S3
 * storage, pipeline servers, MLflow), at /test-resources, its path from when
 * it was called Test resources (A08-15), so links keep working. The old
 * /dashboard-dev?tab=resources link redirects here.
 */
export const EnvironmentPage: React.FC = () => {
  const blocker = useMutationBlocker();
  // Reading a credential (the storage's admin password) needs the same
  // cluster-admin check as a change, but no free operation slot.
  const { reason: readBlocker } = usePermissions();
  const onResult = useClusterBusyHandler();
  const { status } = useClusterStatus();
  useEffect(() => {
    document.title = "Environment — RHOAI Nightly Updater";
  }, []);
  return (
    <QuickResourceCreator
      mutateBlocker={blocker}
      readBlocker={readBlocker}
      consoleURL={status?.consoleURL}
      onResult={onResult}
      renderHeader={(header) => (
        <PageHeader
          title="Environment"
          description="One-click setups for developing and reproducing issues on this cluster."
          {...header}
        />
      )}
    />
  );
};
