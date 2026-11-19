import React, { useState } from "react";
import {
  Button,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Flex,
  FlexItem,
  Popover,
  Spinner,
  Switch,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, TreeRowWrapper, Tr } from "@patternfly/react-table";
import type { DSCManager, DSCMismatch, DSCNode } from "../../types";
import { StatusLabel, TagLabel, type StatusKind } from "../StatusLabel";
import { TruncatedText } from "../LongText";
import { SingleSelect } from "../SingleSelect";

/** A managementState as a status label. */
function stateKind(state?: string): StatusKind {
  if (state === "Managed") return "success";
  if (state === "Unmanaged") return "info";
  return "neutral";
}

/** What the operator reports for a node: its condition, else its status managementState. */
const ActualCell: React.FC<{ node: DSCNode }> = ({ node }) => {
  const c = node.condition;
  if (!c) return node.actual ? <StatusLabel status={stateKind(node.actual)}>{node.actual}</StatusLabel> : <span className="pf-v6-u-text-color-subtle">Not reported</span>;
  const label = c.status === "True" ? "Ready" : c.reason === "Removed" ? "Removed" : c.reason || "Not ready";
  const kind: StatusKind = c.status === "True" ? "success" : c.reason === "Removed" ? "neutral" : "warning";
  const status = <StatusLabel status={kind}>{label}</StatusLabel>;
  if (kind !== "warning" || !c.message) return status;
  return <>{status}<div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle pf-v6-u-mt-xs"><TruncatedText>{c.message}</TruncatedText></div></>;
};

/** The value a row has without a choice of its own: the release default, else the CRD's default, else Removed. */
function defaultText(n: DSCNode): string {
  if (n.defaultSet) return n.default ?? "";
  return n.schemaDefault ? `${n.schemaDefault} (the CRD's default)` : "not set (Removed)";
}

// A label's onClick makes it a button, so it takes keyboard focus and has a
// name; the Popover around it opens and closes on that click.
const openPopover = () => {};

/** A row's label that explains itself in a popover. */
const ExplainedLabel: React.FC<{ kind: StatusKind | "changed"; text: string; header: string; children: React.ReactNode }> = ({ kind, text, header, children }) => (
  <Popover headerContent={header} bodyContent={children} maxWidth="28rem">
    {kind === "changed"
      ? <TagLabel color="blue" onClick={openPopover}>{text}</TagLabel>
      : <StatusLabel status={kind} onClick={openPopover}>{text}</StatusLabel>}
  </Popover>
);

/** The labels of a row: changed from the default, and each difference that is a problem. */
const RowLabels: React.FC<{ node: DSCNode; mismatches: DSCMismatch[]; byPath: Map<string, DSCNode> }> = ({ node, mismatches, byPath }) => (
  <>
    {node.changed && (
      <FlexItem>
        <ExplainedLabel kind="changed" text="Changed from the default" header="Changed from the default">
          The DSC sets {node.spec}; the release default is {defaultText(node)}. A value you chose, not a problem: Fix leaves it as it is, and Reset to the release default sets it back.
        </ExplainedLabel>
      </FlexItem>
    )}
    {mismatches.map((m) => {
      if (m.kind === "unset") {
        // Missing, but the operator already does what the release default says: only worth setting explicitly.
        const sameInEffect = (node.schemaDefault || "Removed") === m.default;
        return (
          <FlexItem key={m.kind}>
            <ExplainedLabel kind={sameInEffect ? "neutral" : "warning"} text={`Missing — default ${m.default}`} header="Missing in the DSC">
              {m.message} A release that adds a field does not add it to an existing DataScienceCluster, so this is common after an upgrade.
            </ExplainedLabel>
          </FlexItem>
        );
      }
      if (m.kind === "unknown-field") {
        return (
          <FlexItem key={m.kind}>
            <ExplainedLabel kind="danger" text="Not in the installed CRD" header="Not in the installed CRD">{m.message}</ExplainedLabel>
          </FlexItem>
        );
      }
      const parent = m.parent ? byPath.get(m.parent) : undefined;
      return (
        <FlexItem key={m.kind}>
          <ExplainedLabel kind="warning" text={`Deploys nothing — parent ${parent?.specSet ? parent.spec : "not set"}`} header="Deploys nothing">
            {m.message} Fix does not change this: choose one of the two in the table.
          </ExplainedLabel>
        </FlexItem>
      );
    })}
  </>
);

