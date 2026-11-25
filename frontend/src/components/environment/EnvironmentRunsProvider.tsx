import React, { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Alert, AlertActionCloseButton, AlertActionLink, AlertGroup } from "@patternfly/react-core";
import { useNavigate } from "react-router-dom";
import type { LegacyStorageState, OperationResponse, ResourcesStatus, S3KeysState } from "../../types";
import { getResourcesStatus, isSessionExpired } from "../../services/api";
import { RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS } from "../../constants";
import { exponentialBackoff } from "../../hooks/usePolling";
import { useAppScreens, useClusterBusyHandler } from "../../state/AppInfo";
import { useRegisterBackgroundRun } from "../ProgressViews";
import { BUSY_REASON } from "./environmentTabs";
import { MigrateStorageModal } from "./MigrateStorageModal";
import { SecureStorageModal, secureExtraSteps } from "./SecureStorageModal";
import { RunModal, runOutcome } from "./SetupRun";
import { useRunState, type RunState } from "./runState";
import { runStepNow } from "./useGoalRun";
import { MIGRATE_TITLES, migrateOutcome, migrateStepNow, useMigrateStorage, type MigrateKind } from "./useMigrateStorage";

/** The Environment page's address (App.tsx routes). */
const ENVIRONMENT_PATH = "/test-resources";

/**
 * What the open Environment page lends its runs: its reads. A run calls
 * them through the provider, so a run started on one visit of the page
 * refreshes the page of the next visit, and nothing while the page is
 * closed (it reads everything when it opens again).
 */
export interface EnvironmentPageLink {
  fetchResources: () => Promise<void>;
  restartPolling: () => void;
  /** The status the page shows, and the project it reads it for (the Pipelines tab's). */
  statusRef: { current: ResourcesStatus | null };
  project: { current: string | null };
  onResult?: (res: OperationResponse) => void;
}

/** What the open page adds to the runs' dialogs: they show without it on other pages. */
export interface EnvironmentPageView {
  /** The run dialog's Open link after a successful run, and what to do next. */
  next: { label: string; href: string } | null;
  whatNext?: string;
  /** What Migrate storage and Secure S3 storage act on, and why changes are off. */
  legacy?: LegacyStorageState | null;
  keys?: S3KeysState | null;
  mutateBlocker: string | null;
}

export interface EnvironmentRuns {
  runs: RunState;
  migration: ReturnType<typeof useMigrateStorage>;
  /** Which Migrate storage dialog is open. */
  migrateKind: MigrateKind | null;
  setMigrateKind: (kind: MigrateKind | null) => void;
  /** The open page's reads (nothing while it is closed). */
  fetchResources: () => Promise<void>;
  restartPolling: () => void;
  onResult: (res: OperationResponse) => void;
  /** The latest status: the open page's, else the one the run read itself. */
  latestStatus: () => ResourcesStatus | null;
  /**
   * Reads the status for a Set up's next ready check while the page is
   * closed, at the page's own pace, within the time left; rejects when the
   * session ended.
   */
  refreshStatus: (remainingMs: number) => Promise<void>;
  /** A Clean up refused for confirmation: Clean up opens again on the page, or a notification offers it. */
  askCleanupConfirmation: () => void;
  /** Lends the page's reads while it is open; returns the release. */
  attach: (link: { current: EnvironmentPageLink }) => () => void;
  setView: React.Dispatch<React.SetStateAction<EnvironmentPageView | null>>;
}

const EnvironmentRunsContext = createContext<EnvironmentRuns | null>(null);

/**
 * The Environment page's runs (a Set up of several parts, a streamed
 * change, Clean up, Migrate storage, Secure S3 storage) and their dialogs,
 * kept above the routes as AppState keeps the operator's run: leaving the
 * page neither stops a run nor loses its step list, and View progress on
 * the operation alert opens the same dialog from any page. A run's streams
 * are aborted only when the app goes. The page adopts the run when it
 * opens again: one run at a time, one dialog.
 */
