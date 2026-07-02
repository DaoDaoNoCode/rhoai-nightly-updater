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
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import { Link } from "react-router-dom";
import { UpdatePipeline } from "./UpdatePipeline";
import { ReconciliationProgress } from "./ReconciliationProgress";
import type { OperationType as PipelineOperationType } from "./UpdatePipeline";
import type { OperationType as ReconcileOperationType } from "./ReconciliationProgress";
import type { StatusResponse, UpdateStep } from "../types";

interface OperationProgressProps {
  pipelineSteps: UpdateStep[];
  pipelineActive: boolean;
  pipelineOpType: PipelineOperationType;
  onPipelineComplete: () => void;
  onPipelineFailed: (error: string) => void;

  status: StatusResponse | null;
  reconciling: boolean;
  reconcileStartTime: number;
  reconcileTimedOut: boolean;
  reconcileOpType: ReconcileOperationType;
  showRecentComplete: boolean;
}

type Phase = "connecting" | "streaming" | "reconciling" | "complete";

export const OperationProgress: React.FC<OperationProgressProps> = ({
  pipelineSteps,
  pipelineActive,
  pipelineOpType,
  onPipelineComplete,
  onPipelineFailed,
  status,
  reconciling,
  reconcileStartTime,
  reconcileTimedOut,
  reconcileOpType,
  showRecentComplete,
}) => {
  const [pipelineCompleted, setPipelineCompleted] = useState(false);
  const [pipelineSuccessCount, setPipelineSuccessCount] = useState(0);
  const prevPipelineActiveRef = useRef(pipelineActive);
  const [connectingElapsed, setConnectingElapsed] = useState(0);
  const connectingStartRef = useRef(Date.now());

  // Elapsed timer for connecting phase
  useEffect(() => {
    if (pipelineActive && pipelineSteps.length === 0) {
      connectingStartRef.current = Date.now();
      const id = setInterval(() => {
        setConnectingElapsed(Math.floor((Date.now() - connectingStartRef.current) / 1000));
      }, 1000);
      return () => clearInterval(id);
    }
    setConnectingElapsed(0);
  }, [pipelineActive, pipelineSteps.length]);

  // Track pipeline completion for collapsed summary
  useEffect(() => {
    if (prevPipelineActiveRef.current && !pipelineActive && pipelineSteps.length > 0) {
      const successCount = pipelineSteps.filter(s => s.status === "success").length;
      if (successCount > 0) {
        setPipelineCompleted(true);
        setPipelineSuccessCount(successCount);
      }
    }
    prevPipelineActiveRef.current = pipelineActive;
  }, [pipelineActive, pipelineSteps]);

  // Reset when a new operation starts
  useEffect(() => {
    if (pipelineActive && pipelineSteps.length === 0) {
      setPipelineCompleted(false);
      setPipelineSuccessCount(0);
    }
  }, [pipelineActive, pipelineSteps.length]);

  // Determine current phase
  const phase: Phase = (() => {
    if (pipelineActive && pipelineSteps.length === 0) return "connecting";
    if (pipelineActive) return "streaming";
    if (reconciling || (showRecentComplete && !pipelineActive && pipelineSteps.length === 0)) return "reconciling";
    if (showRecentComplete) return "complete";
    if (pipelineSteps.length > 0) return "streaming";
    return "reconciling";
  })();

  return (
    <Stack hasGutter>
      {/* Phase 1: Connecting — SSE not delivering events yet */}
      {phase === "connecting" && (
        <StackItem>
          <Card isCompact>
            <CardBody>
              <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
                <FlexItem>
                  <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                    <FlexItem><Spinner size="md" /></FlexItem>
                    <FlexItem>
                      <Content component="p" style={{ fontWeight: 600, margin: 0 }}>Applying update to cluster...</Content>
                      <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)" }}>
                        Steps will appear as they complete.
                      </Content>
                    </FlexItem>
                  </Flex>
                </FlexItem>
                <FlexItem>
                  <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
                    {connectingElapsed > 0 && <FlexItem><Content component="small">Elapsed: {connectingElapsed}s</Content></FlexItem>}
                    <FlexItem><Link to="/components">View pods</Link></FlexItem>
                  </Flex>
                </FlexItem>
              </Flex>
            </CardBody>
          </Card>
        </StackItem>
      )}

      {/* Phase 2: Streaming — SSE steps are arriving (UpdatePipeline has its own Card) */}
      {phase === "streaming" && (
        <StackItem>
          <UpdatePipeline
            steps={pipelineSteps}
            active={pipelineActive}
            operationType={pipelineOpType}
            onComplete={onPipelineComplete}
            onFailed={onPipelineFailed}
          />
        </StackItem>
      )}

      {/* Phase 3: Reconciling — show collapsed pipeline summary + reconciliation */}
      {phase === "reconciling" && (
        <>
          {pipelineCompleted && (
            <StackItem>
              <Card isCompact>
                <CardBody>
                  <Label color="green" icon={<CheckCircleIcon />} isCompact>
                    Applied update ({pipelineSuccessCount} steps completed)
                  </Label>
                </CardBody>
              </Card>
            </StackItem>
          )}
          <StackItem>
            <ReconciliationProgress
              status={status}
              active={reconciling}
              startTime={reconcileStartTime}
              timedOut={reconcileTimedOut}
              operationType={reconcileOpType}
            />
          </StackItem>
        </>
      )}

      {/* Phase 4: Complete — persistent success/failure card with guidance */}
      {phase === "complete" && (
        <StackItem>
          {(() => {
            const csvPhase = status?.csv?.phase || "";
            const csvName = status?.csv?.name || "";
            const succeeded = csvPhase === "Succeeded";
            const failed = csvPhase === "Failed";

            return (
              <Card isCompact>
                <CardBody>
                  <Stack hasGutter>
                    <StackItem>
                      <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
                        <FlexItem>
                          {succeeded ? (
                            <Label color="green" icon={<CheckCircleIcon />}>Update complete</Label>
                          ) : failed ? (
                            <Label color="red" icon={<ExclamationCircleIcon />}>Update failed</Label>
                          ) : (
                            <Label color="green" icon={<CheckCircleIcon />}>Operator installed</Label>
                          )}
                        </FlexItem>
                        {csvName && (
                          <FlexItem>
                            <Content component="small">{csvName}</Content>
                          </FlexItem>
                        )}
                      </Flex>
                    </StackItem>
                    <StackItem>
                      <Alert
                        variant={succeeded ? "success" : failed ? "danger" : "info"}
                        title={succeeded
                          ? "The operator has been updated successfully."
                          : failed
                          ? "The operator installation failed. Check pod logs for details."
                          : "The operator is installed. OLM reconciliation is complete."
                        }
                        isInline
                        isPlain
                      >
                        <Content component="small" style={{ marginTop: "0.25rem" }}>
                          Some components may still be rolling out new pods. Check the{" "}
                          <Link to="/components">Components page</Link> to verify all deployments are
                          ready and the DSC has finished reconciling.
                        </Content>
                      </Alert>
                    </StackItem>
                  </Stack>
                </CardBody>
              </Card>
            );
          })()}
        </StackItem>
      )}
    </Stack>
  );
};
