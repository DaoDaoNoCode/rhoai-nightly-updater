import { useCallback, useEffect, useState } from "react";
import type { WorkbenchState, WorkbenchStatus } from "../../types";
import { getWorkbench, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { IDLE_REFRESH_MS, RESOURCE_SETTLE_MAX_MS } from "../../constants";
import type { EnvStatus } from "./resourceStatus";
import type { Busy, FeatureState } from "./featureState";

/** Canonical identifiers defined in pkg/cluster/internal/workbench. */
export const WORKBENCH_NAME = "nightly-workbench";
export const WORKBENCH_STORAGE = "nightly-workbench-storage";
/** Where the workbench's storage is mounted: only files there outlive a stop (pkg/cluster/internal/workbench storageMountPath). */
export const WORKBENCH_FILES = "/opt/app-root/src";

/** A hardware profile as the page shows it: "Default (2 CPU, 4Gi memory)". */
export function profileText(name: string | undefined, cpu?: string, memory?: string): string {
  const parts = [cpu && `${cpu} CPU`, memory && `${memory} memory`].filter(Boolean);
  return `${name || "None"}${parts.length ? ` (${parts.join(", ")})` : ""}`;
}

/** A storage as the page shows it: "nightly-workbench-storage, 20Gi, storage class gp3-csi". */
export function storageText(size?: string, storageClass?: string): string {
  return [WORKBENCH_STORAGE, size, storageClass ? `storage class ${storageClass}` : "the cluster's default storage class"].filter(Boolean).join(", ");
}

/** The project's workbench could not be read: its state is unknown. */
export function workbenchUnreadable(wb: WorkbenchStatus | null, project: string): boolean {
  return !!wb?.itemsUnreadable?.includes(project);
}

/** Storage this tool kept from a deleted workbench (no workbench uses it). */
export function keptStorage(wb: WorkbenchStatus | null, project: string) {
  const item = workbenchIn(wb, project);
  return item && !item.deployed && item.storage?.managedByTool ? item.storage : undefined;
}

/** Status payload entry for nightly-workbench—or retained storage—in a given project. */
export function workbenchIn(wb: WorkbenchStatus | null, project: string): WorkbenchState | undefined {
  return (wb?.items ?? []).find((i) => i.namespace === project);
}

/** Display strings keyed by workbench lifecycle state when one is deployed. */
const STATE_WORDS: Record<string, string> = { starting: "Starting", running: "Running", stopping: "Stopping", stopped: "Stopped", failed: "Failed" };

/** Maps workbench to EnvStatus plus UI label ("Running", "Stopped", …); stopped uses the same grey treatment as "Not set up" per dashboard Stopped styling. */
export function workbenchStatusOf(wb: WorkbenchStatus | null, project: string, busy: Busy): { status: EnvStatus; word: string } {
  const op = busy.workbench?.project === project ? busy.workbench.action : null;
  const item = workbenchIn(wb, project);
  if (op === "teardown") return { status: "deleting", word: "Deleting" };
  if (op === "setup") return { status: "setting-up", word: "Setting up" };
  if (op === "start") return { status: "setting-up", word: "Starting" };
  if (op === "stop") return { status: "setting-up", word: "Stopping" };
  if (op === "delete-storage") return { status: "deleting", word: "Deleting its storage" };
  if (workbenchUnreadable(wb, project) && !item?.deployed) return { status: "unknown", word: "Unknown" };
  if (!item?.deployed) return { status: "not-set-up", word: "Not set up" };
  if (item.terminating) return { status: "deleting", word: "Deleting" };
  switch (item.state) {
    case "running": return { status: "ready", word: STATE_WORDS.running };
    case "failed": return { status: "error", word: STATE_WORDS.failed };
    case "stopped": return { status: "not-set-up", word: STATE_WORDS.stopped };
    case "starting":
    case "stopping": return { status: "setting-up", word: STATE_WORDS[item.state] };
    default: return { status: "unknown", word: "Unknown" };
  }
}

/** Blocker message when setup is unavailable in this project (cluster, platform, plan), otherwise null. */
export function workbenchSetUpReason(wb: WorkbenchStatus | null, project: string): string | null {
  if (!wb || wb.project !== project) return "The workbench's state is still loading.";
  if (workbenchUnreadable(wb, project)) return "The workbench in this project cannot be read now.";
  if (wb.blocked) return wb.blocked.reason;
  if (wb.unsupported) return wb.unsupported;
  if (wb.planProblem) return wb.planProblem;
  return null;
}

function inProject(wb: WorkbenchStatus | null, project: string, busy: Busy): FeatureState {
  const item = workbenchIn(wb, project);
  const { status, word } = workbenchStatusOf(wb, project, busy);
  const unmanaged = !!item?.deployed && item.managedByTool === false;
  const own = unmanaged ? { unmanaged: true as const } : {};
  if (status === "unknown" && workbenchUnreadable(wb, project)) return { status, label: `Unknown in ${project}`, project, next: null };
  if (status === "not-set-up" && !item?.deployed) {
    return { status, label: `Not set up in ${project}`, project, next: { kind: "set-up", label: `Set up a workbench in ${project}`, setup: [{ kind: "setup-workbench", project }], reason: workbenchSetUpReason(wb, project) } };
  }
  if (status === "ready" && item?.url) return { status, label: `${word} in ${project}`, ...own, project, next: { kind: "open", label: "Open workbench", href: item.url } };
  if (status === "error") return { status, label: `${word} in ${project}`, ...own, project, next: { kind: "show-issue", part: "workbench" } };
  return { status, label: `${word} in ${project}`, ...own, project, next: null };
}

/**
 * Workbench feature state for a single project tab, or aggregated across every
 * project that hosts this tool's workbench on Overview. wb carries GET
 * /api/resources/workbench payload; null until the first fetch completes.
 */
export function workbenchState(wb: WorkbenchStatus | null, project: string | null, busy: Busy): FeatureState {
  if (!wb) return { status: "unknown", label: "Loading", next: null, project, loading: true };
  if (project) return inProject(wb, project, busy);
  const projects = (wb.items ?? []).filter((i) => i.deployed && i.managedByTool).map((i) => i.namespace!).filter(Boolean);
  if (busy.workbench && !projects.includes(busy.workbench.project)) projects.push(busy.workbench.project);
  // A project whose workbench cannot be read is shown as unknown, never as without one.
  for (const p of wb.itemsUnreadable ?? []) if (!projects.includes(p)) projects.push(p);
  if (projects.length === 0) return { status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" }, project: null };
  const states = projects.map((p) => inProject(wb, p, busy));
  if (states.length === 1) return states[0];
  const problem = states.find((s) => s.status === "error");
  const unknown = states.filter((s) => s.status === "unknown");
  if (problem) return { status: "error", label: `Failed in ${problem.project}`, next: { kind: "show-issue", part: "workbench", project: problem.project ?? undefined }, project: problem.project, projects: states };
  const running = states.filter((s) => s.status === "ready").length;
  if (unknown.length > 0 && running === 0) return { status: "unknown", label: unknown.length === states.length ? `Unknown in ${unknown.length} projects` : `${running} of ${states.length} running, ${unknown.length} unknown`, next: null, project: null, projects: states };
  return { status: running > 0 ? "ready" : states[0].status, label: `${running} of ${states.length} running${unknown.length ? `, ${unknown.length} unknown` : ""}`, next: null, project: null, projects: states };
}

/** Initial tab project from the URL when present, otherwise the first namespace with a managed workbench. */
export function defaultWorkbenchProject(wb: WorkbenchStatus | null): string | null {
  return (wb?.items ?? []).find((i) => i.deployed && i.managedByTool)?.namespace ?? null;
}

const WB_POLL_MS = 5_000;

/** True while any workbench is mid-transition—start, stop, or teardown—without user action pending. */
export function workbenchSettling(wb: WorkbenchStatus | null): boolean {
  return (wb?.items ?? []).some((i) => i.terminating || (i.deployed && (i.state === "starting" || i.state === "stopping")));
}

/**
 * Fetches and holds workbench status for project scope; refetches on refreshKey
 * and polls on an interval whenever settle transitions are in flight or busy
 * indicates an in-progress workbench operation.
 */
export function useWorkbenchStatus(project: string | null, refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<WorkbenchStatus | null>(null);
  const [error, setError] = useState<{ project: string | null; message: string } | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const latest = useLatestRequest();
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    const ticket = latest.start();
    try {
      const st = await getWorkbench(project, ticket.signal);
      if (!ticket.isLatest()) return;
      setData(st);
      setError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      setError({ project, message: toApiError(e, "Cannot read the workbench").message });
    }
  }, [project, enabled, latest]);
  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);
  // Quick while a workbench starts or stops, else a slow idle re-read (as
  // MaaS's); usePolling skips hidden browser tabs and reads at once when
  // the tab is shown again.
  const settling = busy || workbenchSettling(data);
  usePolling(fetchNow, {
    enabled,
    restartKey: `${pollRound}|${settling}`,
    delay: ({ elapsedMs }) => (settling && elapsedMs < RESOURCE_SETTLE_MAX_MS ? WB_POLL_MS : IDLE_REFRESH_MS),
  });
  return { data, error: error?.project === project ? error.message : null, reload: fetchNow };
}
