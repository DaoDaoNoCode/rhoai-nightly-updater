import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
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
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import { Link, useNavigate } from "react-router-dom";
import { UpdatePipeline } from "./UpdatePipeline";
import { ReconciliationProgress } from "./ReconciliationProgress";
import { BuildSummary } from "./BuildSummary";
import { OPERATION_NAMES, STEP_SETS, type ReconcileKind } from "../operationSteps";
import { describeOutcomeError } from "../errors";

/** Backend refusals that guarantee nothing changed on the cluster (pkg/cluster operations). */
const NOTHING_CHANGED_CODES = new Set(["dashboard_dev_active", "downgrade_requires_confirmation", "validation", "prerequisites", "catalog_image_pull", "cluster_busy"]);
import { isRunning } from "../state/operation";
import { useClusterStatus, useOperation } from "../state/AppState";

const COMPLETE_TITLES: Record<ReconcileKind, string> = {
  update: "Update complete",
  refresh: "Re-deploy complete",
  reinstall: "Reinstall complete",
};

const FAILED_TITLES: Record<ReconcileKind, string> = {
  update: "The operator did not come up after the update",
  refresh: "The operator did not come up after the re-deploy",
  reinstall: "The operator did not come up after the reinstall",
};

/** True when the progress card has something to show. */
export function useHasOperationProgress(): boolean {
  const { state } = useOperation();
  return !!state.run || state.reconcile.active || state.reconcile.finished;
}

/**
 * Unified progress for the current operation: connecting, streamed steps,
 * then OLM reconciliation and the final result. Reads the app-level
 * operation store, so it shows the same state after navigating away and
 * back, after a reload (reconcile tracking is persisted), and for an
 * operation another tab or teammate started (GET /api/operation).
 */
export const OperationProgress: React.FC = () => {
  const { state, dismissFinished } = useOperation();
  const { status } = useClusterStatus();
  const navigate = useNavigate();
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

  const startedBy = run?.source === "server" ? run.user || "another session" : undefined;
  const [dismissedRun, setDismissedRun] = useState<number | null>(null);

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
                    {OPERATION_NAMES[run.kind]}: starting...
                  </Content>
                  <Content component="small" className="rhoai-subtle">Steps appear as they run.</Content>
                </FlexItem>
              </Flex>
            </FlexItem>
            {connectingElapsed > 0 && <FlexItem><Content component="small">Elapsed: {connectingElapsed}s</Content></FlexItem>}
          </Flex>
        </CardBody>
      </Card>
    );
  }

  if (running) {
    return <UpdatePipeline steps={run.steps} active startedAt={run.startedAt} operationType={run.kind} startedBy={startedBy} />;
  }

  const outcome = run?.outcome;
  const completedSteps = run ? run.steps.filter((s) => s.status === "success").length : 0;
  const csvPhase = status?.csv.phase || "";
  const succeeded = csvPhase === "Succeeded";
  const failed = csvPhase === "Failed";
  const installedBuild = status?.nightly?.installed;

  // A run that failed before its first step finished changed nothing: one
  // alert says so, instead of a pipeline of pending steps.
  const firstStep = run ? STEP_SETS[run.kind][0].id : "";
  const notStarted = !!run && outcome?.status === "failed" && run.steps.every((s) => s.step === firstStep || s.step === "operation_complete");
  const notStartedText = notStarted && outcome?.status === "failed" ? describeOutcomeError(outcome, `${OPERATION_NAMES[run.kind]} did not start`) : null;
  const nothingChanged = outcome?.status === "failed" && (outcome.rejected || NOTHING_CHANGED_CODES.has(outcome.errorCode ?? ""));

  return (
    <Stack hasGutter>
      {notStartedText && run && dismissedRun !== run.id && (
        <StackItem>
          <Alert
            variant={nothingChanged ? "warning" : notStartedText.variant}
            isInline
            component="p"
            title={nothingChanged ? `${OPERATION_NAMES[run.kind]} did not start; nothing was changed` : notStartedText.title}
            actionClose={<AlertActionCloseButton onClose={() => setDismissedRun(run.id)} />}
          >
            {notStartedText.body}{notStartedText.hint && <> {notStartedText.hint}</>}
          </Alert>
        </StackItem>
      )}

      {/* A failed run keeps its step list, with the failed step's details. */}
      {run && outcome?.status === "failed" && !notStarted && run.steps.length > 0 && (
        <StackItem>
          <UpdatePipeline steps={run.steps} active={false} operationType={run.kind} startedBy={startedBy} />
        </StackItem>
      )}

      {reconcile.active && run && outcome && outcome.status !== "failed" && run.steps.length > 0 && (
        <StackItem>
          <Card isCompact>
            <CardBody>
              {outcome.status === "detached" ? (
                <Label color="orange" icon={<ExclamationTriangleIcon />} isCompact>
                  Live progress stopped after {completedSteps} steps; following the operator status instead
                </Label>
              ) : (
                <Label color="green" icon={<CheckCircleIcon />} isCompact>
                  {OPERATION_NAMES[run.kind]} applied ({completedSteps} steps)
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
          <Alert
            variant={failed ? "danger" : succeeded ? "success" : "info"}
            isInline
            component="p"
            title={failed ? FAILED_TITLES[reconcile.kind] : COMPLETE_TITLES[reconcile.kind]}
            actionClose={<AlertActionCloseButton onClose={dismissFinished} />}
            actionLinks={failed ? (
              <AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>
            ) : (
              <AlertActionLink onClick={() => navigate("/components")}>Check components</AlertActionLink>
            )}
          >
            <Stack>
              <StackItem>
                {status?.csv.name ? <>Operator {status.csv.name} is {csvPhase || "installed"}.</> : "The operator is installed."}
                {installedBuild && !failed && (
                  <span> Running <BuildSummary build={installedBuild} inline /></span>
                )}
              </StackItem>
              <StackItem>
                {failed
                  ? "OLM reports the new operator version as Failed. Diagnostics shows the reason; updating to another build usually fixes a broken nightly."
                  : <>Components may still be rolling out new pods; the <Link to="/components">Components page</Link> shows when every deployment is ready.</>}
              </StackItem>
            </Stack>
          </Alert>
        </StackItem>
      )}
    </Stack>
  );
};
