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
// pkg/api writeAuthError and sessionProblem (scopeRefusal): an expired or
// revoked sign-in, and one made before 4.0.0 that cannot carry a change.
export const SESSION_EXPIRED_MESSAGE = "Your OpenShift session has expired. Log in again.";
export const SIGN_IN_SCOPE_MESSAGE = "Your sign-in cannot make changes: it was made before this updater acted with your own OpenShift permissions. Log out and log in again.";
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
    cluster: { server: "https://api.example-cluster.example.com:443", version: "4.20.6", user: USER, controlPlane: "hosted" },
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

/**
 * The command a self-managed cluster's setup shows: the backend's own file
 * (pkg/cluster/node_pull_secret.sh, embedded as NodePullSecretCommand).
 */
export const NODE_PULL_SECRET_COMMAND = readFileSync(join(here, "../../../pkg/cluster/node_pull_secret.sh"), "utf8").trim();

/**
 * A self-managed cluster (controlPlane "self-managed"), or one whose topology
 * could not be read (controlPlane absent): the nodes pull with
 * openshift-config/pull-secret, so the status carries pullSecret.nodes.
 */
export function selfManaged(s, { known = true, pulled = false } = {}) {
  if (known) s.cluster.controlPlane = "self-managed";
  else delete s.cluster.controlPlane;
  s.pullSecret = { ...s.pullSecret, nodes: { command: NODE_PULL_SECRET_COMMAND, pulled } };
  return s;
}

/** What POST /api/setup/node-pull-check answers. */
export const nodePullCheck = (image, ok) => ok
  ? { success: true, message: `A temporary catalog pod pulled ${image} with the nodes' pull secret, and was removed again. A node whose machine config pool is still updating may not have the credentials yet: oc get mcp shows UPDATED True for every pool when they all do.`, logs: [] }
  : { success: false, errorCode: "catalog_image_pull", message: `The catalog image ${image} cannot be pulled (ErrImagePull: unauthorized: access to the requested resource is not authorized). This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with additional-pull-secret, which only the updater uses here. A cluster-admin adds the quay.io/rhoai entry with the command under Pull secret in the cluster setup (Status page); the nodes pick it up within a few minutes. Also check that the image exists.`, logs: [] };

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
    managedByTool: true, dataPVCs: ["seaweedfs-data"], uiUser: "admin", namespaceDeletable: true, teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
  },
  incomplete: {
    deployed: true, ready: false, terminalError: true, namespace: "seaweedfs", currentImage: SEAWEEDFS, managedByTool: true, uiUser: "admin",
    dataPVCs: ["seaweedfs-data"],
    message: "Deployment seaweedfs is scaled to 0 replicas.",
    repairNeeded: "Deployment seaweedfs is scaled to 0 replicas.",
    teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
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
    message: "Namespace seaweedfs exists but was not created by this tool, so setup will not modify it.",
    setupBlockedReason: "Namespace seaweedfs exists but was not created by this tool, so setup will not modify it.",
    teardownBlockedReason: "Namespace seaweedfs was not created by this tool.",
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
      ["check", "running", "Checking the storage in namespace minio..."],
      ["check", "success", "OK: SeaweedFS in namespace minio is running"],
      ["storage", "running", "Setting up the storage in namespace seaweedfs with new keys..."],
      ["storage", "success", "OK: The new storage is ready"],
      ["copy", "running", "Copying every bucket and object from minio to 'seaweedfs' (Job seaweedfs-migrate)..."],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["repoint", "running", "Moving pipeline server dev-project/dspa..."],
      ["repoint", "success", "OK: 1 pipeline server moved"],
      ["operation_complete", "success", "Migrated 37 objects (18204511 bytes) to namespace seaweedfs. 1 pipeline server now uses it: dev-project/dspa. The old storage in namespace minio is kept: delete it later from the S3 storage's menu, once nothing else uses it."],
    ]
    : [
      ["check", "success", "OK: Nothing of this tool uses the old storage"],
      ["copy", "running", "Copying every bucket and object from minio to 'seaweedfs' (Job seaweedfs-migrate)..."],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["remove", "running", "Deleting the old storage in namespace minio..."],
      ["remove", "success", "OK: The old storage is deleted"],
      ["namespace", "success", "OK: Namespace minio is being deleted, with everything else in it"],
      ["operation_complete", "success", "The old storage in namespace minio was removed; its 37 objects (18204511 bytes) are in namespace seaweedfs. Namespace minio is being deleted, with everything else in it."],
    ];
  return rows.map(([step, status, message]) => ({ step, status, message }));
}

export const pipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "dev-project", name: "dspa", managedByTool: true,
  apiRoute: `https://ds-pipeline-dspa-dev-project.${APPS}`, uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/dev-project`, dataPVCs: ["mariadb-dspa"],
  connection: "rhoai-nightly-updater-s3", connectionState: "present",
};

/** A pipeline server an earlier version set up as nightly-dspa (pkg/types NonStandardName): Recreate is offered. */
export const pipelineServerOldName = {
  ...pipelineServer, name: "nightly-dspa", nonStandardName: true, connection: undefined, connectionState: "missing",
  apiRoute: `https://ds-pipeline-nightly-dspa-dev-project.${APPS}`, dataPVCs: ["mariadb-nightly-dspa"],
};

/** A pipeline server made in the dashboard, not by this tool (pkg/types UnmanagedPipelineServers). */
export const foreignPipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "fraud-detection", name: "dspa", managedByTool: false,
  uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/fraud-detection`,
  teardownBlockedReason: "This pipeline server was not created by this tool, so the tool will not delete it.",
};

/** A pipeline server that has just been set up (DSPO is starting it). */
export const pipelineServerStarting = { ...pipelineServer, ready: false, message: "Starting (1/3 ready)", uiRoute: undefined };

/** This tool's pipeline server in project ns. */
export const pipelineServerIn = (ns, over = {}) => ({
  ...pipelineServer, namespace: ns, apiRoute: pipelineServer.apiRoute.replace("dev-project", ns), uiRoute: pipelineServer.uiRoute.replace("dev-project", ns), ...over,
});

/** Not ready long after it was created (pkg/cluster pipeline_server.go: dspaStuckAfter), with the Ready condition's message. */
export const pipelineServerStuck = (ns) => pipelineServerIn(ns, {
  ready: false, terminalError: true, uiRoute: undefined, apiRoute: undefined,
  message: "Deployment ds-pipeline-dspa: 0/1 replicas available (database connection refused)",
});

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
// The projects the tool's gateway lists by name (allowedRoutes
// kubernetes.io/metadata.name In [...], RHOAI's documented values array): its
// own namespace, then each project a model was set up in.
const ALLOWED = ["openshift-ingress", "a-test-project", "dev-project", "fraud-detection", "llm-eval"];
const GW = { namespace: "openshift-ingress", name: "openshift-ai-inference", allowedProjects: ALLOWED };
const usedBy = (models) => models.length
  ? { usedBy: models, teardownBlockedReason: `Models route through it: ${models.join(", ")}. Delete ${models.length > 1 ? "those models" : "that model"} first.` }
  : {};

export const gatewayStates = {
  none: { deployed: false, ready: false, message: "Not set up", managedByTool: false },
  starting: { ...GW, deployed: true, ready: false, managedByTool: true, message: "Programmed, waiting for an address", externalUrl: INFERENCE },
  ready: (models = []) => ({ ...GW, deployed: true, ready: true, managedByTool: true, message: "Running", externalUrl: INFERENCE, address: "a1b2c3d4e5f6-1234567890.us-east-1.elb.amazonaws.com", dns: "published", addedProjects: models.length ? ["dev-project"] : undefined, authPolicy: models.length ? "enforced" : "missing", ...usedBy(models) }),
  // RHOAI's chart (or an admin) made it: used as it is, never changed. The
  // chart's documented allowedRoutes: a list of project names.
  foreign: (models = []) => ({ ...GW, allowedProjects: ["openshift-ingress", "llm-eval"], deployed: true, ready: true, managedByTool: false, message: "Running", externalUrl: INFERENCE, address: "a9f8e7d6-0987654321.us-east-1.elb.amazonaws.com", authPolicy: "enforced", ...usedBy(models), teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." }),
  // models ("namespace/name") route through the gateway from projects it
  // does not list (deployed from the dashboard): their routes are rejected.
  notAllowed: (gw, models) => ({ ...gw, allowedProjects: gw.allowedProjects.filter((p) => !models.some((m) => m.startsWith(`${p}/`))),
    notAllowedModels: models, notAllowedModelProjects: [...new Set(models.map((m) => m.split("/")[0]))] }),
  // Someone else's that reports no Programmed condition: unknown, not failed.
  unknown: { ...GW, deployed: true, ready: false, managedByTool: false, healthUnknown: true, message: "Reports no Programmed condition", externalUrl: INFERENCE, teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." },
};

/** The Models catalog, as pkg/cluster modelCatalog reports it. */
export const MODEL_CATALOG = [
  { name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", uri: "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac", cpu: "2", memoryRequest: "4Gi", memory: "5Gi",
    type: "chat", useCase: "Chat (smoke test)", downloadSize: "270 MB", args: "--max-model-len 4096",
    description: "A very small chat model, for checking that model serving works. From Hugging Face (HuggingFaceTB/SmolLM2-135M-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775", cpu: "4", memoryRequest: "6Gi", memory: "8Gi",
    type: "chat", useCase: "Chat", downloadSize: "1.0 GB", args: "--max-model-len 4096", default: true,
    description: "A small chat model. From Hugging Face (Qwen/Qwen2.5-0.5B-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", uri: "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e", cpu: "4", memoryRequest: "12Gi", memory: "14Gi",
    type: "chat", toolCalling: true, useCase: "Chat, Tool calling, Reasoning", downloadSize: "4.1 GB",
    args: `--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking":false}'`,
    description: "A chat model that can call tools; its reasoning (thinking) is off unless a request turns it on. From Hugging Face (Qwen/Qwen3-1.7B), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", uri: "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f", cpu: "2", memoryRequest: "3Gi", memory: "4Gi",
    type: "embedding", embeddingDimension: 768, useCase: "Embeddings, Semantic search", downloadSize: "250 MB", args: "--runner pooling --max-model-len 512",
    description: "An English embedding model (768 dimensions) for semantic search. From Hugging Face (ibm-granite/granite-embedding-125m-english), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
];
const displayOf = (name) => MODEL_CATALOG.find((c) => c.name === name)?.displayName ?? name;

