import React, { useCallback, useEffect, useState } from "react";
import { Alert, AlertActionLink, Content, ExpandableSection, Stack, StackItem } from "@patternfly/react-core";
import type { FeaturePrerequisites, ModelExposureKind, PrerequisiteFeature, PrerequisitePart, PrerequisitesStatus } from "../../types";
import { getPrerequisites, toApiError } from "../../services/api";
import { type ConfirmSpec, type Pending, type PrerequisitesPending } from "./resourceRow";
import { FeatureSection, PartItem, PartList, PrimaryAction } from "./FeatureLayout";
import { CopyableCode } from "./UseIt";
import { type FeatureState, maasPartStatus } from "./featureState";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import type { MaaSPart } from "../../types";

/**
 * One-click prerequisites (pkg/cluster feature_setup.go): what a feature
 * needs on the cluster before its own setup runs (DataScienceCluster
 * components, the operators they need), as a checklist like the MaaS tab's,
 * one confirmation that lists every change, and the same changes from a
 * terminal for an admin who prefers them.
 */

export const FEATURE_TITLES: Record<PrerequisiteFeature, string> = { pipelines: "Pipelines", mlflow: "MLflow", models: "Models", playground: "the playground" };

/** GET /api/resources/prerequisites, read again when refreshKey or exposure changes. */
export function usePrerequisites(refreshKey: number, exposure: ModelExposureKind | null, enabled = true) {
  const [data, setData] = useState<PrerequisitesStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    try {
      setData(await getPrerequisites(exposure));
      setError(null);
    } catch (e) {
      setError(toApiError(e, "Cannot read what the features need on the cluster").message);
    }
  }, [enabled, exposure]);
  useEffect(() => {
    void fetchNow();
  }, [fetchNow, refreshKey]);
  return { data, error, reload: fetchNow };
}

/** Why a feature cannot be set up yet because of its prerequisites, or null (ready, or unknown). */
export function prerequisitesReason(p: FeaturePrerequisites | undefined, feature: PrerequisiteFeature): string | null {
  if (!p || p.ready) return null;
  return `Needs what ${FEATURE_TITLES[feature]} needs on the cluster first: Set up the prerequisites above.`;
}

/**
 * The Overview's state of a feature whose cluster prerequisites are not
 * ready: while the feature is not set up, its next step sets them up.
 */
export function withPrerequisites(state: FeatureState, p: FeaturePrerequisites | undefined, feature: PrerequisiteFeature): FeatureState {
  if (!p || p.ready || state.loading || state.status !== "not-set-up") return state;
  const plan = p.setupPlan ?? [];
  return {
    ...state,
    label: "Needs cluster setup",
    next: { kind: "set-up", label: `Set up what ${FEATURE_TITLES[feature]} needs`, setup: [{ kind: "setup-prerequisites", feature }], reason: p.setupBlockedReason ?? (plan.length === 0 ? "A cluster-admin finishes it: the tab shows each part." : null) },
  };
}

const PrerequisitePartItem: React.FC<{ part: PrerequisitePart; feature: PrerequisiteFeature }> = ({ part, feature }) => {
  const onlyCommands = part.state === "manual";
  return (
    <PartItem id={`prereq-${feature}-${part.id}`} title={part.title} scope="Shared by the cluster" status={maasPartStatus(part as MaaSPart)} unmanaged={!!part.notManaged}>
      {part.message && <Content component="p">{part.message}</Content>}
      {part.warning && <Alert variant="warning" isInline component="p" title="Needs attention">{part.warning}</Alert>}
      {part.commands && (onlyCommands
        ? <CopyableCode id={`prereq-${feature}-${part.id}-commands`} label="the commands for a cluster-admin" code={part.commands} />
        : (
          <FeatureSection title="Do it from the CLI instead">
            <CopyableCode id={`prereq-${feature}-${part.id}-commands`} label="the commands" code={part.commands} />
          </FeatureSection>
        ))}
    </PartItem>
  );
};

/**
 * A feature's cluster prerequisites on its tab: collapsed to one line when
 * everything is ready; otherwise the checklist and Set up.
 */
export const PrerequisitesSection: React.FC<{
  feature: PrerequisiteFeature;
  prereq: FeaturePrerequisites | undefined;
  error: string | null;
  onRetry: () => void;
  /** Models: how the model about to be set up is reached (the plan depends on it). */
  exposure?: ModelExposureKind;
  readOnly: boolean;
  baseReason: string | null;
  setPending: (p: Pending) => void;
}> = ({ feature, prereq, error, onRetry, exposure, readOnly, baseReason, setPending }) => {
  if (!prereq) {
    return error
      ? <Alert variant="warning" isInline component="div" title="Cannot read what this feature needs on the cluster" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}><Content component="p">{error}</Content></Alert>
      : null;
  }
  const parts = prereq.parts ?? [];
  if (prereq.ready) {
    return (
      <ExpandableSection toggleText={`Everything ${FEATURE_TITLES[feature]} needs on the cluster is ready (${parts.length} ${parts.length === 1 ? "part" : "parts"})`} isIndented>
        <PartList aria-label={`What ${FEATURE_TITLES[feature]} needs on the cluster`}>
          {parts.map((p) => <PrerequisitePartItem key={p.id} part={p} feature={feature} />)}
        </PartList>
      </ExpandableSection>
    );
  }
  const plan = prereq.setupPlan ?? [];
  const reason = readOnly ? READ_ONLY_SETUP : baseReason ?? prereq.setupBlockedReason ?? (plan.length === 0 ? "Nothing Set up can do now: the parts above say what they wait for or who does it." : null);
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="h3">On the cluster first</Content>
        <Content component="p">{FEATURE_TITLES[feature].charAt(0).toUpperCase() + FEATURE_TITLES[feature].slice(1)} needs these on the cluster before its own setup can run. Set up does only what is missing, after one confirmation that lists every change.</Content>
      </StackItem>
      <StackItem>
        <PartList aria-label={`What ${FEATURE_TITLES[feature]} needs on the cluster`}>
          {parts.map((p) => <PrerequisitePartItem key={p.id} part={p} feature={feature} />)}
        </PartList>
      </StackItem>
      <StackItem>
        <PrimaryAction label={`Set up what ${FEATURE_TITLES[feature]} needs`} variant="secondary" onClick={() => setPending({ kind: "setup-prerequisites", feature, ...(exposure ? { exposure } : {}) })} reason={reason} />
      </StackItem>
    </Stack>
  );
};

/** The confirmation of a prerequisites setup: every change of the plan. */
export function prerequisitesConfirm(pending: Pending, st: PrerequisitesStatus | null): ConfirmSpec | null {
  if (pending.kind !== "setup-prerequisites") return null;
  const p = st?.features?.[pending.feature];
  const title = `Set up what ${FEATURE_TITLES[pending.feature]} needs on the cluster`;
  return {
    title: `${title}?`, confirm: "Set up", runTitle: title,
    summary: <Content component="p">Only what is missing, in this order; each change is checked on the server before the first one is made. Components this tool turns on can be turned back with Clean up while nothing uses them; operators stay, as other features share them.{pending.exposure === "default-gateway" || pending.exposure === "gateway" ? " Red Hat Connectivity Link is included because the model's calls go through a gateway, which it authenticates." : ""}</Content>,
    changes: [
      ...(p?.setupPlan ?? []).map((c) => <>{c}.</>),
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
    ...(parts.some((p) => p.id === "kuadrant") ? [{ id: "kuadrant", title: "Kuadrant CR" }] : []),
    { id: "ready", title: "Wait until it is ready" },
  ];
}
