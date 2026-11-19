import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { MaaSStatus } from "../../types";
import { IDLE, maasState } from "./featureState";
import { maasConfirm, maasCurl, maasPublication, maasSettling, maasSteps } from "./MaaSTab";
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
    expect(s.next).toEqual({ kind: "open", label: "Open MaaS", href: "https://rh-ai.apps.example.com/maas/maas-governance" });
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
    // The address is single-quoted: double quotes would still run $().
    expect(c).toContain("HOST='https://maas.apps.example.com'\n");
    expect(maasCurl("https://maas.example.com$(id)", "s")).toContain("HOST='https://maas.example.com$(id)'\n");
  });

  it("the steps match the backend's order", () => {
    expect(maasSteps({ kind: "setup-maas" }).map((s) => s.id)).toEqual(["check", "rhcl", "kuadrant", "cnpg", "database", "gateway", "authorino-tls", "dsc", "ready"]);
    expect(maasSteps({ kind: "maas-action", action: "upgrade-rhcl" }).map((s) => s.id)).toEqual(["check", "upgrade", "authorino-tls", "ready"]);
    expect(maasSteps({ kind: "maas-action", action: "rhcl-memory" }).map((s) => s.id)).toEqual(["check", "memory", "ready"]);
    expect(maasSteps({ kind: "maas-action", action: "authorino-tls" }).map((s) => s.id)).toEqual(["check", "authorino-tls"]);
    expect(maasSteps({ kind: "maas-action", action: "rhcl-deny-on-failure" }).map((s) => s.id)).toEqual(["check", "env", "ready"]);
    expect(maasSteps({ kind: "maas-action", action: "remove-tier-policies" }).map((s) => s.id)).toEqual(["check", "delete"]);
    expect(maasSteps({ kind: "teardown-maas" }).map((s) => s.id)).toEqual(["check", "unpublish", "dsc", "gateway", "database"]);
    expect(maasSteps({ kind: "publish-maas", project: "p", model: "qwen3-1-7b" }).map((s) => s.id)).toEqual(["check", "gateway", "model", "maas", "ready"]);
    expect(maasSteps({ kind: "unpublish-maas", project: "p", model: "qwen3-1-7b" }).map((s) => s.title)).toContain("Remove the model from MaaS");
  });
});

