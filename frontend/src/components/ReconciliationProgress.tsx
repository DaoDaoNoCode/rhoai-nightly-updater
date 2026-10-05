import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  ProgressStepper,
  ProgressStep,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import WrenchIcon from "@patternfly/react-icons/dist/esm/icons/wrench-icon";
import { Link } from "react-router-dom";
import type { StatusResponse, Problem, OperationResponse } from "../types";
import { getDiagnostics, assistRollout, fixProblem, toApiError } from "../services/api";
import { formatElapsed } from "../utils";

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

function buildSteps(operationType: OperationType): Step[] {
  switch (operationType) {
    case "reinstall":
      return [
        { id: "initiated", label: "Full uninstall/reinstall initiated", description: "CSV deleted; OLM will recreate the operator" },
        { id: "waiting", label: "Resolving", description: "OLM is creating an InstallPlan (this may take several minutes for a full reinstall)" },
        { id: "installing", label: "Installing", description: "Operator is being deployed" },
        { id: "succeeded", label: "Complete", description: "Operator is running with updated images" },
      ];
    case "refresh":
      return [
        { id: "initiated", label: "Operator refresh initiated", description: "CSV deleted to trigger image refresh" },
        { id: "waiting", label: "Resolving", description: "OLM is creating an InstallPlan" },
        { id: "installing", label: "Installing", description: "Operator is being deployed" },
        { id: "succeeded", label: "Complete", description: "Operator is running with updated images" },
      ];
    default:
      return [
        { id: "initiated", label: "Operation initiated", description: "Request submitted to the cluster" },
        { id: "waiting", label: "Resolving", description: "OLM is creating an InstallPlan" },
        { id: "installing", label: "Installing", description: "Operator is being deployed" },
        { id: "succeeded", label: "Complete", description: "Operator is running with updated images" },
      ];
  }
}

const FAILED_STEP: Step = { id: "failed", label: "Failed", description: "Check pod logs for details" };

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
  fixError: string | null;
  onFix: (problem: Problem) => void;
}

