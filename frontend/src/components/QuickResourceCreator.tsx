import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  AlertGroup,
  Content,
  PageSection,
  Stack,
  StackItem,
  Tab,
  Tabs,
  TabTitleText,
} from "@patternfly/react-core";
import { useSearchParams } from "react-router-dom";
import type { OperationResponse, ResourceState, ResourcesStatus } from "../types";
import {
  getResourcesStatus,
  getDSProjects,
  setupMinIO,
  teardownMinIO,
  setupPipelineServer,
  teardownPipelineServer,
  trackFeature,
  setupMLflow,
  teardownMLflow,
  deployMLflowPR,
  revertMLflow,
  streamCleanup,
  streamMLflowSamples,
  streamModels,
  streamPlayground,
  streamMaaS,
  streamPrerequisites,
  toApiError,
  type ApiError,
  type StreamDoneHandler,
  type StreamDetachHandler,
  type CleanupRequest,
} from "../services/api";
import { exponentialBackoff, usePolling } from "../hooks/usePolling";
import { RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS, RESOURCE_SETTLE_MAX_MS } from "../constants";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { TruncatedText } from "./LongText";
import { PageErrorState } from "./PageStates";
import { errorResult } from "../outcomes";
import { type ConfirmSpec, type ModelsPending, type Pending, type PlaygroundPending, type MaaSPending, isMaaSPending, type PrerequisitesPending, isPrerequisitesPending, type ResourcePending, type SamplePending, isModelsPending, isPlaygroundPending, isSamplePending, isTerminating, nextStep, problemTitle, terminalReason, type ResourceKind } from "./environment/resourceRow";
import { ModelsTab, modelsConfirm, modelsSteps } from "./environment/ModelsTab";
import { PlaygroundTab, playgroundConfirm, playgroundSteps } from "./environment/PlaygroundTab";
import { MaaSTab, maasConfirm, maasRequestOf, maasSteps, useMaaSStatus } from "./environment/MaaSTab";
import { usePlaygroundStatus } from "./environment/usePlaygroundStatus";
import { envStatusText } from "./environment/resourceStatus";
import { PipelinesTab, READ_ONLY_SETUP, pipelineServerConfirm, storageConfirm } from "./environment/PipelinesTab";
import { MLflowTab, mlflowConfirm } from "./environment/MLflowTab";
import { Overview, type OverviewRow } from "./environment/Overview";
import { FEATURE_PURPOSE, partTitleId } from "./environment/FeatureLayout";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
import { useMigrateStorage, type MigrateKind } from "./environment/useMigrateStorage";
import { type ProjectOption } from "./environment/ProjectSelect";
import { RunModal, runOutcome } from "./environment/SetupRun";
import { type SampleOp, mlflowSampleSteps, samplesConfirm } from "./environment/samples";
import { MLFLOW_SAMPLES_ID } from "./environment/MLflowSamples";
import { PIPELINE_SAMPLE_ID } from "./environment/PipelineSample";
import { mlflowOpenHref } from "./environment/UseIt";
import { useGoalRun, type GoalRunStep, type ReadyCheck } from "./environment/useGoalRun";
import { CleanupModal, type CleanupScope, cleanupCandidates, cleanupSteps } from "./environment/CleanupModal";
import {
  type Busy,
  type FeatureState,
  type NextStep,
  defaultModelsProject,
  defaultModelName,
  defaultProject,
  mlflowState,
  modelsState,
  type ModelsOp,
  type PlaygroundOp,
  defaultPlaygroundProject,
  playgroundIn,
  playgroundState,
  maasState,
  type MaaSOp,
  pipelineServerIn,
  pipelineServerStatus,
  pipelineServerUnmanaged,
  pipelinesState,
  setUpBlocked,
  unreadableState,
} from "./environment/featureState";
import { FeatureLoading, LoadFailed } from "./environment/Loading";
import { PrerequisitesSection, RECOMMENDED_EXPOSURE, headerWithPrerequisites, prerequisitesConfirm, prerequisitesReason, prerequisitesSteps, usePrerequisites, withPrerequisites } from "./environment/Prerequisites";
import type { ModelExposureKind, PrerequisiteFeature } from "../types";

export { nextStep, terminalReason } from "./environment/resourceRow";

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

const BUSY_REASON = "Another operation is in progress";

/** The tabs, in the URL as ?tab= (overview is the default and is left out). */
export const ENVIRONMENT_TABS = ["overview", "pipelines", "mlflow", "models", "playground", "maas"] as const;
export type EnvironmentTab = (typeof ENVIRONMENT_TABS)[number];
const TAB_TITLES: Record<EnvironmentTab, string> = { overview: "Overview", pipelines: "Pipelines", mlflow: "MLflow", models: "Models", playground: "Playground", maas: "MaaS" };
/** How many parts each feature tab lists: as many skeleton rows show while its status loads. */
const TAB_PARTS: Record<Exclude<EnvironmentTab, "overview" | "maas">, number> = { pipelines: 2, mlflow: 1, models: 2, playground: 2 };
/** The tabs with a project picker. */
const PROJECT_TABS: EnvironmentTab[] = ["pipelines", "mlflow", "models", "playground"];
/** Tabs of the earlier layout: links to them open Pipelines. */
const OLD_TABS = ["cluster", "projects"];

interface QuickResourceCreatorProps {
  /**
   * Why cluster changes are disabled now, or null: the app-wide
   * useMutationBlocker (permissions, session, the operation lock).
   */
  mutateBlocker: string | null;
  /** Why this user cannot read credentials or change anything through the updater (not a cluster-admin), or null. */
  readBlocker?: string | null;
  /** The OpenShift console's address, for links to pod logs. */
  consoleURL?: string;
  /** Sees every action result (e.g. to pick up a cluster_busy refusal). */
  onResult?: (res: OperationResponse) => void;
  /** The page header, given the refresh state of the resource status. */
  renderHeader?: (header: { loading: boolean; lastRefreshed: Date | null; onRefresh: () => void }) => React.ReactNode;
}

/** A Set up waiting for its confirmation. */
interface SetupConfirm {
  feature: string;
  title: string;
  specs: ConfirmSpec[];
  steps: GoalRunStep[];
  done: string;
  /** The feature's state on fresh data, for the run's first check. */
  recheck: (s: ResourcesStatus | null, projects: string[]) => FeatureState;
}

