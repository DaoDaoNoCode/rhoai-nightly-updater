import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { RecipesPage } from "./RecipesPage";
import { RECIPE_RUN_IDLE_POLL_MS } from "../constants";
import { jsonResponse, stubApi } from "../test/apiStub";
import { contract } from "../test/contract";
import { renderPage, settleApps } from "../test/providers";
import { setDocumentHidden } from "../test/utils";
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

describe("Recipes page follows a run changed elsewhere", () => {
  it("F1: an idle page reads the run again while the tab is visible, so a run resumed elsewhere shows without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status = ended({}, { state: "stopped", errorCode: "stopped", stopReason: "Stopped after Pipeline server, as asked. Resume continues with What MLflow needs." });
    const api = stubApi({ ...editorReads, "GET /api/recipes/run": () => jsonResponse(status) });
    renderPage(<RecipesPage />, "/recipes");
    await settleApps();
    await settle();
    expect(screen.getByText("Stopped after Pipeline server, as asked. Resume continues with What MLflow needs.")).toBeInTheDocument();
    // Run state advances to succeeded out of band—different tab, another user, or the API.
    status = ended({}, { state: "succeeded", errorCode: undefined, stopReason: "Every step of dev is set up.", resumedAt: new Date().toISOString() });
    const reads = () => api.calls.filter((c) => c === "GET /api/recipes/run").length;
    const before = reads();
    await act(() => vi.advanceTimersByTimeAsync(RECIPE_RUN_IDLE_POLL_MS));
    await settle();
    expect(reads()).toBe(before + 1);
    expect(screen.getByText("Every step of dev is set up.")).toBeInTheDocument();
    // With the tab hidden, polling does not refetch; visibility again forces one read right away.
    await act(async () => { setDocumentHidden(true); });
    const hidden = reads();
    await act(() => vi.advanceTimersByTimeAsync(RECIPE_RUN_IDLE_POLL_MS * 3));
    await settle();
    expect(reads()).toBe(hidden);
    await act(async () => { setDocumentHidden(false); });
    await settle();
    expect(reads()).toBe(hidden + 1);
  });
});
