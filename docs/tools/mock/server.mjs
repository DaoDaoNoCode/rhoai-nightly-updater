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
//   POST /__docs/job?name=X&out=DIR[&contrast=M], GET /__docs/job   the capture job to run
//                                 (M: the contrast mode, glass unless named)
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { dirname, extname, join, normalize, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import * as F from "./fixtures.mjs";
import { baseWorld, reads, scenarios } from "./scenarios.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const args = Object.fromEntries(
  process.argv.slice(2).flatMap((a, i, all) => (a.startsWith("--") ? [[a.slice(2), all[i + 1]]] : [])),
);
const PORT = Number(args.port || process.env.DOCS_MOCK_PORT || 18181);
const DIST = resolve(args.dist || join(here, "..", "..", "..", "frontend", "dist"));

let scenario = args.scenario || process.env.DOCS_SCENARIO || "healthy";
let world;
let job = { name: "", out: "", contrast: "" };
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
    message: "S3 storage (SeaweedFS) deployed with bucket 'pipelines'; pipeline servers reach it at http://seaweedfs-s3.seaweedfs.svc:8333. Sign in to the admin UI as 'admin'; the password (the S3 secret key) and the S3 keys are in Secret 'seaweedfs-s3-credentials' in namespace seaweedfs.",
    logs: ["OK: Namespace created", "OK: PVC created", "OK: Secret created", "OK: NetworkPolicy created", "OK: Deployment created", "OK: SeaweedFS is running", "OK: Service created", "OK: Admin UI Route created", "OK: Bucket 'pipelines' created"],
  };
};

/** Migrate storage: its steps wait for /__docs/advance in gated mode. */
// The one-click setup's run: the plan's steps, then the mock cluster
// changes as the backend's would.
async function nodeSetupStream(req, res) {
  const { action = "", replaceExisting = false } = await body(req);
  const plan = F.nodeSetupPlan(world, action, replaceExisting);
  beginOperation("node-setup", "One-time cluster setup", action);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  const steps = plan.ready ? F.nodeSetupSteps(plan) : [
    { step: "check", status: "failed", errorCode: plan.refusalCode, message: plan.refusal ?? plan.state },
    { step: "operation_complete", status: plan.nothingToDo ? "success" : "failed", errorCode: plan.refusalCode, message: plan.refusal ?? `Nothing to change: ${plan.state}` },
  ];
  let n = 0;
  for (const step of steps) {
    n += 1;
    if (step.status !== "running") await nextStep();
    if (res.destroyed) return;
    if (step.step === "operation_complete") {
      if (plan.ready) {
        if (action === "mirror-create") world.status.imageMirror = { exists: true, name: "rhoai-quay-mirror", source: "registry.redhat.io/rhoai" };
        if (action === "mirror-remove") world.status.imageMirror = { exists: false, name: "", source: "" };
        // The backend records the entry it wrote, and clears the record
        // when it removes it (pkg/cluster/internal/node/node_pull_record.go).
        const nodes = world.status.pullSecret.nodes;
        if (action === "pull-secret-add") {
          world.nodes = { pullEntry: "same" };
          if (nodes) nodes.addedByTool = F.NODE_PULL_RECORD;
        }
        if (action === "pull-secret-remove") {
          world.nodes = { pullEntry: "none" };
          if (nodes) delete nodes.addedByTool;
        }
      }
      endOperation(step.status === "success", step.message);
    }
    res.write(`data: ${JSON.stringify({ ...step, elapsedMs: n * 1500 })}\n\n`);
  }
  res.end();
}

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

/** Secure S3 storage: the checks, the rollout, one step per recorded pipeline server; each waits for /__docs/advance in gated mode. */
async function secureStream(req, res) {
  await body(req);
  beginOperation("secure-s3", "Secure S3 storage", "");
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  const servers = world.resources.s3Keys?.oldKey ?? [];
  const kept = world.resources.s3Keys?.keepOldKey ?? [];
  const keptNames = kept;
  const steps = [
    { step: "check", status: "success", message: `${servers.length === 1 ? "OK: Found 1 pipeline server to give a key of its own" : `OK: Found ${servers.length} pipeline servers to give each a key of its own`}; left on the old key: ${keptNames.join(", ")}` },
    { step: "rollout", status: "running", message: "Waiting up to 1m30s for SeaweedFS to become ready" },
    { step: "rollout", status: "success", message: "OK: The S3 storage runs with the new admin key; the old key no longer works" },
    ...servers.flatMap((n) => [
      { step: `server:${n}`, status: "running", message: "Checking the new key against the storage" },
      { step: `server:${n}`, status: "success", message: `OK: Pipeline server ${n} has the key of SeaweedFS user rnu-dspa-${n.split("/")[0]} in its Secrets; its API server started before the key was written, so it keeps the old key, which no longer works, until its pods restart`, detail: `oc delete pod -n ${n.split("/")[0]} -l app=ds-pipeline-${n.split("/")[1]},dspa=${n.split("/")[1]}` },
    ]),
  ];
  const done = `New admin key, kept out of every project. Given a key of their own: ${servers.join(", ")}.${kept.length ? ` Left on the old admin key, which no longer works: ${keptNames.join(", ")}. ${kept.length > 1 ? "Their owners must give those pipeline servers new S3 keys, or delete them and set them up again with this tool." : "Its owner must give that pipeline server new S3 keys, or delete it and set it up again with this tool."}` : ""}${servers.length ? ` The API server${servers.length > 1 ? "s" : ""} of ${servers.join(", ")} ${servers.length > 1 ? "keep" : "keeps"} using the old key, which no longer works, until ${servers.length > 1 ? "their" : "its"} pods restart: the pipeline API server reads its S3 key only when it starts. Delete the pods (the Deployment makes new ones): ${servers.map((n) => `oc delete pod -n ${n.split("/")[0]} -l app=ds-pipeline-${n.split("/")[1]},dspa=${n.split("/")[1]}`).join("; ")}.` : ""} Restart the workbenches and model deployments that use Connection rhoai-nightly-updater-s3, and Elyra pipelines in workbenches, so they pick up the new key.`;
  let n = 0;
  for (const step of [...steps, { step: "operation_complete", status: "success", message: done }]) {
    n += 1;
    await nextStep();
    if (res.destroyed) return;
    if (step.step === "operation_complete") {
      world.resources.s3Keys = { secured: true, needsSecuring: false, ownKey: servers, keepOldKey: kept };
      endOperation(true, done);
    }
    res.write(`data: ${JSON.stringify({ ...step, elapsedMs: n * 3000 })}\n\n`);
  }
  res.end();
}

