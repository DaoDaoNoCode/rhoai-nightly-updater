#!/usr/bin/env node
// Docs mock backend: serves the built frontend (frontend/dist) and answers
// /api/* from fixtures, so every documentation screenshot is reproducible
// and shows no real cluster. Not a test double for the backend.
//
//   node docs/tools/mock/server.mjs [--port 18181] [--scenario healthy]
//
// Control endpoints (used by docs/tools/capture.js):
//   POST /__docs/scenario?name=X  reset the world to scenario X
//   POST /__docs/advance          release the next held update step (gated mode)
//   POST /__docs/release          finish the held mutation (gated mode)
//   GET  /__docs/state            scenario name, held work, the fixed "now"
//   POST /__docs/job?name=X&out=DIR, GET /__docs/job   the capture job to run
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { dirname, extname, join, normalize, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import * as F from "./fixtures.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const args = Object.fromEntries(
  process.argv.slice(2).flatMap((a, i, all) => (a.startsWith("--") ? [[a.slice(2), all[i + 1]]] : [])),
);
const PORT = Number(args.port || process.env.DOCS_MOCK_PORT || 18181);
const DIST = resolve(args.dist || join(here, "..", "..", "..", "frontend", "dist"));

// --- Scenarios ------------------------------------------------------------------

const REMOTE_POD = "rhoai-nightly-updater-7c9d5f8b64-k2x7q";

function baseWorld() {
  return {
    status: F.healthyStatus(),
    operation: { inProgress: false, operation: null },
    version: F.versionInfo(),
    updateCheck: F.noUpdate,
    components: F.healthyComponents(),
    diagnostics: F.diagnostics("healthy"),
    dashboard: F.dashboardIdle(),
    resources: { minio: F.s3States.running, mlflow: F.mlflowStates.none, pipelineServers: [F.pipelineServer] },
    // gated: update steps and mutations wait for /__docs/advance and /__docs/release.
    gated: false,
  };
}

