import React, { useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  InputGroup,
  InputGroupItem,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";
import type { ResourceState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { CardList, CardListItem } from "../CardList";
import { TagLabel } from "../StatusLabel";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  RowTitle,
  StatusDetails,
  isTerminating,
  pvcsLost,
  rowActions,
  unmanagedNote,
} from "./resourceRow";

/** The MLflow server (CR mlflow), with deploying an opendatahub-io/mlflow PR image and reverting it. */
export const MLflowCard: React.FC<CardActionProps & { mlflow?: ResourceState }> = ({ mlflow, baseReason, resAction, setPending }) => {
  const [mlflowPR, setMlflowPR] = useState("");
  const mlflowPRValid = /^\d+$/.test(mlflowPR) && Number(mlflowPR) > 0;
  return (
    <Card>
      <CardHeader><CardTitle><Title headingLevel="h3" size="lg">MLflow</Title></CardTitle></CardHeader>
      <CardBody>
        <CardList aria-label="MLflow">
          <CardListItem labelledBy="mlflow-item" actions={rowActions(
            <>
              {mlflow?.ready && mlflow.uiRoute && <FlexItem><ExternalLink href={mlflow.uiRoute}>Open MLflow</ExternalLink></FlexItem>}
              {mlflow && !mlflow.deployed && (
                <FlexItem>
                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "setup-mlflow" })} isLoading={resAction === "setup-mlflow"}
                    disabledReason={baseReason ?? mlflow.setupBlockedReason ?? null}>Set up</TooltipButton>
                </FlexItem>
              )}
              {mlflow?.deployed && mlflow.managedByTool !== false && !isTerminating(mlflow) && (
                <FlexItem>
                  <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-mlflow" })} isLoading={resAction === "teardown-mlflow"}
                    disabledReason={baseReason ?? mlflow.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                </FlexItem>
              )}
            </>,
          )}>
            <RowTitle id="mlflow-item" name="MLflow server" state={mlflow}
              extra={mlflow?.prOverride ? <FlexItem><TagLabel color="blue">PR #{mlflow.prNumber ?? "?"}</TagLabel></FlexItem> : undefined} />
            <Content component="p" className="pf-v6-u-text-color-subtle">An MLflow instance in <code>redhat-ods-applications</code>, managed by the MLflow operator.{unmanagedNote(mlflow)}</Content>
            {mlflow && <StatusDetails kind="mlflow" state={mlflow} />}
            {mlflow?.deployed && (
              <Form className="pf-v6-u-mt-md" onSubmit={(e) => e.preventDefault()}>
                <FormGroup label="Deploy an opendatahub-io/mlflow PR" fieldId="mlflow-pr">
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsFlexStart" }}>
                    <FlexItem>
                      <InputGroup>
                        <InputGroupItem>
                          <TextInput id="mlflow-pr" type="text" inputMode="numeric" value={mlflowPR} onChange={(_e, val) => setMlflowPR(val.trim())} placeholder="PR number" aria-label="opendatahub-io/mlflow PR number" />
                        </InputGroupItem>
                        <InputGroupItem>
                          <TooltipButton variant="control" onClick={() => setPending({ kind: "deploy-mlflow-pr", pr: Number(mlflowPR) })} isLoading={resAction === "deploy-mlflow-pr"}
                            disabledReason={baseReason ?? (!mlflowPRValid ? "Enter an opendatahub-io/mlflow PR number first." : null)}>Deploy PR</TooltipButton>
                        </InputGroupItem>
                      </InputGroup>
                    </FlexItem>
                    {mlflow.prOverride && (
                      <FlexItem>
                        <TooltipButton variant="secondary" onClick={() => setPending({ kind: "revert-mlflow" })} isLoading={resAction === "revert-mlflow"} disabledReason={baseReason}>
                          Revert the PR image
                        </TooltipButton>
                      </FlexItem>
                    )}
                  </Flex>
                </FormGroup>
              </Form>
            )}
          </CardListItem>
        </CardList>
      </CardBody>
    </Card>
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
