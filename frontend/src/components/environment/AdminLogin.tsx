import React, { useState } from "react";
import {
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInputGroup,
  TextInputGroupMain,
  TextInputGroupUtilities,
} from "@patternfly/react-core";
import CopyIcon from "@patternfly/react-icons/dist/esm/icons/copy-icon";
import EyeIcon from "@patternfly/react-icons/dist/esm/icons/eye-icon";
import EyeSlashIcon from "@patternfly/react-icons/dist/esm/icons/eye-slash-icon";
import { getS3AdminLogin, toApiError } from "../../services/api";
import { TooltipButton } from "../TooltipButton";

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

/** What a hidden password field shows: dots, not the password (it is not fetched until asked for). */
const HIDDEN = "••••••••••";

/**
 * The storage's admin UI password: masked, with PatternFly's password
 * pattern (a TextInputGroup with an eye toggle) and a copy button. It is
 * fetched from the server only to show or copy it, held only while shown,
 * and gone when hidden or when the page or tab closes (unmount). For a
 * user who cannot read it, both buttons say why.
 */
export const AdminPassword: React.FC<{ user: string; disabledReason: string | null }> = ({ user, disabledReason }) => {
  const [password, setPassword] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const fetchPassword = async () => (await getS3AdminLogin()).password;

  const toggle = async () => {
    setOutcome(null);
    if (password !== null) {
      setPassword(null);
      return;
    }
    setBusy(true);
    try {
      setPassword(await fetchPassword());
    } catch (e) {
      setOutcome({ ok: false, text: toApiError(e, "Could not read the password").message });
    } finally {
      setBusy(false);
    }
  };
  const copy = async () => {
    setOutcome(null);
    try {
      await copyFetchedText(password !== null ? async () => password : fetchPassword);
      setOutcome({ ok: true, text: `Copied the password of user ${user}.` });
    } catch (e) {
      setOutcome({ ok: false, text: toApiError(e, "Could not copy").message });
    }
  };

  return (
    // The description list's "Password" term names the field; the input's
    // own label says whose it is.
    <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
      <FormGroup fieldId="storage-admin-password">
        <TextInputGroup>
          <TextInputGroupMain inputId="storage-admin-password" type={password !== null ? "text" : "password"} value={password ?? HIDDEN}
            inputProps={{ readOnly: true }} aria-label={`Password of ${user}`} />
          <TextInputGroupUtilities>
            <TooltipButton variant="plain" aria-label={password !== null ? "Hide password" : "Show password"} icon={password !== null ? <EyeSlashIcon /> : <EyeIcon />}
              isLoading={busy} disabledReason={disabledReason} onClick={() => void toggle()} />
            <TooltipButton variant="plain" aria-label="Copy password" icon={<CopyIcon />} disabledReason={disabledReason} onClick={() => void copy()} />
          </TextInputGroupUtilities>
        </TextInputGroup>
        {/* The live region exists before its text, so the outcome is announced. */}
        <FormHelperText>
          <HelperText isLiveRegion>
            {outcome && <HelperTextItem variant={outcome.ok ? "success" : "error"}>{outcome.text}</HelperTextItem>}
          </HelperText>
        </FormHelperText>
      </FormGroup>
    </Form>
  );
};
