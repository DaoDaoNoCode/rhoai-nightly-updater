import type { ClusterSummary, MLflowSamplesStatus, PipelineSampleSpec, PlaygroundStatus, ProjectStatus } from "../types";
import { stubApi, jsonResponse, type Handler } from "./apiStub";
import { contract } from "./contract";

// What a user who is not a cluster-admin reads (docs/research/per-user-views.md),
// built from the JSON the Go handlers produce (./contract.ts): a fixture
// here cannot drift from the backend's types.

const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
/** A build the Build Explorer lists (its tags carry image references; the summary does not). */
export const USER_LATEST_BUILD = `${FBC}:rhoai-3.6@sha256:${"0b3f9b4d".padEnd(64, "1")}`;

export function summary(overrides: Partial<ClusterSummary> = {}): ClusterSummary {
  return { ...contract.summary(), ...overrides };
}

/** The project-scoped status a user gets for one of their projects: a ready pipeline server, one model, one InferenceService. */
export function projectStatus(project = "dev-project"): ProjectStatus {
  return contract.projectStatus(project);
}

export function playground(project = "dev-project"): PlaygroundStatus {
  return contract.playground(project);
}

export function mlflowSamples(project = "dev-project"): MLflowSamplesStatus {
  return contract.mlflowSamples(project);
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
    "/api/user/permissions": { ...contract.permissionsUser(), user: "dev-user" },
    "/api/version": { version: "v5.0.0", commit: "c", buildDate: "d", templateOutdated: false },
    "/api/update-check": { updateAvailable: false, majorUpgrade: false },
    "/api/operation": { running: false, label: "", startedAt: "" },
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
    "/api/resources/workbench": (url: string) => jsonResponse(contract.workbenchUser(new URL(url, "http://x").searchParams.get("project") ?? "dev-project")),
    "/api/resources/projects/capabilities": (url: string) => jsonResponse(contract.capabilitiesUser(new URL(url, "http://x").searchParams.get("project") ?? "dev-project")),
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
  /^\/api\/recipes\//,
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
  // The project actions (non-admin design B3): one of the user's projects.
  /^\/api\/resources\/workbench\?project=[^&]+$/,
  /^\/api\/resources\/projects\/capabilities\?project=[^&]+$/,
];
