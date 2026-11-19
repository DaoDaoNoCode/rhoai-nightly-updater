import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Checkbox,
  CodeBlock,
  CodeBlockCode,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  ExpandableSection,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Popover,
  Radio,
  Spinner,
  Stack,
  StackItem,
  Switch,
  TextInput,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, TreeRowWrapper, Tr } from "@patternfly/react-table";
import type { DSCChange, DSCManager, DSCMismatch, DSCNode, DSCPendingHardReset, DSCPlan, OperationResponse, UpdateStep } from "../../types";
import { finishDSCHardReset, getDSCManager, getDSCPlan, runDSCPlan, setDSCManagementState, streamDSCHardReset, toApiError } from "../../services/api";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { StatusLabel, TagLabel, type StatusKind } from "../StatusLabel";
import { TooltipButton } from "../TooltipButton";
import { TechnicalDetails, TruncatedText } from "../LongText";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { errorResult, outcomeTitle, outcomeVariant } from "../../outcomes";

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

/** A change of a plan in words. */
export function changeText(ch: DSCChange): React.ReactNode {
  const from = ch.fromSet ? ch.from : "not set";
  const to = ch.toSet ? ch.to : "not set";
  const effect = ch.effect === "deploys" ? " (deploys it)" : ch.effect === "removes" ? " (removes it)" : "";
  if (from === to) {
    return <><code>{ch.path}</code> stays {from}{ch.effect === "removes" ? ", but deploys nothing: its parent is no longer Managed" : ch.effect === "deploys" ? " and is deployed again: its parent is Managed" : ""}</>;
  }
  return <><code>{ch.path}</code>: {from} → {to}{effect}</>;
}

/** Paths as a comma-separated list of code. */
const PathList: React.FC<{ paths: string[] }> = ({ paths }) => (
  <>{paths.map((p, i) => <React.Fragment key={p}>{i > 0 && ", "}<code>{p}</code></React.Fragment>)}</>
);

