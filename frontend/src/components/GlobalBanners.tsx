import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  AlertGroup,
  Button,
} from "@patternfly/react-core";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
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
import { sentence, shortTarget } from "../build";

const INTERRUPTED_DISMISS_KEY = "rhoai-interrupted-dismissed";
const TEMPLATE_DISMISS_KEY = "rhoai-template-hint-dismissed";

/** Notices shown at once; the rest collapse behind "View N more notices" (PF: at most 3 alerts). */
export const MAX_VISIBLE_NOTICES = 2;

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
  if (type.includes("minio") || type.includes("mlflow") || type.includes("pipeline")) return ["/test-resources", "Open Test resources"];
  if (type.includes("dashboard")) return ["/dashboard-dev", "Open Dashboard Dev"];
  if (type === "diagnostics-fix" || type === "assist-rollout") return ["/diagnostics", "Open Diagnostics"];
  if (type === "repair-dsc") return ["/components", "Open Components"];
  return null;
}

/** What to do after an interrupted operation, by type. */
function interruptedGuidance(op: InterruptedOperation): string {
  switch (operationKindForServerType(op.type, op.target) ? "operator" : op.type) {
    case "operator":
      return "The operator may be partly changed. Check the operator status on the Status page, then run the same operation again: every step is safe to repeat. Diagnostics can help if the operator does not come back.";
    case "deploy-dashboard-pr":
    case "deploy-dashboard-main":
    case "revert-dashboard":
      return "The dashboard may be partly switched. Open Dashboard Dev and run Revert, which is safe to repeat, or deploy again.";
    default:
      return "Check the result on the related page and run it again if needed.";
  }
}

/** One app-wide notice; lower `priority` shows first. */
interface Notice {
  key: string;
  priority: number;
  element: React.ReactElement;
}

const BuildId: React.FC<{ target: string }> = ({ target }) => (
  <code className="pf-v6-u-text-nowrap" title={target}>{shortTarget(target)}</code>
);

function useOperationNotice(onStatusPage: boolean): Notice | null {
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
    return {
      key: "operation",
      priority: 1,
      element: (
        <Alert
          variant="info"
          isInline
          component="p"
          customIcon={<InProgressIcon />}
          title={`Your ${OPERATION_NAMES[ownStream.kind].toLowerCase()} is running: ${step} (${formatElapsed(ownStream.startedAt, now)})`}
          actionLinks={<AlertActionLink onClick={() => navigate("/")}>View progress</AlertActionLink>}
        >
          Leaving this page does not stop it.
        </Alert>
      ),
    };
  }
  if (!server.inProgress) return null;
  if (!op) {
    return {
      key: "operation",
      priority: 1,
      element: (
        <Alert variant="info" isInline component="p" customIcon={<InProgressIcon />} title="Another operation is changing this cluster">
          Changes are disabled until it finishes.
        </Alert>
      ),
    };
  }
  const kind = operationKindForServerType(op.type, op.target);
  // On the Status page the progress card shows streamed operator operations.
  if (onStatusPage && kind) return null;
  const step = serverOperationStep(op);
  const started = Date.parse(op.startedAt);
  const elapsed = Number.isFinite(started) ? ` (${formatElapsed(started, now)})` : "";
  const link = followLink(op.type);
  return {
    key: "operation",
    priority: 1,
    element: (
      <Alert
        variant="info"
        isInline
        component="p"
        customIcon={<InProgressIcon />}
        title={`${describeServerOperation(op, status?.cluster.user)}${step ? `: ${step}` : ""}${elapsed}`}
        actionLinks={link ? <AlertActionLink onClick={() => navigate(link[0])}>{link[1]}</AlertActionLink> : undefined}
      >
        {op.target && <>Target: <BuildId target={op.target} />. </>}
        {op.remote && op.message && <>Latest progress: {sentence(op.message)} </>}
        {op.remote && "Another updater pod runs it (for example one that is shutting down finishes its operation first). "}
        Cluster changes from this tool are disabled until it finishes.
      </Alert>
    ),
  };
}

function useInterruptedNotice(): Notice | null {
  const { server } = useOperation();
  const op = server.inProgress ? null : server.interrupted;
  const [dismissed, setDismissed] = useState(() => readSession(INTERRUPTED_DISMISS_KEY));
  if (!op || dismissed === op.startedAt) return null;
  return {
    key: "interrupted",
    priority: 3,
    element: (
      <Alert
        variant="warning"
        isInline
        isExpandable
        component="p"
        title={`"${op.label || op.type}" was interrupted`}
        actionClose={<AlertActionCloseButton onClose={() => { writeSession(INTERRUPTED_DISMISS_KEY, op.startedAt); setDismissed(op.startedAt); }} />}
      >
        {op.user || "Someone"} started it {formatRelativeTime(op.startedAt)}
        {op.target && <> (target <BuildId target={op.target} />)</>}, and the updater pod
        {op.pod ? ` ${op.pod}` : ""} stopped before it finished. {interruptedGuidance(op)}
      </Alert>
    ),
  };
}

function overrideWhat(o: DashboardOverride): string {
  if (!o.sessionRecorded) return "dashboard-operator was scaled down outside this tool";
  const what = o.mode === "pr" && o.prNumber ? `PR #${o.prNumber}` : o.mode === "main" ? "the main branch" : "a dashboard build";
  const flavor = o.flavor === "odh" ? " (ODH build)" : o.flavor === "rhoai" ? " (RHOAI build)" : "";
  const who = o.startedBy ? `${o.startedBy} deployed` : "Deployed";
  const when = o.startedAt ? ` ${formatRelativeTime(o.startedAt)}` : "";
  return `${who} ${what}${flavor}${when}`;
}

