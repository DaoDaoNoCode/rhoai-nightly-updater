import React, { useEffect } from "react";
import { PageSection, Title, Content } from "@patternfly/react-core";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";

/**
 * Test resources (MinIO, MLflow, pipeline servers), at /test-resources
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
      <PageSection>
        <Title headingLevel="h1" size="xl">Test resources</Title>
        <Content component="p">Storage, MLflow and pipeline servers to test the dashboard against.</Content>
      </PageSection>
      <PageSection>
        <QuickResourceCreator mutateBlocker={blocker} onResult={onResult} />
      </PageSection>
    </>
  );
};
