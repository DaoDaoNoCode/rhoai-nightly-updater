import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Content,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  Form,
  FormGroup,
  PageSection,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import CubesIcon from "@patternfly/react-icons/dist/esm/icons/cubes-icon";
import { useSearchParams } from "react-router-dom";
import { getDSProjects, toApiError, type ApiError } from "../services/api";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { ProjectSelect } from "../components/environment/ProjectSelect";
import { UserProjectView } from "../components/environment/UserProjectView";

/**
 * Environment for a user who is not a cluster-admin: their own data
 * science projects (the projects API with their token) and, per project,
 * what runs there, read-only. The shared parts (S3 storage, the inference
 * gateway, MaaS, prerequisites) and every change are a cluster-admin's.
 */
export const UserEnvironmentPage: React.FC = () => {
  const [searchParams, setSearchParams] = useSearchParams();
  const [projects, setProjects] = useState<string[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);

  useEffect(() => {
    document.title = "Environment — RHOAI Nightly Updater";
  }, []);

  useEffect(() => {
    let live = true;
    setLoading(true);
    getDSProjects().then(
      (res) => {
        if (!live) return;
        setProjects([...(res.projects ?? [])].sort());
        setError(null);
        setLastRefreshed(new Date());
      },
      (e: unknown) => { if (live) setError(toApiError(e, "Could not load your projects")); },
    ).finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, [refreshKey]);
  const refresh = useCallback(() => setRefreshKey((n) => n + 1), []);

  const urlProject = searchParams.get("project");
  const missing = !!projects && !!urlProject && !projects.includes(urlProject) ? urlProject : null;
  const project = projects?.length ? (urlProject && projects.includes(urlProject) ? urlProject : projects[0]) : null;
  const choose = (p: string) => setSearchParams((params) => {
    const next = new URLSearchParams(params);
    next.set("project", p);
    return next;
  });

  let content: React.ReactNode;
  if (!projects && error) {
    content = <PageErrorState error={error} title="Can't load your projects" onRetry={refresh} diagnostics={false} />;
  } else if (!projects) {
    content = <PageLoading title="Loading your projects" />;
  } else if (!project) {
    content = (
      <PageSection isFilled>
        <EmptyState headingLevel="h2" titleText="You have no data science projects" icon={CubesIcon} variant={EmptyStateVariant.lg}>
          <EmptyStateBody>Create one in the RHOAI dashboard, or ask a project admin to add you to theirs. Your projects show here.</EmptyStateBody>
        </EmptyState>
      </PageSection>
    );
  } else {
    content = (
      <PageSection isFilled>
        <Stack hasGutter>
          <StackItem>
            <Alert variant="info" isInline isPlain component="div" title="A cluster-admin sets up the shared parts">
              <Content component="p">
                S3 storage, the inference gateway, Models as a Service and the operators these features need. Setting up or
                removing anything on this page is also done by a cluster-admin.
              </Content>
            </Alert>
          </StackItem>
          {missing && (
            <StackItem>
              <Alert variant="warning" isInline component="p" title={`${missing} is not one of your projects`}>
                It does not exist, or you have no access to it. Showing {project}.
              </Alert>
            </StackItem>
          )}
          <StackItem>
            <Form onSubmit={(e) => e.preventDefault()}>
              <FormGroup label="Project" fieldId="user-env-project">
                <ProjectSelect id="user-env-project" projects={projects.map((name) => ({ name }))} selected={project} onSelect={choose} />
              </FormGroup>
            </Form>
          </StackItem>
          <StackItem>
            <UserProjectView project={project} refreshKey={refreshKey} />
          </StackItem>
        </Stack>
      </PageSection>
    );
  }

  return (
    <>
      <PageHeader
        title="Environment"
        description="What runs in your data science projects. Only cluster-admins set things up or remove them."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={refresh}
      />
      {projects && error && <LoadErrorAlert error={error} genericTitle="Could not refresh your projects" onRetry={refresh} stale />}
      {content}
    </>
  );
};
