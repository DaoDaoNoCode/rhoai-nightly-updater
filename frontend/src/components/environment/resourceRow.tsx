import React from "react";
import { Alert, AlertActionLink, Button, Content, DescriptionList, DescriptionListDescription, DescriptionListGroup, DescriptionListTerm, ExpandableSection, Stack, StackItem } from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { MaaSActionId, MLflowSampleAction, MLflowSampleId, ModelExposureKind, ModelExposureOption, PlaygroundModelRef, PrerequisiteFeature, ResourceState } from "../../types";
import type { AudienceForm, DatabaseForm, LimitForm } from "./maasForms";
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

export type ResourceKind = "minio" | "mlflow" | "pipeline" | "gateway" | "model" | "playground";

const SUBJECT: Record<ResourceKind, string> = { minio: "The S3 storage", mlflow: "MLflow", pipeline: "The pipeline server", gateway: "The inference gateway", model: "The model", playground: "The playground" };

const rawText = (state: ResourceState) => `${state.waitingReason ?? ""} ${state.message ?? ""}`;
const imagePull = (state: ResourceState) => /ImagePullBackOff|ErrImagePull|InvalidImageName/.test(rawText(state));
const crashing = (state: ResourceState) => /CrashLoopBackOff/.test(rawText(state));
const badConfig = (state: ResourceState) => /CreateContainerConfigError|CreateContainerError/.test(rawText(state));

/** What is wrong with a resource that cannot start, in words (no pod reasons): an alert's title. */
export function problemTitle(kind: ResourceKind, state: ResourceState): string {
  const who = SUBJECT[kind];
  if (imagePull(state)) {
    if (kind === "mlflow" && state.prOverride) return "The MLflow PR image can't be downloaded";
    return kind === "pipeline" ? "An image of the pipeline server can't be downloaded" : `${who}'s image can't be downloaded`;
  }
  if (crashing(state)) return `${who} keeps crashing`;
  if (badConfig(state)) return `${who} can't start: a setting it needs is missing`;
  if (kind === "pipeline") return "The pipeline server did not start";
  return `${who} can't start`;
}

/** What to do about a resource that cannot start or needs a repair (A08-3, A06-5), in words. */
export function nextStep(kind: ResourceKind, state: ResourceState, minioReady = true): string | null {
  const managed = state.managedByTool !== false;
  if (kind === "minio" && managed && state.repairNeeded) {
    return "Repair sets it up again with this version's settings and starts it. Stored data is kept.";
  }
  if (!terminalReason(state)) return null;
  const notOurs = managed ? "" : " It was created outside this tool (for example in the dashboard), so this tool does not change it: ask its owner.";
  if (imagePull(state)) {
    if (kind === "minio" && managed) return "Repair applies it again with the image this updater uses. Stored data is kept.";
    if (kind === "mlflow" && state.prOverride) return "Revert to the image used before the PR, or deploy another PR.";
    return `Check that the image exists and that the cluster's pull secret can read it.${notOurs}`;
  }
  if (crashing(state)) {
    if (kind === "mlflow" && state.prOverride) return "The PR build keeps crashing. Revert to the image used before the PR, or deploy another PR.";
    return `Its logs say why.${notOurs}`;
  }
  if (badConfig(state)) return `A Secret or ConfigMap it needs is missing or invalid; the technical details name it.${notOurs}`;
  if (kind === "pipeline") {
    return minioReady
      ? "It did not become ready within 5 minutes. The project's Pipelines page in the dashboard shows its conditions."
      : "It cannot reach the S3 storage. Fix the storage first; the pipeline server recovers once the storage works.";
  }
  return null;
}

/** The raw reasons, for whoever needs them: under "Technical details". */
export const TechnicalDetails: React.FC<{ rows: { term: string; value: string }[] }> = ({ rows }) => {
  const [expanded, setExpanded] = React.useState(false);
  return (
  <ExpandableSection toggleText="Technical details" isIndented isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)}>
    <DescriptionList isCompact isHorizontal aria-label="Technical details">
      {rows.map((r) => (
        <DescriptionListGroup key={r.term}>
          <DescriptionListTerm>{r.term}</DescriptionListTerm>
          <DescriptionListDescription><span className="pf-v6-u-text-break-word"><TruncatedText>{withImageRefs(r.value)}</TruncatedText></span></DescriptionListDescription>
        </DescriptionListGroup>
      ))}
    </DescriptionList>
  </ExpandableSection>
  );
};