const MODEL = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true };
export const modelStates = {
  ready: (ns = "dev-project", over = {}) => ({ ...MODEL, namespace: ns, ready: true, managedByTool: true, message: "Running", url: `${INFERENCE}/${ns}/${over.name ?? MODEL.name}`, imageKind: "preset", hardwareProfile: "redhat-ods-applications/default-profile",
    // The dashboard's token authentication the tool makes for each model (pkg/cluster models_token.go).
    tokenSecret: `default-name-${over.name ?? MODEL.name}-sa`, tokenServiceAccount: `${over.name ?? MODEL.name}-sa`, ...over }),
  /** Another catalog model, ready (on the released image, as the mock cluster is a nightly one). */
  readyAs: (name, ns = "dev-project") => modelStates.ready(ns, { name, displayName: displayOf(name), imageKind: "released", imageVersion: "3.5.3", currentImage: RELEASED_IMAGE,
    hardwareProfile: name === "qwen3-1-7b" ? undefined : "redhat-ods-applications/default-profile" }),
  startingAs: (name, ns = "dev-project") => ({ ...MODEL, name, displayName: displayOf(name), namespace: ns, ready: false, managedByTool: true, message: `MainWorkloadReady: Deployment ${name}-kserve is not ready`, imageKind: "released" }),
  released: (ns = "dev-project") => modelStates.ready(ns, { imageKind: "released", imageVersion: "3.5.3", currentImage: RELEASED_IMAGE }),
  starting: (ns = "dev-project") => ({ ...MODEL, namespace: ns, ready: false, managedByTool: true, message: "MainWorkloadReady: Deployment qwen25-05b-instruct-kserve is not ready", imageKind: "preset" }),
  imageMissing: (ns = "dev-project") => ({
    ...MODEL, namespace: ns, ready: false, managedByTool: true, terminalError: true, presetImageMissing: true, imageKind: "preset", currentImage: undefined,
    hardwareProfile: "redhat-ods-applications/default-profile", tokenSecret: "default-name-qwen25-05b-instruct-sa", tokenServiceAccount: "qwen25-05b-instruct-sa",
    waitingReason: "ErrImagePull", message: `ErrImagePull: reading manifest ${PRESET_IMAGE.split("@")[1]} in registry.redhat.io/rhaii/vllm-cpu-rhel9: manifest unknown`,
  }),
  foreign: (ns = "llm-eval") => ({ ...MODEL, displayName: "Qwen (team copy)", namespace: ns, ready: false, managedByTool: false, healthUnknown: true, message: "Reports no Ready condition", teardownBlockedReason: "This model was not created by this tool (for example in the dashboard), so the tool does not change or delete it." }),
};

export const modelsStatus = (over = {}) => {
  const out = {
    dependencies: "True", preset: PRESET, imagePlan: "released",
    imagePlanReason: "This cluster runs a nightly RHOAI build, whose preset names an image that Red Hat has not published yet, and no mirror set serves registry.redhat.io/rhaii/vllm-cpu-rhel9; the newest released Red Hat image is used instead.",
    catalog: MODEL_CATALOG,
    items: [],
    ...over,
  };
  // The backend reports how every deployed model is reached (addModelRouting); a
  // fixture that names none is on KServe's default gateway, as models were before 4.0.0.
  out.items = out.items.map((i) => (i && i.deployed && !i.exposure ? routed(i) : i));
  return out;
};

/** The status's room check of one catalog model (pkg/cluster roomWarning). */
export const CAPACITY_WARNING = "No worker seems to have room for the model: it needs 4 CPUs and 12 GiB of memory free (up to 14 GiB); the worker with the most free CPU, ip-10-0-1-23.ec2.internal, has 5.5 CPUs and 8.2 GiB free.";
/** The catalog with that warning on Qwen3. */
export const catalogWithoutRoom = (name = "qwen3-1-7b") => MODEL_CATALOG.map((c) => (c.name === name ? { ...c, capacityWarning: CAPACITY_WARNING } : c));

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
      s.message = id === "genai-demo"
        ? "Experiment 'MLflow Demo' holds MLflow's demo, made in the MLflow UI or by someone else, not by this tool. Regenerating it deletes traces and evaluations in that experiment, whoever logged them, so this tool leaves it alone: regenerate or delete it in the MLflow UI."
        : `${foreign} exists but was not created by this tool (it lacks the ${SAMPLE_MARKER} tag), so it is left unchanged.`;
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

/** The demo on an MLflow CR with the 2Gi limit earlier versions set (pkg/cluster demoStatus). */
export const DEMO_LOW_MEMORY = {
  message: "MLflow still has the 2Gi memory limit an earlier version of this tool set, and generating the demo runs MLflow out of memory. Add or Regenerate first removes that limit (spec.resources of MLflow CR mlflow, so the operator's defaults of 4 CPU / 3Gi apply) and MLflow restarts; then add the demo again once MLflow is ready.",
  repair: "MLflow CR mlflow: spec.resources (requests 500m/1Gi, limits 2/2Gi, set by an earlier version of this tool) is removed, so the operator's defaults (limits 4 CPU / 3Gi) apply. MLflow restarts, and the demo is not generated in this run: add it again once MLflow is ready.",
};
export const DEMO_LOW_MEMORY_FOREIGN = "MLflow's memory limit is 2Gi (spec.resources of MLflow CR mlflow), and generating the demo runs MLflow out of memory. This tool did not create that MLflow, so it does not change the limit: raise it, or remove spec.resources so the operator's defaults (3Gi) apply.";

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
  version: "v4.0.0", commit: hex("v4.0.0", 40), buildDate: "2026-11-19T09:00:00Z", goVersion: "go1.25.1",
  templateRevision: "7", expectedTemplateRevision: "7", templateOutdated: false, ...overrides,
});

