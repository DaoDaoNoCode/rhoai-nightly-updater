// The docs mock's scenarios: the world each one starts from. server.mjs
// serves them; frontend/src/test/docsMockFixtures.test.ts checks their
// answers against the frontend's API types.
import * as F from "./fixtures.mjs";

// --- Scenarios ------------------------------------------------------------------

const REMOTE_POD = "rhoai-nightly-updater-7c9d5f8b64-k2x7q";

export function baseWorld() {
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
    // The Workbench tab (GET /api/resources/workbench): items, and the plan of a project.
    workbench: { items: [] },
    // The Distributed workloads tab (GET /api/resources/kueue).
    kueue: F.kueueWorld("none"),
    // Models as a Service (GET /api/resources/maas).
    maas: F.maasWorld("none"),
    // This tool's model registry (GET /api/resources/model-registry).
    modelRegistry: F.modelRegistryStates.absent,
    // Sample states by project; a project not named has none of the samples.
    samples: { mlflow: {} },
    // The last environment recipe run (GET /api/recipes/run).
    recipeRun: { run: null, running: false },
    // gated: update steps and mutations wait for /__docs/advance and /__docs/release.
    gated: false,
  };
}

// The Subscription, catalog and DSC carry Argo CD tracking from "rhoai" that
// Argo CD does not back (left over): no manager, only the note, and every
// action is on as on a cluster without Argo CD.
function staleArgo(w, verdict) {
  w.status = F.updateAvailableStatus();
  const t = { application: "rhoai", state: "stale", ...verdict };
  w.status.subscription = { ...w.status.subscription, argoCD: t };
  w.status.catalogSource = { ...w.status.catalogSource, argoCD: t };
  w.status.dscArgoCD = t;
}

