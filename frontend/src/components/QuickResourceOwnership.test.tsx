import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator, nextStep } from "./QuickResourceCreator";
import { stubApi, jsonResponse } from "../test/apiStub";
import type { LegacyStorageState, ResourceState, ResourcesStatus } from "../types";

// A SeaweedFS whose image cannot be pulled, used by a pipeline server
// (GET /api/resources/status).
const brokenMinio: ResourceState = {
  deployed: true, ready: false, namespace: "seaweedfs", managedByTool: true,
  message: "ImagePullBackOff: Back-off pulling image \"mirror.example.com/seaweedfs:4.54\": unauthorized",
  waitingReason: "ImagePullBackOff", terminalError: true, dataPVCs: ["seaweedfs-data"], uiUser: "admin",
  teardownBlockedReason: "1 pipeline server uses this S3 storage: juntao-test/dspa. Tear it down first.",
};
const browserMlflow: ResourceState = {
  deployed: true, ready: true, message: "Running", namespace: "redhat-ods-applications", managedByTool: false,
  currentImage: "quay.io/opendatahub/mlflow:odh-stable",
  teardownBlockedReason: "This MLflow instance was not created by this tool, so the tool will not delete it.",
};
const legacyDspa: ResourceState = {
  deployed: true, ready: false, namespace: "juntao-test", name: "dspa", managedByTool: true, terminalError: true,
  message: "Could not connect to (minio-service.minio.svc:9000): connection refused", dataPVCs: ["mariadb-dspa"],
};
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running" };

function live(over: Partial<ResourcesStatus> = {}): ResourcesStatus {
  return { minio: brokenMinio, mlflow: browserMlflow, pipelineServers: [legacyDspa], unmanagedPipelineProjects: ["team-x"], ...over };
}

function setup(status: ResourcesStatus, extra: Record<string, unknown> = {}) {
  return stubApi({
    "/api/resources/status": status,
    "/api/resources/projects": { projects: ["juntao-test", "team-x", "fresh"] },
    ...extra,
  });
}

/** The page at path (/test-resources?tab=...), as the router serves it. */
function renderAt(path = "/test-resources?tab=cluster", props: Partial<React.ComponentProps<typeof QuickResourceCreator>> = {}) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} {...props} /></MemoryRouter>);
}

/** A compact card, by its title. */
const card = (title: string) => screen.findByRole("region", { name: title });

/** Opens a card's details; returns the drawer panel. */
async function details(title: string, cardTitle = title): Promise<HTMLElement> {
  fireEvent.click(within(await card(cardTitle)).getByRole("button", { name: `${title} details` }));
  const panel = document.getElementById("environment-details");
  if (!panel) throw new Error("no details panel");
  return panel;
}

const tooltipOf = async (button: HTMLElement) => {
  fireEvent.mouseEnter(button);
  return (await screen.findByRole("tooltip")).textContent;
};

describe("terminal states (A08-3, A06-5)", () => {
  it("S3 storage in ImagePullBackOff reads Error, offers Repair, and the details say why and what to do", async () => {
    setup(live());
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Error")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Repair" })).toHaveClass("pf-m-primary");
    const panel = await details("Storage");
    expect(within(panel).getByText("ImagePullBackOff")).toBeInTheDocument();
    expect(panel).toHaveTextContent(/Repair applies the SeaweedFS Deployment again/);
    // Tear down's tooltip gives the blocked reason; it is not printed.
    const teardown = within(panel).getByRole("button", { name: "Tear down" });
    expect(teardown).toHaveAttribute("aria-disabled", "true");
    expect(panel).not.toHaveTextContent(/Tear it down first/);
    expect(await tooltipOf(teardown)).toMatch(/1 pipeline server uses this S3 storage/);
  });

  it("a pipeline server that cannot reach the storage says so instead of spinning", async () => {
    setup(live());
    renderAt("/test-resources?tab=projects&project=juntao-test");
    const ps = await card("Pipeline server");
    expect(within(ps).getByText("Error")).toBeInTheDocument();
    const panel = await details("Pipeline server");
    expect(panel).toHaveTextContent(/cannot reach the S3 storage. Fix the storage first/);
    expect(screen.queryByText("Setting up")).not.toBeInTheDocument();
  });

  it("nextStep per reason", () => {
    expect(nextStep("mlflow", { deployed: true, ready: false, waitingReason: "CrashLoopBackOff", terminalError: true, prOverride: true })).toMatch(/PR build keeps crashing. Revert/);
    expect(nextStep("minio", { deployed: true, ready: false, waitingReason: "CreateContainerConfigError", terminalError: true })).toMatch(/Secret or ConfigMap/);
    expect(nextStep("minio", { deployed: true, ready: false, message: "Provisioning" })).toBeNull();
  });
});

