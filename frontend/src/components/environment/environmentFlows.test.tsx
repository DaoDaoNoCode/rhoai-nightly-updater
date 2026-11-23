import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { jsonResponse, stubApi } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";
import { cleanupBlocked } from "./CleanupModal";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", dataPVCs: ["seaweedfs-data"], namespaceDeletable: true };
const dspa = (ns: string, over: Partial<ResourceState> = {}): ResourceState => ({
  deployed: true, ready: true, managedByTool: true, namespace: ns, name: "nightly-dspa", message: "Running", dataPVCs: [`mariadb-nightly-dspa`],
  uiRoute: `https://rh-ai.apps.example.com/develop-train/pipelines/definitions/${ns}`, ...over,
});
const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running", uiRoute: "https://rh-ai.apps.example.com/mlflow" };

const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string, props: Partial<React.ComponentProps<typeof QuickResourceCreator>> = {}) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} {...props} /></MemoryRouter>);
}

const footerClose = (dialog: HTMLElement) => within(dialog).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!;

afterEach(() => vi.unstubAllGlobals());

// Item 4: one confirmed operation that removes what the tool created.
describe("Clean up", () => {
  const everything: ResourcesStatus = { minio: { ...readyMinio, usedBy: ["a/nightly-dspa", "b/nightly-dspa"] }, mlflow: ownMlflow, pipelineServers: [dspa("a"), dspa("b")] };

  it("Clean up everything lists what this tool created, ticked (the namespace not), and runs it as one streamed operation", async () => {
    const api = stubApi({
      "/api/resources/status": everything,
      "/api/resources/projects": { projects: ["a", "b"] },
      "POST /api/resources/cleanup/stream": sse(
        { step: "pipeline-server:a", status: "success", message: "Pipeline server removed from 'a'.", elapsedMs: 1 },
        { step: "pipeline-server:b", status: "success", message: "Pipeline server removed from 'b'.", elapsedMs: 2 },
        { step: "storage", status: "success", message: "S3 storage removed.", elapsedMs: 3 },
        { step: "mlflow", status: "success", message: "MLflow removed.", elapsedMs: 4 },
        { step: "operation_complete", status: "success", message: "Deleted the pipeline server in 'a', the pipeline server in 'b', the S3 storage and MLflow.", elapsedMs: 5 },
      ),
    });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything…" }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    for (const name of ["Pipeline server in a", "Pipeline server in b", "S3 storage", "MLflow server"]) {
      expect(within(dialog).getByRole("checkbox", { name })).toBeChecked();
    }
    expect(within(dialog).getByRole("checkbox", { name: /Also delete namespace seaweedfs/ })).not.toBeChecked();
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent("Every MLflow experiment, run and model");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Clean up" });
    expect(await within(run).findByText("Deleted the pipeline server in 'a', the pipeline server in 'b', the S3 storage and MLflow.")).toBeInTheDocument();
    expect(within(run).getByText("Delete the pipeline server in a")).toBeInTheDocument();
    expect(within(run).getByText("Delete MLflow")).toBeInTheDocument();
    // One request for the whole run: one lock and one rate-limit slot on the server.
    expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([{ pipelineServers: ["a", "b"], storage: true, mlflow: true }]);
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/pipeline-server/teardown"))).toHaveLength(0);
  });

  it("Clean up Pipelines offers only the pipeline servers, never the S3 storage the cluster shares", async () => {
    const api = stubApi({
      "/api/resources/status": everything, "/api/resources/projects": { projects: ["a", "b"] },
      "POST /api/resources/cleanup/stream": () => new Response("", { status: 200, headers: { "Content-Type": "text/event-stream" } }),
    });
    renderAt("/test-resources?tab=pipelines&project=a");
    expect(await screen.findByText(/The S3 storage stays: other features can use it/)).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up Pipelines/ }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up Pipelines\?/ });
    expect(within(dialog).queryByRole("checkbox", { name: "MLflow server" })).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("checkbox", { name: /S3 storage/ })).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Pipeline server in b" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([{ pipelineServers: ["a"] }]));
  });

  it("Clean up everything: the storage cannot go while an unselected pipeline server uses it: Delete says why, inline", async () => {
    stubApi({ "/api/resources/status": everything, "/api/resources/projects": { projects: ["a", "b"] } });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up everything/ }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Pipeline server in b" }));
    const del = within(dialog).getByRole("button", { name: "Delete" });
    expect(del).toHaveAttribute("aria-disabled", "true");
    expect(del).toHaveAccessibleDescription(/The S3 storage is used by b\/nightly-dspa. Select that pipeline server too, or keep the storage./);
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "S3 storage" }));
    expect(within(dialog).getByRole("button", { name: "Delete" })).not.toHaveAttribute("aria-disabled");
  });

  it("a pipeline server this tool did not create blocks the storage, and is never offered", () => {
    const s: ResourcesStatus = { ...everything, minio: { ...readyMinio, usedBy: ["a/nightly-dspa", "team-x/dspa"] } };
    expect(cleanupBlocked(s, { pipelineServers: ["a", "b"], storage: true })).toMatch(/team-x\/dspa, which this tool did not create/);
    expect(cleanupBlocked(s, { pipelineServers: ["a", "b"] })).toBeNull();
  });

  it("a failed part stops the run; the outcome says what was done and what was not", async () => {
    stubApi({
      "/api/resources/status": everything,
      "/api/resources/projects": { projects: ["a", "b"] },
      "POST /api/resources/cleanup/stream": sse(
        { step: "pipeline-server:a", status: "success", message: "Pipeline server removed from 'a'.", elapsedMs: 1 },
        { step: "pipeline-server:b", status: "failed", message: "Deletion of pipeline server 'nightly-dspa' was requested, but it is still terminating.", elapsedMs: 2 },
        { step: "operation_complete", status: "failed", message: "Deletion of pipeline server 'nightly-dspa' was requested, but it is still terminating. Deleted before it stopped: the pipeline server in 'a'. Not done: the S3 storage and MLflow.", elapsedMs: 3 },
      ),
    });
    renderAt("/test-resources");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything…" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Clean up" });
    expect(await within(run).findByText("Clean up: stopped")).toBeInTheDocument();
    expect(run).toHaveTextContent("Not done: the S3 storage and MLflow.");
  });
});

