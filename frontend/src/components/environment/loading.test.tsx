import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { jsonResponse, stubApi } from "../../test/apiStub";
import type { MaaSStatus, PlaygroundModel, PlaygroundStatus, ResourceState, ResourcesStatus } from "../../types";

// The Environment page before, while and after its data loads: every read
// is held until the test answers it, so each state is looked at as it is.

const tick = () => act(() => vi.advanceTimersByTimeAsync(0));

/** Requests to one endpoint, each waiting until the test answers it (oldest first). */
function gate() {
  const waiting: { url: string; answer: (r: Response) => void }[] = [];
  return {
    handler: (url: string) => new Promise<Response>((answer) => waiting.push({ url, answer })),
    /** The requests not answered yet. */
    pending: () => waiting.map((w) => w.url),
    async send(body: unknown, status = 200) {
      const next = waiting.shift();
      if (!next) throw new Error("no request is waiting");
      next.answer(jsonResponse(body, status));
      await tick();
    },
  };
}

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const projects = { projects: ["dev", "team"] };
const playground: PlaygroundStatus = { operatorReady: "True", genAiUi: "available", items: [] };
const maas: MaaSStatus = {
  platform: { deployed: false, ready: false, managedByTool: false },
  parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }],
  subscription: "rhoai-nightly-updater-subscription",
};
const qwen: PlaygroundModel = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", source: "namespace", status: "Running", ready: true, chat: true };

/** Every read the page makes, held; the pipeline sample's spec never answers (it is not what these tests look at). */
function stubHeld() {
  const g = { status: gate(), projects: gate(), playground: gate(), maas: gate() };
  stubApi({
    "/api/resources/status": g.status.handler,
    "/api/resources/projects": g.projects.handler,
    "/api/resources/playground": g.playground.handler,
    "/api/resources/maas": g.maas.handler,
    "/api/resources/samples/pipeline/spec": () => new Promise<Response>(() => {}),
  });
  return g;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QuickResourceCreator mutateBlocker={null} renderHeader={({ onRefresh }) => <button type="button" onClick={onRefresh}>Refresh</button>} />
    </MemoryRouter>,
  );
}

const panel = () => screen.getByRole("tabpanel");
/** Not one empty, missing or not-set-up word while the data that would say so is not in. */
const NEGATIVE = /Not set up|No data science projects|No model of this tool yet|no ready chat model|Not published|of 6 ready/;

/** Picks a project in the tab's project picker. */
async function chooseProject(name: string) {
  fireEvent.click(screen.getByRole("button", { name: "Show projects" }));
  await tick();
  fireEvent.click(screen.getByRole("option", { name: new RegExp(`^${name}`) }));
  await tick();
}

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Environment loading: the Overview", () => {
  it("shows a Skeleton per feature's state until its status is in, never Not set up", async () => {
    const g = stubHeld();
    renderAt("/test-resources");
    await tick();
    // The page's tabs are there from the start.
    expect(screen.getByRole("tab", { name: "MaaS" })).toBeInTheDocument();
    const table = screen.getByRole("grid", { name: "Features" });
    expect(table).toHaveAttribute("aria-busy", "true");
    for (const name of ["S3 storage", "Pipelines", "MLflow", "Models", "Playground", "MaaS"]) {
      expect(within(table).getByText(`Loading the state of ${name}`)).toBeInTheDocument();
    }
    expect(panel()).not.toHaveTextContent(NEGATIVE);

    await g.projects.send(projects);
    await g.status.send(status);
    // The status's features are in; the playground's and MaaS's own reads are not.
    expect(within(table).queryByText("Loading the state of Pipelines")).toBeNull();
    // Pipelines and Models.
    expect(within(table).getAllByText("Not set up in any project", { selector: ".pf-v6-c-label *" })).toHaveLength(2);
    expect(within(table).getByText("Loading the state of Playground")).toBeInTheDocument();
    expect(within(table).getByText("Loading the state of MaaS")).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent(/of 6 ready/);

    await g.playground.send(playground);
    await g.maas.send(maas);
    expect(within(table).queryByText(/^Loading the state of/)).toBeNull();
    expect(table).toHaveAttribute("aria-busy", "false");
    expect(panel()).toHaveTextContent("0 of 6 ready");
    expect(screen.getByRole("button", { name: /^Set up MaaS/ })).toBeInTheDocument();
  });

  it("a status that cannot be read shows why with Retry, not an empty page; Retry reads it again", async () => {
    const g = stubHeld();
    renderAt("/test-resources");
    await tick();
    await g.projects.send(projects);
    await g.status.send({ error: "the cluster API answered 503", errorCode: "cluster_unavailable" }, 503);
    expect(screen.getByRole("heading", { name: "Can't load the environment" })).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent(NEGATIVE);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await tick();
    await g.projects.send(projects);
    await g.status.send(status);
    expect(screen.queryByRole("heading", { name: "Can't load the environment" })).toBeNull();
    // The S3 storage and MLflow.
    expect(within(screen.getByRole("grid", { name: "Features" })).getAllByText("Not set up", { selector: ".pf-v6-c-label *" })).toHaveLength(2);
  });

  it("Refresh reads every status again and keeps showing the current states meanwhile", async () => {
    const g = stubHeld();
    renderAt("/test-resources");
    await tick();
    await g.projects.send(projects);
    await g.status.send(status);
    await g.playground.send(playground);
    await g.maas.send(maas);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await tick();
    expect(g.status.pending()).toHaveLength(1);
    expect(g.projects.pending()).toHaveLength(1);
    expect(g.playground.pending()).toHaveLength(1);
    expect(g.maas.pending()).toHaveLength(1);
    const table = screen.getByRole("grid", { name: "Features" });
    expect(within(table).queryByText(/^Loading the state of/)).toBeNull();
    expect(panel()).toHaveTextContent("0 of 6 ready");
    await g.status.send({ ...status, mlflow: { deployed: true, ready: true, managedByTool: true, uiRoute: "https://mlflow.example.com" } });
    expect(panel()).toHaveTextContent("0 of 6 ready");
    await g.projects.send(projects);
    expect(panel()).toHaveTextContent("1 of 6 ready");
  });
});

