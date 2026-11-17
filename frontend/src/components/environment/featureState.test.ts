import { describe, expect, it } from "vitest";
import type { LegacyStorageState, ResourceState, ResourcesStatus } from "../../types";
import { defaultProject, mlflowState, pipelinesState, setUpBlocked } from "./featureState";

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
  it("readiness is health: someone else's running MLflow is Ready, with ownership in the label", () => {
    // MLflow's root is its list of workspaces: the label says so.
    expect(mlflowState(FIXTURES.live)).toEqual({ status: "ready", label: "Ready · not managed by this tool", next: { kind: "open", label: "Open MLflow workspaces", href: "https://mlflow" }, project: null });
    const broken = { ...FIXTURES.live, mlflow: { ...FIXTURES.live.mlflow, ready: false, terminalError: true, waitingReason: "CrashLoopBackOff" } };
    expect(mlflowState(broken)).toMatchObject({ status: "error", label: "Error · not managed by this tool", next: { kind: "show-issue", part: "mlflow" } });
    expect(mlflowState(FIXTURES.none)).toMatchObject({ status: "not-set-up", next: { kind: "set-up", setup: [{ kind: "setup-mlflow" }], reason: null } });
  });

  it.each([
    ["live", "ready", "Ready in juntao-test"],
    ["none", "not-set-up", "Not set up in any project"],
    ["storageOnly", "not-set-up", "Not set up in any project"],
    ["starting", "setting-up", "Setting up in juntao-test"],
    ["storageError", "error", "Error"],
    ["serverError", "error", "Error in zeta"],
    ["foreign", "ready", "Ready in zeta · not managed by this tool"],
    ["migrate", "needs-attention", "Needs attention"],
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
      if (!overview.label.includes(" of ")) expect(tab.label).toBe(overview.label.startsWith("Needs attention") || overview.label.startsWith("Error") && !overview.label.includes(" in ") ? `${overview.label} in ${tabProject}` : overview.label);
      // A link to a project says which, and the Overview's is the tab's.
      if (overview.next?.kind === "open") {
        expect(overview.next.label).toBe(`Open pipelines in ${tabProject}`);
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

  it("several ready projects: the Overview offers the tab's picker, not a jump into one of them", () => {
    const two: ResourcesStatus = { ...FIXTURES.live, pipelineServers: [dspa("juntao-test"), dspa("zeta")] };
    const overview = pipelinesState(two, null, projects);
    expect(overview).toMatchObject({ status: "ready", label: "Ready in 2 projects", next: { kind: "choose-project" } });
    // The tab, in one of them, links there by name.
    expect(pipelinesState(two, "zeta", projects).next).toMatchObject({ kind: "open", label: "Open pipelines in zeta" });
  });

  it("nothing is offered while every part exists and one is starting; storage of an earlier version points at Migrate storage", () => {
    const starting: ResourcesStatus = { ...FIXTURES.storageOnly, pipelineServers: [{ deployed: true, ready: false, managedByTool: true, namespace: "a-test-project", name: "nightly-dspa", message: "Provisioning" }] };
    expect(pipelinesState(starting, "a-test-project", projects)).toMatchObject({ status: "setting-up", next: null });
    expect(setUpBlocked(pipelinesState(starting, "a-test-project", projects))).toMatch(/nothing left to set up/);
  });
});
