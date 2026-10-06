import React from "react";
import { Flex, FlexItem, List, ListItem } from "@patternfly/react-core";

/**
 * Rows inside a card: a plain bordered list (PF List isPlain isBordered),
 * so row content lines up with the card title and dividers sit only
 * between rows, never under the last one.
 */
export const CardList: React.FC<{ "aria-label": string; children: React.ReactNode }> = ({ "aria-label": ariaLabel, children }) => (
  <List isPlain isBordered aria-label={ariaLabel}>{children}</List>
);

interface CardListItemProps {
  /** id of the row's title, for aria-labelledby. */
  labelledBy?: string;
  /** Row actions: on the right from md, under the content on phones. */
  actions?: React.ReactNode;
  /** Shown under the row (for example an expanded form). */
  expanded?: React.ReactNode;
  children: React.ReactNode;
}

export const CardListItem: React.FC<CardListItemProps> = ({ labelledBy, actions, expanded, children }) => (
  <ListItem aria-labelledby={labelledBy} className="pf-v6-u-pb-sm">
    <div className="pf-v6-u-w-100">
      <Flex
        direction={{ default: "column", md: "row" }}
        justifyContent={{ md: "justifyContentSpaceBetween" }}
        alignItems={{ md: "alignItemsFlexStart" }}
        gap={{ default: "gapSm", md: "gapLg" }}
        flexWrap={{ default: "nowrap" }}
      >
        <FlexItem flex={{ default: "flex_1" }}>{children}</FlexItem>
        {actions && <FlexItem>{actions}</FlexItem>}
      </Flex>
      {expanded}
    </div>
  </ListItem>
);
