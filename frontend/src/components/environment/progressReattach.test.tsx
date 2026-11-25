import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes, useNavigate } from "react-router-dom";
import { GlobalBanners } from "../GlobalBanners";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { renderWithApp, settleApps } from "../../test/providers";
import { jsonResponse, stubApi } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { LegacyStorageState, MaaSStatus, ResourceState, ResourcesStatus } from "../../types";
import { READY_CHECK_MS } from "./useGoalRun";

// Routing narrowed to the Environment screen plus one alternate destination, with
// helpers so the spec can switch paths the way the sidebar would.
let go: (to: string) => void = () => {};
const Navigator: React.FC = () => {
  const navigate = useNavigate();
  go = (to) => { void navigate(to); };
  return null;
};
const Shell: React.FC = () => (
  <>
    <GlobalBanners />
    <Navigator />
    <Routes>
      <Route path="/test-resources" element={<QuickResourceCreator mutateBlocker={null} />} />
      <Route path="/components" element={<p>Another page</p>} />
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

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("leaving the Environment page while its run goes on", () => {
  const maas: MaaSStatus = {
    platform: { deployed: false, ready: false, managedByTool: false },
    parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "missing" }, { id: "database", title: "Database", state: "missing" }],
    subscription: "rhoai-nightly-updater-subscription",
  };

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