/**
 * The Environment page: one tab per feature (Pipelines, MLflow) after an
 * Overview of them all. The feature's state comes from one function per
 * feature (environment/featureState.ts), which the Overview and the tab
 * both render. The tab and the project are in the URL (?tab=, ?project=),
 * so links, reloads and back and forward keep the view. It loads and polls
 * the status and the projects, and runs every change in one dialog that
 * shows its progress and outcome.
 */

/** The feature a run of the dialog belongs to. */
type RunFeature = "pipelines" | "mlflow" | "models" | "playground" | "maas" | "samples" | "other";

export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ mutateBlocker, readBlocker = null, consoleURL, onResult, renderHeader }) => {
  const [resStatus, setResStatus] = useState<ResourcesStatus | null>(null);
  // The latest status, for a Set up run that waits until its parts work.
  const statusRef = useRef<ResourcesStatus | null>(null);
  statusRef.current = resStatus;
  const [resAction, setResAction] = useState<string | null>(null);
  const [actionProject, setActionProject] = useState<string | null>(null);
  // errorCode of this tab's last storage deletion that did not succeed.
  const [lastMinIOTeardown, setLastMinIOTeardown] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [projectList, setProjectList] = useState<string[] | null>(null);
  const readOnly = !!readBlocker;

  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab") ?? "";
  const tab: EnvironmentTab = (ENVIRONMENT_TABS as readonly string[]).includes(tabParam) ? (tabParam as EnvironmentTab) : OLD_TABS.includes(tabParam) ? "pipelines" : "overview";
  const urlProject = searchParams.get("project") || null;
  // project: a name; "" an explicit clear (the URL keeps an empty project=,
  // so the default does not come back); null drops it (the default applies).
  const navigate = useCallback((next: { tab?: EnvironmentTab; project?: string | null }, replace = false) => {
    setSearchParams((params) => {
      const p = new URLSearchParams(params);
      if (next.tab !== undefined) {
        if (next.tab === "overview") p.delete("tab");
        else p.set("tab", next.tab);
      }
      if (next.project !== undefined) {
        if (next.project === null) p.delete("project");
        else p.set("project", next.project);
      }
      return p;
    }, { replace });
  }, [setSearchParams]);
  // A link to the earlier layout's tabs opens Pipelines, replacing the old address.
  useEffect(() => {
    if (OLD_TABS.includes(tabParam)) navigate({ tab: "pipelines" }, true);
  }, [tabParam, navigate]);

  // "Show issue" moves the focus to the part (or a field) once its tab shows.
  const [focusTarget, setFocusTarget] = useState<string | null>(null);
  useEffect(() => {
    if (!focusTarget) return;
    const el = document.getElementById(focusTarget);
    if (el) {
      el.focus();
      el.scrollIntoView?.({ block: "center" });
      setFocusTarget(null);
    }
  }, [focusTarget, tabParam, urlProject]);

  const [resError, setResError] = useState<ApiError | null>(null);
  const [projectsError, setProjectsError] = useState<ApiError | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  // The Pipelines tab's selected project (set below, once it is known):
  // the status also reads its dashboard Connection from the Secret itself.
  const selectedPipelinesProject = useRef<string | null>(tab === "pipelines" ? urlProject : null);
  // The project the status shown was read for: until the selected project's
  // is in, its Connection shows as loading (never another project's).
  const [statusProject, setStatusProject] = useState<string | null>(null);
  const statusRequest = useRef(0);
  // The status and the projects load together, on every refresh and poll,
  // and are shown together: the first view never has part of the projects,
  // and a project deleted elsewhere leaves the picker with the next load.
  // Only the latest read is shown: an earlier one (a poll, another
  // project's) that answers later would show older data.
  const fetchResources = useCallback(async () => {
    const req = ++statusRequest.current;
    const asked = selectedPipelinesProject.current;
    setStatusLoading(true);
    const [s, p] = await Promise.allSettled([getResourcesStatus(asked), getDSProjects()]);
    if (req !== statusRequest.current) return;
    if (p.status === "fulfilled") {
      setProjectList(p.value.projects || []);
      setProjectsError(null);
    } else {
      // Keep the last list; until one is read, no project list is shown as empty.
      setProjectsError(toApiError(p.reason, "Could not load the data science projects"));
    }
    if (s.status === "fulfilled") {
      setResStatus(s.value);
      setStatusProject(asked);
      setResError(null);
      setLastRefreshed(new Date());
    } else {
      // Keep the last known state; show why it may be stale.
      setResError(toApiError(s.reason, "Could not load resource status"));
    }
    setStatusLoading(false);
  }, []);

  useEffect(() => { void fetchResources(); }, [fetchResources]);

  const minio = resStatus?.minio;
  const mlflow = resStatus?.mlflow;
  const legacy = resStatus?.legacyStorage;
  const pipelineServers = resStatus?.pipelineServers ?? [];
  const allResources = resStatus ? [resStatus.minio, resStatus.mlflow, ...pipelineServers, ...(resStatus.inferenceGateway ? [resStatus.inferenceGateway] : []), ...(resStatus.models?.items ?? [])] : [];
  // Settling: something is starting or terminating and may still change on its
  // own. Terminal states are not settling: they need a change (A08-3).
  const resourcesSettling = allResources.some((r) => isTerminating(r) || (r.deployed && !r.ready && !terminalReason(r)));

  const run = useGoalRun();
  const [runOpen, setRunOpen] = useState(false);
  // Poll quickly after a change, back off to 30 s, stop after 10 minutes or
  // once nothing is settling; hidden tabs don't poll (A06-5).
  const [pollRound, setPollRound] = useState(0);
  const [pollGaveUp, setPollGaveUp] = useState(false);
  usePolling(fetchResources, {
    enabled: !!resAction || resourcesSettling || !!run.run?.running,
    restartKey: pollRound,
    delay: ({ attempt, elapsedMs }) => {
      if (elapsedMs >= RESOURCE_SETTLE_MAX_MS) {
        setPollGaveUp(true);
        return null;
      }
      return exponentialBackoff(attempt, RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS);
    },
  });
  /** Runs one request of a run; the run's dialog shows its outcome. */
  const runAction = async (action: string, fn: () => Promise<OperationResponse>, target: string | null): Promise<OperationResponse> => {
    trackFeature(action);
    setResAction(action);
    setActionProject(target);
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    let res: OperationResponse;
    try {
      res = await fn();
    } catch (e) {
      res = errorResult(e, "Operation failed");
    }
    setResAction(null);
    setActionProject(null);
    if (action === "teardown-minio") setLastMinIOTeardown(res.success ? null : res.errorCode || "failed");
    else if (action === "setup-minio" && res.success) setLastMinIOTeardown(null);
    onResult?.(res);
    void fetchResources();
    return res;
  };

  const [migrateKind, setMigrateKind] = useState<MigrateKind | null>(null);
  const migration = useMigrateStorage(useCallback(() => { void fetchResources(); }, [fetchResources]));
  const migrating = !!migration.run?.running;

  const [setupConfirm, setSetupConfirm] = useState<SetupConfirm | null>(null);
  const [cleanupScope, setCleanupScope] = useState<CleanupScope | null>(null);

  // The sample operation running now (its rows show it), the last sample
  // run (for the run dialog's next step), and a key that changes when one
  // ends, so the tabs read the sample state again.
  const [sampleOp, setSampleOp] = useState<SampleOp | null>(null);
  const [sampleRun, setSampleRun] = useState<SampleOp | null>(null);
  const [samplesRefresh, setSamplesRefresh] = useState(0);
  // The Models operation running now (its parts show it), and the last one (for the run dialog's next step).
  const [modelsOp, setModelsOp] = useState<ModelsOp | null>(null);
  const [modelsRun, setModelsRun] = useState<ModelsOp | null>(null);
  // The Playground operation running now, the last one, and a key that reads its state again.
  const [playgroundOp, setPlaygroundOp] = useState<PlaygroundOp | null>(null);
  const [playgroundRun, setPlaygroundRun] = useState<PlaygroundOp | null>(null);
  const [playgroundRefresh, setPlaygroundRefresh] = useState(0);
  // The Playground tab's default project, once the playgrounds are known.
  const [playgroundDefault, setPlaygroundDefault] = useState<string | null>(null);
  const playgroundProjectParam = searchParams.has("project") ? urlProject : playgroundDefault;
  const playgroundRead = usePlaygroundStatus(tab === "playground" ? playgroundProjectParam : null, playgroundRefresh, !!playgroundOp, tab === "overview" || tab === "playground" || !!cleanupScope);
  const pg = playgroundRead.data;
  const [maasOp, setMaaSOp] = useState<MaaSOp | null>(null);
  // The last MaaS operation, for the run dialog's next step.
  const [maasRun, setMaaSRun] = useState<MaaSOp | null>(null);
  // Which feature the run dialog's run belongs to, set by whatever starts
  // it: the dialog's next step and words come from it (never guessed from
  // the run's title or the last runs of other features).
  const [runFeature, setRunFeature] = useState<RunFeature>("other");
  const [maasRefresh, setMaaSRefresh] = useState(0);
  const maasRead = useMaaSStatus(maasRefresh, !!maasOp, tab === "overview" || tab === "maas" || !!cleanupScope);
  const maas = maasRead.data;
  // One-click prerequisites: read for the feature tabs and the Overview;
  // Models' plan depends on how the model about to be set up is reached.
  const [prereqRefresh, setPrereqRefresh] = useState(0);
  const [modelsExposure, setModelsExposure] = useState<ModelExposureKind | null>(null);
  const prereqRead = usePrerequisites(prereqRefresh, tab === "models" ? modelsExposure : RECOMMENDED_EXPOSURE, tab !== "maas");
  const prereq = prereqRead.data;
  useEffect(() => {
    if (!searchParams.has("project")) setPlaygroundDefault(defaultPlaygroundProject(pg, null));
  }, [pg, searchParams]);
  // Refresh (and Check again) reads everything the page shows again; what
  // is shown stays until the new answers are in.
  const checkAgain = () => {
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    setPlaygroundRefresh((n) => n + 1);
    setMaaSRefresh((n) => n + 1);
    setSamplesRefresh((n) => n + 1);
    setPrereqRefresh((n) => n + 1);
    void fetchResources();
  };

  const request = (p: ResourcePending): [string, () => Promise<OperationResponse>, string | null] => {
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
  };
  const confirmOf = (p: Pending) => storageConfirm(p, minio, legacy, setPending) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, pipelineServers) ?? modelsConfirm(p, resStatus) ?? playgroundConfirm(p, pg) ?? maasConfirm(p, maas, setPending) ?? prerequisitesConfirm(p, prereq) ?? samplesConfirm(p);

  /**
   * Check again: it changes nothing on the cluster, so it runs in place,
   * without a confirmation or the run dialog; the result appears on the
   * model once the status is read again.
   */
  const [checking, setChecking] = useState<{ project: string; model: string } | null>(null);
  const [checkError, setCheckError] = useState<string | null>(null);
  const checkModel = (project: string, model: string) => {
    if (checking || run.run?.running) return;
    setChecking({ project, model });
    setCheckError(null);
    streamModels({ action: "check", project, model }, () => {}, (success, error, apiError) => {
      // A model that does not answer is the check's result, shown on the
      // model; only a check that could not run is an error here.
      if (!success && apiError?.errorCode !== "not_callable") setCheckError(error ?? apiError?.message ?? "The check could not run.");
      setChecking(null);
      void fetchResources();
    }, () => {
      setCheckError("The connection ended before the check did. Check again.");
      setChecking(null);
      void fetchResources();
    });
  };

  /** A confirmed Models change: one streamed request whose steps the run dialog shows. */
  const startModels = (p: ModelsPending) => {
    if (run.run?.running) return;
    const spec = modelsConfirm(p, resStatus);
    const op: ModelsOp = p.kind === "setup-models"
      ? { action: "setup", gateway: p.gateway, project: p.project, model: p.project ? p.model : null }
      : p.kind === "teardown-model" ? { action: "teardown", gateway: false, project: p.project, model: p.model }
      : p.kind === "allow-gateway-project" ? { action: "allow", gateway: true, project: null, model: null, allowProject: p.project }
      : p.kind === "disallow-gateway-project" ? { action: "disallow", gateway: true, project: null, model: null, allowProject: p.project }
      : p.kind === "check-model" ? { action: "check", gateway: false, project: p.project, model: p.model }
      : { action: "teardown", gateway: true, project: null, model: null };
    setPending(null);
    setSampleRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setModelsOp(op);
    setModelsRun(op);
    setRunFeature("models");
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    const end = () => {
      setModelsOp(null);
      void fetchResources();
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, modelsSteps(p), (onStep, onDone, onDetach) => streamModels(op.action === "allow" || op.action === "disallow" ? { action: op.action, project: op.allowProject } : op.action === "check" ? { action: "check", project: op.project!, model: op.model! } : {
      action: op.action,
      ...(op.gateway ? { gateway: true } : {}),
      ...(op.project ? { project: op.project } : {}),
      ...(op.project && op.model ? { model: op.model } : {}),
      ...(p.kind === "setup-models" && p.releasedImage ? { releasedImage: true } : {}),
      ...(p.kind === "setup-models" && p.exposure ? { exposure: p.exposure } : {}),
      ...(p.kind === "setup-models" && p.gatewayRef ? { gatewayRef: p.gatewayRef } : {}),
    }, onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }), op.action === "teardown" || op.action === "disallow" ? "delete" : op.action === "check" ? "change" : "setup");
  };

  /** A confirmed Playground change: one streamed request whose steps the run dialog shows. */
  const startPlayground = (p: PlaygroundPending) => {
    if (run.run?.running) return;
    const spec = playgroundConfirm(p, pg);
    const op: PlaygroundOp = { action: p.kind === "setup-playground" ? "setup" : "teardown", project: p.project };
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setRunFeature("playground");
    setPlaygroundOp(op);
    setPlaygroundRun(op);
    const end = () => {
      setPlaygroundOp(null);
      setPlaygroundRefresh((n) => n + 1);
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, playgroundSteps(p), (onStep, onDone, onDetach) => streamPlayground({
      action: op.action,
      project: op.project,
      ...(p.kind === "setup-playground" && p.models?.length ? { models: p.models } : {}),
    }, onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }), op.action === "setup" ? "setup" : "delete");
  };

  /** A confirmed MaaS change: one streamed request whose steps the run dialog shows. */
  const startMaaS = (p: MaaSPending) => {
    if (run.run?.running) return;
    const spec = maasConfirm(p, maas);
    const req = maasRequestOf(p, maas);
    const { action } = req;
    const project = req.project ?? null;
    const model = req.model ?? null;
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setMaaSOp({ action, project, model });
    setMaaSRun({ action, project, model });
    setRunFeature("maas");
    const end = () => {
      setMaaSOp(null);
      setMaaSRefresh((n) => n + 1);
      void fetchResources();
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, maasSteps(p), (onStep, onDone, onDetach) => streamMaaS(req,
      onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }),
    action === "teardown" || action === "unpublish" || action === "remove-tier-policies" ? "delete" : "setup");
  };

  /** A confirmed prerequisites setup: one streamed request whose steps the run dialog shows. */
  const startPrerequisites = (p: PrerequisitesPending) => {
    if (run.run?.running) return;
    const spec = prerequisitesConfirm(p, prereq);
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setRunFeature("other");
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    const end = () => {
      setPrereqRefresh((n) => n + 1);
      void fetchResources();
    };
    run.follow(spec?.runTitle ?? p.kind, prerequisitesSteps(p, prereq), (onStep, onDone, onDetach) => streamPrerequisites({ feature: p.feature, ...(p.exposure ? { exposure: p.exposure } : {}) },
      onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }), "setup");
  };

  /**
   * A confirmed sample change: one streamed request (one rate-limit slot on
   * the server) whose steps the run dialog shows; when it ends, the tab
   * reads the samples again.
   */
  const startSample = (p: SamplePending) => {
    if (run.run?.running) return;
    const spec = samplesConfirm(p);
    const op: SampleOp = { feature: "mlflow", project: p.project, action: p.action, samples: p.samples };
    setPending(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setSampleOp(op);
    setSampleRun(op);
    setRunFeature("samples");
    const end = () => {
      setSampleOp(null);
      setSamplesRefresh((n) => n + 1);
    };
    const steps = mlflowSampleSteps(p.action, p.samples);
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, steps, (onStep, onDone, onDetach) => {
      const done: StreamDoneHandler = (success, error, apiError) => { end(); onDone(success, error, apiError); };
      const detach: StreamDetachHandler = (reason) => { end(); onDetach(reason); };
      return streamMLflowSamples({ project: p.project, action: p.action, samples: p.samples }, onStep, done, detach);
    }, p.action === "remove" ? "delete" : "change");
  };

  /** A single confirmed change: a run of one step, in the run dialog. */
  const execute = (p: Pending) => {
    if (isSamplePending(p)) {
      startSample(p);
      return;
    }
    if (isModelsPending(p)) {
      startModels(p);
      return;
    }
    if (isPlaygroundPending(p)) {
      startPlayground(p);
      return;
    }
    if (isMaaSPending(p)) {
      startMaaS(p);
      return;
    }
    if (isPrerequisitesPending(p)) {
      startPrerequisites(p);
      return;
    }
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    const spec = confirmOf(p);
    const [action, fn, target] = request(p);
    const title = spec?.runTitle ?? spec?.title ?? action;
    setPending(null);
    setRunOpen(true);
    const kind = p.kind.startsWith("teardown") ? "delete" : "change";
    setRunFeature("other");
    void run.start(title, [{ id: p.kind, title, run: () => runAction(action, fn, target) }], { kind });
  };

  /** Set up: confirm every change, then check every part's prerequisites, then run the missing parts in order. */
  const setUp = (feature: string, setup: Pending[]) => {
    // Models is one streamed operation: its own confirmation, then its run.
    const models = setup.find(isModelsPending);
    if (models) {
      setPending(models);
      return;
    }
    const playground = setup.find(isPlaygroundPending);
    if (playground) {
      setPending(playground);
      return;
    }
    const maasSetup = setup.find(isMaaSPending);
    if (maasSetup) {
      setPending(maasSetup);
      return;
    }
    const prereqSetup = setup.find(isPrerequisitesPending);
    if (prereqSetup) {
      setPending(prereqSetup);
      return;
    }
    const specs: ConfirmSpec[] = [];
    const steps: GoalRunStep[] = [];
    for (const p of setup) {
      if (isSamplePending(p) || isModelsPending(p) || isPlaygroundPending(p) || isMaaSPending(p) || isPrerequisitesPending(p)) continue;
      const spec = confirmOf(p);
      if (spec) specs.push(spec);
      const [action, fn, target] = request(p);
      const title = spec?.runTitle ?? action;
      // The request returns once the part is created; the step waits until
      // the status the page keeps loading reports it working.
      const waitReady: GoalRunStep["waitReady"] = p.kind === "setup-pipeline-server"
        ? { check: () => readyCheck("pipeline", pipelineServerIn(statusRef.current, target!)), typical: "1 to 3 minutes" }
        : p.kind === "setup-mlflow"
          ? { check: () => readyCheck("mlflow", statusRef.current?.mlflow), typical: "1 to 3 minutes" }
          : { check: () => readyCheck("minio", statusRef.current?.minio), typical: "under a minute" };
      const readyMessage = p.kind === "setup-minio" ? "The S3 storage is ready." : p.kind === "setup-mlflow" ? "MLflow is ready." : `The pipeline server in ${target} is ready.`;
      steps.push({ id: p.kind, title, run: () => runAction(action, fn, target), waitReady, readyMessage });
    }
    const project = setup.flatMap((p) => (p.kind === "setup-pipeline-server" ? [p.project] : []))[0];
    const done = project ? `Pipelines are ready in ${project}.` : `${feature} is ready.`;
    const title = steps.length === 1 ? steps[0].title : project ? `Set up ${feature} in ${project}` : `Set up ${feature}`;
    const recheck = (s: ResourcesStatus | null, projects: string[]) => (feature === "MLflow" ? mlflowState(s) : pipelinesState(s, project ?? null, projects));
    if (steps.length > 0) setSetupConfirm({ feature, title, specs, steps, done, recheck });
  };

  const startCleanup = (req: CleanupRequest) => {
    setCleanupScope(null);
    setRunFeature("other");
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature("cleanup");
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    run.follow("Clean up", cleanupSteps(req), (onStep, onDone, onDetach) => streamCleanup(req, onStep, (success, error, apiError) => {
      void fetchResources();
      setPlaygroundRefresh((n) => n + 1);
      setMaaSRefresh((n) => n + 1);
      onDone(success, error, apiError);
    }, (reason) => {
      void fetchResources();
      setPlaygroundRefresh((n) => n + 1);
      onDetach(reason);
    }));
  };

  // A run that ends while its dialog is closed reports in a toast, with a
  // way back to the dialog: the outcome is never lost, and nothing pushes
  // the page down.
  const [toast, setToast] = useState(false);
  const wasRunning = useRef(false);
  useEffect(() => {
    const now = !!run.run?.running;
    if (wasRunning.current && !now && !runOpen) setToast(true);
    wasRunning.current = now;
  }, [run.run?.running, runOpen]);

  const runningSomething = !!resAction || migrating || !!run.run?.running || !!checking;
  const busy: Busy = { resAction, actionProject, migrating, models: modelsOp, playground: playgroundOp, maas: maasOp };
  const baseReason = mutateBlocker ?? (runningSomething ? BUSY_REASON : null);
  // After a deletion the namespace stays (deployed:false, managedByTool:true).
  // Offer Delete again while tool objects are still reported (the data
  // PVC, a Route) or the last deletion from this tab did not finish.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute ||
    (lastMinIOTeardown !== null && ["partial_failure", "delete_failed", "in_progress", "forbidden", "unauthorized", "network"].includes(lastMinIOTeardown))
  );
  const modal = pending ? confirmOf(pending) : null;

  // Projects: the data science projects, and any project the status names.
  const projectNames = [...new Set([
    ...(projectList ?? []),
    ...pipelineServers.map((ps) => ps.namespace ?? ""),
    ...(resStatus?.unmanagedPipelineServers ?? []).map((ps) => ps.namespace ?? ""),
  ].filter(Boolean))].sort();
  // A project in the address that is gone (deleted elsewhere) is not
  // chosen: Set up would create it again.
  const missingProject = urlProject && projectList !== null && !projectNames.includes(urlProject) ? urlProject : null;
  // One state for the picker and the content: the URL's project; an empty
  // project= after a clear means none; only without project= at all (a
  // first visit) the default applies.
  const project = missingProject ? null : searchParams.has("project") ? urlProject : defaultProject(resStatus, projectNames, null);
  const pipelinesProject = tab === "pipelines" ? project : null;
  useEffect(() => {
    if (selectedPipelinesProject.current === pipelinesProject) return;
    selectedPipelinesProject.current = pipelinesProject;
    if (pipelinesProject) void fetchResources();
  }, [pipelinesProject, fetchResources]);
  const projectOptions: ProjectOption[] = projectNames.map((name) => {
    const status = pipelineServerStatus(resStatus, name, busy);
    const foreign = pipelineServerUnmanaged(resStatus, name);
    const has = !!pipelineServerIn(resStatus, name) || foreign;
    return { name, description: has ? `Pipeline server: ${envStatusText(status)}${foreign ? " · not managed by this tool" : ""}` : undefined };
  });
  const workspaceOptions: ProjectOption[] = projectNames.map((name) => ({ name }));
  const modelOptions: ProjectOption[] = projectNames.map((name) => {
    const here = (resStatus?.models?.items ?? []).filter((i) => i.namespace === name);
    const ready = here.filter((m) => m.ready).length;
    const words = here.length === 0 ? undefined : here.length === 1
      ? `Model: ${here[0].name} ${here[0].ready ? "ready" : here[0].terminating ? "deleting" : "not ready"}${here[0].managedByTool === false ? " · not managed by this tool" : ""}`
      : `Models: ${here.length}, ${ready} ready`;
    return { name, description: words };
  });
  // The Models tab's own default: the first project with a model, preferring a ready one.
  const modelsProject = missingProject ? null : searchParams.has("project") ? urlProject : defaultModelsProject(resStatus, projectNames, null);
  // The Models tab's model: the URL's (model=<catalog name>), else the project's first, else the default.
  const modelsModel = defaultModelName(resStatus, modelsProject, searchParams.get("model"));
  const chooseModel = (name: string) => setSearchParams((params) => {
    const next = new URLSearchParams(params);
    next.set("model", name);
    return next;
  });

  // After an explicit clear the tab says so instead of the cross-project
  // summary, which names a project the picker no longer shows.
  const cleared = (searchParams.has("project") && !project) || !!missingProject;
  const pipelines: FeatureState = cleared
    ? { status: "unknown", label: "No project chosen", next: { kind: "choose-project" }, project: null }
    : pipelinesState(resStatus, project, projectNames, busy);
  const pipelinesSummary = pipelinesState(resStatus, null, projectNames, busy);
  const mlflowNow = mlflowState(resStatus, busy);
  const models: FeatureState = cleared
    ? { status: "unknown", label: "No project chosen", next: { kind: "choose-project" }, project: null }
    : modelsState(resStatus, modelsProject, projectNames, busy, modelsModel);
  const modelsSummary = modelsState(resStatus, null, projectNames, busy);
  const playgroundProject = missingProject ? null : playgroundProjectParam;
  const playgroundNow: FeatureState = cleared
    ? { status: "unknown", label: "No project chosen", next: { kind: "choose-project" }, project: null }
    : !pg && playgroundRead.error ? unreadableState(playgroundProject) : playgroundState(resStatus, pg, playgroundProject, busy);
  // A status that was never read is loading; one that could not be read says so.
  const playgroundSummary = !pg && playgroundRead.error ? unreadableState() : playgroundState(resStatus, pg, null, busy);
  const maasNow = !maas && maasRead.error ? unreadableState() : maasState(maas, busy);
  const playgroundOptions: ProjectOption[] = projectNames.map((name) => {
    const i = playgroundIn(pg, name);
    return { name, description: i?.deployed ? `Playground: ${i.ready ? "Ready" : i.terminating ? "Deleting" : "Not ready"}${i.managedByTool ? "" : " · not managed by this tool"}` : undefined };
  });
  // Once a feature works, its samples are the natural next step.
  const pipelinesSamples = pipelinesSummary.status === "ready" && !!pipelinesSummary.project;
  const prereqFor = (feature: PrerequisiteFeature) => prereq?.features?.[feature];
  const overviewRows: OverviewRow[] = [
    {
      id: "pipelines", name: "Pipelines", gives: "Run data science pipelines; artifacts in S3", state: withPrerequisites(pipelinesSummary, prereqFor("pipelines"), "pipelines"),
      href: `?tab=pipelines${pipelinesSummary.project ? `&project=${encodeURIComponent(pipelinesSummary.project)}` : ""}`,
      ...(pipelinesSamples ? { samples: { label: "Run a sample pipeline", focus: PIPELINE_SAMPLE_ID } } : {}),
    },
    {
      id: "mlflow", name: "MLflow", gives: "Track experiments, runs and models", state: withPrerequisites(mlflowNow, prereqFor("mlflow"), "mlflow"), href: "?tab=mlflow",
      ...(mlflowNow.status === "ready" ? { samples: { label: "Add sample data", focus: MLFLOW_SAMPLES_ID } } : {}),
    },
    {
      id: "models", name: "Models", gives: "Serve a small chat model on CPU; call it or use the playground", state: withPrerequisites(modelsSummary, prereqFor("models"), "models"),
      href: `?tab=models${modelsSummary.project ? `&project=${encodeURIComponent(modelsSummary.project)}` : ""}`,
    },
    {
      id: "playground", name: "Playground", gives: "Chat with a project's models in the dashboard's Gen AI playground", state: withPrerequisites(playgroundSummary, prereqFor("playground"), "playground"),
      href: `?tab=playground${playgroundSummary.project ? `&project=${encodeURIComponent(playgroundSummary.project)}` : ""}`,
    },
    {
      id: "maas", name: "MaaS", gives: "Offer models to everyone on the cluster with API keys and token limits", state: maasNow, href: "?tab=maas",
    },
  ];
  const anythingToClean = !!resStatus && (() => {
    const c = cleanupCandidates(resStatus, "all", minioLeftovers, pg, maas);
    return c.servers.length > 0 || c.storage || c.mlflow || c.models.length > 0 || c.gateway || c.playgrounds.length > 0 || c.maasPublished.length > 0 || c.maasPlatform;
  })();

  // The run's "what next" and main action, from the feature it set up.
  // A sample run: after adding MLflow samples, open the project's workspace.
  const sampleNext: NextStep | null = sampleRun?.feature === "mlflow" && sampleRun.action !== "remove" && mlflow?.uiRoute
    ? { kind: "open", label: `Open ${sampleRun.project} in MLflow`, href: mlflowOpenHref(resStatus?.dashboardUrl, mlflow.uiRoute, sampleRun.project) }
    : null;
  const modelsNext = modelsRun?.project ? modelsState(resStatus, modelsRun.project, projectNames, busy, modelsRun.model ?? undefined).next : null;
  const playgroundNext = playgroundRun ? playgroundState(resStatus, pg, playgroundRun.project, busy).next : null;
  const maasNext = runFeature === "maas" && maasNow.next?.kind === "open" ? maasNow.next : null;
  const runNext: NextStep | null = run.run?.kind === "setup"
    ? (runFeature === "playground" ? playgroundNext : runFeature === "models" ? modelsNext : runFeature === "mlflow" ? mlflowNow.next : runFeature === "maas" ? maasNext : runFeature === "pipelines" ? pipelines.next : null)
    : runFeature === "samples" ? sampleNext : null;
  const whatNext = run.run?.kind === "setup" && runFeature === "maas"
    ? <Content component="p">{maasRun?.action === "publish"
      ? `On the MaaS tab, Use it mints an API key (shown once) and calls ${maasRun.project ? `${maasRun.model ?? "the model"} in ${maasRun.project}` : "the model"} with it. The dashboard's Models as a Service pages list it with its subscription.`
      : "On the MaaS tab, publish a model to offer it through MaaS: callers then use an API key instead of their OpenShift token, within the subscription's token limits."}</Content>
    : run.run?.kind === "setup" && runFeature === "playground"
    ? <Content component="p">{`Open the playground in ${playgroundRun?.project}, choose a model and ask it something. On the Playground tab, Use it also has a chat request to run from a terminal.`}</Content>
    : run.run?.kind === "setup" && runFeature === "models" && modelsRun?.action === "allow"
    ? <Content component="p">{`The models in ${modelsRun.allowProject} attach to the inference gateway within a few seconds, and the dashboard shows them as it shows any other model.`}</Content>
    : run.run?.kind === "setup" && runFeature === "models"
    ? <Content component="p">{modelsRun?.project
      ? `On the Models tab, Use it has the model's address and commands that call it with your OpenShift token. In the dashboard, ${modelsRun.project} lists the model, and Gen AI studio offers it in the playground.`
      : "Choose a project on the Models tab and set up a model there: it routes through this gateway."}</Content>
    : run.run?.kind === "setup" && (runFeature === "mlflow" || runFeature === "pipelines")
    ? <Content component="p">{runFeature === "mlflow"
      ? "Open MLflow workspaces to choose a project and see its experiments. On the MLflow tab, Sample data adds example experiments, traces and a model to a project in one click."
      : `${runNext?.kind === "open" ? runNext.label : "Open pipelines"} to import a pipeline and create a run. The Pipelines tab has a sample pipeline to check that runs work, with the commands that run it with your own token.`}</Content>
    : runFeature === "samples" && sampleRun && sampleRun.action !== "remove"
      ? <Content component="p">{`Open ${sampleRun.project} in MLflow to see the sample data; the dashboard shows it on the pages each sample names.`}</Content>
      : undefined;
  const outcome = run.run ? runOutcome(run.run) : null;

  const startSetUp = () => {
    if (!setupConfirm) return;
    const { feature, title, steps, done, recheck } = setupConfirm;
    setSetupConfirm(null);
    setRunFeature(feature === "MLflow" ? "mlflow" : "pipelines");
    setSampleRun(null);
    setRunOpen(true);
    void run.start(title, steps, {
      kind: "setup",
      doneMessage: done,
      // Every part's prerequisites on the latest status, before anything changes.
      preflight: async () => {
        const asked = selectedPipelinesProject.current;
        const fresh = await getResourcesStatus(asked);
        setResStatus(fresh);
        setStatusProject(asked);
        statusRef.current = fresh;
        return setUpBlocked(recheck(fresh, projectNames));
      },
    });
  };

  // One-click prerequisites on each feature's tab, above its parts.
  const prereqNode = (feature: PrerequisiteFeature, exposure?: ModelExposureKind | null) => (
    <PrerequisitesSection feature={feature} prereq={prereq?.features?.[feature]} error={prereqRead.error} onRetry={() => void prereqRead.reload()}
      exposure={exposure ?? undefined} stale={feature === "models" && prereqRead.stale} readOnly={readOnly} baseReason={baseReason} setPending={setPending} />
  );
  const onModelsExposure = useCallback((e: ModelExposureKind | null) => setModelsExposure(e), []);

  // Rows still loading (the status, the playground's, MaaS's) show Skeletons.
  const overview = (
    <Overview rows={overviewRows} baseReason={baseReason} setUpReason={readOnly ? READ_ONLY_SETUP : null}
      onSetUp={(row, setup) => setUp(row.name, setup)}
      onShowIssue={(row, part) => { navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) }); setFocusTarget(partTitleId(part)); }}
      onOpenTab={(row) => { navigate({ tab: row.id as EnvironmentTab, project: "" }); setFocusTarget(`${row.id}-project`); }}
      onMigrate={() => setMigrateKind("migrate")}
      onSamples={(row, focus) => { navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) }); setFocusTarget(focus); }}
      onCleanup={!readOnly && anythingToClean ? () => setCleanupScope("all") : null} />
  );

  return (
    <>
      {renderHeader?.({ loading: statusLoading, lastRefreshed, onRefresh: checkAgain })}
      <PageSection type="tabs">
        <Tabs activeKey={tab} onSelect={(_e, k) => navigate({ tab: k as EnvironmentTab })} usePageInsets aria-label="Environment features">
          {ENVIRONMENT_TABS.map((key) => (
            <Tab key={key} eventKey={key} id={`environment-tab-${key}`} title={<TabTitleText>{TAB_TITLES[key]}</TabTitleText>} tabContentId="environment-tab-content" />
          ))}
        </Tabs>
      </PageSection>
      {/*
        Only the active tab is rendered: leaving a tab clears what it
        fetched on request (the storage's keys). Until a tab's status is
        read, it shows where its data will be (Skeletons), never an empty
        or "not set up" state; a status that cannot be read shows why,
        with Retry.
      */}
      <PageSection isFilled id="environment-tab-content" role="tabpanel" aria-labelledby={`environment-tab-${tab}`}>
        <Stack hasGutter>
          {resError && resStatus && tab !== "maas" && (
            <StackItem>
              <Alert variant="warning" title="Resource status may be out of date" isInline component="p"
                actionLinks={<AlertActionLink onClick={checkAgain}>Retry</AlertActionLink>}>
                <TruncatedText>The data below is from the last successful refresh. {resError.message}</TruncatedText>
              </Alert>
            </StackItem>
          )}
          {projectsError && resStatus && PROJECT_TABS.includes(tab) && (
            <StackItem>
              <Alert variant="warning" title="Cannot load the data science projects" isInline component="p"
                actionLinks={<AlertActionLink onClick={checkAgain}>Retry</AlertActionLink>}>
                <TruncatedText>{projectList ? "The project list is from the last successful refresh. " : "Until it loads, the project picker lists only the projects with a pipeline server. "}{projectsError.message}</TruncatedText>
              </Alert>
            </StackItem>
          )}
          {tab === "overview" && !pg && playgroundRead.error && (
            <StackItem><LoadFailed title="Cannot read the playground" error={playgroundRead.error} onRetry={() => void playgroundRead.reload()} /></StackItem>
          )}
          {tab === "overview" && !maas && maasRead.error && (
            <StackItem><LoadFailed title="Cannot read Models as a Service" error={maasRead.error} onRetry={() => void maasRead.reload()} /></StackItem>
          )}
          {pollGaveUp && resourcesSettling && (
            <StackItem>
              <Alert variant="info" title="Stopped checking automatically after 10 minutes" isInline component="p"
                actionLinks={<AlertActionLink onClick={checkAgain}>Check again</AlertActionLink>} />
            </StackItem>
          )}
          {!resStatus && tab !== "maas" ? (
            <StackItem>
              {resError ? <PageErrorState inSection={false} error={resError} title="Cannot load the environment" onRetry={checkAgain} />
                : tab === "overview" ? overview
                  : <FeatureLoading title={TAB_TITLES[tab]} purpose={FEATURE_PURPOSE[tab]} parts={TAB_PARTS[tab]} />}
            </StackItem>
          ) : (
            <StackItem>
              {tab === "overview" && overview}
              {tab === "pipelines" && resStatus && (
                <PipelinesTab prerequisites={prereqNode("pipelines")} status={resStatus} project={project} projects={projectOptions} projectsLoaded={projectList !== null}
                  onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })} missingProject={missingProject}
                  connectionLoading={!!project && statusProject !== project}
                  state={headerWithPrerequisites(pipelines, prereq?.features?.pipelines)} prerequisitesReason={prerequisitesReason(prereq?.features?.pipelines, "pipelines")} busy={busy} baseReason={baseReason} readOnly={readOnly} readReason={readBlocker} minioLeftovers={minioLeftovers} consoleURL={consoleURL}
                  setPending={setPending} onMigrate={() => setMigrateKind("migrate")} onRemoveOldStorage={() => setMigrateKind("remove")}
                  onSetUp={(setup) => setUp("Pipelines", setup)} onCleanup={() => setCleanupScope("pipelines")}
                  sampleOp={sampleOp} samplesRefresh={samplesRefresh} />
              )}
              {tab === "mlflow" && resStatus && (
                <MLflowTab prerequisites={prereqNode("mlflow")} status={resStatus} state={headerWithPrerequisites(mlflowNow, prereq?.features?.mlflow)} prerequisitesReason={prerequisitesReason(prereq?.features?.mlflow, "mlflow")} busy={busy} baseReason={baseReason} readOnly={readOnly} consoleURL={consoleURL}
                  project={project} projects={workspaceOptions} projectsLoaded={projectList !== null}
                  onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })}
                  setPending={setPending} onSetUp={(setup) => setUp("MLflow", setup)} onCleanup={() => setCleanupScope("mlflow")}
                  sampleOp={sampleOp} samplesRefresh={samplesRefresh} />
              )}
              {tab === "models" && resStatus && (
                <ModelsTab prerequisites={prereqNode("models", modelsExposure)} prerequisitesReason={prereqRead.stale ? "Checking what this way to reach the model needs on the cluster..." : prerequisitesReason(prereq?.features?.models, "models")} onExposure={onModelsExposure} status={resStatus} project={modelsProject} projects={modelOptions} projectsLoaded={projectList !== null}
                  onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })} missingProject={missingProject}
                  model={modelsModel} onModel={chooseModel}
                  state={headerWithPrerequisites(models, prereq?.features?.models)} busy={busy} baseReason={baseReason} readOnly={readOnly} consoleURL={consoleURL}
                  setPending={setPending} onSetUp={(setup) => setUp("Models", setup)} onCleanup={() => setCleanupScope("models")}
                  onMaaS={() => navigate({ tab: "maas" })} onCheckModel={checkModel} checking={checking} checkError={checkError} />
              )}
              {tab === "playground" && resStatus && (
                <PlaygroundTab prerequisites={prereqNode("playground")} status={resStatus} playground={pg} playgroundError={playgroundRead.error} onRetry={() => void playgroundRead.reload()}
                  project={playgroundProject} projects={playgroundOptions} projectsLoaded={projectList !== null}
                  onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })} missingProject={missingProject}
                  state={headerWithPrerequisites(playgroundNow, prereq?.features?.playground)} prerequisitesReason={prerequisitesReason(prereq?.features?.playground, "playground")} busy={busy} baseReason={baseReason} readOnly={readOnly}
                  setPending={setPending} onSetUp={(setup) => setUp("Playground", setup)} onCleanup={() => setCleanupScope("playground")}
                  onModels={() => navigate({ tab: "models", ...(playgroundProject ? { project: playgroundProject } : {}) })} />
              )}
              {tab === "maas" && (
                <MaaSTab maas={maas} maasError={maasRead.error} onRetry={() => void maasRead.reload()} state={maasNow} busy={busy} baseReason={baseReason} readOnly={readOnly}
                  setPending={setPending} onSetUp={(setup) => setUp("MaaS", setup)} onModels={() => navigate({ tab: "models" })} />
              )}
            </StackItem>
          )}
        </Stack>
      </PageSection>

      {migrateKind && (
        <MigrateStorageModal
          isOpen
          kind={migrateKind}
          legacy={legacy}
          run={migration.run}
          baseReason={migrating ? null : mutateBlocker ?? (resAction || run.run?.running ? BUSY_REASON : null)}
          onStart={(kind, deleteNamespace) => migration.start(kind, kind === "remove" ? { removeOldStorage: true, deleteNamespace } : {})}
          onClose={() => {
            setMigrateKind(null);
            migration.reset();
          }}
        />
      )}

      {modal && (
        <ConfirmActionModal
          isOpen
          title={modal.title}
          summary={modal.summary}
          changes={modal.changes}
          dataLoss={modal.dataLoss}
          choice={modal.choice}
          confirmLabel={modal.confirm}
          confirmVariant={modal.danger ? "danger" : "primary"}
          confirmDisabled={modal.confirmDisabled}
          changesOpen={modal.changesOpen}
          onConfirm={() => pending && execute(pending)}
          onCancel={() => setPending(null)}
        >
          {modal.extra}
        </ConfirmActionModal>
      )}

      {setupConfirm && (
        <ConfirmActionModal
          isOpen
          title={`${setupConfirm.title}?`}
          summary={<Stack hasGutter>{setupConfirm.specs.map((s, i) => <StackItem key={i}>{s.summary}</StackItem>)}</Stack>}
          changes={setupConfirm.specs.flatMap((s) => s.changes)}
          confirmLabel="Set up"
          onConfirm={startSetUp}
          onCancel={() => setSetupConfirm(null)}
        >
          {setupConfirm.steps.length > 1 && <Content component="p">The parts are set up one after another, after a check that each can be. A part that fails stops the rest.</Content>}
        </ConfirmActionModal>
      )}

      {cleanupScope && resStatus && (
        <CleanupModal scope={cleanupScope} status={resStatus} playground={pg} maas={maas} minioLeftovers={minioLeftovers} baseReason={baseReason}
          restorable={Object.values(prereq?.features ?? {}).flatMap((f) => f?.restorable ?? [])}
          onConfirm={startCleanup} onCancel={() => setCleanupScope(null)} />
      )}

      <RunModal run={run.run} isOpen={runOpen} open={runNext} whatNext={whatNext}
        onClose={() => { setRunOpen(false); if (!run.run?.running) run.reset(); }} />

      <AlertGroup isToast isLiveRegion>
        {toast && run.run && outcome && (
          <Alert variant={outcome.variant} title={outcome.variant === "danger" ? outcome.title : `${run.run.title}: ${outcome.title}`}
            timeout={outcome.variant === "success" ? 8000 : false} onTimeout={() => setToast(false)}
            actionClose={<AlertActionCloseButton onClose={() => setToast(false)} />}
            actionLinks={<AlertActionLink onClick={() => { setToast(false); setRunOpen(true); }}>Show details</AlertActionLink>} />
        )}
      </AlertGroup>
    </>
  );
};
