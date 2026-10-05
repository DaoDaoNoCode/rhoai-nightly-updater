import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Card,
  CardBody,
  CardTitle,
  CodeBlock,
  CodeBlockCode,
  Content,
  Dropdown,
  DropdownItem,
  DropdownList,
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
  Tooltip,
  Flex,
  FlexItem,
  Button,
  Bullseye,
  ExpandableSection,
  List,
  ListItem,
} from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td, ExpandableRowContent } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { Link } from "react-router-dom";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import MinusCircleIcon from "@patternfly/react-icons/dist/esm/icons/minus-circle-icon";
import type { DeploymentInfo } from "../types";
import { assistRollout, fixProblem, repairDSC, getDSCPreview, toApiError } from "../services/api";
import { formatRelativeTime } from "../utils";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { CopyableText } from "../components/CopyableText";
import { useComponentsData } from "../hooks/useComponentsData";

function statusColor(
  status: string,
): "green" | "red" | "blue" | "grey" | "orange" {
  switch (status) {
    case "Available":
      return "green";
    case "Degraded":
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

/** Derive the ready label text, color, and icon from the pod list. */
function readyLabel(dep: DeploymentInfo): { text: string; color: "green" | "orange" | "grey"; icon?: React.ReactNode } {
  if (dep.desired === 0) {
    return { text: "Scaled down", color: "grey", icon: <MinusCircleIcon /> };
  }
  const pods = dep.pods || [];
  if (pods.length === 0) {
    if (dep.ready === dep.desired && dep.desired > 0) {
      return { text: `${dep.ready}/${dep.desired}`, color: "green", icon: <CheckCircleIcon /> };
    }
    return { text: `${dep.ready}/${dep.desired}`, color: "orange" };
  }
  const readyCount = pods.filter(p => p.ready).length;
  const pendingCount = pods.filter(p => !p.ready && p.phase === "Pending").length;
  const total = pods.length;
  const notReadyCount = total - readyCount;

  if (notReadyCount === 0 && total === dep.desired) {
    return { text: `${readyCount}/${dep.desired}`, color: "green", icon: <CheckCircleIcon /> };
  }
  let text = `${readyCount}/${dep.desired} ready`;
  if (pendingCount > 0) {
    text += ` (${pendingCount} pending)`;
  } else if (notReadyCount > 0) {
    text += ` (${notReadyCount} not ready)`;
  }
  return { text, color: "orange" };
}

/** Detect whether a deployment has a stuck rollout (pods from multiple ReplicaSets with pending pods). */
function rolloutStuckInfo(dep: DeploymentInfo): { stuck: boolean; reason?: string } {
  const pods = dep.pods || [];
  if (pods.length === 0) return { stuck: false };

  const hashes = new Set(pods.map(p => p.podTemplateHash).filter(Boolean));
  if (hashes.size < 2) return { stuck: false };

  // Multiple template hashes => old + new RS. Check if any pod is Pending.
  const pendingPods = pods.filter(p => p.phase === "Pending");
  if (pendingPods.length === 0) return { stuck: false };

  const reason = pendingPods.find(p => p.schedulingReason)?.schedulingReason;
  return { stuck: true, reason };
}

export const ComponentsPage: React.FC = () => {
  // Polled data with git labels merged in by deployment and image.
  const { data, loading, labelsLoading, error, lastRefreshed, refresh: handleRefresh } = useComponentsData();

  // Expandable rows state for deployments
  const [expandedDeps, setExpandedDeps] = useState<Record<string, boolean>>({});
  const [logsDropdownOpen, setLogsDropdownOpen] = useState<Record<string, boolean>>({});
  const [rolloutLoading, setRolloutLoading] = useState<string | null>(null);
  const [rolloutResult, setRolloutResult] = useState<Record<string, { success: boolean; message: string }>>({});
  const [rolloutCooldown, setRolloutCooldown] = useState<Record<string, boolean>>({});
  const [unblockConfirmKey, setUnblockConfirmKey] = useState<string | null>(null);

  // DSC component fix state
  const [fixConfirm, setFixConfirm] = useState<{ action: string; title: string; message: string } | null>(null);
  const [fixLoading, setFixLoading] = useState<string | null>(null);
  const [fixResult, setFixResult] = useState<Record<string, { success: boolean; message: string }>>({});
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
    try {
      const res = await fixProblem(action);
      setFixResult(prev => ({ ...prev, [action]: { success: res.success, message: res.message } }));
      if (res.success) handleRefresh();
    } catch (e) {
      setFixResult(prev => ({ ...prev, [action]: { success: false, message: toApiError(e).message } }));
    } finally {
      setFixLoading(null);
    }
  }, [handleRefresh]);

  const handleAssistRollout = useCallback((depKey: string) => {
    setUnblockConfirmKey(null);
    setRolloutLoading(depKey);
    setRolloutResult(prev => { const next = { ...prev }; delete next[depKey]; return next; });
    assistRollout()
      .then(res => {
        const msg = res.success
          ? "Rollout unblocked. Pods will update one at a time — this takes a few minutes."
          : res.message;
        setRolloutResult(prev => ({ ...prev, [depKey]: { success: res.success, message: msg } }));
        if (res.success) {
          setRolloutCooldown(prev => ({ ...prev, [depKey]: true }));
          setTimeout(() => setRolloutCooldown(prev => { const next = { ...prev }; delete next[depKey]; return next; }), 60_000);
        }
        handleRefresh();
      })
      .catch(err => {
        setRolloutResult(prev => ({ ...prev, [depKey]: { success: false, message: toApiError(err).message } }));
      })
      .finally(() => setRolloutLoading(null));
  }, [handleRefresh]);

  useEffect(() => {
    document.title = "Components — RHOAI Nightly Updater";
  }, []);

  // The backend answers 500 "failed to get components" when no DSC exists or
  // the operator isn't installed (pkg/api HandleComponents). It has no
  // specific errorCode yet, so match that exact response, not a substring.
  const componentsUnavailable = !!error && error.status === 500 && error.message === "failed to get components";

  return (
    <>
      <PageHeader
        title="Components"
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={handleRefresh}
      />

      {error && !(componentsUnavailable && !data) && (
        <ErrorAlert error={error} genericTitle="Failed to load components" />
      )}

      {componentsUnavailable && !data && (
        <PageSection>
          <Alert variant="info" title="RHOAI operator is not installed yet" isInline component="p">
            <p>Components will appear here once the RHOAI operator is installed on this cluster.</p>
            <p style={{ marginTop: "0.5rem" }}>
              Go to the <Link to="/">Dashboard</Link> to install RHOAI using the <strong>Upgrade to Nightly Build</strong> panel.
            </p>
          </Alert>
        </PageSection>
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

      {data && (
        <>
          {data.dscCompatibility && (
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
                        <StackItem><Flex gap={{ default: "gapSm" }}>
                          {data.dscCompatibility.invalidFields.length > 0 && <FlexItem><Button variant="secondary" onClick={() => setRepairMode("remove-invalid")} isDisabled={repairLoading || !!data.dscCompatibility.validationError}>Remove invalid fields</Button></FlexItem>}
                          {(data.dscCompatibility.extraComponents?.length ?? 0) > 0 && <FlexItem><Button variant="secondary" onClick={() => setRepairMode("remove-extra-components")} isDisabled={repairLoading || !!data.dscCompatibility.defaultsError || !!data.dscCompatibility.validationError}>Remove extra components</Button></FlexItem>}
                          <FlexItem><Button variant="secondary" onClick={() => setRepairMode("reset-defaults")} isDisabled={repairLoading || !!data.dscCompatibility.defaultsError || !!data.dscCompatibility.validationError}>Reset to version defaults</Button></FlexItem>
                        </Flex></StackItem>
                      </Stack>
                    </Alert>
                  </StackItem>
                )}
                {repairResult && <StackItem><Alert component="p" variant={repairResult.success ? "success" : "danger"} title={repairResult.message} isInline /></StackItem>}
              </Stack>
            </PageSection>
          )}
          {/* DSC Status Card — shows exactly what the operator reports */}
          <PageSection>
            <Card isCompact>
              <CardBody>
                {(() => {
                  const components = data.components || [];
                  const sortByName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);
                  const ready = components.filter(c => c.status === "Available").sort(sortByName);
                  const notReady = components.filter(c => c.status !== "Available" && c.status !== "Removed").sort(sortByName);
                  const removed = components.filter(c => c.status === "Removed").sort(sortByName);

                  return (
                    <Stack hasGutter>
                      {/* Header: DSC name, phase, summary counts */}
                      <StackItem>
                        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
                          <FlexItem>
                            <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                              <FlexItem>
                                <Title headingLevel="h3" size="md">{data.dscName || "DataScienceCluster"}</Title>
                              </FlexItem>
                              {data.consoleURL && (
                                <FlexItem>
                                  <Button variant="link" isInline size="sm" component="a"
                                    href={`${data.consoleURL}/k8s/cluster/datasciencecluster.opendatahub.io~v2~DataScienceCluster/${data.dscName}/yaml`}
                                    target="_blank" rel="noopener noreferrer"
                                    icon={<ExternalLinkAltIcon />} iconPosition="end"
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
                              {notReady.length > 0 && <FlexItem><Label color="orange" isCompact>{notReady.length} needs attention</Label></FlexItem>}
                              {removed.length > 0 && <FlexItem><Label color="grey" isCompact>{removed.length} removed</Label></FlexItem>}
                            </Flex>
                          </FlexItem>
                        </Flex>
                        {data.dscPhase !== "Ready" && data.dscReason && (
                          <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)", marginTop: "0.25rem" }}>
                            {data.dscReason}
                          </Content>
                        )}
                      </StackItem>

                      {/* Components needing attention — show operator's actual status and message */}
                      {notReady.length > 0 && (
                        <StackItem>
                          <ExpandableSection toggleText={`Needs Attention (${notReady.length})`} isIndented>
                            <Stack hasGutter>
                              {notReady.map(c => (
                                <StackItem key={c.name}>
                                  <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                                    <FlexItem><ExclamationTriangleIcon color="var(--pf-t--global--color--status--warning--default)" /></FlexItem>
                                    <FlexItem><Content component="small"><strong>{c.name}</strong></Content></FlexItem>
                                    <FlexItem><Label isCompact color={statusColor(c.status)}>{c.status}</Label></FlexItem>
                                    {c.message && (
                                      <FlexItem>
                                        <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)" }}>
                                          {c.message}
                                        </Content>
                                      </FlexItem>
                                    )}
                                    {c.fixAction && c.fixTitle && (
                                      <FlexItem>
                                        {fixResult[c.fixAction] ? (
                                          <Label isCompact color={fixResult[c.fixAction].success ? "green" : "red"}>
                                            {fixResult[c.fixAction].message}
                                          </Label>
                                        ) : (
                                          <Button variant="link" isInline size="sm"
                                            isLoading={fixLoading === c.fixAction}
                                            isDisabled={fixLoading !== null}
                                            onClick={() => setFixConfirm({
                                              action: c.fixAction!,
                                              title: c.fixTitle!,
                                              message: c.fixConfirm || "This will modify resources on the shared cluster.",
                                            })}
                                          >{c.fixTitle}</Button>
                                        )}
                                      </FlexItem>
                                    )}
                                  </Flex>
                                </StackItem>
                              ))}
                            </Stack>
                          </ExpandableSection>
                        </StackItem>
                      )}

                      {/* Ready components */}
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

                      {/* Removed components */}
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

          {/* Deployments Table */}
          <PageSection>
            <Card>
              <CardTitle>
                <Title headingLevel="h3">Deployments</Title>
              </CardTitle>
              <CardBody>
                {(data.changedCount ?? 0) > 0 && data.snapshotTime && (
                  <Content component="small" style={{ marginBottom: "0.5rem" }}>
                    <Label isCompact color="blue">{data.changedCount} updated</Label>{' '}
                    since last snapshot ({formatRelativeTime(data.snapshotTime)})
                  </Content>
                )}
                <Table
                  aria-label="Deployments table"
                  variant="compact"
                  gridBreakPoint="grid-md"
                >
                  <Thead>
                    <Tr>
                      <Th screenReaderText="Expand" />
                      <Th>Name</Th>
                      <Th>Ready</Th>
                      <Th info={{
                        popover: (
                          <Content component="small">
                            Resolved from the OCI image config labels (<code>vcs-ref</code>,{' '}
                            <code>git.url</code>) embedded at build time by Konflux.
                            The merge date comes from the GitHub API (60 requests/hr
                            unauthenticated — may show &quot;-&quot; if rate limited).
                          </Content>
                        ),
                        popoverProps: { headerContent: "How commit info is resolved" },
                      }}>Commit</Th>
                      <Th>Built</Th>
                      <Th>Version</Th>
                    </Tr>
                  </Thead>
                  {data.deployments && data.deployments.length > 0 ? (
                    data.deployments.map((dep, rowIndex) => {
                      const depKey = `${dep.namespace}/${dep.name}`;
                      const isExpanded = !!expandedDeps[depKey];
                      const shortSha = dep.gitCommit ? dep.gitCommit.slice(0, 7) : "";
                      const commitURL = dep.gitCommit && dep.gitURL ? `${dep.gitURL}/commit/${dep.gitCommit}` : "";
                      const compareURL = dep.gitCommit && dep.gitURL ? `${dep.gitURL}/compare/${dep.gitCommit}...main` : "";
                      const hasPods = !!dep.pods && dep.pods.length > 0;
                      const canExpand = hasPods || !!dep.image;
                      const consoleURL = data.consoleURL?.startsWith("https://") ? data.consoleURL : "";

                      return (
                        <Tbody key={depKey} isExpanded={isExpanded}>
                          <Tr>
                            <Td
                              expand={canExpand ? {
                                rowIndex,
                                isExpanded,
                                onToggle: () => setExpandedDeps(prev => ({ ...prev, [depKey]: !prev[depKey] })),
                              } : undefined}
                            />
                            <Td dataLabel="Name" id={`simple-node${rowIndex}`}>
                              {dep.image ? <CopyableText text={dep.name} value={dep.image} what="image reference" /> : dep.name}
                              {dep.changeStatus === "updated" && <>{' '}<Label isCompact color="green">Updated</Label></>}
                              {dep.changeStatus === "new" && <>{' '}<Label isCompact color="blue">New</Label></>}
                              {(() => {
                                const rsi = rolloutStuckInfo(dep);
                                const onCooldown = rolloutCooldown[depKey];
                                const result = rolloutResult[depKey];

                                if (onCooldown && result?.success) {
                                  return (
                                    <>
                                      {' '}
                                      <Label isCompact color="blue" icon={<Spinner size="sm" aria-label="Rollout in progress" />}>Rollout in progress</Label>
                                      {' '}
                                      <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)" }}>
                                        {result.message}
                                      </Content>
                                    </>
                                  );
                                }

                                if (!rsi.stuck && !result) return null;

                                return (
                                  <>
                                    {rsi.stuck && (
                                      <>
                                        {' '}
                                        <Tooltip content={rsi.reason || "Pods from old and new ReplicaSets detected with pending pods"}>
                                          <Label isCompact color="orange" icon={<ExclamationTriangleIcon />}>
                                            Rollout stuck
                                          </Label>
                                        </Tooltip>
                                        {' '}
                                        <Button
                                          variant="link"
                                          isInline
                                          size="sm"
                                          isLoading={rolloutLoading === depKey}
                                          isDisabled={rolloutLoading !== null || onCooldown}
                                          onClick={() => setUnblockConfirmKey(depKey)}
                                        >
                                          {onCooldown ? "Cooldown..." : "Unblock Rollout"}
                                        </Button>
                                      </>
                                    )}
                                    {result && !onCooldown && (
                                      <>{' '}<Label isCompact color={result.success ? "green" : "red"}>
                                        {result.message}
                                      </Label></>
                                    )}
                                  </>
                                );
                              })()}
                            </Td>
                            <Td dataLabel="Ready">
                              {(() => {
                                const rl = readyLabel(dep);
                                return (
                                  <Label isCompact color={rl.color} icon={rl.icon}>
                                    {rl.text}
                                  </Label>
                                );
                              })()}
                            </Td>
                            <Td dataLabel="Commit">
                              {labelsLoading && !shortSha ? (
                                <Spinner size="sm" aria-label="Loading commit info" />
                              ) : shortSha ? (
                                <>
                                  <Flex spaceItems={{ default: "spaceItemsSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                                    <FlexItem>
                                      <Tooltip content={dep.gitCommit}>
                                        <Button variant="link" isInline component="a" href={commitURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">{shortSha}</Button>
                                      </Tooltip>
                                    </FlexItem>
                                    {compareURL && (
                                      <FlexItem>
                                        <Button variant="link" isInline component="a" href={compareURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">diff</Button>
                                      </FlexItem>
                                    )}
                                  </Flex>
                                  {dep.commitDate && (
                                    <Content component="small">
                                      merged <time dateTime={dep.commitDate} title={new Date(dep.commitDate).toLocaleString()}>{formatRelativeTime(dep.commitDate)}</time>
                                    </Content>
                                  )}
                                </>
                              ) : "-"}
                            </Td>
                            <Td dataLabel="Built">
                              {labelsLoading && !dep.buildDate ? (
                                <Spinner size="sm" aria-label="Loading build info" />
                              ) : dep.buildDate ? (
                                <Content component="small">
                                  <time dateTime={dep.buildDate} title={new Date(dep.buildDate).toLocaleString()}>{formatRelativeTime(dep.buildDate)}</time>
                                </Content>
                              ) : "-"}
                            </Td>
                            <Td dataLabel="Version">{dep.version || "-"}</Td>
                          </Tr>
                          {canExpand && (
                            <Tr isExpanded={isExpanded}>
                              <Td colSpan={6}>
                                <ExpandableRowContent>
                                  {dep.image && (
                                    <Content component="small" style={{ overflowWrap: "anywhere" }}>
                                      Image: <code>{dep.image}</code>
                                    </Content>
                                  )}
                                  {hasPods && (
                                  <Table aria-label={`Pods for ${dep.name}`} variant="compact" borders={false}>
                                    <Thead>
                                      <Tr>
                                        <Th>Pod</Th>
                                        <Th>Containers</Th>
                                        <Th>Node</Th>
                                        <Th modifier="nowrap">Restarts</Th>
                                        <Th modifier="nowrap">Age</Th>
                                        <Th modifier="nowrap">Logs</Th>
                                      </Tr>
                                    </Thead>
                                    <Tbody>
                                      {(dep.pods ?? []).map(pod => {
                                        const containers = pod.containers || [];
                                        const podLogBase = `${consoleURL}/k8s/ns/${pod.namespace}/pods/${pod.name}/logs`;
                                        const dropdownKey = pod.name;
                                        const hasPendingScheduling = pod.phase === "Pending" && pod.schedulingReason;

                                        return (
                                          <React.Fragment key={pod.name}>
                                          {pod.schedulingReason && (
                                            <Tr>
                                              <Td colSpan={6} style={{ padding: "0.25rem 0.5rem" }}>
                                                <Alert component="p"
                                                  variant="warning"
                                                  title={pod.schedulingReason}
                                                  isInline
                                                  isPlain
                                                >
                                                  {pod.schedulingMessage && (
                                                    <Content component="small">{pod.schedulingMessage}</Content>
                                                  )}
                                                </Alert>
                                              </Td>
                                            </Tr>
                                          )}
                                          <Tr>
                                            <Td dataLabel="Pod">
                                              {hasPendingScheduling ? (
                                                <Tooltip content={`Scheduling: ${pod.schedulingReason}`}>
                                                  <Content component="small" style={{ color: "var(--pf-t--global--color--status--warning--default)" }}>
                                                    <ExclamationTriangleIcon style={{ marginRight: "0.25rem" }} />
                                                    {pod.name}
                                                  </Content>
                                                </Tooltip>
                                              ) : (
                                                <Content component="small">{pod.name}</Content>
                                              )}
                                            </Td>
                                            <Td dataLabel="Containers">
                                              <Flex gap={{ default: "gapXs" }} flexWrap={{ default: "wrap" }}>
                                                {containers.map(c => {
                                                  const tooltipText = c.reason || c.state || "Not started (pod pending)";
                                                  const labelColor = c.ready ? "green" : c.state === "waiting" ? "orange" : c.state === "terminated" ? "red" : "grey";
                                                  return (
                                                    <FlexItem key={c.name}>
                                                      <Tooltip content={tooltipText}>
                                                        <Label isCompact
                                                          color={labelColor}
                                                          icon={c.ready ? <CheckCircleIcon /> : (c.reason || !c.state) ? <ExclamationCircleIcon /> : undefined}>
                                                          {c.name}
                                                        </Label>
                                                      </Tooltip>
                                                    </FlexItem>
                                                  );
                                                })}
                                              </Flex>
                                            </Td>
                                            <Td dataLabel="Node">
                                              {pod.phase === "Pending" && !pod.node ? (
                                                <Label isCompact color="orange">Unscheduled</Label>
                                              ) : (
                                                <Content component="small">{pod.node?.split('.')[0] || "-"}</Content>
                                              )}
                                            </Td>
                                            <Td dataLabel="Restarts">{pod.restarts}</Td>
                                            <Td dataLabel="Age">{pod.age}</Td>
                                            <Td dataLabel="Logs">
                                              {consoleURL && containers.length === 0 ? (
                                                <Button variant="link" isInline component="a"
                                                  href={podLogBase}
                                                  target="_blank" rel="noopener noreferrer"
                                                  icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">
                                                  Logs
                                                </Button>
                                              ) : consoleURL && containers.length === 1 ? (
                                                <Button variant="link" isInline component="a"
                                                  href={`${podLogBase}?container=${containers[0].name}`}
                                                  target="_blank" rel="noopener noreferrer"
                                                  icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">
                                                  Logs
                                                </Button>
                                              ) : consoleURL && containers.length > 1 ? (
                                                <Dropdown
                                                  isOpen={!!logsDropdownOpen[dropdownKey]}
                                                  onSelect={() => setLogsDropdownOpen(prev => ({...prev, [dropdownKey]: false}))}
                                                  onOpenChange={(open) => setLogsDropdownOpen(prev => ({...prev, [dropdownKey]: open}))}
                                                  toggle={(toggleRef) => (
                                                    <Button ref={toggleRef} variant="link" isInline size="sm"
                                                      onClick={() => setLogsDropdownOpen(prev => ({...prev, [dropdownKey]: !prev[dropdownKey]}))}>
                                                      <span style={{ whiteSpace: "nowrap" }}>Logs&nbsp;▾</span>
                                                    </Button>
                                                  )}
                                                >
                                                  <DropdownList>
                                                    {containers.map(c => (
                                                      <DropdownItem key={c.name}
                                                        onClick={() => window.open(`${podLogBase}?container=${c.name}`, '_blank')}>
                                                        {c.name} {!c.ready && c.reason ? `(${c.reason})` : ''}
                                                      </DropdownItem>
                                                    ))}
                                                  </DropdownList>
                                                </Dropdown>
                                              ) : null}
                                            </Td>
                                          </Tr>
                                          </React.Fragment>
                                        );
                                      })}
                                    </Tbody>
                                  </Table>
                                  )}
                                </ExpandableRowContent>
                              </Td>
                            </Tr>
                          )}
                        </Tbody>
                      );
                    })
                  ) : (
                    <Tbody>
                      <Tr>
                        <Td colSpan={6}>
                          <Content component="p">No deployments found</Content>
                        </Td>
                      </Tr>
                    </Tbody>
                  )}
                </Table>
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

      {/* Unblock Rollout Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-unblock-title"
        variant={ModalVariant.small}
        isOpen={unblockConfirmKey !== null}
        onClose={() => setUnblockConfirmKey(null)}
      >
        <ModalHeader title="Confirm: Unblock Rollout" labelId="confirm-unblock-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                This will patch the deployment's rollout strategy to allow terminating one old pod,
                freeing resources for the new pod to schedule. The deployment may be briefly
                unavailable.
              </Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This action will modify resources on the shared cluster." isInline />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => unblockConfirmKey && handleAssistRollout(unblockConfirmKey)}
            isLoading={rolloutLoading !== null}
            isDisabled={rolloutLoading !== null}
          >
            Confirm
          </Button>
          <Button variant="link" onClick={() => setUnblockConfirmKey(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>

      {/* Component Fix Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-fix-title"
        variant={ModalVariant.small}
        isOpen={fixConfirm !== null}
        onClose={() => setFixConfirm(null)}
      >
        <ModalHeader title={`Confirm: ${fixConfirm?.title || "Fix"}`} labelId="confirm-fix-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">{fixConfirm?.message}</Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This action will modify resources on the shared cluster." isInline />
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
            Confirm
          </Button>
          <Button variant="link" onClick={() => setFixConfirm(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
