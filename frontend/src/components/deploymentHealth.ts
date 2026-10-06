import type { DeploymentInfo, PodInfo } from "../types";

/** Pods that finished (Completed, Evicted) are not part of the running workload. */
export function activePods(dep: Pick<DeploymentInfo, "pods">): PodInfo[] {
  return (dep.pods ?? []).filter((p) => p.phase !== "Succeeded" && p.phase !== "Failed");
}

export type HealthKind = "problem" | "progressing" | "scaled-down" | "healthy";

export interface DeploymentHealth {
  kind: HealthKind;
  /** Short ready text, for example "1/1" or "0/1 ready". */
  readyText: string;
  /** The most specific cause known for a problem, for example "CrashLoopBackOff (534 restarts)". */
  reason?: string;
}

/** Waiting reasons that do not clear on their own (Kubernetes "Debug Pods"). */
const STUCK_REASONS = new Set([
  "CrashLoopBackOff",
  "ImagePullBackOff",
  "ErrImagePull",
  "InvalidImageName",
  "CreateContainerConfigError",
  "CreateContainerError",
  "RunContainerError",
]);

function podCause(pods: PodInfo[]): string | undefined {
  for (const pod of pods) {
    if (pod.ready) continue;
    const since = pod.age && ageSeconds(pod.age) >= NOT_READY_GRACE_SECONDS ? ` for ${pod.age}` : "";
    if (pod.schedulingReason) return `${pod.schedulingReason}${since}`;
    for (const c of pod.containers ?? []) {
      if (!c.ready && c.reason && c.reason !== "Completed") {
        return c.restarts > 0 ? `${c.reason} (${c.restarts} restart${c.restarts === 1 ? "" : "s"})` : `${c.reason}${since}`;
      }
    }
    if (pod.phase === "Pending") return `Pending${since}`;
  }
  return undefined;
}

/** Seconds in a backend age string ("45s", "12m", "3h", "2d"); NaN when unknown. */
export function ageSeconds(age: string | undefined): number {
  const m = /^(\d+)([smhd])$/.exec(age ?? "");
  if (!m) return NaN;
  return Number(m[1]) * { s: 1, m: 60, h: 3600, d: 86400 }[m[2] as "s" | "m" | "h" | "d"];
}

/**
 * A pod that is still not ready after this long is stuck, not starting. It
 * matches the Deployment default progressDeadlineSeconds (600 s).
 */
export const NOT_READY_GRACE_SECONDS = 600;

/**
 * Health of one Deployment from its replica counts and pods. It is a problem
 * when a pod is in a state that does not clear on its own, cannot be
 * scheduled, or has not become ready within the grace period, or when the
 * server reports the rollout stuck. Not-ready pods younger than that are
 * "progressing".
 */
export function deploymentHealth(dep: DeploymentInfo): DeploymentHealth {
  if (dep.desired === 0) return { kind: "scaled-down", readyText: "Scaled down" };
  const pods = activePods(dep);
  const readyText = dep.ready >= dep.desired ? `${dep.ready}/${dep.desired}` : `${dep.ready}/${dep.desired} ready`;
  const cause = podCause(pods);
  const notReady = pods.filter((p) => !p.ready);
  const stuckPod = notReady.some((p) =>
    !!p.schedulingReason || (p.containers ?? []).some((c) => !c.ready && !!c.reason && STUCK_REASONS.has(c.reason)));
  const oldNotReady = notReady.some((p) => ageSeconds(p.age) >= NOT_READY_GRACE_SECONDS);
  if (dep.rolloutStuck || stuckPod) {
    return { kind: "problem", readyText, reason: cause ?? dep.rolloutMessage ?? "Rollout stuck" };
  }
  if (dep.ready < dep.desired) {
    const young = notReady.length > 0 && !oldNotReady;
    return young
      ? { kind: "progressing", readyText, reason: cause }
      : { kind: "problem", readyText, reason: cause ?? dep.rolloutMessage };
  }
  if (notReady.length > 0) {
    return oldNotReady
      ? { kind: "problem", readyText, reason: cause }
      : { kind: "progressing", readyText, reason: cause };
  }
  return { kind: "healthy", readyText };
}

const KIND_RANK: Record<HealthKind, number> = { problem: 0, progressing: 1, "scaled-down": 2, healthy: 3 };

export type DeploymentSortKey = "status" | "name" | "built";
export type SortDirection = "asc" | "desc";

/** Sort a copy. "status" ascending puts problems first, then by name. */
export function sortDeployments(deps: DeploymentInfo[], key: DeploymentSortKey, direction: SortDirection): DeploymentInfo[] {
  const sign = direction === "asc" ? 1 : -1;
  const byName = (a: DeploymentInfo, b: DeploymentInfo) => a.name.localeCompare(b.name) || a.namespace.localeCompare(b.namespace);
  return [...deps].sort((a, b) => {
    if (key === "status") {
      const diff = KIND_RANK[deploymentHealth(a).kind] - KIND_RANK[deploymentHealth(b).kind];
      return diff !== 0 ? sign * diff : byName(a, b);
    }
    if (key === "built") {
      const ta = a.buildDate ? Date.parse(a.buildDate) : NaN;
      const tb = b.buildDate ? Date.parse(b.buildDate) : NaN;
      // Unknown dates always sort last.
      if (Number.isNaN(ta) && Number.isNaN(tb)) return byName(a, b);
      if (Number.isNaN(ta)) return 1;
      if (Number.isNaN(tb)) return -1;
      return ta !== tb ? sign * (ta - tb) : byName(a, b);
    }
    return sign * byName(a, b);
  });
}

export type StatusFilter = "all" | "attention" | "healthy" | "changed";

/** Text filter (name, namespace, image, version, commit) plus a status filter. */
export function filterDeployments(deps: DeploymentInfo[], text: string, status: StatusFilter): DeploymentInfo[] {
  const needle = text.trim().toLowerCase();
  return deps.filter((dep) => {
    const kind = deploymentHealth(dep).kind;
    if (status === "attention" && kind !== "problem" && kind !== "progressing") return false;
    if (status === "healthy" && kind !== "healthy") return false;
    if (status === "changed" && !dep.changeStatus) return false;
    if (!needle) return true;
    return [dep.name, dep.namespace, dep.image, dep.version, dep.gitCommit]
      .some((v) => !!v && v.toLowerCase().includes(needle));
  });
}
