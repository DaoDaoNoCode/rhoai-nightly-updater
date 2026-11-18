import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { CatalogModel, InferenceGatewayState, ModelState, ModelsStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE, modelsState } from "./featureState";
import { cleanupBlocked, cleanupCandidates, cleanupSteps } from "./CleanupModal";
import { envStatus } from "./resourceStatus";
import { embeddingCurl, modelCurl, modelsSteps, thinkingCurl, toolCurl } from "./ModelsTab";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const gateway: InferenceGatewayState = {
  deployed: true, ready: true, managedByTool: true, namespace: "openshift-ingress", name: "openshift-ai-inference", message: "Running",
  externalUrl: "https://inference.apps.example.com", address: "a1.elb.example.com", dns: "published", authPolicy: "enforced",
};
const noGateway: InferenceGatewayState = { deployed: false, ready: false, managedByTool: false, message: "Not set up" };
const model = (ns: string, over: Partial<ModelState> = {}): ModelState => ({
  deployed: true, ready: true, managedByTool: true, namespace: ns, name: "qwen25-05b-instruct", message: "Running",
  displayName: "Qwen2.5 0.5B Instruct (CPU)", url: `https://inference.apps.example.com/${ns}/qwen25-05b-instruct`, imageKind: "preset", ...over,
});
const catalog: CatalogModel[] = [
  { name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", uri: "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac", cpu: "2", memoryRequest: "5Gi", memory: "6Gi",
    type: "chat", useCase: "Chat (smoke test)", description: "A very small chat model.", args: "--max-model-len 4096", downloadSize: "270 MB" },
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775", cpu: "4", memoryRequest: "6Gi", memory: "8Gi",
    type: "chat", useCase: "Chat", description: "A small chat model.", args: "--max-model-len 4096", downloadSize: "1.0 GB", default: true },
  { name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", uri: "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e", cpu: "4", memoryRequest: "10Gi", memory: "12Gi",
    type: "chat", toolCalling: true, useCase: "Chat, Tool calling, Reasoning", description: "A chat model that can call tools.", downloadSize: "4.1 GB",
    args: `--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking": false}'` },
  { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", uri: "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f", cpu: "2", memoryRequest: "4Gi", memory: "6Gi",
    type: "embedding", embeddingDimension: 768, useCase: "Embeddings, Semantic search", description: "An English embedding model.", args: "--runner pooling --max-model-len 512", downloadSize: "250 MB" },
];
const withRoom = (name: string, capacityWarning: string) => catalog.map((c) => (c.name === name ? { ...c, capacityWarning } : c));
const modelsStatus = (over: Partial<ModelsStatus> = {}): ModelsStatus => ({ dependencies: "True", preset: "v3-6-0-kserve-config-llm-template-cpu", imagePlan: "preset", catalog, ...over });
const status = (over: Partial<ResourcesStatus> = {}): ResourcesStatus => ({
  minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com",
  inferenceGateway: noGateway, models: modelsStatus(), ...over,
});
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

