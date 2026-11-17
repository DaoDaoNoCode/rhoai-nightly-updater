import React, { useEffect } from "react";
import { PageHeader } from "../components/PageHeader";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";

/**
 * Environment: one-click setups for developing and reproducing issues (S3
 * storage, pipeline servers, MLflow), at /test-resources, its path from when
 * it was called Test resources (A08-15), so links keep working. The old
 * /dashboard-dev?tab=resources link redirects here.
 */
export const EnvironmentPage: React.FC = () => {
  const blocker = useMutationBlocker();
  const onResult = useClusterBusyHandler();
  useEffect(() => {
    document.title = "Environment — RHOAI Nightly Updater";
  }, []);
  return (
    <>
      <QuickResourceCreator
        mutateBlocker={blocker}
        onResult={onResult}
        renderHeader={(header) => (
          <PageHeader
            title="Environment"
            description="One-click setups for developing and reproducing issues on this cluster: storage, pipelines, MLflow, models, a Gen AI playground and Models as a Service."
            {...header}
          />
        )}
      />
    </>
  );
};
