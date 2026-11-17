import { describe, expect, it } from "vitest";
import type { MaaSStatus } from "../../types";
import { IDLE, maasState } from "./featureState";
import { maasCurl, maasSteps } from "./MaaSTab";
import { cleanupBlocked, cleanupCandidates, cleanupSteps } from "./CleanupModal";
import type { ResourcesStatus } from "../../types";

const base = (parts: MaaSStatus["parts"], extra: Partial<MaaSStatus> = {}): MaaSStatus => ({
  platform: { deployed: false, ready: false, managedByTool: false },
  parts, subscription: "rhoai-nightly-updater-subscription", ...extra,
});

describe("maasState", () => {
  it("offers Set up while parts are missing, with the platform's block as the reason", () => {
    const s = maasState(base([{ id: "database", title: "Database", state: "missing" }], { platform: { deployed: false, ready: false, managedByTool: false, setupBlockedReason: "No Kuadrant" } }), IDLE);
    expect(s.status).toBe("not-set-up");
    expect(s.next).toEqual({ kind: "set-up", label: "Set up MaaS", setup: [{ kind: "setup-maas" }], reason: "No Kuadrant" });
  });

  it("a manual part (Authorino TLS) needs attention and points at it", () => {
    const s = maasState(base([{ id: "database", title: "Database", state: "ready" }, { id: "authorino-tls", title: "Authorino TLS", state: "manual" }]), IDLE);
    expect(s.status).toBe("needs-attention");
    expect(s.next).toEqual({ kind: "show-issue", part: "maas-authorino-tls" });
  });

  it("ready: counts the published models and opens the dashboard's MaaS pages", () => {
    const s = maasState(base([{ id: "dsc", title: "DSC", state: "ready" }], {
      dashboardUrl: "https://rh-ai.apps.example.com", platform: { deployed: true, ready: true, managedByTool: true },
      items: [{ namespace: "p", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true }],
    }), IDLE);
    expect(s.label).toBe("Ready, 1 model published");
    expect(s.next).toEqual({ kind: "open", label: "Open MaaS in the dashboard", href: "https://rh-ai.apps.example.com/maas" });
  });

  it("someone else's MaaS is labelled not managed", () => {
    const s = maasState(base([{ id: "dsc", title: "DSC", state: "ready", notManaged: true }], { platform: { deployed: true, ready: true, managedByTool: false } }), IDLE);
    expect(s.label).toContain("not managed by this tool");
  });
});

describe("MaaS use and steps", () => {
  it("the docs' key, models and chat requests with the user's token", () => {
    const c = maasCurl("https://maas.apps.example.com", "rhoai-nightly-updater-subscription");
    expect(c).toContain("$(oc whoami -t)");
    expect(c).toContain('"subscription":"rhoai-nightly-updater-subscription"');
    expect(c).toContain("$HOST/v1/chat/completions");
  });

  it("the steps match the backend's order", () => {
    expect(maasSteps({ kind: "setup-maas" }).map((s) => s.id)).toEqual(["check", "database", "gateway", "authorino-tls", "dsc", "ready"]);
    expect(maasSteps({ kind: "teardown-maas" }).map((s) => s.id)).toEqual(["check", "unpublish", "dsc", "gateway", "database"]);
    expect(maasSteps({ kind: "publish-maas", project: "p" }).map((s) => s.id)).toEqual(["check", "gateway", "model", "maas", "ready"]);
    expect(maasSteps({ kind: "unpublish-maas", project: "p" }).map((s) => s.id)).toEqual(["check", "maas", "model", "gateway"]);
  });
});

describe("Clean up everything with MaaS", () => {
  const maas = base([{ id: "dsc", title: "DSC", state: "ready" }], {
    platform: { deployed: true, ready: true, managedByTool: true },
    items: [{ namespace: "p", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true }],
  });
  const status = { minio: { deployed: false, ready: false, managedByTool: false }, mlflow: { deployed: false, ready: false, managedByTool: false }, pipelineServers: [] } as unknown as ResourcesStatus;

  it("offers the publications and the platform, unpublishing first and removing the platform last", () => {
    const c = cleanupCandidates(status, "all", false, null, maas);
    expect(c.maasPublished).toEqual(["p"]);
    expect(c.maasPlatform).toBe(true);
    expect(cleanupSteps({ maasPublished: ["p"], models: ["p"], maasPlatform: true }).map((s) => s.id)).toEqual(["maas-unpublish:p", "model:p", "maas-platform"]);
    expect(cleanupCandidates(status, "models", false, null, maas).maasPlatform).toBe(false);
  });

  it("a published model cannot go without its publication", () => {
    expect(cleanupBlocked(status, { models: ["p"] }, ["p"])).toContain("published to MaaS");
    expect(cleanupBlocked(status, { models: ["p"], maasPublished: ["p"] }, ["p"])).toBeNull();
  });
});
