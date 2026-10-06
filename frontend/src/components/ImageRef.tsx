import React from "react";
import { ClipboardCopy } from "@patternfly/react-core";

interface ImageRefProps {
  /** Full value (image reference, commit, name); it is what gets copied. */
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
  /** Monospace (default true); false for human names such as component names. */
  isCode?: boolean;
}

/**
 * The one way to show a copyable identifier (image, digest, commit,
 * component image): PatternFly's inline-compact ClipboardCopy (A08-11). The
 * copy button is a normal focusable button that copies the full value, and
 * truncated text takes focus and shows the full value.
 */
export const ImageRef: React.FC<ImageRefProps> = ({ image, what = "image reference", truncate = true, display, isCode = true }) => (
  <ClipboardCopy
    variant="inline-compact"
    isCode={isCode}
    hoverTip={`Copy ${what}`}
    clickTip="Copied"
    copyAriaLabel={`Copy ${what} ${image}`}
    truncation={truncate && !display ? { position: "middle", trailingNumChars: 14 } : false}
    onCopy={display ? () => { void navigator.clipboard?.writeText(image).catch(() => {}); } : undefined}
  >
    {display ?? image}
  </ClipboardCopy>
);
