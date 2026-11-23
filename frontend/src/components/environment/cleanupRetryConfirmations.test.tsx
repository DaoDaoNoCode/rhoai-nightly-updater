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
    req: { pipelineServers: ["juntao-test"], confirmUnrecorded: { "juntao-test": "uid-1" }, recipeProjects: ["recipe-old"] },
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
    fireEvent.click(within(ask).getByRole("checkbox", { name: "Delete it with project recipe-old" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({
      pipelineServers: ["juntao-test"], confirmUnrecorded: { "juntao-test": "uid-1" },
      recipeProjects: ["recipe-old"], confirmProjectContents: { "recipe-old": ["Notebook wb"] },
    });
    // Made again since the refusal: the confirmation was of the other one.
    rerender(modal(status({ ...unrecordedDspa, uid: "uid-2" })));
    expect(box()).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ recipeProjects: ["recipe-old"], confirmProjectContents: { "recipe-old": ["Notebook wb"] } });
  });
});