export const noUpdate = { current: "v4.0.0", latest: "v4.0.0", updateAvailable: false, majorUpgrade: false };

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
  granite: { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", source: "namespace", status: "Running", ready: true, chat: false, embeddingDimension: 768 },
  deepseek: { name: "deepseek-r1-qwen-7b", displayName: "DeepSeek R1 Distill Qwen 7B", source: "namespace", status: "Failed", ready: false, chat: true },
  maas: { name: "gpt-oss-20b", source: "maas", status: "Ready", ready: true, chat: true, subscriptions: ["rhoai-nightly-updater-subscription"] },
  opt: { name: "publishers/llm/models/facebook/opt-125m", displayName: "Facebook OPT 125M (Simulated)", source: "maas", status: "Ready", ready: true, chat: true, subscriptions: ["rhoai-nightly-updater-subscription"] },
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
  -p '{"spec":{"listener":{"tls":{"certSecretRef":{"name":"authorino-server-cert"},"enabled":true}}}}'
oc -n kuadrant-system set env deployment/authorino \\
  SSL_CERT_FILE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt \\
  REQUESTS_CA_BUNDLE=/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
oc rollout restart deployment/authorino -n kuadrant-system`;
const RHCL_WARNING = "Red Hat Connectivity Link 1.4.3 is installed; the MaaS docs ask for 1.5 or later: on 1.4.x a caller with two or more subscriptions can hang. MaaS works for a single subscription meanwhile. Upgrade Connectivity Link takes the upgrade path OLM offers, one channel at a time, and sets up Authorino TLS again afterwards.";
const RHCL_UPGRADE = {
  id: "upgrade-rhcl", label: "Upgrade Connectivity Link",
  summary: "Upgrades Red Hat Connectivity Link from 1.4.3 to 1.5 or later, as the MaaS docs ask, one channel at a time along the path OLM offers. Gateway authentication and rate limits restart during each step; it takes a few minutes.",
  changes: [
    "Subscription kuadrant-system/kuadrant-operator: channel stable to stable-1.5 (version 1.5.0). OLM upgrades the operator; there is no way back to 1.4.3.",
    "If OLM has no path from 1.4.3 straight to 1.5.0 (it then stays at 1.4.3), channel stable-1.4 first, to 1.4.4, then stable-1.5.",
    "Afterwards, Authorino TLS is set up again where an upgrade step recreated Authorino's Service (which removes its certificate).",
  ],
};
const RHCL_UPGRADE_COMMANDS = `# OLM may have no path from 1.4.3 straight to stable-1.5: go through stable-1.4 first
oc patch subscription 'kuadrant-operator' -n 'kuadrant-system' --type=merge -p '{"spec":{"channel":"stable-1.4"}}'
# once rhcl-operator.v1.4.4 is installed:
oc patch subscription 'kuadrant-operator' -n 'kuadrant-system' --type=merge -p '{"spec":{"channel":"stable-1.5"}}'`;
const RHCL_INSTALL_COMMANDS = `oc apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: kuadrant-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kuadrant-operator
  namespace: kuadrant-system
spec:
  channel: stable-1.5
  installPlanApproval: Automatic
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  config:
    env:
    - name: RATELIMIT_CHECK_SERVICE_FAILURE_MODE
      value: deny
    - name: RATELIMIT_REPORT_SERVICE_FAILURE_MODE
      value: deny
EOF
oc wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/kuadrant-operator -n kuadrant-system --timeout=300s`;
const AUTHORINO_ACTION = {
  id: "authorino-tls", label: "Configure Authorino TLS",
  summary: "Applies the steps of the MaaS docs' setup-authorino-tls.sh to Connectivity Link's Authorino in kuadrant-system (missing: trust of the service CA (SSL_CERT_FILE)). Only what is not set yet changes; Authorino restarts. It stays after Clean up: RHOAI's inference gateway uses Authorino too.",
  changes: [
    "Service kuadrant-system/authorino-authorino-authorization: annotation service.beta.openshift.io/serving-cert-secret-name=authorino-server-cert (service-ca makes the certificate)",
    "Authorino kuadrant-system/authorino: spec.listener.tls enabled, certSecretRef authorino-server-cert",
    "Deployment kuadrant-system/authorino: SSL_CERT_FILE and REQUESTS_CA_BUNDLE at the service CA (/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt), unless it trusts the service CA already; then a rollout restart",
  ],
};
const TIER_POLICIES = {
  id: "tier-policies", title: "Old gateway policies", docUrl: `${MAAS_DOCS}migration/upgrade-to-3.4.md#phase-4-manual-cleanup-of-old-tier-resources`, state: "manual", notManaged: true,
  message: "AuthPolicy gateway-auth-policy, TokenRateLimitPolicy gateway-tier-rate-limits are on maas-default-gateway and are not managed by maas-controller. RHOAI's upgrade from the tier-based MaaS leaves its gateway policies in place, and with old and new gateway policies \"Kuadrant behavior is undefined\" (the MaaS docs' upgrade guide), so this tool publishes nothing until they are gone.",
  commands: "oc delete authpolicy 'gateway-auth-policy' -n openshift-ingress\noc delete tokenratelimitpolicy 'gateway-tier-rate-limits' -n openshift-ingress",
  actions: [{ id: "remove-tier-policies", label: "Remove the old tier policies", danger: true,
    summary: "Deletes the gateway policies of the tier-based MaaS that RHOAI's upgrade leaves behind, as the MaaS docs' upgrade guide (Phase 4) does by hand. maas-controller's own policies stay. Clients that relied on the old tiers' rules lose them.",
    changes: ["Deleted: AuthPolicy openshift-ingress/gateway-auth-policy (targets maas-default-gateway; not managed by maas-controller)", "Deleted: TokenRateLimitPolicy openshift-ingress/gateway-tier-rate-limits (targets maas-default-gateway; not managed by maas-controller)"] }],
};

/**
 * The platform's parts by state, in setup order (pkg/cluster
 * GetMaaSStatus): "none" (not set up), "starting", "ready", "foreign"
 * (someone else's). rhcl: "1.4.3" (installed by someone, below the docs'
 * minimum), "1.5.0" (installed), or "missing" (a cluster that never had
 * MaaS: nothing of Connectivity Link, Kuadrant or CloudNativePG).
 */
