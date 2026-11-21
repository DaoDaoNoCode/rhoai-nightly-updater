import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { UserEnvironmentPage } from "./UserEnvironmentPage";
import { USER_PERMISSIONS, renderPage, settleApps } from "../test/providers";
import { jsonResponse } from "../test/apiStub";
import { ADMIN_ONLY_GETS, projectStatus, stubUserApi } from "../test/userFixtures";
import { contract } from "../test/contract";

afterEach(() => {
  vi.unstubAllGlobals();
});

const Where: React.FC = () => <span data-testid="where">{useLocation().search}</span>;

const render = (route = "/test-resources") => renderPage(<><UserEnvironmentPage /><Where /></>, route, { permissions: async () => USER_PERMISSIONS });
const part = (title: string) => screen.getByRole("region", { name: title });

describe("Environment for a user who is not a cluster-admin", () => {
  it("shows the first of their projects, read-only, with the cluster-admin note", async () => {
    const api = stubUserApi();
    render();
    expect(await screen.findByRole("combobox", { name: "Project" })).toHaveValue("dev-project");
    expect(screen.getByText("A cluster-admin sets up the shared parts", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/S3 storage, the inference gateway, Models as a Service and the operators/)).toBeInTheDocument();

    // Pipelines: the server and the sample pipeline.
    await within(part("Pipelines")).findByText("Ready");
    expect(within(part("Pipelines")).getByRole("link", { name: "Open pipelines in dev-project" })).toHaveAttribute("href", expect.stringContaining("/pipelines/definitions/dev-project"));
    expect(await screen.findByRole("region", { name: "Run a sample pipeline" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Download sample-artifact-check.yaml" })).toBeInTheDocument();
    // Models, playground, MLflow sample data.
    // The models and InferenceServices of the generated contract fixture (pkg/api TestFrontendContract).
    expect(within(part("Models")).getByRole("grid", { name: "Models in dev-project" })).toHaveTextContent("Qwen2.5 0.5B Instruct (CPU)");
    const isvcs = within(part("Models")).getByRole("grid", { name: "InferenceServices in dev-project" });
    expect(isvcs).toHaveTextContent("iris");
    expect(isvcs).toHaveTextContent("Loading");
    expect(await within(part("Playground")).findByText("Gen AI playground")).toBeInTheDocument();
    const samples = await within(part("MLflow sample data")).findByRole("grid", { name: "MLflow sample data in dev-project" });
    expect(within(samples).getByText("Added")).toBeInTheDocument();
    expect(within(samples).getByText("Not added")).toBeInTheDocument();
    expect(within(samples).getByRole("link", { name: "Open in MLflow" })).toBeInTheDocument();

    // Nothing to change: no Set up, Delete, Add, Remove or Clean up.
    expect(screen.queryAllByRole("button").map((b) => b.textContent ?? "")).toEqual(expect.not.arrayContaining([
      expect.stringMatching(/^(Set up|Delete|Remove|Add|Add all|Remove all|Clean up|Publish)/),
    ]));
    // Every read is the project's, with the user's token; none only cluster-admins may make.
    expect(api.calls).toEqual(expect.arrayContaining([
      "GET /api/resources/status?project=dev-project",
      "GET /api/resources/playground?project=dev-project",
      "GET /api/resources/samples/mlflow?project=dev-project",
    ]));
    expect(api.calls.map((c) => c.replace(/^GET /, "")).filter((c) => ADMIN_ONLY_GETS.some((re) => re.test(c)))).toEqual([]);
    await settleApps();
  });

  it("switches to another project and reads it", async () => {
    const api = stubUserApi();
    render();
    const box = await screen.findByRole("combobox", { name: "Project" });
    fireEvent.click(box);
    fireEvent.click(await screen.findByRole("option", { name: "llm-eval" }));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("project=llm-eval"));
    await waitFor(() => expect(api.calls).toContain("GET /api/resources/status?project=llm-eval"));
    expect(await within(part("Models")).findByRole("grid", { name: "Models in llm-eval" })).toBeInTheDocument();
    await settleApps();
  });

  it("opens the project in the address", async () => {
    stubUserApi();
    render("/test-resources?project=llm-eval");
    expect(await screen.findByRole("combobox", { name: "Project" })).toHaveValue("llm-eval");
    await settleApps();
  });

  it("says when the project in the address is not one of theirs", async () => {
    stubUserApi();
    render("/test-resources?project=someone-elses");
    expect(await screen.findByText("someone-elses is not one of your projects", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Project" })).toHaveValue("dev-project");
    await settleApps();
  });

  it("without projects, says how to get one", async () => {
    const api = stubUserApi({ "/api/resources/projects": { projects: null } });
    render();
    expect(await screen.findByRole("heading", { name: "You have no data science projects" })).toBeInTheDocument();
    expect(screen.getByText(/Create one in the RHOAI dashboard/)).toBeInTheDocument();
    expect(api.calls.filter((c) => c.includes("/api/resources/status"))).toEqual([]);
    await settleApps();
  });

  it("says a cluster-admin sets up what the project does not have", async () => {
    stubUserApi({
      "/api/resources/status": { ...projectStatus(), pipelineServers: [], models: [], inferenceServices: [] },
      "/api/resources/playground": { items: [] },
      "/api/resources/samples/mlflow": { project: "dev-project", available: false, reason: "MLflow is not set up on this cluster.", samples: [] },
    });
    render();
    expect(await within(await screen.findByRole("region", { name: "Pipelines" })).findByText("No pipeline server in dev-project. Add one in the RHOAI dashboard, or ask a cluster-admin to set one up here.")).toBeInTheDocument();
    expect(within(part("Models")).getByText("No models in dev-project. Add one in the RHOAI dashboard, or ask a cluster-admin to set one up here.")).toBeInTheDocument();
    expect(await within(part("Playground")).findByText("No playground in dev-project. Add one in the RHOAI dashboard, or ask a cluster-admin to set one up here.")).toBeInTheDocument();
    expect(await within(part("MLflow sample data")).findByText("MLflow is not set up on this cluster. A cluster-admin sets this up.")).toBeInTheDocument();
    await settleApps();
  });

  it("a part the backend could not read says why: not yours to see, not installed, or the error", async () => {
    let statusReads = 0;
    stubUserApi({
      "/api/resources/status": () => { statusReads++; return jsonResponse(contract.projectStatusProblems("dev-project")); },
      "/api/resources/playground": contract.playgroundProblem("dev-project"),
    });
    render();
    const pipelines = await screen.findByRole("region", { name: "Pipelines" });
    expect(await within(pipelines).findByText("You cannot see this")).toBeInTheDocument();
    expect(within(pipelines).queryByText(/No pipeline server/)).not.toBeInTheDocument();
    const models = part("Models");
    expect(within(models).getByText(/Model serving is not installed on this cluster/)).toBeInTheDocument();
    expect(within(models).getByText("Cannot read the InferenceServices")).toBeInTheDocument();
    expect(within(models).queryByText(/No models in/)).not.toBeInTheDocument();
    expect(await within(part("Playground")).findByText("You cannot see this")).toBeInTheDocument();
    expect(within(part("Playground")).queryByText(/No playground in/)).not.toBeInTheDocument();
    const before = statusReads;
    fireEvent.click(within(models).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(statusReads).toBe(before + 1));
    await settleApps();
  });

  it("a part the user may not see says so, not Access denied", async () => {
    stubUserApi({ "/api/resources/playground": () => jsonResponse({ error: "projects \"dev-project\" is forbidden", errorCode: "forbidden_for_user" }, 403) });
    render();
    expect(await within(await screen.findByRole("region", { name: "Playground" })).findByText("You cannot see this")).toBeInTheDocument();
    expect(within(part("Playground")).getByText(/Your OpenShift account has no access to it/)).toBeInTheDocument();
    expect(screen.queryByText(/Access denied/)).not.toBeInTheDocument();
    expect(screen.queryByText(/make upgrade/)).not.toBeInTheDocument();
    expect(within(part("Playground")).queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("an admin_only answer shows the cluster-admin note", async () => {
    stubUserApi({ "/api/resources/samples/mlflow": () => jsonResponse({ error: "Only cluster-admins", errorCode: "admin_only" }, 403) });
    render();
    expect(await within(await screen.findByRole("region", { name: "MLflow sample data" })).findByText("Only cluster-admins can see this")).toBeInTheDocument();
    expect(screen.queryByText(/Access denied/)).not.toBeInTheDocument();
    await settleApps();
  });

  it("another failure offers Try again and reads again", async () => {
    let fail = true;
    const api = stubUserApi({ "/api/resources/playground": () => (fail ? jsonResponse({ error: "The cluster API did not answer", errorCode: "cluster_unavailable" }, 503) : jsonResponse({ items: [{ deployed: true, ready: true, namespace: "dev-project" }] })) });
    render();
    const playground = await screen.findByRole("region", { name: "Playground" });
    expect(await within(playground).findByText("Cannot read the playground")).toBeInTheDocument();
    fail = false;
    fireEvent.click(within(playground).getByRole("button", { name: "Try again" }));
    expect(await within(part("Playground")).findByText("Gen AI playground")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.includes("/api/resources/playground"))).toHaveLength(2);
    await settleApps();
  });

  it("a project list the user may not read says so, without Diagnostics", async () => {
    stubUserApi({ "/api/resources/projects": () => jsonResponse({ error: "no", errorCode: "forbidden_for_user" }, 403) });
    render();
    expect(await screen.findByRole("heading", { name: "You cannot see this" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open Diagnostics" })).not.toBeInTheDocument();
    await settleApps();
  });
});
