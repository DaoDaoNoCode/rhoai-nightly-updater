import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  Button,
  Card,
  CardBody,
  CardFooter,
  CardHeader,
  CardTitle,
  Content,
  DrawerActions,
  DrawerCloseButton,
  DrawerHead,
  DrawerPanelBody,
  DrawerPanelContent,
  ExpandableSection,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { EnvStatusLabel, type EnvStatus } from "./resourceStatus";

/** The id of the details drawer panel, for the Details buttons' aria-controls. */
export const DETAILS_PANEL_ID = "environment-details";
/** The drawer title's id: it takes the focus when the drawer opens. */
export const DETAILS_TITLE_ID = "environment-details-title";

/** Title and its one status label, wrapping below the title when narrow. */
const TitleWithStatus: React.FC<{ id?: string; level: "h2" | "h3"; size: "lg" | "xl"; status: EnvStatus; focusable?: boolean; children: React.ReactNode }> = ({ id, level, size, status, focusable, children }) => (
  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
    <FlexItem>
      <Title headingLevel={level} size={size} id={id} tabIndex={focusable ? -1 : undefined}>{children}</Title>
    </FlexItem>
    <FlexItem><EnvStatusLabel status={status} /></FlexItem>
  </Flex>
);

export interface ResourceCardProps {
  /** Unique; the heading id is derived from it. */
  id: string;
  title: string;
  status: EnvStatus;
  /** One sentence: what it is for. */
  purpose: React.ReactNode;
  /** What it waits for ("Needs: <link>"), as helper text. */
  needs?: React.ReactNode;
  /** The one main action (a button or an external link), or none. */
  action?: React.ReactNode;
  /** Opens the details drawer; opener gets the focus back when it closes. */
  onDetails: (opener: HTMLElement) => void;
  detailsOpen: boolean;
}

/**
 * A compact resource card: title with one status, one sentence, what it
 * needs, and one action beside Details. Names, addresses and every other
 * action are in the details drawer.
 */
export const ResourceCard: React.FC<ResourceCardProps> = ({ id, title, status, purpose, needs, action, onDetails, detailsOpen }) => {
  const headingId = `${id}-title`;
  return (
    <Card isFullHeight component="section" aria-labelledby={headingId}>
      <CardHeader>
        <CardTitle><TitleWithStatus id={headingId} level="h3" size="lg" status={status}>{title}</TitleWithStatus></CardTitle>
      </CardHeader>
      <CardBody>
        <Content component="p">{purpose}</Content>
        {needs && (
          <HelperText className="pf-v6-u-mt-sm">
            <HelperTextItem>{needs}</HelperTextItem>
          </HelperText>
        )}
      </CardBody>
      <CardFooter>
        <ActionList>
          <ActionListGroup>
            {action && <ActionListItem>{action}</ActionListItem>}
            <ActionListItem>
              <Button variant="link" aria-label={`${title} details`} aria-expanded={detailsOpen} aria-controls={DETAILS_PANEL_ID}
                onClick={(e) => onDetails(e.currentTarget)}>Details</Button>
            </ActionListItem>
          </ActionListGroup>
        </ActionList>
      </CardFooter>
    </Card>
  );
};

/** A titled part of the details drawer. */
export const DetailsSection: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => {
  const id = React.useId();
  return (
    <section aria-labelledby={id}>
      <Stack hasGutter>
        <StackItem><Title headingLevel="h3" size="md" id={id}>{title}</Title></StackItem>
        <StackItem>{children}</StackItem>
      </Stack>
    </section>
  );
};

/** "What this creates": the objects, one per line. */
export const CreatesList: React.FC<{ items: React.ReactNode[] }> = ({ items }) => (
  <DetailsSection title="What this creates">
    <List>{items.map((item, i) => <ListItem key={i}>{item}</ListItem>)}</List>
  </DetailsSection>
);

/** The parts of a resource's details, one gutter apart (false and null parts are left out). */
export const DetailsStack: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <Stack hasGutter>
    {React.Children.toArray(children).map((child, i) => <StackItem key={i}>{child}</StackItem>)}
  </Stack>
);

/**
 * "How to use", collapsed until opened: paragraphs in a PatternFly Content
 * (which spaces them), then any controls.
 */
export const HowToUse: React.FC<{ text: React.ReactNode; children?: React.ReactNode }> = ({ text, children }) => {
  const [expanded, setExpanded] = React.useState(false);
  return (
    <ExpandableSection toggleText="How to use" isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)}>
      <DetailsStack>
        <Content>{text}</Content>
        {children}
      </DetailsStack>
    </ExpandableSection>
  );
};

/** The secondary actions of a resource (Repair, Tear down, ...), as one action row. */
export const DetailsActions: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <ActionList>
    <ActionListGroup>{children}</ActionListGroup>
  </ActionList>
);

/** An inline alert in the drawer. */
export const DetailsAlert: React.FC<{ variant: "info" | "warning" | "danger"; title: string; children?: React.ReactNode }> = ({ variant, title, children }) => (
  <Alert variant={variant} isInline title={title} component="p">{children}</Alert>
);

/**
 * The details drawer's panel: the resource's title and status, a close
 * button, and its details. Escape closes it; the title (DETAILS_TITLE_ID)
 * can take the focus when it opens.
 */
export const DetailsPanel: React.FC<{
  title: string;
  status: EnvStatus;
  onClose: () => void;
  children: React.ReactNode;
}> = ({ title, status, onClose, children }) => (
  <DrawerPanelContent id={DETAILS_PANEL_ID} widths={{ default: "width_100", lg: "width_50", "2xl": "width_33" }}
    onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); onClose(); } }}>
    <DrawerHead>
      <TitleWithStatus id={DETAILS_TITLE_ID} level="h2" size="xl" status={status} focusable>{title}</TitleWithStatus>
      <DrawerActions>
        <DrawerCloseButton onClick={onClose} aria-label={`Close ${title} details`} />
      </DrawerActions>
    </DrawerHead>
    <DrawerPanelBody>{children}</DrawerPanelBody>
  </DrawerPanelContent>
);
