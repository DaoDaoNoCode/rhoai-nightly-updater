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
import BanIcon from "@patternfly/react-icons/dist/esm/icons/ban-icon";
import { useNavigate } from "react-router-dom";
import { describeLoadError } from "../outcomes";
import { ADMIN_ONLY_BODY, describeError, isNotForUser } from "../errors";
import { SIGN_IN_URL } from "./ErrorAlert";
import { TechnicalDetails } from "./LongText";
import { gitOpsProfileReason } from "../profile";

/** An empty state's way back: Go to Status. */
export const GoToStatusAction: React.FC = () => {
  const navigate = useNavigate();
  return (
    <EmptyStateActions>
      <Button variant="primary" onClick={() => navigate("/")}>Go to Status</Button>
    </EmptyStateActions>
  );
};

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
  /** In its own PageSection (the default), or inside one (a tab's panel). */
  inSection?: boolean;
  /** Offer Diagnostics (default true); off for users who cannot open it. */
  diagnostics?: boolean;
}

/**
 * A back-end failure that leaves the page with nothing to show: what
 * failed in plain words, Retry, Diagnostics, and the raw error on demand.
 */
export const PageErrorState: React.FC<PageErrorStateProps> = ({ error, title, onRetry, inSection = true, diagnostics = true }) => {
  const navigate = useNavigate();
  // A read the user may not make (per-user views) is not a failure: no Retry, no Diagnostics.
  if (isNotForUser(error)) return <NotForUserState error={error} inSection={inSection} />;
  const { title: cause, body } = describeLoadError(error, title);
  const state = (
    <EmptyState
      headingLevel="h2"
      titleText={title}
      icon={ExclamationCircleIcon}
      status="danger"
      variant={EmptyStateVariant.lg}
    >
      <EmptyStateBody>
        {cause !== title && <><strong>{cause}.</strong>{" "}</>}{diagnostics ? "Retry in a moment; Diagnostics checks the cluster and the operator." : "Retry in a moment."}
        {body && <TechnicalDetails text={body} toggleText="Show the error" />}
      </EmptyStateBody>
      <EmptyStateFooter>
        <EmptyStateActions>
          <Button variant="primary" onClick={onRetry}>Retry</Button>
        </EmptyStateActions>
        {diagnostics && (
          <EmptyStateActions>
            <Button variant="link" onClick={() => navigate("/diagnostics")}>Open Diagnostics</Button>
          </EmptyStateActions>
        )}
      </EmptyStateFooter>
    </EmptyState>
  );
  return inSection ? <PageSection isFilled>{state}</PageSection> : state;
};

/**
 * The page's files could not be loaded (a chunk-load failure, usually after
 * the updater was updated while the tab was open): only a reload loads them
 * again. The masthead and navigation stay.
 */
export const PageLoadFailedState: React.FC<{ onReload: () => void }> = ({ onReload }) => (
  <PageSection isFilled>
    <EmptyState headingLevel="h1" titleText="This page could not be loaded" icon={ExclamationCircleIcon} status="warning" variant={EmptyStateVariant.lg}>
      <EmptyStateBody>
        A new version of the updater may have been installed since this tab was opened, or the connection dropped. Reload to load the page again.
      </EmptyStateBody>
      <EmptyStateFooter>
        <EmptyStateActions>
          <Button variant="primary" onClick={onReload}>Reload</Button>
        </EmptyStateActions>
      </EmptyStateFooter>
    </EmptyState>
  </PageSection>
);

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

/**
 * A page the gitops profile does not have (docs/GITOPS.md, "Profiles"):
 * what is missing and why, and the way back to Status.
 */
export const ProfileOffState: React.FC<{ feature: string }> = ({ feature }) => {
  return (
    <PageSection isFilled>
      <EmptyState headingLevel="h1" titleText={`${feature} is not available here`} icon={BanIcon} variant={EmptyStateVariant.lg}>
        <EmptyStateBody>{gitOpsProfileReason(feature)} Change the cluster in Git instead.</EmptyStateBody>
        <EmptyStateFooter>
          <GoToStatusAction />
        </EmptyStateFooter>
      </EmptyState>
    </PageSection>
  );
};

/** The whole page for a signed-in user who is not a cluster-admin, in the gitops profile. */
export const AdminRequiredState: React.FC = () => (
  <PageSection isFilled>
    <EmptyState headingLevel="h1" titleText="Cluster-admin required" icon={LockIcon} variant={EmptyStateVariant.lg}>
      <EmptyStateBody>
        This updater runs the gitops profile, where only cluster-admins can see what it reads from the cluster and install the build
        the managed catalog serves. Ask a cluster-admin, or check the cluster through Argo CD, ACM and the OpenShift console.
      </EmptyStateBody>
      <EmptyStateFooter>
        <EmptyStateActions>
          <Button variant="link" onClick={() => window.location.reload()}>Reload page</Button>
        </EmptyStateActions>
      </EmptyStateFooter>
    </EmptyState>
  </PageSection>
);

/**
 * A read the signed-in user may not make: 403 forbidden_for_user (their
 * own OpenShift account may not read it) or admin_only (only
 * cluster-admins read it through the updater). Says so plainly, without
 * Retry or Diagnostics: nothing is broken.
 */
export const NotForUserState: React.FC<{ error: unknown; inSection?: boolean; headingLevel?: "h1" | "h2" | "h3" }> = ({ error, inSection = true, headingLevel = "h2" }) => {
  const { title, body } = describeError(error);
  const state = (
    <EmptyState headingLevel={headingLevel} titleText={title} icon={LockIcon} variant={EmptyStateVariant.lg}>
      <EmptyStateBody>{body}</EmptyStateBody>
    </EmptyState>
  );
  return inSection ? <PageSection isFilled>{state}</PageSection> : state;
};

/** A page only cluster-admins see (Components, Dashboard Dev, Diagnostics), opened by a user who is not one. */
export const AdminOnlyPageState: React.FC = () => {
  return (
    <PageSection isFilled>
      <EmptyState headingLevel="h1" titleText="Only cluster-admins can see this page" icon={LockIcon} variant={EmptyStateVariant.lg}>
        <EmptyStateBody>{ADMIN_ONLY_BODY}</EmptyStateBody>
        <EmptyStateFooter>
          <GoToStatusAction />
        </EmptyStateFooter>
      </EmptyState>
    </PageSection>
  );
};

/** The gitops profile when the permission check could not answer: nothing is shown until it does. */
export const PermissionsUnknownState: React.FC<{ onRetry: () => void }> = ({ onRetry }) => (
  <PageSection isFilled>
    <EmptyState headingLevel="h1" titleText="Your permissions could not be checked" icon={ExclamationCircleIcon} status="warning" variant={EmptyStateVariant.lg}>
      <EmptyStateBody>The OpenShift API did not answer. The check is retried automatically.</EmptyStateBody>
      <EmptyStateFooter>
        <EmptyStateActions>
          <Button variant="primary" onClick={onRetry}>Retry</Button>
        </EmptyStateActions>
      </EmptyStateFooter>
    </EmptyState>
  </PageSection>
);
