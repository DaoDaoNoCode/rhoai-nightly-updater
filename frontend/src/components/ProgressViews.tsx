import React, { createContext, useCallback, useContext, useEffect, useId, useMemo, useState } from "react";
import { Alert, AlertActionLink } from "@patternfly/react-core";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
import type { ServerOperation } from "../types";
import { formatElapsed } from "../utils";
import { ServerOperationModal } from "./ServerOperationModal";
import { withIds } from "./environment/s3KeysText";

/**
 * A run this tab started whose progress dialog can be closed while it goes
 * on (the Environment page's runs, Migrate storage). While it runs, the
 * app's operation alert names it and its step, with View progress to open
 * the dialog again: the run and its step list are kept above the routes
 * (EnvironmentRunsProvider), so nothing is lost by closing the dialog or
 * leaving the page.
 */
export interface BackgroundRun {
  /** The run's name, its dialog's title ("Set up Pipelines in dev-project"). */
  title: string;
  /** When it started (epoch ms). */
  startedAt: number;
  /** The step it is on, or null before its first step starts. */
  step: { title: string; index: number; count: number } | null;
  /** Opens its progress dialog again. Keep its identity across renders (useCallback). */
  open: () => void;
  /** The operation it follows (GET /api/operation's ID), once its stream named it. */
  operationId?: string;
}

interface Registry {
  set: (owner: string, run: BackgroundRun | null) => void;
  /** Opens the read-only view of an operation this tab has no dialog for. */
  viewServerOperation: (op: ServerOperation) => void;
}

const RegistryContext = createContext<Registry | null>(null);
const BackgroundRunContext = createContext<BackgroundRun | null>(null);

/**
 * Keeps which run of this tab has a progress dialog to reopen, and hosts the
 * read-only view of an operation started elsewhere. Without it (tests of a
 * single page) the hooks below do nothing.
 */
export const ProgressViewsProvider: React.FC<React.PropsWithChildren> = ({ children }) => {
  const [runs, setRuns] = useState<Record<string, BackgroundRun>>({});
  const [viewing, setViewing] = useState<ServerOperation | null>(null);
  const set = useCallback((owner: string, run: BackgroundRun | null) => {
    setRuns((all) => {
      if (!run) {
        if (!(owner in all)) return all;
        const rest = { ...all };
        delete rest[owner];
        return rest;
      }
      return { ...all, [owner]: run };
    });
  }, []);
  const registry = useMemo<Registry>(() => ({ set, viewServerOperation: setViewing }), [set]);
  // One operation runs at a time, so at most one run is registered.
  const background = Object.values(runs)[0] ?? null;
  return (
    <RegistryContext.Provider value={registry}>
      <BackgroundRunContext.Provider value={background}>
        {children}
        {viewing && <ServerOperationModal operation={viewing} onClose={() => setViewing(null)} />}
      </BackgroundRunContext.Provider>
    </RegistryContext.Provider>
  );
};

/** The run of this tab whose progress dialog can be opened again, if one runs. */
export function useBackgroundRun(): BackgroundRun | null {
  return useContext(BackgroundRunContext);
}

/** Opens the read-only view of an operation started elsewhere; null without the provider. */
export function useViewServerOperation(): ((op: ServerOperation) => void) | null {
  return useContext(RegistryContext)?.viewServerOperation ?? null;
}

/** Offers `run`'s dialog to the operation alert while it runs (null: nothing runs). */
export function useRegisterBackgroundRun(run: BackgroundRun | null): void {
  const registry = useContext(RegistryContext);
  const owner = useId();
  const title = run?.title;
  const startedAt = run?.startedAt;
  const stepTitle = run?.step?.title;
  const stepIndex = run?.step?.index;
  const stepCount = run?.step?.count;
  const open = run?.open;
  const operationId = run?.operationId;
  useEffect(() => {
    if (!registry) return;
    registry.set(owner, title !== undefined && startedAt !== undefined && open
      ? { title, startedAt, open, operationId, step: stepTitle !== undefined && stepIndex !== undefined && stepCount !== undefined ? { title: stepTitle, index: stepIndex, count: stepCount } : null }
      : null);
  }, [registry, owner, title, startedAt, stepTitle, stepIndex, stepCount, open, operationId]);
  useEffect(() => () => registry?.set(owner, null), [registry, owner]);
}

/**
 * An alert's View progress action. It takes the focus before the dialog
 * opens, so PatternFly's modal returns the focus to it when it closes (a
 * click does not focus a button in every browser).
 */
export const ViewProgressLink: React.FC<{ onOpen: () => void }> = ({ onOpen }) => (
  <AlertActionLink onClick={(e) => { e.currentTarget.focus(); onOpen(); }}>View progress</AlertActionLink>
);

/** Ticks once a second, so the elapsed time stays current. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  return now;
}

/** A step title after a comma: its first word lower-cased, unless it is a name ("MLflow", "S3"). */
const midSentence = (title: string) => (/^[A-Z][a-z]/.test(title) ? title.charAt(0).toLowerCase() + title.slice(1) : title);

/** The operation alert while a run of this tab goes on with its dialog closed. */
/**
 * The alert of this tab's background run. projectAction: the run is a
 * project action (its server entry), which holds its project's lock, not
 * the cluster's; cancel is its Cancel action, when it can be cancelled.
 */
export const BackgroundRunAlert: React.FC<{ run: BackgroundRun; projectAction?: boolean; cancel?: React.ReactNode; note?: React.ReactNode }> = ({ run, projectAction, cancel, note }) => {
  const now = useNow();
  // A one-step run's step is the run itself: its name says it.
  const step: React.ReactNode = !run.step ? ": starting" : run.step.count > 1 ? <>: step {run.step.index} of {run.step.count}, {withIds(midSentence(run.step.title))}</> : "";
  return (
    <Alert
      variant="info"
      isInline
      component="p"
      customIcon={<InProgressIcon />}
      title={<>{run.title} is running{step} ({formatElapsed(run.startedAt, now)})</>}
      actionLinks={<><ViewProgressLink onOpen={run.open} />{cancel}</>}
    >
      {projectAction
        ? "Closing its dialog does not stop it. Your next project action can start when it ends."
        : "Closing its dialog does not stop it. Cluster changes from this tool are disabled until it finishes."}
      {note && <> {note}</>}
    </Alert>
  );
};
