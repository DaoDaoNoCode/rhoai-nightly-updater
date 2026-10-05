import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Spinner,
} from "@patternfly/react-core";
import { useLocation, useNavigate } from "react-router-dom";
import type { DashboardOverride, InterruptedOperation, ServerOperation } from "../types";
import { useClusterStatus, useOperation } from "../state/AppState";
import {
  describeServerOperation,
  serverOperationStep,
  useDashboardOverride,
  usePermissions,
  useSessionExpired,
  useVersion,
} from "../state/AppInfo";
import { isRunning } from "../state/operation";
import { OPERATION_NAMES, STEP_SETS, operationKindForServerType, stepLabel } from "../operationSteps";
import { formatElapsed, formatRelativeTime } from "../utils";
import { SIGN_IN_URL } from "./ErrorAlert";

const INTERRUPTED_DISMISS_KEY = "rhoai-interrupted-dismissed";
const TEMPLATE_DISMISS_KEY = "rhoai-template-hint-dismissed";

function readSession(key: string): string | null {
  try { return sessionStorage.getItem(key); } catch { return null; }
}
function writeSession(key: string, value: string): void {
  try { sessionStorage.setItem(key, value); } catch { /* sessionStorage may be disabled */ }
}

/** Ticks once a second while `active`, so elapsed times stay current. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/** Where to follow an operation of this backend type, as [path, link text]. */
function followLink(type: string): [string, string] | null {
  if (operationKindForServerType(type)) return ["/", "View progress"];
  if (type.includes("dashboard") || type.includes("minio") || type.includes("mlflow") || type.includes("pipeline")) return ["/dashboard-dev", "Open Dashboard Dev"];
  if (type === "diagnostics-fix" || type === "assist-rollout") return ["/diagnostics", "Open Diagnostics"];
  if (type === "repair-dsc") return ["/components", "Open Components"];
  return null;
}

/** What to do after an interrupted operation, by type. */
function interruptedGuidance(op: InterruptedOperation): string {
  switch (operationKindForServerType(op.type, op.target) ? "operator" : op.type) {
    case "operator":
      return "The operator may be partly changed. Check the operator status below, then run the same operation again: every step is safe to repeat. Diagnostics can help if the operator does not come back.";
    case "deploy-dashboard-pr":
    case "deploy-dashboard-main":
    case "revert-dashboard":
      return "The dashboard may be partly switched. Open Dashboard Dev and run Revert, which is safe to repeat, or deploy again.";
    default:
      return "Check the result on the related page and run it again if needed.";
  }
}

const OperationBanner: React.FC<{ onStatusPage: boolean }> = ({ onStatusPage }) => {
  const { run, server } = useOperation();
  const { status } = useClusterStatus();
  const navigate = useNavigate();
  const ownStream = isRunning(run) && run.source === "stream" ? run : null;
  const op: ServerOperation | null = server.inProgress ? server.operation : null;
  const now = useNow(!!ownStream || !!op);

  if (ownStream) {
    // The Status page shows this tab's own run in full.
    if (onStatusPage) return null;
    const current = [...ownStream.steps].reverse().find((s) => s.status === "running") ?? ownStream.steps[ownStream.steps.length - 1];
    const defs = STEP_SETS[ownStream.kind];
    const index = current ? defs.findIndex((d) => d.id === current.step) : -1;
    const step = current ? (index >= 0 ? `step ${index + 1} of ${defs.length}: ${stepLabel(ownStream.kind, current.step)}` : stepLabel(ownStream.kind, current.step)) : "starting";
    return (
      <Alert
        variant="info"
        isInline
        component="p"
        customIcon={<Spinner size="md" aria-hidden="true" />}
        title={`Your ${OPERATION_NAMES[ownStream.kind].toLowerCase()} is running: ${step} (${formatElapsed(ownStream.startedAt, now)})`}
        actionLinks={<AlertActionLink onClick={() => navigate("/")}>View progress</AlertActionLink>}
      >
        Leaving this page does not stop it.
      </Alert>
    );
  }
  if (!server.inProgress) return null;
  if (!op) {
    return (
      <Alert variant="info" isInline component="p" customIcon={<Spinner size="md" aria-hidden="true" />} title="Another operation is changing this cluster">
        Changes are disabled until it finishes.
      </Alert>
    );
  }
  const kind = operationKindForServerType(op.type, op.target);
  // On the Status page the progress card shows streamed operator operations.
  if (onStatusPage && kind) return null;
  const step = serverOperationStep(op);
  const started = Date.parse(op.startedAt);
  const elapsed = Number.isFinite(started) ? ` (${formatElapsed(started, now)})` : "";
  const link = followLink(op.type);
  return (
    <Alert
      variant="info"
      isInline
      component="p"
      customIcon={<Spinner size="md" aria-hidden="true" />}
      title={`${describeServerOperation(op, status?.cluster.user)}${step ? `: ${step}` : ""}${elapsed}`}
      actionLinks={link ? <AlertActionLink onClick={() => navigate(link[0])}>{link[1]}</AlertActionLink> : undefined}
    >
      {op.target && <>Target: <code style={{ overflowWrap: "anywhere" }}>{op.target}</code>. </>}
      Cluster changes from this tool are disabled until it finishes.
    </Alert>
  );
};

