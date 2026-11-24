import React, { useCallback, useEffect, useState } from "react";
import { Alert, AlertActionLink, Content, ExpandableSection, Skeleton, Stack, StackItem } from "@patternfly/react-core";
import type { FeaturePrerequisites, ModelExposureKind, PrerequisiteFeature, PrerequisitePart, PrerequisitesStatus } from "../../types";
import { getPrerequisites, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { IDLE_REFRESH_MS } from "../../constants";
import { type ConfirmSpec, type Pending, type PrerequisitesPending, WarningAlert } from "./resourceRow";
import { FeatureSection, PartItem, PartList, PrimaryAction } from "./FeatureLayout";
import { CopyableCode } from "./UseIt";
import { type FeatureState, maasPartLabelStatus } from "./featureState";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import type { MaaSPart } from "../../types";
import { useLatestRequest } from "../../hooks/useLatestRequest";

/**
 * One-click prerequisites (pkg/cluster feature_setup.go): what a feature
 * needs on the cluster before its own setup runs (DataScienceCluster
 * components, the operators they need), as a checklist like the MaaS tab's,
 * one confirmation that lists every change, and the same changes from a
 * terminal for an admin who prefers them.
 */

export const FEATURE_TITLES: Record<PrerequisiteFeature, string> = { pipelines: "Pipelines", mlflow: "MLflow", "model-registry": "the model registry", models: "Models", playground: "the playground", kueue: "the distributed workloads setup" };

/**
 * The way to reach a model the Overview plans Models for: the recommended
 * one, the inference gateway (KServe's default route, as the dashboard
 * deploys models), which needs Red Hat Connectivity Link.
 */
export const RECOMMENDED_EXPOSURE: ModelExposureKind = "default-gateway";

const capitalized = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/**
 * GET /api/resources/prerequisites, read again when refreshKey or exposure
 * changes. An answer to an older request is dropped, and stale says that
 * the answer kept is for another way to reach a model than exposure (the
 * backend echoes the one it planned for).
 */
export function usePrerequisites(refreshKey: number, exposure: ModelExposureKind | null, enabled = true) {
  const [data, setData] = useState<PrerequisitesStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const latest = useLatestRequest();
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const d = await getPrerequisites(exposure, ticket.signal);
      if (!ticket.isLatest()) return;
      setData(d);
      setError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError(toApiError(e, "Cannot read what the features need on the cluster").message);
    }
  }, [enabled, exposure, latest]);
  useEffect(() => {
    void fetchNow();
  }, [fetchNow, refreshKey]);
  // A slow re-read while the browser tab is visible, so a change made elsewhere shows on its own.
  usePolling(fetchNow, { enabled, delay: IDLE_REFRESH_MS, pauseWhenHidden: true, refreshOnVisible: true });
  const stale = !!data && !plannedFor(data, exposure);
  return { data, error, reload: fetchNow, stale };
}

/**
 * Whether an answer was planned for exposure (null/"" none). An answer
 * that does not say (an older backend) is taken as it is.
 */
export const plannedFor = (st: PrerequisitesStatus | null | undefined, exposure: string | null | undefined) =>
  st?.modelsExposure === undefined || st.modelsExposure === (exposure ?? "");

/** Why a feature cannot be set up yet because of its prerequisites, or null (ready, or unknown). */
export function prerequisitesReason(p: FeaturePrerequisites | undefined, feature: PrerequisiteFeature): string | null {
  if (!p || p.ready) return null;
  return `First set up what ${FEATURE_TITLES[feature]} needs on the cluster (above).`;
}

/**
 * The Overview's state of a feature whose cluster prerequisites are not
 * ready: while the feature is not set up, its next step sets them up
 * (Models: for the recommended way to reach a model); a feature that
 * reads ready needs attention.
 */
