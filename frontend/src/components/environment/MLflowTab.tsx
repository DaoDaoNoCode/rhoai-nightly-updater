import React, { useState } from "react";
import {
  ActionListItem,
  Alert,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  InputGroup,
  InputGroupItem,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import type { ResourceState, ResourcesStatus } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type Pending, ExternalLink, ProblemAlerts, isTerminating, pvcsLost } from "./resourceRow";
import { ActionRow, CreatesList, FeatureHeader, FeatureSection, PartItem, PartList, PrimaryAction } from "./FeatureLayout";
import { notManaged } from "./resourceStatus";
import { type Busy, type FeatureState, mlflowStatus } from "./featureState";

export interface MLflowTabProps {
  status: ResourcesStatus;
  /** mlflowState: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
}

/**
 * The MLflow tab: the MLflow server (CR mlflow) with its health, Open
 * MLflow, deploying an opendatahub-io/mlflow PR image and Revert, Set up
 * when there is none, and what it creates and Tear down below.
 */
export const MLflowTab: React.FC<MLflowTabProps> = ({ status, state, busy, baseReason, setPending, onSetUp }) => {
  const mlflow = status.mlflow;
  const [pr, setPr] = useState("");
  const prValid = /^\d+$/.test(pr) && Number(pr) > 0;
  const deployed = !!mlflow?.deployed;
  const unmanaged = notManaged(mlflow);
  const managed = deployed && mlflow?.managedByTool !== false;
  const next = state.next;
  const actions: React.ReactNode[] = [];
  if (mlflow?.prOverride) {
    actions.push(<ActionListItem key="revert"><TooltipButton variant="secondary" onClick={() => setPending({ kind: "revert-mlflow" })} isLoading={busy.resAction === "revert-mlflow"} disabledReason={baseReason}>Revert the PR image</TooltipButton></ActionListItem>);
  }
  if (mlflow?.ready && mlflow.uiRoute) actions.push(<ActionListItem key="open"><ExternalLink href={mlflow.uiRoute}>Open MLflow</ExternalLink></ActionListItem>);
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="MLflow" purpose="Track experiments: log runs, metrics and models to an MLflow server on the cluster." state={state} /></StackItem>
      <StackItem>
        <PartList aria-label="What MLflow needs">
          <PartItem id="mlflow" title="MLflow server" scope="cluster" status={mlflowStatus(status, busy)} unmanaged={unmanaged} actions={actions.length ? actions : undefined}>
            <Content component="p">
              MLflow CR <code>mlflow</code>; the MLflow operator runs the server in <code>redhat-ods-applications</code>.
              {unmanaged ? " This tool did not create it: it can deploy a PR image on it, but does not tear it down." : ""}
            </Content>
            {mlflow && <ProblemAlerts kind="mlflow" state={mlflow} />}
            {deployed && (
              <DescriptionList isCompact isHorizontal aria-label="MLflow details">
                {mlflow?.currentImage && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Image</DescriptionListTerm>
                    <DescriptionListDescription><ImageRef image={mlflow.currentImage} />{mlflow.prOverride ? ` (opendatahub-io/mlflow PR #${mlflow.prNumber ?? "?"})` : ""}</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                {mlflow?.uiRoute && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Address</DescriptionListTerm>
                    <DescriptionListDescription><ImageRef image={mlflow.uiRoute} what="MLflow address" truncate={false} isCode={false} /> (sign in with your OpenShift login)</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                {!mlflow?.ready && mlflow?.message && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>State</DescriptionListTerm>
                    <DescriptionListDescription>{mlflow.message}</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
              </DescriptionList>
            )}
            {deployed && (
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label="Deploy an opendatahub-io/mlflow PR" fieldId="mlflow-pr">
                  <InputGroup>
                    <InputGroupItem isFill>
                      <TextInput id="mlflow-pr" type="text" inputMode="numeric" value={pr} onChange={(_e, v) => setPr(v.trim())} placeholder="PR number" />
                    </InputGroupItem>
                    <InputGroupItem>
                      <TooltipButton variant="control" onClick={() => setPending({ kind: "deploy-mlflow-pr", pr: Number(pr) })} isLoading={busy.resAction === "deploy-mlflow-pr"}
                        disabledReason={baseReason ?? (!prValid ? "Enter an opendatahub-io/mlflow PR number first." : null)}>Deploy PR</TooltipButton>
                    </InputGroupItem>
                  </InputGroup>
                </FormGroup>
              </Form>
            )}
          </PartItem>
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction>
            <TooltipButton variant="primary" onClick={() => onSetUp(next.setup)} disabledReason={baseReason ?? next.reason}>{next.label}</TooltipButton>
          </PrimaryAction>
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[<>MLflow CR <code>mlflow</code>; the MLflow operator deploys the server in <code>redhat-ods-applications</code> with its default (RHOAI) image</>]} />
        </FeatureSection>
      </StackItem>
      {deployed && (
        <StackItem>
          <FeatureSection title="Tear down">
            {managed && !isTerminating(mlflow) ? (
              <ActionRow>
                <ActionListItem>
                  <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-mlflow" })} isLoading={busy.resAction === "teardown-mlflow"}
                    disabledReason={baseReason ?? mlflow?.teardownBlockedReason ?? null}>Tear down MLflow</TooltipButton>
                </ActionListItem>
              </ActionRow>
            ) : (
              <Content component="p">{unmanaged ? "This MLflow server was not created by this tool, so it is not torn down here." : "It is being deleted."}</Content>
            )}
          </FeatureSection>
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
        title: "Set up MLflow?", confirm: "Set up MLflow",
        changes: [<>MLflow CR <code>mlflow</code> is created; the MLflow operator deploys the server in <code>redhat-ods-applications</code> with its default (RHOAI) image.</>],
      };
    case "teardown-mlflow":
      return {
        title: "Tear down MLflow?", confirm: "Tear down MLflow", danger: true,
        changes: [<>MLflow CR <code>mlflow</code> is deleted; the operator removes the server.</>],
        dataLoss: pvcsLost(mlflow, "every experiment, run and artifact stored in it") ?? <>Every experiment, run and artifact stored in MLflow.</>,
      };
    case "deploy-mlflow-pr":
      return {
        title: `Deploy MLflow PR #${pending.pr}?`, confirm: `Deploy PR #${pending.pr}`,
        changes: [
          <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is set to <code>quay.io/opendatahub/mlflow:odh-pr-{pending.pr}</code>, pinned to its current digest.</>,
          ...(!mlflow?.prOverride ? [<>The image in use now is saved on the CR so Revert can restore it.</>] : []),
        ],
        extra: (
          <Stack hasGutter>
            <StackItem><Content component="p">The MLflow server restarts; it is unavailable until the new pod is ready.</Content></StackItem>
            {mlflow?.managedByTool === false && (
              <StackItem><Alert component="p" variant="warning" isInline title="This MLflow instance was not created by this tool">Someone else may rely on it. Revert restores its current image.</Alert></StackItem>
            )}
          </Stack>
        ),
      };
    case "revert-mlflow":
      return {
        title: "Revert MLflow?", confirm: "Revert MLflow",
        changes: [mlflow?.revertImage
          ? <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> goes back to <code>{mlflow.revertImage}</code>, the image used before the PR.</>
          : <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is removed, so the operator&apos;s default (RHOAI) image is used again.</>],
        extra: <Content component="p">The MLflow server restarts.</Content>,
      };
    default:
      return null;
  }
}
