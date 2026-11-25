import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes, useNavigate } from "react-router-dom";
import { GlobalBanners } from "../GlobalBanners";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { renderWithApp, settleApps } from "../../test/providers";
import { jsonResponse, stubApi } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { LegacyStorageState, MaaSStatus, OperationStatusResponse, ResourceState, ResourcesStatus } from "../../types";
import { READY_CHECK_MS, type GoalRun } from "./useGoalRun";
import { useMutationBlocker } from "../../state/AppInfo";
import { IDLE, maasState, mlflowState } from "./featureState";
import { runFollowUp } from "./runFollowUp";
import { RESOURCE_POLL_BASE_MS } from "../../constants";
import { deferred } from "../../test/utils";
import { ApiError } from "../../services/api";
import { useAppScreens } from "../../state/AppInfo";
import type { UserPermissions, VersionInfo } from "../../types";

// Routing narrowed to the Environment screen plus one alternate destination, with
// helpers so the spec can switch paths the way the sidebar would.
let go: (to: string) => void = () => {};
const Navigator: React.FC = () => {
  const navigate = useNavigate();
  go = (to) => { void navigate(to); };
  return null;
};
// What another page's change buttons say while a run of the Environment page goes on.
const Blocker: React.FC = () => <p>Changes: {useMutationBlocker() ?? "allowed"}</p>;
const Shell: React.FC = () => (
  <>
    <GlobalBanners />
    <Navigator />
    <Routes>
      <Route path="/test-resources" element={<QuickResourceCreator mutateBlocker={null} />} />
      <Route path="/components" element={<><p>Another page</p><Blocker /></>} />
    </Routes>
  </>
);
const leave = () => act(async () => go("/components"));
const back = (path: string) => act(async () => go(path));

const none: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin" };
const footerClose = (dialog: HTMLElement) => within(dialog).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!;
const alertOf = (text: RegExp) => screen.getByText(text).closest(".pf-v6-c-alert") as HTMLElement;
const closeDialog = async (dialog: HTMLElement) => {
  fireEvent.click(footerClose(dialog));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
};
const viewProgress = (title: RegExp) => fireEvent.click(within(alertOf(title)).getByRole("button", { name: "View progress" }));
const gets = (calls: string[], path: string) => calls.filter((c) => c === `GET ${path}` || c.startsWith(`GET ${path}?`)).length;

