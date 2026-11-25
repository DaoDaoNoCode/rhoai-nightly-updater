import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import { contract } from "../../test/contract";
import type { ResourceState, ResourcesStatus, WorkbenchState, WorkbenchStatus } from "../../types";
import { IDLE } from "./featureState";
import { cleanupCandidates, cleanupSteps } from "./CleanupModal";
import { DEPRECATED_IMAGE_NOTE, workbenchConfirm, workbenchSteps } from "./WorkbenchTab";
import { workbenchState } from "./workbenchState";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const running = (): WorkbenchStatus => ({ ...contract.workbench("dev"), itemsUnreadable: undefined, itemsProblem: undefined });
const item = (over: Partial<WorkbenchState>): WorkbenchStatus => {
  const st = running();
  st.items = [{ ...st.items![0], ...over }];
  return st;
};
const notSetUp = (over: Partial<WorkbenchStatus> = {}): WorkbenchStatus => ({ ...running(), items: [], ...over });
const sse = (...events: object[]) => () => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

describe("workbenchState", () => {
  it("maps the dashboard's states: Running opens it, Stopped is grey, Failed shows the issue", () => {
    expect(workbenchState(running(), "dev", IDLE)).toMatchObject({ status: "ready", label: "Running in dev", next: { kind: "open", label: "Open workbench", href: "https://rh-ai.apps.example.com/notebook/dev/nightly-workbench" } });
    expect(workbenchState(item({ state: "starting", ready: false }), "dev", IDLE)).toMatchObject({ status: "setting-up", label: "Starting in dev" });
    expect(workbenchState(item({ state: "stopping", ready: false }), "dev", IDLE)).toMatchObject({ status: "setting-up", label: "Stopping in dev" });
    expect(workbenchState(item({ state: "stopped", ready: false }), "dev", IDLE)).toMatchObject({ status: "not-set-up", label: "Stopped in dev", next: null });
    expect(workbenchState(item({ state: "failed", ready: false }), "dev", IDLE)).toMatchObject({ status: "error", next: { kind: "show-issue", part: "workbench" } });
    expect(workbenchState(item({ managedByTool: false }), "dev", IDLE)).toMatchObject({ unmanaged: true });
    expect(workbenchState(running(), "dev", { ...IDLE, workbench: { action: "stop", project: "dev" } })).toMatchObject({ status: "setting-up", label: "Stopping in dev" });
  });

  it("not set up offers Set up, blocked with the plan's problem, the component or the platform", () => {
    expect(workbenchState(notSetUp(), "dev", IDLE).next).toMatchObject({ kind: "set-up", setup: [{ kind: "setup-workbench", project: "dev" }], reason: null });
    expect(workbenchState(notSetUp({ plan: undefined, planProblem: "No enabled hardware profile is offered for workbenches.", planProblemCode: "no_profile" }), "dev", IDLE).next).toMatchObject({ reason: "No enabled hardware profile is offered for workbenches." });
    expect(workbenchState(notSetUp({ unsupported: "This cluster runs RHOAI 2.x." }), "dev", IDLE).next).toMatchObject({ reason: "This cluster runs RHOAI 2.x." });
    expect(workbenchState(notSetUp({ project: "team" }), "dev", IDLE).next).toMatchObject({ reason: expect.stringContaining("loading") });
    expect(workbenchState(null, "dev", IDLE)).toMatchObject({ loading: true });
    expect(workbenchState(notSetUp(), null, IDLE)).toMatchObject({ status: "not-set-up", label: "Not set up in any project", next: { kind: "choose-project" } });
  });
});

