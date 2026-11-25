import React, { useCallback, useEffect, useState } from "react";
import { ActionListItem, Alert, AlertActionLink, Content, Label, List, ListItem, Stack, StackItem } from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { ObservabilityPart, ObservabilityStatus } from "../../types";
import { getObservability, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { IDLE_REFRESH_MS, RESOURCE_SETTLE_MAX_MS } from "../../constants";
import { type ConfirmSpec, type ObservabilityPending, type Pending, ExternalLink, WarningAlert } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, PrimaryAction, TabSection } from "./FeatureLayout";
import { ComponentAlert, CopyableCode } from "./UseIt";
import { LoadFailed, PartsSkeleton } from "./Loading";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type FeatureState, maasPartLabelStatus, observabilitySettling } from "./featureState";
import type { MaaSPart } from "../../types";

/** How often the tab reads again while a part settles. */
export const OBSERVABILITY_POLL_MS = 5_000;

/**
 * What the stack costs, measured on a test cluster with RHOAI 3.7.0-ea.1
 * (n8 spike, 2026-11-25): its disks and pods. The tab shows the cluster's
 * own PVCs once they exist.
 */
export const OBSERVABILITY_COST =
  "About 51Gi of disks in 7 PersistentVolumeClaims (Prometheus 2 × 5Gi, Perses 1Gi, Loki 4 × 10Gi) and about 25 pods in RHOAI's monitoring namespace, measured with RHOAI 3.7. The LokiStack is the test size 1x.demo: one copy of everything, not for production.";

/**
 * GET /api/resources/observability, read again when refreshKey changes,
 * every few seconds while a part settles, and otherwise every
 * IDLE_REFRESH_MS while the browser tab is visible.
 */
export function useObservabilityStatus(refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<ObservabilityStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const latest = useLatestRequest();
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const o = await getObservability(ticket.signal);
      if (!ticket.isLatest()) return;
      setData(o);
      setError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError(toApiError(e, "Cannot read the usage logs and dashboards").message);
    }
  }, [enabled, latest]);
  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);
  const settling = busy || observabilitySettling(data);
  usePolling(fetchNow, {
    enabled,
    restartKey: `${pollRound}|${settling}`,
    delay: ({ elapsedMs }) => (settling && elapsedMs < RESOURCE_SETTLE_MAX_MS ? OBSERVABILITY_POLL_MS : IDLE_REFRESH_MS),
  });
  return { data, error, reload: fetchNow };
}

/** The label of a part's commands: a manual fix outside the docs says so. */
const commandsLabel = (part: ObservabilityPart) => (part.state === "problem" ? "the manual fix (not in RHOAI docs; worked in our test)" : "the commands");

/** One part, with its Docs link and, where it has them, the same change from a terminal. */
const ObservabilityPartItem: React.FC<{ part: ObservabilityPart }> = ({ part }) => (
  <PartItem id={`observability-${part.id}`} title={part.title} scope="Shared by the cluster" unmanaged={!!part.notManaged}
    status={part.state === "waiting" ? "not-set-up" : maasPartLabelStatus(part as unknown as MaaSPart)} statusText={part.state === "waiting" ? "Waiting" : undefined}
    actions={part.docUrl ? <ActionListItem><ExternalLink href={part.docUrl} isInline>Docs</ExternalLink></ActionListItem> : undefined}>
    {part.message && <Content component="p">{part.message}</Content>}
    {part.warning && <WarningAlert text={part.warning} />}
    {part.commands && (part.state === "problem" || part.state === "manual"
      ? <CopyableCode id={`observability-${part.id}-commands`} label={commandsLabel(part)} code={part.commands} />
      : (
        <FeatureSection title="Do it from the CLI instead">
          <CopyableCode id={`observability-${part.id}-commands`} label="the commands" code={part.commands} />
        </FeatureSection>
      ))}
  </PartItem>
);

