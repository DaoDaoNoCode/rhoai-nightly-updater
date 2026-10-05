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
  Title,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import MinusCircleIcon from "@patternfly/react-icons/dist/esm/icons/minus-circle-icon";
import type { UpdateStep } from "../types";
import { PIPELINE_TITLES, RESTORE_STEP, STEP_SETS, type OperationKind, type PipelineStepDef } from "../operationSteps";
import { formatElapsed } from "../utils";

export type { OperationKind as OperationType } from "../operationSteps";

export interface UpdatePipelineProps {
  steps: UpdateStep[];
  active: boolean;
  /** When the operation started (epoch ms), for the elapsed timer. */
  startedAt?: number;
  error?: string;
  operationType?: OperationKind;
  /** Who started it, when it is not this tab's own run. */
  startedBy?: string;
}

function formatStepDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 60) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(totalSec / 60)}m ${totalSec % 60}s`;
}

/** Latest event for a step id; events arrive in order, so the last match wins. */
function latestEventForStep(step: string, events: UpdateStep[]): UpdateStep | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].step === step) return events[i];
  }
  return undefined;
}

function stepVariant(event: UpdateStep | undefined): "success" | "info" | "pending" | "danger" | "warning" {
  if (!event) return "pending";
  switch (event.status) {
    case "success": return "success";
    case "running": return "info";
    case "failed": return "danger";
    case "skipped": return "pending";
    default: return "pending";
  }
}

function stepIcon(event: UpdateStep | undefined): React.ReactNode {
  if (!event) return undefined; // PatternFly default (grey circle)
  switch (event.status) {
    case "running": return <Spinner size="sm" aria-label="Running" />;
    case "success": return <CheckCircleIcon />;
    case "failed": return <ExclamationCircleIcon />;
    case "skipped": return <MinusCircleIcon />;
    default: return undefined;
  }
}

/**
 * How long step `index` took. The backend's elapsedMs is the time since the
 * stream started, so a step's duration is its last event minus the last
 * event of the step before it. 0 when unknown (e.g. steps reported by
 * GET /api/operation carry no times).
 */
function stepDuration(defs: PipelineStepDef[], index: number, events: UpdateStep[]): number {
  const end = latestEventForStep(defs[index].id, events)?.elapsedMs ?? 0;
  if (end <= 0) return 0;
  for (let i = index - 1; i >= 0; i--) {
    const prev = latestEventForStep(defs[i].id, events);
    if (prev) return Math.max(0, end - (prev.elapsedMs || 0));
  }
  return end;
}

/** The pipeline's steps, plus the restore step once the backend runs it. */
export function pipelineStepsFor(kind: OperationKind, events: UpdateStep[]): PipelineStepDef[] {
  const defs = STEP_SETS[kind];
  return events.some((e) => e.step === RESTORE_STEP.id) ? [...defs, RESTORE_STEP] : defs;
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
  startedBy,
}) => {
  const pipelineSteps = pipelineStepsFor(operationType, events);
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

  // The step in progress, else the first one without an event.
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
  const lastStepSucceeded = latestEventForStep(STEP_SETS[operationType][STEP_SETS[operationType].length - 1].id, events)?.status === "success";

  // Compact summary once the pipeline finished without failures.
  if (!active && events.length > 0 && lastStepSucceeded && !anyFailed) {
    const totalMs = events.reduce((max, e) => Math.max(max, e.elapsedMs || 0), 0);
    return (
      <Card isCompact>
        <CardBody>
          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
            <FlexItem>
              <Label color="green" icon={<CheckCircleIcon />}>{pipelineTitle} finished</Label>
            </FlexItem>
            <FlexItem>
              <Content component="small">
                {pipelineSteps.length} steps{totalMs > 0 ? ` in ${formatStepDuration(totalMs)}` : ""}
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
        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
          <FlexItem>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
              <FlexItem>
                <Title headingLevel="h3" size="md">{pipelineTitle}{active ? " in progress" : anyFailed ? " failed" : ""}</Title>
              </FlexItem>
              {active && (
                <FlexItem>
                  <Spinner size="sm" aria-label={`${pipelineTitle} running`} />
                </FlexItem>
              )}
              {startedBy && (
                <FlexItem>
                  <Label isCompact variant="outline">Started by {startedBy}</Label>
                </FlexItem>
              )}
            </Flex>
          </FlexItem>
          <FlexItem>
            {active && elapsed && <Content component="small">Elapsed: {elapsed}</Content>}
          </FlexItem>
        </Flex>
      </CardTitle>
      <CardBody>
        <Stack hasGutter>
          {error && (
            <StackItem>
              <Alert variant="danger" title="Connection error" isInline component="p">{error}</Alert>
            </StackItem>
          )}

          <StackItem>
            <ProgressStepper isVertical isCompact aria-label={`${pipelineTitle} progress`}>
              {pipelineSteps.map((def, index) => {
                const event = latestEventForStep(def.id, events);
                const duration = event && event.status !== "running" ? stepDuration(pipelineSteps, index, events) : 0;
                return (
                  <ProgressStep
                    key={def.id}
                    id={def.id}
                    titleId={`pipeline-step-${def.id}`}
                    variant={stepVariant(event)}
                    isCurrent={def.id === currentStepId}
                    icon={stepIcon(event)}
                    aria-label={`${def.label}${event ? `: ${event.status}` : ""}`}
                    description={event?.message || def.description}
                  >
                    <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                      <FlexItem>{def.label}</FlexItem>
                      {duration >= 1000 && (
                        <FlexItem>
                          <Label isCompact color="grey">{formatStepDuration(duration)}</Label>
                        </FlexItem>
                      )}
                      {event?.status === "skipped" && (
                        <FlexItem><Label isCompact color="grey">skipped</Label></FlexItem>
                      )}
                    </Flex>
                  </ProgressStep>
                );
              })}
            </ProgressStepper>
          </StackItem>

          {events
            .filter((e) => e.status === "failed")
            .map((e) => (
              <StackItem key={`error-${e.step}`}>
                <Alert
                  variant="danger"
                  title={`${pipelineSteps.find((s) => s.id === e.step)?.label ?? e.step} failed`}
                  isInline
                  component="p"
                >
                  {e.message}
                  {e.errorCode && <> <Label isCompact color="red">{e.errorCode}</Label></>}
                </Alert>
              </StackItem>
            ))}
        </Stack>
      </CardBody>
    </Card>
  );
};
