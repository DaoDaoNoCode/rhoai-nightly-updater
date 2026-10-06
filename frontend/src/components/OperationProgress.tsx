import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
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

const UNKNOWN_TITLES: Record<ReconcileKind, string> = {
  update: "Update finished (outcome unknown, see the activity log)",
  refresh: "Re-deploy finished (outcome unknown, see the activity log)",
  reinstall: "Reinstall finished (outcome unknown, see the activity log)",
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
      <Card>
        <CardHeader actions={{ actions: <Spinner size="md" aria-label={`${OPERATION_NAMES[run.kind]} starting`} />, hasNoOffset: true }}>
          <CardTitle><Title headingLevel="h2" size="lg">{OPERATION_NAMES[run.kind]}: starting</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Content component="p" className="pf-v6-u-text-color-subtle">
            Steps appear as they run.{connectingElapsed > 0 ? ` ${connectingElapsed}s elapsed.` : ""}
          </Content>
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

      {/* The server reported the failure (lastCompleted) without a failed step event. */}
      {run && outcome?.status === "failed" && !notStarted && !run.steps.some((s) => s.status === "failed") && (
        <StackItem>
          <Alert variant={nothingChanged ? "warning" : "danger"} isInline component="p" title={`${OPERATION_NAMES[run.kind]} failed`}>
            {outcome.message}
          </Alert>
        </StackItem>
      )}

      {reconcile.active && run && outcome && outcome.status !== "failed" && run.steps.length > 0 && (
        <StackItem>
          {outcome.status === "detached" ? (
            <Alert
              variant="warning"
              isInline
              isPlain
              component="p"
              title={reconcile.awaitingServer
                ? `Live progress stopped after ${completedSteps} steps; waiting for the server to report the end of the operation`
                : `Live progress stopped after ${completedSteps} steps; following the operator status instead`}
            />
          ) : (
            <Alert variant="success" isInline isPlain component="p" title={`${OPERATION_NAMES[run.kind]} applied (${completedSteps} steps)`} />
          )}
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

      {!reconcile.active && reconcile.finished && reconcile.result === "unknown" && !failed && (
        <StackItem>
          <Alert
            variant="info"
            isInline
            component="p"
            title={UNKNOWN_TITLES[reconcile.kind]}
            actionClose={<AlertActionCloseButton onClose={dismissFinished} />}
            actionLinks={<AlertActionLink onClick={() => navigate("/components")}>Check components</AlertActionLink>}
          >
            <Stack>
              <StackItem>
                This tab did not receive the result of the operation, so it cannot say whether it succeeded.
                {" "}{status?.csv.name ? <>Operator {status.csv.name} is {csvPhase || "installed"}.</> : null}
                {reconcile.baseline && !reconcile.sawChange && " No change to the installed operator was detected."}
              </StackItem>
              <StackItem>The activity log on the Status page records whether it succeeded.</StackItem>
            </Stack>
          </Alert>
        </StackItem>
      )}

      {!reconcile.active && reconcile.finished && (reconcile.result !== "unknown" || failed) && (
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