/** A camelCase field name that may wrap between its words (argoWorkflows|Controllers); inside a word only when the column is narrower than it (overflow-wrap). */
function breakableName(name: string): React.ReactNode {
  const words = name.split(/(?<=[a-z0-9])(?=[A-Z])/);
  return words.map((w, i) => <React.Fragment key={i}>{i > 0 && <wbr />}{w}</React.Fragment>);
}

/** Whether a row differs from the release default: changed, or a difference that is a problem. */
const differs = (n: DSCNode, mismatches: Map<string, DSCMismatch[]>) => !!n.changed || !!n.unknown || (mismatches.get(n.path)?.length ?? 0) > 0;

/** The tree of components and sub-components, with a state control per row. */
export const ComponentTree: React.FC<{
  manager: DSCManager;
  disabledReason: string | null;
  pending: string | null;
  onSet: (node: DSCNode, to: string) => void;
}> = ({ manager, disabledReason, pending, onSet }) => {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const [onlyDiffs, setOnlyDiffs] = useState(false);
  const nodes = manager.nodes;
  const byPath = new Map(nodes.map((n) => [n.path, n]));
  const mismatches = new Map<string, DSCMismatch[]>();
  for (const m of manager.mismatches) mismatches.set(m.path, [...(mismatches.get(m.path) ?? []), m]);
  // With only differences: the rows that differ and every row above them, for context.
  const kept = new Set<string>();
  for (const n of nodes) {
    if (!differs(n, mismatches)) continue;
    for (let p: DSCNode | undefined = n; p && !kept.has(p.path); p = p.parent ? byPath.get(p.parent) : undefined) kept.add(p.path);
  }
  const shown = onlyDiffs ? nodes.filter((n) => kept.has(n.path)) : nodes;
  const children = (path: string) => shown.filter((n) => n.parent === path);
  const hidden = (n: DSCNode): boolean => {
    for (let p = n.parent; p; p = byPath.get(p)?.parent ?? "") {
      if (collapsed[p]) return true;
    }
    return false;
  };
  // Rows the filter leaves out stay in the table, collapsed (visibility:
  // collapse, hidden from assistive technology): a collapsed row still counts
  // for the column widths, so the columns do not move when the filter changes.
  const rows = nodes.filter((n) => !hidden(n));
  const visible = rows.filter((n) => !onlyDiffs || kept.has(n.path));
  const differing = nodes.filter((n) => differs(n, mismatches)).length;
  const defaultsRead = !!manager.comparison?.defaultsRead;
  return (
    <>
      <Toolbar inset={{ default: "insetMd" }}>
        <ToolbarContent>
          <ToolbarItem>
            <Switch id="dsc-only-differences" label="Show only differences" isChecked={onlyDiffs} onChange={(_e, v) => setOnlyDiffs(v)} />
          </ToolbarItem>
          <ToolbarItem alignSelf="center">
            <span className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm">
              {!defaultsRead
                ? `No release default to compare with; ${differing === 0 ? "no row has" : differing === 1 ? "1 row has" : `${differing} rows have`} a problem the CRD shows`
                : differing === 0 ? "No row differs from the release default" : `${differing} ${differing === 1 ? "row differs" : "rows differ"} from the release default`}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>
      {visible.length === 0 ? (
        <EmptyState headingLevel="h3" titleText={defaultsRead ? "Nothing differs from the release default" : "No problems found"} variant="xs">
          <EmptyStateBody>
            {defaultsRead
              ? "Every value in the DSC is the release default's, nothing is missing, and the DSC has no field the installed CRD does not define."
              : "There is no release default to compare with, so only fields the installed CRD does not define and Managed sub-components under a parent that is not Managed are looked for: there are none."}
          </EmptyStateBody>
          <EmptyStateFooter>
            <EmptyStateActions>
              <Button variant="link" onClick={() => setOnlyDiffs(false)}>Show all components</Button>
            </EmptyStateActions>
          </EmptyStateFooter>
        </EmptyState>
      ) : (
        // A fixed layout: the column widths follow the table's width only, never its
        // content, so no column moves when an alert appears, the filter changes the
        // rows, or an operator message appears after a change; and the table never
        // grows past its card at narrow widths (names wrap, selects shrink).
        <Table isTreeTable aria-label="Components of the DataScienceCluster" variant="compact" style={{ tableLayout: "fixed" }}>
          <Thead>
            <Tr>
              <Th modifier="wrap" width={30}>Component</Th>
              <Th modifier="wrap" width={35}>In the DSC</Th>
              <Th modifier="wrap" width={15}>Release default</Th>
              <Th modifier="wrap" width={20}>Operator reports</Th>
            </Tr>
          </Thead>
          <Tbody>
            {rows.map((n, rowIndex) => {
              const filteredOut = onlyDiffs && !kept.has(n.path);
              const siblings = n.parent ? children(n.parent) : shown.filter((x) => x.depth === 0);
              const kids = children(n.path);
              const treeRow = {
                onCollapse: () => setCollapsed((c) => ({ ...c, [n.path]: !c[n.path] })),
                rowIndex,
                props: {
                  isExpanded: !collapsed[n.path],
                  "aria-level": n.depth + 1,
                  "aria-posinset": siblings.indexOf(n) + 1,
                  "aria-setsize": kids.length,
                },
              };
              const allowed = n.allowed ?? [];
              const rowReason = disabledReason ?? (allowed.length === 0 ? "The CRD lists no values for it, so this page does not change it." : null);
              return (
                <TreeRowWrapper key={n.path} row={{ props: treeRow.props }}
                  {...(filteredOut ? { "aria-hidden": true, style: { visibility: "collapse" } } : {})}>
                  <Td dataLabel="Component" treeRow={treeRow}>
                    <code style={{ overflowWrap: "anywhere" }}>{breakableName(n.name)}</code>
                  </Td>
                  <Td dataLabel="In the DSC">
                    {n.unknown ? (
                      <Flex><RowLabels node={n} mismatches={mismatches.get(n.path) ?? []} byPath={byPath} /></Flex>
                    ) : !n.toggle ? <span className="pf-v6-u-text-color-subtle">Group</span> : (
                      // The labels wrap under the select when the column has no room
                      // for them; the column keeps its width.
                      <Flex gap={{ default: "gapSm" }} rowGap={{ default: "rowGapXs" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                        {/* The value in the DSC, and the control that changes it (the CRD's values only). */}
                        {/* The same width in every row (room for "Not set (Removed)" and the caret),
                            narrower only when the column is. */}
                        <FlexItem style={{ width: "22ch", maxWidth: "100%" }}>
                          <SingleSelect id={`dsc-set-${n.path}`} label={`Set ${n.path}`} selected={n.specSet ? n.spec : undefined}
                            text={n.specSet ? n.spec ?? "" : `Not set (${n.schemaDefault || "Removed"})`} isPlaceholder={!n.specSet}
                            options={allowed.map((v) => ({ value: v, label: v }))} isDisabled={!!rowReason || pending !== null}
                            onSelect={(v) => { if (!(n.specSet && n.spec === v)) onSet(n, v); }} />
                        </FlexItem>
                        {pending === n.path && <FlexItem><Spinner size="sm" aria-label={`Changing ${n.path}`} /></FlexItem>}
                        <RowLabels node={n} mismatches={mismatches.get(n.path) ?? []} byPath={byPath} />
                      </Flex>
                    )}
                  </Td>
                  <Td dataLabel="Release default">
                    {!n.toggle ? null : !defaultsRead ? <span className="pf-v6-u-text-color-subtle">Unknown</span> : n.defaultSet ? n.default : <span className="pf-v6-u-text-color-subtle">{n.schemaDefault ? `Not set (CRD default ${n.schemaDefault})` : "Not set"}</span>}
                  </Td>
                  <Td dataLabel="Operator reports"><ActualCell node={n} /></Td>
                </TreeRowWrapper>
              );
            })}
          </Tbody>
        </Table>
      )}
    </>
  );
};