/** The monitoring namespace's disks: what the stack costs now, and which Clean up deletes. */
const Disks: React.FC<{ o: ObservabilityStatus }> = ({ o }) => {
  const volumes = o.volumes ?? [];
  return (
    <TabSection title="Disks" id="observability-disks">
      <Content component="p">
        {volumes.length > 0
          ? `RHOAI's monitoring namespace ${o.namespace ?? ""} has ${volumes.length} PersistentVolumeClaims, ${o.volumesTotal ?? "of unknown size"} requested in total.`
          : `Nothing yet. Set up adds: ${OBSERVABILITY_COST}`}
      </Content>
      {volumes.length > 0 && (
        <Table aria-label="Disks of RHOAI's monitoring" variant="compact">
          <Thead><Tr><Th>PersistentVolumeClaim</Th><Th modifier="nowrap">Size</Th><Th modifier="nowrap">StorageClass</Th><Th modifier="nowrap">Clean up</Th></Tr></Thead>
          <Tbody>
            {volumes.map((v) => (
              <Tr key={v.name}>
                <Td dataLabel="PersistentVolumeClaim"><code>{v.name}</code></Td>
                <Td dataLabel="Size">{v.size}</Td>
                <Td dataLabel="StorageClass">{v.storageClass ?? "Default"}</Td>
                <Td dataLabel="Clean up">{v.setUpByTool ? <Label color="orange" isCompact>Deleted with its data</Label> : "Kept"}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
    </TabSection>
  );
};

export interface ObservabilityTabProps {
  observability: ObservabilityStatus | null;
  observabilityError: string | null;
  onRetry: () => void;
  state: FeatureState;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
}

/** The Observability tab: RHOAI's monitoring with the MaaS usage logs and the dashboards. */
export const ObservabilityTab: React.FC<ObservabilityTabProps> = ({ observability: o, observabilityError, onRetry, state, baseReason, readOnly, setPending, onSetUp }) => {
  const setupPlan = o?.setupPlan ?? [];
  const setupLabel = o?.managedByTool ? "Finish setting up usage logs" : "Set up usage logs and dashboards";
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Observability" purpose={FEATURE_PURPOSE.observability} state={state} /></StackItem>
      {o?.blocked && <StackItem><ComponentAlert feature="Observability" block={o.blocked} /></StackItem>}
      {o && observabilityError && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="Observability may be out of date" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>
            The parts below are from the last successful read. {observabilityError}
          </Alert>
        </StackItem>
      )}
      {o?.external && (
        <StackItem>
          <Alert variant="info" isInline component="p" title="Usage logs are set up on this cluster, not by this tool">
            This tool shows them as they are and never changes or deletes them.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        {o ? (
          <PartList aria-label="What usage logs and the dashboards need">
            {o.parts.map((p) => <ObservabilityPartItem key={p.id} part={p} />)}
          </PartList>
        ) : readOnly ? (
          <Alert variant="info" isInline component="p" title="Shown to cluster-admins only">
            The usage logs and dashboards setup is read with the updater&apos;s own rights for cluster-admins. Ask a cluster-admin to check it, or open the RHOAI dashboard&apos;s Observe &amp; monitor.
          </Alert>
        ) : observabilityError ? <LoadFailed title="Cannot read the usage logs and dashboards" error={observabilityError} onRetry={onRetry} />
          : <PartsSkeleton aria-label="What usage logs and the dashboards need" what="what usage logs and the dashboards need" count={7} />}
      </StackItem>
      {o && !o.external && !o.blocked && setupPlan.length > 0 && (
        <StackItem>
          <PrimaryAction label={setupLabel} onClick={() => onSetUp([{ kind: "setup-observability" }])}
            reason={readOnly ? READ_ONLY_SETUP : baseReason ?? o.setupBlockedReason ?? null} />
        </StackItem>
      )}
      {o && state.status === "ready" && (
        <StackItem>
          <TabSection title="Use it" id="observability-use">
            <List>
              <ListItem>Call a model through the MaaS gateway: each call is logged from then on, and older calls were not.</ListItem>
              <ListItem>
                In the RHOAI dashboard, Observe & monitor, Dashboard: the Usage tab shows the calls by model, subscription and user.
                It is shown to cluster-admins only: the dashboard offers the admin views to users who may read the platform&apos;s Prometheus, and RHOAI documents no other grant.
              </ListItem>
              <ListItem>Per-user &quot;My Usage&quot; stays empty until MaaS records user IDs (the MaaS tab).</ListItem>
            </List>
            {o.dashboardUrl && <ExternalLink href={o.dashboardUrl}>Open the RHOAI dashboard</ExternalLink>}
          </TabSection>
        </StackItem>
      )}
      {o && <StackItem><Disks o={o} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Only what is missing, as RHOAI&apos;s docs and its own GitOps repository install them: the Cluster Observability Operator, the Red Hat build of OpenTelemetry and the Loki Operator from <code>redhat-operators</code> (default channels), after cert-manager. They stay after Clean up: other features and admins use them</>,
            <>A bucket <code>rhoai-usage-logs-&lt;random suffix&gt;</code> on this tool&apos;s S3 storage, with a SeaweedFS user <code>rnu-usage-logs-&lt;the same suffix&gt;</code> whose key reaches that bucket only (their names are recorded on the DSCInitialization before they are made), and Secret <code>rhoai-nightly-updater-usage-logs</code> in RHOAI&apos;s monitoring namespace with the seven keys the docs list</>,
            <>In the DSCInitialization: <code>spec.monitoring.metrics.storage</code> (5Gi, 90 days, as the docs write it; it also deploys Perses, the dashboards&apos; server) and <code>spec.monitoring.usageLogs</code> (size 1x.demo, on the cluster&apos;s default StorageClass). An annotation records what this tool added; Clean up puts back only that</>,
            <>RHOAI&apos;s operator then deploys Prometheus, Thanos, Perses, the usage-logs LokiStack and its collector. {OBSERVABILITY_COST}</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && o?.managedByTool && (
        <StackItem>
          <CleanupSection id="observability-cleanup" label="Clean up usage logs" onClick={() => setPending({ kind: "teardown-observability" })} reason={baseReason ?? o.cleanupBlockedReason ?? null}
            text="Removes the monitoring fields this tool added to the DSCInitialization (RHOAI then deletes the stack), then the disks and certificates the stack left, the Secret, the SeaweedFS user and the bucket, with every usage log and metric in them. The operators stay: other features use them." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The steps an Observability operation streams, in its order (pkg/cluster observability.Run). */
export function observabilitySteps(p: ObservabilityPending): { id: string; title: string }[] {
  if (p.kind === "teardown-observability") {
    return [
      { id: "observability-check", title: "Check what this tool's setup changed" },
      { id: "monitoring-settings", title: "Put the DSCInitialization back" },
      { id: "monitoring-gc", title: "Wait for RHOAI to delete the stack" },
      { id: "monitoring-leftovers", title: "Delete the disks and certificates the stack left" },
      { id: "usage-logs-storage", title: "Delete the Secret, the SeaweedFS user and the bucket" },
    ];
  }
  return [
    { id: "observability-check", title: "Check what usage logs and the dashboards need" },
    { id: "cert-manager", title: "cert-manager Operator for Red Hat OpenShift" },
    { id: "cluster-observability-operator", title: "Cluster Observability Operator" },
    { id: "opentelemetry", title: "Red Hat build of OpenTelemetry" },
    { id: "loki-operator", title: "Loki Operator" },
    { id: "usage-logs-storage", title: "Bucket, SeaweedFS user and Secret" },
    { id: "monitoring-settings", title: "Monitoring fields in the DSCInitialization" },
    { id: "monitoring-ready", title: "Wait for the monitoring stack and the usage logs" },
  ];
}

/** The confirmation of an Observability change. */
export function observabilityConfirm(p: Pending, o: ObservabilityStatus | null): ConfirmSpec | null {
  if (p.kind === "setup-observability") {
    return {
      title: o?.managedByTool ? "Finish setting up usage logs and dashboards" : "Set up usage logs and dashboards",
      confirm: "Set up",
      summary: <>Sets up RHOAI&apos;s monitoring as its docs do, for the MaaS usage logs and the dashboards. {OBSERVABILITY_COST}</>,
      changes: (o?.setupPlan ?? []).map((line) => <>{line}</>),
      changesOpen: true,
      runTitle: "Set up usage logs and dashboards",
    };
  }
  if (p.kind === "teardown-observability") {
    const disks = (o?.volumes ?? []).filter((v) => v.setUpByTool);
    const gi = disks.reduce((sum, d) => sum + (Number(/^(\d+)Gi$/.exec(d.size)?.[1]) || 0), 0);
    return {
      title: "Clean up usage logs and dashboards?",
      confirm: "Clean up",
      danger: true,
      titleIcon: "danger",
      summary: <>Removes what this tool&apos;s setup added and what the stack left, in that order. Everything else stays.</>,
      changes: (o?.cleanupPlan ?? []).map((line) => <>{line}</>),
      changesOpen: true,
      dataLoss: <>Deletes every usage log and metric: the usage-logs bucket with its objects{disks.length > 0 ? <>, and {disks.length} disks ({gi}Gi, listed under Disks)</> : null}. They cannot be brought back.</>,
      extra: (o?.cleanupKept ?? []).length > 0 ? (
        <Content component="p">Kept: {(o?.cleanupKept ?? []).join(". ")}.</Content>
      ) : undefined,
      runTitle: "Clean up usage logs and dashboards",
    };
  }
  return null;
}
