import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi, jsonResponse } from "../../test/apiStub";
import { sseEvent, sseResponse } from "../../test/utils";
import type { ResourceState, ResourcesStatus, S3KeysState } from "../../types";
import { NOT_SECURED, SECURE_FIRST, storageNotReadyReason } from "./featureState";

const readyMinio: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running", uiUser: "admin", uiRoute: "https://seaweedfs-admin.apps.example.com", usedBy: ["dev/dspa", "hand/dspa", "team-b/dspa", "unrec/dspa"] };
const none: ResourceState = { deployed: false, ready: false, managedByTool: false, message: "Not deployed" };
const dspa: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "dspa", message: "Running", uiRoute: "https://dash/dev" };
const notSecured: S3KeysState = {
  secured: false, needsSecuring: true, oldKey: ["dev/dspa", "team-b/dspa"],
  keepOldKey: ["hand/dspa (not set up by this tool)", "unrec/dspa (not recorded by this tool)"],
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
    expect(screen.getByText("Every pipeline project holds the S3 storage's admin key").closest(".pf-v6-c-alert")).toHaveTextContent("After Secure S3 storage, hand/dspa (not set up by this tool) and unrec/dspa (not recorded by this tool) lose S3 access: their owners must give them new S3 keys, or delete them and set them up again with this tool.");
    // One list: each server that uses the storage with the key it uses; the reason is words, not code.
    const usedBy = storage.querySelector('dl[aria-label="S3 storage details"]') as HTMLElement;
    expect(usedBy).toHaveTextContent("dev/dspa: the shared admin key");
    // The reason right after the server's name, as words.
    expect(usedBy).toHaveTextContent("unrec/dspa (not recorded by this tool): the shared admin key; it stops working after Secure S3 storage");
    expect(within(usedBy).getByText("(not recorded by this tool)").closest("code")).toBeNull();
    expect(usedBy).toHaveTextContent("Admin access key");
    unmount();

    const p = renderAt("/test-resources?tab=pipelines&project=dev");
    expect(within(await part("S3 storage")).getByText(NOT_SECURED)).toBeInTheDocument();
    expect(await part("S3 storage")).toHaveTextContent("holds the S3 storage's admin key. Secure S3 storage, on the S3 storage tab, gives each pipeline server this tool set up a key of its own");
    p.unmount();

    renderAt("/test-resources");
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(s3).toHaveTextContent(NOT_SECURED));
  });

  it("Secure S3 storage shows the plan, then streams its steps: checks, rollout, one per pipeline server", async () => {
    let status = statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured });
    const run = sseResponse();
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev"] },
      "POST /api/resources/s3/secure/stream": () => run.response,
    });
    renderAt("/test-resources?tab=storage");
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    expect(dialog).toHaveTextContent("The old admin key, which every pipeline project holds, stops working.");
    // One bullet per server, each starting with the server.
    const bullets = within(dialog).getAllByRole("listitem").map((li) => li.textContent);
    expect(bullets).toContain("dev/dspa gets a key of its own (SeaweedFS user rnu-dspa-dev), checked against the storage before it goes into its Secret nightly-dspa-s3 and its Connection rhoai-nightly-updater-s3.");
    expect(bullets).toContain("unrec/dspa (not recorded by this tool) keeps the old admin key, which then stops working; its owner must give it new S3 keys, or delete it and set it up again with this tool.");
    expect(dialog).not.toHaveTextContent(/IAM/);
    expect(dialog).toHaveTextContent("Pipeline runs in progress may fail");
    expect(dialog).toHaveTextContent("restart them afterwards");
    expect(dialog).toHaveTextContent("keep their old key, which no longer works until Secure S3 storage is run again");
    expect(api.calls.filter((c) => c.startsWith("POST "))).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    const progress = await within(dialog).findByRole("list", { name: "Secure S3 storage progress" });
    // Every step is listed from the start, one per recorded pipeline server.
    for (const title of ["Check the S3 storage and its pipeline servers", "Roll out a new admin key and turn on per-server keys", "Give dev/dspa a key of its own", "Give team-b/dspa a key of its own"]) {
      expect(progress).toHaveTextContent(title);
    }
    run.push(sseEvent("check", "success", "OK: 2 pipeline servers to give a key of its own"));
    run.push(sseEvent("rollout", "running", "Waiting up to 1m30s for SeaweedFS to become ready"));
    expect(await within(dialog).findByText("Waiting up to 1m30s for SeaweedFS to become ready")).toBeInTheDocument();
    run.push(sseEvent("rollout", "success", "OK: The S3 storage runs with the new admin key; the old key no longer works"));
    run.push(sseEvent("server:dev/dspa", "running", "Checking the new key against the storage"));
    expect(await within(dialog).findByText("Checking the new key against the storage")).toBeInTheDocument();
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
          r.push(sseEvent("rollout", "skipped", "The S3 storage already runs with a new admin key and per-server keys"));
          r.push(sseEvent("server:dev/dspa", "success", "OK: dev/dspa already uses its own key"));
          r.push(sseEvent("operation_complete", "success", "The S3 storage is secured: it has a new admin key, which is in no project. On a key of their own: dev/dspa, team-b/dspa."));
        }
        r.close();
        return r.response;
      },
    });
    renderAt("/test-resources?tab=storage");
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    const outcome = (await within(dialog).findByText("Stopped")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(outcome).toHaveTextContent("Still on the old key: team-b/dspa; run Secure S3 storage again");
    // The reason is said once, in the outcome, not again under the step.
    expect(dialog.textContent!.split("did not get its own key").length - 1).toBe(1);
    fireEvent.click(within(dialog).getByRole("button", { name: "Run Secure S3 storage again" }));
    expect(await within(dialog).findByText("The S3 storage is secured")).toBeInTheDocument();
    await waitFor(() => expect(dialog).toHaveTextContent("OK: dev/dspa already uses its own key"));
    // Identifiers in the operation's own text are code.
    expect(within(dialog).getAllByText("dev/dspa").some((el) => el.tagName === "CODE")).toBe(true);
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
    fireEvent.click(within(await part("SeaweedFS")).getByRole("button", { name: "Secure S3 storage" }));
    const dialog = await screen.findByRole("dialog", { name: "Secure the S3 storage?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Secure S3 storage" }));
    run.push(sseEvent("check", "success", "OK"));
    run.push(sseEvent("rollout", "running", "Rolling out a new admin key and turning on per-server keys"));
    expect(await within(dialog).findByText("Rolling out a new admin key and turning on per-server keys")).toBeInTheDocument();
    run.fail();
    const outcome = (await within(dialog).findByText("Still running", {}, { timeout: 5000 })).closest(".pf-v6-c-alert") as HTMLElement;
    expect(outcome).toHaveTextContent("The operation continues on the server; the S3 storage tab shows the outcome when it ends.");
  });

  it("after Secure: a server this tool has no record of lost its S3 access: named, with why, and the S3 row says so", async () => {
    const after: S3KeysState = { secured: true, needsSecuring: false, ownKey: ["dev/dspa", "team-b/dspa"], keepOldKey: ["unrec/dspa (not recorded by this tool)"] };
    stubApi({ "/api/resources/status": statusOf({ minio: { ...readyMinio, usedBy: ["dev/dspa", "team-b/dspa", "unrec/dspa"] }, pipelineServers: [dspa], s3Keys: after }), "/api/resources/projects": { projects: ["dev"] } });
    const { unmount } = renderAt("/test-resources?tab=storage");
    const alert = (await screen.findByText((_, el) => el?.classList.contains("pf-v6-c-alert__title") === true && /Pipeline server unrec\/dspa lost its S3 access/.test(el.textContent ?? ""))).closest(".pf-v6-c-alert") as HTMLElement;
    // One sentence, no one-item list repeating the name.
    expect(alert).toHaveTextContent("This tool has no record of it, so Secure S3 storage did not give it a key.");
    expect(within(alert).queryByRole("list")).toBeNull();
    expect(alert).toHaveTextContent("The old admin key no longer works: its owner must give it new S3 keys, or delete it and set it up again with this tool.");
    const details = (await part("SeaweedFS")).querySelector('dl[aria-label="S3 storage details"]') as HTMLElement;
    expect(details).toHaveTextContent("unrec/dspa (not recorded by this tool): the old admin key, which no longer works");
    expect(details).toHaveTextContent("dev/dspa: its own key");
    expect(headerLabel("S3 storage")).toHaveTextContent(/^1 server lost access$/);
    unmount();
    renderAt("/test-resources");
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(s3).toHaveTextContent("1 server lost access"));
  });

  it("leftover keys: their own label, one action that lists them, and Set up still works", async () => {
    const status = statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: false, ownKey: ["dev/dspa"], leftoverUsers: ["rnu-dspa-gone"], leftovers: [{ user: "rnu-dspa-gone", project: "gone", connectionInUse: true }] } });
    const api = stubApi({
      "/api/resources/status": () => jsonResponse(status),
      "/api/resources/projects": { projects: ["dev", "new"] },
      "POST /api/resources/s3/leftover-keys/remove": () => jsonResponse({ success: true, message: "Removed the keys of pipeline servers that no longer exist: rnu-dspa-gone.", logs: [] }),
    });
    const { unmount } = renderAt("/test-resources?tab=storage");
    const alert = (await screen.findByText("1 key of a deleted pipeline server still works")).closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveTextContent("Project gone has no pipeline server any more (deleted while SeaweedFS did not answer, or outside this tool), but its S3 key still works. Remove leftover keys deletes it.");
    expect(alert).toHaveTextContent("The Connection rhoai-nightly-updater-s3 in gone still holds this key: workbenches and model deployments using it lose S3 access when it is removed.");
    expect(headerLabel("S3 storage")).toHaveTextContent(/^Leftover keys$/);
    expect(screen.queryByRole("button", { name: "Secure S3 storage" })).not.toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "Remove leftover keys" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Remove the leftover key?");
    expect(dialog).toHaveTextContent("(a SeaweedFS user,");
    expect(dialog).toHaveTextContent("Deletes the S3 key of a project that has no pipeline server any more");
    expect(dialog).toHaveTextContent("The S3 key of project gone (SeaweedFS user rnu-dspa-gone). Its Connection rhoai-nightly-updater-s3 still holds this key");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/s3/leftover-keys/remove"]).toEqual([{ users: ["rnu-dspa-gone"] }]));
    unmount();
    renderAt("/test-resources?tab=pipelines&project=new");
    const setUp = await screen.findByRole("button", { name: "Set up the pipeline server in new" });
    expect(setUp).not.toHaveAttribute("aria-disabled", "true");
    expect(within(await part("S3 storage")).getByText("Leftover keys")).toBeInTheDocument();
  });

  it("a reader sees the state but no Secure action", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources?tab=storage", "Read-only access: changing the cluster through this tool requires the cluster-admin role.");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.queryByRole("button", { name: "Secure S3 storage" })).not.toBeInTheDocument();
    expect(screen.getByText("A cluster-admin runs it.")).toBeInTheDocument();
  });

  it("Set up Pipelines in a new project is refused and points at Secure S3 storage", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: notSecured }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=pipelines&project=new");
    const setUp = await screen.findByRole("button", { name: "Set up the pipeline server in new" });
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(setUp).toHaveAccessibleDescription(/Secure the S3 storage first \(S3 storage tab\)/);
    expect(storageNotReadyReason(readyMinio, null, false, notSecured)).toBe(SECURE_FIRST);
    expect(storageNotReadyReason(readyMinio, null, false, secured)).toBeNull();
  });

  it("secured storage with one server left on the old key: a new project can still set up Pipelines (review 4)", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: true, ownKey: ["dev/dspa"], oldKey: ["team-b/dspa"] } }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=pipelines&project=new");
    const setUp = await screen.findByRole("button", { name: "Set up the pipeline server in new" });
    expect(setUp).not.toHaveAttribute("aria-disabled", "true");
    // The storage still says what is unfinished.
    expect(within(await part("S3 storage")).getByText(NOT_SECURED)).toBeInTheDocument();
  });

  it("secured storage with one server left on the old key: still Not secured, and Set up works", async () => {
    stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: true, ownKey: ["dev/dspa"], oldKey: ["team-b/dspa"] } }), "/api/resources/projects": { projects: ["dev", "new"] } });
    renderAt("/test-resources?tab=storage");
    await waitFor(() => expect(headerLabel("S3 storage")).toHaveTextContent(/^Not secured$/));
    expect(screen.getByText("Some pipeline servers still use the old admin key")).toBeInTheDocument();
    expect(await part("SeaweedFS")).toHaveTextContent("dev/dspa: its own key");
  });

  it("the Overview's Pipelines row and the Pipelines tab heading say the same in every S3 key state", async () => {
    const states: Record<string, S3KeysState> = {
      "not secured": notSecured,
      secured,
      "lost access": { secured: true, needsSecuring: false, ownKey: ["dev/dspa"], keepOldKey: ["unrec/dspa (not recorded by this tool)"] },
      leftover: { secured: true, needsSecuring: false, ownKey: ["dev/dspa"], leftoverUsers: ["rnu-dspa-gone"], leftovers: [{ user: "rnu-dspa-gone", project: "gone", connectionInUse: false }] },
      unfinished: { secured: true, needsSecuring: true, ownKey: ["dev/dspa"], oldKey: ["team-b/dspa"] },
    };
    for (const [name, keys] of Object.entries(states)) {
      stubApi({ "/api/resources/status": statusOf({ minio: readyMinio, pipelineServers: [dspa], s3Keys: keys }), "/api/resources/projects": { projects: ["dev"] } });
      const overview = renderAt("/test-resources");
      const row = (await screen.findByRole("link", { name: "Pipelines" })).closest("tr") as HTMLElement;
      await waitFor(() => expect(row.querySelector(".pf-v6-c-label")).not.toBeNull());
      const rowLabel = row.querySelector(".pf-v6-c-label")!.textContent;
      overview.unmount();
      const tab = renderAt("/test-resources?tab=pipelines&project=dev");
      await waitFor(() => expect(headerLabel("Pipelines")).not.toBeNull());
      expect(headerLabel("Pipelines")!.textContent, name).toBe(rowLabel);
      if (name === "leftover") expect(document.body).toHaveTextContent("A project with no pipeline server still has a working S3 key: gone. Remove leftover keys, on the S3 storage tab, deletes it.");
      if (name === "lost access") expect(document.body.textContent).toMatch(/Pipeline server unrec\/dspa.*lost S3 access\./);
      tab.unmount();
    }
  });

  it("a storage that needs Repair is never shown as only Leftover keys, and Pipelines is not green", async () => {
    const minio = { ...readyMinio, repairNeeded: "NetworkPolicy seaweedfs-ingress is missing." };
    stubApi({ "/api/resources/status": statusOf({ minio, pipelineServers: [dspa], s3Keys: { secured: true, needsSecuring: false, ownKey: ["dev/dspa"], leftoverUsers: ["rnu-dspa-gone"], leftovers: [{ user: "rnu-dspa-gone", project: "gone", connectionInUse: false }] } }), "/api/resources/projects": { projects: ["dev"] } });
    renderAt("/test-resources");
    const s3 = (await screen.findByRole("link", { name: "S3 storage" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(s3.querySelector(".pf-v6-c-label")).not.toBeNull());
    expect(s3.querySelector(".pf-v6-c-label")!.textContent).not.toBe("Leftover keys");
    const pipelines = (await screen.findByRole("link", { name: "Pipelines" })).closest("tr") as HTMLElement;
    expect(pipelines.querySelector(".pf-v6-c-label")!.textContent).not.toMatch(/^Ready/);
  });
});
