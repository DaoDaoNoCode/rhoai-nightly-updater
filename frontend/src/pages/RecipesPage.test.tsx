import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { RecipesPage } from "./RecipesPage";
import { StatusPage } from "./StatusPage";
import { App } from "../App";
import { GlobalBanners } from "../components/GlobalBanners";
import { RecipeRunNote } from "../components/recipes/RecipeEntry";
import { RECIPE_RUN_POLL_MS } from "../constants";
import type { CleanupRequest } from "../services/api";
import { jsonResponse, stubApi } from "../test/apiStub";
import { contract } from "../test/contract";
import { IDLE_OPERATION, nightlyStatus, renderPage, settleApps } from "../test/providers";
import { sseEvent, sseResponse } from "../test/utils";
import type { OperationStatusResponse, RecipePlan, RecipeRun, RecipeRunStatus, RecipeRunStep, ResourcesStatus } from "../types";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** A closed PatternFly Modal leaves the page once its close effect ran (it may animate). */
async function dialogClosed(): Promise<void> {
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
}

/**
 * Lets the page's own requests (stubbed: they answer at once) and the app
 * reads they start answer, all inside act: a click that sends a request is
 * followed by this, not by a findBy that would apply the answers outside act.
 */
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)); });
    await settleApps();
  }
}

/** The steps of the contract's recipe (dev: RHOAI, the DSC, project recipe-dev, Pipelines, MLflow), as the runner lists them. */
const STEPS: [string, string][] = [
  ["rhoai", "RHOAI"], ["dsc", "DataScienceCluster"], ["project", "Project recipe-dev"], ["pipelines-prereq", "What Pipelines needs"],
  ["s3", "S3 storage"], ["pipeline-server", "Pipeline server"], ["mlflow-prereq", "What MLflow needs"], ["mlflow", "MLflow"],
];

/** How long each step usually takes and at most, in seconds (pkg/cluster internal/recipe Step.Seconds). */
const SECONDS: Record<string, [number, number]> = {
  rhoai: [420, 900], dsc: [60, 420], project: [10, 60], "pipelines-prereq": [120, 600], s3: [60, 300], "pipeline-server": [120, 600], "mlflow-prereq": [120, 600], mlflow: [120, 600],
};

/** The run's steps: the first `done` set up by the run (RHOAI found installed), then `current` with its status, the rest pending. */
function steps(done: number, current: RecipeRunStep["status"] = "running", message = ""): RecipeRunStep[] {
  return STEPS.map(([id, title], i) => {
    const [typicalSeconds, maxSeconds] = SECONDS[id];
    if (i < done) {
      return id === "rhoai"
        ? { id, title, typicalSeconds, maxSeconds, status: "skipped", message: "Already set up: RHOAI 3.6.0 is installed.", startedAt: "2026-11-20T14:05:00Z", finishedAt: "2026-11-20T14:06:00Z" }
        : { id, title, typicalSeconds, maxSeconds, status: "success", created: true, message: `${title} is set up.`, startedAt: "2026-11-20T14:05:00Z", finishedAt: "2026-11-20T14:06:00Z" };
    }
    if (i === done) return { id, title, typicalSeconds, maxSeconds, status: current, message, startedAt: new Date(Date.now() - 65_000).toISOString() };
    return { id, title, typicalSeconds, maxSeconds, status: "pending" };
  });
}

/** A run of the contract's recipe in this updater process. */
function runStatus(run: Partial<RecipeRun>, status: Partial<RecipeRunStatus> = {}): RecipeRunStatus {
  const base = contract.recipeRun().run!;
  return { run: { ...base, bootId: "this-process", pod: "pod-1", resumedAt: undefined, steps: steps(3), ...run }, running: false, ...status };
}

/** The contract's plan without its problems: RHOAI runs, the rest is checked later or set up. */
function okPlan(): RecipePlan {
  const plan = contract.recipePlan();
  return { ...plan, problems: [], steps: plan.steps.map((s) => (s.action === "blocked" ? { ...s, action: "run", reason: undefined } : s)) };
}

const RECIPE_OPERATION: OperationStatusResponse = {
  inProgress: true,
  operation: { id: "op1", type: "recipe", label: "Environment recipe", user: "me", target: "dev: step 4 of 8 (What Pipelines needs)", startedAt: new Date(Date.now() - 90_000).toISOString(), step: "pipelines-prereq", message: "Turning on aipipelines" },
};

