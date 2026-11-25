import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { MaaSAction, MaaSStatus, ResourcesStatus } from "../../types";
import { IDLE, maasPlatformStatus } from "./featureState";
import { maasSteps, SubscriptionPriority } from "./MaaSTab";

const FIX: MaaSAction = {
  id: "subscription-priority", label: "Set to the lowest priority (0)",
  summary: "Sets the priority of this tool's subscription from 10 to 0, the lowest MaaS honours.",
  changes: ["MaaSSubscription models-as-a-service/rhoai-nightly-updater-subscription: spec.priority 10 to 0 (a merge patch with the read resourceVersion, after a server-side dry run); nothing else of it changes."],
};

/** MaaS ready with this tool's model published, and the subscription's priority as given. */
const published = (subscriptionPriority?: MaaSStatus["subscriptionPriority"]): MaaSStatus => ({
  platform: { deployed: true, ready: true, managedByTool: true }, subscription: "rhoai-nightly-updater-subscription",
  parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }], url: "https://maas.apps.example.com", access: { everyone: true },
  items: [{ namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true, ready: true, managedByTool: true, modelReady: true, published: true }],
  ...(subscriptionPriority ? { subscriptionPriority } : {}),
});

const none = { deployed: false, ready: false, managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

const subscriptionRow = async () => (await screen.findByText("Subscription")).closest(".pf-v6-c-description-list__group") as HTMLElement;

describe("The Subscription row: priority", () => {
  it("at 0, says it is the lowest MaaS honours, how a tie is decided and to name the subscription; no warning", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/maas": published({ value: 0 }) });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const row = await subscriptionRow();
    expect(row.textContent).toContain("It has the lowest priority MaaS honours (0), so it does not outrank other subscriptions.");
    expect(row.textContent).toContain("among those at 0 the one whose largest single token limit on any of its models is larger (the time window is not compared; Unlimited counts as largest), then the first by name");
    expect(row.textContent).toContain("Name the subscription when you create a key to avoid the choice");
    expect(within(row).queryByText(/This tool's subscription has priority/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Set to the lowest priority (0)" })).toBeNull();
    // The old promise is gone from what Publish creates.
    expect(screen.queryByText(/never takes a key meant for someone else/)).toBeNull();
  });

  it("names the subscriptions MaaS ranks level with it, a negative one with its own value, read-only", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": published({ value: 0, ties: [{ name: "org-49315", priority: 0 }, { name: "legacy-free", priority: -2 }] }) });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const row = await subscriptionRow();
    expect(row.textContent).toContain("Subscriptions MaaS ranks level with it: org-49315, legacy-free (-2, read as 0).");
    expect(row.textContent).toContain("SpecPriorityDuplicate");
    expect(row.textContent).toContain("A priority of 1 or more on theirs avoids the tie.");
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("with no priority read, says nothing about it", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/maas": published() });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const row = await subscriptionRow();
    expect(row.textContent).not.toContain("lowest priority");
  });

  it("the warning shows also without Use it (the platform not ready)", async () => {
    const m = { ...published({ value: 10, fix: FIX }), platform: { deployed: true, ready: false, managedByTool: true },
      parts: [{ id: "maas-api", title: "MaaS API (maas-api)", state: "waiting" as const }] };
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/maas": m });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByText("This tool's subscription has priority 10")).toBeInTheDocument();
    expect(screen.queryByText("Subscription")).toBeNull();
    expect(screen.getByRole("button", { name: "Set to the lowest priority (0)" })).toBeInTheDocument();
  });

  it("above 0 (an earlier version's 10): a warning that keys already made keep it, and the one-click fix through its confirmation", async () => {
    const api = stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": published({ value: 10, fix: FIX }),
      "POST /api/resources/maas/stream": sse({ step: "check", status: "success", message: "ok" }, { step: "operation_complete", status: "success", message: "This tool's subscription has priority 0, the lowest MaaS honours." }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const row = await subscriptionRow();
    expect(within(row).getByText("This tool's subscription has priority 10")).toBeInTheDocument();
    expect(row.textContent).toContain("users of this subscription who are also in such a subscription get this tool's one");
    expect(row.textContent).not.toContain("signed-in users");
    expect(row.textContent).toContain("Until then, name the subscription when you create a key.");
    expect(row.textContent).toContain("API keys made before keep the subscription they were bound to: revoke them and make them again");
    expect(row.textContent).not.toContain("It has the lowest priority MaaS honours (0)");
    fireEvent.click(within(row).getByRole("button", { name: "Set to the lowest priority (0)" }));
    const confirm = await screen.findByRole("dialog", { name: /Set to the lowest priority \(0\)\?/ });
    expect(within(confirm).getByText(/spec.priority 10 to 0/)).toBeInTheDocument();
    fireEvent.click(within(confirm).getByRole("button", { name: "Set to the lowest priority (0)" }));
    const run = await screen.findByRole("dialog", { name: "Set to the lowest priority (0)" });
    expect(within(run).getByText("Set its priority to 0")).toBeInTheDocument();
    expect(await within(run).findByText(/no longer outranks other subscriptions.*Keys made before keep the subscription they were bound to: revoke them and make them again/)).toBeInTheDocument();
    expect(within(run).queryByText("Wait until MaaS applies it")).toBeNull();
    expect(within(run).queryByText(/publish a model to offer it through MaaS/)).toBeNull();
    expect(api.bodies["POST /api/resources/maas/stream"]).toEqual([{ action: "subscription-priority" }]);
  });

  it("below 0: says MaaS reads it as 0 and offers the fix", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/maas": published({ value: -4, fix: { ...FIX, changes: ["spec.priority -4 to 0"] } }) });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const row = await subscriptionRow();
    expect(within(row).getByText("This tool's subscription has priority -4")).toBeInTheDocument();
    expect(row.textContent).toContain("MaaS reads a negative priority as 0");
    expect(row.textContent).toContain("users of this subscription who are also in one of those get the one whose largest single token limit");
    expect(row.textContent).toContain("Until then, name the subscription when you create a key.");
    expect(row.textContent).toContain("revoke them and make them again");
    expect(within(row).getByRole("button", { name: "Set to the lowest priority (0)" })).toBeInTheDocument();
  });

  it("read-only: the warning without the action; while busy: the action is disabled", () => {
    const pending: unknown[] = [];
    const m = published({ value: 10, fix: FIX });
    const { unmount } = render(<SubscriptionPriority maas={m} readOnly reason={null} setPending={(p) => pending.push(p)} />);
    expect(screen.getByText("This tool's subscription has priority 10")).toBeInTheDocument();
    expect(screen.getByText(/A cluster-admin sets it to 0/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Set to the lowest priority (0)" })).toBeNull();
    unmount();
    render(<SubscriptionPriority maas={m} readOnly={false} reason="Another change is running." setPending={(p) => pending.push(p)} />);
    const button = screen.getByRole("button", { name: "Set to the lowest priority (0)" });
    expect(button).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(button);
    expect(pending).toEqual([]);
  });

  it("runs in the backend's steps and is a subscription change, not a platform setup", () => {
    expect(maasSteps({ kind: "maas-action", action: "subscription-priority" }).map((s) => s.id)).toEqual(["check", "priority"]);
    expect(maasPlatformStatus(published({ value: 10, fix: FIX }), { ...IDLE, maas: { action: "subscription-priority", project: null } })).toBe("ready");
  });
});

describe("Use it: what to try next", () => {
  it("says the tool's chat models report usage when streaming, so MaaS counts streamed tokens too", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/maas": published({ value: 0 }) });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const next = await screen.findByText(/MaaS counts the tokens each response reports/);
    expect(next.textContent).toContain("this tool's chat models run with --enable-force-include-usage, so streaming responses report them too");
    expect(next.textContent).toContain("A model set up by an earlier version lacks it: set it up again on the Models tab (it restarts).");
    expect(next.textContent).not.toContain("only when the model server runs with");
  });
});

describe("The MaaS portal row", () => {
  it("says the portal comes up once Dashboard Dev is reverted, when RHOAI reports nothing yet", async () => {
    const note = "Dashboard Dev keeps the dashboard operator redhat-ods-applications/rhods-dashboard-operator at 0 replicas, and that operator deploys the portal: it comes up once Dashboard Dev is reverted (Dashboard Dev page).";
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": { ...published({ value: 0 }), portal: { managementState: "Managed", on: true, available: "", message: note } } });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByText(/RHOAI reports nothing about it yet\. Dashboard Dev keeps the dashboard operator/)).toHaveTextContent("it comes up once Dashboard Dev is reverted (Dashboard Dev page).");
  });
});

describe("A published model without usage reporting", () => {
  it("its row warns that streamed tokens are not counted and says how to fix it", async () => {
    const warning = "Its vLLM runs without --enable-force-include-usage (set up by an earlier version). The MaaS docs require it so that streamed chats report their tokens; where the gateway does not add them itself, MaaS counts such chats as 0 tokens and token limits do not apply to them. Set it up again on the Models tab to add it (the model restarts); Publish refuses it until then.";
    const m = published({ value: 0 });
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": { ...m, items: m.items!.map((i) => ({ ...i, usageWarning: warning })) } });
    render(<MemoryRouter initialEntries={["/test-resources?tab=maas"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    const title = await screen.findByText(/Set up without the usage option the MaaS docs require/);
    const table = screen.getByRole("grid", { name: "This tool's models in MaaS" });
    expect(table).toContainElement(title);
    expect(table).toHaveTextContent(warning);
  });
});