export function maasParts(state, { tls = true, rhcl = "1.4.3", tierPolicies = false, cnpg = true } = {}) {
  const tool = state !== "foreign";
  const ready = state === "ready" || state === "foreign";
  const on = state !== "none";
  const fresh = rhcl === "missing";
  const parts = [
    fresh ? { id: "rhcl", title: "Red Hat Connectivity Link", docUrl: `${MAAS_DOCS}install/platform-setup.md#install-gateway-api-controller`, state: "missing",
      message: "Not installed. Set up installs version 1.5.0 from redhat-operators (channel stable-1.5), as the MaaS docs' platform setup does.", commands: RHCL_INSTALL_COMMANDS }
      : { id: "rhcl", title: "Red Hat Connectivity Link", docUrl: `${MAAS_DOCS}install/platform-setup.md#install-gateway-api-controller`, state: "ready", notManaged: true,
        message: `Version ${rhcl} runs (Subscription kuadrant-system/kuadrant-operator, channel ${rhcl === "1.4.3" ? "stable" : "stable-1.5"})`,
        ...(rhcl === "1.4.3" ? { warning: RHCL_WARNING, actions: [RHCL_UPGRADE], commands: RHCL_UPGRADE_COMMANDS } : {}) },
    fresh ? { id: "kuadrant", title: "Connectivity Link instance (Kuadrant)", docUrl: `${MAAS_DOCS}install/platform-setup.md#install-gateway-api-controller`, state: "missing",
      message: "No Kuadrant CR. Set up creates kuadrant-system/kuadrant once Connectivity Link runs, as the docs' platform setup does.", commands: "oc apply -f - <<'EOF'\napiVersion: kuadrant.io/v1beta1\nkind: Kuadrant\nmetadata:\n  name: kuadrant\n  namespace: kuadrant-system\nEOF" }
      : { id: "kuadrant", title: "Connectivity Link instance (Kuadrant)", docUrl: `${MAAS_DOCS}install/platform-setup.md#install-gateway-api-controller`, state: "ready", message: "kuadrant-system/kuadrant is ready", notManaged: true },
    !cnpg || fresh ? { id: "cnpg", title: "CloudNativePG operator", docUrl: `${MAAS_DOCS}install/maas-setup.md#database-setup`, state: "missing",
      message: "Not installed. Set up installs version 1.27.1 from certified-operators (channel stable-v1) for this tool's database. Or a cluster-admin creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md).",
      commands: "oc apply -f - <<'EOF'\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: cnpg-system\n---\napiVersion: operators.coreos.com/v1\nkind: OperatorGroup\nmetadata:\n  name: cloudnative-pg\n  namespace: cnpg-system\n---\napiVersion: operators.coreos.com/v1alpha1\nkind: Subscription\nmetadata:\n  name: cloudnative-pg\n  namespace: cnpg-system\nspec:\n  channel: stable-v1\n  installPlanApproval: Automatic\n  name: cloudnative-pg\n  source: certified-operators\n  sourceNamespace: openshift-marketplace\nEOF" }
      : { id: "cnpg", title: "CloudNativePG operator", docUrl: `${MAAS_DOCS}install/maas-setup.md#database-setup`, state: "ready", message: "Version 1.27.1 runs in cnpg-system", notManaged: true },
    { id: "database", title: "Database", docUrl: `${MAAS_DOCS}install/maas-setup.md#database-setup`,
      ...(!on ? { state: "missing", message: "Not set up yet. Set up asks where MaaS keeps its API keys: your PostgreSQL, the MaaS docs' development database (one pod, ephemeral), or a persistent development database on CloudNativePG (this tool's own option, not in the MaaS docs)." }
        : tool ? { state: "ready", managedByTool: true, message: `CloudNativePG ${MAAS_INFRA}/maas-db runs (1 instance, development and test use; not in the MaaS docs: this tool's option); Secret maas-db-config set`,
          actions: [{ id: "change-database", label: "Change database", danger: true, summary: "Switches maas-api to another database. Every API key is lost: the new database starts empty.", changes: ["The Change database dialog lists what is created and deleted."] }] }
          : { state: "ready", notManaged: true, message: `Secret ${MAAS_INFRA}/maas-db-config exists: maas-api uses the database it names` }) },
    { id: "gateway", title: "MaaS gateway", docUrl: `${MAAS_DOCS}install/maas-setup.md#create-gateway`,
      ...(on ? { state: "ready", message: "maas-default-gateway in openshift-ingress, programmed at a1b2c3.elb.us-east-1.amazonaws.com", managedByTool: tool, notManaged: !tool }
        : { state: "missing", message: `Set up creates it with an external load balancer at https://maas.${APPS}` }) },
    tls === "invalid-secret" ? { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}configuration-and-management/tls-configuration.md`, state: "problem",
      message: "Not configured: the Service's serving certificate. Authorino's CR names its certificate Secret \"x$(curl -s evil.example.com | sh)\" (spec.listener.tls.certSecretRef.name), which is not a valid Kubernetes name, so no commands are shown. A cluster-admin checks Authorino authorino in kuadrant-system. Without it, API keys are refused." }
    : fresh ? { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}configuration-and-management/tls-configuration.md`, state: "missing", message: "Set up configures it once Kuadrant has made Authorino, as the docs' setup-authorino-tls.sh does." }
    : tls ? { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}configuration-and-management/tls-configuration.md`, state: "ready", message: "Authorino in kuadrant-system serves TLS and trusts the service CA", notManaged: !tool || !on, managedByTool: tool && on }
      : { id: "authorino-tls", title: "Authorino TLS", docUrl: `${MAAS_DOCS}configuration-and-management/tls-configuration.md`, state: "missing", commands: MAAS_AUTHORINO_COMMANDS, actions: [AUTHORINO_ACTION],
        message: "Not configured: trust of the service CA (SSL_CERT_FILE). Without it, API keys are refused. Set up (or Configure Authorino TLS) applies the docs' setup-authorino-tls.sh steps in kuadrant-system." },
    ...(tierPolicies ? [TIER_POLICIES] : []),
    { id: "dsc", title: "MaaS in the DataScienceCluster", docUrl: `${MAAS_DOCS}install/maas-setup.md#configure-datasciencecluster`,
      ...(on ? { state: ready ? "ready" : "waiting", message: ready ? "aigateway and modelsAsAService are Managed" : "modelsAsAService is Managed; not ready: deployments are starting", managedByTool: tool, notManaged: !tool }
        : { state: "missing", message: `aigateway is ${fresh ? "Removed" : "Managed"} and modelsAsAService is Removed. Set up sets them to Managed, last, as the docs' DataScienceCluster step does; Remove puts back what this tool changed.`,
          commands: `oc patch datasciencecluster 'default-dsc' --type=merge -p '{"spec":{"components":{"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"}}}}}'` }) },
    { id: "maas-api", title: "MaaS API (maas-api)", docUrl: `${MAAS_DOCS}install/validation.md`,
      ...(!on ? { state: "missing", message: "The operator creates it once MaaS is turned on" } : ready ? { state: "ready", message: `Available in ${MAAS_INFRA}` } : { state: "waiting", message: "Not available yet (it restarts until the database answers)" }) },
    { id: "tenant", title: "Default tenant and gateway authentication", docUrl: `${MAAS_DOCS}install/maas-setup.md#maastenantconfig-cr`,
      ...(!on ? { state: "missing", message: "The operator creates it once MaaS is turned on" } : ready ? { state: "ready", message: "Tenant models-as-a-service is Ready; the gateway's authentication is enforced" } : { state: "waiting", message: "Waiting for the operator (tenant missing, configuration missing, authentication missing)" }) },
  ];
  // CloudNativePG is a part only once it is the database (pkg/cluster
  // maasDatabaseStatus); before Set up it is one of the database choices.
  return on ? parts : parts.filter((p) => p.id !== "cnpg");
}

/** The database choice (pkg/cluster maasDatabaseStatus): none yet, or this tool's CloudNativePG one, or someone else's. */
export function maasDatabase(state, { fresh = false, current: kind } = {}) {
  const choices = [
    { id: "external", plan: [`Write Secret ${MAAS_INFRA}/maas-db-config (label app=maas-api) with the connection URL you enter; nothing else is created, and your database is never changed`] },
    { id: "development", plan: [`Run the MaaS docs' development PostgreSQL (scripts/setup-database.sh) in ${MAAS_INFRA}: Secret postgres-creds (a generated password), Deployment postgres (one pod, image registry.redhat.io/rhel9/postgresql-16@sha256:${"5".repeat(64)}, from the RHOAI operator's CSV (postgresql_16_image); no volume, so its data is lost when the pod restarts) and Service postgres, then Secret maas-db-config with its address (sslmode=disable, as the script does)`] },
    { id: "cnpg", plan: [
      ...(fresh ? ["Install CloudNativePG 1.27.1: Namespace cnpg-system, OperatorGroup cnpg-system/cloudnative-pg, Subscription cnpg-system/cloudnative-pg (package cloudnative-pg, channel stable-v1, from certified-operators; in its own namespace, not openshift-operators, where an install can pull an OSSM upgrade that breaks the gateway; it stays after Clean up)"] : []),
      `Run a one-instance CloudNativePG database ${MAAS_INFRA}/maas-db (1 GiB volume, so keys survive restarts) and write Secret ${MAAS_INFRA}/maas-db-config with its address (sslmode=require)`] },
  ];
  const current = kind ?? (state === "none" ? "" : state === "foreign" ? "adopted" : "cnpg");
  return { current, namespace: MAAS_INFRA, default: "development", ...(current === "" ? { needed: true } : {}), ...(current === "adopted" ? {} : { choices }), ...(current === "adopted" ? { choices: [] } : {}) };
}

/**
 * The parts for a database of kind dbCurrent ("development" or "external";
 * otherwise the parts as they are): the Database part's message as the
 * backend words it, and no CloudNativePG part (shown only for its database).
 */
function maasDBParts(parts, dbCurrent) {
  if (dbCurrent !== "development" && dbCurrent !== "external") return parts;
  const message = dbCurrent === "development"
    ? `The MaaS docs' development database ${MAAS_INFRA}/postgres runs (one pod, no volume). Ephemeral: API keys are lost when its pod restarts, and maas-api then needs a restart to make its tables again.`
    : `Your PostgreSQL: Secret ${MAAS_INFRA}/maas-db-config, written by this tool, holds the connection URL you entered (never shown). Remove deletes only that Secret; your database is never touched.`;
  return parts.filter((p) => p.id !== "cnpg").map((p) => (p.id === "database" && p.state === "ready" ? { ...p, message } : p));
}

/** The groups that exist on the mock cluster (GET /api/resources/maas/groups). */
export const MAAS_GROUPS = ["ml-team"];

/** pkg/cluster maasGroupProblems: a missing group, with the commands when its name can be an OpenShift Group's. */
export function maasGroupProblems(names) {
  const openShiftName = (n) => n !== "." && n !== ".." && n !== "~" && !/[/%:]/.test(n);
  return names.filter((g) => !MAAS_GROUPS.includes(g)).map((g) => ({
    name: g, missing: true, message: `${g} is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.`,
    ...(openShiftName(g) ? { commands: `oc adm groups new ${g}\noc adm groups add-users ${g} <user>` } : {}),
  }));
}

/** A published model's limit (pkg/types MaaSLimit), by preset. */
export const maasLimit = (preset = "free") => ({
  free: { preset: "free", rates: [{ limit: 100, window: "1m" }] },
  premium: { preset: "premium", rates: [{ limit: 100000, window: "24h" }] },
  unlimited: { preset: "unlimited", unlimited: true },
  custom: { preset: "custom", rates: [{ limit: 500, window: "1m" }, { limit: 20000, window: "1h" }] },
})[preset];

