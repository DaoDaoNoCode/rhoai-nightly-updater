import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
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
import type {
  CheckResult,
  Problem,
  DiagnosticResult,
} from "../types";
import { getDiagnostics, fixProblem, toApiError } from "../services/api";

type ScanState = "idle" | "loading" | "done" | "error";

const statusIcon = (status: CheckResult["status"]) => {
  switch (status) {
    case "pass":
      return <CheckCircleIcon color="var(--pf-t--global--color--status--success--default)" />;
    case "fail":
      return <ExclamationCircleIcon color="var(--pf-t--global--color--status--danger--default)" />;
    case "warn":
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" />;
  }
};

const severityIcon = (severity: Problem["severity"]) => {
  switch (severity) {
    case "critical":
      return <ExclamationCircleIcon color="var(--pf-t--global--color--status--danger--default)" />;
    case "warning":
      return <ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" />;
    case "info":
      return <InfoCircleIcon color="var(--pf-t--global--color--status--info--default)" />;
  }
};

const severityLabel = (severity: Problem["severity"]) => {
  switch (severity) {
    case "critical":
      return <Label color="red" isCompact>Critical</Label>;
    case "warning":
      return <Label color="yellow" isCompact>Warning</Label>;
    case "info":
      return <Label color="blue" isCompact>Info</Label>;
  }
};

function buildDiagnosticReport(data: DiagnosticResult): string {
  const lines: string[] = [];
  lines.push("=== Diagnostic Report ===");
  lines.push(`Generated: ${new Date().toISOString()}`);
  lines.push("");

  lines.push("--- Health Checks ---");
  for (const check of (data.checks ?? [])) {
    const icon = check.status === "pass" ? "[OK]" : check.status === "fail" ? "[FAIL]" : "[WARN]";
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
        for (const e of problem.evidence) {
          lines.push(`    - ${e}`);
        }
      }
      lines.push(`  Fix: ${problem.fix}`);
      if (problem.technicalCmd) {
        lines.push(`  Command: $ ${problem.technicalCmd}`);
      }
    }
  } else {
    lines.push("");
    lines.push("No problems detected.");
  }

  return lines.join("\n");
}

