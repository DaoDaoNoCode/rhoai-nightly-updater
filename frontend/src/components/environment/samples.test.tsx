import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { jsonResponse, stubApi } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { MLflowSamplesStatus, PipelineSampleStatus, ResourceState, ResourcesStatus, SampleStatus } from "../../types";
import { mlflowSampleSteps, pipelineSampleSteps, runLabel, runTime, sampleLabel, sampleNote, samplesFor } from "./samples";

// No real waits: every test runs on fake timers and moves them inside act().
const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));
beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-17T12:00:00Z"));
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const MLFLOW_URL = "https://rh-ai.apps.example.com/mlflow";
const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running", uiRoute: MLFLOW_URL };
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", usedBy: ["dev/nightly-dspa"] };
const dspa = (ns: string, over: Partial<ResourceState> = {}): ResourceState => ({
  deployed: true, ready: true, managedByTool: true, namespace: ns, name: "nightly-dspa", message: "Running",
  uiRoute: `https://rh-ai.apps.example.com/develop-train/pipelines/definitions/${ns}`, ...over,
});

const sample = (id: SampleStatus["id"], state: SampleStatus["state"], over: Partial<SampleStatus> = {}): SampleStatus => ({
  id, name: id, state,
  actions: state === "absent" || state === "removed" ? ["add"] : state === "present" ? (id === "genai-demo" ? ["regenerate"] : ["remove"]) : state === "partial" ? (id === "genai-demo" ? ["regenerate"] : ["add", "remove"]) : [],
  ...over,
});
const DEMO_HINT = "The updater cannot remove the demo (it holds no right to delete MLflow data it did not name); remove it from the MLflow UI. Clean up leaves it in place.";
const mlflowSamples = (samples: SampleStatus[], over: Partial<MLflowSamplesStatus> = {}): MLflowSamplesStatus => ({ project: "dev", available: true, mlflowURL: MLFLOW_URL, samples, ...over });

const status = (over: Partial<ResourcesStatus> = {}): ResourcesStatus => ({ minio: readyMinio, mlflow: ownMlflow, pipelineServers: [dspa("dev")], ...over });

function renderAt(path: string, props: Partial<React.ComponentProps<typeof QuickResourceCreator>> = {}) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} {...props} /></MemoryRouter>);
}

const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
const samplesSection = () => screen.getByRole("region", { name: "Sample data" });
const row = (name: string) => within(samplesSection()).getByText(name, { selector: "strong" }).closest("li") as HTMLElement;
const calls = (api: { calls: string[] }, prefix: string) => api.calls.filter((c) => c.startsWith(prefix)).length;

