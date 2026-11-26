import { describe, expect, it } from "vitest";
import type { LegacyStorageState, ResourceState, ResourcesStatus } from "../../types";
import { PLAYGROUND_ALTERNATIVES, defaultPlaygroundModels, defaultProject, IDLE, loadingState, playgroundModelsWithoutPort, maasPartLabelStatus, maasState, mlflowState, modelsState, pipelinesState, playgroundState, setUpBlocked, storageState, unreadableState } from "./featureState";

describe("A platform part's own label", () => {
  it("a ready part with a warning needs attention; without one it is ready", () => {
    expect(maasPartLabelStatus({ id: "rhcl", title: "Red Hat Connectivity Link", state: "ready", warning: "Version 1.4.3 is below the docs' 1.5. Upgrade it." })).toBe("needs-attention");
    expect(maasPartLabelStatus({ id: "rhcl", title: "Red Hat Connectivity Link", state: "ready" })).toBe("ready");
  });

  it("MaaS being cleaned up reads Deleting, as the other features", () => {
    const m = { platform: { deployed: true, ready: true, managedByTool: true }, parts: [], subscription: "s" };
    expect(maasState(m, { ...IDLE, maas: { action: "teardown", project: null } }).label).toBe("Deleting");
  });
});

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const storage: ResourceState = { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs", message: "Running" };
const dspa = (ns: string, over: Partial<ResourceState> = {}): ResourceState => ({ deployed: true, ready: true, managedByTool: true, namespace: ns, name: "nightly-dspa", message: "Running", uiRoute: `https://dash/${ns}`, ...over });
const legacy = (stage: "migrate" | "remove"): LegacyStorageState => ({ namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000", objects: [], stage, namespaceDeletable: false, usedBy: stage === "migrate" ? ["juntao-test/dspa"] : [] });
const projects = ["a-test-project", "juntao-test", "zeta"];

/** The states the docs mock and the live cluster showed, by name. */
const FIXTURES: Record<string, ResourcesStatus> = {
  // Live, 2026-11-17: an empty project first, the server in a later one, MLflow made in the dashboard.
  live: { minio: storage, mlflow: { ...storage, namespace: "redhat-ods-applications", managedByTool: false, uiRoute: "https://mlflow" }, pipelineServers: [dspa("juntao-test")] },
  none: { minio: none, mlflow: none, pipelineServers: [] },
  storageOnly: { minio: storage, mlflow: none, pipelineServers: [] },
  starting: { minio: { ...storage, ready: false, message: "0/1 ready" }, mlflow: none, pipelineServers: [dspa("juntao-test", { ready: false, message: "Starting (1/3 ready)", uiRoute: undefined })] },
  storageError: { minio: { ...storage, ready: false, terminalError: true, waitingReason: "ImagePullBackOff", message: "ImagePullBackOff" }, mlflow: none, pipelineServers: [dspa("juntao-test")] },
  serverError: { minio: storage, mlflow: none, pipelineServers: [dspa("zeta", { ready: false, terminalError: true, message: "Could not connect" })] },
  foreign: { minio: { ...none, setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." }, mlflow: none, pipelineServers: [], unmanagedPipelineProjects: ["zeta"], unmanagedPipelineServers: [dspa("zeta", { managedByTool: false, name: "dspa" })] },
  migrate: { minio: { ...none, migrationPending: true }, mlflow: none, pipelineServers: [dspa("juntao-test", { name: "dspa" })], legacyStorage: legacy("migrate") },
  migrateRemove: { minio: { ...storage, migrationPending: true }, mlflow: none, pipelineServers: [dspa("juntao-test", { name: "dspa" })], legacyStorage: legacy("remove") },
  incomplete: { minio: { ...storage, ready: false, terminalError: true, repairNeeded: "scaled to 0", message: "scaled to 0" }, mlflow: none, pipelineServers: [] },
};

describe("a status not read yet", () => {
  it("is loading, never Not set up, with nothing to do", () => {
    expect(pipelinesState(null, null, projects)).toEqual(loadingState());
    expect(pipelinesState(null, "zeta", projects)).toEqual(loadingState("zeta"));
    expect(mlflowState(null)).toEqual(loadingState());
    expect(modelsState(null, null, projects)).toEqual(loadingState());
    expect(playgroundState(FIXTURES.none, null, null)).toEqual(loadingState());
    expect(playgroundState(null, { items: [] }, "zeta")).toEqual(loadingState("zeta"));
    expect(maasState(null)).toEqual(loadingState());
    expect(loadingState()).toMatchObject({ loading: true, next: null });
    expect(unreadableState()).not.toHaveProperty("loading");
  });
});

describe("defaultProject", () => {
  it("is the URL's project, else the first with a ready pipeline server, else the first with any, else none", () => {
    const s: ResourcesStatus = { minio: storage, mlflow: none, pipelineServers: [dspa("b", { ready: false }), dspa("c")] };
    expect(defaultProject(s, ["a", "b", "c"], "a")).toBe("a");
    expect(defaultProject(s, ["a", "b", "c"], null)).toBe("c");
    expect(defaultProject({ ...s, pipelineServers: [dspa("b", { ready: false })] }, ["a", "b", "c"], null)).toBe("b");
    expect(defaultProject(FIXTURES.none, projects, null)).toBeNull();
    // Someone else's ready server counts like this tool's.
    expect(defaultProject(FIXTURES.foreign, projects, null)).toBe("zeta");
  });
});

describe("pipelinesState and mlflowState", () => {
  it("readiness is health: someone else's running MLflow is Ready, with ownership apart from the label", () => {
    // MLflow's root is its list of workspaces: the label says so.
    expect(mlflowState(FIXTURES.live)).toEqual({ status: "ready", label: "Ready", unmanaged: true, next: { kind: "open", label: "Open MLflow experiments", href: "https://mlflow" }, project: null });
    const broken = { ...FIXTURES.live, mlflow: { ...FIXTURES.live.mlflow, ready: false, terminalError: true, waitingReason: "CrashLoopBackOff" } };
    expect(mlflowState(broken)).toMatchObject({ status: "error", label: "Error", unmanaged: true, next: { kind: "show-issue", part: "mlflow" } });
    // This tool's own: nothing to say about ownership.
    expect(mlflowState({ ...FIXTURES.live, mlflow: { ...FIXTURES.live.mlflow, managedByTool: true } })).not.toHaveProperty("unmanaged");
    expect(mlflowState(FIXTURES.none)).toMatchObject({ status: "not-set-up", next: { kind: "set-up", setup: [{ kind: "setup-mlflow" }], reason: null } });
  });

  it.each([
    ["live", "ready", "Ready in juntao-test"],
    ["none", "not-set-up", "Not set up in any project"],
    ["storageOnly", "not-set-up", "Not set up in any project"],
    ["starting", "setting-up", "Setting up in juntao-test"],
    ["storageError", "error", "Error"],
    ["serverError", "error", "Error in zeta"],
    ["foreign", "ready", "Ready in zeta"],
    ["migrate", "needs-attention", "Migration needed"],
    ["migrateRemove", "ready", "Ready in juntao-test"],
    ["incomplete", "needs-attention", "Needs attention"],
  ])("%s: the Overview reads %s, %s", (name, status, label) => {
    expect(pipelinesState(FIXTURES[name], null, projects)).toMatchObject({ status, label });
  });

  // The single source of truth: the Overview row opens the tab at the
  // project its state is about, and the tab, opened without a project,
  // picks that same project and shows the same state.
  it.each(Object.keys(FIXTURES))("%s: the Overview and the Pipelines tab agree", (name) => {
    const s = FIXTURES[name];
    const overview = pipelinesState(s, null, projects);
    const tabProject = defaultProject(s, projects, null);
    if (overview.project) expect(tabProject).toBe(overview.project);
    if (tabProject) {
      const tab = pipelinesState(s, tabProject, projects);
      expect(tab.status).toBe(overview.status);
      // A storage problem is the cluster's: the tab names no project for it either.
      if (!overview.label.includes(" of ")) expect(tab.label).toBe(overview.label);
      expect(tab.unmanaged).toBe(overview.unmanaged);
      // The label already names the project: the link does not repeat it, and the Overview's is the tab's.
      if (overview.next?.kind === "open") {
        expect(overview.next.label).toBe("Open pipelines");
        expect(tab.next).toEqual(overview.next);
      }
    } else {
      // No project to open: the tab shows the same cross-project state.
      expect(overview.project).toBeNull();
      expect(pipelinesState(s, tabProject, projects)).toEqual(overview);
    }
  });

  it("Set up sets up only what is missing, in dependency order, and a dependency it cannot set up blocks it with the reason", () => {
    // Item 9: Set up names what it sets up, and where.
    expect(pipelinesState(FIXTURES.none, "a-test-project", projects).next).toEqual({ kind: "set-up", label: "Set up Pipelines in a-test-project", setup: [{ kind: "setup-minio" }, { kind: "setup-pipeline-server", project: "a-test-project" }], reason: null });
    expect(pipelinesState(FIXTURES.storageOnly, "a-test-project", projects).next).toEqual({ kind: "set-up", label: "Set up the pipeline server in a-test-project", setup: [{ kind: "setup-pipeline-server", project: "a-test-project" }], reason: null });
    const blocked = pipelinesState(FIXTURES.foreign, "a-test-project", projects).next;
    expect(blocked).toMatchObject({ kind: "set-up", setup: [], reason: "Namespace 'seaweedfs' exists but was not created by this tool." });
    // Someone else's running storage: the storage is Ready, a pipeline server here is not offered.
    const foreignReady: ResourcesStatus = { minio: { ...storage, managedByTool: false }, mlflow: none, pipelineServers: [] };
    const s = pipelinesState(foreignReady, "a-test-project", projects);
    expect(s.next).toMatchObject({ kind: "set-up", setup: [], reason: expect.stringMatching(/was not created by this tool/) });
  });

  // Item 3: a component that is off blocks Set up before anything is created.
  it("a component that is off blocks Set up with its reason, and setUpBlocked reads it on fresh data", () => {
    const off = "Data Science Pipelines is turned off on this cluster.";
    const s = pipelinesState({ ...FIXTURES.none, pipelinesBlocked: { component: "aipipelines", reason: off } }, "a-test-project", projects);
    expect(s.next).toMatchObject({ kind: "set-up", reason: off });
    expect(setUpBlocked(s)).toBe(off);
    expect(setUpBlocked(pipelinesState(FIXTURES.none, "a-test-project", projects))).toBeNull();
    const m = mlflowState({ ...FIXTURES.none, mlflowBlocked: { component: "mlflowoperator", reason: "MLflow is turned off." } });
    expect(m.next).toMatchObject({ kind: "set-up", label: "Set up MLflow", reason: "MLflow is turned off." });
  });

  it("several ready projects: the Overview offers the tab's picker, not a jump into one of them, and lists each project", () => {
    const two: ResourcesStatus = { ...FIXTURES.live, pipelineServers: [dspa("juntao-test"), dspa("zeta")] };
    const overview = pipelinesState(two, null, projects);
    expect(overview).toMatchObject({ status: "ready", label: "Ready in 2 projects", next: { kind: "choose-project" }, project: null });
    // Each project with its own state, as its tab shows it, in the projects' order.
    expect(overview.projects).toEqual([pipelinesState(two, "juntao-test", projects), pipelinesState(two, "zeta", projects)]);
    // The tab, in one of them, links there; its label names the project.
    expect(pipelinesState(two, "zeta", projects)).toMatchObject({ label: "Ready in zeta", next: { kind: "open", label: "Open pipelines" } });
  });

  it("one project: no list of projects", () => {
    expect(pipelinesState(FIXTURES.live, null, projects)).not.toHaveProperty("projects");
    expect(pipelinesState(FIXTURES.none, null, projects)).not.toHaveProperty("projects");
  });

  it("a problem among several projects: counted among those that have it, its step names the project with the problem", () => {
    const mixed: ResourcesStatus = { ...FIXTURES.live, pipelineServers: [dspa("a-test-project"), dspa("juntao-test"), dspa("zeta", { ready: false, terminalError: true, message: "Could not connect" })] };
    const overview = pipelinesState(mixed, null, [...projects, "empty"]);
    expect(overview).toMatchObject({ status: "error", label: "Error in 1 of 3 projects", project: "zeta", next: { kind: "show-issue", part: "pipeline-server", project: "zeta" } });
    expect(overview.projects?.map((p) => [p.project, p.status])).toEqual([["a-test-project", "ready"], ["juntao-test", "ready"], ["zeta", "error"]]);
    // In the project itself (its tab), the step does not name it again.
    expect(pipelinesState(mixed, "zeta", projects).next).toEqual({ kind: "show-issue", part: "pipeline-server" });
    // An error comes before a warning, whichever project sorts first; the label counts the projects in that state, the list shows the others.
    const two: ResourcesStatus = { ...mixed, pipelineServers: [dspa("a-test-project", { ready: true, warning: "old image" }), dspa("zeta", { ready: false, terminalError: true, message: "Could not connect" })] };
    expect(pipelinesState(two, null, projects)).toMatchObject({ status: "error", label: "Error in 1 of 2 projects", project: "zeta", next: { project: "zeta" } });
  });

  it("the operation running from this page comes first, even among several projects", () => {
    const two: ResourcesStatus = { ...FIXTURES.live, pipelineServers: [dspa("juntao-test"), dspa("zeta", { ready: false, terminalError: true, message: "Could not connect" })] };
    const busy = { ...IDLE, resAction: "setup-pipeline-server", actionProject: "a-test-project" };
    expect(pipelinesState(two, null, projects, busy)).toMatchObject({ status: "setting-up", label: "Setting up in a-test-project", project: "a-test-project" });
  });

  it("someone else's part that has the problem is said apart from the label, in the project's own state", () => {
    const s: ResourcesStatus = { ...FIXTURES.foreign, unmanagedPipelineServers: [dspa("zeta", { managedByTool: false, name: "dspa", ready: false, terminalError: true, message: "Could not connect" })] };
    expect(pipelinesState(s, "zeta", projects)).toMatchObject({ status: "error", label: "Error in zeta", unmanaged: true });
  });

  it("nothing is offered while every part exists and one is starting; storage of an earlier version points at Migrate storage", () => {
    const starting: ResourcesStatus = { ...FIXTURES.storageOnly, pipelineServers: [{ deployed: true, ready: false, managedByTool: true, namespace: "a-test-project", name: "nightly-dspa", message: "Provisioning" }] };
    expect(pipelinesState(starting, "a-test-project", projects)).toMatchObject({ status: "setting-up", next: null });
    expect(setUpBlocked(pipelinesState(starting, "a-test-project", projects))).toMatch(/nothing left to set up/);
  });
});

describe("The S3 storage on its own (its tab and its Overview row)", () => {
  it("loading, not set up, ready, starting, broken, migration needed and someone else's", () => {
    expect(storageState(null)).toMatchObject({ loading: true, project: null });
    expect(storageState(FIXTURES.none)).toEqual({ status: "not-set-up", label: "Not set up", next: { kind: "set-up", label: "Set up S3 storage", setup: [{ kind: "setup-minio" }], reason: null }, project: null });
    expect(storageState({ ...FIXTURES.storageOnly, minio: { ...storage, uiRoute: "https://admin" } })).toEqual({ status: "ready", label: "Ready", next: { kind: "open", label: "Open admin UI", href: "https://admin" }, project: null });
    expect(storageState(FIXTURES.starting)).toMatchObject({ status: "setting-up", next: null });
    expect(storageState(FIXTURES.storageError)).toMatchObject({ status: "error", label: "Error", next: { kind: "show-issue", part: "storage" } });
    expect(storageState({ ...FIXTURES.none, legacyStorage: legacy("migrate") })).toMatchObject({ status: "needs-attention", label: "Migration needed", next: { kind: "migrate" } });
    const blocked: ResourceState = { ...none, setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool." };
    expect(storageState({ ...FIXTURES.none, minio: blocked })).toEqual({ status: "not-set-up", label: "Not set up", unmanaged: true, next: null, project: null });
  });

  it("Set up running from this page reads Setting up, with nothing to do", () => {
    expect(storageState(FIXTURES.none, { ...IDLE, resAction: "setup-minio" })).toMatchObject({ status: "setting-up", next: null });
    expect(storageState(FIXTURES.storageOnly, { ...IDLE, resAction: "teardown-minio" })).toMatchObject({ status: "deleting", next: null });
  });
});

describe("playground models the playground cannot use", () => {
  const note = "RHOAI's Gen AI playground cannot use it, because KServe reports its address without the port.";
  const pg = {
    project: "p", items: [],
    models: [
      { name: "smol", source: "namespace" as const, ready: true, chat: true, unusable: note },
      { name: "gpt", source: "maas" as const, ready: true, chat: true },
    ],
  };
  it("are not chosen, and the Set up reason says why when no project chat model is left", () => {
    expect(defaultPlaygroundModels(pg, "p").map((m) => m.name)).toEqual([]);
    expect(playgroundModelsWithoutPort(pg, "p").map((m) => m.name)).toEqual(["smol"]);
    const only = { ...pg, models: [pg.models[0]] };
    const st = playgroundState({} as ResourcesStatus, only, "p");
    expect(st.next?.kind === "set-up" && st.next.reason).toBe(`Needs: a chat model the playground can use. smol: ${note} ${PLAYGROUND_ALTERNATIVES}`);
  });
});
