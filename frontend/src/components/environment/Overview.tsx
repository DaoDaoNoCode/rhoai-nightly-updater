import React from "react";
import { Button, Content, Flex, FlexItem, HelperText, HelperTextItem, Stack, StackItem, Title } from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import { Link } from "react-router-dom";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { TooltipButton } from "../TooltipButton";
import { EnvStatusLabel } from "./resourceStatus";
import { CleanupSection } from "./FeatureLayout";
import type { FeatureState, NextStep } from "./featureState";
import type { Pending } from "./resourceRow";

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
  // The next step is text in a cell, so it is an inline link, as PatternFly
  // puts links in table cells (Button variant="link" isInline): it sits on
  // the row's text line. A step that opens a dialog ends with an ellipsis.
  const next = (row: OverviewRow, step: NextStep | null) => {
    switch (step?.kind) {
      case "open":
        return <Button variant="link" isInline component="a" href={step.href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{step.label}</Button>;
      case "set-up": {
        const reason = setUpReason ?? baseReason ?? step.reason;
        const reasonId = `overview-${row.id}-reason`;
        return (
          <Stack>
            <StackItem><TooltipButton variant="link" isInline onClick={() => onSetUp(row, step.setup)} disabledReason={reason} aria-describedby={reason ? reasonId : undefined}>{step.label}…</TooltipButton></StackItem>
            {reason && <StackItem><HelperText id={reasonId}><HelperTextItem>{reason}</HelperTextItem></HelperText></StackItem>}
          </Stack>
        );
      }
      case "show-issue":
        return <Button variant="link" isInline onClick={() => onShowIssue(row, step.part)}>Show issue</Button>;
      case "migrate":
        return setUpReason
          ? <Button variant="link" isInline onClick={() => onShowIssue(row, "storage")}>Show issue</Button>
          : <Button variant="link" isInline onClick={onMigrate}>Migrate storage…</Button>;
      case "choose-project":
        return <Button variant="link" isInline onClick={() => onOpenTab(row)}>Choose a project</Button>;
      default:
        return null;
    }
  };
  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsBaseline" }}>
          <FlexItem><Title headingLevel="h2" size="xl">Overview</Title></FlexItem>
          <FlexItem><Content component="p">{ready} of {rows.length} ready</Content></FlexItem>
        </Flex>
      </StackItem>
      <StackItem>
        <Content component="p">Set up what you need to develop or reproduce an issue on this cluster: each feature takes a few minutes and asks before it changes anything. Its tab shows how to use it.</Content>
      </StackItem>
      <StackItem>
        <Table variant="compact" aria-label="Features">
          <Thead>
            <Tr>
              <Th>Feature</Th>
              <Th>What it gives you</Th>
              <Th>State</Th>
              <Th>Next step</Th>
            </Tr>
          </Thead>
          <Tbody>
            {rows.map((row) => (
              <Tr key={row.id}>
                <Td dataLabel="Feature"><Link to={row.href}>{row.name}</Link></Td>
                <Td dataLabel="What it gives you">{row.gives}</Td>
                <Td dataLabel="State"><EnvStatusLabel status={row.state.status} text={row.state.label} /></Td>
                <Td dataLabel="Next step">
                  {row.samples ? (
                    <Flex gap={{ default: "gapMd" }} rowGap={{ default: "rowGapXs" }}>
                      <FlexItem>{next(row, row.state.next)}</FlexItem>
                      <FlexItem><Button variant="link" isInline onClick={() => onSamples(row, row.samples!.focus)}>{row.samples.label}</Button></FlexItem>
                    </Flex>
                  ) : next(row, row.state.next)}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </StackItem>
      {onCleanup && (
        <StackItem>
          <CleanupSection id="overview-cleanup" label="Clean up everything…" onClick={onCleanup} reason={baseReason}
            text="Deletes everything this tool created on this cluster: the pipeline servers, the S3 storage and MLflow. You choose what goes, then it runs as one operation." />
        </StackItem>
      )}
    </Stack>
  );
};
