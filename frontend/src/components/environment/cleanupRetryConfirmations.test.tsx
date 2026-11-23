import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import type { ResourceState, ResourcesStatus } from "../../types";
import { CleanupModal, type CleanupRetry } from "./CleanupModal";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin" };
const noMlflow: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const unrecordedDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "dspa", managedByTool: true, unrecorded: true, uid: "uid-1", message: "Running" };

// A Clean up everything that a recipe project refused comes back with the
// same choices, so a pipeline server the user confirmed as this tool's (by
// UID) stays confirmed; one made again since (another UID) is unticked.
describe("a refused clean-up with an unrecorded pipeline server", () => {
  const status = (dspa: ResourceState): ResourcesStatus => ({
    minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa],
    recipeProjects: [{ name: "recipe-old", uid: "u2", recipe: "old", createdAt: "2026-11-22T05:20:00Z" }],
  });
  const retry: CleanupRetry = {
    req: { pipelineServers: ["juntao-test"], confirmUnrecorded: { "juntao-test": "uid-1" }, recipeProjects: ["recipe-old"], recipeProjectUIDs: { "recipe-old": "u2" } },
    contents: { "recipe-old": ["Notebook wb"] },
  };

  it("keeps the server's confirmation and adds the project's", () => {
    const onConfirm = vi.fn();
    const modal = (s: ResourcesStatus) => <CleanupModal scope="all" status={s} minioLeftovers={false} baseReason={null} retry={retry} onConfirm={onConfirm} onCancel={() => {}} />;
    const { rerender } = render(modal(status(unrecordedDspa)));
    const box = () => screen.getByRole("checkbox", { name: /Pipeline server in juntao-test/ });
    expect(box()).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "S3 storage" })).not.toBeChecked();
    const ask = screen.getByText("Project recipe-old holds what this tool did not create").closest(".pf-v6-c-alert") as HTMLElement;
    // The list's limits are visible: it names only the kinds the tool checks.
    expect(ask).toHaveTextContent("Only Notebooks (workbenches), InferenceServices and LLMInferenceServices (models), PersistentVolumeClaims (volumes) and Pods are listed; anything else in the project is deleted with it.");
    fireEvent.click(within(ask).getByRole("checkbox", { name: "Delete it with project recipe-old" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({
      pipelineServers: ["juntao-test"], confirmUnrecorded: { "juntao-test": "uid-1" },
      recipeProjects: ["recipe-old"], recipeProjectUIDs: { "recipe-old": "u2" }, confirmProjectContents: { "recipe-old": ["Notebook wb"] },
    });
    // Made again since the refusal: the confirmation was of the other one.
    rerender(modal(status({ ...unrecordedDspa, uid: "uid-2" })));
    expect(box()).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ recipeProjects: ["recipe-old"], recipeProjectUIDs: { "recipe-old": "u2" }, confirmProjectContents: { "recipe-old": ["Notebook wb"] } });
  });
});

// Each recipe project is deleted as the user reviewed it: the request
// carries the UID the dialog showed when it opened. One deleted and made
// again since (another UID in a refreshed status) is unticked, and ticking
// it again is the review of the one there now.
describe("recipe projects in Clean up everything", () => {
  const status = (uid: string): ResourcesStatus => ({
    minio: noMlflow, mlflow: noMlflow, pipelineServers: [],
    recipeProjects: [{ name: "recipe-old", uid, recipe: "old", createdAt: "2026-11-22T05:20:00Z" }],
  });

  it("sends the UID shown; a project made again since is unticked until ticked again", () => {
    const onConfirm = vi.fn();
    const modal = (s: ResourcesStatus) => <CleanupModal scope="all" status={s} minioLeftovers={false} baseReason={null} onConfirm={onConfirm} onCancel={() => {}} />;
    const { rerender } = render(modal(status("u2")));
    const box = () => screen.getByRole("checkbox", { name: "Project recipe-old" });
    expect(box()).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ recipeProjects: ["recipe-old"], recipeProjectUIDs: { "recipe-old": "u2" } });
    rerender(modal(status("u3")));
    expect(box()).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Delete" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(box());
    expect(box()).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ recipeProjects: ["recipe-old"], recipeProjectUIDs: { "recipe-old": "u3" } });
  });
});
