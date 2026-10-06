// Fixtures of the docs mock backend. Every object follows the JSON of the
// real backend: pkg/types/types.go, pkg/api (OperationStatus, VersionInfo,
// UpdateInfo, UpdateStep) and pkg/cluster (DiagnosticsResponse). The
// Diagnostics answers in ./diagnostics are written by the backend itself
// (make docs-fixtures). All names are fictional: cluster example-cluster,
// user dev-user.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

/** The fixed "now" of every screenshot; the browser clock is set to it too. */
export const NOW = Date.parse("2026-10-06T14:30:00Z");

/** RFC 3339 time `minutes` before NOW. */
export const ago = (minutes) => new Date(NOW - minutes * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");

/** A stable, realistic-looking digest for a name. */
export const digest = (seed) => "sha256:" + createHash("sha256").update(seed).digest("hex");
const hex = (seed, n) => createHash("sha1").update(seed).digest("hex").slice(0, n);

export const USER = "dev-user";
const APPS = "apps.example-cluster.example.com";
export const CONSOLE_URL = `https://console-openshift-console.${APPS}`;
const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
const fbc = (tag, seed) => `${FBC}:${tag}@${digest(seed)}`;

// The builds of the rhoai-3.6 stream: the installed one and the newest.
const installedBuild = { tag: "rhoai-3.6", seed: "rhoai-3.6-2026-10-04", buildDate: ago(60 * 59 + 17) };
const latestBuild = { tag: "rhoai-3.6", seed: "rhoai-3.6-2026-10-06", buildDate: ago(60 * 11 + 22) };
export const INSTALLED_IMAGE = fbc(installedBuild.tag, installedBuild.seed);
export const LATEST_IMAGE = fbc(latestBuild.tag, latestBuild.seed);

const nightlyBuild = (b, dashboardCommit) => ({
  image: fbc(b.tag, b.seed),
  tag: b.tag,
  digest: digest(b.seed),
  buildDate: b.buildDate,
  dashboardCommit,
  dashboardGitURL: "https://github.com/red-hat-data-services/odh-dashboard",
});

// --- Activity log (newest first, as the backend returns it) -----------------

const updateEntry = (minutesAgo, b) => ({
  timestamp: ago(minutesAgo), user: USER, action: "update", detail: fbc(b.tag, b.seed), success: true,
  label: "Updated to nightly", category: "operator", build: `${b.tag} · ${digest(b.seed).slice(7, 19)}`,
});
const olderActivity = [
  { timestamp: ago(60 * 27), user: USER, action: "setup-pipeline-server", detail: "Project dev-project", success: true, label: "Pipeline server set up", category: "test-resources" },
  { timestamp: ago(60 * 27 + 5), user: USER, action: "setup-minio", detail: "S3 storage in namespace minio", success: true, label: "S3 storage set up", category: "test-resources" },
  { timestamp: ago(60 * 50), user: "qa-user", action: "deploy-dashboard-pr", detail: "PR #5123 (RHOAI build)", success: true, label: "Dashboard PR deployed", category: "dashboard-dev" },
  { timestamp: ago(60 * 51), user: "qa-user", action: "revert-dashboard", detail: "Restored the release images", success: true, label: "Dashboard reverted", category: "dashboard-dev" },
  { timestamp: ago(60 * 74), user: USER, action: "update", detail: fbc("rhoai-3.6", "rhoai-3.6-2026-10-03"), success: false, reason: "The catalog image could not be pulled: unauthorized. Nothing was changed.", label: "Updated to nightly", category: "operator", build: `rhoai-3.6 · ${digest("rhoai-3.6-2026-10-03").slice(7, 19)}` },
  { timestamp: ago(60 * 75), user: USER, action: "create-pull-secret", detail: "kube-system/additional-pull-secret", success: true, label: "Pull secret configured", category: "setup" },
];
/** The activity log (newest first) after the update to build b, `minutesAgo` ago. */
const activityAfter = (b, minutesAgo) => [updateEntry(minutesAgo, b), ...(b === installedBuild ? [] : [updateEntry(60 * 26, installedBuild)]), ...olderActivity];

// --- GET /api/status ----------------------------------------------------------

/** Up to date: the newest build was installed `updatedMinutesAgo` ago. */
export function healthyStatus(updatedMinutesAgo = 60 * 10) {
  return {
    cluster: { server: "https://api.example-cluster.example.com:443", version: "4.20.6", user: USER },
    subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "stable-3.x", state: "AtLatestKnown" },
    csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Succeeded" },
    catalogSource: { exists: true, name: "rhoai-catalog-dev", image: LATEST_IMAGE, state: "READY" },
    pullSecret: { exists: true, valid: true },
    imageMirror: { exists: true, name: "rhoai-quay-mirror", source: "registry.redhat.io/rhoai" },
    installPlan: { name: "install-7xk2p", phase: "Complete", approved: true },
    catalogPod: { name: "rhoai-catalog-dev-q8z4n", phase: "Running", ready: true, restartCount: 0 },
    stableSource: "redhat-operators",
    consoleURL: CONSOLE_URL,
    stableChannel: "stable-3.x",
    stableVersion: "3.5.1",
    dscExists: true,
    nightly: {
      installed: nightlyBuild(latestBuild, "8c41d0e2a7b95f13d6e0c4a8b2f7e1d9c3a5b604"),
      latest: nightlyBuild(latestBuild, "8c41d0e2a7b95f13d6e0c4a8b2f7e1d9c3a5b604"),
      updateAvailable: false,
      checkedAt: ago(4),
    },
    activity: activityAfter(latestBuild, updatedMinutesAgo),
  };
}

