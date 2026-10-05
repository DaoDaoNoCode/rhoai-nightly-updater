import React, { useEffect, useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  Label,
  ProgressStepper,
  ProgressStep,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import MinusCircleIcon from "@patternfly/react-icons/dist/esm/icons/minus-circle-icon";
import type { UpdateStep } from "../types";
import { PIPELINE_TITLES, STEP_SETS, type OperationKind } from "../operationSteps";
import { formatElapsed } from "../utils";

export type { OperationKind as OperationType } from "../operationSteps";

export interface UpdatePipelineProps {
  steps: UpdateStep[];
  active: boolean;
  /** When the operation started (epoch ms), for the elapsed timer. */
  startedAt?: number;
  error?: string;
  operationType?: OperationKind;
}

function formatStepDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const sec = (ms / 1000).toFixed(1);
  return `${sec}s`;
}

/**
 * Find the latest event for a given step id from the accumulated events list.
 * Events arrive in chronological order; the last match wins.
 */
function latestEventForStep(
  step: string,
  events: UpdateStep[],
): UpdateStep | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].step === step) return events[i];
  }
  return undefined;
}

/**
 * Map a step's status to a ProgressStep variant.
 */
function stepVariant(
  event: UpdateStep | undefined,
): "success" | "info" | "pending" | "danger" {
  if (!event) return "pending";
  switch (event.status) {
    case "success":
      return "success";
    case "running":
      return "info";
    case "failed":
      return "danger";
    default:
      return "pending";
  }
}

/**
 * Render the appropriate icon for a step based on its event status.
 */
function stepIcon(event: UpdateStep | undefined): React.ReactNode {
  if (!event) return undefined; // PatternFly default (grey circle)
  switch (event.status) {
    case "running":
      return <Spinner size="sm" aria-label="Running" />;
    case "success":
      return <CheckCircleIcon />;
    case "failed":
      return <ExclamationCircleIcon />;
    case "skipped":
      return <MinusCircleIcon />;
    default:
      return undefined;
  }
}

/**
 * Step-by-step view of a streamed operation. Purely presentational: the
 * operation store decides when the operation has finished.
 */
export const UpdatePipeline: React.FC<UpdatePipelineProps> = ({
  steps: events,
  active,
  startedAt,
  error,
  operationType = "update",
}) => {
  const pipelineSteps = STEP_SETS[operationType];
  const pipelineTitle = PIPELINE_TITLES[operationType];

  // Tick the elapsed timer while the pipeline is active.
  const [elapsed, setElapsed] = useState("");
  useEffect(() => {
    if (!active || !startedAt) return;
    const tick = () => setElapsed(formatElapsed(startedAt));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [active, startedAt]);

  // Determine which step is currently active (for isCurrent)
  const currentStepId = (() => {
    for (const def of pipelineSteps) {
      if (latestEventForStep(def.id, events)?.status === "running") return def.id;
    }
    for (const def of pipelineSteps) {
      if (!latestEventForStep(def.id, events)) return def.id;
    }
    return pipelineSteps[pipelineSteps.length - 1].id;
  })();

  const anyFailed = events.some((e) => e.status === "failed");
  const lastStepSucceeded = latestEventForStep(pipelineSteps[pipelineSteps.length - 1].id, events)?.status === "success";

  // Compact summary once the pipeline finished without failures.
  if (!active && events.length > 0 && lastStepSucceeded && !anyFailed) {
    const totalMs = events.reduce((max, e) => Math.max(max, e.elapsedMs || 0), 0);
    return (
      <Card isCompact>
        <CardBody>
          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
            <FlexItem>
              <Label color="green" icon={<CheckCircleIcon />}>
                {pipelineTitle} completed
              </Label>
            </FlexItem>
            <FlexItem>
              <Content component="small">
                {pipelineSteps.length} steps in {formatStepDuration(totalMs)}
              </Content>
            </FlexItem>
          </Flex>
        </CardBody>
      </Card>
    );
  }

  return (
    <Card isCompact>
      <CardTitle>
        <Flex
          justifyContent={{ default: "justifyContentSpaceBetween" }}
          alignItems={{ default: "alignItemsCenter" }}
        >
          <FlexItem>
            <Flex
              alignItems={{ default: "alignItemsCenter" }}
              gap={{ default: "gapSm" }}
            >
              <FlexItem>
                <Content component="h4">{pipelineTitle}</Content>
              </FlexItem>
              {active && (
                <FlexItem>
                  <Spinner size="sm" aria-label={`${pipelineTitle} running`} />
                </FlexItem>
              )}
            </Flex>
          </FlexItem>
          <FlexItem>
            {active && elapsed && (
              <Content component="small">Elapsed: {elapsed}</Content>
            )}
          </FlexItem>
        </Flex>
      </CardTitle>
      <CardBody>
        <Stack hasGutter>
          {/* Connection-level error */}
          {error && (
            <StackItem>
              <Alert
                variant="danger"
                title="Connection error"
                isInline
                component="p"
              >
                {error}
              </Alert>
            </StackItem>
          )}

          {/* Pipeline steps */}
          <StackItem>
            <ProgressStepper isVertical aria-label={`${pipelineTitle} progress`}>
              {pipelineSteps.map((def) => {
                const event = latestEventForStep(def.id, events);
                const variant = stepVariant(event);
                const isCurrent = def.id === currentStepId;
                const icon = stepIcon(event);

                return (
                  <ProgressStep
                    key={def.id}
                    id={def.id}
                    titleId={`pipeline-step-${def.id}`}
                    variant={variant}
                    isCurrent={isCurrent}
                    icon={icon}
                    aria-label={def.label}
                    description={
                      event?.message
                        ? `${def.description} -- ${event.message}`
                        : def.description
                    }
                  >
                    <Flex
                      alignItems={{ default: "alignItemsCenter" }}
                      gap={{ default: "gapSm" }}
                    >
                      <FlexItem>{def.label}</FlexItem>
                      {event?.elapsedMs != null && event.elapsedMs > 0 && (
                        <FlexItem>
                          <Label isCompact color="grey">
                            {formatStepDuration(event.elapsedMs)}
                          </Label>
                        </FlexItem>
                      )}
                      {event?.status === "skipped" && (
                        <FlexItem>
                          <Label isCompact color="orange">
                            skipped
                          </Label>
                        </FlexItem>
                      )}
                    </Flex>
                  </ProgressStep>
                );
              })}
            </ProgressStepper>
          </StackItem>

          {/* Per-step failure details */}
          {events
            .filter((e) => e.status === "failed")
            .map((e) => (
              <StackItem key={`error-${e.step}`}>
                <Alert
                  variant="danger"
                  title={`Step failed: ${
                    pipelineSteps.find((s) => s.id === e.step)?.label ??
                    e.step
                  }`}
                  isInline
                  component="p"
                >
                  <Stack hasGutter>
                    <StackItem>{e.message}</StackItem>
                    {e.errorCode && (
                      <StackItem>
                        <Content component="small">
                          Error code:{" "}
                          <Label isCompact color="red">
                            {e.errorCode}
                          </Label>
                        </Content>
                      </StackItem>
                    )}
                  </Stack>
                </Alert>
              </StackItem>
            ))}
        </Stack>
      </CardBody>
    </Card>
  );
};