export function withPrerequisites(state: FeatureState, p: FeaturePrerequisites | undefined, feature: PrerequisiteFeature): FeatureState {
  if (!p || p.ready || state.loading) return state;
  const plan = p.setupPlan ?? [];
  const setup: Pending[] = [{ kind: "setup-prerequisites", feature, ...(feature === "models" ? { exposure: RECOMMENDED_EXPOSURE, recommended: true } : {}) }];
  const reason = p.setupBlockedReason ?? (plan.length === 0 ? "A cluster-admin finishes it: the tab shows each part." : null);
  if (state.status === "ready") {
    return { ...state, status: "needs-attention", label: `Needs attention${state.project ? ` in ${state.project}` : ""}`, next: { kind: "set-up", label: `Set up what ${FEATURE_TITLES[feature]} needs`, setup, reason } };
  }
  if (state.status !== "not-set-up") return state;
  return { ...state, label: "Needs cluster setup", next: { kind: "set-up", label: `Set up what ${FEATURE_TITLES[feature]} needs`, setup, reason } };
}

/** A tab header's state: a feature that reads ready while its cluster prerequisites are not needs attention. */
export function headerWithPrerequisites(state: FeatureState, p: FeaturePrerequisites | undefined): FeatureState {
  if (!p || p.ready || state.loading || state.status !== "ready") return state;
  return { ...state, status: "needs-attention", label: `Needs attention${state.project ? ` in ${state.project}` : ""}` };
}

const PrerequisitePartItem: React.FC<{ part: PrerequisitePart; feature: PrerequisiteFeature }> = ({ part, feature }) => {
  // Waiting for something else (an operator, KServe's check) is not this
  // part setting itself up: a grey "Waiting", not the blue spinner.
  const waiting = part.state === "waiting";
  return (
    <PartItem id={`prereq-${feature}-${part.id}`} title={part.title} scope="Shared by the cluster" status={waiting ? "not-set-up" : maasPartLabelStatus(part as MaaSPart)}
      statusText={waiting ? "Waiting" : undefined} unmanaged={!!part.notManaged}>
      {part.message && <Content component="p">{part.message}</Content>}
      {part.warning && <WarningAlert text={part.warning} />}
      {part.commands && part.state === "manual" && (
        <CopyableCode id={`prereq-${feature}-${part.id}-commands`} label="the commands for a cluster-admin" code={part.commands} />
      )}
    </PartItem>
  );
};

/** What Set up does, from a terminal: each missing part's commands, in setup order. */
export function prerequisitesScript(parts: PrerequisitePart[]): string {
  return parts.filter((p) => p.commands && p.state === "missing").map((p) => `# ${p.title}\n${p.commands}`).join("\n\n");
}

/**
 * A feature's cluster prerequisites on its tab: collapsed to one line when
 * everything is ready; otherwise the checklist, Set up (the page's primary
 * action until they are ready) and the same steps from a terminal.
 */
export const PrerequisitesSection: React.FC<{
  feature: PrerequisiteFeature;
  prereq: FeaturePrerequisites | undefined;
  error: string | null;
  onRetry: () => void;
  /** Models: how the model about to be set up is reached (the plan depends on it). */
  exposure?: ModelExposureKind;
  /** The answer is for another way to reach the model: it is being read again. */
  stale?: boolean;
  readOnly: boolean;
  baseReason: string | null;
  setPending: (p: Pending) => void;
}> = ({ feature, prereq, error, onRetry, exposure, stale, readOnly, baseReason, setPending }) => {
  if (!prereq) {
    return error
      ? <Alert variant="warning" isInline component="div" title="Cannot read what this feature needs on the cluster" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}><Content component="p">{error}</Content></Alert>
      : null;
  }
  if (stale) {
    return <Skeleton height="2.5rem" screenreaderText={`Checking what ${FEATURE_TITLES[feature]} needs on the cluster for this choice`} />;
  }
  const parts = prereq.parts ?? [];
  if (prereq.ready) {
    return (
      <ExpandableSection toggleText={`Cluster prerequisites for ${FEATURE_TITLES[feature]}: ${parts.length} of ${parts.length} ready`} isIndented>
        <PartList aria-label={`What ${FEATURE_TITLES[feature]} needs on the cluster`}>
          {parts.map((p) => <PrerequisitePartItem key={p.id} part={p} feature={feature} />)}
        </PartList>
      </ExpandableSection>
    );
  }
  const plan = prereq.setupPlan ?? [];
  const reason = readOnly ? READ_ONLY_SETUP : baseReason ?? prereq.setupBlockedReason ?? (plan.length === 0 ? "Nothing Set up can do now: the parts above say what they wait for or who does it." : null);
  const script = prerequisitesScript(parts);
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="h3">Needed on the cluster first</Content>
        <Content component="p">{capitalized(FEATURE_TITLES[feature])} needs these on the cluster before its own setup can run. Set up does only what is missing, after one confirmation that lists every change.</Content>
      </StackItem>
      <StackItem>
        <PartList aria-label={`What ${FEATURE_TITLES[feature]} needs on the cluster`}>
          {parts.map((p) => <PrerequisitePartItem key={p.id} part={p} feature={feature} />)}
        </PartList>
      </StackItem>
      <StackItem>
        <PrimaryAction label={`Set up what ${FEATURE_TITLES[feature]} needs`} onClick={() => setPending({ kind: "setup-prerequisites", feature, ...(exposure ? { exposure } : {}) })} reason={reason} />
      </StackItem>
      {script && (
        <StackItem>
          <FeatureSection title="Do it from the CLI instead">
            <Stack hasGutter>
              <StackItem><Content component="p">The same steps, in order, for a cluster-admin who prefers a terminal.</Content></StackItem>
              <StackItem><CopyableCode id={`prereq-${feature}-commands`} label="the commands" code={script} /></StackItem>
            </Stack>
          </FeatureSection>
        </StackItem>
      )}
    </Stack>
  );
};

