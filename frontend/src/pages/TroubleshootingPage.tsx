import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  ClipboardCopy,
  Content,
  ExpandableSection,
  Flex,
  FlexItem,
  Grid,
  GridItem,
  Label,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import InfoCircleIcon from "@patternfly/react-icons/dist/esm/icons/info-circle-icon";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import type { CheckResult, OperationResponse, Problem, DiagnosticResult } from "../types";
import { getDiagnostics, fixProblem, toApiError, type ApiError } from "../services/api";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { TooltipButton, NO_PERMISSION_REASON } from "../components/TooltipButton";
import { usePermissions, CHECKING_PERMISSIONS_REASON } from "../hooks/usePermissions";

type ScanState = "idle" | "loading" | "done" | "error";

const statusIcon = (status: CheckResult["status"]) => {
  switch (status) {
    case "pass":
      return <CheckCircleIcon color="var(--pf-t--global--color--status--success--default)" />;
    case "fail":
      return <ExclamationCircleIcon color="var(--pf-t--global--color--status--danger--default)" />;
    case "warn":
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" />;
    default:
      return <InfoCircleIcon color="var(--pf-t--global--color--status--info--default)" />;
  }
};

const statusText = (status: CheckResult["status"]) =>
  status === "pass" ? "Passed" : status === "fail" ? "Failed" : status === "warn" ? "Warning" : "Info";
const statusColor = (status: CheckResult["status"]): "green" | "red" | "yellow" | "blue" =>
  status === "pass" ? "green" : status === "fail" ? "red" : status === "warn" ? "yellow" : "blue";

const severityIcon = (severity: Problem["severity"]) => {
  switch (severity) {
    case "critical":
      return <ExclamationCircleIcon color="var(--pf-t--global--color--status--danger--default)" />;
    case "warning":
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" />;
    default:
      return <InfoCircleIcon color="var(--pf-t--global--color--status--info--default)" />;
  }
};

const severityLabel = (severity: Problem["severity"]) => {
  switch (severity) {
    case "critical":
      return <Label color="red" isCompact>Critical</Label>;
    case "warning":
      return <Label color="yellow" isCompact>Warning</Label>;
    default:
      return <Label color="blue" isCompact>Info</Label>;
  }
};

/** Guidance-only problems carry no autoFixAction: they get instructions, never a Fix button. */
export function hasAutoFix(problem: Problem): boolean {
  return !!problem.autoFixable && !!problem.autoFixAction;
}

export function buildDiagnosticReport(data: DiagnosticResult): string {
  const lines: string[] = [];
  lines.push("=== Diagnostic Report ===");
  lines.push(`Generated: ${new Date().toISOString()}`);
  lines.push("");

  lines.push("--- Health Checks ---");
  for (const check of (data.checks ?? [])) {
    const icon = check.status === "pass" ? "[OK]" : check.status === "fail" ? "[FAIL]" : check.status === "warn" ? "[WARN]" : "[INFO]";
    lines.push(`${icon} ${check.name}: ${check.detail}`);
  }

  const reportProblems = data.problems ?? [];
  if (reportProblems.length > 0) {
    lines.push("");
    lines.push("--- Problems ---");
    for (const problem of reportProblems) {
      lines.push("");
      lines.push(`[${problem.severity.toUpperCase()}] ${problem.title}`);
      lines.push(`  ${problem.description}`);
      if (problem.evidence && problem.evidence.length > 0) {
        lines.push("  Evidence:");
        for (const e of problem.evidence) lines.push(`    - ${e}`);
      }
      if (problem.affectedObjects && problem.affectedObjects.length > 0) {
        lines.push(`  Objects: ${problem.affectedObjects.join(", ")}`);
      }
      lines.push(`  Fix: ${problem.fix ?? ""}${hasAutoFix(problem) ? " (automatic fix available)" : " (manual)"}`);
      if (problem.technicalCmd) lines.push(`  Command: $ ${problem.technicalCmd}`);
    }
  } else {
    lines.push("");
    lines.push("No problems detected.");
  }

  return lines.join("\n");
}

