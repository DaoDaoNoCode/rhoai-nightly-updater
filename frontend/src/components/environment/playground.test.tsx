import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { PlaygroundModel, PlaygroundState, PlaygroundStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE, playgroundState } from "./featureState";
import { cleanupCandidates, cleanupSteps } from "./CleanupModal";
import { playgroundCurl, playgroundSteps } from "./PlaygroundTab";
import { PLAYGROUND_POLL_MS } from "./usePlaygroundStatus";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const qwen: PlaygroundModel = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", source: "namespace", status: "Running", ready: true, chat: true };
const maas: PlaygroundModel = { name: "gpt-oss-20b", source: "maas", status: "Ready", ready: true, chat: true };
const item = (ns: string, over: Partial<PlaygroundState> = {}): PlaygroundState => ({
  namespace: ns, name: "lsd-genai-playground", deployed: true, ready: true, phase: "Ready", managedByTool: true, requested: true, message: "Ready", ...over,
});
const pg = (over: Partial<PlaygroundStatus> = {}): PlaygroundStatus => ({ operatorReady: "True", genAiUi: "available", project: "dev", models: [qwen, maas], ...over });
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

describe("playgroundState", () => {
  it("not set up: one Set up of the playground, blocked with the reason when something is missing", () => {
    expect(playgroundState(status, pg(), "dev")).toMatchObject({ status: "not-set-up", next: { kind: "set-up", label: "Set up the playground in dev", setup: [{ kind: "setup-playground", project: "dev" }], reason: null } });
    expect(playgroundState(status, pg({ models: [maas] }), "dev").next).toMatchObject({ reason: null });
    expect(playgroundState(status, pg({ models: [] }), "dev").next).toMatchObject({ reason: expect.stringContaining("Needs: Models") });
    // The dashboard's install checks only the models it is given: another one that is not ready does not block.
    expect(playgroundState(status, pg({ models: [qwen, { name: "deepseek", source: "namespace", status: "Failed", ready: false, chat: true }] }), "dev").next).toMatchObject({ reason: null });
    expect(playgroundState(status, pg({ blocked: { component: "ogx", reason: "The Gen AI playground is turned off." } }), "dev").next).toMatchObject({ reason: "The Gen AI playground is turned off." });
    expect(playgroundState(status, pg({ apiProblem: "This RHOAI release's dashboard gen-ai API differs." }), "dev").next).toMatchObject({ reason: expect.stringContaining("differs") });
    expect(playgroundState(status, null, "dev").next).toMatchObject({ reason: expect.stringContaining("loading") });
  });

  it("ready opens the dashboard's playground page for the project; ownership shows in the label only", () => {
    expect(playgroundState(status, pg({ items: [item("dev")] }), "dev")).toMatchObject({ status: "ready", label: "Ready in dev", next: { kind: "open", href: "https://rh-ai.apps.example.com/gen-ai-studio/playground/dev" } });
    expect(playgroundState(status, pg({ items: [item("dev", { managedByTool: false, requested: false })] }), "dev")).toMatchObject({ status: "ready", label: "Ready in dev · not managed by this tool" });
  });

  it("starting, failed, deleting and an unreported phase", () => {
    expect(playgroundState(status, pg({ items: [item("dev", { ready: false, phase: "Initializing" })] }), "dev")).toMatchObject({ status: "setting-up", next: null });
    expect(playgroundState(status, pg({ items: [item("dev", { ready: false, phase: "Failed", terminalError: true })] }), "dev")).toMatchObject({ status: "error", next: { kind: "show-issue", part: "playground" } });
    expect(playgroundState(status, pg({ items: [item("dev", { ready: false, phase: "Terminating", terminating: true })] }), "dev")).toMatchObject({ status: "deleting" });
    expect(playgroundState(status, pg({ items: [item("dev", { ready: false, phase: undefined, healthUnknown: true })] }), "dev")).toMatchObject({ status: "unknown" });
    // The operation running from this page shows at once.
    expect(playgroundState(status, pg(), "dev", { ...IDLE, playground: { action: "setup", project: "dev" } })).toMatchObject({ status: "setting-up" });
  });

  it("a record of a playground that is gone is not a playground", () => {
    expect(playgroundState(status, pg({ items: [item("dev", { deployed: false, ready: false, phase: undefined })] }), "dev")).toMatchObject({ status: "not-set-up" });
  });

  it("the Overview summarises the projects that have one, from the same function as the tab", () => {
    const st = pg({ items: [item("dev"), item("team", { ready: false, phase: "Failed", terminalError: true })] });
    const summary = playgroundState(status, st, null);
    expect(summary).toEqual({ ...playgroundState(status, st, "team"), label: "Error in 1 of 2 projects" });
    expect(playgroundState(status, pg({ items: [item("dev")] }), null)).toEqual(playgroundState(status, pg({ items: [item("dev")] }), "dev"));
    expect(playgroundState(status, pg({ items: [] }), null)).toMatchObject({ status: "not-set-up", label: "Not set up in any project" });
    expect(playgroundState(status, null, null)).toMatchObject({ status: "unknown" });
  });
});

