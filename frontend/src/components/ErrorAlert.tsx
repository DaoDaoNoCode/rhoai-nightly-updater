import React from "react";
import {
  Alert,
  AlertActionLink,
  PageSection,
} from "@patternfly/react-core";
import { describeError } from "../errors";
import { TruncatedText } from "./LongText";

interface ErrorAlertProps {
  error: unknown;
  /** Override the generic error title (default: "Error") */
  genericTitle?: string;
  /** Offer a Retry action. */
  onRetry?: () => void;
  /** Extra actions after Retry (for example a link to Diagnostics). */
  extraActions?: React.ReactNode;
  /** Render without the PageSection wrapper (inside a card or a stack). */
  inline?: boolean;
}

/** Where oauth-proxy starts a new sign-in (it also clears the old session cookie). */
export const SIGN_IN_URL = "/oauth/sign_in";

/**
 * Shared error alert. Picks the title by HTTP status and errorCode (session
 * expired, access denied, cluster busy, API unavailable, ...), shows the
 * cause and the next step, and announces itself to screen readers.
 */
export const ErrorAlert: React.FC<ErrorAlertProps> = React.memo(({
  error,
  genericTitle = "Error",
  onRetry,
  extraActions,
  inline = false,
}) => {
  const { title, body, variant, reload, hint } = describeError(error, genericTitle);
  const actions = reload ? (
    <>
      <AlertActionLink component="a" href={SIGN_IN_URL}>Sign in again</AlertActionLink>
      <AlertActionLink onClick={() => window.location.reload()}>Reload page</AlertActionLink>
    </>
  ) : onRetry || extraActions ? (
    <>
      {onRetry && <AlertActionLink onClick={onRetry}>Retry</AlertActionLink>}
      {extraActions}
    </>
  ) : undefined;
  const alert = (
    <Alert
      variant={variant}
      title={title}
      isInline
      isLiveRegion
      component="p"
      actionLinks={actions}
    >
      <TruncatedText>{body}{hint && <> {hint}</>}</TruncatedText>
    </Alert>
  );
  if (inline) return alert;
  return <PageSection>{alert}</PageSection>;
});
ErrorAlert.displayName = "ErrorAlert";
