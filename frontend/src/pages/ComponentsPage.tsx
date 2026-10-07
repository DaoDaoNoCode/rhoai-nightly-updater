import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  CodeBlock,
  CodeBlockCode,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  ExpandableSection,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { Link } from "react-router-dom";
import type { OperationResponse } from "../types";
import { fixProblem, repairDSC, getDSCPreview, toApiError } from "../services/api";
import { repairPreview, type DSCRepairMode } from "../components/dscRepair";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { ConfirmActionModal } from "../components/ConfirmActionModal";
import { RelativeTime } from "../components/RelativeTime";
import { InlineItems } from "../components/InlineItems";
import { StatusLabel, type StatusKind } from "../components/StatusLabel";
import { TechnicalDetails, TruncatedText, firstClause } from "../components/LongText";
import { DeploymentsTable } from "../components/DeploymentsTable";
import { DSCEmptyState } from "../components/DSCEmptyState";
import { TooltipButton } from "../components/TooltipButton";
import { useComponentsData } from "../hooks/useComponentsData";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";

/** The status of a DSC component as a status label kind. */
function componentStatus(status: string): StatusKind {
  switch (status) {
    case "Available":
      return "success";
    case "Degraded":
    case "Error":
    case "DeployFailed":
      return "danger";
    case "Progressing":
      return "progress";
    case "Removed":
      return "neutral";
    default:
      return "warning";
  }
}

/** The DSC phase: Ready is success, a failure is danger, anything else needs attention. */
function dscPhaseStatus(phase?: string): StatusKind {
  if (phase === "Ready") return "success";
  if (!phase) return "neutral";
  return /fail|error/i.test(phase) ? "danger" : "warning";
}

/** Machine names as code, comma separated. */
const CodeList: React.FC<{ items: string[] }> = ({ items }) => (
  <div>
    {items.map((item, i) => (
      <React.Fragment key={item}>
        {i > 0 && ", "}
        <code className="pf-v6-u-text-break-word">{item}</code>
      </React.Fragment>
    ))}
  </div>
);