const maasNone: MaaSStatus = {
  platform: { deployed: false, ready: false, managedByTool: false },
  parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }, { id: "database", title: "Database", state: "missing" }],
  subscription: "rhoai-nightly-updater-subscription",
};

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("leaving the Environment page while its run goes on", () => {
  const maas = maasNone;

  it("MaaS setup: View progress opens the same dialog, with its step list, on another page and after returning; the stream keeps updating it; the page reads again when it ends", async () => {
    const stream = sseResponse();
    const api = stubApi({
      "/api/resources/status": { minio: none, mlflow: none, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maas,
      "POST /api/resources/maas/stream": () => stream.response,
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=maas");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ })).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    await act(async () => stream.push(sseEvent("check", "running", "Reading what the platform needs")));
    expect(await within(run).findByText("Reading what the platform needs")).toBeInTheDocument();
    await closeDialog(run);

    // Off-route: the Environment UI unmounts while the background run keeps going.
    await leave();
    expect(screen.getByText("Another page")).toBeInTheDocument();
    viewProgress(/Set up Models as a Service is running/);
    const away = await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    // Expect the original step timeline, not whatever step the server last emitted.
    expect(within(away).getByLabelText("Set up Models as a Service: progress")).toBeInTheDocument();
    expect(within(away).getByText("Check what the platform needs")).toBeInTheDocument();
    expect(within(away).getByText("Red Hat Connectivity Link")).toBeInTheDocument();
    expect(within(away).queryByText(/This page did not start it/)).toBeNull();
    // Stream still open, so subsequent events should render inside the modal.
    await act(async () => {
      stream.push(sseEvent("check", "success", "Everything the platform needs is there."));
      stream.push(sseEvent("rhcl", "running", "Installing Red Hat Connectivity Link"));
    });
    expect(await within(away).findByText("Installing Red Hat Connectivity Link")).toBeInTheDocument();
    expect(within(away).getByText("Everything the platform needs is there.")).toBeInTheDocument();
    await closeDialog(away);

    // After navigating back, the page should reattach—one run, one dialog, no duplicate POST.
    await back("/test-resources?tab=maas");
    await settleApps();
    // The run's part shows busy, so Set up is not offered a second time.
    expect((await screen.findAllByText("Setting up")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: "Set up MaaS" })).toBeNull();
    viewProgress(/Set up Models as a Service is running/);
    const again = await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    expect(within(again).getByText("Installing Red Hat Connectivity Link")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/maas/stream"))).toHaveLength(1);

    // Finish with Environment visible: completion text in the modal and a fresh MaaS fetch.
    const readsBefore = gets(api.calls, "/api/resources/maas");
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "The MaaS platform is ready."));
      stream.close();
    });
    expect(await within(again).findByText("The MaaS platform is ready.")).toBeInTheDocument();
    await waitFor(() => expect(gets(api.calls, "/api/resources/maas")).toBeGreaterThan(readsBefore));
    expect(screen.queryByText(/Set up Models as a Service is running/)).toBeNull();
    await settleApps();
  });

  it("a run that ends while away: its outcome shows on the other page, Show details opens its dialog, and the page shows the new state on return", async () => {
    const stream = sseResponse();
    let platform = maas;
    stubApi({
      "/api/resources/status": { minio: none, mlflow: none, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": () => jsonResponse(platform),
      "POST /api/resources/maas/stream": () => stream.response,
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=maas");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ })).getByRole("button", { name: "Set up" }));
    await closeDialog(await screen.findByRole("dialog", { name: "Set up Models as a Service" }));
    await leave();

    platform = { ...maas, platform: { deployed: true, ready: true, managedByTool: true }, parts: maas.parts.map((p) => ({ ...p, state: "ready" as const })) };
    await act(async () => {
      stream.push(sseEvent("check", "success", "Everything the platform needs is there."));
      stream.push(sseEvent("operation_complete", "success", "The MaaS platform is ready."));
      stream.close();
    });
    const toast = (await screen.findByText("Set up Models as a Service: The MaaS platform is ready.")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(screen.queryByText(/Set up Models as a Service is running/)).toBeNull();
    fireEvent.click(within(toast).getByRole("button", { name: "Show details" }));
    const details = await screen.findByRole("dialog", { name: "Set up Models as a Service" });
    expect(within(details).getByLabelText("Set up Models as a Service: progress")).toBeInTheDocument();
    expect(within(details).getAllByText("The MaaS platform is ready.").length).toBeGreaterThan(0);
    await closeDialog(details);

    await back("/test-resources?tab=maas");
    await settleApps();
    expect(await screen.findByText("Ready, no model of this tool published")).toBeInTheDocument();
    await settleApps();
  });

  it("a Set up of several parts goes on while away: it reads the status itself, judges each part on it, sends the later parts' requests and finishes", async () => {
    // Mocked timers so readiness polling advances only when the test winds the clock.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
    const starting = (ns: string): ResourceState => ({ deployed: true, ready: false, managedByTool: true, namespace: ns, name: "dspa", message: "Starting (1/3 ready)" });
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: {} },
      "POST /api/resources/s3/setup": () => {
        status = { ...status, minio: { ...readyMinio, ready: false, message: "Starting" } };
        return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] });
      },
      "POST /api/resources/pipeline-server/setup": () => {
        status = { ...status, pipelineServers: [starting("dev")] };
        return jsonResponse({ success: true, message: "Pipeline server created.", logs: [] });
      },
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=pipelines&project=dev");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    expect(await within(run).findByText(/Created\. Starting \(typically under a minute\)/)).toBeInTheDocument();
    await closeDialog(run);
    await leave();

    // S3 becomes ready before the user returns to Environment.
    status = { ...status, minio: readyMinio };
    await act(() => vi.advanceTimersByTimeAsync(READY_CHECK_MS));
    await waitFor(() => expect(api.calls).toContain("POST /api/resources/pipeline-server/setup"));
    expect(screen.getByText(/^Set up Pipelines in dev is running: step 3 of 3/)).toBeInTheDocument();
    viewProgress(/Set up Pipelines in dev is running/);
    const away = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    expect(within(away).getByText("The S3 storage is ready.", { exact: false })).toBeInTheDocument();
    expect(await within(away).findByText(/Created\. Starting \(typically 1 to 3 minutes\)/)).toBeInTheDocument();

    // Pipeline server ready on the next status poll should close out the multi-step flow.
    status = { ...status, pipelineServers: [{ ...starting("dev"), ready: true, message: "Running" }] };
    await act(() => vi.advanceTimersByTimeAsync(READY_CHECK_MS));
    expect(await within(away).findByText("Pipelines are ready in dev.")).toBeInTheDocument();
    expect(within(away).getByText("The pipeline server in dev is ready.", { exact: false })).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources")).map((c) => c.split(" ")[1])).toEqual(["/api/resources/s3/setup", "/api/resources/pipeline-server/setup"]);
    await settleApps();
  });

  it("Migrate storage: View progress on another page opens the Migrate storage dialog, which keeps following the stream", async () => {
    const stream = sseResponse();
    const legacy: LegacyStorageState = {
      namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000",
      objects: ["Deployment seaweedfs"], dataPVCs: [{ name: "seaweedfs-pvc", size: "20Gi" }], usedBy: ["dev/dspa"],
      stage: "migrate", repoint: ["dev/dspa"], namespaceDeletable: false, namespaceKeptReason: "Namespace 'minio' is kept.",
    };
    const api = stubApi({
      "/api/resources/status": { minio: { ...none, namespace: "seaweedfs", migrationPending: true }, mlflow: none, pipelineServers: [], legacyStorage: legacy },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/migrate/stream": () => stream.response,
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=storage");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Migrate storage" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    await act(async () => stream.push(sseEvent("check", "running", "Reading the old storage")));
    expect(await within(dialog).findByText("Reading the old storage")).toBeInTheDocument();
    await closeDialog(dialog);
    await leave();

    viewProgress(/Migrate storage is running/);
    const away = await screen.findByRole("dialog", { name: "Migrate storage" });
    expect(within(away).getByLabelText("Migrate storage progress")).toBeInTheDocument();
    await act(async () => {
      stream.push(sseEvent("check", "success", "OK: the old storage is running"));
      stream.push(sseEvent("storage", "running", "Starting the new storage"));
    });
    expect(await within(away).findByText("Starting the new storage")).toBeInTheDocument();
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "Migrated 4 objects."));
      stream.close();
    });
    expect(await within(away).findByText("The data is in the new storage")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/s3/migrate/stream"))).toHaveLength(1);
    await closeDialog(away);
    await settleApps();
  });
});

