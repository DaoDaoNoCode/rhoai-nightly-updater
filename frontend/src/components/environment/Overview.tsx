import React from "react";
import { Button, Content, Flex, FlexItem, HelperText, HelperTextItem, Popover, Skeleton, Stack, StackItem, Title } from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import { Link } from "react-router-dom";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { TooltipButton } from "../TooltipButton";
import { EnvStatusLabel } from "./resourceStatus";
import { CleanupSection } from "./FeatureLayout";
import { LabelSkeleton } from "./Loading";
import type { FeatureState, NextStep } from "./featureState";
import type { Pending } from "./resourceRow";
import { RecipeRunNote } from "../recipes/RecipeEntry";

/** One feature as the Overview lists it. */
export interface OverviewRow {
  id: string;
  name: string;
  /** What it gives you, in a few words. */
  gives: string;
  /** From the feature's state function: the same the feature's tab shows. */
  state: FeatureState;
  /** The tab's address, with the project the state is about. */
  href: string;
  /** Once it works, its samples: the link's label and the id of the tab's samples heading. */
  samples?: { label: string; focus: string };
}

/**
 * A link in a table cell stays on one line: a wrapped inline-flex button
 * takes the cell's width and pushes its icon to the far edge.
 */
const ONE_LINE = "pf-v6-u-text-nowrap";

const NotManaged: React.FC = () => (
  <HelperText><HelperTextItem>Not managed by this tool</HelperTextItem></HelperText>
);

/** The projects a feature is in, each with its state and a link to the feature's tab there. */
const ProjectsPopover: React.FC<{ row: OverviewRow; projects: FeatureState[] }> = ({ row, projects }) => (
  <Popover
    headerContent={`${row.name} in ${projects.length} projects`}
    maxWidth="40rem"
    bodyContent={
      <Table variant="compact" borders={false} isPlain gridBreakPoint="" aria-label={`${row.name} by project`}>
        <Thead>
          <Tr>
            <Th>Project</Th>
            <Th modifier="nowrap">State</Th>
          </Tr>
        </Thead>
        <Tbody>
          {projects.map((p) => (
            <Tr key={p.project}>
              <Td dataLabel="Project"><Link to={`?tab=${row.id}&project=${encodeURIComponent(p.project ?? "")}`}>{p.project}</Link></Td>
              <Td dataLabel="State">
                <Stack>
                  <StackItem><EnvStatusLabel status={p.status} /></StackItem>
                  {p.unmanaged && <StackItem><NotManaged /></StackItem>}
                </Stack>
              </Td>
            </Tr>
          ))}
        </Tbody>
      </Table>
    }
  >
    <Button variant="link" isInline className={ONE_LINE}>Show projects</Button>
  </Popover>
);

/**
 * The Overview: what the page is for, how many features are ready, then
 * one row per feature in dependency order with its state and next step,
 * and Clean up for everything. No endpoints or logins: those are on the
 * feature's tab.
 */
