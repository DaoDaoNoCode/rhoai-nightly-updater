import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  ActionListItem,
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardExpandableContent,
  CardHeader,
  CardTitle,
  ClipboardCopy,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  ExpandableSection,
  Flex,
  FlexItem,
  List,
  ListItem,
  PageSection,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import CopyIcon from "@patternfly/react-icons/dist/esm/icons/copy-icon";
import type { OperationResponse, Problem, DiagnosticResult } from "../types";
import { getDiagnostics, fixProblem, toApiError, type ApiError } from "../services/api";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { withImageRefs } from "../components/ImageRef";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { ConfirmActionModal } from "../components/ConfirmActionModal";
import { StatusLabel, TagLabel, type StatusKind } from "../components/StatusLabel";
import { TechnicalDetails, TruncatedText } from "../components/LongText";
import { TooltipButton } from "../components/TooltipButton";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";

type ScanState = "idle" | "loading" | "done" | "error";

const CHECK_STATUS: Record<string, { kind: StatusKind; text: string; order: number }> = {
  fail: { kind: "danger", text: "Failed", order: 0 },
  warn: { kind: "warning", text: "Warning", order: 1 },
  info: { kind: "info", text: "Info", order: 2 },
  pass: { kind: "success", text: "Passed", order: 3 },
};

const SEVERITY: Record<string, { kind: StatusKind; text: string }> = {
  critical: { kind: "danger", text: "Critical" },
  warning: { kind: "warning", text: "Warning" },
  info: { kind: "info", text: "Info" },
};

/**
 * The Diagnostics summary, counted from the same problem list the page shows:
 * "4 problems need attention · 2 informational". Critical and warning
 * problems need attention; info problems do not.
 */
export function problemSummary(problems: Problem[]): { title: string; variant: "danger" | "warning" | "info" } {
  const attention = problems.filter((p) => p.severity !== "info");
  const info = problems.length - attention.length;
  const infoText = info > 0 ? ` · ${info} informational` : "";
  if (attention.length > 0) {
    const plural = attention.length !== 1;
    return {
      title: `${attention.length} problem${plural ? "s" : ""} need${plural ? "" : "s"} attention${infoText}`,
      variant: attention.some((p) => p.severity === "critical") ? "danger" : "warning",
    };
  }
  if (info > 0) return { title: `No problems need attention${infoText}`, variant: "info" };
  // Failed or warning checks without a problem entry: the checks list shows them.
  return { title: "Some health checks did not pass", variant: "warning" };
}

/** Guidance-only problems carry no autoFixAction: they get instructions, never a Fix button. */
export function hasAutoFix(problem: Problem): boolean {
  return !!problem.autoFixable && !!problem.autoFixAction;
}

/** The problems of the report that fix this problem's cause, in the order the backend gave. */
export function relatedProblems(problem: Problem, all: Problem[]): Problem[] {
  return (problem.relatedProblems ?? [])
    .map((id) => all.find((p) => p.id === id))
    .filter((p): p is Problem => !!p && p.id !== problem.id);
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
      const related = relatedProblems(problem, reportProblems);
      if (related.length > 0) lines.push(`  Related: ${related.map((r) => r.title).join("; ")}`);
      if (problem.technicalCmd) lines.push(`  Command: $ ${problem.technicalCmd}`);
    }
  } else {
    lines.push("");
    lines.push("No problems detected.");
  }

  return lines.join("\n");
}

