import { afterEach, describe, expect, it, vi } from "vitest";
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
const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name}$`) });

/** A part's menu item, or null when the part has no menu or no such item. */
async function menuItem(name: string, label: string | RegExp): Promise<HTMLElement | null> {
  const toggle = within(await part(name)).queryByRole("button", { name: `${name} actions` });
  if (!toggle) return null;
  fireEvent.click(toggle);
  return within(await screen.findByRole("menu")).queryByRole("menuitem", { name: label });
}

// Tests that stub the clipboard put it back, so no test sees another's stub.
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
afterEach(() => {
  vi.unstubAllGlobals();
  if (originalClipboard) Object.defineProperty(navigator, "clipboard", originalClipboard);
  else delete (navigator as { clipboard?: unknown }).clipboard;
});

/** The dialog footer's Close button (the header's X is also named Close). */
const footerClose = (dialog: HTMLElement) => within(dialog).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!;

describe("terminal states (A08-3, A06-5)", () => {
  it("S3 storage in ImagePullBackOff reads Error, says it in words with Repair in the alert, raw reasons in Technical details; Delete says why it is off", async () => {
    setup(live());
    renderAt();
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Error")).toBeInTheDocument();
    // Item 6: the title in words, the fix as the alert's own action, the pod reason only in the details.
    expect(within(storage).getByText("The S3 storage's image can't be downloaded")).toBeInTheDocument();
    expect(storage).toHaveTextContent(/Repair applies it again with the image this updater uses/);
    expect(within(storage).getByRole("button", { name: "Repair" })).toBeInTheDocument();
    expect(within(storage).queryByText("ImagePullBackOff")).not.toBeVisible();
    fireEvent.click(within(storage).getByRole("button", { name: "Technical details" }));
    expect(within(storage).getByText("ImagePullBackOff")).toBeVisible();
    // Item 4: Delete is in the part's menu, with why it is off.
    const del = await menuItem("S3 storage", /Delete S3 storage/);
    expect(del).toHaveAttribute("aria-disabled", "true");
    expect(del).toHaveTextContent(/1 pipeline server uses this S3 storage/);
  });

  it("with the console's address, a crash offers its pod logs", async () => {
    const crashing: ResourceState = { ...browserMlflow, ready: false, terminalError: true, waitingReason: "CrashLoopBackOff", message: "CrashLoopBackOff" };
    setup(live({ mlflow: crashing }));
    renderAt("/test-resources?tab=mlflow", { consoleURL: "https://console.apps.example.com" });
    const server = await part("MLflow server");
    expect(within(server).getByText("MLflow keeps crashing")).toBeInTheDocument();
    expect(server).toHaveTextContent(/created outside this tool .* ask its owner/);
    expect(within(server).getByRole("link", { name: /View logs/ })).toHaveAttribute("href", "https://console.apps.example.com/k8s/ns/redhat-ods-applications/deployments/mlflow/pods");
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
    expect(screen.getByRole("button", { name: "Set up Pipelines in fresh" })).toBeInTheDocument();
  });

  it("someone else's storage namespace: ownership apart; the block shows before Set up, inline", async () => {
    const foreign: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Namespace exists", setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." };
    setup(live({ minio: foreign, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Not set up")).toBeInTheDocument();
    expect(within(storage).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(within(storage).getByText("The S3 storage can't be set up: namespace seaweedfs belongs to someone else")).toBeInTheDocument();
    expect(storage).toHaveTextContent("oc delete project seaweedfs");
    const setUp = screen.getByRole("button", { name: "Set up Pipelines in fresh" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    // Item 8: the reason is under the primary action, not only in a tooltip.
    expect(setUp).toHaveAccessibleDescription(/was not created by this tool/);
  });

  it("a component that is off shows before Set up, with the way to the fix, and blocks Set up", async () => {
    const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
    setup(live({ minio: none, pipelineServers: [], pipelinesBlocked: { component: "aipipelines", dsc: "default-dsc", state: "Removed", reason: "Data Science Pipelines is turned off on this cluster: component aipipelines is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Managed." } }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const alert = (await screen.findByText("Data Science Pipelines is turned off on this cluster")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(alert).getByRole("button", { name: "Open Components" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up Pipelines in fresh" })).toHaveAccessibleDescription(/component aipipelines is Removed/);
  });

  it("no data science project: says where to make one", async () => {
    setup(live({ minio: readyMinio, pipelineServers: [], unmanagedPipelineProjects: [], dashboardUrl: "https://rh-ai.apps.example.com" }), { "/api/resources/projects": { projects: [] } });
    renderAt("/test-resources?tab=pipelines");
    expect(await screen.findByText("There are no data science projects yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Create a project in the dashboard/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/projects");
    expect(await part("Pipeline server")).toHaveTextContent("Choose a project above");
  });

  const brokenForeignMlflow: ResourceState = { ...browserMlflow, ready: false, terminalError: true, waitingReason: "CrashLoopBackOff", message: "CrashLoopBackOff" };
  const ownMlflow: ResourceState = { ...browserMlflow, managedByTool: true, teardownBlockedReason: undefined };
  it.each([
    ["someone else's, running", browserMlflow, "Ready", true, true],
    ["someone else's, broken", brokenForeignMlflow, "Error", true, false],
    ["this tool's, running", ownMlflow, "Ready", false, true],
  ])("MLflow tab, %s: health, ownership apart, Open its workspaces when ready, Delete only for this tool's", async (_name, mlflow, status, foreign, open) => {
    setup(live({ mlflow: mlflow as ResourceState }));
    renderAt("/test-resources?tab=mlflow&project=juntao-test");
    const server = await part("MLflow server");
    expect(within(server).getByText(status as string)).toBeInTheDocument();
    if (foreign) expect(within(server).getByText("Not managed by this tool")).toBeInTheDocument();
    else expect(within(server).queryByText("Not managed by this tool")).not.toBeInTheDocument();
    if (open) expect(within(server).getByRole("link", { name: /Open MLflow experiments/ })).toHaveAttribute("href", "https://mlflow.apps.example.com");
    else expect(within(server).queryByRole("link", { name: /Open MLflow/ })).not.toBeInTheDocument();
    if (foreign) {
      expect(await menuItem("MLflow server", /Delete MLflow/)).toBeNull();
      expect(server).toHaveTextContent("created outside this tool");
    } else {
      expect(await menuItem("MLflow server", /Delete MLflow/)).not.toHaveAttribute("aria-disabled", "true");
    }
  });

  it("MLflow ready: workspaces explained, the project's link and the client settings", async () => {
    setup(live());
    renderAt("/test-resources?tab=mlflow&project=juntao-test");
    const use = await screen.findByRole("region", { name: "Use it" });
    expect(use).toHaveTextContent("a project is an MLflow workspace");
    expect(within(use).getByRole("link", { name: /Open juntao-test in MLflow/ })).toHaveAttribute("href", "https://mlflow.apps.example.com/#/experiments?workspace=juntao-test");
    // The address comes from the cluster: single-quoted for the shell.
    expect(use).toHaveTextContent("export MLFLOW_TRACKING_URI='https://mlflow.apps.example.com'");
    expect(use).toHaveTextContent("export MLFLOW_TRACKING_TOKEN=$(oc whoami -t)");
    expect(use).toHaveTextContent("export MLFLOW_WORKSPACE=juntao-test");
    // The PR deploy is a developer's tool: in its own collapsed section.
    expect(screen.getByRole("button", { name: "Test an MLflow PR build" })).toHaveAttribute("aria-expanded", "false");
  });

  it("a project with someone else's pipeline server shows its health and Open, is not set up again, and keeps its storage unclaimed", async () => {
    const foreign: ResourceState = { deployed: true, ready: true, namespace: "team-x", name: "dspa", managedByTool: false, message: "Running", uiRoute: "https://rh-ai.apps.example.com/develop-train/pipelines/definitions/team-x" };
    setup(live({ minio: readyMinio, pipelineServers: [], unmanagedPipelineServers: [foreign] }));
    renderAt("/test-resources?tab=pipelines&project=team-x");
    const ps = await part("Pipeline server");
    expect(within(ps).getByText("Ready")).toBeInTheDocument();
    expect(within(ps).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(ps).toHaveTextContent("Created outside this tool");
    expect(ps).not.toHaveTextContent("keeps their artifacts in the S3 storage");
    expect(within(ps).getByRole("link", { name: /Open pipelines/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    expect(screen.getByText("Ready in team-x")).toBeInTheDocument();
    // Ownership beside the state, not in its words.
    expect(screen.getByText("Ready in team-x").closest(".pf-v6-l-flex")).toHaveTextContent("Not managed by this tool");
    expect(within(ps).queryByRole("button", { name: "Pipeline server actions" })).not.toBeInTheDocument();
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
    expect(within(server).getByText("MLflow keeps crashing")).toBeInTheDocument();
    // The fix is in the alert, and on the part.
    expect(within(server).getAllByRole("button", { name: "Revert the PR image" })).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Test an MLflow PR build" }));
    expect(screen.getByRole("button", { name: "Deploy PR" })).toBeInTheDocument();
    fireEvent.click(within(server).getAllByRole("button", { name: "Revert the PR image" })[0]);
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Details" }));
    expect(within(dialog).getByText(/so the operator's default \(RHOAI\) image is used again/)).toBeInTheDocument();
  });

  it("deploying a PR checks the number on submit, and warns in the confirmation on an MLflow the tool did not create", async () => {
    setup(live());
    renderAt("/test-resources?tab=mlflow");
    fireEvent.click(await screen.findByRole("button", { name: "Test an MLflow PR build" }));
    const field = screen.getByRole("textbox", { name: "Pull request number" });
    fireEvent.click(screen.getByRole("button", { name: "Deploy PR" }));
    expect(field).toHaveAccessibleDescription(/Enter the number of an opendatahub-io\/mlflow pull request/);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.change(field, { target: { value: "42" } });
    fireEvent.click(screen.getByRole("button", { name: "Deploy PR" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("This MLflow server was not created by this tool")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Details" }));
    expect(within(dialog).getByText("quay.io/opendatahub/mlflow:odh-pr-42")).toBeInTheDocument();
  });
});

describe("teardown confirmations state the data loss", () => {
  it("pipeline server Delete: the outcome first, the database volume in the data loss; in_progress ends as still deleting, in the run dialog", async () => {
    const api = setup(live({ minio: readyMinio }), {
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: false, message: "The DSPA is still being deleted by the pipelines operator.", logs: [], errorCode: "in_progress" }, 422),
    });
    renderAt();
    fireEvent.click((await menuItem("Pipeline server", /Delete pipeline server/))!);
    const dialog = await screen.findByRole("dialog", { name: /Delete the pipeline server in juntao-test\?/ });
    expect(dialog).toHaveTextContent("Deletes the pipeline server in juntao-test, with its runs and experiments.");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered")).toBeInTheDocument();
    expect(within(dialog).getByText("mariadb-dspa")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete the pipeline server in juntao-test" });
    expect(await within(run).findByText("Still deleting")).toBeInTheDocument();
    expect(run).toHaveTextContent("The DSPA is still being deleted by the pipelines operator.");
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test" }]);
  });

  it("a conversion-webhook refusal is shown with the server's reason", async () => {
    setup(live({ minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: false, errorCode: "prerequisites", logs: [], message: "Refusing to delete namespace seaweedfs: CRD mcpservers.mcp.x-k8s.io has a conversion webhook whose Service redhat-ods-applications/mcp-lifecycle-operator-webhook-service is missing." }, 422),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText("seaweedfs/seaweedfs-data").length).toBeGreaterThan(0);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete the S3 storage" });
    expect(await within(run).findByText("Delete the S3 storage: stopped")).toBeInTheDocument();
    expect(within(run).getByText(/mcpservers.mcp.x-k8s.io has a conversion webhook/)).toBeInTheDocument();
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
    expect(storage).toHaveTextContent("SeaweedFS runs mirror.example.com/seaweedfs:4.54, not the image this version deploys.");
    expect(storage).toHaveTextContent("Re-run setup to update it; the data PVC is kept.");
    expect(storage).toHaveTextContent("http://seaweedfs-s3.seaweedfs.svc:8333");
    expect(storage).toHaveTextContent("Admin UI loginUser admin, with a password of its own:");
    expect(within(storage).getByLabelText("admin UI password")).toHaveAttribute("type", "password");
    expect(within(storage).getByLabelText("S3 secret key")).toHaveAttribute("type", "password");
    expect(within(storage).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", "https://seaweedfs-admin.apps.example.com");
    expect(document.body).not.toHaveTextContent(/oc extract|minio/i);
  });

  it("the password is fetched only to show or copy it, shown only while toggled, and gone when the tab is left", async () => {
    const writes: string[] = [];
    vi.stubGlobal("ClipboardItem", undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn(async (t: string) => { writes.push(t); }) }, configurable: true });
    const api = setup(live({ minio: running, pipelineServers: [] }), {
      "GET /api/resources/minio/admin-password": { user: "admin", password: "admin-value", secretKey: "s3cret-value" },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    const field = within(storage).getByLabelText("S3 secret key") as HTMLInputElement;
    const fetched = () => api.calls.filter((c) => c.includes("admin-password")).length;
    expect(fetched()).toBe(0);
    expect(field.value).not.toBe("s3cret-value");
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Show the S3 secret key" })); });
    await waitFor(() => expect(field.value).toBe("s3cret-value"));
    expect(field).toHaveAttribute("type", "text");
    expect(fetched()).toBe(1);
    fireEvent.click(within(storage).getByRole("button", { name: "Hide the S3 secret key" }));
    expect(field.value).not.toBe("s3cret-value");
    expect(field).toHaveAttribute("type", "password");
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Copy the S3 secret key" })); });
    expect(await within(storage).findByText("Copied the S3 secret key.")).toBeInTheDocument();
    expect(writes).toEqual(["s3cret-value"]);
    expect(fetched()).toBe(2);
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Show the S3 secret key" })); });
    await waitFor(() => expect(document.body.innerHTML).toContain("s3cret-value"));
    fireEvent.click(screen.getByRole("tab", { name: "MLflow" }));
    await waitFor(() => expect(document.body.innerHTML).not.toContain("s3cret-value"));
  });

  it("item 5: the access key shows on demand, and the connection copies as environment variables", async () => {
    const writes: string[] = [];
    vi.stubGlobal("ClipboardItem", undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn(async (t: string) => { writes.push(t); }) }, configurable: true });
    const api = setup(live({ minio: running, pipelineServers: [] }), {
      "GET /api/resources/minio/admin-password": { user: "admin", accessKey: "AKEY", password: "admin-value", secretKey: "s3cret-value" },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    const storage = await part("S3 storage");
    expect(storage).toHaveTextContent("us-east-1, path-style addressing");
    expect(storage).toHaveTextContent("with a password of its own");
    const key = within(storage).getByLabelText("S3 access key") as HTMLInputElement;
    expect(key.value).not.toBe("AKEY");
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Show the S3 access key" })); });
    await waitFor(() => expect(key.value).toBe("AKEY"));
    await act(async () => { fireEvent.click(within(storage).getByRole("button", { name: "Copy as environment variables" })); });
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toContain("AWS_ACCESS_KEY_ID=AKEY\nAWS_SECRET_ACCESS_KEY=s3cret-value\n");
    expect(document.body).not.toHaveTextContent("s3cret-value");
    expect(api.calls.filter((c) => c.includes("admin-password"))).toHaveLength(2);
  });

  it("a reader sees who can help instead of keys and changes, and nothing is read", async () => {
    const api = setup(live({ minio: running, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh", { readBlocker: "Read-only access: changing the cluster through this tool requires the cluster-admin role." });
    const storage = await part("S3 storage");
    expect(within(storage).queryByRole("button", { name: "Show the S3 secret key" })).not.toBeInTheDocument();
    expect(storage).toHaveTextContent("Only cluster-admins can read the S3 keys here. Ask one for them.");
    // Item 8: no menu of changes; Set up says who can help, inline.
    expect(within(storage).queryByRole("button", { name: "S3 storage actions" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Clean up" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up the pipeline server in fresh" })).toHaveAccessibleDescription(/Ask one to set it up for you/);
    expect(api.calls.some((c) => c.includes("admin-password"))).toBe(false);
  });

  it("after a deletion the kept namespace reads Not set up with Set up, and the run's outcome names oc delete project", async () => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: ["fresh"] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: true, message: kept, logs: [] }); },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/Deletes the S3 storage with every file in it. Namespace seaweedfs is kept./);
    expect(dialog).not.toHaveTextContent(/minio/i);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete the S3 storage" });
    // The outcome is what the request said, specific, with the kept namespace's command.
    await waitFor(() => expect(run.querySelector(".pf-v6-c-alert__title")).toHaveTextContent(/S3 storage removed\. .*oc delete project seaweedfs/));
    fireEvent.click(footerClose(run));
    const storage = await part("S3 storage");
    expect(await within(storage).findByText("Not set up")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up Pipelines in fresh" })).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(1);
  });

  it.each([
    ["partial_failure", "Delete the S3 storage: stopped", "Some S3 storage objects could not be removed: Secret seaweedfs/seaweedfs-s3-credentials: forbidden. Re-run teardown to retry."],
    ["in_progress", "Still deleting", "The S3 storage objects were deleted, but PVC seaweedfs/seaweedfs-data is still terminating after 1m0s; re-run teardown to check again."],
  ])("a %s deletion keeps Delete available", async (code, title, message) => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: ["fresh"] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: false, errorCode: code, message, logs: [] }, 422); },
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete the S3 storage" });
    expect(await within(run).findByText(title)).toBeInTheDocument();
    fireEvent.click(footerClose(run));
    await waitFor(() => expect(screen.getByRole("button", { name: "Set up Pipelines in fresh" })).toBeInTheDocument());
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
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

  it("a namespace minio this tool did not make is said plainly, offers no migration, and does not block setup", async () => {
    setup(live({ minio: notDeployed, legacyStorage: null, foreignLegacyNamespace: true, pipelineServers: [] }));
    renderAt();
    const storage = await part("S3 storage");
    expect(within(storage).getByText("Namespace minio is not this tool's")).toBeInTheDocument();
    expect(within(storage).getByText(/not used as storage from an earlier version/)).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Migrate storage" })).not.toBeInTheDocument();
  });

  it("Migrate storage gives the new storage new keys, not the old ones", async () => {
    setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    renderAt();
    const storage = await part("S3 storage");
    expect(within(storage).getByText(/Migrate storage creates it, with new keys/)).toBeInTheDocument();
  });

  it("the Migrate storage and Delete old storage dialogs say the keys are new and where to find them", async () => {
    setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }));
    const { unmount } = renderAt();
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const migrate = await screen.findByRole("dialog", { name: "Migrate storage" });
    expect(migrate).toHaveTextContent("Sets up the storage in seaweedfs with new keys: a new access key and secret key. The old keys do not work on it.");
    expect(migrate).toHaveTextContent("to the new storage and its keys");
    expect(migrate).toHaveTextContent("Anything else that uses the old keys, such as your own S3 clients, needs the new ones: the S3 storage on the Pipelines tab shows them after the migration.");
    expect(migrate).not.toHaveTextContent(/same (access )?key|keys are the same/);
    unmount();

    const removable: LegacyStorageState = { ...legacy, stage: "remove", repoint: [], notMoved: [], usedBy: [], namespaceDeletable: true, namespaceKeptReason: undefined };
    setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: removable, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete old storage/))!);
    const remove = await screen.findByRole("dialog", { name: /Delete the old storage\?/ });
    expect(remove).toHaveTextContent("Access key and secret key: the new ones, shown under S3 storage on the Pipelines tab. The old keys do not work on the new storage.");
    expect(remove).not.toHaveTextContent(/same (access )?key|keys are the same/);
  });

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
    // Item 7: why, at the top of the tab, with the action.
    const why = screen.getByText("Your pipeline data is in storage from an earlier version").closest(".pf-v6-c-alert") as HTMLElement;
    expect(why).toHaveTextContent("Your pipeline servers keep working on the old storage meanwhile.");
    expect(within(why).getByRole("button", { name: "Migrate storage" })).toBeInTheDocument();
  });

  it("Migrate storage shows the plan, runs as a stream, and ends with its outcome: no removal in the same dialog", async () => {
    const api = setup(live({ minio: notDeployed, legacyStorage: legacy, pipelineServers: [runningDspa] }), {
      "POST /api/resources/minio/migrate/stream": sse(
        { step: "check", status: "success", message: "OK: SeaweedFS in namespace 'minio' is running", elapsedMs: 1 },
        { step: "storage", status: "success", message: "OK: The new storage is ready", elapsedMs: 2 },
        { step: "copy", status: "success", message: "OK: 4 objects (4330 bytes) copied and checked byte for byte", elapsedMs: 3 },
        { step: "repoint", status: "success", message: "OK: 1 pipeline server moved", elapsedMs: 4 },
        { step: "operation_complete", status: "success", message: "Migrated 4 objects (4330 bytes) to namespace 'seaweedfs'. The old storage in namespace 'minio' is kept: delete it later from the S3 storage's menu, once nothing else uses it.", elapsedMs: 5 },
      ),
    });
    renderAt();
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("juntao-test/dspa");
    // The data that cannot move is a warning, not a remark.
    expect(within(dialog).getByText("Data from before SeaweedFS can't be moved")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    expect(await within(dialog).findByText("The data is in the new storage")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{}]);
    // Item 7: success ends there; the removal is a later, separate decision.
    expect(screen.getByRole("dialog", { name: "Migrate storage" })).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: /Delete old storage|Remove old storage/ })).not.toBeInTheDocument();
    // The backend's message says once that the old storage is kept, and where to delete it.
    const outcome = within(dialog).getByText("The data is in the new storage").closest(".pf-v6-c-alert") as HTMLElement;
    expect(outcome).toHaveTextContent("The old storage in namespace 'minio' is kept: delete it later from the S3 storage's menu, once nothing else uses it.");
    expect(outcome.textContent!.match(/is kept/g)).toHaveLength(1);
    expect(footerClose(dialog)).toHaveClass("pf-m-primary");
  });

  it("an unrecorded pipeline server moves only when its project is ticked; unticked by default", async () => {
    const run = sse({ step: "operation_complete", status: "success", message: "Migrated.", elapsedMs: 1 });
    const unrecorded: LegacyStorageState = { ...legacy, repoint: ["evil/dspa", "juntao-test/dspa"], unrecorded: ["evil/dspa"] };
    const api = setup(live({ minio: notDeployed, legacyStorage: unrecorded, pipelineServers: [runningDspa] }), { "POST /api/resources/minio/migrate/stream": run });
    renderAt();
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Confirm the pipeline servers this tool set up")).toBeInTheDocument();
    const box = within(dialog).getByRole("checkbox", { name: /Move evil\/dspa: this tool set it up/ });
    expect(box).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    await within(dialog).findByText("The data is in the new storage");
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{}]);
  });

  it("a ticked project is sent as confirmed", async () => {
    const run = sse({ step: "operation_complete", status: "success", message: "Migrated.", elapsedMs: 1 });
    const unrecorded: LegacyStorageState = { ...legacy, repoint: ["evil/dspa", "juntao-test/dspa"], unrecorded: ["evil/dspa"] };
    const api = setup(live({ minio: notDeployed, legacyStorage: unrecorded, pipelineServers: [runningDspa] }), { "POST /api/resources/minio/migrate/stream": run });
    renderAt();
    fireEvent.click(within(await part("S3 storage")).getByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /Move evil\/dspa: this tool set it up/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    await within(dialog).findByText("The data is in the new storage");
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{ confirmProjects: ["evil"] }]);
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
    expect(await screen.findByText(/The old storage can be deleted/)).toBeInTheDocument();
    fireEvent.click((await menuItem("S3 storage", /Delete old storage/))!);
    const dialog = await screen.findByRole("dialog", { name: /Delete the old storage\?/ });
    const box = within(dialog).getByRole("checkbox", { name: "Also delete namespace minio" });
    const danger = within(dialog).getByText("Data is deleted and cannot be recovered");
    expect(box.compareDocumentPosition(danger) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.click(box);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete old storage" }));
    expect(await within(dialog).findByText("The old storage was deleted")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/minio/migrate/stream"]).toEqual([{ removeOldStorage: true, deleteNamespace: true }]);
  });

  it("Delete covers the earlier storage too, and names its data", async () => {
    setup(live({ minio: { ...notDeployed, teardownBlockedReason: undefined }, legacyStorage: { ...legacy, usedBy: [] }, pipelineServers: [] }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Details" }));
    expect(dialog).toHaveTextContent("The storage an earlier version left in namespace minio is deleted");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent(
      "Every object stored in it: pipeline artifacts and uploaded files, and the data of earlier versions (volumes minio/seaweedfs-pvc, minio/minio-pvc).");
  });
});

// Every setup the page offers has its removal on the same tab: Delete in
// the part's menu (confirmed with what is deleted and what is kept), Revert
// for a PR image, Delete old storage after a migration; what the tool did
// not create has no menu and says so.
describe("every setup has its removal on the same tab", () => {
  const ownMlflow: ResourceState = { ...browserMlflow, managedByTool: true, teardownBlockedReason: undefined };
  const runningDspa: ResourceState = { deployed: true, ready: true, namespace: "juntao-test", name: "nightly-dspa", managedByTool: true, message: "Running", dataPVCs: ["mariadb-nightly-dspa"] };

  it.each([
    ["the storage and the pipeline server", "/test-resources?tab=pipelines&project=juntao-test", { minio: readyMinio, pipelineServers: [runningDspa] }, [["Pipeline server", /Delete pipeline server/], ["S3 storage", /Delete S3 storage/]], []],
    ["the storage alone", "/test-resources?tab=pipelines&project=fresh", { minio: readyMinio, pipelineServers: [] }, [["S3 storage", /Delete S3 storage/]], []],
    ["a broken storage", "/test-resources?tab=pipelines&project=fresh", { minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }, [["S3 storage", /Delete S3 storage/]], []],
    ["MLflow", "/test-resources?tab=mlflow", { mlflow: ownMlflow }, [["MLflow server", /Delete MLflow/]], []],
    ["someone else's MLflow", "/test-resources?tab=mlflow", { mlflow: browserMlflow }, [], [["MLflow server", "created outside this tool"]]],
    ["someone else's pipeline server and storage namespace", "/test-resources?tab=pipelines&project=team-x",
      { minio: { deployed: false, ready: false, managedByTool: false, setupBlockedReason: "x" }, pipelineServers: [], unmanagedPipelineServers: [{ ...runningDspa, namespace: "team-x", name: "dspa", managedByTool: false }] }, [],
      [["Pipeline server", "Created outside this tool"], ["S3 storage", "belongs to someone else"]]],
  ])("%s: the part's menu offers Delete, or the part says it is not this tool's", async (_name, path, over, items, texts) => {
    setup(live(over as Partial<ResourcesStatus>));
    renderAt(path);
    for (const [name, label] of items as [string, RegExp][]) {
      expect(await menuItem(name, label)).not.toBeNull();
      fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    }
    for (const [name, text] of texts as [string, string][]) {
      const p = await part(name);
      expect(p).toHaveTextContent(text);
      expect(within(p).queryByRole("button", { name: `${name} actions` })).not.toBeInTheDocument();
    }
  });

  it("each Delete confirms what is deleted and what is kept, with the objects under Details", async () => {
    setup(live({ minio: readyMinio, pipelineServers: [runningDspa], mlflow: ownMlflow }));
    renderAt();
    fireEvent.click((await menuItem("Pipeline server", /Delete pipeline server/))!);
    let dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("The project and the S3 storage stay.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Details" }));
    expect(dialog).toHaveTextContent("DataSciencePipelinesApplication nightly-dspa and its S3 credentials Secret are deleted, with the dashboard S3 Connection rhoai-nightly-updater-s3 this tool created (workbenches or model deployments that use it lose it). The project stays.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Namespace seaweedfs is kept");
    fireEvent.click(within(dialog).getByRole("button", { name: "Details" }));
    expect(dialog).toHaveTextContent("Deployment seaweedfs, NetworkPolicy seaweedfs-ingress, Route seaweedfs-admin, Service seaweedfs-s3, Secret seaweedfs-s3-credentials and PersistentVolumeClaim seaweedfs-data");
  });

  // Follow-up 8: the storage's namespace, which this tool created, can go
  // with it, as the user's informed choice.
  it("storage Delete offers namespace seaweedfs, unticked, above the data loss, and sends it only when ticked", async () => {
    const own: ResourceState = { ...readyMinio, dataPVCs: ["seaweedfs-data"], namespaceDeletable: true, otherObjects: ["Deployment notes-app"] };
    const api = setup(live({ minio: own, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: true, message: "S3 storage removed. Namespace 'seaweedfs' is being deleted, with everything else in it.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    const dialog = await screen.findByRole("dialog");
    const box = within(dialog).getByRole("checkbox", { name: "Also delete namespace seaweedfs (created by this tool)" });
    expect(box).not.toBeChecked();
    expect(dialog).toHaveTextContent("Deletes namespace seaweedfs and everything in it, including anything this tool did not create.");
    expect(dialog).toHaveTextContent("this tool sees there: Deployment notes-app.");
    expect(dialog).toHaveTextContent("Namespace seaweedfs is kept");
    const danger = within(dialog).getByText("Data is deleted and cannot be recovered");
    expect(box.compareDocumentPosition(danger) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.click(box);
    expect(await within(dialog).findByText(/Then namespace/)).toHaveTextContent("Then namespace seaweedfs is deleted, with everything in it.");
    expect(dialog).not.toHaveTextContent("Namespace seaweedfs is kept");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/minio/teardown"]).toEqual([{ deleteNamespace: true }]));
  });

  it("a namespace this tool did not write is not offered, and says why; unticked sends nothing extra", async () => {
    const own: ResourceState = { ...readyMinio, dataPVCs: ["seaweedfs-data"], namespaceDeletable: false, namespaceKeptReason: "Namespace 'seaweedfs' is kept: it carries this tool's label, but its managedFields show no write by this tool." };
    const api = setup(live({ minio: own, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: true, message: "S3 storage removed.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=fresh");
    fireEvent.click((await menuItem("S3 storage", /Delete S3 storage/))!);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByRole("checkbox")).not.toBeInTheDocument();
    expect(dialog).toHaveTextContent("its managedFields show no write by this tool");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/minio/teardown"]).toEqual([{}]));
  });

  it("a deployed PR image is removed with Revert, and storage of an earlier version with Delete old storage", async () => {
    const legacy: LegacyStorageState = { namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"], stage: "remove", namespaceDeletable: false, usedBy: [] };
    setup(live({ minio: { ...readyMinio, migrationPending: true }, legacyStorage: legacy, pipelineServers: [], mlflow: { ...ownMlflow, prOverride: true, prNumber: 7 } }));
    renderAt("/test-resources?tab=pipelines&project=fresh");
    expect(await menuItem("S3 storage", /Delete old storage/)).not.toBeNull();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
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
    expect(within(storage).getByText("The S3 storage needs a repair")).toBeInTheDocument();
    expect(storage).toHaveTextContent(text);
    expect(storage).toHaveTextContent(/Repair sets it up again with this version's settings and starts it. Stored data is kept./);
    // The fix is the alert's own action.
    fireEvent.click(within(storage).getByRole("button", { name: "Repair" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Repair" }));
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
