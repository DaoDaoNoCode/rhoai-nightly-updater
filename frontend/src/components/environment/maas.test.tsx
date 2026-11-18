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
    expect(s.next).toEqual({ kind: "open", label: "Open MaaS", href: "https://rh-ai.apps.example.com/maas" });
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
    expect(s).toMatchObject({ label: "Ready, no model of this tool published", unmanaged: true });
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
    expect(maasSteps({ kind: "publish-maas", project: "p", model: "qwen3-1-7b" }).map((s) => s.id)).toEqual(["check", "gateway", "model", "maas", "ready"]);
    expect(maasSteps({ kind: "unpublish-maas", project: "p", model: "qwen3-1-7b" }).map((s) => s.title)).toContain("Remove qwen3-1-7b from MaaS");
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

  it("Publish says, before you confirm, that the model's direct address answers 404 while it is published", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], {
        platform: { deployed: true, ready: true, managedByTool: true },
        items: [{ namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not published" }],
      }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByText(/its direct address on the Models tab answers 404 until it is unpublished/)).toBeInTheDocument();
    expect(screen.queryByText(/keep working/)).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Publish" }));
    const confirm = await screen.findByRole("dialog", { name: /Publish qwen25-05b-instruct in dev to MaaS\?/ });
    expect(within(confirm).getByText(/its direct address \(the Models tab's commands\) answers 404 until it is unpublished/)).toBeInTheDocument();
    expect(within(confirm).getByText(/routes through the MaaS gateway only/)).toBeInTheDocument();
    expect(within(confirm).queryByText(/keep working/)).toBeNull();
  });

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

  it("publishes the row's model by name; the embedding model says why it is not offered", async () => {
    const reason = "This tool publishes chat models only. Use the embedding model at its own address (the Models tab shows it), or add it to the Gen AI playground.";
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], {
        platform: { deployed: true, ready: true, managedByTool: true },
        items: [
          { namespace: "dev", name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not offered for MaaS", setupBlockedReason: reason },
          { namespace: "dev", name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not published" },
        ],
      }),
      "POST /api/resources/maas/stream": sse({ step: "operation_complete", status: "success", message: "done" }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const granite = (await screen.findByText("Granite Embedding 125M English (CPU)")).closest("tr")!;
    expect(within(granite).getByText(reason)).toBeInTheDocument();
    expect(within(granite).getByRole("button", { name: "Publish" })).toHaveAttribute("aria-disabled", "true");
    const qwen3 = screen.getByText("Qwen3 1.7B (CPU, tool calling)").closest("tr")!;
    fireEvent.click(within(qwen3).getByRole("button", { name: "Publish" }));
    const confirm = await screen.findByRole("dialog", { name: /Publish qwen3-1-7b in dev to MaaS\?/ });
    fireEvent.click(within(confirm).getByRole("button", { name: "Publish" }));
    await screen.findByRole("dialog", { name: "Publish qwen3-1-7b in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "publish", project: "dev", model: "qwen3-1-7b" }]);
  });
});