describe("Workbench confirmations", () => {
  it("Set up names the image, the profile and the storage", () => {
    const spec = workbenchConfirm({ kind: "setup-workbench", project: "dev" }, notSetUp())!;
    render(<>{spec.summary}</>);
    expect(screen.getByText(/with Jupyter \| Minimal \| CPU \| Python 3\.12 \(2025\.2\)/)).toBeInTheDocument();
    expect(screen.getByText(/^Default/)).toBeInTheDocument();
    expect(screen.getByText("storage class gp3-csi")).toBeInTheDocument();
    expect(screen.getByText("20Gi")).toBeInTheDocument();
  });

  it("Set up with a deprecated image labels it as the dashboard does and says why", () => {
    const st = notSetUp();
    st.plan = { ...st.plan!, imageDeprecated: true };
    const spec = workbenchConfirm({ kind: "setup-workbench", project: "dev" }, st)!;
    render(<>{spec.summary}</>);
    expect(screen.getByText("Deprecated")).toBeInTheDocument();
    expect(screen.getByText("Its image is deprecated")).toBeInTheDocument();
    expect(screen.getByText(DEPRECATED_IMAGE_NOTE)).toBeInTheDocument();
  });

  it("a workbench on a deprecated image shows the dashboard's Deprecated label", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/workbench": item({ imageDeprecated: true }), "/api/resources/prerequisites": { features: {} } });
    renderAt("/test-resources?tab=workbench&project=dev");
    const details = await screen.findByLabelText("Workbench details");
    expect(within(details).getByText("Deprecated")).toBeInTheDocument();
  });

  it("Set up with a current image shows no Deprecated label", () => {
    render(<>{workbenchConfirm({ kind: "setup-workbench", project: "dev" }, notSetUp())!.summary}</>);
    expect(screen.queryByText("Deprecated")).not.toBeInTheDocument();
  });

  it("Delete keeps the storage unless ticked; the tick asks for the storage's deletion", () => {
    const setPending = vi.fn();
    const spec = workbenchConfirm({ kind: "teardown-workbench", project: "dev" }, running(), setPending)!;
    expect(spec.dataLoss).toBeUndefined();
    render(<>{spec.choice}</>);
    const box = screen.getByRole("checkbox", { name: "Also delete its storage (files are lost)" });
    expect(box).not.toBeChecked();
    fireEvent.click(box);
    expect(setPending).toHaveBeenCalledWith({ kind: "teardown-workbench", project: "dev", deleteStorage: true });
    expect(workbenchConfirm({ kind: "teardown-workbench", project: "dev", deleteStorage: true }, running())!.dataLoss).toBeTruthy();
    expect(workbenchSteps({ kind: "teardown-workbench", project: "dev", deleteStorage: true }).map((s) => s.id)).toEqual(["workbench", "storage"]);
    // Volumes this tool never provisioned are omitted from the teardown choices.
    const foreign = running();
    foreign.items![0].storage!.managedByTool = false;
    expect(workbenchConfirm({ kind: "teardown-workbench", project: "dev" }, foreign)!.choice).toBeUndefined();
  });
});

