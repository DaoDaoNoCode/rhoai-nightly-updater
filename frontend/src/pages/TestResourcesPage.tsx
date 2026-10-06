import React, { useEffect } from "react";
import { PageHeader } from "../components/PageHeader";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";

/**
 * Test resources (S3 storage, MLflow, pipeline servers), at /test-resources
 * (A08-15). The old /dashboard-dev?tab=resources link redirects here.
 */
export const TestResourcesPage: React.FC = () => {
  const blocker = useMutationBlocker();
  const onResult = useClusterBusyHandler();
  useEffect(() => {
    document.title = "Test resources — RHOAI Nightly Updater";
  }, []);
  return (
    <>
      <QuickResourceCreator
        mutateBlocker={blocker}
        onResult={onResult}
        renderHeader={(header) => (
          <PageHeader
            title="Test resources"
            description="Storage, MLflow and pipeline servers for testing the dashboard, with defaults that work on a fresh cluster."
            {...header}
          />
        )}
      />
    </>
  );
};
