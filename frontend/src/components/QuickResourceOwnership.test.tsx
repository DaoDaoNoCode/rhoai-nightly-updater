import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QuickResourceCreator, nextStep } from "./QuickResourceCreator";
import { stubApi, jsonResponse } from "../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../types";

// The live cluster's state on 2026-10-05 (GET /api/resources/status, B3).
const brokenMinio: ResourceState = {
  deployed: true, ready: false, namespace: "minio", managedByTool: true,
  message: "ImagePullBackOff: Back-off pulling image \"quay.io/minio/minio:latest\": unauthorized",
  waitingReason: "ImagePullBackOff", terminalError: true, dataPVCs: ["minio-pvc"],
  teardownBlockedReason: "1 pipeline server(s) use this MinIO: juntao-test/dspa. Tear them down first.",
};
const browserMlflow: ResourceState = {
  deployed: true, ready: true, message: "Running", namespace: "redhat-ods-applications", managedByTool: false,
  currentImage: "quay.io/opendatahub/mlflow:odh-stable",
  teardownBlockedReason: "This MLflow instance was not created by this tool, so the tool will not delete it.",
};
const legacyDspa: ResourceState = {
  deployed: true, ready: false, namespace: "juntao-test", name: "dspa", managedByTool: true, terminalError: true,
  message: "Could not connect to (minio-service.minio.svc:9000): connection refused", dataPVCs: ["mariadb-dspa"],
};

function live(over: Partial<ResourcesStatus> = {}): ResourcesStatus {
  return { minio: brokenMinio, mlflow: browserMlflow, pipelineServers: [legacyDspa], unmanagedPipelineProjects: ["team-x"], ...over };
}

function setup(status: ResourcesStatus, extra: Record<string, unknown> = {}) {
  return stubApi({
    "/api/resources/status": status,
    "/api/resources/projects": { projects: ["juntao-test", "team-x", "fresh"] },
    ...extra,
  });
}