export const ComponentsPage: React.FC = () => {
  // Polled data with git labels merged in by deployment and image.
  const { data, loading, labelsLoading, error, lastRefreshed, refresh: handleRefresh } = useComponentsData();
  // One source for every cluster change: permissions, session and the
  // operation lock. Fixes repair what may be stuck in an operator install,
  // so that install alone does not block them.
  const mutateReason = useMutationBlocker();
  const fixReason = useMutationBlocker({ ignoreReconcile: true });
  const onResult = useClusterBusyHandler();

  // DSC component fix state
  const [fixConfirm, setFixConfirm] = useState<{ action: string; title: string; message: string } | null>(null);
  const [fixLoading, setFixLoading] = useState<string | null>(null);
  const [fixResult, setFixResult] = useState<Record<string, OperationResponse>>({});
  const [othersExpanded, setOthersExpanded] = useState(false);
  const [repairMode, setRepairMode] = useState<DSCRepairMode | null>(null);
  const [repairLoading, setRepairLoading] = useState(false);
  const [repairResult, setRepairResult] = useState<OperationResponse | null>(null);
  const [defaultsPreview, setDefaultsPreview] = useState("");
  const [previewVersion, setPreviewVersion] = useState("");
  const [previewAPIVersion, setPreviewAPIVersion] = useState("");
  const [previewError, setPreviewError] = useState("");
  // Bumped by "Refresh" in the reset dialog, to fetch the defaults preview again.
  const [previewRequest, setPreviewRequest] = useState(0);

  useEffect(() => {
    if (repairMode !== "reset-defaults") return;
    let active = true;
    setDefaultsPreview("");
    setPreviewVersion("");
    setPreviewAPIVersion("");
    setPreviewError("");
    getDSCPreview().then(res => { if (active) { setDefaultsPreview(res.yaml); setPreviewVersion(res.operatorVersion); setPreviewAPIVersion(res.apiVersion); } })
      .catch(err => { if (active) setPreviewError(toApiError(err).message); });
    return () => { active = false; };
  }, [repairMode, previewRequest]);

  const preview = repairMode ? repairPreview(data?.dscCompatibility, repairMode) : null;
  const repairBlocked = (preview?.blocked.length ?? 0) > 0;
  // The reset dialog shows removals from the page's data and defaults fetched when it opened. They must describe
  // the same DSC: a conversion that recovers (or an operator update) in between changes the version or operator.
  const comparedAPIVersion = data?.dscCompatibility?.defaultsAPIVersion || data?.dscAPIVersion || "";
  const comparedOperatorVersion = data?.dscCompatibility?.operatorVersion ?? "";
  const resetStale = repairMode === "reset-defaults" && !!defaultsPreview &&
    (previewAPIVersion !== comparedAPIVersion || previewVersion !== comparedOperatorVersion);
  const shortVersion = (apiVersion: string) => apiVersion.split("/")[1] || apiVersion || "unknown";
  const resetBlocked = repairPreview(data?.dscCompatibility, "reset-defaults").blocked;
  const extraBlocked = repairPreview(data?.dscCompatibility, "remove-extra-components").blocked;

  const handleRepairDSC = async () => {
    if (!repairMode || !data || repairBlocked || resetStale) return;
    setRepairLoading(true);
    setRepairResult(null);
    let result: OperationResponse;
    try {
      // A failed repair is a 422 OperationResponse with errorCode and logs (N7).
      // The API version the user reviewed: the defaults shown for a reset, else the DSC the page compared.
      const reviewedAPIVersion = repairMode === "reset-defaults" ? previewAPIVersion : data.dscAPIVersion;
      result = await repairDSC(data.dscName, repairMode, reviewedAPIVersion, repairMode === "reset-defaults" ? previewVersion
        : repairMode === "remove-extra-components" ? data.dscCompatibility?.operatorVersion : undefined,
        repairMode === "remove-extra-components" ? data.dscCompatibility?.extraComponents : undefined,
        repairMode === "reset-defaults" ? preview?.removals ?? [] : undefined);
    } catch (err) {
      result = errorResult(err, "Could not repair the DataScienceCluster");
    }
    setRepairResult(result);
    setRepairMode(null);
    setRepairLoading(false);
    onResult(result);
    // Success, "nothing to do" and "changed meanwhile" all mean the shown DSC state is stale.
    handleRefresh();
  };

  const handleComponentFix = useCallback(async (action: string) => {
    setFixConfirm(null);
    setFixLoading(action);
    let res: OperationResponse;
    try {
      res = await fixProblem(action);
    } catch (e) {
      res = errorResult(e, "Fix failed");
    }
    setFixResult(prev => ({ ...prev, [action]: res }));
    setFixLoading(null);
    onResult(res);
    if (res.success || res.errorCode === "nothing_to_do") handleRefresh();
  }, [handleRefresh, onResult]);

  useEffect(() => {
    document.title = "Components — RHOAI Nightly Updater";
  }, []);

  // No DSC is a state the backend reports with 200 (dscState), never an error (A05-8, A08-6).
  const dscState = data?.dscState ?? (data?.dscName ? "present" : undefined);
  const noDSC = !!data && (dscState === "no-dsc" || dscState === "no-crd");
  const compat = data?.dscCompatibility;
  const drift = !!compat && (compat.invalidFields.length > 0 || compat.missingComponents.length > 0 || (compat.extraComponents?.length ?? 0) > 0);
  const fallback = data?.dscVersionFallback;
  // The console's resource reference for the version the DSC was read at.
  const [dscGroup, dscVersion] = (data?.dscAPIVersion || "datasciencecluster.opendatahub.io/v2").split("/");

  return (
    <>
      <PageHeader
        title="Components"
        description="The DataScienceCluster and every RHOAI deployment: what is ready, what changed since the last update, and how to fix what is not."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={handleRefresh}
      />

      {error && data && (
        <LoadErrorAlert error={error} genericTitle="Could not load components" onRetry={handleRefresh} stale />
      )}
      {error && !data && <PageErrorState error={error} title="Can't load the components" onRetry={handleRefresh} />}
      {loading && !data && !error && <PageLoading title="Loading the components" />}

      {data && noDSC && dscState && (
        <DSCEmptyState
          dscState={dscState}
          operatorVersion={data.operatorVersion}
          operatorPhase={data.operatorPhase}
          mutateBlocker={mutateReason}
          onResult={onResult}
          onCreated={handleRefresh}
        />
      )}

      {data && !noDSC && (fallback || (compat && (compat.validationError || compat.defaultsError || drift || repairResult))) && (
        <PageSection>
          <Stack hasGutter>
            {fallback && (
              <StackItem>
                <Alert
                  component="p"
                  variant="warning"
                  title={`The DataScienceCluster is shown as ${fallback.used}: the operator cannot convert it to ${fallback.version}`}
                  isInline
                >
                  Reading it as {fallback.version}, the version the cluster prefers, fails in the operator&apos;s conversion webhook, so this
                  page shows the {fallback.used} view. Component names can differ between API versions.{" "}
                  <Link to="/diagnostics">Diagnostics</Link> explains the cause and the fix.
                  <TechnicalDetails text={fallback.message} toggleText="Show the API server's message" />
                </Alert>
              </StackItem>
            )}
            {compat?.validationError && (
              <StackItem>
                <Alert component="p" variant="warning" title="DSC field validation is unavailable" isInline>
                  <TruncatedText>{compat.validationError}</TruncatedText>
                </Alert>
              </StackItem>
            )}
            {compat?.defaultsError && (
              <StackItem>
                <Alert component="p" variant="warning" title="The DSC defaults are unavailable" isInline>
                  <TruncatedText>{compat.defaultsError}</TruncatedText>
                </Alert>
              </StackItem>
            )}
            {compat && drift && (
              <StackItem>
                <Card>
                  <CardHeader>
                    <CardTitle>
                      <Title headingLevel="h2" size="lg">
                        The DataScienceCluster differs from operator {compat.operatorVersion || "defaults"}
                      </Title>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    <Stack hasGutter>
                      <StackItem>
                        <Alert
                          component="p"
                          variant="warning"
                          isInline
                          isPlain
                          title="Some field or component names don't match what the installed operator expects"
                        />
                      </StackItem>
                      <StackItem>
                        <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "16ch" }} aria-label="Differences">
                          {compat.invalidFields.length > 0 && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Invalid fields</DescriptionListTerm>
                              <DescriptionListDescription>
                                <CodeList items={compat.invalidFields} />
                                <span className="pf-v6-u-text-color-subtle">Invalid or deprecated in the installed CRD.</span>
                              </DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          {compat.missingComponents.length > 0 && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Missing</DescriptionListTerm>
                              <DescriptionListDescription>
                                <CodeList items={compat.missingComponents} />
                                <span className="pf-v6-u-text-color-subtle">In the version defaults but not in this DSC; this may be intentional.</span>
                              </DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          {(compat.extraComponents?.length ?? 0) > 0 && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Extra</DescriptionListTerm>
                              <DescriptionListDescription>
                                <CodeList items={compat.extraComponents ?? []} />
                                <span className="pf-v6-u-text-color-subtle">
                                  In this DSC but not in the {compat.branch} defaults. The installed CRD may still accept them; review them before removing their configuration.
                                </span>
                              </DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          <DescriptionListGroup>
                            <DescriptionListTerm>Defaults</DescriptionListTerm>
                            <DescriptionListDescription>
                              {compat.defaultsSource === "csv"
                                ? <>From the alm-examples of the installed operator {compat.operatorVersion}.</>
                                : compat.sourceURL
                                  ? (
                                    <a href={compat.sourceURL} target="_blank" rel="noopener noreferrer">
                                      Defaults for {compat.operatorVersion} ({compat.branch}) <ExternalLinkAltIcon />
                                    </a>
                                  )
                                  : <>{compat.branch || "unknown"}</>}
                            </DescriptionListDescription>
                          </DescriptionListGroup>
                        </DescriptionList>
                      </StackItem>
                      <StackItem>
                        <Flex gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                          {compat.invalidFields.length > 0 && (
                            <FlexItem>
                              <TooltipButton variant="secondary" onClick={() => setRepairMode("remove-invalid")} isDisabled={repairLoading || !!compat.validationError} disabledReason={mutateReason}>
                                Remove invalid fields
                              </TooltipButton>
                            </FlexItem>
                          )}
                          {(compat.extraComponents?.length ?? 0) > 0 && (
                            <FlexItem>
                              <TooltipButton variant="secondary" onClick={() => setRepairMode("remove-extra-components")} isDisabled={repairLoading || !!compat.defaultsError || !!compat.validationError} disabledReason={mutateReason}>
                                Remove extra components
                              </TooltipButton>
                            </FlexItem>
                          )}
                          <FlexItem>
                            <TooltipButton variant="secondary" isDanger onClick={() => setRepairMode("reset-defaults")} isDisabled={repairLoading || !!compat.defaultsError || !!compat.validationError} disabledReason={mutateReason}>
                              Reset to version defaults
                            </TooltipButton>
                          </FlexItem>
                        </Flex>
                      </StackItem>
                      {(resetBlocked.length > 0 || extraBlocked.length > 0) && (
                        <StackItem>
                          <HelperText>
                            {resetBlocked.length > 0 && (
                              <HelperTextItem variant="warning">
                                Reset to version defaults is not possible now: it would remove {resetBlocked.map((b) => b.component).join(", ")}, which must not be removed yet. Open the action to see why.
                              </HelperTextItem>
                            )}
                            {extraBlocked.length > 0 && (
                              <HelperTextItem variant="warning">
                                Remove extra components is not possible now: {extraBlocked.map((b) => b.component).join(", ")} must not be removed yet. Open the action to see why.
                              </HelperTextItem>
                            )}
                          </HelperText>
                        </StackItem>
                      )}
                    </Stack>
                  </CardBody>
                </Card>
              </StackItem>
            )}
            {repairResult && (
              <StackItem>
                <Alert component="p" variant={outcomeVariant(repairResult)} title={outcomeTitle(repairResult, "Could not repair the DataScienceCluster")} isInline isLiveRegion>
                  {repairResult.success ? undefined : repairResult.message}
                  {(repairResult.logs?.length ?? 0) > 0 && <TechnicalDetails text={repairResult.logs!} />}
                </Alert>
              </StackItem>
            )}
          </Stack>
        </PageSection>
      )}

      {/* DSC status card: what the operator reports */}
      {data && !noDSC && (() => {
        const components = data.components;
        const sortByName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);
        const ready = components.filter(c => c.status === "Available").sort(sortByName);
        const notReady = components.filter(c => c.status !== "Available" && c.status !== "Removed").sort(sortByName);
        const removed = components.filter(c => c.status === "Removed").sort(sortByName);
        const reason = data.dscPhase !== "Ready" ? data.dscReason?.trim() : "";
        const reasonSummary = reason ? firstClause(reason) : "";
        const counts = [`${ready.length} ready`, notReady.length > 0 ? `${notReady.length} need attention` : "", removed.length > 0 ? `${removed.length} removed` : ""].filter(Boolean).join(" · ");
        return (
          <PageSection>
            <Card>
              <CardHeader>
                <CardTitle>
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                    <FlexItem>
                      <Title headingLevel="h2" size="lg">DataScienceCluster {data.dscName && <code>{data.dscName}</code>}</Title>
                    </FlexItem>
                    <FlexItem><StatusLabel status={dscPhaseStatus(data.dscPhase)}>{data.dscPhase || "Unknown"}</StatusLabel></FlexItem>
                  </Flex>
                </CardTitle>
              </CardHeader>
              <CardBody>
                <Stack hasGutter>
                  <StackItem>
                    <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                      <FlexItem>
                        <InlineItems className="pf-v6-u-text-color-subtle">{counts}</InlineItems>
                      </FlexItem>
                      {data.consoleURL && data.dscName && (
                        <FlexItem>
                          <Button
                            variant="link"
                            isInline
                            component="a"
                            href={`${data.consoleURL}/k8s/cluster/${dscGroup}~${dscVersion}~DataScienceCluster/${data.dscName}/yaml`}
                            target="_blank"
                            rel="noopener noreferrer"
                            icon={<ExternalLinkAltIcon />}
                            iconPosition="end"
                            aria-label={`Edit ${data.dscName} in the OpenShift console`}
                          >
                            Edit in console
                          </Button>
                        </FlexItem>
                      )}
                    </Flex>
                  </StackItem>
                  {reason && (
                    <StackItem>
                      <Alert component="p" variant="danger" isInline isPlain title={reasonSummary}>
                        {reason !== reasonSummary && <TechnicalDetails text={reason} toggleText="Show the full message" />}
                      </Alert>
                    </StackItem>
                  )}
                  {notReady.length === 0 && (
                    <StackItem>
                      <Alert component="p" variant="success" isInline isPlain title="Every enabled component is ready" />
                    </StackItem>
                  )}
                </Stack>
              </CardBody>
              {notReady.length > 0 && (
                <CardBody className="pf-v6-u-px-0">
                  <Table aria-label="Components that need attention" variant="compact" borders={false}>
                        <Thead>
                          <Tr>
                            <Th width={20}>Component</Th>
                            <Th width={15}>Status</Th>
                            <Th>Message</Th>
                            <Th screenReaderText="Fix" />
                          </Tr>
                        </Thead>
                        <Tbody>
                          {notReady.map((c, rowIndex) => {
                            const res = c.fixAction ? fixResult[c.fixAction] : undefined;
                            return (
                              <Tr key={c.name} isStriped={rowIndex % 2 === 1}>
                                <Td dataLabel="Component"><strong>{c.name}</strong></Td>
                                <Td dataLabel="Status"><StatusLabel status={componentStatus(c.status)}>{c.status}</StatusLabel></Td>
                                <Td dataLabel="Message">
                                  {c.message ? <TruncatedText>{c.message}</TruncatedText> : <span className="pf-v6-u-text-color-subtle">No message</span>}
                                  {c.cause && (
                                    <HelperText className="pf-v6-u-mt-xs">
                                      <HelperTextItem variant="warning">
                                        Cause: {c.cause}. <Link to="/diagnostics">Diagnostics</Link> shows the fix.
                                      </HelperTextItem>
                                    </HelperText>
                                  )}
                                  {res && (
                                    <Alert component="p" isInline isPlain isLiveRegion variant={outcomeVariant(res)} title={outcomeTitle(res, "Fix failed")}>
                                      {res.success ? undefined : res.message}
                                    </Alert>
                                  )}
                                </Td>
                                <Td dataLabel={c.fixAction && c.fixTitle && !res?.success ? "Fix" : undefined} modifier="fitContent">
                                  {c.fixAction && c.fixTitle && !res?.success && (
                                    <TooltipButton
                                      variant="secondary"
                                      isLoading={fixLoading === c.fixAction}
                                      isDisabled={fixLoading !== null}
                                      disabledReason={fixReason}
                                      onClick={() => setFixConfirm({
                                        action: c.fixAction!,
                                        title: c.fixTitle!,
                                        message: c.fixConfirm || c.fixDescription || "This changes the DataScienceCluster on the shared cluster.",
                                      })}
                                    >
                                      {c.fixTitle}
                                    </TooltipButton>
                                  )}
                                </Td>
                              </Tr>
                            );
                          })}
                        </Tbody>
                      </Table>
                </CardBody>
              )}
              {(ready.length > 0 || removed.length > 0) && (
                <CardBody>
                      <ExpandableSection
                        toggleText={`Ready (${ready.length})${removed.length > 0 ? ` and removed (${removed.length})` : ""} components`}
                        isExpanded={othersExpanded}
                        onToggle={(_e, v) => setOthersExpanded(v)}
                      >
                        <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "10ch" }}>
                          {ready.length > 0 && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Ready</DescriptionListTerm>
                              <DescriptionListDescription>{ready.map((c) => c.name).join(", ")}</DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          {removed.length > 0 && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Removed</DescriptionListTerm>
                              <DescriptionListDescription>{removed.map((c) => c.name).join(", ")}</DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                        </DescriptionList>
                      </ExpandableSection>
                </CardBody>
              )}
            </Card>
          </PageSection>
        );
      })()}

      {data && dscState !== "no-crd" && (
        <PageSection isFilled>
          <Card>
            <CardHeader>
              <CardTitle><Title headingLevel="h2" size="lg">Deployments</Title></CardTitle>
            </CardHeader>
            {(data.changedCount ?? 0) > 0 && data.snapshotTime && (
              <CardBody>
                <Content component="p" className="pf-v6-u-text-color-subtle">
                  {data.changedCount} changed since the snapshot taken at the last install or update (<RelativeTime date={data.snapshotTime} size="inherit" />).
                </Content>
              </CardBody>
            )}
            {/* Tables use PF's own cell inset, so their body has no inline padding: rows line up with the title. */}
            <CardBody className="pf-v6-u-px-0">
              <DeploymentsTable
                deployments={data.deployments}
                labelsLoading={labelsLoading}
                consoleURL={data.consoleURL}
                mutateBlocker={fixReason}
                onResult={onResult}
                onRefresh={handleRefresh}
              />
            </CardBody>
          </Card>
        </PageSection>
      )}

      <Modal variant={ModalVariant.medium} isOpen={repairMode !== null} onClose={() => !repairLoading && setRepairMode(null)} aria-labelledby="dsc-repair-title">
        <ModalHeader title={repairMode === "reset-defaults" ? "Reset DSC to version defaults" : repairMode === "remove-extra-components" ? "Remove extra DSC components" : "Remove invalid DSC fields"} labelId="dsc-repair-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem><Content component="p">{repairMode === "reset-defaults"
              ? `Replace the spec of ${data?.dscName} with the defaults for the currently installed operator. This resets management states and custom settings and may enable or disable components.`
              : repairMode === "remove-extra-components"
              ? `Remove the component entries below from ${data?.dscName} because they are absent from the ${data?.dscCompatibility?.branch} defaults. The remaining component settings and management states will be preserved. Removing component configuration may affect running workloads.`
              : `Remove only invalid keys from ${data?.dscName}. Valid settings and management states will be preserved.`}</Content></StackItem>
            {repairMode === "remove-invalid" && <StackItem><List>{data?.dscCompatibility?.invalidFields.map(field => <ListItem key={field}><code>{field}</code></ListItem>)}</List></StackItem>}
            {repairMode === "remove-extra-components" && <StackItem><List>{data?.dscCompatibility?.extraComponents?.map(name => <ListItem key={name}><code>{name}</code></ListItem>)}</List></StackItem>}
            {preview && repairBlocked && <StackItem>
              <Alert component="div" variant="danger" isInline title="This repair is not possible now">
                <Content component="p">
                  It would remove {preview.removals.join(", ")}. The components below must not be removed now, and the
                  repair is refused as a whole while any of them is blocked. Resolve the reasons, refresh this page, and try again.
                </Content>
                <List>{preview.blocked.map((b) => (
                  <ListItem key={b.component}><code>{b.component}</code>: {b.reasons.join(" ")}</ListItem>
                ))}</List>
              </Alert>
            </StackItem>}
            {resetStale && <StackItem>
              <Alert
                component="p"
                variant="warning"
                isInline
                title="The DataScienceCluster changed since this page loaded"
                actionLinks={<AlertActionLink onClick={() => { handleRefresh(); setPreviewRequest((n) => n + 1); }}>Refresh</AlertActionLink>}
              >
                The defaults below are {shortVersion(previewAPIVersion)} for operator {previewVersion || "unknown"}, but the components
                this reset would set to Removed were worked out for {shortVersion(comparedAPIVersion)} and operator{" "}
                {comparedOperatorVersion || "unknown"}. Refresh to review the reset again.
              </Alert>
            </StackItem>}
            {preview && !repairBlocked && repairMode === "reset-defaults" && <StackItem>
              {preview.removals.length > 0
                ? <Alert component="div" variant="warning" isInline title={`${preview.removals.length} enabled ${preview.removals.length === 1 ? "component" : "components"} will be set to Removed`}>
                    <List>{preview.removals.map((name) => <ListItem key={name}><code>{name}</code></ListItem>)}</List>
                  </Alert>
                : <Content component="p">No enabled component is set to Removed.</Content>}
            </StackItem>}
            {repairMode === "reset-defaults" && !repairBlocked && <StackItem>{previewError ? <Alert component="p" variant="danger" title="Could not load defaults" isInline>{previewError}</Alert>
              : defaultsPreview ? <CodeBlock><CodeBlockCode>{defaultsPreview}</CodeBlockCode></CodeBlock> : <Spinner aria-label="Loading DSC defaults" />}</StackItem>}
          </Stack>
        </ModalBody>
        <ModalFooter>
          {repairBlocked ? (
            <Button variant="primary" onClick={() => setRepairMode(null)}>Close</Button>
          ) : (
            <>
              <Button variant={repairMode === "reset-defaults" ? "danger" : "primary"} onClick={handleRepairDSC} isLoading={repairLoading} isDisabled={repairLoading || (repairMode === "reset-defaults" && (!defaultsPreview || resetStale))}>Confirm</Button>
              <Button variant="link" onClick={() => setRepairMode(null)} isDisabled={repairLoading}>Cancel</Button>
            </>
          )}
        </ModalFooter>
      </Modal>

      <ConfirmActionModal
        isOpen={fixConfirm !== null}
        title={`${fixConfirm?.title || "Fix"}?`}
        changes={[<>DataScienceCluster <code>{data?.dscName || "default-dsc"}</code>: {fixConfirm?.message}</>]}
        confirmLabel={fixConfirm?.title || "Confirm"}
        isLoading={fixLoading !== null}
        confirmDisabled={!!fixReason}
        onConfirm={() => fixConfirm && handleComponentFix(fixConfirm.action)}
        onCancel={() => setFixConfirm(null)}
      >
        <Content component="p" className="pf-v6-u-text-color-subtle">
          The server re-checks the component first and changes nothing when it is no longer needed or a prerequisite is missing.
        </Content>
      </ConfirmActionModal>
    </>
  );
};