/** The installed build is two days old; a newer rhoai-3.6 build exists. */
export function updateAvailableStatus() {
  const s = healthyStatus();
  s.catalogSource.image = INSTALLED_IMAGE;
  s.nightly.installed = nightlyBuild(installedBuild, "3f9e7b1c5d2a8e406b1f9c3d7a2e5b8f0c6d4a19");
  s.nightly.updateAvailable = true;
  s.activity = activityAfter(installedBuild, 60 * 26);
  return s;
}

export function operatorFailedStatus() {
  const s = updateAvailableStatus();
  s.csv.phase = "Failed";
  s.subscription.state = "AtLatestKnown";
  s.installPlan = { name: "install-m4w9t", phase: "Complete", approved: true };
  return s;
}

/** rhods-operator still runs, but its Subscription was deleted. */
export function noSubscriptionStatus() {
  const s = updateAvailableStatus();
  s.subscription = { name: "", source: "", channel: "", state: "" };
  delete s.installPlan;
  delete s.nightly;
  return s;
}

/** A cluster before the one-time setup. */
export function freshStatus() {
  const s = healthyStatus();
  s.subscription = { name: "", source: "", channel: "", state: "" };
  s.csv = { name: "", version: "", phase: "Not Found" };
  s.catalogSource = { exists: false, name: "", image: "", state: "" };
  s.pullSecret = { exists: false, valid: false };
  s.imageMirror = { exists: false, name: "", source: "" };
  s.dscExists = false;
  delete s.installPlan;
  delete s.catalogPod;
  delete s.nightly;
  s.activity = [];
  return s;
}

/** Setup done, nothing installed yet. */
export function readyToInstallStatus() {
  const s = freshStatus();
  s.pullSecret = { exists: true, valid: true };
  s.imageMirror = { exists: true, name: "rhoai-quay-mirror", source: "registry.redhat.io/rhoai" };
  s.activity = [olderActivity[5]];
  return s;
}

/** Just installed: the operator runs, no DataScienceCluster yet. */
export function noDSCStatus() {
  const s = healthyStatus(0);
  s.dscExists = false;
  s.activity = [updateEntry(0, latestBuild), olderActivity[5]];
  return s;
}

export const latestNightly = () => ({ tag: "rhoai-3.6", image: LATEST_IMAGE, digest: digest(latestBuild.seed), buildDate: latestBuild.buildDate });

// --- Build Explorer --------------------------------------------------------------

const tagBuilds = [
  { tag: "rhoai-3.7-ea.1", seed: "rhoai-3.7-ea.1-2026-10-06", buildDate: ago(60 * 9 + 41) },
  latestBuild,
  { tag: "rhoai-3.6-ea.2", seed: "rhoai-3.6-ea.2-2026-09-18", buildDate: ago(60 * 24 * 18 + 200) },
  { tag: "rhoai-3.5", seed: "rhoai-3.5-2026-10-05", buildDate: ago(60 * 33 + 5) },
  { tag: "rhoai-3.4", seed: "rhoai-3.4-2026-10-05", buildDate: ago(60 * 35 + 48) },
  { tag: "rhoai-3.3", seed: "rhoai-3.3-2026-09-29", buildDate: ago(60 * 24 * 7 + 95) },
];

