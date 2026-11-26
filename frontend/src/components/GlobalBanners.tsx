import React, { useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  AlertGroup,
  Button,
  ClipboardCopy,
  Content,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
} from "@patternfly/react-core";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
import { useLocation, useNavigate } from "react-router-dom";
import type { DashboardOverride, InterruptedOperation, ServerOperation } from "../types";
import { useClusterStatus, useOperation, type ProjectActionNotice } from "../state/AppState";
import { cancelOperation, toApiError } from "../services/api";
import {
  NO_PERMISSION_REASON,
  describeServerOperation,
  serverOperationStep,
  useDashboardOverride,
  usePermissions,
  useRole,
  useSessionExpired,
  useSignedInUser,
  useUpdateCheck,
  useVersion,
} from "../state/AppInfo";
import { isRunning } from "../state/operation";
import { OPERATION_NAMES, RECIPE_OPERATION_TYPE, STEP_SETS, operationKindForServerType, recipeTarget, stepLabel } from "../operationSteps";
import { formatClockTime, formatElapsed, formatRelativeTime } from "../utils";
import { sentence, shortTarget } from "../build";
import { BackgroundRunAlert, ViewProgressLink, useBackgroundRun, useViewServerOperation } from "./ProgressViews";
import { dashboardOperatorLabel } from "./dashboardOperator";
import { DocLink } from "./DocLink";

const INTERRUPTED_DISMISS_KEY = "rhoai-interrupted-dismissed";
const TEMPLATE_DISMISS_KEY = "rhoai-template-hint-dismissed";

/** The release whose "is available" notice was dismissed (localStorage: it stays dismissed). */
const UPDATE_DISMISS_KEY = "rhoai-update-dismissed";

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
  if (type === RECIPE_OPERATION_TYPE || type === "recipe-plan") return ["/recipes", "Open Recipes"];
  if (type.includes("minio") || type.includes("mlflow") || type.includes("pipeline") || type === "migrate-storage" || type === "cleanup") return ["/test-resources", "Open Environment"];
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
    case RECIPE_OPERATION_TYPE:
      return "Nothing resumes by itself. Resume it on the Recipes page: each step checks the cluster first, so what is set up stays and the run continues where it stopped.";
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

/** This tab's own project action, when it runs one (it holds at most one: one per user). */
interface OwnProjectAction {
  action: ProjectActionNotice;
  cancel: React.ReactNode;
  note: React.ReactNode;
}

function useOperationNotice(onStatusPage: boolean, pathname = "", own: OwnProjectAction | null = null): Notice | null {
  const { run, server } = useOperation();
  const { status } = useClusterStatus();
  const navigate = useNavigate();
  const ownStream = isRunning(run) && run.source === "stream" ? run : null;
  const op: ServerOperation | null = server.inProgress ? server.operation : null;
  const now = useNow(!!ownStream || !!op);
  // A run of this tab with a progress dialog (the Environment page), and the read-only view for the others.
  const background = useBackgroundRun();
  const viewServerOperation = useViewServerOperation();

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
  // This tab's run of the Environment page; when it is the user's project action, its Cancel is here (one banner).
  if (background) return { key: "operation", priority: 1, element: <BackgroundRunAlert run={background} projectAction={!!own} cancel={own?.cancel} note={own?.note} /> };
  if (!server.inProgress) return null;
  if (server.brief) {
    // A user who is not a cluster-admin: when a cluster operation started, never who runs it or what.
    const clock = formatClockTime(server.brief.startedAt);
    return {
      key: "operation",
      priority: 1,
      element: (
        <Alert
          variant="info"
          isInline
          component="p"
          customIcon={<InProgressIcon />}
          title={`A cluster operation is running${clock ? ` since ${clock}` : ""}. Project actions can start when it ends.`}
        />
      ),
    };
  }
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
  // A recipe run: the Recipes page shows its steps; elsewhere, its step and the page to follow it.
  const recipe = op.type === RECIPE_OPERATION_TYPE ? recipeTarget(op.target) ?? { recipe: "", step: "" } : null;
  if (recipe && pathname === "/recipes") return null;
  const step = serverOperationStep(op);
  const started = Date.parse(op.startedAt);
  const elapsed = Number.isFinite(started) ? ` (${formatElapsed(started, now)})` : "";
  // No link to the page the user is on (the Environment page shows its own progress).
  const followed = followLink(op.type);
  const link = followed && followed[0] !== pathname ? followed : null;
  // Streamed operator operations have their full progress on the Status page (the link); the others, what the server records.
  const view = !kind && !recipe && viewServerOperation ? <ViewProgressLink onOpen={() => viewServerOperation(op)} /> : null;
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
        actionLinks={link || view ? <>{view}{link && <AlertActionLink onClick={() => navigate(link[0])}>{link[1]}</AlertActionLink>}</> : undefined}
      >
        {op.target && !recipe && <>Target: <BuildId target={op.target} />. </>}
        {recipe?.recipe && <>Recipe {recipe.recipe}. </>}
        {op.remote && op.message && <>Latest progress: {sentence(op.message)} </>}
        {op.remote && "Another updater pod runs it (for example one that is shutting down finishes its operation first). "}
        Cluster changes from this tool are disabled until it finishes.
      </Alert>
    ),
  };
}

