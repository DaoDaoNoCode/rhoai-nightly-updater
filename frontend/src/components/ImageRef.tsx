import React from "react";
import { ClipboardCopy } from "@patternfly/react-core";

interface ImageRefProps {
  /** Full image reference; it is what gets copied. */
  image: string;
  /** What the value is, for the copy button's accessible name. */
  what?: string;
  /** Truncate in the middle to fit the cell (default true). */
  truncate?: boolean;
  /**
   * Shorter text to show instead of the full reference (for example the
   * digest when every row has the same repository). The copy button still
   * copies the full reference.
   */
  display?: string;
}

/**
 * An image reference with a copy button (A08-11). It needs no hover: the
 * copy button is a normal focusable button that copies the full reference,
 * and when the text is truncated the text itself takes focus and shows the
 * full value.
 */
export const ImageRef: React.FC<ImageRefProps> = ({ image, what = "image reference", truncate = true, display }) => (
  <ClipboardCopy
    variant="inline-compact"
    isCode
    hoverTip={`Copy ${what}`}
    clickTip="Copied"
    copyAriaLabel={`Copy ${what} ${image}`}
    truncation={truncate && !display ? { position: "middle", trailingNumChars: 14 } : false}
    onCopy={display ? () => { void navigator.clipboard?.writeText(image).catch(() => {}); } : undefined}
  >
    {display ?? image}
  </ClipboardCopy>
);
