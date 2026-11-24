import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import { contract } from "../../test/contract";
import type { ModelRegistryStatus, PrerequisitesStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE, modelRegistryState } from "./featureState";
import { cleanupCandidates, cleanupSteps } from "./CleanupModal";
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
    expect(modelRegistryState(ready())).toMatchObject({ status: "ready", label: "Ready, with the sample model", next: { kind: "open", href: "https://rh-ai.apps.example.com/ai-hub/models/registry/nightly-registry" } });
    expect(modelRegistryState({ ...absent, state: "foreign" })).toMatchObject({ status: "ready", unmanaged: true });
    expect(modelRegistryState({ ...absent, state: "unknown", message: "Cannot read" })).toMatchObject({ status: "unknown" });
    expect(modelRegistryState(absent, { ...IDLE, modelRegistry: { action: "teardown" } })).toMatchObject({ status: "deleting" });
  });

  it("clean-up offers only this tool's registry, after MLflow; the steps", () => {
    expect(cleanupCandidates(status, "all", false, null, null, ready()).modelRegistry).toBe(true);
    expect(cleanupCandidates(status, "model-registry", false, null, null, ready()).modelRegistry).toBe(true);
    expect(cleanupCandidates(status, "mlflow", false, null, null, ready()).modelRegistry).toBe(false);
    expect(cleanupCandidates(status, "all", false, null, null, { ...ready(), state: "foreign", managedByTool: false }).modelRegistry).toBe(false);
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
    expect(registry).toHaveTextContent("https://rh-ai.apps.example.com/model-registry/nightly-registry");
    expect(within(registry).getByRole("link", { name: /Open in the dashboard/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/ai-hub/models/registry/nightly-registry");
    const sample = screen.getByRole("listitem", { name: /^Sample model/ });
    expect(within(sample).getByText("Ready")).toBeInTheDocument();
    expect(sample).toHaveTextContent("qwen25-05b-instruct");
    expect(screen.getByRole("button", { name: "Clean up the model registry..." })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "What this creates" }));
    expect(screen.getByRole("link", { name: /Managing model registries/ })).toHaveAttribute("href", expect.stringContaining("managing_model_registries"));
  });

  it("someone else's registry: shown as not managed, nothing to delete or add", async () => {
    stub({ ...ready(), state: "foreign", managedByTool: false, sample: undefined, message: "A model registry named nightly-registry exists in rhoai-model-registries that this tool did not create." });
    renderAt("/test-resources?tab=model-registry");
    const registry = await screen.findByRole("listitem", { name: /^Model registry/ });
    expect(within(registry).getByText("Not managed by this tool")).toBeInTheDocument();
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

  it("Clean up lists the registry with what is lost, and never offers to turn the component off", async () => {
    stub(ready(), {
      "/api/resources/prerequisites": { features: { "model-registry": { ...prereqReady.features["model-registry"], restorable: [{ component: "aiHub", previous: "Removed", feature: "model-registry" }] } } },
    });
    renderAt("/test-resources?tab=model-registry");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up the model registry..." }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up the model registry\?/ });
    expect(within(dialog).getByRole("checkbox", { name: /Model registry nightly-registry/ })).toBeChecked();
    expect(dialog).toHaveTextContent("With its registered models and its database, which are deleted with it.");
    expect(dialog).toHaveTextContent("Every model, version and artifact record in the model registry, and its database.");
    expect(within(dialog).queryByRole("checkbox", { name: /aiHub/ })).toBeNull();
  });
});
