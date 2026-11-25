import React, { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Alert, AlertActionCloseButton, AlertActionLink, AlertGroup } from "@patternfly/react-core";
import type { LegacyStorageState, OperationResponse, ResourcesStatus, S3KeysState } from "../../types";
import { getResourcesStatus } from "../../services/api";
import { useRegisterBackgroundRun } from "../ProgressViews";
import { BUSY_REASON } from "./environmentTabs";
import { MigrateStorageModal } from "./MigrateStorageModal";
import { SecureStorageModal, secureExtraSteps } from "./SecureStorageModal";
import { RunModal, runOutcome } from "./SetupRun";
import { useRunState, type RunState } from "./runState";
import { runStepNow } from "./useGoalRun";
import { MIGRATE_TITLES, migrateStepNow, useMigrateStorage, type MigrateKind } from "./useMigrateStorage";

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
  /** Reads the status for a Set up's next ready check while the page is closed. */
  refreshStatus: () => Promise<void>;
  /** Lends the page's reads while it is open; returns the release. */
  attach: (link: { current: EnvironmentPageLink }) => () => void;
  setView: (view: EnvironmentPageView | null) => void;
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
export const EnvironmentRunsProvider: React.FC<React.PropsWithChildren> = ({ children }) => {
  const runs = useRunState();
  const page = useRef<{ current: EnvironmentPageLink } | null>(null);
  // While the page is closed: the last status it showed (then the run's own reads) and its project.
  const away = useRef<{ status: ResourcesStatus | null; project: string | null }>({ status: null, project: null });
  const fetchResources = useCallback(() => page.current?.current.fetchResources() ?? Promise.resolve(), []);
  const restartPolling = useCallback(() => page.current?.current.restartPolling(), []);
  const onResult = useCallback((res: OperationResponse) => page.current?.current.onResult?.(res), []);
  const latestStatus = useCallback(() => page.current?.current.statusRef.current ?? away.current.status, []);
  const refreshStatus = useCallback(async () => {
    if (page.current) return;
    try {
      away.current.status = await getResourcesStatus(away.current.project);
    } catch {
      // The last status stays; the next check reads again.
    }
  }, []);
  const attach = useCallback((link: { current: EnvironmentPageLink }) => {
    page.current = link;
    return () => {
      if (page.current !== link) return;
      away.current = { status: link.current.statusRef.current ?? away.current.status, project: link.current.project.current };
      page.current = null;
    };
  }, []);
  const [view, setView] = useState<EnvironmentPageView | null>(null);
  const [migrateKind, setMigrateKind] = useState<MigrateKind | null>(null);
  const migration = useMigrateStorage(useCallback(() => { void fetchResources(); }, [fetchResources]));

  const { run, runOpen, setRunOpen, cleanupRetry, resAction } = runs;
  // Migrate storage and Secure S3 storage share one runner; each has its own dialog.
  const migrating = !!migration.run?.running && migration.run.kind !== "secure";
  const securing = !!migration.run?.running && migration.run.kind === "secure";
  const mutateBlocker = view?.mutateBlocker ?? null;
  const keys = view?.keys;
  // A Migrate storage dialog shows its plan only on the page; elsewhere only its run.
  const migrateShown = view || migration.run ? migrateKind : null;

  // Finished runs whose dialog was dismissed surface as toast with a path back—outcomes stay
  // visible without shifting page layout.
  const [toast, setToast] = useState(false);
  const wasRunning = useRef(false);
  useEffect(() => {
    const now = !!run.run?.running;
    // When cleanup aborts before confirmation, reopen the Clean up flow.
    if (wasRunning.current && !now && !runOpen && !cleanupRetry) setToast(true);
    wasRunning.current = now;
  }, [run.run?.running, runOpen, cleanupRetry]);

  // Background runs (dialog closed) still appear in the global operation strip with step text
  // and View progress to restore the run dialog.
  const reopenRun = useCallback(() => { setToast(false); setRunOpen(true); }, [setRunOpen]);
  const migrationKind = migration.run?.running ? migration.run.kind : null;
  const reopenMigration = useCallback(() => { if (migrationKind) setMigrateKind(migrationKind); }, [migrationKind]);
  const goal = run.run?.running ? run.run : null;
  useRegisterBackgroundRun(goal
    ? { title: goal.title, startedAt: goal.startedAt, step: runStepNow(goal), open: reopenRun }
    : migration.run?.running ? { title: MIGRATE_TITLES[migration.run.kind], startedAt: migration.run.startedAt, step: migrateStepNow(migration.run), open: reopenMigration } : null);

  const outcome = run.run ? runOutcome(run.run) : null;
  const value = useMemo<EnvironmentRuns>(() => ({
    runs, migration, migrateKind, setMigrateKind, fetchResources, restartPolling, onResult, latestStatus, refreshStatus, attach, setView,
  }), [runs, migration, migrateKind, fetchResources, restartPolling, onResult, latestStatus, refreshStatus, attach]);

  return (
    <EnvironmentRunsContext.Provider value={value}>
      {children}

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
        {toast && run.run && outcome && (
          <Alert variant={outcome.variant} title={outcome.variant === "danger" ? outcome.title : `${run.run.title}: ${outcome.title}`}
            timeout={outcome.variant === "success" ? 8000 : false} onTimeout={() => setToast(false)}
            actionClose={<AlertActionCloseButton onClose={() => setToast(false)} />}
            actionLinks={<AlertActionLink onClick={() => { setToast(false); setRunOpen(true); }}>Show details</AlertActionLink>} />
        )}
      </AlertGroup>
    </EnvironmentRunsContext.Provider>
  );
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
    setView({ next: label !== undefined && href !== undefined ? { label, href } : null, whatNext, legacy, keys, mutateBlocker });
  }, [setView, label, href, whatNext, legacy, keys, mutateBlocker]);
  useEffect(() => () => setView(null), [setView]);
}
