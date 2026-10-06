import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QuickResourceCreator, nextStep } from "./QuickResourceCreator";
import { stubApi, jsonResponse } from "../test/apiStub";
import type { ResourceState, ResourcesStatus } from "../types";

// A SeaweedFS whose image cannot be pulled, used by a pipeline server
// (GET /api/resources/status).
const brokenMinio: ResourceState = {
  deployed: true, ready: false, namespace: "minio", managedByTool: true,
  message: "ImagePullBackOff: Back-off pulling image \"mirror.example.com/seaweedfs:4.48\": unauthorized",
  waitingReason: "ImagePullBackOff", terminalError: true, dataPVCs: ["seaweedfs-pvc"], uiUser: "admin",
  teardownBlockedReason: "1 pipeline server uses this S3 storage: juntao-test/dspa. Tear it down first.",
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
  it("S3 storage in ImagePullBackOff shows the reason and the next step, offers Repair, and explains why teardown is blocked", async () => {
    setup(live());
    render(<QuickResourceCreator mutateBlocker={null} />);
    expect(await screen.findByText("Failed: ImagePullBackOff")).toBeInTheDocument();
    expect(screen.getByText(/Repair applies the SeaweedFS Deployment again/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Repair" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Migrate to SeaweedFS" })).not.toBeInTheDocument();
    expect(screen.getByText(/Tear down is blocked: 1 pipeline server uses this S3 storage/)).toBeInTheDocument();
    const minioTeardown = within(screen.getByRole("list", { name: "Storage" })).getByRole("button", { name: "Tear down" });
    expect(minioTeardown).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByText("Pipeline servers need the S3 storage, which is not ready (ImagePullBackOff)")).toBeInTheDocument();
    // The pipeline server cannot reach the storage: say so instead of spinning.
    expect(screen.getByText(/cannot reach the S3 storage. Fix the storage first/)).toBeInTheDocument();
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
    expect(within(mlflow).getByRole("button", { name: "Revert the PR image" })).not.toHaveAttribute("aria-disabled");
    expect(within(mlflow).getByRole("button", { name: "Deploy PR" })).toBeInTheDocument();
    expect(within(mlflow).getByRole("button", { name: "Tear down" })).toBeInTheDocument();
    fireEvent.click(within(mlflow).getByRole("button", { name: "Revert the PR image" }));
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
    expect(within(dialog).getAllByText("seaweedfs-pvc").length).toBeGreaterThan(0);
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText(/mcpservers.mcp.x-k8s.io has a conversion webhook/)).toBeInTheDocument();
  });
});

