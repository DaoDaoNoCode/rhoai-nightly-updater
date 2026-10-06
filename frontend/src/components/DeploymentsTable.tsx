import React, { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Alert,
  Badge,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Dropdown,
  DropdownItem,
  DropdownList,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Flex,
  FlexItem,
  MenuToggle,
  SearchInput,
  Select,
  SelectList,
  SelectOption,
  Skeleton,
  Stack,
  StackItem,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  Truncate,
} from "@patternfly/react-core";
import { ExpandableRowContent, Table, Tbody, Td, Th, Thead, Tr, type IAction, type ThProps } from "@patternfly/react-table";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import type { DeploymentInfo, OperationResponse } from "../types";
import { assistRolloutFor, trackFeature } from "../services/api";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { ImageRef } from "./ImageRef";
import { InlineItems } from "./InlineItems";
import { RowActions } from "./RowActions";
import { NotRecorded, RelativeTime } from "./RelativeTime";
import { StatusLabel, TagLabel, type StatusKind } from "./StatusLabel";
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

const HEALTH_STATUS: Record<DeploymentHealth["kind"], StatusKind> = {
  healthy: "success",
  "scaled-down": "neutral",
  progressing: "progress",
  problem: "danger",
};

const HealthLabel: React.FC<{ health: DeploymentHealth }> = ({ health }) => (
  <StatusLabel status={HEALTH_STATUS[health.kind] ?? "danger"}>{health.readyText}</StatusLabel>
);

/** Container waiting reasons that are part of a normal start. */
const STARTING_REASONS = new Set(["ContainerCreating", "PodInitializing"]);

function containerStatus(c: { ready: boolean; state?: string; reason?: string }): StatusKind {
  if (c.ready) return "success";
  if (c.state === "terminated") return "neutral";
  if (!c.reason || STARTING_REASONS.has(c.reason)) return "progress";
  return "danger";
}

const COLUMNS: { key: DeploymentSortKey | null; label: string }[] = [
  { key: null, label: "" },
  { key: "name", label: "Name" },
  { key: "status", label: "Status" },
  { key: null, label: "Commit" },
  { key: "built", label: "Built" },
  { key: null, label: "Version" },
];

const FILTER_LABELS: Record<StatusFilter, string> = {
  all: "All",
  attention: "Needs attention",
  healthy: "Healthy",
  changed: "Changed since the last install",
};

/** Commit, Built and Version are in the expanded row too; below lg they are hidden (UX-Components-12). */
const WIDE_ONLY: ThProps["visibility"] = ["hidden", "visibleOnLg"];

interface DeploymentsTableProps {
  deployments: DeploymentInfo[];
  labelsLoading: boolean;
  consoleURL?: string;
  /** Why cluster changes are disabled now (useMutationBlocker), or null. */
  mutateBlocker: string | null;
  /** Sees every unblock result (e.g. to pick up a cluster_busy refusal). */
  onResult?: (res: OperationResponse) => void;
  onRefresh: () => void;
}

/**
 * The Deployments table (A08-14): problems first by default, sortable by
 * name, status and build date, with a text filter and a status filter. Each
 * row expands to the full image (copyable) and its pods; problem rows have
 * their actions (Diagnostics, Unblock rollout) in a row menu.
 */
