import React from "react";
import { Button, Content, Flex, FlexItem, Stack, StackItem, Title } from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import { Link } from "react-router-dom";
import { TooltipButton } from "../TooltipButton";
import { ExternalLink } from "./resourceRow";
import { EnvStatusLabel } from "./resourceStatus";
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
}

/**
 * The Overview: how many features are ready, then one row per feature in
 * dependency order with its state and next step. No endpoints, logins or
 * teardown: those are on the feature's tab.
 */
export const Overview: React.FC<{
  rows: OverviewRow[];
  baseReason: string | null;
  onSetUp: (row: OverviewRow, setup: Pending[]) => void;
  /** Opens the feature's tab at the part that needs attention. */
  onShowIssue: (row: OverviewRow, part: string) => void;
  /** Opens the feature's tab (to choose a project). */
  onOpenTab: (row: OverviewRow) => void;
}> = ({ rows, baseReason, onSetUp, onShowIssue, onOpenTab }) => {
  const ready = rows.filter((r) => r.state.status === "ready").length;
  const next = (row: OverviewRow, step: NextStep | null) => {
    switch (step?.kind) {
      case "open":
        return <ExternalLink href={step.href}>{step.label}</ExternalLink>;
      case "set-up":
        return <TooltipButton variant="secondary" onClick={() => onSetUp(row, step.setup)} disabledReason={baseReason ?? step.reason}>{step.label}</TooltipButton>;
      case "show-issue":
        return <Button variant="link" onClick={() => onShowIssue(row, step.part)}>Show issue</Button>;
      case "choose-project":
        return <Button variant="link" onClick={() => onOpenTab(row)}>Set up in a project</Button>;
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
                <Td dataLabel="Next step">{next(row, row.state.next)}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </StackItem>
    </Stack>
  );
};
