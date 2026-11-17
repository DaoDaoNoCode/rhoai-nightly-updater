import React from "react";
import { Divider, Flex, FlexItem, Stack, StackItem } from "@patternfly/react-core";

/**
 * Rows inside a card: a list laid out as a PatternFly Stack with gutter,
 * with a Divider item between rows (never under the last one). The gutter
 * sits on both sides of each divider, so the space from a row's last line
 * to the divider equals the space from the divider to the next row's
 * title. (A bordered PF List puts its gap above the border and its padding
 * below it; with the rows' own bottom padding the two sides differed.)
 */
export const CardList: React.FC<{ "aria-label": string; children: React.ReactNode }> = ({ "aria-label": ariaLabel, children }) => {
  const rows = React.Children.toArray(children).filter(React.isValidElement);
  return (
    <Stack hasGutter component="ul" aria-label={ariaLabel} role="list">
      {rows.map((row, i) => (
        <React.Fragment key={row.key ?? i}>
          {i > 0 && <Divider component="li" />}
          {row}
        </React.Fragment>
      ))}
    </Stack>
  );
};

interface CardListItemProps {
  /** id of the row's title, for aria-labelledby. */
  labelledBy?: string;
  /** Row actions: on the right from md, under the content on phones. */
  actions?: React.ReactNode;
  /**
   * From which breakpoint the actions sit beside the content (default md).
   * A row with several wide actions uses a later one, so its text keeps a
   * readable width on mid-size windows instead of being squeezed.
   */
  actionsBesideFrom?: "md" | "lg" | "xl";
  /** Shown under the row (for example an expanded form). */
  expanded?: React.ReactNode;
  children: React.ReactNode;
}

export const CardListItem: React.FC<CardListItemProps> = ({ labelledBy, actions, actionsBesideFrom = "md", children, expanded }) => (
  <StackItem component="li" aria-labelledby={labelledBy}>
    <div className="pf-v6-u-w-100">
      <Flex
        direction={{ default: "column", [actionsBesideFrom]: "row" }}
        justifyContent={{ [actionsBesideFrom]: "justifyContentSpaceBetween" }}
        alignItems={{ [actionsBesideFrom]: "alignItemsFlexStart" }}
        gap={{ default: "gapSm", [actionsBesideFrom]: "gapLg" }}
        flexWrap={{ default: "nowrap" }}
      >
        <FlexItem flex={{ default: "flex_1" }}>{children}</FlexItem>
        {actions && <FlexItem>{actions}</FlexItem>}
      </Flex>
      {expanded}
    </div>
  </StackItem>
);
