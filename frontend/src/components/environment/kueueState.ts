import { useCallback, useEffect, useState } from "react";
import type { KueueSample, KueueStatus } from "../../types";
import { getKueue, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { IDLE_REFRESH_MS, RESOURCE_SETTLE_MAX_MS } from "../../constants";
import type { EnvStatus } from "./resourceStatus";
import type { Busy, FeatureState } from "./featureState";
import type { Pending } from "./resourceRow";

/** Constants mirrored from pkg/cluster/internal/kueue. */
export const KUEUE_LOCAL_QUEUE = "nightly-kueue";
export const KUEUE_CLUSTER_QUEUE = "nightly-kueue";
export const KUEUE_FLAVOR = "nightly-kueue-cpu";
export const KUEUE_PROFILE = "nightly-kueue-cpu";
export const KUEUE_DEFAULT_PROJECT = "nightly-kueue";
export const KUEUE_DEFAULT_IMAGE = "quay.io/modh/ray:2.47.1-py311-cu121";
export const KUEUE_LABEL = "kueue.openshift.io/managed";
export const KUEUE_POLL_MS = 5_000;

/** Identifies a Distributed workloads action still running. */
export interface KueueOp {
  action: "setup" | "teardown" | "delete-sample" | "optin" | "optout";
  project: string | null;
}

/** All queue resources owned by the tool exist (project LocalQueue included). */
export function kueueSetUp(k: KueueStatus | null): boolean {
  return !!k && k.objects.length === 3 && k.objects.every((o) => o.present && o.managedByTool);
}

/** Any tool-managed Kueue artifact is present (Clean up becomes available). */
export function kueueAnything(k: KueueStatus | null): boolean {
  return !!k && (k.objects.some((o) => o.managedByTool) || k.samples.some((s) => s.managedByTool) || !!k.label?.addedByTool || !!k.optIn.on);
}

/** Derives the EnvStatus a sample should show on the page. */
export function sampleStatus(s: KueueSample | undefined): EnvStatus {
  if (!s?.present) return "not-set-up";
  if (s.terminating) return "deleting";
  switch (s.state) {
    case "Running":
    case "Admitted":
    case "Complete":
      return "ready";
    case "Failed":
    case "Evicted":
    case "Preempted":
      return "error";
    case "Inadmissible":
      return "needs-attention";
    default:
      return "setting-up";
  }
}

/** EnvStatus for the queues portion only. */
export function queuesStatus(k: KueueStatus | null, busy: Busy): EnvStatus {
  if (busy.kueue?.action === "teardown") return "deleting";
  if (busy.kueue?.action === "setup") return "setting-up";
  if (!k) return "unknown";
  if (k.objects.some((o) => o.terminating)) return "deleting";
  if (!kueueSetUp(k)) return k.objects.some((o) => o.present) ? "needs-attention" : "not-set-up";
  if (k.objects.some((o) => o.kind !== "ResourceFlavor" && !o.active)) return "setting-up";
  return "ready";
}

/** Link to the project's Distributed workloads screen in the dashboard. */
export function kueueDashboardHref(k: KueueStatus | null): string | undefined {
  const base = k?.dashboardUrl;
  if (!base?.startsWith("https://") || !k?.project) return undefined;
  return `${base.replace(/\/$/, "")}/observe-monitor/workload-metrics/workload-status/${encodeURIComponent(k.project)}`;
}

/** Link to Observe & monitor → Infrastructure in the dashboard. */
export function kueueInfrastructureHref(k: KueueStatus | null): string | undefined {
  const base = k?.dashboardUrl;
  return base?.startsWith("https://") ? `${base.replace(/\/$/, "")}/observe-and-monitor/infrastructure` : undefined;
}

/** Setup Pending the UI kicks off for a project (RayCluster sample on, RayJob off). */
export const kueueSetupPending = (project: string, k: KueueStatus | null): Pending => ({
  kind: "setup-kueue", project, rayCluster: true, rayJob: false, rayImage: k?.defaultRayImage || KUEUE_DEFAULT_IMAGE,
});

/** FeatureState for Distributed workloads on the tab and Overview. */
export function kueueState(k: KueueStatus | null, project: string | null, busy: Busy): FeatureState {
  if (!k && !busy.kueue) return { status: "unknown", label: "Loading", next: null, project, loading: true };
  const where = k?.project ?? project;
  const s = queuesStatus(k, busy);
  const target = where ? ` in ${where}` : "";
  switch (s) {
    case "ready": {
      const href = kueueDashboardHref(k);
      const samples = (k?.samples ?? []).filter((x) => x.present).length;
      return { status: s, label: `Ready${target}${samples ? `, ${samples === 1 ? "1 sample" : `${samples} samples`}` : ""}`, next: href ? { kind: "open", label: "Open distributed workloads", href } : null, project: where };
    }
    case "setting-up":
    case "deleting":
      return { status: s, label: s === "deleting" ? "Deleting" : "Setting up", next: null, project: where };
    case "not-set-up":
      if (!where) return { status: s, label: "Not set up", next: { kind: "choose-project" }, project: null };
      return { status: s, label: "Not set up", next: { kind: "set-up", label: "Set up distributed workloads", setup: [kueueSetupPending(where, k)], reason: k?.refusal ?? null }, project: where };
    case "needs-attention":
      return { status: s, label: `Incomplete${target}`, next: { kind: "show-issue", part: "kueue-queues", ...(where ? { project: where } : {}) }, project: where };
    default:
      return { status: s, label: "Unknown", next: { kind: "show-issue", part: "kueue-queues" }, project: where };
  }
}

/** While settling, cluster objects may still update without further input. */
export function kueueSettling(k: KueueStatus | null): boolean {
  if (!k) return false;
  return k.objects.some((o) => o.terminating || (o.present && !o.active && o.kind !== "ResourceFlavor")) || k.samples.some((s) => s.terminating || (s.present && sampleStatus(s) === "setting-up"));
}

/**
 * Fetches GET /api/resources/kueue (?project=) and reloads when refreshKey or
 * project changes; polls faster during settlement, on a slower cadence when idle.
 */
export function useKueueStatus(project: string | null, refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<KueueStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const latest = useLatestRequest();
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const k = await getKueue(project, ticket.signal);
      if (!ticket.isLatest()) return;
      setData(k);
      setError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError(toApiError(e, "Cannot read distributed workloads").message);
    }
  }, [enabled, latest, project]);
  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);
  usePolling(fetchNow, {
    enabled: enabled && (busy || kueueSettling(data)),
    restartKey: pollRound,
    delay: ({ elapsedMs }) => (elapsedMs >= RESOURCE_SETTLE_MAX_MS ? null : KUEUE_POLL_MS),
  });
  usePolling(fetchNow, { enabled: enabled && !busy && !kueueSettling(data), delay: IDLE_REFRESH_MS, pauseWhenHidden: true, refreshOnVisible: true });
  return { data, error, reload: fetchNow };
}