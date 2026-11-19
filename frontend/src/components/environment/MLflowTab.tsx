import React, { useState } from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import type { ResourceState, ResourcesStatus } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type Pending, ExternalLink, ProblemAlerts, StartingNote, consolePodsHref, isTerminating, pvcsLost, terminalReason } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { notManaged } from "./resourceStatus";
import { MLFLOW_WORKSPACES, type Busy, type FeatureState, mlflowStatus } from "./featureState";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { ComponentAlert, CopyableCode, NoProjectsAlert, mlflowClientEnv, mlflowOpenHref } from "./UseIt";
import { MLflowSamples } from "./MLflowSamples";
import type { SampleOp } from "./samples";

export interface MLflowTabProps {
  /** What MLflow needs on the cluster (one-click prerequisites), above the parts. */
  prerequisites?: React.ReactNode;
  /** Why the feature cannot be set up until its cluster prerequisites are, or null. */
  prerequisitesReason?: string | null;
  status: ResourcesStatus;
  /** mlflowState: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  /** The project (MLflow workspace) the "Use it" section is about, or null. */
  project: string | null;
  projects: ProjectOption[];
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  onClearProject: () => void;
  consoleURL?: string;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
  /** The sample operation running now, if any. */
  sampleOp: SampleOp | null;
  /** Changes when a sample operation ends, so the sample data is read again. */
  samplesRefresh: number;
}

/**
 * "Test an MLflow PR build": deploys an opendatahub-io/mlflow PR image on
 * the server. A developer's tool, so it sits in a collapsed section; the
 * number is checked on submit, with the error under the field.
 */
const DeployPR: React.FC<{ busy: Busy; baseReason: string | null; prOverride: boolean; setPending: (p: Pending) => void }> = ({ busy, baseReason, prOverride, setPending }) => {
  const [pr, setPr] = useState("");
  const [error, setError] = useState<string | null>(null);
  const submit = () => {
    if (!/^\d+$/.test(pr) || Number(pr) <= 0) {
      setError("Enter the number of an opendatahub-io/mlflow pull request, for example 123.");
      return;
    }
    setError(null);
    setPending({ kind: "deploy-mlflow-pr", pr: Number(pr) });
  };
  return (
    <FeatureSection title="Test an MLflow PR build">
      <Form isWidthLimited onSubmit={(e) => { e.preventDefault(); submit(); }}>
        <Content component="p">
          Runs this MLflow server on the image built for an opendatahub-io/mlflow pull request.{prOverride ? " Revert, on the MLflow server, goes back to the image used before." : " Revert goes back to the image used now."}
        </Content>
        <FormGroup label="Pull request number" fieldId="mlflow-pr">
          <TextInput id="mlflow-pr" type="text" inputMode="numeric" value={pr} validated={error ? "error" : "default"}
            onChange={(_e, v) => { setPr(v.trim()); setError(null); }} aria-describedby="mlflow-pr-helper" />
          <FormHelperText>
            <HelperText id="mlflow-pr-helper">
              <HelperTextItem variant={error ? "error" : "default"}>{error ?? "The image quay.io/opendatahub/mlflow:odh-pr-<number>, pinned to its current digest."}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FormGroup>
        <ActionList>
          <ActionListGroup>
            <ActionListItem>
              <TooltipButton type="submit" variant="secondary" isLoading={busy.resAction === "deploy-mlflow-pr"} disabledReason={baseReason}>Deploy PR</TooltipButton>
            </ActionListItem>
          </ActionListGroup>
        </ActionList>
      </Form>
    </FeatureSection>
  );
};