export const DeploymentsTable: React.FC<DeploymentsTableProps> = ({
  deployments,
  labelsLoading,
  consoleURL: rawConsoleURL,
  mutateBlocker,
  onResult,
  onRefresh,
}) => {
  const navigate = useNavigate();
  const [sortKey, setSortKey] = useState<DeploymentSortKey>("status");
  const [sortDirection, setSortDirection] = useState<SortDirection>("asc");
  const [filterText, setFilterText] = useState("");
  // On a phone the full list is very long: start with the rows that need attention, if any (UX-Components-12).
  const [statusFilter, setStatusFilter] = useState<StatusFilter>(() => {
    const narrow = typeof window.matchMedia === "function" && window.matchMedia("(max-width: 767px)").matches;
    return narrow && deployments.some((d) => ["problem", "progressing"].includes(deploymentHealth(d).kind)) ? "attention" : "all";
  });
  const [statusSelectOpen, setStatusSelectOpen] = useState(false);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [logsOpen, setLogsOpen] = useState<Record<string, boolean>>({});
  const [unblockTarget, setUnblockTarget] = useState<DeploymentInfo | null>(null);
  const [unblocking, setUnblocking] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, OperationResponse>>({});

  const consoleURL = rawConsoleURL?.startsWith("https://") ? rawConsoleURL : "";
  const counts = useMemo(() => {
    const c: Record<StatusFilter, number> = { all: deployments.length, attention: 0, healthy: 0, changed: 0 };
    for (const dep of deployments) {
      const kind = deploymentHealth(dep).kind;
      if (kind === "problem" || kind === "progressing") c.attention++;
      else if (kind === "healthy") c.healthy++;
      if (dep.changeStatus) c.changed++;
    }
    return c;
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

  const mutateReason = mutateBlocker ?? (unblocking ? "Another rollout is being unblocked." : null);

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
    onResult?.(res);
    onRefresh();
  };

  const clearFilters = () => {
    setFilterText("");
    setStatusFilter("all");
  };

  const statusOptions: StatusFilter[] = counts.changed > 0 ? ["all", "attention", "healthy", "changed"] : ["all", "attention", "healthy"];

  return (
    <>
      <Toolbar clearAllFilters={clearFilters} collapseListedFiltersBreakpoint="md" inset={{ default: "insetLg" }}>
        <ToolbarContent alignItems="center">
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
            <Select
              isOpen={statusSelectOpen}
              selected={statusFilter}
              onOpenChange={setStatusSelectOpen}
              onSelect={(_e, value) => {
                setStatusFilter(value as StatusFilter);
                setStatusSelectOpen(false);
              }}
              toggle={(ref) => (
                <MenuToggle
                  ref={ref}
                  onClick={() => setStatusSelectOpen(!statusSelectOpen)}
                  isExpanded={statusSelectOpen}
                  aria-label={`Filter deployments by status: ${FILTER_LABELS[statusFilter]}`}
                  badge={<Badge isRead>{counts[statusFilter]}</Badge>}
                >
                  {FILTER_LABELS[statusFilter]}
                </MenuToggle>
              )}
              shouldFocusToggleOnSelect
            >
              <SelectList aria-label="Deployment status">
                {statusOptions.map((f) => (
                  <SelectOption key={f} value={f}>
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                      <FlexItem>{FILTER_LABELS[f]}</FlexItem>
                      <FlexItem><Badge isRead>{counts[f]}</Badge></FlexItem>
                    </Flex>
                  </SelectOption>
                ))}
              </SelectList>
            </Select>
          </ToolbarItem>
          <ToolbarItem variant="pagination" align={{ default: "alignEnd" }}>
            <span className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm" aria-live="polite">{rows.length} of {deployments.length}</span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      <Table aria-label="Deployments" variant="compact" gridBreakPoint="grid-md" borders={false}>
        <Thead>
          <Tr>
            <Th screenReaderText="Expand row" />
            <Th sort={sortParams(1)} width={30}>Name</Th>
            <Th sort={sortParams(2)} width={25}>Status</Th>
            <Th
              modifier="nowrap"
              visibility={WIDE_ONLY}
              info={{
                popover: (
                  <Content component="p">
                    Read from the image labels (<code>vcs-ref</code>, <code>git.url</code>) that Konflux writes at build time.
                    The merge date comes from GitHub and may be missing when GitHub rate-limits the server.
                  </Content>
                ),
                popoverProps: { headerContent: "Where the commit comes from" },
              }}
            >
              Commit
            </Th>
            <Th sort={sortParams(4)} modifier="nowrap" visibility={WIDE_ONLY}>Built</Th>
            <Th modifier="nowrap" visibility={WIDE_ONLY}>Version</Th>
            <Th screenReaderText="Actions" />
          </Tr>
        </Thead>
        {rows.length === 0 && (
          <Tbody>
            <Tr>
              <Td colSpan={7}>
                {deployments.length === 0 ? (
                  <EmptyState headingLevel="h3" titleText="No deployments found" variant="xs">
                    <EmptyStateBody>The RHOAI namespaces have no Deployments yet.</EmptyStateBody>
                  </EmptyState>
                ) : (
                  <EmptyState headingLevel="h3" icon={SearchIcon} titleText="No deployments match the filters" variant="xs">
                    <EmptyStateFooter>
                      <EmptyStateActions><Button variant="link" onClick={clearFilters}>Clear filters</Button></EmptyStateActions>
                    </EmptyStateFooter>
                  </EmptyState>
                )}
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
          const actions: IAction[] = [];
          if (health.kind === "problem") actions.push({ title: "Find the cause in Diagnostics", onClick: () => navigate("/diagnostics") });
          if (canUnblock) {
            actions.push({
              title: unblocking === key ? "Unblocking rollout..." : "Unblock rollout",
              onClick: () => setUnblockTarget(dep),
              isAriaDisabled: !!mutateReason,
              tooltipProps: mutateReason ? { content: mutateReason } : undefined,
            });
          }
          return (
            <Tbody key={key} isExpanded={isExpanded}>
              <Tr isStriped={rowIndex % 2 === 1}>
                <Td expand={{ rowIndex, isExpanded, onToggle: () => setExpanded((prev) => ({ ...prev, [key]: !prev[key] })) }} />
                <Td dataLabel="Name" id={`simple-node${rowIndex}`}>
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                    <span>{dep.name}</span>
                    {dep.changeStatus === "updated" && <TagLabel color="blue">Changed</TagLabel>}
                    {dep.changeStatus === "new" && <TagLabel color="teal">New</TagLabel>}
                  </Flex>
                  {dep.namespace !== "redhat-ods-applications" && <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{dep.namespace}</div>}
                </Td>
                <Td dataLabel="Status">
                  <HealthLabel health={health} />
                  {health.reason && (
                    <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">
                      <Truncate content={health.reason} />
                    </div>
                  )}
                  {result && (
                    <Alert component="p" isInline isPlain isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "Unblock failed")}>
                      {result.success ? undefined : result.message}
                    </Alert>
                  )}
                </Td>
                <Td dataLabel="Commit" visibility={WIDE_ONLY}>
                  {labelsLoading && !shortSha ? (
                    <Skeleton width="5rem" screenreaderText="Loading commit" />
                  ) : shortSha ? (
                    <>
                      {commitURL ? (
                        <a href={commitURL} target="_blank" rel="noopener noreferrer" aria-label={`Commit ${dep.gitCommit} on GitHub`}><code>{shortSha}</code></a>
                      ) : <code>{shortSha}</code>}
                      {dep.commitDate && <div className="pf-v6-u-text-nowrap"><RelativeTime date={dep.commitDate} prefix="merged " /></div>}
                    </>
                  ) : <NotRecorded />}
                </Td>
                <Td dataLabel="Built" modifier="nowrap" visibility={WIDE_ONLY}>
                  {labelsLoading && !dep.buildDate ? (
                    <Skeleton width="4rem" screenreaderText="Loading build date" />
                  ) : dep.buildDate ? (
                    <RelativeTime date={dep.buildDate} />
                  ) : <NotRecorded />}
                </Td>
                <Td dataLabel="Version" visibility={WIDE_ONLY}>
                  {labelsLoading && !dep.version ? <Skeleton width="3rem" screenreaderText="Loading version" /> : dep.version || <NotRecorded />}
                </Td>
                <Td isActionCell>
                  {actions.length > 0 && <RowActions items={actions} rowName={dep.name} />}
                </Td>
              </Tr>
              <Tr isExpanded={isExpanded}>
                <Td colSpan={7}>
                  <ExpandableRowContent>
                    <Stack hasGutter>
                      <StackItem>
                        <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "10ch" }} aria-label={`Details of ${dep.name}`}>
                          {dep.image && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Image</DescriptionListTerm>
                              <DescriptionListDescription><ImageRef image={dep.image} /></DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          {shortSha && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Commit</DescriptionListTerm>
                              <DescriptionListDescription>
                                <InlineItems><code>{dep.gitCommit}</code>{dep.version ? `version ${dep.version}` : null}</InlineItems>
                              </DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                          {dep.rolloutMessage && (
                            <DescriptionListGroup>
                              <DescriptionListTerm>Rollout</DescriptionListTerm>
                              <DescriptionListDescription>{dep.rolloutMessage}</DescriptionListDescription>
                            </DescriptionListGroup>
                          )}
                        </DescriptionList>
                      </StackItem>
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
                                      <Td dataLabel="Pod" className="pf-v6-u-text-break-word">{pod.name}</Td>
                                      <Td dataLabel="Containers">
                                        <Flex gap={{ default: "gapXs" }} flexWrap={{ default: "wrap" }}>
                                          {containers.map((c) => (
                                            <FlexItem key={c.name}>
                                              <StatusLabel status={containerStatus(c)}>
                                                {c.ready ? c.name : `${c.name}: ${c.reason || c.state || "not started"}`}
                                              </StatusLabel>
                                            </FlexItem>
                                          ))}
                                          {containers.length === 0 && <FlexItem>{pod.phase}</FlexItem>}
                                        </Flex>
                                      </Td>
                                      <Td dataLabel="Node">
                                        {pod.phase === "Pending" && !pod.node
                                          ? <StatusLabel status="warning">Unscheduled</StatusLabel>
                                          : pod.node?.split(".")[0] || <span className="pf-v6-u-text-color-subtle">None</span>}
                                      </Td>
                                      <Td dataLabel="Restarts">{pod.restarts}</Td>
                                      <Td dataLabel="Age">{pod.age}</Td>
                                      <Td dataLabel="Logs">
                                        {!consoleURL ? null : containers.length <= 1 ? (
                                          <a
                                            href={containers.length === 1 ? `${podLogBase}?container=${containers[0].name}` : podLogBase}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            aria-label={`Logs of ${pod.name}`}
                                          >
                                            Logs
                                          </a>
                                        ) : (
                                          <Dropdown
                                            isOpen={!!logsOpen[pod.name]}
                                            onSelect={() => setLogsOpen((prev) => ({ ...prev, [pod.name]: false }))}
                                            onOpenChange={(open) => setLogsOpen((prev) => ({ ...prev, [pod.name]: open }))}
                                            toggle={(toggleRef) => (
                                              <MenuToggle
                                                ref={toggleRef}
                                                variant="plainText"
                                                size="sm"
                                                aria-label={`Logs of ${pod.name}`}
                                                isExpanded={!!logsOpen[pod.name]}
                                                onClick={() => setLogsOpen((prev) => ({ ...prev, [pod.name]: !prev[pod.name] }))}
                                              >
                                                Logs
                                              </MenuToggle>
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

      <ConfirmActionModal
        isOpen={unblockTarget !== null}
        title={`Unblock the rollout of ${unblockTarget?.name ?? ""}?`}
        changes={[
          <><code>Deployment {unblockTarget?.namespace}/{unblockTarget?.name}</code>: <code>spec.strategy.rollingUpdate.maxUnavailable</code> is set to 1.</>,
        ]}
        confirmLabel="Unblock rollout"
        confirmDisabled={!!mutateBlocker}
        onConfirm={() => unblockTarget && runUnblock(unblockTarget)}
        onCancel={() => setUnblockTarget(null)}
      >
        <Content component="p">
          One old pod can then stop, so the new pod that cannot be scheduled gets its resources. The deployment may be briefly unavailable.
          The original value is saved in an annotation, and Diagnostics offers to restore it once the rollout finishes.
          The server re-checks first and changes nothing if the rollout is no longer blocked or an operator manages this field.
        </Content>
      </ConfirmActionModal>
    </>
  );
};