describe("sample words (samples.tsx)", () => {
  it("maps every state to one label of the page's vocabulary; someone else's sample has no health label", () => {
    expect(sampleLabel(sample("training", "absent"), null)).toEqual({ status: "not-set-up", text: "Not added" });
    expect(sampleLabel(sample("training", "present"), null)).toEqual({ status: "ready", text: "Added" });
    expect(sampleLabel(sample("training", "partial"), null)).toEqual({ status: "needs-attention", text: "Partly added" });
    expect(sampleLabel(sample("training", "removed"), null)).toEqual({ status: "not-set-up", text: "Removed" });
    expect(sampleLabel(sample("training", "foreign"), null)).toBeNull();
    expect(sampleLabel(sample("skill", "unsupported"), null)).toEqual({ status: "unknown", text: "Not available" });
    expect(sampleLabel(sample("skill", "unknown"), null)).toEqual({ status: "unknown", text: "Unknown" });
    const op = { feature: "mlflow" as const, project: "dev", action: "remove" as const, samples: ["training" as const] };
    expect(sampleLabel(sample("training", "present"), op)).toEqual({ status: "deleting", text: "Removing" });
    expect(sampleLabel(sample("skill", "present"), op)).toEqual({ status: "ready", text: "Added" });
  });

  it("explains partial, foreign, unsupported and unknown states", () => {
    expect(sampleNote(sample("training", "partial", { missing: ["registered model"] }))).toBe("Only part of it is there. Missing: registered model. Add completes it; Remove deletes what is there.");
    expect(sampleNote(sample("genai-demo", "partial", { message: "An earlier Add or Regenerate did not finish; Regenerate completes it.", missing: ["evaluation"] })))
      .toBe("An earlier Add or Regenerate did not finish; Regenerate completes it. Missing: evaluation.");
    expect(sampleNote(sample("mcp-server", "foreign", { message: "MCP server 'x' exists but was not created by this tool." }))).toBe("MCP server 'x' exists but was not created by this tool.");
    expect(sampleNote(sample("skill", "unsupported", { message: "This MLflow has no Skill Registry." }))).toBe("This MLflow has no Skill Registry. This sample cannot be added here.");
    expect(sampleNote(sample("skill", "unknown", { message: "Cannot read the sample's state: 503." }))).toBe("Cannot read the sample's state: 503.");
    expect(sampleNote(sample("training", "absent"))).toBeNull();
  });

  it("Add all and Remove all take only the samples whose state allows it, in the server's order", () => {
    const list = [sample("skill", "absent"), sample("genai-demo", "partial"), sample("training", "present"), sample("mcp-server", "removed")];
    expect(samplesFor(list, "add")).toEqual(["mcp-server", "skill"]);
    expect(samplesFor(list, "remove")).toEqual(["training"]);
  });

  it("names the steps the server streams: removal in reverse; the pipeline's optional steps only once reported", () => {
    expect(mlflowSampleSteps("remove", ["skill", "training"]).map((s) => s.id)).toEqual(["check", "skill", "training"]);
    expect(mlflowSampleSteps("add", ["skill", "genai-demo"]).map((s) => s.title)).toEqual(["Check MLflow", "Add the agent and prompt demo", "Add the agent skill"]);
    expect(pipelineSampleSteps("run").filter((s) => s.optional).map((s) => s.id)).toEqual(["experiment", "pipeline"]);
  });

  it("reads a run's state and time in words", () => {
    expect(runLabel(undefined)).toEqual({ status: "not-set-up", text: "Not run yet" });
    expect(runLabel({ id: "r", state: "RUNNING" }).text).toBe("Running");
    expect(runLabel({ id: "r", state: "FAILED" }).status).toBe("error");
    expect(runLabel({ id: "r", state: "SUCCEEDED" }).status).toBe("ready");
    const now = Date.parse("2026-11-17T12:00:00Z");
    expect(runTime({ id: "r", state: "RUNNING", createdAt: "2026-11-17T11:58:40Z" }, now)).toBe("for 1m 20s");
    expect(runTime({ id: "r", state: "SUCCEEDED", createdAt: "2026-11-17T11:50:00Z", finishedAt: "2026-11-17T11:55:00Z" }, now)).toBe("finished 5m ago");
  });
});

