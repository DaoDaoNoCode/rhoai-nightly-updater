import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { Alert, Content, Stack, StackItem } from "@patternfly/react-core";
import type { MLflowSamplesRequest } from "../../services/api";
import type { PlaygroundRequest, ProjectCapabilities, UpdateStep, WorkbenchRequest } from "../../types";
import { getProjectCapabilities, streamMLflowSamples, streamPlayground, streamWorkbench, toApiError, type ApiError } from "../../services/api";
import { isNotForUser } from "../../errors";
import { StepProgress, type StepState } from "./StepProgress";
import { TooltipButton } from "../TooltipButton";

/*
 * Project actions for a user who is not a cluster-admin (the non-admin
 * design, B3): in one of their own projects they may set up and run the
 * workbench, the playground and the MLflow samples when their RBAC allows
 * it. The backend answers per action (GET /api/resources/projects/
 * capabilities); a control the user may not use is disabled with the
 * reason, and the action's own gate checks again.
 */

interface CapabilitiesValue {
  project: string;
  caps: ProjectCapabilities | null;
  error: ApiError | null;
  reload: () => void;
}

const CapabilitiesContext = createContext<CapabilitiesValue | null>(null);

/** Reads what the user may do in project, again when refreshKey changes or after a run. */
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
  /** Why the control is disabled; null when it may be used. */
  reason: string | null;
}

/**
 * Whether the user may run action ("workbench.setup", ...) in project, with
 * the reason a disabled control shows. Fails closed: while the answer is
 * loading, when it failed, or for an action it does not name, the action
 * is not allowed.
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
  if (!cap.allowed) return { allowed: false, reason: cap.reason || CAPABILITY_UNKNOWN };
  return { allowed: true, reason: null };
}

/** Reloads the capabilities (after a run changed the project). */
export function useReloadCapabilities(): () => void {
  const value = useContext(CapabilitiesContext);
  return value?.reload ?? (() => undefined);
}

export type ProjectRunRequest =
  | { kind: "workbench"; req: WorkbenchRequest }
  | { kind: "playground"; req: PlaygroundRequest }
  | { kind: "mlflow-samples"; req: MLflowSamplesRequest };

interface RunState {
  title: string;
  running: boolean;
  order: string[];
  states: Record<string, StepState>;
  outcome: { success: boolean; message: string } | null;
}

/**
 * One project action at a time from this view: start(run) streams its
 * steps; onEnd runs when it ended (the parts read again).
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
    }
  }, [onEnd]);
  return { run, start, running: !!run?.running, dismiss: () => setRun(null) };
}

/** The steps and the outcome of the view's latest run. */
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

/** A project action's button: disabled with the reason when the user may not run it, or while another run of this view runs. */
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
  return <TooltipButton variant={variant} size="sm" disabledReason={disabledReason} onClick={onClick}>{children}</TooltipButton>;
};