describe("one feedback pattern", () => {
  it("a run whose dialog was closed reports in a toast, with a way back to the dialog", async () => {
    let finish: (r: Response) => void = () => {};
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: ownMlflow, pipelineServers: [] },
      "/api/resources/projects": { projects: ["a"] },
      "POST /api/resources/mlflow/teardown": () => new Promise<Response>((r) => { finish = r; }),
    });
    renderAt("/test-resources?tab=mlflow");
    fireEvent.click(await screen.findByRole("button", { name: "MLflow server actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Delete MLflow/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete MLflow" });
    expect(run).toHaveTextContent("You can close this dialog: the deletion goes on");
    fireEvent.click(footerClose(run));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await act(async () => finish(jsonResponse({ success: true, message: "MLflow removed.", logs: [] })));
    const toast = await screen.findByText(/Delete MLflow: MLflow removed./);
    fireEvent.click(within(toast.closest(".pf-v6-c-alert") as HTMLElement).getByRole("button", { name: "Show details" }));
    expect(await screen.findByRole("dialog", { name: "Delete MLflow" })).toBeInTheDocument();
  });
});

// Item 3: Set up never creates part of a feature when a later part cannot be set up.
describe("Set up checks every part first", () => {
  it("a component turned off since the page loaded stops the run before anything is created", async () => {
    let status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/setup": () => jsonResponse({ success: true, message: "ok", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    const confirm = await screen.findByRole("dialog");
    status = { ...status, pipelinesBlocked: { component: "aipipelines", dsc: "default-dsc", state: "Removed", reason: "Data Science Pipelines is turned off on this cluster." } };
    fireEvent.click(within(confirm).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    expect(await within(run).findByText("Data Science Pipelines is turned off on this cluster. Nothing was changed.")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST"))).toEqual([]);
  });
});

// Item 13: the project list follows the cluster.
describe("project picker", () => {
  it("a project deleted elsewhere leaves the picker on refresh, and the address naming it offers no Set up", async () => {
    let projects = ["dev", "gone"];
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: none, pipelineServers: [] },
      "/api/resources/projects": () => jsonResponse({ projects }),
    });
    const renderHeader = ({ onRefresh }: { onRefresh: () => void }) => <button type="button" onClick={onRefresh}>Refresh</button>;
    renderAt("/test-resources?tab=pipelines&project=gone", { renderHeader });
    expect(await screen.findByRole("button", { name: "Set up the pipeline server in gone" })).toBeInTheDocument();
    projects = ["dev"];
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByText(/Project gone no longer exists/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Project" })).toHaveValue("");
    fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
    // The menu shows on PatternFly Popper's timer (setTimeout).
    await waitFor(() => expect(screen.getByRole("listbox")).toBeVisible());
    expect(screen.queryByRole("option", { name: "gone" })).not.toBeInTheDocument();
  });
});

