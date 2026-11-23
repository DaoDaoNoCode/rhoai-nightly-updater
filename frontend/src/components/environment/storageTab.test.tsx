import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import type { LegacyStorageState, ResourceState, ResourcesStatus } from "../../types";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com", usedBy: ["dev/dspa"] };
const none: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const foreign: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Namespace exists", setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." };
const legacy: LegacyStorageState = { namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"], stage: "migrate", namespaceDeletable: false, usedBy: ["dev/dspa"] };
const dspa: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "dspa", message: "Running", uiRoute: "https://dash/dev" };
const statusOf = (over: Partial<ResourcesStatus>): ResourcesStatus => ({ minio: none, mlflow: none, pipelineServers: [], ...over });

function renderAt(path: string, readBlocker: string | null = null) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} readBlocker={readBlocker} /></MemoryRouter>);
}

const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name}$`) });
/** Locator for the flex row that holds the tab title and status label. */
async function header(): Promise<HTMLElement> {
  return waitFor(() => {
    const h = screen.getByRole("heading", { level: 2, name: "S3 storage" }).closest(".pf-v6-l-flex") as HTMLElement;
    expect(h.querySelector(".pf-v6-c-label")).not.toBeNull();
    return h;
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the S3 storage tab", () => {
  it("ready: its health, who uses it, its details and admin UI; no project picker and nothing to set up", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa] }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage");
    expect(screen.getByRole("tab", { name: "S3 storage" })).toHaveAttribute("aria-selected", "true");
    expect((await header()).querySelector(".pf-v6-c-label")).toHaveTextContent(/^Ready$/);
    expect(screen.getByRole("tabpanel")).toHaveTextContent("The pipeline servers this tool sets up keep their artifacts here, in bucket pipelines");
    expect(screen.getByRole("tabpanel")).toHaveTextContent("Other features can use it too.");
    const storage = await part("SeaweedFS");
    expect(storage).toHaveTextContent("Shared by the cluster");
    expect(storage.querySelector('dl[aria-label="S3 storage details"]')).toHaveTextContent(/Used by\s*dev\/dspa/);
    expect(within(storage).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", readyMinio.uiRoute);
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
  });

  it("not set up: Set up S3 storage asks before it changes anything", async () => {
    stubApi({ "/api/resources/status": statusOf({}), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage");
    expect((await header()).querySelector(".pf-v6-c-label")).toHaveTextContent(/^Not set up$/);
    fireEvent.click(await screen.findByRole("button", { name: "Set up S3 storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up the S3 storage?" });
    expect(dialog).toHaveTextContent("On the cluster: S3 storage (20 GiB), shared by every pipeline server this tool sets up.");
  });

  it("a reader: Set up says who can help", async () => {
    stubApi({ "/api/resources/status": statusOf({}), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage", "Read-only access: changing the cluster through this tool requires the cluster-admin role.");
    const setUp = await screen.findByRole("button", { name: "Set up S3 storage" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(setUp).toHaveAccessibleDescription(/Ask one to set it up for you/);
  });

  it("migration needed: Migration needed with Migrate storage, and no Set up", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: { ...none, namespace: "seaweedfs", migrationPending: true }, legacyStorage: legacy, pipelineServers: [dspa] }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage");
    expect((await header()).querySelector(".pf-v6-c-label")).toHaveTextContent(/^Migration needed$/);
    const why = screen.getByText("Your pipeline data is in storage from an earlier version").closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(why).queryByRole("button", { name: "Migrate storage" })).not.toBeInTheDocument();
    const storage = await part("SeaweedFS");
    expect(within(storage).getByText("Migration needed")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Migrate storage" })).toHaveLength(1);
    expect(within(storage).getByRole("button", { name: "Migrate storage" })).toHaveClass("pf-m-primary");
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
  });

  it("a namespace someone else made: why, inline, ownership apart, and no Set up", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: foreign }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage");
    expect(await header()).toHaveTextContent("Not managed by this tool");
    const storage = await part("SeaweedFS");
    expect(within(storage).getByText("The S3 storage can't be set up: namespace seaweedfs belongs to someone else")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
  });
});

describe("the S3 storage elsewhere", () => {
  it("migration needed: Pipelines and the Overview name the storage, not a project, and send to the S3 storage tab", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: { ...none, namespace: "seaweedfs", migrationPending: true }, legacyStorage: legacy, pipelineServers: [dspa] }), "/api/resources/projects": { projects: ["dev"] } });
    const { unmount } = renderAt("/test-resources?tab=pipelines&project=dev");
    const label = () => screen.getByRole("heading", { level: 2, name: "Pipelines" }).closest(".pf-v6-l-flex")!.querySelector(".pf-v6-c-label");
    await waitFor(() => expect(label()).toHaveTextContent(/^Migration needed$/));
    expect(screen.getByRole("tabpanel")).not.toHaveTextContent(/Needs attention in dev/);
    expect(within(await part("Pipeline server")).getByText("Ready")).toBeInTheDocument();
    expect(within(await part("S3 storage")).getByText("Migration needed")).toBeInTheDocument();
    unmount();
    renderAt("/test-resources");
    const pipelines = (await screen.findByRole("link", { name: "Pipelines" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(pipelines).toHaveTextContent("Migration needed"));
    // Migration is offered only on the S3 storage row and tab.
    expect(within(pipelines).queryByRole("button", { name: /Migrate storage/ })).not.toBeInTheDocument();
    expect(within(pipelines).getByRole("button", { name: "Show issue" })).toBeInTheDocument();
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    expect(s3).toHaveTextContent("Migration needed");
    expect(within(s3).getByRole("button", { name: /^Migrate storage/ })).toBeInTheDocument();
  });

  it("Pipelines shows the storage's health only, with a link to its tab", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa] }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=pipelines&project=dev");
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Ready")).toBeInTheDocument();
    expect(storage).toHaveTextContent("Pipeline servers this tool sets up keep their artifacts in it, in bucket pipelines.");
    // Detail fields, credentials, and mutating controls stay on the storage tab; this block is summary-only.
    expect(storage).not.toHaveTextContent(/Endpoint|Access key/);
    expect(within(storage).queryByRole("button", { name: "S3 storage actions" })).not.toBeInTheDocument();
    const link = within(storage).getByRole("link", { name: "Open S3 storage" });
    expect(link).toHaveAttribute("href", "/test-resources?tab=storage");
    fireEvent.click(link);
    expect(await screen.findByRole("heading", { level: 2, name: "S3 storage" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "S3 storage" })).toHaveAttribute("aria-selected", "true");
  });

  it("Pipelines without storage: Set up Pipelines still sets it up first", async () => {
    stubApi({ "/api/resources/status": statusOf({}), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=pipelines&project=dev");
    expect(await part("S3 storage")).toHaveTextContent("Setting up a pipeline server sets it up first.");
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up Pipelines in dev?" });
    expect(dialog).toHaveTextContent("On the cluster: S3 storage (20 GiB)");
  });

  it("the Overview's S3 storage row sets up the storage alone, checked against the storage's own state", async () => {
    let status = statusOf({});
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/setup": () => { status = statusOf({ minio: readyMinio }); return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] }); },
    });
    renderAt("/test-resources");
    const row = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(row).toHaveTextContent("Not set up"));
    fireEvent.click(await within(row).findByRole("button", { name: /^Set up S3 storage/ }));
    const confirm = await screen.findByRole("dialog", { name: "Set up the S3 storage?" });
    fireEvent.click(within(confirm).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up the S3 storage" });
    expect(await within(run).findByText(/Pipeline servers set up on the Pipelines tab keep their artifacts in it/)).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources")).map((c) => c.split(" ")[1])).toEqual(["/api/resources/s3/setup"]);
  });
});