const scenarios = {
  healthy: () => {},
  "update-available": (w) => { w.status = F.updateAvailableStatus(); },
  "operator-failed": (w) => { w.status = F.operatorFailedStatus(); },
  "no-subscription": (w) => { w.status = F.noSubscriptionStatus(); },
  fresh: (w) => {
    w.status = F.freshStatus();
    w.components = { components: [], deployments: [], dscName: "", dscExists: false, dscState: "no-crd", dscPhase: "", changedCount: 0 };
    w.resources = { minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [] };
  },
  "ready-to-install": (w) => {
    scenarios.fresh(w);
    w.status = F.readyToInstallStatus();
  },
  // The update GIFs: steps wait for /__docs/advance.
  "update-flow": (w) => { w.status = F.updateAvailableStatus(); w.gated = true; },
  "install-flow": (w) => { scenarios["ready-to-install"](w); w.gated = true; w.afterUpdate = F.noDSCStatus; },
  // Installed, no DataScienceCluster yet.
  "no-dsc": (w) => {
    w.status = F.noDSCStatus();
    w.components = { components: [], deployments: [], dscName: "", dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded", dscPhase: "", changedCount: 0 };
  },
  "remote-operation": (w) => {
    w.status = F.updateAvailableStatus();
    w.operation = {
      inProgress: true,
      operation: {
        id: "op-5f3a9c", type: "update", label: "Update to nightly", user: "qa-user", target: F.LATEST_IMAGE,
        startedAt: F.ago(3), updatedAt: F.ago(0.2), step: "verify_installplan", stepStatus: "running",
        message: "CSV rhods-operator.3.6.0: Installing (InstallWaiting)", pod: REMOTE_POD, remote: true,
      },
    };
  },
  interrupted: (w) => {
    w.status = F.updateAvailableStatus();
    w.operation = {
      inProgress: false, operation: null,
      interrupted: { type: "update", label: "Update to nightly", user: "qa-user", target: F.LATEST_IMAGE, startedAt: F.ago(22), pod: REMOTE_POD, id: "op-1d7e42", bootId: "b-9a1c" },
    };
  },
  "major-update": (w) => {
    w.version = F.versionInfo({ version: "v1.0.0", commit: "5a0c7e3b9d1f4a6c8e2b0d4f6a8c1e3b5d7f9a2c", templateRevision: "3", expectedTemplateRevision: "3" });
    w.updateCheck = {
      current: "v1.0.0", latest: "v2.0.0", updateAvailable: true, majorUpgrade: true,
      releaseNotesURL: "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases/v2.0.0",
      installerURL: "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases/v2.0.0/downloads/install.sh",
    };
  },
  "patch-update": (w) => {
    w.updateCheck = {
      current: "v2.0.0", latest: "v2.0.1", updateAvailable: true, majorUpgrade: false,
      releaseNotesURL: "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases/v2.0.1",
      installerURL: "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases/v2.0.1/downloads/install.sh",
    };
  },
  "template-outdated": (w) => { w.version = F.versionInfo({ templateRevision: "3", templateOutdated: true }); },
  "components-not-ready": (w) => { w.components = F.notReadyComponents(); },
  "dashboard-session": (w) => { w.dashboard = F.dashboardSession(); },
  "s3-none": (w) => { w.resources = { minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [] }; w.gated = true; },
  "s3-running": () => {},
  "s3-pending": (w) => { w.resources.minio = F.s3States.pending; w.gated = true; },
  "s3-incomplete": (w) => { w.resources.minio = F.s3States.incomplete; w.gated = true; },
  mlflow: (w) => { w.resources.mlflow = F.mlflowStates.running; },
};
for (const name of ["healthy", "prerequisite-missing", "operand-missing", "module-backoff", "certificate-stale", "apply-failures", "upgrade-gates", "conversion-webhook"]) {
  scenarios[`diag-${name}`] = (w) => {
    w.diagnostics = F.diagnostics(name);
    // The operator cannot convert the DSC to v3: the Components page reads it as v2.
    if (name === "conversion-webhook") w.components = F.conversionFallbackComponents();
    else if (name !== "healthy") w.components = F.notReadyComponents();
    if (name === "module-backoff") w.gated = true;
  };
}

let scenario = args.scenario || process.env.DOCS_SCENARIO || "healthy";
let world;
let job = { name: "", out: "" };
// steps: resolvers of update steps waiting for /__docs/advance; credits: advances not yet used.
const held = { steps: [], credits: 0, mutation: null };

function reset(name) {
  if (!scenarios[name]) throw new Error(`unknown scenario ${name}`);
  scenario = name;
  world = baseWorld();
  scenarios[name](world);
  held.steps.forEach((r) => r());
  held.steps = [];
  held.credits = 0;
  if (held.mutation) held.mutation();
  held.mutation = null;
}
reset(scenario);

// --- Helpers ---------------------------------------------------------------------

const json = (res, code, body) => {
  res.writeHead(code, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(JSON.stringify(body));
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Waits for /__docs/advance in gated mode, else a short pause. */
function nextStep() {
  if (!world.gated) return sleep(700);
  if (held.credits > 0) { held.credits -= 1; return Promise.resolve(); }
  return new Promise((r) => held.steps.push(r));
}
/** Waits for /__docs/release in gated mode, else a short pause. */
const mutationDone = () => (world.gated ? new Promise((r) => { held.mutation = r; }) : sleep(1500));

function beginOperation(type, label, target) {
  world.operation = {
    inProgress: true,
    operation: { id: "op-" + type, type, label, user: F.USER, ...(target ? { target } : {}), startedAt: new Date(F.NOW).toISOString(), updatedAt: new Date(F.NOW).toISOString() },
    lastCompleted: world.operation.lastCompleted,
  };
}
function endOperation(success, message) {
  const op = world.operation.operation;
  world.operation = {
    inProgress: false, operation: null,
    lastCompleted: { ...op, finishedAt: new Date(F.NOW).toISOString(), success, message },
  };
}

async function body(req) {
  let data = "";
  for await (const chunk of req) data += chunk;
  try { return data ? JSON.parse(data) : {}; } catch { return {}; }
}

// --- API ---------------------------------------------------------------------------

async function updateStream(req, res) {
  const { image = F.LATEST_IMAGE } = await body(req);
  beginOperation("update", "Update to nightly", image);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  const start = Date.now();
  let n = 0;
  for (const step of F.updateSteps(image)) {
    n += 1;
    await nextStep();
    if (res.destroyed) return;
    // Gated (GIFs): a plausible 12 s per event instead of the real time.
    const ev = { ...step, elapsedMs: world.gated ? n * 12000 : Date.now() - start };
    Object.assign(world.operation.operation ?? {}, { step: ev.step, stepStatus: ev.status, message: ev.message });
    if (step.step === "operation_complete") {
      world.status = world.afterUpdate ? world.afterUpdate() : F.healthyStatus(0);
      endOperation(true, step.message);
    }
    res.write(`data: ${JSON.stringify(ev)}\n\n`);
  }
  res.end();
}

async function mutation(res, type, label, apply) {
  beginOperation(type, label);
  await mutationDone();
  const result = apply();
  endOperation(result.success, result.message);
  json(res, 200, result);
}

const minioSetup = () => {
  const migrated = world.resources.minio.migrationPending || world.resources.minio.keptPVCs;
  world.resources.minio = migrated ? F.s3States.migrated : F.s3States.running;
  if (!world.resources.pipelineServers.length) world.resources.minio = { ...world.resources.minio, teardownBlockedReason: undefined };
  const note = world.resources.minio === F.s3States.migrated
    ? " MinIO from the earlier version was replaced. SeaweedFS started fresh: objects stored in MinIO were not copied, so artifacts of earlier pipeline runs return 404; pipeline servers keep working with no edits. Its data volume, PVC minio-pvc (20Gi), is kept for a rollback or a manual copy; teardown deletes it."
    : "";
  return {
    success: true,
    message: `S3 storage (SeaweedFS) deployed with bucket 'pipelines'; pipeline servers reach it at minio-service.minio.svc:9000. Sign in to the admin UI as 'admin'; the password and the S3 keys are in secret 'minio-secret' in namespace 'minio'.${note}`,
    logs: ["OK: Namespace minio", "OK: Secret minio-secret", "OK: PersistentVolumeClaim seaweedfs-pvc", "OK: Deployment seaweedfs ready", "OK: Service minio-service points at SeaweedFS", "OK: Route minio-ui", "OK: Bucket pipelines"],
  };
};

const routes = {
  "GET /api/status": () => world.status,
  "GET /api/operation": () => world.operation,
  "GET /api/version": () => world.version,
  "GET /api/update-check": () => world.updateCheck,
  "GET /api/user/permissions": () => ({ canMutate: true, user: F.USER }),
  "GET /api/latest-nightly": () => F.latestNightly(),
  "GET /api/nightly-tags": () => F.nightlyTags(true),
  "GET /api/build-explorer/tags": (u) => F.nightlyTags(u.searchParams.get("includeDates") !== "false"),
  "GET /api/build-explorer/content": (u) => F.fbcContent(u.searchParams.get("image") || F.INSTALLED_IMAGE),
  "GET /api/build-explorer/contains": (u) => F.prContains(Number(u.searchParams.get("pr")), u.searchParams.getAll("image")),
  "GET /api/components": () => world.components,
  "GET /api/diagnostics": () => world.diagnostics,
  "GET /api/dashboard/state": () => world.dashboard,
  "GET /api/resources/status": () => world.resources,
  "GET /api/resources/projects": () => ({ projects: F.projects }),
  "GET /api/activity": () => world.status.activity ?? [],
  "GET /api/test-pull-secret": () => ({ success: true, message: "The pull secret can pull from quay.io/rhoai.", logs: [] }),
  "GET /api/verify-nodes": () => ({ success: true, message: "All 3 nodes have the pull secret and the image mirror.", logs: [] }),
  "GET /api/setup/dsc/preview": () => ({
    yaml: "apiVersion: datasciencecluster.opendatahub.io/v2\nkind: DataScienceCluster\nmetadata:\n  name: default-dsc\nspec:\n  components:\n    aipipelines:\n      managementState: Managed\n    dashboard:\n      managementState: Managed\n    kserve:\n      managementState: Managed\n    workbenches:\n      managementState: Managed\n",
    operatorVersion: "3.6.0", branch: "", sourceURL: "", source: "csv", sourceDescription: "alm-examples of rhods-operator.3.6.0",
  }),
  "POST /api/update": () => ({ success: true, message: "Dry run passed: the update would install rhods-operator from this build. Nothing was changed.", logs: ["OK: pull secret", "OK: image mirror", "OK: catalog image verified"] }),
};

async function api(req, res, u) {
  const key = `${req.method} ${u.pathname}`;
  if (key === "POST /api/pageview") { res.writeHead(204); res.end(); return; }
  if (key === "POST /api/update/stream") return updateStream(req, res);
  if (key === "POST /api/resources/minio/setup") return mutation(res, "setup-minio", "Set up S3 storage", minioSetup);
  if (key === "POST /api/resources/pipeline-server/setup") {
    const { project = "dev-project" } = await body(req);
    return mutation(res, "setup-pipeline-server", "Set up pipeline server", () => {
      world.resources.pipelineServers = [{ ...F.pipelineServer, namespace: project, ready: false, message: "Starting (1/3 ready)" }];
      return { success: true, message: "Pipeline server created. It will take 1-3 minutes to become ready.", logs: [] };
    });
  }
  if (key === "POST /api/diagnostics/fix") {
    const { problemId = "" } = await body(req);
    return mutation(res, "diagnostics-fix", "Apply diagnostics fix", () => ({
      success: true,
      message: problemId.includes("restart-module-operator")
        ? "Restarted redhat-ods-applications/trainer-operator-controller-manager. The trainer operator reconciles when its new pod starts; re-scan in a minute or two."
        : "Done.",
      logs: [],
    }));
  }
  const handler = routes[key];
  if (handler) return json(res, 200, handler(u));
  if (req.method !== "GET") return json(res, 200, { success: true, message: "Done (docs mock).", logs: [] });
  return json(res, 404, { error: `not in the docs mock: ${key}`, errorCode: "not_found" });
}

// --- Control ----------------------------------------------------------------------

function control(req, res, u) {
  switch (u.pathname) {
    case "/__docs/scenario":
      try { reset(u.searchParams.get("name")); } catch (e) { return json(res, 400, { error: e.message }); }
      return json(res, 200, { scenario });
    case "/__docs/advance": {
      const n = Number(u.searchParams.get("n") || 1);
      for (let i = 0; i < n; i++) {
        if (held.steps.length) held.steps.shift()();
        else held.credits += 1;
      }
      return json(res, 200, { released: n });
    }
    case "/__docs/release":
      if (held.mutation) { held.mutation(); held.mutation = null; }
      return json(res, 200, {});
    case "/__docs/state":
      return json(res, 200, { scenario, now: F.NOW, waitingSteps: held.steps.length, waitingMutation: !!held.mutation, scenarios: Object.keys(scenarios) });
    case "/__docs/job":
      if (req.method === "POST") job = { name: u.searchParams.get("name") || "", out: u.searchParams.get("out") || "" };
      return json(res, 200, job);
    default:
      return json(res, 404, {});
  }
}

// --- Static files ------------------------------------------------------------------

const TYPES = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".woff": "font/woff", ".json": "application/json", ".ico": "image/x-icon" };

async function file(res, path) {
  const full = normalize(join(DIST, path));
  if (full !== DIST && !full.startsWith(DIST + sep)) return false;
  try {
    if (!(await stat(full)).isFile()) return false;
    res.writeHead(200, { "Content-Type": TYPES[extname(full)] || "application/octet-stream" });
    res.end(await readFile(full));
    return true;
  } catch {
    return false;
  }
}

createServer(async (req, res) => {
  const u = new URL(req.url, "http://localhost");
  try {
    if (u.pathname.startsWith("/__docs/")) return control(req, res, u);
    if (u.pathname.startsWith("/api/")) return await api(req, res, u);
    if (await file(res, decodeURIComponent(u.pathname))) return;
    if (!(await file(res, "index.html"))) json(res, 500, { error: `no frontend build in ${DIST}: run npm run build in frontend/` });
  } catch (e) {
    json(res, 500, { error: String(e) });
  }
}).listen(PORT, "127.0.0.1", () => console.log(`docs mock on http://127.0.0.1:${PORT} (scenario ${scenario})`));