// A server set up under the earlier name: workbench pipelines need "dspa"
// (odh-notebook-controller dspaInstanceName); Recreate replaces it.
describe("Pipelines: the earlier name", () => {
  it("explains the workbench limitation and recreates the server as dspa after a confirmation that names the loss", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: none, pipelineServers: [dspa("dev", { nonStandardName: true })] },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/pipeline-server/setup": { success: true, message: "Pipeline server 'nightly-dspa' replaced by 'dspa'." },
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    const alert = await screen.findByText(/Workbench pipelines don.t work with this pipeline server/);
    const box = alert.closest(".pf-v6-c-alert") as HTMLElement;
    expect(box).toHaveTextContent("nightly-dspa");
    fireEvent.click(within(box).getByRole("button", { name: "Recreate with the standard name" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("DataSciencePipelinesApplication nightly-dspa");
    expect(dialog).toHaveTextContent("mariadb-nightly-dspa");
    expect(dialog).toHaveTextContent(/Artifacts already in the S3 storage stay there/);
    expect(dialog).toHaveTextContent("DataSciencePipelinesApplication dspa");
    fireEvent.click(within(dialog).getByRole("button", { name: "Recreate" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/setup"]).toEqual([{ project: "dev", recreate: true }]));
  });

  // The Connection is shown from the Secret itself (connectionState, read
  // for the selected project), never from the DSPA's annotation alone.
  const withConnection = (over: Partial<ResourceState>) => ({
    "/api/resources/status": { minio: readyMinio, mlflow: none, pipelineServers: [dspa("dev", { name: "dspa", dataPVCs: ["mariadb-dspa"], connection: "rhoai-nightly-updater-s3", ...over })] },
    "/api/resources/projects": { projects: ["dev"] },
  });

  it("asks for the selected project's Connection and shows it when it is there", async () => {
    const api = stubApi(withConnection({ connectionState: "present" }));
    renderAt("/test-resources?tab=pipelines&project=dev");
    const details = await screen.findByLabelText("Pipeline server details");
    expect(details).toHaveTextContent("Connection in the dashboard");
    expect(details).toHaveTextContent("rhoai-nightly-updater-s3");
    expect(details).toHaveTextContent(/workbenches and model deployments/);
    expect(details).toHaveTextContent(/in-cluster address only/);
    expect(api.calls).toContain("GET /api/resources/status?project=dev");
  });

  it("does not claim a Connection the status did not read", async () => {
    stubApi(withConnection({}));
    renderAt("/test-resources?tab=pipelines&project=dev");
    const details = await screen.findByLabelText("Pipeline server details");
    expect(details).not.toHaveTextContent("rhoai-nightly-updater-s3");
  });

  it("says when the Connection was deleted, and Set up again recreates it", async () => {
    const api = stubApi({ ...withConnection({ connectionState: "missing" }), "POST /api/resources/pipeline-server/setup": { success: true, message: "Pipeline server created." } });
    renderAt("/test-resources?tab=pipelines&project=dev");
    const details = await screen.findByLabelText("Pipeline server details");
    expect(details).toHaveTextContent(/Missing: rhoai-nightly-updater-s3 was deleted, for example in the dashboard/);
    fireEvent.click(within(details).getByRole("button", { name: "Set up again" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/runs and data are kept/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up again" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/setup"]).toEqual([{ project: "dev" }]));
  });

  it("says when a Secret of that name is someone else's", async () => {
    stubApi(withConnection({ connectionState: "foreign" }));
    renderAt("/test-resources?tab=pipelines&project=dev");
    const details = await screen.findByLabelText("Pipeline server details");
    expect(details).toHaveTextContent(/not created by this tool/);
    expect(within(details).queryByRole("button", { name: "Set up again" })).not.toBeInTheDocument();
  });

  it("a server named dspa shows no such notice", async () => {
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: none, pipelineServers: [dspa("dev", { name: "dspa", dataPVCs: ["mariadb-dspa"] })] },
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    await screen.findByRole("region", { name: "Use it" });
    expect(screen.queryByText(/Workbench pipelines don.t work/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Recreate with the standard name" })).not.toBeInTheDocument();
  });
});

// Item 12: how to use a ready pipeline server.
describe("Pipelines: use it", () => {
  it("a ready server shows where runs are, the API endpoint and a KFP client, and what next", async () => {
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: none, pipelineServers: [dspa("dev", { apiRoute: "https://ds-pipeline-nightly-dspa-dev.apps.example.com" })] },
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    const use = await screen.findByRole("region", { name: "Use it" });
    expect(use).toHaveTextContent("Choose Import pipeline");
    expect(use).toHaveTextContent("API endpointhttps://ds-pipeline-nightly-dspa-dev.apps.example.com");
    // A short-lived token that may only use this server's API, never the
    // user's own: the server runs in a project its editors control.
    expect(use).toHaveTextContent("oc create role pipeline-api -n dev --verb=get,create");
    expect(use).toHaveTextContent("--resource-name=nightly-dspa");
    expect(use).toHaveTextContent("export TOKEN=$(oc create token pipeline-api -n dev --duration=15m)");
    expect(use).toHaveTextContent('client = kfp.Client(host="https://ds-pipeline-nightly-dspa-dev.apps.example.com", existing_token=os.environ["TOKEN"])');
    expect(use).not.toHaveTextContent("whoami");
    expect(use).not.toHaveTextContent("Sign in with your OpenShift token");
    expect(screen.getByRole("region", { name: "What next" })).toHaveTextContent("Create a run");
  });
});