const InterruptedBanner: React.FC = () => {
  const { server } = useOperation();
  const op = server.inProgress ? null : server.interrupted;
  const [dismissed, setDismissed] = useState(() => readSession(INTERRUPTED_DISMISS_KEY));
  if (!op || dismissed === op.startedAt) return null;
  return (
    <Alert
      variant="warning"
      isInline
      component="p"
      title={`"${op.label || op.type}" was interrupted`}
      actionClose={<AlertActionCloseButton onClose={() => { writeSession(INTERRUPTED_DISMISS_KEY, op.startedAt); setDismissed(op.startedAt); }} />}
    >
      {op.user || "Someone"} started it {formatRelativeTime(op.startedAt)}
      {op.target && <> (target <code style={{ overflowWrap: "anywhere" }}>{op.target}</code>)</>}, and the updater pod
      {op.pod ? ` ${op.pod}` : ""} stopped before it finished. {interruptedGuidance(op)}
    </Alert>
  );
};

function overrideWhat(o: DashboardOverride): string {
  if (!o.sessionRecorded) return "dashboard-operator was scaled down outside this tool";
  const what = o.mode === "pr" && o.prNumber ? `PR #${o.prNumber}` : o.mode === "main" ? "the main branch" : "a dashboard build";
  const flavor = o.flavor === "odh" ? " (ODH build)" : o.flavor === "rhoai" ? " (RHOAI build)" : "";
  const who = o.startedBy ? `${o.startedBy} deployed` : "Deployed";
  const when = o.startedAt ? ` ${formatRelativeTime(o.startedAt)}` : "";
  return `${who} ${what}${flavor}${when}`;
}

/** A04-1: a paused dashboard-operator is invisible unless every page says so. */
export const DashboardOverrideBanner: React.FC<{ onDashboardDevPage: boolean }> = ({ onDashboardDevPage }) => {
  const { override } = useDashboardOverride();
  const navigate = useNavigate();
  if (!override?.active) return null;
  const severe = override.dashboardDeleting || override.stale;
  // The Dashboard Dev page describes a healthy session itself.
  if (onDashboardDevPage && !severe) return null;
  const variant = override.dashboardDeleting ? "danger" : override.stale ? "warning" : "info";
  const title = override.dashboardDeleting
    ? "A dashboard deletion is waiting for the paused dashboard-operator"
    : override.stale
      ? "RHOAI was updated while the dashboard was paused"
      : "Dashboard Dev session active: the dashboard does not follow RHOAI updates";
  const reasons = [...(override.staleReasons ?? []), ...(override.dashboardDeleting ? override.warnings ?? [] : [])];
  return (
    <Alert
      variant={variant}
      isInline
      component="p"
      title={title}
      actionLinks={onDashboardDevPage ? undefined : <AlertActionLink onClick={() => navigate("/dashboard-dev")}>Open Dashboard Dev</AlertActionLink>}
    >
      {overrideWhat(override)}; dashboard-operator stays paused until someone reverts it on the Dashboard Dev page.
      {override.stale && " The dashboard still runs the images from before the update."}
      {reasons.length > 0 && <> {reasons.join(" ")}</>}
      {" "}Update, Re-deploy and Reinstall offer to revert it first.
    </Alert>
  );
};

