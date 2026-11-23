import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";
import { CleanupModal, cleanupCandidates } from "./CleanupModal";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin" };
const noMlflow: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const unrecordedDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "dspa", managedByTool: true, unrecorded: true, message: "Running" };

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name}$`) });

async function menuItem(name: string, label: string | RegExp): Promise<HTMLElement | null> {
  const toggle = within(await part(name)).queryByRole("button", { name: `${name} actions` });
  if (!toggle) return null;
  fireEvent.click(toggle);
  return within(await screen.findByRole("menu")).queryByRole("menuitem", { name: label });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// G7: If the UID was never recorded, a pipeline server still labeled as this
// tool's is removed solely after the user confirms this tool provisioned it.
describe("an unrecorded pipeline server", () => {
  it("Delete says the tool has no record of it and sends the confirmation", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [unrecordedDspa] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["juntao-test"] },
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: true, message: "Deleted.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=juntao-test");
    fireEvent.click((await menuItem("Pipeline server", /Delete pipeline server/))!);
    const dialog = await screen.findByRole("dialog", { name: /Delete the pipeline server in juntao-test\?/ });
    expect(dialog).toHaveTextContent(/no record that it set it up/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await screen.findByRole("dialog", { name: "Delete the pipeline server in juntao-test" });
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test", confirmUnrecorded: true }]);
  });

  it("Clean up offers it unticked, says why, and confirms it only when ticked", () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [unrecordedDspa, { ...unrecordedDspa, namespace: "kept", unrecorded: false }] };
    expect(cleanupCandidates(status, "pipelines", false).servers).toEqual(["juntao-test", "kept"]);
    const onConfirm = vi.fn();
    render(<CleanupModal scope="pipelines" status={status} minioLeftovers={false} baseReason={null} onConfirm={onConfirm} onCancel={() => {}} />);
    const box = screen.getByRole("checkbox", { name: /Pipeline server in juntao-test/ });
    expect(box).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: /Pipeline server in kept/ })).toBeChecked();
    expect(screen.getByText(/no record that it set it up/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("checkbox", { name: "S3 storage" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["kept"] });
    fireEvent.click(box);
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["kept", "juntao-test"], confirmUnrecorded: ["juntao-test"] });
  });
});
