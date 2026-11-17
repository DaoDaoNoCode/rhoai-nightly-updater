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
  streamPipelineSample,
  toApiError,
  type StreamDoneHandler,
  type StreamDetachHandler,
  type CleanupRequest,
} from "../services/api";
import { exponentialBackoff, usePolling } from "../hooks/usePolling";
import { RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS, RESOURCE_SETTLE_MAX_MS } from "../constants";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { TruncatedText } from "./LongText";
import { PageLoading } from "./PageStates";
import { errorResult } from "../outcomes";
import { type ConfirmSpec, type Pending, type ResourcePending, type SamplePending, isSamplePending, isTerminating, nextStep, problemTitle, terminalReason, type ResourceKind } from "./environment/resourceRow";
import { envStatusText } from "./environment/resourceStatus";
import { PipelinesTab, READ_ONLY_SETUP, pipelineServerConfirm, storageConfirm } from "./environment/PipelinesTab";
import { MLflowTab, mlflowConfirm } from "./environment/MLflowTab";
import { Overview, type OverviewRow } from "./environment/Overview";
import { partTitleId } from "./environment/FeatureLayout";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
import { useMigrateStorage, type MigrateKind } from "./environment/useMigrateStorage";
import { type ProjectOption } from "./environment/ProjectSelect";
import { RunModal, runOutcome } from "./environment/SetupRun";
import { type SampleOp, mlflowSampleSteps, pipelineSampleSteps, samplesConfirm } from "./environment/samples";
import { MLFLOW_SAMPLES_ID } from "./environment/MLflowSamples";
import { PIPELINE_SAMPLE_ID } from "./environment/PipelineSample";
import { mlflowWorkspaceHref } from "./environment/UseIt";
import { useGoalRun, type GoalRunStep, type ReadyCheck } from "./environment/useGoalRun";
import { CleanupModal, type CleanupScope, cleanupCandidates, cleanupSteps } from "./environment/CleanupModal";
import {
  type Busy,
  type FeatureState,
  type NextStep,
  defaultProject,
  mlflowState,
  pipelineServerIn,
  pipelineServerStatus,
  pipelineServerUnmanaged,
  pipelinesState,
  setUpBlocked,
} from "./environment/featureState";

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
export const ENVIRONMENT_TABS = ["overview", "pipelines", "mlflow"] as const;
export type EnvironmentTab = (typeof ENVIRONMENT_TABS)[number];
const TAB_TITLES: Record<EnvironmentTab, string> = { overview: "Overview", pipelines: "Pipelines", mlflow: "MLflow" };
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

  const [resError, setResError] = useState<string | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const refreshProjects = useCallback(async () => {
    try {
      const r = await getDSProjects();
      setProjectList(r.projects || []);
    } catch {
      setProjectList((l) => l ?? []);
    }
  }, []);
  // The status and the projects load together, on every refresh and poll:
  // a project deleted elsewhere leaves the picker with the next load.
  const fetchResources = useCallback(async () => {
    setStatusLoading(true);
    const projects = refreshProjects();
    try {
      const s = await getResourcesStatus();
      setResStatus(s);
      setResError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      // Keep the last known state; show why it may be stale.
      setResError(toApiError(e, "Could not load resource status").message);
    } finally {
      await projects;
      setStatusLoading(false);
    }
  }, [refreshProjects]);

  useEffect(() => { void fetchResources(); }, [fetchResources]);

  const minio = resStatus?.minio;
  const mlflow = resStatus?.mlflow;
  const legacy = resStatus?.legacyStorage;
  const pipelineServers = resStatus?.pipelineServers ?? [];
  const allResources = resStatus ? [resStatus.minio, resStatus.mlflow, ...pipelineServers] : [];
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
  const checkAgain = () => {
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    void fetchResources();
  };

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
      case "teardown-pipeline-server": return ["teardown-pipeline-server", () => teardownPipelineServer(p.project), p.project];
    }
  };
  const confirmOf = (p: Pending) => storageConfirm(p, minio, legacy, setPending) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, pipelineServers) ?? samplesConfirm(p);

  /**
   * A confirmed sample change: one streamed request (one rate-limit slot on
   * the server) whose steps the run dialog shows; when it ends, the tab
   * reads the samples again.
   */
  const startSample = (p: SamplePending) => {
    if (run.run?.running) return;
    const spec = samplesConfirm(p);
    const op: SampleOp = p.kind === "mlflow-samples"
      ? { feature: "mlflow", project: p.project, action: p.action, samples: p.samples }
      : { feature: "pipeline", project: p.project, action: p.action, samples: [] };
    setPending(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setSampleOp(op);
    setSampleRun(op);
    const end = () => {
      setSampleOp(null);
      setSamplesRefresh((n) => n + 1);
    };
    const steps = p.kind === "mlflow-samples" ? mlflowSampleSteps(p.action, p.samples) : pipelineSampleSteps(p.action);
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, steps, (onStep, onDone, onDetach) => {
      const done: StreamDoneHandler = (success, error, apiError) => { end(); onDone(success, error, apiError); };
      const detach: StreamDetachHandler = (reason) => { end(); onDetach(reason); };
      return p.kind === "mlflow-samples"
        ? streamMLflowSamples({ project: p.project, action: p.action, samples: p.samples }, onStep, done, detach)
        : streamPipelineSample({ project: p.project, action: p.action }, onStep, done, detach);
    }, p.action === "remove" ? "delete" : "change");
  };

  /** A single confirmed change: a run of one step, in the run dialog. */
  const execute = (p: Pending) => {
    if (isSamplePending(p)) {
      startSample(p);
      return;
    }
    setSampleRun(null);
    const spec = confirmOf(p);
    const [action, fn, target] = request(p);
    const title = spec?.runTitle ?? spec?.title ?? action;
    setPending(null);
    setRunOpen(true);
    const kind = p.kind.startsWith("teardown") ? "delete" : "change";
    void run.start(title, [{ id: p.kind, title, run: () => runAction(action, fn, target) }], { kind });
  };

  /** Set up: confirm every change, then check every part's prerequisites, then run the missing parts in order. */
  const setUp = (feature: string, setup: Pending[]) => {
    const specs: ConfirmSpec[] = [];
    const steps: GoalRunStep[] = [];
    for (const p of setup) {
      if (isSamplePending(p)) continue;
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
    setSampleRun(null);
    setRunOpen(true);
    trackFeature("cleanup");
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    run.follow("Clean up", cleanupSteps(req), (onStep, onDone, onDetach) => streamCleanup(req, onStep, (success, error, apiError) => {
      void fetchResources();
      onDone(success, error, apiError);
    }, (reason) => {
      void fetchResources();
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

  const runningSomething = !!resAction || migrating || !!run.run?.running;
  const busy: Busy = { resAction, actionProject, migrating };
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
  const projectOptions: ProjectOption[] = projectNames.map((name) => {
    const status = pipelineServerStatus(resStatus, name, busy);
    const foreign = pipelineServerUnmanaged(resStatus, name);
    const has = !!pipelineServerIn(resStatus, name) || foreign;
    return { name, description: has ? `Pipeline server: ${envStatusText(status)}${foreign ? " · not managed by this tool" : ""}` : undefined };
  });
  const workspaceOptions: ProjectOption[] = projectNames.map((name) => ({ name }));

  // After an explicit clear the tab says so instead of the cross-project
  // summary, which names a project the picker no longer shows.
  const cleared = (searchParams.has("project") && !project) || !!missingProject;
  const pipelines: FeatureState = cleared
    ? { status: "unknown", label: "No project chosen", next: { kind: "choose-project" }, project: null }
    : pipelinesState(resStatus, project, projectNames, busy);
  const pipelinesSummary = pipelinesState(resStatus, null, projectNames, busy);
  const mlflowNow = mlflowState(resStatus, busy);
  // Once a feature works, its samples are the natural next step: the
  // sample pipeline only on a pipeline server this tool set up.
  const summaryServer = pipelinesSummary.project ? pipelineServerIn(resStatus, pipelinesSummary.project) : undefined;
  const pipelinesSamples = pipelinesSummary.status === "ready" && !!summaryServer && summaryServer.managedByTool !== false;
  const overviewRows: OverviewRow[] = [
    {
      id: "pipelines", name: "Pipelines", gives: "Run data science pipelines; artifacts in S3", state: pipelinesSummary,
      href: `?tab=pipelines${pipelinesSummary.project ? `&project=${encodeURIComponent(pipelinesSummary.project)}` : ""}`,
      ...(pipelinesSamples ? { samples: { label: "Run a sample pipeline", focus: PIPELINE_SAMPLE_ID } } : {}),
    },
    {
      id: "mlflow", name: "MLflow", gives: "Track experiments, runs and models", state: mlflowNow, href: "?tab=mlflow",
      ...(mlflowNow.status === "ready" ? { samples: { label: "Add sample data", focus: MLFLOW_SAMPLES_ID } } : {}),
    },
  ];
  const anythingToClean = !!resStatus && (() => {
    const c = cleanupCandidates(resStatus, "all", minioLeftovers);
    return c.servers.length > 0 || c.storage || c.mlflow;
  })();

  // The run's "what next" and main action, from the feature it set up.
  // A sample run: after adding MLflow samples, open the project's workspace.
  const runFeature = run.run?.title.includes("MLflow") ? "mlflow" : "pipelines";
  const sampleNext: NextStep | null = sampleRun?.feature === "mlflow" && sampleRun.action !== "remove" && mlflow?.uiRoute
    ? { kind: "open", label: `Open ${sampleRun.project} in MLflow`, href: mlflowWorkspaceHref(mlflow.uiRoute, sampleRun.project) }
    : null;
  const runNext: NextStep | null = run.run?.kind === "setup" ? (runFeature === "mlflow" ? mlflowNow.next : pipelines.next) : sampleNext;
  const whatNext = run.run?.kind === "setup"
    ? <Content component="p">{runFeature === "mlflow"
      ? "Open MLflow workspaces to choose a project and see its experiments. On the MLflow tab, Sample data adds example experiments, traces and a model to a project in one click."
      : `${runNext?.kind === "open" ? runNext.label : "Open pipelines"} to import a pipeline and create a run, or run the sample pipeline on the Pipelines tab to check that runs work.`}</Content>
    : sampleRun && sampleRun.action !== "remove"
      ? <Content component="p">{sampleRun.feature === "mlflow"
        ? `Open ${sampleRun.project} in MLflow to see the sample data; the dashboard shows it on the pages each sample names.`
        : "The Pipelines tab shows the run's state until it ends, with a link to the run in the dashboard."}</Content>
      : undefined;
  const outcome = run.run ? runOutcome(run.run) : null;

  const startSetUp = () => {
    if (!setupConfirm) return;
    const { title, steps, done, recheck } = setupConfirm;
    setSetupConfirm(null);
    setSampleRun(null);
    setRunOpen(true);
    void run.start(title, steps, {
      kind: "setup",
      doneMessage: done,
      // Every part's prerequisites on the latest status, before anything changes.
      preflight: async () => {
        const fresh = await getResourcesStatus();
        setResStatus(fresh);
        statusRef.current = fresh;
        return setUpBlocked(recheck(fresh, projectNames));
      },
    });
  };

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
      {!resStatus && !resError ? (
        <PageLoading title="Loading the environment" />
      ) : (
        // Only the active tab is rendered: leaving a tab clears what it
        // fetched on request (the storage's keys).
        <PageSection isFilled id="environment-tab-content" role="tabpanel" aria-labelledby={`environment-tab-${tab}`}>
          <Stack hasGutter>
            {resError && (
              <StackItem>
                <Alert variant="warning" title="Resource status may be out of date" isInline component="p">
                  <TruncatedText>{resError}</TruncatedText>
                </Alert>
              </StackItem>
            )}
            {pollGaveUp && resourcesSettling && (
              <StackItem>
                <Alert variant="info" title="Stopped checking automatically after 10 minutes" isInline component="p"
                  actionLinks={<AlertActionLink onClick={checkAgain}>Check again</AlertActionLink>} />
              </StackItem>
            )}
            {resStatus && (
              <StackItem>
                {tab === "overview" && (
                  <Overview rows={overviewRows} baseReason={baseReason} setUpReason={readOnly ? READ_ONLY_SETUP : null}
                    onSetUp={(row, setup) => setUp(row.name, setup)}
                    onShowIssue={(row, part) => { navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) }); setFocusTarget(partTitleId(part)); }}
                    onOpenTab={(row) => { navigate({ tab: row.id as EnvironmentTab, project: "" }); setFocusTarget("pipelines-project"); }}
                    onMigrate={() => setMigrateKind("migrate")}
                    onSamples={(row, focus) => { navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) }); setFocusTarget(focus); }}
                    onCleanup={!readOnly && anythingToClean ? () => setCleanupScope("all") : null} />
                )}
                {tab === "pipelines" && (
                  <PipelinesTab status={resStatus} project={project} projects={projectOptions} projectsLoaded={projectList !== null}
                    onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })} missingProject={missingProject}
                    state={pipelines} busy={busy} baseReason={baseReason} readOnly={readOnly} readReason={readBlocker} minioLeftovers={minioLeftovers} consoleURL={consoleURL}
                    setPending={setPending} onMigrate={() => setMigrateKind("migrate")} onRemoveOldStorage={() => setMigrateKind("remove")}
                    onSetUp={(setup) => setUp("Pipelines", setup)} onCleanup={() => setCleanupScope("pipelines")}
                    sampleOp={sampleOp} samplesRefresh={samplesRefresh} />
                )}
                {tab === "mlflow" && (
                  <MLflowTab status={resStatus} state={mlflowNow} busy={busy} baseReason={baseReason} readOnly={readOnly} consoleURL={consoleURL}
                    project={project} projects={workspaceOptions} projectsLoaded={projectList !== null}
                    onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })}
                    setPending={setPending} onSetUp={(setup) => setUp("MLflow", setup)} onCleanup={() => setCleanupScope("mlflow")}
                    sampleOp={sampleOp} samplesRefresh={samplesRefresh} />
                )}
              </StackItem>
            )}
          </Stack>
        </PageSection>
      )}

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
        <CleanupModal scope={cleanupScope} status={resStatus} minioLeftovers={minioLeftovers} baseReason={baseReason}
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