/** What Set up would change, for the parts that are missing (pkg/cluster GetMaaSStatus setupPlan). */
function maasSetupPlan(parts, { fresh }) {
  const plan = [];
  const missing = (id) => parts.find((p) => p.id === id)?.state === "missing";
  if (missing("rhcl")) plan.push("Install Red Hat Connectivity Link 1.5.0: Namespace kuadrant-system, OperatorGroup kuadrant-system/kuadrant-operator-group, Subscription kuadrant-system/kuadrant-operator (package rhcl-operator, channel stable-1.5, from redhat-operators, with spec.config.env RATELIMIT_CHECK_SERVICE_FAILURE_MODE=deny and RATELIMIT_REPORT_SERVICE_FAILURE_MODE=deny, as the MaaS docs recommend, so requests are refused rather than let through unlimited while Limitador is unavailable)");
  if (missing("kuadrant")) plan.push("Create the Kuadrant CR kuadrant-system/kuadrant");
  // The database's own plan is its chosen option's (maasDatabase); only the namespace is Set up's.
  if (missing("database") && fresh) plan.push(`Create namespace ${MAAS_INFRA} (the AI gateway's infrastructure namespace, where the docs' setup-database.sh puts the database; recorded, and deleted by Remove when nothing else is in it)`);
  if (missing("gateway")) plan.push(`Create Gateway openshift-ingress/maas-default-gateway with an external load balancer at maas.${APPS} (a cloud cost while it exists), and GatewayClass openshift-default if the cluster has none`);
  if (missing("authorino-tls")) plan.push("Configure Authorino TLS as the docs' setup-authorino-tls.sh does: the serving-certificate annotation on Service authorino-authorino-authorization, TLS on the Authorino CR's listener, SSL_CERT_FILE on Deployment authorino, then restart it");
  if (missing("dsc")) plan.push(`DataScienceCluster default-dsc: ${fresh ? "aigateway.managementState Removed to Managed, " : ""}aigateway.modelsAsAService.managementState Removed to Managed (the values before are recorded, and Remove restores them)`);
  return plan;
}

/**
 * This tool's model in dev-project (qwen25 unless named): published or not.
 * maasOnlySetup: set up on the Models tab through MaaS only, so Unpublish
 * leaves it with no route (pkg/cluster MaaSModel.MaaSOnlySetup).
 */
export function maasModel(published, { ready = true, name = "qwen25-05b-instruct", maasOnlySetup = false } = {}) {
  return {
    namespace: "dev-project", name, displayName: displayOf(name), managedByTool: true, modelReady: true,
    deployed: published, ready: published && ready, published, message: published ? (ready ? "Published" : "Published; waiting") : "Not published",
    ...(published ? { phase: ready ? "Ready" : "Pending", endpoint: `https://maas.${APPS}/dev-project/${name}`, access: ready ? "Active" : "Pending", subscription: ready ? "Active" : "Pending" } : {}),
    ...(published && maasOnlySetup ? { maasOnlySetup: true } : {}),
  };
}

/** A publication by an earlier build: the model on the MaaS and the inference gateway (GetMaaSStatus). */
export function maasEarlierBuildPublication({ name = "qwen25-05b-instruct" } = {}) {
  return { ...maasModel(true, { name }), ready: false, partly: true,
    message: "Partly published: the model routes through openshift-ingress/maas-default-gateway and openshift-ingress/openshift-ai-inference, as an earlier build of this tool published it, so calls to its direct address fail with 500. Unpublish, then Publish again." };
}

/**
 * A model this build published (its refs recorded) that someone added the
 * inference gateway to since (GetMaaSStatus refsAddedStatus).
 */
export function maasSomeoneAddedPublication({ name = "qwen25-05b-instruct" } = {}) {
  return { ...maasModel(true, { name }), ready: false, partly: true,
    message: `Partly published: someone added openshift-ingress/openshift-ai-inference since the model was published, so direct calls to the model through that gateway fail with 500 once the rate limit applies to it. Unpublish and Publish refuse, since writing back the gateways the model had before would drop it. Set the gateways the model should use yourself, without maas-default-gateway (for KServe's default gateway: oc patch llminferenceservice ${name} -n dev-project --type=merge -p '{"spec":{"router":{"gateway":{"refs":null}}}}'), then Unpublish.` };
}

/** This tool's three models in dev-project (env-models-several) in MaaS's list: Qwen2.5 as given, Qwen3 as given, the embedding model never offered. */
export function maasSeveral(qwen25 = maasModel(true), qwen3 = maasModel(false, { name: "qwen3-1-7b" })) {
  return [maasEmbeddingModel(), qwen25, qwen3];
}

/**
 * Routes the Models tab's model in project through the MaaS gateway alone
 * (ModelState.maasPublished), or back: the inference gateway then no longer
 * lists it under Used by (pkg/cluster gatewayUsers). Other models of the
 * project keep their routes. maasOnlySetup: the Models tab set it up
 * through MaaS only (ModelState.maasOnlySetup); unpublished, such a model
 * has no route at all (reached only inside the cluster) and never goes on
 * the inference gateway.
 */
export function setMaaSPublished(resources, project, model, published, { maasOnlySetup = false } = {}) {
  const ref = `${project}/${model}`;
  let noRoute = false;
  const items = resources?.models?.items ?? [];
  for (const [i, m] of items.entries()) {
    if (m.namespace !== project || m.name !== model) continue;
    if (published) {
      // As pkg/cluster reports a published model: routed through the MaaS gateway alone, its address the MaaS route's, its check MaaS's own.
      const url = `https://maas.${APPS}/${project}/${model}`;
      items[i] = { ...routed({ ...m, url }, { exposure: "maas", check: undefined }), maasPublished: true, ...(maasOnlySetup ? { maasOnlySetup: true } : {}),
        callCheck: { result: "maas", ok: true, url, message: "MaaS lists the model (its reference is Ready). Calls go through MaaS with an API key, which this tool does not create: the MaaS tab's commands get one and call the model.", at: "2026-11-18T18:04:12Z" } };
      continue;
    }
    noRoute = !!m.maasOnlySetup;
    const { maasPublished: _p, maasOnlySetup: _o, ...rest } = m;
    items[i] = noRoute ? routed(rest, { exposure: "internal", check: undefined }) : routed({ ...rest, url: `${INFERENCE}/${project}/${model}` });
  }
  const gw = resources?.inferenceGateway;
  if (!gw?.deployed || !gw.managedByTool) return;
  const others = (gw.usedBy ?? []).filter((u) => u !== ref);
  const { usedBy: _list, teardownBlockedReason: _reason, ...rest } = gw;
  resources.inferenceGateway = { ...rest, ...usedBy(published || noRoute ? others : [...others, ref].sort()) };
}

/** This tool's embedding model in MaaS's list: never offered (pkg/cluster maasEmbeddingReason). */
export const maasEmbeddingModel = () => ({
  namespace: "dev-project", name: "granite-embedding-125m-english", displayName: displayOf("granite-embedding-125m-english"), managedByTool: true, modelReady: true,
  deployed: false, ready: false, published: false, message: "Not offered for MaaS",
  setupBlockedReason: "This tool publishes chat models only. Use the embedding model at its own address (the Models tab shows it), or add it to the Gen AI playground.",
});

/** A recorded publication whose model is gone (GetMaaSStatus's extra row). */
export function maasOrphanPublication(namespace) {
  return { namespace, name: "qwen25-05b-instruct", displayName: "qwen25-05b-instruct", deployed: true, ready: false, managedByTool: true, published: true, partly: true,
    message: `Partly published: no model of this name is in project ${namespace} any more (it was deleted there, or the project was); this tool's record of the publication still in place. Unpublish to finish removing it.` };
}

/** Removal refused because others use MaaS (pkg/cluster teardown, foreignMaaSObjects). */
export const MAAS_IN_USE = "Others use this MaaS platform: MaaSModelRef team-a/granite-3-8b, MaaSSubscription models-as-a-service/team-a-subscription. Removing it would break them. They unpublish and remove theirs first. Nothing was deleted.";

/** The MaaS status by state. */
export function maasWorld(state = "none", { tls = true, items = [], blocked = null, noCNPG = false, rhcl = state === "none" ? "1.4.3" : "1.5.0", tierPolicies = false, access = { everyone: true }, dbCurrent, noUnlimited = false } = {}) {
  const parts = maasDBParts(maasParts(state, { tls, rhcl, tierPolicies, cnpg: !noCNPG }), dbCurrent);
  const shared = ["rhcl", "kuadrant", "cnpg", "authorino-tls", "tier-policies"];
  const allReady = parts.every((p) => p.state === "ready");
  const tool = parts.some((p) => p.managedByTool && !shared.includes(p.id));
  const waiting = parts.filter((p) => p.state !== "ready" && p.state !== "missing").map((p) => p.title);
  const setupPlan = blocked ? [] : maasSetupPlan(parts, { fresh: rhcl === "missing" });
  const notReady = parts.find((p) => p.state !== "ready");
  return {
    ...(blocked ? { blocked } : {}),
    platform: {
      namespace: MAAS_INFRA, deployed: state !== "none", ready: allReady, managedByTool: tool,
      message: allReady ? "Ready" : state === "none" && waiting.length === 0 ? "Not set up" : `Needs attention: ${waiting.join(", ")}`,
      ...(blocked ? { setupBlockedReason: blocked.reason } : {}),
      ...(!tool ? { teardownBlockedReason: "No part of the MaaS platform was set up by this tool, so there is nothing for it to remove." } : {}),
    },
    parts, url: `https://maas.${APPS}`, subscription: "rhoai-nightly-updater-subscription", dashboardUrl: DASHBOARD_URL,
    ...(setupPlan.length ? { setupPlan } : {}),
    items: items.map((i) => (i.published || allReady || !notReady ? i : { ...i, setupBlockedReason: state === "none" && !tool ? "Set up the MaaS platform first." : `The MaaS platform is not ready: ${notReady.title} (see above).` }))
      .map((i) => (i.published && !i.partly && !i.limit ? { ...i, limit: maasLimit("free") } : i)),
    database: maasDatabase(state, { fresh: rhcl === "missing" || noCNPG, current: dbCurrent }),
    ...(noUnlimited ? { noUnlimited: true } : {}),
    ...(items.some((i) => i.published) ? { access } : {}),
  };
}