/** The banner title of a running project action, by who reads it (the non-admin design, section 6). */
export function projectActionTitle(a: ProjectActionNotice, admin: boolean, mine: boolean): string {
  const clock = formatClockTime(a.startedAt);
  if (mine) return `Your action (${a.label}) in project ${a.project} is running${clock ? ` since ${clock}` : ""}.`;
  if (admin) return `${a.label} in project ${a.project}, started by ${a.user || "someone"}${clock ? ` at ${clock}` : ""}.`;
  return `An action (${a.label}) is running in project ${a.project}${clock ? ` since ${clock}` : ""}.`;
}

/** What a cancel request did, by action ID: in flight, or why it failed. */
type CancelState = Record<string, { pending?: boolean; error?: string }>;

/**
 * The running project actions' notices and their Cancel: a cluster-admin
 * sees every one with who started it; a user the ones in their own
 * projects (the backend filters them), never who runs another person's.
 * Cancel asks after a confirmation that says what the action leaves; it is
 * a request the action takes before its next step. The tab's own action
 * (it runs the background run) shows in the run's banner instead (own).
 */
function useProjectActions(): { notices: Notice[]; own: OwnProjectAction | null; modal: React.ReactElement | null } {
  const { server, refreshServerOperation } = useOperation();
  const admin = useRole() === "admin";
  const me = useSignedInUser();
  const background = useBackgroundRun();
  const [asking, setAsking] = useState<ProjectActionNotice | null>(null);
  const [state, setState] = useState<CancelState>({});
  const setFor = (id: string, s: { pending?: boolean; error?: string }) => setState((all) => ({ ...all, [id]: s }));
  const send = (a: ProjectActionNotice) => {
    const id = a.id as string;
    setAsking(null);
    setFor(id, { pending: true });
    cancelOperation(id)
      .then(() => setFor(id, {}))
      .catch((e: unknown) => setFor(id, { error: toApiError(e).message }))
      .finally(() => refreshServerOperation());
  };
  const isMine = (a: ProjectActionNotice) => a.own || (admin && !!me && a.user === me);
  const cancelLink = (a: ProjectActionNotice) => {
    if (!a.id || !a.cancellable || a.cancelling) return undefined;
    return <AlertActionLink onClick={() => setAsking(a)} isDisabled={!!state[a.id]?.pending}>Cancel</AlertActionLink>;
  };
  const note = (a: ProjectActionNotice) => {
    const error = a.id ? state[a.id]?.error : undefined;
    return <>
      {a.cancelling && "Cancel requested: it stops before its next step. If it finishes first, it is recorded as done."}
      {error && sentence(error)}
    </>;
  };
  let own: OwnProjectAction | null = null;
  const notices: Notice[] = [];
  for (const a of server.projectActions ?? []) {
    const mine = isMine(a);
    if (mine && background && !own) {
      own = { action: a, cancel: cancelLink(a), note: note(a) };
      continue;
    }
    notices.push({
      key: `project-action-${a.id ?? `${a.project}-${a.startedAt}`}`,
      // Other people's actions rank below the permission and interrupted notices.
      priority: mine ? 1 : 8.5,
      element: (
        <Alert variant="info" isInline component="p" customIcon={<InProgressIcon />} title={projectActionTitle(a, admin, mine)} actionLinks={cancelLink(a)}>
          {note(a)}
        </Alert>
      ),
    });
  }
  const modal = asking && (
    <Modal variant={ModalVariant.small} isOpen aria-labelledby="cancel-project-action-title" onClose={() => setAsking(null)}>
      <ModalHeader title={`Cancel ${asking.label} in project ${asking.project}?`} labelId="cancel-project-action-title" />
      <ModalBody>
        <Content component="p">{asking.cancelNote || "It stops before its next step."}</Content>
        <Content component="p">If it finishes before its next step, it is recorded as done.</Content>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={() => send(asking)}>Cancel action</Button>
        <Button variant="link" onClick={() => setAsking(null)}>Keep it running</Button>
      </ModalFooter>
    </Modal>
  );
  return { notices, own, modal: modal || null };
}

