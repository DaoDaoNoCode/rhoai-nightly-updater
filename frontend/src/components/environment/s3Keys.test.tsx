import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { ResourceState, ResourcesStatus, S3KeysState } from "../../types";
import { NOT_SECURED, SECURE_FIRST, storageNotReadyReason } from "./featureState";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com", usedBy: ["dev/dspa", "team-b/dspa"] };
const none: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const dspa: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "dspa", message: "Running", uiRoute: "https://dash/dev" };
const notSecured: S3KeysState = {
  secured: false, needsSecuring: true, oldKey: ["dev/dspa", "team-b/dspa"],
  keepOldKey: ["hand/dspa (not created by this tool)", "unrec/dspa (not recorded as this tool's)"],
};
const secured: S3KeysState = { secured: true, needsSecuring: false, ownKey: ["dev/dspa", "team-b/dspa"] };
const statusOf = (over: Partial<ResourcesStatus>): ResourcesStatus => ({ minio: none, mlflow: none, pipelineServers: [], ...over });

function renderAt(path: string, readBlocker: string | null = null) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} readBlocker={readBlocker} /></MemoryRouter>);
}
const part = (name: string) => screen.findByRole("listitem", { name: new RegExp(`^${name}$`) });
const headerLabel = (title: string) => screen.getByRole("heading", { level: 2, name: title }).closest(".pf-v6-l-flex")!.querySelector(".pf-v6-c-label");

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("S3 storage that is not secured", () => {
  it("one label, Not secured, on the S3 storage tab, its SeaweedFS part, the Pipelines S3 part and the Overview row", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev", "new"] } });
    const { unmount } = renderAt("/test-resources?tab=storage");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    const storage = await part("SeaweedFS");
    expect(within(storage).getByText(NOT_SECURED)).toBeInTheDocument();
    expect(screen.getByText("Every pipeline project holds the S3 storage's admin key")).toBeInTheDocument();
    expect(storage.querySelector('dl[aria-label="S3 keys"]')).toHaveTextContent(/Old shared key\s*dev\/dspa, team-b\/dspa/);
    unmount();

    const p = renderAt("/test-resources?tab=pipelines&project=dev");
    expect(within(await part("S3 storage")).getByText(NOT_SECURED)).toBeInTheDocument();
    expect(await part("S3 storage")).toHaveTextContent("Secure S3 storage, on the S3 storage tab, gives each pipeline server a key of its own");
    p.unmount();

    renderAt("/test-resources");
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(s3).toHaveTextContent(NOT_SECURED));
  });

  it("Secure S3 storage... shows the plan, then streams its steps: checks, rollout, one per pipeline server", async () => {
    let status = statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured });
    const run = sseResponse();
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/secure/stream": () => run.response,
    });
    renderAt("/test-resources?tab=storage");
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage..." }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    expect(dialog).toHaveTextContent("The old admin key, which every pipeline project holds, stops working.");
    expect(dialog).toHaveTextContent(/These pipeline servers get a key of their own.*dev\/dspa, team-b\/dspa/);
    expect(dialog).toHaveTextContent(/they keep the old key, which then no longer works: hand\/dspa \(not created by this tool\), unrec\/dspa \(not recorded as this tool's\)/);
    expect(dialog).toHaveTextContent("Pipeline runs in progress may fail");
    expect(dialog).toHaveTextContent("restart them afterwards");
    expect(dialog).toHaveTextContent("running it again finishes them and skips the ones done");
    expect(api.calls.filter((c) => c.startsWith("POST "))).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    const progress = await within(dialog).findByRole("list", { name: "Secure S3 storage progress" });
    // Every step is listed from the start, one per recorded pipeline server.
    for (const title of ["Check the S3 storage and its pipeline servers", "Roll out a new admin key with IAM writes on", "Give dev/dspa a key of its own", "Give team-b/dspa a key of its own"]) {
      expect(progress).toHaveTextContent(title);
    }
    run.push(sseEvent("check", "success", "OK: 2 pipeline servers to give a key of its own"));
    run.push(sseEvent("rollout", "running", "Waiting up to 1m30s for SeaweedFS to become ready..."));
    expect(await within(dialog).findByText("Waiting up to 1m30s for SeaweedFS to become ready...")).toBeInTheDocument();
    run.push(sseEvent("rollout", "success", "OK: The S3 storage runs with the new admin key; the old key no longer works"));
    run.push(sseEvent("server:dev/dspa", "running", "Checking the new key against the storage..."));
    expect(await within(dialog).findByText("Checking the new key against the storage...")).toBeInTheDocument();
    run.push(sseEvent("server:dev/dspa", "success", "OK: dev/dspa uses SeaweedFS user rnu-dspa-dev"));
    run.push(sseEvent("server:team-b/dspa", "success", "OK: team-b/dspa uses SeaweedFS user rnu-dspa-team-b"));
    status = { ...status, s3Keys: secured };
    run.push(sseEvent("operation_complete", "success", "The S3 storage is secured: it has a new admin key, which is in no project. On a key of their own: dev/dspa, team-b/dspa."));
    run.close();
    expect(await within(dialog).findByText("The S3 storage is secured")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("On a key of their own: dev/dspa, team-b/dspa.");
    expect(api.bodies["POST /api/resources/s3/secure/stream"]).toEqual([{}]);
    expect(within(dialog).queryByRole("button", { name: /^(Run )?Secure S3 storage/ })).not.toBeInTheDocument();
  });

  it("a stop part-way says what is left and offers to run it again, which resumes", async () => {
    const stopped = "Pipeline server team-b/dspa did not get its own key: connection refused. The storage is secured. On a key of their own: dev/dspa. Still on the old key: team-b/dspa; run Secure S3 storage again: it skips the servers already done.";
    let n = 0;
    const api = stubApi({
      "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/secure/stream": () => {
        n += 1;
        const r = sseResponse();
        if (n === 1) {
          r.push(sseEvent("check", "success", "OK"));
          r.push(sseEvent("rollout", "success", "OK"));
          r.push(sseEvent("server:dev/dspa", "success", "OK: dev/dspa uses SeaweedFS user rnu-dspa-dev"));
          r.push(sseEvent("server:team-b/dspa", "failed", stopped, { errorCode: "s3_keys" }));
          r.push(sseEvent("operation_complete", "failed", stopped, { errorCode: "s3_keys" }));
        } else {
          r.push(sseEvent("rollout", "skipped", "The S3 storage already runs with a new admin key and IAM writes on"));
          r.push(sseEvent("server:dev/dspa", "success", "OK: dev/dspa already uses its own key"));
          r.push(sseEvent("operation_complete", "success", "The S3 storage is secured: it has a new admin key, which is in no project. On a key of their own: dev/dspa, team-b/dspa."));
        }
        r.close();
        return r.response;
      },
    });
    renderAt("/test-resources?tab=storage");
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage..." }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    const outcome = (await within(dialog).findByText("Stopped")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(outcome).toHaveTextContent("Still on the old key: team-b/dspa; run Secure S3 storage again");
    // The reason is said once, in the outcome, not again under the step.
    expect(dialog.textContent!.split("did not get its own key").length - 1).toBe(1);
    fireEvent.click(within(dialog).getByRole("button", { name: "Run Secure S3 storage again" }));
    expect(await within(dialog).findByText("The S3 storage is secured")).toBeInTheDocument();
    expect(await within(dialog).findByText("OK: dev/dspa already uses its own key")).toBeInTheDocument();
    expect(api.bodies["POST /api/resources/s3/secure/stream"]).toEqual([{}, {}]);
  });

  it("a lost connection: the run goes on on the server and the page says so", async () => {
    const run = sseResponse();
    stubApi({
      "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/secure/stream": () => run.response,
    });
    renderAt("/test-resources?tab=storage");
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage..." }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    run.push(sseEvent("check", "success", "OK"));
    run.push(sseEvent("rollout", "running", "Rolling out a new admin key with IAM writes on..."));
    expect(await within(dialog).findByText("Rolling out a new admin key with IAM writes on...")).toBeInTheDocument();
    run.fail();
    const outcome = (await within(dialog).findByText("Still running", {}, { timeout: 5000 })).closest(".pf-v6-c-alert") as HTMLElement;
    expect(outcome).toHaveTextContent("The operation continues on the server; the S3 storage tab shows the outcome when it ends.");
  });

  it("a reader sees the state but no Secure action", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage", "Read-only access: changing the cluster through this tool requires the cluster-admin role.");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.queryByRole("button", { name: "Secure S3 storage..." })).not.toBeInTheDocument();
    expect(screen.getByText("A cluster-admin runs it.")).toBeInTheDocument();
  });

  it("Set up Pipelines in a new project is refused and points at Secure S3 storage", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=pipelines&project=new");
    const setUp = await screen.findByRole("button", { name: "Set up Pipelines in new" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(setUp).toHaveAccessibleDescription(/Secure S3 storage, on the S3 storage tab/);
    expect(storageNotReadyReason(readyMinio, null, false, notSecured)).toBe(SECURE_FIRST);
    expect(storageNotReadyReason(readyMinio, null, false, secured)).toBeNull();
  });

  it("secured storage with one server left on the old key: still Not secured, and Set up works", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: true, ownKey: ["dev/dspa"], oldKey: ["team-b/dspa"] } }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=storage");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.getByText("Some pipeline servers still use the old key")).toBeInTheDocument();
    expect(await part("SeaweedFS")).toHaveTextContent(/Own key\s*dev\/dspa/);
  });
});