export const MAAS_BLOCKED = { component: "aigateway",
  reason: "MaaS needs RHOAI's DataScienceCluster, and this cluster has none. A cluster-admin creates one on the Components page." };

// --- Models routing (GET /api/resources/models/exposures, ModelState routing fields) ---

const GW_KEY = "openshift-ingress/openshift-ai-inference";

/** The ways a model can be reached from a project (pkg/cluster planExposures), for the gateway state given. */
export function modelExposures(project, model, gw, { maasReady = false, otherGateway = false, authReady = true } = {}) {
  const chat = MODEL_CATALOG.find((c) => c.name === model)?.type !== "embedding";
  const gwUsable = !!gw?.deployed && !!gw.ready;
  const listed = (gw?.allowedProjects ?? []).includes(project);
  const address = `${INFERENCE}/${project}/${model}`;
  const def = {
    key: "default-gateway", exposure: "default-gateway", title: "Through the cluster's default inference gateway",
    description: `Reachable from outside the cluster at ${gwUsable ? address : `https://inference.<apps domain>/${project}/${model}`}. Each call needs an OpenShift token that can see ${project}.`,
    available: gwUsable, address: gwUsable ? address : undefined,
    reason: gwUsable ? undefined : "The inference gateway isn't set up yet. Set it up below, or choose another way.",
    fix: gwUsable ? undefined : "inference-gateway",
    changes: gwUsable && gw.managedByTool && !listed ? [`Gateway ${GW_KEY} (this tool's): ${project} is added to the projects it allows. Anyone who can edit ${project} can then attach routes to this shared gateway.`] : undefined,
    needsAuth: gwUsable && !authReady ? true : undefined,
  };
  const maas = {
    key: "maas", exposure: "maas", gatewayRef: "openshift-ingress/maas-default-gateway", title: "Through MaaS only",
    description: "Published to Models as a Service: anyone signed in to the cluster can call it with a MaaS API key, within their subscription's token limit. The dashboard's playground lists it as a MaaS model.",
    available: chat && maasReady, address: maasReady ? `https://maas.${APPS}` : undefined,
    reason: !chat ? "Only chat models are published to MaaS." : maasReady ? undefined : "MaaS isn't set up yet (Red Hat Connectivity Link is missing).",
    fix: chat && !maasReady ? "maas-tab" : undefined,
    changes: chat && maasReady ? [`Gateway openshift-ingress/maas-default-gateway: admits routes from ${project}.`, `Published to MaaS: MaaSModelRef ${model} in ${project}, and this tool's subscription and authorization policy list the model.`] : undefined,
  };
  // Outside openshift-ingress: offered, never recommended (its token checks cannot be read).
  const other = otherGateway ? [{
    key: "gateway:team-gateways/shared-llm", exposure: "gateway", gatewayRef: "team-gateways/shared-llm", title: "Through another gateway: team-gateways/shared-llm",
    description: "Uses a gateway someone else runs; this tool doesn't change it. RHOAI adds token checks.",
    available: true, address: `https://shared-llm.${APPS}/${project}/${model}`,
  }] : [];
  const internal = {
    key: "internal", exposure: "internal", title: "Only inside the cluster", available: true,
    description: "No outside address. Programs on this cluster (for example a workbench) can call it without a token.",
    warning: "Calls to the in-cluster address are not authenticated: any pod on the cluster whose network can reach the project can use the model.",
  };
  const options = [def, maas, ...other, internal];
  const chosen = def.available ? def : maas.available ? maas : undefined;
  return {
    project, model, kserveDefaultGateway: GW_KEY, authReady, options,
    default: chosen?.key ?? "",
    defaultReason: chosen === def ? "works from your laptop with an OpenShift token, the way the dashboard deploys models."
      : chosen === maas ? "MaaS is set up and the inference gateway can't be used: callers use a MaaS API key." : undefined,
    recommendGateway: !def.available,
    recommendGatewayReason: def.available ? undefined : "Set up the inference gateway to reach models from outside the cluster with an OpenShift token, as the dashboard does.",
  };
}

/** A model's routing fields (pkg/cluster addModelRouting), with a "Can it be called?" result. */
export const routed = (m, { exposure = "default-gateway", check = "ok" } = {}) => ({
  ...m, exposure, servedName: m.name,
  gateway: exposure === "internal" ? undefined : exposure === "maas" ? "openshift-ingress/maas-default-gateway" : GW_KEY,
  gatewayAuth: exposure === "internal" ? undefined : exposure === "maas" ? "maas" : "enforced",
  // As RHOAI 3.6 reports it (live): the model's path on its gateway's in-cluster Service; with no route, its own Service on port 8000.
  internalUrl: exposure === "internal" ? `https://${m.name}-kserve-workload-svc.${m.namespace}.svc.cluster.local:8000`
    : exposure === "maas" ? `https://maas-default-gateway-openshift-default.openshift-ingress.svc.cluster.local/${m.namespace}/${m.name}`
      : `https://openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local/${m.namespace}/${m.name}`,
  url: exposure === "internal" ? undefined : m.url,
  callCheck: check === "ok" ? { result: "ok", ok: true, status: 200, message: exposure === "internal" ? `A chat request for "${m.name}" was answered (HTTP 200).` : `A chat request for "${m.name}" was answered (HTTP 200). A request without a token was refused (HTTP 401).`, url: `${m.url}/v1/chat/completions`, at: "2026-11-18T18:04:12Z" }
    : check === "unauthenticated" ? { result: "unauthenticated", ok: false, status: 200, message: `The model also answered a request without a token (HTTP 200): gateway ${GW_KEY} does not check who calls, so anyone who can reach ${m.url} can use the model. Check that RHOAI's AuthPolicy on the gateway is enforced (odh-model-controller and Red Hat Connectivity Link run), and that the gateway is not labelled opendatahub.io/managed=false.`, url: `${m.url}/v1/chat/completions`, at: "2026-11-18T18:04:12Z" }
    : check === "model-name" ? { result: "model-name", ok: false, status: 404, message: `The model's server answered 404: it does not serve a model named "Qwen/Qwen2.5-0.5B-Instruct". A request must send "model": "${m.name}", the name the model was set up with (not its display name or its Hugging Face id).`, at: "2026-11-18T18:04:12Z" }
      : check === "route" ? { result: "route", ok: false, status: 404, message: `No answer at ${m.url}/v1/chat/completions (404, empty body): the gateway didn't route the request to the model. Right after setup the gateway can take a minute to load the route; check again shortly.`, at: "2026-11-18T18:04:12Z" }
        : undefined,
});