describe("what a run of the Environment page leaves when the page closes", () => {
  const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running" };

  it("a confirmation open when the page is left (Set up, Clean up, Migrate storage) is cancelled: it does not open again on return", async () => {
    const legacy: LegacyStorageState = {
      namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"],
      dataPVCs: [], usedBy: [], stage: "migrate", repoint: [], namespaceDeletable: false, namespaceKeptReason: "kept",
    };
    // MLflow is there to clean up, and old storage to migrate; then a cluster with only the S3 storage, for Set up.
    let status: ResourcesStatus = { minio: { ...none, namespace: "seaweedfs", migrationPending: true }, mlflow: ownMlflow, pipelineServers: [], legacyStorage: legacy };
    stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
    });
    renderWithApp(<Shell />, {}, "/test-resources");
    await settleApps();
    const cases: [string, () => Promise<void>][] = [
      ["/test-resources", async () => { fireEvent.click(await screen.findByRole("button", { name: /^Clean up everything/ })); await screen.findByRole("dialog", { name: /Clean up everything\?/ }); }],
      ["/test-resources?tab=storage", async () => { fireEvent.click(await screen.findByRole("button", { name: "Migrate storage" })); await screen.findByRole("dialog", { name: "Migrate storage" }); }],
      ["/test-resources?tab=mlflow", async () => { fireEvent.click((await screen.findAllByRole("button", { name: /^Set up MLflow/ }))[0]); await screen.findByRole("dialog"); }],
    ];
    for (const [path, open] of cases) {
      if (path.endsWith("mlflow")) status = { minio: readyMinio, mlflow: none, pipelineServers: [] };
      // Each from a fresh visit, so the page reads the status above.
      await leave();
      await back(path);
      await settleApps();
      await open();
      await leave();
      await back(path);
      await settleApps();
      expect(await screen.findByRole("tab", { name: "Overview" })).toBeInTheDocument();
      expect(screen.queryByRole("dialog")).toBeNull();
    }
  });

  it("a Clean up refused for confirmation while away: a notification says so, and Open Clean up opens it on the Environment page with what to confirm", async () => {
    const stream = sseResponse();
    stubApi({
      "/api/resources/status": { minio: none, mlflow: ownMlflow, pipelineServers: [], recipeProjects: [{ name: "recipe-old", uid: "u2", recipe: "old", createdAt: "2026-11-22T05:20:00Z" }] },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/cleanup/stream": () => stream.response,
    });
    renderWithApp(<Shell />, {}, "/test-resources");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up everything/ }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Clean up everything\?/ })).getByRole("button", { name: "Delete" }));
    await closeDialog(await screen.findByRole("dialog", { name: "Clean up" }));
    await leave();
    await act(async () => {
      stream.push(sseEvent("recipe-project:recipe-old", "failed", "Project recipe-old still holds what this tool did not create.", { errorCode: "confirm_project_contents", detail: "Notebook wb" }));
      stream.push(sseEvent("operation_complete", "failed", "Project recipe-old still holds what this tool did not create. Nothing was deleted.", { errorCode: "confirm_project_contents" }));
      stream.close();
    });
    const toast = (await screen.findByText("Clean up needs your confirmation")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(toast).toHaveTextContent("Nothing was deleted.");
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(within(toast).getByRole("button", { name: "Open Clean up" }));
    const dialog = await screen.findByRole("dialog", { name: /Clean up everything\?/ });
    expect(within(dialog).getByText("Project recipe-old holds what this tool did not create")).toBeInTheDocument();
    expect(screen.queryByText("Clean up needs your confirmation")).toBeNull();
    await settleApps();
  });

  it("while a run goes on, other pages' changes are off; a request answered cluster_busy reads the operation again (the app's handler, not the page's)", async () => {
    const stream = sseResponse();
    const refused = deferred<Response>();
    let operationReads = 0;
    const busy = { error: "Another operation is changing this cluster.", errorCode: "cluster_busy" };
    stubApi({
      "/api/resources/status": { minio: none, mlflow: ownMlflow, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maasNone,
      "POST /api/resources/maas/stream": () => stream.response,
      "POST /api/resources/mlflow/teardown": () => refused.promise,
    });
    const operation = async (): Promise<OperationStatusResponse> => { operationReads += 1; return { inProgress: false, operation: null }; };
    renderWithApp(<Shell />, { operation }, "/test-resources?tab=maas");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ })).getByRole("button", { name: "Set up" }));
    await closeDialog(await screen.findByRole("dialog", { name: "Set up Models as a Service" }));
    await leave();
    expect(screen.getByText("Changes: Set up Models as a Service is still running. Wait for it to finish.")).toBeInTheDocument();
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "The MaaS platform is ready."));
      stream.close();
    });
    expect(await screen.findByText("Changes: allowed")).toBeInTheDocument();

    await back("/test-resources?tab=mlflow");
    await settleApps();
    const before = operationReads;
    fireEvent.click(await screen.findByRole("button", { name: "MLflow server actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Delete MLflow/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    await screen.findByRole("dialog", { name: "Delete MLflow" });
    await act(async () => refused.resolve(jsonResponse(busy, 409)));
    await settleApps();
    expect(operationReads).toBeGreaterThan(before);
  });

  it("a Set up away from its page reads the status at the page's pace, and stops when the session ended; nothing draws over the sign-in screen", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
    let expired = false;
    const api = stubApi({
      "/api/resources/status": () => (expired ? jsonResponse({ error: "Your OpenShift session has expired.", errorCode: "session_expired" }, 401) : jsonResponse(status)),
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: {} },
      "POST /api/resources/s3/setup": () => {
        status = { ...status, minio: { ...readyMinio, ready: false, message: "Starting" } };
        return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] });
      },
      "POST /api/resources/pipeline-server/setup": () => jsonResponse({ success: true, message: "created", logs: [] }),
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=pipelines&project=dev");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    expect(await within(run).findByText(/Created\. Starting \(typically under a minute\)/)).toBeInTheDocument();
    await closeDialog(run);
    await leave();
    const reads = () => gets(api.calls, "/api/resources/status");
    const atLeave = reads();
    // Two ready checks: the page polls no sooner than RESOURCE_POLL_BASE_MS, nor does the run.
    await act(() => vi.advanceTimersByTimeAsync(2 * READY_CHECK_MS));
    expect(reads()).toBe(atLeave);
    await act(() => vi.advanceTimersByTimeAsync(RESOURCE_POLL_BASE_MS));
    expect(reads()).toBe(atLeave + 1);
    // Then backing off: one more read in the next 8 s, not four.
    await act(() => vi.advanceTimersByTimeAsync(4 * READY_CHECK_MS));
    expect(reads()).toBeLessThanOrEqual(atLeave + 2);

    // The session ends: the run stops at its next read instead of waiting out the 10 minutes.
    expired = true;
    await act(() => vi.advanceTimersByTimeAsync(4 * RESOURCE_POLL_BASE_MS));
    await waitFor(() => expect(screen.queryByText(/Set up Pipelines in dev is running/)).toBeNull());
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/pipeline-server"))).toEqual([]);
    // Its outcome waits behind the sign-in screen: no notification or dialog over it.
    expect(screen.queryByText(/^Set up Pipelines in dev: /)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    await settleApps();
  });

  it("Migrate storage that ends with its dialog closed shows its outcome, with Show details; a new run replaces an old outcome", async () => {
    const stream = sseResponse();
    const teardown = deferred<Response>();
    const legacy: LegacyStorageState = {
      namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"],
      dataPVCs: [], usedBy: [], stage: "migrate", repoint: [], namespaceDeletable: false, namespaceKeptReason: "kept",
    };
    let teardowns = 0;
    stubApi({
      "/api/resources/status": { minio: { ...none, namespace: "seaweedfs", migrationPending: true }, mlflow: ownMlflow, pipelineServers: [], legacyStorage: legacy },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/migrate/stream": () => stream.response,
      "POST /api/resources/mlflow/teardown": () => (++teardowns === 1 ? teardown.promise : new Promise<Response>(() => {})),
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=storage");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Migrate storage" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    await closeDialog(dialog);
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "Migrated 4 objects."));
      stream.close();
    });
    const toast = (await screen.findByText("Migrate storage: The data is in the new storage")).closest(".pf-v6-c-alert") as HTMLElement;
    fireEvent.click(within(toast).getByRole("button", { name: "Show details" }));
    const details = await screen.findByRole("dialog", { name: "Migrate storage" });
    expect(within(details).getByText("Migrated 4 objects.")).toBeInTheDocument();
    await closeDialog(details);

    // A run that fails with its dialog closed: its outcome stays until the next run starts.
    await back("/test-resources?tab=mlflow");
    await settleApps();
    const deleteMlflow = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "MLflow server actions" }));
      fireEvent.click(await screen.findByRole("menuitem", { name: /Delete MLflow/ }));
      fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
      await closeDialog(await screen.findByRole("dialog", { name: "Delete MLflow" }));
    };
    await deleteMlflow();
    await act(async () => teardown.resolve(jsonResponse({ success: false, message: "MLflow is in use.", logs: [] })));
    expect(await screen.findByText("Delete MLflow: stopped")).toBeInTheDocument();
    await deleteMlflow();
    expect(screen.queryByText("Delete MLflow: stopped")).toBeNull();
  });

  it("a Set up's next step opens the project it ran in, not the one the tab shows now", () => {
    const dspa = (ns: string): ResourceState => ({ deployed: true, ready: true, managedByTool: true, namespace: ns, name: "dspa", message: "Running", uiRoute: `https://rh-ai.apps.example.com/pipelines/${ns}` });
    const status: ResourcesStatus = { minio: readyMinio, mlflow: none, pipelineServers: [dspa("dev"), dspa("other")] };
    const run: GoalRun = { title: "Set up Pipelines in dev", kind: "setup", startedAt: 0, steps: [], states: {}, running: false, result: { success: true, message: "Pipelines are ready in dev." } };
    const follow = runFollowUp({ run, runFeature: "pipelines", runProject: "dev", sampleRun: null, modelsRun: null, playgroundRun: null, maasRun: null,
      status, pg: null, busy: IDLE, projectNames: ["dev", "other"], mlflowNow: mlflowState(status), maasNow: maasState(null) });
    expect(follow.open).toMatchObject({ kind: "open", href: "https://rh-ai.apps.example.com/pipelines/dev" });
  });
});

