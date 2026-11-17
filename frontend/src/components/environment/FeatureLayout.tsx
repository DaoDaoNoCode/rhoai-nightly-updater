import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Card,
  CardBody,
  Content,
  ExpandableSection,
  Flex,
  FlexItem,
  List,
  ListItem,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { CardList, CardListItem } from "../CardList";
import { EnvStatusLabel, NotManagedLabel, type EnvStatus } from "./resourceStatus";
import type { FeatureState } from "./featureState";

/** The id of a part's title: "Show issue" moves the focus there. */
export const partTitleId = (part: string) => `environment-part-${part}`;

/**
 * A feature tab's header: its title with the feature's state for the
 * selected project (from the same state function as the Overview), and
 * one sentence on what it is for.
 */
export const FeatureHeader: React.FC<{ title: string; purpose: string; state: FeatureState }> = ({ title, purpose, state }) => (
  <Stack>
    <StackItem>
      <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
        <FlexItem><Title headingLevel="h2" size="xl">{title}</Title></FlexItem>
        <FlexItem><EnvStatusLabel status={state.status} text={state.label} /></FlexItem>
      </Flex>
    </StackItem>
    <StackItem><Content component="p">{purpose}</Content></StackItem>
  </Stack>
);

/** The parts a feature needs, in dependency order. */
export const PartList: React.FC<{ "aria-label": string; children: React.ReactNode }> = ({ "aria-label": label, children }) => (
  <Card>
    <CardBody>
      <CardList aria-label={label}>{children}</CardList>
    </CardBody>
  </Card>
);

/**
 * One part: its name and where it lives, its health, ownership apart (it
 * only limits actions), its actions, and its details inline.
 */
export const PartItem: React.FC<{
  id: string;
  title: string;
  /** "cluster" or the project. */
  scope: string;
  status: EnvStatus;
  unmanaged: boolean;
  /** ActionListItems. */
  actions?: React.ReactNode;
  children?: React.ReactNode;
}> = ({ id, title, scope, status, unmanaged, actions, children }) => (
  <CardListItem labelledBy={partTitleId(id)} actionsBesideFrom="lg" actions={actions ? (
    <ActionList>
      <ActionListGroup>{actions}</ActionListGroup>
    </ActionList>
  ) : undefined}>
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><Title headingLevel="h3" size="md" id={partTitleId(id)} tabIndex={-1}>{title} ({scope})</Title></FlexItem>
          <FlexItem><EnvStatusLabel status={status} /></FlexItem>
          {unmanaged && <FlexItem><NotManagedLabel /></FlexItem>}
        </Flex>
      </StackItem>
      {React.Children.toArray(children).map((child, i) => <StackItem key={i}>{child}</StackItem>)}
    </Stack>
  </CardListItem>
);

/** The feature's one primary action (Set up the missing parts), as an action row. */
export const PrimaryAction: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <ActionList>
    <ActionListGroup>
      <ActionListItem>{children}</ActionListItem>
    </ActionListGroup>
  </ActionList>
);

/** Secondary actions (ActionListItems) as one action row. */
export const ActionRow: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <ActionList>
    <ActionListGroup>{children}</ActionListGroup>
  </ActionList>
);

/** A collapsed section under the parts ("What this creates", "Tear down"). */
export const FeatureSection: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => {
  const [expanded, setExpanded] = React.useState(false);
  return (
    <ExpandableSection toggleText={title} isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)}>
      {children}
    </ExpandableSection>
  );
};

/** "What this creates": the objects, one per line. */
export const CreatesList: React.FC<{ items: React.ReactNode[] }> = ({ items }) => (
  <List>{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
);
