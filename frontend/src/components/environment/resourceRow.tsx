import React from "react";
import { Alert, Button, Stack, StackItem } from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { ResourceState } from "../../types";
import { withImageRefs } from "../ImageRef";
import { TruncatedText } from "../LongText";

/**
 * Container waiting reasons that do not resolve on their own (Kubernetes
 * docs, "Debug Pods": ImagePullBackOff/ErrImagePull, CrashLoopBackOff, and
 * configuration errors). Polling stops for these instead of spinning forever.
 */
const TERMINAL_REASONS = [
  "ImagePullBackOff",
  "ErrImagePull",
  "InvalidImageName",
  "CrashLoopBackOff",
  "CreateContainerConfigError",
  "CreateContainerError",
  "FailingToDeploy",
];

/** Why a deployed resource can't become ready without a change, or null. */
export function terminalReason(state: ResourceState | undefined): string | null {
  if (!state || !state.deployed || state.ready) return null;
  if (state.terminalError) return state.waitingReason || "Failed";
  const candidates = [state.waitingReason, state.message].filter((v): v is string => !!v);
  for (const text of candidates) {
    const reason = TERMINAL_REASONS.find((r) => text.includes(r));
    if (reason) return state.message && state.message !== reason ? state.message : reason;
  }
  return null;
}

export function isTerminating(state: ResourceState | undefined): boolean {
  return !!state && (!!state.terminating || state.message === "Terminating");
}

export type ResourceKind = "minio" | "mlflow" | "pipeline";

/** What to do about a resource that cannot start (A08-3, A06-5). */
export function nextStep(kind: ResourceKind, state: ResourceState, minioReady = true): string | null {
  const managed = state.managedByTool !== false;
  if (kind === "minio" && managed && state.repairNeeded) {
    return "Repair re-runs setup: it applies the SeaweedFS Deployment with the image and settings of this version (scaling it back up if needed), waits for it, and points Service seaweedfs-s3 and Route seaweedfs-admin at it. Stored data is kept.";
  }
  if (!terminalReason(state)) return null;
  const text = `${state.waitingReason ?? ""} ${state.message ?? ""}`;
  if (/ImagePullBackOff|ErrImagePull|InvalidImageName/.test(text)) {
    if (kind === "minio" && managed) return "The image cannot be pulled. Repair applies the SeaweedFS Deployment again with the image this updater is configured to use; the stored data is kept.";
    if (kind === "mlflow" && state.prOverride) return "The PR image cannot be pulled. Revert to the image used before the PR, or deploy another PR.";
    return "The image cannot be pulled. Check that the image exists and that the cluster pull secret can read it.";
  }
  if (/CrashLoopBackOff/.test(text)) {
    if (kind === "mlflow" && state.prOverride) return "The PR build keeps crashing. Revert to the image used before the PR, or deploy another PR.";
    return "The container keeps crashing. Read its logs in the OpenShift console.";
  }
  if (/CreateContainerConfigError|CreateContainerError/.test(text)) {
    return "A Secret or ConfigMap the pod needs is missing or invalid. The message above names it.";
  }
  if (kind === "pipeline") {
    return minioReady
      ? "The pipeline server did not become ready within 5 minutes. Check its conditions in the project's Pipelines page."
      : "The pipeline server cannot reach the S3 storage. Fix the storage first; the pipeline server recovers once it serves requests.";
  }
  return null;
}

/**
 * Why a resource does not work and what to do, as the drawer's inline
 * alerts: an error (red) with its next step, a repair reason (orange) with
 * what Repair does, and a warning (orange).
 */
export const ProblemAlerts: React.FC<{ kind: ResourceKind; state: ResourceState; minioReady?: boolean }> = ({ kind, state, minioReady }) => {
  const step = nextStep(kind, state, minioReady);
  const reason = terminalReason(state);
  const alerts: React.ReactNode[] = [];
  // The next step goes with the repair reason when there is one (Repair is
  // the step), otherwise with the error.
  if (reason && !(state.repairNeeded && state.message === state.repairNeeded)) {
    const title = state.waitingReason || (reason.length <= 60 ? reason : "It cannot start");
    alerts.push(
      <Alert key="error" variant="danger" isInline component="p" title={title}>
        {state.message && state.message !== title && <span className="pf-v6-u-text-break-word"><TruncatedText>{withImageRefs(state.message)}</TruncatedText></span>}
        {!state.repairNeeded && step && <> {step}</>}
      </Alert>,
    );
  }
  if (state.repairNeeded) {
    alerts.push(
      <Alert key="repair" variant="warning" isInline component="p" title="Needs repair">
        <span className="pf-v6-u-text-break-word">{withImageRefs(state.repairNeeded)}</span>
        {step && <> {step}</>}
      </Alert>,
    );
  }
  if (state.warning) {
    alerts.push(<Alert key="warning" variant="warning" isInline component="p" title="Warning"><span className="pf-v6-u-text-break-word">{withImageRefs(state.warning)}</span></Alert>);
  }
  return alerts.length > 0 ? <Stack hasGutter>{alerts.map((a, i) => <StackItem key={i}>{a}</StackItem>)}</Stack> : null;
};

/** Who created a resource, for its details. */
export const createdBy = (state: ResourceState | undefined) => state?.managedByTool === false ? "Not this tool: it is left alone" : "This tool";

/** An external link as a full-size link button, for action rows (isInline is
 *  for links inside running text: it drops the button height, so the link
 *  would sit higher than the buttons beside it). */
export const ExternalLink: React.FC<{ href: string; children: React.ReactNode }> = ({ href, children }) => (
  <Button variant="link" component="a" href={href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{children}</Button>
);

/** A change the user is asked to confirm. */
export type Pending =
  | { kind: "setup-minio" | "repair-minio" | "setup-mlflow" | "revert-mlflow" | "teardown-minio" | "teardown-mlflow" }
  | { kind: "deploy-mlflow-pr"; pr: number }
  | { kind: "setup-pipeline-server" | "teardown-pipeline-server"; project: string; name?: string };

/** What the ConfirmActionModal shows for a pending change. */
export interface ConfirmSpec {
  title: string;
  confirm: string;
  changes: React.ReactNode[];
  dataLoss?: React.ReactNode;
  danger?: boolean;
  extra?: React.ReactNode;
}

/** What every card needs to offer its actions. */
export interface CardActionProps {
  /** Why changes are disabled now (permissions, session, a running operation), or null. */
  baseReason: string | null;
  /** The action running from this page, or null. */
  resAction: string | null;
  setPending: (p: Pending) => void;
}

export const pvcList = (state: ResourceState | undefined) => (state?.dataPVCs ?? []);

/** "PersistentVolumeClaim(s) a, b, with what." for a data-loss notice, or undefined without PVCs. */
export const pvcsLost = (state: ResourceState | undefined, what: string) => pvcList(state).length > 0
  ? <>PersistentVolumeClaim{pvcList(state).length > 1 ? "s" : ""} {pvcList(state).map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>)}, with {what}.</>
  : undefined;