function useInterruptedNotice(pathname: string): Notice | null {
  const { server } = useOperation();
  const navigate = useNavigate();
  const op = server.inProgress ? null : server.interrupted;
  const [dismissed, setDismissed] = useState(() => readSession(INTERRUPTED_DISMISS_KEY));
  if (!op || dismissed === op.startedAt) return null;
  // A recipe run: the Recipes page shows it with Resume.
  const recipe = op.type === RECIPE_OPERATION_TYPE ? recipeTarget(op.target) ?? { recipe: "", step: "" } : null;
  if (recipe && pathname === "/recipes") return null;
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
        actionLinks={recipe ? <AlertActionLink onClick={() => navigate("/recipes")}>Open Recipes to resume</AlertActionLink> : undefined}
      >
        {op.user || "Someone"} started it {formatRelativeTime(op.startedAt)}
        {op.target && !recipe && <> (target <BuildId target={op.target} />)</>}
        {recipe?.recipe && <> (recipe {recipe.recipe}{recipe.step ? `, at ${recipe.step}` : ""})</>}, and the updater pod
        {op.pod ? ` ${op.pod}` : ""} stopped before it finished. {interruptedGuidance(op)}
      </Alert>
    ),
  };
}

function overrideWhat(o: DashboardOverride): string {
  if (!o.sessionRecorded) return `${dashboardOperatorLabel(o.operatorName)} was scaled down outside this tool`;
  const what = o.mode === "pr" && o.prNumber ? `PR #${o.prNumber}` : o.mode === "main" ? "the main branch" : "a dashboard build";
  const flavor = o.flavor === "odh" ? " (ODH build)" : o.flavor === "rhoai" ? " (RHOAI build)" : "";
  const who = o.startedBy ? `${o.startedBy} deployed` : "Deployed";
  const when = o.startedAt ? ` ${formatRelativeTime(o.startedAt)}` : "";
  return `${who} ${what}${flavor}${when}`;
}

/**
 * A04-1: a paused dashboard operator is invisible unless every page says
 * so. The Dashboard Dev page's session panel says it all there, so the
 * notice is not repeated on that page.
 */
function useDashboardOverrideNotice(onDashboardDevPage: boolean): Notice | null {
  const { override } = useDashboardOverride();
  const navigate = useNavigate();
  if (!override?.active || onDashboardDevPage) return null;
  const variant = override.dashboardDeleting ? "danger" : override.stale ? "warning" : "info";
  const title = override.dashboardDeleting
    ? `A dashboard deletion is waiting for the paused ${dashboardOperatorLabel(override.operatorName)}`
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
        {overrideWhat(override)}; {dashboardOperatorLabel(override.operatorName)} stays paused until someone reverts it on the Dashboard Dev page.
        {override.stale && " The dashboard still runs the images from before the update."}
        {reasons.length > 0 && <> {reasons.map(sentence).join(" ")}</>}
        {" "}Update, Re-deploy and Reinstall offer to revert it first.
      </Alert>
    ),
  };
}