export const scenarios = {
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
  "self-managed-installed": (w) => { F.selfManaged(w.status, { pulled: true, addedByTool: F.NODE_PULL_RECORD }); w.nodes = { pullEntry: "same" }; },
  // A cluster someone else set up (a sandbox provisioner): a quay.io/rhoai
  // entry of its own in openshift-config/pull-secret and an
  // ImageDigestMirrorSet quay-registry. The nodes pull (the catalog runs),
  // or are not checked yet; the tool neither replaces nor removes the
  // entry.
  "provisioned-by-someone-else": (w) => {
    scenarios["self-managed-saved"](w);
    F.selfManaged(w.status, { pulled: true });
    w.status.imageMirror = { exists: true, name: "quay-registry", source: "registry.redhat.io/rhoai" };
    w.nodes = { pullEntry: "other" };
  },
  "provisioned-by-someone-else-unchecked": (w) => { scenarios["provisioned-by-someone-else"](w); F.selfManaged(w.status); },
  // The one-click setup (POST /api/setup/nodes/plan and /stream): the
  // mirror missing on a self-managed cluster, a paused pool, ROSA Classic
  // or OSD (the mirror allowed, the pull secret not), an unknown platform
  // whose admission webhook refuses the attempt, and ROSA HCP.
  "self-managed-mirror": (w) => { scenarios["self-managed-saved"](w); w.status.imageMirror = { exists: false, name: "", source: "" }; },
  // After Add to the nodes' pull secret: the tool's record is in the status.
  "self-managed-mirror-added": (w) => { scenarios["self-managed-mirror"](w); w.status.pullSecret.nodes.addedByTool = F.NODE_PULL_RECORD; w.nodes = { pullEntry: "same" }; },
  "self-managed-paused-pool": (w) => { scenarios["self-managed-mirror"](w); w.stuckPools = ["worker (paused)"]; },
  // A corporate mirror serves all of registry.redhat.io: Create the image
  // mirror refuses (a mirror for exactly registry.redhat.io/rhoai would take
  // those pulls away from it) and says what to add instead.
  "self-managed-mirror-parent-scope": (w) => { scenarios["self-managed-mirror"](w); w.parentScopeMirror = true; },
  "managed-classic": (w) => { scenarios["ready-to-install"](w); w.status.imageMirror = { exists: false, name: "", source: "" }; F.selfManaged(w.status, { managed: true }); },
  "unknown-denied": (w) => { scenarios["self-managed-unknown"](w); w.status.imageMirror = { exists: false, name: "", source: "" }; w.nodeSetupDenied = true; },
  "hosted-mirror-missing": (w) => { scenarios["ready-to-install"](w); w.status.imageMirror = { exists: false, name: "", source: "" }; },
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
  // An Environment operation another user runs: the banner's View progress opens what the server records.
  "env-operation-elsewhere": (w) => {
    scenarios["env-models-ready"](w);
    w.operation = {
      inProgress: true,
      operation: {
        id: "op-2c81e0", type: "cleanup", label: "Clean up", user: "qa-user", startedAt: F.ago(2), updatedAt: F.ago(0.1),
        step: "storage", stepStatus: "running", message: "Deleting the S3 storage (SeaweedFS)",
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
      // The v2.0.1 release notes' own line (GitHub mirror, read 2026-11-18).
      installerSHA256: "c642197caae4feaed7994375fa68532cab386cc0b63629923914343017ce2346",
    };
  },
  "template-outdated": (w) => { w.version = F.versionInfo({ templateRevision: "3", templateOutdated: true }); },
  // The user's OpenShift token expired or was revoked: the identity check
  // (GET /api/user/permissions) and every change answer 401 session_expired
  // (pkg/api writeAuthError, sessionProblem).
  "session-expired": (w) => { w.signIn = { status: 401, code: "session_expired", message: F.SESSION_EXPIRED_MESSAGE }; },
  // A cluster-admin still signed in from before 4.0.0 (a user:info
  // user:check-access token): GET /api/user/permissions says canMutate false
  // with the reason, and every change answers 401 sign_in_again
  // (pkg/api HandleUserPermissions, sessionProblem).
  "sign-in-before-4": (w) => {
    w.status = F.updateAvailableStatus();
    w.signIn = { status: 401, code: "sign_in_again", message: F.SIGN_IN_SCOPE_MESSAGE };
  },
  "components-not-ready": (w) => { w.components = F.notReadyComponents(); },
  // An upgraded DSC: things a later release added are not set,
  // modelsAsAService is Managed under a Removed aigateway, and fields the
  // installed CRD does not define (a v2 component, an old kserve setting).
  "components-differences": (w) => {
    w.dscManager = F.dscManager({ "workbenches.workbenchesV2": null, databaseservice: null, "dashboard.standard": null, aigateway: "Removed" },
      { unknown: { codeflare: { managementState: "Removed" }, "kserve.serving": { managementState: "Managed" } } });
  },
  // The installed CSV ships no DataScienceCluster example: nothing to compare with.
  "components-no-default": (w) => { w.dscManager = F.dscManager({}, { noDefault: true }); },
  // MaaS set up and a model published (aigateway and modelsAsAService are
  // Managed, off in the release default), ray Removed by hand, and a
  // sub-component a later release added not set: the reset dialogs offer
  // Keep the components you turned on, and the MaaS guard refuses turning
  // aigateway off.
  "components-turned-on": (w) => {
    scenarios["env-maas-published"](w);
    w.dscManager = F.dscManager({ ray: "Removed", "workbenches.workbenchesV2": null });
  },
  // The same, with the hard reset blocked as on the live cluster (RHOAI
  // 3.6.0): two modules whose operator the tool cannot check, and MLflow in
  // use.
  "components-hard-reset-blocked": (w) => {
    scenarios["env-maas-published"](w);
    w.dscManager = F.dscManager({ ray: "Removed", "workbenches.workbenchesV2": null }, { hardResetBlocked: true });
  },
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
  // Argo CD's tracking annotation confirmed with Argo CD (pkg/cluster/internal/argo/argocd.go),
  // and the four ways it is not: three left over, one not confirmed.
  "gitops-confirmed": (w) => {
    scenarios["gitops-synced"](w);
    const t = { application: "rhoai", state: "managed", namespace: "openshift-gitops", syncStatus: "Synced" };
    w.status.subscription.argoCD = t;
    w.status.catalogSource.argoCD = t;
    w.status.dscArgoCD = t;
  },
  "gitops-stale-app-missing": (w) => staleArgo(w, { staleReason: "application-missing" }),
  "gitops-stale-argo-not-installed": (w) => staleArgo(w, { staleReason: "argo-cd-not-installed" }),
  "gitops-stale-not-in-resources": (w) => staleArgo(w, { staleReason: "not-in-resources", namespace: "openshift-gitops" }),
  "gitops-read-error": (w) => {
    scenarios["gitops-synced"](w);
    const t = { application: "rhoai", state: "unconfirmed", reason: 'reading Argo CD application "rhoai" failed: k8s API error 403 (Forbidden): applications.argoproj.io is forbidden' };
    w.status.subscription.argoCD = t;
    w.status.catalogSource.argoCD = t;
    w.status.dscArgoCD = t;
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
  // Storage an earlier version set up: its admin key is in every pipeline project until Secure S3 storage runs.
  // Secured storage: each recorded pipeline server on its own key, one server this tool did not set up lost access.
  "s3-secured": (w) => {
    w.resources.minio = { ...F.s3States.running, usedBy: ["dev-project/dspa", "team-b/dspa", "old-team/dspa"] };
    w.resources.s3Keys = { secured: true, needsSecuring: false, ownKey: ["dev-project/dspa", "team-b/dspa"], keepOldKey: ["old-team/dspa (not recorded by this tool)"] };
    w.gated = true;
  },
  // Secured storage with the key of a pipeline server deleted outside this tool.
  "s3-leftover": (w) => {
    w.resources.minio = { ...F.s3States.running, usedBy: ["dev-project/dspa"] };
    w.resources.s3Keys = { secured: true, needsSecuring: false, ownKey: ["dev-project/dspa"], leftoverUsers: ["rnu-dspa-gone-team"], leftovers: [{ user: "rnu-dspa-gone-team", project: "gone-team", connectionInUse: true }] };
    w.gated = true;
  },
  "s3-not-secured": (w) => {
    w.resources.minio = { ...F.s3States.running, usedBy: ["dev-project/dspa", "team-b/dspa", "old-team/dspa"] };
    w.resources.s3Keys = { secured: false, needsSecuring: true, oldKey: ["dev-project/dspa", "team-b/dspa"], keepOldKey: ["old-team/dspa (not recorded by this tool)"] };
    w.gated = true;
  },
  mlflow: (w) => { w.resources.mlflow = F.mlflowStates.running; },
  // The Model registry tab: not set up, starting, and ready with the sample model.
  "env-registry-none": (w) => { w.modelRegistry = F.modelRegistryStates.absent; },
  "env-registry-starting": (w) => { w.modelRegistry = F.modelRegistryStates.starting; },
  "env-registry-ready": (w) => { w.modelRegistry = F.modelRegistryStates.ready; },
  // Its DataScienceCluster component was turned on by this tool: Clean up offers it back.
  "env-registry-turned-on": (w) => { w.modelRegistry = F.modelRegistryStates.ready; w.prereq = { ...(w.prereq ?? {}), registryRestorable: true }; },
  // The Environment page's other states, one scenario each.
  "env-ready": (w) => { w.resources.mlflow = F.mlflowStates.running; },
  // Workbenches use the pipeline server's Connection: the Pipelines tab names them, Delete lists them.
  "env-connection-workbenches": (w) => {
    w.resources.mlflow = F.mlflowStates.running;
    w.resources.pipelineServers = [{ ...F.pipelineServer, connectionWorkbenches: [{ name: "my-notebook", uid: "0b5e2c1a-6f0d-4c4e-9a52-3f1d7e8a9b10", running: true }, { name: "training-wb", uid: "7c9d4e2f-1a3b-4d5e-8f60-2b4c6d8e0f12" }], connectionEnvOnlyWorkbenches: ["old-notebook"] }];
  },
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
  "env-maas-orphan": (w) => { scenarios["env-models-ready"](w); w.maas = { ...F.maasWorld("ready", { items: [F.maasModel(true)] }), removing: [F.maasGonePublication("old-project")] }; },
  // Cluster truth (batch 2): this tool's pipeline server with no record of its
  // UID, its pipeline server and model in a project that is no longer a data
  // science project, a deleted project still on the inference gateway's list,
  // and a MaaS publication of which nothing is left on the cluster.
  "env-cluster-truth": (w) => {
    scenarios["env-models-ready"](w);
    w.resources.pipelineServers = [F.pipelineServerIn("dev-project", { unrecorded: true, uid: "3f6c2a9e-dev-project-dspa" })];
    w.resources.outsidePipelineServers = [F.pipelineServerIn("old-team")];
    w.resources.minio = { ...F.s3States.running, usedBy: ["dev-project/dspa", "old-team/dspa"], teardownBlockedReason: "2 pipeline servers use this S3 storage: dev-project/dspa, old-team/dspa. Delete them first." };
    w.resources.models = F.modelsStatus({ items: [F.modelStates.released("dev-project")], outside: [F.modelStates.released("old-team")] });
    w.resources.inferenceGateway = { ...w.resources.inferenceGateway, allowedProjects: ["openshift-ingress", "dev-project", "e2e-old", "team-x"], unusedProjects: ["e2e-old", "team-x"], goneProjects: ["e2e-old"], addedProjects: ["dev-project", "e2e-old"] };
    w.maas = { ...F.maasWorld("ready", { items: [F.maasModel(false)] }), removing: [{ namespace: "dev-project", name: "qwen25-05b-instruct", reason: "the MaaSModelRef of model qwen25-05b-instruct in dev-project no longer exists" }] };
  },
  // F6 and F2 (live, 2026-11-23): a model whose MaaSModelRef was deleted
  // outside this tool while it still routes through the MaaS gateway, and a
  // published model in a project being deleted.
  "env-maas-routing-pending": (w) => {
    scenarios["env-models-ready"](w);
    w.maas = F.maasWorld("ready", { items: [
      { ...F.maasModel(false), routingPending: true, message: "No MaaS reference; the model still routes through the MaaS gateway, so its direct address does not answer." },
      { ...F.maasModel(false, { name: "smollm2-135m-instruct" }), namespace: "old-team", removing: true, message: "Being removed with its project", setupBlockedReason: "Being removed with its project." },
    ] });
  },
  // A storage deletion that did not finish: the Deployment is gone, its Secret is left.
  "env-s3-leftovers": (w) => {
    w.resources.minio = { deployed: false, ready: false, namespace: "seaweedfs", managedByTool: true, message: "Namespace exists but the S3 storage is not deployed", leftovers: ["Secret seaweedfs-s3-credentials"], namespaceDeletable: true };
    w.resources.pipelineServers = [];
  },
  // Others publish through the platform: Remove refuses before changing anything.
  "env-maas-in-use": (w) => { scenarios["env-models-ready"](w); w.maas = F.maasWorld("ready", { items: [F.maasModel(false)] }); w.maasRefusal = F.MAAS_IN_USE; },
  // The Workbench tab: not set up (the plan shown), running, stopped, and no hardware profile fits.
  // The Distributed workloads tab: not set up, operator missing, ready with samples, opt-in on.
  "env-kueue-none": () => {},
  "env-kueue-operator-missing": (w) => { w.kueue = F.kueueWorld("operator-missing"); w.prereq = { ...(w.prereq ?? {}), kueue: "missing" }; },
  "env-kueue-ready": (w) => { w.kueue = F.kueueWorld("ready", { samples: true }); },
  "env-kueue-optin": (w) => { w.kueue = F.kueueWorld("ready", { samples: true, optIn: true }); },
  "env-workbench-none": () => {},
  "env-workbench-running": (w) => { w.workbench.items = [F.workbenchItem("dev-project", "running")]; },
  "env-workbench-stopped": (w) => { w.workbench.items = [{ ...F.workbenchItem("dev-project", "stopped"), ready: false }]; },
  "env-workbench-no-profile": (w) => { w.workbench.noProfile = true; },
  "env-workbench-storage-used": (w) => { const it = F.workbenchItem("dev-project", "running"); w.workbench.items = [{ ...it, storage: { ...it.storage, usedBy: ["workbench my-wb"] } }]; },
  "env-workbench-kept-storage-used": (w) => { scenarios["env-workbench-kept-storage"](w); w.workbench.items[0].storage = { ...w.workbench.items[0].storage, usedBy: ["workbench my-wb"] }; },
  "env-workbench-kept-storage": (w) => { const { state: _s, url: _u, image: _i, hardwareProfile: _h, cpu: _c, memory: _m, connectionAttached: _a, ...kept } = F.workbenchItem("dev-project", "stopped"); w.workbench.items = [{ ...kept, deployed: false, ready: false }]; },
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
for (const name of ["healthy", "prerequisite-missing", "operand-missing", "module-backoff", "certificate-stale", "apply-failures", "upgrade-gates", "conversion-webhook", "leftovers"]) {
  scenarios[`diag-${name}`] = (w) => {
    w.diagnostics = F.diagnostics(name);
    // The operator cannot convert the DSC to v3: the Components page reads it as v2.
    if (name === "conversion-webhook") w.components = F.conversionFallbackComponents();
    else if (name !== "healthy") w.components = F.notReadyComponents();
    if (name === "module-backoff") w.gated = true;
  };
}
// Per-user views: a signed-in user who is not a cluster-admin (role
// "user"). The server answers them as the backend does: the summary, the
// short operation, their own projects, and 403 admin_only for every read
// only cluster-admins may make (server.mjs, userRead).
scenarios.user = (w) => {
  scenarios["env-overview-three"](w);
  w.status = F.updateAvailableStatus();
  w.role = "user";
  w.canMutate = false;
  // The masthead and the summary name the release the docs describe
  // (F.DOCS_VERSION, baseWorld's version and update check).
  w.userProjects = ["dev-project", "llm-eval"];
  w.samples.mlflow["dev-project"] = F.mlflowSamples("dev-project", { "genai-demo": "present", training: "present" });
};
scenarios["user-no-projects"] = (w) => { scenarios.user(w); w.userProjects = []; };
scenarios["user-update-running"] = (w) => {
  scenarios.user(w);
  w.operation = { inProgress: true, operation: { id: "op-update", type: "update", label: "Update to nightly", user: "qa-admin", startedAt: new Date(F.NOW - 4 * 60_000).toISOString(), updatedAt: new Date(F.NOW).toISOString() } };
};

// The leftover fixes refused: the module operator that recreates the
// Deployment is not running (the backend's prerequisites refusal).
scenarios["diag-leftovers-refused"] = (w) => { scenarios["diag-leftovers"](w); w.leftoverRefusal = true; };

// Environment recipes (the Recipes page): no run yet on a cluster with
// RHOAI and no DataScienceCluster (new, and its review), a run at step 5
// (POST /__docs/advance moves it on), one that failed at the pipeline
// server, and one whose updater pod went away.
const RECIPE_OPERATION = (w, step, title, name = "dev", of = 8) => {
  w.operation = { inProgress: true, operation: { id: "op-recipe", type: "recipe", label: "Environment recipe", user: F.USER, target: `${name}: step ${step} of ${of} (${title})`, startedAt: F.ago(12), updatedAt: F.ago(0.1) } };
};
scenarios["recipe-new"] = (w) => { scenarios["no-dsc"](w); w.resources = { minio: F.s3States.none, mlflow: F.mlflowStates.none, pipelineServers: [] }; };
scenarios["recipe-review"] = (w) => { scenarios["recipe-new"](w); };
scenarios["recipe-review-blocked"] = (w) => { scenarios["recipe-new"](w); w.recipePlanBlocked = true; };
scenarios["recipe-running"] = (w) => {
  w.recipeRun = { run: F.recipeRun({ done: 4 }), running: true, pod: "rhoai-nightly-updater-6f7b8c9d5-x2k4p", step: { step: "s3", status: "running", message: "Waiting for SeaweedFS to start (1/1 pods starting)" } };
  RECIPE_OPERATION(w, 5, "S3 storage");
};
scenarios["recipe-failed"] = (w) => {
  const why = "The pipeline server did not become ready in 10 minutes: pod ds-pipeline-dspa-6c9f7d8b5-k8w2n is in ImagePullBackOff (registry.redhat.io/rhoai/odh-ml-pipelines-api-server-v2-rhel9).";
  w.recipeRun = { run: F.recipeRun({ done: 5, current: "failed", message: why, state: "failed", errorCode: "timeout", stopReason: `Step 6 of 8 (Pipeline server) failed: ${why}` }), running: false };
};
scenarios["recipe-succeeded"] = (w) => {
  w.recipeRun = { run: F.recipeRun({ done: 8, current: "", state: "succeeded", stopReason: "Every step of dev is set up." }), running: false };
  // What the run set up is there: MLflow, and the pipeline server in its project.
  w.resources = { ...w.resources, mlflow: F.mlflowStates.running, pipelineServers: [...w.resources.pipelineServers, { ...F.pipelineServer, namespace: "recipe-dev", apiRoute: undefined, uiRoute: undefined }] };
  // An earlier run's project is still there too.
  w.recipeProjects = [
    { name: "recipe-dev", uid: "uid-recipe-dev", recipe: "dev", createdAt: F.ago(40) },
    { name: "recipe-review", uid: "uid-recipe-review", recipe: "review", createdAt: F.ago(60 * 26) },
  ];
};
// A recipe with every feature (recipes phase 2): the form (the editor's
// switches are turned on in the browser), running at the model step, and
// succeeded with its Clean up offering the MaaS platform and the model
// registry it created.
scenarios["recipe-full-new"] = (w) => { scenarios["recipe-new"](w); };
// The review of a recipe on a cluster that has part of it: skipped steps,
// steps that run, and a blocked one with its problem.
scenarios["recipe-full-review-mixed"] = (w) => { scenarios["recipe-new"](w); w.recipePlanMixed = true; };
scenarios["recipe-full-running"] = (w) => {
  const rcp = F.fullRecipe();
  const steps = F.recipeSteps(rcp);
  const at = steps.findIndex(([id]) => id === "model");
  w.recipeRun = { run: F.recipeRun({ rcp, done: at }), running: true, pod: "rhoai-nightly-updater-6f7b8c9d5-x2k4p",
    step: { step: "model-ready", status: "running", message: "Waiting for Qwen3 1.7B to serve: pulling its image on worker-2" } };
  RECIPE_OPERATION(w, at + 1, steps[at][1], "full", steps.length);
};
scenarios["recipe-full-succeeded"] = (w) => {
  const rcp = F.fullRecipe();
  const steps = F.recipeSteps(rcp);
  w.recipeRun = { run: F.recipeRun({ rcp, done: steps.length, current: "", state: "succeeded", stopReason: "Every step of full is set up." }), running: false };
  w.resources = { ...w.resources, mlflow: F.mlflowStates.running, pipelineServers: [...w.resources.pipelineServers, { ...F.pipelineServer, namespace: "recipe-full", apiRoute: undefined, uiRoute: undefined }] };
};
scenarios["recipe-interrupted"] = (w) => {
  w.recipeRun = { run: F.recipeRun({ done: 3, pod: "rhoai-nightly-updater-6f7b8c9d5-q9m3z" }), running: false, interrupted: true };
  w.operation = { inProgress: false, operation: null, interrupted: { type: "recipe", label: "Environment recipe", user: F.USER, target: "dev: step 4 of 8 (What Pipelines needs)", startedAt: F.ago(14), pod: "rhoai-nightly-updater-6f7b8c9d5-q9m3z", id: "op-recipe", bootId: "b-4e2a" } };
};

// The reads every page makes, as the server answers them; the frontend's
// types check these (frontend/src/test/docsMockFixtures.test.ts).
/** A nightly build as the summary names it (pkg/types SummaryBuild): tag and build date, no image. */
const summaryBuildOf = (b) => (b ? { tag: b.tag || "", buildDate: b.buildDate || "" } : null);

/** The summary's nightly part (pkg/types SummaryNightly): unknown comparisons carry a problem code. */
function summaryNightlyOf(n) {
  if (!n?.installed) return { installedBuild: null, latestBuild: null, newerAvailable: false, problem: "" };
  const known = !!n.latest && typeof n.updateAvailable === "boolean" && !n.error;
  return {
    installedBuild: summaryBuildOf(n.installed),
    latestBuild: summaryBuildOf(n.latest),
    newerAvailable: known && n.updateAvailable === true,
    problem: known ? "" : "lookup_failed",
  };
}

/** GET /api/summary (pkg/types ClusterSummary): the read-only summary every signed-in user may see. */
function summaryOf(w) {
  const s = w.status;
  const installed = !!s.csv?.phase && s.csv.phase !== "Not Found";
  return {
    updaterVersion: w.version.version,
    rhoai: { installed, version: installed ? s.csv.version || "" : "", channel: installed ? s.subscription?.channel || "" : "", phase: installed ? s.csv.phase : "" },
    nightly: summaryNightlyOf(s.nightly),
    dsc: {
      exists: w.components.dscExists !== false,
      ready: w.components.dscPhase === "Ready",
      components: w.components.dscExists === false ? null : (w.components.components || []).map((c) => ({ name: c.name, managementState: c.managementState, ready: c.status === "Available" })),
    },
  };
}

export const reads = {
  "GET /api/status": (w) => w.status,
  "GET /api/operation": (w) => w.operation,
  "GET /api/user/permissions": (w) => ({ canMutate: w.canMutate !== false, user: F.USER, role: w.role || "admin" }),
  "GET /api/summary": (w) => summaryOf(w),
  "GET /api/components": (w) => w.components,
  // The DSC manager: none without a DataScienceCluster, else the scenario's.
  "GET /api/components/dsc": (w) => (w.components.dscExists === false ? { exists: false, state: w.components.dscState || "no-dsc", dscCount: 0, conditions: [], nodes: [], mismatches: [] } : w.dscManager ?? F.dscManager()),
  "GET /api/diagnostics": (w) => w.diagnostics,
  "GET /api/dashboard/state": (w) => w.dashboard,
  "GET /api/resources/status": (w) => ({ dashboardUrl: F.DASHBOARD_URL, inferenceGateway: F.gatewayStates.none, models: F.modelsStatus(), ...w.resources, recipeProjects: F.recipeProjects(w) }),
  "GET /api/resources/projects": (w) => ({ projects: w.projects ?? F.projects }),
  "GET /api/resources/maas": (w) => w.maas,
  "GET /api/resources/model-registry": (w) => w.modelRegistry,
  "GET /api/activity": (w) => w.status.activity ?? [],
  // The clean-up request for what the run created, once it is not running.
  "GET /api/recipes/run": (w) => {
    const cleanup = w.recipeRun.run && !w.recipeRun.running ? F.recipeCleanup(w.recipeRun.run) : null;
    const projects = w.recipeRun.running ? undefined : F.recipeProjects(w);
    return { ...w.recipeRun, ...(cleanup ? { cleanup } : {}), ...(projects?.length ? { projects } : {}) };
  },
};
