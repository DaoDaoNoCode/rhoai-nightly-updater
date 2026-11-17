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
import type { ResourceState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  ProblemAlerts,
  createdBy,
  isTerminating,
  pvcsLost,
  terminalReason,
} from "./resourceRow";
import { CreatesList, DetailsActions, DetailsStack, HowToUse, ResourceCard } from "./ResourceCard";
import { envStatus, notManaged, type EnvStatus } from "./resourceStatus";

export interface MLflowProps extends CardActionProps {
  mlflow?: ResourceState;
}

export function mlflowStatus({ mlflow, resAction }: Pick<MLflowProps, "mlflow" | "resAction">): EnvStatus {
  const busy = resAction === "setup-mlflow" || resAction === "deploy-mlflow-pr" || resAction === "revert-mlflow" ? "setting-up" : resAction === "teardown-mlflow" ? "deleting" : null;
  return envStatus(mlflow, { busy });
}

/** The MLflow server (CR mlflow) as a compact card. */
export const MLflowCard: React.FC<MLflowProps & { onDetails: (opener: HTMLElement) => void; detailsOpen: boolean }> = (props) => {
  const { mlflow, baseReason, resAction, setPending, onDetails, detailsOpen } = props;
  let action: React.ReactNode = null;
  if (mlflow && !mlflow.deployed && !isTerminating(mlflow)) {
    action = <TooltipButton variant="primary" onClick={() => setPending({ kind: "setup-mlflow" })} isLoading={resAction === "setup-mlflow"}
      disabledReason={baseReason ?? mlflow.setupBlockedReason ?? null}>Set up</TooltipButton>;
  } else if (mlflow?.prOverride && terminalReason(mlflow)) {
    // The PR image broke MLflow: going back is the way out.
    action = <TooltipButton variant="primary" onClick={() => setPending({ kind: "revert-mlflow" })} isLoading={resAction === "revert-mlflow"} disabledReason={baseReason}>Revert the PR image</TooltipButton>;
  } else if (mlflow?.ready && mlflow.uiRoute) {
    action = <ExternalLink href={mlflow.uiRoute}>Open MLflow</ExternalLink>;
  }
  return (
    <ResourceCard id="mlflow" title="MLflow" status={mlflowStatus(props)} unmanaged={notManaged(mlflow)} action={action} onDetails={onDetails} detailsOpen={detailsOpen}
      needs={mlflow?.prOverride ? `Runs the image of opendatahub-io/mlflow PR #${mlflow.prNumber ?? "?"}.` : undefined}
      purpose="An MLflow server for tracking experiments, run by the MLflow operator." />
  );
};

/** The MLflow server's details, in the drawer: deploying a PR image, Revert and Tear down. */
export const MLflowDetails: React.FC<MLflowProps> = ({ mlflow, baseReason, resAction, setPending }) => {
  const [pr, setPr] = useState("");
  const prValid = /^\d+$/.test(pr) && Number(pr) > 0;
  const deployed = !!mlflow?.deployed;
  return (
    <DetailsStack>
      {mlflow && <ProblemAlerts kind="mlflow" state={mlflow} />}
      <DescriptionList isCompact isHorizontal aria-label="MLflow details">
        <DescriptionListGroup>
          <DescriptionListTerm>Namespace</DescriptionListTerm>
          <DescriptionListDescription><code>redhat-ods-applications</code></DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>MLflow CR</DescriptionListTerm>
          <DescriptionListDescription><code>mlflow</code></DescriptionListDescription>
        </DescriptionListGroup>
        {deployed && mlflow?.currentImage && (
          <DescriptionListGroup>
            <DescriptionListTerm>Image</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image={mlflow.currentImage} />{mlflow.prOverride ? ` (PR #${mlflow.prNumber ?? "?"})` : ""}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {deployed && !mlflow?.ready && mlflow?.message && (
          <DescriptionListGroup>
            <DescriptionListTerm>State</DescriptionListTerm>
            <DescriptionListDescription>{mlflow.message}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {deployed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Created by</DescriptionListTerm>
            <DescriptionListDescription>{createdBy(mlflow)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      {deployed && (
        <Form onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Deploy an opendatahub-io/mlflow PR" fieldId="mlflow-pr">
            <InputGroup>
              <InputGroupItem isFill>
                <TextInput id="mlflow-pr" type="text" inputMode="numeric" value={pr} onChange={(_e, v) => setPr(v.trim())} placeholder="PR number" />
              </InputGroupItem>
              <InputGroupItem>
                <TooltipButton variant="control" onClick={() => setPending({ kind: "deploy-mlflow-pr", pr: Number(pr) })} isLoading={resAction === "deploy-mlflow-pr"}
                  disabledReason={baseReason ?? (!prValid ? "Enter an opendatahub-io/mlflow PR number first." : null)}>Deploy PR</TooltipButton>
              </InputGroupItem>
            </InputGroup>
          </FormGroup>
        </Form>
      )}
      {deployed && (mlflow?.prOverride || (mlflow?.managedByTool !== false && !isTerminating(mlflow))) && (
        <DetailsActions>
          {mlflow?.prOverride && (
            <ActionListItem>
              <TooltipButton variant="secondary" onClick={() => setPending({ kind: "revert-mlflow" })} isLoading={resAction === "revert-mlflow"} disabledReason={baseReason}>Revert the PR image</TooltipButton>
            </ActionListItem>
          )}
          {mlflow?.managedByTool !== false && !isTerminating(mlflow) && (
            <ActionListItem>
              <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-mlflow" })} isLoading={resAction === "teardown-mlflow"}
                disabledReason={baseReason ?? mlflow?.teardownBlockedReason ?? null}>Tear down</TooltipButton>
            </ActionListItem>
          )}
        </DetailsActions>
      )}
      {deployed && (
        <HowToUse text={<p>Open MLflow{mlflow?.uiRoute ? <> at <a href={mlflow.uiRoute} target="_blank" rel="noopener noreferrer">{mlflow.uiRoute.replace(/^https:\/\//, "")}</a></> : ""} with your OpenShift login.</p>} />
      )}
      <CreatesList items={[<>MLflow CR <code>mlflow</code>; the MLflow operator deploys the server in <code>redhat-ods-applications</code></>]} />
    </DetailsStack>
  );
};

/** The confirmation of an MLflow change, or null for another card's change. */
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
