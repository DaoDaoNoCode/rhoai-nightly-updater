import React from "react";
import { Flex, FlexItem } from "@patternfly/react-core";

/**
 * A short run of facts on one line ("rhoai-3.6  0aa1d3d  built 3h ago"):
 * PF Flex items with a gap and no separator glyphs, each item kept whole.
 * When the run wraps, every line starts and ends with an item, never with
 * a dangling "·".
 */
export const InlineItems: React.FC<{ children: React.ReactNode; inline?: boolean; className?: string }> = ({ children, inline, className }) => (
  <Flex
    component="span"
    display={inline ? { default: "inlineFlex" } : undefined}
    columnGap={{ default: "columnGapMd" }}
    rowGap={{ default: "rowGapXs" }}
    alignItems={{ default: "alignItemsCenter" }}
    flexWrap={{ default: "wrap" }}
    className={className}
  >
    {React.Children.toArray(children).filter(Boolean).map((child, i) => (
      <FlexItem component="span" key={i} className="pf-v6-u-text-nowrap">{child}</FlexItem>
    ))}
  </Flex>
);
