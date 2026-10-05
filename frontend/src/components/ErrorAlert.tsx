import React from "react";
import {
  Alert,
  AlertActionLink,
  PageSection,
} from "@patternfly/react-core";
import { describeError } from "../errors";

interface ErrorAlertProps {
  error: unknown;
  /** Override the generic error title (default: "Error") */
  genericTitle?: string;
}

/**
 * Shared error alert. Picks the title by HTTP status and errorCode (session
 * expired, access denied, cluster busy, ...) and announces itself to screen
 * readers.
 */
export const ErrorAlert: React.FC<ErrorAlertProps> = React.memo(({
  error,
  genericTitle = "Error",
}) => {
  const { title, body, variant, reload } = describeError(error, genericTitle);
  return (
    <PageSection padding={{ default: "noPadding" }}>
      <Alert
        variant={variant}
        title={title}
        isInline
        isLiveRegion
        component="p"
        actionLinks={reload ? (
          <AlertActionLink onClick={() => window.location.reload()}>
            Reload page
          </AlertActionLink>
        ) : undefined}
      >
        {body}
      </Alert>
    </PageSection>
  );
});
ErrorAlert.displayName = "ErrorAlert";