describe("ownership", () => {
  it("does not call a resource that does not exist 'not managed by this tool'", async () => {
    // The backend reports managedByTool false when nothing is deployed.
    const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
    setup(live({ minio: none, mlflow: none, pipelineServers: [] }));
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Not set up")).toBeInTheDocument();
    expect(screen.queryByText("Not managed by this tool")).not.toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Set up" })).toBeInTheDocument();
  });

  it("still marks a foreign namespace that blocks setup, and Set up says why", async () => {
    const foreign: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Namespace exists but the S3 storage is not deployed", setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." };
    setup(live({ minio: foreign }));
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(await tooltipOf(within(storage).getByRole("button", { name: "Set up" }))).toMatch(/was not created by this tool/);
  });

  it("a browser-created MLflow is 'not managed by this tool' and has no Tear down", async () => {
    setup(live());
    renderAt();
    const mlflow = await card("MLflow");
    expect(within(mlflow).getByText("Not managed by this tool")).toBeInTheDocument();
    const panel = await details("MLflow");
    expect(panel).toHaveTextContent("Not this tool: it is left alone");
    expect(within(panel).queryByRole("button", { name: "Tear down" })).not.toBeInTheDocument();
  });

  it("a project with someone else's pipeline server is not set up again, and says why", async () => {
    setup(live({ minio: readyMinio }));
    renderAt("/test-resources?tab=projects&project=team-x");
    const ps = await card("Pipeline server");
    expect(within(ps).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(ps).toHaveTextContent("this tool did not create");
    expect(within(ps).queryByRole("button", { name: "Set up" })).not.toBeInTheDocument();
    // The project picker says what each project has.
    fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
    const team = await screen.findByRole("option", { name: /team-x/ });
    expect(team).toHaveTextContent("Pipeline server: Not managed by this tool");
  });
});

describe("MLflow PR override (A04-5)", () => {
  it("the card offers Revert when the PR image broke MLflow; the details keep Deploy PR and Tear down", async () => {
    setup(live({ mlflow: { deployed: true, ready: false, managedByTool: true, prOverride: true, prNumber: 399, revertImage: "", waitingReason: "CrashLoopBackOff", terminalError: true, message: "CrashLoopBackOff" } }));
    renderAt();
    const mlflow = await card("MLflow");
    expect(mlflow).toHaveTextContent("Runs the image of opendatahub-io/mlflow PR #399.");
    const revert = within(mlflow).getByRole("button", { name: "Revert the PR image" });
    expect(revert).not.toHaveAttribute("aria-disabled");
    const panel = await details("MLflow");
    expect(within(panel).getByRole("button", { name: "Deploy PR" })).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: "Tear down" })).toBeInTheDocument();
    fireEvent.click(revert);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/so the operator's default \(RHOAI\) image is used again/)).toBeInTheDocument();
  });

  it("deploying a PR on an MLflow the tool did not create warns in the confirmation", async () => {
    setup(live());
    renderAt();
    const panel = await details("MLflow");
    fireEvent.change(within(panel).getByRole("textbox", { name: /Deploy an opendatahub-io\/mlflow PR/ }), { target: { value: "42" } });
    fireEvent.click(within(panel).getByRole("button", { name: "Deploy PR" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("quay.io/opendatahub/mlflow:odh-pr-42")).toBeInTheDocument();
    expect(within(dialog).getByText("This MLflow instance was not created by this tool")).toBeInTheDocument();
  });
});