/**
 * A04-1: a paused dashboard-operator is invisible unless every page says
 * so. The Dashboard Dev page's session panel says it all there, so the
 * notice is not repeated on that page.
 */
function useDashboardOverrideNotice(onDashboardDevPage: boolean): Notice | null {
  const { override } = useDashboardOverride();
  const navigate = useNavigate();
  if (!override?.active || onDashboardDevPage) return null;
  const variant = override.dashboardDeleting ? "danger" : override.stale ? "warning" : "info";
  const title = override.dashboardDeleting
    ? "A dashboard deletion is waiting for the paused dashboard-operator"
    : override.stale
      ? "RHOAI was updated while the dashboard was paused"
      : "Dashboard Dev session active: the dashboard does not follow RHOAI updates";
  const reasons = [...(override.staleReasons ?? []), ...(override.dashboardDeleting ? override.warnings ?? [] : [])];
  return {
    key: "dashboard-override",
    priority: override.dashboardDeleting ? 2 : 5,
    element: (
      <Alert
        variant={variant}
        isInline
        isExpandable
        component="p"
        title={title}
        actionLinks={<AlertActionLink onClick={() => navigate("/dashboard-dev")}>Open Dashboard Dev</AlertActionLink>}
      >
        {overrideWhat(override)}; dashboard-operator stays paused until someone reverts it on the Dashboard Dev page.
        {override.stale && " The dashboard still runs the images from before the update."}
        {reasons.length > 0 && <> {reasons.map(sentence).join(" ")}</>}
        {" "}Update, Re-deploy and Reinstall offer to revert it first.
      </Alert>
    ),
  };
}

function useTemplateNotice(): Notice | null {
  const version = useVersion();
  const [dismissed, setDismissed] = useState(() => readSession(TEMPLATE_DISMISS_KEY) === "1");
  if (!version?.templateOutdated || dismissed) return null;
  return {
    key: "template",
    priority: 9,
    element: (
      <Alert
        variant="info"
        isInline
        isExpandable
        component="p"
        title="For admins: this updater's deployment is out of date"
        actionClose={<AlertActionCloseButton onClose={() => { writeSession(TEMPLATE_DISMISS_KEY, "1"); setDismissed(true); }} />}
      >
        Build {version.version} expects deployment template revision {version.expectedTemplateRevision || "?"}, but this
        Deployment was created from {version.templateRevision ? `revision ${version.templateRevision}` : "an older template"}.
        Re-apply it from the repository with <code>make upgrade</code>.
      </Alert>
    ),
  };
}

function usePermissionNotice(): Notice | null {
  const permissions = usePermissions();
  const sessionExpired = useSessionExpired();
  if (sessionExpired) return null;
  if (permissions.status === "unknown") {
    return {
      key: "permissions",
      priority: 2,
      element: (
        <Alert
          variant="warning"
          isInline
          isExpandable
          component="p"
          title="Your permissions could not be checked; changes are disabled"
          actionLinks={<AlertActionLink onClick={permissions.retry}>Retry</AlertActionLink>}
        >
          {sentence(permissions.error?.message || "The OpenShift API did not answer")} The page stays read-only until the check
          succeeds; it is retried automatically.
        </Alert>
      ),
    };
  }
  if (permissions.status === "denied") {
    return {
      key: "permissions",
      priority: 8,
      element: (
        <Alert variant="info" isInline isPlain component="p" title="Read-only access">
          Changing the cluster through this tool requires the cluster-admin role. You can still view everything.
        </Alert>
      ),
    };
  }
  return null;
}

function useReconcileTimeoutNotice(): Notice | null {
  const { state, dismissTimeout } = useOperation();
  if (!state.reconcile.timedOut) return null;
  return {
    key: "reconcile-timeout",
    priority: 4,
    element: (
      <Alert
        variant="warning"
        title="Stopped watching the operator install after 10 minutes"
        isInline
        component="p"
        actionClose={<AlertActionCloseButton onClose={dismissTimeout} />}
      >
        The operator may still be installing. Refresh the Status page to check it, or open Diagnostics.
      </Alert>
    ),
  };
}

/**
 * App-wide notices, rendered by PageHeader under each page title: the
 * running or interrupted operation (any user), permissions, a paused
 * dashboard-operator and the deployment template hint. At most
 * MAX_VISIBLE_NOTICES show at once, most important first; the rest are one
 * click away. An expired session replaces the whole page instead (App).
 */
export const GlobalBanners: React.FC = () => {
  const { pathname } = useLocation();
  const [showAll, setShowAll] = useState(false);
  const notices = [
    useOperationNotice(pathname === "/"),
    usePermissionNotice(),
    useInterruptedNotice(),
    useReconcileTimeoutNotice(),
    useDashboardOverrideNotice(pathname === "/dashboard-dev"),
    useTemplateNotice(),
  ].filter((n): n is Notice => n !== null).sort((a, b) => a.priority - b.priority);
  if (notices.length === 0) return null;
  const hidden = showAll ? 0 : Math.max(0, notices.length - MAX_VISIBLE_NOTICES);
  const visible = hidden > 0 ? notices.slice(0, MAX_VISIBLE_NOTICES) : notices;
  const extra = notices.length - MAX_VISIBLE_NOTICES;
  return (
    <div>
      <AlertGroup aria-label="Notices">
        {visible.map((n) => React.cloneElement(n.element, { key: n.key }))}
      </AlertGroup>
      {extra > 0 && (
        // A plain link: AlertGroup's overflow button is styled for toast groups.
        <Button variant="link" isInline className="pf-v6-u-mt-sm" onClick={() => setShowAll(!showAll)} aria-expanded={showAll}>
          {hidden > 0 ? `View ${hidden} more ${hidden === 1 ? "notice" : "notices"}` : "Show fewer notices"}
        </Button>
      )}
    </div>
  );
};