/** GET /api/resources/prerequisites: every feature ready, or Models missing what it needs. */
export function prerequisites({ models = "ready", pipelines = "ready", exposure = "" } = {}) {
  const readyPart = (id, title, message) => ({ id, title, state: "ready", message, notManaged: true });
  const ready = (parts) => ({ ready: true, parts });
  const missingModels = {
    ready: false,
    parts: [
      { id: "dsc-kserve", title: "Model serving (KServe)", state: "missing", message: "Removed in DataScienceCluster default-dsc. Set up sets it to Managed; Clean up can turn it back.", commands: `oc patch datasciencecluster default-dsc --type=merge -p '{"spec":{"components":{"kserve":{"managementState":"Managed"}}}}'` },
      { id: "cert-manager", title: "cert-manager Operator for Red Hat OpenShift", state: "missing", message: "Not installed. Set up installs version 1.20.1 from redhat-operators (channel stable-v1).", commands: "oc apply -f - <<'EOF'\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: cert-manager-operator\nEOF" },
      ...(models === "missing-routing" ? [
        { id: "rhcl", title: "Red Hat Connectivity Link", state: "missing", message: "Not installed. Set up installs version 1.4.4 from redhat-operators (channel stable)." },
        { id: "kuadrant", title: "Connectivity Link instance (Kuadrant)", state: "missing", message: "Not created yet. Set up creates it once Connectivity Link serves its API; it starts Authorino, which checks each call's token." },
      ] : []),
      { id: "dependencies", title: "KServe's dependency check", state: "waiting", message: "Waits for cert-manager: KServe reports it isn't installed. Set up installs it, then waits until KServe sees it." },
    ],
    setupPlan: [
      "DataScienceCluster default-dsc: set kserve to Managed (it is Removed; Clean up can turn it back while nothing else uses it)",
      "Install cert-manager Operator for Red Hat OpenShift 1.20.1: Namespace cert-manager-operator, OperatorGroup cert-manager-operator, Subscription cert-manager-operator/openshift-cert-manager-operator (package openshift-cert-manager-operator, channel stable-v1, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
      ...(models === "missing-routing" ? [
        "Install Red Hat Connectivity Link 1.4.4: Namespace kuadrant-system, OperatorGroup kuadrant-operator-group, Subscription kuadrant-system/kuadrant-operator (package rhcl-operator, channel stable, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
        "Create the Kuadrant CR kuadrant-system/kuadrant (as the MaaS docs do); it stays after Clean up, as other features share it",
      ] : []),
    ],
  };
  const missingPipelines = {
    ready: false,
    parts: [{ id: "dsc-aipipelines", title: "Data Science Pipelines (aipipelines)", state: "missing", message: "Removed in DataScienceCluster default-dsc. Set up sets it to Managed; Clean up can turn it back.", commands: `oc patch datasciencecluster default-dsc --type=merge -p '{"spec":{"components":{"aipipelines":{"managementState":"Managed"}}}}'` }],
    setupPlan: ["DataScienceCluster default-dsc: set aipipelines to Managed (it is Removed; Clean up can turn it back while nothing else uses it)"],
  };
  return {
    modelsExposure: exposure,
    features: {
      pipelines: pipelines === "missing" ? missingPipelines : ready([readyPart("dsc-aipipelines", "Data Science Pipelines (aipipelines)", "Managed in DataScienceCluster default-dsc")]),
      mlflow: ready([readyPart("dsc-mlflowoperator", "MLflow (mlflowoperator)", "Managed in DataScienceCluster default-dsc")]),
      models: models === "ready" ? ready([readyPart("dsc-kserve", "Model serving (KServe)", "Managed in DataScienceCluster default-dsc"), readyPart("cert-manager", "cert-manager Operator for Red Hat OpenShift", "Version 1.20.1 runs in cert-manager-operator")]) : missingModels,
      playground: ready([readyPart("dsc-ogx", "Gen AI playground (ogx)", "Managed in DataScienceCluster default-dsc")]),
    },
  };
}

// --- The Components page's DataScienceCluster manager (GET /api/components/dsc) ---
// The tree of a RHOAI 3.6 cluster's CRD (v3), with the live DSC's values and
// the release default of its CSV's alm-examples (pkg/cluster/testdata/dsc).
const MR = ["Managed", "Removed"];
const dscTree = [
  // [path, spec (null: not set; undefined: a group), release default, CRD default, allowed, actual, condition]
  ["aigateway", "Managed", "Removed", "", MR, "Managed", ["AIGatewayReady", "True"]],
  ["aigateway.aiGrid", null, null, "", MR, "Removed", ["AIGridReady", "False", "Removed"]],
  ["aigateway.batchGateway", "Removed", "Removed", "", MR, "Removed", ["BatchGatewayReady", "False", "Removed"]],
  ["aigateway.modelsAsAService", "Managed", "Removed", "Removed", MR, "Managed", ["ModelsAsAServiceReady", "True"]],
  ["aiHub", "Managed", "Managed", "", MR, "Managed", ["AIHubReady", "True"]],
  ["aipipelines", "Managed", "Managed", "", MR, "Managed", ["AIPipelinesReady", "True"]],
  ["aipipelines.argoWorkflowsControllers", null, null, "Managed", MR, "", null],
  ["dashboard", undefined, undefined, "", null, "Managed", ["DashboardReady", "True"]],
  ["dashboard.maasPortal", "Removed", "Removed", "", MR, "Removed", ["MaaSPortalAvailable", "False", "Removed"]],
  ["dashboard.standard", "Managed", "Managed", "", MR, "", null],
  ["data", undefined, undefined, "", null, "Managed", ["DataReady", "True"]],
  ["data.dataRegistry", null, null, "", MR, "", null],
  ["data.featureStore", "Managed", "Managed", "", MR, "", null],
  ["databaseservice", "Removed", "Removed", "", MR, "Removed", ["DatabaseServiceReady", "False", "Removed"]],
  ["kserve", "Managed", "Managed", "", MR, "Managed", ["KserveReady", "True"]],
  ["kserve.modelCache", null, null, "Removed", MR, "", null],
  ["kserve.modelExpress", "Removed", "Removed", "Removed", MR, "", null],
  ["kserve.nim", "Managed", "Managed", "Managed", MR, "", null],
  ["kueue", "Removed", "Removed", "", ["Managed", "Unmanaged", "Removed"], "Removed", ["KueueReady", "False", "Removed"]],
  ["mcplifecycleoperator", "Managed", "Managed", "", MR, "Managed", ["MCPLifecycleOperatorReady", "True"]],
  ["mlflowoperator", "Managed", "Managed", "", MR, "Managed", ["MLflowOperatorReady", "True"]],
  ["ogx", "Managed", "Managed", "", MR, "Managed", ["OGXReady", "True"]],
  ["ray", "Managed", "Managed", "", MR, "Managed", ["RayReady", "True"]],
  ["sparkoperator", "Removed", "Removed", "", MR, "Removed", ["SparkOperatorReady", "False", "Removed"]],
  ["trainer", "Managed", "Managed", "", MR, "Managed", ["TrainerReady", "True"]],
  ["trustyai", "Managed", "Managed", "", MR, "Managed", ["TrustyAIReady", "True"]],
  ["workbenches", "Managed", "Managed", "", MR, "Managed", ["WorkbenchesReady", "True"]],
  ["workbenches.workbenchesV2", "Removed", "Removed", "Removed", MR, "Removed", ["WorkbenchesV2Ready", "False", "Removed"]],
];

/**
 * The DSC manager. overrides maps a path to its value in the DSC (null: not
 * set). Mismatches are worked out as the backend does: a toggle the default
 * sets and the DSC leaves unset, and Managed under a parent that is not.
 * unknown maps the paths of fields the CRD does not define to their value
 * in the DSC (a row each, under the nearest node above it); noDefault
 * leaves the release default out (no DataScienceCluster example in the CSV).
 */
