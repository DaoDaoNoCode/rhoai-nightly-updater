import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  List,
  ListItem,
  ProgressStepper,
  ProgressStep,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import WrenchIcon from "@patternfly/react-icons/dist/esm/icons/wrench-icon";
import { Link } from "react-router-dom";
import type { StatusResponse, Problem, OperationResponse } from "../types";
import { getDiagnostics, fixProblem } from "../services/api";
import { formatElapsed } from "../utils";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { TooltipButton } from "./TooltipButton";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { TechnicalDetails } from "./LongText";
import { StatusLabel } from "./StatusLabel";
import { PhaseLabel } from "./StatusCards";

export type { ReconcileKind as OperationType } from "../operationSteps";
import type { ReconcileKind as OperationType } from "../operationSteps";

interface ReconciliationProgressProps {
  status: StatusResponse | null;
  active: boolean;
  startTime?: number;
  timedOut?: boolean;
  operationType?: OperationType;
}


type Phase = "initiated" | "waiting" | "installing" | "succeeded" | "failed";

interface Step {
  id: Phase;
  label: string;
  description: string;
}

const STARTED_LABELS: Record<OperationType, string> = {
  update: "Update applied",
  refresh: "Re-deploy applied",
  reinstall: "Reinstall applied",
};

/** OLM's side after the tool's steps: the CSV phases it moves through. */
function buildSteps(operationType: OperationType): Step[] {
  return [
    { id: "initiated", label: STARTED_LABELS[operationType], description: "The tool's steps are done; OLM takes over" },
    { id: "waiting", label: "Resolving", description: "OLM creates an InstallPlan for the new CSV" },
    { id: "installing", label: "Installing", description: "OLM deploys the operator" },
    { id: "succeeded", label: "Operator running", description: "The CSV reached Succeeded" },
  ];
}

const FAILED_STEP: Step = { id: "failed", label: "Failed", description: "OLM reports the CSV as Failed; Diagnostics shows why" };

function phaseToStep(csvPhase: string): Phase {
  switch (csvPhase) {
    case "Succeeded": return "succeeded";
    case "Failed": return "failed";
    case "Installing": return "installing";
    case "Replacing": return "installing";
    case "Pending":
    case "Not Found":
    case "": return "waiting";
    default: return "initiated";
  }
}

function stepVariant(stepId: Phase, currentPhase: Phase, steps: Step[]): "success" | "info" | "pending" | "danger" {
  if (stepId === "failed") return "danger";
  const currentIdx = steps.findIndex(s => s.id === currentPhase);
  const stepIdx = steps.findIndex(s => s.id === stepId);
  if (stepIdx < 0 || currentIdx < 0) return "pending";
  if (stepIdx < currentIdx) return "success";
  if (stepIdx === currentIdx) return currentPhase === "succeeded" ? "success" : "info";
  return "pending";
}

const PHASE_ORDER: Phase[] = ["initiated", "waiting", "installing", "succeeded"];

// --- Stuck Guidance Card ---

interface StuckGuidanceCardProps {
  timedOut: boolean;
  problems: Problem[];
  diagLoading: boolean;
  fixLoading: boolean;
  fixResult: OperationResponse | null;
  /** Why a fix can't run now (permissions, another operation), or null. */
  fixDisabledReason: string | null;
  onFix: (problem: Problem) => void;
}

function severityToAlertVariant(severity: string): "danger" | "warning" | "info" {
  switch (severity) {
    case "critical":
      return "danger";
    case "warning":
      return "warning";
    default:
      return "info";
  }
}