function useTemplateNotice(): Notice | null {
  const version = useVersion();
  const admin = useRole() === "admin";
  const [dismissed, setDismissed] = useState(() => readSession(TEMPLATE_DISMISS_KEY) === "1");
  // Only a cluster-admin can re-apply the deployment.
  if (!admin || !version?.templateOutdated || dismissed) return null;
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
        {version.installMethod === "gitops" ? (
          <>
            {" "}Argo CD installed it from the Helm chart: in the GitOps repository, set the chart&apos;s{" "}
            <code>targetRevision</code> to the release of the image in <code>parameters.IMAGE</code>. See{" "}
            <DocLink path="docs/GITOPS.md" anchor="6-upgrading-and-rolling-back">the GitOps guide: Upgrading and rolling back</DocLink>.
          </>
        ) : (
          <>
            {" "}Re-apply it from a checkout of the running release with <code>make upgrade</code>, or with that release&apos;s{" "}
            <code>install.sh</code>. See <DocLink path="docs/UPGRADING.md">the upgrade guide</DocLink>.
          </>
        )}
      </Alert>
    ),
  };
}

function readLocal(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}
function writeLocal(key: string, value: string): void {
  try { localStorage.setItem(key, value); } catch { /* localStorage may be disabled */ }
}

/** A newer updater release; dismissing hides it until an even newer one appears. */
function useUpdateNotice(): Notice | null {
  const update = useUpdateCheck();
  const gitops = useVersion()?.installMethod === "gitops";
  const admin = useRole() === "admin";
  const [dismissed, setDismissed] = useState(() => readLocal(UPDATE_DISMISS_KEY));
  // Only a cluster-admin can upgrade the updater.
  if (!admin || !update?.updateAvailable || !update.latest || dismissed === update.latest) return null;
  const latest = update.latest;
  const gitRoute = <code>git checkout {latest} &amp;&amp; make upgrade</code>;
  // The no-clone installer of exactly that release (scripts/install.sh, attached to every release).
  // The backend only sends URLs without shell syntax; quote it anyway. With
  // the release notes' SHA-256 the command checks the download before
  // anything runs: a different file stops it at sha256sum.
  const sum = update.installerSHA256 && /^[0-9a-f]{64}$/.test(update.installerSHA256) ? update.installerSHA256 : "";
  const check = sum ? ` && echo '${sum}  install.sh' | sha256sum -c -` : "";
  const installer = update.installerURL ? `curl -fsSLO '${update.installerURL}'${check} && bash install.sh` : "";
  return {
    key: "update",
    priority: 10,
    element: (
      <Alert
        variant="info"
        isInline
        component="p"
        title={`${latest} is available`}
        actionClose={<AlertActionCloseButton onClose={() => { writeLocal(UPDATE_DISMISS_KEY, latest); setDismissed(latest); }} />}
        actionLinks={update.releaseNotesURL
          ? <AlertActionLink component="a" href={update.releaseNotesURL} target="_blank" rel="noopener noreferrer">Release notes</AlertActionLink>
          : undefined}
      >
        This updater runs {update.current}.{" "}
        {update.majorUpgrade && <>{latest} is a new major version and requires a full redeploy. </>}
        {gitops ? (
          // Argo CD applies the chart: install.sh would be undone by the next sync.
          <>
            Argo CD installed this updater from the Helm chart, so an admin upgrades in Git: in one commit, set the
            chart&apos;s <code>targetRevision</code> to <code>{latest}</code> and <code>parameters.IMAGE</code> to the{" "}
            {latest} image by digest; Argo CD applies both{update.majorUpgrade ? ", including the new template" : ""}.
            Do not run <code>install.sh</code> on this install. See{" "}
            <DocLink path="docs/GITOPS.md" anchor="6-upgrading-and-rolling-back">the GitOps guide: Upgrading and rolling back</DocLink>.
          </>
        ) : installer ? (
          <>
            An admin {update.majorUpgrade ? "redeploys" : "upgrades"} with the {latest} installer (needs <code>oc</code>, logged in as cluster-admin):
            <ClipboardCopy
              className="pf-v6-u-my-sm"
              variant="inline-compact"
              isBlock
              isCode
              hoverTip="Copy command"
              clickTip="Copied"
              copyAriaLabel={`Copy the ${latest} install command`}
            >
              {installer}
            </ClipboardCopy>
            {sum
              ? <>The command checks install.sh against the SHA-256 in the {latest} release notes (on macOS without <code>sha256sum</code>, use <code>shasum -a 256 -c -</code>). </>
              : <>Before running it, check install.sh against the SHA-256 in the {latest} release notes. </>}
            Pass the options this install was made with (for example <code>--namespace</code>, <code>--app-name</code>,{" "}
            <code>--image-repo</code>); see {update.majorUpgrade
              ? <DocLink path="docs/UPGRADING.md">the upgrade guide</DocLink>
              : <DocLink path="docs/UPGRADING.md" anchor="patch-and-minor-releases">the upgrade guide: Patch and minor releases</DocLink>}.
            {/* A GitLab release asset of a non-public project redirects anonymous downloads to its sign-in page. */}
            {update.installerURL?.includes("/-/releases/") && <> If the download asks you to sign in, get <code>install.sh</code> from the release notes.</>}
            {" "}From a clone: {gitRoute}.
          </>
        ) : (
          <>
            An admin {update.majorUpgrade ? "redeploys" : "upgrades"} with that release&apos;s <code>install.sh</code>
            {update.releaseNotesURL ? " (in the release notes)" : ""}, or from a clone with {gitRoute}.
          </>
        )}
      </Alert>
    ),
  };
}

