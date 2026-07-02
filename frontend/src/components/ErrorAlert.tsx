import React from "react";
import {
  Alert,
  AlertActionLink,
  PageSection,
} from "@patternfly/react-core";

interface ErrorAlertProps {
  error: string;
  /** Override the generic error title (default: "Error") */
  genericTitle?: string;
}

/**
 * Shared error alert that parses 401:/403:/409: prefixes from API errors
 * and shows the appropriate PatternFly alert with contextual actions.
 */
export const ErrorAlert: React.FC<ErrorAlertProps> = React.memo(({
  error,
  genericTitle = "Error",
}) => {
  if (error.startsWith("401:")) {
    return (
      <PageSection padding={{ default: "noPadding" }}>
        <Alert
          variant="danger"
          title="Session expired"
          isInline
          actionLinks={
            <AlertActionLink onClick={() => window.location.reload()}>
              Reload page
            </AlertActionLink>
          }
        >
          Session expired -- please refresh the page to re-authenticate.
        </Alert>
      </PageSection>
    );
  }

  if (error.startsWith("403:")) {
    return (
      <PageSection padding={{ default: "noPadding" }}>
        <Alert variant="danger" title="Access denied" isInline>
          You don&apos;t have permission to access this tool. You need access to
          the <code>redhat-ods-operator</code> namespace.
        </Alert>
      </PageSection>
    );
  }

  if (error.startsWith("409:")) {
    return (
      <PageSection padding={{ default: "noPadding" }}>
        <Alert
          variant="warning"
          title="Cluster busy"
          isInline
          actionLinks={
            <AlertActionLink onClick={() => window.location.reload()}>
              Refresh status
            </AlertActionLink>
          }
        >
          Another operation is in progress on the cluster. Please wait and try
          again.
        </Alert>
      </PageSection>
    );
  }

  return (
    <PageSection padding={{ default: "noPadding" }}>
      <Alert variant="danger" title={genericTitle} isInline>
        {error}
      </Alert>
    </PageSection>
  );
});
