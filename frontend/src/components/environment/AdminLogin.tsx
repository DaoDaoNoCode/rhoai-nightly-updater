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

/** Which credential a field shows (the fields of GET /api/resources/minio/admin-password). */
export type S3CredentialField = "accessKey" | "secretKey" | "password";

/**
 * One of the storage's S3 keys, masked, with PatternFly's password pattern
 * (a TextInputGroup with an eye toggle) and a copy button. It is fetched
 * from the server only to show or copy it, held only while shown, and gone
 * when hidden or when the page or tab closes (unmount). For a user who
 * cannot read it, both buttons say why.
 */
export const S3SecretField: React.FC<{ id: string; field: S3CredentialField; label: string; disabledReason: string | null }> = ({ id, field, label, disabledReason }) => {
  const [value, setValue] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const fetchValue = async () => {
    const v = (await getS3AdminLogin())[field];
    // Only the admin UI password can be missing: storage an earlier version
    // set up used the S3 secret key there until Repair gives it its own.
    if (!v) throw new Error(`There is no ${label} yet. Repair the S3 storage to set one; the S3 keys and the data stay.`);
    return v;
  };

  const toggle = async () => {
    setOutcome(null);
    if (value !== null) {
      setValue(null);
      return;
    }
    setBusy(true);
    try {
      setValue(await fetchValue());
    } catch (e) {
      setOutcome({ ok: false, text: toApiError(e, `Could not read the ${label}`).message });
    } finally {
      setBusy(false);
    }
  };
  const copy = async () => {
    setOutcome(null);
    try {
      await copyFetchedText(value !== null ? async () => value : fetchValue);
      setOutcome({ ok: true, text: `Copied the ${label}.` });
    } catch (e) {
      setOutcome({ ok: false, text: toApiError(e, "Could not copy").message });
    }
  };

  return (
    // The description list's term names the field; the input has its own label.
    <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
      <FormGroup fieldId={id}>
        <TextInputGroup>
          <TextInputGroupMain inputId={id} type={value !== null ? "text" : "password"} value={value ?? HIDDEN}
            inputProps={{ readOnly: true }} aria-label={label} />
          <TextInputGroupUtilities>
            <TooltipButton variant="plain" aria-label={`${value !== null ? "Hide" : "Show"} the ${label}`} icon={value !== null ? <EyeSlashIcon /> : <EyeIcon />}
              isLoading={busy} disabledReason={disabledReason} onClick={() => void toggle()} />
            <TooltipButton variant="plain" aria-label={`Copy the ${label}`} icon={<CopyIcon />} disabledReason={disabledReason} onClick={() => void copy()} />
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

/**
 * The storage's connection as environment variables, fetched on click and
 * copied, never shown: what an S3 client (boto3, rclone, the AWS CLI)
 * reads, plus AWS_S3_BUCKET as the dashboard's data connections name it.
 */
export function s3EnvText(endpoint: string, keys: { accessKey: string; secretKey: string }): string {
  return [
    `AWS_ENDPOINT_URL=${endpoint}`,
    `AWS_ACCESS_KEY_ID=${keys.accessKey}`,
    `AWS_SECRET_ACCESS_KEY=${keys.secretKey}`,
    "AWS_DEFAULT_REGION=us-east-1",
    "AWS_S3_BUCKET=pipelines",
  ].join("\n") + "\n";
}

export const CopyS3EnvButton: React.FC<{ endpoint: string; disabledReason: string | null }> = ({ endpoint, disabledReason }) => {
  const [outcome, setOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  const copy = async () => {
    setOutcome(null);
    try {
      await copyFetchedText(async () => s3EnvText(endpoint, await getS3AdminLogin()));
      setOutcome({ ok: true, text: "Copied: AWS_ENDPOINT_URL, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_DEFAULT_REGION, AWS_S3_BUCKET." });
    } catch (e) {
      setOutcome({ ok: false, text: toApiError(e, "Could not copy").message });
    }
  };
  return (
    <div>
      <TooltipButton variant="secondary" icon={<CopyIcon />} disabledReason={disabledReason} onClick={() => void copy()}>Copy as environment variables</TooltipButton>
      <HelperText isLiveRegion className={outcome ? "pf-v6-u-mt-sm" : undefined}>
        {outcome && <HelperTextItem variant={outcome.ok ? "success" : "error"}>{outcome.text}</HelperTextItem>}
      </HelperText>
    </div>
  );
};