export const EnvironmentRunsProvider: React.FC<React.PropsWithChildren<{
  /** Each request's answer, for the app (a cluster_busy refusal reads the operation again); else the open page's. */
  onResult?: (res: OperationResponse) => void;
  /** A screen replaces every page (the session ended, no access): the dialogs and notifications wait behind it. */
  hidden?: boolean;
}>> = ({ children, onResult: appOnResult, hidden = false }) => {
  const runs = useRunState();
  const navigate = useNavigate();
  const page = useRef<{ current: EnvironmentPageLink } | null>(null);
  // While the page is closed: the last status it showed (then the run's own reads), its project, and when to read next.
  const away = useRef<{ status: ResourcesStatus | null; project: string | null; reads: number; nextReadAt: number }>({ status: null, project: null, reads: 0, nextReadAt: 0 });
  const appResult = useRef(appOnResult);
  useLayoutEffect(() => { appResult.current = appOnResult; });
  const fetchResources = useCallback(() => page.current?.current.fetchResources() ?? Promise.resolve(), []);
  const restartPolling = useCallback(() => page.current?.current.restartPolling(), []);
  const onResult = useCallback((res: OperationResponse) => (appResult.current ?? page.current?.current.onResult)?.(res), []);
  const latestStatus = useCallback(() => page.current?.current.statusRef.current ?? away.current.status, []);
  const refreshStatus = useCallback(async (remainingMs: number) => {
    const a = away.current;
    // The page's own polling reads while it is open; closed, as often as it would (quick after a change, backing off).
    if (page.current || Date.now() < a.nextReadAt) return;
    const ctrl = new AbortController();
    const deadline = setTimeout(() => ctrl.abort(), Math.max(0, remainingMs));
    try {
      a.status = await getResourcesStatus(a.project, ctrl.signal);
    } catch (e) {
      if (isSessionExpired(e)) throw e;
      // Otherwise the last status stays; the next check reads again.
    } finally {
      clearTimeout(deadline);
      a.nextReadAt = Date.now() + exponentialBackoff(a.reads, RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS);
      a.reads += 1;
    }
  }, []);
  const [view, setView] = useState<EnvironmentPageView | null>(null);
  const [migrateKind, setMigrateKind] = useState<MigrateKind | null>(null);
  const migration = useMigrateStorage(useCallback(() => { void fetchResources(); }, [fetchResources]));
  // Finished runs whose dialog was dismissed surface as toast with a path back—outcomes stay
  // visible without shifting page layout. confirm: a Clean up waits for confirmation on the page.
  const [toast, setToast] = useState<"run" | "migrate" | "confirm" | null>(null);

  const { run, runOpen, setRunOpen, cleanupScope, setCleanupScope, setSetupConfirm, setCleanupRetry, resAction } = runs;
  // What a closing page leaves open: its confirmations close with it (Back cancels them), a running Migrate dialog stays.
  const closing = useRef({ migrating: false, confirmAsked: false });
  closing.current = { migrating: !!migration.run, confirmAsked: toast === "confirm" };
  const attach = useCallback((link: { current: EnvironmentPageLink }) => {
    page.current = link;
    return () => {
      if (page.current !== link) return;
      away.current = { status: link.current.statusRef.current ?? away.current.status, project: link.current.project.current, reads: 0, nextReadAt: Date.now() + RESOURCE_POLL_BASE_MS };
      page.current = null;
      setSetupConfirm(null);
      setCleanupScope(null);
      if (!closing.current.confirmAsked) setCleanupRetry(null);
      if (!closing.current.migrating) setMigrateKind(null);
    };
  }, [setSetupConfirm, setCleanupScope, setCleanupRetry]);
  const askCleanupConfirmation = useCallback(() => {
    if (page.current) setCleanupScope("all");
    else setToast("confirm");
  }, [setCleanupScope]);
  // Migrate storage and Secure S3 storage share one runner; each has its own dialog.
  const migrating = !!migration.run?.running && migration.run.kind !== "secure";
  const securing = !!migration.run?.running && migration.run.kind === "secure";
  const mutateBlocker = view?.mutateBlocker ?? null;
  const keys = view?.keys;
  // A Migrate storage dialog shows its plan only on the page; elsewhere only its run.
  const migrateShown = view || migration.run ? migrateKind : null;

  // A new run replaces the last one's notification.
  const wasRunning = useRef(false);
  useEffect(() => {
    const now = !!run.run?.running;
    if (now && !wasRunning.current) setToast(null);
    // When cleanup aborts before confirmation, reopen the Clean up flow (it then needs no notification).
    if (wasRunning.current && !now && !runOpen && !cleanupScope) setToast((t) => t ?? "run");
    wasRunning.current = now;
  }, [run.run?.running, runOpen, cleanupScope]);
  const wasMigrating = useRef(false);
  useEffect(() => {
    const now = !!migration.run?.running;
    if (now && !wasMigrating.current) setToast(null);
    if (wasMigrating.current && !now && !migrateKind) setToast("migrate");
    wasMigrating.current = now;
  }, [migration.run?.running, migrateKind]);

  // Background runs (dialog closed) still appear in the global operation strip with step text
  // and View progress to restore the run dialog.
  const reopenRun = useCallback(() => { setToast(null); setRunOpen(true); }, [setRunOpen]);
  const migrationKind = migration.run?.running ? migration.run.kind : null;
  const reopenMigration = useCallback(() => { if (migrationKind) setMigrateKind(migrationKind); }, [migrationKind]);
  const goal = run.run?.running ? run.run : null;
  useRegisterBackgroundRun(goal
    ? { title: goal.title, startedAt: goal.startedAt, step: runStepNow(goal), open: reopenRun }
    : migration.run?.running ? { title: MIGRATE_TITLES[migration.run.kind], startedAt: migration.run.startedAt, step: migrateStepNow(migration.run), open: reopenMigration } : null);

  const outcome = run.run ? runOutcome(run.run) : null;
  const migrated = migration.run ? migrateOutcome(migration.run) : null;
  const value = useMemo<EnvironmentRuns>(() => ({
    runs, migration, migrateKind, setMigrateKind, fetchResources, restartPolling, onResult, latestStatus, refreshStatus, askCleanupConfirmation, attach, setView,
  }), [runs, migration, migrateKind, fetchResources, restartPolling, onResult, latestStatus, refreshStatus, askCleanupConfirmation, attach]);

  return (
    <EnvironmentRunsContext.Provider value={value}>
      {children}

      {!hidden && <>
      {migrateShown === "secure" && (
        <SecureStorageModal
          isOpen
          keys={keys}
          run={migration.run}
          baseReason={securing ? null : mutateBlocker ?? (resAction || migrating || run.run?.running ? BUSY_REASON : null)}
          onStart={() => migration.start("secure", { removeLeftoverKeys: (keys?.leftovers ?? []).map((l) => l.user) }, secureExtraSteps(keys))}
          onClose={() => {
            setMigrateKind(null);
            migration.reset();
          }}
        />
      )}

      {migrateShown && migrateShown !== "secure" && (
        <MigrateStorageModal
          isOpen
          kind={migrateShown}
          legacy={view?.legacy}
          run={migration.run}
          baseReason={migrating ? null : mutateBlocker ?? (resAction || run.run?.running ? BUSY_REASON : null)}
          onStart={(kind, deleteNamespace, confirmProjects) => migration.start(kind, kind === "remove" ? { removeOldStorage: true, deleteNamespace } : confirmProjects.length > 0 ? { confirmProjects } : {})}
          onClose={() => {
            setMigrateKind(null);
            migration.reset();
          }}
        />
      )}

      <RunModal run={run.run} isOpen={runOpen} open={view?.next ? { kind: "open", ...view.next } : null} whatNext={view?.whatNext}
        onClose={() => { setRunOpen(false); if (!run.run?.running) run.reset(); }} />

      <AlertGroup isToast isLiveRegion>
        {toast === "run" && run.run && outcome && (
          <Alert variant={outcome.variant} title={outcome.variant === "danger" ? outcome.title : `${run.run.title}: ${outcome.title}`}
            timeout={outcome.variant === "success" ? 8000 : false} onTimeout={() => setToast(null)}
            actionClose={<AlertActionCloseButton onClose={() => setToast(null)} />}
            actionLinks={<AlertActionLink onClick={() => { setToast(null); setRunOpen(true); }}>Show details</AlertActionLink>} />
        )}
        {toast === "migrate" && migration.run && migrated && (
          <Alert variant={migrated.variant} title={`${MIGRATE_TITLES[migration.run.kind]}: ${migrated.title}`}
            timeout={migrated.variant === "success" ? 8000 : false} onTimeout={() => setToast(null)}
            actionClose={<AlertActionCloseButton onClose={() => setToast(null)} />}
            actionLinks={<AlertActionLink onClick={() => { setToast(null); if (migration.run) setMigrateKind(migration.run.kind); }}>Show details</AlertActionLink>} />
        )}
        {toast === "confirm" && run.run && (
          <Alert variant="warning" title={`${run.run.title} needs your confirmation`}
            actionClose={<AlertActionCloseButton onClose={() => setToast(null)} />}
            actionLinks={<AlertActionLink onClick={() => { setToast(null); setCleanupScope("all"); void navigate(ENVIRONMENT_PATH); }}>Open Clean up</AlertActionLink>}>
            {run.run.result?.message}
          </Alert>
        )}
      </AlertGroup>
      </>}
    </EnvironmentRunsContext.Provider>
  );
};