/** The confirmation of a prerequisites setup: every change of the plan, open. */
export function prerequisitesConfirm(pending: Pending, st: PrerequisitesStatus | null): ConfirmSpec | null {
  if (pending.kind !== "setup-prerequisites") return null;
  const p = st?.features?.[pending.feature];
  // The plan shown must be the one for this choice of how a model is
  // reached: until the answer for it is in, Confirm waits.
  const planned = pending.feature !== "models" || plannedFor(st, pending.exposure);
  const plan = planned ? p?.setupPlan ?? [] : [];
  const installs = plan.some((c) => c.startsWith("Install "));
  const title = `Set up what ${FEATURE_TITLES[pending.feature]} needs on the cluster`;
  const gatewayChoice = pending.exposure === "default-gateway" || pending.exposure === "gateway";
  return {
    title: `${title}?`, confirm: installs ? "Install and turn on" : "Turn on", runTitle: title, changesOpen: true, confirmDisabled: !planned,
    summary: (
      <Stack hasGutter>
        {installs && <StackItem><Content component="p">Operators it installs stay installed after Clean up, because other features share them.</Content></StackItem>}
        <StackItem>
          <Content component="p">
            Only what is missing, in this order; each change is checked on the server before the first one is made. Components this tool turns on can be turned back with Clean up while nothing uses them.
            {pending.recommended ? " Includes Red Hat Connectivity Link, for the recommended inference gateway." : gatewayChoice ? " Red Hat Connectivity Link is included because the model's calls go through a gateway, which it authenticates." : ""}
          </Content>
        </StackItem>
        {!planned && <StackItem><Skeleton screenreaderText="Reading the plan for this choice" /></StackItem>}
      </Stack>
    ),
    changes: [
      ...plan.map((c) => <>{c}.</>),
      <>Not changed: anything that exists already, and everything this tool did not create.</>,
    ],
  };
}

/** The steps a prerequisites setup streams (pkg/cluster RunPrerequisites). */
export function prerequisitesSteps(pending: PrerequisitesPending, st: PrerequisitesStatus | null): { id: string; title: string }[] {
  const parts = st?.features?.[pending.feature]?.parts ?? [];
  const operators = parts.filter((p) => !p.id.startsWith("dsc-") && p.id !== "dependencies" && p.id !== "kuadrant");
  return [
    { id: "prerequisites", title: "Check what is needed" },
    { id: "dsc", title: "DataScienceCluster components" },
    ...operators.map((p) => ({ id: p.id, title: p.title })),
    ...(parts.some((p) => p.id === "kuadrant") ? [{ id: "kuadrant", title: "Connectivity Link instance (Kuadrant)" }] : []),
    { id: "ready", title: "Wait until it is ready" },
  ];
}