describe("terminal states (A08-3, A06-5)", () => {
  it("MinIO in ImagePullBackOff shows the reason and the next step, offers Repair, and explains why teardown is blocked", async () => {
    setup(live());
    render(<QuickResourceCreator mutateBlocker={null} />);
    expect(await screen.findByText("Failed: ImagePullBackOff")).toBeInTheDocument();
    expect(screen.getByText(/Repair applies the MinIO Deployment again/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Repair" })).toBeInTheDocument();
    expect(screen.getByText(/Tear down is blocked: 1 pipeline server\(s\) use this MinIO/)).toBeInTheDocument();
    const minioTeardown = within(screen.getByRole("list", { name: "Storage" })).getByRole("button", { name: "Tear down" });
    expect(minioTeardown).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByText("Pipeline servers need MinIO, which is not ready (ImagePullBackOff)")).toBeInTheDocument();
    // The pipeline server cannot reach MinIO: say so instead of spinning.
    expect(screen.getByText(/cannot reach MinIO. Fix MinIO first/)).toBeInTheDocument();
    expect(screen.queryByText("Starting")).not.toBeInTheDocument();
  });

  it("nextStep per reason", () => {
    expect(nextStep("mlflow", { deployed: true, ready: false, waitingReason: "CrashLoopBackOff", terminalError: true, prOverride: true })).toMatch(/PR build keeps crashing. Revert/);
    expect(nextStep("minio", { deployed: true, ready: false, waitingReason: "CreateContainerConfigError", terminalError: true })).toMatch(/Secret or ConfigMap/);
    expect(nextStep("minio", { deployed: true, ready: false, message: "Provisioning" })).toBeNull();
  });
});

describe("ownership", () => {
  it("a browser-created MLflow is 'not managed by this tool' and has no Tear down", async () => {
    setup(live());
    render(<QuickResourceCreator mutateBlocker={null} />);
    const mlflow = await screen.findByRole("list", { name: "MLflow" });
    await within(mlflow).findByText("Not managed by this tool");
    expect(within(mlflow).getByText(/was not created by this tool/)).toBeInTheDocument();
    expect(within(mlflow).queryByRole("button", { name: "Tear down" })).not.toBeInTheDocument();
  });

  it("projects with someone else's pipeline server are not offered, and say why", async () => {
    setup(live({ minio: { deployed: true, ready: true, managedByTool: true, namespace: "minio" } }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    expect(await screen.findByText(/already have a pipeline server this tool did not create: team-x/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add to a project" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual(["fresh"]);
  });
});

describe("MLflow PR override (A04-5)", () => {
  it("Revert and Deploy PR stay available when the PR image broke MLflow", async () => {
    setup(live({ mlflow: { deployed: true, ready: false, managedByTool: true, prOverride: true, prNumber: 399, revertImage: "", waitingReason: "CrashLoopBackOff", terminalError: true, message: "CrashLoopBackOff" } }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const mlflow = await screen.findByRole("list", { name: "MLflow" });
    expect(within(mlflow).getByText("PR #399")).toBeInTheDocument();
    expect(within(mlflow).getByRole("button", { name: "Revert" })).not.toHaveAttribute("aria-disabled");
    expect(within(mlflow).getByRole("button", { name: "Deploy PR" })).toBeInTheDocument();
    expect(within(mlflow).getByRole("button", { name: "Tear down" })).toBeInTheDocument();
    fireEvent.click(within(mlflow).getByRole("button", { name: "Revert" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/so the operator's default \(RHOAI\) image is used again/)).toBeInTheDocument();
  });

  it("deploying a PR on an MLflow the tool did not create warns in the confirmation", async () => {
    setup(live());
    render(<QuickResourceCreator mutateBlocker={null} />);
    const mlflow = await screen.findByRole("list", { name: "MLflow" });
    fireEvent.change(within(mlflow).getByRole("textbox", { name: /mlflow PR number/ }), { target: { value: "42" } });
    fireEvent.click(within(mlflow).getByRole("button", { name: "Deploy PR" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("quay.io/opendatahub/mlflow:odh-pr-42")).toBeInTheDocument();
    expect(within(dialog).getByText("This MLflow instance was not created by this tool")).toBeInTheDocument();
  });
});

describe("teardown confirmations state the data loss", () => {
  it("pipeline server teardown names the DSPA and the database PVC; in_progress is info and keeps checking", async () => {
    const api = setup(live({ minio: { deployed: true, ready: true, managedByTool: true, namespace: "minio" } }), {
      "POST /api/resources/pipeline-server/teardown": () => jsonResponse({ success: false, message: "The DSPA is still being deleted by the pipelines operator.", logs: [], errorCode: "in_progress" }, 422),
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const list = await screen.findByRole("list", { name: "Pipeline servers" });
    fireEvent.click(within(list).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered")).toBeInTheDocument();
    expect(within(dialog).getByText("mariadb-dspa")).toBeInTheDocument();
    expect(within(dialog).getByText("dspa")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down" }));
    expect(await screen.findByText("Still in progress")).toBeInTheDocument();
    expect(screen.getByText(/This page keeps checking/)).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/pipeline-server/teardown"]).toEqual([{ project: "juntao-test" }]);
  });

  it("a conversion-webhook refusal is shown with the server's reason", async () => {
    setup(live({ minio: { ...brokenMinio, teardownBlockedReason: undefined }, pipelineServers: [] }), {
      "POST /api/resources/minio/teardown": () => jsonResponse({ success: false, errorCode: "prerequisites", logs: [], message: "Refusing to delete namespace minio: CRD mcpservers.mcp.x-k8s.io has a conversion webhook whose Service redhat-ods-applications/mcp-lifecycle-operator-webhook-service is missing." }, 422),
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    await waitFor(() => expect(within(storage).getByRole("button", { name: "Tear down" })).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(within(storage).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("minio-pvc")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down MinIO" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText(/mcpservers.mcp.x-k8s.io has a conversion webhook/)).toBeInTheDocument();
  });
});
