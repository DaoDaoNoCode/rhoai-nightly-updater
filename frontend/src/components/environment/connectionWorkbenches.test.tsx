import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { jsonResponse, stubApi } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";
import { CleanupModal } from "./CleanupModal";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin" };
const noMlflow: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const dspa = (ns: string, over: Partial<ResourceState> = {}): ResourceState => ({
  deployed: true, ready: true, managedByTool: true, namespace: ns, name: "dspa", message: "Running", dataPVCs: ["mariadb-dspa"],
  connection: "rhoai-nightly-updater-s3", ...over,
});
const used = dspa("dev", { connectionState: "present", connectionWorkbenches: [{ name: "wb-a", uid: "u-a", running: true }, { name: "wb-b", uid: "u-b" }], connectionEnvOnlyWorkbenches: ["old-wb"] });
const RESTART = "Running workbenches restart; work not saved to disk is lost.";
const ENV_ONLY = "These workbenches use the Connection without the annotation: dev/old-wb. They will fail to start after the deletion; edit them in the dashboard and remove the Connection.";

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

async function menuItem(name: string, label: RegExp): Promise<HTMLElement | null> {
  const toggle = within(await screen.findByRole("listitem", { name: new RegExp(`^${name}$`) })).queryByRole("button", { name: `${name} actions` });
  if (!toggle) return null;
  fireEvent.click(toggle);
  return within(await screen.findByRole("menu")).queryByRole("menuitem", { name: label });
}

afterEach(() => vi.unstubAllGlobals());

