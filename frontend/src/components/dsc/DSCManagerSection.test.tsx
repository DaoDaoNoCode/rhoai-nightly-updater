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
  comparison: { defaultsRead: true, schemaRead: true, defaultsCSV: "rhods-operator.3.6.0", crdComponents: ["aigateway", "dashboard", "ray"], defaultComponents: ["aigateway", "dashboard", "ray"], dscComponents: ["aigateway", "dashboard", "ray"] },
};

/** The same DSC with nothing that differs from the release default but a user choice (the live cluster). */
const matching: DSCManager = {
  ...manager,
  nodes: manager.nodes.map((n) => (n.path === "dashboard.standard" ? { ...n, specSet: true, spec: "Managed" } : n)),
  mismatches: [],
};

/** A DSC with every kind of problem: missing values, a field the CRD does not define, Managed under a Removed parent. */
const problems: DSCManager = {
  ...manager,
  nodes: [
    node("aigateway", { spec: "Removed", default: "Removed" }),
    node("aigateway.batchGateway", { spec: "Removed", default: "Removed" }),
    node("aigateway.modelsAsAService", { default: "Removed", changed: true }),
    node("dashboard", { toggle: false, allowed: undefined, specSet: false, spec: undefined, defaultSet: false, default: undefined }),
    node("dashboard.standard", { specSet: false, spec: undefined }),
    node("databaseservice", { specSet: false, spec: undefined, default: "Removed" }),
    node("ray"),
    { path: "ray.legacy", name: "legacy", parent: "ray", depth: 1, toggle: false, specSet: false, defaultSet: false, unknown: true },
    { path: "codeflare", name: "codeflare", parent: "", depth: 0, toggle: false, specSet: false, defaultSet: false, unknown: true },
  ],
  mismatches: [
    { kind: "unset", path: "dashboard.standard", default: "Managed", fixable: true, message: "Not set in the DataScienceCluster, so the operator treats it as Removed, but the release default is Managed. Fix sets it to Managed." },
    { kind: "unset", path: "databaseservice", default: "Removed", fixable: true, message: "Not set in the DataScienceCluster, so the operator treats it as Removed, which is also the release default. Fix sets it explicitly." },
    { kind: "parent-removed", path: "aigateway.modelsAsAService", parent: "aigateway", fixable: false, message: "Managed, but aigateway is Removed, so the operator deploys nothing for it. Set aigateway to Managed, or set this to Removed." },
    { kind: "unknown-field", path: "codeflare", fixable: true, message: "The installed CRD does not define this field, so the operator never sees it. Fix removes it." },
    { kind: "unknown-field", path: "ray.legacy", fixable: true, message: "The installed CRD does not define this field, so the operator never sees it. Fix removes it." },
  ],
  comparison: { defaultsRead: true, schemaRead: true, defaultsCSV: "rhods-operator.3.6.0", crdComponents: ["aigateway", "dashboard", "databaseservice", "ray"], defaultComponents: ["aigateway", "dashboard", "databaseservice", "ray"], dscComponents: ["aigateway", "codeflare", "dashboard", "ray"] },
};

/** The text of a row of the comparison summary. */
function summaryRow(term: string): string {
  const list = screen.getByLabelText("Comparison with the release default");
  const group = within(list).getByText(term, { selector: "dt *, dt" }).closest(".pf-v6-c-description-list__group");
  return group?.querySelector("dd")?.textContent ?? "";
}

/** The visible rows' component paths, by their first cell. */
function rowNames(table: HTMLElement): string[] {
  return within(table).getAllByRole("row").slice(1).map((r) => r.querySelector("code")?.textContent ?? "");
}

/** A button that is off: disabled, or aria-disabled (a TooltipButton with a reason). */
const isOff = (el: HTMLElement) => el.hasAttribute("disabled") || el.getAttribute("aria-disabled") === "true";

