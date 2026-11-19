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
//   POST /__docs/hold?path=P      hold GET requests under path P (repeatable) until
//                                 POST /__docs/hold without a path (the loading states)
//   --delay <ms>                  answer every GET /api/resources/* that much later
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
    // No PR or main build runs: nothing to describe.
    dashboardBuilds: { builds: [] },
    resources: { minio: F.s3States.running, mlflow: F.mlflowStates.none, pipelineServers: [F.pipelineServer] },
    // The Gen AI playground (GET /api/resources/playground).
    playground: F.playgroundWorld(),
    // Models as a Service (GET /api/resources/maas).
    maas: F.maasWorld("none"),
    // Sample states by project; a project not named has none of the samples.
    samples: { mlflow: {} },
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
  // Self-managed OpenShift: the nodes pull with openshift-config/pull-secret.
  // Before step 1, after it (nodes not checked; the check passes, or fails
  // with -fails), with an unreadable topology, and once a nightly runs.
  "self-managed": (w) => { scenarios.fresh(w); F.selfManaged(w.status); },
  "self-managed-saved": (w) => { scenarios["ready-to-install"](w); F.selfManaged(w.status); },
  "self-managed-saved-fails": (w) => { scenarios["self-managed-saved"](w); w.nodePullFails = true; },
  "self-managed-unknown": (w) => { scenarios["ready-to-install"](w); F.selfManaged(w.status, { known: false }); },
  "self-managed-installed": (w) => { F.selfManaged(w.status, { pulled: true }); },
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
      installerURL: "https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/download/v2.0.0/install.sh",
    };
  },
  "patch-update": (w) => {
    w.updateCheck = {
      current: "v2.0.0", latest: "v2.0.1", updateAvailable: true, majorUpgrade: false,
      releaseNotesURL: "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases/v2.0.1",
      installerURL: "https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/download/v2.0.1/install.sh",
    };
  },
  "template-outdated": (w) => { w.version = F.versionInfo({ templateRevision: "3", templateOutdated: true }); },
  "components-not-ready": (w) => { w.components = F.notReadyComponents(); },
  // The page compared the DSC as v2; when the reset dialog opens, the defaults are v3 (the conversion recovered).
  "components-reset-stale": (w) => {
    w.components.dscCompatibility = { ...w.components.dscCompatibility, missingComponents: ["sparkoperator"], resetRemovals: ["trainer"] };
    w.previewAPIVersion = "datasciencecluster.opendatahub.io/v3";
  },
  // Argo CD manages the catalog, the Subscription and the DSC (RUNBOOK §13):
  // Git moved the catalog to a build the operator does not run yet.
  gitops: (w) => {
    w.status = F.updateAvailableStatus();
    w.status.subscription = { ...w.status.subscription, channel: "beta", managedBy: "rhoai" };
    w.status.catalogSource = { ...w.status.catalogSource, managedBy: "rhoai" };
    w.status.csv = { ...w.status.csv, name: "rhods-operator.3.6.0-ea.1", version: "3.6.0-ea.1" };
    w.status.catalogServes = { channel: "beta", csv: "rhods-operator.3.6.0-ea.1", operatorImage: "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:3f9e7b1c5d2a8e406b1f9c3d7a2e5b8f0c6d4a193f9e7b1c5d2a8e406b1f9c3d", built: "2026-10-05T21:47:53Z", installed: false };
    w.status.dscManagedBy = "rhoai";
  },
  // Same, once the operator runs the build Git set; Quay has a newer one.
  "gitops-synced": (w) => {
    scenarios.gitops(w);
    w.status.catalogServes = { ...w.status.catalogServes, installed: true };
  },
  // The operator Git set is Failed: the fix is to install it again, or change Git.
  "gitops-failed": (w) => {
    scenarios["gitops-synced"](w);
    w.status.csv = { ...w.status.csv, phase: "Failed" };
  },
  // Argo CD created the catalog and the Subscription, nothing installed yet (OLM stuck or first sync).
  "gitops-first-install": (w) => {
    scenarios.gitops(w);
    w.status.csv = { name: "", version: "", phase: "Not Found" };
    w.status.subscription = { ...w.status.subscription, state: "UpgradePending" };
    delete w.status.installPlan;
    w.status.catalogServes = { ...w.status.catalogServes, installed: undefined };
  },
  // Git just moved the image: OLM is still replacing the catalog pod.
  "gitops-updating": (w) => {
    scenarios["gitops-synced"](w);
    w.status.catalogServes = { ...w.status.catalogServes, installed: undefined, updating: true };
  },
  // Argo CD created the Subscription; OLM is running the first InstallPlan.
  "gitops-installing": (w) => {
    scenarios["gitops-first-install"](w);
    w.status.installPlan = { name: "install-7xk2p", phase: "Installing", approved: true };
  },
  // The GitOps install, step by step (gated like update-flow).
  "gitops-update-flow": (w) => {
    scenarios.gitops(w);
    w.gated = true;
    // Once installed, the operator runs the build Git set.
    w.afterUpdate = () => { const done = {}; scenarios["gitops-synced"](done); return done.status; };
  },
  // Components and Diagnostics while Argo CD manages the DSC.
  "gitops-components": (w) => { scenarios["gitops-synced"](w); w.components = F.notReadyComponents(); w.components.dscCompatibility = { ...w.components.dscCompatibility, missingComponents: ["sparkoperator"], resetRemovals: ["trainer"] }; },
  "gitops-diag": (w) => { scenarios["gitops-synced"](w); w.diagnostics = F.diagnostics("upgrade-gates"); w.components = F.notReadyComponents(); },
  // The updater's gitops profile (PROFILE=gitops, docs/GITOPS.md "Profiles"):
  // the backend refuses every route but Update and the reads with 403
  // profile_gitops, and only cluster-admins may read.
  "gitops-profile": (w) => { scenarios.gitops(w); w.profile = "gitops"; },
  "gitops-profile-synced": (w) => { scenarios["gitops-synced"](w); w.profile = "gitops"; },
  "gitops-profile-components": (w) => { scenarios["gitops-components"](w); w.profile = "gitops"; },
  "gitops-profile-diag": (w) => { scenarios["gitops-diag"](w); w.profile = "gitops"; },
  // The profile on a cluster whose catalog Argo CD does not manage: Update refuses.
  "gitops-profile-unmanaged": (w) => { w.status = F.updateAvailableStatus(); w.profile = "gitops"; },
  // The profile's admission policy is gone (Update is off until Argo CD syncs it).
  "gitops-profile-no-policy": (w) => {
    scenarios.gitops(w); w.profile = "gitops";
    w.admissionPolicy = "The gitops profile's admission policy is not in place (read ValidatingAdmissionPolicy rhoai-nightly-updater-rhoai-nightly-updater-gitops: k8s API error 404 (NotFound)), so Update is off until Argo CD syncs it again (docs/GITOPS.md, section 11).";
  },
  // A signed-in user who is not a cluster-admin.
  "gitops-profile-reader": (w) => { scenarios.gitops(w); w.profile = "gitops"; w.canMutate = false; },
  "dashboard-session": (w) => { w.dashboard = F.dashboardSession(); w.dashboardBuilds = F.dashboardBuilds(w.dashboard); },
  "s3-none": (w) => { w.resources = { minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [] }; w.gated = true; },
  "s3-running": () => {},
  "s3-migrate": (w) => {
    w.resources = { minio: F.s3States.migrationNeeded, mlflow: F.mlflowStates.none, pipelineServers: [F.legacyPipelineServer], legacyStorage: F.legacyStorage("migrate") };
    w.gated = true;
  },
  "s3-migrate-remove": (w) => {
    w.resources = { minio: F.s3States.migrated, mlflow: F.mlflowStates.none, pipelineServers: [F.legacyPipelineServer], legacyStorage: F.legacyStorage("remove") };
    w.gated = true;
  },
  "s3-incomplete": (w) => { w.resources.minio = F.s3States.incomplete; w.gated = true; },
  mlflow: (w) => { w.resources.mlflow = F.mlflowStates.running; },
  // The Environment page's other states, one scenario each.
  "env-ready": (w) => { w.resources.mlflow = F.mlflowStates.running; },
  // A pipeline server an earlier version set up as nightly-dspa: the workbench notice and Recreate.
  "env-old-name": (w) => {
    w.resources.pipelineServers = [F.pipelineServerOldName];
    w.resources.minio = { ...w.resources.minio, teardownBlockedReason: "1 pipeline server uses this S3 storage: dev-project/nightly-dspa. Delete it first.", usedBy: ["dev-project/nightly-dspa"] };
  },
  "env-starting": (w) => { w.resources = { minio: F.s3States.starting, mlflow: F.mlflowStates.none, pipelineServers: [F.pipelineServerStarting] }; },
  "env-error": (w) => {
    w.resources = { minio: { ...F.s3States.imagePull, teardownBlockedReason: F.s3States.running.teardownBlockedReason, usedBy: F.s3States.running.usedBy }, mlflow: F.mlflowStates.prBroken, pipelineServers: [F.pipelineServer] };
  },
  "env-foreign": (w) => {
    w.resources = {
      minio: F.s3States.foreign, mlflow: F.mlflowStates.foreign, pipelineServers: [],
      unmanagedPipelineProjects: ["fraud-detection"], unmanagedPipelineServers: [F.foreignPipelineServer],
    };
  },
  // MLflow made in the dashboard (by the MLflow operator), running or broken; the rest is this tool's.
  "env-mlflow-unmanaged": (w) => { w.resources.mlflow = F.mlflowStates.foreign; },
  // The storage alone: its Tear down is not blocked, and offers the namespace.
  "env-storage-only": (w) => {
    w.resources = { minio: { ...F.s3States.running, teardownBlockedReason: undefined, usedBy: undefined, otherObjects: ["Deployment notes-app"] }, mlflow: F.mlflowStates.none, pipelineServers: [] };
  },
  "env-mlflow-unmanaged-broken": (w) => { w.resources.mlflow = F.mlflowStates.foreignBroken; },
  // Like the live cluster on 2026-11-17: an empty project sorts first, the
  // pipeline server is in a later one, and MLflow was made in the dashboard.
  "env-live": (w) => {
    w.projects = ["a-test-project", ...F.projects];
    w.resources.mlflow = F.mlflowStates.foreign;
  },
  "env-reader": (w) => { w.resources.mlflow = F.mlflowStates.running; w.canMutate = false; },
  // A fresh cluster whose DataScienceCluster turns pipelines and MLflow off.
  "env-components-off": (w) => {
    w.resources = {
      minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [],
      pipelinesBlocked: { component: "aipipelines", dsc: "default-dsc", state: "Removed", reason: "Data Science Pipelines is turned off on this cluster: component aipipelines is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Managed." },
      mlflowBlocked: { component: "mlflowoperator", dsc: "default-dsc", state: "Removed", reason: "MLflow is turned off on this cluster: component mlflowoperator is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Managed." },
    };
  },
  // No data science project yet.
  "env-no-projects": (w) => { w.resources = { minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [] }; w.projects = []; },
  // Samples in dev-project, every state at least once across these scenarios.
  "env-samples": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { "genai-demo": "present", training: "partial", "mcp-server": "foreign", skill: "unsupported" });
  },
  "env-samples-new": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { training: "removed" });
  },
  "env-samples-busy": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { "genai-demo": "partial", training: "present", "mcp-server": "present", skill: "unknown" });
  },
  "env-samples-failed": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { "genai-demo": "present", training: "present", "mcp-server": "present", skill: "present" });
  },
  "env-samples-foreign": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { training: "foreign", "mcp-server": "foreign", skill: "foreign" });
  },
  "env-samples-reader": (w) => { scenarios["env-samples"](w); w.canMutate = false; },
  // The demo: an MLflow UI demo (foreign), the old 2Gi limit on this tool's
  // MLflow and on someone else's, and the per-user limit on a change.
  "env-samples-demo-foreign": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { "genai-demo": "foreign", training: "present" });
  },
  "env-samples-demo-lowmem": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    const st = F.mlflowSamples("dev-project", { training: "present" });
    st.samples[0] = { ...st.samples[0], ...F.DEMO_LOW_MEMORY };
    w.samples.mlflow["dev-project"] = st;
  },
  "env-samples-demo-lowmem-foreign": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { training: "present" });
    w.samples.mlflow["dev-project"].samples[0] = F.sampleStatus("dev-project", "genai-demo", "unsupported", { message: F.DEMO_LOW_MEMORY_FOREIGN });
  },
  "env-samples-rate-limited": (w) => { scenarios["env-samples-new"](w); w.samplesRateLimited = true; },
  // Models: every state of the gateway and the model, one scenario each.
  "env-models-ready": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.modelStates.released("dev-project")] });
  },
  // Several catalog models in one project: Qwen2.5, Qwen3 and the embedding model.
  "env-models-several": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/granite-embedding-125m-english", "dev-project/qwen25-05b-instruct", "dev-project/qwen3-1-7b"]);
    w.resources.models = F.modelsStatus({ items: [F.modelStates.released("dev-project"), F.modelStates.readyAs("qwen3-1-7b"), F.modelStates.readyAs("granite-embedding-125m-english")] });
  },
  "env-models-starting": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.modelStates.starting("dev-project")] });
  },
  "env-models-image-missing": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ imagePlan: "preset", imagePlanReason: undefined, items: [F.modelStates.imageMissing("dev-project")] });
  },
  "env-models-foreign": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.foreign(["llm-eval/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ imagePlan: "preset", imagePlanReason: undefined, items: [F.modelStates.foreign("llm-eval")] });
  },
  // A model deployed from the dashboard in a project the gateway does not
  // list: its route is rejected; Allow adds the project.
  "env-models-not-allowed": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.notAllowed(F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]), ["fraud-detection/fraud-assistant"]);
    w.resources.models = F.modelsStatus({ items: [F.modelStates.released("dev-project")] });
  },
  "env-models-unknown": (w) => { w.resources.inferenceGateway = F.gatewayStates.unknown; },
  "env-models-gateway-starting": (w) => { w.resources.inferenceGateway = F.gatewayStates.starting; },
  // The room check: no worker has room for Qwen3 now, so the picker disables it.
  "env-models-capacity": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ catalog: F.catalogWithoutRoom(), items: [F.modelStates.released("dev-project")] });
  },
  "env-models-blocked": (w) => {
    w.resources.models = F.modelsStatus({ imagePlan: undefined, imagePlanReason: undefined, preset: undefined, dependencies: "False",
      blocked: { component: "kserve", reason: "KServe cannot serve LLMInferenceServices yet: DataScienceCluster default-dsc reports KserveLLMInferenceServiceDependencies=False (LeaderWorkerSet operator is not installed). A cluster-admin installs what it names; the Components page shows the details." } });
  },
  "env-models-reader": (w) => { scenarios["env-models-ready"](w); w.canMutate = false; },
  // Models routing: how a new model is reached, and whether a ready one can be called.
  "env-models-choose": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"))] });
    w.routing = { maasReady: true, otherGateway: true };
  },
  "env-models-choose-none": (w) => { w.resources.inferenceGateway = F.gatewayStates.none; w.resources.models = F.modelsStatus(); w.routing = {}; },
  "env-models-routed": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"))] });
  },
  "env-models-not-callable": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"), { check: "route" })] });
  },
  // The gateway answers a call without a token: the check fails as unauthenticated.
  "env-models-unauthenticated": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"), { check: "unauthenticated" })] });
  },
  // A fresh cluster: the Overview's Models row needs cluster setup (planned for the recommended gateway).
  "env-overview-prereq": (w) => { w.resources.inferenceGateway = F.gatewayStates.none; w.resources.models = F.modelsStatus(); w.prereq = { models: "missing", pipelines: "missing" }; w.routing = { authReady: false }; },
  "env-models-internal": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.none;
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"), { exposure: "internal", check: undefined })] });
  },
  // Set up on the Models tab through MaaS only: published as part of its setup.
  "env-models-maas-only": (w) => {
    w.resources.inferenceGateway = F.gatewayStates.none;
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"), { exposure: "maas", check: undefined })] });
    F.setMaaSPublished(w.resources, "dev-project", "qwen25-05b-instruct", true, { maasOnlySetup: true });
    w.maas = F.maasWorld("ready", { items: [F.maasModel(true, { maasOnlySetup: true })] });
  },
  "env-models-stale-projects": (w) => {
    w.resources.inferenceGateway = { ...F.gatewayStates.ready(["dev-project/qwen25-05b-instruct"]), allowedProjects: ["openshift-ingress", "dev-project", "e2e-v300", "llm-eval"], unusedProjects: ["e2e-v300", "llm-eval"], goneProjects: ["e2e-v300"] };
    w.resources.models = F.modelsStatus({ items: [F.routed(F.modelStates.released("dev-project"))] });
  },
  "env-models-prereq": (w) => { w.resources.inferenceGateway = F.gatewayStates.ready([]); w.resources.models = F.modelsStatus(); w.prereq = { models: "missing" }; w.routing = { authReady: false }; },
  "env-pipelines-prereq": (w) => { w.prereq = { pipelines: "missing" }; },
  "env-models-gated": (w) => { w.resources.models = F.modelsStatus(); w.gated = true; },
  // Playground: every state in dev-project, on a ready model.
  // With this tool's embedding model, which Set up adds as one.
  "env-playground-none": (w) => { scenarios["env-models-several"](w); w.playground.modelsByProject["dev-project"] = [F.playgroundModels.qwen, F.playgroundModels.granite, F.playgroundModels.maas, F.playgroundModels.opt]; },
  // MaaS: the platform and this tool's model in dev-project.
  "env-maas-none": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("none", { items: [F.maasModel(false)] }); },
  "env-maas-blocked": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("none", { blocked: F.MAAS_BLOCKED, items: [F.maasModel(false)] }); },
  "env-maas-no-cnpg": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("none", { noCNPG: true, items: [F.maasModel(false)] }); },
  // A cluster that never had MaaS: Set up installs and creates everything.
  "env-maas-fresh": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("none", { rhcl: "missing", items: [F.maasModel(false)] }); },
  // Set up by this tool on Connectivity Link 1.4.3: Upgrade Connectivity Link is offered.
  "env-maas-rhcl-143": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { rhcl: "1.4.3", items: [F.maasModel(false)] }); },
  // A tier-based MaaS left its gateway policies: Remove the old tier policies is offered.
  "env-maas-tier-policies": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { tierPolicies: true, items: [F.maasModel(false)] }); },
  "env-maas-starting": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("starting", { items: [F.maasModel(false)] }); },
  "env-maas-tls": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { tls: false, items: [F.maasModel(false)] }); },
  // Authorino's CR names a certificate Secret that is not a Kubernetes name: no commands.
  "env-maas-tls-invalid": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { tls: "invalid-secret", items: [F.maasModel(false)] }); },
  "env-maas-ready": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { items: [F.maasModel(false)] }); },
  "env-maas-publishing": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { items: [F.maasModel(true, { ready: false })] }); },
  // Three models in dev-project: Qwen2.5 published, Qwen3 not, the embedding model never offered.
  "env-maas-published": (w) => { scenarios["env-models-several"](w); w.maas = F.maasWorld("ready", { items: F.maasSeveral() }); F.setMaaSPublished(w.resources, "dev-project", "qwen25-05b-instruct", true); },
  // Two published models with different limits, for two groups (one without an OpenShift Group).
  "env-maas-limits": (w) => {
    scenarios["env-models-several"](w);
    w.maas = F.maasWorld("ready", { access: { everyone: false, groups: ["ml-team", "data-science"], groupProblems: F.maasGroupProblems(["ml-team", "data-science"]) },
      items: F.maasSeveral({ ...F.maasModel(true), limit: F.maasLimit("premium") }, { ...F.maasModel(true, { name: "qwen3-1-7b" }), limit: F.maasLimit("unlimited") }) });
    F.setMaaSPublished(w.resources, "dev-project", "qwen25-05b-instruct", true);
    F.setMaaSPublished(w.resources, "dev-project", "qwen3-1-7b", true);
  },
  // Ready on the MaaS docs' development database: Change database from it (to CloudNativePG or yours).
  "env-maas-dev-db": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { dbCurrent: "development", items: [F.maasModel(false)] }); },
  // Ready on the user's PostgreSQL: Change database to a new URL of theirs.
  "env-maas-your-db": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { dbCurrent: "external", items: [F.maasModel(false)] }); },
  // The cluster's MaaSSubscription API has no unlimited: Publish shows it disabled.
  "env-maas-no-unlimited": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { noUnlimited: true, items: [F.maasModel(false)] }); },
  // The same, on the Models tab: Use it of Qwen2.5 points at the MaaS tab's commands; Qwen3's keeps its curl.
  "env-models-published": (w) => { scenarios["env-maas-published"](w); },
  // A publication by an earlier build (the model also on the inference gateway): partly published.
  "env-maas-earlier-build": (w) => { scenarios["env-models-several"](w); w.maas = F.maasWorld("ready", { items: F.maasSeveral(F.maasEarlierBuildPublication()) }); F.setMaaSPublished(w.resources, "dev-project", "qwen25-05b-instruct", true); },
  // Qwen3 published by this build, then someone added the inference gateway to it: partly published, set the refs, then Unpublish.
  "env-maas-someone-added": (w) => {
    scenarios["env-models-several"](w);
    w.maas = F.maasWorld("ready", { items: F.maasSeveral(F.maasModel(true), F.maasSomeoneAddedPublication({ name: "qwen3-1-7b" })) });
    F.setMaaSPublished(w.resources, "dev-project", "qwen25-05b-instruct", true);
    F.setMaaSPublished(w.resources, "dev-project", "qwen3-1-7b", true);
  },
  "env-maas-foreign": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("foreign", { items: [F.maasModel(false)] }); },
  "env-maas-no-model": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready"); },
  // Not turned on yet while Authorino TLS needs a cluster-admin: Set up is still offered.
  "env-maas-new-tls": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("none", { tls: false, items: [F.maasModel(false)] }); },
  // A recorded publication whose model was deleted elsewhere: a row with Unpublish.
  "env-maas-orphan": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { items: [F.maasModel(true), F.maasOrphanPublication("old-project")] }); },
  // Others publish through the platform: Remove refuses before changing anything.
  "env-maas-in-use": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { items: [F.maasModel(false)] }); w.maasRefusal = F.MAAS_IN_USE; },
  "env-playground-no-model": (w) => { w.playground.modelsByProject["dev-project"] = []; },
  "env-playground-model-not-ready": (w) => {
    scenarios["env-models-ready"](w);
    w.playground.modelsByProject["dev-project"] = [F.playgroundModels.qwen, F.playgroundModels.deepseek, F.playgroundModels.maas];
  },
  "env-playground-starting": (w) => { scenarios["env-models-ready"](w); w.playground.items = [F.playgroundStates.starting()]; },
  "env-playground-ready": (w) => { scenarios["env-models-ready"](w); w.playground.items = [F.playgroundStates.ready()]; },
  "env-playground-foreign": (w) => { scenarios["env-models-ready"](w); w.playground.items = [F.playgroundStates.foreign()]; },
  "env-playground-failed": (w) => { scenarios["env-models-ready"](w); w.playground.items = [F.playgroundStates.failed()]; },
  "env-playground-api-differs": (w) => { scenarios["env-models-ready"](w); w.playground.apiProblem = F.PLAYGROUND_API_DIFFERS; w.playground.apiCode = "api_missing"; },
  "env-playground-blocked": (w) => { scenarios["env-models-ready"](w); w.playground.blocked = F.PLAYGROUND_BLOCKED; },
  // The Overview with features in several projects (overview-review).
  // Pipelines, Models and Playground ready in all three projects.
  "env-overview-three": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.resources.pipelineServers = F.projects.map((ns) => F.pipelineServerIn(ns));
    w.resources.inferenceGateway = F.gatewayStates.ready(F.projects.map((ns) => `${ns}/qwen25-05b-instruct`));
    w.resources.models = F.modelsStatus({ items: F.projects.map((ns) => F.modelStates.released(ns)) });
    w.playground.items = F.projects.map((ns) => F.playgroundStates.ready(ns));
    w.maas = F.maasWorld("ready", { items: [F.maasModel(true)] });
  },
  // The same, with one project failing in each: "Error in 1 of 3 projects".
  "env-overview-mixed": (w) => {
    scenarios["env-overview-three"](w);
    w.resources.pipelineServers = [F.pipelineServerIn("dev-project"), F.pipelineServerStuck("fraud-detection"), F.pipelineServerIn("llm-eval")];
    w.resources.models = F.modelsStatus({ imagePlan: "preset", imagePlanReason: undefined, items: [F.modelStates.released("dev-project"), F.modelStates.released("fraud-detection"), F.modelStates.imageMissing("llm-eval")] });
    w.playground.items = [F.playgroundStates.ready("dev-project"), F.playgroundStates.failed("fraud-detection"), F.playgroundStates.ready("llm-eval")];
  },
  // A 40-character project name: alone (Pipelines, Playground) and as the project with the problem (Models).
  "env-overview-long-name": (w) => {
    const long = "customer-churn-prediction-experiments-v2";
    w.projects = [long, ...F.projects];
    w.resources.mlflow = F.mlflowStates.foreign;
    w.resources.pipelineServers = [F.pipelineServerIn(long)];
    w.resources.inferenceGateway = F.gatewayStates.ready([`${long}/qwen25-05b-instruct`, "dev-project/qwen25-05b-instruct"]);
    w.resources.models = F.modelsStatus({ imagePlan: "preset", imagePlanReason: undefined, items: [F.modelStates.imageMissing(long), F.modelStates.released("dev-project")] });
    w.playground.items = [F.playgroundStates.ready(long)];
    w.maas = F.maasWorld("foreign", { items: [F.maasModel(false)] });
  },
  // Sample changes wait for /__docs/advance: a row shows its operation.
  "env-samples-gated": (w) => { scenarios["env-samples-new"](w); w.gated = true; },
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
// Reads held for the loading-state images: GET /api/* requests whose path
// starts with one of these prefixes wait until POST /__docs/hold without a
// path lets them through (or a scenario reset does).
const heldReads = { prefixes: [], waiting: [] };
// --delay <ms> (make docs-mock DELAY=<ms>): every GET /api/resources/* answers
// that much later, to look at the Environment page's loading states by hand.
const READ_DELAY_MS = Number(args.delay || process.env.DOCS_MOCK_DELAY || 0);