describe("modelsState", () => {
  it("one Set up for what is missing: the gateway and the model, or only the model", () => {
    const s = modelsState(status(), "dev", ["dev"]);
    expect(s).toMatchObject({ status: "not-set-up", label: "Not set up in dev", next: { kind: "set-up", label: "Set up Models in dev", setup: [{ kind: "setup-models", gateway: true, project: "dev", model: "qwen25-05b-instruct" }], reason: null } });
    const onlyModel = modelsState(status({ inferenceGateway: gateway }), "dev", ["dev"]);
    expect(onlyModel.next).toMatchObject({ label: "Set up Qwen2.5 0.5B Instruct in dev", setup: [{ kind: "setup-models", gateway: false, project: "dev", model: "qwen25-05b-instruct" }] });
    // The chosen model: its own Set up.
    expect(modelsState(status({ inferenceGateway: gateway }), "dev", ["dev"], IDLE, "qwen3-1-7b").next).toMatchObject({ label: "Set up Qwen3 1.7B in dev", setup: [{ model: "qwen3-1-7b" }] });
    // Someone else's gateway is used as it is: only the model is set up.
    const foreign = modelsState(status({ inferenceGateway: { ...gateway, managedByTool: false } }), "dev", ["dev"]);
    expect(foreign.next).toMatchObject({ setup: [{ gateway: false }] });
  });

  it("ready opens the project in the dashboard; ownership shows in the label only", () => {
    const s = modelsState(status({ inferenceGateway: { ...gateway, managedByTool: false }, models: modelsStatus({ items: [model("dev")] }) }), "dev", ["dev"]);
    expect(s).toMatchObject({ status: "ready", label: "Ready in dev · not managed by this tool", next: { kind: "open", href: "https://rh-ai.apps.example.com/projects/dev?section=model-server" } });
  });

  it("a cluster that cannot serve models says why on Set up", () => {
    const s = modelsState(status({ models: modelsStatus({ blocked: { component: "kserve", reason: "Model serving is turned off." } }) }), "dev", ["dev"]);
    expect(s.next).toMatchObject({ kind: "set-up", reason: "Model serving is turned off." });
  });

  it("a model that cannot start needs attention; an unknown health stays unknown, never an error", () => {
    const broken = modelsState(status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev", { ready: false, terminalError: true, presetImageMissing: true, waitingReason: "ErrImagePull" })] }) }), "dev", ["dev"]);
    expect(broken).toMatchObject({ status: "error", next: { kind: "show-issue", part: "model" } });
    const unknown: InferenceGatewayState = { ...gateway, ready: false, managedByTool: false, healthUnknown: true };
    expect(envStatus(unknown)).toBe("unknown");
    expect(modelsState(status({ inferenceGateway: unknown }), null, ["dev"]).status).not.toBe("error");
  });

  it("across projects for the Overview, and busy while a run sets it up", () => {
    const two = status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("a"), model("b")] }) });
    // Counted among the projects that have a model, not every project.
    expect(modelsState(two, null, ["a", "b", "c"])).toMatchObject({ status: "ready", label: "Ready in 2 projects", next: { kind: "choose-project" } });
    const ten = status({ inferenceGateway: gateway, models: modelsStatus({ items: Array.from({ length: 10 }, (_, i) => model(`p${i}`)) }) });
    expect(modelsState(ten, null, Array.from({ length: 25 }, (_, i) => `p${i}`))).toMatchObject({ status: "ready", label: "Ready in 10 projects" });
    expect(modelsState(status({ inferenceGateway: gateway }), null, ["a"])).toMatchObject({ label: "Not set up in any project", next: { kind: "choose-project" } });
    const busy = { ...IDLE, models: { action: "setup" as const, gateway: true, project: "a" } };
    expect(modelsState(status(), "a", ["a"], busy)).toMatchObject({ status: "setting-up", label: "Setting up in a", next: null });
  });
});

describe("modelsState on the Overview", () => {
  it("never hides a problem behind ready projects", () => {
    // A problem is never hidden by ready projects; its next step opens that project.
    const oneBad = status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("a"), model("b", { warning: "The model restarted 3 times." }), model("c")] }) });
    expect(modelsState(oneBad, null, ["a", "b", "c"])).toMatchObject({ status: "needs-attention", label: "Needs attention in 1 of 3 projects", project: "b", next: { kind: "show-issue" } });
  });

  it("shows the operation running from this page", () => {
    // An operation running from this page shows on the Overview (live: it said "Not set up in any project").
    const settingUp = { ...IDLE, models: { action: "setup" as const, gateway: true, project: "juntao-test" } };
    expect(modelsState(status({ inferenceGateway: gateway }), null, ["juntao-test"], settingUp)).toMatchObject({ status: "setting-up", label: "Setting up in juntao-test" });
    const removing = { ...IDLE, models: { action: "teardown" as const, gateway: false, project: "a" } };
    const two = status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("a"), model("b")] }) });
    expect(modelsState(two, null, ["a", "b"], removing)).toMatchObject({ status: "deleting", label: "Deleting in a" });
  });
});

