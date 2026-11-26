import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { Alert, Content, Stack, StackItem } from "@patternfly/react-core";
import type { MLflowSamplesRequest } from "../../services/api";
import type { PlaygroundRequest, ProjectCapabilities, ProjectModelRequest, ProjectPipelineServerRequest, ProjectStorageRequest, UpdateStep, WorkbenchRequest } from "../../types";
import { getProjectCapabilities, streamMLflowSamples, streamPlayground, streamProjectModel, streamProjectPipelineServer, streamProjectStorage, streamWorkbench, toApiError, type ApiError } from "../../services/api";
import { isNotForUser } from "../../errors";
import { StepProgress, type StepState } from "./StepProgress";
import { TooltipButton } from "../TooltipButton";

/*
 * Actions scoped to a project for users who lack cluster-admin (non-admin
 * layout, B3): within a project they own they can configure and launch the
 * workbench, playground, and MLflow samples when RBAC permits. The backend
 * reports allowance per action (GET /api/resources/projects/capabilities);
 * controls the user cannot use stay disabled and show why, and each action
 * re-validates on its own.
 */

interface CapabilitiesValue {
  project: string;
  caps: ProjectCapabilities | null;
  error: ApiError | null;
  reload: () => void;
}

const CapabilitiesContext = createContext<CapabilitiesValue | null>(null);

/** Fetches permitted actions for the project whenever refreshKey moves or a run finishes. */
export const ProjectCapabilitiesProvider: React.FC<React.PropsWithChildren<{ project: string; refreshKey: number }>> = ({ project, refreshKey, children }) => {
  const [state, setState] = useState<{ project: string; caps: ProjectCapabilities | null; error: ApiError | null }>({ project: "", caps: null, error: null });
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    const ctl = new AbortController();
    getProjectCapabilities(project, ctl.signal).then(
      (caps) => setState({ project, caps, error: null }),
      (e: unknown) => { if (!ctl.signal.aborted) setState({ project, caps: null, error: toApiError(e, "Could not check what you may do in this project") }); },
    );
    return () => ctl.abort();
  }, [project, refreshKey, attempt]);
  const reload = useCallback(() => setAttempt((n) => n + 1), []);
  const mine = state.project === project;
  return (
    <CapabilitiesContext.Provider value={{ project, caps: mine ? state.caps : null, error: mine ? state.error : null, reload }}>
      {children}
    </CapabilitiesContext.Provider>
  );
};

export const CAPABILITY_CHECKING = "Checking what you may do in this project.";
export const CAPABILITY_UNKNOWN = "What you may do in this project could not be checked, so changes are disabled. Refresh the page to try again.";

export interface ProjectCapabilityAnswer {
  allowed: boolean;
  /** Shown on a disabled control; null if the control is usable. */
  reason: string | null;
  /** The refusal's code from the capabilities ("missing_permissions", ...), when the backend gave one. */
  code?: string;
}

/**
 * Decides if action ("workbench.setup", ...) may run in project and supplies
 * the message for a disabled control. Denies by default: no permission while
 * the query is pending, after an error, or for an action absent from the
 * capability list.
 */
export function useProjectCapability(project: string, action: string): ProjectCapabilityAnswer {
  const value = useContext(CapabilitiesContext);
  if (!value || value.project !== project) return { allowed: false, reason: CAPABILITY_UNKNOWN };
  if (value.error) {
    return { allowed: false, reason: isNotForUser(value.error) ? value.error.message : CAPABILITY_UNKNOWN };
  }
  if (!value.caps) return { allowed: false, reason: CAPABILITY_CHECKING };
  const cap = value.caps.actions[action];
  if (!cap) return { allowed: false, reason: CAPABILITY_UNKNOWN };
  if (!cap.allowed) return { allowed: false, reason: cap.reason || CAPABILITY_UNKNOWN, code: cap.code };
  return { allowed: true, reason: null };
}

/** Triggers a fresh capabilities fetch (e.g. once a run has altered the project). */
export function useReloadCapabilities(): () => void {
  const value = useContext(CapabilitiesContext);
  return value?.reload ?? (() => undefined);
}