describe("MLflow tab: Sample data", () => {
  const everyState = mlflowSamples([
    sample("genai-demo", "present", { message: DEMO_HINT, link: `${MLFLOW_URL}/#/experiments/7?workspace=dev` }),
    sample("training", "partial", { missing: ["registered model"], link: `${MLFLOW_URL}/#/experiments/9?workspace=dev` }),
    sample("mcp-server", "foreign", { message: "MCP server 'com.deepwiki/rhoai-nightly-sample' exists but was not created by this tool (it lacks the rhoai-nightly-updater.sample tag), so it is left unchanged." }),
    sample("skill", "unsupported", { message: "This MLflow has no Skill Registry." }),
  ]);

  it("shows one row per sample with its value, state, what the state means and only the actions it allows", async () => {
    stubApi({ "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] }, "/api/resources/samples/mlflow": everyState });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    const demo = row("Agent and prompt demo");
    expect(demo).toHaveTextContent("Agent traces, prompts and evaluations of a sample agent. In the dashboard: Agent traces, Prompt management.");
    expect(within(demo).getByText("Added")).toBeInTheDocument();
    expect(within(demo).getByRole("button", { name: "Regenerate agent and prompt demo" })).toBeInTheDocument();
    // The demo is never removed by the tool: its note says how.
    expect(within(demo).queryByRole("button", { name: /^Remove/ })).not.toBeInTheDocument();
    expect(demo).toHaveTextContent("remove it from the MLflow UI");
    expect(within(demo).getByRole("link", { name: "Open in MLflow" })).toHaveAttribute("href", `${MLFLOW_URL}/#/experiments/7?workspace=dev`);

    const training = row("Training experiment and registered model");
    expect(within(training).getByText("Partly added")).toBeInTheDocument();
    expect(training).toHaveTextContent("Missing: registered model. Add completes it; Remove deletes what is there.");
    expect(within(training).getByRole("button", { name: "Add training experiment and registered model" })).toBeInTheDocument();
    expect(within(training).getByRole("button", { name: "Remove training experiment and registered model" })).toBeInTheDocument();

    const mcp = row("MCP server");
    expect(within(mcp).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(mcp).toHaveTextContent("was not created by this tool");
    expect(within(mcp).queryByRole("button")).not.toBeInTheDocument();

    const skill = row("Agent skill");
    expect(within(skill).getByText("Not available")).toBeInTheDocument();
    expect(skill).toHaveTextContent("This MLflow has no Skill Registry. This sample cannot be added here.");
    expect(within(skill).queryByRole("button")).not.toBeInTheDocument();
  });

  it("shows absent, removed and unknown samples with their words", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([
        sample("genai-demo", "absent"),
        sample("training", "removed", { message: "Removed: the experiment is in MLflow's deleted view. Add restores it." }),
        sample("mcp-server", "unknown", { message: "Cannot read the sample's state: MLflow answered 503." }),
        sample("skill", "absent"),
      ]),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    expect(within(row("Agent and prompt demo")).getByText("Not added")).toBeInTheDocument();
    expect(within(row("Training experiment and registered model")).getByText("Removed")).toBeInTheDocument();
    expect(row("Training experiment and registered model")).toHaveTextContent("Add restores it.");
    expect(within(row("MCP server")).getByText("Unknown")).toBeInTheDocument();
    expect(row("MCP server")).toHaveTextContent("MLflow answered 503.");
  });

  it("Add all is one confirmed request for every sample it can add; the run ends with Open <project> in MLflow and the state is read again", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("genai-demo", "absent"), sample("training", "removed"), sample("mcp-server", "present"), sample("skill", "unsupported")]),
      "POST /api/resources/samples/mlflow/stream": sse(
        { step: "check", status: "success", message: "MLflow is ready at " + MLFLOW_URL, elapsedMs: 1 },
        { step: "genai-demo", status: "success", message: "Agent and prompt demo: generated.", elapsedMs: 2 },
        { step: "training", status: "success", message: "Training experiment and registered model: restored.", elapsedMs: 3 },
        { step: "operation_complete", status: "success", message: "Samples added in project 'dev'.", elapsedMs: 4 },
      ),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    expect(calls(api, "GET /api/resources/samples/mlflow?project=dev")).toBe(1);
    fireEvent.click(within(samplesSection()).getByRole("button", { name: "Add all" }));
    await tick();
    const confirm = screen.getByRole("dialog", { name: /Add sample data to dev\?/ });
    expect(confirm).toHaveTextContent("In dev's MLflow workspace: the agent and prompt demo (about 30 seconds) and a training experiment with a registered model.");
    fireEvent.click(within(confirm).getByRole("button", { name: "Add" }));
    await tick();
    expect(api.bodies["POST /api/resources/samples/mlflow/stream"]).toEqual([{ project: "dev", action: "add", samples: ["genai-demo", "training"] }]);
    const run = screen.getByRole("dialog", { name: "Add sample data to dev" });
    expect(within(run).getByText("Add the agent and prompt demo")).toBeInTheDocument();
    expect(within(run).getByText("Samples added in project 'dev'.")).toBeInTheDocument();
    expect(within(run).getByRole("link", { name: /Open dev in MLflow/ })).toHaveAttribute("href", `${MLFLOW_URL}/#/experiments?workspace=dev`);
    expect(calls(api, "GET /api/resources/samples/mlflow?project=dev")).toBe(2);
  });

  it("Remove all removes only what this tool added (never the demo), as one confirmed request", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("genai-demo", "present"), sample("training", "present"), sample("mcp-server", "absent"), sample("skill", "present")]),
      "POST /api/resources/samples/mlflow/stream": sse({ step: "operation_complete", status: "success", message: "Samples removed in project 'dev'.", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    fireEvent.click(within(samplesSection()).getByRole("button", { name: "Remove all" }));
    await tick();
    const confirm = screen.getByRole("dialog", { name: /Remove the sample data from dev\?/ });
    expect(confirm).toHaveTextContent("The agent and prompt demo stays; remove it in the MLflow UI.");
    fireEvent.click(within(confirm).getByRole("button", { name: "Remove" }));
    await tick();
    expect(api.bodies["POST /api/resources/samples/mlflow/stream"]).toEqual([{ project: "dev", action: "remove", samples: ["training", "skill"] }]);
    // A removal has no "open" step.
    expect(within(screen.getByRole("dialog", { name: "Remove the sample data from dev" })).queryByRole("link", { name: /in MLflow/ })).not.toBeInTheDocument();
  });

  it("Regenerate on a partial demo sends regenerate for the demo only", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("genai-demo", "partial", { message: "An earlier Add or Regenerate did not finish; Regenerate completes it.", missing: ["evaluation"] })]),
      "POST /api/resources/samples/mlflow/stream": sse({ step: "operation_complete", status: "success", message: "Samples regenerated in project 'dev'.", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    expect(within(row("Agent and prompt demo")).getByText("Partly added")).toBeInTheDocument();
    fireEvent.click(within(row("Agent and prompt demo")).getByRole("button", { name: "Regenerate agent and prompt demo" }));
    await tick();
    fireEvent.click(within(screen.getByRole("dialog", { name: /Regenerate the agent and prompt demo in dev\?/ })).getByRole("button", { name: "Regenerate" }));
    await tick();
    expect(api.bodies["POST /api/resources/samples/mlflow/stream"]).toEqual([{ project: "dev", action: "regenerate", samples: ["genai-demo"] }]);
  });

  it("a row shows its operation while it runs, and every action says why it waits", async () => {
    const stream = sseResponse();
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("training", "absent"), sample("skill", "absent")]),
      "POST /api/resources/samples/mlflow/stream": () => stream.response,
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    fireEvent.click(within(row("Agent skill")).getByRole("button", { name: "Add agent skill" }));
    await tick();
    fireEvent.click(within(screen.getByRole("dialog", { name: /Add the agent skill to dev\?/ })).getByRole("button", { name: "Add" }));
    await tick();
    stream.push(sseEvent("skill", "running", "Agent skill: add..."));
    await tick();
    fireEvent.click(within(screen.getByRole("dialog", { name: "Add the agent skill to dev" })).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!);
    await tick();
    expect(within(row("Agent skill")).getByText("Adding")).toBeInTheDocument();
    expect(within(row("Training experiment and registered model")).getByText("Not added")).toBeInTheDocument();
    const add = within(row("Training experiment and registered model")).getByRole("button", { name: "Add training experiment and registered model" });
    expect(add).toHaveAttribute("aria-disabled", "true");
    expect(add).toHaveAccessibleDescription("Another operation is in progress");
    stream.push(sseEvent("operation_complete", "success", "Samples added in project 'dev'."));
    stream.close();
    await tick();
    expect(within(row("Agent skill")).queryByText("Adding")).not.toBeInTheDocument();
  });

  it("the server's per-user limit ends the run with its message", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("training", "absent")]),
      "POST /api/resources/samples/mlflow/stream": () => jsonResponse({ error: "Too many requests. Please wait 30 seconds before retrying.", errorCode: "rate_limited" }, 429),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    fireEvent.click(within(row("Training experiment and registered model")).getByRole("button", { name: "Add training experiment and registered model" }));
    await tick();
    fireEvent.click(within(screen.getByRole("dialog", { name: /Add the training experiment and registered model to dev\?/ })).getByRole("button", { name: "Add" }));
    await tick();
    expect(screen.getByRole("dialog", { name: "Add the training experiment and registered model to dev" })).toHaveTextContent("Too many requests. Please wait 30 seconds before retrying.");
  });

  it("read-only: every action is disabled with the reason shown", async () => {
    stubApi({ "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] }, "/api/resources/samples/mlflow": mlflowSamples([sample("training", "absent"), sample("skill", "present")]) });
    renderAt("/test-resources?tab=mlflow&project=dev", { readBlocker: "You are not a cluster-admin." });
    await tick();
    const reason = "Read-only access: adding or removing sample data needs a cluster-admin. Ask one to do it for you.";
    expect(within(samplesSection()).getByText(reason)).toBeInTheDocument();
    for (const name of ["Add all", "Remove all", "Add training experiment and registered model", "Remove agent skill"]) {
      const b = within(samplesSection()).getByRole("button", { name });
      expect(b).toHaveAttribute("aria-disabled", "true");
      expect(b).toHaveAccessibleDescription(reason);
    }
  });

  it("Add all and Remove all say why when they have nothing to do", async () => {
    stubApi({ "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] }, "/api/resources/samples/mlflow": mlflowSamples([sample("genai-demo", "present"), sample("training", "present")]) });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    expect(within(samplesSection()).getByRole("button", { name: "Add all" })).toHaveAccessibleDescription("Add all: Every sample that can be added is added already.");
    expect(within(samplesSection()).getByRole("button", { name: "Remove all" })).not.toHaveAttribute("aria-disabled");
  });

  it("when MLflow is not set up, explains it and links to Set up; nothing is read", async () => {
    const api = stubApi({ "/api/resources/status": status({ mlflow: none }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    const alert = within(samplesSection()).getByText("Sample data needs a working MLflow").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("MLflow, which is not set up yet");
    fireEvent.click(within(alert).getByRole("button", { name: "Set up MLflow..." }));
    await tick();
    expect(screen.getByRole("dialog", { name: /Set up MLflow\?/ })).toBeInTheDocument();
    expect(calls(api, "GET /api/resources/samples/mlflow")).toBe(0);
  });

  it("the server's reason when samples are not available, and a read error with Try again", async () => {
    let fail = true;
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev", "other"] },
      "/api/resources/samples/mlflow": (url: string) => url.includes("project=other")
        ? jsonResponse(mlflowSamples([], { project: "other", available: false, reason: "MLflow reports no https address (status.url) yet." }))
        : fail ? jsonResponse({ error: "Cannot read the MLflow samples: timeout", errorCode: "network" }, 503) : jsonResponse(mlflowSamples([sample("training", "absent")])),
    });
    renderAt("/test-resources?tab=mlflow&project=dev");
    await tick();
    const alert = within(samplesSection()).getByText("Cannot read the sample data of dev").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("timeout");
    fail = false;
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }));
    await tick();
    expect(within(row("Training experiment and registered model")).getByText("Not added")).toBeInTheDocument();
    cleanupAndRender("/test-resources?tab=mlflow&project=other");
    await tick();
    expect(within(samplesSection()).getByText("MLflow reports no https address (status.url) yet.")).toBeInTheDocument();
  });

  it("without a project, asks for one", async () => {
    stubApi({ "/api/resources/status": status({ pipelineServers: [] }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=mlflow&project=");
    await tick();
    expect(within(samplesSection()).getByText("Choose a project under Use it to see its sample data.")).toBeInTheDocument();
  });
});

function cleanupAndRender(path: string) {
  cleanup();
  renderAt(path);
}

describe("Pipelines tab: Sample pipeline", () => {
  const pipelineStatus = (over: Partial<PipelineSampleStatus> = {}): PipelineSampleStatus => ({
    project: "dev", available: true, server: "nightly-dspa", image: "registry.access.redhat.com/ubi9/ubi-minimal@sha256:" + "a".repeat(64),
    state: "absent", runs: 0, runsLink: "https://rh-ai.apps.example.com/develop-train/pipelines/runs/dev/runs", ...over,
  });
  const section = () => screen.getByRole("region", { name: "Sample pipeline" });
  const runLink = "https://rh-ai.apps.example.com/develop-train/pipelines/runs/dev/runs/r1";

  it("Run is one confirmed request; the steps created only when missing show only when reported", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus(),
      "POST /api/resources/samples/pipeline/stream": sse(
        { step: "check", status: "success", message: "The pipeline server is ready.", elapsedMs: 1 },
        { step: "experiment", status: "success", message: "Created experiment 'rhoai-nightly-sample'", elapsedMs: 2 },
        { step: "run", status: "success", message: "Started run r1: " + runLink, elapsedMs: 3 },
        { step: "operation_complete", status: "success", message: "Sample pipeline run started. It usually finishes in 1-2 minutes.", elapsedMs: 4 },
      ),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(within(section()).getByText("Not run yet")).toBeInTheDocument();
    expect(within(section()).queryByRole("button", { name: "Remove sample" })).not.toBeInTheDocument();
    fireEvent.click(within(section()).getByRole("button", { name: "Run sample pipeline" }));
    await tick();
    const confirm = screen.getByRole("dialog", { name: /Run the sample pipeline in dev\?/ });
    expect(confirm).toHaveTextContent("The first step writes a small file to the S3 storage, the second reads it back.");
    fireEvent.click(within(confirm).getByRole("button", { name: "Run" }));
    await tick();
    expect(api.bodies["POST /api/resources/samples/pipeline/stream"]).toEqual([{ project: "dev", action: "run" }]);
    const run = screen.getByRole("dialog", { name: "Run the sample pipeline in dev" });
    expect(within(run).getByText("Create the sample's experiment")).toBeInTheDocument();
    expect(within(run).queryByText("Upload the sample pipeline")).not.toBeInTheDocument();
    expect(within(run).getByText("Sample pipeline run started. It usually finishes in 1-2 minutes.")).toBeInTheDocument();
    expect(calls(api, "GET /api/resources/samples/pipeline?project=dev")).toBe(2);
  });

  it("polls every 5 seconds while the run is active, with its elapsed time, and stops once it ends", async () => {
    let state = "RUNNING";
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": () => jsonResponse(pipelineStatus({ state: "present", runs: 1, latestRun: { id: "r1", state, createdAt: "2026-11-17T11:59:00Z", link: runLink, ...(state === "SUCCEEDED" ? { finishedAt: "2026-11-17T12:00:05Z" } : {}) } })),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(within(section()).getByText("Running")).toBeInTheDocument();
    expect(within(section()).getByText("for 1m 0s")).toBeInTheDocument();
    expect(within(section()).getByRole("link", { name: /View run in dashboard/ })).toHaveAttribute("href", runLink);
    expect(within(section()).getByText("1 run of the sample in dev")).toBeInTheDocument();
    await tick(5_000);
    expect(calls(api, "GET /api/resources/samples/pipeline")).toBe(2);
    expect(within(section()).getByText("for 1m 5s")).toBeInTheDocument();
    state = "SUCCEEDED";
    await tick(5_000);
    expect(within(section()).getByText("Succeeded")).toBeInTheDocument();
    await tick(60_000);
    expect(calls(api, "GET /api/resources/samples/pipeline")).toBe(3);
  });

  it("stops polling a run that does not end after 10 minutes, with Check again", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ state: "present", runs: 1, latestRun: { id: "r1", state: "PENDING", createdAt: "2026-11-17T12:00:00Z" } }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    await tick(11 * 60_000);
    const n = calls(api, "GET /api/resources/samples/pipeline");
    await tick(60_000);
    expect(calls(api, "GET /api/resources/samples/pipeline")).toBe(n);
    fireEvent.click(within(section()).getByRole("button", { name: "Check again" }));
    await tick();
    expect(calls(api, "GET /api/resources/samples/pipeline")).toBe(n + 1);
  });

  it("a failed run shows the reason and its link", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ state: "present", runs: 2, latestRun: { id: "r1", state: "FAILED", error: "task read-artifact failed", link: runLink, createdAt: "2026-11-17T11:50:00Z", finishedAt: "2026-11-17T11:52:00Z" } }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(within(section()).getByText("Failed")).toBeInTheDocument();
    const alert = within(section()).getByText("The sample run failed").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("task read-artifact failed");
    expect(within(alert).getByRole("link", { name: /View run in dashboard/ })).toHaveAttribute("href", runLink);
  });

  it("Remove says how many runs go and that the files stay, then sends one request", async () => {
    const api = stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ state: "present", runs: 3, latestRun: { id: "r1", state: "SUCCEEDED" } }),
      "POST /api/resources/samples/pipeline/stream": sse(
        { step: "check", status: "success", message: "ok", elapsedMs: 1 },
        { step: "runs", status: "success", message: "Deleted 3 runs", elapsedMs: 2 },
        { step: "pipeline", status: "success", message: "Deleted pipeline", elapsedMs: 3 },
        { step: "experiment", status: "skipped", message: "Kept experiment 'rhoai-nightly-sample': it holds 1 run this tool did not start.", elapsedMs: 4 },
        { step: "operation_complete", status: "success", message: "Sample pipeline removed.", elapsedMs: 5 },
      ),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    fireEvent.click(within(section()).getByRole("button", { name: "Remove sample" }));
    await tick();
    const confirm = screen.getByRole("dialog", { name: /Remove the sample pipeline from dev\?/ });
    expect(confirm).toHaveTextContent("Deletes the sample's 3 runs, its pipeline and its experiment from dev. The files its runs wrote stay in the S3 storage");
    fireEvent.click(within(confirm).getByRole("button", { name: "Remove" }));
    await tick();
    expect(api.bodies["POST /api/resources/samples/pipeline/stream"]).toEqual([{ project: "dev", action: "remove" }]);
    const run = screen.getByRole("dialog", { name: "Remove the sample pipeline from dev" });
    expect(within(run).getByText("Delete the sample's experiment")).toBeInTheDocument();
    expect(within(run).queryByText("Remove the sample")).not.toBeInTheDocument();
  });

  it("a pipeline server this tool did not create: explained, not offered, nothing read", async () => {
    const api = stubApi({
      "/api/resources/status": status({ pipelineServers: [], unmanagedPipelineProjects: ["team"], unmanagedPipelineServers: [dspa("team", { managedByTool: false, name: "dspa" })] }),
      "/api/resources/projects": { projects: ["team"] },
    });
    renderAt("/test-resources?tab=pipelines&project=team");
    await tick();
    expect(section()).toHaveTextContent("The one in team was created outside this tool");
    expect(within(section()).queryByRole("button")).not.toBeInTheDocument();
    expect(calls(api, "GET /api/resources/samples/pipeline")).toBe(0);
  });

  it("someone else's experiment or pipeline of the sample's name: explained, no Run", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ state: "foreign", message: "Experiment 'rhoai-nightly-sample' was not created by this tool." }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(section()).toHaveTextContent("Experiment 'rhoai-nightly-sample' was not created by this tool. It is left alone, so the sample cannot run here.");
    expect(within(section()).queryByRole("button", { name: "Run sample pipeline" })).not.toBeInTheDocument();
  });

  it("the server's reason when the sample is not available", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ available: false, reason: "The pipeline server reports no https API address (status.components.apiServer); its authenticated endpoint may be turned off." }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(section()).toHaveTextContent("its authenticated endpoint may be turned off");
  });

  it("read-only: Run and Remove are disabled with the reason shown", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/pipeline": pipelineStatus({ state: "present", runs: 1, latestRun: { id: "r1", state: "SUCCEEDED" } }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev", { readBlocker: "You are not a cluster-admin." });
    await tick();
    const reason = "Read-only access: adding or removing sample data needs a cluster-admin. Ask one to do it for you.";
    for (const name of ["Run sample pipeline", "Remove sample"]) {
      const b = within(section()).getByRole("button", { name });
      expect(b).toHaveAttribute("aria-disabled", "true");
      expect(b).toHaveAccessibleDescription(reason);
    }
  });
});

