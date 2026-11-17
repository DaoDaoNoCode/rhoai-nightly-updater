import React, { useState } from "react";
import { HelperText, HelperTextItem } from "@patternfly/react-core";
import CopyIcon from "@patternfly/react-icons/dist/esm/icons/copy-icon";
import { TooltipButton } from "../TooltipButton";
import { toApiError } from "../../services/api";

/**
 * Copies text that the server returns to the clipboard, from a click.
 * Safari lets a page write to the clipboard only while the click is being
 * handled; a ClipboardItem whose content is a promise keeps that permission
 * while the request runs (W3C Clipboard API, ClipboardItem). Browsers
 * without ClipboardItem write the text once it has arrived. The text is
 * never stored by the page.
 */
export async function copyFetchedText(fetchText: () => Promise<string>): Promise<void> {
  const clipboard = navigator.clipboard;
  if (!clipboard) throw new Error("This browser does not allow this page to use the clipboard.");
  if (typeof ClipboardItem !== "undefined" && typeof clipboard.write === "function") {
    const text = fetchText();
    try {
      await clipboard.write([new ClipboardItem({ "text/plain": text.then((t) => new Blob([t], { type: "text/plain" })) })]);
    } catch (e) {
      // A failed request rejects the write too: report the request's reason.
      await text;
      throw e;
    }
    return;
  }
  await clipboard.writeText(await fetchText());
}

/**
 * A button that fetches a value from the server and copies it, with the
 * outcome as helper text under it (announced to screen readers).
 */
export const CopyFromServerButton: React.FC<{
  fetchText: () => Promise<string>;
  /** Shown after a copy, for example "Copied the password of user admin." */
  copiedText: string;
  disabledReason?: string | null;
  children: React.ReactNode;
}> = ({ fetchText, copiedText, disabledReason, children }) => {
  const [state, setState] = useState<{ busy: boolean; done?: boolean; error?: string }>({ busy: false });
  const onClick = async () => {
    setState({ busy: true });
    try {
      await copyFetchedText(fetchText);
      setState({ busy: false, done: true });
    } catch (e) {
      setState({ busy: false, error: toApiError(e, "Could not copy").message });
    }
  };
  return (
    <div>
      <TooltipButton variant="secondary" icon={<CopyIcon />} isLoading={state.busy} disabledReason={disabledReason} onClick={() => void onClick()}>{children}</TooltipButton>
      {/* The live region exists before its text, so the outcome is announced. */}
      <HelperText isLiveRegion className={state.done || state.error ? "pf-v6-u-mt-sm" : undefined}>
        {state.done && <HelperTextItem variant="success">{copiedText}</HelperTextItem>}
        {state.error && <HelperTextItem variant="error">{state.error}</HelperTextItem>}
      </HelperText>
    </div>
  );
};
