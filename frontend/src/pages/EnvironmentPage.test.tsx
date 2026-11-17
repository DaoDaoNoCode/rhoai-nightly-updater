import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { EnvironmentPage } from "./EnvironmentPage";
import type { ResourcesStatus, ResourceState } from "../types";
import { jsonResponse, stubApi } from "../test/apiStub";

vi.mock("../state/AppInfo", () => ({
  useMutationBlocker: () => null,
  useClusterBusyHandler: () => () => {},
  usePermissions: () => ({ reason: null }),
}));
vi.mock("../components/GlobalBanners", () => ({ GlobalBanners: () => null }));

const notDeployed: ResourceState = { deployed: false, ready: false, message: "Not deployed" };
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiRoute: "https://seaweedfs-admin.apps.example.com" };

function renderPage(path: string) {
  const router = createMemoryRouter([{ path: "/test-resources", element: <EnvironmentPage /> }], { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Environment page", () => {
  it("is titled Environment, opens on Overview with the goals, and lists only goals this release can set up", async () => {
    stubApi({ "/api/resources/status": { minio: notDeployed, mlflow: notDeployed, pipelineServers: [] }, "/api/resources/projects": { projects: [] } });
    renderPage("/test-resources");
    expect(screen.getByRole("heading", { level: 1, name: "Environment" })).toBeInTheDocument();
    expect(document.title).toBe("Environment — RHOAI Nightly Updater");
    expect(screen.getByText("One-click setups for developing and reproducing issues on this cluster.")).toBeInTheDocument();
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Overview", "Cluster", "Projects"]);
    expect(screen.getByRole("tab", { name: "Overview" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("heading", { level: 2, name: "What do you want to do?" })).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(["Run a data science pipeline", "Track experiments with MLflow"]);
  });

  it("keeps the tab and the project in the URL, so links open them and back and forward work", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [] }, "/api/resources/projects": { projects: ["dev-project", "llm-eval"] } });
    const router = renderPage("/test-resources?tab=cluster");
    expect(screen.getByRole("tab", { name: "Cluster" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("region", { name: "Storage" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Projects" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=projects"));
    expect(await screen.findByText("Choose a project")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
    fireEvent.click(await screen.findByRole("option", { name: "llm-eval" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=projects&project=llm-eval"));
    expect(await screen.findByRole("region", { name: "Pipeline server" })).toBeInTheDocument();

    await act(() => router.navigate(-1));
    expect(await screen.findByText("Choose a project")).toBeInTheDocument();
    await act(() => router.navigate(-1));
    expect(screen.getByRole("tab", { name: "Cluster" })).toHaveAttribute("aria-selected", "true");
    await act(() => router.navigate(1));
    expect(screen.getByRole("tab", { name: "Projects" })).toHaveAttribute("aria-selected", "true");
  });

  it("the pipeline goal asks for a project on the card, then sets up only what is missing, in order", async () => {
    let status: ResourcesStatus = { minio: notDeployed, mlflow: notDeployed, pipelineServers: [] };
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev-project"] },
      "POST /api/resources/minio/setup": () => { status = { ...status, minio: readyMinio }; return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] }); },
      "POST /api/resources/pipeline-server/setup": () => jsonResponse({ success: true, message: "Pipeline server created.", logs: [] }),
    });
    const router = renderPage("/test-resources");
    const goal = await screen.findByRole("region", { name: "Run a data science pipeline" });
    expect(goal).toHaveTextContent("0 of 2 ready");
    const setUp = within(goal).getByRole("button", { name: "Set up" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(within(goal).getByRole("button", { name: "Show projects" }));
    fireEvent.click(await screen.findByRole("option", { name: "dev-project" }));
    await waitFor(() => expect(router.state.location.search).toBe("?project=dev-project"));
    expect(goal).toHaveTextContent("Project dev-project: Not set up");
    fireEvent.click(within(goal).getByRole("button", { name: "Set up" }));
    const confirm = await screen.findByRole("dialog");
    expect(confirm).toHaveTextContent("Run a data science pipeline: set up 2 parts?");
    expect(confirm).toHaveTextContent("Deployment seaweedfs");
    expect(confirm).toHaveTextContent("DataSciencePipelinesApplication nightly-dspa");
    fireEvent.click(within(confirm).getByRole("button", { name: "Set up" }));
    const progress = await screen.findByRole("dialog", { name: "Run a data science pipeline" });
    expect(await within(progress).findByText("All 2 parts are set up.")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources")).map((c) => c.split(" ")[1])).toEqual(["/api/resources/minio/setup", "/api/resources/pipeline-server/setup"]);
    expect(api.bodies["POST /api/resources/pipeline-server/setup"]).toEqual([{ project: "dev-project" }]);
  });

  it("a goal whose step needs attention points to it instead of setting up", async () => {
    stubApi({
      "/api/resources/status": { minio: { deployed: true, ready: false, managedByTool: true, terminalError: true, waitingReason: "ImagePullBackOff", message: "ImagePullBackOff" }, mlflow: notDeployed, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev-project"] },
    });
    renderPage("/test-resources?project=dev-project");
    const goal = await screen.findByRole("region", { name: "Run a data science pipeline" });
    expect(within(goal).queryByRole("button", { name: /^Set up/ })).not.toBeInTheDocument();
    fireEvent.click(within(goal).getByRole("button", { name: "Show the storage" }));
    expect(await screen.findByRole("heading", { level: 2, name: "Storage" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Cluster" })).toHaveAttribute("aria-selected", "true");
  });

  it("the details drawer takes the focus, closes with Escape and gives the focus back", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [] }, "/api/resources/projects": { projects: [] } });
    renderPage("/test-resources?tab=cluster");
    const opener = within(await screen.findByRole("region", { name: "Storage" })).getByRole("button", { name: "Storage details" });
    opener.focus();
    fireEvent.click(opener);
    const title = await screen.findByRole("heading", { level: 2, name: "Storage" });
    await waitFor(() => expect(title).toHaveFocus());
    expect(opener).toHaveAttribute("aria-expanded", "true");
    fireEvent.keyDown(title, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("heading", { level: 2, name: "Storage" })).not.toBeInTheDocument());
    expect(opener).toHaveFocus();
  });
});
