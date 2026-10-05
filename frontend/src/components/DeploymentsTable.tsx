import React, { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  Alert,
  Button,
  Content,
  Dropdown,
  DropdownItem,
  DropdownList,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
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
  SearchInput,
  Skeleton,
  Stack,
  StackItem,
  ToggleGroup,
  ToggleGroupItem,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import { ExpandableRowContent, Table, Tbody, Td, Th, Thead, Tr, type ThProps } from "@patternfly/react-table";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import MinusCircleIcon from "@patternfly/react-icons/dist/esm/icons/minus-circle-icon";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
import type { DeploymentInfo, OperationResponse } from "../types";
import { assistRolloutFor, trackFeature } from "../services/api";
import { formatRelativeTime } from "../utils";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { ImageRef } from "./ImageRef";
import { TooltipButton, NO_PERMISSION_REASON } from "./TooltipButton";
import { CHECKING_PERMISSIONS_REASON } from "../hooks/usePermissions";
import {
  activePods,
  deploymentHealth,
  filterDeployments,
  sortDeployments,
  type DeploymentHealth,
  type DeploymentSortKey,
  type SortDirection,
  type StatusFilter,
} from "./deploymentHealth";

/** Rollout blocked by Unschedulable new pods while old pods still serve (what assist-rollout can unblock). */
export function unschedulableRollout(dep: DeploymentInfo): boolean {
  const pods = activePods(dep);
  const hashes = new Set(pods.map((p) => p.podTemplateHash).filter(Boolean));
  return hashes.size >= 2 && pods.some((p) => p.ready) && pods.some((p) => !p.ready && p.schedulingReason === "Unschedulable");
}

const HealthLabel: React.FC<{ health: DeploymentHealth }> = ({ health }) => {
  switch (health.kind) {
    case "healthy":
      return <Label isCompact color="green" icon={<CheckCircleIcon />}>{health.readyText}</Label>;
    case "scaled-down":
      return <Label isCompact color="grey" icon={<MinusCircleIcon />}>{health.readyText}</Label>;
    case "progressing":
      return <Label isCompact color="blue" icon={<InProgressIcon />}>{health.readyText}</Label>;
    default:
      return <Label isCompact color="red" icon={<ExclamationCircleIcon />}>{health.readyText}</Label>;
  }
};

const COLUMNS: { key: DeploymentSortKey | null; label: string }[] = [
  { key: null, label: "" },
  { key: "name", label: "Name" },
  { key: "status", label: "Status" },
  { key: null, label: "Commit" },
  { key: "built", label: "Built" },
  { key: null, label: "Version" },
];

interface DeploymentsTableProps {
  deployments: DeploymentInfo[];
  labelsLoading: boolean;
  consoleURL?: string;
  canMutate: boolean;
  permissionsLoaded: boolean;
  onRefresh: () => void;
}

/**
 * The Deployments table (A08-14): problems first by default, sortable by
 * name, status and build date, with a text filter and a status filter. Each
 * row expands to the full image (copyable) and its pods.
 */
export const DeploymentsTable: React.FC<DeploymentsTableProps> = ({
  deployments,
  labelsLoading,
  consoleURL: rawConsoleURL,
  canMutate,
  permissionsLoaded,
  onRefresh,
}) => {
  const [sortKey, setSortKey] = useState<DeploymentSortKey>("status");
  const [sortDirection, setSortDirection] = useState<SortDirection>("asc");
  const [filterText, setFilterText] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [logsOpen, setLogsOpen] = useState<Record<string, boolean>>({});
  const [unblockTarget, setUnblockTarget] = useState<DeploymentInfo | null>(null);
  const [unblocking, setUnblocking] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, OperationResponse>>({});

  const consoleURL = rawConsoleURL?.startsWith("https://") ? rawConsoleURL : "";
  const counts = useMemo(() => {
    let attention = 0;
    let healthy = 0;
    for (const dep of deployments) {
      const kind = deploymentHealth(dep).kind;
      if (kind === "problem" || kind === "progressing") attention++;
      else if (kind === "healthy") healthy++;
    }
    return { attention, healthy };
  }, [deployments]);
  const rows = useMemo(
    () => sortDeployments(filterDeployments(deployments, filterText, statusFilter), sortKey, sortDirection),
    [deployments, filterText, statusFilter, sortKey, sortDirection],
  );

  const sortParams = (columnIndex: number): ThProps["sort"] => {
    const key = COLUMNS[columnIndex].key;
    if (!key) return undefined;
    const activeIndex = COLUMNS.findIndex((c) => c.key === sortKey);
    return {
      sortBy: { index: activeIndex, direction: sortDirection, defaultDirection: key === "built" ? "desc" : "asc" },
      onSort: (_e, _index, direction) => {
        setSortKey(key);
        setSortDirection(direction);
      },
      columnIndex,
    };
  };

  const mutateReason = !permissionsLoaded ? CHECKING_PERMISSIONS_REASON : !canMutate ? NO_PERMISSION_REASON : unblocking ? "Another rollout is being unblocked." : null;

  const runUnblock = async (dep: DeploymentInfo) => {
    const key = `${dep.namespace}/${dep.name}`;
    trackFeature("assist_rollout");
    setUnblockTarget(null);
    setUnblocking(key);
    let res: OperationResponse;
    try {
      res = await assistRolloutFor({ namespace: dep.namespace, deployment: dep.name });
    } catch (e) {
      res = errorResult(e, "Assist rollout failed");
    }
    setResults((prev) => ({ ...prev, [key]: res }));
    setUnblocking(null);
    onRefresh();
  };

  const clearFilters = () => {
    setFilterText("");
    setStatusFilter("all");
  };

  return (
    <>
      <Toolbar clearAllFilters={clearFilters} collapseListedFiltersBreakpoint="md">
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              aria-label="Filter deployments by name, namespace, image or version"
              placeholder="Filter by name or image"
              value={filterText}
              onChange={(_e, value) => setFilterText(value)}
              onClear={() => setFilterText("")}
            />
          </ToolbarItem>
          <ToolbarItem>
            <ToggleGroup aria-label="Filter deployments by status" isCompact>
              <ToggleGroupItem text={`All (${deployments.length})`} isSelected={statusFilter === "all"} onChange={() => setStatusFilter("all")} />
              <ToggleGroupItem text={`Needs attention (${counts.attention})`} isSelected={statusFilter === "attention"} onChange={() => setStatusFilter("attention")} />
              <ToggleGroupItem text={`Healthy (${counts.healthy})`} isSelected={statusFilter === "healthy"} onChange={() => setStatusFilter("healthy")} />
            </ToggleGroup>
          </ToolbarItem>
          <ToolbarItem alignSelf="center">
            <Content component="small" aria-live="polite">Showing {rows.length} of {deployments.length}</Content>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      <Table aria-label="Deployments" variant="compact" gridBreakPoint="grid-md">
        <Thead>
          <Tr>
            <Th screenReaderText="Expand row" />
            <Th sort={sortParams(1)}>Name</Th>
            <Th sort={sortParams(2)}>Status</Th>
            <Th info={{
              popover: (
                <Content component="small">
                  Read from the image labels (<code>vcs-ref</code>, <code>git.url</code>) that Konflux writes at build time.
                  The merge date comes from GitHub and may show &quot;-&quot; when GitHub rate-limits the server.
                </Content>
              ),
              popoverProps: { headerContent: "Where the commit comes from" },
            }}>Commit</Th>
            <Th sort={sortParams(4)}>Built</Th>
            <Th>Version</Th>
          </Tr>
        </Thead>
        {rows.length === 0 && (
          <Tbody>
            <Tr>
              <Td colSpan={6}>
                <EmptyState headingLevel="h4" titleText={deployments.length === 0 ? "No deployments found" : "No deployments match the filters"} variant="xs">
                  {deployments.length > 0 && (
                    <EmptyStateFooter>
                      <EmptyStateActions><Button variant="link" onClick={clearFilters}>Clear filters</Button></EmptyStateActions>
                    </EmptyStateFooter>
                  )}
                  {deployments.length === 0 && <EmptyStateBody>The RHOAI namespaces have no Deployments yet.</EmptyStateBody>}
                </EmptyState>
              </Td>
            </Tr>
          </Tbody>
        )}
        {rows.map((dep, rowIndex) => {
          const key = `${dep.namespace}/${dep.name}`;
          const isExpanded = !!expanded[key];
          const health = deploymentHealth(dep);
          const shortSha = dep.gitCommit ? dep.gitCommit.slice(0, 7) : "";
          const commitURL = dep.gitCommit && dep.gitURL ? `${dep.gitURL}/commit/${dep.gitCommit}` : "";
          const pods = dep.pods ?? [];
          const canUnblock = unschedulableRollout(dep);
          const result = results[key];
          return (
            <Tbody key={key} isExpanded={isExpanded}>
              <Tr>
                <Td expand={{ rowIndex, isExpanded, onToggle: () => setExpanded((prev) => ({ ...prev, [key]: !prev[key] })) }} />
                <Td dataLabel="Name" id={`simple-node${rowIndex}`}>
                  <div>
                    <span>{dep.name}</span>
                    {dep.changeStatus === "updated" && <>{" "}<Label isCompact color="green">Updated</Label></>}
                    {dep.changeStatus === "new" && <>{" "}<Label isCompact color="blue">New</Label></>}
                    {dep.namespace !== "redhat-ods-applications" && <Content component="small">{dep.namespace}</Content>}
                  </div>
                </Td>
                <Td dataLabel="Status">
                  <div>
                    <HealthLabel health={health} />
                    {health.reason && <Content component="small" style={{ overflowWrap: "anywhere" }}>{health.reason}</Content>}
                    {health.kind === "problem" && (
                      <Content component="small"><Link to="/diagnostics">Find the cause in Diagnostics</Link></Content>
                    )}
                    {canUnblock && (
                      <TooltipButton variant="link" isInline size="sm" isLoading={unblocking === key} onClick={() => setUnblockTarget(dep)} disabledReason={mutateReason}>
                        Unblock rollout
                      </TooltipButton>
                    )}
                    {result && (
                      <Alert component="p" isInline isPlain isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "Unblock failed")}>
                        {result.success ? undefined : result.message}
                      </Alert>
                    )}
                  </div>
                </Td>
                <Td dataLabel="Commit">
                  {labelsLoading && !shortSha ? (
                    <Skeleton width="5rem" screenreaderText="Loading commit" />
                  ) : shortSha ? (
                    <div>
                      <Button variant="link" isInline component="a" href={commitURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm" aria-label={`Commit ${dep.gitCommit} on GitHub`}>
                        <code>{shortSha}</code>
                      </Button>
                      {dep.commitDate && (
                        <Content component="small">
                          merged <time dateTime={dep.commitDate} title={new Date(dep.commitDate).toLocaleString()}>{formatRelativeTime(dep.commitDate)}</time>
                        </Content>
                      )}
                    </div>
                  ) : "-"}
                </Td>
                <Td dataLabel="Built">
                  {labelsLoading && !dep.buildDate ? (
                    <Skeleton width="4rem" screenreaderText="Loading build date" />
                  ) : dep.buildDate ? (
                    <Content component="small"><time dateTime={dep.buildDate} title={new Date(dep.buildDate).toLocaleString()}>{formatRelativeTime(dep.buildDate)}</time></Content>
                  ) : "-"}
                </Td>
                <Td dataLabel="Version">
                  {labelsLoading && !dep.version ? <Skeleton width="3rem" screenreaderText="Loading version" /> : dep.version || "-"}
                </Td>
              </Tr>
              <Tr isExpanded={isExpanded}>
                <Td colSpan={6}>
                  <ExpandableRowContent>
                    <Stack hasGutter>
                      {dep.image && (
                        <StackItem>
                          <Content component="small"><strong>Image</strong></Content>
                          <ImageRef image={dep.image} truncate={false} />
                        </StackItem>
                      )}
                      {dep.rolloutMessage && (
                        <StackItem><Content component="small">Rollout: {dep.rolloutMessage}</Content></StackItem>
                      )}
                      {pods.length > 0 && (
                        <StackItem>
                          <Table aria-label={`Pods of ${dep.name}`} variant="compact" borders={false}>
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
                              {pods.map((pod) => {
                                const containers = pod.containers ?? [];
                                const podLogBase = `${consoleURL}/k8s/ns/${pod.namespace}/pods/${pod.name}/logs`;
                                return (
                                  <React.Fragment key={pod.name}>
                                    {pod.schedulingReason && (
                                      <Tr>
                                        <Td colSpan={6}>
                                          <Alert component="p" variant="warning" title={`${pod.name}: ${pod.schedulingReason}`} isInline isPlain>
                                            {pod.schedulingMessage}
                                          </Alert>
                                        </Td>
                                      </Tr>
                                    )}
                                    <Tr>
                                      <Td dataLabel="Pod"><Content component="small" style={{ overflowWrap: "anywhere" }}>{pod.name}</Content></Td>
                                      <Td dataLabel="Containers">
                                        <Flex gap={{ default: "gapXs" }} flexWrap={{ default: "wrap" }}>
                                          {containers.map((c) => (
                                            <FlexItem key={c.name}>
                                              <Label isCompact color={c.ready ? "green" : c.state === "terminated" ? "grey" : "orange"} icon={c.ready ? <CheckCircleIcon /> : <ExclamationTriangleIcon />}>
                                                {c.ready ? c.name : `${c.name}: ${c.reason || c.state || "not started"}`}
                                              </Label>
                                            </FlexItem>
                                          ))}
                                          {containers.length === 0 && <Content component="small">{pod.phase}</Content>}
                                        </Flex>
                                      </Td>
                                      <Td dataLabel="Node">
                                        {pod.phase === "Pending" && !pod.node
                                          ? <Label isCompact color="orange">Unscheduled</Label>
                                          : <Content component="small">{pod.node?.split(".")[0] || "-"}</Content>}
                                      </Td>
                                      <Td dataLabel="Restarts">{pod.restarts}</Td>
                                      <Td dataLabel="Age">{pod.age}</Td>
                                      <Td dataLabel="Logs">
                                        {!consoleURL ? null : containers.length <= 1 ? (
                                          <Button variant="link" isInline component="a" size="sm" target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end"
                                            href={containers.length === 1 ? `${podLogBase}?container=${containers[0].name}` : podLogBase}
                                            aria-label={`Logs of ${pod.name}`}>
                                            Logs
                                          </Button>
                                        ) : (
                                          <Dropdown
                                            isOpen={!!logsOpen[pod.name]}
                                            onSelect={() => setLogsOpen((prev) => ({ ...prev, [pod.name]: false }))}
                                            onOpenChange={(open) => setLogsOpen((prev) => ({ ...prev, [pod.name]: open }))}
                                            toggle={(toggleRef) => (
                                              <Button ref={toggleRef} variant="link" isInline size="sm" aria-label={`Logs of ${pod.name}`}
                                                onClick={() => setLogsOpen((prev) => ({ ...prev, [pod.name]: !prev[pod.name] }))}>
                                                <span style={{ whiteSpace: "nowrap" }}>Logs&nbsp;▾</span>
                                              </Button>
                                            )}
                                          >
                                            <DropdownList>
                                              {containers.map((c) => (
                                                <DropdownItem key={c.name} to={`${podLogBase}?container=${c.name}`} isExternalLink>
                                                  {c.name}{!c.ready && c.reason ? ` (${c.reason})` : ""}
                                                </DropdownItem>
                                              ))}
                                            </DropdownList>
                                          </Dropdown>
                                        )}
                                      </Td>
                                    </Tr>
                                  </React.Fragment>
                                );
                              })}
                            </Tbody>
                          </Table>
                        </StackItem>
                      )}
                    </Stack>
                  </ExpandableRowContent>
                </Td>
              </Tr>
            </Tbody>
          );
        })}
      </Table>

      <Modal aria-labelledby="confirm-unblock-title" variant={ModalVariant.small} isOpen={unblockTarget !== null} onClose={() => setUnblockTarget(null)}>
        <ModalHeader title={`Unblock the rollout of ${unblockTarget?.name ?? ""}?`} labelId="confirm-unblock-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">This changes one object:</Content>
              <List>
                <ListItem><code>Deployment {unblockTarget?.namespace}/{unblockTarget?.name}</code>: <code>spec.strategy.rollingUpdate.maxUnavailable</code> is set to 1.</ListItem>
              </List>
            </StackItem>
            <StackItem>
              <Content component="p">
                One old pod can then stop, so the new pod that cannot be scheduled gets its resources. The deployment may be briefly unavailable.
                The original value is saved in an annotation, and Diagnostics offers to restore it once the rollout finishes.
                The server re-checks first and changes nothing if the rollout is no longer blocked or an operator manages this field.
              </Content>
            </StackItem>
            <StackItem>
              <Alert component="p" variant="warning" title="This changes a shared cluster." isInline isPlain />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={() => unblockTarget && runUnblock(unblockTarget)}>Unblock rollout</Button>
          <Button variant="link" onClick={() => setUnblockTarget(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
