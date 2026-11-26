import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../../types";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const sse = (...events: object[]) => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

afterEach(() => vi.unstubAllGlobals());

// B5: Clean up everything lists the project S3 storages this tool
// recorded; each volume (its data) is a confirmation of its own.
describe("Clean up everything with project S3 storage", () => {
  const status: ResourcesStatus = {
    minio: none, mlflow: none, pipelineServers: [],
    projectStorages: [{ project: "team-a", by: "alice", dataVolume: "nightly-s3-data" }],
  };

  it("offers the storage ticked and its volume unticked, and sends the volume only when ticked", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["team-a"] },
      "POST /api/resources/cleanup/stream": () => sse(
        { step: "project-s3:team-a", status: "success", message: "S3 storage nightly-s3 was deleted from team-a." },
        { step: "operation_complete", status: "success", message: "Done." },
      ),
    });
    render(<MemoryRouter initialEntries={["/test-resources"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    fireEvent.click(await screen.findByRole("button", { name: "Clean up everything" }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    expect(within(dialog).getByRole("checkbox", { name: "S3 storage of team-a" })).toBeChecked();
    const volume = within(dialog).getByRole("checkbox", { name: /Also delete its volume nightly-s3-data/ });
    expect(volume).not.toBeChecked();
    fireEvent.click(volume);
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent("Every file in the S3 storage of team-a");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/cleanup/stream"]).toEqual([{ projectStorages: ["team-a"], projectStorageData: ["team-a"] }]));
  });
});
