import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ComponentsPage } from "./ComponentsPage";
import { stubApi, jsonResponse } from "../test/apiStub";
import { renderPage as renderInApp } from "../test/providers";
import type { ComponentsResponse, DeploymentInfo } from "../types";

function dep(name: string, over: Partial<DeploymentInfo> = {}): DeploymentInfo {
  return {
    name, namespace: "redhat-ods-applications", ready: 1, desired: 1, available: 1,
    image: `registry.redhat.io/rhoai/odh-${name}-rhel9@sha256:${"a".repeat(64)}`,
    unavailableReplicas: 0, updatedReplicas: 1, rolloutStuck: false,
    pods: [{ name: `${name}-abc`, namespace: "redhat-ods-applications", phase: "Running", node: "n", ready: true, restarts: 0, image: "", imageID: "", age: "2d" }],
    ...over,
  };
}

function present(deployments: DeploymentInfo[]): ComponentsResponse {
  return {
    components: [{ name: "dashboard", managementState: "Managed", status: "Available" }],
    deployments, dscName: "default-dsc", dscPhase: "Ready", changedCount: 0, dscExists: true, dscState: "present",
  };
}

function renderPage() {
  return renderInApp(<ComponentsPage />);
}


describe("ComponentsPage component causes", () => {
  it("shows the classified cause with a link to Diagnostics", async () => {
    const resp = present([dep("agent-ops-ui")]);
    resp.components = [{ name: "trainer", managementState: "Managed", status: "Error", message: "dependency not met: JobSet Operator is not installed.", cause: "Prerequisite operator not installed: JobSet Operator" }];
    stubApi({ "/api/components": resp });
    renderPage();
    expect(await screen.findByText(/Cause: Prerequisite operator not installed: JobSet Operator/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Diagnostics" })).toHaveAttribute("href", "/diagnostics");
  });
});

describe("ComponentsPage DSC states (A05-8, A08-6)", () => {
  it("no-dsc shows the create flow with the operator's defaults, not 'not installed'", async () => {
    const api = stubApi({
      "/api/components": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded" },
      "/api/setup/dsc/preview": { yaml: "kind: DataScienceCluster\nmetadata:\n  name: default-dsc", operatorVersion: "3.6.0", branch: "rhoai-3.6", sourceURL: "", source: "csv", sourceDescription: "alm-examples of the installed CSV rhods-operator.3.6.0" },
      "POST /api/setup/dsc": { success: true, message: "DataScienceCluster default-dsc created", logs: [] },
    });
    renderPage();
    expect(await screen.findByRole("heading", { name: "No DataScienceCluster yet" })).toBeInTheDocument();
    expect(screen.getByText(/RHOAI operator 3.6.0 is installed \(Succeeded\)/)).toBeInTheDocument();
    expect(screen.queryByText(/not installed/i)).not.toBeInTheDocument();
    await screen.findByRole("button", { name: "Preview and create DataScienceCluster" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Preview and create DataScienceCluster" })).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("button", { name: "Preview and create DataScienceCluster" }));
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/kind: DataScienceCluster/)).toBeInTheDocument();
    expect(within(dialog).getByText(/alm-examples of the installed CSV/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await screen.findByText("DataScienceCluster default-dsc created")).toBeInTheDocument();
    expect(api.calls.filter((c) => c === "POST /api/setup/dsc")).toHaveLength(1);
  });

  it("no-crd says RHOAI is not installed and links to the Dashboard", async () => {
    stubApi({ "/api/components": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-crd" } });
    renderPage();
    expect(await screen.findByRole("heading", { name: "RHOAI is not installed" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /install RHOAI/ })).toHaveAttribute("href", "/");
  });

  it.each([
    [403, "forbidden", "Access denied"],
    [502, "cluster_unavailable", "The cluster API returned an error"],
    [504, "timeout", "The request timed out"],
    [503, "rate_limited", "The cluster API is throttling requests"],
    [500, "internal", "Can't load the components"],
  ])("HTTP %i %s is an error with Retry, never an empty state", async (status, errorCode, title) => {
    stubApi({ "/api/components": () => jsonResponse({ error: "Could not read components: boom", errorCode }, status) });
    renderPage();
    expect(await screen.findByText(new RegExp(`^${title}`))).toBeInTheDocument();
    // The raw error is one click away (UX-Global-9).
    fireEvent.click(screen.getByRole("button", { name: "Show the error" }));
    expect(screen.getByText(/Could not read components: boom/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText(/not installed/i)).not.toBeInTheDocument();
  });
});

describe("ComponentsPage Deployments table (A08-14, A08-11)", () => {
  const deployments = [
    dep("agent-ops-ui"),
    dep("odh-observability", { ready: 0, rolloutStuck: true, pods: [{ name: "odh-observability-1", namespace: "redhat-ods-applications", phase: "Pending", node: "n", ready: false, restarts: 0, image: "", imageID: "", age: "2d", containers: [{ name: "manager", ready: false, restarts: 0, state: "waiting", reason: "ContainerCreating" }] }] }),
    dep("gen-ai-ui", { buildDate: "2026-10-01T00:00:00Z" }),
  ];

  function rowNames(): string[] {
    const table = screen.getByRole("grid", { name: "Deployments" });
    return within(table).getAllByRole("row").slice(1)
      .map((r) => r.querySelector('td[data-label="Name"] span')?.textContent ?? "")
      .filter(Boolean);
  }

  it("shows problems first with their cause, and filters by status and text", async () => {
    stubApi({ "/api/components": present(deployments) });
    renderPage();
    await screen.findByRole("grid", { name: "Deployments" });
    expect(rowNames()).toEqual(["odh-observability", "agent-ops-ui", "gen-ai-ui"]);
    expect(screen.getByText("ContainerCreating for 2d")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Filter deployments by status: All/ }));
    fireEvent.click(screen.getByRole("option", { name: /Needs attention\s*1/ }));
    expect(screen.getByRole("button", { name: /Filter deployments by status: Needs attention/ })).toBeInTheDocument();
    expect(rowNames()).toEqual(["odh-observability"]);

    fireEvent.click(screen.getByRole("button", { name: /Filter deployments by status/ }));
    fireEvent.click(screen.getByRole("option", { name: /All\s*3/ }));
    fireEvent.change(screen.getByRole("textbox", { name: /Filter deployments/ }), { target: { value: "gen-ai" } });
    expect(rowNames()).toEqual(["gen-ai-ui"]);
    expect(screen.getByText("1 of 3")).toBeInTheDocument();

    fireEvent.change(screen.getByRole("textbox", { name: /Filter deployments/ }), { target: { value: "nothing-matches" } });
    expect(screen.getByText("No deployments match the filters")).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "Clear filters" })[0]);
    expect(rowNames()).toHaveLength(3);
  });

  it("sorts by name when the Name header is clicked", async () => {
    stubApi({ "/api/components": present(deployments) });
    renderPage();
    await screen.findByRole("grid", { name: "Deployments" });
    fireEvent.click(screen.getByRole("button", { name: "Name" }));
    expect(rowNames()).toEqual(["agent-ops-ui", "gen-ai-ui", "odh-observability"]);
    fireEvent.click(screen.getByRole("button", { name: "Name" }));
    expect(rowNames()).toEqual(["odh-observability", "gen-ai-ui", "agent-ops-ui"]);
  });

  it("an expanded row shows the full image and a keyboard-reachable copy button", async () => {
    stubApi({ "/api/components": present([dep("agent-ops-ui")]) });
    renderPage();
    await screen.findByRole("grid", { name: "Deployments" });
    fireEvent.click(screen.getByRole("button", { name: /agent-ops-ui/ }));
    const image = deployments[0].image;
    expect(screen.getByRole("button", { name: `Copy image reference ${image}` }).closest(".pf-v6-c-clipboard-copy")).toHaveTextContent(image.slice(0, 20));
    expect(screen.getByRole("button", { name: `Copy image reference ${image}` })).toBeInTheDocument();
  });

  it("Unblock rollout names the Deployment and sends it; nothing to do is info, not an error", async () => {
    const stuck = dep("rhods-dashboard", {
      ready: 1, desired: 1,
      pods: [
        { name: "old", namespace: "redhat-ods-applications", phase: "Running", node: "n", ready: true, restarts: 0, image: "", imageID: "", age: "2d", podTemplateHash: "a" },
        { name: "new", namespace: "redhat-ods-applications", phase: "Pending", node: "", ready: false, restarts: 0, image: "", imageID: "", age: "1m", podTemplateHash: "b", schedulingReason: "Unschedulable", schedulingMessage: "0/2 nodes are available: 2 Insufficient cpu." },
      ],
    });
    const api = stubApi({
      "/api/components": present([stuck]),
      "POST /api/assist-rollout": () => jsonResponse({ success: false, message: "Nothing to do: the rollout is no longer blocked.", logs: [], errorCode: "nothing_to_do" }, 422),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Actions for rhods-dashboard" }));
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Unblock rollout" })).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("menuitem", { name: "Unblock rollout" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Deployment redhat-ods-applications/rhods-dashboard")).toBeInTheDocument();
    expect(within(dialog).getByText("spec.strategy.rollingUpdate.maxUnavailable")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Unblock rollout" }));
    expect(await screen.findByText("Nothing to do")).toBeInTheDocument();
    expect(api.bodies["POST /api/assist-rollout"]).toEqual([{ namespace: "redhat-ods-applications", deployment: "rhods-dashboard" }]);
  });

  it("read-only users cannot open the unblock dialog", async () => {
    const stuck = dep("x", { pods: [
      { name: "old", namespace: "redhat-ods-applications", phase: "Running", node: "n", ready: true, restarts: 0, image: "", imageID: "", age: "2d", podTemplateHash: "a" },
      { name: "new", namespace: "redhat-ods-applications", phase: "Pending", node: "", ready: false, restarts: 0, image: "", imageID: "", age: "1m", podTemplateHash: "b", schedulingReason: "Unschedulable" },
    ] });
    stubApi({ "/api/components": present([stuck]), "/api/user/permissions": { canMutate: false, user: "viewer" } });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Actions for x" }));
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Unblock rollout" })).toHaveAttribute("aria-disabled", "true"));
    await new Promise((r) => setTimeout(r, 50));
    fireEvent.click(screen.getByRole("menuitem", { name: "Unblock rollout" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

describe("ComponentsPage and the app-wide lock (R4b c, N1, N7)", () => {
  const busy = async () => ({ inProgress: true, operation: { id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } });

  it("a teammate's running operation disables Create DSC with the reason", async () => {
    stubApi({ "/api/components": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded" } });
    renderInApp(<ComponentsPage />, "/", { operation: busy });
    const create = await screen.findByRole("button", { name: "Preview and create DataScienceCluster" });
    await waitFor(() => expect(create).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(create);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/alice is running "Update to nightly"/);
  });

  it("a DSC repair refused with a 422 OperationResponse keeps its errorCode and logs, and re-fetches", async () => {
    const api = stubApi({
      "/api/components": { ...present([dep("dashboard")]), dscCompatibility: { operatorVersion: "3.6.0", branch: "rhoai-3.6", invalidFields: ["spec.components.foo"], missingComponents: [] } },
      "POST /api/components/dsc/repair": () => jsonResponse({ success: false, errorCode: "nothing_to_do", message: "No invalid fields left.", logs: ["checked spec.components.foo"] }, 422),
    });
    renderInApp(<ComponentsPage />);
    const remove = await screen.findByRole("button", { name: "Remove invalid fields" });
    await waitFor(() => expect(remove).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(remove);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Confirm" }));
    expect(await screen.findByText("Nothing to do")).toBeInTheDocument();
    expect(screen.getByText("No invalid fields left.").closest(".pf-v6-c-alert")).toHaveClass("pf-m-info");
    expect(screen.getByText("checked spec.components.foo")).toBeInTheDocument();
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("GET /api/components")).length).toBeGreaterThan(1));
  });
});

describe("ComponentsPage DSC repair preview (FXA R5-F1)", () => {
  const block = { component: "mcplifecycleoperator", reasons: ["CRD(s) mcpservers.mcp.x-k8s.io convert their objects through a webhook Service of mcplifecycleoperator."] };
  const compat = {
    operatorVersion: "3.6.0", branch: "rhoai-3.6", invalidFields: [], missingComponents: [], extraComponents: ["legacycomponent"],
    resetRemovals: ["mcplifecycleoperator", "trainer"],
  };

  it("lists what a reset sets to Removed before the user confirms", async () => {
    const api = stubApi({
      "/api/components": { ...present([dep("dashboard")]), dscCompatibility: { ...compat, removalBlocks: null } },
      "/api/setup/dsc/preview": { yaml: "kind: DataScienceCluster", apiVersion: "datasciencecluster.opendatahub.io/v3", operatorVersion: "3.6.0", branch: "rhoai-3.6", sourceURL: "" },
      "POST /api/components/dsc/repair": { success: true, message: "DSC reset. Components set to Removed: mcplifecycleoperator, trainer", logs: [] },
    });
    renderInApp(<ComponentsPage />);
    const reset = await screen.findByRole("button", { name: "Reset to version defaults" });
    await waitFor(() => expect(reset).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(reset);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("2 enabled components will be set to Removed")).toBeInTheDocument();
    expect(within(dialog).getByText("trainer")).toBeInTheDocument();
    await within(dialog).findByText(/kind: DataScienceCluster/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/components/dsc/repair"))).toHaveLength(1));
    // Bound to the defaults the user reviewed (HIGH 2): the backend refuses if the DSC's version changed.
    expect(api.bodies["POST /api/components/dsc/repair"][0]).toMatchObject({
      mode: "reset-defaults", expectedOperatorVersion: "3.6.0", expectedAPIVersion: "datasciencecluster.opendatahub.io/v3",
    });
  });

  it("remove-invalid sends the API version the page compared", async () => {
    const api = stubApi({
      "/api/components": {
        ...present([dep("dashboard")]), dscAPIVersion: "datasciencecluster.opendatahub.io/v2",
        dscCompatibility: { ...compat, extraComponents: [], resetRemovals: [], invalidFields: ["spec.components.dashboard.oldField"] },
      },
      "POST /api/components/dsc/repair": { success: true, message: "Removed invalid DSC fields: spec.components.dashboard.oldField", logs: [] },
    });
    renderInApp(<ComponentsPage />);
    const remove = await screen.findByRole("button", { name: "Remove invalid fields" });
    await waitFor(() => expect(remove).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(remove);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(api.bodies["POST /api/components/dsc/repair"]).toHaveLength(1));
    expect(api.bodies["POST /api/components/dsc/repair"][0]).toMatchObject({ mode: "remove-invalid", expectedAPIVersion: "datasciencecluster.opendatahub.io/v2" });
  });

  it("a blocked reset shows each blocked component and why, and offers no Confirm", async () => {
    const api = stubApi({
      "/api/components": { ...present([dep("dashboard")]), dscCompatibility: { ...compat, removalBlocks: [block] } },
      "/api/setup/dsc/preview": { yaml: "kind: DataScienceCluster", operatorVersion: "3.6.0", branch: "rhoai-3.6", sourceURL: "" },
    });
    renderInApp(<ComponentsPage />);
    expect(await screen.findByText(/Reset to version defaults is not possible now: it would remove mcplifecycleoperator/)).toBeInTheDocument();
    const reset = screen.getByRole("button", { name: "Reset to version defaults" });
    await waitFor(() => expect(reset).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(reset);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("This repair is not possible now")).toBeInTheDocument();
    expect(within(dialog).getByText(/refused as a whole/)).toBeInTheDocument();
    expect(within(dialog).getByText(/mcpservers.mcp.x-k8s.io convert their objects/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Confirm" })).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getAllByRole("button", { name: "Close" }).slice(-1)[0]);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(api.calls.filter((c) => c.startsWith("POST"))).toEqual([]);
  });

  it("remove-extra is blocked only by its own components; a reset block does not block it", async () => {
    stubApi({
      "/api/components": { ...present([dep("dashboard")]), dscCompatibility: { ...compat, removalBlocks: [block] } },
    });
    renderInApp(<ComponentsPage />);
    const extra = await screen.findByRole("button", { name: "Remove extra components" });
    await waitFor(() => expect(extra).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(extra);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("button", { name: "Confirm" })).toBeInTheDocument();
    expect(within(dialog).queryByText("This repair is not possible now")).not.toBeInTheDocument();
  });

  it("remove-extra with a blocked extra component offers no Confirm", async () => {
    stubApi({
      "/api/components": { ...present([dep("dashboard")]), dscCompatibility: { ...compat, removalBlocks: [{ component: "legacycomponent", reasons: ["Its operator has no ready pod."] }] } },
    });
    renderInApp(<ComponentsPage />);
    expect(await screen.findByText(/Remove extra components is not possible now: legacycomponent/)).toBeInTheDocument();
    const extra = screen.getByRole("button", { name: "Remove extra components" });
    await waitFor(() => expect(extra).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(extra);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Its operator has no ready pod/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Confirm" })).not.toBeInTheDocument();
  });
});

describe("ComponentsPage DSC API versions", () => {
  const unreadableV3 = (): ComponentsResponse => ({
    ...present([dep("agent-ops-ui")]),
    consoleURL: "https://console.example.com",
    components: [
      { name: "aiHub", managementState: "Managed", status: "Available" },
      { name: "someFutureComponent", managementState: "Unknown", status: "Unknown" },
    ],
    dscAPIVersion: "datasciencecluster.opendatahub.io/v2",
    dscVersionFallback: { version: "v3", used: "v2", message: "conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed" },
    dscCompatibility: {
      invalidFields: [], missingComponents: [], extraComponents: [],
      defaultsError: "This DataScienceCluster can only be read as v2: reading it as v3 fails because the operator's conversion webhook cannot convert it. Operator 3.6.0 has no DataScienceCluster v2 defaults; it ships them as v3 only.",
    },
  });

  it("says the DSC is shown at an older version and why, with the defaults message instead of a URL", async () => {
    stubApi({ "/api/components": unreadableV3() });
    renderPage();
    expect(await screen.findByText("The DataScienceCluster is shown as v2: the operator cannot convert it to v3")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Diagnostics" })).toHaveAttribute("href", "/diagnostics");
    expect(screen.getByText(/it ships them as v3 only/)).toBeInTheDocument();
    expect(screen.queryByText(/raw\.githubusercontent|HTTP 404/)).not.toBeInTheDocument();
    // No drift card: nothing was compared across versions.
    expect(screen.queryByText(/differs from operator/)).not.toBeInTheDocument();
  });

  it("links the console to the version the DSC was read at, and shows unknown component keys as they are", async () => {
    stubApi({ "/api/components": unreadableV3() });
    renderPage();
    const edit = await screen.findByRole("link", { name: "Edit default-dsc in the OpenShift console" });
    expect(edit).toHaveAttribute("href", "https://console.example.com/k8s/cluster/datasciencecluster.opendatahub.io~v2~DataScienceCluster/default-dsc/yaml");
    expect(screen.getByText("someFutureComponent")).toBeInTheDocument();
  });

  it("uses v3 in the console link when the DSC is read as v3", async () => {
    stubApi({ "/api/components": { ...unreadableV3(), dscAPIVersion: "datasciencecluster.opendatahub.io/v3", dscVersionFallback: undefined, dscCompatibility: undefined } });
    renderPage();
    const edit = await screen.findByRole("link", { name: "Edit default-dsc in the OpenShift console" });
    expect(edit.getAttribute("href")).toContain("datasciencecluster.opendatahub.io~v3~DataScienceCluster");
    expect(screen.queryByText(/is shown as/)).not.toBeInTheDocument();
  });
});