export type ProjectRunRequest =
  | { kind: "workbench"; req: WorkbenchRequest }
  | { kind: "playground"; req: PlaygroundRequest }
  | { kind: "mlflow-samples"; req: MLflowSamplesRequest }
  | { kind: "project-s3"; req: ProjectStorageRequest }
  | { kind: "project-pipeline-server"; req: ProjectPipelineServerRequest }
  | { kind: "models"; req: ProjectModelRequest };

interface RunState {
  title: string;
  running: boolean;
  order: string[];
  states: Record<string, StepState>;
  outcome: { success: boolean; message: string } | null;
}

/**
 * Serializes project actions in this view: start(run) drives step streaming;
 * onEnd fires after completion (related data is re-fetched).
 */
export function useProjectRun(onEnd: () => void) {
  const [run, setRun] = useState<RunState | null>(null);
  const ctl = useRef<AbortController | null>(null);
  useEffect(() => () => ctl.current?.abort(), []);
  const start = useCallback((title: string, r: ProjectRunRequest) => {
    setRun({ title, running: true, order: [], states: {}, outcome: null });
    const onStep = (s: UpdateStep) => {
      if (s.step === "operation_complete") return;
      setRun((prev) => prev && {
        ...prev,
        order: prev.order.includes(s.step) ? prev.order : [...prev.order, s.step],
        states: { ...prev.states, [s.step]: { status: s.status, message: s.message } },
      });
    };
    const onDone = (success: boolean, error?: string, apiError?: ApiError) => {
      setRun((prev) => prev && { ...prev, running: false, outcome: { success, message: apiError?.message || error || (success ? "Done." : "The action failed.") } });
      onEnd();
    };
    switch (r.kind) {
      case "workbench": ctl.current = streamWorkbench(r.req, onStep, onDone); break;
      case "playground": ctl.current = streamPlayground(r.req, onStep, onDone); break;
      case "mlflow-samples": ctl.current = streamMLflowSamples(r.req, onStep, onDone); break;
      case "project-s3": ctl.current = streamProjectStorage(r.req, onStep, onDone); break;
      case "project-pipeline-server": ctl.current = streamProjectPipelineServer(r.req, onStep, onDone); break;
      case "models": ctl.current = streamProjectModel(r.req, onStep, onDone); break;
    }
  }, [onEnd]);
  return { run, start, running: !!run?.running, dismiss: () => setRun(null) };
}

/** Renders step progress and final outcome for the most recent run in the view. */
export const ProjectRunPanel: React.FC<{ run: RunState | null; idPrefix: string }> = ({ run, idPrefix }) => {
  if (!run) return null;
  return (
    <Stack hasGutter>
      <StackItem><Content component="p"><strong>{run.title}</strong></Content></StackItem>
      {run.order.length > 0 && (
        <StackItem>
          <StepProgress idPrefix={idPrefix} aria-label={run.title} running={run.running} shownElsewhere={run.outcome?.message}
            steps={run.order.map((id) => ({ id, title: id.charAt(0).toUpperCase() + id.slice(1) }))} states={run.states} />
        </StackItem>
      )}
      {run.outcome && (
        <StackItem>
          <Alert isInline variant={run.outcome.success ? "success" : "danger"} title={run.outcome.success ? "Done" : "Not done"} component="div">
            <Content component="p">{run.outcome.message}</Content>
          </Alert>
        </StackItem>
      )}
    </Stack>
  );
};

/** Project action trigger: blocked with an explanation when RBAC forbids it or another run from this view is active. */
export const ProjectActionButton: React.FC<{
  project: string;
  action: string;
  busy: boolean;
  variant?: "primary" | "secondary" | "danger" | "link";
  onClick: () => void;
  children: React.ReactNode;
}> = ({ project, action, busy, variant = "secondary", onClick, children }) => {
  const { reason } = useProjectCapability(project, action);
  const disabledReason = reason ?? (busy ? "Another action of this page is running in this project." : null);
  // Link variant blends with text links (e.g. Open in MLflow): inline, without extra padding or muted disabled chrome.
  return <TooltipButton variant={variant} isInline={variant === "link"} size="sm" disabledReason={disabledReason} onClick={onClick}>{children}</TooltipButton>;
};