// Pipeline server teardown also removes its dashboard Connection. Workbenches
// that still referenced that name could no longer be started or stopped, so
// those links are cleared first and the user must approve each one.
describe("workbenches that use the pipeline server's Connection", () => {
  it("the Pipelines tab names them", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [used] }, "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=pipelines&project=dev");
    const details = await screen.findByLabelText("Pipeline server details");
    expect(details).toHaveTextContent("Used by workbenches wb-a (running), wb-b. Deleting the pipeline server removes the Connection from them first; running workbenches restart.");
    expect(details).toHaveTextContent("Also loaded without the annotation by old-wb: they will fail to start after the pipeline server is deleted; edit them in the dashboard and remove the Connection.");
  });

  it("says when they cannot be read", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa("dev", { connectionState: "present", connectionWorkbenchesMessage: "Your OpenShift account may not read the workbenches of project dev." })] }, "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=pipelines&project=dev");
    expect(await screen.findByLabelText("Pipeline server details")).toHaveTextContent("Cannot check which workbenches use it: Your OpenShift account may not read the workbenches of project dev.");
  });

  it("Delete lists them and sends them as confirmed", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [used] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: true, message: "Deleted.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click((await menuItem("Pipeline server", /Delete pipeline server/))!);
    const dialog = await screen.findByRole("dialog", { name: /Delete the pipeline server in dev\?/ });
    expect(dialog).toHaveTextContent("These workbenches use the Connection rhoai-nightly-updater-s3; it is removed from them first: dev/wb-a (running), dev/wb-b.");
    expect(dialog).toHaveTextContent(RESTART);
    expect(dialog).toHaveTextContent(ENV_ONLY);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "dev", confirmWorkbenches: { "wb-a": "u-a", "wb-b": "u-b" } }]));
  });

  it("Delete without workbenches confirms none", async () => {
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa("dev", { connectionState: "present" })] },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: true, message: "Deleted.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click((await menuItem("Pipeline server", /Delete pipeline server/))!);
    const dialog = await screen.findByRole("dialog", { name: /Delete the pipeline server in dev\?/ });
    expect(dialog).not.toHaveTextContent(/These workbenches use the Connection/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "dev" }]));
  });

  it("Clean up lists them per chosen pipeline server and confirms exactly those", () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [used, dspa("other")] };
    const onConfirm = vi.fn();
    render(<CleanupModal scope="pipelines" status={status} minioLeftovers={false} baseReason={null} onConfirm={onConfirm} onCancel={() => {}}
      retry={{ req: { pipelineServers: ["dev", "other"] }, contents: {}, workbenches: { dev: [], other: [{ name: "notebook", uid: "u-n" }] } }} />);
    const devAlert = screen.getByText("Workbenches in dev use the dashboard Connection").closest(".pf-v6-c-alert");
    expect(devAlert).toHaveTextContent("dev/wb-a (running)");
    expect(devAlert).toHaveTextContent(RESTART);
    expect(devAlert).toHaveTextContent(ENV_ONLY);
    expect(screen.getByText("Workbenches in other use the dashboard Connection").closest(".pf-v6-c-alert")).toHaveTextContent("other/notebook");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["dev", "other"], confirmWorkbenches: { dev: { "wb-a": "u-a", "wb-b": "u-b" }, other: { notebook: "u-n" } } });
    // Unchecked pipeline servers contribute no confirmations.
    fireEvent.click(screen.getByRole("checkbox", { name: "Pipeline server in other" }));
    expect(screen.queryByText("Workbenches in other use the dashboard Connection")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["dev"], confirmWorkbenches: { dev: { "wb-a": "u-a", "wb-b": "u-b" } } });
  });

  it("a clean-up refused for unconfirmed workbenches comes back listing them, and the retry confirms them", async () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa("dev"), dspa("other")] };
    let round = 0;
    const api = stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev", "other"] },
      "POST /api/resources/cleanup/stream": () => {
        round++;
        const events = round === 1
          ? [{ step: "pipeline-server:other", status: "failed", message: "2 workbenches in other use the dashboard connection.", errorCode: "confirm_connection_workbenches", detail: "wb-x=u-x\nwb-y=u-y" },
            { step: "operation_complete", status: "failed", message: "Nothing was deleted.", errorCode: "confirm_connection_workbenches" }]
          : [{ step: "operation_complete", status: "success", message: "Deleted." }];
        return new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
      },
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up Pipelines/ }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Clean up Pipelines\?/ })).getByRole("button", { name: "Delete" }));
    const again = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    expect(within(again).getByText("Workbenches in other use the dashboard Connection").closest(".pf-v6-c-alert")).toHaveTextContent("other/wb-x, other/wb-y");
    fireEvent.click(within(again).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/cleanup/stream"]).toHaveLength(2));
    expect(api.bodies["POST /api/resources/cleanup/stream"][1]).toEqual({ pipelineServers: ["dev", "other"], confirmWorkbenches: { other: { "wb-x": "u-x", "wb-y": "u-y" } } });
  });

  it("a refusal after earlier parts ran keeps the run's outcome on screen; the next Clean up lists the workbenches", async () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa("dev"), dspa("other")] };
    let round = 0;
    const api = stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev", "other"] },
      "POST /api/resources/cleanup/stream": () => {
        round++;
        const events = round === 1
          ? [{ step: "pipeline-server:dev", status: "success", message: "Pipeline server removed from 'dev'." },
            { step: "pipeline-server:other", status: "failed", message: "1 workbench in other uses the dashboard connection (since the deletion started).", errorCode: "confirm_connection_workbenches", detail: "late-wb=u-late" },
            { step: "operation_complete", status: "failed", message: "Late refusal. Deleted before it stopped: the pipeline server in 'dev'.", errorCode: "confirm_connection_workbenches" }]
          : [{ step: "operation_complete", status: "success", message: "Deleted." }];
        return new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
      },
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up Pipelines/ }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Clean up Pipelines\?/ })).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Clean up" });
    expect(await within(run).findByText(/Deleted before it stopped: the pipeline server in 'dev'/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: /Clean up everything\?/ })).not.toBeInTheDocument();
    fireEvent.click(within(run).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!);
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up Pipelines/ }));
    const again = await screen.findByRole("dialog", { name: /Clean up Pipelines\?/ });
    expect(within(again).getByText("Workbenches in other use the dashboard Connection").closest(".pf-v6-c-alert")).toHaveTextContent("other/late-wb");
    fireEvent.click(within(again).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/cleanup/stream"]).toHaveLength(2));
    expect(api.bodies["POST /api/resources/cleanup/stream"][1]).toMatchObject({ confirmWorkbenches: { other: { "late-wb": "u-late" } } });
  });

  it("Recreate says the running workbenches restart, and sends the confirmation", async () => {
    const old = { ...used, name: "nightly-dspa", nonStandardName: true, dataPVCs: ["mariadb-nightly-dspa"] };
    const api = stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: noMlflow, pipelineServers: [old] } satisfies ResourcesStatus,
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/pipeline-server/setup": () => jsonResponse({ success: true, message: "Recreated.", logs: [] }),
    });
    renderAt("/test-resources?tab=pipelines&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Recreate with the standard name" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("it is removed from them first: dev/wb-a (running), dev/wb-b.");
    expect(dialog).toHaveTextContent(RESTART);
    expect(dialog).toHaveTextContent(ENV_ONLY);
    fireEvent.click(within(dialog).getByRole("button", { name: "Recreate" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/setup"]).toEqual([{ project: "dev", recreate: true, confirmWorkbenches: { "wb-a": "u-a", "wb-b": "u-b" } }]));
  });
});