const editorReads = {
  "/api/nightly-tags": { tags: [{ tag: "rhoai-3.6", image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64) }, { tag: "rhoai-3.5", image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:" + "b".repeat(64) }] },
  "/api/resources/projects": { projects: ["team-a"] },
};

const NO_RUN: RecipeRunStatus = { run: null, running: false };

/** The Environment status with the S3 storage, MLflow and a pipeline server in each of `servers` up. */
const environmentUp = (...servers: string[]): ResourcesStatus => ({
  minio: { deployed: true, ready: true },
  mlflow: { deployed: true, ready: true },
  pipelineServers: servers.map((namespace) => ({ deployed: true, ready: true, namespace })),
});

/** The texts of the dialog's "Left in place" list. */
const leftInPlaceItems = (dialog: HTMLElement) =>
  within(within(dialog).getByRole("list", { name: "Not deleted by this Clean up" })).getAllByRole("listitem").map((li) => li.textContent);

describe("Recipes page", () => {
  it("new: the editor's defaults, Review and run plans the recipe, and Run recipe starts it", async () => {
    let status: RecipeRunStatus = NO_RUN;
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/recipes/plan": okPlan(),
      "POST /api/recipes/run": () => { status = runStatus({ steps: steps(0, "running") }, { running: true, step: { step: "rhoai", status: "running", message: "Checking RHOAI" } }); return jsonResponse({ runId: "run1" }, 202); },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    expect(screen.getByRole("heading", { level: 1, name: "Recipes" })).toBeInTheDocument();
    expect(document.title).toBe("Recipes — RHOAI Nightly Updater");
    expect(await screen.findByRole("heading", { level: 2, name: "Run a recipe" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Recipe name" })).toHaveValue("dev");
    expect(screen.getByRole("radio", { name: "The newest nightly" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Keep it" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Create the default DataScienceCluster when there is none" })).toBeChecked();
    expect(screen.getByRole("textbox", { name: "Name" })).toHaveValue("recipe-dev");
    expect(screen.getByRole("switch", { name: "Pipelines" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "MLflow" })).toBeChecked();

    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    const dialog = screen.getByRole("dialog", { name: "Run recipe dev?" });
    expect(within(dialog).getByText(/The run usually takes 13 minutes, at most 52 minutes\./)).toBeInTheDocument();
    expect(dialog).toHaveTextContent("It installs rhoai-3.6 build aaaaaaaaaaaa, and keeps it if you resume.");
    expect(dialog).toHaveTextContent("Teammates cannot change this cluster until the run ends.");
    const plan = within(dialog).getByRole("list", { name: "Steps of this operation" });
    expect(plan).toHaveTextContent("RHOAIRuns: usually 7 minutes, up to 15 minutes.");
    expect(plan).toHaveTextContent("S3 storageAlready set up: The S3 storage is set up and ready.");
    expect(plan).toHaveTextContent("DataScienceClusterLater: usually 1 minute, up to 7 minutes.");
    // Said once, above the steps.
    expect(within(dialog).getAllByText(/checked when the run gets there/i)).toHaveLength(1);
    expect(api.bodies["POST /api/recipes/plan"]).toEqual([{ recipe: {
      apiVersion: "rhoai-nightly-updater/v1alpha1", kind: "EnvironmentRecipe", metadata: { name: "dev" },
      spec: {
        rhoai: { build: "latest", ifInstalled: "keep" }, dataScienceCluster: { create: true },
        project: { mode: "create", name: "recipe-dev", displayName: "Recipe dev" },
        features: { pipelines: { enabled: true }, mlflow: { enabled: true }, models: { enabled: false }, playground: { enabled: false }, maas: { enabled: false } },
        dashboardDev: { deploy: "none" },
      },
    } }]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Run recipe" }));
    await settle();
    expect(screen.getByRole("heading", { level: 2, name: "Recipe dev" })).toBeInTheDocument();
    expect(api.bodies["POST /api/recipes/run"]).toEqual(api.bodies["POST /api/recipes/plan"]);
    await dialogClosed();
    // One run at a time: no editor while it runs.
    expect(screen.queryByRole("button", { name: "Review and run" })).not.toBeInTheDocument();
    expect(screen.getByText(/Checking RHOAI\. /)).toBeInTheDocument();
  });

  it("review: a plan with problems lists them, with a link to the page that fixes each, and Run recipe stays off; nothing was changed", async () => {
    const plan = contract.recipePlan();
    // A blocked step the Components page fixes (the contract's blocked step has no page).
    plan.steps[1] = { ...plan.steps[1], action: "blocked", reason: "There is no DataScienceCluster, and the recipe creates none.", fix: { code: "dsc_missing", page: "/components" } };
    const api = stubApi({ ...editorReads, "GET /api/recipes/run": NO_RUN, "POST /api/recipes/plan": plan });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    const dialog = screen.getByRole("dialog", { name: "Run recipe dev?" });
    expect(within(dialog).getByText("The recipe cannot run on this cluster yet. Nothing has run: fix this, then run it.")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("Project recipe-dev: Project recipe-dev exists and was not created by a recipe.");
    expect(dialog).toHaveTextContent("Build: Cannot resolve the build on Quay.");
    // Only the problem a page fixes links there; the others are fixed in the recipe or by trying again.
    const problems = within(dialog).getByText("The recipe cannot run on this cluster yet. Nothing has run: fix this, then run it.").closest(".pf-v6-c-alert") as HTMLElement;
    // "Nothing was changed" is not repeated in each problem: the title says once that nothing has run.
    expect(problems.textContent?.match(/Nothing was changed/g) ?? []).toHaveLength(0);
    expect(within(problems).getAllByRole("link").map((a) => [a.textContent, a.getAttribute("href")])).toEqual([["Open Status", "/"]]);
    const stepList = within(dialog).getByRole("list", { name: "Steps of this operation" });
    expect(stepList).toHaveTextContent("Project recipe-devBlocked: Project recipe-dev exists and was not created by a recipe.");
    expect(stepList).toHaveTextContent("DataScienceClusterBlocked: There is no DataScienceCluster, and the recipe creates none. Open Components");
    expect(within(stepList).getAllByRole("link").map((a) => [a.textContent, a.getAttribute("href")])).toEqual([["Open Components", "/components"]]);
    expect(within(dialog).getByRole("button", { name: "Run recipe" })).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await settle();
    await dialogClosed();
    expect(api.calls.filter((c) => c.startsWith("POST /api/recipes/run"))).toEqual([]);
  });

  it("dependencies: No project is off while Pipelines is on; without a project Pipelines is off, with the reason", async () => {
    stubApi({ ...editorReads, "GET /api/recipes/run": NO_RUN });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    const none = screen.getByRole("radio", { name: "No project" });
    expect(none).toBeDisabled();
    expect(screen.getByText("Pipelines needs a project. Turn Pipelines off to run without one.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Pipelines" }));
    expect(none).toBeEnabled();
    fireEvent.click(none);
    expect(screen.getByRole("switch", { name: "Pipelines" })).toBeDisabled();
    expect(screen.getByText("Pipelines needs a project: choose one under Project.")).toBeInTheDocument();
    // An existing project: its picker lists the data science projects.
    fireEvent.click(screen.getByRole("radio", { name: "Use one of my data science projects" }));
    expect(screen.getByRole("switch", { name: "Pipelines" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    expect(screen.getByText("Choose a project.")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Project" }));
    await settle();
    fireEvent.click(screen.getByRole("option", { name: "team-a" }));
    expect(screen.queryByText("Choose a project.")).not.toBeInTheDocument();
    // A wrong name is caught before the backend is asked.
    fireEvent.change(screen.getByRole("textbox", { name: "Recipe name" }), { target: { value: "Dev Cluster" } });
    expect(screen.getByText(/Use at most 63 lower-case letters/)).toBeInTheDocument();
    await settleApps();
  });

  it("running: follows the run every 2 seconds, with the step's latest progress and its elapsed time", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status = runStatus({ steps: steps(3, "running") }, { running: true, step: { step: "pipelines-prereq", status: "running", message: "Turning on aipipelines in the DataScienceCluster" } });
    const api = stubApi({ "GET /api/recipes/run": () => jsonResponse(status) });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    const list = screen.getByRole("list", { name: "Recipe dev: steps" });
    // One line: the phase, the time so far, one estimate.
    expect(list).toHaveTextContent(/What Pipelines needsTurning on aipipelines in the DataScienceCluster\. 1:0\d so far; usually 2 minutes, up to 10 minutes\./);
    // The steps still to come say how long they usually take; the ones done say what they did.
    expect(list).toHaveTextContent("S3 storageUsually 1 minute, up to 5 minutes.");
    expect(list).toHaveTextContent("Project recipe-devProject recipe-dev is set up.");
    expect(screen.getByText(/The steps run on the updater: you can leave this page or close the browser/)).toHaveTextContent("Logging out does not stop the run; Stop after this step does.");
    expect(screen.getByText("Running", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
    // The run's own notice is the page: not repeated in the banners here.
    expect(screen.queryByText(/"Environment recipe"/)).not.toBeInTheDocument();
    status = runStatus({ steps: steps(4, "running") }, { running: true, step: { step: "s3", status: "running", message: "Waiting for SeaweedFS" } });
    const before = api.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(RECIPE_RUN_POLL_MS));
    await settle();
    expect(api.calls.slice(before).filter((c) => c === "GET /api/recipes/run")).toHaveLength(1);
    expect(within(list).getByText(/Waiting for SeaweedFS/)).toBeInTheDocument();
    expect(list).toHaveTextContent("What Pipelines needsWhat Pipelines needs is set up.");
    // Ended: the poll stops.
    status = runStatus({ state: "succeeded", stopReason: "Every step of dev is set up.", steps: steps(8), finishedAt: new Date().toISOString() });
    await act(() => vi.advanceTimersByTimeAsync(RECIPE_RUN_POLL_MS));
    await settle();
    expect(screen.getByText("Every step of dev is set up.")).toBeInTheDocument();
    const calls = api.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(RECIPE_RUN_POLL_MS * 3));
    await settleApps();
    expect(api.calls.slice(calls).filter((c) => c === "GET /api/recipes/run")).toEqual([]);
  });

  it("a run on another updater pod: shows the pod, and Stop is off there", async () => {
    stubApi({ "GET /api/recipes/run": contract.recipeRun() });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    expect(screen.getByText(/Running on updater pod pod-2 for/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Stop after this step" })).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByText(/Started by alice/)).toHaveTextContent("Project recipe-dev, created by the recipe");
    await settleApps();
  });

  it("stop: asks first, then the run says it stops after this step", async () => {
    let status = runStatus({ steps: steps(4, "running") }, { running: true, step: { step: "s3", status: "running", message: "Waiting for SeaweedFS" } });
    const api = stubApi({
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/recipes/stop": () => { status = { ...status, stopRequested: true }; return jsonResponse({ stopRequested: true }); },
    });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Stop after this step" }));
    const dialog = screen.getByRole("dialog", { name: "Stop after this step?" });
    expect(dialog).toHaveTextContent("Step 5 of 8, S3 storage, finishes, then the run stops and the cluster is free for others.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop after this step" }));
    await settle();
    expect(screen.getByText("Stopping after this step", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
    await dialogClosed();
    expect(api.calls.filter((c) => c.startsWith("POST"))).toEqual(["POST /api/recipes/stop"]);
    expect(screen.getByRole("button", { name: "Stop after this step" })).toHaveAttribute("aria-disabled", "true");
  });

  it("several steps at once: how many are done, each running step with its own progress, and Stop after the running steps", async () => {
    const parallel = steps(4, "running").map((s) => (s.id === "mlflow" ? { ...s, status: "running" as const, startedAt: new Date(Date.now() - 30_000).toISOString() } : s));
    let status = runStatus({ steps: parallel }, {
      running: true, step: { step: "mlflow", status: "running", message: "Waiting for MLflow" },
      progress: { s3: { step: "s3", status: "running", message: "Waiting for SeaweedFS" }, mlflow: { step: "mlflow", status: "running", message: "Waiting for MLflow" } },
    });
    stubApi({
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/recipes/stop": () => { status = { ...status, stopRequested: true }; return jsonResponse({ stopRequested: true }); },
    });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    expect(screen.getByText(/The steps run on the updater/)).toHaveTextContent(/: 4 of 8 steps done; running: S3 storage, MLflow\. Steps that do not depend on each other run at the same time\./);
    const list = screen.getByRole("list", { name: "Recipe dev: steps" });
    expect(list).toHaveTextContent(/S3 storageWaiting for SeaweedFS\. 1:0\d so far/);
    expect(list).toHaveTextContent(/MLflowWaiting for MLflow\. 0:3\d so far/);
    fireEvent.click(screen.getByRole("button", { name: "Stop after the running steps" }));
    const dialog = screen.getByRole("dialog", { name: "Stop after the running steps?" });
    expect(dialog).toHaveTextContent("The running steps, S3 storage and MLflow, finish, then the run stops and the cluster is free for others. No other step starts.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop after the running steps" }));
    await settle();
    expect(screen.getByText("Stopping after the running steps", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
    await dialogClosed();
  });

  it("progress only where it is the row's: another step's end is not shown on a running row; another pod's run shows its pod", async () => {
    // One step runs here; the run's latest progress is the end of the step before it.
    let status = runStatus({ steps: steps(4, "running") }, { running: true, step: { step: "pipelines-prereq", status: "success", message: "What Pipelines needs is set up." } });
    stubApi({ "GET /api/recipes/run": () => jsonResponse(status) });
    const { unmount } = renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    expect(screen.getByRole("list", { name: "Recipe dev: steps" })).toHaveTextContent(/S3 storageStarting\. 1:0\d so far/);
    unmount();
    // Another pod runs two steps: no per-step progress there, so each running row names the pod.
    const two = steps(4, "running").map((s) => (s.id === "mlflow" ? { ...s, status: "running" as const, startedAt: new Date(Date.now() - 30_000).toISOString() } : s));
    status = runStatus({ steps: two }, { running: true, remote: true, pod: "pod-2", step: { step: "mlflow-inner", status: "running", message: "Waiting for MLflow" } });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    const list = screen.getByRole("list", { name: "Recipe dev: steps" });
    expect(list).toHaveTextContent(/S3 storageRunning on updater pod pod-2\. /);
    expect(list).toHaveTextContent(/MLflowRunning on updater pod pod-2\. /);
    expect(list).not.toHaveTextContent("Waiting for MLflow");
  });

  it("a step failed while others run: says so right away, the failed row is failed, and no other step starts", async () => {
    const failing = steps(4, "failed", "SeaweedFS did not start.").map((s) => (s.id === "mlflow" ? { ...s, status: "running" as const, startedAt: new Date(Date.now() - 30_000).toISOString() } : s));
    stubApi({ "GET /api/recipes/run": runStatus({ steps: failing }, { running: true, ending: "failed", progress: { mlflow: { step: "mlflow", status: "running", message: "Waiting for MLflow" } } }) });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    expect(screen.getByText("S3 storage failed")).toBeInTheDocument();
    expect(screen.getByText(/SeaweedFS did not start\. No other step starts; MLflow finishes first, then the run stops\. Resume runs the steps that are not set up\./)).toBeInTheDocument();
    // The message once: in the alert, not again on the row.
    expect(screen.getAllByText(/SeaweedFS did not start/)).toHaveLength(1);
    expect(screen.getByText("Stopping: a step failed", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Stop after this step" })).toHaveAttribute("aria-disabled", "true");
  });

  it("stop with steps in flight: says which finish first", async () => {
    const two = steps(4, "running").map((s) => (s.id === "mlflow" ? { ...s, status: "running" as const, startedAt: new Date(Date.now() - 30_000).toISOString() } : s));
    stubApi({ "GET /api/recipes/run": runStatus({ steps: two }, { running: true, stopRequested: true, ending: "stopped" }) });
    renderPage(<RecipesPage />, "/recipes", { operation: async () => RECIPE_OPERATION });
    await settleApps();
    await settle();
    expect(screen.getByText("Stopping as asked")).toBeInTheDocument();
    expect(screen.getByText(/No other step starts; S3 storage and MLflow finish first, then the run stops\./)).toBeInTheDocument();
    expect(screen.getByText("Stopping after the running steps", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
  });

  it("several failed steps: the alert names each, with the backend's reason", async () => {
    const why = "Step 5 of 8 (S3 storage) failed: SeaweedFS did not start. Also failed: step 8 of 8 (MLflow).";
    const failedSteps = steps(4, "failed", "SeaweedFS did not start.").map((s) => (s.id === "mlflow" ? { ...s, status: "failed" as const, message: "MLflow did not start." } : s));
    stubApi({ ...editorReads, "GET /api/recipes/run": runStatus({ state: "failed", errorCode: "timeout", finishedAt: new Date().toISOString(), stopReason: why, steps: failedSteps }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("2 steps failed: S3 storage and MLflow")).toBeInTheDocument();
    expect(screen.getByText(why)).toBeInTheDocument();
    const list = screen.getByRole("list", { name: "Recipe dev: steps" });
    expect(list).toHaveTextContent("MLflowMLflow did not start.");
  });

  it("failed: the step's message and Resume, which continues where it stopped", async () => {
    let status = runStatus({
      state: "failed", errorCode: "timeout", finishedAt: new Date().toISOString(),
      stopReason: "Step 6 of 8 (Pipeline server) failed: The pipeline server did not become ready in 10 minutes.",
      steps: steps(5, "failed", "The pipeline server did not become ready in 10 minutes."),
    });
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/recipes/resume": () => { status = runStatus({ steps: steps(5, "running") }, { running: true }); return jsonResponse({ runId: "run1" }, 202); },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("Step 6 of 8 failed: Pipeline server")).toBeInTheDocument();
    // The step's message once, in the alert under the steps.
    expect(screen.getAllByText("The pipeline server did not become ready in 10 minutes.")).toHaveLength(1);
    expect(screen.getByText("Resume runs the steps that are not set up: Pipeline server, What MLflow needs and MLflow; what is set up stays.")).toBeInTheDocument();
    // The editor starts from the run's recipe.
    expect(screen.getByRole("heading", { level: 2, name: "Run a recipe again" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    const dialog = screen.getByRole("dialog", { name: "Resume recipe dev?" });
    expect(dialog).toHaveTextContent("It runs the steps that are not set up: Pipeline server, What MLflow needs and MLflow. Each step checks the cluster first");
    expect(within(dialog).getByRole("checkbox", { name: "Look up the newest build again" })).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Resume" }));
    await settle();
    expect(screen.getByRole("heading", { level: 2, name: "Recipe dev" })).toBeInTheDocument();
    expect(api.bodies["POST /api/recipes/resume"]).toEqual([{ runId: "run1" }]);
  });

  it("interrupted: the record says running but its updater pod is gone; Resume", async () => {
    stubApi({ ...editorReads, "GET /api/recipes/run": runStatus({ pod: "pod-old", steps: steps(4, "running") }, { interrupted: true }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("The run was interrupted")).toBeInTheDocument();
    expect(screen.getByText(/The updater pod pod-old stopped during step 5 of 8, S3 storage/)).toHaveTextContent("Nothing resumes by itself.");
    expect(screen.getByText("Interrupted", { selector: ".pf-v6-c-label__text" })).toBeInTheDocument();
    expect(within(screen.getByRole("list", { name: "Recipe dev: steps" })).getByText("Was running when the updater stopped.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume" })).toBeEnabled();
    await settleApps();
  });

  it("sign in again: the run stopped before a step; log out and log in, then Resume", async () => {
    stubApi({ ...editorReads, "GET /api/recipes/run": runStatus({
      state: "stopped", errorCode: "sign_in_again", finishedAt: new Date().toISOString(), steps: steps(4, "pending"),
      stopReason: "Stopped before S3 storage: Your sign-in expires before the next step could finish. Log out and log in again, then Resume.",
    }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    const alert = screen.getByText("Sign in again, then Resume").closest(".pf-v6-c-alert") as HTMLElement;
    // The backend's reason says what happened and what to do.
    expect(alert).toHaveTextContent(/^Warning alert:Sign in again, then ResumeStopped before S3 storage: Your sign-in expires before the next step could finish\. Log out and log in again, then Resume\.Log out$/);
    expect(within(alert).getByRole("link", { name: "Log out" })).toHaveAttribute("href", "/oauth/sign_out");
    expect(screen.getByRole("button", { name: "Resume" })).toBeEnabled();
    await settleApps();
  });

  it("cluster busy: a teammate's operation refuses the run; the page asks the backend who holds the lock", async () => {
    let busy = false;
    stubApi({
      ...editorReads,
      "GET /api/recipes/run": NO_RUN,
      "POST /api/recipes/plan": okPlan(),
      "POST /api/recipes/run": () => { busy = true; return jsonResponse({ error: "qa-user is running \"Update to nightly\". Try again when it finishes.", errorCode: "cluster_busy" }, 409); },
    });
    const operation = vi.fn(async () => (busy
      ? { inProgress: true, operation: { id: "op2", type: "update", label: "Update to nightly", user: "qa-user", startedAt: new Date().toISOString() } }
      : IDLE_OPERATION));
    renderPage(<RecipesPage />, "/recipes", { operation });
    await settleApps();
    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    const dialog = screen.getByRole("dialog", { name: "Run recipe dev?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Run recipe" }));
    await settle();
    expect(within(dialog).getByText("Another operation is changing this cluster")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("qa-user is running \"Update to nightly\". Try again when it finishes.");
    expect(operation.mock.calls.length).toBeGreaterThan(1);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await settle();
    expect(screen.getByRole("button", { name: "Review and run" })).toHaveAttribute("aria-disabled", "true");
  });

  it("clean up: sends the backend's clean-up request for what the run created; a project with a workbench is deleted only once it is confirmed", async () => {
    // A failed run: the cluster still shows the project it created and the aipipelines it turned on as its own;
    // the storage and MLflow are the cluster's, and the pipeline server goes with the project.
    const ended = contract.recipeRunEnded();
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": ended,
      "GET /api/resources/status": environmentUp("recipe-dev", "team-b"),
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("Step 5 of 5 failed: Pipeline server")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up" }));
    const dialog = screen.getByRole("dialog", { name: /Clean up what recipe dev set up\?/ });
    await settle();
    expect(within(dialog).getAllByRole("checkbox").map((c) => [c.getAttribute("id"), (c as HTMLInputElement).checked])).toEqual([
      ["recipe-cleanup-recipe-project:recipe-dev", true], ["recipe-cleanup-components", true],
    ]);
    // aipipelines stays on while the pipeline server in team-b stays: the label does not promise it goes back.
    expect(within(dialog).getByRole("checkbox", { name: "Turn back the DataScienceCluster components the run turned on" })).toBeChecked();
    expect(dialog).toHaveTextContent("None goes back now. Stays on while what uses it stays: Data Science Pipelines (the pipeline server in team-b stays).");
    expect(dialog).toHaveTextContent("Removes everything in the project: its models, playground, workbench and pipeline server.");
    expect(dialog).toHaveTextContent("Deleting project recipe-dev deletes everything in it.");
    // Left in place, from the live status: the shared parts; the project's pipeline server goes with it,
    // and one in a project the run never used is not the run's to name.
    expect(leftInPlaceItems(dialog)).toEqual(["The S3 storage", "MLflow"]);
    expect(within(dialog).getByRole("link", { name: "Environment page" })).toHaveAttribute("href", "/environment");
    // Keeping the project keeps its pipeline server too.
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Project recipe-dev, which the recipe created" }));
    expect(leftInPlaceItems(dialog)).toEqual(["The pipeline server in recipe-dev", "The S3 storage", "MLflow"]);
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Project recipe-dev, which the recipe created" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(streams).toHaveLength(1);
    const refusal = "Project recipe-dev still holds what this tool did not create: Notebook wb, PersistentVolumeClaim wb-data. Deleting the project deletes them too; confirm them to go ahead.";
    await act(async () => {
      streams[0].push(sseEvent("recipe-project:recipe-dev", "failed", refusal, { errorCode: "confirm_project_contents", detail: "Notebook wb\nPersistentVolumeClaim wb-data" }));
      streams[0].push(sseEvent("operation_complete", "failed", `${refusal} Nothing was deleted.`, { errorCode: "confirm_project_contents" }));
      streams[0].close();
    });
    await settle();
    const leftovers = within(dialog).getByText("Project recipe-dev holds what this tool did not create").closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(leftovers).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Notebook wb", "PersistentVolumeClaim wb-data"]);
    // The list's limits are visible: it names only the kinds the tool checks.
    expect(leftovers).toHaveTextContent("Only Notebooks (workbenches), InferenceServices and LLMInferenceServices (models), PersistentVolumeClaims (volumes) and Pods are listed; anything else in the project is deleted with it.");
    expect(within(dialog).getByRole("button", { name: "Clean up" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(leftovers).getByRole("checkbox", { name: "Delete them with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(streams).toHaveLength(2);
    await act(async () => {
      streams[1].push(sseEvent("recipe-project:recipe-dev", "success", "Deleted project recipe-dev; OpenShift removes what it holds."));
      streams[1].push(sseEvent("operation_complete", "success", "Deleted project recipe-dev."));
      streams[1].close();
    });
    await settle();
    expect(within(dialog).getByText("Cleaned up")).toBeInTheDocument();
    // aipipelines, which the dialog says stays on, is not sent.
    const sent: CleanupRequest = { ...ended.cleanup };
    delete sent.components;
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      sent,
      { ...sent, confirmProjectContents: { "recipe-dev": ["Notebook wb", "PersistentVolumeClaim wb-data"] } },
    ]);
  });

  it("clean up in an existing project: offers only the components; the pipeline server, the storage and MLflow are left in place", async () => {
    // The run set up all of them, in a project it did not create: none of them is only the run's.
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": runStatus({ state: "succeeded", steps: steps(8), project: { name: "team-a" } }, { cleanup: { components: ["aipipelines"] } }),
      "GET /api/resources/status": environmentUp("team-a"),
      "POST /api/resources/cleanup/stream": () => sseResponse().response,
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up" }));
    const dialog = screen.getByRole("dialog");
    await settle();
    expect(within(dialog).getAllByRole("checkbox").map((c) => c.getAttribute("id"))).toEqual(["recipe-cleanup-components"]);
    expect(within(dialog).queryByText(/Project team-a/)).not.toBeInTheDocument();
    expect(leftInPlaceItems(dialog)).toEqual(["The pipeline server in team-a", "The S3 storage", "MLflow"]);
    // Its pipeline server stays, so aipipelines stays on: nothing goes back, and nothing is sent.
    expect(dialog).toHaveTextContent("None goes back now. Stays on while what uses it stays: Data Science Pipelines (the pipeline server in team-a stays).");
    expect(within(dialog).getByRole("button", { name: "Clean up" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toBeUndefined();
  });

  it("clean up says what it leaves in place even when the Environment status cannot be read", async () => {
    stubApi({
      ...editorReads,
      "GET /api/recipes/run": runStatus({ state: "succeeded", steps: steps(8), project: { name: "team-a" } }, { cleanup: { components: ["aipipelines"] } }),
      "GET /api/resources/status": () => jsonResponse({ error: "forbidden", errorCode: "forbidden" }, 403),
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up" }));
    const dialog = screen.getByRole("dialog");
    await settle();
    expect(dialog).toHaveTextContent("This clean-up does not delete the S3 storage, MLflow or a pipeline server in a project the recipe did not create. Their status could not be read.");
    expect(within(dialog).getByRole("link", { name: "Environment page" })).toHaveAttribute("href", "/environment");
  });
});

const Where: React.FC = () => <span data-testid="where">{useLocation().pathname}</span>;

describe("Recipes elsewhere in the app", () => {
  it("the banner names a teammate's recipe run, its step and where to follow it", async () => {
    stubApi({});
    renderPage(<GlobalBanners />, "/components", { operation: async () => ({ ...RECIPE_OPERATION, operation: { ...RECIPE_OPERATION.operation!, user: "qa-user" } }) });
    await settleApps();
    const alert = (await screen.findByText(/qa-user is running "Environment recipe": step 4 of 8, What Pipelines needs \(1m 3\ds\)/)).closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("Recipe dev. Cluster changes from this tool are disabled until it finishes.");
    expect(within(alert).getByRole("button", { name: "Open Recipes" })).toBeInTheDocument();
    expect(alert).not.toHaveTextContent("Target:");
    await settleApps();
  });

  it("the interrupted notice of a recipe run leads to Resume on the Recipes page", async () => {
    stubApi({});
    const interrupted: OperationStatusResponse = {
      inProgress: false, operation: null,
      interrupted: { type: "recipe", label: "Environment recipe", user: "me", target: "dev: step 5 of 8 (S3 storage)", startedAt: new Date(Date.now() - 600_000).toISOString(), pod: "pod-old" },
    };
    renderPage(<><GlobalBanners /><Routes><Route path="*" element={<Where />} /></Routes></>, "/components", { operation: async () => interrupted });
    await settleApps();
    const alert = (await screen.findByText("\"Environment recipe\" was interrupted")).closest(".pf-v6-c-alert") as HTMLElement;
    fireEvent.click(within(alert).getAllByRole("button").find((b) => b.hasAttribute("aria-expanded"))!);
    expect(alert).toHaveTextContent("(recipe dev, at step 5 of 8, S3 storage)");
    expect(alert).toHaveTextContent("Resume it on the Recipes page");
    fireEvent.click(within(alert).getByRole("button", { name: "Open Recipes to resume" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/recipes");
    await settleApps();
  });

  it("the gitops profile has no Recipes page", async () => {
    stubApi({ "/api/status": nightlyStatus(), "/api/operation": IDLE_OPERATION, "/api/version": { version: "dev", commit: "c", buildDate: "d", templateOutdated: false, profile: "gitops", admissionPolicy: "enforced" } });
    window.history.pushState({}, "", "/recipes");
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Recipes is not available here" })).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Global", hidden: true });
    expect(within(nav).queryByRole("link", { name: "Recipes", hidden: true })).not.toBeInTheDocument();
    window.history.pushState({}, "", "/");
  });

  it("the Status page offers a recipe while the DataScienceCluster is missing, and not once it exists", async () => {
    stubApi({ "/api/latest-nightly": { tag: "rhoai-3.6", image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64) }, "/api/nightly-tags": { tags: [] } });
    const first = renderPage(<StatusPage />, "/", { status: async () => nightlyStatus({ dscExists: false }) });
    await settleApps();
    const card = (await screen.findByRole("heading", { name: "Set up this cluster from a recipe" })).closest(".pf-v6-c-card") as HTMLElement;
    expect(card).toHaveTextContent("One run installs an RHOAI nightly, creates the DataScienceCluster, a project, and the Environment features you choose");
    expect(card).not.toHaveTextContent("Finish the one-time cluster setup first");
    expect(within(card).getByRole("button", { name: "Open Recipes" })).toBeInTheDocument();
    first.unmount();
    renderPage(<StatusPage />, "/", { status: async () => nightlyStatus() });
    await settleApps();
    expect(screen.queryByRole("heading", { name: "Set up this cluster from a recipe" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("the Environment Overview names the last run's recipe and project", async () => {
    stubApi({ "GET /api/recipes/run": runStatus({ state: "succeeded" }) });
    render(<MemoryRouter><RecipeRunNote /></MemoryRouter>);
    const note = await screen.findByText(/The last recipe run/);
    expect(note).toHaveTextContent(/The last recipe run, recipe dev, started .*, created project recipe-dev\./);
    expect(within(note).getByRole("link", { name: "recipe dev" })).toHaveAttribute("href", "/recipes");
  });
});
