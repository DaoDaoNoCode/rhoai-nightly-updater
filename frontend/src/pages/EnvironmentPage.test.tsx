import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { EnvironmentPage } from "./EnvironmentPage";
import type { ResourcesStatus, ResourceState } from "../types";
import { jsonResponse, stubApi } from "../test/apiStub";
import { RESOURCE_POLL_BASE_MS } from "../constants";
import { READY_CHECK_MS } from "../components/environment/useGoalRun";

vi.mock("../state/AppInfo", () => ({
  useMutationBlocker: () => null,
  useClusterBusyHandler: () => () => {},
  usePermissions: () => ({ reason: null }),
}));
vi.mock("../state/AppState", () => ({ useClusterStatus: () => ({ status: null }) }));
vi.mock("../components/GlobalBanners", () => ({ GlobalBanners: () => null }));

const notDeployed: ResourceState = { deployed: false, ready: false, message: "Not deployed" };
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com" };
const readyDspa = (ns: string): ResourceState => ({ deployed: true, ready: true, managedByTool: true, namespace: ns, name: "dspa", message: "Running", uiRoute: `https://rh-ai.apps.example.com/develop-train/pipelines/definitions/${ns}` });
const foreignMlflow: ResourceState = { deployed: true, ready: true, managedByTool: false, namespace: "redhat-ods-applications", message: "Running", uiRoute: "https://mlflow.apps.example.com" };