describe("S3 storage after FXB (kept namespace, warning, teardown codes)", () => {
  const kept = "S3 storage removed. PVC seaweedfs-pvc and all stored objects (pipeline artifacts, models, test files) are deleted with it. Namespace 'minio' was kept: it may hold objects this tool did not create. Delete it with `oc delete project minio` once you've checked it's empty.";
  const afterTeardown: ResourceState = { deployed: false, ready: false, namespace: "minio", managedByTool: true, message: "Namespace exists but the S3 storage is not deployed" };
  const running: ResourceState = { deployed: true, ready: true, namespace: "minio", managedByTool: true, message: "Running", dataPVCs: ["seaweedfs-pvc"], uiUser: "admin", uiRoute: "https://minio-ui-minio.apps.example.com" };

  it("shows the warning of a running S3 storage, the admin UI link and its login", async () => {
    const warning = "SeaweedFS runs ghcr.io/chrislusf/seaweedfs:4.47, not the image this version deploys. Re-run setup to update it; the data PVC is kept.";
    setup(live({ minio: { ...running, warning }, pipelineServers: [] }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    expect(await within(storage).findByText(warning)).toBeInTheDocument();
    expect(within(storage).getByText("S3 storage")).toBeInTheDocument();
    expect(within(storage).getByText(/a name kept from MinIO for compatibility/)).toBeInTheDocument();
    expect(within(storage).getByRole("link", { name: /Open admin UI/ })).toHaveAttribute("href", "https://minio-ui-minio.apps.example.com");
    expect(within(storage).getByText(/Admin UI login: user/)).toHaveTextContent("Admin UI login: user admin, password in Secret minio-secret (key minio_root_password)");
  });

  it("after a teardown the kept namespace is 'Not deployed' with Setup only, and the result names oc delete project", async () => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: [] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: true, message: kept, logs: [] }); },
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    const teardown = await within(storage).findByRole("button", { name: "Tear down" });
    fireEvent.click(teardown);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/Namespace minio is kept/);
    expect(within(dialog).queryByText(/deleted with everything in it/)).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Tear down S3 storage" }));
    expect((await screen.findByText(/^S3 storage removed/)).closest(".pf-v6-c-alert")).toHaveTextContent(/was kept: run oc delete project minio once you have checked it is empty/);
    expect(await within(storage).findByText("Not deployed")).toBeInTheDocument();
    expect(within(storage).queryByText("Running")).not.toBeInTheDocument();
    expect(within(storage).queryByText(/^Failed/)).not.toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Set up" })).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Tear down" })).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(1);
  });

  it.each([
    ["partial_failure", "Partly done", "Some S3 storage objects could not be removed: Secret minio-secret: forbidden. Re-run teardown to retry."],
    ["delete_failed", "Nothing could be deleted", "Some S3 storage objects could not be removed: Deployment seaweedfs: forbidden."],
    ["in_progress", "Still in progress", "The S3 storage objects were deleted, but PVC seaweedfs-pvc is still terminating after 1m0s; re-run teardown to check again."],
  ])("a %s teardown keeps Tear down available next to Setup", async (code, title, message) => {
    let status: ResourceState = running;
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(live({ minio: status, pipelineServers: [] })),
      "/api/resources/projects": { projects: [] },
      "POST /api/resources/minio/teardown": () => { status = afterTeardown; return jsonResponse({ success: false, errorCode: code, message, logs: [] }, 422); },
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    fireEvent.click(await within(storage).findByRole("button", { name: "Tear down" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Tear down S3 storage" }));
    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(message.slice(0, 30).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
    expect(await within(storage).findByText("Not deployed")).toBeInTheDocument();
    expect(within(storage).getByRole("button", { name: "Set up" })).toBeInTheDocument();
    fireEvent.click(within(storage).getByRole("button", { name: "Tear down" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Tear down S3 storage" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/teardown"))).toHaveLength(2));
  });

  it("a kept namespace that still reports the data PVC offers Tear down without an earlier failure in this tab", async () => {
    setup(live({ minio: { ...afterTeardown, dataPVCs: ["minio-pvc"] }, pipelineServers: [] }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    expect(await within(storage).findByRole("button", { name: "Tear down" })).toBeInTheDocument();
    expect(within(storage).getByText("Not deployed")).toBeInTheDocument();
  });
});

describe("migration from MinIO (start fresh)", () => {
  // An earlier version's MinIO that still serves a pipeline server.
  const pendingMinio: ResourceState = {
    deployed: true, ready: true, namespace: "minio", managedByTool: true, message: "Running", migrationPending: true,
    dataPVCs: ["minio-pvc"], uiRoute: "https://minio-ui-minio.apps.example.com",
    currentImage: "quay.io/hummingbird-community/minio@sha256:25268b5a6539d9ffc7d23b89a2ba846d12a49aac4e81172336700222818d5f45",
    warning: "MinIO from an earlier version of this tool still runs here and serves the pipeline servers. Re-run setup to replace it with SeaweedFS.",
    teardownBlockedReason: "1 pipeline server uses this S3 storage: juntao-test/dspa. Tear it down first.",
  };

  it("offers Migrate to SeaweedFS with a confirmation that explains starting fresh, then runs setup", async () => {
    const api = setup(live({ minio: pendingMinio, pipelineServers: [] }), {
      "POST /api/resources/minio/setup": () => jsonResponse({ success: true, message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'. MinIO from the earlier version was replaced.", logs: [] }),
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    expect(await within(storage).findByText("MinIO (migration pending)")).toBeInTheDocument();
    expect(within(storage).queryByText("SeaweedFS")).not.toBeInTheDocument();
    expect(within(storage).getByText(/Migrate to SeaweedFS replaces this MinIO/)).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Repair" })).not.toBeInTheDocument();
    expect(within(storage).getByRole("link", { name: /Open MinIO console/ })).toBeInTheDocument();
    // Pipeline servers can still be added: MinIO serves them until the migration.
    expect(screen.getByRole("button", { name: "Add to a project" })).not.toHaveAttribute("aria-disabled");

    fireEvent.click(within(storage).getByRole("button", { name: "Migrate to SeaweedFS" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Replace MinIO with SeaweedFS?")).toBeInTheDocument();
    expect(within(dialog).getByText("Start fresh: stored objects are not copied")).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/Artifacts, logs and cached outputs of earlier pipeline runs return 404/);
    expect(dialog).toHaveTextContent(/PersistentVolumeClaim minio-pvc is kept, unused, for a rollback or a manual copy/);
    expect(dialog).toHaveTextContent(/Pipeline servers keep their settings and need no edits/);
    expect(within(dialog).queryByText("Data is deleted and cannot be recovered")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Migrate and start fresh" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/setup"))).toHaveLength(1));
  });

  it("Cancel leaves MinIO alone", async () => {
    const api = setup(live({ minio: pendingMinio, pipelineServers: [] }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    fireEvent.click(await within(storage).findByRole("button", { name: "Migrate to SeaweedFS" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(api.calls.filter((c) => c.startsWith("POST"))).toHaveLength(0);
  });

  it("after the migration, the kept MinIO volume is reported with its size and Tear down names it", async () => {
    const migrated: ResourceState = {
      deployed: true, ready: true, namespace: "minio", managedByTool: true, message: "Running", uiUser: "admin",
      dataPVCs: ["seaweedfs-pvc", "minio-pvc"], keptPVCs: [{ name: "minio-pvc", size: "20Gi" }],
    };
    setup(live({ minio: migrated, pipelineServers: [] }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    const notice = await within(storage).findByText(/Old MinIO data volume kept/);
    expect(notice).toHaveTextContent("Old MinIO data volume kept: PersistentVolumeClaim minio-pvc (20Gi). SeaweedFS does not use it; it is kept for a rollback or a manual copy of old objects (see RUNBOOK §10). Tear down deletes it.");
    expect(within(storage).queryByText("MinIO (migration pending)")).not.toBeInTheDocument();
    expect(within(storage).getByText("SeaweedFS")).toBeInTheDocument();
    fireEvent.click(within(storage).getByRole("button", { name: "Tear down" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Data is deleted and cannot be recovered").closest(".pf-v6-c-alert")).toHaveTextContent(/seaweedfs-pvc, minio-pvc/);
  });
});

describe("status from the serving configuration (Repair)", () => {
  const base: ResourceState = { deployed: true, namespace: "minio", managedByTool: true, uiUser: "admin", dataPVCs: ["seaweedfs-pvc"], ready: false };
  const onMinIO = "Service minio-service selects app=minio instead of SeaweedFS (app=seaweedfs), so pipeline servers do not reach SeaweedFS.";
  const scaledDown = "Deployment seaweedfs is scaled to 0 replicas (for example by a manual rollback).";

  it.each([
    ["the Service still points at MinIO", { ...base, terminalError: true, message: onMinIO, repairNeeded: onMinIO }, "Incomplete", onMinIO],
    ["SeaweedFS is scaled to zero", { ...base, terminalError: true, message: scaledDown, repairNeeded: scaledDown }, "Incomplete", scaledDown],
    ["it is still starting before the switch", { ...base, message: "0/1 ready", repairNeeded: onMinIO }, "Starting", `Needs repair: ${onMinIO}`],
    ["the MinIO cleanup is unfinished", { ...base, ready: true, message: "Running", repairNeeded: "NetworkPolicy minio-ingress of the replaced MinIO was not removed yet (its pods were still shutting down)." }, "Running", "Needs repair: NetworkPolicy minio-ingress"],
  ])("when %s, it says so and offers Repair, which re-runs setup", async (_name, minio, label, text) => {
    const api = setup(live({ minio: minio as ResourceState, pipelineServers: [] }), {
      "POST /api/resources/minio/setup": () => jsonResponse({ success: true, message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'.", logs: [] }),
    });
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    expect(await within(storage).findByText(label)).toBeInTheDocument();
    expect(within(storage).getByText(new RegExp(text.slice(0, 40).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
    expect(within(storage).getByText(/Repair re-runs setup/)).toBeInTheDocument();
    expect(within(storage).queryByText(/^Failed/)).not.toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Set up" })).not.toBeInTheDocument();
    fireEvent.click(within(storage).getByRole("button", { name: "Repair" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Repair S3 storage" }));
    await waitFor(() => expect(api.calls.filter((c) => c.startsWith("POST /api/resources/minio/setup"))).toHaveLength(1));
  });

  it("a serving SeaweedFS has no Repair", async () => {
    setup(live({ minio: { ...base, ready: true, message: "Running" }, pipelineServers: [] }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    const storage = await screen.findByRole("list", { name: "Storage" });
    expect(await within(storage).findByText("Running")).toBeInTheDocument();
    expect(within(storage).queryByRole("button", { name: "Repair" })).not.toBeInTheDocument();
    expect(within(storage).queryByText(/Needs repair/)).not.toBeInTheDocument();
  });
});