describe("review round 3", () => {
  const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running" };
  const legacyOf = (stage: "migrate" | "remove"): LegacyStorageState => ({
    namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: ["Deployment seaweedfs"],
    dataPVCs: [], usedBy: [], stage, repoint: [], notMoved: [], namespaceDeletable: true, otherObjects: [],
  });

  it("a finished migration does not keep a later Delete old storage confirmation open across leaving the page", async () => {
    const stream = sseResponse();
    let status: ResourcesStatus = { minio: { ...readyMinio, migrationPending: true }, mlflow: none, pipelineServers: [], legacyStorage: legacyOf("migrate") };
    stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/migrate/stream": () => stream.response,
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=storage");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Migrate storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Migrate storage" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate storage" }));
    await closeDialog(dialog);
    status = { ...status, legacyStorage: legacyOf("remove") };
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "Migrated 4 objects."));
      stream.close();
    });
    expect(await screen.findByText("Migrate storage: The data is in the new storage")).toBeInTheDocument();
    expect(await screen.findByText(/The old storage can be deleted/)).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "SeaweedFS actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Delete old storage/ }));
    await screen.findByRole("dialog", { name: /Delete the old storage\?/ });
    await leave();
    await back("/test-resources?tab=storage");
    await settleApps();
    expect(await screen.findByRole("tab", { name: "S3 storage" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a streamed run refused cluster_busy after the page was left reads the operation again", async () => {
    const refused = deferred<Response>();
    let operationReads = 0;
    stubApi({
      "/api/resources/status": { minio: none, mlflow: ownMlflow, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/cleanup/stream": () => refused.promise,
    });
    const operation = async (): Promise<OperationStatusResponse> => { operationReads += 1; return { inProgress: false, operation: null }; };
    renderWithApp(<Shell />, { operation }, "/test-resources?tab=mlflow");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: /^Clean up MLflow/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    await closeDialog(await screen.findByRole("dialog", { name: "Clean up" }));
    await leave();
    await settleApps();
    const before = operationReads;
    await act(async () => refused.resolve(jsonResponse({ error: "Another operation is changing this cluster.", errorCode: "cluster_busy" }, 409)));
    await settleApps();
    expect(operationReads).toBeGreaterThan(before);
  });

  it("each part's request starts the run's reads at the quick end again: a long wait for one part does not delay the next part's first read", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [] };
    const dspa: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "dspa", message: "Running" };
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: {} },
      "POST /api/resources/s3/setup": () => {
        status = { ...status, minio: { ...readyMinio, ready: false, message: "Starting" } };
        return jsonResponse({ success: true, message: "S3 storage deployed.", logs: [] });
      },
      // Ready at once: only the run's next read tells it.
      "POST /api/resources/pipeline-server/setup": () => {
        status = { ...status, pipelineServers: [dspa] };
        return jsonResponse({ success: true, message: "created", logs: [] });
      },
    });
    renderWithApp(<Shell />, {}, "/test-resources?tab=pipelines&project=dev");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    expect(await within(run).findByText(/Created\. Starting \(typically under a minute\)/)).toBeInTheDocument();
    await closeDialog(run);
    await leave();
    const tick = () => act(() => vi.advanceTimersByTimeAsync(READY_CHECK_MS));
    const posted = () => api.calls.includes("POST /api/resources/pipeline-server/setup");
    // The storage takes a while: the run's reads back off (5, 10, 20 s apart) before it is ready.
    for (let t = 0; t < 40_000; t += READY_CHECK_MS) await tick();
    status = { ...status, minio: readyMinio };
    for (let t = 0; t < 60_000 && !posted(); t += READY_CHECK_MS) await tick();
    expect(posted()).toBe(true);
    // The pipeline server's first read comes at the quick end, not after the storage's long backoff.
    for (let t = 0; t < RESOURCE_POLL_BASE_MS + 2 * READY_CHECK_MS; t += READY_CHECK_MS) await tick();
    expect(screen.queryByText(/Set up Pipelines in dev is running/)).toBeNull();
    expect(await screen.findByText("Set up Pipelines in dev: Pipelines are ready in dev.")).toBeInTheDocument();
    await settleApps();
  });
});

