import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { MaaSStatus } from "../../types";
import { IDLE, maasState } from "./featureState";
import { maasCurl, maasPublication, maasSettling, maasSteps } from "./MaaSTab";
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

  it("once MaaS is on, a manual part (Authorino TLS) needs attention and points at it", () => {
    const s = maasState(base([{ id: "database", title: "Database", state: "ready" }, { id: "authorino-tls", title: "Authorino TLS", state: "manual" }], { platform: { deployed: true, ready: false, managedByTool: true } }), IDLE);
    expect(s.status).toBe("needs-attention");
    expect(s.next).toEqual({ kind: "show-issue", part: "maas-authorino-tls" });
  });

  it("before MaaS is on, manual and waiting parts do not hide Set up: only the platform's block does", () => {
    const parts: MaaSStatus["parts"] = [
      { id: "dsc", title: "DSC", state: "missing" },
      { id: "kuadrant", title: "Kuadrant", state: "waiting" },
      { id: "authorino-tls", title: "Authorino TLS", state: "manual" },
      { id: "maas-api", title: "maas-api", state: "missing" },
    ];
    const s = maasState(base(parts), IDLE);
    expect(s.status).toBe("not-set-up");
    expect(s.next).toEqual({ kind: "set-up", label: "Set up MaaS", setup: [{ kind: "setup-maas" }], reason: null });
  });

  it("ready: counts the published models and opens the dashboard's MaaS pages", () => {
    const s = maasState(base([{ id: "dsc", title: "DSC", state: "ready" }], {
      dashboardUrl: "https://rh-ai.apps.example.com", platform: { deployed: true, ready: true, managedByTool: true },
      items: [{ namespace: "p", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true }],
    }), IDLE);
    expect(s.label).toBe("Ready, 1 model published");
    expect(s.next).toEqual({ kind: "open", label: "Open MaaS in the dashboard", href: "https://rh-ai.apps.example.com/maas" });
  });

  it("a publication left half-way says what is left, is not counted, and does not keep the tab polling", () => {
    const partly = { namespace: "old", deployed: true, ready: false, managedByTool: true, modelReady: false, published: true, partly: true,
      message: "Partly published: the model is gone; this tool's record of the publication still in place. Unpublish to finish removing it." };
    const m = base([{ id: "dsc", title: "DSC", state: "ready" }], { platform: { deployed: true, ready: true, managedByTool: true }, items: [partly] });
    expect(maasState(m, IDLE).label).toBe("Ready, no model of this tool published");
    expect(maasSettling(m)).toBe(false);
    expect(maasPublication(partly)).toBe(partly.message);
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

  it("Clean up from the Models tab offers the publication and blocks deleting a published model without it", () => {
    const withModel = { ...status, models: { items: [{ namespace: "p", name: "m", deployed: true, ready: true, managedByTool: true }] } } as unknown as ResourcesStatus;
    const c = cleanupCandidates(withModel, "models", false, null, maas);
    expect(c.maasPublished).toEqual(["p"]);
    expect(cleanupBlocked(withModel, { models: ["p"] }, c.maasPublished)).toContain("published to MaaS");
  });

  it("a published model cannot go without its publication", () => {
    expect(cleanupBlocked(status, { models: ["p"] }, ["p"])).toContain("published to MaaS");
    expect(cleanupBlocked(status, { models: ["p"], maasPublished: ["p"] }, ["p"])).toBeNull();
  });
});

describe("MaaS run dialog", () => {
  const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
  const none = { deployed: false, ready: false, managedByTool: false };
  const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };

  it("after setting up the platform, says what to do with MaaS (live: it showed the Pipelines tab's next step)", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }, { id: "database", title: "Database", state: "missing" }]),
      "POST /api/resources/maas/stream": sse(
        { step: "check", status: "success", message: "Everything the platform needs is there." },
        { step: "operation_complete", status: "success", message: "The MaaS platform is ready." },
      ),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    const confirm = await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ });
    fireEvent.click(within(confirm).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    expect(await within(run).findByText(/publish a model to offer it through MaaS/)).toBeInTheDocument();
    expect(within(run).queryByText(/import a pipeline/)).toBeNull();
    expect(within(run).queryByRole("link", { name: /Open pipelines/ })).toBeNull();
  });
});
