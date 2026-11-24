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
const used = dspa("dev", { connectionState: "present", connectionWorkbenches: ["wb-a", "wb-b"] });

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
    expect(details).toHaveTextContent("Used by workbenches wb-a, wb-b. Deleting the pipeline server removes the Connection from them first.");
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
    expect(dialog).toHaveTextContent("These workbenches use the Connection rhoai-nightly-updater-s3; it is removed from them first: dev/wb-a, dev/wb-b.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "dev", confirmWorkbenches: ["wb-a", "wb-b"] }]));
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
      retry={{ req: { pipelineServers: ["dev", "other"] }, contents: {}, workbenches: { other: ["notebook"] } }} />);
    expect(screen.getByText("Workbenches in dev use the dashboard Connection")).toBeInTheDocument();
    expect(screen.getByText("Workbenches in other use the dashboard Connection").closest(".pf-v6-c-alert")).toHaveTextContent("other/notebook");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["dev", "other"], confirmWorkbenches: { dev: ["wb-a", "wb-b"], other: ["notebook"] } });
    // Unchecked pipeline servers contribute no confirmations.
    fireEvent.click(screen.getByRole("checkbox", { name: "Pipeline server in other" }));
    expect(screen.queryByText("Workbenches in other use the dashboard Connection")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenLastCalledWith({ pipelineServers: ["dev"], confirmWorkbenches: { dev: ["wb-a", "wb-b"] } });
  });

  it("a clean-up refused for unconfirmed workbenches comes back listing them, and the retry confirms them", async () => {
    const status: ResourcesStatus = { minio: readyMinio, mlflow: noMlflow, pipelineServers: [dspa("dev"), dspa("other")] };
    let round = 0;
    const api = stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev", "other"] },
      "POST /api/resources/cleanup/stream": () => {
        round++;
        const events = round === 1
          ? [{ step: "pipeline-server:other", status: "failed", message: "2 workbenches in other use the dashboard connection.", errorCode: "confirm_connection_workbenches", detail: "wb-x\nwb-y" },
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
    expect(api.bodies["POST /api/resources/cleanup/stream"][1]).toEqual({ pipelineServers: ["dev", "other"], confirmWorkbenches: { other: ["wb-x", "wb-y"] } });
  });
});
