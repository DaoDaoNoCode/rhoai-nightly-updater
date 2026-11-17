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
  currentImage: "quay.io/opendatahub/mlflow:odh-stable", uiRoute: "https://mlflow.apps.example.com",
  teardownBlockedReason: "This MLflow instance was not created by this tool, so the tool will not delete it.",
};
const legacyDspa: ResourceState = {
  deployed: true, ready: false, namespace: "juntao-test", name: "dspa", managedByTool: true, terminalError: true,
  message: "Could not connect to (minio-service.minio.svc:9000): connection refused", dataPVCs: ["mariadb-dspa"],
};
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com" };

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

function renderAt(path = "/test-resources?tab=pipelines&project=juntao-test", props: Partial<React.ComponentProps<typeof QuickResourceCreator>> = {}) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} {...props} /></MemoryRouter>);
}

/** A part of a feature, by its name ("S3 storage", "Pipeline server", "MLflow server"). */
const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name} \\(`) });

/** Opens a feature's Tear down section; returns the tab panel. */
async function teardownSection(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: "Tear down" }));
  return screen.getByRole("tabpanel");
}

const tooltipOf = async (button: HTMLElement) => {
  fireEvent.mouseEnter(button);
  return (await screen.findByRole("tooltip")).textContent;
};

describe("terminal states (A08-3, A06-5)", () => {
  it("S3 storage in ImagePullBackOff reads Error, offers Repair, says why and what to do; Tear down says why it is off", async () => {
    setup(live());
    renderAt();
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Error")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Repair" })).toBeInTheDocument();
    expect(within(storage).getByText("ImagePullBackOff")).toBeInTheDocument();
    expect(storage).toHaveTextContent(/Repair applies the SeaweedFS Deployment again/);
    const teardown = within(await teardownSection()).getByRole("button", { name: "Tear down the S3 storage" });
    expect(teardown).toHaveAttribute("aria-disabled", "true");
    expect(await tooltipOf(teardown)).toMatch(/1 pipeline server uses this S3 storage/);
  });

  it("a pipeline server that cannot reach the storage says so instead of spinning", async () => {
    setup(live());
    renderAt();
    const ps = await part("Pipeline server");
    expect(within(ps).getByText("Error")).toBeInTheDocument();
    expect(ps).toHaveTextContent(/cannot reach the S3 storage. Fix the storage first/);
    expect(screen.queryByText("Setting up")).not.toBeInTheDocument();
  });

  it("nextStep per reason", () => {
    expect(nextStep("mlflow", { deployed: true, ready: false, waitingReason: "CrashLoopBackOff", terminalError: true, prOverride: true })).toMatch(/PR build keeps crashing. Revert/);
    expect(nextStep("minio", { deployed: true, ready: false, waitingReason: "CreateContainerConfigError", terminalError: true })).toMatch(/Secret or ConfigMap/);
    expect(nextStep("minio", { deployed: true, ready: false, message: "Provisioning" })).toBeNull();
  });
});

describe("health and ownership are separate", () => {
  it("does not call a resource that does not exist 'not managed by this tool'", async () => {
    // The backend reports managedByTool false when nothing is deployed.
    const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
    setup(live({ minio: none, mlflow: none, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Not set up")).toBeInTheDocument();
    expect(screen.queryByText("Not managed by this tool")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up" })).toBeInTheDocument();
  });

  it("someone else's storage namespace: ownership apart, Set up says why", async () => {
    const foreign: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Namespace exists", setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." };
    setup(live({ minio: foreign, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Not set up")).toBeInTheDocument();
    expect(within(storage).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(await tooltipOf(screen.getByRole("button", { name: "Set up" }))).toMatch(/was not created by this tool/);
  });

  const brokenForeignMlflow: ResourceState = { ...browserMlflow, ready: false, terminalError: true, waitingReason: "CrashLoopBackOff", message: "CrashLoopBackOff" };
  const ownMlflow: ResourceState = { ...browserMlflow, managedByTool: true, teardownBlockedReason: undefined };
  it.each([
    ["someone else's, running", browserMlflow, "Ready", true, true],
    ["someone else's, broken", brokenForeignMlflow, "Error", true, false],
    ["this tool's, running", ownMlflow, "Ready", false, true],
  ])("MLflow tab, %s: health, ownership apart, Open when ready, Tear down only for this tool's", async (_name, mlflow, status, foreign, open) => {
    setup(live({ mlflow: mlflow as ResourceState }));
    renderAt("/test-resources?tab=mlflow");
    const server = await part("MLflow server");
    expect(within(server).getByText(status as string)).toBeInTheDocument();
    if (foreign) expect(within(server).getByText("Not managed by this tool")).toBeInTheDocument();
    else expect(within(server).queryByText("Not managed by this tool")).not.toBeInTheDocument();
    if (open) expect(within(server).getByRole("link", { name: /Open MLflow/ })).toHaveAttribute("href", "https://mlflow.apps.example.com");
    else expect(within(server).queryByRole("link", { name: /Open MLflow/ })).not.toBeInTheDocument();
    const panel = await teardownSection();
    if (foreign) {
      expect(within(panel).queryByRole("button", { name: "Tear down MLflow" })).not.toBeInTheDocument();
      expect(panel).toHaveTextContent("was not created by this tool, so it is not torn down here");
    } else {
      expect(within(panel).getByRole("button", { name: "Tear down MLflow" })).toBeInTheDocument();
    }
  });

  it("a project with someone else's pipeline server shows its health and Open, is not set up again, and keeps its storage unclaimed", async () => {
    const foreign: ResourceState = { deployed: true, ready: true, namespace: "team-x", name: "dspa", managedByTool: false, message: "Running", uiRoute: "https://rh-ai.apps.example.com/develop-train/pipelines/definitions/team-x" };
    setup(live({ minio: readyMinio, pipelineServers: [], unmanagedPipelineServers: [foreign] }));
    renderAt("/test-resources?tab=pipelines&project=team-x");
    const ps = await part("Pipeline server");
    expect(within(ps).getByText("Ready")).toBeInTheDocument();
    expect(within(ps).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(ps).toHaveTextContent("was not created by this tool");
    expect(ps).not.toHaveTextContent("S3 storage above");
    expect(within(ps).getByRole("link", { name: /Open pipelines/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    expect(screen.getByText("Ready in team-x · not managed by this tool")).toBeInTheDocument();
    const panel = await teardownSection();
    expect(within(panel).queryByRole("button", { name: "Tear down the pipeline server" })).not.toBeInTheDocument();
    // The project picker says what each project has, and whose it is.
    fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
    expect(await screen.findByRole("option", { name: /team-x/ })).toHaveTextContent("Pipeline server: Ready · not managed by this tool");
  });
});

describe("MLflow PR override (A04-5)", () => {
  it("Revert and Deploy PR stay available when the PR image broke MLflow", async () => {
    setup(live({ mlflow: { deployed: true, ready: false, managedByTool: true, prOverride: true, prNumber: 399, revertImage: "", waitingReason: "CrashLoopBackOff", terminalError: true, message: "CrashLoopBackOff", currentImage: "quay.io/opendatahub/mlflow:odh-pr-399" } }));
    renderAt("/test-resources?tab=mlflow");
    const server = await part("MLflow server");
    expect(server).toHaveTextContent("opendatahub-io/mlflow PR #399");
    expect(within(server).getByRole("button", { name: "Deploy PR" })).toBeInTheDocument();
    fireEvent.click(within(server).getByRole("button", { name: "Revert the PR image" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/so the operator's default \(RHOAI\) image is used again/)).toBeInTheDocument();
  });

  it("deploying a PR on an MLflow the tool did not create warns in the confirmation", async () => {
    setup(live());
    renderAt("/test-resources?tab=mlflow");
    const server = await part("MLflow server");
    fireEvent.change(within(server).getByRole("textbox", { name: /Deploy an opendatahub-io\/mlflow PR/ }), { target: { value: "42" } });
    fireEvent.click(within(server).getByRole("button", { name: "Deploy PR" }));
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
    renderAt();
    fireEvent.click(within(await teardownSection()).getByRole("button", { name: "Tear down the pipeline server" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered")).toBeInTheDocument();
    expect(within(dialog).getByText("mariadb-dspa")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down" }));
    expect(await screen.findByText("Still in progress")).toBeInTheDocument();
    expect(screen.getByText(/This page keeps checking/)).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test" }]);
  });

  it("a conversion-webhook refusal is shown with the server's reason", async () => {
    setup(live({ minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: false, errorCode: "prerequisites", logs: [], message: "Refusing to delete namespace seaweedfs: CRD mcpservers.mcp.x-k8s.io has a conversion webhook whose Service redhat-ods-applications/mcp-lifecycle-operator-webhook-service is missing." }, 422),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click(within(await teardownSection()).getByRole("button", { name: "Tear down the S3 storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText("seaweedfs/seaweedfs-data").length).toBeGreaterThan(0);
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText(/mcpservers.mcp.x-k8s.io has a conversion webhook/)).toBeInTheDocument();
  });
});

describe("S3 storage (names, login, kept namespace)", () => {
  const kept = "S3 storage removed. PVC seaweedfs/seaweedfs-data and all stored objects (pipeline artifacts, models, test files) are deleted with it. Namespace 'seaweedfs' was kept: it may hold objects this tool did not create. Delete it with `oc delete project seaweedfs` once you've checked it's empty.";
  const afterTeardown: ResourceState = { deployed: false, ready: false, namespace: "seaweedfs", managedByTool: true, message: "Namespace exists but the S3 storage is not deployed" };
  const running: ResourceState = { ...readyMinio, dataPVCs: ["seaweedfs-data"] };

  it("a running storage shows its endpoint, bucket and login inline, its warning, and no MinIO-era name", async () => {
    const warning = "SeaweedFS runs mirror.example.com/seaweedfs:4.54, not the image this version deploys. Re-run setup to update it; the data PVC is kept.";
    setup(live({ minio: { ...running, warning }, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Needs attention")).toBeInTheDocument();
    expect(within(storage).getByText(warning)).toBeInTheDocument();
    expect(storage).toHaveTextContent("http://seaweedfs-s3.seaweedfs.svc:8333");
    expect(storage).toHaveTextContent("Admin UI useradmin");
    expect(within(storage).getByLabelText("Password of admin")).toHaveAttribute("type", "password");
    expect(within(storage).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", "https://seaweedfs-admin.apps.example.com");
    expect(document.body).not.toHaveTextContent(/oc extract|minio/i);
  });

  it("the password is fetched only to show or copy it, shown only while toggled, and gone when the tab is left", async () => {
    const writes: string[] = [];
    vi.stubGlobal("ClipboardItem", undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn(async (t: string) => { writes.push(t); }) }, configurable: true });
    const api = setup(live({ minio: running, pipelineServers: [] }), {
      "GET /api/resources/minio/admin-password": { user: "admin", password: "s3cret-value" },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    const field = within(storage).getByLabelText("Password of admin") as HTMLInputElement;
    const fetched = () => api.calls.filter((c) => c.includes("admin-password")).length;
    expect(fetched()).toBe(0);
    expect(field.value).not.toBe("s3cret-value");
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Show password" })); });
    await waitFor(() => expect(field.value).toBe("s3cret-value"));
    expect(field).toHaveAttribute("type", "text");
    expect(fetched()).toBe(1);
    fireEvent.click(within(storage).getByRole("button", { name: "Hide password" }));
    expect(field.value).not.toBe("s3cret-value");
    expect(field).toHaveAttribute("type", "password");
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Copy password" })); });
    expect(await within(storage).findByText("Copied the password of user admin.")).toBeInTheDocument();
    expect(writes).toEqual(["s3cret-value"]);
    expect(fetched()).toBe(2);
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Show password" })); });
    await waitFor(() => expect(document.body.innerHTML).toContain("s3cret-value"));
    fireEvent.click(screen.getByRole("tab", { name: "MLflow" }));
    await waitFor(() => expect(document.body.innerHTML).not.toContain("s3cret-value"));
  });

  it("a reader is told why the password buttons are off, and nothing is read", async () => {
    const api = setup(live({ minio: running, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh", { readBlocker: "Read-only access: changing the cluster through this tool requires the cluster-admin role." });
    const storage = await part("S3 storage");
    const show = within(storage).getByRole("button", { name: "Show password" });
    expect(show).toHaveAttribute("aria-disabled", "true");
    expect(within(storage).getByRole("button", { name: "Copy password" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(show);
    expect(await tooltipOf(show)).toMatch(/cluster-admin/);
    expect(api.calls.some((c) => c.includes("admin-password"))).toBe(false);
  });

  it("after a teardown the kept namespace reads Not set up with Set up, and the result names oc delete project", async () => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: ["fresh"] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: true, message: kept, logs: [] }); },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click(within(await teardownSection()).getByRole("button", { name: "Tear down the S3 storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/Namespace seaweedfs is kept: delete it yourself with oc delete project seaweedfs once empty/);
    expect(dialog).not.toHaveTextContent(/minio/i);
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect((await screen.findByText(/^S3 storage removed/)).closest(".pf-v6-c-alert")).toHaveTextContent(/Namespace seaweedfs was kept: run oc delete project seaweedfs once you have checked it is empty/);
    const storage = await part("S3 storage");
    expect(await within(storage).findByText("Not set up")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up" })).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(1);
  });

  it.each([
    ["partial_failure", "Partly done", "Some S3 storage objects could not be removed: Secret seaweedfs/seaweedfs-s3-credentials: forbidden. Re-run teardown to retry."],
    ["in_progress", "Still in progress", "The S3 storage objects were deleted, but PVC seaweedfs/seaweedfs-data is still terminating after 1m0s; re-run teardown to check again."],
  ])("a %s teardown keeps Tear down available", async (code, title, message) => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: ["fresh"] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: false, errorCode: code, message, logs: [] }, 422); },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const panel = await teardownSection();
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down the S3 storage" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText(title)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Set up" })).toBeInTheDocument());
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down the S3 storage" }));
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

  it("reads Needs attention with Migrate storage, and names the old objects only in its collapsed section", async () => {
    setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    renderAt();
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Needs attention")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Migrate storage" })).toHaveClass("pf-m-primary");
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    const toggle = within(storage).getByRole("button", { name: "Storage of an earlier version" });
    const rest = storage.cloneNode(true) as HTMLElement;
    rest.querySelector(".pf-v6-c-expandable-section")?.remove();
    expect(rest).not.toHaveTextContent(/minio-service|minio-secret|minio-pvc|seaweedfs-pvc|minio-ui/);
    fireEvent.click(toggle);
    const old = storage.querySelector("dl[aria-label=\"Storage from an earlier version\"]");
    expect(old).toHaveTextContent("SeaweedFS in namespace minio, at minio-service.minio.svc:9000");
    expect(old).toHaveTextContent("seaweedfs-pvc (20Gi), minio-pvc (20Gi). minio-pvc holds MinIO's files from before SeaweedFS and cannot be copied.");
  });

  it("a new pipeline server waits until Migrate storage has moved the data, even with the new storage up (follow-up 3)", async () => {
    setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    expect(await part("S3 storage")).toHaveTextContent("Needs attention");
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Migrate storage" })).toBeInTheDocument();
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
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("juntao-test/dspa");
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    expect(await within(dialog).findByText("Migrated")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{}]);
    expect(dialog).toHaveTextContent("Endpoint: http://seaweedfs-s3.seaweedfs.svc:8333 instead of http://minio-service.minio.svc:9000.");
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
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    expect(await within(dialog).findByText("Stopped")).toBeInTheDocument();
    expect(within(dialog).getAllByText(reason)).toHaveLength(1);
  });

  it("removal: the namespace choice comes before the data-loss alert (follow-up 4) and is sent only when ticked", async () => {
    const removable: LegacyStorageState = { ...legacy, stage: "remove", repoint: [], notMoved: [], usedBy: [], namespaceDeletable: true, namespaceKeptReason: undefined, otherObjects: ["Deployment notes-app"] };
    const api = setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: removable, pipelineServers: [] }), {
      "POST /api/resources/minio/migrate/stream": sse({ step: "operation_complete", status: "success", message: "The old storage in namespace 'minio' was removed.", elapsedMs: 1 }),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(storage).toHaveTextContent(/kept until you remove it/);
    fireEvent.click(within(storage).getByRole("button", { name: "Remove old storage" }));
    const dialog = await screen.findByRole("dialog");
    const box = within(dialog).getByRole("checkbox", { name: "Also delete namespace minio" });
    const danger = within(dialog).getByText("Data is deleted and cannot be recovered");
    expect(box.compareDocumentPosition(danger) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.click(box);
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove old storage" }));
    expect(await within(dialog).findByText("Old storage removed")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{ removeOldStorage: true, deleteNamespace: true }]);
  });

  it("Tear down covers the earlier storage too, and names its data", async () => {
    setup(live({ minio: { ...notDeployed, teardownBlockedReason: undefined }, legacyStorage: { ...legacy, usedBy: [] }, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click(within(await teardownSection()).getByRole("button", { name: "Tear down the S3 storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("The storage an earlier version left in namespace minio is deleted");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent(
      "PersistentVolumeClaims minio/seaweedfs-pvc, minio/minio-pvc, with every object stored in them (pipeline artifacts, uploaded files, and the data of earlier versions).");
  });
});

// Every setup the page offers has its removal on the same tab: Tear down
// (confirmed with what is deleted and what is kept), Revert for a PR image,
// Remove old storage after a migration; for what the tool did not create,
// Tear down says it is not touched.
describe("every setup has its removal on the same tab", () => {
  const ownMlflow: ResourceState = { ...browserMlflow, managedByTool: true, teardownBlockedReason: undefined };
  const runningDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "nightly-dspa", managedByTool: true, message: "Running", dataPVCs: ["mariadb-nightly-dspa"] };

  it.each([
    ["the storage and the pipeline server", "/test-resources?tab=pipelines&project=juntao-test", { minio: readyMinio, pipelineServers: [runningDspa] }, ["Tear down the pipeline server", "Tear down the S3 storage"], []],
    ["the storage alone", "/test-resources?tab=pipelines&project=fresh", { minio: readyMinio, pipelineServers: [] }, ["Tear down the S3 storage"], []],
    ["a broken storage", "/test-resources?tab=pipelines&project=fresh", { minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }, ["Tear down the S3 storage"], []],
    ["MLflow", "/test-resources?tab=mlflow", { mlflow: ownMlflow }, ["Tear down MLflow"], []],
    ["someone else's MLflow", "/test-resources?tab=mlflow", { mlflow: browserMlflow }, [], ["This MLflow server was not created by this tool, so it is not torn down here."]],
    ["someone else's pipeline server and storage namespace", "/test-resources?tab=pipelines&project=team-x",
      { minio: { deployed: false, ready: false, managedByTool: false, setupBlockedReason: "x" }, pipelineServers: [], unmanagedPipelineServers: [{ ...runningDspa, namespace: "team-x", name: "dspa", managedByTool: false }] }, [],
      ["The pipeline server in team-x was not created by this tool, so it is not torn down here.", "Namespace seaweedfs was not created by this tool, so it is not torn down here."]],
  ])("%s: Tear down offers the removal, or says what is not touched", async (_name, path, over, buttons, texts) => {
    setup(live(over as Partial<ResourcesStatus>));
    renderAt(path);
    const panel = await teardownSection();
    for (const b of buttons as string[]) expect(within(panel).getByRole("button", { name: b })).toBeInTheDocument();
    for (const t of texts as string[]) expect(panel).toHaveTextContent(t);
    if ((buttons as string[]).length === 0) expect(within(panel).queryByRole("button", { name: /^Tear down / })).not.toBeInTheDocument();
  });

  it("each Tear down confirms what is deleted and what is kept", async () => {
    setup(live({ minio: readyMinio, pipelineServers: [runningDspa], mlflow: ownMlflow }));
    renderAt();
    const panel = await teardownSection();
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down the pipeline server" }));
    let dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("DataSciencePipelinesApplication nightly-dspa and its S3 credentials Secret are deleted. The project stays.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Tear down the S3 storage" }));
    dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Deployment seaweedfs, NetworkPolicy seaweedfs-ingress, Route seaweedfs-admin, Service seaweedfs-s3, Secret seaweedfs-s3-credentials and PersistentVolumeClaim seaweedfs-data");
    expect(dialog).toHaveTextContent("Namespace seaweedfs is kept");
  });

  it("a deployed PR image is removed with Revert, and storage of an earlier version with Remove old storage", async () => {
    const legacy: LegacyStorageState = { namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"], stage: "remove", namespaceDeletable: false, usedBy: [] };
    setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: legacy, pipelineServers: [], mlflow: { ...ownMlflow, prOverride: true, prNumber: 7 } }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    expect(within(await part("S3 storage")).getByRole("button", { name: "Remove old storage" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "MLflow" }));
    expect(within(await part("MLflow server")).getByRole("button", { name: "Revert the PR image" })).toBeInTheDocument();
  });
});

describe("status from the serving configuration (Repair)", () => {
  const base: ResourceState = { deployed: true, namespace: "seaweedfs", managedByTool: true, uiUser: "admin", dataPVCs: ["seaweedfs-data"], ready: false };
  const elsewhere = "Service seaweedfs-s3 selects app=other instead of SeaweedFS (app=seaweedfs), so pipeline servers do not reach SeaweedFS.";
  const scaledDown = "Deployment seaweedfs is scaled to 0 replicas.";

  it.each([
    ["the Service points elsewhere", { ...base, terminalError: true, message: elsewhere, repairNeeded: elsewhere }, "Needs attention", elsewhere],
    ["SeaweedFS is scaled to zero", { ...base, terminalError: true, message: scaledDown, repairNeeded: scaledDown }, "Needs attention", scaledDown],
    ["it runs another image", { ...base, ready: true, message: "Running", repairNeeded: "SeaweedFS runs mirror.example.com/seaweedfs:4.54, not the image this version deploys (ghcr.io/chrislusf/seaweedfs@sha256:4b42c212). Re-run setup to update it; the data PVC is kept." }, "Needs attention", "SeaweedFS runs mirror.example.com/seaweedfs"],
  ])("when %s, it says so and offers Repair, which re-runs setup", async (_name, minio, label, text) => {
    const api = setup(live({ minio: minio as ResourceState, pipelineServers: [] }), {
      "POST /api/resources/minio/setup": () => jsonResponse({ success: true, message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).getByText(label)).toBeInTheDocument();
    expect(within(storage).getByText("Needs repair")).toBeInTheDocument();
    expect(storage).toHaveTextContent(text);
    expect(storage).toHaveTextContent(/Repair re-runs setup/);
    fireEvent.click(within(storage).getByRole("button", { name: "Repair" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Repair S3 storage" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/setup"))).toHaveLength(1));
  });

  it("someone else's storage gets no Repair", async () => {
    setup(live({ minio: { ...base, managedByTool: false, terminalError: true, message: scaledDown, repairNeeded: scaledDown }, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).queryByRole("button", { name: "Repair" })).not.toBeInTheDocument();
    expect(within(storage).getByText("Not managed by this tool")).toBeInTheDocument();
  });
});