function severityIcon(severity: string): React.ReactNode {
  switch (severity) {
    case "critical":
      return <ExclamationCircleIcon color="var(--pf-t--global--color--status--danger--default)" />;
    case "warning":
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" />;
    default:
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--info--default)" />;
  }
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
  fixError,
  onFix,
}) => {
  const topProblem = problems.length > 0 ? problems[0] : null;

  if (diagLoading) {
    return (
      <Alert component="p" variant="info" title="Checking for problems..." isInline isPlain>
        <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
          <FlexItem><Spinner size="sm" aria-label="Diagnosing" /></FlexItem>
          <FlexItem>Running diagnostics on the cluster...</FlexItem>
        </Flex>
      </Alert>
    );
  }

  if (fixResult) {
    return (
      <Alert component="p"
        variant={fixResult.success ? "success" : "danger"}
        title={fixResult.message}
        isInline
      >
        {fixResult.logs && fixResult.logs.length > 0 && (
          <Content component="small" style={{ whiteSpace: "pre-wrap", marginTop: "0.25rem" }}>
            {fixResult.logs.join("\n")}
          </Content>
        )}
      </Alert>
    );
  }

  if (fixError) {
    return (
      <Alert component="p" variant="danger" title="Fix action failed" isInline>
        {fixError}
      </Alert>
    );
  }

  if (!topProblem) {
    return (
      <Alert component="p"
        variant={timedOut ? "warning" : "info"}
        title={timedOut ? "Reconciliation appears stuck" : "Taking longer than usual"}
        isInline
        isPlain
      >
        <Stack hasGutter>
          <StackItem>
            Reconciliation is taking longer than expected. The operation may still be in progress.
          </StackItem>
          <StackItem>
            <Link to="/diagnostics">View detailed diagnostics</Link>
          </StackItem>
        </Stack>
      </Alert>
    );
  }

  const alertVariant = severityToAlertVariant(topProblem.severity);
  return (
    <Stack hasGutter>
      <StackItem>
        <Alert component="p" variant={alertVariant} title={topProblem.title} isInline
          customIcon={severityIcon(topProblem.severity)}
        >
          <Stack hasGutter>
            <StackItem>
              <Content component="p" style={{ marginTop: "0.25rem" }}>
                {topProblem.description}
              </Content>
            </StackItem>
            <StackItem>
              {topProblem.autoFixable && topProblem.autoFixAction ? (
                <Button
                  variant="primary"
                  icon={<WrenchIcon />}
                  onClick={() => onFix(topProblem)}
                  isLoading={fixLoading}
                  isDisabled={fixLoading}
                  size="sm"
                >
                  Fix: {topProblem.fix}
                </Button>
              ) : topProblem.fix ? (
                <Content component="small">
                  <strong>How to fix:</strong> {topProblem.fix}
                </Content>
              ) : null}
            </StackItem>
            {problems.length > 1 && (
              <StackItem>
                <Content component="small">
                  <Link to="/diagnostics">
                    View all diagnostics ({problems.length} problem{problems.length !== 1 ? "s" : ""} found)
                  </Link>
                </Content>
              </StackItem>
            )}
            {problems.length === 1 && (
              <StackItem>
                <Content component="small">
                  <Link to="/diagnostics">View all diagnostics</Link>
                </Content>
              </StackItem>
            )}
          </Stack>
        </Alert>
      </StackItem>
    </Stack>
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
  const [fixError, setFixError] = useState<string | null>(null);
  const [fixConfirmProblem, setFixConfirmProblem] = useState<Problem | null>(null);

  // Reset when a new reconciliation starts (keyed on startTime changing)
  useEffect(() => {
    if (active && startTime > 0 && startTime !== prevStartTimeRef.current) {
      setHighestPhase("initiated");
      setSeenTransition(false);
      setShowAssist(false);
      setDiagnosticProblems([]);
      setDiagFetched(false);
      setFixResult(null);
      setFixError(null);
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
    setFixLoading(true);
    setFixError(null);
    setFixResult(null);
    try {
      let res: OperationResponse;
      if (fixConfirmProblem.autoFixAction === "assist-rollout" || fixConfirmProblem.autoFixAction?.includes("rollout")) {
        res = await assistRollout();
      } else if (fixConfirmProblem.autoFixAction) {
        res = await fixProblem(fixConfirmProblem.autoFixAction);
      } else {
        return;
      }
      setFixResult(res);
    } catch (e) {
      setFixError(toApiError(e, "Fix action failed").message);
    } finally {
      setFixLoading(false);
      setFixConfirmProblem(null);
    }
  }, [fixConfirmProblem]);

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
      <Card isCompact>
        <CardBody>
          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
            <FlexItem>
              {displayPhase === "succeeded" ? (
                <Label color="green" icon={<CheckCircleIcon />}>Reconciliation complete</Label>
              ) : (
                <Label color="red" icon={<ExclamationCircleIcon />}>Reconciliation failed</Label>
              )}
            </FlexItem>
            <FlexItem>
              <Content component="small">{csvName}</Content>
            </FlexItem>
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
    <Card isCompact>
      <CardTitle>
        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
              <FlexItem>
                <Title headingLevel="h4" size="md">Reconciliation Progress</Title>
              </FlexItem>
              {!isFinal && <FlexItem><Spinner size="sm" aria-label="Reconciling" /></FlexItem>}
            </Flex>
          </FlexItem>
          <FlexItem>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              {elapsed && <FlexItem><Content component="small">Elapsed: {elapsed}</Content></FlexItem>}
              <FlexItem><Link to="/components">View pods</Link></FlexItem>
            </Flex>
          </FlexItem>
        </Flex>
      </CardTitle>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
              <FlexItem><Content component="small">CSV:</Content></FlexItem>
              <FlexItem><Content component="small"><strong>{csvName || "Pending..."}</strong></Content></FlexItem>
              <FlexItem>
                {phase === "Succeeded" ? (
                  <Label color="green" icon={<CheckCircleIcon />} isCompact>Succeeded</Label>
                ) : phase === "Failed" ? (
                  <Label color="red" icon={<ExclamationCircleIcon />} isCompact>Failed</Label>
                ) : phase ? (
                  <Label color="blue" icon={<Spinner size="sm" aria-label="In progress" />} isCompact>{phase}</Label>
                ) : (
                  <Label color="orange" isCompact>Waiting</Label>
                )}
              </FlexItem>
            </Flex>
          </StackItem>
          <StackItem>
            <ProgressStepper isVertical aria-label="Reconciliation progress">
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
                fixError={fixError}
                onFix={(problem) => setFixConfirmProblem(problem)}
              />
            </StackItem>
          )}
        </Stack>
      </CardBody>

      {/* Fix Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-fix-stuck-title"
        variant={ModalVariant.small}
        isOpen={fixConfirmProblem !== null}
        onClose={() => setFixConfirmProblem(null)}
      >
        <ModalHeader
          title="Confirm: Apply Fix"
          labelId="confirm-fix-stuck-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                {fixConfirmProblem?.confirmMessage || "This will modify resources on the shared cluster to resolve the detected problem."}
              </Content>
            </StackItem>
            <StackItem>
              <Alert component="p"
                variant="warning"
                title="This action will modify resources on the shared cluster."
                isInline
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={handleFixConfirm}
            isLoading={fixLoading}
            isDisabled={fixLoading}
          >
            Confirm
          </Button>
          <Button
            variant="link"
            onClick={() => setFixConfirmProblem(null)}
            isDisabled={fixLoading}
          >
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </Card>
  );
};