describe("teardown confirmations state the data loss", () => {
  it("pipeline server teardown names the DSPA and the database PVC; in_progress is info and keeps checking", async () => {
    const api = setup(live({ minio: readyMinio }), {
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: false, message: "The DSPA is still being deleted by the pipelines operator.", logs: [], errorCode: "in_progress" }, 422),
    });
    renderAt("/test-resources?tab=projects&project=juntao-test");
    const panel = await details("Pipeline server");
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered")).toBeInTheDocument();
    expect(within(dialog).getByText("mariadb-dspa")).toBeInTheDocument();
    expect(within(dialog).getByText("dspa")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down" }));
    expect(await screen.findByText("Still in progress")).toBeInTheDocument();
    expect(screen.getByText(/This page keeps checking/)).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test" }]);
  });

  it("a conversion-webhook refusal is shown with the server's reason", async () => {
    setup(live({ minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: false, errorCode: "prerequisites", logs: [], message: "Refusing to delete namespace seaweedfs: CRD mcpservers.mcp.x-k8s.io has a conversion webhook whose Service redhat-ods-applications/mcp-lifecycle-operator-webhook-service is missing." }, 422),
    });
    renderAt();
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText("seaweedfs/seaweedfs-data").length).toBeGreaterThan(0);
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText(/mcpservers.mcp.x-k8s.io has a conversion webhook/)).toBeInTheDocument();
  });
});

describe("S3 storage (kept namespace, warning, admin login)", () => {
  const kept = "S3 storage removed. PVC seaweedfs/seaweedfs-data and all stored objects (pipeline artifacts, models, test files) are deleted with it. Namespace 'seaweedfs' was kept: it may hold objects this tool did not create. Delete it with `oc delete project seaweedfs` once you've checked it's empty.";
  const afterTeardown: ResourceState = { deployed: false, ready: false, namespace: "seaweedfs", managedByTool: true, message: "Namespace exists but the S3 storage is not deployed" };
  const running: ResourceState = { deployed: true, ready: true, namespace: "seaweedfs", managedByTool: true, message: "Running", dataPVCs: ["seaweedfs-data"], uiUser: "admin", uiRoute: "https://seaweedfs-admin-seaweedfs.apps.example.com" };

  it("a running storage: one sentence and Open admin UI on the card; names, warning and login in the details; no MinIO-era name anywhere", async () => {
    const warning = "SeaweedFS runs mirror.example.com/seaweedfs:4.54, not the image this version deploys. Re-run setup to update it; the data PVC is kept.";
    setup(live({ minio: { ...running, warning }, pipelineServers: [] }));
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Needs attention")).toBeInTheDocument();
    expect(storage).toHaveTextContent("S3 storage (SeaweedFS) for pipeline artifacts and test files.");
    expect(storage).not.toHaveTextContent(/seaweedfs-s3|oc extract|namespace/);
    expect(within(storage).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", "https://seaweedfs-admin-seaweedfs.apps.example.com");
    const panel = await details("Storage");
    expect(within(panel).getByText(warning)).toBeInTheDocument();
    expect(panel).toHaveTextContent("http://seaweedfs-s3.seaweedfs.svc:8333");
    fireEvent.click(within(panel).getByRole("button", { name: "How to use" }));
    expect(panel).toHaveTextContent("Sign in to the admin UI as admin; the password is the S3 secret key.");
    expect(panel).not.toHaveTextContent(/oc extract/);
    expect(document.body).not.toHaveTextContent(/minio/i);
  });

  it("Copy admin password reads it from the server only on click and copies it, without showing it", async () => {
    const writes: string[] = [];
    const write = vi.fn(async (items: ClipboardItem[]) => {
      for (const item of items) writes.push(await (await item.getType("text/plain")).text());
    });
    class FakeClipboardItem {
      constructor(private data: Record<string, Promise<Blob>>) {}
      getType(type: string) { return this.data[type]; }
    }
    vi.stubGlobal("ClipboardItem", FakeClipboardItem);
    Object.defineProperty(navigator, "clipboard", { value: { write, writeText: vi.fn() }, configurable: true });
    const api = setup(live({ minio: running, pipelineServers: [] }), {
      "GET /api/resources/minio/admin-password": { user: "admin", password: "s3cret-value" },
    });
    renderAt();
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "How to use" }));
    expect(api.calls.some((c) => c.includes("admin-password"))).toBe(false);
    await act(async () => { fireEvent.click(within(panel).getByRole("button", { name: "Copy admin password" })); });
    expect(await within(panel).findByText("Copied the password of user admin.")).toBeInTheDocument();
    expect(writes).toEqual(["s3cret-value"]);
    expect(api.calls.filter((c) => c.includes("admin-password"))).toHaveLength(1);
    expect(document.body).not.toHaveTextContent("s3cret-value");
  });

  it("a reader is told why Copy admin password is off, and nothing is read", async () => {
    const api = setup(live({ minio: running, pipelineServers: [] }));
    renderAt("/test-resources?tab=cluster", { readBlocker: "Read-only access: changing the cluster through this tool requires the cluster-admin role." });
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "How to use" }));
    const copy = within(panel).getByRole("button", { name: "Copy admin password" });
    expect(copy).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(copy);
    expect(await tooltipOf(copy)).toMatch(/cluster-admin/);
    expect(api.calls.some((c) => c.includes("admin-password"))).toBe(false);
  });

  it("after a teardown the kept namespace reads Not set up with Set up only, and the result names oc delete project", async () => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: [] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: true, message: kept, logs: [] }); },
    });
    renderAt();
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/Namespace seaweedfs is kept: delete it yourself with oc delete project seaweedfs once empty/);
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent(
      "PersistentVolumeClaim seaweedfs/seaweedfs-data, with every object stored in it (pipeline artifacts, uploaded files).");
    expect(dialog).not.toHaveTextContent(/minio/i);
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect((await screen.findByText(/^S3 storage removed/)).closest(".pf-v6-c-alert")).toHaveTextContent(/Namespace seaweedfs was kept: run oc delete project seaweedfs once you have checked it is empty/);
    const storage = await card("Storage");
    expect(await within(storage).findByText("Not set up")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Set up" })).toBeInTheDocument();
    expect(within(document.getElementById("environment-details")!).queryByRole("button", { name: "Tear down" })).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(1);
  });

  it.each([
    ["partial_failure", "Partly done", "Some S3 storage objects could not be removed: Secret seaweedfs/seaweedfs-s3-credentials: forbidden. Re-run teardown to retry."],
    ["in_progress", "Still in progress", "The S3 storage objects were deleted, but PVC seaweedfs/seaweedfs-data is still terminating after 1m0s; re-run teardown to check again."],
  ])("a %s teardown keeps Tear down available beside Set up", async (code, title, message) => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: [] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: false, errorCode: code, message, logs: [] }, 422); },
    });
    renderAt();
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText(title)).toBeInTheDocument();
    const storage = await card("Storage");
    expect(await within(storage).findByRole("button", { name: "Set up" })).toBeInTheDocument();
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Tear down S3 storage" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(2));
  });
});