export const nightlyTags = (withDates = true) => ({
  tags: tagBuilds.map((b) => ({ tag: b.tag, image: fbc(b.tag, b.seed), ...(withDates ? { buildDate: b.buildDate } : {}) })),
});

const relatedImageDefs = [
  ["odh_dashboard_image", "core", "odh-dashboard"],
  ["odh_rhel9_operator_image", "core", "rhods-operator"],
  ["odh_model_controller_image", "core", "odh-model-controller"],
  ["odh_kserve_controller_image", "core", "kserve"],
  ["odh_notebook_controller_image", "workbench", "kubeflow"],
  ["odh_workbench_jupyter_datascience_cpu_py312_image", "workbench", "notebooks"],
  ["odh_workbench_codeserver_datascience_cpu_py312_image", "workbench", "notebooks"],
  ["odh_data_science_pipelines_operator_controller_image", "pipeline", "data-science-pipelines-operator"],
  ["odh_ml_pipelines_api_server_v2_image", "pipeline", "data-science-pipelines"],
  ["odh_trainer_image", "training", "trainer"],
  ["odh_kuberay_operator_controller_image", "training", "kuberay"],
  ["odh_model_registry_operator_image", "other", "model-registry-operator"],
  ["odh_trustyai_service_operator_image", "other", "trustyai-service-operator"],
  ["odh_mlflow_operator_image", "other", "mlflow-operator"],
  ["odh_kube_rbac_proxy_image", "infra", "kube-rbac-proxy"],
];

/** GET /api/build-explorer/content for one FBC image. */
export function fbcContent(image) {
  const b = tagBuilds.find((t) => image === fbc(t.tag, t.seed)) ?? installedBuild;
  const isInstalled = image === INSTALLED_IMAGE;
  const relatedImages = relatedImageDefs.map(([name, category, repo], i) => {
    // The newest build has new commits in the dashboard, kserve and trainer.
    const changed = !isInstalled && [0, 3, 9].includes(i);
    const seed = `${name}-${changed ? b.seed : installedBuild.seed}`;
    return {
      name,
      image: `registry.redhat.io/rhoai/${name.replace(/^odh_/, "odh-").replace(/_image$/, "").replaceAll("_", "-")}-rhel9@${digest(seed)}`,
      category,
      gitCommit: hex(seed, 40),
      gitURL: `https://github.com/red-hat-data-services/${repo}`,
      commitDate: ago(60 * (changed ? 14 : 62) + i * 7),
      buildDate: changed ? b.buildDate : installedBuild.buildDate,
      version: "3.6.0",
    };
  });
  const categories = {};
  for (const r of relatedImages) categories[r.category] = (categories[r.category] ?? 0) + 1;
  return { tag: b.tag, image, bundleName: "rhods-operator.3.6.0", relatedImages, categories };
}

/** GET /api/build-explorer/contains: builds newer than the merge contain the PR. */
export function prContains(pr, images) {
  const mergedAt = ago(60 * 40);
  return {
    repo: "opendatahub-io/odh-dashboard",
    pr,
    title: "Show the model deployment status in the project overview",
    url: `https://github.com/opendatahub-io/odh-dashboard/pull/${pr}`,
    mergeCommit: hex(`merge-${pr}`, 40),
    mergedAt,
    builds: images.map((image) => {
      const b = tagBuilds.find((t) => image === fbc(t.tag, t.seed)) ?? installedBuild;
      const commit = hex(`dashboard-${b.seed}`, 40);
      const contains = Date.parse(b.buildDate) > Date.parse(mergedAt) && !b.tag.startsWith("rhoai-3.3");
      return {
        image, tag: b.tag, installed: image === INSTALLED_IMAGE, commit, commitRepo: "red-hat-data-services/odh-dashboard",
        result: contains ? "contains" : "not_contained",
        compareURL: `https://github.com/red-hat-data-services/odh-dashboard/compare/${hex(`merge-${pr}`, 40)}...${commit}`,
      };
    }),
  };
}

// --- Components ------------------------------------------------------------------

const pod = (name, ns, extra = {}) => ({
  name, namespace: ns, phase: "Running", node: "ip-10-0-41-17.ec2.internal", ready: true, restarts: 0,
  image: "", imageID: "", age: "26h", ...extra,
});

