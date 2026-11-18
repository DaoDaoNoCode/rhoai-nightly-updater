import React, { useState } from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Card,
  CardBody,
  Content,
  Dropdown,
  DropdownItem,
  DropdownList,
  ExpandableSection,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  MenuToggle,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import EllipsisVIcon from "@patternfly/react-icons/dist/esm/icons/ellipsis-v-icon";
import { CardList, CardListItem } from "../CardList";
import { TooltipButton } from "../TooltipButton";
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
        {state.unmanaged && <FlexItem><NotManagedLabel /></FlexItem>}
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

/** One entry of a part's menu: a change it offers, with why it cannot run now. */
export interface PartMenuItem {
  key: string;
  label: string;
  onClick: () => void;
  /** Why it cannot run now, shown in the menu under the label; null when it can. */
  disabledReason?: string | null;
  isDanger?: boolean;
}

/**
 * A part's less frequent changes (Delete, Repair when it is not the fix
 * shown above) in a kebab menu: one click away, never in the way. A
 * disabled entry says why under its label, so the reason is visible on
 * touch screens too.
 */
export const PartMenu: React.FC<{ part: string; items: PartMenuItem[] }> = ({ part, items }) => {
  const [open, setOpen] = useState(false);
  return (
    <Dropdown isOpen={open} onOpenChange={setOpen} onSelect={() => setOpen(false)} popperProps={{ position: "right" }}
      toggle={(ref) => <MenuToggle ref={ref} variant="plain" aria-label={`${part} actions`} isExpanded={open} onClick={() => setOpen(!open)} icon={<EllipsisVIcon />} />}>
      <DropdownList>
        {items.map((item) => (
          <DropdownItem key={item.key} isDanger={item.isDanger && !item.disabledReason} isAriaDisabled={!!item.disabledReason}
            description={item.disabledReason ?? undefined} onClick={item.disabledReason ? undefined : item.onClick}>
            {item.label}
          </DropdownItem>
        ))}
      </DropdownList>
    </Dropdown>
  );
};

/**
 * One part: its name, where it lives and its health, ownership apart (it
 * only limits actions), its actions and menu, and its details inline.
 */
export const PartItem: React.FC<{
  id: string;
  title: string;
  /** Where it lives, in words: "Shared by the cluster", "In project dev-project". */
  scope: string;
  status: EnvStatus;
  unmanaged: boolean;
  /** ActionListItems. */
  actions?: React.ReactNode;
  /** The part's menu entries (Delete, ...). */
  menu?: PartMenuItem[];
  children?: React.ReactNode;
}> = ({ id, title, scope, status, unmanaged, actions, menu, children }) => {
  const hasMenu = !!menu && menu.length > 0;
  return (
    <CardListItem labelledBy={partTitleId(id)} actionsBesideFrom="lg" actions={actions || hasMenu ? (
      <ActionList>
        {actions && <ActionListGroup>{actions}</ActionListGroup>}
        {hasMenu && (
          <ActionListGroup>
            <ActionListItem><PartMenu part={title} items={menu!} /></ActionListItem>
          </ActionListGroup>
        )}
      </ActionList>
    ) : undefined}>
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><Title headingLevel="h3" size="md" id={partTitleId(id)} tabIndex={-1}>{title}</Title></FlexItem>
            <FlexItem><EnvStatusLabel status={status} /></FlexItem>
            {unmanaged && <FlexItem><NotManagedLabel /></FlexItem>}
          </Flex>
          <Content component="small">{scope}</Content>
        </StackItem>
        {React.Children.toArray(children).map((child, i) => <StackItem key={i}>{child}</StackItem>)}
      </Stack>
    </CardListItem>
  );
};

/**
 * The feature's one primary action (Set up what is missing), as an action
 * row. When it cannot run, the reason is shown under it, not only in a
 * tooltip: a newcomer does not hover, and touch screens cannot.
 */
export const PrimaryAction: React.FC<{ label: string; onClick: () => void; reason: string | null; variant?: "primary" | "secondary"; isDanger?: boolean }> = ({ label, onClick, reason, variant = "primary", isDanger }) => {
  const reasonId = React.useId();
  return (
    <Stack hasGutter>
      <StackItem>
        <ActionList>
          <ActionListGroup>
            <ActionListItem>
              <TooltipButton variant={variant} isDanger={isDanger} onClick={onClick} disabledReason={reason} aria-describedby={reason ? reasonId : undefined}>{label}</TooltipButton>
            </ActionListItem>
          </ActionListGroup>
        </ActionList>
      </StackItem>
      {reason && (
        <StackItem>
          <HelperText id={reasonId}>
            <HelperTextItem>{reason}</HelperTextItem>
          </HelperText>
        </StackItem>
      )}
    </Stack>
  );
};

/** A collapsed section ("What this creates", "Technical details", "Test an MLflow PR build"). */
export const FeatureSection: React.FC<{ title: string; isIndented?: boolean; children: React.ReactNode }> = ({ title, isIndented, children }) => {
  const [expanded, setExpanded] = React.useState(false);
  return (
    <ExpandableSection toggleText={title} isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)} isIndented={isIndented}>
      {children}
    </ExpandableSection>
  );
};

/** "What this creates": the objects, one per line. */
export const CreatesList: React.FC<{ items: React.ReactNode[] }> = ({ items }) => (
  <List>{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
);

/**
 * A titled section of a tab ("Use it", "What next", "Clean up"): an h3
 * heading and its content.
 */
export const TabSection: React.FC<{ title: string; id: string; children: React.ReactNode }> = ({ title, id, children }) => (
  <section aria-labelledby={id}>
    <Stack hasGutter>
      <StackItem><Title headingLevel="h3" size="lg" id={id} tabIndex={-1}>{title}</Title></StackItem>
      {React.Children.toArray(children).map((child, i) => <StackItem key={i}>{child}</StackItem>)}
    </Stack>
  </section>
);

/**
 * "What next": the steps that make a ready feature useful, as an ordered
 * list. Samples (one-click example runs and data) add their items here.
 */
export const WhatNext: React.FC<{ id: string; items: React.ReactNode[] }> = ({ id, items }) => (
  <TabSection title="What next" id={id}>
    <List component="ol">{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
  </TabSection>
);

/**
 * "Clean up": the feature's (or the page's) one action that deletes what
 * this tool created, after a confirmation that lists it.
 */
export const CleanupSection: React.FC<{ id: string; text: string; label: string; onClick: () => void; reason: string | null }> = ({ id, text, label, onClick, reason }) => (
  <TabSection title="Clean up" id={id}>
    <Content component="p">{text}</Content>
    <PrimaryAction label={label} onClick={onClick} reason={reason} variant="secondary" isDanger />
  </TabSection>
);
