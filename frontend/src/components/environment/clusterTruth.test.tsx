import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";
import { CleanupModal, cleanupBlocked, cleanupCandidates } from "./CleanupModal";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin" };
const noMlflow: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const unrecordedDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "dspa", managedByTool: true, unrecorded: true, uid: "uid-1", message: "Running" };

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
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test", confirmUnrecorded: "uid-1" }]);
  });

  it("Clean up offers it unticked, says why, and confirms it only when ticked", () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [unrecordedDspa, { ...unrecordedDspa, namespace: "kept", unrecorded: false, uid: undefined }] };
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
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["kept", "juntao-test"], confirmUnrecorded: { "juntao-test": "uid-1" } });
  });

  // A refresh while the dialog is open can turn a ticked recorded server
  // unrecorded, or replace a confirmed one: the confirmation is the user's
  // explicit tick of that server (its UID), never derived from the ticks.
  it("Clean up never confirms a server the user did not tick as unrecorded", () => {
    const recorded: ResourceState = { ...unrecordedDspa, namespace: "kept", unrecorded: false, uid: undefined };
    const at = (kept: ResourceState): ResourcesStatus => ({ minio: readyMinio, mlflow: noMlflow, pipelineServers: [kept] });
    const onConfirm = vi.fn();
    const modal = (status: ResourcesStatus) => <CleanupModal scope="pipelines" status={status} minioLeftovers={false} baseReason={null} onConfirm={onConfirm} onCancel={() => {}} />;
    const { rerender } = render(modal(at(recorded)));
    fireEvent.click(screen.getByRole("checkbox", { name: "S3 storage" }));
    const box = () => screen.getByRole("checkbox", { name: /Pipeline server in kept/ });
    expect(box()).toBeChecked();
    // The record lost it: unticked, not sent, and not confirmed.
    rerender(modal(at({ ...recorded, unrecorded: true, uid: "uid-k" })));
    expect(box()).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).not.toHaveBeenCalled();
    // Ticked: the confirmation names that server.
    fireEvent.click(box());
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["kept"], confirmUnrecorded: { kept: "uid-k" } });
    // Made again (another UID): the confirmation was of the other one.
    rerender(modal(at({ ...recorded, unrecorded: true, uid: "uid-k2" })));
    expect(box()).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});

// N3: this tool's objects in a project that is no longer a Data Science
// project are listed apart (the project picker does not show them), can be
// deleted, and count as this tool's in Clean up.
describe("this tool's objects outside Data Science projects", () => {
  const outsideDspa: ResourceState = { deployed: true, ready: true, namespace: "plain", name: "dspa", managedByTool: true, message: "Running" };
  const usedMinio: ResourceState = { ...readyMinio, usedBy: ["plain/dspa"], teardownBlockedReason: "1 pipeline server uses this S3 storage: plain/dspa." };

  it("the Pipelines tab names the pipeline server and deletes it", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: usedMinio, mlflow: noMlflow, pipelineServers: [], outsidePipelineServers: [outsideDspa] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["juntao-test"] },
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: true, message: "Deleted.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=juntao-test");
    const alert = (await screen.findByText("Set up by this tool outside data science projects")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("Pipeline server dspa in project plain (no longer a data science project)");
    fireEvent.click(within(alert).getByRole("button", { name: /Delete/ }));
    const dialog = await screen.findByRole("dialog", { name: /Delete the pipeline server in plain\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await screen.findByRole("dialog", { name: "Delete the pipeline server in plain" });
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "plain" }]);
  });

  it("Clean up offers them and the storage blocker counts them as this tool's", () => {
    const status: ResourcesStatus = {
      minio: usedMinio, mlflow: noMlflow, pipelineServers: [], outsidePipelineServers: [outsideDspa],
      models: { catalog: [], outside: [{ deployed: true, ready: true, namespace: "plain", name: "qwen3-1-7b", managedByTool: true, message: "Running" }] },
    };
    const c = cleanupCandidates(status, "all", false);
    expect(c.servers).toEqual(["plain"]);
    expect(c.models).toEqual(["plain"]);
    expect(cleanupBlocked(status, { storage: true })).toMatch(/Select that pipeline server too/);
    expect(cleanupBlocked(status, { storage: true, pipelineServers: ["plain"] })).toBeNull();
  });
});

// N1: Clean up lists what a fresh read finds, not the last snapshot.
describe("opening Clean up", () => {
  it("reads the status again", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [{ ...unrecordedDspa, unrecorded: false }] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["juntao-test"] },
    });
    const reads = () => api.calls.filter((c) => c.startsWith("GET /api/resources/status")).length;
    renderAt("/test-resources?tab=pipelines&project=juntao-test");
    await screen.findByText("Pipeline server");
    const open = screen.getAllByRole("button").find((b) => b.textContent?.includes("Clean up Pipelines"))!;
    await waitFor(() => expect(reads()).toBe(1));
    fireEvent.click(open);
    await screen.findByRole("dialog");
    await waitFor(() => expect(reads()).toBe(2));
  });
});

// N8: what a storage deletion left is named, from the cluster, with Delete.
describe("a storage deletion that did not finish", () => {
  it("names what it left and offers Delete", async () => {
    stubApi({
      "/api/resources/status": { minio: { deployed: false, ready: false, namespace: "seaweedfs", managedByTool: true, message: "Namespace exists but the S3 storage is not deployed", leftovers: ["Secret seaweedfs-s3-credentials"] }, mlflow: noMlflow, pipelineServers: [] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["juntao-test"] },
    });
    renderAt("/test-resources?tab=pipelines&project=juntao-test");
    const storage = await part("S3 storage");
    expect(storage).toHaveTextContent("A deletion that did not finish left Secret seaweedfs-s3-credentials in namespace seaweedfs; Delete S3 storage removes it.");
    expect(await menuItem("S3 storage", /Delete S3 storage/)).not.toBeNull();
  });
});
