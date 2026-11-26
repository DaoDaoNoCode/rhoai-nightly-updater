import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { UserProjectView } from "./UserProjectView";
import { projectStatus, stubUserApi } from "../../test/userFixtures";
import { contract } from "../../test/contract";
import { jsonResponse } from "../../test/apiStub";
import type { CatalogModel, ModelExposures, ModelState, PlaygroundStatus, ProjectCapabilities } from "../../types";

// The B5+B6 live fix round: a model whose address KServe reports without
// the port (the playground cannot use it), the in-cluster address users
// see, the pipeline server's wording before the storage exists, the
// project's confirmations, the deploy dialog's image, and re-reading a
// part until it settles.

const NO_PORT = "KServe currently reports its address without the port, so RHOAI's Gen AI playground cannot use it.";
const SMOL_URL = "https://smollm2-135m-instruct-kserve-workload-svc.team-a.svc.cluster.local:8000";

const smol: ModelState = {
  deployed: true, ready: true, managedByTool: true, namespace: "team-a", name: "smollm2-135m-instruct",
  displayName: "SmolLM2 135M Instruct (CPU)", message: "Running", internalUrl: SMOL_URL,
};

function playgroundWithoutPort(project: string): PlaygroundStatus {
  return {
    project, operatorReady: "True", genAiUi: "available", items: [],
    models: [{ name: "smollm2-135m-instruct", source: "namespace", status: "Running", ready: true, chat: true, unusable: NO_PORT }],
  };
}

afterEach(() => { vi.useRealTimers(); });

describe("a model the playground cannot use", () => {
  it("shows its in-cluster address, and the Playground card says why and what to ask for, Set up disabled", async () => {
    stubUserApi({
      "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), models: [smol] }),
      "/api/resources/playground": () => jsonResponse(playgroundWithoutPort("team-a")),
    });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const models = await screen.findByRole("region", { name: "Models" });
    expect(await within(models).findByText(SMOL_URL)).toBeInTheDocument();
    expect(within(models).getByText("(inside the cluster)")).toBeInTheDocument();

    const pg = await screen.findByRole("region", { name: "Playground" });
    expect(await within(pg).findByText(`No playground in team-a. smollm2-135m-instruct: ${NO_PORT} Ask a cluster-admin to expose a model through the inference gateway.`)).toBeInTheDocument();
    expect(within(pg).queryByText(/Models as a Service/)).not.toBeInTheDocument();
    const setUp = within(pg).getByRole("button", { name: "Set up playground" });
    // Disabled with the reason, once the capabilities are in.
    await waitFor(() => expect(setUp).toHaveAttribute("aria-disabled", "true"));
  });
});