/** The blocked removals and warnings of a plan. */
const PlanAlerts: React.FC<{ plan: DSCPlan }> = ({ plan }) => (
  <Stack hasGutter>
    {(plan.blocked?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="danger" isInline title="This change is not possible now">
          <Content component="p">These components must not be removed yet; nothing is changed while any is listed. Resolve the reasons and try again.</Content>
          <List>{plan.blocked!.map((b) => <ListItem key={b.component}><code>{b.component}</code>: {b.reasons.join(" ")}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
    {(plan.warnings?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="warning" isInline title="Before you confirm">
          <List>{plan.warnings!.map((w) => <ListItem key={w}>{w}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
  </Stack>
);

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

/** Whether a row differs from the release default: changed, or a difference that is a problem. */
const differs = (n: DSCNode, mismatches: Map<string, DSCMismatch[]>) => !!n.changed || !!n.unknown || (mismatches.get(n.path)?.length ?? 0) > 0;

/** The tree of components and sub-components, with a state control per row. */
const ComponentTree: React.FC<{
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
        <Table isTreeTable aria-label="Components of the DataScienceCluster" variant="compact">
          <Thead>
            <Tr>
              {/* Set widths: an operator message appearing after a change must not take
                  the room of the select and its label (it wraps in its own column). */}
              <Th modifier="wrap" width={25}>Component</Th>
              <Th modifier="wrap" width={40}>In the DSC</Th>
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
                    <code>{n.name}</code>
                  </Td>
                  <Td dataLabel="In the DSC">
                    {n.unknown ? (
                      <Flex><RowLabels node={n} mismatches={mismatches.get(n.path) ?? []} byPath={byPath} /></Flex>
                    ) : !n.toggle ? <span className="pf-v6-u-text-color-subtle">Group</span> : (
                      // The labels wrap under the select when the column has no room
                      // for them; the column keeps its width.
                      <Flex gap={{ default: "gapSm" }} rowGap={{ default: "rowGapXs" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                        {/* The value in the DSC, and the control that changes it (the CRD's values only). */}
                        <FlexItem className="pf-v6-u-min-width" style={{ "--pf-v6-u-min-width--MinWidth": "22ch" } as React.CSSProperties}>
                          <FormSelect aria-label={`Set ${n.path}`} id={`dsc-set-${n.path}`} value={n.specSet ? n.spec : ""}
                            isDisabled={!!rowReason || pending !== null}
                            onChange={(_e, v) => { if (v && !(n.specSet && n.spec === v)) onSet(n, v); }}>
                            {!n.specSet && <FormSelectOption value="" label={`Not set (${n.schemaDefault || "Removed"})`} isDisabled />}
                            {allowed.map((v) => <FormSelectOption key={v} value={v} label={v} />)}
                          </FormSelect>
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

/** Paths as "<count>: a, b (note)", or "None". */
const CountedPaths: React.FC<{ items: { path: string; note?: string }[] }> = ({ items }) => (
  items.length === 0 ? <>None</> : (
    <>{items.length}: {items.map((it, i) => <React.Fragment key={it.path}>{i > 0 && ", "}<code>{it.path}</code>{it.note && ` (${it.note})`}</React.Fragment>)}</>
  )
);

/** What the comparison means, behind a link in the summary. */
const ComparisonHelp: React.FC = () => (
  <Popover
    headerContent="About this comparison"
    maxWidth="36rem"
    bodyContent={
      <Stack hasGutter>
        <StackItem>
          The release default is the DataScienceCluster example the installed operator&apos;s CSV ships (<code>alm-examples</code>). Each row compares the value in your DSC with it:
        </StackItem>
        <StackItem>
          <List>
            <ListItem><strong>Changed from the default</strong>: a value you chose. Not a problem; Fix leaves it.</ListItem>
            <ListItem><strong>Missing</strong>: the release default sets it, your DSC does not, so the operator uses the CRD&apos;s default for it or treats it as Removed. Fix sets it to the release default.</ListItem>
            <ListItem><strong>Not in the installed CRD</strong>: the operator never sees it. Fix removes it.</ListItem>
            <ListItem><strong>Deploys nothing</strong>: Managed under a parent that is not Managed. Set the parent to Managed, or this to Removed.</ListItem>
          </List>
        </StackItem>
        <StackItem>
          <strong>Deprecated fields.</strong> The CRD marks only whole API versions as deprecated, not single fields, so this page does not list deprecated fields.
          A field the installed CRD no longer defines cannot stay in the DSC: the API server prunes fields the CRD does not define.
          So after an upgrade, what matters is what the new release default sets that your DSC lacks: those rows show <strong>Missing</strong>.
          Components renamed between API versions (v2 to v3) are converted by the operator&apos;s conversion webhook.
        </StackItem>
      </Stack>
    }
  >
    <Button variant="link" isInline>About this comparison</Button>
  </Popover>
);

/**
 * The comparison with the release default in one place, always shown: what
 * was compared with, and for each kind of difference its count and paths or
 * "None", or "Not checked" when it could not be.
 */
const ComparisonSummary: React.FC<{ manager: DSCManager; fix: React.ReactNode }> = ({ manager, fix }) => {
  const cmp = manager.comparison;
  const defaultsRead = !!cmp?.defaultsRead;
  const schemaRead = !!cmp?.schemaRead;
  const of = (kind: DSCMismatch["kind"]) => manager.mismatches.filter((m) => m.kind === kind);
  const missing = of("unset");
  const unknown = of("unknown-field");
  const parentRemoved = of("parent-removed");
  const changed = manager.nodes.filter((n) => n.changed).map((n) => ({ path: n.path }));
  const problems = missing.length + unknown.length + parentRemoved.length;
  const byPath = new Map(manager.nodes.map((n) => [n.path, n]));
  // Missing where the operator already does what the release default says is only worth setting explicitly.
  const missingInEffect = missing.some((m) => (byPath.get(m.path)?.schemaDefault || "Removed") !== m.default);
  const defaultComps = cmp?.defaultComponents ?? [];
  const dscComps = cmp?.dscComponents ?? [];
  const absent = defaultComps.filter((c) => !dscComps.includes(c));
  const notChecked = "Not checked: no release default";

  let variant: "success" | "info" | "warning" | "danger" = "success";
  if (unknown.length > 0) variant = "danger";
  else if (!defaultsRead || parentRemoved.length > 0 || missingInEffect) variant = "warning";
  else if (problems > 0) variant = "info";
  const title = !defaultsRead
    ? "Not compared with a release default"
    : problems === 0
      ? "Nothing to fix compared with the release default"
      : `${problems} ${problems === 1 ? "difference" : "differences"} from the release default to fix`;

  return (
    <Alert component="h3" isInline isPlain={variant === "success"} variant={variant} title={title}
      actionLinks={<>{fix}<ComparisonHelp /></>}>
      <DescriptionList isHorizontal isCompact aria-label="Comparison with the release default" horizontalTermWidthModifier={{ default: "14rem" }}>
        <DescriptionListGroup>
          <DescriptionListTerm>Release default</DescriptionListTerm>
          <DescriptionListDescription>
            {defaultsRead
              ? <>Operator {manager.operatorVersion}: the DataScienceCluster example in CSV <code>{cmp?.defaultsCSV}</code> (<code>alm-examples</code>)</>
              : manager.defaultsError || "Could not be read"}
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Components</DescriptionListTerm>
          <DescriptionListDescription>
            {!defaultsRead
              ? `${dscComps.length} in the DSC; the installed CRD defines ${cmp?.crdComponents?.length ?? 0}`
              : absent.length === 0
                ? `All ${defaultComps.length} components of the release default are in the DSC`
                : <>{defaultComps.length - absent.length} of {defaultComps.length} components of the release default are in the DSC; not in it: <PathList paths={absent} /></>}
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Missing</DescriptionListTerm>
          <DescriptionListDescription>
            {defaultsRead ? <CountedPaths items={missing.map((m) => ({ path: m.path, note: `default ${m.default}` }))} /> : notChecked}
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Not in the installed CRD</DescriptionListTerm>
          <DescriptionListDescription>
            {schemaRead ? <CountedPaths items={unknown.map((m) => ({ path: m.path }))} /> : `Not checked: ${manager.schemaError || "the CRD's schema could not be read"}`}
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Deploys nothing</DescriptionListTerm>
          <DescriptionListDescription>
            {schemaRead ? <CountedPaths items={parentRemoved.map((m) => ({ path: m.path, note: `parent ${m.parent}` }))} /> : "Not checked: the CRD's schema could not be read"}
          </DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Changed from the default</DescriptionListTerm>
          <DescriptionListDescription>
            {defaultsRead ? <CountedPaths items={changed} /> : notChecked}
          </DescriptionListDescription>
        </DescriptionListGroup>
      </DescriptionList>
    </Alert>
  );
};

type HardStep = { id: string; title: string };
const HARD_RESET_STEPS: HardStep[] = [
  { id: "check", title: "Check the DataScienceCluster, the operator and every component" },
  { id: "delete", title: "Delete the DataScienceCluster" },
  { id: "wait_deleted", title: "Wait for the operator to remove every component" },
  { id: "create", title: "Create the release default" },
  { id: "wait_ready", title: "Wait for the components" },
];

/** Reset to the release default: soft (in place) or hard (delete and recreate). */
const ResetModal: React.FC<{
  name: string;
  disabledReason: string | null;
  onClose: () => void;
  onDone: (res: OperationResponse) => void;
}> = ({ name, disabledReason, onClose, onDone }) => {
  const titleId = React.useId();
  const [kind, setKind] = useState<"soft-reset" | "hard-reset">("soft-reset");
  // Keep the components the user turned on: checked by default; the plan is
  // made again when it changes.
  const [keep, setKeep] = useState(true);
  // The toggles the user turned on, from the last plan (kept while a new
  // plan loads, so the option does not flicker).
  const [turnedOn, setTurnedOn] = useState<string[]>([]);
  const [plan, setPlan] = useState<DSCPlan | null>(null);
  const [planError, setPlanError] = useState("");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [steps, setSteps] = useState<Record<string, StepState>>({});
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [yamlOpen, setYamlOpen] = useState(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const started = Object.keys(steps).length > 0;

  useEffect(() => {
    let active = true;
    setPlan(null);
    setPlanError("");
    getDSCPlan(kind, undefined, { keepEnabled: keep })
      .then((p) => { if (active) { setPlan(p); setTurnedOn(p.turnedOn ?? []); } })
      .catch((e) => { if (active) setPlanError(toApiError(e).message); });
    return () => { active = false; };
  }, [kind, keep]);

  const blocked = !plan || !!plan.refusal || (plan.blocked?.length ?? 0) > 0;
  const confirm = async () => {
    if (!plan || blocked) return;
    setBusy(true);
    if (kind === "soft-reset") {
      let res: OperationResponse;
      try {
        res = await runDSCPlan("soft-reset", plan);
      } catch (e) {
        res = errorResult(e, "Could not reset the DataScienceCluster");
      }
      setBusy(false);
      onDone(res);
      onClose();
      return;
    }
    let final = "";
    controller.current = streamDSCHardReset(plan, typed,
      (step: UpdateStep) => {
        if (step.step === "operation_complete") { final = step.message; return; }
        setSteps((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
      },
      (success, error, apiError) => {
        const res: OperationResponse = { success, message: success ? final : error || final || "The hard reset failed.", logs: [], errorCode: apiError?.errorCode };
        setBusy(false);
        setResult(res);
        onDone(res);
      },
      () => {
        const res: OperationResponse = { success: false, message: "The connection to the server was lost. The hard reset continues on the server; this page shows the DataScienceCluster when it ends.", logs: [], errorCode: "detached" };
        setBusy(false);
        setResult(res);
        onDone(res);
      });
  };

  const removals = plan?.changes.filter((c) => c.effect === "removes") ?? [];
  const deploys = plan?.changes.filter((c) => c.effect === "deploys") ?? [];
  const kept = plan?.kept ?? [];
  // A blocked removal of a component the user turned on: keeping them avoids it.
  const keepWouldHelp = !keep && (plan?.blocked ?? []).some((b) => turnedOn.includes(b.component));
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={() => !busy && onClose()}>
      <ModalHeader title="Reset to the release default" labelId={titleId} titleIconVariant={kind === "hard-reset" ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {!started && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup role="radiogroup" isStack fieldId="dsc-reset-kind" label="How">
                  <Radio id="dsc-reset-soft" name="dsc-reset-kind" isChecked={kind === "soft-reset"} onChange={() => setKind("soft-reset")}
                    label="Soft reset (recommended)"
                    description={<>Changes <code>{name}</code> in place: only the components and settings that differ from the release default. Namespaces of workbenches and the AI hub are kept.</>} />
                  <Radio id="dsc-reset-hard" name="dsc-reset-kind" isChecked={kind === "hard-reset"} onChange={() => setKind("hard-reset")}
                    label="Hard reset"
                    description={<>Deletes <code>{name}</code> and creates the release default in its place. Use it when components are stuck and a soft reset does not help.</>} />
                </FormGroup>
                {turnedOn.length > 0 && (
                  <Checkbox id="dsc-reset-keep" isChecked={keep} onChange={(_e, v) => setKeep(v)}
                    label={<>Keep the components you turned on ({turnedOn.length}): <PathList paths={turnedOn} /></>}
                    description="Enabled now, but off in the release default. Unchecked, the reset turns them off." />
                )}
              </Form>
            </StackItem>
          )}
          {planError && <StackItem><Alert component="p" variant="danger" isInline title="Could not work out the reset">{planError}</Alert></StackItem>}
          {!plan && !planError && <StackItem><Spinner size="md" aria-label="Working out the reset" /></StackItem>}
          {plan?.refusal && <StackItem><Alert component="p" variant={plan.code === "nothing_to_do" ? "info" : "warning"} isInline title={plan.code === "nothing_to_do" ? "Nothing to change" : "Not possible now"}>{plan.refusal}</Alert></StackItem>}
          {plan && !plan.refusal && !started && kind === "soft-reset" && (
            <StackItem>
              <Content component="p">This changes the shared cluster:</Content>
              <List aria-label="Changes of the reset">{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
              {kept.length > 0 && <Content component="p" className="pf-v6-u-mt-sm">Kept: <PathList paths={kept} /> (you turned these on).</Content>}
            </StackItem>
          )}
          {plan && !plan.refusal && !started && kind === "hard-reset" && (
            <>
              <StackItem>
                <Alert component="div" variant="danger" isInline title="Every component is torn down, then comes back as the release default">
                  <List>
                    <ListItem>The operator removes every component ({(plan.tornDown ?? []).join(", ") || "none enabled"}), then the release default brings back what it enables. This takes minutes; RHOAI is unavailable meanwhile.</ListItem>
                    <ListItem>Your resources (notebooks, pipeline servers, models, model registries) are kept, but stop working until their components are back.</ListItem>
                    {(plan.stayRemoved?.length ?? 0) > 0 && (
                      <ListItem>Enabled now, but Removed in the release default, so they stay Removed: <PathList paths={plan.stayRemoved!} />. Turn them on again afterwards if you need them.</ListItem>
                    )}
                    {kept.length > 0 && <ListItem>Kept: <PathList paths={kept} /> (you turned these on). They come back as they are now.</ListItem>}
                    <ListItem>If the old DataScienceCluster is not gone within 10 minutes, nothing is created and the reset stops; the finalizer is never removed by hand. Once it is gone, this page offers to finish the reset with the same object.</ListItem>
                  </List>
                </Alert>
              </StackItem>
              {plan.changes.length > 0 && (
                <StackItem>
                  <Content component="p">Compared with now, the new DataScienceCluster differs in:</Content>
                  <List aria-label="Differences of the new DataScienceCluster">{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
                </StackItem>
              )}
              <StackItem>
                <ExpandableSection toggleText="The DataScienceCluster it creates" isExpanded={yamlOpen} onToggle={(_e, v) => setYamlOpen(v)}>
                  <CodeBlock><CodeBlockCode>{plan.newObject}</CodeBlockCode></CodeBlock>
                </ExpandableSection>
              </StackItem>
            </>
          )}
          {plan && !started && <StackItem><PlanAlerts plan={plan} /></StackItem>}
          {plan && !started && keepWouldHelp && (
            <StackItem>
              <Content component="p">Check <strong>Keep the components you turned on</strong> to reset everything else and keep these as they are.</Content>
            </StackItem>
          )}
          {plan && !plan.refusal && !started && kind === "hard-reset" && !blocked && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label={<>Type <code>{name}</code> to confirm</>} fieldId="dsc-hard-reset-confirm" isRequired>
                  <TextInput id="dsc-hard-reset-confirm" value={typed} onChange={(_e, v) => setTyped(v)} autoComplete="off" />
                </FormGroup>
              </Form>
            </StackItem>
          )}
          {started && (
            <StackItem>
              <StepProgress idPrefix="dsc-hard-reset" aria-label="Hard reset progress" steps={HARD_RESET_STEPS} states={steps} running={busy}
                shownElsewhere={result && !result.success ? result.message : undefined} />
            </StackItem>
          )}
          {result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "The hard reset stopped")}>{result.success ? undefined : result.message}</Alert>
            </StackItem>
          )}
          {kind === "soft-reset" && plan && !plan.refusal && (removals.length > 0 || deploys.length > 0) && (
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">
                {removals.length > 0 && <>{removals.length} {removals.length === 1 ? "component is" : "components are"} removed. </>}
                {deploys.length > 0 && <>{deploys.length} {deploys.length === 1 ? "component is" : "components are"} deployed.</>}
              </Content>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {result || started ? (
          <Button variant="primary" onClick={onClose} isDisabled={busy}>Close</Button>
        ) : (
          <>
            <TooltipButton variant={kind === "hard-reset" ? "danger" : "primary"} onClick={confirm} isLoading={busy}
              isDisabled={busy || blocked || !!disabledReason || (kind === "hard-reset" && typed !== name)} disabledReason={disabledReason}>
              {kind === "hard-reset" ? "Hard reset" : "Reset"}
            </TooltipButton>
            <Button variant="link" onClick={onClose} isDisabled={busy}>Cancel</Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};

/** A hard reset that deleted the DataScienceCluster and did not create its replacement, while none exists. */
const FinishHardReset: React.FC<{
  pending?: DSCPendingHardReset;
  error?: string;
  disabledReason: string | null;
  onDone: (res: OperationResponse) => void;
}> = ({ pending, error, disabledReason, onDone }) => {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [yamlOpen, setYamlOpen] = useState(false);
  const finish = async () => {
    if (!pending) return;
    setBusy(true);
    let res: OperationResponse;
    try {
      res = await finishDSCHardReset(pending.name, pending.token);
    } catch (e) {
      res = errorResult(e, "Could not finish the hard reset");
    }
    setBusy(false);
    setConfirming(false);
    onDone(res);
  };
  return (
    <PageSection>
      <Card>
        <CardHeader actions={pending ? {
          actions: (
            <TooltipButton variant="primary" onClick={() => setConfirming(true)} isDisabled={!!disabledReason || busy} disabledReason={disabledReason}>
              Finish the hard reset…
            </TooltipButton>
          ),
          hasNoOffset: false,
        } : undefined}>
          <CardTitle><Title headingLevel="h2" size="lg">A hard reset did not finish</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            {error && <StackItem><Alert component="p" variant="danger" isInline title="Could not read the recorded hard reset">{error}</Alert></StackItem>}
            {pending && (
              <>
                <StackItem>
                  <Content component="p">
                    A hard reset{pending.user ? <> by {pending.user}</> : null} at {pending.startedAt} deleted DataScienceCluster <code>{pending.name}</code> and did not create its replacement.
                    Finish it to create exactly the object it planned: the release default of operator {pending.operatorVersion || "(unknown)"}, with the old name and namespaces.
                    Create DataScienceCluster would create the release default without the old name and namespaces instead.
                  </Content>
                </StackItem>
                <StackItem>
                  <ExpandableSection toggleText="The DataScienceCluster it creates" isExpanded={yamlOpen} onToggle={(_e, v) => setYamlOpen(v)}>
                    <CodeBlock><CodeBlockCode>{pending.newObject}</CodeBlockCode></CodeBlock>
                  </ExpandableSection>
                </StackItem>
              </>
            )}
          </Stack>
        </CardBody>
      </Card>
      {pending && confirming && (
        <ConfirmActionModal
          isOpen
          title="Finish the hard reset?"
          changes={[<>Creates DataScienceCluster <code>{pending.name}</code> ({pending.apiVersion}) exactly as the hard reset planned it.</>]}
          changesOpen
          confirmLabel={`Create ${pending.name}`}
          isLoading={busy}
          confirmDisabled={busy || !!disabledReason}
          onConfirm={finish}
          onCancel={() => !busy && setConfirming(false)}
        >
          <Content component="p" className="pf-v6-u-text-color-subtle">
            The server checks again that no DataScienceCluster exists and that the operator is the one the reset planned for, then runs a dry run first.
          </Content>
        </ConfirmActionModal>
      )}
    </PageSection>
  );
};

/** A change waiting for confirmation: a toggle or a fix. */
interface PendingChange {
  title: string;
  confirmLabel: string;
  toggle?: { node: DSCNode; to: string; withParent: boolean };
  plan: DSCPlan | null;
  error?: string;
}

/**
 * The DataScienceCluster's components and sub-components from the installed
 * CRD, compared with the release default, with a state control per row,
 * the differences that are real problems (Fix), and Reset to the release
 * default. Every change is confirmed with its exact change list.
 */
export const DSCManagerSection: React.FC<{
  /** Reloads the tree when it changes (the page's last refresh). */
  refreshKey: unknown;
  /** Why changes are off for this user or profile, or null. */
  mutateReason: string | null;
  onResult: (res: OperationResponse) => void;
  onChanged: () => void;
}> = ({ refreshKey, mutateReason, onResult, onChanged }) => {
  const [manager, setManager] = useState<DSCManager | null>(null);
  const [loadError, setLoadError] = useState("");
  const [pending, setPending] = useState<PendingChange | null>(null);
  const [running, setRunning] = useState<string | null>(null);
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [resetOpen, setResetOpen] = useState(false);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let active = true;
    getDSCManager().then((m) => { if (active) { setManager(m); setLoadError(""); } })
      .catch((e) => { if (active) setLoadError(toApiError(e).message); });
    return () => { active = false; };
  }, [refreshKey, reload]);

  const changed = useCallback((res: OperationResponse) => {
    setResult(res);
    onResult(res);
    onChanged();
    setReload((n) => n + 1);
  }, [onChanged, onResult]);

  const openPlan = (p: Omit<PendingChange, "plan">) => {
    setPending({ ...p, plan: null });
    const req = p.toggle ? getDSCPlan("toggle", { path: p.toggle.node.path, to: p.toggle.to, withParent: p.toggle.withParent }) : getDSCPlan("fix");
    req.then((plan) => setPending((cur) => (cur && cur.title === p.title && cur.toggle?.withParent === p.toggle?.withParent ? { ...cur, plan } : cur)))
      .catch((e) => setPending((cur) => (cur ? { ...cur, error: toApiError(e).message } : cur)));
  };

  if (!manager && !loadError) return null;
  if (loadError && !manager) {
    return (
      <PageSection>
        <Alert component="p" variant="warning" isInline title="Could not load the component tree">{loadError}</Alert>
      </PageSection>
    );
  }
  if (!manager) return null;
  if (!manager.exists) {
    if (!manager.pendingHardReset && !manager.pendingHardResetError) return null;
    return <FinishHardReset pending={manager.pendingHardReset} error={manager.pendingHardResetError} disabledReason={mutateReason} onDone={changed} />;
  }

  const disabledReason = mutateReason ?? (manager.readOnlyReason || null);
  const fixable = manager.mismatches.filter((m) => m.fixable);

  const confirmPending = async () => {
    if (!pending?.plan || !manager.name) return;
    const p = pending;
    setPending(null);
    setResult(null);
    let res: OperationResponse;
    try {
      if (p.toggle) {
        setRunning(p.toggle.node.path);
        res = await setDSCManagementState({ name: manager.name, path: p.toggle.node.path, from: p.toggle.node.specSet ? p.toggle.node.spec ?? "" : "", to: p.toggle.to, withParent: p.toggle.withParent, uid: p.plan!.uid ?? "", token: p.plan!.token ?? "" });
      } else {
        setRunning("fix");
        res = await runDSCPlan("fix", p.plan!);
      }
    } catch (e) {
      res = errorResult(e, "Could not change the DataScienceCluster");
    }
    setRunning(null);
    changed(res);
  };

  const plan = pending?.plan;
  const planBlocked = !plan || !!plan.refusal || (plan.blocked?.length ?? 0) > 0;
  return (
    <PageSection>
      <Card>
        <CardHeader actions={{
          actions: (
            <TooltipButton variant="secondary" onClick={() => setResetOpen(true)} isDisabled={!!disabledReason || !!manager.defaultsError || running !== null} disabledReason={disabledReason ?? (manager.defaultsError || null)}>
              Reset to the release default…
            </TooltipButton>
          ),
          hasNoOffset: false,
        }}>
          <CardTitle><Title headingLevel="h2" size="lg">Components and sub-components</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            <StackItem>
              <ComparisonSummary manager={manager} fix={fixable.length > 0 ? (
                <TooltipButton variant="link" isInline isDisabled={!!disabledReason || running !== null} disabledReason={disabledReason}
                  onClick={() => openPlan({ title: "Fix the differences from the release default?", confirmLabel: "Fix" })}>
                  Fix
                </TooltipButton>
              ) : null} />
            </StackItem>
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">
                From the installed DataScienceCluster CRD ({manager.apiVersion?.split("/")[1]}). A change here patches only the field you change.
              </Content>
            </StackItem>
            {manager.readOnlyReason && <StackItem><Alert component="p" variant="info" isInline title="This page does not change the DataScienceCluster now">{manager.readOnlyReason}</Alert></StackItem>}
            {(manager.upgradeGates?.length ?? 0) > 0 && (
              <StackItem>
                <Alert component="div" variant="warning" isInline title="An upgrade gate holds provisioning">
                  <Content component="p">The operator waits for an admin acknowledgement in ConfigMap <code>odh-upgrade-acks</code>. Diagnostics shows the command.</Content>
                  <TechnicalDetails text={manager.upgradeGates!} />
                </Alert>
              </StackItem>
            )}
            {result && (
              <StackItem>
                <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "Could not change the DataScienceCluster")}
                  actionClose={<AlertActionCloseButton onClose={() => setResult(null)} />}>
                  {result.success ? undefined : result.message}
                </Alert>
              </StackItem>
            )}
          </Stack>
        </CardBody>
        <CardBody className="pf-v6-u-px-0">
          <ComponentTree manager={manager} disabledReason={disabledReason} pending={running}
            onSet={(node, to) => openPlan({ title: `Set ${node.path} to ${to}?`, confirmLabel: `Set to ${to}`, toggle: { node, to, withParent: false } })} />
        </CardBody>
      </Card>

      {pending && (
        <ConfirmActionModal
          isOpen
          title={pending.title}
          changes={plan && !plan.refusal ? plan.changes.map((c) => changeText(c)) : []}
          summary={!plan ? (pending.error ? <Content component="p">Nothing is changed.</Content> : <Spinner size="md" aria-label="Working out the change" />) : plan.refusal ? <Content component="p">Nothing would change.</Content> : undefined}
          changesOpen
          confirmLabel={pending.confirmLabel}
          confirmVariant={plan?.changes.some((c) => c.effect === "removes") ? "danger" : "primary"}
          confirmDisabled={planBlocked || !!disabledReason}
          onConfirm={confirmPending}
          onCancel={() => setPending(null)}
        >
          <Stack hasGutter>
            {pending.error && <StackItem><Alert component="p" variant="danger" isInline title="Could not work out the change">{pending.error}</Alert></StackItem>}
            {plan?.refusal && (
              <StackItem>
                <Alert component="div" variant="warning" isInline title={plan.code === "parent_removed" ? "Its parent is not Managed" : "Not possible now"}>
                  <Content component="p">{plan.refusal}</Content>
                  {plan.code === "parent_removed" && pending.toggle && plan.parentPath && (
                    <Button variant="link" isInline onClick={() => openPlan({ ...pending, title: `Set ${pending.toggle!.node.path} and ${plan.parentPath} to Managed?`, toggle: { ...pending.toggle!, withParent: true } })}>
                      Also set {plan.parentPath} to Managed
                    </Button>
                  )}
                </Alert>
              </StackItem>
            )}
            {plan && <StackItem><PlanAlerts plan={plan} /></StackItem>}
            {plan && !plan.refusal && (
              <StackItem>
                <Content component="p" className="pf-v6-u-text-color-subtle">
                  The server checks the DataScienceCluster again, runs a dry run, and changes nothing if any of these fields changed since you looked.
                </Content>
              </StackItem>
            )}
          </Stack>
        </ConfirmActionModal>
      )}

      {resetOpen && manager.name && (
        <ResetModal name={manager.name} disabledReason={disabledReason} onClose={() => setResetOpen(false)} onDone={changed} />
      )}
    </PageSection>
  );
};