describe("Overview: samples as the next step", () => {
  it("a ready feature links to its samples, on its tab", async () => {
    stubApi({
      "/api/resources/status": status(), "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/samples/mlflow": mlflowSamples([sample("training", "absent")]),
      "/api/resources/samples/pipeline": { project: "dev", available: true, image: "x", state: "absent", runs: 0 },
    });
    renderAt("/test-resources");
    await tick();
    const table = screen.getByRole("grid", { name: "Features" });
    expect(within(table).getByRole("button", { name: "Run a sample pipeline" })).toBeInTheDocument();
    fireEvent.click(within(table).getByRole("button", { name: "Add sample data" }));
    await tick();
    expect(screen.getByRole("heading", { name: "Sample data" })).toHaveFocus();
  });

  it("no samples link while a feature is not ready, or on someone else's pipeline server", async () => {
    stubApi({
      "/api/resources/status": status({ mlflow: none, pipelineServers: [], unmanagedPipelineProjects: ["team"], unmanagedPipelineServers: [dspa("team", { managedByTool: false, name: "dspa" })] }),
      "/api/resources/projects": { projects: ["team"] },
    });
    renderAt("/test-resources");
    await tick();
    const table = screen.getByRole("grid", { name: "Features" });
    expect(within(table).queryByRole("button", { name: "Add sample data" })).not.toBeInTheDocument();
    expect(within(table).queryByRole("button", { name: "Run a sample pipeline" })).not.toBeInTheDocument();
  });
});
