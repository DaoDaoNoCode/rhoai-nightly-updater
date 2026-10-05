import React, { useEffect } from "react";
import { PageSection, Title, Content } from "@patternfly/react-core";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { usePermissions } from "../hooks/usePermissions";

/**
 * Test resources (MinIO, MLflow, pipeline servers) as their own page
 * (A08-15). Not routed yet: the route and nav item are in App.tsx and
 * constants.ts (see the F2 handoff). Until then the same content is the
 * "Test resources" tab of Dashboard Dev (/dashboard-dev?tab=resources).
 */
export const TestResourcesPage: React.FC = () => {
  const { canMutate } = usePermissions();
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
        <QuickResourceCreator canMutate={canMutate} />
      </PageSection>
    </>
  );
};
