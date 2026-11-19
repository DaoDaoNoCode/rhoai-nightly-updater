import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { DSCManagerSection } from "./DSCManagerSection";
import { stubApi } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { DSCManager, DSCNode, DSCPlan } from "../../types";

function node(path: string, over: Partial<DSCNode> = {}): DSCNode {
  const parts = path.split(".");
  return {
    path, name: parts[parts.length - 1], parent: parts.slice(0, -1).join("."), depth: parts.length - 1,
    toggle: true, allowed: ["Managed", "Removed"], specSet: true, spec: "Managed", defaultSet: true, default: "Managed", ...over,
  };
}

const manager: DSCManager = {
  exists: true, state: "present", name: "default-dsc", uid: "u-1", apiVersion: "datasciencecluster.opendatahub.io/v3", storageVersion: "v3",
  dscCount: 1, operatorVersion: "3.6.0", conditions: [],
  nodes: [
    node("aigateway", { default: "Removed", changed: true }),
    node("aigateway.batchGateway", { spec: "Removed", default: "Removed" }),
    node("dashboard", { toggle: false, allowed: undefined, specSet: false, spec: undefined, defaultSet: false, default: undefined, actual: "Managed" }),
    node("dashboard.standard", { specSet: false, spec: undefined }),
    node("ray", { condition: { type: "RayReady", status: "True" } }),
  ],
  mismatches: [{ kind: "unset", path: "dashboard.standard", default: "Managed", fixable: true, message: "Not set in the DataScienceCluster, so the operator treats it as Removed, but the release default is Managed. Fix sets it to Managed." }],
};

/** A button that is off: disabled, or aria-disabled (a TooltipButton with a reason). */
const isOff = (el: HTMLElement) => el.hasAttribute("disabled") || el.getAttribute("aria-disabled") === "true";

const plan = (over: Partial<DSCPlan>): DSCPlan => ({ action: "toggle", name: "default-dsc", uid: "u-1", changes: [], token: "0123456789abcdef01234567", ...over });

function renderSection(onChanged = vi.fn(), mutateReason: string | null = null) {
  render(<DSCManagerSection refreshKey={1} mutateReason={mutateReason} onResult={() => {}} onChanged={onChanged} />);
  return onChanged;
}

