import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  CardTitle,
  CodeBlock,
  CodeBlockCode,
  Content,
  Label,
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
  Flex,
  FlexItem,
  Button,
  Bullseye,
  ExpandableSection,
  List,
  ListItem,
} from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import type { OperationResponse } from "../types";
import { fixProblem, repairDSC, getDSCPreview, toApiError } from "../services/api";
import { formatRelativeTime } from "../utils";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { DeploymentsTable } from "../components/DeploymentsTable";
import { DSCEmptyState } from "../components/DSCEmptyState";
import { TooltipButton, NO_PERMISSION_REASON } from "../components/TooltipButton";
import { useComponentsData } from "../hooks/useComponentsData";
import { usePermissions, CHECKING_PERMISSIONS_REASON } from "../hooks/usePermissions";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";

function statusColor(
  status: string,
): "green" | "red" | "blue" | "grey" | "orange" {
  switch (status) {
    case "Available":
      return "green";
    case "Degraded":
    case "Error":
    case "DeployFailed":
      return "red";
    case "Deleting":
      return "orange";
    case "Progressing":
      return "blue";
    case "Removed":
      return "grey";
    default:
      return "orange";
  }
}

export const ComponentsPage: React.FC = () => {
  // Polled data with git labels merged in by deployment and image.
  const { data, loading, labelsLoading, error, lastRefreshed, refresh: handleRefresh } = useComponentsData();
  const { canMutate, loaded: permissionsLoaded } = usePermissions();
  const mutateReason = !permissionsLoaded ? CHECKING_PERMISSIONS_REASON : !canMutate ? NO_PERMISSION_REASON : null;

  // DSC component fix state
  const [fixConfirm, setFixConfirm] = useState<{ action: string; title: string; message: string } | null>(null);
  const [fixLoading, setFixLoading] = useState<string | null>(null);
  const [fixResult, setFixResult] = useState<Record<string, OperationResponse>>({});
  const [attentionExpanded, setAttentionExpanded] = useState(true);
  const [repairMode, setRepairMode] = useState<"remove-invalid" | "remove-extra-components" | "reset-defaults" | null>(null);
  const [repairLoading, setRepairLoading] = useState(false);
  const [repairResult, setRepairResult] = useState<{ success: boolean; message: string } | null>(null);
  const [defaultsPreview, setDefaultsPreview] = useState("");
  const [previewVersion, setPreviewVersion] = useState("");
  const [previewError, setPreviewError] = useState("");

  useEffect(() => {
    if (repairMode !== "reset-defaults") return;
    let active = true;
    setDefaultsPreview("");
    setPreviewVersion("");
    setPreviewError("");
    getDSCPreview().then(res => { if (active) { setDefaultsPreview(res.yaml); setPreviewVersion(res.operatorVersion); } })
      .catch(err => { if (active) setPreviewError(toApiError(err).message); });
    return () => { active = false; };
  }, [repairMode]);

  const handleRepairDSC = async () => {
    if (!repairMode || !data) return;
    setRepairLoading(true);
    setRepairResult(null);
    try {
      const result = await repairDSC(data.dscName, repairMode, repairMode === "reset-defaults" ? previewVersion
        : repairMode === "remove-extra-components" ? data.dscCompatibility?.operatorVersion : undefined,
        repairMode === "remove-extra-components" ? data.dscCompatibility?.extraComponents : undefined);
      setRepairResult(result);
      setRepairMode(null);
      if (result.success) handleRefresh();
    } catch (err) {
      setRepairResult({ success: false, message: toApiError(err).message });
      setRepairMode(null);
    } finally {
      setRepairLoading(false);
    }
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
    if (res.success || res.errorCode === "nothing_to_do") handleRefresh();
  }, [handleRefresh]);

  useEffect(() => {
    document.title = "Components — RHOAI Nightly Updater";
  }, []);

  // No DSC is a state the backend reports with 200 (dscState), never an error (A05-8, A08-6).
  const dscState = data?.dscState ?? (data?.dscName ? "present" : undefined);
  const noDSC = !!data && (dscState === "no-dsc" || dscState === "no-crd");

  return (
    <>
      <PageHeader
        title="Components"
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={handleRefresh}
      />

      {error && (
        <LoadErrorAlert error={error} genericTitle="Could not load components" onRetry={handleRefresh} stale={!!data} />
      )}

      {loading && !data && !error && (
        <PageSection>
          <Bullseye>
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              <FlexItem><Spinner size="xl" aria-label="Loading components" /></FlexItem>
              <FlexItem><Content component="p">Loading component status...</Content></FlexItem>
            </Flex>
          </Bullseye>
        </PageSection>
      )}

      {data && noDSC && dscState && (
        <DSCEmptyState
          dscState={dscState}
          operatorVersion={data.operatorVersion}
          operatorPhase={data.operatorPhase}
          canMutate={canMutate}
          permissionsLoaded={permissionsLoaded}
          onCreated={handleRefresh}
        />
      )}

      {data && (
        <>
          {!noDSC && data.dscCompatibility && (
            data.dscCompatibility.validationError ||
            data.dscCompatibility.defaultsError ||
            data.dscCompatibility.invalidFields.length > 0 ||
            data.dscCompatibility.missingComponents.length > 0 ||
            (data.dscCompatibility.extraComponents?.length ?? 0) > 0 ||
            repairResult
          ) && (
            <PageSection>
              <Stack hasGutter>
                {data.dscCompatibility.validationError && (
                  <StackItem><Alert component="p" variant="warning" title="DSC field validation unavailable" isInline>{data.dscCompatibility.validationError}</Alert></StackItem>
                )}
                {data.dscCompatibility.defaultsError && (
                  <StackItem><Alert component="p" variant="warning" title="DSC defaults unavailable" isInline>{data.dscCompatibility.defaultsError}</Alert></StackItem>
                )}
                {(data.dscCompatibility.invalidFields.length > 0 || data.dscCompatibility.missingComponents.length > 0 || (data.dscCompatibility.extraComponents?.length ?? 0) > 0) && (
                  <StackItem>
                    <Alert component="p" variant="warning" title="DSC field names differ from the installed operator" isInline>
                      <Stack hasGutter>
                        {data.dscCompatibility.invalidFields.length > 0 && <StackItem>
                          <Content component="p">Invalid or deprecated fields in the installed CRD:</Content>
                          <List>{data.dscCompatibility.invalidFields.map(field => <ListItem key={field}><code>{field}</code></ListItem>)}</List>
                        </StackItem>}
                        {data.dscCompatibility.missingComponents.length > 0 && <StackItem>
                          <Content component="p">Components present in the version defaults but missing from this DSC: {data.dscCompatibility.missingComponents.join(", ")}. Missing components may be intentional.</Content>
                        </StackItem>}
                        {(data.dscCompatibility.extraComponents?.length ?? 0) > 0 && <StackItem>
                          <Content component="p">Components present in this DSC but absent from the {data.dscCompatibility.branch} version defaults:</Content>
                          <List>{data.dscCompatibility.extraComponents?.map(name => <ListItem key={name}><code>{name}</code></ListItem>)}</List>
                          <Content component="p">The installed CRD may still accept these names. Review them before removing their configuration.</Content>
                        </StackItem>}
                        <StackItem><Content component="p">Removing extra components preserves the remaining component settings and management states. Removing invalid fields preserves valid settings. Resetting replaces the entire DSC spec, including management states and custom settings.</Content></StackItem>
                        {data.dscCompatibility.sourceURL && <StackItem>
                          <Button variant="link" isInline component="a" href={data.dscCompatibility.sourceURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                            Defaults for {data.dscCompatibility.operatorVersion} ({data.dscCompatibility.branch})
                          </Button>
                        </StackItem>}
                        {data.dscCompatibility.defaultsSource === "csv" && <StackItem>
                          <Content component="small">Defaults come from the alm-examples of the installed operator {data.dscCompatibility.operatorVersion}.</Content>
                        </StackItem>}
                        <StackItem><Flex gap={{ default: "gapSm" }}>
                          {data.dscCompatibility.invalidFields.length > 0 && <FlexItem><TooltipButton variant="secondary" onClick={() => setRepairMode("remove-invalid")} isDisabled={repairLoading || !!data.dscCompatibility.validationError} disabledReason={mutateReason}>Remove invalid fields</TooltipButton></FlexItem>}
                          {(data.dscCompatibility.extraComponents?.length ?? 0) > 0 && <FlexItem><TooltipButton variant="secondary" onClick={() => setRepairMode("remove-extra-components")} isDisabled={repairLoading || !!data.dscCompatibility.defaultsError || !!data.dscCompatibility.validationError} disabledReason={mutateReason}>Remove extra components</TooltipButton></FlexItem>}
                          <FlexItem><TooltipButton variant="secondary" onClick={() => setRepairMode("reset-defaults")} isDisabled={repairLoading || !!data.dscCompatibility.defaultsError || !!data.dscCompatibility.validationError} disabledReason={mutateReason}>Reset to version defaults</TooltipButton></FlexItem>
                        </Flex></StackItem>
                      </Stack>
                    </Alert>
                  </StackItem>
                )}
                {repairResult && <StackItem><Alert component="p" variant={repairResult.success ? "success" : "danger"} title={repairResult.message} isInline isLiveRegion /></StackItem>}
              </Stack>
            </PageSection>
          )}

          {/* DSC Status Card: what the operator reports */}
          {!noDSC && (
          <PageSection>
            <Card isCompact>
              <CardBody>
                {(() => {
                  const components = data.components;
                  const sortByName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);
                  const ready = components.filter(c => c.status === "Available").sort(sortByName);
                  const notReady = components.filter(c => c.status !== "Available" && c.status !== "Removed").sort(sortByName);
                  const removed = components.filter(c => c.status === "Removed").sort(sortByName);

                  return (
                    <Stack hasGutter>
                      <StackItem>
                        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
                          <FlexItem>
                            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                              <FlexItem>
                                <Title headingLevel="h2" size="md">{data.dscName || "DataScienceCluster"}</Title>
                              </FlexItem>
                              {data.consoleURL && data.dscName && (
                                <FlexItem>
                                  <Button variant="link" isInline size="sm" component="a"
                                    href={`${data.consoleURL}/k8s/cluster/datasciencecluster.opendatahub.io~v2~DataScienceCluster/${data.dscName}/yaml`}
                                    target="_blank" rel="noopener noreferrer"
                                    icon={<ExternalLinkAltIcon />} iconPosition="end"
                                    aria-label={`Edit ${data.dscName} in the OpenShift console`}
                                  >Edit</Button>
                                </FlexItem>
                              )}
                              <FlexItem>
                                <Label isCompact
                                  color={data.dscPhase === "Ready" ? "green" : "orange"}
                                  icon={data.dscPhase === "Ready" ? <CheckCircleIcon /> : <ExclamationTriangleIcon />}
                                >{data.dscPhase || "Unknown"}</Label>
                              </FlexItem>
                            </Flex>
                          </FlexItem>
                          <FlexItem>
                            <Flex gap={{ default: "gapSm" }}>
                              <FlexItem><Label color="green" isCompact>{ready.length} ready</Label></FlexItem>
                              {notReady.length > 0 && <FlexItem><Label color="orange" isCompact>{notReady.length} need attention</Label></FlexItem>}
                              {removed.length > 0 && <FlexItem><Label color="grey" isCompact>{removed.length} removed</Label></FlexItem>}
                            </Flex>
                          </FlexItem>
                        </Flex>
                        {data.dscPhase !== "Ready" && data.dscReason && (
                          <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)", marginTop: "0.25rem", overflowWrap: "anywhere", whiteSpace: "pre-line" }}>
                            {data.dscReason}
                          </Content>
                        )}
                      </StackItem>

                      {notReady.length > 0 && (
                        <StackItem>
                          <ExpandableSection toggleText={`Need attention (${notReady.length})`} isIndented isExpanded={attentionExpanded} onToggle={(_e, v) => setAttentionExpanded(v)}>
                            <Stack hasGutter>
                              {notReady.map(c => {
                                const res = c.fixAction ? fixResult[c.fixAction] : undefined;
                                return (
                                  <StackItem key={c.name}>
                                    <Flex alignItems={{ default: "alignItemsFlexStart" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                                      <FlexItem><ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" /></FlexItem>
                                      <FlexItem flex={{ default: "flex_1" }} style={{ minWidth: 0 }}>
                                        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                                          <FlexItem><strong>{c.name}</strong></FlexItem>
                                          <FlexItem><Label isCompact color={statusColor(c.status)}>{c.status}</Label></FlexItem>
                                          {c.fixAction && c.fixTitle && !res?.success && (
                                            <FlexItem>
                                              <TooltipButton variant="link" isInline size="sm"
                                                isLoading={fixLoading === c.fixAction}
                                                isDisabled={fixLoading !== null}
                                                disabledReason={mutateReason}
                                                onClick={() => setFixConfirm({
                                                  action: c.fixAction!,
                                                  title: c.fixTitle!,
                                                  message: c.fixConfirm || c.fixDescription || "This changes the DataScienceCluster on the shared cluster.",
                                                })}
                                              >{c.fixTitle}</TooltipButton>
                                            </FlexItem>
                                          )}
                                        </Flex>
                                        {c.message && (
                                          <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)", overflowWrap: "anywhere" }}>
                                            {c.message}
                                          </Content>
                                        )}
                                        {res && (
                                          <Alert component="p" isInline isPlain isLiveRegion variant={outcomeVariant(res)} title={outcomeTitle(res, "Fix failed")}>
                                            {res.success ? undefined : res.message}
                                          </Alert>
                                        )}
                                      </FlexItem>
                                    </Flex>
                                  </StackItem>
                                );
                              })}
                            </Stack>
                          </ExpandableSection>
                        </StackItem>
                      )}

                      <StackItem>
                        <ExpandableSection toggleText={`Ready (${ready.length})`} isIndented>
                          <Flex gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                            {ready.map(c => (
                              <FlexItem key={c.name}>
                                <Label isCompact color="green" icon={<CheckCircleIcon />}>{c.name}</Label>
                              </FlexItem>
                            ))}
                          </Flex>
                        </ExpandableSection>
                      </StackItem>

                      {removed.length > 0 && (
                        <StackItem>
                          <ExpandableSection toggleText={`Removed (${removed.length})`} isIndented>
                            <Flex gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                              {removed.map(c => (
                                <FlexItem key={c.name}>
                                  <Label isCompact color="grey">{c.name}</Label>
                                </FlexItem>
                              ))}
                            </Flex>
                          </ExpandableSection>
                        </StackItem>
                      )}
                    </Stack>
                  );
                })()}
              </CardBody>
            </Card>
          </PageSection>
          )}

          <PageSection>
            <Card>
              <CardTitle>
                <Title headingLevel="h2" size="lg">Deployments</Title>
                {(data.changedCount ?? 0) > 0 && data.snapshotTime && (
                  <Content component="small">
                    <Label isCompact color="blue">{data.changedCount} updated</Label>{" "}
                    since the snapshot taken at the last install or update ({formatRelativeTime(data.snapshotTime)})
                  </Content>
                )}
              </CardTitle>
              <CardBody>
                <DeploymentsTable
                  deployments={data.deployments}
                  labelsLoading={labelsLoading}
                  consoleURL={data.consoleURL}
                  canMutate={canMutate}
                  permissionsLoaded={permissionsLoaded}
                  onRefresh={handleRefresh}
                />
              </CardBody>
            </Card>
          </PageSection>
        </>
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
            {repairMode === "reset-defaults" && <StackItem>{previewError ? <Alert component="p" variant="danger" title="Could not load defaults" isInline>{previewError}</Alert>
              : defaultsPreview ? <CodeBlock><CodeBlockCode>{defaultsPreview}</CodeBlockCode></CodeBlock> : <Spinner aria-label="Loading DSC defaults" />}</StackItem>}
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button variant={repairMode === "reset-defaults" ? "danger" : "primary"} onClick={handleRepairDSC} isLoading={repairLoading} isDisabled={repairLoading || (repairMode === "reset-defaults" && !defaultsPreview)}>Confirm</Button>
          <Button variant="link" onClick={() => setRepairMode(null)} isDisabled={repairLoading}>Cancel</Button>
        </ModalFooter>
      </Modal>

      <Modal
        aria-labelledby="confirm-fix-title"
        variant={ModalVariant.small}
        isOpen={fixConfirm !== null}
        onClose={() => setFixConfirm(null)}
      >
        <ModalHeader title={`${fixConfirm?.title || "Fix"}?`} labelId="confirm-fix-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">{fixConfirm?.message}</Content>
            </StackItem>
            <StackItem>
              <Content component="small">The server re-checks the component first and changes nothing when it is no longer needed or a prerequisite is missing.</Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This changes a shared cluster." isInline isPlain />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => fixConfirm && handleComponentFix(fixConfirm.action)}
            isLoading={fixLoading !== null}
            isDisabled={fixLoading !== null}
          >
            {fixConfirm?.title || "Confirm"}
          </Button>
          <Button variant="link" onClick={() => setFixConfirm(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
