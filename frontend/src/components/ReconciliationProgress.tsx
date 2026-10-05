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
  List,
  ListItem,
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
import WrenchIcon from "@patternfly/react-icons/dist/esm/icons/wrench-icon";
import { Link } from "react-router-dom";
import type { StatusResponse, Problem, OperationResponse } from "../types";
import { getDiagnostics, fixProblem, toApiError } from "../services/api";
import { formatElapsed } from "../utils";
import { describeServerOperation, usePermissions } from "../state/AppInfo";
import { useOperation } from "../state/AppState";
import { TooltipButton } from "./TooltipButton";

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
  fixError: string | null;
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

/** A fix result: nothing_to_do is information, not a failure (B2 contract). */
export function fixResultVariant(result: OperationResponse): "success" | "info" | "danger" {
  if (result.success) return "success";
  return result.errorCode === "nothing_to_do" ? "info" : "danger";
}

const StuckGuidanceCard: React.FC<StuckGuidanceCardProps> = ({
  timedOut,
  problems,
  diagLoading,
  fixLoading,
  fixResult,
  fixError,
  fixDisabledReason,
  onFix,
}) => {
  const topProblem = problems.length > 0 ? problems[0] : null;

  if (diagLoading) {
    return (
      <Alert component="p" variant="info" title="Checking for problems..." isInline isPlain customIcon={<Spinner size="sm" aria-label="Diagnosing" />}>
        Running read-only diagnostics on the cluster.
      </Alert>
    );
  }

  if (fixResult) {
    return (
      <Alert component="p" variant={fixResultVariant(fixResult)} title={fixResult.message} isInline isLiveRegion>
        {fixResult.errorCode === "conflict" && "The object changed since the diagnosis. Open Diagnostics to scan again. "}
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
      <Alert component="p" variant="danger" title="The fix could not run" isInline isLiveRegion>
        {fixError}
      </Alert>
    );
  }

  if (!topProblem) {
    return (
      <Alert component="p"
        variant={timedOut ? "warning" : "info"}
        title={timedOut ? "The operator install looks stuck" : "Taking longer than usual"}
        isInline
        isPlain
      >
        Diagnostics found no specific problem; OLM may still be working. <Link to="/diagnostics">Open Diagnostics</Link>
      </Alert>
    );
  }

  const fixable = !!(topProblem.autoFixable && topProblem.autoFixAction);
  const evidence = (topProblem.evidence ?? []).slice(0, 3);
  return (
    <Alert component="p" variant={severityToAlertVariant(topProblem.severity)} title={topProblem.title} isInline>
      <Stack hasGutter>
        <StackItem>{topProblem.description}</StackItem>
        {evidence.length > 0 && (
          <StackItem>
            <List isPlain>
              {evidence.map((line, i) => (
                <ListItem key={i}><Content component="small" style={{ overflowWrap: "anywhere" }}>{line}</Content></ListItem>
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
              size="sm"
            >
              Apply fix...
            </TooltipButton>
          </StackItem>
        )}
        <StackItem>
          <Content component="small">
            <Link to="/diagnostics">
              {problems.length > 1 ? `All diagnostics (${problems.length} problems)` : "All diagnostics"}
            </Link>
          </Content>
        </StackItem>
      </Stack>
    </Alert>
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

  // A fix is a cluster change: it needs the permission and a free lock. The
  // reconcile in progress itself is not a reason (that is what is stuck).
  const permissions = usePermissions();
  const { server } = useOperation();
  const fixDisabledReason = permissions.reason
    ?? (server.inProgress ? (server.operation ? `${describeServerOperation(server.operation, status?.cluster.user)}. Wait for it to finish.` : "Another operation is running.") : null);

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
      // Every action, including assist-rollout:<ns>/<deployment> and
      // restore-rollout-strategy:<ns>/<deployment>, goes to the diagnostics
      // fix endpoint as-is (B2 contract); it re-checks before acting.
      if (!fixConfirmProblem.autoFixAction) return;
      setFixResult(await fixProblem(fixConfirmProblem.autoFixAction));
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
                <Label color="green" icon={<CheckCircleIcon />}>Operator running</Label>
              ) : (
                <Label color="red" icon={<ExclamationCircleIcon />}>Operator install failed</Label>
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
                <Title headingLevel="h3" size="md">Waiting for the operator</Title>
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
                fixDisabledReason={fixDisabledReason}
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
          title={`Apply the fix for "${fixConfirmProblem?.title ?? ""}"?`}
          labelId="confirm-fix-stuck-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                {fixConfirmProblem?.confirmMessage || fixConfirmProblem?.fix || "This changes cluster objects to resolve the problem."}
              </Content>
            </StackItem>
            {(fixConfirmProblem?.affectedObjects?.length ?? 0) > 0 && (
              <StackItem>
                <Content component="p"><strong>Objects it may change</strong></Content>
                <List>
                  {fixConfirmProblem?.affectedObjects?.map((obj) => (
                    <ListItem key={obj}><code style={{ overflowWrap: "anywhere" }}>{obj}</code></ListItem>
                  ))}
                </List>
              </StackItem>
            )}
            <StackItem>
              <Content component="small" className="rhoai-subtle">
                The fix checks the problem again first and changes nothing if it is already gone.
              </Content>
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <TooltipButton
            variant="primary"
            onClick={handleFixConfirm}
            isLoading={fixLoading}
            isDisabled={fixLoading}
            disabledReason={fixDisabledReason}
          >
            Apply fix
          </TooltipButton>
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