function releaseReads() {
  heldReads.prefixes = [];
  heldReads.waiting.splice(0).forEach((r) => r());
}

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
  releaseReads();
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
  const gitOps = !!(world.status?.catalogSource?.managedBy || world.status?.subscription?.managedBy);
  for (const step of (gitOps ? F.gitOpsUpdateSteps : F.updateSteps)(image)) {
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
  world.resources.minio = F.s3States.running;
  if (!world.resources.pipelineServers.length) world.resources.minio = { ...world.resources.minio, teardownBlockedReason: undefined, usedBy: undefined };
  return {
    success: true,
    message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'; pipeline servers reach it at http://seaweedfs-s3.seaweedfs.svc:8333. Sign in to the admin UI as 'admin'; the password (the S3 secret key) and the S3 keys are in Secret 'seaweedfs-s3-credentials' in namespace 'seaweedfs'.",
    logs: ["OK: Namespace created", "OK: PVC created", "OK: Secret created", "OK: NetworkPolicy created", "OK: Deployment created", "OK: SeaweedFS is running", "OK: Service created", "OK: Admin UI Route created", "OK: Bucket 'pipelines' created"],
  };
};

/** Migrate storage: its steps wait for /__docs/advance in gated mode. */
async function migrateStream(req, res) {
  const { removeOldStorage = false } = await body(req);
  const kind = removeOldStorage ? "remove" : "migrate";
  beginOperation("migrate-storage", "Migrate S3 storage", removeOldStorage ? "remove old storage" : "migrate");
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  for (const step of F.migrateSteps(kind)) {
    n += 1;
    await nextStep();
    if (res.destroyed) return;
    const ev = { ...step, elapsedMs: n * 4000 };
    if (step.step === "operation_complete") {
      if (kind === "migrate") {
        world.resources.minio = F.s3States.migrated;
        world.resources.legacyStorage = F.legacyStorage("remove");
      } else {
        world.resources.minio = { ...F.s3States.running, teardownBlockedReason: F.s3States.migrated.teardownBlockedReason, usedBy: F.s3States.migrated.usedBy };
        delete world.resources.legacyStorage;
      }
      endOperation(true, step.message);
    }
    res.write(`data: ${JSON.stringify(ev)}\n\n`);
  }
  res.end();
}

