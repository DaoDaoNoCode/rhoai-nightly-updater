import { describe, expect, it } from "vitest";
import type { DeploymentInfo, PodInfo } from "../types";
import { ageSeconds, deploymentHealth, filterDeployments, sortDeployments } from "./deploymentHealth";

function pod(over: Partial<PodInfo> = {}): PodInfo {
  return { name: "p", namespace: "ns", phase: "Running", node: "n1", ready: true, restarts: 0, image: "", imageID: "", age: "2d", ...over };
}

function dep(name: string, over: Partial<DeploymentInfo> = {}): DeploymentInfo {
  return {
    name, namespace: "redhat-ods-applications", ready: 1, desired: 1, available: 1, image: `quay.io/x/${name}:1`,
    unavailableReplicas: 0, updatedReplicas: 1, rolloutStuck: false, pods: [pod({ name: `${name}-1` })], ...over,
  };
}

describe("ageSeconds", () => {
  it.each([["45s", 45], ["12m", 720], ["3h", 10800], ["2d", 172800], ["unknown", NaN], [undefined, NaN]])("%s", (age, s) => {
    expect(ageSeconds(age)).toBe(s);
  });
});

describe("deploymentHealth (A08-14)", () => {
  it("healthy, scaled down", () => {
    expect(deploymentHealth(dep("a")).kind).toBe("healthy");
    expect(deploymentHealth(dep("a", { desired: 0, ready: 0, pods: [] })).kind).toBe("scaled-down");
  });

  it("a pod stuck in ContainerCreating for days is a problem and says how long (live odh-observability)", () => {
    const h = deploymentHealth(dep("odh-observability", {
      ready: 0, rolloutStuck: true,
      pods: [pod({ phase: "Pending", ready: false, containers: [{ name: "manager", ready: false, restarts: 0, state: "waiting", reason: "ContainerCreating" }] })],
    }));
    expect(h.kind).toBe("problem");
    expect(h.readyText).toBe("0/1 ready");
    expect(h.reason).toBe("ContainerCreating for 2d");
  });

  it("a crash-looping container is a problem even when the replica count looks fine", () => {
    const h = deploymentHealth(dep("trustyai", {
      pods: [pod(), pod({ name: "b", ready: false, containers: [{ name: "manager", ready: false, restarts: 534, state: "waiting", reason: "CrashLoopBackOff" }] })],
    }));
    expect(h.kind).toBe("problem");
    expect(h.reason).toBe("CrashLoopBackOff (534 restarts)");
  });

  it("a young pod that is still starting is progressing, not a problem", () => {
    const h = deploymentHealth(dep("new", { ready: 0, pods: [pod({ phase: "Pending", ready: false, age: "40s" })] }));
    expect(h.kind).toBe("progressing");
  });

  it("an Unschedulable pod is a problem at once", () => {
    const h = deploymentHealth(dep("x", { pods: [pod(), pod({ name: "y", phase: "Pending", ready: false, age: "20s", schedulingReason: "Unschedulable" })] }));
    expect(h).toMatchObject({ kind: "problem", reason: "Unschedulable" });
  });

  it("completed pods (Succeeded) do not count (live maas-ui)", () => {
    const h = deploymentHealth(dep("maas-ui", { pods: [pod(), pod({ name: "old", phase: "Succeeded", ready: false, containers: [{ name: "c", ready: false, restarts: 0, state: "terminated", reason: "Completed" }] })] }));
    expect(h.kind).toBe("healthy");
  });
});

describe("sortDeployments and filterDeployments", () => {
  const broken = dep("zeta-broken", { ready: 0, rolloutStuck: true, pods: [pod({ ready: false, phase: "Pending" })], buildDate: "2026-10-01T00:00:00Z" });
  const ok1 = dep("alpha", { buildDate: "2026-10-03T00:00:00Z", version: "v3.6.0" });
  const ok2 = dep("beta", { image: "registry.redhat.io/rhoai/odh-beta@sha256:abc" });
  const all = [ok2, ok1, broken];

  it("status ascending puts problems first, then by name", () => {
    expect(sortDeployments(all, "status", "asc").map((d) => d.name)).toEqual(["zeta-broken", "alpha", "beta"]);
    expect(sortDeployments(all, "status", "desc").map((d) => d.name)).toEqual(["alpha", "beta", "zeta-broken"]);
  });

  it("name and build date sorts; unknown dates last", () => {
    expect(sortDeployments(all, "name", "desc").map((d) => d.name)).toEqual(["zeta-broken", "beta", "alpha"]);
    expect(sortDeployments(all, "built", "desc").map((d) => d.name)).toEqual(["alpha", "zeta-broken", "beta"]);
    expect(sortDeployments(all, "built", "asc").map((d) => d.name)).toEqual(["zeta-broken", "alpha", "beta"]);
  });

  it("filters by text in name, image or version, and by status", () => {
    expect(filterDeployments(all, "odh-beta", "all").map((d) => d.name)).toEqual(["beta"]);
    expect(filterDeployments(all, "V3.6", "all").map((d) => d.name)).toEqual(["alpha"]);
    expect(filterDeployments(all, "", "attention").map((d) => d.name)).toEqual(["zeta-broken"]);
    expect(filterDeployments(all, "", "healthy").map((d) => d.name)).toEqual(["beta", "alpha"]);
  });
});