export const TroubleshootingPage: React.FC = () => {
  const [scanState, setScanState] = useState<ScanState>("idle");
  const [data, setData] = useState<DiagnosticResult | null>(null);
  const [error, setError] = useState<string>("");
  const [fixingId, setFixingId] = useState<string | null>(null);
  const [fixError, setFixError] = useState<string>("");
  const [copied, setCopied] = useState(false);
  const [confirmFix, setConfirmFix] = useState<{ action: string; message: string } | null>(null);
  const mountedRef = useRef(true);

  const runScan = useCallback(async () => {
    setScanState("loading");
    setError("");
    setFixError("");
    try {
      const result = await getDiagnostics();
      if (!mountedRef.current) return;
      setData(result);
      setScanState("done");
    } catch (err) {
      if (!mountedRef.current) return;
      setError(toApiError(err, "Diagnostics scan failed").message);
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
    setFixError("");
    try {
      const result = await fixProblem(autoFixAction);
      if (!mountedRef.current) return;
      if (!result.success) {
        setFixError(result.message);
      }
      // Re-scan to reflect the new state
      await runScan();
    } catch (err) {
      if (!mountedRef.current) return;
      setFixError(toApiError(err, "Fix failed").message);
    } finally {
      if (mountedRef.current) {
        setFixingId(null);
      }
    }
  };

  const handleCopyReport = () => {
    if (!data) return;
    const report = buildDiagnosticReport(data);
    navigator.clipboard.writeText(report).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => { /* clipboard may be blocked */ });
  };

  // Compute summary counts (guard against null checks/problems from backend)
  const checks = data?.checks ?? [];
  const problems = data?.problems ?? [];
  const failCount = checks.filter((c) => c.status === "fail").length;
  const warnCount = checks.filter((c) => c.status === "warn").length;
  const passCount = checks.filter((c) => c.status === "pass").length;
  const totalChecks = checks.length;
  const problemCount = problems.length;

  return (
    <>
      {/* Title section */}
      <PageSection>
        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
          <FlexItem>
            <Title headingLevel="h1" size="xl" style={{ marginBottom: "0.25rem" }}>
              Diagnostics
            </Title>
            <Content component="p">
              Live health checks and problem detection for RHOAI nightly builds
            </Content>
          </FlexItem>
          <FlexItem>
            <Flex>
              <FlexItem>
                <Button
                  variant="secondary"
                  icon={<SyncAltIcon />}
                  onClick={runScan}
                  isDisabled={scanState === "loading"}
                  isLoading={scanState === "loading"}
                >
                  Re-scan
                </Button>
              </FlexItem>
              {data && (
                <FlexItem>
                  <Button
                    variant="secondary"
                    onClick={handleCopyReport}
                  >
                    {copied ? "Copied!" : "Copy diagnostic report"}
                  </Button>
                  <span className="pf-v6-screen-reader" role="status">{copied ? "Diagnostic report copied to the clipboard" : ""}</span>
                </FlexItem>
              )}
            </Flex>
          </FlexItem>
        </Flex>
      </PageSection>

      {/* Loading state */}
      {scanState === "loading" && !data && (
        <PageSection>
          <Bullseye>
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              <FlexItem>
                <Spinner size="xl" aria-label="Scanning cluster health" />
              </FlexItem>
              <FlexItem>
                <Content component="p">Scanning cluster health...</Content>
              </FlexItem>
            </Flex>
          </Bullseye>
        </PageSection>
      )}

      {/* Error state */}
      {scanState === "error" && (
        <PageSection>
          <Alert variant="danger" title="Diagnostic scan failed" isInline isLiveRegion component="p">
            {error}
          </Alert>
        </PageSection>
      )}

      {/* Results */}
      {data && (
        <>
          {/* Summary banner */}
          <PageSection aria-live="polite">
            {failCount === 0 && warnCount === 0 && (
              <Alert component="p" variant="success" title={`All ${totalChecks} checks passed — no issues detected`} isInline />
            )}
            {failCount === 0 && warnCount > 0 && (
              <Alert component="p" variant="warning" title={`${warnCount} warning${warnCount !== 1 ? "s" : ""} found`} isInline />
            )}
            {failCount > 0 && (
              <Alert component="p"
                variant="danger"
                title={`${failCount + warnCount} issue${failCount + warnCount !== 1 ? "s" : ""} found that need attention`}
                isInline
              >
                {passCount > 0 && <Content component="p">{passCount} of {totalChecks} checks passed.</Content>}
                {problemCount > 0 && <Content component="p">See the problems section below for details and fix instructions.</Content>}
              </Alert>
            )}
          </PageSection>

          {/* Health checks grid */}
          <PageSection>
            <Title headingLevel="h3" style={{ marginBottom: "1rem" }}>
              Health checks
            </Title>
            <Grid hasGutter>
              {checks.map((check) => (
                <GridItem key={check.name} span={12} md={6}>
                  <Card isCompact>
                    <CardBody>
                      <div style={{ display: "flex", gap: "0.5rem", alignItems: "flex-start" }}>
                        <div style={{ flexShrink: 0, paddingTop: "2px" }}>{statusIcon(check.status)}</div>
                        <div style={{ flex: 1, minWidth: 0 }}>
                          <Content component="p" style={{ fontWeight: 600, margin: 0 }}>
                            {check.name}
                          </Content>
                          <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)", overflowWrap: "anywhere" }}>
                            {check.detail}
                          </Content>
                        </div>
                        <div style={{ flexShrink: 0 }}>
                          <Label
                            color={check.status === "pass" ? "green" : check.status === "fail" ? "red" : "yellow"}
                            isCompact
                          >
                            {check.status === "pass" ? "Passed" : check.status === "fail" ? "Failed" : "Warning"}
                          </Label>
                        </div>
                      </div>
                    </CardBody>
                  </Card>
                </GridItem>
              ))}
            </Grid>
          </PageSection>

          {/* Fix error */}
          {fixError && (
            <PageSection>
              <Alert variant="danger" title="Fix failed" isInline isLiveRegion component="p">
                {fixError}
              </Alert>
            </PageSection>
          )}

          {/* Problems section */}
          {problems.length > 0 && (
            <PageSection>
              <Title headingLevel="h3" style={{ marginBottom: "1rem" }}>
                Problems ({problems.length})
              </Title>
              <Stack hasGutter>
                {problems.map((problem) => (
                  <StackItem key={problem.id}>
                    <Card>
                      <CardTitle>
                        <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                          <FlexItem>{severityIcon(problem.severity)}</FlexItem>
                          <FlexItem flex={{ default: "flex_1" }}>
                            <Title headingLevel="h4" style={{ margin: 0 }}>
                              {problem.title}
                            </Title>
                          </FlexItem>
                          <FlexItem>{severityLabel(problem.severity)}</FlexItem>
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
                                  <ListItem key={i}>{e}</ListItem>
                                ))}
                              </List>
                            </StackItem>
                          )}

                          <StackItem>
                            <Content component="p" style={{ fontWeight: 600 }}>How to fix:</Content>
                            <Content component="p">{problem.fix}</Content>
                          </StackItem>

                          {problem.autoFixable && problem.autoFixAction && (
                            <StackItem>
                              <Button
                                variant="primary"
                                onClick={() => setConfirmFix({
                                  action: problem.autoFixAction!,
                                  message: problem.confirmMessage || "This will modify resources on the shared cluster.",
                                })}
                                isDisabled={fixingId !== null}
                                isLoading={fixingId === problem.autoFixAction}
                              >
                                {fixingId === problem.autoFixAction ? "Fixing..." : "Fix"}
                              </Button>
                            </StackItem>
                          )}

                          {problem.learnMore && (
                            <StackItem>
                              <ExpandableSection toggleText="Learn more">
                                <Content component="p" style={{ whiteSpace: "pre-line" }}>
                                  {problem.learnMore}
                                </Content>
                              </ExpandableSection>
                            </StackItem>
                          )}

                          {problem.technicalCmd && (
                            <StackItem>
                              <ExpandableSection toggleText="Technical details">
                                <Stack hasGutter>
                                  <StackItem>
                                    <ClipboardCopy isBlock isReadOnly>
                                      {problem.technicalCmd}
                                    </ClipboardCopy>
                                  </StackItem>
                                </Stack>
                              </ExpandableSection>
                            </StackItem>
                          )}
                        </Stack>
                      </CardBody>
                    </Card>
                  </StackItem>
                ))}
              </Stack>
            </PageSection>
          )}

          {/* Scanning overlay for re-scan */}
          {scanState === "loading" && data && (
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

      {/* Confirmation modal for fix actions */}
      <Modal
        aria-labelledby="confirm-fix-title"
        variant={ModalVariant.small}
        isOpen={confirmFix !== null}
        onClose={() => setConfirmFix(null)}
      >
        <ModalHeader title="Confirm action" labelId="confirm-fix-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">{confirmFix?.message}</Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This action will modify resources on the shared cluster." isInline isPlain />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => {
              if (confirmFix) {
                handleFix(confirmFix.action);
                setConfirmFix(null);
              }
            }}
            isLoading={fixingId !== null}
          >
            Confirm
          </Button>
          <Button variant="link" onClick={() => setConfirmFix(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