describe("DSCManagerSection", () => {
  it("shows the tree from the CRD, user choices and the real differences", async () => {
    stubApi({ "GET /api/components/dsc": manager });
    renderSection();
    const table = await screen.findByRole("treegrid", { name: "Components of the DataScienceCluster" });
    const rows = within(table).getAllByRole("row");
    expect(rows.map((r) => r.getAttribute("aria-level")).filter(Boolean)).toEqual(["1", "2", "1", "2", "1"]);
    expect(within(table).getByText("Changed from the default")).toBeInTheDocument();
    expect(within(table).getByText("Not set (Removed)")).toBeInTheDocument();
    expect(within(table).getByText("Group")).toBeInTheDocument();
    const diffs = screen.getByRole("list", { name: "Differences from the release default" });
    expect(within(diffs).getByText("dashboard.standard")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Fix" })).toBeEnabled();
  });

  it("collapses a component's sub-components", async () => {
    stubApi({ "GET /api/components/dsc": manager });
    renderSection();
    const table = await screen.findByRole("treegrid");
    expect(within(table).getByText("batchGateway")).toBeInTheDocument();
    fireEvent.click(within(table).getAllByRole("button", { name: /collapse|expand/i })[0]);
    expect(within(table).queryByText("batchGateway")).not.toBeInTheDocument();
  });

  it("previews a toggle, then sends only that field with the value seen", async () => {
    const api = stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": plan({ changes: [{ path: "ray", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" }] }),
      "POST /api/components/dsc/management": { success: true, message: "Set ray Managed → Removed.", logs: [] },
    });
    const onChanged = renderSection();
    await screen.findByRole("treegrid");
    fireEvent.change(screen.getByRole("combobox", { name: "Set ray" }), { target: { value: "Removed" } });
    const dialog = await screen.findByRole("dialog", { name: "Set ray to Removed?" });
    expect(api.calls).toContain("GET /api/components/dsc/plan?action=toggle&path=ray&to=Removed");
    expect(await within(dialog).findByText(/Managed → Removed \(removes it\)/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set to Removed" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/management"]).toHaveLength(1));
    expect(api.bodies["POST /api/components/dsc/management"][0]).toEqual({ name: "default-dsc", path: "ray", from: "Managed", to: "Removed", withParent: false, uid: "u-1", token: "0123456789abcdef01234567" });
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(await screen.findByText("Set ray Managed → Removed.")).toBeInTheDocument();
  });

  it("offers to set a parent that is not Managed in the same change", async () => {
    const api = stubApi({
      "GET /api/components/dsc": { ...manager, nodes: manager.nodes.map((n) => (n.path === "aigateway" ? { ...n, spec: "Removed" } : n)) },
      "GET /api/components/dsc/plan": (url: string) => new Response(JSON.stringify(url.includes("withParent=true")
        ? plan({ changes: [{ path: "aigateway", from: "Removed", fromSet: true, to: "Managed", toSet: true, toggle: true, effect: "deploys" }] })
        : plan({ refusal: "aigateway is Removed, so setting aigateway.batchGateway to Managed would deploy nothing.", code: "parent_removed", parentPath: "aigateway" })), { headers: { "Content-Type": "application/json" } }),
      "POST /api/components/dsc/management": { success: true, message: "ok", logs: [] },
    });
    renderSection();
    await screen.findByRole("treegrid");
    fireEvent.change(screen.getByRole("combobox", { name: "Set aigateway.batchGateway" }), { target: { value: "Managed" } });
    let dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/would deploy nothing/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Set to Managed" })).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Also set aigateway to Managed" }));
    dialog = await screen.findByRole("dialog", { name: "Set aigateway.batchGateway and aigateway to Managed?" });
    await within(dialog).findByText(/Removed → Managed \(deploys it\)/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Set to Managed" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/management"]).toHaveLength(1));
    expect(api.bodies["POST /api/components/dsc/management"][0]).toMatchObject({ path: "aigateway.batchGateway", withParent: true });
  });

  it("does not confirm a change whose removal is blocked", async () => {
    stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": plan({ changes: [{ path: "ray", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" }], blocked: [{ component: "ray", reasons: ["rhods-operator must be running"] }] }),
    });
    renderSection();
    await screen.findByRole("treegrid");
    fireEvent.change(screen.getByRole("combobox", { name: "Set ray" }), { target: { value: "Removed" } });
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/rhods-operator must be running/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Set to Removed" })).toBeDisabled();
  });

  it("fixes exactly the previewed plan", async () => {
    const fix = plan({ action: "fix", changes: [{ path: "dashboard.standard", fromSet: false, to: "Managed", toSet: true, toggle: true, effect: "deploys" }] });
    const api = stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": fix,
      "POST /api/components/dsc/fix": { success: true, message: "Changed.", logs: [] },
    });
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Fix" }));
    const dialog = await screen.findByRole("dialog", { name: "Fix the differences from the release default?" });
    expect(await within(dialog).findByText(/not set → Managed \(deploys it\)/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Fix" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/fix"]).toEqual([{ name: "default-dsc", uid: "u-1", token: fix.token }]));
  });

  it("turns every control off with the reason when the DSC is read-only", async () => {
    stubApi({ "GET /api/components/dsc": { ...manager, readOnlyReason: "DataScienceCluster default-dsc is managed by Argo CD application \"rhoai\"." } });
    renderSection();
    expect(await screen.findByText(/managed by Argo CD application/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Set ray" })).toBeDisabled();
    expect(isOff(screen.getByRole("button", { name: "Fix" }))).toBe(true);
    expect(isOff(screen.getByRole("button", { name: "Reset to the release default…" }))).toBe(true);
  });

  it("hard reset needs the typed name and streams its steps", async () => {
    const hard = plan({ action: "hard-reset", tornDown: ["aigateway", "ray"], stayRemoved: ["aigateway"], newObject: "kind: DataScienceCluster\n", changes: [], keepEnabled: true });
    const stream = sseResponse();
    const api = stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": (url: string) => new Response(JSON.stringify(url.includes("hard-reset") ? hard : plan({ action: "soft-reset", changes: [] })), { headers: { "Content-Type": "application/json" } }),
      "POST /api/components/dsc/hard-reset/stream": () => stream.response,
    });
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "Reset to the release default…" }));
    const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
    fireEvent.click(within(dialog).getByLabelText("Hard reset"));
    expect(await within(dialog).findByText(/Every component is torn down/)).toBeInTheDocument();
    expect(within(dialog).getByText(/so they stay Removed/)).toBeInTheDocument();
    const confirm = within(dialog).getByRole("button", { name: "Hard reset" });
    expect(isOff(confirm)).toBe(true);
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "default-dsc" } });
    await waitFor(() => expect(isOff(confirm)).toBe(false));
    fireEvent.click(confirm);
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/hard-reset/stream"]).toEqual([{ name: "default-dsc", uid: "u-1", token: hard.token, confirm: "default-dsc", keepEnabled: true }]));
    stream.push(sseEvent("check", "success", "ok"));
    stream.push(sseEvent("delete", "running", "Deleting"));
    expect(await within(dialog).findByText("Deleting")).toBeInTheDocument();
    stream.push(sseEvent("operation_complete", "success", "The DataScienceCluster was reset to the release default and is Ready."));
    stream.close();
    expect(await within(dialog).findByText("The DataScienceCluster was reset to the release default and is Ready.")).toBeInTheDocument();
  });

  it("hard reset lists what the new DataScienceCluster changes", async () => {
    const hard = plan({ action: "hard-reset", tornDown: ["ray"], newObject: "kind: DataScienceCluster\n", changes: [
      { path: "aigateway", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" },
      { path: "kueue.defaultLocalQueueName", from: "team", fromSet: true, toSet: false, toggle: false },
    ] });
    stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": (url: string) => new Response(JSON.stringify(url.includes("hard-reset") ? hard : plan({ action: "soft-reset", changes: [] })), { headers: { "Content-Type": "application/json" } }),
    });
    renderSection();
    await screen.findByRole("treegrid");
    fireEvent.click(screen.getByRole("button", { name: "Reset to the release default…" }));
    const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
    fireEvent.click(within(dialog).getByLabelText("Hard reset"));
    const list = await within(dialog).findByRole("list", { name: "Differences of the new DataScienceCluster" });
    expect(within(list).getByText(/Managed → Removed \(removes it\)/)).toBeInTheDocument();
    expect(within(list).getByText(/team → not set/)).toBeInTheDocument();
  });

  it("labels an unset state with the CRD default it takes", async () => {
    stubApi({ "GET /api/components/dsc": { ...manager, nodes: [...manager.nodes, node("ray.argo", { specSet: false, spec: undefined, defaultSet: false, default: undefined, schemaDefault: "Managed" })] } });
    renderSection();
    const table = await screen.findByRole("treegrid");
    expect(within(table).getByRole("option", { name: "Not set (Managed)" })).toBeInTheDocument();
    expect(within(table).getByText("Not set (CRD default Managed)")).toBeInTheDocument();
  });

  it("offers to finish a hard reset that deleted the DataScienceCluster", async () => {
    const pending = { name: "my-dsc", apiVersion: "datasciencecluster.opendatahub.io/v3", newObject: "kind: DataScienceCluster\nmetadata:\n  name: my-dsc\n", operatorVersion: "3.6.0", user: "admin", startedAt: "2026-11-19T10:00:00Z", token: "abcdefabcdefabcdefabcdef" };
    const api = stubApi({
      "GET /api/components/dsc": { exists: false, state: "no-dsc", dscCount: 0, conditions: [], nodes: [], mismatches: [], pendingHardReset: pending },
      "POST /api/components/dsc/hard-reset/finish": { success: true, message: "Created DataScienceCluster my-dsc: the hard reset is finished.", logs: [] },
    });
    const onChanged = renderSection();
    expect(await screen.findByText("A hard reset did not finish")).toBeInTheDocument();
    expect(screen.getByText(/without the old name and namespaces/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Finish the hard reset…" }));
    const dialog = await screen.findByRole("dialog", { name: "Finish the hard reset?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create my-dsc" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/hard-reset/finish"]).toEqual([{ name: "my-dsc", token: pending.token }]));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("shows nothing without a DataScienceCluster or a hard reset to finish", async () => {
    const api = stubApi({ "GET /api/components/dsc": { exists: false, state: "no-dsc", dscCount: 0, conditions: [], nodes: [], mismatches: [] } });
    const { container } = render(<DSCManagerSection refreshKey={1} mutateReason={null} onResult={() => {}} onChanged={() => {}} />);
    await waitFor(() => expect(api.calls).toContain("GET /api/components/dsc"));
    expect(container).toBeEmptyDOMElement();
  });

  // The live scenario: MaaS set up and a model published; a reset keeps
  // aigateway and MaaS by default, and the plan without them is blocked.
  describe("keep the components you turned on", () => {
    const turnedOn = ["aigateway", "aigateway.modelsAsAService"];
    const maasReason = "MaaS is in use (published model proj/qwen25-05b-instruct; subscription models-as-a-service/rhoai-nightly-updater-subscription). Removing aigateway takes MaaS away: maas-api and the gateway's policies go, so every published model and every API key stops working. Remove MaaS on the MaaS tab first (Clean up MaaS unpublishes and removes it in order), or keep aigateway Managed";
    const keepPlan = plan({ action: "soft-reset", keepEnabled: true, turnedOn, kept: turnedOn, token: "aaaaaaaaaaaaaaaaaaaaaaaa",
      changes: [{ path: "kserve.rawDeploymentServiceConfig", from: "Headed", fromSet: true, toSet: false, toggle: false }] });
    const pureDefault = plan({ action: "soft-reset", turnedOn, token: "bbbbbbbbbbbbbbbbbbbbbbbb",
      changes: [
        { path: "aigateway", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" },
        { path: "aigateway.modelsAsAService", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" },
      ],
      blocked: [{ component: "aigateway", reasons: [maasReason] }] });
    const resetPlans = (url: string) => new Response(JSON.stringify(url.includes("keepEnabled=true") ? keepPlan : pureDefault), { headers: { "Content-Type": "application/json" } });

    it("is checked by default, lists them, and resets everything else", async () => {
      const api = stubApi({
        "GET /api/components/dsc": manager,
        "GET /api/components/dsc/plan": resetPlans,
        "POST /api/components/dsc/reset": { success: true, message: "Changed 1 field. Kept: aigateway, aigateway.modelsAsAService (you turned these on).", logs: [] },
      });
      renderSection();
      fireEvent.click(await screen.findByRole("button", { name: "Reset to the release default…" }));
      const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
      const keep = await within(dialog).findByRole("checkbox", { name: /Keep the components you turned on \(2\): aigateway, aigateway\.modelsAsAService/ });
      expect(keep).toBeChecked();
      expect(api.calls).toContain("GET /api/components/dsc/plan?action=soft-reset&keepEnabled=true");
      expect(await within(dialog).findByText(/\(you turned these on\)/)).toBeInTheDocument();
      const changes = within(dialog).getByRole("list", { name: "Changes of the reset" });
      expect(within(changes).queryByText("aigateway")).not.toBeInTheDocument();
      fireEvent.click(within(dialog).getByRole("button", { name: "Reset" }));
      await waitFor(() => expect(api.bodies["POST /api/components/dsc/reset"]).toEqual([{ name: "default-dsc", uid: "u-1", token: keepPlan.token, keepEnabled: true }]));
    });

    it("unchecked, plans the pure release default again and shows why it is blocked", async () => {
      const api = stubApi({ "GET /api/components/dsc": manager, "GET /api/components/dsc/plan": resetPlans });
      renderSection();
      fireEvent.click(await screen.findByRole("button", { name: "Reset to the release default…" }));
      const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
      await within(dialog).findByText(/\(you turned these on\)/);
      fireEvent.click(within(dialog).getByRole("checkbox", { name: /Keep the components you turned on/ }));
      expect(await within(dialog).findByText(/MaaS is in use/)).toBeInTheDocument();
      expect(api.calls).toContain("GET /api/components/dsc/plan?action=soft-reset&keepEnabled=false");
      const changes = within(dialog).getByRole("list", { name: "Changes of the reset" });
      expect(within(changes).getAllByText(/Managed → Removed \(removes it\)/)).toHaveLength(2);
      expect(within(dialog).getByText(/Clean up MaaS unpublishes and removes it in order/)).toBeInTheDocument();
      expect(within(dialog).getByText(/to reset everything else and keep these as they are/)).toBeInTheDocument();
      expect(within(dialog).getByRole("button", { name: "Reset" })).toBeDisabled();
      // Checked again: the plan that keeps them, and Reset is possible.
      fireEvent.click(within(dialog).getByRole("checkbox", { name: /Keep the components you turned on/ }));
      await waitFor(() => expect(within(dialog).queryByText(/MaaS is in use/)).not.toBeInTheDocument());
      await waitFor(() => expect(within(dialog).getByRole("button", { name: "Reset" })).toBeEnabled());
    });

    it("hard reset keeps them too and says so", async () => {
      const hardKeep = plan({ action: "hard-reset", keepEnabled: true, turnedOn, kept: turnedOn, tornDown: ["aigateway", "ray"], newObject: "kind: DataScienceCluster\n", changes: [],
        warnings: ["While aigateway is down, MaaS does not serve (published model proj/qwen25-05b-instruct): every published model and every API key stops working until the new DataScienceCluster brings it back."] });
      const hardPure = plan({ action: "hard-reset", turnedOn, tornDown: ["aigateway", "ray"], stayRemoved: turnedOn, newObject: "kind: DataScienceCluster\n", changes: [], blocked: [{ component: "aigateway", reasons: [maasReason] }] });
      const api = stubApi({
        "GET /api/components/dsc": manager,
        "GET /api/components/dsc/plan": (url: string) => new Response(JSON.stringify(!url.includes("hard-reset") ? keepPlan : url.includes("keepEnabled=true") ? hardKeep : hardPure), { headers: { "Content-Type": "application/json" } }),
      });
      renderSection();
      fireEvent.click(await screen.findByRole("button", { name: "Reset to the release default…" }));
      const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
      fireEvent.click(within(dialog).getByLabelText("Hard reset"));
      expect(await within(dialog).findByText(/They come back as they are now/)).toBeInTheDocument();
      expect(within(dialog).getByText(/While aigateway is down, MaaS does not serve/)).toBeInTheDocument();
      expect(api.calls).toContain("GET /api/components/dsc/plan?action=hard-reset&keepEnabled=true");
      fireEvent.click(within(dialog).getByRole("checkbox", { name: /Keep the components you turned on/ }));
      expect(await within(dialog).findByText(/MaaS is in use/)).toBeInTheDocument();
      expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
      expect(isOff(within(dialog).getByRole("button", { name: "Hard reset" }))).toBe(true);
    });

    it("has no option when nothing was turned on", async () => {
      stubApi({ "GET /api/components/dsc": manager, "GET /api/components/dsc/plan": plan({ action: "soft-reset", keepEnabled: true, changes: [] }) });
      renderSection();
      fireEvent.click(await screen.findByRole("button", { name: "Reset to the release default…" }));
      const dialog = await screen.findByRole("dialog", { name: "Reset to the release default" });
      await within(dialog).findByText("This changes the shared cluster:");
      expect(within(dialog).queryByRole("checkbox")).not.toBeInTheDocument();
    });
  });

  it("blocks turning aigateway off while MaaS is in use, with the way to remove MaaS", async () => {
    stubApi({
      "GET /api/components/dsc": manager,
      "GET /api/components/dsc/plan": plan({
        changes: [{ path: "aigateway", from: "Managed", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" }],
        blocked: [{ component: "aigateway", reasons: ["MaaS is in use (published model proj/qwen25-05b-instruct). Removing aigateway takes MaaS away: maas-api and the gateway's policies go, so every published model and every API key stops working. Remove MaaS on the MaaS tab first (Clean up MaaS unpublishes and removes it in order), or keep aigateway Managed"] }],
      }),
    });
    renderSection();
    await screen.findByRole("treegrid");
    fireEvent.change(screen.getByRole("combobox", { name: "Set aigateway" }), { target: { value: "Removed" } });
    const dialog = await screen.findByRole("dialog", { name: "Set aigateway to Removed?" });
    expect(await within(dialog).findByText(/published model proj\/qwen25-05b-instruct/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Remove MaaS on the MaaS tab first/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Set to Removed" })).toBeDisabled();
  });
});
