import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import { contract } from "../../test/contract";
import type { ModelRegistryStatus, PrerequisitesStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE, modelRegistryState } from "./featureState";
import { cleanupCandidates, cleanupSteps, modelRegistryComponentLine } from "./CleanupModal";
import { modelRegistrySteps } from "./ModelRegistryTab";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const ready = (): ModelRegistryStatus => contract.modelRegistry();
const absent: ModelRegistryStatus = { name: "nightly-registry", namespace: "rhoai-model-registries", component: "aiHub", state: "absent", dashboardUrl: "https://rh-ai.apps.example.com" };
const prereqReady: PrerequisitesStatus = { features: { "model-registry": { ready: true, parts: [{ id: "dsc-aiHub", title: "Model registry (aiHub)", state: "ready", message: "Managed in DataScienceCluster default-dsc" }] } } };
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

function stub(registry: ModelRegistryStatus, extra: Record<string, unknown> = {}) {
  return stubApi({
    "/api/resources/status": status,
    "/api/resources/projects": { projects: [] },
    "/api/resources/prerequisites": prereqReady,
    "/api/resources/model-registry": registry,
    ...extra,
  });
}

const tick = () => act(async () => { await new Promise((r) => setTimeout(r, 0)); });

afterEach(() => vi.unstubAllGlobals());

describe("modelRegistryState", () => {
  it("each state, from the backend's answer", () => {
    expect(modelRegistryState(null)).toMatchObject({ loading: true });
    expect(modelRegistryState(absent)).toMatchObject({ status: "not-set-up", label: "Not set up", next: { kind: "set-up", label: "Set up the model registry", setup: [{ kind: "setup-model-registry", samples: true }], reason: null } });
    expect(modelRegistryState({ ...absent, blocked: { component: "aiHub", reason: "Model registry is turned off on this cluster." } }).next).toMatchObject({ reason: "Model registry is turned off on this cluster." });
    expect(modelRegistryState({ ...absent, state: "starting", managedByTool: true })).toMatchObject({ status: "setting-up", label: "Setting up", next: null });
    expect(modelRegistryState({ ...absent, state: "terminating", managedByTool: true })).toMatchObject({ status: "deleting" });
    expect(modelRegistryState(ready())).toMatchObject({ status: "ready", label: "Ready, with the sample model", next: { kind: "open", label: "Open model registry", href: "https://rh-ai.apps.example.com/ai-hub/models/registry/nightly-registry" } });
    // Someone else's keeps its own lifecycle state; ownership is apart (C4).
    expect(modelRegistryState({ ...absent, state: "ready", foreign: true })).toMatchObject({ status: "ready", unmanaged: true });
    expect(modelRegistryState({ ...absent, state: "starting", foreign: true })).toMatchObject({ status: "setting-up", unmanaged: true });
    // An incomplete sample is not "with the sample model" (C3).
    expect(modelRegistryState({ ...ready(), sample: { ...ready().sample!, complete: false } })).toMatchObject({ status: "ready", label: "Ready" });
    expect(modelRegistryState({ ...absent, state: "unknown", message: "Cannot read" })).toMatchObject({ status: "unknown" });
    expect(modelRegistryState(absent, { ...IDLE, modelRegistry: { action: "teardown" } })).toMatchObject({ status: "deleting" });
  });

  it("clean-up offers only this tool's registry, after MLflow; the steps", () => {
    expect(cleanupCandidates(status, "all", false, null, null, ready()).modelRegistry).toBe(true);
    expect(cleanupCandidates(status, "model-registry", false, null, null, ready()).modelRegistry).toBe(true);
    expect(cleanupCandidates(status, "mlflow", false, null, null, ready()).modelRegistry).toBe(false);
    expect(cleanupCandidates(status, "all", false, null, null, { ...ready(), foreign: true, managedByTool: false }).modelRegistry).toBe(false);
    expect(cleanupSteps({ mlflow: true, modelRegistry: true, models: ["dev"] }).map((s) => s.id)).toEqual(["mlflow", "model-registry", "model:dev"]);
    expect(modelRegistrySteps({ kind: "setup-model-registry", samples: true }).map((s) => s.id)).toEqual(["check", "create", "ready", "samples"]);
    expect(modelRegistrySteps({ kind: "setup-model-registry" }).map((s) => s.id)).toEqual(["check", "create", "ready"]);
    expect(modelRegistrySteps({ kind: "teardown-model-registry" }).map((s) => s.id)).toEqual(["check", "delete", "gone"]);
  });
});