function deployment(name, replicas, repo, extra = {}) {
  const ns = "redhat-ods-applications";
  const commit = hex(`deploy-${name}`, 40);
  const image = `registry.redhat.io/rhoai/${name}-rhel9@${digest(`img-${name}`)}`;
  return {
    name, namespace: ns, ready: replicas, desired: replicas, available: replicas, image,
    unavailableReplicas: 0, updatedReplicas: replicas, rolloutStuck: false,
    pods: Array.from({ length: replicas }, (_, i) => pod(`${name}-${hex(name, 9)}-${hex(name + i, 5)}`, ns, { image, imageID: image })),
    gitCommit: commit, gitURL: `https://github.com/red-hat-data-services/${repo}`, commitDate: ago(60 * 62), buildDate: installedBuild.buildDate, version: "3.6.0",
    ...extra,
  };
}

const componentNames = [
  ["aipipelines", "Managed"], ["dashboard", "Managed"], ["feastoperator", "Managed"], ["kserve", "Managed"],
  ["kueue", "Removed"], ["llamastackoperator", "Removed"], ["modelregistry", "Managed"], ["ray", "Managed"],
  ["trainer", "Managed"], ["trustyai", "Managed"], ["workbenches", "Managed"],
];

export function healthyComponents() {
  return {
    components: componentNames.map(([name, managementState]) => ({
      name, managementState, status: managementState === "Removed" ? "Removed" : "Available",
    })),
    deployments: [
      deployment("rhods-dashboard", 2, "odh-dashboard", { changeStatus: "updated" }),
      deployment("odh-model-controller", 1, "odh-model-controller"),
      deployment("kserve-controller-manager", 1, "kserve", { changeStatus: "updated" }),
      deployment("data-science-pipelines-operator-controller-manager", 1, "data-science-pipelines-operator"),
      deployment("notebook-controller-deployment", 1, "kubeflow"),
      deployment("odh-notebook-controller-manager", 1, "kubeflow"),
      deployment("kuberay-operator", 1, "kuberay"),
      deployment("trainer-operator-controller-manager", 1, "trainer", { changeStatus: "updated" }),
      deployment("model-registry-operator-controller-manager", 1, "model-registry-operator"),
      deployment("trustyai-service-operator-controller-manager", 1, "trustyai-service-operator"),
      deployment("feast-operator-controller-manager", 1, "feast"),
    ],
    dscName: "default-dsc",
    dscExists: true,
    dscState: "present",
    operatorVersion: "3.6.0",
    operatorPhase: "Succeeded",
    dscPhase: "Ready",
    snapshotTime: ago(60 * 26),
    changedCount: 3,
    consoleURL: CONSOLE_URL,
    dscCompatibility: {
      operatorVersion: "3.6.0", invalidFields: [], missingComponents: [], extraComponents: [], defaultsSource: "csv",
    },
  };
}

/** The trainer needs the JobSet operator; ray's module operator lags. */
export function notReadyComponents() {
  const c = healthyComponents();
  c.dscPhase = "Not Ready";
  c.dscReason = "Some modules are not ready: ray, trainer";
  for (const comp of c.components) {
    if (comp.name === "trainer") {
      Object.assign(comp, {
        status: "Error",
        message: "dependency not met: JobSet Operator is not installed. Please install the JobSet Operator via OLM (OperatorHub) before deploying Trainer.",
        cause: "Prerequisite operator not installed: JobSet Operator",
      });
    }
    if (comp.name === "ray") {
      Object.assign(comp, {
        status: "NotReady",
        message: "Module status is stale (observedGeneration < generation)",
        cause: "Its module operator has not reconciled the current spec yet",
      });
    }
  }
  return c;
}

// --- Dashboard Dev ----------------------------------------------------------------

const dashImage = (container, env, repo) => {
  const release = `registry.redhat.io/rhoai/odh-${container}-rhel9@${digest(`release-${container}`)}`;
  return { deployment: "rhods-dashboard", container, envVar: env, repository: repo, defaultImage: release, baselineImage: release };
};

export function dashboardIdle() {
  const images = [
    dashImage("rhods-dashboard", "RELATED_IMAGE_ODH_DASHBOARD_IMAGE", "opendatahub/odh-dashboard"),
    dashImage("model-registry-ui", "RELATED_IMAGE_ODH_MOD_ARCH_MODEL_REGISTRY_IMAGE", "opendatahub/odh-mod-arch-modular-architecture"),
    dashImage("gen-ai-ui", "RELATED_IMAGE_ODH_MOD_ARCH_GEN_AI_IMAGE", "opendatahub/odh-mod-arch-gen-ai"),
    dashImage("maas-ui", "RELATED_IMAGE_ODH_MOD_ARCH_MAAS_IMAGE", "opendatahub/mod-arch-maas"),
  ].map((i) => ({ ...i, currentImage: i.defaultImage, ready: true, matchesTarget: false, running: "release" }));
  return {
    operatorAvailable: true, operatorPaused: false, isDevMode: false, devImages: images, allDevImagesReady: true,
    devImagesMatchTarget: false, defaultImagesRestored: true, currentImage: images[0].currentImage, deploymentMode: "operator",
    isCustomPR: false, managed: true, podStatus: "Running", podReady: true, containersReady: 5, containersTotal: 5,
    rolloutPending: false, canAssistRollout: false, dashboardURL: `https://data-science-gateway.${APPS}`,
    override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false },
    defaultFlavor: "rhoai", availableFlavors: ["rhoai", "odh"], rolloutStuck: false,
  };
}