describe("the screens that replace every page", () => {
  // As AppLayout: one of these screens instead of the page.
  const Screens: React.FC = () => {
    const s = useAppScreens();
    if (s.sessionExpired) return <p>Signed out screen</p>;
    if (s.adminRequired) return <p>No access screen</p>;
    if (s.permissionsUnknown) return <p>Permissions unknown screen</p>;
    return <Shell />;
  };
  const admin: UserPermissions = { canMutate: true, role: "admin", user: "me" } as UserPermissions;
  const gitops: VersionInfo = { version: "dev", commit: "unknown", buildDate: "unknown", templateOutdated: false, profile: "gitops" } as VersionInfo;

  it.each<[string, () => Promise<UserPermissions>, string]>([
    ["the session ends", async () => { throw new ApiError({ status: 401, errorCode: "session_expired", message: "Your OpenShift session has expired." }); }, "Signed out screen"],
    ["the admin check now says no", async () => ({ canMutate: false, role: "user", user: "me" } as UserPermissions), "No access screen"],
    ["the admin check cannot answer", async () => { throw new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot check." }); }, "Permissions unknown screen"],
  ])("when %s, the run's dialog and its outcome do not draw over the screen", async (_name, nextAnswer, screenText) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const stream = sseResponse();
    stubApi({
      "/api/resources/status": { minio: none, mlflow: none, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/maas": maasNone,
      "POST /api/resources/maas/stream": () => stream.response,
    });
    let answer: () => Promise<UserPermissions> = async () => admin;
    renderWithApp(<Screens />, { permissions: () => answer(), version: async () => gitops }, "/test-resources?tab=maas");
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Set up MaaS" }));
    fireEvent.click(within(await screen.findByRole("dialog", { name: /Set up Models as a Service\?/ })).getByRole("button", { name: "Set up" }));
    expect(await screen.findByRole("dialog", { name: "Set up Models as a Service" })).toBeInTheDocument();
    // The next permission check (every 10 minutes) answers differently.
    answer = nextAnswer;
    await act(() => vi.advanceTimersByTimeAsync(10 * 60_000));
    await settleApps();
    expect(screen.getByText(screenText)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    await act(async () => {
      stream.push(sseEvent("operation_complete", "success", "The MaaS platform is ready."));
      stream.close();
    });
    await settleApps();
    expect(screen.queryByText(/^Set up Models as a Service: /)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
