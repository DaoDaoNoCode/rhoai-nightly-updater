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
import { jsonResponse, stubApi } from "../test/apiStub";
import { contract } from "../test/contract";
import { IDLE_OPERATION, nightlyStatus, renderPage, settleApps } from "../test/providers";
import { sseEvent, sseResponse } from "../test/utils";
import type { OperationStatusResponse, RecipePlan, RecipeRun, RecipeRunStatus, RecipeRunStep } from "../types";

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

/** The run's steps: the first `done` set up (RHOAI skipped), then `current` with its status, the rest pending. */
function steps(done: number, current: RecipeRunStep["status"] = "running", message = ""): RecipeRunStep[] {
  return STEPS.map(([id, title], i) => {
    if (i < done) return { id, title, status: id === "rhoai" ? "skipped" : "success", message: id === "rhoai" ? "Already set up: RHOAI 3.6.0 is installed." : `${title} is set up.`, startedAt: "2026-11-20T14:05:00Z", finishedAt: "2026-11-20T14:06:00Z" };
    if (i === done) return { id, title, status: current, message, startedAt: new Date(Date.now() - 65_000).toISOString() };
    return { id, title, status: "pending" };
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

describe("Recipes page", () => {
  it("new: the editor's defaults, Review and run plans the recipe, and Run recipe starts it", async () => {
    let status: RecipeRunStatus = NO_RUN;
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/recipes/plan": okPlan(),
      "POST /api/recipes/run": () => { status = runStatus({ steps: steps(0, "running") }, { running: true, step: { step: "rhoai", status: "running", message: "Checking RHOAI..." } }); return jsonResponse({ runId: "run1" }, 202); },
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
    expect(screen.getByText(/Models, the playground, Models as a Service and Dashboard Dev come to recipes in a later version/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    const dialog = screen.getByRole("dialog", { name: "Run recipe dev?" });
    expect(within(dialog).getByText(/The run takes usually 13 minutes, up to 52 minutes\./)).toBeInTheDocument();
    expect(dialog).toHaveTextContent("It installs rhoai-3.6 build aaaaaaaaaaaa, and keeps it if you resume.");
    expect(dialog).toHaveTextContent("Teammates cannot change this cluster until the run ends.");
    const plan = within(dialog).getByRole("list", { name: "Steps of this operation" });
    expect(plan).toHaveTextContent("RHOAIRuns: usually 7 minutes, up to 15 minutes.");
    expect(plan).toHaveTextContent("S3 storageAlready set up: The S3 storage is set up and ready.");
    expect(plan).toHaveTextContent("DataScienceClusterChecked when the run gets there");
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
    expect(screen.getByRole("heading", { level: 2, name: "Running: recipe dev" })).toBeInTheDocument();
    expect(api.bodies["POST /api/recipes/run"]).toEqual(api.bodies["POST /api/recipes/plan"]);
    await dialogClosed();
    // One run at a time: no editor while it runs.
    expect(screen.queryByRole("button", { name: "Review and run" })).not.toBeInTheDocument();
    expect(screen.getByText(/Checking RHOAI\.\.\./)).toBeInTheDocument();
  });

  it("review: a plan with problems lists them and Run recipe stays off; nothing was changed", async () => {
    const api = stubApi({ ...editorReads, "GET /api/recipes/run": NO_RUN, "POST /api/recipes/plan": contract.recipePlan() });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    const dialog = screen.getByRole("dialog", { name: "Run recipe dev?" });
    expect(within(dialog).getByText("The recipe cannot run on this cluster now. Nothing was changed.")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("Project recipe-dev: Project recipe-dev exists and was not created by a recipe.");
    expect(dialog).toHaveTextContent("Build: Cannot resolve the build on Quay.");
    expect(within(dialog).getByRole("list", { name: "Steps of this operation" })).toHaveTextContent("Project recipe-devBlocked: Project recipe-dev exists and was not created by a recipe.");
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
    expect(list).toHaveTextContent(/What Pipelines needsTurning on aipipelines in the DataScienceCluster \(1:0\d\)/);
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
    expect(screen.getByText("Step 6 of 8 (Pipeline server) failed: The pipeline server did not become ready in 10 minutes.")).toBeInTheDocument();
    expect(screen.getByText("Resume continues at step 6 of 8, Pipeline server; what is set up stays.")).toBeInTheDocument();
    // The editor starts from the run's recipe.
    expect(screen.getByRole("heading", { level: 2, name: "Run a recipe again" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    const dialog = screen.getByRole("dialog", { name: "Resume recipe dev?" });
    expect(dialog).toHaveTextContent("It continues at step 6 of 8, Pipeline server. Each step checks the cluster first");
    expect(within(dialog).getByRole("checkbox", { name: "Look up the newest build again" })).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Resume" }));
    await settle();
    expect(screen.getByRole("heading", { level: 2, name: "Running: recipe dev" })).toBeInTheDocument();
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
      stopReason: "Stopped before S3 storage: Your sign-in expires in 4 minutes, and a change may take up to 21 minutes. Log out and log in again, then retry. Then Resume.",
    }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    const alert = screen.getByText("Sign in again, then Resume").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("The run stopped before step 5 of 8, S3 storage: your sign-in expires before that step could finish, or it ended. Nothing of that step started. Log out and log in again, then Resume.");
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

  it("clean up: deletes what the run set up; a project with a workbench is deleted only once it is confirmed", async () => {
    const run = runStatus({ state: "succeeded", stopReason: "Every step of dev is set up.", steps: steps(8), finishedAt: new Date().toISOString() });
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": run,
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("Project recipe-dev is ready to use.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up..." }));
    const dialog = screen.getByRole("dialog", { name: /Clean up what recipe dev set up\?/ });
    expect(within(dialog).getAllByRole("checkbox").map((c) => [c.getAttribute("id"), (c as HTMLInputElement).checked])).toEqual([
      ["recipe-cleanup-pipeline-server:recipe-dev", true], ["recipe-cleanup-storage", true], ["recipe-cleanup-mlflow", true], ["recipe-cleanup-recipe-project:recipe-dev", true],
    ]);
    expect(dialog).toHaveTextContent("Deleting project recipe-dev deletes everything in it.");
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
    expect(within(dialog).getByRole("button", { name: "Clean up" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(leftovers).getByRole("checkbox", { name: "Delete them with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(streams).toHaveLength(2);
    await act(async () => {
      streams[1].push(sseEvent("recipe-project:recipe-dev", "success", "Deleted project recipe-dev; OpenShift removes what it holds."));
      streams[1].push(sseEvent("operation_complete", "success", "Deleted the pipeline server, the S3 storage, MLflow and project recipe-dev."));
      streams[1].close();
    });
    await settle();
    expect(within(dialog).getByText("Cleaned up")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      { pipelineServers: ["recipe-dev"], storage: true, mlflow: true, recipeProject: "recipe-dev" },
      { pipelineServers: ["recipe-dev"], storage: true, mlflow: true, recipeProject: "recipe-dev", confirmProjectContents: ["Notebook wb", "PersistentVolumeClaim wb-data"] },
    ]);
  });

  it("clean up offers what was there before the run, unchosen", async () => {
    const before = steps(8).map((s) => (s.id === "s3" || s.id === "mlflow" ? { ...s, status: "skipped" as const } : s));
    stubApi({ ...editorReads, "GET /api/recipes/run": runStatus({ state: "succeeded", steps: before, project: { name: "team-a" } }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up..." }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("checkbox", { name: "The S3 storage" })).not.toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "MLflow" })).not.toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "The pipeline server in team-a" })).toBeChecked();
    // A project the recipe did not create is never offered.
    expect(within(dialog).queryByText(/Project team-a/)).not.toBeInTheDocument();
    await settleApps();
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