function renderPage(path: string) {
  const router = createMemoryRouter([{ path: "/test-resources", element: <EnvironmentPage /> }], { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

/** The Overview row of a feature. */
const row = async (feature: string) => (await screen.findByRole("link", { name: feature })).closest("tr") as HTMLElement;

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Environment page", () => {
  it("opens on the Overview: a summary line and one row per feature with its state and next step", async () => {
    stubApi({ "/api/resources/status": { minio: notDeployed, mlflow: notDeployed, pipelineServers: [] }, "/api/resources/projects": { projects: [] }, "/api/resources/playground": { items: [] },
      "/api/resources/model-registry": { name: "nightly-registry", namespace: "rhoai-model-registries", component: "aiHub", state: "absent" },
      "/api/resources/maas": { platform: { deployed: false, ready: false, managedByTool: false }, parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }], subscription: "rhoai-nightly-updater-subscription" } });
    renderPage("/test-resources");
    expect(screen.getByRole("heading", { level: 1, name: "Environment" })).toBeInTheDocument();
    expect(document.title).toBe("Environment — RHOAI Nightly Updater");
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Overview", "S3 storage", "Pipelines", "MLflow", "Model registry", "Models", "Playground", "MaaS"]);
    expect(screen.getByRole("tab", { name: "Overview" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByText("0 of 7 ready")).toBeInTheDocument();
    const table = screen.getByRole("grid", { name: "Features" });
    expect(within(table).getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["S3 storage", "Pipelines", "MLflow", "Model registry", "Models", "Playground", "MaaS"]);
    // The S3 storage is its own row: the cluster's, set up on its own.
    expect(await row("S3 storage")).toHaveTextContent(/Not set up\s*Set up S3 storage/);
    expect(within(await row("S3 storage")).getByRole("link", { name: "S3 storage" })).toHaveAttribute("href", "/test-resources?tab=storage");
    expect(await row("MaaS")).toHaveTextContent(/Not set up\s*Set up MaaS/);
    expect(await row("Pipelines")).toHaveTextContent("Not set up in any project");
    expect(within(await row("Pipelines")).getByRole("button", { name: "Choose a project" })).toHaveClass("pf-m-inline");
    expect(within(await row("MLflow")).getByRole("button", { name: "Set up MLflow…" })).toBeInTheDocument();
    // No endpoints, logins or teardown on the Overview.
    expect(screen.getByRole("tabpanel")).not.toHaveTextContent(/seaweedfs|Password|Tear down/);
  });

  it("the live state: someone else's running MLflow is ready; an empty project first in the list is not the default", async () => {
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: foreignMlflow, pipelineServers: [readyDspa("juntao-test")] },
      "/api/resources/projects": { projects: ["a-test-project", "juntao-test", "zeta"] },
      "/api/resources/playground": { items: [] },
      "/api/resources/model-registry": { name: "nightly-registry", namespace: "rhoai-model-registries", component: "aiHub", state: "absent" },
    });
    const router = renderPage("/test-resources");
    expect(await screen.findByText("3 of 7 ready")).toBeInTheDocument();
    expect(within(await row("S3 storage")).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", readyMinio.uiRoute);
    const mlflow = await row("MLflow");
    // Ownership under the status label, in its own words.
    expect(mlflow.querySelector(".pf-v6-c-label")).toHaveTextContent(/^Ready$/);
    expect(within(mlflow).getByText("Not managed by this tool")).toBeInTheDocument();
    expect(within(mlflow).getByRole("link", { name: /Open MLflow experiments/ })).toHaveAttribute("href", "https://mlflow.apps.example.com");
    // One ready project: the State names it, the link does not repeat it; an inline link in the cell.
    expect(within(await row("Pipelines")).getByRole("link", { name: "Open pipelines" })).toHaveClass("pf-m-inline");
    const pipelines = await row("Pipelines");
    expect(pipelines).toHaveTextContent("Ready in juntao-test");
    expect(within(pipelines).getByRole("link", { name: "Pipelines" })).toHaveAttribute("href", "/test-resources?tab=pipelines&project=juntao-test");
    // The Pipelines tab without a project opens the one with a ready server, not a-test-project.
    fireEvent.click(screen.getByRole("tab", { name: "Pipelines" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=pipelines"));
    expect(await screen.findByText("Ready in juntao-test")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toHaveValue("juntao-test");
    // The MLflow tab says the same as its row: Ready, with ownership beside it.
    fireEvent.click(screen.getByRole("tab", { name: "MLflow" }));
    const header = (await screen.findByRole("heading", { name: "MLflow", level: 2 })).closest(".pf-v6-l-flex") as HTMLElement;
    expect(header.querySelector(".pf-v6-c-label")).toHaveTextContent(/^Ready$/);
    expect(header).toHaveTextContent("Not managed by this tool");
  });

  it("project-scoped features summarise across projects", async () => {
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [readyDspa("a"), readyDspa("b")] },
      "/api/resources/projects": { projects: ["a", "b", "c", "d", "e"] },
    });
    renderPage("/test-resources");
    expect(await row("Pipelines")).toHaveTextContent("Ready in 2 projects");
    // Which ones: a popover lists each with its state and a link to the tab there.
    fireEvent.click(within(await row("Pipelines")).getByRole("button", { name: "Show projects" }));
    const popover = await screen.findByRole("dialog", { name: "Pipelines in 2 projects" });
    expect(within(popover).getAllByRole("link").map((l) => [l.textContent, l.getAttribute("href")])).toEqual([["a", "/test-resources?tab=pipelines&project=a"], ["b", "/test-resources?tab=pipelines&project=b"]]);
    fireEvent.click(within(await row("Pipelines")).getByRole("button", { name: "Show projects" }));
    // Several ready projects: no jump into one of them; the tab's picker decides.
    expect(within(await row("Pipelines")).queryByRole("link", { name: /Open pipelines/ })).not.toBeInTheDocument();
    fireEvent.click(within(await row("Pipelines")).getByRole("button", { name: "Choose a project" }));
    expect(await screen.findByText("No project chosen")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Project" })).toHaveValue("");
  });

  it("keeps the tab and the project in the URL; back and forward work; old tabs open Pipelines", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [] }, "/api/resources/projects": { projects: ["dev-project", "llm-eval"] } });
    const router = renderPage("/test-resources?tab=cluster");
    await waitFor(() => expect(router.state.location.search).toBe("?tab=pipelines"));
    expect(screen.getByRole("tab", { name: "Pipelines" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByText(/Choose a project above/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
    fireEvent.click(await screen.findByRole("option", { name: "llm-eval" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=pipelines&project=llm-eval"));
    expect(await screen.findByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In project llm-eval");
    fireEvent.click(screen.getByRole("tab", { name: "MLflow" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=mlflow&project=llm-eval"));
    await act(() => router.navigate(-1));
    expect(screen.getByRole("tab", { name: "Pipelines" })).toHaveAttribute("aria-selected", "true");
    await act(() => router.navigate(-1));
    expect(await screen.findByText(/Choose a project above/)).toBeInTheDocument();
    await act(() => router.navigate(1));
    expect(await screen.findByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In project llm-eval");

    for (const old of ["projects"]) {
      const r = renderPage(`/test-resources?tab=${old}`);
      await waitFor(() => expect(r.state.location.search).toBe("?tab=pipelines"));
    }
  });

  // The picker and the content come from one state: a clear is kept in the
  // URL (an empty project=), so the default project does not come back.
  it("clearing the project shows the prompt in the picker and the content; reload keeps it; back restores the project", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [readyDspa("juntao-test")] }, "/api/resources/projects": { projects: ["a-test-project", "juntao-test"] } });
    const router = renderPage("/test-resources?tab=pipelines&project=juntao-test");
    expect(await screen.findByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In project juntao-test");
    expect(screen.getByRole("combobox")).toHaveValue("juntao-test");
    fireEvent.click(screen.getByRole("button", { name: "Clear the project filter" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=pipelines&project="));
    expect(await screen.findByText(/Choose a project above/)).toBeInTheDocument();
    expect(screen.getByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In the project you choose");
    expect(screen.getByRole("listitem", { name: /^S3 storage/ })).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toHaveValue("");
    // The header agrees: no project, not a summary naming one.
    expect(screen.getByText("No project chosen")).toBeInTheDocument();
    expect(screen.queryByText("Ready in juntao-test")).not.toBeInTheDocument();
    await act(() => router.navigate(-1));
    expect(await screen.findByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In project juntao-test");
    expect(screen.getByRole("combobox")).toHaveValue("juntao-test");
  });

  it("a cleared project stays cleared on reload; without project= the default applies", async () => {
    stubApi({ "/api/resources/status": { minio: readyMinio, mlflow: notDeployed, pipelineServers: [readyDspa("juntao-test")] }, "/api/resources/projects": { projects: ["a-test-project", "juntao-test"] } });
    renderPage("/test-resources?tab=pipelines&project=");
    expect(await screen.findByText(/Choose a project above/)).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toHaveValue("");
    expect(screen.getByRole("listitem", { name: "Pipeline server" })).toHaveTextContent("In the project you choose");
  });

  it("Set up on the Pipelines tab sets up only what is missing, in order, after one confirmation, and succeeds only once it works", async () => {
    // A fake clock, so the page's status poll and the run's ready check run when the test says.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status: ResourcesStatus = { minio: notDeployed, mlflow: notDeployed, pipelineServers: [] };
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev-project"] },
      "POST /api/resources/s3/setup": () => { status = { ...status, minio: readyMinio }; return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] }); },
      // Created, then starting: the run waits until the status says ready.
      "POST /api/resources/pipeline-server/setup": () => {
        status = { ...status, pipelineServers: [{ ...readyDspa("dev-project"), ready: false, message: "Starting (1/3 ready)", uiRoute: undefined }] };
        return jsonResponse({ success: true, message: "Pipeline server created.", logs: [] });
      },
    });
    renderPage("/test-resources?tab=pipelines&project=dev-project");
    expect(await screen.findByText("Not set up in dev-project")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Pipelines in dev-project" }));
    const confirm = await screen.findByRole("dialog");
    expect(confirm).toHaveTextContent("Set up Pipelines in dev-project?");
    // Item 9: the outcome first; the objects under Details.
    expect(confirm).toHaveTextContent("On the cluster: S3 storage (20 GiB)");
    expect(confirm).toHaveTextContent("In dev-project: a pipeline server");
    expect(confirm).toHaveTextContent("Deployment seaweedfs");
    expect(confirm).toHaveTextContent("DataSciencePipelinesApplication dspa (the name workbench pipelines use)");
    fireEvent.click(within(confirm).getByRole("button", { name: "Set up" }));
    const progress = await screen.findByRole("dialog", { name: "Set up Pipelines in dev-project" });
    // Never a success before the part works: first "Starting", then ready, with the main action.
    // The S3 storage is ready at the run's next ready check; then the pipeline server is created.
    expect(await within(progress).findByText(/Created\. Starting \(typically under a minute\)/)).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(READY_CHECK_MS));
    expect(await within(progress).findByText(/Created\. Starting \(typically 1 to 3 minutes\)/)).toBeInTheDocument();
    expect(within(progress).queryByText("Pipelines are ready in dev-project.")).not.toBeInTheDocument();
    // The server comes up; the page's next status poll sees it, and the run's next ready check after that.
    status = { ...status, pipelineServers: [readyDspa("dev-project")] };
    await act(() => vi.advanceTimersByTimeAsync(RESOURCE_POLL_BASE_MS + READY_CHECK_MS));
    expect(await within(progress).findByText("Pipelines are ready in dev-project.")).toBeInTheDocument();
    expect(within(progress).getByText("The pipeline server in dev-project is ready.", { exact: false })).toBeInTheDocument();
    expect(within(progress).getByRole("link", { name: /Open pipelines/ })).toHaveClass("pf-m-primary");
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources")).map((c) => c.split(" ")[1])).toEqual(["/api/resources/s3/setup", "/api/resources/pipeline-server/setup"]);
    expect(api.bodies["POST /api/resources/pipeline-server/setup"]).toEqual([{ project: "dev-project" }]);
  });

  it("Show issue opens the feature's tab at the part that needs attention", async () => {
    stubApi({
      "/api/resources/status": { minio: { ...readyMinio, ready: false, terminalError: true, waitingReason: "ImagePullBackOff", message: "ImagePullBackOff" }, mlflow: notDeployed, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev-project"] },
    });
    const router = renderPage("/test-resources");
    expect(await row("S3 storage")).toHaveTextContent("Error");
    const pipelines = await row("Pipelines");
    expect(pipelines).toHaveTextContent("Error");
    // Pipelines needs the storage, but the storage's issue is shown on its own tab.
    fireEvent.click(within(pipelines).getByRole("button", { name: "Show issue" }));
    await waitFor(() => expect(router.state.location.search).toBe("?tab=storage"));
    const title = await screen.findByRole("heading", { level: 3, name: "SeaweedFS" });
    await waitFor(() => expect(title).toHaveFocus());
    expect(screen.queryByRole("combobox", { name: "Project" })).not.toBeInTheDocument();
  });
});