describe("storage from an earlier version", () => {
  // What 2.1 to 2.3 left in namespace minio, still used by a pipeline server
  // an earlier version created and by one made by hand.
  const legacy: LegacyStorageState = {
    namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000",
    objects: ["Deployment seaweedfs", "NetworkPolicy seaweedfs-ingress", "Route minio-ui", "Service minio-service", "Secret minio-secret", "PersistentVolumeClaim seaweedfs-pvc", "PersistentVolumeClaim minio-pvc"],
    dataPVCs: [{ name: "seaweedfs-pvc", size: "20Gi" }, { name: "minio-pvc", size: "20Gi" }],
    uncopiedPVCs: ["minio-pvc"], usedBy: ["juntao-test/dspa", "team-x/dspa"],
    stage: "migrate", repoint: ["juntao-test/dspa"], notMoved: ["team-x/dspa (not created by this tool)"],
    namespaceDeletable: false, namespaceKeptReason: "Namespace 'minio' is kept: it does not carry this tool's label (app.kubernetes.io/managed-by=rhoai-nightly-updater).",
  };
  const blocked = "2 pipeline servers use this S3 storage: juntao-test/dspa, team-x/dspa. Tear them down first.";
  const notDeployed: ResourceState = {
    deployed: false, ready: false, namespace: "seaweedfs", managedByTool: false, message: "Not deployed", migrationPending: true, teardownBlockedReason: blocked,
  };
  const runningDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "dspa", managedByTool: true, message: "Running", dataPVCs: ["mariadb-dspa"] };
  const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

  it("reads Needs attention, offers Migrate storage, and names the old objects only in the details", async () => {
    setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Needs attention")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Migrate storage" })).toHaveClass("pf-m-primary");
    expect(within(storage).queryByRole("button", { name: "Set up" })).not.toBeInTheDocument();
    expect(storage).not.toHaveTextContent(/minio/i);
    const panel = await details("Storage");
    expect(within(panel).getByRole("button", { name: "Tear down" })).toHaveAttribute("aria-disabled", "true");
    const old = panel.querySelector("dl[aria-label=\"Storage from an earlier version\"]");
    expect(old).toHaveTextContent("SeaweedFS in namespace minio, at minio-service.minio.svc:9000");
    expect(old).toHaveTextContent("seaweedfs-pvc (20Gi), minio-pvc (20Gi). minio-pvc holds MinIO's files from before SeaweedFS and cannot be copied.");
  });

  it("new pipeline servers wait until Migrate storage has moved the data, even with the new storage up (follow-up 3)", async () => {
    setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    renderAt("/test-resources?tab=projects&project=fresh");
    const ps = await card("Pipeline server");
    const setUp = within(ps).getByRole("button", { name: "Set up" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(await tooltipOf(setUp)).toMatch(/once Migrate storage has moved the data/);
    expect(ps).toHaveTextContent("Needs: Storage on the Cluster tab.");
    // The link opens the Storage details on the Cluster tab.
    fireEvent.click(within(ps).getByRole("button", { name: "Storage" }));
    expect(await screen.findByRole("heading", { level: 2, name: "Storage" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Cluster", selected: true })).toBeInTheDocument();
  });

  it("Migrate storage shows the plan, runs as a stream, then offers the removal with the checklist", async () => {
    const api = setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }), {
      "POST /api/resources/minio/migrate/stream": sse(
        { step: "check", status: "success", message: "OK: SeaweedFS in namespace 'minio' is running", elapsedMs: 1 },
        { step: "storage", status: "success", message: "OK: The new storage is ready", elapsedMs: 2 },
        { step: "copy", status: "success", message: "OK: 4 objects (4330 bytes) copied and checked byte for byte", elapsedMs: 3 },
        { step: "repoint", status: "success", message: "OK: 1 pipeline server moved", elapsedMs: 4 },
        { step: "operation_complete", status: "success", message: "Migrated 4 objects (4330 bytes) to namespace 'seaweedfs'. The old storage in namespace 'minio' is kept until you remove it.", elapsedMs: 5 },
      ),
    });
    renderAt();
    fireEvent.click(within(await card("Storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Moves the pipeline server this tool created to the new storage, one at a time, and waits until each reaches it: juntao-test/dspa.");
    expect(dialog).toHaveTextContent("team-x/dspa (not created by this tool)");
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    expect(await within(dialog).findByText("Migrated")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{}]);
    expect(dialog).toHaveTextContent("OK: 4 objects (4330 bytes) copied and checked byte for byte");
    expect(dialog).toHaveTextContent("Endpoint: http://seaweedfs-s3.seaweedfs.svc:8333 instead of http://minio-service.minio.svc:9000.");
    expect(dialog).toHaveTextContent(legacy.namespaceKeptReason!);
    expect(within(dialog).getByRole("button", { name: "Remove old storage" })).toHaveClass("pf-m-danger");
  });

  it("a failed run shows its reason once, in the outcome (follow-up 1)", async () => {
    const reason = "Setting up the new storage did not finish: SeaweedFS deployed but bucket creation failed: connection refused. Run Migrate storage again to retry, or create bucket 'pipelines' from the admin UI. The old storage is unchanged.";
    setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }), {
      "POST /api/resources/minio/migrate/stream": sse(
        { step: "check", status: "success", message: "OK: SeaweedFS in namespace 'minio' is running", elapsedMs: 1 },
        { step: "storage", status: "failed", message: reason, elapsedMs: 2 },
        { step: "operation_complete", status: "failed", message: reason, elapsedMs: 3 },
      ),
    });
    renderAt();
    fireEvent.click(within(await card("Storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    expect(await within(dialog).findByText("Stopped")).toBeInTheDocument();
    expect(within(dialog).getAllByText(reason)).toHaveLength(1);
    expect(dialog).not.toHaveTextContent(/re-run setup/i);
  });

  it("removal: the namespace choice comes before the data-loss alert (follow-up 4) and is sent only when ticked", async () => {
    const removable: LegacyStorageState = { ...legacy, stage: "remove", repoint: [], notMoved: [], usedBy: [], namespaceDeletable: true, namespaceKeptReason: undefined, otherObjects: ["Deployment notes-app"] };
    const ready: ResourceState = { ...readyMinio, migrationPending: true };
    const api = setup(live({ minio: ready, legacyStorage: removable, pipelineServers: [] }), {
      "POST /api/resources/minio/migrate/stream": sse({ step: "operation_complete", status: "success", message: "The old storage in namespace 'minio' was removed.", elapsedMs: 1 }),
    });
    renderAt();
    const storage = await card("Storage");
    expect(storage).toHaveTextContent(/kept until you remove it/);
    fireEvent.click(within(storage).getByRole("button", { name: "Remove old storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: /Remove the old storage\?/ })).toBeInTheDocument();
    expect(dialog).toHaveTextContent("Besides the objects this tool created, it sees there: Deployment notes-app.");
    const box = within(dialog).getByRole("checkbox", { name: "Also delete namespace minio" });
    const danger = within(dialog).getByText("Data is deleted and cannot be recovered");
    expect(box.compareDocumentPosition(danger) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(box).not.toBeChecked();
    fireEvent.click(box);
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove old storage" }));
    expect(await within(dialog).findByText("Old storage removed")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{ removeOldStorage: true, deleteNamespace: true }]);
  });

  it("Tear down covers the earlier storage too, and names its data", async () => {
    setup(live({ minio: { ...notDeployed, teardownBlockedReason: undefined }, legacyStorage: { ...legacy, usedBy: [] }, pipelineServers: [] }));
    renderAt();
    const panel = await details("Storage");
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("The storage an earlier version left in namespace minio is deleted: Deployment seaweedfs, NetworkPolicy seaweedfs-ingress, Route minio-ui, Service minio-service, Secret minio-secret, PersistentVolumeClaim seaweedfs-pvc, PersistentVolumeClaim minio-pvc.");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent(
      "PersistentVolumeClaims minio/seaweedfs-pvc, minio/minio-pvc, with every object stored in them (pipeline artifacts, uploaded files, and the data of earlier versions).");
  });
});

describe("status from the serving configuration (Repair)", () => {
  const base: ResourceState = { deployed: true, namespace: "seaweedfs", managedByTool: true, uiUser: "admin", dataPVCs: ["seaweedfs-data"], ready: false };
  const elsewhere = "Service seaweedfs-s3 selects app=other instead of SeaweedFS (app=seaweedfs), so pipeline servers do not reach SeaweedFS.";
  const scaledDown = "Deployment seaweedfs is scaled to 0 replicas.";

  it.each([
    ["the Service points elsewhere", { ...base, terminalError: true, message: elsewhere, repairNeeded: elsewhere }, "Needs attention", elsewhere],
    ["SeaweedFS is scaled to zero", { ...base, terminalError: true, message: scaledDown, repairNeeded: scaledDown }, "Needs attention", scaledDown],
    ["it is still starting before the Service follows", { ...base, message: "0/1 ready", repairNeeded: elsewhere }, "Setting up", elsewhere],
    // After SEAWEEDFS_IMAGE changed, for example.
    ["it runs another image", { ...base, ready: true, message: "Running", repairNeeded: "SeaweedFS runs mirror.example.com/seaweedfs:4.54, not the image this version deploys (ghcr.io/chrislusf/seaweedfs@sha256:4b42c212). Re-run setup to update it; the data PVC is kept." }, "Needs attention", "SeaweedFS runs mirror.example.com/seaweedfs"],
  ])("when %s, it says so and offers Repair, which re-runs setup", async (_name, minio, label, text) => {
    const api = setup(live({ minio: minio as ResourceState, pipelineServers: [] }), {
      "POST /api/resources/minio/setup": () => jsonResponse({ success: true, message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'.", logs: [] }),
    });
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText(label)).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Set up" })).not.toBeInTheDocument();
    const panel = await details("Storage");
    expect(within(panel).getByText("Needs repair")).toBeInTheDocument();
    expect(panel).toHaveTextContent(text);
    expect(panel).toHaveTextContent(/Repair re-runs setup/);
    fireEvent.click(within(storage).getByRole("button", { name: "Repair" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Repair S3 storage" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/setup"))).toHaveLength(1));
  });

  it("a serving SeaweedFS has no Repair", async () => {
    setup(live({ minio: { ...base, ready: true, message: "Running" }, pipelineServers: [] }));
    renderAt();
    const storage = await card("Storage");
    expect(within(storage).getByText("Ready")).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Repair" })).not.toBeInTheDocument();
    const panel = await details("Storage");
    expect(within(panel).queryByText("Needs repair")).not.toBeInTheDocument();
  });
});
