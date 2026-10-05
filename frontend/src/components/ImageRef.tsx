import React from "react";
import { ClipboardCopy } from "@patternfly/react-core";

interface ImageRefProps {
  /** Full image reference; it is what gets copied. */
  image: string;
  /** What the value is, for the copy button's accessible name. */
  what?: string;
  /** Truncate in the middle to fit the cell (default true). */
  truncate?: boolean;
}

/**
 * A full image reference with a copy button (A08-11). It needs no hover:
 * the copy button is a normal focusable button, and when the reference is
 * truncated the text itself takes focus and shows the full value.
 */
export const ImageRef: React.FC<ImageRefProps> = ({ image, what = "image reference", truncate = true }) => (
  <ClipboardCopy
    variant="inline-compact"
    isCode
    hoverTip={`Copy ${what}`}
    clickTip="Copied"
    copyAriaLabel={`Copy ${what} ${image}`}
    truncation={truncate ? { position: "middle", trailingNumChars: 14 } : false}
  >
    {image}
  </ClipboardCopy>
);