describe("Models runs and clean-up", () => {
  it("names the streamed steps in their order", () => {
    expect(modelsSteps({ kind: "setup-models", gateway: true, project: "dev", model: "qwen3-1-7b" }).map((s) => s.id)).toEqual(["check", "gateway", "gateway-ready", "model", "model-ready"]);
    expect(modelsSteps({ kind: "setup-models", gateway: false, project: "dev", model: "qwen3-1-7b", releasedImage: true }).map((s) => s.title)).toContain("Switch qwen3-1-7b in dev to the released image");
    expect(modelsSteps({ kind: "teardown-model", project: "dev", model: "smollm2-135m-instruct" }).map((s) => s.title)).toEqual(["Delete smollm2-135m-instruct in dev"]);
    expect(cleanupSteps({ mlflow: true, models: ["a"], inferenceGateway: true }).map((s) => s.id)).toEqual(["mlflow", "model:a", "inference-gateway"]);
  });

  it("offers only this tool's models and gateway; the gateway waits for every model that uses it", () => {
    const s = status({ inferenceGateway: { ...gateway, usedBy: ["a/qwen25-05b-instruct", "team/granite"] }, models: modelsStatus({ items: [model("a"), model("b", { managedByTool: false })] }) });
    expect(cleanupCandidates(s, "models", false)).toMatchObject({ models: ["a"], gateway: true, mlflow: false, servers: [] });
    expect(cleanupCandidates(s, "pipelines", false)).toMatchObject({ models: [], gateway: false });
    expect(cleanupBlocked(s, { models: ["a"], inferenceGateway: true })).toMatch(/team\/granite/);
    const own = status({ inferenceGateway: { ...gateway, usedBy: ["a/qwen25-05b-instruct"] }, models: modelsStatus({ items: [model("a")] }) });
    expect(cleanupBlocked(own, { models: ["a"], inferenceGateway: true })).toBeNull();
    expect(cleanupBlocked(own, { inferenceGateway: true })).toMatch(/Select that model too/);
  });

  it("the embedding, tool and thinking commands call the right paths with the user's token", () => {
    const base = "https://inference.apps.example.com/dev/m/";
    expect(embeddingCurl(base, "granite-embedding-125m-english")).toContain(`https://inference.apps.example.com/dev/m/v1/embeddings \\\n  -d '{"model":"granite-embedding-125m-english","input":`);
    expect(toolCurl(base, "qwen3-1-7b")).toContain(`"tools":[{"type":"function"`);
    expect(thinkingCurl(base, "qwen3-1-7b")).toContain(`"chat_template_kwargs":{"enable_thinking":true}`);
    for (const c of [embeddingCurl(base, "x"), toolCurl(base, "x"), thinkingCurl(base, "x")]) {
      expect(c).toContain("TOKEN=$(oc whoami -t)");
      // One single-quoted body: no single quote inside it.
      expect(c.split("-d '")[1].slice(0, -1)).not.toContain("'");
    }
  });

  it("a project with several models: the Overview shows a problem of any of them", () => {
    const s = status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev"), model("dev", { name: "qwen3-1-7b", ready: false, terminalError: true, message: "OOMKilled" })] }) });
    expect(modelsState(s, null, ["dev"])).toMatchObject({ status: "error", project: "dev" });
    expect(modelsState(s, "dev", ["dev"], IDLE, "qwen25-05b-instruct")).toMatchObject({ status: "ready" });
  });

  it("the curl uses the user's own token and the model's resource name", () => {
    const text = modelCurl("https://inference.apps.example.com/dev/qwen25-05b-instruct/", "qwen25-05b-instruct");
    expect(text).toContain("TOKEN=$(oc whoami -t)");
    expect(text).toContain("https://inference.apps.example.com/dev/qwen25-05b-instruct/v1/models");
    expect(text).toContain(`"model":"qwen25-05b-instruct"`);
  });
});

