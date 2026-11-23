import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, within } from "@testing-library/react";
import { RecipesPage } from "./RecipesPage";

import { jsonResponse, stubApi } from "../test/apiStub";
import { contract } from "../test/contract";
import { renderPage, settleApps } from "../test/providers";
import { sseEvent, sseResponse } from "../test/utils";
import type { RecipeRunStatus } from "../types";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** Inside act, wait until stubbed fetches resolve and any app reads they trigger have settled. */
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)); });
    await settleApps();
  }
}

const editorReads = {
  "/api/nightly-tags": { tags: [] },
  "/api/resources/projects": { projects: ["team-a"] },
  "GET /api/resources/status": { minio: { deployed: false, ready: false }, mlflow: { deployed: false, ready: false }, pipelineServers: [] },
};

/** Fixture for a completed run from the contract; cluster-visible fields come from `over` and nested run overrides. */
function ended(over: Partial<RecipeRunStatus> = {}, run: Partial<NonNullable<RecipeRunStatus["run"]>> = {}): RecipeRunStatus {
  const base = contract.recipeRunEnded();
  return { ...base, ...over, run: { ...base.run!, ...run } };
}

describe("Recipes page follow-ups", () => {
  it("F4: lists every project recipes created that the cluster has, each deleted on its own with the same guards", async () => {
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": ended(),
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    const section = screen.getByRole("region", { name: "Projects recipes created" });
    expect(within(section).getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["recipe-dev", "recipe-old"]);
    expect(within(section).getAllByRole("row")[2]).toHaveTextContent("old");
    fireEvent.click(within(section).getByRole("button", { name: "Delete project recipe-old..." }));
    const dialog = screen.getByRole("dialog", { name: /Delete project recipe-old\?/ });
    await settle();
    expect(within(dialog).getByRole("checkbox", { name: "Project recipe-old, which the recipe created" })).toBeChecked();
    expect(within(dialog).queryByText("Left in place")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    await act(async () => {
      streams[0].push(sseEvent("recipe-project:recipe-old", "failed", "Project recipe-old still holds what this tool did not create: Notebook wb.", { errorCode: "confirm_project_contents", detail: "Notebook wb" }));
      streams[0].push(sseEvent("operation_complete", "failed", "Project recipe-old still holds what this tool did not create: Notebook wb. Nothing was deleted.", { errorCode: "confirm_project_contents" }));
      streams[0].close();
    });
    await settle();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Delete it with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    // Each carries the UID of the project the row showed.
    const uids = { recipeProjectUIDs: { "recipe-old": "7a8b9c0d-uid" } };
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      { recipeProjects: ["recipe-old"], ...uids },
      { recipeProjects: ["recipe-old"], ...uids, confirmProjectContents: { "recipe-old": ["Notebook wb"] } },
    ]);
  });

  /** The refusal of a clean-up: per project, everything it holds that the tool did not create (the backend lists all of it). */
  const refuse = async (stream: ReturnType<typeof sseResponse>, lists: Record<string, string[]>) => {
    await act(async () => {
      for (const [p, items] of Object.entries(lists)) {
        stream.push(sseEvent(`recipe-project:${p}`, "failed", `Project ${p} still holds what this tool did not create.`, { errorCode: "confirm_project_contents", detail: items.join("\n") }));
      }
      stream.push(sseEvent("operation_complete", "failed", "Nothing was deleted.", { errorCode: "confirm_project_contents" }));
      stream.close();
    });
    await settle();
  };
  const asked = (dialog: HTMLElement, project: string) => within(dialog).getByText(`Project ${project} holds what this tool did not create`).closest(".pf-v6-c-alert") as HTMLElement;
  const listed = (alert: HTMLElement) => within(alert).getAllByRole("listitem").map((li) => li.textContent);

  it("two refusals in a row: the second lists what was confirmed too, and confirming all of it goes ahead", async () => {
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": ended(),
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(within(screen.getByRole("region", { name: "Projects recipes created" })).getByRole("button", { name: "Delete project recipe-old..." }));
    const dialog = screen.getByRole("dialog", { name: /Delete project recipe-old\?/ });
    await settle();
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    await refuse(streams[0], { "recipe-old": ["Notebook wb"] });
    fireEvent.click(within(asked(dialog, "recipe-old")).getByRole("checkbox", { name: "Delete it with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    // A claim appeared meanwhile: the backend lists the notebook (confirmed) and the claim.
    await refuse(streams[1], { "recipe-old": ["Notebook wb", "PersistentVolumeClaim data"] });
    expect(listed(asked(dialog, "recipe-old"))).toEqual(["Notebook wb", "PersistentVolumeClaim data"]);
    expect(within(dialog).getByRole("button", { name: "Clean up" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(asked(dialog, "recipe-old")).getByRole("checkbox", { name: "Delete them with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    const uids = { recipeProjectUIDs: { "recipe-old": "7a8b9c0d-uid" } };
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      { recipeProjects: ["recipe-old"], ...uids },
      { recipeProjects: ["recipe-old"], ...uids, confirmProjectContents: { "recipe-old": ["Notebook wb"] } },
      { recipeProjects: ["recipe-old"], ...uids, confirmProjectContents: { "recipe-old": ["Notebook wb", "PersistentVolumeClaim data"] } },
    ]);
  });

  it("two projects, only one gains an object: both come back in full, and the retry confirms both", async () => {
    const uids = { "recipe-dev": "0f3c1d2e-uid", "recipe-old": "7a8b9c0d-uid" };
    const streams: ReturnType<typeof sseResponse>[] = [];
    const api = stubApi({
      ...editorReads,
      "GET /api/recipes/run": ended({ cleanup: { recipeProjects: ["recipe-dev", "recipe-old"], recipeProjectUIDs: uids } }),
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up..." }));
    const dialog = screen.getByRole("dialog", { name: /Clean up what recipe dev set up\?/ });
    await settle();
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    await refuse(streams[0], { "recipe-dev": ["Pod a"], "recipe-old": ["Notebook wb"] });
    for (const p of ["recipe-dev", "recipe-old"]) fireEvent.click(within(asked(dialog, p)).getByRole("checkbox", { name: "Delete it with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    // Only recipe-old gained a pod; recipe-dev, all of it confirmed, is listed again too.
    await refuse(streams[1], { "recipe-dev": ["Pod a"], "recipe-old": ["Notebook wb", "Pod b"] });
    expect(listed(asked(dialog, "recipe-dev"))).toEqual(["Pod a"]);
    expect(listed(asked(dialog, "recipe-old"))).toEqual(["Notebook wb", "Pod b"]);
    fireEvent.click(within(asked(dialog, "recipe-dev")).getByRole("checkbox", { name: "Delete it with the project" }));
    fireEvent.click(within(asked(dialog, "recipe-old")).getByRole("checkbox", { name: "Delete them with the project" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    expect(api.bodies["POST /api/resources/cleanup/stream"]?.[2]).toEqual({
      recipeProjects: ["recipe-dev", "recipe-old"], recipeProjectUIDs: uids,
      confirmProjectContents: { "recipe-dev": ["Pod a"], "recipe-old": ["Notebook wb", "Pod b"] },
    });
  });

  it("F4: a list that cannot be read says so, and offers nothing", async () => {
    stubApi({ ...editorReads, "GET /api/recipes/run": ended({ projects: undefined, projectsError: "Cannot read the projects recipes created: read namespace recipe-dev: 500" }) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    const section = screen.getByRole("region", { name: "Projects recipes created" });
    expect(section).toHaveTextContent("Cannot read the projects recipes created: read namespace recipe-dev: 500");
    expect(within(section).queryByRole("button", { name: /Delete project/ })).not.toBeInTheDocument();
  });

  it("F7: a clean-up that deleted the project says what it did, also once the run has nothing left to clean up", async () => {
    let status = ended({ cleanup: { recipeProjects: ["recipe-dev"], recipeProjectUIDs: { "recipe-dev": "0f3c1d2e-uid" } } });
    const streams: ReturnType<typeof sseResponse>[] = [];
    stubApi({
      ...editorReads,
      "GET /api/recipes/run": () => jsonResponse(status),
      "POST /api/resources/cleanup/stream": () => { const s = sseResponse(); streams.push(s); return s.response; },
    });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Clean up what the recipe set up..." }));
    const dialog = screen.getByRole("dialog", { name: /Clean up what recipe dev set up\?/ });
    await settle();
    fireEvent.click(within(dialog).getByRole("button", { name: "Clean up" }));
    await settle();
    // After deletion, the following status poll exposes no leftover cleanup or projects for the run.
    status = ended({ cleanup: undefined, projects: [] });
    await act(async () => {
      streams[0].push(sseEvent("recipe-project:recipe-dev", "success", "Deleted project recipe-dev; OpenShift removes what it holds."));
      streams[0].push(sseEvent("operation_complete", "success", "Deleted project recipe-dev; OpenShift removes what it holds."));
      streams[0].close();
    });
    await settle();
    expect(within(dialog).getByText("Cleaned up").closest(".pf-v6-c-alert")).toHaveTextContent("Deleted project recipe-dev; OpenShift removes what it holds.");
    expect(dialog).not.toHaveTextContent("0 parts");
    expect(within(dialog).getByText("Delete project recipe-dev, which a recipe created")).toBeInTheDocument();
  });
});