/** PR #5123 (RHOAI Konflux build) deployed by dev-user 40 minutes ago. */
export function dashboardSession() {
  const s = dashboardIdle();
  const pr = 5123;
  s.devImages = s.devImages.map((i) => {
    const hasBuild = i.container === "rhods-dashboard" || i.container === "gen-ai-ui";
    const target = hasBuild ? `quay.io/${i.repository}:odh-pr-${pr}` : i.defaultImage;
    return {
      ...i, currentImage: target, targetImage: target, matchesTarget: true, ready: true,
      targetSource: hasBuild ? "pr" : "baseline", targetTag: hasBuild ? `odh-pr-${pr}` : undefined,
      running: hasBuild ? "pr" : "release", runningPR: hasBuild ? pr : undefined, flavor: hasBuild ? "rhoai" : undefined,
    };
  });
  Object.assign(s, {
    operatorPaused: true, isDevMode: true, devMode: "pr", isCustomPR: true, prNumber: pr, prContainers: ["rhods-dashboard", "gen-ai-ui"],
    devImagesMatchTarget: true, defaultImagesRestored: false, currentImage: s.devImages[0].currentImage, flavor: "rhoai",
  });
  s.override = {
    active: true, operatorPaused: true, sessionRecorded: true, mode: "pr", prNumber: pr, flavor: "rhoai",
    startedAt: ago(40), startedBy: USER, updatedAt: ago(40), updatedBy: USER,
    releaseVersionAtStart: "3.6.0", releaseVersion: "3.6.0", stale: false, dashboardDeleting: false,
    components: s.devImages.map((i) => ({ deployment: i.deployment, container: i.container, image: i.targetImage, source: i.targetSource, tag: i.targetTag })),
    lastAction: { action: "deploy-pr", by: USER, at: ago(40), detail: `PR #${pr} (RHOAI build)` },
  };
  return s;
}

// --- Test resources -----------------------------------------------------------

const SEAWEEDFS = `ghcr.io/chrislusf/seaweedfs@${digest("seaweedfs-4.48")}`;
const MINIO_OLD = `quay.io/minio/minio@${digest("minio-2025")}`;
const S3_UI = `https://minio-ui-minio.${APPS}`;

export const s3States = {
  running: {
    deployed: true, ready: true, message: "Running", namespace: "minio", uiRoute: S3_UI, currentImage: SEAWEEDFS,
    managedByTool: true, dataPVCs: ["seaweedfs-pvc"], uiUser: "admin",
    teardownBlockedReason: "1 pipeline server uses this S3 storage: dev-project/nightly-dspa. Tear it down first.",
  },
  pending: {
    deployed: true, ready: true, message: "Running", namespace: "minio", uiRoute: S3_UI, currentImage: MINIO_OLD,
    managedByTool: true, dataPVCs: ["minio-pvc"], migrationPending: true,
    warning: "MinIO from an earlier version of this tool still runs here and serves the pipeline servers. Re-run setup to replace it with SeaweedFS. Setup starts fresh: the objects stored in MinIO are not copied, so artifacts, logs and cached outputs of earlier pipeline runs return 404 (new runs work). Pipeline servers keep working with no edits (same Service, credentials and bucket), and the MinIO data volume, PVC minio-pvc, is kept for a rollback or a manual copy until teardown.",
    teardownBlockedReason: "1 pipeline server uses this S3 storage: dev-project/nightly-dspa. Tear it down first.",
  },
  migrated: {
    deployed: true, ready: true, message: "Running", namespace: "minio", uiRoute: S3_UI, currentImage: SEAWEEDFS,
    managedByTool: true, dataPVCs: ["seaweedfs-pvc", "minio-pvc"], keptPVCs: [{ name: "minio-pvc", size: "20Gi" }], uiUser: "admin",
    teardownBlockedReason: "1 pipeline server uses this S3 storage: dev-project/nightly-dspa. Tear it down first.",
  },
  incomplete: {
    deployed: true, ready: false, terminalError: true, namespace: "minio", currentImage: SEAWEEDFS, managedByTool: true, uiUser: "admin",
    dataPVCs: ["seaweedfs-pvc", "minio-pvc"], keptPVCs: [{ name: "minio-pvc", size: "20Gi" }],
    message: "Deployment seaweedfs is scaled to 0 replicas (for example by a manual rollback).",
    repairNeeded: "Deployment seaweedfs is scaled to 0 replicas (for example by a manual rollback).",
    teardownBlockedReason: "1 pipeline server uses this S3 storage: dev-project/nightly-dspa. Tear it down first.",
  },
  none: { deployed: false, ready: false, message: "Not deployed", managedByTool: false },
};