describe("Playground clean-up and steps", () => {
  it("offers only the playgrounds this tool set up, before the models", () => {
    const st = pg({ items: [item("dev"), item("team", { managedByTool: false }), item("old", { deployed: false })] });
    expect(cleanupCandidates(status, "all", false, st).playgrounds).toEqual(["dev"]);
    expect(cleanupCandidates(status, "mlflow", false, st).playgrounds).toEqual([]);
    expect(cleanupSteps({ models: ["dev"], playgrounds: ["dev"] }).map((s) => s.id)).toEqual(["playground:dev", "model:dev"]);
    expect(playgroundSteps({ kind: "teardown-playground", project: "dev" }).map((s) => s.id)).toEqual(["delete", "gone"]);
  });

  it("the chat request quotes a model name for the shell and JSON", () => {
    const curl = playgroundCurl("https://rh-ai.apps.example.com", "dev", `it's "x" $(id)`, "maas");
    expect(curl).toContain(`-d '{"model":"it'\\''s \\"x\\" $(id)","model_source_type":"maas",`);
  });

  it("the chat request uses the user's own token and the plain model name", () => {
    const curl = playgroundCurl("https://rh-ai.apps.example.com/", "dev", "qwen25-05b-instruct", "namespace");
    expect(curl).toContain("TOKEN=$(oc whoami -t)");
    expect(curl).toContain('"https://rh-ai.apps.example.com/gen-ai/api/v1/lsd/responses?namespace=dev"');
    expect(curl).toContain('"model":"qwen25-05b-instruct","model_source_type":"namespace"');
  });
});

