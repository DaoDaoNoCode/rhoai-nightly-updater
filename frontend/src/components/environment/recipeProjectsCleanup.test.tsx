import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";

const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running" };
const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };

const sse = (...events: object[]) => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

// F4: recipe-backed projects count toward Clean up everything and use the
// same safeguards as the Recipes page.
describe("Clean up everything with recipe projects", () => {
  const status: ResourcesStatus = {
    minio: none, mlflow: ownMlflow, pipelineServers: [],
    recipeProjects: [
      { name: "recipe-dev", uid: "u1", recipe: "dev", createdAt: "2026-11-23T05:20:00Z" },
      { name: "recipe-old", uid: "u2", recipe: "old", createdAt: "2026-11-22T05:20:00Z" },
    ],
  };

  it("offers each, ticked; a project that holds what the tool did not create is asked about, then deleted with it", async () => {
    let posts = 0;
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["a"] },
      "POST /api/resources/cleanup/stream": () => (++posts === 1
        ? sse(
          { step: "recipe-project:recipe-old", status: "failed", message: "Project recipe-old still holds what this tool did not create: Notebook wb.", errorCode: "confirm_project_contents", detail: "Notebook wb" },
          { step: "operation_complete", status: "failed", message: "Project recipe-old still holds what this tool did not create: Notebook wb. Nothing was deleted.", errorCode: "confirm_project_contents" },
        )
        : sse(
          { step: "mlflow", status: "success", message: "MLflow removed." },
          { step: "recipe-project:recipe-dev", status: "success", message: "Deleted project recipe-dev; OpenShift removes what it holds." },
          { step: "recipe-project:recipe-old", status: "success", message: "Deleted project recipe-old; OpenShift removes what it holds." },
          { step: "operation_complete", status: "success", message: "Deleted MLflow. Deleted project recipe-dev; OpenShift removes what it holds. Deleted project recipe-old; OpenShift removes what it holds." },
        )),
    });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything…" }));
    let dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    for (const name of ["Project recipe-dev", "Project recipe-old"]) {
      expect(within(dialog).getByRole("checkbox", { name })).toBeChecked();
    }
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent("Everything in project recipe-old.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));

    // Nothing is removed yet; the modal reappears with the prior selections and adds a confirm step.
    dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    const ask = within(dialog).getByText("Project recipe-old holds what this tool did not create").closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(ask).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Notebook wb"]);
    expect(within(dialog).getByRole("checkbox", { name: "Project recipe-old" })).toBeChecked();
    expect(within(dialog).getByRole("button", { name: "Delete" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(ask).getByRole("checkbox", { name: "Delete it with project recipe-old" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Clean up" });
    expect(await within(run).findByText("Deleted MLflow. Deleted project recipe-dev; OpenShift removes what it holds. Deleted project recipe-old; OpenShift removes what it holds.")).toBeInTheDocument();
    expect(within(run).getByText("Delete project recipe-old, which a recipe created")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([
      { mlflow: true, recipeProjects: ["recipe-dev", "recipe-old"] },
      { mlflow: true, recipeProjects: ["recipe-dev", "recipe-old"], confirmProjectContents: { "recipe-old": ["Notebook wb"] } },
    ]);
  });

  it("only recipe projects left: Clean up everything is still offered; a list that cannot be read is said", async () => {
    stubApi({
      "/api/resources/status": { minio: none, mlflow: none, pipelineServers: [], recipeProjects: [status.recipeProjects![1]], recipeProjectsError: "" },
      "/api/resources/projects": { projects: ["a"] },
    });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything…" }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    expect(within(dialog).getByRole("checkbox", { name: "Project recipe-old" })).toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("the projects recipes created cannot be read: the dialog says so, and offers none", async () => {
    stubApi({
      "/api/resources/status": { minio: none, mlflow: ownMlflow, pipelineServers: [], recipeProjectsError: "Cannot read the projects recipes created: read namespace recipe-dev: 500" },
      "/api/resources/projects": { projects: ["a"] },
    });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything…" }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    expect(dialog).toHaveTextContent("Cannot read the projects recipes created: read namespace recipe-dev: 500");
    expect(within(dialog).queryByRole("checkbox", { name: /^Project / })).not.toBeInTheDocument();
  });
});