const technicalRows = (state: ResourceState) => [
  ...(state.waitingReason ? [{ term: "Reason", value: state.waitingReason }] : []),
  ...(state.message && state.message !== state.waitingReason ? [{ term: "Message", value: state.message }] : []),
];

/** A fix an alert offers as its action link (Repair, Revert); reason, when set, is why it cannot run now. */
export interface AlertFix {
  label: string;
  onClick: () => void;
  reason?: string | null;
}

/**
 * Why a resource does not work and what to do, as inline alerts: the
 * problem in words as the title, the next step in the body, the raw
 * reasons under "Technical details", and the fix and the logs as the
 * alert's action links, next to the explanation.
 */
export const ProblemAlerts: React.FC<{
  kind: ResourceKind;
  state: ResourceState;
  minioReady?: boolean;
  fix?: AlertFix | null;
  /** The OpenShift console's page of the resource's pods, when known. */
  logsHref?: string;
}> = ({ kind, state, minioReady, fix, logsHref }) => {
  const step = nextStep(kind, state, minioReady);
  const reason = terminalReason(state);
  // A fix that cannot run now is not offered as a link; the body says why.
  const fixLink = fix && !fix.reason ? <AlertActionLink key="fix" onClick={fix.onClick}>{fix.label}</AlertActionLink> : null;
  const fixBlocked = fix?.reason ? <StackItem><Content component="p">{fix.label} is not available now: {fix.reason}</Content></StackItem> : null;
  const logsLink = logsHref ? (
    <AlertActionLink key="logs" component="a" href={logsHref} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">View logs</AlertActionLink>
  ) : null;
  const alerts: React.ReactNode[] = [];
  // The next step goes with the repair alert when there is one (Repair is
  // the step), otherwise with the error.
  if (reason && !(state.repairNeeded && state.message === state.repairNeeded)) {
    const actions = [...(!state.repairNeeded && fixLink ? [fixLink] : []), ...(logsLink ? [logsLink] : [])];
    alerts.push(
      <Alert key="error" variant="danger" isInline component="div" title={problemTitle(kind, state)} actionLinks={actions.length ? actions : undefined}>
        <Stack hasGutter>
          {!state.repairNeeded && step && <StackItem><Content component="p">{step}</Content></StackItem>}
          {!state.repairNeeded && fixBlocked}
          {technicalRows(state).length > 0 && <StackItem><TechnicalDetails rows={technicalRows(state)} /></StackItem>}
        </Stack>
      </Alert>,
    );
  }
  if (state.repairNeeded) {
    alerts.push(
      <Alert key="repair" variant="warning" isInline component="div" title={`${SUBJECT[kind]} needs a repair`} actionLinks={fixLink ?? undefined}>
        <Stack hasGutter>
          <StackItem><div className="pf-v6-c-content--p pf-v6-u-text-break-word">{withImageRefs(state.repairNeeded)}</div></StackItem>
          {step && <StackItem><Content component="p">{step}</Content></StackItem>}
          {fixBlocked}
        </Stack>
      </Alert>,
    );
  }
  if (state.warning) {
    // The warning's first sentence says what is wrong: the title; the rest is what to do.
    const cut = state.warning.indexOf(". ");
    const [head, rest] = cut > 0 ? [state.warning.slice(0, cut + 1), state.warning.slice(cut + 2)] : [state.warning, ""];
    alerts.push(
      <Alert key="warning" variant="warning" isInline component="div" title={<span className="pf-v6-u-text-break-word">{withImageRefs(head)}</span>}>
        {rest && <span className="pf-v6-u-text-break-word">{withImageRefs(rest)}</span>}
      </Alert>,
    );
  }
  return alerts.length > 0 ? <Stack hasGutter>{alerts.map((a, i) => <StackItem key={i}>{a}</StackItem>)}</Stack> : null;
};

/** A warning whose first sentence (what is wrong) is the title and the rest (what to do) the body, as ProblemAlerts shows a resource's. */
export const WarningAlert: React.FC<{ text: string }> = ({ text }) => {
  const cut = text.indexOf(". ");
  const [head, rest] = cut > 0 ? [text.slice(0, cut + 1), text.slice(cut + 2)] : [text, ""];
  return (
    <Alert variant="warning" isInline component="div" title={<span className="pf-v6-u-text-break-word">{head}</span>}>
      {rest && <span className="pf-v6-u-text-break-word">{rest}</span>}
    </Alert>
  );
};

