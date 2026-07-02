import React, { useEffect, useRef, useState } from "react";
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

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

import type { UpdateStep } from "../types";

export type OperationType =
  | "update"
  | "reinstall_stable"
  | "reinstall_nightly"
  | "refresh";

export interface UpdatePipelineProps {
  steps: UpdateStep[];
  active: boolean;
  error?: string;
  operationType?: OperationType;
  onComplete?: () => void;
  onFailed?: (error: string) => void;
}

// ---------------------------------------------------------------------------
// Pipeline step definitions (mirrors the backend execution order)
// ---------------------------------------------------------------------------

interface PipelineStepDef {
  id: string;
  label: string;
  description: string;
}

const UPDATE_STEPS: PipelineStepDef[] = [
  {
    id: "validate_prerequisites",
    label: "Validate Prerequisites",
    description: "Check pull secret, image mirror, and operator group",
  },
  {
    id: "save_snapshot",
    label: "Save Snapshot",
    description: "Record current deployment state for change detection",
  },
  {
    id: "apply_catalog_source",
    label: "Apply CatalogSource",
    description: "Create or update the nightly catalog with the FBC image",
  },
  {
    id: "wait_catalog_ready",
    label: "CatalogSource Loading",
    description:
      "Wait for the catalog pod to pull the image and serve content",
  },
  {
    id: "detect_channel",
    label: "Detect Channel",
    description: "Find the best channel from the catalog for this version",
  },
  {
    id: "apply_subscription",
    label: "Apply Subscription",
    description: "Point the operator subscription to the nightly catalog",
  },
  {
    id: "delete_csv",
    label: "Delete CSV",
    description: "Remove old operator to trigger fresh install",
  },
  {
    id: "verify_installplan",
    label: "Verify InstallPlan",
    description: "Confirm OLM created a new install plan",
  },
];

const REINSTALL_STABLE_STEPS: PipelineStepDef[] = [
  {
    id: "validate_target",
    label: "Validate Target",
    description: "Verify the target stable version is available",
  },
  {
    id: "save_snapshot",
    label: "Save Snapshot",
    description: "Record current deployment state before reinstall",
  },
  {
    id: "delete_catalog_source",
    label: "Delete CatalogSource",
    description: "Remove the nightly catalog source",
  },
  {
    id: "delete_subscription",
    label: "Delete Subscription",
    description: "Remove the current operator subscription",
  },
  {
    id: "delete_csv",
    label: "Delete CSV",
    description: "Remove the installed ClusterServiceVersion",
  },
  {
    id: "cleanup_webhooks",
    label: "Cleanup Webhooks",
    description: "Remove orphaned webhook configurations",
  },
  {
    id: "patch_crds",
    label: "Patch CRDs",
    description: "Remove owner references from CRDs to allow re-adoption",
  },
  {
    id: "wait_propagation",
    label: "Wait Propagation",
    description: "Wait for resource deletion to propagate across the cluster",
  },
  {
    id: "create_subscription",
    label: "Create Subscription",
    description: "Create a new subscription to the stable channel",
  },
  {
    id: "verify_installplan",
    label: "Verify InstallPlan",
    description: "Confirm OLM created a new install plan for the stable version",
  },
];

const REINSTALL_NIGHTLY_STEPS: PipelineStepDef[] = [
  {
    id: "validate_target",
    label: "Validate Target",
    description: "Verify the target nightly image is accessible",
  },
  {
    id: "save_snapshot",
    label: "Save Snapshot",
    description: "Record current deployment state before reinstall",
  },
  {
    id: "delete_catalog_source",
    label: "Delete CatalogSource",
    description: "Remove the existing catalog source",
  },
  {
    id: "delete_subscription",
    label: "Delete Subscription",
    description: "Remove the current operator subscription",
  },
  {
    id: "delete_csv",
    label: "Delete CSV",
    description: "Remove the installed ClusterServiceVersion",
  },
  {
    id: "cleanup_webhooks",
    label: "Cleanup Webhooks",
    description: "Remove orphaned webhook configurations",
  },
  {
    id: "patch_crds",
    label: "Patch CRDs",
    description: "Remove owner references from CRDs to allow re-adoption",
  },
  {
    id: "wait_propagation",
    label: "Wait Propagation",
    description: "Wait for resource deletion to propagate across the cluster",
  },
  {
    id: "create_catalog_source",
    label: "Create CatalogSource",
    description: "Create a new catalog source pointing to the nightly FBC image",
  },
  {
    id: "wait_catalog_ready",
    label: "CatalogSource Loading",
    description: "Wait for the catalog pod to pull the image and serve content",
  },
  {
    id: "detect_channel",
    label: "Detect Channel",
    description: "Find the best channel from the catalog for this version",
  },
  {
    id: "create_subscription",
    label: "Create Subscription",
    description: "Create a new subscription to the nightly catalog",
  },
  {
    id: "verify_installplan",
    label: "Verify InstallPlan",
    description: "Confirm OLM created a new install plan for the nightly version",
  },
];