/** How to use MLflow: workspaces are projects; open a project's, or point a client at it. */
const UseMLflow: React.FC<{ url: string } & Pick<MLflowTabProps, "project" | "projects" | "projectsLoaded" | "onProject" | "onClearProject" | "status">> = ({ url, project, projects, projectsLoaded, onProject, onClearProject, status }) => (
  <TabSection title="Use it" id="mlflow-use">
    <Content component="p">
      On RHOAI, MLflow keeps each data science project apart: a project is an MLflow workspace, with its own experiments, runs and models, and who can see them follows who can see the project.
      Choose the project to work in.
    </Content>
    {projectsLoaded && projects.length === 0 ? <NoProjectsAlert dashboardUrl={status.dashboardUrl} /> : (
      <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
        <FormGroup label="Project (workspace)" fieldId="mlflow-project">
          <ProjectSelect id="mlflow-project" projects={projects} selected={project} onSelect={onProject} onClear={onClearProject} />
        </FormGroup>
      </Form>
    )}
    {project && (
      <DescriptionList aria-label={`How to use MLflow in ${project}`}>
        <DescriptionListGroup>
          <DescriptionListTerm>In the browser</DescriptionListTerm>
          <DescriptionListDescription>
            <ActionList>
              <ActionListGroup>
                <ActionListItem><ExternalLink href={mlflowOpenHref(status.dashboardUrl, url, project)} variant="secondary">Open {project} in MLflow</ExternalLink></ActionListItem>
              </ActionListGroup>
            </ActionList>
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>From a notebook or script</DescriptionListTerm>
          <DescriptionListDescription>
            <Stack hasGutter>
              <StackItem><Content component="p">Set these before you use the MLflow client (Python <code>mlflow</code> 3.x). The token is your OpenShift token; in a workbench, use a token that can access {project}.</Content></StackItem>
              <StackItem><CopyableCode id="mlflow-env" label="the environment variables" code={mlflowClientEnv(url, project)} /></StackItem>
            </Stack>
          </DescriptionListDescription>
        </DescriptionListGroup>
      </DescriptionList>
    )}
    {project && (
      <WhatNext id="mlflow-next" items={[
        <>Add sample data below to see experiments, traces and a registered model in {project} without writing code.</>,
        <>Log a run from a notebook with the settings above: <code>mlflow.start_run()</code>, then <code>mlflow.log_metric()</code>.</>,
        <>Open {project} in MLflow to compare runs and register a model.</>,
      ]} />
    )}
  </TabSection>
);

/**
 * The MLflow tab: what it needs, the MLflow server (CR mlflow) with its
 * health and actions, Set up when there is none, how to use it per
 * project, testing a PR build, what it creates, and Clean up.
 */