const ObjectList: React.FC<{ objects: string[] }> = ({ objects }) => (
  <List isPlain>
    {objects.map((o) => <ListItem key={o}><code className="pf-v6-u-text-break-word">{o}</code></ListItem>)}
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
  const [lastScanned, setLastScanned] = useState<Date | null>(null);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  // Permissions, session and the operation lock. An operator install in
  // progress alone does not block a fix: that may be what is stuck.
  const fixReason = useMutationBlocker({ ignoreReconcile: true });
  const onResult = useClusterBusyHandler();
  const mountedRef = useRef(true);

  const runScan = useCallback(async () => {
    setScanState("loading");
    setError(null);
    try {
      const result = await getDiagnostics();
      if (!mountedRef.current) return;
      setData(result);
      setLastScanned(new Date());
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
    onResult(result);
    // Re-scan to show the state after the fix (or after someone else's change).
    await runScan();
  };

  // Opens a related problem's card and moves focus to it.
  const showProblem = (id: string) => {
    setExpanded((prev) => ({ ...prev, [id]: true }));
    window.requestAnimationFrame(() => {
      const toggle = document.getElementById(`problem-${id}-toggle`);
      // Optional call: not every environment implements scrollIntoView (jsdom does not).
      toggle?.scrollIntoView?.({ block: "start" });
      toggle?.focus();
    });
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
  // The summary counts the same problems the list shows, split by severity.
  const summary = problemSummary(problems);
  const groups = [
    { id: "attention", title: "Need attention", items: problems.filter((p) => p.severity !== "info") },
    { id: "info", title: "Informational", items: problems.filter((p) => p.severity === "info") },
  ].filter((g) => g.items.length > 0);

  return (
    <>
      <PageHeader
        title="Diagnostics"
        description="Live health checks of the RHOAI install, with the cause of each problem and how to fix it."
        lastRefreshed={lastScanned}
        loading={scanState === "loading"}
        onRefresh={runScan}
        refreshText="Re-scan"
        actions={data && (
          <ActionListItem>
            <Button variant="secondary" icon={<CopyIcon />} onClick={handleCopyReport}>
              {copied ? "Copied" : "Copy diagnostic report"}
            </Button>
            <span className="pf-v6-screen-reader" role="status">{copied ? "Diagnostic report copied to the clipboard" : ""}</span>
          </ActionListItem>
        )}
      />

      {scanState === "loading" && !data && <PageLoading title="Scanning the cluster health" />}

      {scanState === "error" && error && data && (
        <LoadErrorAlert error={error} genericTitle="The diagnostic scan failed" onRetry={runScan} stale />
      )}
      {scanState === "error" && error && !data && (
        <PageErrorState error={error} title="The diagnostic scan failed" onRetry={runScan} />
      )}

      {data && (
        <PageSection isFilled aria-live="polite">
          <Stack hasGutter>
            <StackItem>
              {problems.length === 0 && failCount === 0 && warnCount === 0 ? (
                <Alert component="p" variant="success" title={`All ${totalChecks} checks passed, no issues detected`} isInline>
                  The scan only reads the cluster.
                </Alert>
              ) : (
                <Alert component="p" variant={summary.variant} title={summary.title} isInline>
                  {totalChecks > 0 && <>{passCount} of {totalChecks} health checks passed{failCount + warnCount > 0 ? ` (${failCount} failed, ${warnCount} with warnings)` : ""}. </>}
                  The scan only reads the cluster: fixes run only after you confirm them, and commands are for you to read and run with <code>oc</code>.
                </Alert>
              )}
            </StackItem>

            {fixResult && (
              <StackItem>
                <Alert variant={outcomeVariant(fixResult)} title={outcomeTitle(fixResult, "Fix failed")} isInline isLiveRegion component="p"
                  actionClose={<AlertActionCloseButton onClose={() => setFixResult(null)} />}>
                  {fixResult.success ? undefined : fixResult.message}
                  {fixResult.errorCode === "conflict" ? " The scan was refreshed; check the problem again before retrying." : ""}
                  {(fixResult.logs?.length ?? 0) > 0 && <TechnicalDetails text={fixResult.logs!} />}
                </Alert>
              </StackItem>
            )}

            {groups.map((group) => (
              <StackItem key={group.id}>
                <Stack hasGutter>
                  <StackItem>
                    <Title headingLevel="h2" size="lg">{group.title} ({group.items.length})</Title>
                  </StackItem>
                  {group.items.map((problem) => {
                    const auto = hasAutoFix(problem);
                    const objects = problem.affectedObjects ?? [];
                    const related = relatedProblems(problem, problems);
                    const isOpen = !!expanded[problem.id];
                    const severity = SEVERITY[problem.severity] ?? SEVERITY.info;
                    const toggleId = `problem-${problem.id}-toggle`;
                    const titleId = `problem-${problem.id}-title`;
                    return (
                      <StackItem key={problem.id}>
                        <Card isExpanded={isOpen}>
                          <CardHeader
                            onExpand={() => setExpanded((prev) => ({ ...prev, [problem.id]: !prev[problem.id] }))}
                            toggleButtonProps={{ id: toggleId, "aria-label": "Details", "aria-labelledby": `${toggleId} ${titleId}`, "aria-expanded": isOpen }}
                            actions={{
                              actions: (
                                <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                                  {auto && <FlexItem><TagLabel color="blue">Auto-fix available</TagLabel></FlexItem>}
                                  <FlexItem><StatusLabel status={severity.kind}>{severity.text}</StatusLabel></FlexItem>
                                </Flex>
                              ),
                              hasNoOffset: true,
                            }}
                          >
                            <CardTitle><Title headingLevel="h3" size="md" id={titleId} className="pf-v6-u-text-break-word">{problem.title}</Title></CardTitle>
                          </CardHeader>
                          <CardExpandableContent>
                            <CardBody>
                              <Stack hasGutter>
                                <StackItem>
                                  <Content component="p">{problem.description}</Content>
                                </StackItem>
                                <StackItem>
                                  <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label={`Details of ${problem.title}`}>
                                    {problem.evidence && problem.evidence.length > 0 && (
                                      <DescriptionListGroup>
                                        <DescriptionListTerm>Observed</DescriptionListTerm>
                                        <DescriptionListDescription>
                                          <TruncatedText lines={3}>
                                            <List isPlain>
                                              {problem.evidence.map((e, i) => <ListItem key={i} className="pf-v6-u-text-break-word">{withImageRefs(e)}</ListItem>)}
                                            </List>
                                          </TruncatedText>
                                        </DescriptionListDescription>
                                      </DescriptionListGroup>
                                    )}
                                    {objects.length > 0 && (
                                      <DescriptionListGroup>
                                        <DescriptionListTerm>Objects</DescriptionListTerm>
                                        <DescriptionListDescription><ObjectList objects={objects} /></DescriptionListDescription>
                                      </DescriptionListGroup>
                                    )}
                                    <DescriptionListGroup>
                                      <DescriptionListTerm>Fix</DescriptionListTerm>
                                      <DescriptionListDescription>{problem.fix}{!auto && " This page does not change anything for this problem."}</DescriptionListDescription>
                                    </DescriptionListGroup>
                                    {related.length > 0 && (
                                      <DescriptionListGroup>
                                        <DescriptionListTerm>Related</DescriptionListTerm>
                                        <DescriptionListDescription>
                                          <List isPlain>
                                            {related.map((r) => (
                                              <ListItem key={r.id}>
                                                <Button variant="link" isInline className="pf-v6-u-text-align-left" onClick={() => showProblem(r.id)}>{r.title}</Button>
                                              </ListItem>
                                            ))}
                                          </List>
                                        </DescriptionListDescription>
                                      </DescriptionListGroup>
                                    )}
                                    {problem.technicalCmd && (
                                      <DescriptionListGroup>
                                        <DescriptionListTerm>Command</DescriptionListTerm>
                                        <DescriptionListDescription>
                                          <ClipboardCopy isReadOnly isCode variant="expansion" hoverTip="Copy command" clickTip="Copied" textAriaLabel={`Command for ${problem.title}`}>
                                            {problem.technicalCmd}
                                          </ClipboardCopy>
                                        </DescriptionListDescription>
                                      </DescriptionListGroup>
                                    )}
                                  </DescriptionList>
                                </StackItem>
                                {auto && (
                                  <StackItem>
                                    <TooltipButton
                                      variant="secondary"
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
                                      <Content component="p" className="pf-v6-u-text-break-word">{problem.learnMore}</Content>
                                    </ExpandableSection>
                                  </StackItem>
                                )}
                              </Stack>
                            </CardBody>
                          </CardExpandableContent>
                        </Card>
                      </StackItem>
                    );
                  })}
                </Stack>
              </StackItem>
            ))}

            <StackItem>
              <Card>
                <CardHeader><CardTitle><Title headingLevel="h2" size="lg">Health checks ({totalChecks})</Title></CardTitle></CardHeader>
                <CardBody className="pf-v6-u-px-0">
                  <Table aria-label="Health checks" variant="compact" borders={false}>
                    <Thead>
                      <Tr>
                        <Th width={25}>Check</Th>
                        <Th modifier="fitContent">Result</Th>
                        <Th>Detail</Th>
                      </Tr>
                    </Thead>
                    <Tbody>
                      {[...checks]
                        .sort((a, b) => (CHECK_STATUS[a.status]?.order ?? 2) - (CHECK_STATUS[b.status]?.order ?? 2))
                        .map((check, rowIndex) => {
                          const st = CHECK_STATUS[check.status] ?? CHECK_STATUS.info;
                          return (
                            <Tr key={check.name} isStriped={rowIndex % 2 === 1}>
                              <Td dataLabel="Check"><strong>{check.name}</strong></Td>
                              <Td dataLabel="Result" modifier="fitContent"><StatusLabel status={st.kind}>{st.text}</StatusLabel></Td>
                              <Td dataLabel="Detail"><TruncatedText>{withImageRefs(check.detail)}</TruncatedText></Td>
                            </Tr>
                          );
                        })}
                    </Tbody>
                  </Table>
                </CardBody>
              </Card>
            </StackItem>
          </Stack>
        </PageSection>
      )}

      <ConfirmActionModal
        isOpen={confirmFix !== null}
        title={`Fix "${confirmFix?.title ?? ""}"?`}
        changes={(confirmFix?.affectedObjects?.length ?? 0) > 0
          ? confirmFix!.affectedObjects!.map((obj) => <code key={obj} className="pf-v6-u-text-break-word">{obj}</code>)
          : [confirmFix?.confirmMessage || "Resources on the shared cluster involved in this problem."]}
        confirmLabel="Fix"
        isLoading={fixingId !== null}
        confirmDisabled={!!fixReason}
        onConfirm={() => {
          if (confirmFix?.autoFixAction) {
            void handleFix(confirmFix.autoFixAction);
            setConfirmFix(null);
          }
        }}
        onCancel={() => setConfirmFix(null)}
      >
        {(confirmFix?.affectedObjects?.length ?? 0) > 0 && confirmFix?.confirmMessage && <Content component="p">{confirmFix.confirmMessage}</Content>}
        <Content component="p" className="pf-v6-u-text-color-subtle">The server checks the problem again first and changes nothing if it is already gone.</Content>
      </ConfirmActionModal>
    </>
  );
};