export const Overview: React.FC<{
  rows: OverviewRow[];
  baseReason: string | null;
  /** Why Set up is not available to this user (read-only), or null. */
  setUpReason: string | null;
  onSetUp: (row: OverviewRow, setup: Pending[]) => void;
  /** Opens the feature's tab at the part that needs attention. */
  onShowIssue: (row: OverviewRow, part: string) => void;
  /** Opens the feature's tab (to choose a project). */
  onOpenTab: (row: OverviewRow) => void;
  onMigrate: () => void;
  /** Opens the feature's tab at its samples. */
  onSamples: (row: OverviewRow, focus: string) => void;
  /** Clean up everything, or null when there is nothing of this tool's (or the user may only read). */
  onCleanup: (() => void) | null;
}> = ({ rows, baseReason, setUpReason, onSetUp, onShowIssue, onOpenTab, onMigrate, onSamples, onCleanup }) => {
  const ready = rows.filter((r) => r.state.status === "ready").length;
  // Until every feature's status is read, the count would change under the reader's eyes.
  const loading = rows.some((r) => r.state.loading);
  // The next step is text in a cell, so it is an inline link, as PatternFly
  // puts links in table cells (Button variant="link" isInline): it sits on
  // the row's text line. A step that opens a dialog ends with an ellipsis.
  // The State cell names the project, so the links do not repeat it.
  const next = (row: OverviewRow, step: NextStep | null) => {
    switch (step?.kind) {
      case "open":
        return <Button variant="link" isInline component="a" href={step.href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" className={ONE_LINE}>{step.label}</Button>;
      case "set-up": {
        const reason = setUpReason ?? baseReason ?? step.reason;
        const reasonId = `overview-${row.id}-reason`;
        return (
          <Stack>
            <StackItem><TooltipButton variant="link" isInline className={ONE_LINE} onClick={() => onSetUp(row, step.setup)} disabledReason={reason} aria-describedby={reason ? reasonId : undefined}>{step.label}</TooltipButton></StackItem>
            {reason && <StackItem><HelperText id={reasonId}><HelperTextItem>{reason}</HelperTextItem></HelperText></StackItem>}
          </Stack>
        );
      }
      case "show-issue":
        // Among several projects, the step names the one with the problem.
        return <Button variant="link" isInline className={ONE_LINE} onClick={() => onShowIssue(row, step.part)}>{step.project ? `Show issue in ${step.project}` : "Show issue"}</Button>;
      case "migrate":
        return setUpReason
          ? <Button variant="link" isInline className={ONE_LINE} onClick={() => onShowIssue(row, "storage")}>Show issue</Button>
          : <Button variant="link" isInline className={ONE_LINE} onClick={onMigrate}>Migrate storage</Button>;
      case "choose-project":
        return <Button variant="link" isInline className={ONE_LINE} onClick={() => onOpenTab(row)}>Choose a project</Button>;
      default:
        return null;
    }
  };
  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsBaseline" }}>
          <FlexItem><Title headingLevel="h2" size="xl">Overview</Title></FlexItem>
          <FlexItem>{loading ? <Skeleton width="6rem" screenreaderText="Loading how many features are ready" /> : <Content component="p">{ready} of {rows.length} ready</Content>}</FlexItem>
        </Flex>
      </StackItem>
      <StackItem>
        <Content component="p">Set up what you need to develop or reproduce an issue on this cluster: each feature takes a few minutes and asks before it changes anything. Its tab shows how to use it.</Content>
      </StackItem>
      <RecipeRunNote />
      <StackItem>
        {/* Below lg the rows stack (label: value), so the one-line State and Next step never squeeze the table. */}
        <Table variant="compact" aria-label="Features" gridBreakPoint="grid-lg" aria-busy={loading}>
          <Thead>
            <Tr>
              <Th modifier="fitContent">Feature</Th>
              <Th>What it gives you</Th>
              <Th width={25}>State</Th>
              <Th width={20}>Next step</Th>
            </Tr>
          </Thead>
          <Tbody>
            {rows.map((row) => (
              <Tr key={row.id}>
                <Td dataLabel="Feature"><Link to={row.href}>{row.name}</Link></Td>
                <Td dataLabel="What it gives you">{row.gives}</Td>
                <Td dataLabel="State">
                  {row.state.loading ? <LabelSkeleton what={`the state of ${row.name}`} /> : (
                    <Stack>
                      <StackItem><EnvStatusLabel status={row.state.status} text={row.state.label} /></StackItem>
                      {row.state.unmanaged && <StackItem><NotManaged /></StackItem>}
                      {row.state.projects && <StackItem><ProjectsPopover row={row} projects={row.state.projects} /></StackItem>}
                    </Stack>
                  )}
                </Td>
                <Td dataLabel="Next step">
                  {row.state.loading ? <Skeleton width="7rem" /> : row.samples ? (
                    <Stack>
                      <StackItem>{next(row, row.state.next)}</StackItem>
                      <StackItem><Button variant="link" isInline className={ONE_LINE} onClick={() => onSamples(row, row.samples!.focus)}>{row.samples.label}</Button></StackItem>
                    </Stack>
                  ) : next(row, row.state.next)}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </StackItem>
      {onCleanup && (
        <StackItem>
          <CleanupSection id="overview-cleanup" label="Clean up everything" onClick={onCleanup} reason={baseReason}
            text="Deletes everything this tool created on this cluster: it unpublishes its models from Models as a Service, then deletes the workbenches (their storage only if you tick it), the pipeline servers, the S3 storage, MLflow, the model registry, the playgrounds, the models, the inference gateway, the distributed workloads setup and the MaaS platform. You choose what goes, then it runs as one operation." />
        </StackItem>
      )}
    </Stack>
  );
};