export const MLflowTab: React.FC<MLflowTabProps> = (props) => {
  const { status, state, busy, baseReason, readOnly, consoleURL, setPending, onSetUp, onCleanup } = props;
  const mlflow = status.mlflow;
  const deployed = !!mlflow?.deployed;
  const unmanaged = notManaged(mlflow);
  const managed = deployed && mlflow?.managedByTool !== false;
  const next = state.next;
  const actions: React.ReactNode[] = [];
  const revert = () => setPending({ kind: "revert-mlflow" });
  if (mlflow?.prOverride && !readOnly) {
    actions.push(<ActionListItem key="revert"><TooltipButton variant="secondary" onClick={revert} isLoading={busy.resAction === "revert-mlflow"} disabledReason={baseReason}>Revert the PR image</TooltipButton></ActionListItem>);
  }
  if (mlflow?.ready && mlflow.uiRoute) actions.push(<ActionListItem key="open"><ExternalLink href={mlflowOpenHref(status?.dashboardUrl, mlflow.uiRoute, null)}>{MLFLOW_WORKSPACES}</ExternalLink></ActionListItem>);
  const menu: PartMenuItem[] = managed && !readOnly && !isTerminating(mlflow)
    ? [{ key: "delete", label: "Delete MLflow…", onClick: () => setPending({ kind: "teardown-mlflow" }), disabledReason: baseReason ?? mlflow?.teardownBlockedReason ?? null, isDanger: true }]
    : [];
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="MLflow" purpose={FEATURE_PURPOSE.mlflow} state={state} /></StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {status.mlflowBlocked && <StackItem><ComponentAlert feature="MLflow" block={status.mlflowBlocked} /></StackItem>}
      <StackItem>
        <PartList aria-label="What MLflow needs">
          <PartItem id="mlflow" title="MLflow server" scope="Shared by the cluster" status={mlflowStatus(status, busy)} unmanaged={unmanaged} actions={actions.length ? actions : undefined} menu={menu}>
            <Content component="p">
              One MLflow server for the whole cluster. Sign in with your OpenShift login.
              {unmanaged ? " It was created outside this tool (for example in the dashboard): this tool can test a PR build on it, but does not delete it." : deployed ? " Its data is kept on a 10 GiB volume until you delete it." : ""}
            </Content>
            {mlflow && (
              <ProblemAlerts kind="mlflow" state={mlflow} fix={mlflow.prOverride && !readOnly ? { label: "Revert the PR image", onClick: revert, reason: baseReason } : null}
                logsHref={consolePodsHref(consoleURL, mlflow.namespace, "mlflow")} />
            )}
            {deployed && !mlflow?.ready && !terminalReason(mlflow) && !isTerminating(mlflow) && <StartingNote state={mlflow!} typical="1 to 3 minutes" />}
            {deployed && (
              <DescriptionList isCompact isHorizontal aria-label="MLflow details">
                {mlflow?.uiRoute && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Address</DescriptionListTerm>
                    <DescriptionListDescription><ImageRef image={mlflow.uiRoute} what="MLflow address" truncate={false} isCode={false} /></DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                {mlflow?.currentImage && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Image</DescriptionListTerm>
                    <DescriptionListDescription><ImageRef image={mlflow.currentImage} />{mlflow.prOverride ? ` (opendatahub-io/mlflow PR #${mlflow.prNumber ?? "?"})` : ""}</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
              </DescriptionList>
            )}
          </PartItem>
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} variant={props.prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? props.prerequisitesReason ?? null} />
        </StackItem>
      )}
      {mlflow?.ready && mlflow.uiRoute && !isTerminating(mlflow) && <StackItem><UseMLflow url={mlflow.uiRoute} {...props} /></StackItem>}
      <StackItem>
        <MLflowSamples state={state} ready={!!mlflow?.ready && !!mlflow.uiRoute && !isTerminating(mlflow)} project={props.project} readOnly={readOnly}
          baseReason={baseReason} op={props.sampleOp} refreshKey={props.samplesRefresh} setPending={setPending} onSetUp={onSetUp} />
      </StackItem>
      {deployed && !readOnly && !isTerminating(mlflow) && (
        <StackItem><DeployPR busy={busy} baseReason={baseReason} prOverride={!!mlflow?.prOverride} setPending={setPending} /></StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[<>On the cluster: MLflow CR <code>mlflow</code>; the MLflow operator runs the server in <code>{mlflow?.namespace || "redhat-ods-applications"}</code> with its default (RHOAI) image, a SQLite database and a 10Gi PersistentVolumeClaim</>]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && managed && !isTerminating(mlflow) && (
        <StackItem>
          <CleanupSection id="mlflow-cleanup" label="Clean up MLflow…" onClick={onCleanup} reason={baseReason ?? mlflow?.teardownBlockedReason ?? null}
            text="Deletes the MLflow server this tool created, with every experiment, run and model in it." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The confirmation of an MLflow change, or null for another feature's change. */
export function mlflowConfirm(pending: Pending, mlflow: ResourceState | undefined): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-mlflow":
      return {
        title: "Set up MLflow?", confirm: "Set up", runTitle: "Set up MLflow",
        summary: <Content component="p">On the cluster: one MLflow server, with a workspace per data science project and its data on a 10 GiB volume. Takes 1 to 3 minutes.</Content>,
        changes: [<>MLflow CR <code>mlflow</code> is created; the MLflow operator deploys the server in <code>redhat-ods-applications</code> with its default (RHOAI) image.</>],
      };
    case "teardown-mlflow":
      return {
        title: "Delete MLflow?", confirm: "Delete", danger: true, runTitle: "Delete MLflow",
        summary: <Content component="p">Deletes the MLflow server, with its data in every workspace.</Content>,
        changes: [<>MLflow CR <code>mlflow</code> is deleted; the operator removes the server.</>],
        dataLoss: pvcsLost(mlflow, "every experiment, run and artifact stored in it") ?? <>Every experiment, run and artifact stored in MLflow.</>,
      };
    case "deploy-mlflow-pr":
      return {
        title: `Deploy MLflow PR #${pending.pr}?`, confirm: `Deploy PR #${pending.pr}`, runTitle: `Deploy MLflow PR #${pending.pr}`,
        summary: <Content component="p">The MLflow server restarts on the image of opendatahub-io/mlflow PR #{pending.pr}; it is unavailable until the new pod is ready.{!mlflow?.prOverride ? " Revert goes back to the image used now." : ""}</Content>,
        changes: [
          <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is set to <code>quay.io/opendatahub/mlflow:odh-pr-{pending.pr}</code>, pinned to its current digest.</>,
          ...(!mlflow?.prOverride ? [<>The image in use now is saved on the CR so Revert can restore it.</>] : []),
        ],
        extra: mlflow?.managedByTool === false
          ? <Alert component="p" variant="warning" isInline title="This MLflow server was not created by this tool">Someone else may rely on it. Revert restores its current image.</Alert>
          : undefined,
      };
    case "revert-mlflow":
      return {
        title: "Revert MLflow?", confirm: "Revert", runTitle: "Revert MLflow",
        summary: <Content component="p">The MLflow server restarts on {mlflow?.revertImage ? "the image used before the PR" : "the operator's default (RHOAI) image"}.</Content>,
        changes: [mlflow?.revertImage
          ? <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> goes back to <code>{mlflow.revertImage}</code>, the image used before the PR.</>
          : <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is removed, so the operator&apos;s default (RHOAI) image is used again.</>],
      };
    default:
      return null;
  }
}