/**
 * The app's provider: requests answered cluster_busy read the operation
 * again (useClusterBusyHandler), and the dialogs and notifications wait
 * behind the screens that replace every page (AppLayout).
 */
export const AppEnvironmentRunsProvider: React.FC<React.PropsWithChildren> = ({ children }) => {
  const onClusterBusy = useClusterBusyHandler();
  const screens = useAppScreens();
  return <EnvironmentRunsProvider onResult={onClusterBusy} hidden={screens.any}>{children}</EnvironmentRunsProvider>;
};

/** Whether the Environment page's runs are kept above it (the app's provider). */
export function useHasEnvironmentRuns(): boolean {
  return useContext(EnvironmentRunsContext) !== null;
}

/** The Environment page's runs; the page renders inside EnvironmentRunsProvider. */
export function useEnvironmentRuns(): EnvironmentRuns {
  const runs = useContext(EnvironmentRunsContext);
  if (!runs) throw new Error("useEnvironmentRuns needs an EnvironmentRunsProvider");
  return runs;
}

/**
 * Lends the open page's reads to the runs, and what it adds to their
 * dialogs, until it closes.
 */
export function useLinkEnvironmentPage(link: EnvironmentPageLink, view: EnvironmentPageView): void {
  const { attach, setView } = useEnvironmentRuns();
  const current = useRef(link);
  useLayoutEffect(() => { current.current = link; });
  useLayoutEffect(() => attach(current), [attach]);
  const { next, whatNext, legacy, keys, mutateBlocker } = view;
  const label = next?.label;
  const href = next?.href;
  useEffect(() => {
    // Unchanged (most polls): no new render of the provider.
    setView((v) => (v && v.next?.label === label && v.next?.href === href && v.whatNext === whatNext && v.legacy === legacy && v.keys === keys && v.mutateBlocker === mutateBlocker
      ? v
      : { next: label !== undefined && href !== undefined ? { label, href } : null, whatNext, legacy, keys, mutateBlocker }));
  }, [setView, label, href, whatNext, legacy, keys, mutateBlocker]);
  useEffect(() => () => setView(null), [setView]);
}
