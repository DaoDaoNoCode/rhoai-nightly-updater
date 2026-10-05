import React, { useEffect, useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  Content,
  Flex,
  FlexItem,
  Label,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import { Link } from "react-router-dom";
import { UpdatePipeline } from "./UpdatePipeline";
import { ReconciliationProgress } from "./ReconciliationProgress";
import { OPERATION_NAMES } from "../operationSteps";
import { isRunning } from "../state/operation";
import { useClusterStatus, useOperation } from "../state/AppState";

const COMPLETE_TITLES = {
  update: "Update complete",
  refresh: "Refresh complete",
  reinstall: "Reinstall complete",
} as const;

/** True when the progress card has something to show. */
export function useHasOperationProgress(): boolean {
  const { state } = useOperation();
  return !!state.run || state.reconcile.active || state.reconcile.finished;
}

/**
 * Unified progress for the current operation: connecting, streamed steps,
 * then OLM reconciliation and the final result. Reads the app-level
 * operation store, so it shows the same state after navigating away and back.
 */
export const OperationProgress: React.FC = () => {
  const { state } = useOperation();
  const { status } = useClusterStatus();
  const { run, reconcile } = state;
  const running = isRunning(run);

  const [connectingElapsed, setConnectingElapsed] = useState(0);
  const connecting = running && run.steps.length === 0;
  const runStartedAt = run?.startedAt;
  useEffect(() => {
    if (!connecting || !runStartedAt) {
      setConnectingElapsed(0);
      return;
    }
    const tick = () => setConnectingElapsed(Math.floor((Date.now() - runStartedAt) / 1000));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [connecting, runStartedAt]);

  if (running && connecting) {
    return (
      <Card isCompact>
        <CardBody>
          <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem>
              <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                <FlexItem><Spinner size="md" aria-label={`${OPERATION_NAMES[run.kind]} starting`} /></FlexItem>
                <FlexItem>
                  <Content component="p" style={{ fontWeight: 600, margin: 0 }}>
                    {run.kind === "update" ? "Applying update to cluster..." : `${OPERATION_NAMES[run.kind]}: starting...`}
                  </Content>
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
    );
  }

  if (running) {
    return <UpdatePipeline steps={run.steps} active startedAt={run.startedAt} operationType={run.kind} />;
  }

  const outcome = run?.outcome;
  const completedSteps = run ? run.steps.filter((s) => s.status === "success").length : 0;

  return (
    <Stack hasGutter>
      {/* A failed run keeps its step list, with the failed step's details. */}
      {run && outcome?.status === "failed" && run.steps.length > 0 && (
        <StackItem>
          <UpdatePipeline steps={run.steps} active={false} operationType={run.kind} />
        </StackItem>
      )}

      {reconcile.active && run && outcome && outcome.status !== "failed" && run.steps.length > 0 && (
        <StackItem>
          <Card isCompact>
            <CardBody>
              {outcome.status === "detached" ? (
                <Label color="orange" icon={<ExclamationTriangleIcon />} isCompact>
                  Live progress interrupted after {completedSteps} steps; following the operator status instead
                </Label>
              ) : (
                <Label color="green" icon={<CheckCircleIcon />} isCompact>
                  Applied {OPERATION_NAMES[run.kind].toLowerCase()} ({completedSteps} steps completed)
                </Label>
              )}
            </CardBody>
          </Card>
        </StackItem>
      )}

      {reconcile.active && (
        <StackItem>
          <ReconciliationProgress
            status={status}
            active
            startTime={reconcile.startTime}
            timedOut={reconcile.timedOut}
            operationType={reconcile.kind}
          />
        </StackItem>
      )}

      {!reconcile.active && reconcile.finished && (
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
                            <Label color="green" icon={<CheckCircleIcon />}>{COMPLETE_TITLES[reconcile.kind]}</Label>
                          ) : failed ? (
                            <Label color="red" icon={<ExclamationCircleIcon />}>Operator install failed</Label>
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
                        component="p"
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
