import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { MaaSModel, MaaSStatus, ResourcesStatus } from "../../types";

// F6 and F2 (live, 2026-11-23): the MaaS tab's rows come from the live
// state. A model whose MaaSModelRef was deleted outside the tool, still
// routed through the MaaS gateway, offers "Restore direct routing" (not
// Unpublish); a model in a project being deleted offers nothing.

const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
const none = { deployed: false, ready: false, managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
const routingText = "No MaaS reference; the model still routes through the MaaS gateway, so its direct address does not answer.";

const maas = (items: MaaSModel[]): MaaSStatus => ({
  platform: { deployed: true, ready: true, managedByTool: true },
  parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }],
  subscription: "rhoai-nightly-updater-subscription",
  items,
});

const pendingRow = (maasOnlySetup = false): MaaSModel => ({
  namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: false, ready: false, managedByTool: true,
  modelReady: true, published: false, routingPending: true, maasOnlySetup, message: routingText,
});

function renderTab(items: MaaSModel[]) {
  const api = stubApi({
    "/api/resources/status": status,
    "/api/resources/projects": { projects: ["dev"] },
    "/api/resources/maas": maas(items),
    "POST /api/resources/maas/stream": sse({ step: "operation_complete", status: "success", message: "done" }),
  });
  render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
  return api;
}

describe("A model whose MaaS reference was deleted outside the tool", () => {
  it("says so from the live state and offers only Restore direct routing, which sends restore-routing", async () => {
    const api = renderTab([pendingRow()]);
    const row = (await screen.findByText("Qwen2.5 0.5B Instruct (CPU)")).closest("tr")!;
    expect(within(row).getByText(routingText)).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "Unpublish" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Change limit" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Publish" })).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: "Restore direct routing" }));
    const confirm = await screen.findByRole("dialog", { name: /Restore direct routing of Qwen2.5 0.5B Instruct in dev\?/ });
    expect(within(confirm).getByText(/its direct address answers again/)).toBeInTheDocument();
    // The other way out: a setup through MaaS publishes it.
    expect(within(confirm).getByText(/To publish it instead, set it up again on the Models tab/)).toBeInTheDocument();
    fireEvent.click(within(confirm).getByRole("button", { name: "Restore direct routing" }));
    await screen.findByRole("dialog", { name: "Restore direct routing of Qwen2.5 0.5B Instruct in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "restore-routing", project: "dev", model: "qwen25-05b-instruct" }]);
  });

  it("a model set up through MaaS only is told it goes back to no outside route", async () => {
    renderTab([pendingRow(true)]);
    const row = (await screen.findByText("Qwen2.5 0.5B Instruct (CPU)")).closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: "Restore direct routing" }));
    const confirm = await screen.findByRole("dialog", { name: /Restore direct routing/ });
    expect(within(confirm).getByText(/it then answers only at its in-cluster address/)).toBeInTheDocument();
    expect(within(confirm).queryByText(/its direct address answers again/)).toBeNull();
  });
});

describe("A model in a project being deleted", () => {
  it("is being removed with its project and offers nothing, whatever the record says", async () => {
    renderTab([{ namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, removing: true, message: "Being removed with its project" }]);
    const row = (await screen.findByText("Qwen2.5 0.5B Instruct (CPU)")).closest("tr")!;
    expect(within(row).getByText("Being removed with its project")).toBeInTheDocument();
    expect(within(row).getByText("Removing")).toBeInTheDocument();
    expect(within(row).queryByRole("button")).toBeNull();
  });
});