const REFRESH_STEPS: PipelineStepDef[] = [
  {
    id: "verify_csv",
    label: "Verify CSV",
    description: "Check the current ClusterServiceVersion exists and is healthy",
  },
  {
    id: "save_snapshot",
    label: "Save Snapshot",
    description: "Record current deployment state before refresh",
  },
  {
    id: "get_subscription",
    label: "Get Subscription",
    description: "Retrieve current subscription details for recreation",
  },
  {
    id: "delete_csv",
    label: "Delete CSV",
    description: "Remove the installed ClusterServiceVersion",
  },
  {
    id: "delete_subscription",
    label: "Delete Subscription",
    description: "Remove the current operator subscription",
  },
  {
    id: "wait_cleanup",
    label: "Wait Cleanup",
    description: "Wait for OLM to finish cleaning up removed resources",
  },
  {
    id: "recreate_subscription",
    label: "Recreate Subscription",
    description: "Re-create the subscription to trigger a fresh install",
  },
  {
    id: "verify_installplan",
    label: "Verify InstallPlan",
    description: "Confirm OLM created a new install plan",
  },
];

/** Map operation type to its step definitions. */
const STEP_SETS: Record<OperationType, PipelineStepDef[]> = {
  update: UPDATE_STEPS,
  reinstall_stable: REINSTALL_STABLE_STEPS,
  reinstall_nightly: REINSTALL_NIGHTLY_STEPS,
  refresh: REFRESH_STEPS,
};

/** Map operation type to a human-readable pipeline title. */
const PIPELINE_TITLES: Record<OperationType, string> = {
  update: "Update Pipeline",
  reinstall_stable: "Reinstall Pipeline (Stable)",
  reinstall_nightly: "Reinstall Pipeline (Nightly)",
  refresh: "Refresh Pipeline",
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function formatElapsed(startMs: number): string {
  const elapsed = Math.max(0, Math.floor((Date.now() - startMs) / 1000));
  if (elapsed < 60) return `${elapsed}s`;
  const min = Math.floor(elapsed / 60);
  const sec = elapsed % 60;
  return `${min}m ${sec}s`;
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
    case "skipped":
      return "pending";
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

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export const UpdatePipeline: React.FC<UpdatePipelineProps> = ({
  steps: events,
  active,
  error,
  operationType = "update",
  onComplete,
  onFailed,
}) => {
  const pipelineSteps = STEP_SETS[operationType];
  const pipelineTitle = PIPELINE_TITLES[operationType];

  // Track total elapsed time while the pipeline is active
  const [elapsed, setElapsed] = useState("");
  const startTimeRef = useRef<number>(0);

  // Guards to ensure callbacks fire only once per pipeline run
  const completeFiredRef = useRef(false);
  const failFiredRef = useRef(false);

  // Reset guards when a new pipeline starts (active transitions to true)
  const prevActiveRef = useRef(false);
  useEffect(() => {
    if (active && !prevActiveRef.current) {
      completeFiredRef.current = false;
      failFiredRef.current = false;
      startTimeRef.current = Date.now();
    }
    prevActiveRef.current = active;
  }, [active]);

  // Tick the elapsed timer
  useEffect(() => {
    if (!active) return;
    if (startTimeRef.current === 0) {
      startTimeRef.current = Date.now();
    }
    const tick = () => setElapsed(formatElapsed(startTimeRef.current));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [active]);

  // Check for completion or failure whenever events change
  useEffect(() => {
    if (events.length === 0) return;

    // Check for any failed step
    const failedEvent = events.find((e) => e.status === "failed");
    if (failedEvent && !failFiredRef.current) {
      failFiredRef.current = true;
      onFailed?.(failedEvent.message);
      return;
    }

    // Check if the last pipeline step succeeded
    const lastStepId = pipelineSteps[pipelineSteps.length - 1].id;
    const lastStepEvent = latestEventForStep(lastStepId, events);
    if (lastStepEvent?.status === "success" && !completeFiredRef.current) {
      completeFiredRef.current = true;
      // Brief delay so the user can see the final checkmark
      const timer = setTimeout(() => {
        onComplete?.();
      }, 1500);
      return () => clearTimeout(timer);
    }
  }, [events, pipelineSteps, onComplete, onFailed]);

  // Determine which step is currently active (for isCurrent)
  const currentStepId = (() => {
    // Find the first step that is "running"
    for (const def of pipelineSteps) {
      const event = latestEventForStep(def.id, events);
      if (event?.status === "running") return def.id;
    }
    // If no step is running, find the first step with no event (next pending)
    for (const def of pipelineSteps) {
      const event = latestEventForStep(def.id, events);
      if (!event) return def.id;
    }
    // All steps have events -- the last one is current
    return pipelineSteps[pipelineSteps.length - 1].id;
  })();

  // Compact summary when pipeline completed (not active, has events)
  const allCompleted = !active && events.length > 0 && completeFiredRef.current;
  const anyFailed = events.some((e) => e.status === "failed");

  if (allCompleted && !anyFailed) {
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
                  <Spinner size="sm" aria-label="Pipeline running" />
                </FlexItem>
              )}
            </Flex>
          </FlexItem>
          <FlexItem>
            {elapsed && (
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