/** Chooses a value in a row's state picker (PF Select): opens it, clicks the option. */
async function choose(name: string, value: string) {
  fireEvent.click(screen.getByRole("button", { name }));
  fireEvent.click(await screen.findByRole("option", { name: value }));
}

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
    expect(summaryRow("Missing")).toBe("1: dashboard.standard (default Managed)");
    expect(screen.getByRole("button", { name: "Fix" })).toBeEnabled();
  });

  describe("comparison summary", () => {
    it("says what was compared and that nothing is missing when nothing differs", async () => {
      stubApi({ "GET /api/components/dsc": matching });
      renderSection();
      expect(await screen.findByRole("heading", { name: /Nothing to fix compared with the release default/ })).toBeInTheDocument();
      expect(summaryRow("Release default")).toBe("Operator 3.6.0: the DataScienceCluster example in CSV rhods-operator.3.6.0 (alm-examples)");
      expect(summaryRow("Components")).toBe("All 3 components of the release default are in the DSC");
      expect(summaryRow("Missing")).toBe("None");
      expect(summaryRow("Not in the installed CRD")).toBe("None");
      expect(summaryRow("Deploys nothing")).toBe("None");
      expect(summaryRow("Changed from the default")).toBe("1: aigateway");
      expect(screen.queryByRole("button", { name: "Fix" })).not.toBeInTheDocument();
    });

    it("counts each kind of problem with its paths and keeps Fix", async () => {
      stubApi({ "GET /api/components/dsc": problems });
      renderSection();
      expect(await screen.findByRole("heading", { name: /5 differences from the release default to fix/ })).toBeInTheDocument();
      expect(summaryRow("Components")).toBe("3 of 4 components of the release default are in the DSC; not in it: databaseservice");
      expect(summaryRow("Missing")).toBe("2: dashboard.standard (default Managed), databaseservice (default Removed)");
      expect(summaryRow("Not in the installed CRD")).toBe("2: codeflare, ray.legacy");
      expect(summaryRow("Deploys nothing")).toBe("1: aigateway.modelsAsAService (parent aigateway)");
      expect(summaryRow("Changed from the default")).toBe("1: aigateway.modelsAsAService");
      expect(screen.getByRole("button", { name: "Fix" })).toBeEnabled();
    });

    it("says there is no release default instead of claiming nothing is missing", async () => {
      const defaultsError = "The installed operator 3.6.0 ships no DataScienceCluster v3 example in its CSV's alm-examples, so there is no release default to compare with.";
      stubApi({ "GET /api/components/dsc": {
        ...matching, defaultsError,
        nodes: matching.nodes.map((n) => ({ ...n, changed: false, defaultSet: false, default: undefined })),
        comparison: { ...matching.comparison!, defaultsRead: false, defaultsCSV: undefined, defaultComponents: [] },
      } });
      renderSection();
      expect(await screen.findByRole("heading", { name: /Not compared with a release default/ })).toBeInTheDocument();
      expect(summaryRow("Release default")).toBe(defaultsError);
      expect(summaryRow("Components")).toBe("3 in the DSC; the installed CRD defines 3");
      expect(summaryRow("Missing")).toBe("Not checked: no release default");
      expect(summaryRow("Changed from the default")).toBe("Not checked: no release default");
      expect(summaryRow("Not in the installed CRD")).toBe("None");
      expect(screen.queryByRole("button", { name: "Fix" })).not.toBeInTheDocument();
      expect(isOff(screen.getByRole("button", { name: "Reset to the release default…" }))).toBe(true);
    });

    it("explains the comparison and deprecated fields in a popover", async () => {
      stubApi({ "GET /api/components/dsc": matching });
      renderSection();
      fireEvent.click(await screen.findByRole("button", { name: "About this comparison" }));
      const help = await screen.findByRole("dialog", { name: "About this comparison" });
      expect(within(help).getByText(/marks only whole API versions as deprecated/)).toBeInTheDocument();
      expect(within(help).getByText(/prunes fields the CRD does not define/)).toBeInTheDocument();
      expect(within(help).getByText(/conversion webhook/)).toBeInTheDocument();
    });
  });

  describe("row labels", () => {
    it("labels each difference in its row, with an explanation and what Fix does", async () => {
      stubApi({ "GET /api/components/dsc": problems });
      renderSection();
      const table = await screen.findByRole("treegrid");
      const row = (path: string) => within(table).getAllByRole("row").find((r) => r.querySelector("code")?.textContent === path)!;
      expect(within(row("standard")).getByRole("button", { name: "Missing — default Managed" })).toBeInTheDocument();
      expect(within(row("databaseservice")).getByRole("button", { name: "Missing — default Removed" })).toBeInTheDocument();
      expect(within(row("modelsAsAService")).getByRole("button", { name: "Deploys nothing — parent Removed" })).toBeInTheDocument();
      expect(within(row("modelsAsAService")).getByRole("button", { name: "Changed from the default" })).toBeInTheDocument();
      expect(within(row("codeflare")).getByRole("button", { name: "Not in the installed CRD" })).toBeInTheDocument();
      expect(within(row("legacy")).getByRole("button", { name: "Not in the installed CRD" })).toBeInTheDocument();
      // A field the CRD does not define has no control.
      expect(within(row("codeflare")).queryByRole("button", { name: /^Set / })).not.toBeInTheDocument();

      fireEvent.click(within(row("standard")).getByRole("button", { name: "Missing — default Managed" }));
      const missing = await screen.findByRole("dialog", { name: "Missing in the DSC" });
      expect(within(missing).getByText(/Fix sets it to Managed/)).toBeInTheDocument();
      fireEvent.click(within(row("codeflare")).getByRole("button", { name: "Not in the installed CRD" }));
      const unknown = await screen.findByRole("dialog", { name: "Not in the installed CRD" });
      expect(within(unknown).getByText(/Fix removes it/)).toBeInTheDocument();
      fireEvent.click(within(row("modelsAsAService")).getByRole("button", { name: "Deploys nothing — parent Removed" }));
      const parent = await screen.findByRole("dialog", { name: "Deploys nothing" });
      expect(within(parent).getByText(/Fix does not change this/)).toBeInTheDocument();
    });
  });

  describe("show only differences", () => {
    it("keeps the rows that differ and their parents", async () => {
      stubApi({ "GET /api/components/dsc": problems });
      renderSection();
      const table = await screen.findByRole("treegrid");
      expect(rowNames(table)).toEqual(["aigateway", "batchGateway", "modelsAsAService", "dashboard", "standard", "databaseservice", "ray", "legacy", "codeflare"]);
      fireEvent.click(screen.getByRole("switch", { name: "Show only differences" }));
      // aigateway and ray match the release default, but stay as the
      // parents of rows that differ; batchGateway matches and goes.
      expect(rowNames(screen.getByRole("treegrid"))).toEqual(["aigateway", "modelsAsAService", "dashboard", "standard", "databaseservice", "ray", "legacy", "codeflare"]);
      // The row it leaves out stays in the table, collapsed, so the columns keep their widths.
      const gone = within(screen.getByRole("treegrid")).getAllByRole("row", { hidden: true }).find((r) => r.querySelector("code")?.textContent === "batchGateway")!;
      expect(gone).toHaveAttribute("aria-hidden", "true");
      expect(gone).toHaveStyle({ visibility: "collapse" });
    });

    it("fits narrow cards: a fixed layout, and names that wrap between their words", async () => {
      stubApi({ "GET /api/components/dsc": problems });
      renderSection();
      const table = await screen.findByRole("treegrid");
      // The widths follow the table's width only: content (an operator message, the filter) never moves a column.
      expect(table).toHaveStyle({ tableLayout: "fixed" });
      const name = [...table.querySelectorAll("code")].find((c) => c.textContent === "modelsAsAService")!;
      expect(name.innerHTML).toBe("models<wbr>As<wbr>AService");
      expect(name).toHaveStyle({ overflowWrap: "anywhere" });
    });

    it("hides rows that match the release default", async () => {
      stubApi({ "GET /api/components/dsc": manager });
      renderSection();
      await screen.findByRole("treegrid");
      fireEvent.click(screen.getByRole("switch", { name: "Show only differences" }));
      expect(rowNames(screen.getByRole("treegrid"))).toEqual(["aigateway", "dashboard", "standard"]);
      fireEvent.click(screen.getByRole("switch", { name: "Show only differences" }));
      expect(rowNames(screen.getByRole("treegrid"))).toEqual(["aigateway", "batchGateway", "dashboard", "standard", "ray"]);
    });

    it("says so when nothing differs", async () => {
      stubApi({ "GET /api/components/dsc": { ...matching, nodes: matching.nodes.map((n) => ({ ...n, changed: false })) } });
      renderSection();
      await screen.findByRole("treegrid");
      fireEvent.click(screen.getByRole("switch", { name: "Show only differences" }));
      expect(screen.getByRole("heading", { name: "Nothing differs from the release default" })).toBeInTheDocument();
      expect(screen.queryByRole("treegrid")).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole("button", { name: "Show all components" }));
      expect(screen.getByRole("treegrid")).toBeInTheDocument();
    });

    it("does not claim nothing differs without a release default", async () => {
      stubApi({ "GET /api/components/dsc": {
        ...matching, defaultsError: "No example.",
        nodes: matching.nodes.map((n) => ({ ...n, changed: false, defaultSet: false, default: undefined })),
        comparison: { ...matching.comparison!, defaultsRead: false, defaultComponents: [] },
      } });
      renderSection();
      await screen.findByRole("treegrid");
      expect(screen.getByText("No release default to compare with; no row has a problem the CRD shows")).toBeInTheDocument();
      expect(within(screen.getByRole("treegrid")).getAllByText("Unknown")).toHaveLength(4);
      expect(within(screen.getByRole("treegrid")).queryByText("Not set")).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole("switch", { name: "Show only differences" }));
      expect(screen.getByRole("heading", { name: "No problems found" })).toBeInTheDocument();
      expect(screen.queryByText("Nothing differs from the release default")).not.toBeInTheDocument();
    });
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
    await choose("Set ray", "Removed");
    const dialog = await screen.findByRole("dialog", { name: "Set ray to Removed?" });
    expect(api.calls).toContain("GET /api/components/dsc/plan?action=toggle&path=ray&to=Removed");
    expect(await within(dialog).findByText(/Managed → Removed \(removes it\)/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set to Removed" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/management"]).toHaveLength(1));
    expect(api.bodies["POST /api/components/dsc/management"][0]).toEqual({ name: "default-dsc", path: "ray", from: "Managed", to: "Removed", withParent: false, uid: "u-1", token: "0123456789abcdef01234567" });
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(await screen.findByText("Set ray Managed → Removed.")).toBeInTheDocument();
  });

  it("picks a row's state from a PatternFly menu: the DSC's value marked, the same value does nothing, Escape closes", async () => {
    const api = stubApi({ "GET /api/components/dsc": manager });
    renderSection();
    await screen.findByRole("treegrid");
    const toggle = screen.getByRole("button", { name: "Set ray" });
    expect(toggle).toHaveAccessibleDescription("Managed");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const list = await screen.findByRole("listbox");
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual(["Managed", "Removed"]);
    expect(within(list).getByRole("option", { name: "Managed" })).toHaveAttribute("aria-selected", "true");
    expect(within(list).getByRole("option", { name: "Removed" })).toHaveAttribute("aria-selected", "false");
    // The value it already has: the menu closes, nothing is planned.
    fireEvent.click(within(list).getByRole("option", { name: "Managed" }));
    await waitFor(() => expect(screen.queryByRole("listbox")).not.toBeInTheDocument());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("GET /api/components/dsc/plan"))).toEqual([]);
    expect(toggle).toHaveFocus();
    // Escape closes the menu and leaves the focus on the toggle.
    fireEvent.click(toggle);
    await screen.findByRole("listbox");
    fireEvent.keyDown(screen.getByRole("option", { name: "Removed" }), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).not.toBeInTheDocument());
    expect(toggle).toHaveFocus();
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    // Open, the arrow keys go into the menu, not to the next row of the tree table.
    fireEvent.click(toggle);
    await screen.findByRole("listbox");
    fireEvent.keyDown(toggle, { key: "ArrowDown" });
    expect(screen.getByRole("option", { name: "Managed" })).toHaveFocus();
    toggle.focus();
    fireEvent.keyDown(toggle, { key: "ArrowUp" });
    expect(screen.getByRole("option", { name: "Removed" })).toHaveFocus();
  });

  it("shows a value the DSC does not set on the toggle, with nothing marked", async () => {
    stubApi({ "GET /api/components/dsc": manager });
    renderSection();
    await screen.findByRole("treegrid");
    const toggle = screen.getByRole("button", { name: "Set dashboard.standard" });
    expect(toggle).toHaveTextContent("Not set (Removed)");
    fireEvent.click(toggle);
    const list = await screen.findByRole("listbox");
    expect(within(list).getAllByRole("option").every((o) => o.getAttribute("aria-selected") === "false")).toBe(true);
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
    await choose("Set aigateway.batchGateway", "Managed");
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
    await choose("Set ray", "Removed");
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
    expect(screen.getByRole("button", { name: "Set ray" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Set ray" }));
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
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
    expect(await within(dialog).findByText(/tears down every enabled component/)).toBeInTheDocument();
    expect(within(dialog).getByText(/^Stay Removed/)).toBeInTheDocument();
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
    fireEvent.click(await within(dialog).findByRole("button", { name: "Changes in the new DataScienceCluster (2)" }));
    const list = await within(dialog).findByRole("list", { name: "Differences of the new DataScienceCluster" });
    expect(within(list).getByText(/Managed → Removed \(removes it\)/)).toBeInTheDocument();
    expect(within(list).getByText(/team → not set/)).toBeInTheDocument();
  });

  it("labels an unset state with the CRD default it takes", async () => {
    stubApi({ "GET /api/components/dsc": { ...manager, nodes: [...manager.nodes, node("ray.argo", { specSet: false, spec: undefined, defaultSet: false, default: undefined, schemaDefault: "Managed" })] } });
    renderSection();
    const table = await screen.findByRole("treegrid");
    expect(within(table).getByRole("button", { name: "Set ray.argo" })).toHaveTextContent("Not set (Managed)");
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
      // Blocked: why first, what it would do on demand.
      fireEvent.click(within(dialog).getByRole("button", { name: "What a soft reset would do" }));
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
        stopsWorking: [{ resource: "published model proj/qwen25-05b-instruct", components: ["aigateway"] }, { resource: "every MaaS API key", components: ["aigateway"] }] });
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
      fireEvent.click(within(dialog).getByRole("button", { name: "Stops working until its component is back (2)" }));
      expect(within(dialog).getByText(/published model proj\/qwen25-05b-instruct/)).toBeInTheDocument();
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
    await choose("Set aigateway", "Removed");
    const dialog = await screen.findByRole("dialog", { name: "Set aigateway to Removed?" });
    expect(await within(dialog).findByText(/published model proj\/qwen25-05b-instruct/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Remove MaaS on the MaaS tab first/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Set to Removed" })).toBeDisabled();
  });
});
