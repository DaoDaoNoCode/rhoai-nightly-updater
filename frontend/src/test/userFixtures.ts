import type { ClusterSummary, MLflowSamplesStatus, PipelineSampleSpec, PlaygroundStatus, ResourcesStatus } from "../types";
import { stubApi, jsonResponse, type Handler } from "./apiStub";

// What a user who is not a cluster-admin reads (docs/research/per-user-views.md).

const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
export const USER_INSTALLED_BUILD = `${FBC}:rhoai-3.6@sha256:${"4eff06d6".padEnd(64, "0")}`;
export const USER_LATEST_BUILD = `${FBC}:rhoai-3.6@sha256:${"0b3f9b4d".padEnd(64, "1")}`;

export function summary(overrides: Partial<ClusterSummary> = {}): ClusterSummary {
  return {
    updaterVersion: "v5.0.0",
    rhoai: { installed: true, version: "3.6.0", channel: "stable-3.x", phase: "Succeeded" },
    nightly: { installedBuild: USER_INSTALLED_BUILD, latestBuild: USER_LATEST_BUILD, newerAvailable: true },
    dsc: {
      exists: true,
      ready: false,
      components: [
        { name: "dashboard", managementState: "Managed", ready: true },
        { name: "aipipelines", managementState: "Managed", ready: false },
        { name: "trustyai", managementState: "Removed", ready: false },
      ],
    },
    ...overrides,
  };
}

/** The project-scoped status of dev-project: a ready pipeline server and one model. */
export function projectStatus(project = "dev-project"): ResourcesStatus {
  return {
    minio: { deployed: false, ready: false },
    mlflow: { deployed: false, ready: false },
    pipelineServers: [{
      deployed: true, ready: true, message: "Running", namespace: project, name: "dspa",
      apiRoute: `https://ds-pipeline-dspa-${project}.apps.example.com`, uiRoute: `https://rh-ai.apps.example.com/develop-train/pipelines/definitions/${project}`,
    }],
    inferenceGateway: { deployed: false, ready: false },
    models: {
      catalog: null,
      items: [{ deployed: true, ready: true, namespace: project, name: "qwen3-0-6b", displayName: "Qwen3 0.6B", url: `https://inference.apps.example.com/${project}/qwen3-0-6b` }],
    },
  };
}

export function playground(project = "dev-project"): PlaygroundStatus {
  return { items: [{ deployed: true, ready: true, namespace: project, managedByTool: true }], project, models: [] };
}

export function mlflowSamples(project = "dev-project"): MLflowSamplesStatus {
  return {
    project,
    available: true,
    samples: [
      { id: "genai-demo", name: "Agent and prompt demo", state: "present", actions: ["remove"], link: "https://mlflow.apps.example.com/#/experiments/1" },
      { id: "training", name: "Training run", state: "absent", actions: ["add"] },
    ],
  };
}

export const PIPELINE_SPEC: PipelineSampleSpec = {
  name: "sample-artifact-check", fileName: "sample-artifact-check.yaml", description: "Write and read an artifact",
  image: "registry.access.redhat.com/ubi9/python-312@sha256:" + "1".repeat(64), yaml: "pipelineSpec: {}\n",
};

/**
 * Every read a user's pages make, answered as the backend answers a user;
 * every read only cluster-admins may make answers 403 admin_only.
 */
export function stubUserApi(extra: Record<string, Handler | unknown> = {}) {
  const adminOnly: Handler = () => jsonResponse({ error: "Only cluster-admins can see this.", errorCode: "admin_only" }, 403);
  return stubApi({
    "/api/user/permissions": { canMutate: false, user: "dev-user", role: "user" },
    "/api/version": { version: "v5.0.0", commit: "c", buildDate: "d", templateOutdated: false },
    "/api/update-check": { updateAvailable: false, majorUpgrade: false },
    "/api/operation": { running: false },
    "/api/summary": summary(),
    "/api/build-explorer/tags": { tags: [{ tag: "rhoai-3.6", image: USER_LATEST_BUILD }] },
    "/api/resources/projects": { projects: ["dev-project", "llm-eval"] },
    "/api/resources/status": (url: string) => {
      const project = new URL(url, "http://x").searchParams.get("project");
      return project ? jsonResponse(projectStatus(project)) : adminOnly(url, undefined);
    },
    "/api/resources/playground": (url: string) => jsonResponse(playground(new URL(url, "http://x").searchParams.get("project") ?? "dev-project")),
    "/api/resources/samples/mlflow": (url: string) => jsonResponse(mlflowSamples(new URL(url, "http://x").searchParams.get("project") ?? "dev-project")),
    "/api/resources/samples/pipeline/spec": PIPELINE_SPEC,
    "/api/status": adminOnly,
    "/api/dashboard/state": adminOnly,
    ...extra,
  });
}

/**
 * The GET routes only cluster-admins may read (design, "API"): a user's tab
 * must never ask for one. The full /api/resources/status (no project) is
 * one too.
 */
export const ADMIN_ONLY_GETS: RegExp[] = [
  /^\/api\/status(\?|$)/,
  /^\/api\/activity/,
  /^\/api\/components/,
  /^\/api\/debug/,
  /^\/api\/dashboard\//,
  /^\/api\/diagnostics/,
  /^\/api\/setup\/dsc\/preview/,
  /^\/api\/test-pull-secret/,
  /^\/api\/verify-nodes/,
  /^\/api\/latest-nightly/,
  /^\/api\/nightly-tags/,
  /^\/api\/resources\/maas/,
  /^\/api\/resources\/prerequisites/,
  /^\/api\/resources\/models\/exposures/,
  /^\/api\/resources\/s3\/admin-password/,
  /^\/api\/resources\/status(?!\?project=)/,
];

/** The reads a user's pages may make, and nothing else. */
export const USER_ALLOWED_GETS: RegExp[] = [
  /^\/api\/user\/permissions$/,
  /^\/api\/version$/,
  /^\/api\/update-check$/,
  /^\/api\/operation$/,
  /^\/api\/summary$/,
  /^\/api\/build-explorer\//,
  /^\/api\/resources\/projects$/,
  /^\/api\/resources\/status\?project=[^&]+$/,
  /^\/api\/resources\/playground\?project=[^&]+$/,
  /^\/api\/resources\/samples\/mlflow\?project=[^&]+$/,
  /^\/api\/resources\/samples\/pipeline\/spec$/,
];
