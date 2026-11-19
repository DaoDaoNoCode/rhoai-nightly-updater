import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { GROUP_CHECK_DELAY_MS } from "./MaaSDialogFields";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { MaaSDatabase, MaaSStatus, ResourcesStatus } from "../../types";

const none = { deployed: false, ready: false, managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
const done = sse({ step: "operation_complete", status: "success", message: "done" });
const database = (extra: Partial<MaaSDatabase> = {}): MaaSDatabase => ({
  current: "", namespace: "redhat-ai-gateway-infra", needed: true, default: "development",
  choices: [
    { id: "external", plan: ["Write Secret redhat-ai-gateway-infra/maas-db-config"] },
    { id: "development", plan: ["Run the MaaS docs' development PostgreSQL"] },
    { id: "cnpg", plan: ["Install CloudNativePG 1.27.1", "Run a one-instance CloudNativePG database"] },
  ],
  ...extra,
});
const maas = (extra: Partial<MaaSStatus>): MaaSStatus => ({
  platform: { deployed: false, ready: false, managedByTool: false }, subscription: "rhoai-nightly-updater-subscription",
  parts: [{ id: "database", title: "Database (PostgreSQL) and maas-db-config", state: "missing" }], ...extra,
});
const open = () => render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);

describe("MaaS dialogs", () => {
  it("Set up asks for the database: the docs' development one by default, the URL masked and checked before Set up", async () => {
    const api = stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: [] },
      "/api/resources/maas": maas({ database: database(), setupPlan: ["Create Gateway openshift-ingress/maas-default-gateway"] }), "POST /api/resources/maas/stream": done });
    open();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up Models as a Service?" });
    expect(within(dialog).getByRole("radio", { name: /^Development database/ })).toBeChecked();
    // The development database warns while it is chosen.
    expect(within(dialog).getByText("API keys are lost whenever this database restarts")).toBeInTheDocument();
    expect(within(dialog).getByText("Not in the MaaS docs")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Your PostgreSQL/ }));
    const setUp = within(dialog).getByRole("button", { name: "Set up" });
    expect(setUp).toBeDisabled();
    const url = within(dialog).getByLabelText(/Connection URL/);
    expect(url).toHaveAttribute("type", "password");
    fireEvent.change(url, { target: { value: "mysql://u:S3cret@db/maas" } });
    // Checked when the field is left, not at every keystroke.
    expect(within(dialog).queryByText(/must begin with postgresql:\/\//)).toBeNull();
    fireEvent.blur(url);
    expect(within(dialog).getByText(/must begin with postgresql:\/\//)).toBeInTheDocument();
    expect(dialog.textContent).not.toContain("S3cret");
    fireEvent.change(url, { target: { value: "postgresql://u:S3cret@db.example.com:5432/maas?sslmode=require" } });
    expect(setUp).toBeEnabled();
    fireEvent.click(setUp);
    await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "setup", database: { kind: "external", connectionUrl: "postgresql://u:S3cret@db.example.com:5432/maas?sslmode=require" } }]);
  });

  it("Publish sends the chosen preset and audience; the published table shows the limit; Change limit edits it", async () => {
    const api = stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maas({ parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], platform: { deployed: true, ready: true, managedByTool: true },
        items: [{ namespace: "dev", name: "qwen3-1-7b", displayName: "Qwen3 1.7B", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not published" }] }),
      "POST /api/resources/maas/stream": done });
    open();
    fireEvent.click(await screen.findByRole("button", { name: "Publish" }));
    const dialog = await screen.findByRole("dialog", { name: /Publish qwen3-1-7b in dev to MaaS\?/ });
    fireEvent.click(within(dialog).getByRole("radio", { name: /Premium example: 100,000 tokens a day/ }));
    fireEvent.click(within(dialog).getByRole("radio", { name: /Specific groups and users/ }));
    const publish = within(dialog).getByRole("button", { name: "Publish" });
    expect(publish).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText("Groups"), { target: { value: "ml-team\ndata" } });
    fireEvent.click(publish);
    await screen.findByRole("dialog", { name: "Publish qwen3-1-7b in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "publish", project: "dev", model: "qwen3-1-7b", limit: { preset: "premium" }, access: { everyone: false, groups: ["ml-team", "data"], users: [] } }]);
  });

  it("Publish warns about groups with no OpenShift Group while they are typed", async () => {
    const api = stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maas({ parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], platform: { deployed: true, ready: true, managedByTool: true },
        items: [{ namespace: "dev", name: "qwen3-1-7b", displayName: "Qwen3 1.7B", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false }] }),
      "/api/resources/maas/groups": [{ name: "nobody-yet", missing: true, message: "x", commands: "oc adm groups new nobody-yet" }],
      "POST /api/resources/maas/stream": done });
    open();
    fireEvent.click(await screen.findByRole("button", { name: "Publish" }));
    const dialog = await screen.findByRole("dialog", { name: /Publish qwen3-1-7b in dev to MaaS\?/ });
    fireEvent.click(within(dialog).getByRole("radio", { name: /Specific groups and users/ }));
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      fireEvent.change(within(dialog).getByLabelText("Groups"), { target: { value: "ml-team\nnobody-yet\n/admins" } });
      // A name that cannot be an OpenShift Group's is said at once, without asking.
      expect(within(dialog).getByText("/admins is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.")).toBeInTheDocument();
      expect(api.calls.filter((c) => c.includes("/maas/groups"))).toEqual([]);
      await act(() => vi.advanceTimersByTimeAsync(GROUP_CHECK_DELAY_MS));
      expect(api.calls.filter((c) => c.includes("/maas/groups"))).toEqual(["GET /api/resources/maas/groups?name=ml-team&name=nobody-yet"]);
      expect(within(dialog).getByText(/^nobody-yet is not a group on this cluster/)).toBeInTheDocument();
      expect(within(dialog).queryByText(/^ml-team is not a group/)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
    // A warning, not a refusal: Publish stays possible.
    expect(within(dialog).getByRole("button", { name: "Publish" })).toBeEnabled();
  });

  it("Change limit takes custom rates by the CRD's rules", async () => {
    const api = stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maas({ parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], platform: { deployed: true, ready: true, managedByTool: true },
        access: { everyone: false, groups: ["ml-team"], groupProblems: [{ name: "ml-team", missing: true, message: "ml-team is not a group on this cluster.", commands: "oc adm groups new ml-team" }] },
        items: [{ namespace: "dev", name: "qwen3-1-7b", displayName: "Qwen3 1.7B", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true, limit: { preset: "free", rates: [{ limit: 100, window: "1m" }] } }] }),
      "POST /api/resources/maas/stream": done });
    open();
    const row = (await screen.findByText("Qwen3 1.7B")).closest("tr")!;
    expect(within(row).getByText("100 tokens a minute")).toBeInTheDocument();
    expect(screen.getAllByText("Only group ml-team").length).toBeGreaterThan(0);
    expect(screen.getByText("Group ml-team does not exist")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy the commands that create group ml-team" })).toBeInTheDocument();
    fireEvent.click(within(row).getByRole("button", { name: "Change limit" }));
    const dialog = await screen.findByRole("dialog", { name: /Change the limit of qwen3-1-7b in dev\?/ });
    fireEvent.click(within(dialog).getByRole("radio", { name: /Custom/ }));
    fireEvent.change(within(dialog).getByLabelText("Limit 1: tokens"), { target: { value: "0" } });
    expect(within(dialog).getByLabelText("Limit 1: tokens")).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByText(/Enter a whole number of tokens from 1 to 1,000,000,000/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Change limit" })).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText("Limit 1: tokens"), { target: { value: "500" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Change limit" }));
    await screen.findByRole("dialog", { name: "Change the limit of qwen3-1-7b in dev" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "change-limit", project: "dev", model: "qwen3-1-7b", limit: { preset: "custom", rates: [{ limit: 500, window: "1m" }] } }]);
  });

  it("Change database is offered only by the database part, and needs the loss of every key confirmed", async () => {
    const api = stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: [] },
      "/api/resources/maas": maas({ database: database({ current: "development", needed: false }), platform: { deployed: true, ready: true, managedByTool: true },
        parts: [{ id: "database", title: "Database (PostgreSQL) and maas-db-config", state: "ready", managedByTool: true,
          actions: [{ id: "change-database" as never, label: "Change database", summary: "", changes: [], danger: true }] }] }),
      "POST /api/resources/maas/stream": done });
    open();
    fireEvent.click(await screen.findByRole("button", { name: "Change database" }));
    const dialog = await screen.findByRole("dialog", { name: /Change the MaaS database\?/ });
    expect(within(dialog).queryByRole("radio", { name: /Development database/ })).toBeNull();
    // Nothing preselected, no "Default", and the loss shown as the data-loss alert.
    expect(within(dialog).getAllByRole("radio").filter((r) => (r as HTMLInputElement).checked)).toEqual([]);
    expect(within(dialog).queryByText("Default")).toBeNull();
    expect(within(dialog).getByText("Data is deleted and cannot be recovered")).toBeInTheDocument();
    expect(within(dialog).getByText(/permanently deletes the current one \(Development database\)/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("radio", { name: /CloudNativePG/ }));
    const confirm = within(dialog).getByRole("button", { name: "Change database" });
    expect(confirm).toBeDisabled();
    fireEvent.click(within(dialog).getByLabelText("I understand that every API key stops working"));
    fireEvent.click(confirm);
    await screen.findByRole("dialog", { name: "Change the MaaS database" });
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "change-database", database: { kind: "cnpg" }, confirmKeysLost: true }]);
  });
});