describe("Playground tab", () => {
  it("sets up with the project's own models by default and streams the run", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/playground": pg(),
      "POST /api/resources/playground/stream": sse(
        { step: "check", status: "success", message: "Ready to set up" },
        { step: "install", status: "success", message: "asked" },
        { step: "ready", status: "success", message: "The playground in dev is ready." },
        { step: "operation_complete", status: "success", message: "The playground in dev is ready." },
      ),
    });
    renderAt("/test-resources?tab=playground&project=dev");
    const models = await screen.findByRole("listitem", { name: "Models" });
    const table = within(models).getByRole("grid", { name: "Models the playground can use" });
    expect(within(table).getByText("Qwen2.5 0.5B Instruct (CPU)")).toBeInTheDocument();
    expect(within(table).getByRole("checkbox", { name: "Select row 0" })).toBeChecked();
    expect(within(table).getByRole("checkbox", { name: "Select row 1" })).not.toBeChecked();
    expect(within(table).getByText("Not used unless you choose it (needs a Models as a Service subscription)")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up the playground in dev" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up the playground in dev\?/ });
    expect(within(dialog).getByText(/with qwen25-05b-instruct/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    await screen.findByRole("dialog", { name: "Set up the playground in dev" });
    expect(api.bodies["POST /api/resources/playground/stream"]).toEqual([{ action: "setup", project: "dev", models: [{ name: "qwen25-05b-instruct", source: "namespace" }] }]);
  });

  it("this tool's embedding model is added as one; another embedding model is explained, not added", async () => {
    const granite: PlaygroundModel = { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", source: "namespace", status: "Running", ready: true, chat: false, embeddingDimension: 768 };
    const theirs: PlaygroundModel = { name: "bge-small", source: "namespace", status: "Running", ready: true, chat: false };
    const api = stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/playground": pg({ models: [qwen, granite, theirs] }),
      "POST /api/resources/playground/stream": sse({ step: "operation_complete", status: "success", message: "done" }),
    });
    renderAt("/test-resources?tab=playground&project=dev");
    const table = await screen.findByRole("grid", { name: "Models the playground can use" });
    expect(within(table).getByText("Embedding (768 dimensions), for searching documents")).toBeInTheDocument();
    expect(within(table).getByRole("checkbox", { name: "Select row 1" })).toBeChecked();
    expect(within(table).queryByText("bge-small")).toBeNull();
    const hint = screen.getByText(/Set up here does not add bge-small/);
    expect(hint).toHaveTextContent("choose Embedding as its type, and enter its embedding dimension");
    // A chat model is needed: the embedding model alone is not enough.
    fireEvent.click(within(table).getByRole("checkbox", { name: "Select row 0" }));
    expect(screen.getByRole("button", { name: "Set up the playground in dev" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(table).getByRole("checkbox", { name: "Select row 0" }));
    fireEvent.click(screen.getByRole("button", { name: "Set up the playground in dev" }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Set up the playground in dev\?/ })).getByRole("button", { name: "Set up" }));
    await screen.findByRole("dialog", { name: "Set up the playground in dev" });
    expect(api.bodies["POST /api/resources/playground/stream"]).toEqual([{ action: "setup", project: "dev", models: [{ name: "qwen25-05b-instruct", source: "namespace" }, { name: "granite-embedding-125m-english", source: "namespace" }] }]);
  });

  it("no ready chat model: Set up is disabled and points at the Models tab", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/playground": pg({ models: [] }) });
    renderAt("/test-resources?tab=playground&project=dev");
    expect(await screen.findByRole("button", { name: "Set up a model on the Models tab" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up the playground in dev" })).toHaveAttribute("aria-disabled", "true");
  });

  it("a model that is not ready is left out, and Set up stays available", async () => {
    const failed: PlaygroundModel = { name: "deepseek", source: "namespace", status: "Failed", ready: false, chat: true };
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/playground": pg({ models: [qwen, failed] }) });
    renderAt("/test-resources?tab=playground&project=dev");
    expect(await screen.findByText(/not ready, so Set up leaves them out: deepseek \(Failed\)/)).toBeInTheDocument();
    expect(screen.queryByText(/every model/i)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up the playground in dev" })).not.toHaveAttribute("aria-disabled", "true");
  });

  it("a playground someone else set up: open it, no Delete", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/playground": pg({ items: [item("dev", { managedByTool: false, requested: false })] }) });
    renderAt("/test-resources?tab=playground&project=dev");
    const part = await screen.findByRole("listitem", { name: "Playground" });
    expect(within(part).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(within(part).getByRole("link", { name: /Open the playground/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/gen-ai-studio/playground/dev");
    expect(within(part).queryByRole("button", { name: /actions/i })).toBeNull();
    expect(screen.queryByText("Clean up Playground...")).toBeNull();
  });

  it("the Overview row matches the tab", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/playground": pg({ items: [item("dev")] }) });
    renderAt("/test-resources");
    const row = await screen.findByRole("row", { name: /Playground/ });
    expect(await within(row).findByText("Ready in dev")).toBeInTheDocument();
  });
});

// No real waits: the status is read again on fake timers while a playground
// starts or is deleted, and not once it has settled.
describe("Playground polling", () => {
  const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));
  const reads = (api: { calls: string[] }) => api.calls.filter((c) => c.startsWith("GET /api/resources/playground")).length;

  it.each([
    ["starting", item("dev", { ready: false, phase: "Initializing" }), item("dev")],
    ["deleting", item("dev", { ready: false, phase: "Terminating", terminating: true }), undefined],
  ])("reads again while %s, then stops", async (_name, moving, settled) => {
    vi.useFakeTimers();
    try {
      let current: PlaygroundState | undefined = moving;
      const api = stubApi({
        "/api/resources/status": status,
        "/api/resources/projects": { projects: ["dev"] },
        "/api/resources/playground": () => new Response(JSON.stringify(pg({ items: current ? [current] : [] })), { status: 200, headers: { "Content-Type": "application/json" } }),
      });
      renderAt("/test-resources?tab=playground&project=dev");
      await tick();
      const first = reads(api);
      expect(first).toBeGreaterThan(0);
      await tick(PLAYGROUND_POLL_MS);
      expect(reads(api)).toBe(first + 1);
      current = settled;
      await tick(PLAYGROUND_POLL_MS);
      const after = reads(api);
      await tick(PLAYGROUND_POLL_MS * 3);
      expect(reads(api)).toBe(after);
      expect(screen.getByRole("listitem", { name: "Playground" })).toHaveTextContent(settled ? "Ready" : "Not set up");
    } finally {
      vi.useRealTimers();
    }
  });
});