describe("Environment loading: the feature tabs", () => {
  it.each([
    ["storage", "S3 storage", /^S3 storage/],
    ["pipelines", "Pipelines", /^Pipeline server/],
    ["mlflow", "MLflow", /^MLflow server/],
    ["models", "Models", /^Inference gateway/],
  ])("%s: a Skeleton for the state and the parts until the status is in", async (tab, title, part) => {
    const g = stubHeld();
    renderAt(`/test-resources?tab=${tab}&project=dev`);
    await tick();
    expect(screen.getByRole("heading", { name: title })).toBeInTheDocument();
    expect(within(panel()).getByText(`Loading the state of ${title}`)).toBeInTheDocument();
    expect(within(panel()).getByText(`Loading what ${title} needs`)).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent(NEGATIVE);
    expect(screen.queryByRole("listitem", { name: part })).toBeNull();
    await g.projects.send(projects);
    await g.status.send(status);
    expect(within(panel()).queryByText(/^Loading /)).toBeNull();
    expect(screen.getByRole("listitem", { name: part })).toBeInTheDocument();
  });

  it("playground: loading until the playground's own status is in, after the page's", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=playground&project=dev");
    await tick();
    expect(within(panel()).getByText("Loading what Playground needs")).toBeInTheDocument();
    await g.projects.send(projects);
    await g.status.send(status);
    expect(within(panel()).getByText("Loading the state of Playground")).toBeInTheDocument();
    expect(within(panel()).getByText("Loading what Playground needs")).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent(NEGATIVE);
    expect(g.playground.pending()).toEqual(["/api/resources/playground?project=dev"]);
    await g.playground.send({ ...playground, project: "dev", models: [qwen] });
    expect(within(panel()).queryByText(/^Loading /)).toBeNull();
    expect(screen.getByRole("listitem", { name: /^Playground/ })).toHaveTextContent("Not set up in dev yet");
  });

  it("maas: Skeleton parts and model rows until MaaS's status is in", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=maas");
    await tick();
    await g.projects.send(projects);
    await g.status.send(status);
    expect(within(panel()).getByText("Loading the state of MaaS")).toBeInTheDocument();
    expect(within(panel()).getByText("Loading what MaaS needs")).toBeInTheDocument();
    expect(within(screen.getByRole("grid", { name: "This tool's models in MaaS" })).getByText("Loading this tool's models in MaaS")).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent(NEGATIVE);
    await g.maas.send({ ...maas, items: [{ namespace: "dev", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: false, ready: false, managedByTool: true, modelReady: true, published: false, message: "Not published" }] });
    expect(within(panel()).queryByText(/^Loading /)).toBeNull();
    expect(screen.getByRole("listitem", { name: /^MaaS in the DataScienceCluster/ })).toBeInTheDocument();
    expect(screen.getByText("Qwen2.5 0.5B Instruct (CPU)").closest("tr")).toHaveTextContent("Not published");
  });

  it("maas: a status that cannot be read says so with Try again instead of empty lists", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=maas");
    await tick();
    await g.maas.send({ error: "maas-api is not reachable", errorCode: "upstream_error" }, 502);
    const alert = screen.getByText("Cannot read Models as a Service").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("maas-api is not reachable");
    expect(panel()).not.toHaveTextContent(NEGATIVE);
    expect(within(panel()).queryByText(/^Loading /)).toBeNull();
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }));
    await tick();
    await g.maas.send(maas);
    expect(screen.queryByText("Cannot read Models as a Service")).toBeNull();
    expect(screen.getByRole("listitem", { name: /^MaaS in the DataScienceCluster/ })).toBeInTheDocument();
  });

  it("maas: Refresh keeps the parts and models shown while it reads them again", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=maas");
    await tick();
    await g.maas.send(maas);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await tick();
    expect(g.maas.pending()).toHaveLength(1);
    expect(screen.getByRole("listitem", { name: /^MaaS in the DataScienceCluster/ })).toBeInTheDocument();
    expect(within(panel()).queryByText(/^Loading /)).toBeNull();
  });

  it("a project list that cannot be read is said, not shown as no projects", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=models");
    await tick();
    await g.projects.send({ error: "forbidden", errorCode: "forbidden" }, 403);
    await g.status.send(status);
    expect(screen.getByText("Cannot load the data science projects")).toBeInTheDocument();
    expect(panel()).not.toHaveTextContent("No data science projects");
  });
});