describe("the project's own wording", () => {
  it("a delete's confirmation names the project it changes", async () => {
    stubUserApi({ "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), models: [smol] }) });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const models = await screen.findByRole("region", { name: "Models" });
    const del = await within(models).findByRole("button", { name: "Delete" });
    await waitFor(() => expect(del).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(del);
    const dialog = await screen.findByRole("dialog", { name: /Delete SmolLM2/ });
    expect(within(dialog).getByText("This changes project team-a:")).toBeInTheDocument();
    expect(within(dialog).queryByText("This changes the shared cluster:")).not.toBeInTheDocument();
  });

  it("before the S3 storage exists, an editor is told an admin of the project sets it up", async () => {
    const caps: ProjectCapabilities = contract.capabilitiesUser("team-a");
    caps.actions["project-s3.setup"] = { ...caps.actions["project-s3.setup"], allowed: false, code: "missing_permissions", reason: "Only an admin of project team-a can set up the project's S3 storage; ask one of them." };
    stubUserApi({
      "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), pipelineServers: [] }),
      "/api/resources/project-s3": () => jsonResponse({ ...contract.projectStorageUser("team-a"), deployed: false, ready: false }),
      "/api/resources/projects/capabilities": () => jsonResponse(caps),
    });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const pipelines = await screen.findByRole("region", { name: "Pipelines" });
    expect(await within(pipelines).findByText(/which an admin of project team-a sets up: ask one of them\./)).toBeInTheDocument();
  });

  it("the S3 storage says who its network policy lets in", async () => {
    stubUserApi();
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const s3 = await screen.findByRole("region", { name: "S3 storage" });
    expect(await within(s3).findByText(/RHOAI's applications namespace \(the pipelines operator checks the storage from there\); it has no address outside the cluster\./)).toBeInTheDocument();
    expect(within(s3).queryByText(/only\./)).not.toBeInTheDocument();
  });
});

const CATALOG: CatalogModel[] = [
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://y", cpu: "4", memoryRequest: "6Gi", memory: "8Gi", type: "chat", useCase: "Chat", description: "", args: "", downloadSize: "1.0 GB", default: true },
];

function exposures(over: Partial<ModelExposures>): ModelExposures {
  return {
    project: "team-a", model: "qwen25-05b-instruct", default: "internal", authReady: false, capacity: "fits",
    options: [{ key: "internal", exposure: "internal", title: "Only inside the cluster", description: "No outside address.", available: true }],
    ...over,
  };
}

describe("the deploy dialog's image", () => {
  async function open(over: Partial<ModelExposures>) {
    const api = stubUserApi({
      "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), modelCatalog: CATALOG, models: [] }),
      "/api/resources/models/exposures": () => jsonResponse(exposures(over)),
    });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const models = await screen.findByRole("region", { name: "Models" });
    const deploy = await within(models).findByRole("button", { name: "Deploy a model" });
    await waitFor(() => expect(deploy).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(deploy);
    return { api, dialog: await screen.findByRole("dialog", { name: "Deploy a model in team-a" }) };
  }

  it("says the released image runs on a nightly cluster, without the checkbox", async () => {
    const { dialog } = await open({ imagePlan: "released", imagePlanReason: "This cluster runs a nightly RHOAI build." });
    expect(await within(dialog).findByText("Runtime image: the newest released Red Hat vLLM CPU image, pinned by digest. This cluster runs a nightly RHOAI build.")).toBeInTheDocument();
    expect(within(dialog).queryByRole("checkbox", { name: "Use the newest released Red Hat image" })).not.toBeInTheDocument();
  });

  it("shows no plan for a model already in the project: setting it up again keeps its image", async () => {
    const api = stubUserApi({
      "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), modelCatalog: CATALOG, models: [{ ...smol, name: "qwen25-05b-instruct" }] }),
      "/api/resources/models/exposures": () => jsonResponse(exposures({ imagePlan: "released", imagePlanReason: "This cluster's preset image is not published yet." })),
    });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const models = await screen.findByRole("region", { name: "Models" });
    const deploy = await within(models).findByRole("button", { name: "Deploy a model" });
    await waitFor(() => expect(deploy).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(deploy);
    const dialog = await screen.findByRole("dialog", { name: "Deploy a model in team-a" });
    await within(dialog).findByText(/Only inside the cluster/);
    expect(within(dialog).queryByText(/Runtime image:/)).not.toBeInTheDocument();
    expect(api.calls.some((c) => c.includes("/api/resources/models/exposures"))).toBe(true);
  });

  it("offers the released image while the plan is the preset's", async () => {
    const { dialog } = await open({ imagePlan: "preset" });
    expect(await within(dialog).findByText("Runtime image: the preset's own Red Hat vLLM CPU image.")).toBeInTheDocument();
    expect(within(dialog).getByRole("checkbox", { name: "Use the newest released Red Hat image" })).toBeInTheDocument();
  });
});

describe("re-reading a part until it settles", () => {
  const statusReads = (api: { calls: string[] }) => api.calls.filter((c) => c.startsWith("GET /api/resources/status")).length;

  it("reads a settled part only at the slow idle pace", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const api = stubUserApi({ "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), models: [smol] }) });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    await screen.findByRole("region", { name: "Models" });
    await waitFor(() => expect(statusReads(api)).toBe(1));
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(statusReads(api)).toBe(1);
    await act(() => vi.advanceTimersByTimeAsync(16_000));
    await waitFor(() => expect(statusReads(api)).toBe(2));
  });

  it("stops checking quickly after 10 minutes and says so, with Check again", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const starting: ModelState = { ...smol, ready: false, message: "Starting" };
    const api = stubUserApi({ "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), models: [starting] }) });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    await screen.findByRole("region", { name: "Models" });
    for (let i = 0; i < 25; i++) await act(() => vi.advanceTimersByTimeAsync(30_000));
    const note = await screen.findByText("Stopped checking automatically after 10 minutes");
    const before = statusReads(api);
    fireEvent.click(within(note.closest(".pf-v6-c-alert") as HTMLElement).getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(statusReads(api)).toBeGreaterThan(before));
  });

  it("reads the status again while a model starts, without Refresh", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const starting: ModelState = { ...smol, ready: false, message: "Starting" };
    const api = stubUserApi({ "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), models: [starting] }) });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    await screen.findByRole("region", { name: "Models" });
    const reads = () => api.calls.filter((c) => c.startsWith("GET /api/resources/status")).length;
    await waitFor(() => expect(reads()).toBe(1));
    await act(() => vi.advanceTimersByTimeAsync(6_000));
    await waitFor(() => expect(reads()).toBe(2));
  });
});