/** Clean up: one step per part (pkg/cluster CleanUp); each waits for /__docs/advance in gated mode. */
async function cleanupStream(req, res) {
  const { pipelineServers = [], storage = false, deleteNamespace = false, mlflow = false, models = [], inferenceGateway = false, playgrounds = [], recipeProjects = [], components = [] } = await body(req);
  beginOperation("cleanup", "Clean up the environment", [pipelineServers.length ? `${pipelineServers.length} pipeline servers` : "", storage ? "S3 storage" : "", mlflow ? "MLflow" : ""].filter(Boolean).join(", "));
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  const parts = [
    ...pipelineServers.map((p) => ({ id: `pipeline-server:${p}`, title: `the pipeline server in '${p}'`, done: `Pipeline server removed from '${p}'.`, apply: () => { world.resources.pipelineServers = world.resources.pipelineServers.filter((ps) => ps.namespace !== p); } })),
    ...(storage ? [{ id: "storage", title: "the S3 storage", done: deleteNamespace ? "S3 storage removed. Namespace seaweedfs is being deleted." : "S3 storage removed.", apply: () => { world.resources.minio = F.s3States.none; delete world.resources.legacyStorage; } }] : []),
    ...(mlflow ? [{ id: "mlflow", title: "MLflow", done: "MLflow removed.", apply: () => { world.resources.mlflow = F.mlflowStates.none; } }] : []),
    ...playgrounds.map((p) => ({ id: `playground:${p}`, title: `the playground in '${p}'`, done: `Playground deleted from ${p}: the dashboard removed its server, configuration and database. The project and its models stay.`, apply: () => removePlayground(p) })),
    ...models.map((p) => ({ id: `model:${p}`, title: `the models in '${p}'`, done: `Model ${(world.resources.models?.items ?? []).filter((i) => i.namespace === p && i.managedByTool !== false).map((i) => i.name).join(", ")} deleted from '${p}'; KServe removed its Deployment, Service and route. The project stays.`, apply: () => removeModel(p) })),
    ...(inferenceGateway ? [{ id: "inference-gateway", title: "the inference gateway", done: "Inference gateway deleted: Gateway openshift-ingress/openshift-ai-inference, ConfigMap openshift-ingress/openshift-ai-inference-options and GatewayClass openshift-ai-inference. OpenShift removes its load balancer and the ingress operator its DNS record.", apply: () => { world.resources.inferenceGateway = F.gatewayStates.none; } }] : []),
    ...recipeProjects.map((p) => ({ id: `recipe-project:${p}`, title: `project '${p}', which a recipe created`, says: true, done: `Deleted project ${p}; OpenShift removes what it holds.`, apply: () => {
      world.recipeProjects = F.recipeProjects(world).filter((r) => r.name !== p);
      if (world.recipeRun?.run?.project?.name === p) world.recipeRun.run.project.owned = false;
      world.resources.pipelineServers = world.resources.pipelineServers.filter((ps) => ps.namespace !== p);
    } })),
    ...(components.length ? [{ id: "components", title: "the DataScienceCluster components this tool turned on", says: true, done: `${components.join(" and ")} ${components.length === 1 ? "is" : "are"} Removed again, as before setup.`, apply: () => {
      if (world.recipeRun?.run) world.recipeRun.run.components = (world.recipeRun.run.components ?? []).filter((k) => !components.includes(k));
    } }] : []),
  ];
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  for (const p of parts) {
    send({ step: p.id, status: "running", message: `Deleting ${p.title}` });
    await nextStep();
    if (res.destroyed) return;
    p.apply();
    send({ step: p.id, status: "success", message: p.done });
  }
  // As pkg/cluster cleanupSummary: the parts deleted, then what the parts that speak for themselves said.
  const titles = parts.filter((p) => !p.says).map((p) => p.title);
  const message = [
    ...(titles.length ? [`Deleted ${titles.length > 1 ? `${titles.slice(0, -1).join(", ")} and ${titles[titles.length - 1]}` : titles[0]}.`] : []),
    ...parts.filter((p) => p.says).map((p) => p.done),
  ].join(" ");
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
    send({ step: "callable", status: "running", message: `Calling ${model} in '${project}' once` });
    await nextStep();
    const msg = `Answers requests: a request for model "${model}" got 200.`;
    m.items = (m.items ?? []).map((i) => (i.namespace === project && i.name === model ? { ...i, callCheck: { result: "ok", ok: true, status: 200, message: msg, at: new Date().toISOString() } } : i));
    send({ step: "callable", status: "success", message: msg });
    done.push(msg);
  } else if (action === "disallow") {
    send({ step: "check", status: "success", message: "Checked." });
    send({ step: "gateway", status: "running", message: `Taking '${project}' off the projects the inference gateway allows` });
    await nextStep();
    const gw = world.resources.inferenceGateway;
    world.resources.inferenceGateway = { ...gw, allowedProjects: gw.allowedProjects.filter((p) => p !== project), unusedProjects: (gw.unusedProjects ?? []).filter((p) => p !== project), goneProjects: (gw.goneProjects ?? []).filter((p) => p !== project) };
    const msg = `The inference gateway no longer allows '${project}': routes from it no longer attach.`;
    send({ step: "gateway", status: "success", message: msg });
    done.push(msg);
  } else if (action === "allow") {
    send({ step: "check", status: "running", message: `Checking the inference gateway and project ${project}` });
    await nextStep();
    send({ step: "check", status: "success", message: "The gateway is this tool's; the project exists." });
    send({ step: "gateway", status: "running", message: `Adding '${project}' to the projects the inference gateway allows` });
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
      send({ step: "unpublish", status: "running", message: "The model is reached through MaaS only, so it is unpublished first" });
      await nextStep();
      F.setMaaSPublished(world.resources, project, model, false);
      world.maas = { ...world.maas, items: (world.maas.items ?? []).map((i) => (i.namespace === project && i.name === model ? F.maasModel(false, { name: model }) : i)) };
      send({ step: "unpublish", status: "success", message: `${model} in '${project}' is unpublished from MaaS.` });
    }
    if (project) {
      send({ step: "model", status: "running", message: `Deleting ${model} in ${project}` });
      await nextStep();
      removeModel(project, model);
      const msg = `Model ${model} deleted from '${project}'; KServe removed its Deployment, Service and route. The project stays.`;
      send({ step: "model", status: "success", message: msg });
      done.push(msg);
    }
    if (gateway) {
      send({ step: "gateway", status: "running", message: "Deleting the inference gateway" });
      await nextStep();
      world.resources.inferenceGateway = F.gatewayStates.none;
      const msg = "Inference gateway deleted. OpenShift removes its load balancer and the ingress operator its DNS record.";
      send({ step: "gateway", status: "success", message: msg });
      done.push(msg);
    }
  } else {
    send({ step: "check", status: "running", message: "Checking what every part needs" });
    await nextStep();
    send({ step: "check", status: "success", message: "Everything the parts need is there." });
    if (gateway) {
      send({ step: "gateway", status: "running", message: "Creating the inference gateway" });
      world.resources.inferenceGateway = F.gatewayStates.starting;
      send({ step: "gateway", status: "success", message: "GatewayClass, ConfigMap and Gateway openshift-ingress/openshift-ai-inference applied; external hostname inference.apps.example-cluster.example.com." });
      send({ step: "gateway-ready", status: "running", message: "Programmed; waiting for the load balancer's address" });
      await nextStep();
      world.resources.inferenceGateway = F.gatewayStates.ready([]);
      send({ step: "gateway-ready", status: "success", message: "Ready: https://inference.apps.example-cluster.example.com is published in DNS." });
      done.push("The inference gateway is set up.");
    }
    if (project) {
      send({ step: "model", status: "running", message: `Creating ${display} in ${project} (the newest released Red Hat vLLM CPU image 3.5.3)` });
      const others = (i) => i.namespace !== project || i.name !== model;
      m.items = [...(m.items ?? []).filter(others), F.modelStates.startingAs(model, project)];
      send({ step: "model", status: "success", message: `LLMInferenceService ${project}/${model} applied, on preset v3-6-0-kserve-config-llm-template-cpu with the newest released Red Hat vLLM CPU image 3.5.3. Reached through gateway openshift-ingress/openshift-ai-inference, KServe's default.` });
      send({ step: "model-ready", status: "running", message: "Pulling the runtime image (several GB; a few minutes the first time on a node)" });
      await nextStep();
      send({ step: "model-ready", status: "running", message: "Starting vLLM and loading the model" });
      await nextStep();
      m.items = [...m.items.filter(others), model !== "qwen25-05b-instruct" ? F.modelStates.readyAs(model, project) : releasedImage || m.imagePlan === "released" ? F.modelStates.released(project) : F.modelStates.ready(project)];
      const users = m.items.filter((i) => i.managedByTool !== false).map((i) => `${i.namespace}/${i.name}`);
      if (world.resources.inferenceGateway?.managedByTool) world.resources.inferenceGateway = F.gatewayStates.ready(users);
      if (exposure === "maas") {
        // Published as part of its setup (pkg/cluster publishToMaaS).
        send({ step: "model-ready", status: "success", message: "The model's server runs; publishing it to MaaS gives it its route." });
        send({ step: "publish", status: "running", message: "Publishing the model to MaaS (its route is on the MaaS gateway alone)" });
        await nextStep();
        send({ step: "publish", status: "success", message: `${model} in '${project}' is published to MaaS under subscription rhoai-nightly-updater-subscription.` });
      } else {
        send({ step: "model-ready", status: "success", message: `Ready at https://inference.apps.example-cluster.example.com/${project}/${model}.` });
      }
      send({ step: "callable", status: "running", message: "Calling the model once, as a user would" });
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

/** GET /api/resources/workbench (pkg/cluster GetWorkbenchStatus): the plan is the project's when nothing is there. */
function workbenchOf(project) {
  const { items, noProfile } = world.workbench;
  if (!project) return { items };
  if (items.some((i) => i.namespace === project && i.deployed)) return { project, items };
  return noProfile
    ? { project, items, planProblem: F.WORKBENCH_NO_PROFILE, planProblemCode: "no_profile" }
    : { project, items, plan: world.workbench.plan ?? F.workbenchPlan };
}

/** POST /api/resources/workbench/stream (pkg/cluster RunWorkbench). */
async function workbenchStream(req, res) {
  const { action, project, deleteStorage = false } = await body(req);
  beginOperation("workbench", "Workbench", `${action} workbench in ${project}`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  const others = world.workbench.items.filter((i) => i.namespace !== project);
  let message;
  if (action === "delete-storage") {
    world.workbench.items = others;
    send({ step: "storage", status: "success", message: "Deleted storage nightly-workbench-storage; its files are gone." });
    message = `Deleted storage nightly-workbench-storage in ${project}.`;
  } else if (action === "teardown") {
    const was = world.workbench.items.find((i) => i.namespace === project);
    world.workbench.items = deleteStorage || !was?.storage ? others : [...others, { namespace: project, name: "nightly-workbench", deployed: false, ready: false, managedByTool: true, storage: was.storage }];
    send({ step: "workbench", status: "success", message: "Deleted workbench nightly-workbench." });
    if (deleteStorage) send({ step: "storage", status: "success", message: "Deleted storage nightly-workbench-storage; its files are gone." });
    message = `Deleted workbench nightly-workbench in ${project}.`;
  } else {
    const state = action === "stop" ? "stopped" : "running";
    if (action === "setup") {
      send({ step: "check", status: "success", message: "Image Jupyter | Minimal | CPU | Python 3.12 (2025.2), hardware profile Default, storage 20Gi." });
      send({ step: "storage", status: "success", message: "Created storage nightly-workbench-storage." });
    }
    await nextStep();
    world.workbench.items = [...others, { ...F.workbenchItem(project, state), ready: state === "running" }];
    message = `Workbench nightly-workbench in ${project} is ${state === "stopped" ? "stopped" : "starting"}.`;
    send({ step: "workbench", status: "success", message });
  }
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** GET /api/resources/kueue (pkg/cluster GetKueueStatus): one setup per cluster, in its project. */
function kueueOf(project) {
  const k = world.kueue;
  if (!k.project && project) return { ...k, project, objects: k.objects.map((o) => (o.kind === "LocalQueue" ? { ...o, namespace: project } : o)) };
  return { ...k, ...(k.project && project && project !== k.project ? { setUpIn: k.project } : {}) };
}

/** POST /api/resources/kueue/stream (pkg/cluster RunKueue). */
async function kueueStream(req, res) {
  const { action, project, rayCluster = false, rayJob = false, sample } = await body(req);
  beginOperation("kueue", "Distributed workloads", `${action} distributed workloads`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  let message;
  const k = world.kueue;
  if (action === "setup") {
    const was = k.project ? k : F.kueueWorld("ready", { project });
    send({ step: "check", status: "success", message: `Kueue is ready; queues for 14 CPUs, 44Gi of memory and 12Gi of ephemeral storage in ${project}.` });
    send({ step: "queues", status: "success", message: "ClusterQueue nightly-kueue and LocalQueue nightly-kueue are there." });
    send({ step: "project", status: "success", message: `Labelled ${project}: Kueue admits its workloads sent to a queue.` });
    await nextStep();
    const samples = was.samples.map((s) => ((s.kind === "raycluster" && rayCluster) || (s.kind === "rayjob" && rayJob) ? { ...s, present: true, managedByTool: true, image: F.KUEUE_IMAGE, state: "Queued" } : s));
    world.kueue = { ...was, samples, optIn: k.optIn };
    if (rayCluster || rayJob) send({ step: "samples", status: "success", message: "Created the samples." });
    message = `Distributed workloads are set up in ${project}.`;
  } else if (action === "optin" || action === "optout") {
    world.kueue = { ...k, optIn: F.kueueWorld("ready", { optIn: action === "optin" }).optIn };
    message = action === "optin" ? "The dashboard's Kueue opt-in is on." : "disableKueue is unset again; deleted hardware profile nightly-kueue-cpu.";
    send({ step: action === "optin" ? "profile" : "opt-in", status: "success", message });
  } else if (action === "delete-sample") {
    world.kueue = { ...k, samples: k.samples.map((s) => (s.kind === sample ? { kind: s.kind, name: s.name, present: false } : s)) };
    message = "Deleted the sample.";
    send({ step: "samples", status: "success", message });
  } else {
    world.kueue = F.kueueWorld("none");
    message = "Deleted this tool's distributed workloads setup.";
    send({ step: "opt-in", status: "success", message });
  }
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** POST /api/resources/observability/stream (pkg/cluster RunObservability). */
async function observabilityStream(req, res) {
  const { action } = await body(req);
  beginOperation("observability", "Usage logs and dashboards", `${action} usage logs and dashboards`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  let message;
  if (action === "setup") {
    send({ step: "observability-check", status: "success", message: "Everything Set up changes is checked." });
    for (const [step, text] of [["cert-manager", "cert-manager Operator for Red Hat OpenShift 1.20.1 is installed (not by this tool)."], ["cluster-observability-operator", "Cluster Observability Operator 1.5.5 is installed."], ["opentelemetry", "Red Hat build of OpenTelemetry 0.158.0 is installed."], ["loki-operator", "Loki Operator 6.7.0 is installed."]]) {
      send({ step, status: "success", message: text });
    }
    send({ step: "usage-logs-storage", status: "success", message: "Bucket rhoai-usage-logs-3f9a1c, SeaweedFS user rnu-usage-logs-3f9a1c (its key reaches that bucket only) and Secret redhat-ods-monitoring/rhoai-nightly-updater-usage-logs are ready." });
    send({ step: "monitoring-settings", status: "success", message: "DSCInitialization default-dsci sets usage logs and metrics storage; RHOAI's operator deploys the stack." });
    send({ step: "monitoring-ready", status: "running", message: "Waiting for RHOAI's operator to report the monitoring stack, Perses and the usage logs available (about 3 minutes)" });
    await nextStep();
    world.observability = F.observabilityWorld("ready");
    send({ step: "monitoring-ready", status: "success", message: "RHOAI's operator reports the monitoring stack, Perses and the usage logs available." });
    message = "Usage logs and the dashboards are set up. New calls through the MaaS gateway show in the usage dashboards (RHOAI dashboard, Observe & monitor) once they are made; older calls were not logged.";
  } else {
    for (const [step, text] of [["observability-check", "Clean up removes only what this tool's setup added."], ["monitoring-settings", "DSCInitialization default-dsci is as before this tool's setup; RHOAI's operator deletes the stack."], ["monitoring-gc", "RHOAI's operator deleted the stack."], ["monitoring-leftovers", "Deleted 11 objects the stack left."], ["usage-logs-storage", "Secret rhoai-nightly-updater-usage-logs, SeaweedFS user rnu-usage-logs-3f9a1c and bucket rhoai-usage-logs-3f9a1c are deleted."]]) {
      send({ step, status: "success", message: text });
    }
    world.observability = F.observabilityWorld("none");
    message = "Usage logs and the dashboards this tool set up are removed, with their data. The Cluster Observability Operator, OpenTelemetry, the Loki Operator and cert-manager stay: other features share them.";
  }
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
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
    send({ step: "delete", status: "running", message: `Asking the dashboard to delete the playground lsd-genai-playground in ${project}` });
    await nextStep();
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), F.playgroundStates.terminating(project)];
    send({ step: "delete", status: "success", message: `The dashboard is deleting the playground in ${project}.` });
    send({ step: "gone", status: "running", message: "Waiting until the playground is gone" });
    await nextStep();
    removePlayground(project);
    send({ step: "gone", status: "success", message: `The playground in ${project} is gone.` });
    message = `Playground deleted from ${project}: the dashboard removed its server, configuration and database. The project and its models stay.`;
  } else {
    const listed = world.playground.modelsByProject[project] ?? [];
    const chosen = models.length ? models.map((r) => listed.find((m) => m.name === r.name && m.source === r.source) ?? r) : listed.filter((m) => m.source === "namespace" && m.ready && (m.chat || m.embeddingDimension));
    // As pkg/cluster checkPlayground names them (a MaaS model by its display name, with its name).
    const names = chosen.map((m) => (m.source !== "maas" ? m.name : m.displayName && m.displayName !== m.name ? `${m.displayName} (Models as a Service, ${m.name})` : `${m.name} (Models as a Service)`));
    send({ step: "check", status: "running", message: "Checking what the playground needs" });
    await nextStep();
    send({ step: "check", status: "success", message: `Ready to set up: ${project} with ${names.join(", ")}.` });
    send({ step: "install", status: "running", message: `Asking the dashboard to set up the playground in ${project}` });
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), F.playgroundStates.starting(project)];
    send({ step: "install", status: "success", message: `The dashboard is setting up the playground in ${project}.` });
    send({ step: "ready", status: "running", message: "The playground is starting (phase Initializing)" });
    await nextStep();
    // The record keeps the models Set up asked for (pkg/cluster PlaygroundRecord.Models).
    world.playground.items = [...world.playground.items.filter((i) => i.namespace !== project), { ...F.playgroundStates.ready(project), models: chosen.map((m) => ({ name: m.name, source: m.source })) }];
    send({ step: "ready", status: "success", message: `The playground in ${project} is ready.` });
    message = `The playground in ${project} is ready. Open it in the dashboard's Gen AI studio.`;
  }
  endOperation(true, message);
  send({ step: "operation_complete", status: "success", message });
  res.end();
}

/** POST /api/resources/model-registry/stream (pkg/cluster RunModelRegistry), in the backend's steps. */
async function modelRegistryStream(req, res) {
  const { action, samples } = await body(req);
  beginOperation("model-registry", "Model registry", `${action} model registry`);
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  let n = 0;
  const send = (ev) => res.write(`data: ${JSON.stringify({ ...ev, elapsedMs: ++n * 3000 })}\n\n`);
  let message;
  if (action === "teardown") {
    send({ step: "check", status: "success", message: "Model registry rhoai-model-registries/nightly-registry is the one this tool created." });
    send({ step: "delete", status: "running", message: "Deleting model registry rhoai-model-registries/nightly-registry with its database" });
    world.modelRegistry = F.modelRegistryStates.terminating;
    await nextStep();
    send({ step: "delete", status: "success", message: "Model registry rhoai-model-registries/nightly-registry is being deleted; its operator and the garbage collector remove its database." });
    world.modelRegistry = F.modelRegistryStates.absent;
    send({ step: "gone", status: "success", message: "The registry and its database are gone." });
    message = "Model registry nightly-registry removed, with its registered models and its database.";
  } else {
    send({ step: "check", status: "success", message: "Component aiHub is Managed; registries go in rhoai-model-registries." });
    send({ step: "create", status: "success", message: "Model registry rhoai-model-registries/nightly-registry created; its operator starts a PostgreSQL database and the registry server." });
    world.modelRegistry = F.modelRegistryStates.starting;
    send({ step: "ready", status: "running", message: "Waiting for the registry to be available (usually 1 to 3 minutes)" });
    await nextStep();
    world.modelRegistry = samples ? F.modelRegistryStates.ready : F.modelRegistryStates.readyNoSample;
    send({ step: "ready", status: "success", message: "The registry is available; its REST address is https://rh-ai.apps.example-cluster.example.com/model-registry/nightly-registry." });
    if (samples) send({ step: "samples", status: "success", message: "Sample model qwen25-05b-instruct (version v1) is registered." });
    message = "Model registry nightly-registry is ready in rhoai-model-registries.";
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
    send({ step: "check", status: "running", message: "Checking what this tool set up" });
    await nextStep();
    send({ step: "check", status: "failed", message: world.maasRefusal, errorCode: "in_use" });
    endOperation(false, world.maasRefusal);
    send({ step: "operation_complete", status: "failed", message: world.maasRefusal, errorCode: "in_use" });
    res.end();
    return;
  }
  // A model set up through MaaS only goes back to no route (pkg/cluster removeModelRoute).
  const noRoute = action === "unpublish" && (world.maas.items ?? []).some((i) => i.namespace === project && i.name === model && i.maasOnlySetup);
  // [step, the backend's running message (null: it reports only the result), the result].
  const steps = {
    setup: [["check", "Checking what the MaaS platform needs", "Everything the platform needs is there."],
      ["database", `Creating a one-instance CloudNativePG database ${F.MAAS_INFRA}/maas-db (for development and test)`, `The database runs, and Secret ${F.MAAS_INFRA}/maas-db-config holds its connection URL (sslmode=require).`],
      ["gateway", "Creating Gateway maas-default-gateway with an external load balancer", "Programmed; https://maas.apps.example.com is published in DNS."],
      ["authorino-tls", "Configuring Authorino TLS in kuadrant-system as the docs' setup-authorino-tls.sh does", "Authorino serves TLS and trusts the service CA."],
      ["dsc", "Setting aigateway and its modelsAsAService to Managed in DataScienceCluster default-dsc (it was aigateway Removed, modelsAsAService Removed; removal restores that)", "modelsAsAService is Managed."],
      ["ready", "Waiting for the operator: maas-api, the default tenant, and the gateway's authentication (usually 2 to 3 minutes)", "maas-api is available, the tenant is ready, and the gateway checks every request."]],
    teardown: [["check", "Checking what this tool set up", "Removing, in reverse order of setup."], ["unpublish", "Unpublishing this tool's models", "1 model unpublished."],
      ["dsc", "Setting aigateway.modelsAsAService back to Removed", "modelsAsAService is Removed again, as before setup."],
      ["gateway", "Deleting Gateway openshift-ingress/maas-default-gateway", "The gateway this tool created is deleted. GatewayClass openshift-default stays."],
      ["database", `Deleting CloudNativePG Cluster ${F.MAAS_INFRA}/maas-db`, "The database and its connection Secret are deleted."]],
    publish: [["check", "Checking the platform and the model", "The platform is ready and the model can be published."],
      ["gateway", `Letting the MaaS gateway admit routes from ${project}`, `The MaaS gateway admits routes from '${project}'.`],
      ["model", "Routing the model through the MaaS gateway alone", "The model routes through maas-default-gateway alone. While it is published, the model is used through MaaS (with an API key) and the dashboard's playground; its direct address answers 404 until it is unpublished."],
      ["maas", "Creating the model reference, the subscription and the authorization policy", "MaaSModelRef, MaaSSubscription and MaaSAuthPolicy are applied."],
      ["ready", "Waiting for MaaS to accept the model", "MaaS lists the model."]],
    unpublish: [["check", "Checking the publication", "Unpublishing, in reverse order of publishing."],
      ["maas", "Removing the model from the authorization policy and the subscription", "The model reference is deleted."],
      ["model", noRoute ? "Waiting for KServe to remove the model's route" : "Routing the model as before publishing", noRoute ? "The model has no outside route now, as before it was published: it answers only at its in-cluster address." : "The model routes as before publishing: KServe's default gateway (no refs)."],
      ["gateway", null, `The MaaS gateway no longer admits routes from '${project}'.`]],
    "change-limit": [["check", "Checking the publication and the subscription", "The model's limit can change."],
      ["limit", "Changing the model's entry in the subscription", "The subscription lists the model with its new limit."], ["ready", "Waiting for MaaS to apply it", "MaaS applies it."]],
    "change-access": [["check", "Checking the subscription and the authorization policy", "Who can use the published models can change."],
      ["access", "Changing the subscription's owners and the authorization policy's subjects", "The subscription and the authorization policy name the new audience."], ["ready", "Waiting for MaaS to apply it", "MaaS applies it."]],
    "subscription-priority": [["check", "Checking this tool's subscription", "MaaSSubscription models-as-a-service/rhoai-nightly-updater-subscription has priority 10; it can change to 0."],
      ["priority", "Setting the subscription's priority to 0", "MaaSSubscription models-as-a-service/rhoai-nightly-updater-subscription has priority 0; maas-api uses it for the keys minted from now on. API keys made before keep the subscription they were bound to: revoke and make again those meant for another subscription."]],
    "change-database": [["check", "Checking the database and who uses MaaS", "MaaS can switch databases."], ["cnpg", null, "Not needed."],
      ["database", "Waiting for the development database to be available (up to 2:00, as the script waits)", `Secret ${F.MAAS_INFRA}/maas-db-config names the new database.`],
      ["cleanup", `Deleting CloudNativePG Cluster ${F.MAAS_INFRA}/maas-db`, "Deleted the database before."]],
  }[action] ?? [];
  for (const [step, running, done] of steps) {
    if (running) send({ step, status: "running", message: running });
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
  if (action === "subscription-priority") world.maas = { ...world.maas, subscriptionPriority: { value: 0, ties: world.maas.subscriptionPriority?.ties } };
  // The Models tab's model follows: published, it routes through the MaaS gateway alone.
  if (action === "publish" || action === "unpublish") F.setMaaSPublished(world.resources, project, model, action === "publish");
  const message = { setup: "The MaaS platform is ready.", teardown: "The MaaS platform this tool set up is removed.", publish: `${model} in '${project}' is published to MaaS.`, unpublish: `${model} in '${project}' is unpublished from MaaS.`, "subscription-priority": "This tool's subscription has priority 0, the lowest MaaS honours." }[action];
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
    { step: "check", status: "running", message: `Checking MLflow for project '${project}'` },
    { step: "check", status: "success", message: `MLflow is ready at ${current.mlflowURL}` },
    ...ids.flatMap((id) => [
      { step: id, status: "running", message: `${F.SAMPLE_NAMES[id]}: ${action}` },
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

/**
 * Environment recipes (pkg/api recipe_handlers.go): the plan, and a run
 * that starts at its first step and moves one step on per
 * POST /__docs/advance (the poll shows each), as the runner records it.
 */
async function recipeRoute(req, res, key) {
  const { recipe } = await body(req);
  const installed = !!world.status?.csv?.phase && world.status.csv.phase !== "Not Found";
  if (key === "POST /api/recipes/plan") return json(res, 200, F.recipePlan(recipe ?? F.recipe(), installed, !!world.recipePlanBlocked, !!world.recipePlanMixed));
  if (key === "POST /api/recipes/stop") {
    if (!world.recipeRun.running) return json(res, 409, { error: "No recipe runs in this updater process. Nothing was changed.", errorCode: "not_running" });
    world.recipeRun = { ...world.recipeRun, stopRequested: true };
    return json(res, 200, { stopRequested: true });
  }
  // run and resume: the run runs the steps that are not set up, one per
  // POST /__docs/advance. A resume keeps the run's recipe and the steps it
  // set up (by step ID, as the runner's checks skip them).
  const prior = world.recipeRun.run;
  const resume = key === "POST /api/recipes/resume" && prior;
  const rcp = resume ? prior.recipe : recipe ?? F.recipe();
  const name = rcp.metadata?.name ?? "dev";
  const all = F.recipeRun({ rcp, done: F.recipeSteps(rcp).length, current: "", state: "succeeded", stopReason: `Every step of ${name} is set up.` });
  const doneIds = new Set(resume ? prior.steps.filter((s) => s.status === "success" || s.status === "skipped").map((s) => s.id) : installed ? ["rhoai"] : []);
  const todo = all.steps.map((s) => s.id).filter((id) => !doneIds.has(id));
  const advance = (k) => {
    if (k >= todo.length) {
      world.recipeRun = { run: all, running: false };
      world.operation = { inProgress: false, operation: null };
      return;
    }
    const steps = all.steps.map((s) => {
      if (doneIds.has(s.id) || todo.indexOf(s.id) < k) return s;
      const { id, title, typicalSeconds, maxSeconds } = s;
      return id === todo[k] ? { id, title, typicalSeconds, maxSeconds, status: "running", startedAt: new Date(F.NOW).toISOString() } : { id, title, typicalSeconds, maxSeconds, status: "pending" };
    });
    const title = steps.find((s) => s.id === todo[k]).title;
    world.recipeRun = { run: { ...all, state: "running", stopReason: undefined, finishedAt: undefined, steps }, running: true, pod: prior?.pod,
      progress: { [todo[k]]: { step: todo[k], status: "running", message: `Checking ${title}` } }, step: { step: todo[k], status: "running", message: `Checking ${title}` } };
    const doneNow = steps.filter((s) => s.status === "success" || s.status === "skipped").length;
    world.operation = { inProgress: true, operation: { id: "op-recipe", type: "recipe", label: "Environment recipe", user: F.USER, target: `${name}: ${doneNow} of ${steps.length} steps done; running: ${title}`, startedAt: new Date(F.NOW).toISOString(), updatedAt: new Date(F.NOW).toISOString() } };
    nextStep().then(() => advance(k + 1));
  };
  advance(0);
  return json(res, 202, { runId: "run-7c1e" });
}

const routes = {
  "GET /api/status": () => reads["GET /api/status"](world),
  "GET /api/operation": () => reads["GET /api/operation"](world),
  "GET /api/version": () => ({ ...world.version, profile: world.profile || "full", ...(world.profile === "gitops" ? { admissionPolicy: world.admissionPolicy || "enforced" } : {}) }),
  "GET /api/update-check": () => world.updateCheck,
  "GET /api/user/permissions": () => reads["GET /api/user/permissions"](world),
  "GET /api/latest-nightly": () => F.latestNightly(),
  "GET /api/nightly-tags": () => F.nightlyTags(true),
  "GET /api/build-explorer/tags": (u) => F.nightlyTags(u.searchParams.get("includeDates") !== "false"),
  "GET /api/build-explorer/content": (u) => F.fbcContent(u.searchParams.get("image") || F.INSTALLED_IMAGE),
  "GET /api/build-explorer/contains": (u) => F.buildContains({ pr: u.searchParams.get("pr"), commit: u.searchParams.get("commit"), repo: u.searchParams.get("repo") }, u.searchParams.getAll("image")),
  "GET /api/components": () => reads["GET /api/components"](world),
  "GET /api/components/dsc": () => reads["GET /api/components/dsc"](world),
  "GET /api/components/dsc/plan": (u) => F.dscPlan(world.dscManager ?? F.dscManager(), u.searchParams.get("action"), u.searchParams.get("path"), u.searchParams.get("to"), u.searchParams.get("withParent") === "true", u.searchParams.get("keepEnabled") === "true"),
  "GET /api/diagnostics": () => reads["GET /api/diagnostics"](world),
  "GET /api/dashboard/state": () => reads["GET /api/dashboard/state"](world),
  "GET /api/dashboard/builds": () => world.dashboardBuilds,
  "GET /api/resources/status": () => reads["GET /api/resources/status"](world),
  "GET /api/resources/projects": () => reads["GET /api/resources/projects"](world),
  "GET /api/summary": () => reads["GET /api/summary"](world),
  "GET /api/resources/playground": (u) => playgroundOf(u.searchParams.get("project") || ""),
  "GET /api/resources/workbench": (u) => workbenchOf(u.searchParams.get("project") || ""),
  "GET /api/resources/kueue": (u) => kueueOf(u.searchParams.get("project") || ""),
  "GET /api/resources/maas": () => reads["GET /api/resources/maas"](world),
  "GET /api/resources/observability": () => world.observability,
  "GET /api/resources/model-registry": () => reads["GET /api/resources/model-registry"](world),
  "GET /api/resources/maas/groups": (u) => F.maasGroupProblems(u.searchParams.getAll("name")),
  // Models routing and one-click prerequisites.
  "GET /api/resources/models/exposures": (u) => F.modelExposures(u.searchParams.get("project") || "", u.searchParams.get("model") || "qwen25-05b-instruct", world.resources.inferenceGateway ?? F.gatewayStates.none, world.routing ?? {}),
  "GET /api/resources/prerequisites": (u) => F.prerequisites({ ...(world.prereq ?? {}), exposure: u.searchParams.get("exposure") ?? "", ...(world.prereq?.models === "missing" && ["default-gateway", "gateway"].includes(u.searchParams.get("exposure") ?? "") ? { models: "missing-routing" } : {}) }),
  "GET /api/resources/samples/mlflow": (u) => mlflowSamplesOf(u.searchParams.get("project") || ""),
  "GET /api/resources/samples/pipeline/spec": () => F.pipelineSampleSpec,
  // Made up, like every value here.
  "GET /api/resources/s3/admin-password": () => ({ user: "admin", accessKey: "docs-mock-access-key", secretKey: "docs-mock-secret-key", password: "docs-mock-admin-password" }),
  "GET /api/activity": () => reads["GET /api/activity"](world),
  "GET /api/recipes/run": () => reads["GET /api/recipes/run"](world),
  "GET /api/test-pull-secret": () => ({ success: true, message: "The pull secret can pull from quay.io/rhoai.", logs: [] }),
  "GET /api/verify-nodes": () => ({ success: true, message: "All 3 nodes have the pull secret and the image mirror.", logs: [] }),
  "POST /api/setup/node-pull-check": () => F.nodePullCheck(F.latestNightly().image, !world.nodePullFails),
  // ROSA HCP: the internal registry's copy of the pull secret (made up, like every value here).
  "POST /api/setup/pull-secret/registry-copy": () => {
    const reg = world.status?.pullSecret?.registry;
    if (!reg || !reg.namespace) return { success: false, errorCode: "prerequisites", message: "This cluster does not have a hosted control plane: its internal registry pulls with openshift-config/pull-secret, so no copy is made.", logs: [] };
    if (reg.noQuayRHOAIEntry) return { success: false, errorCode: "prerequisites", message: "Workbench images: the pull secret has no quay.io/rhoai entry, so nothing was copied for the internal registry; add the quay.io/rhoai token under Pull secret.", logs: [] };
    if (reg.exists && !reg.ours) return { success: false, errorCode: "not_managed", message: `Workbench images: Secret ${reg.namespace}/${reg.name} exists and was not created by this updater, so it was not changed.`, logs: [] };
    const updated = reg.exists;
    world.status.pullSecret.registry = { ...reg, exists: true, ours: true, current: true, unusable: undefined };
    return { success: true, message: updated ? `Workbench images: updated Secret ${reg.namespace}/${reg.name} with the new token for the internal registry` : `Workbench images: copied the quay.io/rhoai token to Secret ${reg.namespace}/${reg.name}, so the internal registry can pull nightly workbench images`, logs: [] };
  },
  "POST /api/setup/pull-secret/registry-copy/remove": () => {
    const reg = world.status?.pullSecret?.registry;
    if (!reg?.exists) return { success: true, message: "Secret does not exist; nothing to remove.", logs: [] };
    if (!reg.ours) return { success: false, errorCode: "not_managed", message: `Secret ${reg.namespace}/${reg.name} was not created by this updater, so it is not removed.`, logs: [] };
    world.status.pullSecret.registry = { ...reg, exists: false, ours: false, current: false, unusable: undefined };
    return { success: true, message: `removed Secret ${reg.namespace}/${reg.name}. Workbenches cannot pull nightly images through the internal registry until the token is copied again.`, logs: [] };
  },
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
  "GET /api/status", "GET /api/operation", "GET /api/activity", "GET /api/update-check", "GET /api/components", "GET /api/components/dsc",
  "GET /api/diagnostics", "GET /api/user/permissions", "POST /api/update", "POST /api/update/stream", "POST /api/pageview", "GET /api/version",
]);

// The GET routes only cluster-admins may read (per-user views; pkg/api's
// route classification): a user gets 403 admin_only.
const ADMIN_ONLY_READS = [
  "/api/status", "/api/activity", "/api/components", "/api/debug", "/api/dashboard/", "/api/diagnostics", "/api/setup/dsc/preview",
  "/api/test-pull-secret", "/api/verify-nodes", "/api/latest-nightly", "/api/nightly-tags", "/api/resources/maas",
  "/api/resources/prerequisites", "/api/resources/models/exposures", "/api/resources/s3/admin-password", "/api/recipes/",
];

/**
 * A user who is not a cluster-admin (world.role "user"): what the backend
 * answers them differently (true once answered), or false for the reads
 * every user makes.
 */
function userRead(res, key, u) {
  const send = (code, body) => { json(res, code, body); return true; };
  const adminOnly = () => send(403, { error: "Only cluster-admins can see this.", errorCode: "admin_only" });
  if (key === "GET /api/operation") {
    const op = world.operation.inProgress ? world.operation.operation : null;
    return send(200, op ? { running: true, label: op.label, startedAt: op.startedAt } : { running: false, label: "", startedAt: "" });
  }
  if (key === "GET /api/resources/projects") return send(200, { projects: world.userProjects ?? [] });
  if (key === "GET /api/resources/projects/capabilities") {
    const project = u.searchParams.get("project") || "";
    if (!(world.userProjects ?? []).includes(project)) return send(403, { error: `Project ${project} is not one of your data science projects.`, errorCode: "forbidden_for_user" });
    return send(200, F.userCapabilities(project));
  }
  if (key === "GET /api/resources/status") {
    const project = u.searchParams.get("project");
    if (!project) return adminOnly();
    // pkg/types ProjectStatus: that project's objects only.
    const all = reads["GET /api/resources/status"](world);
    return send(200, {
      project,
      pipelineServers: [...(all.pipelineServers ?? []), ...(all.unmanagedPipelineServers ?? [])].filter((ps) => ps.namespace === project),
      models: (all.models?.items ?? []).filter((m) => m.namespace === project),
      inferenceServices: [],
      dashboardUrl: all.dashboardUrl,
    });
  }
  if (key.startsWith("GET ") && ADMIN_ONLY_READS.some((p) => u.pathname.startsWith(p))) return adminOnly();
  if (!key.startsWith("GET ") && key !== "POST /api/pageview") return adminOnly();
  return false;
}

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
      return json(res, 403, { error: "This feature is off: this updater runs the gitops profile, where Argo CD or an ACM policy manages RHOAI from Git and the updater only installs the build the managed catalog serves (https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/main/docs/GITOPS.md#11-profiles).", errorCode: "profile_gitops" });
    }
    if (world.canMutate === false && req.method === "GET" && !["/api/user/permissions", "/api/version"].includes(u.pathname)) {
      return json(res, 403, { error: "This updater runs the gitops profile, where only cluster-admins can see what it reads from the cluster.", errorCode: "forbidden" });
    }
  }
  if (world.role === "user" && userRead(res, key, u)) return;
  if (key === "POST /api/pageview") { res.writeHead(204); res.end(); return; }
  if (world.signIn) {
    // An expired sign-in fails the identity check; a sign-in from before
    // 4.0.0 passes it, so permissions answer with the reason instead. The
    // role stays the cluster-admin's: pkg/api HandleUserPermissions decides
    // it before the session check, which only adds the reason.
    const { status, code, message } = world.signIn;
    if (key === "GET /api/user/permissions") {
      if (code === "session_expired") return json(res, status, { error: message, errorCode: code });
      return json(res, 200, { canMutate: false, user: F.USER, role: "admin", reason: message, reasonCode: code });
    }
    if (req.method !== "GET") return json(res, status, { error: code === "session_expired" ? message : `${message} Nothing was changed.`, errorCode: code });
  }
  if (key === "GET /api/resources/s3/admin-password" && world.canMutate === false) {
    return json(res, 403, { error: "Only cluster-admins can read this through the updater.", errorCode: "forbidden" });
  }
  if ((key === "POST /api/setup/nodes/plan" || key === "POST /api/setup/nodes/stream") && world.canMutate === false) {
    return json(res, 403, { error: "Read-only access: changing the cluster through this tool requires the cluster-admin role.", errorCode: "forbidden" });
  }
  if (key === "POST /api/setup/nodes/plan") {
    const { action = "", replaceExisting = false } = await body(req);
    return json(res, 200, F.nodeSetupPlan(world, action, replaceExisting));
  }
  if (key === "POST /api/setup/nodes/stream") return nodeSetupStream(req, res);
  if (key.startsWith("POST /api/recipes/")) return recipeRoute(req, res, key);
  if (key === "POST /api/update/stream") return updateStream(req, res);
  if (key === "POST /api/resources/s3/setup") return mutation(res, "setup-s3", "Set up S3 storage", minioSetup);
  if (key === "POST /api/resources/s3/secure/stream") return secureStream(req, res);
  if (key === "POST /api/resources/s3/leftover-keys/remove") return mutation(res, "remove-leftover-s3-keys", "Remove leftover S3 keys", () => {
    const gone = world.resources.s3Keys?.leftoverUsers ?? [];
    world.resources.s3Keys = { ...world.resources.s3Keys, leftoverUsers: [], leftovers: [] };
    return { success: true, message: `Removed the keys of pipeline servers that no longer exist: ${gone.join(", ")}.`, logs: [] };
  });
  if (key === "POST /api/resources/s3/migrate/stream") return migrateStream(req, res);
  if (key === "POST /api/resources/cleanup/stream") return cleanupStream(req, res);
  if (key === "POST /api/resources/models/stream") return modelsStream(req, res);
  if (key === "POST /api/resources/playground/stream") return playgroundStream(req, res);
  if (key === "POST /api/resources/model-registry/stream") return modelRegistryStream(req, res);
  if (key === "POST /api/resources/workbench/stream") return workbenchStream(req, res);
  if (key === "POST /api/resources/kueue/stream") return kueueStream(req, res);
  if (key === "POST /api/resources/observability/stream") return observabilityStream(req, res);
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
  if (key === "POST /api/diagnostics/fix" || key === "POST /api/diagnostics/fix/acknowledged") {
    const { problemId = "", acknowledged = false } = await body(req);
    return mutation(res, "diagnostics-fix", "Apply diagnostics fix", () => {
      // The backend's answers (pkg/cluster/internal/diagnostics/diagnostics_leftovers_fix.go).
      if (problemId.startsWith("recreate-leftover:")) {
        if (!acknowledged) {
          return { success: false, errorCode: "acknowledgement_required", logs: [],
            message: "Deleting Deployment redhat-ods-applications/trustyai-service-operator-controller-manager needs your acknowledgement first: the tool cannot tell whether trustyai is in use: it has no reader for that component's resources. Nothing was changed; confirm it in the Fix dialog." };
        }
        if (world.leftoverRefusal) {
          return { success: false, errorCode: "prerequisites", logs: ["Reported by TrustyAI default-trustyai"],
            message: "Not deleting Deployment redhat-ods-applications/trustyai-service-operator-controller-manager: the trustyai module operator redhat-ods-applications/trustyai-operator-module-controller-manager, which creates it again, has no available pod; fix that first. Nothing was deleted." };
        }
        world.diagnostics = { ...world.diagnostics, problems: world.diagnostics.problems.filter((p) => !p.id.startsWith("leftover-recreate-")) };
        return { success: true, logs: ["Deleted Deployment redhat-ods-applications/trustyai-service-operator-controller-manager (UID old-uid, propagation Background)", "It is back and ready"],
          message: "Recreated Deployment redhat-ods-applications/trustyai-service-operator-controller-manager: the trustyai module operator redhat-ods-applications/trustyai-operator-module-controller-manager created it again with the current selector and its pods are ready. Re-scan to check that TrustyAI default-trustyai is ready." };
      }
      if (problemId.startsWith("fix-leftover-fields:")) {
        world.diagnostics = { ...world.diagnostics, problems: world.diagnostics.problems.filter((p) => !p.id.startsWith("leftover-fields-") && !p.id.startsWith("pod-crashloop-")) };
        return { success: true, logs: ["The API server's dry run accepted the change"],
          message: "Changed TrustyAI default-trustyai: .spec.eval.lmeval.permitOnline false -> \"deny\", .spec.eval.lmeval.permitCodeExecution false -> \"deny\". The DataScienceCluster controller applies it again on its own; re-scan in a minute. If the error stays, the scan offers Retry now." };
      }
      return {
        success: true,
        message: problemId.includes("restart-module-operator")
          ? "Restarted redhat-ods-applications/trainer-operator-controller-manager. The trainer operator reconciles when its new pod starts; re-scan in a minute or two."
          : "Done.",
        logs: [],
      };
    });
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
      if (req.method === "POST") job = { name: u.searchParams.get("name") || "", out: u.searchParams.get("out") || "", contrast: u.searchParams.get("contrast") || "" };
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
