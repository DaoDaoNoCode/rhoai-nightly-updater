import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import type { FeaturePrerequisites, ResourceState, ResourcesStatus } from "../../types";
import { prerequisitesConfirm, prerequisitesScript, prerequisitesSteps, withPrerequisites } from "./Prerequisites";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], inferenceGateway: { ...none }, models: { catalog: [] } } as unknown as ResourcesStatus;
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

const models: FeaturePrerequisites = {
  ready: false,
  parts: [
    { id: "dsc-kserve", title: "Model serving (KServe) component (kserve)", state: "missing", message: "Removed in DataScienceCluster default-dsc. Set up sets it to Managed; Clean up can turn it back.", commands: "oc patch datasciencecluster default-dsc --type=merge -p '{}'" },
    { id: "cert-manager", title: "cert-manager Operator for Red Hat OpenShift", state: "missing", message: "Not installed. Set up installs version 1.20.1 from redhat-operators (channel stable-v1)." },
    { id: "rhcl", title: "Red Hat Connectivity Link", state: "missing", message: "Not installed." },
    { id: "kuadrant", title: "Kuadrant CR (Connectivity Link's policies)", state: "missing", message: "Not there." },
    { id: "dependencies", title: "KServe's operator check", state: "ready", message: "KServe finds every operator it needs" },
  ],
  setupPlan: ["DataScienceCluster default-dsc: set kserve to Managed (it is Removed; Clean up can turn it back while nothing else uses it)", "Install cert-manager Operator for Red Hat OpenShift 1.20.1"],
};
const ready: FeaturePrerequisites = { ready: true, parts: [{ id: "dsc-aipipelines", title: "Pipelines component (aipipelines)", state: "ready", message: "Managed in DataScienceCluster default-dsc" }] };

afterEach(() => vi.unstubAllGlobals());

describe("one-click prerequisites", () => {
  it("names the streamed steps from the parts, operators and the Kuadrant CR included", () => {
    const st = { features: { models } };
    expect(prerequisitesSteps({ kind: "setup-prerequisites", feature: "models", exposure: "default-gateway" }, st).map((s) => s.id)).toEqual(["prerequisites", "dsc", "cert-manager", "rhcl", "kuadrant", "ready"]);
  });

  it("the Overview's next step sets them up while the feature is not set up", () => {
    const state = { status: "not-set-up" as const, label: "Not set up in any project", next: { kind: "choose-project" as const }, project: null };
    // Models: planned for the recommended way to reach a model (Connectivity Link included).
    expect(withPrerequisites(state, models, "models")).toMatchObject({ label: "Needs cluster setup", next: { kind: "set-up", label: "Set up what Models needs", setup: [{ kind: "setup-prerequisites", feature: "models", exposure: "default-gateway", recommended: true }], reason: null } });
    expect(withPrerequisites(state, ready, "pipelines")).toBe(state);
    // A feature that reads ready while its prerequisites are not needs attention.
    const readyState = { ...state, status: "ready" as const };
    expect(withPrerequisites(readyState, models, "models")).toMatchObject({ status: "needs-attention", next: { kind: "set-up", label: "Set up what Models needs" } });
  });

  it("the confirmation shows the plan open and says Connectivity Link is for the recommended gateway", () => {
    const st = { features: { models }, modelsExposure: "default-gateway" };
    const spec = prerequisitesConfirm({ kind: "setup-prerequisites", feature: "models", exposure: "default-gateway", recommended: true }, st)!;
    expect(spec.confirm).toBe("Install and turn on");
    expect(spec.changesOpen).toBe(true);
    expect(spec.confirmDisabled).toBe(false);
    render(<>{spec.summary}</>);
    expect(screen.getByText("Operators it installs stay installed after Clean up, because other features share them.")).toBeInTheDocument();
    expect(screen.getByText(/Includes Red Hat Connectivity Link, for the recommended inference gateway\./)).toBeInTheDocument();
    // An answer planned for another way to reach the model: Confirm waits.
    const other = prerequisitesConfirm({ kind: "setup-prerequisites", feature: "models", exposure: "internal" }, st)!;
    expect(other.confirmDisabled).toBe(true);
  });

  it("every missing part's commands are one ordered script", () => {
    const script = prerequisitesScript([
      { id: "dsc-kserve", title: "Model serving (KServe)", state: "missing", commands: "oc patch a" },
      { id: "cert-manager", title: "cert-manager", state: "ready" },
      { id: "kuadrant", title: "Connectivity Link instance (Kuadrant)", state: "missing", commands: "oc apply b" },
    ]);
    expect(script).toBe("# Model serving (KServe)\noc patch a\n\n# Connectivity Link instance (Kuadrant)\noc apply b");
  });

  it("a tab lists what is missing with the CLI alternative, confirms the plan and streams the setup", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: { pipelines: ready, mlflow: ready, models, playground: ready } },
      "/api/resources/models/exposures": { project: "dev", model: "qwen25-05b-instruct", default: "internal", options: [{ key: "internal", exposure: "internal", title: "Only inside the cluster", description: "No route.", available: true }], authReady: false },
      "POST /api/resources/prerequisites/stream": sse({ step: "prerequisites", status: "success", message: "Checked.", elapsedMs: 1 }, { step: "operation_complete", status: "success", message: "What Models needs on the cluster is set up.", elapsedMs: 2 }),
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=models&project=dev"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByText("Needed on the cluster first")).toBeInTheDocument();
    expect(screen.getByRole("listitem", { name: "cert-manager Operator for Red Hat OpenShift" })).toBeInTheDocument();
    expect(screen.getAllByText("Do it from the CLI instead").length).toBeGreaterThan(0);
    // The plan follows the chosen way to reach the model.
    expect(await screen.findByRole("radio", { name: /Only inside the cluster/ })).toBeChecked();
    await vi.waitFor(() => expect(api.calls).toContain("GET /api/resources/prerequisites?exposure=internal"));
    fireEvent.click(screen.getByRole("button", { name: "Set up what Models needs" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up what Models needs on the cluster\?/ });
    expect(within(dialog).getByText(/set kserve to Managed/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Install and turn on" }));
    expect(await screen.findByText("What Models needs on the cluster is set up.")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/prerequisites/stream"]).toEqual([{ feature: "models", exposure: "internal" }]);
    // The plan was read for the chosen way to reach the model.
    expect(api.calls.some((c) => c === "GET /api/resources/prerequisites?exposure=internal")).toBe(true);
  });

  it("ready prerequisites collapse to one line", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: { pipelines: ready, mlflow: ready, models: ready, playground: ready } },
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=pipelines&project=dev"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByText("Cluster prerequisites for Pipelines: 1 of 1 ready")).toBeInTheDocument();
  });

  it("an answer for another way to reach the model is not shown, and Set up waits for the right one", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      // The backend echoes the exposure it planned for: here never "internal".
      "/api/resources/prerequisites": { features: { pipelines: ready, mlflow: ready, models, playground: ready }, modelsExposure: "default-gateway" },
      "/api/resources/models/exposures": { project: "dev", model: "qwen25-05b-instruct", default: "internal", options: [{ key: "internal", exposure: "internal", title: "Only inside the cluster", description: "No route.", available: true }], authReady: false },
    });
    render(<MemoryRouter initialEntries={["/test-resources?tab=models&project=dev"]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
    expect(await screen.findByRole("radio", { name: /Only inside the cluster/ })).toBeChecked();
    await vi.waitFor(() => expect(screen.queryByText("Needed on the cluster first")).not.toBeInTheDocument());
    expect(screen.getByText("Checking what this way to reach the model needs on the cluster...")).toBeInTheDocument();
  });
});