describe("Model registry tab", () => {
  it("ready with the sample: the registry, its address, the sample model, the dashboard link and Clean up", async () => {
    stub(ready());
    renderAt("/test-resources?tab=model-registry");
    const registry = await screen.findByRole("listitem", { name: /^Model registry/ });
    expect(within(registry).getByText("Ready")).toBeInTheDocument();
    expect(registry).toHaveTextContent("rhoai-model-registries/nightly-registry");
    expect(within(registry).getByText("https://rh-ai.apps.example.com/model-registry/nightly-registry").closest("code, .pf-v6-c-code-block, .pf-v6-c-clipboard-copy, [class*=code]")).not.toBeNull();
    expect(within(registry).getByRole("link", { name: /Open model registry/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/ai-hub/models/registry/nightly-registry");
    const sample = screen.getByRole("listitem", { name: /^Sample model/ });
    expect(within(sample).getByText("Ready")).toBeInTheDocument();
    expect(sample).toHaveTextContent("qwen25-05b-instruct");
    expect(screen.getByRole("button", { name: "Clean up the model registry..." })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "What this creates" }));
    expect(screen.getByRole("link", { name: /Managing model registries/ })).toHaveAttribute("href", expect.stringContaining("managing_model_registries"));
  });

  it("someone else's registry: shown as not managed, nothing to delete or add", async () => {
    stub({ ...ready(), state: "starting", foreign: true, managedByTool: false, sample: undefined, message: "Deployment nightly-registry is progressing" });
    renderAt("/test-resources?tab=model-registry");
    const registry = await screen.findByRole("listitem", { name: /^Model registry/ });
    expect(within(registry).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(within(registry).getByText("Setting up")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Model registry actions" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Clean up the model registry/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Add the sample/ })).toBeNull();
  });

  it("starting: the operator's message, no Set up", async () => {
    stub({ ...absent, state: "starting", managedByTool: true, message: "Deployment nightly-registry is progressing" });
    renderAt("/test-resources?tab=model-registry");
    const registry = await screen.findByRole("listitem", { name: /^Model registry/ });
    expect(registry).toHaveTextContent("Deployment nightly-registry is progressing");
    expect(screen.queryByRole("button", { name: /^Set up the model registry/ })).toBeNull();
  });

  it("Set up asks first, with the sample ticked; the run sends what the dialog says", async () => {
    const api = stub(absent, {
      "POST /api/resources/model-registry/stream": sse(
        { step: "check", status: "success", message: "ok" },
        { step: "create", status: "success", message: "created" },
        { step: "ready", status: "success", message: "available" },
        { done: true, success: true, message: "Model registry nightly-registry is ready in rhoai-model-registries." },
      ),
    });
    renderAt("/test-resources?tab=model-registry");
    fireEvent.click(await screen.findByRole("button", { name: "Set up the model registry" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up the model registry?" });
    const box = within(dialog).getByRole("checkbox", { name: "Add a sample model" });
    expect(box).toBeChecked();
    expect(dialog).toHaveTextContent("default database");
    expect(dialog).toHaveTextContent("No access is granted to anyone else.");
    fireEvent.click(box);
    expect(within(dialog).getByRole("checkbox", { name: "Add a sample model" })).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    await tick();
    await tick();
    expect(api.bodies["POST /api/resources/model-registry/stream"]).toEqual([{ action: "setup" }]);
  });

  it("Clean up lists the registry with what is lost, and offers to turn the component back with the Model Catalog line", async () => {
    stub(ready(), {
      "/api/resources/prerequisites": { features: { "model-registry": { ...prereqReady.features["model-registry"], restorable: [{ component: "aiHub", previous: "Removed", feature: "model-registry" }] } } },
    });
    renderAt("/test-resources?tab=model-registry");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up the model registry..." }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up the model registry\?/ });
    expect(within(dialog).getByRole("checkbox", { name: "nightly-registry" })).toBeChecked();
    expect(dialog).toHaveTextContent("Its registered models and its database are deleted with it. The DataScienceCluster component that runs it stays on unless you also tick it below.");
    // One part: no wording about parts running in order (U1).
    expect(dialog).toHaveTextContent("Deletes the model registry this tool created, as one operation.");
    expect(dialog).not.toHaveTextContent(/in this order|a part that fails/);
    // The registry is the cluster's: other people's models go too (U3).
    expect(dialog).toHaveTextContent("including the models other people registered");
    expect(dialog).not.toHaveTextContent("for every dashboard user");
    // The component this tool turned on is offered, unticked, with what else it removes.
    const component = within(dialog).getByRole("checkbox", { name: "Turn the model registry component (aiHub) back to Removed; this also removes the Model Catalog." });
    expect(component).not.toBeChecked();
    expect(component.closest(".pf-v6-c-check")).toHaveTextContent("Runs last. It stays on while anything on the cluster still uses it, or if someone changed it since this tool turned it on. The operators it needed stay installed.");
    fireEvent.click(component);
    expect(dialog).toHaveTextContent("The Model Catalog, for every dashboard user.");
  });

  it("the component line is shown only for a component this tool turned on (restorable)", async () => {
    stub(ready());
    renderAt("/test-resources?tab=model-registry");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up the model registry..." }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up the model registry\?/ });
    expect(within(dialog).queryByRole("checkbox", { name: /aiHub/ })).toBeNull();
    expect(dialog).not.toHaveTextContent("Model Catalog");
    expect(modelRegistryComponentLine("aiHub")).toBe("this also removes the Model Catalog.");
    expect(modelRegistryComponentLine("modelregistry")).toBe("");
    expect(modelRegistryComponentLine("kserve")).toBeNull();
  });
});

describe("Model registry tab: the sample and refresh errors", () => {
  it("a registered sample without its version or artifact is Incomplete, with Add the sample to complete it (C3)", async () => {
    stub({ ...ready(), sample: { ...ready().sample!, complete: false } });
    renderAt("/test-resources?tab=model-registry");
    const sample = await screen.findByRole("listitem", { name: /^Sample model/ });
    expect(within(sample).getByText("Incomplete")).toBeInTheDocument();
    expect(within(sample).getByRole("button", { name: "Add the sample" })).toBeInTheDocument();
    expect(sample).toHaveTextContent("Sample model qwen25-05b-instruct (the Models tab's default chat model), version v1");
  });

  it("before setup the sample is named the same way (U4)", async () => {
    stub(absent);
    renderAt("/test-resources?tab=model-registry");
    const sample = await screen.findByRole("listitem", { name: /^Sample model/ });
    expect(sample).toHaveTextContent("Sample model qwen25-05b-instruct (the Models tab's default chat model), version v1");
    fireEvent.click(await screen.findByRole("button", { name: "Set up the model registry" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up the model registry?" });
    expect(dialog).toHaveTextContent("Registered models cannot be deleted, only archived; they are deleted when the registry is cleaned up.");
  });

  it("an address the tool does not use says why (C1)", async () => {
    stub({ ...ready(), address: undefined, sample: undefined, addressProblem: "the address Service rhoai-model-registries/nightly-registry names (evil.example.com) is not the cluster's gateway (rh-ai.apps.example.com)" });
    renderAt("/test-resources?tab=model-registry");
    const registry = await screen.findByRole("listitem", { name: /^Model registry/ });
    expect(registry).toHaveTextContent("is not the cluster's gateway");
  });

  it("a refresh that fails after data was shown keeps the data, with a warning and Retry (C5)", async () => {
    let fail = false;
    const api = stub(ready(), {
      "/api/resources/model-registry": () => (fail
        ? new Response(JSON.stringify({ error: "the cluster API answered 503", errorCode: "cluster_unavailable" }), { status: 503, headers: { "Content-Type": "application/json" } })
        : new Response(JSON.stringify(ready()), { status: 200, headers: { "Content-Type": "application/json" } })),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=model-registry"]}>
      <QuickResourceCreator mutateBlocker={null} renderHeader={({ onRefresh }) => <button type="button" onClick={onRefresh}>Refresh</button>} />
    </MemoryRouter>);
    await screen.findByRole("link", { name: /Open model registry/ });
    fail = true;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await tick();
    const warning = await screen.findByText("Model registry status may be out of date");
    expect(screen.getByRole("link", { name: /Open model registry/ })).toBeInTheDocument();
    fail = false;
    const before = api.calls.filter((c) => c.includes("/api/resources/model-registry")).length;
    fireEvent.click(within(warning.closest(".pf-v6-c-alert") as HTMLElement).getByRole("button", { name: "Retry" }));
    await tick();
    expect(api.calls.filter((c) => c.includes("/api/resources/model-registry")).length).toBe(before + 1);
    expect(screen.queryByText("Model registry status may be out of date")).toBeNull();
  });
});
