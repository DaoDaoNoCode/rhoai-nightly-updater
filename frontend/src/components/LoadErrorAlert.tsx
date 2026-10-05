import React from "react";
import { Alert, AlertActionLink, PageSection } from "@patternfly/react-core";
import { describeLoadError } from "../outcomes";

interface LoadErrorAlertProps {
  error: unknown;
  genericTitle: string;
  onRetry?: () => void;
  /** Data from an earlier load is still shown below. */
  stale?: boolean;
}

/** A failed page load, classified by errorCode, with a Retry action (A08-6, A04-9). */
export const LoadErrorAlert: React.FC<LoadErrorAlertProps> = ({ error, genericTitle, onRetry, stale }) => {
  const { title, body, variant } = describeLoadError(error, genericTitle);
  return (
    <PageSection>
      <Alert
        variant={variant}
        title={title}
        isInline
        isLiveRegion
        component="p"
        actionLinks={onRetry ? <AlertActionLink onClick={onRetry}>Retry</AlertActionLink> : undefined}
      >
        {body}
        {stale ? " The data below is from the last successful refresh." : ""}
      </Alert>
    </PageSection>
  );
};
