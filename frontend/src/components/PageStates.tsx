import React from "react";
import {
  Button,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  EmptyStateVariant,
  PageSection,
  Spinner,
} from "@patternfly/react-core";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import LockIcon from "@patternfly/react-icons/dist/esm/icons/lock-icon";
import { useNavigate } from "react-router-dom";
import { describeLoadError } from "../outcomes";
import { SIGN_IN_URL } from "./ErrorAlert";
import { TechnicalDetails } from "./LongText";

/** First load of a page, or of a large part of it: PatternFly's loading empty state. */
export const PageLoading: React.FC<{ title: string; inSection?: boolean }> = ({ title, inSection = true }) => {
  const state = <EmptyState headingLevel="h2" titleText={title} icon={Spinner} variant={EmptyStateVariant.lg} />;
  return inSection ? <PageSection isFilled aria-busy="true">{state}</PageSection> : state;
};

interface PageErrorStateProps {
  error: unknown;
  /** "Can't load the cluster status" */
  title: string;
  onRetry: () => void;
}

/**
 * A back-end failure that leaves the page with nothing to show: what
 * failed in plain words, Retry, Diagnostics, and the raw error on demand.
 */
export const PageErrorState: React.FC<PageErrorStateProps> = ({ error, title, onRetry }) => {
  const navigate = useNavigate();
  const { title: cause, body } = describeLoadError(error, title);
  return (
    <PageSection isFilled>
      <EmptyState
        headingLevel="h2"
        titleText={title}
        icon={ExclamationCircleIcon}
        status="danger"
        variant={EmptyStateVariant.lg}
      >
        <EmptyStateBody>
          {cause !== title && <><strong>{cause}.</strong>{" "}</>}Retry in a moment; Diagnostics checks the cluster and the operator.
          {body && <TechnicalDetails text={body} toggleText="Show the error" />}
        </EmptyStateBody>
        <EmptyStateFooter>
          <EmptyStateActions>
            <Button variant="primary" onClick={onRetry}>Retry</Button>
          </EmptyStateActions>
          <EmptyStateActions>
            <Button variant="link" onClick={() => navigate("/diagnostics")}>Open Diagnostics</Button>
          </EmptyStateActions>
        </EmptyStateFooter>
      </EmptyState>
    </PageSection>
  );
};

/** The whole page once the OpenShift session expired: nothing else works until the user signs in again. */
export const SessionExpiredState: React.FC = () => (
  <PageSection isFilled>
    <EmptyState headingLevel="h1" titleText="Your session expired" icon={LockIcon} variant={EmptyStateVariant.lg}>
      <EmptyStateBody>
        Your OpenShift session has expired, so the updater can&apos;t read or change the cluster for you. Sign in again to continue.
      </EmptyStateBody>
      <EmptyStateFooter>
        <EmptyStateActions>
          <Button variant="primary" component="a" href={SIGN_IN_URL}>Sign in again</Button>
        </EmptyStateActions>
        <EmptyStateActions>
          <Button variant="link" onClick={() => window.location.reload()}>Reload page</Button>
        </EmptyStateActions>
      </EmptyStateFooter>
    </EmptyState>
  </PageSection>
);
