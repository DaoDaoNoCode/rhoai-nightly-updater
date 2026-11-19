import React from "react";
import {
  Alert,
  Button,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  List,
  ListItem,
  Popover,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { DSCManager, DSCMismatch } from "../../types";
import { PathList } from "./planText";

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
export const ComparisonSummary: React.FC<{ manager: DSCManager; fix: React.ReactNode }> = ({ manager, fix }) => {
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