/** Clean up: one step per part (pkg/cluster CleanUp); each waits for /__docs/advance in gated mode. */
async function cleanupStream(req, res) {
  const { pipelineServers = [], storage = false, deleteNamespace = false, mlflow = false, models = [], inferenceGateway = false, playgrounds = [] } = await body(req);
  beginOperation("cleanup", "Clean up the environment", [pipelineServers.length ? `${pipelineServers.length} pipeline servers` : "", storage ? "S3 storage" : "", mlflow ? "MLflow" : ""].filter(Boolean).join(", "));
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  const parts = [
    ...pipelineServers.map((p) => ({ id: `pipeline-server:${p}`, title: `the pipeline server in '${p}'`, done: `Pipeline server removed from '${p}'.`, apply: () => { world.resources.pipelineServers = world.resources.pipelineServers.filter((ps) => ps.namespace !== p); } })),
    ...(storage ? [{ id: "storage", title: "the S3 storage", done: deleteNamespace ? "S3 storage removed. Namespace 'seaweedfs' is being deleted." : "S3 storage removed.", apply: () => { world.resources.minio = F.s3States.none; delete world.resources.legacyStorage; } }] : []),
    ...(mlflow ? [{ id: "mlflow", title: "MLflow", done: "MLflow removed.", apply: () => { world.resources.mlflow = F.mlflowStates.none; } }] : []),
    ...playgrounds.map((p) => ({ id: `playground:${p}`, title: `the playground in '${p}'`, done: `Playground deleted from ${p}: the dashboard removed its server, configuration and database. The project and its models stay.`, apply: () => removePlayground(p) })),
    ...models.map((p) => ({ id: `model:${p}`, title: `the models in '${p}'`, done: `Model ${(world.resources.models?.items ?? []).filter((i) => i.namespace === p && i.managedByTool !== false).map((i) => i.name).join(", ")} deleted from '${p}'; KServe removed its Deployment, Service and route. The project stays.`, apply: () => removeModel(p) })),
    ...(inferenceGateway ? [{ id: "inference-gateway", title: "the inference gateway", done: "Inference gateway deleted: Gateway openshift-ingress/openshift-ai-inference, ConfigMap openshift-ingress/openshift-ai-inference-options and GatewayClass openshift-ai-inference. OpenShift removes its load balancer and the ingress operator its DNS record.", apply: () => { world.resources.inferenceGateway = F.gatewayStates.none; } }] : []),
  ];
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  for (const p of parts) {
    send({ step: p.id, status: "running", message: `Deleting ${p.title}...` });
    await nextStep();
    if (res.destroyed) return;
    p.apply();
    send({ step: p.id, status: "success", message: p.done });
  }
  const titles = parts.map((p) => p.title);
  const message = `Deleted ${titles.length > 1 ? `${titles.slice(0, -1).join(", ")} and ${titles[titles.length - 1]}` : titles[0]}.`;
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** The Models part of the world, created on first change. */
function modelsWorld() {
  world.resources.models ??= F.modelsStatus();
  world.resources.inferenceGateway ??= F.gatewayStates.none;
  return world.resources.models;
}

/** Deletes this tool's model name in project (every one of its models when no name is given). */
function removeModel(project, name) {
  const m = modelsWorld();
  m.items = (m.items ?? []).filter((i) => i.namespace !== project || i.managedByTool === false || (name && i.name !== name));
  const left = (m.items ?? []).filter((i) => i.managedByTool !== false).map((i) => `${i.namespace}/${i.name}`);
  if (world.resources.inferenceGateway?.managedByTool) world.resources.inferenceGateway = F.gatewayStates.ready(left);
}

/** POST /api/resources/models/stream (pkg/cluster RunModels). */
async function modelsStream(req, res) {
  const { action, gateway = false, project, releasedImage = false, model = "qwen25-05b-instruct", exposure = "default-gateway" } = await body(req);
  const display = F.MODEL_CATALOG.find((c) => c.name === model)?.displayName ?? model;
  beginOperation("models", "Models", action === "allow" ? `allow project ${project} on the inference gateway` : `${action} ${[gateway ? "inference gateway" : "", project ? `model ${model} in ${project}` : ""].filter(Boolean).join(", ")}`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  const m = modelsWorld();
  const done = [];
  if (action === "check") {
    send({ step: "callable", status: "running", message: `Calling ${model} in '${project}' once...` });
    await nextStep();
    const msg = `Answers requests: a request for model "${model}" got 200.`;
    m.items = (m.items ?? []).map((i) => (i.namespace === project && i.name === model ? { ...i, callCheck: { result: "ok", ok: true, status: 200, message: msg, at: new Date().toISOString() } } : i));
    send({ step: "callable", status: "success", message: msg });
    done.push(msg);
  } else if (action === "disallow") {
    send({ step: "check", status: "success", message: "Checked." });
    send({ step: "gateway", status: "running", message: `Taking '${project}' off the projects the inference gateway allows...` });
    await nextStep();
    const gw = world.resources.inferenceGateway;
    world.resources.inferenceGateway = { ...gw, allowedProjects: gw.allowedProjects.filter((p) => p !== project), unusedProjects: (gw.unusedProjects ?? []).filter((p) => p !== project), goneProjects: (gw.goneProjects ?? []).filter((p) => p !== project) };
    const msg = `The inference gateway no longer allows '${project}': routes from it no longer attach.`;
    send({ step: "gateway", status: "success", message: msg });
    done.push(msg);
  } else if (action === "allow") {
    send({ step: "check", status: "running", message: `Checking the inference gateway and project '${project}'...` });
    await nextStep();
    send({ step: "check", status: "success", message: "The gateway is this tool's; the project exists." });
    send({ step: "gateway", status: "running", message: `Adding '${project}' to the projects the inference gateway allows...` });
    await nextStep();
    const gw = world.resources.inferenceGateway;
    const allowed = [gw.allowedProjects[0], ...[...gw.allowedProjects.slice(1), project].sort()];
    const models = (gw.notAllowedModels ?? []).filter((r) => !r.startsWith(`${project}/`));
    world.resources.inferenceGateway = { ...gw, allowedProjects: allowed, notAllowedModels: models, notAllowedModelProjects: [...new Set(models.map((r) => r.split("/")[0]))] };
    const msg = `The inference gateway now allows '${project}'; models there attach to it. Projects it allows: ${allowed.join(", ")}.`;
    send({ step: "gateway", status: "success", message: msg });
    done.push(msg);
  } else if (action === "teardown") {
    // A model set up through MaaS only is unpublished first, in the same
    // operation (pkg/cluster unpublishMaaSOnly).
    if (project && (m.items ?? []).some((i) => i.namespace === project && i.name === model && i.maasOnlySetup)) {
      send({ step: "unpublish", status: "running", message: "The model was set up to be reached through MaaS only, so it is unpublished first..." });
      await nextStep();
      F.setMaaSPublished(world.resources, project, model, false);
      world.maas = { ...world.maas, items: (world.maas.items ?? []).map((i) => (i.namespace === project && i.name === model ? F.maasModel(false, { name: model }) : i)) };
      send({ step: "unpublish", status: "success", message: `${model} in '${project}' is unpublished from MaaS.` });
    }
    if (project) {
      send({ step: "model", status: "running", message: `Deleting ${model} in '${project}'...` });
      await nextStep();
      removeModel(project, model);
      const msg = `Model ${model} deleted from '${project}'; KServe removed its Deployment, Service and route. The project stays.`;
      send({ step: "model", status: "success", message: msg });
      done.push(msg);
    }
    if (gateway) {
      send({ step: "gateway", status: "running", message: "Deleting the inference gateway..." });
      await nextStep();
      world.resources.inferenceGateway = F.gatewayStates.none;
      const msg = "Inference gateway deleted. OpenShift removes its load balancer and the ingress operator its DNS record.";
      send({ step: "gateway", status: "success", message: msg });
      done.push(msg);
    }
  } else {
    send({ step: "check", status: "running", message: "Checking what every part needs..." });
    await nextStep();
    send({ step: "check", status: "success", message: "Everything the parts need is there." });
    if (gateway) {
      send({ step: "gateway", status: "running", message: "Creating the inference gateway..." });
      world.resources.inferenceGateway = F.gatewayStates.starting;
      send({ step: "gateway", status: "success", message: "GatewayClass, ConfigMap and Gateway openshift-ingress/openshift-ai-inference applied; external hostname inference.apps.example-cluster.example.com." });
      send({ step: "gateway-ready", status: "running", message: "Programmed; waiting for the load balancer's address..." });
      await nextStep();
      world.resources.inferenceGateway = F.gatewayStates.ready([]);
      send({ step: "gateway-ready", status: "success", message: "Ready: https://inference.apps.example-cluster.example.com is published in DNS." });
      done.push("The inference gateway is set up.");
    }
    if (project) {
      send({ step: "model", status: "running", message: `Creating ${display} in '${project}'...` });
      const others = (i) => i.namespace !== project || i.name !== model;
      m.items = [...(m.items ?? []).filter(others), F.modelStates.startingAs(model, project)];
      send({ step: "model", status: "success", message: `LLMInferenceService ${project}/${model} applied, on preset v3-6-0-kserve-config-llm-template-cpu with the newest released Red Hat vLLM CPU image 3.5.3.` });
      send({ step: "model-ready", status: "running", message: "Pulling the runtime image (several GB; a few minutes the first time on a node)..." });
      await nextStep();
      send({ step: "model-ready", status: "running", message: "Starting vLLM and loading the model..." });
      await nextStep();
      m.items = [...m.items.filter(others), model !== "qwen25-05b-instruct" ? F.modelStates.readyAs(model, project) : releasedImage || m.imagePlan === "released" ? F.modelStates.released(project) : F.modelStates.ready(project)];
      const users = m.items.filter((i) => i.managedByTool !== false).map((i) => `${i.namespace}/${i.name}`);
      if (world.resources.inferenceGateway?.managedByTool) world.resources.inferenceGateway = F.gatewayStates.ready(users);
      if (exposure === "maas") {
        // Published as part of its setup (pkg/cluster publishToMaaS).
        send({ step: "model-ready", status: "success", message: "The model's server runs; publishing it to MaaS gives it its route." });
        send({ step: "publish", status: "running", message: "Publishing the model to MaaS (its route is on the MaaS gateway alone)..." });
        await nextStep();
        send({ step: "publish", status: "success", message: `${model} in '${project}' is published to MaaS under subscription rhoai-nightly-updater-subscription.` });
      } else {
        send({ step: "model-ready", status: "success", message: `Ready at https://inference.apps.example-cluster.example.com/${project}/${model}.` });
      }
      send({ step: "callable", status: "running", message: "Calling the model once, as a user would..." });
      await nextStep();
      const msg = `Answers requests: a request for model "${model}" got 200.`;
      m.items = m.items.map((i) => (i.namespace === project && i.name === model ? F.routed(i, { exposure }) : i));
      if (exposure === "maas") {
        F.setMaaSPublished(world.resources, project, model, true, { maasOnlySetup: true });
        const others = (world.maas.items ?? []).filter((i) => i.namespace !== project || i.name !== model);
        world.maas = { ...world.maas, items: [...others, { ...F.maasModel(true, { name: model, maasOnlySetup: true }), namespace: project }] };
      }
      send({ step: "callable", status: "success", message: msg });
      done.push(`${display} is ready in ${project}. ${msg}`);
    }
  }
  const message = done.join(" ");
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** GET /api/resources/playground (pkg/cluster GetPlaygroundStatus): the models are the project's. */
function playgroundOf(project) {
  const { modelsByProject, ...pg } = world.playground;
  if (pg.apiProblem || !project) return { ...pg, project: project || undefined };
  return { ...pg, project, models: modelsByProject[project] ?? [] };
}

function removePlayground(project) {
  world.playground.items = world.playground.items.filter((i) => i.namespace !== project);
}

/** POST /api/resources/playground/stream (pkg/cluster RunPlayground). */
async function playgroundStream(req, res) {
  const { action, project, models = [] } = await body(req);
  beginOperation("playground", "Playground", `${action} playground in ${project}`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  let message;
  if (action === "teardown") {
    send({ step: "delete", status: "running", message: `Asking the dashboard to delete the playground lsd-genai-playground in ${project}...` });
    await nextStep();
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), F.playgroundStates.terminating(project)];
    send({ step: "delete", status: "success", message: `The dashboard is deleting the playground in ${project}.` });
    send({ step: "gone", status: "running", message: "Waiting until the playground is gone..." });
    await nextStep();
    removePlayground(project);
    send({ step: "gone", status: "success", message: `The playground in ${project} is gone.` });
    message = `Playground deleted from ${project}: the dashboard removed its server, configuration and database. The project and its models stay.`;
  } else {
    const names = models.length ? models.map((m) => m.name) : (world.playground.modelsByProject[project] ?? []).filter((m) => m.source === "namespace" && m.ready && (m.chat || m.embeddingDimension)).map((m) => m.name);
    send({ step: "check", status: "running", message: "Checking what the playground needs..." });
    await nextStep();
    send({ step: "check", status: "success", message: `Ready to set up: ${project} with ${names.join(", ")}.` });
    send({ step: "install", status: "running", message: `Asking the dashboard to set up the playground in ${project}...` });
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), F.playgroundStates.starting(project)];
    send({ step: "install", status: "success", message: `The dashboard is setting up the playground in ${project}.` });
    send({ step: "ready", status: "running", message: "The playground is starting (phase Initializing)..." });
    await nextStep();
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), F.playgroundStates.ready(project)];
    send({ step: "ready", status: "success", message: `The playground in ${project} is ready.` });
    message = `The playground in ${project} is ready. Open it in the dashboard's Gen AI studio.`;
  }
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** POST /api/resources/maas/stream (pkg/cluster RunMaaS), in the backend's steps. */
async function maasStream(req, res) {
  const { action, project, model = "qwen25-05b-instruct", limit, access } = await body(req);
  beginOperation("maas", "Models as a Service", project ? `${action} model in ${project}` : `${action} MaaS platform`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  if (action === "teardown" && world.maasRefusal) {
    // A refusal at "check": nothing changed (pkg/cluster teardown).
    send({ step: "check", status: "running", message: "Checking what this tool set up..." });
    await nextStep();
    send({ step: "check", status: "failed", message: world.maasRefusal, errorCode: "in_use" });
    endOperation(false, world.maasRefusal);
    send({ step: "operation_complete", status: "failed", message: world.maasRefusal, errorCode: "in_use" });
    res.end();
    return;
  }
  // A model set up through MaaS only goes back to no route (pkg/cluster removeModelRoute).
  const noRoute = action === "unpublish" && (world.maas.items ?? []).some((i) => i.namespace === project && i.name === model && i.maasOnlySetup);
  const steps = {
    setup: [["check", "Everything the platform needs is there."], ["database", `The database runs, and Secret ${F.MAAS_INFRA}/maas-db-config holds its connection URL (sslmode=require).`],
      ["gateway", "Programmed; https://maas.apps.example.com is published in DNS."], ["authorino-tls", "Authorino serves TLS and trusts the service CA."],
      ["dsc", "modelsAsAService is Managed."], ["ready", "maas-api is available, the tenant is ready, and the gateway checks every request."]],
    teardown: [["check", "Removing, in reverse order of setup."], ["unpublish", "1 model unpublished."], ["dsc", "modelsAsAService is Removed again, as before setup."],
      ["gateway", "The gateway this tool created is deleted. GatewayClass openshift-default stays."], ["database", "The database and its connection Secret are deleted."]],
    publish: [["check", "The platform is ready and the model can be published."], ["gateway", `The MaaS gateway admits routes from '${project}'.`],
      ["model", "The model routes through maas-default-gateway alone. While it is published, the model is used through MaaS (with an API key) and the dashboard's playground; its direct address answers 404 until it is unpublished."],
      ["maas", "MaaSModelRef, MaaSSubscription and MaaSAuthPolicy are applied."], ["ready", "MaaS lists the model."]],
    unpublish: [["check", "Unpublishing, in reverse order of publishing."], ["maas", "The model reference is deleted."], ["model", noRoute ? "The model has no outside route now, as it was set up (through MaaS only): it answers only at its in-cluster address." : "The model routes as before publishing: KServe's default gateway (no refs)."],
      ["gateway", `The MaaS gateway no longer admits routes from '${project}'.`]],
    "change-limit": [["check", "The model's limit can change."], ["limit", "The subscription lists the model with its new limit."], ["ready", "MaaS applies it."]],
    "change-access": [["check", "Who can use the published models can change."], ["access", "The subscription and the access policy name the new audience."], ["ready", "MaaS applies it."]],
    "change-database": [["check", "MaaS can switch databases."], ["cnpg", "Not needed."], ["database", `Secret ${F.MAAS_INFRA}/maas-db-config names the new database.`], ["cleanup", "Deleted the database before."]],
  }[action] ?? [];
  for (const [step, done] of steps) {
    send({ step, status: "running", message: "..." });
    await nextStep();
    send({ step, status: "success", message: done });
  }
  const item = (published) => (world.maas.items ?? []).map((i) => (i.namespace === project && i.name === model ? F.maasModel(published, { name: model }) : i));
  if (action === "setup") world.maas = F.maasWorld("ready", { items: world.maas.items });
  if (action === "teardown") {
    for (const i of world.maas.items ?? []) if (i.published) F.setMaaSPublished(world.resources, i.namespace, i.name, false);
    world.maas = F.maasWorld("none", { items: (world.maas.items ?? []).map((i) => (i.published ? F.maasModel(false, { name: i.name }) : i)) });
  }
  const withLimit = (items) => items.map((i) => (i.namespace === project && i.name === model && i.published ? { ...i, limit: { ...limit, ...(limit?.preset && limit.preset !== "custom" ? F.maasLimit(limit.preset) : {}) } } : i));
  const withProblems = (a) => (a && !a.everyone ? { ...a, groupProblems: F.maasGroupProblems(a.groups ?? []) } : a);
  if (action === "publish") world.maas = { ...world.maas, items: withLimit(item(true)), access: withProblems(access) ?? world.maas.access ?? { everyone: true } };
  if (action === "change-limit") world.maas = { ...world.maas, items: withLimit(world.maas.items ?? []) };
  if (action === "change-access") world.maas = { ...world.maas, access: { ...access, ...(access?.everyone ? {} : { groupProblems: F.maasGroupProblems(access?.groups ?? []) }) } };
  if (action === "unpublish") world.maas = { ...world.maas, items: item(false) };
  // The Models tab's model follows: published, it routes through the MaaS gateway alone.
  if (action === "publish" || action === "unpublish") F.setMaaSPublished(world.resources, project, model, action === "publish");
  const message = { setup: "The MaaS platform is ready.", teardown: "The MaaS platform this tool set up is removed.", publish: `${model} in '${project}' is published to MaaS.`, unpublish: `${model} in '${project}' is unpublished from MaaS.` }[action];
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** The MLflow samples of a project (pkg/cluster GetMLflowSamplesStatus). */
function mlflowSamplesOf(project) {
  const m = world.resources.mlflow;
  if (!m?.deployed) return { project, available: false, reason: "MLflow is not deployed. Set it up first.", samples: [] };
  if (!m.ready) return { project, available: false, reason: "MLflow is not ready.", samples: [] };
  return world.samples.mlflow[project] ?? F.mlflowSamples(project);
}

/** Streams events, each after /__docs/advance in gated mode, then the result. */
async function sampleStream(req, res, type, label, events, apply, done) {
  beginOperation(type, label);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 2000 })}\n\n`);
  for (const ev of events) {
    if (ev.status !== "running") await nextStep();
    if (res.destroyed) return;
    send(ev);
  }
  apply();
  endOperation(true, done);
  send({ step: "operation_complete", status: "success", message: done });
  res.end();
}

/** POST /api/resources/samples/mlflow/stream: add, remove or regenerate samples in one project. */
async function mlflowSamplesStream(req, res) {
  const { project = "dev-project", action = "add", samples = [] } = await body(req);
  const current = mlflowSamplesOf(project);
  const order = action === "remove" ? [...F.SAMPLE_IDS].reverse() : F.SAMPLE_IDS;
  const ids = order.filter((id) => samples.includes(id));
  const verb = { add: "added", remove: "removed", regenerate: "regenerated" }[action];
  const events = [
    { step: "check", status: "running", message: `Checking MLflow for project '${project}'...` },
    { step: "check", status: "success", message: `MLflow is ready at ${current.mlflowURL}` },
    ...ids.flatMap((id) => [
      { step: id, status: "running", message: `${F.SAMPLE_NAMES[id]}: ${action}...` },
      { step: id, status: "success", message: `${F.SAMPLE_NAMES[id]}: ${verb}.` },
    ]),
  ];
  const apply = () => {
    const states = Object.fromEntries(current.samples.map((s) => [s.id, s.state]));
    for (const id of ids) states[id] = action === "remove" ? (id === "training" ? "removed" : "absent") : "present";
    world.samples.mlflow[project] = F.mlflowSamples(project, states);
  };
  return sampleStream(req, res, "mlflow-samples", "MLflow samples", events, apply, `Samples ${verb} in project '${project}'.`);
}

const routes = {
  "GET /api/status": () => world.status,
  "GET /api/operation": () => world.operation,
  "GET /api/version": () => ({ ...world.version, profile: world.profile || "full", ...(world.profile === "gitops" ? { admissionPolicy: world.admissionPolicy || "enforced" } : {}) }),
  "GET /api/update-check": () => world.updateCheck,
  "GET /api/user/permissions": () => ({ canMutate: world.canMutate !== false, user: F.USER }),
  "GET /api/latest-nightly": () => F.latestNightly(),
  "GET /api/nightly-tags": () => F.nightlyTags(true),
  "GET /api/build-explorer/tags": (u) => F.nightlyTags(u.searchParams.get("includeDates") !== "false"),
  "GET /api/build-explorer/content": (u) => F.fbcContent(u.searchParams.get("image") || F.INSTALLED_IMAGE),
  "GET /api/build-explorer/contains": (u) => F.buildContains({ pr: u.searchParams.get("pr"), commit: u.searchParams.get("commit"), repo: u.searchParams.get("repo") }, u.searchParams.getAll("image")),
  "GET /api/components": () => world.components,
  "GET /api/diagnostics": () => world.diagnostics,
  "GET /api/dashboard/state": () => world.dashboard,
  "GET /api/dashboard/builds": () => world.dashboardBuilds,
  "GET /api/resources/status": () => ({ dashboardUrl: F.DASHBOARD_URL, inferenceGateway: F.gatewayStates.none, models: F.modelsStatus(), ...world.resources }),
  "GET /api/resources/projects": () => ({ projects: world.projects ?? F.projects }),
  "GET /api/resources/playground": (u) => playgroundOf(u.searchParams.get("project") || ""),
  "GET /api/resources/maas": () => world.maas,
  "GET /api/resources/maas/groups": (u) => F.maasGroupProblems(u.searchParams.getAll("name")),
  // Models routing and one-click prerequisites.
  "GET /api/resources/models/exposures": (u) => F.modelExposures(u.searchParams.get("project") || "", u.searchParams.get("model") || "qwen25-05b-instruct", world.resources.inferenceGateway ?? F.gatewayStates.none, world.routing ?? {}),
  "GET /api/resources/prerequisites": (u) => F.prerequisites({ ...(world.prereq ?? {}), exposure: u.searchParams.get("exposure") ?? "", ...(world.prereq?.models === "missing" && ["default-gateway", "gateway"].includes(u.searchParams.get("exposure") ?? "") ? { models: "missing-routing" } : {}) }),
  "GET /api/resources/samples/mlflow": (u) => mlflowSamplesOf(u.searchParams.get("project") || ""),
  "GET /api/resources/samples/pipeline/spec": () => F.pipelineSampleSpec,
  // Made up, like every value here.
  "GET /api/resources/minio/admin-password": () => ({ user: "admin", accessKey: "docs-mock-access-key", secretKey: "docs-mock-secret-key", password: "docs-mock-admin-password" }),
  "GET /api/activity": () => world.status.activity ?? [],
  "GET /api/test-pull-secret": () => ({ success: true, message: "The pull secret can pull from quay.io/rhoai.", logs: [] }),
  "GET /api/verify-nodes": () => ({ success: true, message: "All 3 nodes have the pull secret and the image mirror.", logs: [] }),
  "POST /api/setup/node-pull-check": () => F.nodePullCheck(F.latestNightly().image, !world.nodePullFails),
  "GET /api/setup/dsc/preview": () => ({
    yaml: `apiVersion: ${world.previewAPIVersion || "datasciencecluster.opendatahub.io/v2"}\n` + "kind: DataScienceCluster\nmetadata:\n  name: default-dsc\nspec:\n  components:\n    aipipelines:\n      managementState: Managed\n    dashboard:\n      managementState: Managed\n    kserve:\n      managementState: Managed\n    workbenches:\n      managementState: Managed\n",
    apiVersion: world.previewAPIVersion || "datasciencecluster.opendatahub.io/v2",
    operatorVersion: "3.6.0", branch: "", sourceURL: "", source: "csv", sourceDescription: "alm-examples of rhods-operator.3.6.0",
  }),
  "POST /api/update": () => ({ success: true, message: "Dry run passed: the update would install rhods-operator from this build. Nothing was changed.", logs: ["OK: pull secret", "OK: image mirror", "OK: catalog image verified"] }),
};

// What the gitops profile serves (pkg/api gitOpsRoutes); every other route
// answers 403 profile_gitops, and its reads need cluster-admin.
const GITOPS_PROFILE_ROUTES = new Set([
  "GET /api/status", "GET /api/operation", "GET /api/activity", "GET /api/update-check", "GET /api/components",
  "GET /api/diagnostics", "GET /api/user/permissions", "POST /api/update", "POST /api/update/stream", "POST /api/pageview", "GET /api/version",
]);

async function api(req, res, u) {
  const key = `${req.method} ${u.pathname}`;
  if (req.method === "GET" && heldReads.prefixes.some((p) => u.pathname.startsWith(p))) {
    await new Promise((r) => heldReads.waiting.push(r));
  }
  if (req.method === "GET" && READ_DELAY_MS > 0 && u.pathname.startsWith("/api/resources/")) {
    await new Promise((r) => setTimeout(r, READ_DELAY_MS));
  }
  if (world.profile === "gitops") {
    if (!GITOPS_PROFILE_ROUTES.has(key)) {
      return json(res, 403, { error: "This feature is off: this updater runs the gitops profile, where Argo CD or an ACM policy manages RHOAI from Git and the updater only installs the build the managed catalog serves (docs/GITOPS.md).", errorCode: "profile_gitops" });
    }
    if (world.canMutate === false && req.method === "GET" && !["/api/user/permissions", "/api/version"].includes(u.pathname)) {
      return json(res, 403, { error: "This updater runs the gitops profile, where only cluster-admins can see what it reads from the cluster.", errorCode: "forbidden" });
    }
  }
  if (key === "POST /api/pageview") { res.writeHead(204); res.end(); return; }
  if (key === "GET /api/resources/minio/admin-password" && world.canMutate === false) {
    return json(res, 403, { error: "Only cluster-admins can read this through the updater.", errorCode: "forbidden" });
  }
  if (key === "POST /api/update/stream") return updateStream(req, res);
  if (key === "POST /api/resources/minio/setup") return mutation(res, "setup-minio", "Set up S3 storage", minioSetup);
  if (key === "POST /api/resources/minio/migrate/stream") return migrateStream(req, res);
  if (key === "POST /api/resources/cleanup/stream") return cleanupStream(req, res);
  if (key === "POST /api/resources/models/stream") return modelsStream(req, res);
  if (key === "POST /api/resources/playground/stream") return playgroundStream(req, res);
  if (key === "POST /api/resources/maas/stream") return maasStream(req, res);
  if (key.startsWith("POST /api/resources/samples/") && world.canMutate === false) {
    return json(res, 403, { error: "Only cluster-admins can change the cluster through the updater.", errorCode: "forbidden" });
  }
  if (key === "POST /api/resources/samples/mlflow/stream" && world.samplesRateLimited) {
    // pkg/api withMutationAuth: the tool's own per-user limit.
    res.setHeader("Retry-After", "18");
    return json(res, 429, { error: "This tool allows one change per user every 30 seconds on this action. Nothing was changed; try again in 18 seconds.", errorCode: "user_rate_limited" });
  }
  if (key === "POST /api/resources/samples/mlflow/stream") return mlflowSamplesStream(req, res);
  if (key === "POST /api/resources/pipeline-server/setup") {
    const { project = "dev-project", recreate = false } = await body(req);
    return mutation(res, "setup-pipeline-server", "Set up pipeline server", () => {
      world.resources.pipelineServers = [...world.resources.pipelineServers.filter((ps) => ps.namespace !== project), { ...F.pipelineServerStarting, namespace: project }];
      return recreate
        ? { success: true, message: "Pipeline server 'nightly-dspa' replaced by 'dspa', which workbench pipelines use. It will take 1-3 minutes to become ready.", logs: [] }
        : { success: true, message: "Pipeline server created. It will take 1-3 minutes to become ready.", logs: [] };
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
    case "/__docs/hold": {
      // ?path=/api/resources/maas (repeatable) holds those reads; no path lets every held read through.
      const paths = u.searchParams.getAll("path");
      if (paths.length) heldReads.prefixes = paths;
      else releaseReads();
      return json(res, 200, { holding: heldReads.prefixes, waiting: heldReads.waiting.length });
    }
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
    // The app's own CSP (pkg/middleware/security.go), so the docs show what
    // the browser allows there: an inline script would not run here either.
    res.writeHead(200, {
      "Content-Type": TYPES[extname(full)] || "application/octet-stream",
      "Content-Security-Policy": "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:",
    });
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