const StuckGuidanceCard: React.FC<StuckGuidanceCardProps> = ({
  timedOut,
  problems,
  diagLoading,
  fixLoading,
  fixResult,
  fixDisabledReason,
  onFix,
}) => {
  const topProblem = problems.length > 0 ? problems[0] : null;

  // The fix result stays above the re-scanned guidance (N8), so a problem
  // that is gone no longer offers its Fix button.
  const resultAlert = fixResult && (
    <Alert component="p" variant={outcomeVariant(fixResult)} title={outcomeTitle(fixResult, "The fix could not run")} isInline isLiveRegion>
      {!fixResult.success && fixResult.message}
      {fixResult.errorCode === "conflict" && " The object changed since the diagnosis. Open Diagnostics to scan again."}
      {fixResult.logs && fixResult.logs.length > 0 && <TechnicalDetails text={fixResult.logs} />}
    </Alert>
  );
  const withResult = (body: React.ReactNode) => (resultAlert ? (
    <Stack hasGutter>
      <StackItem>{resultAlert}</StackItem>
      <StackItem>{body}</StackItem>
    </Stack>
  ) : body);

  if (diagLoading) {
    return withResult(
      <Alert component="p" variant="info" title="Checking for problems..." isInline isPlain customIcon={<Spinner size="sm" aria-label="Diagnosing" />}>
        Running read-only diagnostics on the cluster.
      </Alert>,
    );
  }

  if (!topProblem) {
    return withResult(
      <Alert component="p"
        variant={timedOut ? "warning" : "info"}
        title={timedOut ? "The operator install looks stuck" : "Taking longer than usual"}
        isInline
        isPlain
      >
        Diagnostics found no specific problem; OLM may still be working. <Link to="/diagnostics">Open Diagnostics</Link>
      </Alert>,
    );
  }

  const fixable = !!(topProblem.autoFixable && topProblem.autoFixAction);
  const evidence = (topProblem.evidence ?? []).slice(0, 3);
  return withResult(
    <Alert component="p" variant={severityToAlertVariant(topProblem.severity)} title={topProblem.title} isInline>
      <Stack hasGutter>
        <StackItem>{topProblem.description}</StackItem>
        {evidence.length > 0 && (
          <StackItem>
            <List isPlain>
              {evidence.map((line, i) => (
                <ListItem key={i} className="pf-v6-u-text-break-word">{line}</ListItem>
              ))}
            </List>
          </StackItem>
        )}
        {topProblem.fix && (
          <StackItem>
            <Content component="small"><strong>{fixable ? "Suggested fix:" : "What to do:"}</strong> {topProblem.fix}</Content>
          </StackItem>
        )}
        {fixable && (
          <StackItem>
            <TooltipButton
              variant="secondary"
              icon={<WrenchIcon />}
              onClick={() => onFix(topProblem)}
              isLoading={fixLoading}
              isDisabled={fixLoading}
              disabledReason={fixDisabledReason}
            >
              Apply fix...
            </TooltipButton>
          </StackItem>
        )}
        <StackItem>
          <Link to="/diagnostics">
            {problems.length > 1 ? `All diagnostics (${problems.length} problems)` : "All diagnostics"}
          </Link>
        </StackItem>
      </Stack>
    </Alert>,
  );
};