export const pipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "dev-project", name: "nightly-dspa", managedByTool: true,
  apiRoute: `https://ds-pipeline-nightly-dspa-dev-project.${APPS}`, dataPVCs: ["mariadb-nightly-dspa"],
};

export const mlflowStates = {
  none: { deployed: false, ready: false, message: "Not deployed", managedByTool: false },
  running: {
    deployed: true, ready: true, message: "Running", namespace: "redhat-ods-applications", name: "mlflow", managedByTool: true,
    uiRoute: `https://data-science-gateway.${APPS}/mlflow`, currentImage: `registry.redhat.io/rhoai/odh-mlflow-rhel9@${digest("mlflow")}`,
    dataPVCs: ["mlflow-pvc"],
  },
};

export const projects = ["dev-project", "fraud-detection", "llm-eval"];

// --- Versions and the update check ---------------------------------------------

export const versionInfo = (overrides = {}) => ({
  version: "v2.0.0", commit: hex("v2.0.0", 40), buildDate: "2026-10-06T09:00:00Z", goVersion: "go1.25.1",
  templateRevision: "4", expectedTemplateRevision: "4", templateOutdated: false, ...overrides,
});

export const noUpdate = { current: "v2.0.0", latest: "v2.0.0", updateAvailable: false, majorUpgrade: false };

// --- Diagnostics -----------------------------------------------------------------

/** A Diagnostics answer written by the backend (make docs-fixtures). */
export function diagnostics(name) {
  return JSON.parse(readFileSync(join(here, "diagnostics", `${name}.json`), "utf8"));
}

// --- Update progress (UpdateStep events, as the backend streams them) ------

/** The update pipeline of a successful update, step by step. */
export function updateSteps(image) {
  return [
    ["validate_prerequisites", "running", "Checking prerequisites..."],
    ["validate_prerequisites", "running", "Verifying the selected catalog image..."],
    ["validate_prerequisites", "success", "Prerequisites validated"],
    ["save_snapshot", "running", "Saving deployment snapshot..."],
    ["save_snapshot", "success", "Snapshot saved"],
    ["apply_catalog_source", "running", "Applying CatalogSource..."],
    ["apply_catalog_source", "success", "CatalogSource applied", image],
    ["wait_catalog_ready", "running", "Waiting for CatalogSource to become READY..."],
    ["wait_catalog_ready", "running", "CatalogSource: CONNECTING", "CONNECTING"],
    ["wait_catalog_ready", "success", "CatalogSource is READY"],
    ["detect_channel", "running", "Detecting target channel..."],
    ["detect_channel", "success", "Detected channel: stable-3.x", "stable-3.x"],
    ["delete_csv", "running", "Deleting old CSV and InstallPlan..."],
    ["delete_csv", "success", "Old CSV and InstallPlan deleted"],
    ["apply_subscription", "running", "Creating Subscription..."],
    ["apply_subscription", "success", "Subscription applied", "stable-3.x"],
    ["verify_installplan", "running", "Waiting for OLM to install the operator..."],
    ["verify_installplan", "running", "InstallPlan install-9fj2k: Installing"],
    ["verify_installplan", "running", "CSV rhods-operator.3.6.0: Installing (InstallWaiting)"],
    ["verify_installplan", "success", "Operator installed: rhods-operator.3.6.0", "rhods-operator.3.6.0"],
    ["operation_complete", "success", "RHOAI nightly update complete: rhods-operator.3.6.0 is installed."],
  ].map(([step, status, message, detail]) => ({ step, status, message, ...(detail ? { detail } : {}) }));
}