export function dscManager(overrides = {}, { unknown = {}, noDefault = false } = {}) {
  const nodes = dscTree.map(([path, s, def, schemaDefault, allowed, actual, cond]) => {
    const parts = path.split(".");
    const toggle = s !== undefined;
    const value = toggle ? (path in overrides ? overrides[path] : s) : null;
    // A component set to Removed is reported as Removed once reconciled.
    if (toggle && value !== "Managed" && actual === "Managed") {
      actual = "Removed";
      cond = cond && [cond[0], "False", "Removed"];
    }
    if (noDefault) def = null;
    const n = {
      path, name: parts[parts.length - 1], parent: parts.slice(0, -1).join("."), depth: parts.length - 1, toggle,
      specSet: value !== null && value !== undefined, defaultSet: def !== null && def !== undefined,
      ...(allowed ? { allowed } : {}), ...(schemaDefault ? { schemaDefault } : {}),
      ...(value ? { spec: value } : {}), ...(def ? { default: def } : {}), ...(actual ? { actual } : {}),
      ...(cond ? { condition: { type: cond[0], status: cond[1], ...(cond[2] ? { reason: cond[2], message: `ManagementState is set to ${cond[2]}` } : {}) } } : {}),
    };
    if (n.specSet && !noDefault) n.changed = n.spec !== (n.defaultSet ? n.default : schemaDefault || "Removed");
    return n;
  });
  const byPath = Object.fromEntries(nodes.map((n) => [n.path, n]));
  const mismatches = [];
  for (const n of nodes) {
    if (!n.toggle) continue;
    if (!n.specSet && n.defaultSet) {
      mismatches.push({ kind: "unset", path: n.path, default: n.default, fixable: true, message: n.default === "Removed"
        ? "Not set in the DataScienceCluster, so the operator treats it as Removed, which is also the release default. Fix sets it explicitly."
        : `Not set in the DataScienceCluster, so the operator treats it as Removed, but the release default is ${n.default}. Fix sets it to ${n.default}.` });
    }
    const parent = byPath[n.parent];
    if (n.spec === "Managed" && parent?.toggle && parent.spec !== "Managed") {
      mismatches.push({ kind: "parent-removed", path: n.path, parent: parent.path, fixable: false,
        message: `Managed, but ${parent.path} is ${parent.spec || "not set"}, so the operator deploys nothing for it. Set ${parent.path} to Managed, or set this to Removed.` });
    }
  }
  // Fields the CRD does not define: mismatches last, sorted, and a row each
  // after the rows of the nearest node above it (pkg/cluster withUnknownNodes).
  for (const path of Object.keys(unknown).sort()) {
    mismatches.push({ kind: "unknown-field", path, fixable: true, message: "The installed CRD does not define this field, so the operator never sees it. Fix removes it." });
    const parts = path.split(".");
    let parent;
    for (let i = parts.length - 1; i >= 1 && !parent; i--) parent = byPath[parts.slice(0, i).join(".")];
    const row = { path, name: parent ? path.slice(parent.path.length + 1) : path, parent: parent?.path ?? "", depth: parent ? parent.depth + 1 : 0, toggle: false, specSet: false, defaultSet: false, unknown: true };
    let at = nodes.length;
    if (parent) {
      at = nodes.indexOf(parent) + 1;
      while (at < nodes.length && nodes[at].path.startsWith(`${parent.path}.`)) at++;
    }
    nodes.splice(at, 0, row);
  }
  const components = nodes.filter((n) => n.depth === 0 && !n.unknown).map((n) => n.path);
  // A component is in the DSC when it or anything below it is set.
  const inDSC = components.filter((c) => nodes.some((n) => (n.path === c || n.path.startsWith(`${c}.`)) && n.specSet));
  const byName = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  // The unknown fields' values, for dscPlan's Fix; not part of the answer (not enumerable, so not sent).
  return Object.defineProperty({
    exists: true, state: "present", name: "default-dsc", uid: "ba54ddbf-ad4a-4257-9ab4-977eb541dc1c", apiVersion: "datasciencecluster.opendatahub.io/v3", storageVersion: "v3",
    dscCount: 1, phase: "Ready", releaseName: "OpenShift AI Self-Managed", releaseVersion: "3.6.0", operatorVersion: "3.6.0",
    ...(noDefault
      ? { defaultsError: "The installed operator 3.6.0 ships no DataScienceCluster v3 example in its CSV's alm-examples, so there is no release default to compare with." }
      : { defaultsSource: "alm-examples of the installed CSV rhods-operator.3.6.0" }),
    conditions: [{ type: "Ready", status: "True" }, { type: "ProvisioningSucceeded", status: "True" }, { type: "ComponentsReady", status: "True" }, { type: "ModulesReady", status: "True" }],
    nodes, mismatches,
    comparison: {
      defaultsRead: !noDefault, schemaRead: true, ...(noDefault ? {} : { defaultsCSV: "rhods-operator.3.6.0" }),
      crdComponents: [...components].sort(byName),
      defaultComponents: noDefault ? [] : [...components].sort(byName),
      dscComponents: [...inDSC, ...Object.keys(unknown).filter((p) => !p.includes("."))].sort(byName),
    },
  }, "unknownValues", { value: Object.fromEntries(Object.entries(unknown).map(([p, v]) => [p, typeof v === "string" ? v : JSON.stringify(v)])) });
}

// The plan's binding (a hash in the real backend); the mock never checks it.
const DSC_PLAN_BINDING = "0".repeat(24);

// The mock cluster's MaaS is in use (a published model, the tool's
// subscription and access policy), as on the live cluster the guard was
// written for (dsc_maas_guard.go).
const DSC_MAAS_IN_USE = "MaaS is in use (published model dev-project/qwen25-05b-instruct; subscription models-as-a-service/rhoai-nightly-updater-subscription; access policy models-as-a-service/rhoai-nightly-updater-access; set up by this tool on the MaaS tab).";

/** What a change would do (GET /api/components/dsc/plan), for the mock's manager m. */
export function dscPlan(m, action, path, to, withParent, keepEnabled = false) {
  const base = { action, name: m.name, uid: m.uid, apiVersion: m.apiVersion, token: DSC_PLAN_BINDING, changes: [] };
  const node = m.nodes.find((n) => n.path === path);
  const change = (n, value) => ({ path: n.path, from: n.spec, fromSet: n.specSet, to: value, toSet: true, toggle: true,
    effect: value === "Managed" && n.spec !== "Managed" ? "deploys" : value !== "Managed" && n.spec === "Managed" ? "removes" : "" });
  const maasBlock = (p) => ({ component: p, reasons: [`${DSC_MAAS_IN_USE} Removing ${p} takes MaaS away: maas-api and the gateway's policies go, so every published model and every API key stops working. Remove MaaS on the MaaS tab first (Clean up MaaS unpublishes and removes it in order), or keep ${p} Managed`] });
  if (action === "toggle" && node) {
    const parent = m.nodes.find((n) => n.path === node.parent);
    if (to === "Managed" && parent?.toggle && parent.spec !== "Managed") {
      if (!withParent) return { ...base, refusal: `${parent.path} is ${parent.spec || "not set"}, so setting ${path} to Managed would deploy nothing. Set ${parent.path} to Managed in the same change, or set it first.`, code: "parent_removed", parentPath: parent.path };
      return { ...base, changes: [change(parent, "Managed"), change(node, "Managed")] };
    }
    const changes = [change(node, to)];
    if (to !== "Managed") {
      for (const c of m.nodes.filter((n) => n.parent === path && n.spec === "Managed")) changes.push({ path: c.path, from: "Managed", fromSet: true, to: "Managed", toSet: true, toggle: true, effect: "removes" });
    }
    const takesMaaS = to !== "Managed" && node.spec === "Managed" && ["aigateway", "aigateway.modelsAsAService"].includes(path);
    return { ...base, changes, ...(takesMaaS ? { blocked: [maasBlock(path)] } : {}) };
  }
  if (action === "fix") {
    return { ...base, changes: [
      ...m.mismatches.filter((x) => x.kind === "unset").map((x) => ({ path: x.path, fromSet: false, to: x.default, toSet: true, toggle: true, effect: x.default === "Managed" ? "deploys" : "" })),
      // A removed field, as the backend's diffFields lists it (its value as JSON).
      ...m.mismatches.filter((x) => x.kind === "unknown-field").map((x) => ({ path: x.path, from: m.unknownValues?.[x.path] ?? "", fromSet: true, toSet: false, toggle: false })),
    ] };
  }
  // Resets: what the user turned on (Managed or Unmanaged and deployed,
  // off in the release default) is kept with keepEnabled.
  const byPath = Object.fromEntries(m.nodes.map((n) => [n.path, n]));
  const deployed = (n) => (n.spec === "Managed" || n.spec === "Unmanaged") && (!byPath[n.parent]?.toggle || byPath[n.parent].spec === "Managed");
  const turnedOn = m.nodes.filter((n) => n.toggle && n.specSet && deployed(n) && (n.default || n.schemaDefault || "Removed") === "Removed").map((n) => n.path);
  const kept = keepEnabled ? turnedOn : [];
  const differing = m.nodes.filter((n) => n.toggle && n.defaultSet && n.spec !== n.default && !kept.includes(n.path)).map((n) => change(n, n.default));
  const takesMaaS = differing.some((c) => c.path === "aigateway" && c.effect === "removes");
  const reset = { ...base, keepEnabled, turnedOn, kept, ...(takesMaaS ? { blocked: [maasBlock("aigateway")] } : {}) };
  if (action === "soft-reset" && differing.length === 0 && kept.length > 0) {
    return { ...reset, refusal: `The DataScienceCluster already matches the release default, apart from the components you turned on, which the reset keeps (${kept.join(", ")}). Nothing to change.`, code: "nothing_to_do" };
  }
  if (action === "soft-reset") return { ...reset, changes: differing };
  return {
    ...reset, changes: differing,
    tornDown: m.nodes.filter((n) => n.depth === 0 && (n.toggle ? n.spec === "Managed" : n.actual === "Managed")).map((n) => n.path),
    stayRemoved: differing.filter((c) => c.effect === "removes").map((c) => c.path),
    warnings: ["Until aipipelines is back, these stop working: pipeline server dev-project/dspa.",
      ...(kept.includes("aigateway") ? ["While aigateway is down, MaaS does not serve (published model dev-project/qwen25-05b-instruct): every published model and every API key stops working until the new DataScienceCluster brings it back."] : [])],
    newObject: `apiVersion: datasciencecluster.opendatahub.io/v3\nkind: DataScienceCluster\nmetadata:\n  labels:\n    app.kubernetes.io/name: datasciencecluster\n  name: default-dsc\nspec:\n  components:\n    aiHub:\n      instancesNamespace: rhoai-model-registries\n      managementState: Managed\n    aigateway:\n      managementState: ${kept.includes("aigateway") ? "Managed" : "Removed"}\n    ...\n`,
  };
}
