import React from "react";
import { ActionList, ActionListGroup, Button, Flex, FlexItem, HelperText, HelperTextItem } from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import LockIcon from "@patternfly/react-icons/dist/esm/icons/lock-icon";
import type { ResourceState } from "../../types";
import { withImageRefs } from "../ImageRef";
import { StatusLabel, TagLabel } from "../StatusLabel";
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

const ResourceStatus: React.FC<{ state: ResourceState }> = ({ state }) => {
  if (isTerminating(state)) return <StatusLabel status="warning">Terminating</StatusLabel>;
  // Running, but not serving through its Service/Route: Repair fixes it.
  if (state.deployed && !state.ready && state.terminalError && state.repairNeeded) return <StatusLabel status="warning">Incomplete</StatusLabel>;
  if (state.ready) return <StatusLabel status="success">Running</StatusLabel>;
  if (!state.deployed) return <StatusLabel status="neutral">Not deployed</StatusLabel>;
  const reason = terminalReason(state);
  if (reason) {
    const short = state.waitingReason || (reason !== "Failed" && reason.length <= 40 ? reason : "");
    return <StatusLabel status="danger">{short ? `Failed: ${short}` : "Failed"}</StatusLabel>;
  }
  return <StatusLabel status="progress">Starting</StatusLabel>;
};

/** The resource's message, warnings and next step as helper text under its description. */
export const StatusDetails: React.FC<{ kind: ResourceKind; state: ResourceState; minioReady?: boolean; teardownBlocked?: string | null; notes?: React.ReactNode[] }> = ({ kind, state, minioReady, teardownBlocked, notes }) => {
  const step = nextStep(kind, state, minioReady);
  // The label already says "Starting" / "Terminating": don't repeat it as the message.
  const showMessage = state.deployed && !state.ready && !!state.message && !["Terminating", "Starting"].includes(state.message);
  const items: React.ReactNode[] = [];
  if (showMessage) {
    items.push(<HelperTextItem key="message" variant={state.repairNeeded && state.message === state.repairNeeded ? "warning" : terminalReason(state) ? "error" : "default"}><span className="pf-v6-u-text-break-word"><TruncatedText>{withImageRefs(state.message!)}</TruncatedText></span></HelperTextItem>);
  }
  if (state.warning) items.push(<HelperTextItem key="warning" variant="warning">{state.warning}</HelperTextItem>);
  if (state.repairNeeded && !(showMessage && state.message === state.repairNeeded)) {
    items.push(<HelperTextItem key="repair" variant="warning">Needs repair: {state.repairNeeded}</HelperTextItem>);
  }
  if (step) items.push(<HelperTextItem key="step">Next step: {step}</HelperTextItem>);
  if (teardownBlocked) items.push(<HelperTextItem key="blocked" variant="warning">Tear down is blocked: {teardownBlocked}</HelperTextItem>);
  notes?.forEach((note, i) => items.push(<HelperTextItem key={`note-${i}`}>{note}</HelperTextItem>));
  return items.length > 0 ? <HelperText className="pf-v6-u-mt-sm">{items}</HelperText> : null;
};

/** Why the tool leaves an object it did not create alone, appended to the row description. */
export function unmanagedNote(state: ResourceState | undefined): string {
  return state?.managedByTool === false && state.teardownBlockedReason ? ` ${state.teardownBlockedReason}` : "";
}

/** Name, status and category tags of one resource row. */
export const RowTitle: React.FC<{ id: string; name: string; state?: ResourceState; extra?: React.ReactNode }> = ({ id, name, state, extra }) => (
  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
    <FlexItem><strong id={id}>{name}</strong></FlexItem>
    {state && <FlexItem><ResourceStatus state={state} /></FlexItem>}
    {/* Only for something that exists: the backend also reports managedByTool false when nothing is deployed. */}
    {state?.managedByTool === false && (state.deployed || isTerminating(state) || !!state.setupBlockedReason) && (
      <FlexItem><TagLabel icon={<LockIcon />}>Not managed by this tool</TagLabel></FlexItem>
    )}
    {extra}
  </Flex>
);

export const ExternalLink: React.FC<{ href: string; children: React.ReactNode }> = ({ href, children }) => (
  <Button variant="link" isInline component="a" href={href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{children}</Button>
);

/**
 * The actions of one resource row (ActionListItems): one ActionListGroup,
 * so they get PatternFly's action-to-action spacing; beside the row on wide
 * screens (right-aligned), below it otherwise.
 */
export const rowActions = (children: React.ReactNode) => (
  <Flex justifyContent={{ md: "justifyContentFlexEnd" }}>
    <ActionList>
      <ActionListGroup>{children}</ActionListGroup>
    </ActionList>
  </Flex>
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