export const ReconciliationProgress: React.FC<ReconciliationProgressProps> = ({
  status,
  active,
  startTime = 0,
  timedOut = false,
  operationType = "update",
}) => {
  const [elapsed, setElapsed] = useState("");
  const [highestPhase, setHighestPhase] = useState<Phase>("initiated");
  const [seenTransition, setSeenTransition] = useState(false);
  const [showAssist, setShowAssist] = useState(false);
  const prevStartTimeRef = React.useRef(startTime);
  const phaseRef = useRef<Phase>("initiated");

  // Diagnostics state
  const [diagnosticProblems, setDiagnosticProblems] = useState<Problem[]>([]);
  const [diagLoading, setDiagLoading] = useState(false);
  const [diagFetched, setDiagFetched] = useState(false);
  const [fixLoading, setFixLoading] = useState(false);
  const [fixResult, setFixResult] = useState<OperationResponse | null>(null);
  const [fixConfirmProblem, setFixConfirmProblem] = useState<Problem | null>(null);

  // A fix is a cluster change: it needs the permission and a free lock. The
  // reconcile in progress itself is not a reason (that is what is stuck).
  const fixDisabledReason = useMutationBlocker({ ignoreReconcile: true });
  const onResult = useClusterBusyHandler();

  // Reset when a new reconciliation starts (keyed on startTime changing)
  useEffect(() => {
    if (active && startTime > 0 && startTime !== prevStartTimeRef.current) {
      setHighestPhase("initiated");
      setSeenTransition(false);
      setShowAssist(false);
      setDiagnosticProblems([]);
      setDiagFetched(false);
      setFixResult(null);
    }
    prevStartTimeRef.current = startTime;
  }, [active, startTime]);

  // Track the highest phase reached (never goes backwards)
  const rawPhase = status ? phaseToStep(status.csv.phase) : "initiated";
  useEffect(() => {
    if (!active) return;

    if (rawPhase === "failed") {
      setHighestPhase("failed");
      setSeenTransition(true);
      return;
    }

    if (!seenTransition) {
      if (rawPhase !== "succeeded") {
        setSeenTransition(true);
        setHighestPhase(rawPhase);
      }
      return;
    }

    const rawIdx = PHASE_ORDER.indexOf(rawPhase);
    const highIdx = PHASE_ORDER.indexOf(highestPhase);
    if (rawIdx >= 0 && rawIdx > highIdx) {
      setHighestPhase(rawPhase);
    }
  }, [rawPhase, active, highestPhase, seenTransition]);

  useEffect(() => {
    phaseRef.current = highestPhase;
  }, [highestPhase]);

  // 90-second assist timer
  useEffect(() => {
    if (!active || startTime <= 0) return;

    const timer = setTimeout(() => {
      const currentPhase = phaseRef.current;
      if (currentPhase !== "succeeded" && currentPhase !== "failed" && active) {
        setShowAssist(true);
      }
    }, 90_000);

    return () => clearTimeout(timer);
  }, [active, startTime]);

  // Fetch diagnostics when reconciliation appears stuck
  useEffect(() => {
    if ((timedOut || showAssist) && active && !diagLoading && !diagFetched) {
      setDiagLoading(true);
      getDiagnostics()
        .then(result => setDiagnosticProblems(result.problems || []))
        .catch(() => {})
        .finally(() => {
          setDiagLoading(false);
          setDiagFetched(true);
        });
    }
  }, [timedOut, showAssist, active, diagLoading, diagFetched]);

  // Fix action with confirmation modal
  const handleFixConfirm = useCallback(async () => {
    if (!fixConfirmProblem) return;
    const action = fixConfirmProblem.autoFixAction;
    setFixConfirmProblem(null);
    if (!action) return;
    setFixLoading(true);
    setFixResult(null);
    let res: OperationResponse;
    try {
      // Every action, including assist-rollout:<ns>/<deployment> and
      // restore-rollout-strategy:<ns>/<deployment>, goes to the diagnostics
      // fix endpoint as-is (B2 contract); it re-checks before acting.
      res = await fixProblem(action);
    } catch (e) {
      // Keeps errorCode (cluster_busy, conflict...) and the 422 logs (N8).
      res = errorResult(e, "Fix action failed");
    }
    setFixResult(res);
    setFixLoading(false);
    onResult(res);
    // Scan again, so a fixed problem no longer offers its Fix button.
    setDiagFetched(false);
  }, [fixConfirmProblem, onResult]);

  useEffect(() => {
    if (!active || startTime <= 0) {
      setElapsed("");
      return;
    }
    const tick = () => setElapsed(formatElapsed(startTime));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [active, startTime]);

  if (!status) return null;

  const phase = status.csv.phase;
  const csvName = status.csv.name;
  const displayPhase = highestPhase;
  const isFinal = displayPhase === "succeeded" || displayPhase === "failed";

  if (!active && !isFinal) return null;

  const baseSteps = buildSteps(operationType);
  const steps = displayPhase === "failed"
    ? [...baseSteps.slice(0, 3), FAILED_STEP]
    : baseSteps;

  // Compact summary when not active (recently completed)
  if (!active && isFinal) {
    return (
      <Card>
        <CardBody>
          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
            <FlexItem>
              {displayPhase === "succeeded" ? (
                <StatusLabel status="success">Operator running</StatusLabel>
              ) : (
                <StatusLabel status="danger">Operator install failed</StatusLabel>
              )}
            </FlexItem>
            <FlexItem>{csvName}</FlexItem>
            {displayPhase === "failed" && (
              <FlexItem><Link to="/components">View pod details</Link></FlexItem>
            )}
          </Flex>
        </CardBody>
      </Card>
    );
  }

  // Full progress view when active
  return (
    <Card>
      <CardHeader
        actions={{
          actions: (
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              {elapsed && <FlexItem className="pf-v6-u-text-color-subtle">{elapsed} elapsed</FlexItem>}
              <FlexItem><Link to="/components">View pods</Link></FlexItem>
            </Flex>
          ),
          hasNoOffset: true,
        }}
      >
        <CardTitle><Title headingLevel="h2" size="lg">Waiting for the operator</Title></CardTitle>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
              <FlexItem>CSV <strong>{csvName || "pending"}</strong></FlexItem>
              <FlexItem>{phase ? <PhaseLabel phase={phase} /> : <StatusLabel status="neutral">Waiting</StatusLabel>}</FlexItem>
            </Flex>
          </StackItem>
          <StackItem>
            <ProgressStepper isVertical isCompact aria-label="Reconciliation progress">
              {steps.map((step) => {
                const variant = stepVariant(step.id, displayPhase, steps);
                const isCurrent = step.id === displayPhase;
                return (
                  <ProgressStep
                    key={step.id}
                    id={step.id}
                    titleId={`step-${step.id}`}
                    variant={variant}
                    isCurrent={isCurrent}
                    aria-label={step.label}
                    description={step.description}
                  >
                    {step.label}
                  </ProgressStep>
                );
              })}
            </ProgressStepper>
          </StackItem>
          {(showAssist || timedOut) && !isFinal && (
            <StackItem>
              <StuckGuidanceCard
                timedOut={timedOut}
                problems={diagnosticProblems}
                diagLoading={diagLoading}
                fixLoading={fixLoading}
                fixResult={fixResult}
                fixDisabledReason={fixDisabledReason}
                onFix={(problem) => setFixConfirmProblem(problem)}
              />
            </StackItem>
          )}
        </Stack>
      </CardBody>

      <ConfirmActionModal
        isOpen={fixConfirmProblem !== null}
        title={`Apply the fix for "${fixConfirmProblem?.title ?? ""}"?`}
        changes={(fixConfirmProblem?.affectedObjects?.length ?? 0) > 0
          ? fixConfirmProblem!.affectedObjects!.map((obj) => <code key={obj} className="pf-v6-u-text-break-word">{obj}</code>)
          : [fixConfirmProblem?.confirmMessage || fixConfirmProblem?.fix || "Cluster objects involved in the problem."]}
        confirmLabel="Apply fix"
        isLoading={fixLoading}
        confirmDisabled={!!fixDisabledReason}
        onConfirm={handleFixConfirm}
        onCancel={() => setFixConfirmProblem(null)}
      >
        {(fixConfirmProblem?.affectedObjects?.length ?? 0) > 0 && (fixConfirmProblem?.confirmMessage || fixConfirmProblem?.fix) && (
          <Content component="p">{fixConfirmProblem?.confirmMessage || fixConfirmProblem?.fix}</Content>
        )}
        <Content component="p" className="pf-v6-u-text-color-subtle">
          The fix checks the problem again first and changes nothing if it is already gone.
        </Content>
      </ConfirmActionModal>
    </Card>
  );
};