const TemplateBanner: React.FC = () => {
  const version = useVersion();
  const [dismissed, setDismissed] = useState(() => readSession(TEMPLATE_DISMISS_KEY) === "1");
  if (!version?.templateOutdated || dismissed) return null;
  return (
    <Alert
      variant="info"
      isInline
      component="p"
      title="For admins: this updater's deployment is out of date"
      actionClose={<AlertActionCloseButton onClose={() => { writeSession(TEMPLATE_DISMISS_KEY, "1"); setDismissed(true); }} />}
    >
      Build {version.version} expects deployment template revision {version.expectedTemplateRevision || "?"}, but this
      Deployment was created from {version.templateRevision ? `revision ${version.templateRevision}` : "an older template"}.
      Re-apply it from the repository with <code>make upgrade</code>.
    </Alert>
  );
};

const PermissionBanner: React.FC = () => {
  const permissions = usePermissions();
  const sessionExpired = useSessionExpired();
  if (sessionExpired) return null;
  if (permissions.status === "unknown") {
    return (
      <Alert
        variant="warning"
        isInline
        component="p"
        title="Your permissions could not be checked; changes are disabled"
        actionLinks={<AlertActionLink onClick={permissions.retry}>Retry</AlertActionLink>}
      >
        {permissions.error?.message || "The OpenShift API did not answer."} The page stays read-only until the check
        succeeds; it is retried automatically.
      </Alert>
    );
  }
  if (permissions.status === "denied") {
    return (
      <Alert variant="info" isInline isPlain component="p" title="Read-only access">
        Changing the cluster through this tool requires the cluster-admin role. You can still view everything.
      </Alert>
    );
  }
  return null;
};

const SessionBanner: React.FC = () => {
  const expired = useSessionExpired();
  if (!expired) return null;
  return (
    <Alert
      variant="danger"
      isInline
      component="p"
      title="Your session expired"
      actionLinks={
        <>
          <AlertActionLink component="a" href={SIGN_IN_URL}>Sign in again</AlertActionLink>
          <AlertActionLink onClick={() => window.location.reload()}>Reload page</AlertActionLink>
        </>
      }
    >
      Your OpenShift session has expired, so the updater can't read or change the cluster for you. Sign in again to continue.
    </Alert>
  );
};

const ReconcileTimeoutBanner: React.FC = () => {
  const { state, dismissTimeout } = useOperation();
  if (!state.reconcile.timedOut) return null;
  return (
    <Alert
      variant="warning"
      title="Stopped watching the operator install after 10 minutes"
      isInline
      component="p"
      actionClose={<AlertActionCloseButton onClose={dismissTimeout} />}
    >
      The operator may still be installing. Refresh the Status page to check it, or open Diagnostics.
    </Alert>
  );
};

/**
 * Page-level banners shown on every page: session, permissions, the running
 * or interrupted operation (any user), a paused dashboard-operator and the
 * deployment template hint.
 */
export const GlobalBanners: React.FC = () => {
  const { pathname } = useLocation();
  // The container collapses (CSS :empty) when no banner renders.
  return (
    <div className="rhoai-global-banners">
      <SessionBanner />
      <PermissionBanner />
      <OperationBanner onStatusPage={pathname === "/"} />
      <InterruptedBanner />
      <ReconcileTimeoutBanner />
      <DashboardOverrideBanner onDashboardDevPage={pathname === "/dashboard-dev"} />
      <TemplateBanner />
    </div>
  );
};
