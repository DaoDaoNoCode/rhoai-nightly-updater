import React from "react";
import { Alert, AlertActionLink, Card, CardBody, Content, Flex, FlexItem, Skeleton, Stack, StackItem, Title } from "@patternfly/react-core";
import { Tbody, Td, Tr } from "@patternfly/react-table";
import { CardList, CardListItem } from "../CardList";
import { TruncatedText } from "../LongText";

/**
 * The Environment page's loading states, one look on every tab: PatternFly
 * Skeletons where the data will be, each read to screen readers as
 * "Loading <what>". Nothing is shown as empty, not set up or missing until
 * the data that says so has been read; a refresh keeps the data shown.
 */

/** A status label still loading: a Skeleton the size of a label. */
export const LabelSkeleton: React.FC<{ what: string }> = ({ what }) => (
  <Skeleton width="8rem" fontSize="xl" screenreaderText={`Loading ${what}`} />
);

const LINE_WIDTHS = ["90%", "75%", "60%", "80%"];

/** A few lines of text still loading. */
export const LinesSkeleton: React.FC<{ what: string; lines?: number }> = ({ what, lines = 2 }) => (
  <Stack hasGutter aria-busy="true">
    {Array.from({ length: lines }, (_, i) => (
      <StackItem key={i}><Skeleton width={LINE_WIDTHS[i % LINE_WIDTHS.length]} screenreaderText={i === 0 ? `Loading ${what}` : undefined} /></StackItem>
    ))}
  </Stack>
);

/** One part of a feature still loading, shaped like a PartItem: title and label, where it lives, a line of text. */
const PartSkeleton: React.FC<{ what: string; first: boolean }> = ({ what, first }) => (
  <CardListItem>
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><Skeleton width="10rem" fontSize="md" screenreaderText={first ? `Loading ${what}` : undefined} /></FlexItem>
          <FlexItem><Skeleton width="6rem" fontSize="xl" /></FlexItem>
        </Flex>
      </StackItem>
      <StackItem><Skeleton width="12rem" fontSize="sm" /></StackItem>
      <StackItem><Skeleton width="80%" /></StackItem>
    </Stack>
  </CardListItem>
);

/** The parts of a feature still loading: the card the parts will be in (PartList), with count skeleton rows. */
export const PartsSkeleton: React.FC<{ "aria-label": string; what: string; count: number }> = ({ "aria-label": label, what, count }) => (
  <Card aria-busy="true">
    <CardBody>
      <CardList aria-label={label}>
        {Array.from({ length: count }, (_, i) => <PartSkeleton key={i} what={what} first={i === 0} />)}
      </CardList>
    </CardBody>
  </Card>
);

/** Table rows still loading: one Skeleton per cell. */
export const RowsSkeleton: React.FC<{ what: string; columns: string[]; rows?: number }> = ({ what, columns, rows = 2 }) => (
  <Tbody aria-busy="true">
    {Array.from({ length: rows }, (_, r) => (
      <Tr key={r}>
        {columns.map((c, i) => <Td key={c} dataLabel={c}><Skeleton width={i === 0 ? "70%" : "50%"} screenreaderText={r === 0 && i === 0 ? `Loading ${what}` : undefined} /></Td>)}
      </Tr>
    ))}
  </Tbody>
);

/**
 * A feature tab before the status it shows has been read: its title and
 * what it is for (both known), a Skeleton for its state, and its parts.
 */
export const FeatureLoading: React.FC<{ title: string; purpose: string; parts: number }> = ({ title, purpose, parts }) => (
  <Stack hasGutter>
    <StackItem>
      <Stack>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><Title headingLevel="h2" size="xl">{title}</Title></FlexItem>
            <FlexItem><LabelSkeleton what={`the state of ${title}`} /></FlexItem>
          </Flex>
        </StackItem>
        <StackItem><Content component="p">{purpose}</Content></StackItem>
      </Stack>
    </StackItem>
    <StackItem><PartsSkeleton aria-label={`What ${title} needs`} what={`what ${title} needs`} count={parts} /></StackItem>
  </Stack>
);

/**
 * Data that could not be read and was never read: why, and Try again, in
 * place of the list it would fill (never an empty list).
 */
export const LoadFailed: React.FC<{ title: string; error: string; onRetry: () => void }> = ({ title, error, onRetry }) => (
  <Alert variant="danger" isInline component="div" title={title} actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>
    <TruncatedText>{error}</TruncatedText>
  </Alert>
);