describe("Unpublish of a model set up through MaaS only", () => {
  const item = { namespace: "p", name: "qwen3-1-7b", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true };
  const confirmOf = (maasOnlySetup: boolean) => {
    const m = base([{ id: "dsc", title: "DSC", state: "ready" }], { platform: { deployed: true, ready: true, managedByTool: true }, items: [{ ...item, maasOnlySetup }] });
    return maasConfirm({ kind: "unpublish-maas", project: "p", model: "qwen3-1-7b" }, m)!;
  };

  it("says the model is left with no route, not that its direct address answers again", () => {
    render(<>{confirmOf(true).changes.map((c, i) => <div key={i}>{c}</div>)}</>);
    expect(screen.getByText(/so its route is removed: it then answers only at its in-cluster address/)).toBeInTheDocument();
    expect(screen.queryByText(/its direct address answers again/)).not.toBeInTheDocument();
  });

  it("a model published on the MaaS tab goes back to the gateways it used before", () => {
    render(<>{confirmOf(false).changes.map((c, i) => <div key={i}>{c}</div>)}</>);
    expect(screen.getByText(/its direct address answers again/)).toBeInTheDocument();
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
    expect(await screen.findByText(/While published, the model.s direct address answers 404/)).toBeInTheDocument();
    expect(screen.queryByText(/keep working/)).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Publish" }));
    const confirm = await screen.findByRole("dialog", { name: /Publish Qwen2.5 0.5B Instruct in dev to MaaS\?/ });
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

  it("Set up lists every change it will make before it runs", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "rhcl", title: "Red Hat Connectivity Link", state: "missing", commands: "oc apply -f - <<'EOF'" }, { id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }], {
        setupPlan: ["Install Red Hat Connectivity Link 1.5.0: Namespace kuadrant-system, Subscription kuadrant-system/kuadrant-operator (package rhcl-operator, channel stable-1.5, from redhat-operators)", "DataScienceCluster default-dsc: aigateway.managementState Removed to Managed"],
      }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByRole("button", { name: "Do it from the CLI instead" })).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    const confirm = await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ });
    expect(within(confirm).getByText(/channel stable-1.5, from redhat-operators/)).toBeInTheDocument();
    expect(within(confirm).getByText(/aigateway.managementState Removed to Managed/)).toBeInTheDocument();
  });

  it("a part's own fix: Upgrade Connectivity Link confirms its changes, then runs as its own action", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "rhcl", title: "Red Hat Connectivity Link", state: "ready", warning: "Red Hat Connectivity Link 1.4.3 is installed",
        actions: [{ id: "upgrade-rhcl", label: "Upgrade Connectivity Link", summary: "Upgrades it.", changes: ["Subscription kuadrant-system/kuadrant-operator: channel stable to stable-1.5 (version 1.5.0)."] }] }], {
        platform: { deployed: true, ready: false, managedByTool: true },
      }),
      "POST /api/resources/maas/stream": sse({ step: "operation_complete", status: "success", message: "done" }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    fireEvent.click(await screen.findByRole("button", { name: "Upgrade Connectivity Link" }));
    const confirm = await screen.findByRole("dialog", { name: /Upgrade Connectivity Link\?/ });
    expect(within(confirm).getByText(/channel stable to stable-1.5/)).toBeInTheDocument();
    fireEvent.click(within(confirm).getByRole("button", { name: "Upgrade Connectivity Link" }));
    await screen.findByRole("dialog", { name: "Upgrade Connectivity Link" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "upgrade-rhcl" }]);
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
    // Only "Not published" in the row; why it cannot be published is the button's tooltip.
    expect(within(granite).queryByText(reason)).toBeNull();
    expect(within(granite).getByText("Not published")).toBeInTheDocument();
    fireEvent.focus(within(granite).getByRole("button", { name: "Publish" }));
    expect((await screen.findByRole("tooltip")).textContent).toBe(reason);
    expect(within(granite).getByRole("button", { name: "Publish" })).toHaveAttribute("aria-disabled", "true");
    const qwen3 = screen.getByText("Qwen3 1.7B (CPU, tool calling)").closest("tr")!;
    fireEvent.click(within(qwen3).getByRole("button", { name: "Publish" }));
    const confirm = await screen.findByRole("dialog", { name: /Publish Qwen3 1.7B in dev to MaaS\?/ });
    fireEvent.click(within(confirm).getByRole("button", { name: "Publish" }));
    await screen.findByRole("dialog", { name: "Publish Qwen3 1.7B in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "publish", project: "dev", model: "qwen3-1-7b", limit: { preset: "premium" }, access: { everyone: true } }]);
  });

  it("several models of a project: each row has its own state, and Unpublish names its model", async () => {
    const added = "Partly published: someone added team/team-gateway since the model was published, so direct calls to the model through that gateway fail with 500 once the rate limit applies to it. Unpublish and Publish refuse, since writing back the gateways the model had before would drop it. Set the gateways the model should use yourself, without maas-default-gateway (for KServe's default gateway: oc patch llminferenceservice smollm2-135m-instruct -n dev --type=merge -p '{\"spec\":{\"router\":{\"gateway\":{\"refs\":null}}}}'), then Unpublish.";
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": base([{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], {
        platform: { deployed: true, ready: true, managedByTool: true },
        items: [
          { namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true, message: "Published" },
          { namespace: "dev", name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not published" },
          { namespace: "dev", name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", deployed: true, ready: false, managedByTool: true, modelReady: true, published: true, partly: true, message: added },
        ],
      }),
      "POST /api/resources/maas/stream": sse({ step: "operation_complete", status: "success", message: "done" }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const qwen25 = (await screen.findByText("Qwen2.5 0.5B Instruct (CPU)")).closest("tr")!;
    expect(within(qwen25).getByRole("button", { name: "Unpublish" })).toBeInTheDocument();
    const qwen3 = screen.getByText("Qwen3 1.7B (CPU, tool calling)").closest("tr")!;
    expect(within(qwen3).getByRole("button", { name: "Publish" })).toBeInTheDocument();
    const smollm2 = screen.getByText("SmolLM2 135M Instruct (CPU)").closest("tr")!;
    expect(within(smollm2).getByText(added)).toBeInTheDocument();
    expect(within(smollm2).queryByText(/earlier build/)).toBeNull();
    fireEvent.click(within(smollm2).getByRole("button", { name: "Unpublish" }));
    const confirm = await screen.findByRole("dialog", { name: /Unpublish SmolLM2 135M Instruct in dev\?/ });
    expect(within(confirm).getByText(/unless another model of this tool is published there/)).toBeInTheDocument();
    fireEvent.click(within(confirm).getByRole("button", { name: "Unpublish" }));
    await screen.findByRole("dialog", { name: "Unpublish SmolLM2 135M Instruct in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "unpublish", project: "dev", model: "smollm2-135m-instruct" }]);
  });
});
