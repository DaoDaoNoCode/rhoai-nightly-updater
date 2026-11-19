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
    ...(storage ? [{ id: "storage", title: "the S3 storage", done: deleteNamespace ? "S3 storage removed. Namespace seaweedfs is being deleted." : "S3 storage removed.", apply: () => { world.resources.minio = F.s3States.none; delete world.resources.legacyStorage; } }] : []),
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
      send({ step: "unpublish", status: "running", message: "The model is reached through MaaS only, so it is unpublished first..." });
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
      send({ step: "model", status: "success", message: `LLMInferenceService ${project}/${model} applied, on preset v3-6-0-kserve-config-llm-template-cpu with the newest released Red Hat vLLM CPU image 3.5.3. Reached through gateway openshift-ingress/openshift-ai-inference, KServe's default.` });
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
    unpublish: [["check", "Unpublishing, in reverse order of publishing."], ["maas", "The model reference is deleted."], ["model", noRoute ? "The model has no outside route now, as before it was published: it answers only at its in-cluster address." : "The model routes as before publishing: KServe's default gateway (no refs)."],
      ["gateway", `The MaaS gateway no longer admits routes from '${project}'.`]],
    "change-limit": [["check", "The model's limit can change."], ["limit", "The subscription lists the model with its new limit."], ["ready", "MaaS applies it."]],
    "change-access": [["check", "Who can use the published models can change."], ["access", "The subscription and the authorization policy name the new audience."], ["ready", "MaaS applies it."]],
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
  "GET /api/resources/playground": (u) => playgroundOf(u.searchParams.get("project") || ""),
  "GET /api/resources/maas": () => reads["GET /api/resources/maas"](world),
  "GET /api/resources/maas/groups": (u) => F.maasGroupProblems(u.searchParams.getAll("name")),
  // Models routing and one-click prerequisites.
  "GET /api/resources/models/exposures": (u) => F.modelExposures(u.searchParams.get("project") || "", u.searchParams.get("model") || "qwen25-05b-instruct", world.resources.inferenceGateway ?? F.gatewayStates.none, world.routing ?? {}),
  "GET /api/resources/prerequisites": (u) => F.prerequisites({ ...(world.prereq ?? {}), exposure: u.searchParams.get("exposure") ?? "", ...(world.prereq?.models === "missing" && ["default-gateway", "gateway"].includes(u.searchParams.get("exposure") ?? "") ? { models: "missing-routing" } : {}) }),
  "GET /api/resources/samples/mlflow": (u) => mlflowSamplesOf(u.searchParams.get("project") || ""),
  "GET /api/resources/samples/pipeline/spec": () => F.pipelineSampleSpec,
  // Made up, like every value here.
  "GET /api/resources/minio/admin-password": () => ({ user: "admin", accessKey: "docs-mock-access-key", secretKey: "docs-mock-secret-key", password: "docs-mock-admin-password" }),
  "GET /api/activity": () => reads["GET /api/activity"](world),
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
  "GET /api/status", "GET /api/operation", "GET /api/activity", "GET /api/update-check", "GET /api/components", "GET /api/components/dsc",
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
  if (world.signIn) {
    // An expired sign-in fails the identity check; a sign-in from before
    // 4.0.0 passes it, so permissions answer with the reason instead.
    const { status, code, message } = world.signIn;
    if (key === "GET /api/user/permissions") {
      if (code === "session_expired") return json(res, status, { error: message, errorCode: code });
      return json(res, 200, { canMutate: false, user: F.USER, reason: message, reasonCode: code });
    }
    if (req.method !== "GET") return json(res, status, { error: code === "session_expired" ? message : `${message} Nothing was changed.`, errorCode: code });
  }
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
  if (key === "POST /api/diagnostics/fix" || key === "POST /api/diagnostics/fix/acknowledged") {
    const { problemId = "", acknowledged = false } = await body(req);
    return mutation(res, "diagnostics-fix", "Apply diagnostics fix", () => {
      // The backend's answers (pkg/cluster/diagnostics_leftovers_fix.go).
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