/**
 * A resource that exists but is not ready and not failing: it is starting.
 * Plain words, with the operator's own text under "Technical details".
 */
export const StartingNote: React.FC<{ state: ResourceState; typical: string }> = ({ state, typical }) => (
  <Stack hasGutter>
    <StackItem><Content component="p">Starting. This usually takes {typical}; this page updates when it is ready.</Content></StackItem>
    {state.message && <StackItem><TechnicalDetails rows={[{ term: "State", value: state.message }]} /></StackItem>}
  </Stack>
);

/** An external link as a full-size link button, for action rows (isInline is
 *  for links inside running text: it drops the button height, so the link
 *  would sit higher than the buttons beside it). */
export const ExternalLink: React.FC<{ href: string; children: React.ReactNode; variant?: "link" | "primary" | "secondary"; isInline?: boolean }> = ({ href, children, variant = "link", isInline }) => (
  <Button variant={variant} isInline={isInline} component="a" href={href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{children}</Button>
);

/** An OpenShift console page of a Deployment's pods (with their logs), or undefined without a console address. */
export function consolePodsHref(consoleURL: string | undefined, namespace: string | undefined, deployment: string): string | undefined {
  if (!consoleURL?.startsWith("https://") || !namespace) return undefined;
  return `${consoleURL.replace(/\/$/, "")}/k8s/ns/${encodeURIComponent(namespace)}/deployments/${encodeURIComponent(deployment)}/pods`;
}

/** A change the user is asked to confirm. */
export type Pending =
  | { kind: "setup-minio" | "repair-minio" | "setup-mlflow" | "revert-mlflow" | "teardown-mlflow" }
  /** deleteNamespace: also delete namespace seaweedfs (the user's informed choice). */
  | { kind: "teardown-minio"; deleteNamespace?: boolean }
  | { kind: "deploy-mlflow-pr"; pr: number }
  /** again: set up an existing pipeline server again (for example to recreate its Connection). */
  | { kind: "setup-pipeline-server" | "teardown-pipeline-server"; project: string; name?: string; again?: boolean }
  /** Replace the tool's pipeline server named nightly-dspa with one named dspa (workbench pipelines need it). */
  | { kind: "recreate-pipeline-server"; project: string; name: string }
  /** MLflow samples in a project's workspace (one request for all of them). */
  | { kind: "mlflow-samples"; project: string; action: MLflowSampleAction; samples: MLflowSampleId[]; repairs?: string[] }
  /** Models: the inference gateway and/or the model in a project, as one streamed operation. */
  /** model: the catalog name of the model in project. */
  /** exposure / gatewayRef: how the model is reached (absent: the recommended way); option: the chosen option, for the confirmation. */
  | { kind: "setup-models"; gateway: boolean; project: string | null; model: string; releasedImage?: boolean; exposure?: ModelExposureKind; gatewayRef?: string; option?: ModelExposureOption }
  /** unpublish: the model was set up through MaaS only, so the teardown unpublishes it first (ModelState.maasOnlySetup). */
  | { kind: "teardown-model"; project: string; model: string; unpublish?: boolean }
  | { kind: "teardown-gateway" }
  /** Add the project to the projects this tool's inference gateway allows (a model someone else deployed there). */
  | { kind: "allow-gateway-project"; project: string }
  /** Models routing: take a project off this tool's gateway's list; gone: the project no longer exists. */
  | { kind: "disallow-gateway-project"; project: string; gone: boolean }
  /** Models routing: "Can it be called?" again. */
  | { kind: "check-model"; project: string; model: string }
  /** One-click prerequisites of a feature (Models: for how its model is reached). */
  /** recommended: planned for the recommended way to reach a model (the Overview), not a choice on the tab. */
  | { kind: "setup-prerequisites"; feature: PrerequisiteFeature; exposure?: ModelExposureKind; recommended?: boolean }
  /** The Gen AI playground in a project, through the dashboard's API (one streamed operation). */
  | { kind: "setup-playground"; project: string; models?: PlaygroundModelRef[] }
  | { kind: "teardown-playground"; project: string }
  /** Models as a Service: the platform, or this tool's model in a project (one streamed operation). */
  /** database: the dialog's choice, while Set up must create one (maasForms.ts). */
  | { kind: "setup-maas"; database?: DatabaseForm }
  | { kind: "teardown-maas" }
  /** access: only for the first published model (the audience is the subscription's). */
  | { kind: "publish-maas"; project: string; model: string; access?: AudienceForm; limit?: LimitForm }
  | { kind: "unpublish-maas"; project: string; model: string }
  /** MaaS: a published model's limit, who can use the published models, the database (every key is lost). */
  | { kind: "maas-limit"; project: string; model: string; limit?: LimitForm }
  | { kind: "maas-access"; access?: AudienceForm }
  | { kind: "maas-database"; database?: DatabaseForm; confirmKeysLost?: boolean }
  /** A MaaS platform part's own fix (its action lists the changes). */
  | { kind: "maas-action"; action: MaaSActionId };

/** A sample change: a streamed request of its own, not a resource's setup or deletion. */
export type SamplePending = Extract<Pending, { kind: "mlflow-samples" }>;
/** A Models change: one streamed request (pkg/api HandleModelsStream). */
export type ModelsPending = Extract<Pending, { kind: "setup-models" | "teardown-model" | "teardown-gateway" | "allow-gateway-project" | "disallow-gateway-project" | "check-model" }>;
/** A prerequisites setup: one streamed request (pkg/api HandlePrerequisitesStream). */
export type PrerequisitesPending = Extract<Pending, { kind: "setup-prerequisites" }>;
export const isPrerequisitesPending = (p: Pending): p is PrerequisitesPending => p.kind === "setup-prerequisites";
/** A Playground change: one streamed request (pkg/api HandlePlaygroundStream). */
export type PlaygroundPending = Extract<Pending, { kind: "setup-playground" | "teardown-playground" }>;
/** A MaaS change: one streamed request (pkg/api HandleMaaSStream). */
export type MaaSPending = Extract<Pending, { kind: "setup-maas" | "teardown-maas" | "publish-maas" | "unpublish-maas" | "maas-action" | "maas-limit" | "maas-access" | "maas-database" }>;
const MAAS_KINDS: string[] = ["setup-maas", "teardown-maas", "publish-maas", "unpublish-maas", "maas-action", "maas-limit", "maas-access", "maas-database"];
export const isMaaSPending = (p: Pending): p is MaaSPending => MAAS_KINDS.includes(p.kind);
export type ResourcePending = Exclude<Pending, SamplePending | ModelsPending | PlaygroundPending | MaaSPending | PrerequisitesPending>;
export const isPlaygroundPending = (p: Pending): p is PlaygroundPending => p.kind === "setup-playground" || p.kind === "teardown-playground";
export const isSamplePending = (p: Pending): p is SamplePending => p.kind === "mlflow-samples";
export const isModelsPending = (p: Pending): p is ModelsPending => p.kind === "setup-models" || p.kind === "teardown-model" || p.kind === "teardown-gateway" || p.kind === "allow-gateway-project" || p.kind === "disallow-gateway-project" || p.kind === "check-model";

/** What the ConfirmActionModal shows for a pending change. */
export interface ConfirmSpec {
  title: string;
  confirm: string;
  /** The outcome in words, first ("Deletes the pipeline server in x, with its runs."). */
  summary?: React.ReactNode;
  /** Exactly what changes on the cluster, one item per object: under "Details". */
  changes: React.ReactNode[];
  dataLoss?: React.ReactNode;
  /** A choice that changes what is deleted, shown above the data-loss alert it changes. */
  choice?: React.ReactNode;
  danger?: boolean;
  /** Confirm stays disabled (a form in the dialog is not complete or valid yet). */
  confirmDisabled?: boolean;
  /** Show the changes open after the summary, not under "Details" (a plan the user should read). */
  changesOpen?: boolean;
  extra?: React.ReactNode;
  /** The run's title and its step's line once done ("Delete the pipeline server in x"). */
  runTitle?: string;
}

export const pvcList = (state: ResourceState | undefined) => (state?.dataPVCs ?? []);

/** "PersistentVolumeClaim(s) a, b, with what." for a data-loss notice, or undefined without PVCs. */
export const pvcsLost = (state: ResourceState | undefined, what: string) => pvcList(state).length > 0
  ? <>{what.charAt(0).toUpperCase() + what.slice(1)} (volume{pvcList(state).length > 1 ? "s" : ""} {pvcList(state).map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>)}).</>
  : undefined;
