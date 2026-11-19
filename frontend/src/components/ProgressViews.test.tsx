import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { QuickResourceCreator } from "./QuickResourceCreator";
import { renderWithApp } from "../test/providers";
import { jsonResponse, stubApi } from "../test/apiStub";
import type { OperationStatusResponse, ResourceState, ServerOperation } from "../types";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", dataPVCs: ["seaweedfs-data"], namespaceDeletable: true };
const ownMlflow: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "redhat-ods-applications", message: "Running", uiRoute: "https://rh-ai.apps.example.com/mlflow" };
const footerClose = (dialog: HTMLElement) => within(dialog).getAllByRole("button", { name: "Close" }).find((b) => b.textContent === "Close")!;
const alertOf = (text: RegExp) => screen.getByText(text).closest(".pf-v6-c-alert") as HTMLElement;

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("View progress: a run of this page whose dialog was closed", () => {
  it("the operation alert names the run, View progress reopens the same dialog with its steps, focus goes in and back, and the end shows there", async () => {
    let finish: (r: Response) => void = () => {};
    stubApi({
      "/api/resources/status": { minio: readyMinio, mlflow: ownMlflow, pipelineServers: [] },
      "/api/resources/projects": { projects: ["a"] },
      "POST /api/resources/mlflow/teardown": () => new Promise<Response>((r) => { finish = r; }),
    });
    renderWithApp(<><GlobalBanners /><QuickResourceCreator mutateBlocker={null} /></>, {}, "/test-resources?tab=mlflow");
    fireEvent.click(await screen.findByRole("button", { name: "MLflow server actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Delete MLflow/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    const run = await screen.findByRole("dialog", { name: "Delete MLflow" });
    expect(within(run).getByText("Working...")).toBeInTheDocument();
    fireEvent.click(footerClose(run));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // Closed: the alert says what runs, and offers its progress.
    const alert = alertOf(/Delete MLflow is running/);
    expect(alert).toHaveTextContent("Closing its dialog does not stop it.");
    const view = within(alert).getByRole("button", { name: "View progress" });
    fireEvent.click(view);
    const again = await screen.findByRole("dialog", { name: "Delete MLflow" });
    // The same run: its step state was kept while the dialog was closed.
    expect(within(again).getByText("Working...")).toBeInTheDocument();
    await waitFor(() => expect(again).toContainElement(document.activeElement as HTMLElement));

    fireEvent.click(footerClose(again));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(document.activeElement).toBe(within(alertOf(/Delete MLflow is running/)).getByRole("button", { name: "View progress" })));

    // Opened again, the dialog follows the run to its end; the alert goes away.
    fireEvent.click(within(alertOf(/Delete MLflow is running/)).getByRole("button", { name: "View progress" }));
    const third = await screen.findByRole("dialog", { name: "Delete MLflow" });
    await act(async () => finish(jsonResponse({ success: true, message: "MLflow removed.", logs: [] })));
    expect(await within(third).findByText("MLflow removed.")).toBeInTheDocument();
    expect(screen.queryByText(/Delete MLflow is running/)).toBeNull();
  });

  it("a run of several steps names the step it is on", async () => {
    let finish: (r: Response) => void = () => {};
    stubApi({
      "/api/resources/status": { minio: { deployed: false, ready: false, managedByTool: false, message: "Not deployed" }, mlflow: ownMlflow, pipelineServers: [] },
      "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/prerequisites": { features: {} },
      "POST /api/resources/s3/setup": () => new Promise<Response>((r) => { finish = r; }),
    });
    renderWithApp(<><GlobalBanners /><QuickResourceCreator mutateBlocker={null} /></>, {}, "/test-resources?tab=pipelines&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Set up Pipelines in dev" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Set up" }));
    const run = await screen.findByRole("dialog", { name: "Set up Pipelines in dev" });
    await within(run).findByText("Creating...");
    fireEvent.click(footerClose(run));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByText(/^Set up Pipelines in dev is running: step 2 of 3, Set up the S3 storage \(\d+s\)$/)).toBeInTheDocument();
    await act(async () => finish(jsonResponse({ success: false, message: "Refused.", errorCode: "validation", logs: [] })));
  });
});

describe("View progress: an operation started elsewhere", () => {
  const cleanup: ServerOperation = {
    id: "op-7", type: "cleanup", label: "Clean up", user: "qa-user", startedAt: new Date(Date.now() - 120_000).toISOString(),
    updatedAt: new Date().toISOString(), step: "storage", stepStatus: "running", message: "Deleting the S3 storage",
  };

  it("opens a read-only view of what the server records, which says how it ended", async () => {
    // A fake clock from the start, so the app's operation poll runs when the test says.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let answer: OperationStatusResponse = { inProgress: true, operation: cleanup };
    renderWithApp(<GlobalBanners />, { operation: async () => answer }, "/components");
    const alert = (await screen.findByText(/qa-user is running "Clean up"/)).closest(".pf-v6-c-alert") as HTMLElement;
    // Its page is offered too.
    expect(within(alert).getByRole("button", { name: "Open Environment" })).toBeInTheDocument();
    fireEvent.click(within(alert).getByRole("button", { name: "View progress" }));
    const dialog = await screen.findByRole("dialog", { name: "Clean up" });
    expect(dialog).toHaveTextContent("Started byqa-user");
    expect(dialog).toHaveTextContent("Latest progressDeleting the S3 storage.");
    expect(dialog).toHaveTextContent("the server keeps only the latest progress");
    expect(within(dialog).getByText("Running")).toBeInTheDocument();

    // It ends: the app's next operation poll (fake clock) brings lastCompleted.
    answer = { inProgress: false, operation: null, lastCompleted: { ...cleanup, finishedAt: new Date().toISOString(), success: true, message: "Deleted the S3 storage" } };
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(within(dialog).getByText("It finished")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("Deleted the S3 storage.");
    expect(dialog).toHaveTextContent("StateFinished");
    fireEvent.click(footerClose(dialog));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("a streamed operator operation keeps its link to the Status page, where its steps are", async () => {
    const update: ServerOperation = { ...cleanup, id: "op-8", type: "update", label: "Update to nightly", step: "verify_installplan" };
    renderWithApp(<GlobalBanners />, { operation: async () => ({ inProgress: true, operation: update }) }, "/components");
    const alert = (await screen.findByText(/qa-user is running "Update to nightly"/)).closest(".pf-v6-c-alert") as HTMLElement;
    expect(within(alert).getAllByRole("button", { name: "View progress" })).toHaveLength(1);
    fireEvent.click(within(alert).getByRole("button", { name: "View progress" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