describe("Environment loading: another project", () => {
  const server = (ns: string, over: Partial<ResourceState> = {}): ResourceState => ({
    namespace: ns, name: "dspa", deployed: true, ready: true, managedByTool: true, connection: "rhoai-nightly-updater-s3", ...over,
  });

  it("pipelines: the new project's Connection loads; the last project's is not shown for it", async () => {
    const g = stubHeld();
    const both = { ...status, minio: { deployed: true, ready: true, managedByTool: true }, pipelineServers: [server("dev"), server("team")] };
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    expect(g.status.pending()).toEqual(["/api/resources/status?project=dev"]);
    await g.projects.send(projects);
    await g.status.send({ ...both, pipelineServers: [server("dev", { connectionState: "present" }), server("team")] });
    const details = () => screen.getByRole("listitem", { name: /^Pipeline server/ });
    expect(details()).toHaveTextContent(/listed as "Shared S3 storage/);

    await chooseProject("team");
    expect(g.status.pending()).toEqual(["/api/resources/status?project=team"]);
    expect(details()).toHaveTextContent("In project team");
    expect(within(details()).getByText("Loading the Connection in team")).toBeInTheDocument();
    expect(details()).not.toHaveTextContent(/listed as|Missing:|None:/);
    await g.projects.send(projects);
    await g.status.send({ ...both, pipelineServers: [server("dev"), server("team", { connectionState: "missing" })] });
    expect(within(details()).queryByText(/^Loading /)).toBeNull();
    expect(details()).toHaveTextContent("Missing:");
  });

  it("an answer for the project left behind does not replace the chosen project's", async () => {
    const g = stubHeld();
    const both = { ...status, minio: { deployed: true, ready: true, managedByTool: true }, pipelineServers: [server("dev"), server("team")] };
    renderAt("/test-resources?tab=pipelines&project=dev");
    await tick();
    await g.projects.send(projects);
    await g.status.send(both);
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await tick();
    await chooseProject("team");
    expect(g.status.pending()).toEqual(["/api/resources/status?project=dev", "/api/resources/status?project=team"]);
    // dev's answers last but was asked first: team's Connection stays loading.
    await g.projects.send(projects);
    await g.status.send({ ...both, pipelineServers: [server("dev", { connectionState: "present" }), server("team")] });
    expect(within(screen.getByRole("listitem", { name: /^Pipeline server/ })).getByText("Loading the Connection in team")).toBeInTheDocument();
    await g.projects.send(projects);
    await g.status.send({ ...both, pipelineServers: [server("dev"), server("team", { connectionState: "present" })] });
    expect(screen.getByRole("listitem", { name: /^Pipeline server/ })).toHaveTextContent(/listed as "Shared S3 storage/);
  });

  it("playground: the new project's models load; the last project's are not shown for it", async () => {
    const g = stubHeld();
    renderAt("/test-resources?tab=playground&project=dev");
    await tick();
    await g.projects.send(projects);
    await g.status.send(status);
    await g.playground.send({ ...playground, project: "dev", models: [qwen] });
    const models = () => screen.getByRole("listitem", { name: /^Models/ });
    expect(within(models()).getByText("Qwen2.5 0.5B Instruct (CPU)")).toBeInTheDocument();

    await chooseProject("team");
    expect(g.playground.pending()).toEqual(["/api/resources/playground?project=team"]);
    expect(models()).toHaveTextContent("In project team");
    expect(within(models()).getByText("Loading the models in team")).toBeInTheDocument();
    expect(models()).not.toHaveTextContent(/Qwen2.5|no ready chat model/);
    await g.playground.send({ ...playground, project: "team", models: [] });
    expect(within(models()).queryByText(/^Loading /)).toBeNull();
    expect(models()).toHaveTextContent("team has no ready chat model yet");
  });
});
