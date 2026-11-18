import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { InferenceGatewayState, ModelState, ModelsStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE, modelsState } from "./featureState";
import { cleanupBlocked, cleanupCandidates, cleanupSteps } from "./CleanupModal";
import { envStatus } from "./resourceStatus";
import { modelCurl, modelsSteps } from "./ModelsTab";

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
const catalog = [{ name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct", cpu: "4", memoryRequest: "6Gi", memory: "8Gi" }];
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
    expect(s).toMatchObject({ status: "not-set-up", label: "Not set up in dev", next: { kind: "set-up", label: "Set up Models in dev", setup: [{ kind: "setup-models", gateway: true, project: "dev" }], reason: null } });
    const onlyModel = modelsState(status({ inferenceGateway: gateway }), "dev", ["dev"]);
    expect(onlyModel.next).toMatchObject({ label: "Set up the model in dev", setup: [{ kind: "setup-models", gateway: false, project: "dev" }] });
    // Someone else's gateway is used as it is: only the model is set up.
    const foreign = modelsState(status({ inferenceGateway: { ...gateway, managedByTool: false } }), "dev", ["dev"]);
    expect(foreign.next).toMatchObject({ setup: [{ gateway: false }] });
  });

  it("ready opens the project in the dashboard; ownership is said apart from the label", () => {
    const s = modelsState(status({ inferenceGateway: { ...gateway, managedByTool: false }, models: modelsStatus({ items: [model("dev")] }) }), "dev", ["dev"]);
    expect(s).toMatchObject({ status: "ready", label: "Ready in dev", unmanaged: true, next: { kind: "open", label: "Open in dashboard", href: "https://rh-ai.apps.example.com/projects/dev?section=model-server" } });
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
    expect(modelsState(two, null, ["a", "b", "c"]).projects?.map((p) => p.project)).toEqual(["a", "b"]);
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
    expect(modelsSteps({ kind: "setup-models", gateway: true, project: "dev" }).map((s) => s.id)).toEqual(["check", "gateway", "gateway-ready", "model", "model-ready"]);
    expect(modelsSteps({ kind: "setup-models", gateway: false, project: "dev", releasedImage: true }).map((s) => s.title)).toContain("Switch the model in dev to the released image");
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
    expect(within(part).getByText(/4 CPUs and 6 GiB of memory \(up to 8 GiB\)/)).toBeInTheDocument();
    // In use: the gateway's Delete says why it is not available.
    fireEvent.click(within(gw).getByRole("button", { name: "Inference gateway actions" }));
    expect(await screen.findByRole("menuitem", { name: /Delete inference gateway/ })).toHaveAttribute("aria-disabled", "true");
  });

  it("Set up confirms what it creates, then runs one streamed operation", async () => {
    const api = stubApi({
      "/api/resources/status": status({ models: modelsStatus({ capacityWarning: "No worker seems to have room for the model." }) }),
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
    expect(await screen.findByText("No worker seems to have room for the model.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Models in dev" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up Models in dev\?/ });
    expect(within(dialog).getByText(/costs money while it exists/)).toBeInTheDocument();
    expect(within(dialog).getByText(/No ServiceAccount, role or token is made/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Models in dev" });
    expect(await within(run).findByText("Qwen2.5 0.5B Instruct (CPU) is ready in dev.")).toBeInTheDocument();
    expect(within(run).getByText("Wait for the gateway and its load balancer")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/models/stream"]).toEqual([{ action: "setup", gateway: true, project: "dev" }]);
  });

  it("no capacity warning once the project has the model: its own pod holds the room, and its state says why it waits", async () => {
    stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ capacityWarning: "No worker seems to have room for the model.", items: [model("dev", { ready: false, message: "Starting vLLM" })] }) }),
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderAt("/test-resources?tab=models&project=dev");
    expect(await screen.findAllByText(/Starting vLLM/)).not.toHaveLength(0);
    expect(screen.queryByText("No worker seems to have room for the model.")).not.toBeInTheDocument();
  });

  it("a missing preset image offers the newest released image, which re-runs only the model", async () => {
    const api = stubApi({
      "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev", { ready: false, terminalError: true, presetImageMissing: true, waitingReason: "ErrImagePull", message: "ErrImagePull: manifest unknown" })] }) }),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/models/stream": sse({ step: "operation_complete", status: "success", message: "done", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=models&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Use the newest released Red Hat image" }));
    const dialog = await screen.findByRole("dialog", { name: /Use the newest released Red Hat image in dev\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Use the released image" }));
    await screen.findByRole("dialog", { name: "Use the released image in dev" });
    expect(api.bodies["POST /api/resources/models/stream"]).toEqual([{ action: "setup", project: "dev", releasedImage: true }]);
  });

  it("the Overview lists Models with the same state as its tab", async () => {
    stubApi({ "/api/resources/status": status({ inferenceGateway: gateway, models: modelsStatus({ items: [model("dev")] }) }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources");
    const table = await screen.findByRole("grid", { name: "Features" });
    const row = within(table).getByRole("link", { name: "Models" }).closest("tr")!;
    expect(within(row).getByText("Ready in dev")).toBeInTheDocument();
    expect(within(row).getByRole("link", { name: "Open in dashboard" })).toBeInTheDocument();
  });
});