describe("Workbench tab", () => {
  it("not set up: shows what Set up creates and streams the setup", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": notSetUp(),
      "POST /api/resources/workbench/stream": sse({ step: "check", status: "success", message: "ok" }, { step: "operation_complete", status: "success", message: "starting" }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    const plan = await screen.findByRole("list", { name: "What Set up creates" }).catch(() => screen.findByLabelText("What Set up creates"));
    expect(within(plan).getByText("Jupyter | Minimal | CPU | Python 3.12 (2025.2)")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up a workbench in dev" }));
    const dialog = await screen.findByRole("dialog", { name: /Set up a workbench in dev\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Set up" }));
    await screen.findByRole("dialog", { name: "Set up a workbench in dev" });
    expect(api.bodies["POST /api/resources/workbench/stream"]).toEqual([{ action: "setup", project: "dev" }]);
  });

  it("no fitting profile: one alert says why and whether it is the cluster or the project; no Set up, no promise it starts", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": notSetUp({ plan: undefined, planProblemCode: "no_profile", planProblem: "No enabled hardware profile is offered for workbenches." }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    expect(await screen.findByText("No hardware profile is available for workbenches on this cluster")).toBeInTheDocument();
    expect(screen.getAllByText("No enabled hardware profile is offered for workbenches.")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Set up a workbench in dev" })).toBeNull();
    expect(screen.queryByText(/starts in 1 to 3 minutes/)).toBeNull();
  });

  it("a project's Kueue problem is the project's", async () => {
    stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": notSetUp({ plan: undefined, planProblemCode: "no_profile_project", planProblem: "The project uses Kueue..." }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    expect(await screen.findByText("No hardware profile is available for workbenches in this project")).toBeInTheDocument();
  });

  it("a workbench that cannot be read is unknown with Retry, never Not set up; no Set up", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": notSetUp({ plan: undefined, itemsUnreadable: ["dev"], itemsProblem: "Cannot read the workbench in dev: 503.", planProblem: "Cannot read the workbench in dev: 503." }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    expect(within(part).getByText("Unknown")).toBeInTheDocument();
    expect(within(part).queryByText("Not set up")).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up a workbench in dev" })).toBeNull();
    const before = api.calls.filter((c) => c.includes("/api/resources/workbench")).length;
    fireEvent.click(within(part).getByRole("button", { name: "Retry" }));
    await screen.findByRole("listitem", { name: "Workbench" });
    expect(api.calls.filter((c) => c.includes("/api/resources/workbench")).length).toBeGreaterThan(before);
  });

  it("storage kept from a deleted workbench: said, reused, and Delete storage deletes it alone", async () => {
    const st = notSetUp();
    st.items = [{ ...running().items![0], deployed: false, ready: false, state: undefined }];
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": st,
      "POST /api/resources/workbench/stream": sse({ step: "operation_complete", status: "success", message: "deleted" }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    expect(within(part).getByText(/Storage kept from a deleted workbench/)).toBeInTheDocument();
    expect(within(part).getByText(/reused by the next Set up/)).toBeInTheDocument();
    fireEvent.click(within(part).getByRole("button", { name: "Workbench actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete storage" }));
    const dialog = await screen.findByRole("dialog", { name: /Delete the kept storage in dev\?/ });
    expect(within(dialog).getByText(/nightly-workbench-storage, 20Gi, storage class gp3-csi/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await screen.findByRole("dialog", { name: "Delete the kept storage in dev" });
    expect(api.bodies["POST /api/resources/workbench/stream"]).toEqual([{ action: "delete-storage", project: "dev" }]);
  });

  it("running: Open and Stop; Stop sends the stop action", async () => {
    const api = stubApi({
      "/api/resources/status": status,
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/workbench": running(),
      "POST /api/resources/workbench/stream": sse({ step: "operation_complete", status: "success", message: "stopping" }),
    });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    const open = within(part).getByRole("link", { name: "Open workbench" });
    expect(open).toHaveAttribute("href", "https://rh-ai.apps.example.com/notebook/dev/nightly-workbench");
    expect(open).toHaveClass("pf-m-primary");
    // One profile and storage format before and after Set up.
    expect(within(part).getByText("Default (2 CPU, 4Gi memory)")).toBeInTheDocument();
    expect(within(part).getByText((_t, el) => el?.textContent === "nightly-workbench-storage, 20Gi, storage class gp3-csi" && el.tagName !== "DIV")).toBeInTheDocument();
    fireEvent.click(within(part).getByRole("button", { name: "Stop" }));
    const dialog = await screen.findByRole("dialog", { name: /Stop the workbench in dev\?/ });
    expect(within(dialog).getByText(/only files saved under \/opt\/app-root\/src are in the storage/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop" }));
    await screen.findByRole("dialog", { name: "Stop the workbench in dev" });
    expect(api.bodies["POST /api/resources/workbench/stream"]).toEqual([{ action: "stop", project: "dev" }]);
  });

  it("stopped: Start, no Open", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/workbench": item({ state: "stopped", ready: false }) });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    expect(within(part).getByRole("button", { name: "Start" })).toBeInTheDocument();
    expect(within(part).queryByRole("link", { name: "Open workbench" })).toBeNull();
  });
});

describe("Delete warnings", () => {
  it("deleting a running workbench warns that the session's unsaved work is lost; ticking the storage makes the title icon danger", () => {
    const spec = workbenchConfirm({ kind: "teardown-workbench", project: "dev" }, running())!;
    render(<>{spec.summary}</>);
    expect(screen.getByText(/Unsaved work in the running session is lost/)).toBeInTheDocument();
    expect(spec.titleIcon).toBeUndefined();
    expect(workbenchConfirm({ kind: "teardown-workbench", project: "dev", deleteStorage: true }, running())!.titleIcon).toBe("danger");
  });
});

describe("Storage used by another workbench", () => {
  const usedBy = (): WorkbenchStatus => {
    const st = running();
    st.items![0].storage!.usedBy = ["workbench my-wb"];
    return st;
  };
  it("the tab warns, Delete cannot tick the storage, Delete storage cannot be confirmed", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/workbench": usedBy() });
    renderAt("/test-resources?tab=workbench&project=dev");
    await screen.findByRole("listitem", { name: "Workbench" });
    expect(screen.getByText(/Storage nightly-workbench-storage is used by workbench my-wb; remove it from that workbench in the dashboard first/)).toBeInTheDocument();
    const del = workbenchConfirm({ kind: "teardown-workbench", project: "dev", deleteStorage: true }, usedBy())!;
    render(<>{del.choice}</>);
    expect(screen.getByRole("checkbox", { name: "Also delete its storage (files are lost)" })).toBeDisabled();
    const kept = usedBy();
    kept.items = [{ ...kept.items![0], deployed: false, ready: false, state: undefined }];
    const spec = workbenchConfirm({ kind: "delete-workbench-storage", project: "dev" }, kept)!;
    expect(spec.confirmDisabled).toBe(true);
  });
  it("Clean up offers its storage disabled, never in the request", () => {
    const offered = cleanupCandidates(status, "workbench", false, null, null, null, usedBy());
    expect(offered.storageOf("dev")?.usedBy).toEqual(["workbench my-wb"]);
  });
});

describe("Kept storage another workbench uses", () => {
  const keptUsed = (): WorkbenchStatus => {
    const st = notSetUp();
    st.items = [{ ...running().items![0], deployed: false, ready: false, state: undefined }];
    st.items[0].storage!.usedBy = ["workbench my-wb"];
    return st;
  };
  const REASON = "Storage nightly-workbench-storage is used by workbench my-wb; a new workbench could not mount it (single-node storage). Remove it from my-wb in the dashboard first.";
  it("Set up is disabled with the reason, and the card does not promise reuse", async () => {
    expect(workbenchState(keptUsed(), "dev", IDLE).next).toMatchObject({ kind: "set-up", reason: REASON });
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/workbench": keptUsed() });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    expect(within(part).queryByText(/reused by the next Set up/)).toBeNull();
    expect(screen.getByRole("button", { name: "Set up a workbench in dev" })).toHaveAttribute("aria-disabled", "true");
    // The full reason once, on the disabled button; the alert only says what this tool does meanwhile.
    expect(screen.getAllByText(REASON)).toHaveLength(1);
    expect(screen.getByText("Until then this tool does not reuse or delete it.")).toBeInTheDocument();
    // The in-use alert spans the tab (outside the card), as for a running workbench.
    expect(within(part).queryByText("Its storage is used by another workbench")).toBeNull();
    expect(screen.getByText("Its storage is used by another workbench")).toBeInTheDocument();
  });
  it("Delete storage: the in-use alert first, no data-loss alert while Delete is disabled", () => {
    const spec = workbenchConfirm({ kind: "delete-workbench-storage", project: "dev" }, keptUsed())!;
    expect(spec.confirmDisabled).toBe(true);
    expect(spec.dataLoss).toBeUndefined();
    render(<>{spec.summary}</>);
    expect(screen.getByText("The storage is in use")).toBeInTheDocument();
  });
  it("the Storage row never breaks a storage class name", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/workbench": running() });
    renderAt("/test-resources?tab=workbench&project=dev");
    const part = await screen.findByRole("listitem", { name: "Workbench" });
    expect(within(part).getByText("storage class gp3-csi")).toHaveClass("pf-v6-u-text-nowrap");
  });
});

describe("Clean up", () => {
  it("offers kept storage on its own, unticked, and runs it as its own step", () => {
    const st = notSetUp();
    st.items = [{ ...running().items![0], deployed: false, ready: false, state: undefined }];
    const offered = cleanupCandidates(status, "workbench", false, null, null, null, st);
    expect(offered.workbenches).toEqual([]);
    expect(offered.keptStorage).toEqual(["dev"]);
    expect(cleanupSteps({ workbenchStorage: ["dev"] }).map((s) => s.id)).toEqual(["workbench-storage:dev"]);
  });

  it("offers this tool's workbenches; their storage only when ticked; workbenches go first", () => {
    const offered = cleanupCandidates(status, "all", false, null, null, null, running());
    expect(offered.workbenches).toEqual(["dev"]);
    expect(offered.workbenchStorage).toEqual(["dev"]);
    expect(cleanupCandidates(status, "all", false, null, null, null, item({ managedByTool: false })).workbenches).toEqual([]);
    expect(cleanupSteps({ workbenches: ["dev"], workbenchStorage: ["dev"], pipelineServers: ["dev"] }).map((s) => s.id)).toEqual(["workbench:dev", "pipeline-server:dev"]);
    expect(cleanupSteps({ workbenches: ["dev"] })[0].title).toBe("Delete the workbench in dev");
  });
});