const ObjectList: React.FC<{ objects: string[] }> = ({ objects }) => (
  <List>
    {objects.map((o) => <ListItem key={o}><code style={{ overflowWrap: "anywhere" }}>{o}</code></ListItem>)}
  </List>
);

export const TroubleshootingPage: React.FC = () => {
  const [scanState, setScanState] = useState<ScanState>("idle");
  const [data, setData] = useState<DiagnosticResult | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [fixingId, setFixingId] = useState<string | null>(null);
  const [fixResult, setFixResult] = useState<OperationResponse | null>(null);
  const [copied, setCopied] = useState(false);
  const [confirmFix, setConfirmFix] = useState<Problem | null>(null);
  const { canMutate, loaded: permissionsLoaded } = usePermissions();
  const mountedRef = useRef(true);

  const runScan = useCallback(async () => {
    setScanState("loading");
    setError(null);
    try {
      const result = await getDiagnostics();
      if (!mountedRef.current) return;
      setData(result);
      setScanState("done");
    } catch (err) {
      if (!mountedRef.current) return;
      setError(toApiError(err, "Diagnostics scan failed"));
      setScanState("error");
    }
  }, []);

  useEffect(() => {
    // The page view is counted once, by the router in App.
    document.title = "Diagnostics — RHOAI Nightly Updater";
    mountedRef.current = true;
    runScan();
    return () => {
      mountedRef.current = false;
    };
  }, [runScan]);

  const handleFix = async (autoFixAction: string) => {
    setFixingId(autoFixAction);
    setFixResult(null);
    let result: OperationResponse;
    try {
      result = await fixProblem(autoFixAction);
    } catch (err) {
      result = errorResult(err, "Fix failed");
    }
    if (!mountedRef.current) return;
    setFixResult(result);
    setFixingId(null);
    // Re-scan to show the state after the fix (or after someone else's change).
    await runScan();
  };

  const handleCopyReport = () => {
    if (!data) return;
    navigator.clipboard.writeText(buildDiagnosticReport(data)).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => { /* clipboard may be blocked */ });
  };

  const checks = data?.checks ?? [];
  const problems = data?.problems ?? [];
  const failCount = checks.filter((c) => c.status === "fail").length;
  const warnCount = checks.filter((c) => c.status === "warn").length;
  const passCount = checks.filter((c) => c.status === "pass").length;
  const totalChecks = checks.length;
  const fixReason = !permissionsLoaded ? CHECKING_PERMISSIONS_REASON : !canMutate ? NO_PERMISSION_REASON : null;

  return (
    <>
      <PageSection>
        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
          <FlexItem>
            <Title headingLevel="h1" size="xl" style={{ marginBottom: "0.25rem" }}>
              Diagnostics
            </Title>
            <Content component="p">
              Live health checks of the RHOAI install, with the cause of each problem and how to fix it
            </Content>
          </FlexItem>
          <FlexItem>
            <Flex>
              <FlexItem>
                <Button variant="secondary" icon={<SyncAltIcon />} onClick={runScan} isDisabled={scanState === "loading"} isLoading={scanState === "loading"}>
                  Re-scan
                </Button>
              </FlexItem>
              {data && (
                <FlexItem>
                  <Button variant="secondary" onClick={handleCopyReport}>
                    {copied ? "Copied!" : "Copy diagnostic report"}
                  </Button>
                  <span className="pf-v6-screen-reader" role="status">{copied ? "Diagnostic report copied to the clipboard" : ""}</span>
                </FlexItem>
              )}
            </Flex>
          </FlexItem>
        </Flex>
      </PageSection>

      {scanState === "loading" && !data && (
        <PageSection>
          <Bullseye>
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              <FlexItem><Spinner size="xl" aria-label="Scanning cluster health" /></FlexItem>
              <FlexItem><Content component="p">Scanning cluster health...</Content></FlexItem>
            </Flex>
          </Bullseye>
        </PageSection>
      )}

      {scanState === "error" && error && (
        <LoadErrorAlert error={error} genericTitle="The diagnostic scan failed" onRetry={runScan} stale={!!data} />
      )}

      {data && (
        <>
          <PageSection aria-live="polite">
            {failCount === 0 && warnCount === 0 && (
              <Alert component="p" variant="success" title={`All ${totalChecks} checks passed, no issues detected`} isInline />
            )}
            {failCount === 0 && warnCount > 0 && (
              <Alert component="p" variant="warning" title={`${warnCount} warning${warnCount !== 1 ? "s" : ""} found`} isInline />
            )}
            {failCount > 0 && (
              <Alert component="p" variant="danger" title={`${failCount + warnCount} issue${failCount + warnCount !== 1 ? "s" : ""} found that need attention`} isInline>
                {passCount > 0 && <Content component="p">{passCount} of {totalChecks} checks passed.</Content>}
                {problems.length > 0 && <Content component="p">The problems below say what was observed and how to fix it.</Content>}
              </Alert>
            )}
          </PageSection>

          {fixResult && (
            <PageSection>
              <Alert variant={outcomeVariant(fixResult)} title={outcomeTitle(fixResult, "Fix failed")} isInline isLiveRegion component="p"
                actionClose={<AlertActionCloseButton onClose={() => setFixResult(null)} />}>
                {fixResult.success ? undefined : fixResult.message}
                {fixResult.errorCode === "conflict" ? " The scan was refreshed; check the problem again before retrying." : ""}
                {(fixResult.logs?.length ?? 0) > 0 && (
                  <List>{fixResult.logs!.map((l, i) => <ListItem key={i}>{l}</ListItem>)}</List>
                )}
              </Alert>
            </PageSection>
          )}

          {problems.length > 0 && (
            <PageSection>
              <Title headingLevel="h2" size="lg" style={{ marginBottom: "1rem" }}>
                Problems ({problems.length})
              </Title>
              <Stack hasGutter>
                {problems.map((problem) => {
                  const auto = hasAutoFix(problem);
                  const objects = problem.affectedObjects ?? [];
                  return (
                    <StackItem key={problem.id}>
                      <Card>
                        <CardTitle>
                          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                            <FlexItem>{severityIcon(problem.severity)}</FlexItem>
                            <FlexItem flex={{ default: "flex_1" }} style={{ minWidth: 0 }}>
                              <Title headingLevel="h3" size="md" style={{ margin: 0, overflowWrap: "anywhere" }}>
                                {problem.title}
                              </Title>
                            </FlexItem>
                            <FlexItem>
                              <Flex gap={{ default: "gapXs" }} flexWrap={{ default: "wrap" }} justifyContent={{ default: "justifyContentFlexEnd" }}>
                                <FlexItem>{auto ? <Label isCompact color="blue">Automatic fix</Label> : <Label isCompact variant="outline">Manual fix</Label>}</FlexItem>
                                <FlexItem>{severityLabel(problem.severity)}</FlexItem>
                              </Flex>
                            </FlexItem>
                          </Flex>
                        </CardTitle>
                        <CardBody>
                          <Stack hasGutter>
                            <StackItem>
                              <Content component="p">{problem.description}</Content>
                            </StackItem>

                            {problem.evidence && problem.evidence.length > 0 && (
                              <StackItem>
                                <Content component="p" style={{ fontWeight: 600 }}>What was observed:</Content>
                                <List>
                                  {problem.evidence.map((e, i) => (
                                    <ListItem key={i}><span style={{ overflowWrap: "anywhere", whiteSpace: "pre-line" }}>{e}</span></ListItem>
                                  ))}
                                </List>
                              </StackItem>
                            )}

                            {objects.length > 0 && (
                              <StackItem>
                                <Content component="p" style={{ fontWeight: 600 }}>Objects:</Content>
                                <ObjectList objects={objects} />
                              </StackItem>
                            )}

                            <StackItem>
                              <Content component="p" style={{ fontWeight: 600 }}>How to fix:</Content>
                              <Content component="p">{problem.fix}</Content>
                            </StackItem>

                            {!auto && problem.technicalCmd && (
                              <StackItem>
                                <Content component="p" style={{ fontWeight: 600 }}>Command:</Content>
                                <ClipboardCopy isReadOnly isCode hoverTip="Copy command" clickTip="Copied" textAriaLabel={`Command for ${problem.title}`}>
                                  {problem.technicalCmd}
                                </ClipboardCopy>
                                <Content component="small">This page changes nothing for this problem. Read the command before you run it with <code>oc</code>.</Content>
                              </StackItem>
                            )}

                            {auto && (
                              <StackItem>
                                <TooltipButton
                                  variant="primary"
                                  onClick={() => setConfirmFix(problem)}
                                  isDisabled={fixingId !== null}
                                  isLoading={fixingId === problem.autoFixAction}
                                  disabledReason={fixReason}
                                >
                                  {fixingId === problem.autoFixAction ? "Fixing..." : "Fix"}
                                </TooltipButton>
                              </StackItem>
                            )}

                            {problem.learnMore && (
                              <StackItem>
                                <ExpandableSection toggleText="Learn more">
                                  <Content component="p" style={{ whiteSpace: "pre-line" }}>{problem.learnMore}</Content>
                                </ExpandableSection>
                              </StackItem>
                            )}

                            {auto && problem.technicalCmd && (
                              <StackItem>
                                <ExpandableSection toggleText="Technical details">
                                  <ClipboardCopy isBlock isReadOnly isCode>{problem.technicalCmd}</ClipboardCopy>
                                </ExpandableSection>
                              </StackItem>
                            )}
                          </Stack>
                        </CardBody>
                      </Card>
                    </StackItem>
                  );
                })}
              </Stack>
            </PageSection>
          )}

          <PageSection>
            <Title headingLevel="h2" size="lg" style={{ marginBottom: "1rem" }}>
              Health checks ({totalChecks})
            </Title>
            <Grid hasGutter>
              {checks.map((check) => (
                <GridItem key={check.name} span={12} md={6}>
                  <Card isCompact>
                    <CardBody>
                      <div style={{ display: "flex", gap: "0.5rem", alignItems: "flex-start" }}>
                        <div style={{ flexShrink: 0, paddingTop: "2px" }}>{statusIcon(check.status)}</div>
                        <div style={{ flex: 1, minWidth: 0 }}>
                          <Content component="p" style={{ fontWeight: 600, margin: 0 }}>{check.name}</Content>
                          <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)", overflowWrap: "anywhere" }}>
                            {check.detail}
                          </Content>
                        </div>
                        <div style={{ flexShrink: 0 }}>
                          <Label color={statusColor(check.status)} isCompact>{statusText(check.status)}</Label>
                        </div>
                      </div>
                    </CardBody>
                  </Card>
                </GridItem>
              ))}
            </Grid>
          </PageSection>

          {scanState === "loading" && (
            <PageSection>
              <Bullseye>
                <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                  <FlexItem><Spinner size="md" aria-label="Re-scanning" /></FlexItem>
                  <FlexItem><Content component="p">Re-scanning...</Content></FlexItem>
                </Flex>
              </Bullseye>
            </PageSection>
          )}
        </>
      )}

      <Modal aria-labelledby="confirm-fix-title" variant={ModalVariant.medium} isOpen={confirmFix !== null} onClose={() => setConfirmFix(null)}>
        <ModalHeader title={`Fix: ${confirmFix?.title ?? ""}`} labelId="confirm-fix-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">{confirmFix?.confirmMessage || "This changes resources on the shared cluster."}</Content>
            </StackItem>
            {(confirmFix?.affectedObjects?.length ?? 0) > 0 && (
              <StackItem>
                <Content component="p">Objects this fix may change:</Content>
                <ObjectList objects={confirmFix!.affectedObjects!} />
              </StackItem>
            )}
            <StackItem>
              <Content component="small">The server checks the problem again first and changes nothing if it is already gone.</Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This changes a shared cluster." isInline isPlain />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => {
              if (confirmFix?.autoFixAction) {
                void handleFix(confirmFix.autoFixAction);
                setConfirmFix(null);
              }
            }}
            isLoading={fixingId !== null}
          >
            Fix
          </Button>
          <Button variant="link" onClick={() => setConfirmFix(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
