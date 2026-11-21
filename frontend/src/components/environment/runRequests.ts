import type { ModelsRequest, OperationResponse, ResourceState, ResourcesStatus } from "../../types";
import {
  setupMinIO,
  teardownMinIO,
  setupPipelineServer,
  teardownPipelineServer,
  setupMLflow,
  teardownMLflow,
  deployMLflowPR,
  revertMLflow,
} from "../../services/api";
import { nextStep, problemTitle, terminalReason, type ModelsPending, type ResourceKind, type ResourcePending } from "./resourceRow";
import { pipelineServerIn, type ModelsOp } from "./featureState";
import type { GoalRunStep, ReadyCheck } from "./useGoalRun";

/**
 * Whether a part that was just created works, from the status: ready, a
 * reason it cannot start (in words, with the next step and the raw
 * reason), or still starting.
 */
export function readyCheck(kind: ResourceKind, state: ResourceState | undefined): ReadyCheck {
  if (!state || !state.deployed) return { state: "starting" };
  if (state.ready) return { state: "ready" };
  const reason = terminalReason(state);
  if (reason) {
    const step = nextStep(kind, state);
    return { state: "failed", reason: `${problemTitle(kind, state)}.${step ? ` ${step}` : ""} Technical details: ${state.message || reason}.` };
  }
  return { state: "starting" };
}

/** A confirmed storage, MLflow or pipeline server change: the action it is tracked and shown as, its request, and the project it changes. */
export function resourceRequest(p: ResourcePending): [string, () => Promise<OperationResponse>, string | null] {
  switch (p.kind) {
    case "setup-minio": return ["setup-minio", setupMinIO, null];
    case "repair-minio": return ["setup-minio", setupMinIO, null];
    case "teardown-minio": return ["teardown-minio", () => teardownMinIO(!!p.deleteNamespace), null];
    case "setup-mlflow": return ["setup-mlflow", setupMLflow, null];
    case "teardown-mlflow": return ["teardown-mlflow", teardownMLflow, null];
    case "deploy-mlflow-pr": return ["deploy-mlflow-pr", () => deployMLflowPR(p.pr), null];
    case "revert-mlflow": return ["revert-mlflow", revertMLflow, null];
    case "setup-pipeline-server": return ["setup-pipeline-server", () => setupPipelineServer(p.project), p.project];
    // The setup request with recreate: the page shows it as setting up.
    case "recreate-pipeline-server": return ["setup-pipeline-server", () => setupPipelineServer(p.project, true), p.project];
    case "teardown-pipeline-server": return ["teardown-pipeline-server", () => teardownPipelineServer(p.project), p.project];
  }
}

/**
 * A step of a Set up run: the request returns once the part is created; the
 * step waits until the status the page keeps loading reports it working.
 */
export function setUpWait(p: ResourcePending, target: string | null, statusRef: { current: ResourcesStatus | null }): Pick<GoalRunStep, "waitReady" | "readyMessage"> {
  const waitReady: GoalRunStep["waitReady"] = p.kind === "setup-pipeline-server"
    ? { check: () => readyCheck("pipeline", pipelineServerIn(statusRef.current, target!)), typical: "1 to 3 minutes" }
    : p.kind === "setup-mlflow"
      ? { check: () => readyCheck("mlflow", statusRef.current?.mlflow), typical: "1 to 3 minutes" }
      : { check: () => readyCheck("minio", statusRef.current?.minio), typical: "under a minute" };
  const readyMessage = p.kind === "setup-minio" ? "The S3 storage is ready." : p.kind === "setup-mlflow" ? "MLflow is ready." : `The pipeline server in ${target} is ready.`;
  return { waitReady, readyMessage };
}

/** What a confirmed Models change does: the parts it shows as busy. */
export function modelsOpOf(p: ModelsPending): ModelsOp {
  return p.kind === "setup-models"
    ? { action: "setup", gateway: p.gateway, project: p.project, model: p.project ? p.model : null }
    : p.kind === "teardown-model" ? { action: "teardown", gateway: false, project: p.project, model: p.model }
    : p.kind === "allow-gateway-project" ? { action: "allow", gateway: true, project: null, model: null, allowProject: p.project }
    : p.kind === "disallow-gateway-project" ? { action: "disallow", gateway: true, project: null, model: null, allowProject: p.project }
    : p.kind === "check-model" ? { action: "check", gateway: false, project: p.project, model: p.model }
    : { action: "teardown", gateway: true, project: null, model: null };
}

/** The streamed request of a confirmed Models change. */
export function modelsRequest(op: ModelsOp, p: ModelsPending): ModelsRequest {
  return op.action === "allow" || op.action === "disallow" ? { action: op.action, project: op.allowProject } : op.action === "check" ? { action: "check", project: op.project!, model: op.model! } : {
    action: op.action,
    ...(op.gateway ? { gateway: true } : {}),
    ...(op.project ? { project: op.project } : {}),
    ...(op.project && op.model ? { model: op.model } : {}),
    ...(p.kind === "setup-models" && p.releasedImage ? { releasedImage: true } : {}),
    ...(p.kind === "setup-models" && p.exposure ? { exposure: p.exposure } : {}),
    ...(p.kind === "setup-models" && p.gatewayRef ? { gatewayRef: p.gatewayRef } : {}),
  };
}
