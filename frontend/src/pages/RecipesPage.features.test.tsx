import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, within } from "@testing-library/react";
import { RecipesPage } from "./RecipesPage";
import { jsonResponse, stubApi } from "../test/apiStub";
import { contract } from "../test/contract";
import { renderPage, settleApps } from "../test/providers";
import { sseResponse } from "../test/utils";
import type { RecipePlan, RecipeRunStatus, ResourcesStatus } from "../types";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** Inside act, wait until stubbed fetches and the app updates they trigger have settled. */
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)); });
    await settleApps();
  }
}

const NO_RUN: RecipeRunStatus = { run: null, running: false };

/** Environment status catalog: chat models and an embedding model recipes do not expose. */
const CATALOG = {
  models: {
    catalog: [
      { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct", type: "chat", default: true, uri: "", cpu: "2", memoryRequest: "4Gi", memory: "6Gi", useCase: "Chat", description: "", args: "", downloadSize: "1 GB" },
      { name: "qwen3-1-7b", displayName: "Qwen3 1.7B", type: "chat", uri: "", cpu: "4", memoryRequest: "8Gi", memory: "10Gi", useCase: "Chat", description: "", args: "", downloadSize: "4 GB" },
      { name: "granite-embedding-125m-english", displayName: "Granite Embedding", type: "embedding", uri: "", cpu: "1", memoryRequest: "2Gi", memory: "2Gi", useCase: "Embedding", description: "", args: "", downloadSize: "500 MB" },
    ],
  },
} as unknown as ResourcesStatus;

const editorReads = {
  "/api/nightly-tags": { tags: [] },
  "/api/resources/projects": { projects: ["team-a"] },
  "GET /api/resources/status": CATALOG,
};

/** Plan for the contract's full feature run—each step, RHOAI verification when the run reaches it. */
function featuresPlan(): RecipePlan {
  const run = contract.recipeRunFeatures().run!;
  const steps = run.steps.map((s, i) => ({ id: s.id, title: s.title, action: i === 0 ? "run" as const : "later" as const, typicalSeconds: s.typicalSeconds, maxSeconds: s.maxSeconds }));
  const warnings = ["Project recipe-full also gets a pipeline server, a model and a workbench. Kueue manages only what is sent to a queue, so they keep running.", "The dashboard's Kueue setting changes every new project on this cluster."];
  return { recipe: run.recipe, steps, problems: [], warnings, typicalSeconds: steps.reduce((n, s) => n + s.typicalSeconds, 0), maxSeconds: steps.reduce((n, s) => n + s.maxSeconds, 0) };
}

const switchNamed = (name: string) => screen.getByRole("switch", { name });

describe("Recipes page: models, MaaS, the playground, the model registry, the workbench and Dashboard Dev", () => {
  it("the form turns every feature on with its options, and Review and run sends that recipe", async () => {
    const api = stubApi({ ...editorReads, "GET /api/recipes/run": NO_RUN, "POST /api/recipes/plan": featuresPlan() });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    for (const name of ["Models", "Playground", "Models as a Service", "Model registry", "Workbench"]) expect(switchNamed(name)).not.toBeChecked();
    expect(screen.getByRole("radio", { name: "Keep the installed dashboard" })).toBeChecked();

    fireEvent.click(screen.getByRole("checkbox", { name: "Add the MLflow samples to the project" }));
    fireEvent.click(switchNamed("Models as a Service"));
    fireEvent.click(screen.getByRole("radio", { name: "CloudNativePG" }));
    fireEvent.click(switchNamed("Models"));
    await settle();
    // Picker lists catalog chat models with the default entry at the top.
    expect(screen.getByRole("button", { name: "Model" })).toHaveTextContent("Qwen2.5 0.5B Instruct (the default)");
    fireEvent.click(screen.getByRole("button", { name: "Model" }));
    await settle();
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Qwen2.5 0.5B Instruct", "Qwen3 1.7B"]);
    fireEvent.click(screen.getByRole("option", { name: "Qwen3 1.7B" }));
    fireEvent.click(screen.getByRole("button", { name: "How it is reached" }));
    await settle();
    fireEvent.click(screen.getByRole("option", { name: "Through the cluster's default gateway" }));
    fireEvent.click(switchNamed("Playground"));
    fireEvent.click(switchNamed("Model registry"));
    fireEvent.click(screen.getByRole("checkbox", { name: "Register the sample model" }));
    fireEvent.click(switchNamed("Workbench"));
    // Distributed workloads: the tab's options; the dashboard setting is off by default, with its cluster-wide effect.
    fireEvent.click(switchNamed("Distributed workloads"));
    expect(screen.getByText("The project also gets a pipeline server, a model and a workbench. Kueue manages only what is sent to a queue, so they keep running; a project of its own is still safer.")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Turn on the dashboard's Kueue setting" })).not.toBeChecked();
    expect(screen.getByText(/It changes every new project on this cluster/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("checkbox", { name: "RayCluster sample" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Sample image" }), { target: { value: "bad image!" } });
    fireEvent.blur(screen.getByRole("textbox", { name: "Sample image" }));
    expect(screen.getByText("Enter an image reference, such as quay.io/project/ray:2.47.")).toBeInTheDocument();
    fireEvent.change(screen.getByRole("textbox", { name: "Sample image" }), { target: { value: "quay.io/x/ray:2" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "Turn on the dashboard's Kueue setting" }));
    expect(screen.getByText("A workbench in the project, as the dashboard creates one. It gets the S3 Connection of the pipeline server.")).toBeInTheDocument();
    // Without Pipelines it says what the workbench lacks.
    fireEvent.click(switchNamed("Pipelines"));
    expect(screen.getByText("A workbench in the project, as the dashboard creates one. Without Pipelines it has no S3 Connection: turn Pipelines on to attach one.")).toBeInTheDocument();
    fireEvent.click(switchNamed("Pipelines"));
    expect(screen.getByText(/Usually about 9 minutes in all, up to 30\./)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: "A pull request build" }));
    expect(screen.getByText(/Deployed last: it pauses the dashboard's operator/)).toBeInTheDocument();
    // PR number validation runs on the client before any plan request goes out.
    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    expect(screen.getByText("Enter the number of an odh-dashboard pull request.")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.change(screen.getByRole("textbox", { name: "Pull request number" }), { target: { value: "4321" } });

    fireEvent.click(screen.getByRole("button", { name: "Review and run" }));
    await settle();
    expect(api.bodies["POST /api/recipes/plan"]).toEqual([{ recipe: {
      apiVersion: "rhoai-nightly-updater/v1alpha1", kind: "EnvironmentRecipe", metadata: { name: "dev" },
      spec: {
        rhoai: { build: "latest", ifInstalled: "keep" }, dataScienceCluster: { create: true },
        project: { mode: "create", name: "recipe-dev", displayName: "Recipe dev" },
        features: {
          pipelines: { enabled: true }, mlflow: { enabled: true, samples: true },
          models: { enabled: true, model: "qwen3-1-7b", exposure: "default-gateway" }, playground: { enabled: true },
          maas: { enabled: true, database: "cnpg", publish: true }, modelRegistry: { enabled: true, sample: true }, workbench: { enabled: true },
          kueue: { enabled: true, rayCluster: true, rayImage: "quay.io/x/ray:2", dashboardOptIn: true },
        },
        dashboardDev: { deploy: "pr", pr: 4321, flavor: "rhoai" },
      },
    } }]);
    // Dialog step list mirrors runner order and shows each title without adornment.
    const plan = within(screen.getByRole("dialog", { name: "Run recipe dev?" })).getByRole("list", { name: "Steps of this operation" });
    const titles = within(plan).getAllByRole("listitem").map((li) => li.textContent ?? "");
    for (const [i, title] of ["MLflow samples", "Models as a Service", "What Models needs", "Model Qwen3 1.7B (CPU, tool calling)", "Publish the model through MaaS",
      "What the playground needs", "Playground", "What the model registry needs", "Model registry", "Workbench", "What distributed workloads need", "Distributed workloads", "Dashboard Dev (PR #4321)"].entries()) {
      expect(titles[8 + i]).toContain(title);
    }
    // The plan's warnings: what else the run changes.
    const warning = within(screen.getByRole("dialog", { name: "Run recipe dev?" })).getByText("The run also changes this").closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(warning).getAllByRole("listitem").map((li) => li.textContent)).toHaveLength(2);
    expect(plan).toHaveTextContent("Model Qwen3 1.7B (CPU, tool calling)Later: usually 5 minutes, up to 15 minutes.");
  });

  it("dependencies: the playground needs a model, publishing is part of a MaaS exposure, and a project is kept while features live in it", async () => {
    stubApi({ ...editorReads, "GET /api/recipes/run": NO_RUN });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(switchNamed("Playground")).toBeDisabled();
    expect(screen.getByText("The playground needs a model: turn Models on.")).toBeInTheDocument();
    fireEvent.click(switchNamed("Models"));
    await settle();
    expect(switchNamed("Playground")).toBeEnabled();
    // MaaS routing choice appears only when the MaaS toggle is enabled.
    fireEvent.click(screen.getByRole("button", { name: "How it is reached" }));
    await settle();
    expect(screen.queryByRole("option", { name: "Through Models as a Service" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("option", { name: "Only inside the cluster" }));
    fireEvent.click(switchNamed("Models as a Service"));
    fireEvent.click(screen.getByRole("button", { name: "How it is reached" }));
    await settle();
    fireEvent.click(screen.getByRole("option", { name: "Through Models as a Service" }));
    expect(screen.getByRole("checkbox", { name: "Publish the model through MaaS" })).toBeDisabled();
    expect(screen.getByText("The model is reached through MaaS, which publishes it already.")).toBeInTheDocument();
    // "No project" stays blocked while Pipelines or Models still need a project.
    expect(screen.getByRole("radio", { name: "No project" })).toBeDisabled();
    expect(screen.getByText("Pipelines and Models need a project. Turn them off to run without one.")).toBeInTheDocument();
    fireEvent.click(switchNamed("Pipelines"));
    fireEvent.click(switchNamed("Models"));
    fireEvent.click(screen.getByRole("radio", { name: "No project" }));
    for (const name of ["Models", "Workbench"]) expect(switchNamed(name)).toBeDisabled();
    expect(screen.getByText("A workbench needs a project: choose one under Project.")).toBeInTheDocument();
    // Model registry switch stays enabled without a project selected.
    expect(switchNamed("Model registry")).toBeEnabled();
  });

  it("run and Clean up: the steps of every feature, and the MaaS platform and the model registry the run created are offered unticked", async () => {
    const ended = contract.recipeRunFeatures();
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": ended,
      "GET /api/resources/status": { minio: { deployed: true, ready: true }, mlflow: { deployed: true, ready: true } } as ResourcesStatus,
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
      "POST /api/recipes/plan": () => jsonResponse(featuresPlan()),
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("Every step of full is set up.")).toBeInTheDocument();
    expect(screen.getByText("Project recipe-full is set up. Its workbench may still be starting: the Workbench tab shows when it runs.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Last run of recipe full" })).toBeInTheDocument();
    for (const title of ["Models as a Service", "Model Qwen3 1.7B (CPU, tool calling)", "Publish the model through MaaS", "Playground", "Model registry", "Workbench", "Dashboard Dev (PR #4321)"]) {
      expect(screen.getAllByText(title).length).toBeGreaterThan(0);
    }
    // Controls load state from the recipe attached to the completed run.
    expect(switchNamed("Models")).toBeChecked();
    expect(screen.getByRole("textbox", { name: "Pull request number" })).toHaveValue("4321");

    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up..." }));
    const dialog = screen.getByRole("dialog", { name: /Clean up what recipe full set up\?/ });
    await settle();
    expect(within(dialog).getAllByRole("checkbox").map((c) => [c.getAttribute("id"), (c as HTMLInputElement).checked])).toEqual([
      ["recipe-cleanup-recipe-project:recipe-full", true], ["recipe-cleanup-model-registry", false], ["recipe-cleanup-maas-platform", false], ["recipe-cleanup-kueue", false], ["recipe-cleanup-components", true],
    ]);
    const left = within(within(dialog).getByText("Left in place").closest(".pf-v6-c-alert") as HTMLElement).getAllByRole("listitem").map((li) => li.textContent);
    // What the checkboxes offer is not listed again; Dashboard Dev has its own page, not the Environment page's Clean up.
    expect(left).toEqual(["The S3 storage", "MLflow"]);
    expect(dialog).toHaveTextContent("The Dashboard Dev build stays: revert it on the Dashboard Dev page.");
    expect(within(dialog).getByRole("link", { name: "Dashboard Dev page" })).toHaveAttribute("href", "/dashboard-dev");
    expect(within(dialog).getByRole("checkbox", { name: "Turn back the DataScienceCluster components the run turned on: kserve" })).toBeChecked();
    // Keeping the project keeps its model, so kserve stays on, and the label says so.
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Project recipe-full, which the recipe created" }));
    expect(dialog).toHaveTextContent("None goes back now. Stays on while what uses it stays: kserve (the model in recipe-full stays).");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Project recipe-full, which the recipe created" }));
    // The run's success says the workbench may still be starting, not that all is ready.
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "The MaaS platform the run set up" }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "The distributed workloads setup the run created" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(streams).toHaveLength(1);
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      // The reviewed identities: the backend checks them again before it deletes the platform.
      { recipeProjects: ["recipe-full"], recipeProjectUIDs: { "recipe-full": "1a2b3c4d-uid" }, maasPlatform: true, maasPlatformUid: "creds-uid", kueue: true, kueueUid: "clusterqueue-uid", recipeRunId: "run2", components: ["kserve"] },
    ]);
    await act(async () => { streams[0].close(); });
    await settle();
  });
});