describe("Models tab", () => {
  it("names the address the gateway will get before it is set up", async () => {
    stubApi({ "/api/resources/status": status({ inferenceGateway: { deployed: false, ready: false, managedByTool: false, message: "Not set up", plannedUrl: "https://inference.apps.example.com" } }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=models&project=dev");
    const gw = await screen.findByRole("listitem", { name: "Inference gateway" });
    expect(within(gw).getByText("https://inference.apps.example.com")).toBeInTheDocument();
    expect(within(gw).queryByText(/apps domain/)).toBeNull();
  });

  it("shows both parts with their details and how to call a ready model", async () => {
    stubApi({ "/api/resources/status": status({ inferenceGateway: { ...gateway, usedBy: ["dev/qwen25-05b-instruct"], teardownBlockedReason: "Models route through it: dev/qwen25-05b-instruct. Delete that model first." }, models: modelsStatus({ items: [model("dev", { hardwareProfile: "redhat-ods-applications/default-profile" })] }) }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=models&project=dev");
    const gw = await screen.findByRole("listitem", { name: "Inference gateway" });
    expect(within(gw).getByText(/runs on a cloud load balancer, which costs money while it exists/)).toBeInTheDocument();
    expect(within(gw).getByText(/On: every call needs an OpenShift token/)).toBeInTheDocument();
    // The real address, not a placeholder.
    expect(within(gw).getAllByText(gateway.externalUrl!)[0]).toBeInTheDocument();
    expect(within(gw).queryByText(/apps domain/)).toBeNull();
    const part = screen.getByRole("listitem", { name: "Qwen2.5 0.5B Instruct (CPU)" });
    expect(within(part).getByRole("link", { name: /Open dev in the dashboard/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/projects/dev?section=model-server");
    expect(screen.getByRole("heading", { name: "Use it" })).toBeInTheDocument();
    expect(screen.getByText(/"model":"qwen25-05b-instruct"/)).toBeInTheDocument();
    // What the dashboard shows for it: the hardware profile, and why it lists no tokens.
    expect(within(part).getByText("redhat-ods-applications/default-profile")).toBeInTheDocument();
    expect(within(part).getByText(/so the dashboard lists no tokens for this model/)).toBeInTheDocument();
    expect(within(part).getAllByText(/4 CPUs and 6 GiB of memory \(up to 8 GiB\)/)).not.toHaveLength(0);
    // In use: the gateway's Delete says why it is not available.
    fireEvent.click(within(gw).getByRole("button", { name: "Inference gateway actions" }));
    expect(await screen.findByRole("menuitem", { name: /Delete inference gateway/ })).toHaveAttribute("aria-disabled", "true");
  });

  it("Set up confirms what it creates, then runs one streamed operation", async () => {
    const api = stubApi({
      "/api/resources/status": status(),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/models/stream": sse(
        { step: "check", status: "success", message: "Everything the parts need is there.", elapsedMs: 1 },
        { step: "gateway", status: "success", message: "Gateway applied.", elapsedMs: 2 },
        { step: "gateway-ready", status: "success", message: "Ready.", elapsedMs: 3 },
        { step: "model", status: "success", message: "Model applied.", elapsedMs: 4 },
        { step: "model-ready", status: "success", message: "Ready at https://inference.apps.example.com/dev/qwen25-05b-instruct.", elapsedMs: 5 },
        { step: "operation_complete", status: "success", message: "Qwen2.5 0.5B Instruct (CPU) is ready in dev.", elapsedMs: 6 },
      ),
    });
    renderAt("/test-resources?tab=models&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Set up Models in dev" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up Models in dev\?/ });
    expect(within(dialog).getByText(/costs money while it exists/)).toBeInTheDocument();
    expect(within(dialog).getByText(/No ServiceAccount, role or token is made/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Models in dev" });
    expect(await within(run).findByText("Qwen2.5 0.5B Instruct (CPU) is ready in dev.")).toBeInTheDocument();
    expect(within(run).getByText("Wait for the gateway and its load balancer")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/models/stream"]).toEqual([{ action: "setup", gateway: true, project: "dev", model: "qwen25-05b-instruct" }]);
  });

  it("no room is no reason once the project has the model: its own pod holds the room, and its state says why it waits", async () => {
    stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ catalog: withRoom("qwen25-05b-instruct", "No worker seems to have room for the model."), items: [model("dev", { ready: false, message: "Starting vLLM" })] }) }),
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderAt("/test-resources?tab=models&project=dev&model=qwen25-05b-instruct");
    expect(await screen.findAllByText(/Starting vLLM/)).not.toHaveLength(0);
    expect(screen.queryByText(/No room for it now/)).not.toBeInTheDocument();
  });

  it("the picker shows what each model is for and reserves, and disables a model with no room, saying why", async () => {
    stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ catalog: withRoom("qwen3-1-7b", "No worker seems to have room for the model: it needs 4 CPUs and 10Gi of memory free.") }) }),
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderAt("/test-resources?tab=models&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Model" }));
    const smol = await screen.findByRole("option", { name: /SmolLM2 135M Instruct \(CPU\)/ });
    expect(smol).toHaveTextContent("Chat. For: Chat (smoke test). Reserves 2 CPUs and 5 GiB of memory (up to 6 GiB). Downloads about 270 MB.");
    expect(screen.getByRole("option", { name: /Granite Embedding/ })).toHaveTextContent("Embedding (768 dimensions)");
    const qwen3 = screen.getByRole("option", { name: /Qwen3 1.7B/ });
    expect(qwen3).toHaveTextContent("Chat, tool calling");
    expect(qwen3).toHaveTextContent("No room for it now. No worker seems to have room for the model: it needs 4 CPUs and 10Gi of memory free.");
    expect(qwen3.closest("li")?.querySelector("button")).toBeDisabled();
  });

  it("choosing a model shows its part and sets up that one", async () => {
    const api = stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway }),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/models/stream": sse({ step: "operation_complete", status: "success", message: "done", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=models&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Model" }));
    fireEvent.click(await screen.findByRole("option", { name: /Granite Embedding/ }));
    const part = await screen.findByRole("listitem", { name: "Granite Embedding 125M English (CPU)" });
    expect(within(part).getByText(/An embedding model: it turns text into vectors of 768 numbers/)).toBeInTheDocument();
    expect(within(part).getByText("--runner pooling --max-model-len 512")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Granite Embedding 125M English in dev" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up Granite Embedding 125M English in dev\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    await screen.findByRole("dialog", { name: "Set up Granite Embedding 125M English in dev" });
    expect(api.bodies["POST /api/resources/models/stream"]).toEqual([{ action: "setup", project: "dev", model: "granite-embedding-125m-english" }]);
  });

  it("Use it fits the model: embeddings, and Qwen3's tool calling and thinking", async () => {
    const qwen3 = model("dev", { name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", url: "https://inference.apps.example.com/dev/qwen3-1-7b" });
    const granite = model("dev", { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", url: "https://inference.apps.example.com/dev/granite-embedding-125m-english" });
    stubApi({ "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ items: [qwen3, granite] }) }), "/api/resources/projects": { projects: ["dev"] } });
    const view = renderAt("/test-resources?tab=models&project=dev&model=qwen3-1-7b");
    expect(await screen.findByText(/"chat_template_kwargs":\{"enable_thinking":true\}/)).toBeInTheDocument();
    expect(screen.getByText(/Thinking is off by default/)).toBeInTheDocument();
    expect(screen.getByText(/"tools":\[/)).toBeInTheDocument();
    view.unmount();
    renderAt("/test-resources?tab=models&project=dev&model=granite-embedding-125m-english");
    expect(await screen.findByText(/\/v1\/embeddings/)).toBeInTheDocument();
    expect(screen.queryByText(/v1\/chat\/completions/)).toBeNull();
  });

  it("a missing preset image offers the newest released image, which re-runs only the model", async () => {
    const api = stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev", { ready: false, terminalError: true, presetImageMissing: true, waitingReason: "ErrImagePull", message: "ErrImagePull: manifest unknown" })] }) }),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/models/stream": sse({ step: "operation_complete", status: "success", message: "done", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=models&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Use the newest released Red Hat image" }));
    const dialog = await screen.findByRole("dialog", { name: /Use the newest released Red Hat image for Qwen2.5 0.5B Instruct in dev\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Use the released image" }));
    await screen.findByRole("dialog", { name: "Use the released image for Qwen2.5 0.5B Instruct in dev" });
    expect(api.bodies["POST /api/resources/models/stream"]).toEqual([{ action: "setup", project: "dev", model: "qwen25-05b-instruct", releasedImage: true }]);
  });

  it("the Overview lists Models with the same state as its tab", async () => {
    stubApi({ "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev")] }) }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources");
    const table = await screen.findByRole("grid", { name: "Features" });
    const row = within(table).getByRole("link", { name: "Models" }).closest("tr")!;
    expect(within(row).getByText("Ready in dev")).toBeInTheDocument();
    expect(within(row).getByRole("link", { name: /Open dev in the dashboard/ })).toBeInTheDocument();
  });
});
