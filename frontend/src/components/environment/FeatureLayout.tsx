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
import { LabelSkeleton } from "./Loading";

/** Explains why viewers with read-only rights cannot use Set up. */
export const READ_ONLY_SETUP = "Read-only access: setting this up needs a cluster-admin. Ask one to set it up for you.";

/** Builds the DOM id for a part heading so "Show issue" can target focus there. */
export const partTitleId = (part: string) => `environment-part-${part}`;

/** Single-sentence blurbs per feature—the tab header shows each one, including during load. */
export const FEATURE_PURPOSE = {
  storage: "S3-compatible object storage (SeaweedFS) shared by the whole cluster, in namespace seaweedfs.",
  pipelines: "Run data science pipelines in a project; their artifacts go to the cluster's S3 storage.",
  mlflow: "Track experiments: log runs, metrics and models to an MLflow server on the cluster, with a workspace per data science project.",
  "model-registry": "A model registry for the cluster: register models and their versions, and deploy them from the dashboard.",
  models: "Serve small models on CPU in a project: chat, tool calling and embeddings. Call them with each model's own token or from the dashboard's Gen AI playground.",
  playground: "The dashboard's Gen AI playground in a project: a chat page to try the project's models and ask about your documents.",
  maas: "Models as a Service: offer models to everyone on the cluster through one gateway, with API keys and token limits.",
} as const;

/**
 * Renders a feature tab header: title plus project-specific state (same
 * source as Overview), plus the one-line purpose blurb. While state is
 * loading, the status slot uses a Skeleton.
 */
export const FeatureHeader: React.FC<{ title: string; purpose: string; state: FeatureState }> = ({ title, purpose, state }) => (
  <Stack>
    <StackItem>
      <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
        <FlexItem><Title headingLevel="h2" size="xl">{title}</Title></FlexItem>
        <FlexItem>{state.loading ? <LabelSkeleton what={`the state of ${title}`} /> : <EnvStatusLabel status={state.status} text={state.label} />}</FlexItem>
        {state.unmanaged && <FlexItem><NotManagedLabel /></FlexItem>}
      </Flex>
    </StackItem>
    <StackItem><Content component="p">{purpose}</Content></StackItem>
  </Stack>
);

/** Feature parts listed in the order their dependencies require. */
export const PartList: React.FC<{ "aria-label": string; children: React.ReactNode }> = ({ "aria-label": label, children }) => (
  <Card>
    <CardBody>
      <CardList aria-label={label}>{children}</CardList>
    </CardBody>
  </Card>
);

/** Describes one kebab-menu choice for a part, including when it is blocked and why. */
export interface PartMenuItem {
  key: string;
  label: string;
  onClick: () => void;
  /** Shown beneath the label when the action is blocked; omitted when the item is enabled. */
  disabledReason?: string | null;
  isDanger?: boolean;
}

/**
 * Infrequent part actions (Delete, Repair when not the inline fix) live
 * in a kebab: reachable in one tap without cluttering the row. Disabled
 * rows surface the blockage reason under the label for touch users as
 * well as mouse users.
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
 * Single part row: title, textual placement and health, ownership called
 * out separately (affects actions only), primary actions plus overflow
 * menu, and expandable detail children.
 */
export const PartItem: React.FC<{
  id: string;
  title: string;
  /** Human-readable placement, e.g. "Shared by the cluster", "In project dev-project". */
  scope: string;
  status: EnvStatus;
  unmanaged: boolean;
  /** Nodes to render as ActionListItems. */
  actions?: React.ReactNode;
  /** Overflow entries for this part (Delete, …). */
  menu?: PartMenuItem[];
  /** When true, swap the status label for a Skeleton until data arrives. */
  loading?: boolean;
  /** Custom label text for the status chip instead of the default status word ("Waiting"). */
  statusText?: string;
  children?: React.ReactNode;
}> = ({ id, title, scope, status, unmanaged, actions, menu, loading, statusText, children }) => {
  const hasMenu = !!menu && menu.length > 0;
  return (
    <CardListItem labelledBy={partTitleId(id)} actionsBesideFrom={actions ? "lg" : "always"} actions={actions || hasMenu ? (
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
            <FlexItem>{loading ? <LabelSkeleton what={`the state of ${title}`} /> : <EnvStatusLabel status={status} text={statusText} />}</FlexItem>
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
 * Wraps the feature's main call-to-action (set up missing pieces) in an
 * action row. If blocked, the explanation sits below the button—not only
 * in a tooltip—so first-time users and touch devices still see why.
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

/** Expandable block for titles like "What this creates", "Technical details", "Test an MLflow PR build". */
export const FeatureSection: React.FC<{ title: string; isIndented?: boolean; children: React.ReactNode }> = ({ title, isIndented, children }) => {
  const [expanded, setExpanded] = React.useState(false);
  return (
    <ExpandableSection toggleText={title} isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)} isIndented={isIndented}>
      {children}
    </ExpandableSection>
  );
};

/** Renders the "What this creates" inventory—one list item per object. */
export const CreatesList: React.FC<{ items: React.ReactNode[] }> = ({ items }) => (
  <List>{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
);

/**
 * Named chunk within a tab ("Use it", "What next", "Clean up"): level-three
 * heading plus stacked body content.
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
 * "What next" section: ordered steps to turn a ready feature into
 * something useful. Sample flows plug extra list items in here.
 */
export const WhatNext: React.FC<{ id: string; items: React.ReactNode[] }> = ({ id, items }) => (
  <TabSection title="What next" id={id}>
    <List component="ol">{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
  </TabSection>
);

/**
 * "Clean up" section: the single destructive action for the feature (or
 * page) that removes what this app provisioned, gated by a confirmation
 * that enumerates what will go away.
 */
export const CleanupSection: React.FC<{ id: string; text: string; label: string; onClick: () => void; reason: string | null }> = ({ id, text, label, onClick, reason }) => (
  <TabSection title="Clean up" id={id}>
    <Content component="p">{text}</Content>
    <PrimaryAction label={label} onClick={onClick} reason={reason} variant="secondary" isDanger />
  </TabSection>
);