import React, { useState } from "react";
import { Button, Tooltip } from "@patternfly/react-core";
import { useAnnounce } from "../state/LiveAnnouncer";

interface CopyableTextProps {
  /** Text shown in the page (may be shortened). */
  text: React.ReactNode;
  /** Full value, shown in the tooltip and copied on click. */
  value: string;
  /** What the value is, for the accessible name ("image reference"). */
  what?: string;
  code?: boolean;
}

/**
 * Shortened text whose full value is reachable without a mouse (WCAG 2.1.1):
 * it is a focusable button, the tooltip shows on hover and keyboard focus,
 * and activating it copies the full value.
 */
export const CopyableText: React.FC<CopyableTextProps> = ({ text, value, what = "value", code = false }) => {
  const [copied, setCopied] = useState(false);
  const announce = useAnnounce();
  const copy = () => {
    navigator.clipboard?.writeText(value).then(() => {
      setCopied(true);
      announce(`Copied ${what} to the clipboard`);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => { /* clipboard may be blocked */ });
  };
  return (
    <Tooltip content={copied ? "Copied" : <span style={{ overflowWrap: "anywhere" }}>{value}</span>}>
      <Button
        variant="link"
        isInline
        onClick={copy}
        aria-label={`${typeof text === "string" ? text : value}: copy full ${what}`}
        style={{ color: "inherit", textAlign: "start", whiteSpace: "normal", overflowWrap: "anywhere" }}
      >
        {code ? <code>{text}</code> : text}
      </Button>
    </Tooltip>
  );
};
