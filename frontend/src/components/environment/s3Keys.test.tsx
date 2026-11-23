import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus, S3KeysState } from "../../types";
import { NOT_SECURED, SECURE_FIRST, storageNotReadyReason } from "./featureState";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com", usedBy: ["dev/dspa", "team-b/dspa"] };
const none: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const dspa: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "dspa", message: "Running", uiRoute: "https://dash/dev" };
const notSecured: S3KeysState = {
  secured: false, needsSecuring: true, oldKey: ["dev/dspa", "team-b/dspa"],
  keepOldKey: ["hand/dspa (not created by this tool)", "unrec/dspa (not recorded as this tool's)"],
};
const secured: S3KeysState = { secured: true, needsSecuring: false, ownKey: ["dev/dspa", "team-b/dspa"] };
const statusOf = (over: Partial<ResourcesStatus>): ResourcesStatus => ({ minio: none, mlflow: none, pipelineServers: [], ...over });

function renderAt(path: string, readBlocker: string | null = null) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} readBlocker={readBlocker} /></MemoryRouter>);
}
const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name}$`) });
const headerLabel = (title: string) => screen.getByRole("heading", { level: 2, name: title }).closest(".pf-v6-l-flex")!.querySelector(".pf-v6-c-label");

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("S3 storage that is not secured", () => {
  it("one label, Not secured, on the S3 storage tab, its SeaweedFS part, the Pipelines S3 part and the Overview row", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev", "new"] } });
    const { unmount } = renderAt("/test-resources?tab=storage");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    const storage = await part("SeaweedFS");
    expect(within(storage).getByText(NOT_SECURED)).toBeInTheDocument();
    expect(screen.getByText("Every pipeline project holds the S3 storage's admin key")).toBeInTheDocument();
    expect(storage.querySelector('dl[aria-label="S3 keys"]')).toHaveTextContent(/Old shared key\s*dev\/dspa, team-b\/dspa/);
    unmount();

    const p = renderAt("/test-resources?tab=pipelines&project=dev");
    expect(within(await part("S3 storage")).getByText(NOT_SECURED)).toBeInTheDocument();
    expect(await part("S3 storage")).toHaveTextContent("Secure S3 storage, on the S3 storage tab, gives each pipeline server a key of its own");
    p.unmount();

    renderAt("/test-resources");
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(s3).toHaveTextContent(NOT_SECURED));
  });

  it("Secure S3 storage... asks first, listing who gets a key, who keeps the old one, and what to restart", async () => {
    let status = statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured });
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/secure": () => { status = { ...status, s3Keys: secured }; return jsonResponse({ success: true, message: "The S3 storage is secured.", logs: [] }); },
    });
    renderAt("/test-resources?tab=storage");
    const storage = await part("SeaweedFS");
    fireEvent.click(within(storage).getByRole("button", { name: "Secure S3 storage..." }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    expect(dialog).toHaveTextContent("The old admin key, which every pipeline project holds, stops working.");
    expect(dialog).toHaveTextContent(/These pipeline servers get a key of their own.*dev\/dspa, team-b\/dspa/);
    expect(dialog).toHaveTextContent(/they keep the old key, which then no longer works: hand\/dspa \(not created by this tool\), unrec\/dspa \(not recorded as this tool's\)/);
    expect(dialog).toHaveTextContent("Pipeline runs in progress may fail");
    expect(dialog).toHaveTextContent("restart them afterwards");
    expect(api.calls.filter((c) => c.startsWith("POST "))).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources")).map((c) => c.split(" ")[1])).toEqual(["/api/resources/s3/secure"]));
    expect((await screen.findAllByText(/The S3 storage is secured/, {}, { timeout: 5000 })).length).toBeGreaterThan(0);
  });

  it("a reader sees the state but no Secure action", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage", "Read-only access: changing the cluster through this tool requires the cluster-admin role.");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.queryByRole("button", { name: "Secure S3 storage..." })).not.toBeInTheDocument();
    expect(screen.getByText("A cluster-admin runs it.")).toBeInTheDocument();
  });

  it("Set up Pipelines in a new project is refused and points at Secure S3 storage", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=pipelines&project=new");
    const setUp = await screen.findByRole("button", { name: "Set up Pipelines in new" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(setUp).toHaveAccessibleDescription(/Secure S3 storage, on the S3 storage tab/);
    expect(storageNotReadyReason(readyMinio, null, false, notSecured)).toBe(SECURE_FIRST);
    expect(storageNotReadyReason(readyMinio, null, false, secured)).toBeNull();
  });

  it("secured storage with one server left on the old key: still Not secured, and Set up works", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: true, ownKey: ["dev/dspa"], oldKey: ["team-b/dspa"] } }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=storage");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.getByText("Some pipeline servers still use the old key")).toBeInTheDocument();
    expect(await part("SeaweedFS")).toHaveTextContent(/Own key\s*dev\/dspa/);
  });
});