function usePermissionNotice(): Notice | null {
  const permissions = usePermissions();
  const sessionExpired = useSessionExpired();
  const role = useRole();
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
          {sentence(permissions.error?.message || "The OpenShift API did not answer")} The page stays read-only, and shows what every
          user sees, until the check succeeds; it is retried automatically.
        </Alert>
      ),
    };
  }
  // Read-only is what a user who is not a cluster-admin is: the navigation
  // and the pages already show their view, nothing to add.
  if (role !== "admin") return null;
  // The backend's reason: a cluster-admin whose sign-in cannot carry a change
  // (made before 4.0.0, or about to expire; pkg/api HandleUserPermissions).
  // Not the read-only notice, which is for users without the role.
  if (permissions.status === "denied" && permissions.reason && permissions.reason !== NO_PERMISSION_REASON) {
    const signInAgain = permissions.reasonCode === "sign_in_again";
    return {
      key: "permissions",
      priority: 2,
      element: (
        <Alert
          variant="warning"
          isInline
          component="p"
          title={signInAgain ? "Sign in again to make changes" : "Changes are disabled"}
          // The user menu's Log out: oauth-proxy's sign_out clears its session cookie (App).
          actionLinks={signInAgain ? <AlertActionLink component="a" href="/oauth/sign_out">Log out</AlertActionLink> : undefined}
        >
          {sentence(permissions.reason)} You can still view everything.
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
 * dashboard operator and the deployment template hint. At most
 * MAX_VISIBLE_NOTICES show at once, most important first; the rest are one
 * click away. An expired session replaces the whole page instead (App).
 */
export const GlobalBanners: React.FC = () => {
  const { pathname } = useLocation();
  const [showAll, setShowAll] = useState(false);
  const projectActions = useProjectActions();
  const notices = [
    useOperationNotice(pathname === "/", pathname, projectActions.own),
    ...projectActions.notices,
    usePermissionNotice(),
    useInterruptedNotice(pathname),
    useReconcileTimeoutNotice(),
    useDashboardOverrideNotice(pathname === "/dashboard-dev"),
    useTemplateNotice(),
    useUpdateNotice(),
  ].filter((n): n is Notice => n !== null).sort((a, b) => a.priority - b.priority);
  if (notices.length === 0) return projectActions.modal;
  const hidden = showAll ? 0 : Math.max(0, notices.length - MAX_VISIBLE_NOTICES);
  const visible = hidden > 0 ? notices.slice(0, MAX_VISIBLE_NOTICES) : notices;
  const extra = notices.length - MAX_VISIBLE_NOTICES;
  return (
    <div>
      <AlertGroup aria-label="Notices">
        {visible.map((n) => React.cloneElement(n.element, { key: n.key }))}
      </AlertGroup>
      {projectActions.modal}
      {extra > 0 && (
        // A plain link: AlertGroup's overflow button is styled for toast groups.
        <Button variant="link" isInline className="pf-v6-u-mt-sm" onClick={() => setShowAll(!showAll)} aria-expanded={showAll}>
          {hidden > 0 ? `View ${hidden} more ${hidden === 1 ? "notice" : "notices"}` : "Show fewer notices"}
        </Button>
      )}
    </div>
  );
};
