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
// The Dashboard CR's status.url on a release behind the data-science gateway.
export const DASHBOARD_URL = `https://rh-ai.${APPS}`;
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
    dashboardURL: DASHBOARD_URL,
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

/**
 * GET /api/build-explorer/contains?pr=N|commit=SHA[&repo=owner/name]. A PR
 * (of any repository: the mock answers every one like the dashboard) is
 * merged 40 hours ago and reaches only the build from main after it;
 * release branches get a cherry-pick (another SHA). A commit is a fixed one
 * of rhoai-3.6, committed 40 hours ago. Builds more than an hour older than
 * either are answered from their date alone (built_before_merge /
 * built_before_commit), as the backend does.
 */
export function buildContains({ pr, commit, repo }, images) {
  repo = repo || "opendatahub-io/odh-dashboard";
  const name = repo.split("/")[1];
  const fork = `red-hat-data-services/${name}`;
  const base = pr ? hex(`merge-${pr}`, 40) : hex("commit-rhoai-3.6-2026-10-05", 40);
  const baseDate = pr ? ago(60 * 40) : ago(60 * 40 + 30);
  const subject = pr
    ? { pr: Number(pr), title: "Show the model deployment status in the project overview", url: `https://github.com/${repo}/pull/${pr}`, mergeCommit: base, mergedAt: baseDate }
    : { commit: base, commitDate: baseDate, commitTitle: "Show the model deployment status in the project overview", commitURL: `https://github.com/${fork}/commit/${base}` };
  return {
    repo,
    ...subject,
    builds: images.map((image) => {
      const b = tagBuilds.find((t) => image === fbc(t.tag, t.seed)) ?? installedBuild;
      const row = { image, tag: b.tag, installed: image === INSTALLED_IMAGE };
      const builtBefore = Date.parse(b.buildDate) < Date.parse(baseDate) - 3_600_000;
      if (builtBefore) {
        const hours = Math.round((Date.parse(baseDate) - Date.parse(b.buildDate)) / 3_600_000);
        const age = hours < 48 ? `${hours} hours` : `${Math.round(hours / 24)} days`;
        return pr
          ? { ...row, result: "not_contained", reason: "built_before_merge", message: `Built ${age} before the PR was merged.` }
          : { ...row, result: "not_contained", reason: "built_before_commit", message: `Built ${age} before the commit.` };
      }
      const buildCommit = hex(`${name}-${b.seed}`, 40);
      const contains = pr ? b.tag === "rhoai-3.7-ea.1" : b.tag === "rhoai-3.6";
      return {
        ...row, commit: buildCommit, commitRepo: fork,
        result: contains ? "contains" : "not_contained",
        compareURL: `https://github.com/${fork}/compare/${base}...${buildCommit}`,
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
    dscAPIVersion: "datasciencecluster.opendatahub.io/v2",
    dscCompatibility: {
      operatorVersion: "3.6.0", invalidFields: [], missingComponents: [], extraComponents: [], defaultsSource: "csv",
      defaultsAPIVersion: "datasciencecluster.opendatahub.io/v2",
    },
  };
}

/**
 * The operator's conversion webhook cannot convert the DSC to v3 (the
 * preferred version), so it is read as v2, and the bundle ships v3 defaults
 * only. Messages as pkg/cluster writes them (dsc_versions_test.go).
 */
export function conversionFallbackComponents() {
  const c = healthyComponents();
  c.dscVersionFallback = {
    version: "v3",
    used: "v2",
    message: "storage is (re)initializing: failed to list datasciencecluster.opendatahub.io/v3, Kind=DataScienceCluster: conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind \"DataScienceCluster\" is registered for version \"datasciencecluster.opendatahub.io/v3\" in scheme \"pkg/runtime/scheme.go:111\"",
  };
  c.dscCompatibility = {
    invalidFields: [], missingComponents: [], extraComponents: [],
    defaultsError: "This DataScienceCluster can only be read as v2: reading it as v3 fails because the operator's conversion webhook cannot convert it. " +
      "Operator 3.6.0 has no DataScienceCluster v2 defaults (none in the alm-examples of rhods-operator.3.6.0 or branch rhoai-3.6 of rhods-operator); it ships them as v3 only. " +
      "Component names differ between API versions, so the DataScienceCluster is not compared with defaults of another version. " +
      "Diagnostics explains the conversion failure and how to fix it.",
  };
  return c;
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
    rolloutPending: false, canAssistRollout: false, dashboardURL: DASHBOARD_URL,
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

/**
 * GET /api/dashboard/builds for dashboardSession(): the host was built from
 * the PR's latest commit; gen-ai-ui from a commit two pushes ago, and the
 * odh-pr-5123 tag has moved on to a newer build.
 */
export function dashboardBuilds(session = dashboardSession()) {
  const pr = session.prNumber;
  const repo = "opendatahub-io/odh-dashboard";
  const head = hex(`pr-${pr}-head`, 40);
  const older = hex(`pr-${pr}-older`, 40);
  const build = (i, commit) => ({
    deployment: i.deployment, container: i.container, image: i.currentImage, running: "pr", pr, flavor: "rhoai",
    commit, repo, commitURL: `https://github.com/${repo}/commit/${commit}`,
  });
  const host = session.devImages.find((i) => i.container === "rhods-dashboard");
  const genAI = session.devImages.find((i) => i.container === "gen-ai-ui");
  return {
    builds: [
      {
        ...build(host, head), buildDate: ago(55), commitDate: ago(70),
        divergence: { base: `pr-${pr}`, baseHead: head, status: "identical", behind: 0, ahead: 0, compareURL: `https://github.com/${repo}/compare/${head}...${head}`, prState: "open", prTitle: "Show the model deployment status in the project overview", checkedAt: ago(1) },
      },
      {
        ...build(genAI, older), buildDate: ago(60 * 5 + 10), commitDate: ago(60 * 5 + 30),
        divergence: { base: `pr-${pr}`, baseHead: head, status: "behind", behind: 2, ahead: 0, compareURL: `https://github.com/${repo}/compare/${older}...${head}`, prState: "open", prTitle: "Show the model deployment status in the project overview", checkedAt: ago(1) },
        newer: { image: `quay.io/${genAI.repository}:odh-pr-${pr}@${digest(`gen-ai-pr-${pr}-newer`)}`, tag: `odh-pr-${pr}`, commit: head, commitURL: `https://github.com/${repo}/commit/${head}`, buildDate: ago(50) },
      },
    ],
  };
}

// --- Environment (/test-resources) ---------------------------------------------

const SEAWEEDFS = `ghcr.io/chrislusf/seaweedfs@${digest("seaweedfs-4.54")}`;
const S3_UI = `https://seaweedfs-admin-seaweedfs.${APPS}`;
const BLOCKED = (ps) => `1 pipeline server uses this S3 storage: ${ps}. Delete it first.`;

export const s3States = {
  running: {
    deployed: true, ready: true, message: "Running", namespace: "seaweedfs", uiRoute: S3_UI, currentImage: SEAWEEDFS,
    managedByTool: true, dataPVCs: ["seaweedfs-data"], uiUser: "admin", namespaceDeletable: true, teardownBlockedReason: BLOCKED("dev-project/nightly-dspa"), usedBy: ["dev-project/nightly-dspa"],
  },
  incomplete: {
    deployed: true, ready: false, terminalError: true, namespace: "seaweedfs", currentImage: SEAWEEDFS, managedByTool: true, uiUser: "admin",
    dataPVCs: ["seaweedfs-data"],
    message: "Deployment seaweedfs is scaled to 0 replicas.",
    repairNeeded: "Deployment seaweedfs is scaled to 0 replicas.",
    teardownBlockedReason: BLOCKED("dev-project/nightly-dspa"), usedBy: ["dev-project/nightly-dspa"],
  },
  none: { deployed: false, ready: false, message: "Not deployed", namespace: "seaweedfs", managedByTool: false },
  starting: {
    deployed: true, ready: false, message: "0/1 ready", namespace: "seaweedfs", currentImage: SEAWEEDFS, managedByTool: true, uiUser: "admin",
    dataPVCs: ["seaweedfs-data"],
  },
  imagePull: {
    deployed: true, ready: false, terminalError: true, namespace: "seaweedfs", currentImage: "mirror.example.com/seaweedfs:4.54", managedByTool: true, uiUser: "admin",
    dataPVCs: ["seaweedfs-data"], waitingReason: "ImagePullBackOff",
    message: 'ImagePullBackOff: Back-off pulling image "mirror.example.com/seaweedfs:4.54": unauthorized',
  },
  // Namespace seaweedfs exists, made by someone else: setup refuses.
  foreign: {
    deployed: false, ready: false, namespace: "seaweedfs", managedByTool: false,
    message: "Namespace 'seaweedfs' exists but was not created by this tool, so setup will not modify it.",
    setupBlockedReason: "Namespace 'seaweedfs' exists but was not created by this tool, so setup will not modify it.",
    teardownBlockedReason: "Namespace 'seaweedfs' was not created by this tool.",
  },
  // Storage of an earlier version is still in namespace minio: nothing in
  // namespace seaweedfs yet, or (stage "remove") the new storage is running.
  migrationNeeded: {
    deployed: false, ready: false, message: "Not deployed", namespace: "seaweedfs", managedByTool: false, migrationPending: true,
    teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
  },
  migrated: {
    deployed: true, ready: true, message: "Running", namespace: "seaweedfs", uiRoute: S3_UI, currentImage: SEAWEEDFS,
    managedByTool: true, dataPVCs: ["seaweedfs-data"], uiUser: "admin", migrationPending: true, teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
  },
};

/** The pipeline server an earlier version created ("dspa"), on the old storage. */
export const legacyPipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "dev-project", name: "dspa", managedByTool: true,
  apiRoute: `https://ds-pipeline-dspa-dev-project.${APPS}`, uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/dev-project`, dataPVCs: ["mariadb-dspa"],
};

/** What 2.1 to 2.3 left in namespace minio (pkg/types LegacyStorageState). */
export function legacyStorage(stage) {
  return {
    namespace: "minio", server: "SeaweedFS", ready: true, endpoint: "minio-service.minio.svc:9000",
    objects: ["Deployment seaweedfs", "NetworkPolicy seaweedfs-ingress", "Route minio-ui", "Service minio-service", "Secret minio-secret", "PersistentVolumeClaim seaweedfs-pvc", "PersistentVolumeClaim minio-pvc"],
    dataPVCs: [{ name: "seaweedfs-pvc", size: "20Gi" }, { name: "minio-pvc", size: "20Gi" }],
    uncopiedPVCs: ["minio-pvc"],
    usedBy: stage === "migrate" ? ["dev-project/dspa"] : [],
    stage,
    repoint: stage === "migrate" ? ["dev-project/dspa"] : [],
    namespaceDeletable: true,
  };
}

/** The steps Migrate storage streams (pkg/cluster MigrateS3Storage). */
export function migrateSteps(kind) {
  const rows = kind === "migrate"
    ? [
      ["check", "running", "Checking the storage in namespace 'minio'..."],
      ["check", "success", "OK: SeaweedFS in namespace 'minio' is running"],
      ["storage", "running", "Setting up the storage in namespace 'seaweedfs' with the same keys..."],
      ["storage", "success", "OK: The new storage is ready"],
      ["copy", "running", "Copying every bucket and object from 'minio' to 'seaweedfs' (Job seaweedfs-migrate)..."],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["repoint", "running", "Moving pipeline server dev-project/dspa..."],
      ["repoint", "success", "OK: 1 pipeline server moved"],
      ["operation_complete", "success", "Migrated 37 objects (18204511 bytes) to namespace 'seaweedfs'. 1 pipeline server now uses it: dev-project/dspa. The old storage in namespace 'minio' is kept until you remove it."],
    ]
    : [
      ["check", "success", "OK: Nothing of this tool uses the old storage"],
      ["copy", "running", "Copying every bucket and object from 'minio' to 'seaweedfs' (Job seaweedfs-migrate)..."],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["remove", "running", "Deleting the old storage in namespace 'minio'..."],
      ["remove", "success", "OK: The old storage is deleted"],
      ["namespace", "success", "OK: Namespace 'minio' is being deleted, with everything else in it"],
      ["operation_complete", "success", "The old storage in namespace 'minio' was removed; its 37 objects (18204511 bytes) are in namespace 'seaweedfs'. Namespace 'minio' is being deleted, with everything else in it."],
    ];
  return rows.map(([step, status, message]) => ({ step, status, message }));
}

export const pipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "dev-project", name: "nightly-dspa", managedByTool: true,
  apiRoute: `https://ds-pipeline-nightly-dspa-dev-project.${APPS}`, uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/dev-project`, dataPVCs: ["mariadb-nightly-dspa"],
};

/** A pipeline server made in the dashboard, not by this tool (pkg/types UnmanagedPipelineServers). */
export const foreignPipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "fraud-detection", name: "dspa", managedByTool: false,
  uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/fraud-detection`,
  teardownBlockedReason: "This pipeline server was not created by this tool, so the tool will not delete it.",
};

/** A pipeline server that has just been set up (DSPO is starting it). */
export const pipelineServerStarting = { ...pipelineServer, ready: false, message: "Starting (1/3 ready)", uiRoute: undefined };

export const mlflowStates = {
  none: { deployed: false, ready: false, message: "Not deployed", managedByTool: false },
  // Made in the dashboard, not by this tool.
  foreign: {
    deployed: true, ready: true, message: "Running", namespace: "redhat-ods-applications", name: "mlflow", managedByTool: false,
    uiRoute: `https://data-science-gateway.${APPS}/mlflow`, currentImage: `registry.redhat.io/rhoai/odh-mlflow-rhel9@${digest("mlflow")}`,
    teardownBlockedReason: "This MLflow instance was not created by this tool, so the tool will not delete it.",
  },
  foreignBroken: {
    deployed: true, ready: false, terminalError: true, message: "CrashLoopBackOff", waitingReason: "CrashLoopBackOff", namespace: "redhat-ods-applications", name: "mlflow",
    managedByTool: false, currentImage: `registry.redhat.io/rhoai/odh-mlflow-rhel9@${digest("mlflow")}`,
    teardownBlockedReason: "This MLflow instance was not created by this tool, so the tool will not delete it.",
  },
  // A PR image that cannot be pulled.
  prBroken: {
    deployed: true, ready: false, terminalError: true, message: "ImagePullBackOff", waitingReason: "ImagePullBackOff", namespace: "redhat-ods-applications", name: "mlflow",
    managedByTool: true, prOverride: true, prNumber: 412, revertImage: "", currentImage: `quay.io/opendatahub/mlflow@${digest("mlflow-pr-412")}`, dataPVCs: ["mlflow-pvc"],
  },
  running: {
    deployed: true, ready: true, message: "Running", namespace: "redhat-ods-applications", name: "mlflow", managedByTool: true,
    uiRoute: `https://data-science-gateway.${APPS}/mlflow`, currentImage: `registry.redhat.io/rhoai/odh-mlflow-rhel9@${digest("mlflow")}`,
    dataPVCs: ["mlflow-pvc"],
  },
};

export const projects = ["dev-project", "fraud-detection", "llm-eval"];

// --- Models (pkg/types InferenceGatewayState, ModelsStatus, ModelState) ---
// The words are the backend's (pkg/cluster inference_gateway.go, models.go).
const INFERENCE = `https://inference.${APPS}`;
const PRESET = "v3-6-0-kserve-config-llm-template-cpu";
const PRESET_IMAGE = `registry.redhat.io/rhaii/vllm-cpu-rhel9@${digest("vllm-cpu-nightly")}`;
const RELEASED_IMAGE = `registry.redhat.io/rhaii/vllm-cpu-rhel9@${digest("vllm-cpu-3.5.3")}`;
const GW = { namespace: "openshift-ingress", name: "openshift-ai-inference" };
const usedBy = (models) => models.length
  ? { usedBy: models, teardownBlockedReason: `Models route through it: ${models.join(", ")}. Delete ${models.length > 1 ? "those models" : "that model"} first.` }
  : {};

export const gatewayStates = {
  none: { deployed: false, ready: false, message: "Not set up", managedByTool: false },
  starting: { ...GW, deployed: true, ready: false, managedByTool: true, message: "Programmed, waiting for an address", externalUrl: INFERENCE },
  ready: (models = []) => ({ ...GW, deployed: true, ready: true, managedByTool: true, message: "Running", externalUrl: INFERENCE, address: "a1b2c3d4e5f6-1234567890.us-east-1.elb.amazonaws.com", dns: "published", authPolicy: models.length ? "enforced" : "missing", ...usedBy(models) }),
  // RHOAI's chart (or an admin) made it: used as it is, never changed.
  foreign: (models = []) => ({ ...GW, deployed: true, ready: true, managedByTool: false, message: "Running", externalUrl: INFERENCE, address: "a9f8e7d6-0987654321.us-east-1.elb.amazonaws.com", authPolicy: "enforced", ...usedBy(models), teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." }),
  // Someone else's that reports no Programmed condition: unknown, not failed.
  unknown: { ...GW, deployed: true, ready: false, managedByTool: false, healthUnknown: true, message: "Reports no Programmed condition", externalUrl: INFERENCE, teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." },
};

const MODEL = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true };
export const modelStates = {
  ready: (ns = "dev-project", over = {}) => ({ ...MODEL, namespace: ns, ready: true, managedByTool: true, message: "Running", url: `${INFERENCE}/${ns}/qwen25-05b-instruct`, imageKind: "preset", hardwareProfile: "redhat-ods-applications/default-profile", ...over }),
  released: (ns = "dev-project") => modelStates.ready(ns, { imageKind: "released", imageVersion: "3.5.3", currentImage: RELEASED_IMAGE }),
  starting: (ns = "dev-project") => ({ ...MODEL, namespace: ns, ready: false, managedByTool: true, message: "MainWorkloadReady: Deployment qwen25-05b-instruct-kserve is not ready", imageKind: "preset" }),
  imageMissing: (ns = "dev-project") => ({
    ...MODEL, namespace: ns, ready: false, managedByTool: true, terminalError: true, presetImageMissing: true, imageKind: "preset", currentImage: undefined,
    waitingReason: "ErrImagePull", message: `ErrImagePull: reading manifest ${PRESET_IMAGE.split("@")[1]} in registry.redhat.io/rhaii/vllm-cpu-rhel9: manifest unknown`,
  }),
  foreign: (ns = "llm-eval") => ({ ...MODEL, displayName: "Qwen (team copy)", namespace: ns, ready: false, managedByTool: false, healthUnknown: true, message: "Reports no Ready condition", teardownBlockedReason: "This model was not created by this tool (for example in the dashboard), so the tool does not change or delete it." }),
};

export const modelsStatus = (over = {}) => ({
  dependencies: "True", preset: PRESET, imagePlan: "released",
  imagePlanReason: "This cluster runs a nightly RHOAI build, whose preset names an image that Red Hat has not published yet, and no mirror set serves registry.redhat.io/rhaii/vllm-cpu-rhel9; the newest released Red Hat image is used instead.",
  catalog: [{ name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct", cpu: "4", memoryRequest: "6Gi", memory: "8Gi" }],
  items: [],
  ...over,
});

export const CAPACITY_WARNING = "No worker seems to have room for the model: it needs 4 CPUs and 6Gi of memory free (up to 8Gi), and the most CPU free is 2.3 CPUs and 41.2 GiB on ip-10-0-1-23.ec2.internal. It may stay pending until a node is added or other workloads stop; the setup still runs.";

// --- Samples (pkg/types SampleStatus, MLflowSamplesStatus, PipelineSampleSpec) ---
// The names, messages and actions are the backend's (pkg/cluster samples_*.go).

const SAMPLE_MARKER = "rhoai-nightly-updater.sample";
export const SAMPLE_NAMES = {
  "genai-demo": "Agent and prompt demo",
  training: "Training experiment and registered model",
  "mcp-server": "MCP server",
  skill: "Agent skill",
};
export const SAMPLE_IDS = Object.keys(SAMPLE_NAMES);
const DEMO_REMOVE_HINT = "To remove the demo, delete it in the MLflow UI. This tool does not remove it, and Clean up leaves it in place.";
const mlflowLink = (project, id) => `https://data-science-gateway.${APPS}/mlflow/#/experiments/${id}?workspace=${encodeURIComponent(project)}`;

/** One MLflow sample of a project in the given state, as GetMLflowSamplesStatus reports it. */
export function sampleStatus(project, id, state, extra = {}) {
  const s = { id, name: SAMPLE_NAMES[id], state, actions: [] };
  const foreign = { "genai-demo": "", training: "Experiment 'nightly-sample-training'", "mcp-server": "MCP server 'com.deepwiki/rhoai-nightly-sample'", skill: "Skill 'nightly-sample-mcp-builder'" }[id];
  switch (state) {
    case "absent":
      s.actions = ["add"];
      break;
    case "removed":
      s.actions = ["add"];
      s.message = id === "genai-demo" ? "Removed in MLflow: Add restores the experiment and generates every feature again." : "Removed: the experiment is in MLflow's deleted view. Add restores it.";
      break;
    case "present":
      if (id === "genai-demo") { s.actions = ["regenerate"]; s.message = DEMO_REMOVE_HINT; s.link = mlflowLink(project, 4); }
      else s.actions = ["remove"];
      if (id === "training") s.link = mlflowLink(project, 7);
      if (id === "mcp-server") s.message = "Points at https://mcp.deepwiki.com/mcp; the playground needs internet access to call it.";
      if (id === "skill") s.message = "Registers https://github.com/anthropics/skills (skills/mcp-builder at e4bb5d960d87); the dashboard's Skills page needs the skillRegistry feature flag.";
      break;
    case "partial":
      if (id === "genai-demo") { s.actions = ["regenerate"]; s.missing = ["evaluation", "issues"]; s.message = "An earlier Add or Regenerate did not finish; Regenerate completes it."; s.link = mlflowLink(project, 4); }
      else if (id === "training") { s.actions = ["add", "remove"]; s.missing = ["registered model"]; s.link = mlflowLink(project, 7); }
      else if (id === "mcp-server") { s.actions = ["add", "remove"]; s.missing = ["endpoint"]; }
      else { s.actions = ["add", "remove"]; s.missing = ["version"]; }
      break;
    case "foreign":
      s.message = `${foreign} exists but was not created by this tool (it lacks the ${SAMPLE_MARKER} tag), so it is left unchanged.`;
      break;
    case "unsupported":
      s.message = id === "skill" ? "This MLflow has no Skill Registry." : "This MLflow has no MCP Server Registry.";
      break;
    case "unknown":
      s.message = "Cannot read the sample's state: MLflow answered 503 Service Unavailable.";
      break;
  }
  return { ...s, ...extra };
}

/** The MLflow samples of a project: states maps a sample id to its state (absent when not named). */
export const mlflowSamples = (project, states = {}) => ({
  project, available: true, mlflowURL: `https://data-science-gateway.${APPS}/mlflow`,
  samples: SAMPLE_IDS.map((id) => sampleStatus(project, id, states[id] || "absent")),
});

export const SAMPLE_PIPELINE_IMAGE = `registry.access.redhat.com/ubi9/ubi-minimal@${digest("ubi9-minimal")}`;
/** GET /api/resources/samples/pipeline/spec (pkg/cluster PipelineSampleSpec: the same YAML). */
export const pipelineSampleSpec = {
  name: "rhoai-nightly-sample", fileName: "rhoai-nightly-sample.yaml",
  description: "Sample created by rhoai-nightly-updater: a two-step pipeline that writes an artifact and reads it back.",
  image: SAMPLE_PIPELINE_IMAGE,
  yaml: `components:
    comp-read-artifact:
        executorLabel: exec-read-artifact
        inputDefinitions:
            artifacts:
                greeting:
                    artifactType:
                        schemaTitle: system.Artifact
                        schemaVersion: 0.0.1
    comp-write-artifact:
        executorLabel: exec-write-artifact
        outputDefinitions:
            artifacts:
                greeting:
                    artifactType:
                        schemaTitle: system.Artifact
                        schemaVersion: 0.0.1
deploymentSpec:
    executors:
        exec-read-artifact:
            container:
                args:
                    - echo "Read $0:" && cat "$0"
                    - '{{$.inputs.artifacts[''greeting''].path}}'
                command:
                    - sh
                    - -c
                image: ${SAMPLE_PIPELINE_IMAGE}
        exec-write-artifact:
            container:
                args:
                    - mkdir -p "$(dirname "$0")" && echo "Hello from the rhoai-nightly-updater sample pipeline" > "$0" && echo "Wrote $0"
                    - '{{$.outputs.artifacts[''greeting''].path}}'
                command:
                    - sh
                    - -c
                image: ${SAMPLE_PIPELINE_IMAGE}
pipelineInfo:
    description: 'Sample created by rhoai-nightly-updater: a two-step pipeline that writes an artifact and reads it back.'
    name: rhoai-nightly-sample
root:
    dag:
        tasks:
            read-artifact:
                cachingOptions: {}
                componentRef:
                    name: comp-read-artifact
                dependentTasks:
                    - write-artifact
                inputs:
                    artifacts:
                        greeting:
                            taskOutputArtifact:
                                outputArtifactKey: greeting
                                producerTask: write-artifact
                taskInfo:
                    name: read-artifact
            write-artifact:
                cachingOptions: {}
                componentRef:
                    name: comp-write-artifact
                taskInfo:
                    name: write-artifact
schemaVersion: 2.1.0
sdkVersion: kfp-2.13.0
`,
};

// --- Versions and the update check ---------------------------------------------

export const versionInfo = (overrides = {}) => ({
  version: "v2.0.0", commit: hex("v2.0.0", 40), buildDate: "2026-10-06T09:00:00Z", goVersion: "go1.25.1",
  templateRevision: "5", expectedTemplateRevision: "5", templateOutdated: false, ...overrides,
});

export const noUpdate = { current: "v2.0.0", latest: "v2.0.0", updateAvailable: false, majorUpgrade: false };

// --- Diagnostics -----------------------------------------------------------------

/** A Diagnostics answer written by the backend (make docs-fixtures). */
export function diagnostics(name) {
  return JSON.parse(readFileSync(join(here, "diagnostics", `${name}.json`), "utf8"));
}

// --- Update progress (UpdateStep events, as the backend streams them) ------

/** The GitOps install (Argo CD manages the operator), step by step. */
export function gitOpsUpdateSteps(image) {
  return [
    ["validate_prerequisites", "running", "Checking prerequisites..."],
    ["validate_prerequisites", "running", "Verifying the selected catalog image..."],
    ["validate_prerequisites", "success", "Prerequisites validated"],
    ["save_snapshot", "running", "Saving deployment snapshot..."],
    ["save_snapshot", "success", "Snapshot saved"],
    ["apply_catalog_source", "success", "CatalogSource left as Git sets it", image],
    ["wait_catalog_ready", "running", "Waiting for the catalog to serve the build..."],
    ["wait_catalog_ready", "success", "CatalogSource serves the build"],
    ["detect_channel", "success", "Channel: beta", "beta"],
    ["delete_csv", "running", "Deleting old CSV and InstallPlan..."],
    ["delete_csv", "success", "Old CSV and InstallPlan deleted"],
    ["apply_subscription", "running", "Recreating the Subscription..."],
    ["apply_subscription", "success", "Subscription applied", "beta"],
    ["verify_installplan", "running", "Waiting for OLM to install the operator..."],
    ["verify_installplan", "running", "InstallPlan install-9fj2k: Installing"],
    ["verify_installplan", "running", "CSV rhods-operator.3.6.0-ea.1: Installing (InstallWaiting)"],
    ["verify_installplan", "success", "Operator installed: rhods-operator.3.6.0-ea.1", "rhods-operator.3.6.0-ea.1"],
    ["operation_complete", "success", "RHOAI nightly update complete: rhods-operator.3.6.0-ea.1 is installed."],
  ].map(([step, status, message, detail]) => ({ step, status, message, ...(detail ? { detail } : {}) }));
}

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

// --- Playground (pkg/types PlaygroundStatus, PlaygroundState, PlaygroundModel) ---
// The words are the backend's (pkg/cluster playground.go); the models are
// what the dashboard's gen-ai API lists (aaa/models, maas/models).

export const playgroundModels = {
  qwen: { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", source: "namespace", status: "Running", ready: true, chat: true },
  deepseek: { name: "deepseek-r1-qwen-7b", displayName: "DeepSeek R1 Distill Qwen 7B", source: "namespace", status: "Failed", ready: false, chat: true },
  maas: { name: "gpt-oss-20b", source: "maas", status: "Ready", ready: true, chat: true },
  opt: { name: "publishers/llm/models/facebook/opt-125m", displayName: "Facebook OPT 125M (Simulated)", source: "maas", status: "Ready", ready: true, chat: true },
};

const PLAYGROUND = { name: "lsd-genai-playground", deployed: true, requested: true, managedByTool: true };
export const playgroundStates = {
  starting: (ns = "dev-project") => ({ ...PLAYGROUND, namespace: ns, ready: false, phase: "Initializing", message: "Starting (phase Initializing)" }),
  ready: (ns = "dev-project", over = {}) => ({ ...PLAYGROUND, namespace: ns, ready: true, phase: "Ready", message: "Ready", ...over }),
  foreign: (ns = "dev-project") => playgroundStates.ready(ns, { managedByTool: false, requested: false }),
  failed: (ns = "dev-project") => ({
    ...PLAYGROUND, namespace: ns, ready: false, phase: "Failed", terminalError: true,
    message: `The playground's server failed to start (phase Failed). The events of project ${ns} and its playground pods say why; delete it and set it up again.`,
  }),
  terminating: (ns = "dev-project") => ({ ...PLAYGROUND, namespace: ns, ready: false, phase: "Terminating", terminating: true, message: "Terminating" }),
};

/** The playground world: cluster parts, items, and the models of each project. */
export const playgroundWorld = (over = {}) => ({
  operatorReady: "True", genAiUi: "available", items: [],
  modelsByProject: { "dev-project": [playgroundModels.qwen, playgroundModels.maas, playgroundModels.opt], "fraud-detection": [playgroundModels.maas], "llm-eval": [playgroundModels.maas] },
  ...over,
});

export const PLAYGROUND_API_DIFFERS = "This RHOAI release's dashboard gen-ai API differs from the one this tool knows: GET /gen-ai/api/v1/aaa/models answered 404 (the requested resource could not be found). Use the playground from the dashboard's Gen AI studio instead.";
export const PLAYGROUND_BLOCKED = { component: "ogx", dsc: "default-dsc", state: "Removed", reason: "The Gen AI playground is turned off on this cluster: component ogx is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Managed." };

// --- Models as a Service (GET /api/resources/maas, pkg/cluster GetMaaSStatus) ---

const MAAS_DOCS = "https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/content/";
export const MAAS_INFRA = "redhat-ai-gateway-infra";
export const MAAS_AUTHORINO_COMMANDS = `oc annotate service authorino-authorino-authorization -n kuadrant-system \\
  service.beta.openshift.io/serving-cert-secret-name=authorino-server-cert --overwrite
oc patch authorino authorino -n kuadrant-system --type=merge \\
  -p '{"spec":{"listener":{"tls":{"enabled":true,"certSecretRef":{"name":"authorino-server-cert"}}}}}'
oc -n kuadrant-system set env deployment/authorino \\
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt \\
  REQUESTS_CA_BUNDLE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
oc rollout restart deployment/authorino -n kuadrant-system`;
const RHCL_WARNING = "Red Hat Connectivity Link 1.4.3 is installed; the MaaS docs ask for 1.5 or later (OLM channel stable-1.5): on 1.4.x a caller with two or more subscriptions can hang. A cluster-admin upgrades it; MaaS works for a single subscription meanwhile.";

/** The platform's parts by state: "none" (not set up), "starting", "ready", "foreign" (someone else's). */
export function maasParts(state, { tls = true } = {}) {
  const tool = state !== "foreign";
  const ready = state === "ready" || state === "foreign";
  const on = state !== "none";
  return [
    { id: "dsc", title: "MaaS in the DataScienceCluster", docUrl: `${MAAS_DOCS}install/maas-setup.md#configure-datasciencecluster`,
      ...(on ? { state: ready ? "ready" : "waiting", message: ready ? "aigateway and modelsAsAService are Managed" : "modelsAsAService is Managed; not ready: deployments are starting", managedByTool: tool, notManaged: !tool }
        : { state: "missing", message: "modelsAsAService is Removed; setup sets it to Managed, last" }) },
    { id: "kuadrant", title: "Connectivity Link (Kuadrant)", docUrl: `${MAAS_DOCS}install/platform-setup.md`, state: "ready", message: "Kuadrant kuadrant-system/kuadrant is Ready", notManaged: true, warning: RHCL_WARNING },
    { id: "database", title: "Database (PostgreSQL) and maas-db-config", docUrl: `${MAAS_DOCS}install/maas-setup.md#database-setup`,
      ...(!on ? { state: "missing", message: `Setup runs a one-instance CloudNativePG database (development and test use) in ${MAAS_INFRA}` }
        : tool ? { state: "ready", managedByTool: true, message: `CloudNativePG ${MAAS_INFRA}/maas-db runs (1 instance, development and test use); Secret maas-db-config set` }
          : { state: "ready", notManaged: true, message: `Secret ${MAAS_INFRA}/maas-db-config exists: maas-api uses the database it names` }) },
    { id: "gateway", title: "Gateway maas-default-gateway", docUrl: `${MAAS_DOCS}install/maas-setup.md#create-gateway`,
      ...(on ? { state: "ready", message: "Programmed at a1b2c3.elb.us-east-1.amazonaws.com", managedByTool: tool, notManaged: !tool }
        : { state: "missing", message: `Setup creates it with an external load balancer at https://maas.${APPS}` }) },
    tls ? { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}install/maas-setup.md#create-gateway`, state: "ready", message: "Authorino in kuadrant-system serves TLS and trusts the service CA", notManaged: true }
      : { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}install/maas-setup.md#create-gateway`, state: "manual", notManaged: true, commands: MAAS_AUTHORINO_COMMANDS,
        message: "Not configured: trust of the service CA (SSL_CERT_FILE). A cluster-admin runs the docs' setup-authorino-tls.sh commands below (this tool does not change Connectivity Link's Authorino). Without it, API keys are refused." },
    { id: "maas-api", title: "MaaS API (maas-api)", docUrl: `${MAAS_DOCS}install/validation.md`,
      ...(!on ? { state: "missing", message: "The operator creates it once MaaS is turned on" } : ready ? { state: "ready", message: `Available in ${MAAS_INFRA}` } : { state: "waiting", message: "Not available yet (it restarts until the database answers)" }) },
    { id: "tenant", title: "Default tenant and gateway authentication", docUrl: `${MAAS_DOCS}install/maas-setup.md#maastenantconfig-cr`,
      ...(!on ? { state: "missing", message: "The operator creates it once MaaS is turned on" } : ready ? { state: "ready", message: "Tenant models-as-a-service is Ready; the gateway's authentication is enforced" } : { state: "waiting", message: "Waiting for the operator (tenant missing, configuration missing, authentication missing)" }) },
  ];
}

/** This tool's model in dev-project: published or not. */
export function maasModel(published, { ready = true } = {}) {
  return {
    namespace: "dev-project", name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", managedByTool: true, modelReady: true,
    deployed: published, ready: published && ready, published, message: published ? (ready ? "Published" : "Published; waiting") : "Not published",
    ...(published ? { phase: ready ? "Ready" : "Pending", endpoint: `https://maas.${APPS}/dev-project/qwen25-05b-instruct`, access: ready ? "Active" : "Pending", subscription: ready ? "Active" : "Pending" } : {}),
  };
}

/** A recorded publication whose model is gone (GetMaaSStatus's extra row). */
export function maasOrphanPublication(namespace) {
  return { namespace, name: "qwen25-05b-instruct", displayName: "qwen25-05b-instruct", deployed: true, ready: false, managedByTool: true, published: true, partly: true,
    message: "Partly published: the model is gone; this tool's record of the publication still in place. Unpublish to finish removing it." };
}

/** Removal refused because others use MaaS (pkg/cluster teardown, foreignMaaSObjects). */
export const MAAS_IN_USE = "Others use this MaaS platform: MaaSModelRef team-a/granite-3-8b, MaaSSubscription models-as-a-service/team-a-subscription. Removing it would break them. They unpublish and remove theirs first. Nothing was deleted.";

/** The MaaS status by state. */
export function maasWorld(state = "none", { tls = true, items = [], blocked = null, noCNPG = false } = {}) {
  const parts = maasParts(state, { tls });
  if (noCNPG) Object.assign(parts[2], { state: "manual", message: "The CloudNativePG operator is not installed. A cluster-admin installs it from OperatorHub (certified operators), or creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md)." });
  const allReady = parts.every((p) => p.state === "ready");
  const tool = parts.some((p) => p.managedByTool);
  return {
    ...(blocked ? { blocked } : {}),
    platform: {
      namespace: MAAS_INFRA, deployed: state !== "none", ready: allReady, managedByTool: tool,
      message: allReady ? "Ready" : state === "none" ? "Not set up" : "Needs attention",
      ...(blocked ? { setupBlockedReason: blocked.reason } : noCNPG ? { setupBlockedReason: parts[2].message } : {}),
      ...(!tool ? { teardownBlockedReason: "No part of the MaaS platform was set up by this tool, so there is nothing for it to remove." } : {}),
    },
    parts, url: `https://maas.${APPS}`, subscription: "rhoai-nightly-updater-subscription", dashboardUrl: DASHBOARD_URL,
    items: items.map((i) => (i.published || allReady ? i : { ...i, setupBlockedReason: "Set up the MaaS platform first." })),
  };
}

export const MAAS_BLOCKED = { component: "aigateway", dsc: "default-dsc", state: "Removed",
  reason: "MaaS runs in RHOAI's AI gateway, which is Removed in DataScienceCluster default-dsc. A cluster-admin sets aigateway to Managed on the Components page (MaaS platform setup, platform-setup.md); then MaaS can be set up here." };
