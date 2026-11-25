import type { ConnectionWorkbench, KueueRequest, KueueStatus, WorkbenchStatus, MaaSStatus, ModelRegistryStatus, OperationResponse, PlaygroundStatus, PrerequisitesStatus, ResourcesStatus } from "../../types";
import {
  trackFeature,
  streamCleanup,
  streamMLflowSamples,
  streamModels,
  streamPlayground,
  streamWorkbench,
  streamMaaS,
  streamModelRegistry,
  streamKueue,
  streamPrerequisites,
  type StreamDoneHandler,
  type StreamDetachHandler,
  type CleanupRequest,
} from "../../services/api";
import { errorResult } from "../../outcomes";
import { type ConfirmSpec, type ModelsPending, type Pending, type PlaygroundPending, type MaaSPending, isMaaSPending, type PrerequisitesPending, isPrerequisitesPending, type ModelRegistryPending, type KueuePending, isKueuePending, isModelRegistryPending, type SamplePending, isModelsPending, isPlaygroundPending, isSamplePending, type WorkbenchPending, isWorkbenchPending } from "./resourceRow";
import { workbenchConfirm, workbenchSteps } from "./WorkbenchTab";
import { modelsConfirm, modelsSteps } from "./modelsConfirm";
import { playgroundConfirm, playgroundSteps } from "./PlaygroundTab";
import { maasConfirm, maasRequestOf, maasSteps } from "./MaaSTab";
import { modelRegistryConfirm, modelRegistrySteps } from "./ModelRegistryTab";
import { kueueConfirm, kueueSteps } from "./KueueTab";
import type { KueueOp } from "./kueueState";
import { type SampleOp, mlflowSampleSteps, samplesConfirm } from "./samples";
import type { RunState } from "./runState";
import type { GoalRunStep } from "./useGoalRun";
import { cleanupSteps } from "./CleanupModal";
import { type ModelRegistryOp, type PlaygroundOp, type WorkbenchOp, mlflowState, pipelinesState, setUpBlocked, storageState } from "./featureState";
import { prerequisitesConfirm, prerequisitesSteps } from "./Prerequisites";
import { modelsOpOf, modelsRequest, resourceRequest, setUpWait } from "./runRequests";
import { TAB_TITLES } from "./environmentTabs";

/** The clean-up's refusal of a recipe project that holds what this tool did not create (pkg/cluster recipe.ErrorCodeConfirmContents). */
const CONFIRM_PROJECT_CONTENTS = "confirm_project_contents";
const CONFIRM_WORKBENCHES = "confirm_connection_workbenches";

/** The workbenches a refused step's detail lists: one "<name>=<uid>" per line (pkg/cluster workbenchesDetail). */
export function parseWorkbenches(detail?: string): ConnectionWorkbench[] {
  return (detail ?? "").split("\n").filter(Boolean).map((line) => {
    const at = line.indexOf("=");
    return at < 0 ? { name: line, uid: "" } : { name: line.slice(0, at), uid: line.slice(at + 1) };
  });
}


export interface EnvironmentActionsContext {
  runs: RunState;
  /** The resource status shown, and the latest one (for a Set up's ready checks). */
  status: ResourcesStatus | null;
  latestStatus: () => ResourcesStatus | null;
  /** Reads the status again before a Set up's later ready checks while the page is closed. */
  refreshStatus: (remainingMs: number) => Promise<void>;
  fetchResources: () => Promise<void>;
  /** Reads the status now and shows it: a Set up's preflight. */
  readFresh: () => Promise<ResourcesStatus>;
  /** Polls quickly again (after a change). */
  restartPolling: () => void;
  /** A Clean up refused for confirmation, nothing changed: Clean up opens again on the page, or a notification offers it. */
  askCleanupConfirmation: () => void;
  pg: PlaygroundStatus | null;
  wb: WorkbenchStatus | null;
  maas: MaaSStatus | null;
  modelRegistry: ModelRegistryStatus | null;
  kueue: KueueStatus | null;
  /** What else this page set up in a project (the Kueue setup's warning). */
  othersIn: (project: string) => string[];
  prereq: PrerequisitesStatus | null;
  confirmOf: (p: Pending) => ConfirmSpec | null;
  setPending: (p: Pending | null) => void;
  projectNames: string[];
  onResult?: (res: OperationResponse) => void;
}

/**
 * What the Environment page's buttons start: a confirmed change runs in one
 * dialog that shows its progress and outcome; Set up confirms every change,
 * then checks every part's prerequisites, then runs the missing parts.
 */
export function environmentActions(ctx: EnvironmentActionsContext) {
  const { runs, status: resStatus, latestStatus, refreshStatus, fetchResources, readFresh, restartPolling, askCleanupConfirmation, pg, wb, maas, modelRegistry, kueue, othersIn, prereq, confirmOf, setPending, projectNames, onResult } = ctx;
  const {
    setResAction, setActionProject, run, setRunOpen, setupConfirm, setSetupConfirm, setCleanupScope, setCleanupRetry,
    setSampleOp, setSampleRun, setSamplesRefresh, setModelsOp, setModelsRun, setPlaygroundOp, setPlaygroundRun, setPlaygroundRefresh,
    setWorkbenchOp, setWorkbenchRun, setWorkbenchRefresh,
    setMaaSOp, setMaaSRun, setRunFeature, setRunProject, setMaaSRefresh, setPrereqRefresh, checking, setChecking, setCheckError,
    setModelRegistryOp, setModelRegistryRefresh, setKueueOp, setKueueRefresh,
  } = runs;

  /** Runs one request of a run; the run's dialog shows its outcome. */
  const runAction = async (action: string, fn: () => Promise<OperationResponse>, target: string | null): Promise<OperationResponse> => {
    trackFeature(action);
    setResAction(action);
    setActionProject(target);
    restartPolling();
    let res: OperationResponse;
    try {
      res = await fn();
    } catch (e) {
      res = errorResult(e, "Operation failed");
    }
    setResAction(null);
    setActionProject(null);
    onResult?.(res);
    void fetchResources();
    return res;
  };

  // Refresh (and Check again) reads everything the page shows again; what
  // is shown stays until the new answers are in.
  const checkAgain = () => {
    restartPolling();
    setPlaygroundRefresh((n) => n + 1);
    setWorkbenchRefresh((n) => n + 1);
    setMaaSRefresh((n) => n + 1);
    setModelRegistryRefresh((n) => n + 1);
    setKueueRefresh((n) => n + 1);
    setSamplesRefresh((n) => n + 1);
    setPrereqRefresh((n) => n + 1);
    void fetchResources();
  };

  /**
   * Check again: it changes nothing on the cluster, so it runs in place,
   * without a confirmation or the run dialog; the result appears on the
   * model once the status is read again.
   */
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
    const op = modelsOpOf(p);
    setPending(null);
    setSampleRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setModelsOp(op);
    setModelsRun(op);
    setRunFeature("models");
    restartPolling();
    const end = () => {
      setModelsOp(null);
      void fetchResources();
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, modelsSteps(p), (onStep, onDone, onDetach) => streamModels(modelsRequest(op, p),
      onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }), op.action === "teardown" || op.action === "disallow" ? "delete" : op.action === "check" ? "change" : "setup");
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

  /** A confirmed Workbench change: one streamed request whose steps the run dialog shows. */
  const startWorkbench = (p: WorkbenchPending) => {
    if (run.run?.running) return;
    const spec = workbenchConfirm(p, wb);
    const action = p.kind === "setup-workbench" ? "setup" : p.kind === "start-workbench" ? "start" : p.kind === "stop-workbench" ? "stop" : p.kind === "delete-workbench-storage" ? "delete-storage" : "teardown";
    const op: WorkbenchOp = { action, project: p.project };
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setRunFeature("workbench");
    setWorkbenchOp(op);
    setWorkbenchRun(op);
    const end = () => {
      setWorkbenchOp(null);
      setWorkbenchRefresh((n) => n + 1);
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, workbenchSteps(p), (onStep, onDone, onDetach) => streamWorkbench({
      action, project: p.project, ...(p.kind === "teardown-workbench" && p.deleteStorage ? { deleteStorage: true } : {}),
    }, onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }),
    action === "teardown" || action === "delete-storage" ? "delete" : action === "setup" ? "setup" : "change");
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

  /** A confirmed Model registry change: one streamed request whose steps the run dialog shows. */
  const startModelRegistry = (p: ModelRegistryPending) => {
    if (run.run?.running) return;
    const spec = modelRegistryConfirm(p, modelRegistry, () => {});
    const op: ModelRegistryOp = { action: p.kind === "setup-model-registry" ? "setup" : "teardown" };
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setRunFeature("model-registry");
    setModelRegistryOp(op);
    const end = () => {
      setModelRegistryOp(null);
      setModelRegistryRefresh((n) => n + 1);
      setPrereqRefresh((n) => n + 1);
    };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, modelRegistrySteps(p), (onStep, onDone, onDetach) => streamModelRegistry(
      { action: op.action, ...(p.kind === "setup-model-registry" && p.samples ? { samples: true } : {}) },
      onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }), op.action === "setup" ? "setup" : "delete");
  };

  /** A confirmed Distributed workloads change: one streamed request whose steps the run dialog shows. */
  const startKueue = (p: KueuePending) => {
    if (run.run?.running) return;
    const spec = kueueConfirm(p, kueue, () => {}, p.kind === "setup-kueue" ? othersIn(p.project) : []);
    const action: KueueOp["action"] = p.kind === "setup-kueue" ? "setup" : p.kind === "teardown-kueue" ? "teardown" : p.kind === "delete-kueue-sample" ? "delete-sample" : p.kind === "kueue-optin" ? "optin" : "optout";
    const op: KueueOp = { action, project: p.kind === "setup-kueue" ? p.project : kueue?.project ?? null };
    setPending(null);
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature(p.kind);
    setRunFeature("kueue");
    setKueueOp(op);
    const end = () => {
      setKueueOp(null);
      setKueueRefresh((n) => n + 1);
      setPrereqRefresh((n) => n + 1);
    };
    const req: KueueRequest = p.kind === "setup-kueue"
      ? { action, project: p.project, ...(p.rayCluster ? { rayCluster: true } : {}), ...(p.rayJob ? { rayJob: true } : {}), ...(p.rayCluster || p.rayJob ? { rayImage: p.rayImage } : {}) }
      : p.kind === "delete-kueue-sample" ? { action, sample: p.sample } : { action };
    run.follow(spec?.runTitle ?? spec?.title ?? p.kind, kueueSteps(p), (onStep, onDone, onDetach) => streamKueue(req,
      onStep, (success, error, apiError) => { end(); onDone(success, error, apiError); }, (reason) => { end(); onDetach(reason); }),
    action === "setup" || action === "optin" ? "setup" : "delete");
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
    restartPolling();
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
    // The Distributed workloads tab's Turn back is Clean up's components part, the same server path and checks.
    if (p.kind === "turn-back-kueue") {
      setPending(null);
      startCleanup({ components: ["kueue"] }, "Turn the DataScienceCluster's kueue back");
      return;
    }
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
    if (isWorkbenchPending(p)) {
      startWorkbench(p);
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
    if (isModelRegistryPending(p)) {
      startModelRegistry(p);
      return;
    }
    if (isKueuePending(p)) {
      startKueue(p);
      return;
    }
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    const spec = confirmOf(p);
    const [action, fn, target] = resourceRequest(p);
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
    const workbenchSetup = setup.find(isWorkbenchPending);
    if (workbenchSetup) {
      setPending(workbenchSetup);
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
    const kueueSetup = setup.find(isKueuePending);
    if (kueueSetup) {
      setPending(kueueSetup);
      return;
    }
    const registrySetup = setup.find(isModelRegistryPending);
    if (registrySetup) {
      setPending(registrySetup);
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
      if (isSamplePending(p) || isModelsPending(p) || isPlaygroundPending(p) || isWorkbenchPending(p) || isMaaSPending(p) || isPrerequisitesPending(p) || isModelRegistryPending(p) || isKueuePending(p) || p.kind === "turn-back-kueue") continue;
      const spec = confirmOf(p);
      if (spec) specs.push(spec);
      const [action, fn, target] = resourceRequest(p);
      const title = spec?.runTitle ?? action;
      steps.push({ id: p.kind, title, run: () => runAction(action, fn, target), ...setUpWait(p, target, latestStatus) });
    }
    const project = setup.flatMap((p) => (p.kind === "setup-pipeline-server" ? [p.project] : []))[0];
    const done = project ? `Pipelines are ready in ${project}.` : `${feature} is ready.`;
    const title = steps.length === 1 ? steps[0].title : project ? `Set up ${feature} in ${project}` : `Set up ${feature}`;
    const recheck = (s: ResourcesStatus | null, projects: string[]) => (feature === "MLflow" ? mlflowState(s) : feature === TAB_TITLES.storage ? storageState(s) : pipelinesState(s, project ?? null, projects));
    if (steps.length > 0) setSetupConfirm({ feature, title, specs, steps, done, project, recheck });
  };

  /** Runs the confirmed Set up. */
  const startSetUp = () => {
    if (!setupConfirm) return;
    const { feature, title, steps, done, project, recheck } = setupConfirm;
    setSetupConfirm(null);
    setRunFeature(feature === "MLflow" ? "mlflow" : feature === TAB_TITLES.storage ? "storage" : "pipelines");
    setRunProject(project ?? null);
    setSampleRun(null);
    setRunOpen(true);
    void run.start(title, steps, {
      kind: "setup",
      doneMessage: done,
      // Every part's prerequisites on the latest status, before anything changes.
      preflight: async () => setUpBlocked(recheck(await readFresh(), projectNames)),
      refresh: refreshStatus,
    });
  };

  const startCleanup = (req: CleanupRequest, title = "Clean up") => {
    setCleanupScope(null);
    setCleanupRetry(null);
    setRunFeature("other");
    setSampleRun(null);
    setModelsRun(null);
    setPlaygroundRun(null);
    setRunOpen(true);
    trackFeature("cleanup");
    restartPolling();
    // Recipe projects the server refused because they hold what this tool
    // did not create, with what to confirm (the step's detail, one per line).
    const refusals: Record<string, string[]> = {};
    // Pipeline servers refused because workbenches use their Connection, with those workbenches.
    const workbenches: Record<string, ConnectionWorkbench[]> = {};
    // Whether a part already changed the cluster before a refusal: its outcome then stays on screen.
    let changed = false;
    run.follow(title, cleanupSteps(req), (onStep, onDone, onDetach) => streamCleanup(req, (step) => {
      if (step.step.startsWith("recipe-project:") && step.errorCode === CONFIRM_PROJECT_CONTENTS) {
        refusals[step.step.slice("recipe-project:".length)] = (step.detail ?? "").split("\n").filter(Boolean);
      }
      if (step.step.startsWith("pipeline-server:") && step.errorCode === CONFIRM_WORKBENCHES) {
        workbenches[step.step.slice("pipeline-server:".length)] = parseWorkbenches(step.detail);
      }
      if (step.status === "success" && step.step !== "operation_complete") changed = true;
      onStep(step);
    }, (success, error, apiError) => {
      void fetchResources();
      setPlaygroundRefresh((n) => n + 1);
      setWorkbenchRefresh((n) => n + 1);
      setMaaSRefresh((n) => n + 1);
      setModelRegistryRefresh((n) => n + 1);
      setKueueRefresh((n) => n + 1);
      // A component turned back changes what the features' prerequisites read.
      if (req.components?.length) setPrereqRefresh((n) => n + 1);
      onDone(success, error, apiError);
      const code = apiError?.errorCode;
      if ((code === CONFIRM_PROJECT_CONTENTS || code === CONFIRM_WORKBENCHES) && Object.keys(refusals).length + Object.keys(workbenches).length > 0) {
        // Back to Clean up, with the same choices, to confirm: at once when nothing changed yet; when parts already ran,
        // the run's outcome stays on screen and the next Clean up asks.
        const confirmed = Object.fromEntries(Object.entries(req.confirmWorkbenches ?? {}).map(([p, c]) => [p, Object.entries(c).map(([name, uid]) => ({ name, uid }))]));
        setCleanupRetry({ req, contents: refusals, workbenches: { ...confirmed, ...workbenches } });
        if (!changed) {
          setRunOpen(false);
          askCleanupConfirmation();
        }
      }
    }, (reason) => {
      void fetchResources();
      setPlaygroundRefresh((n) => n + 1);
      onDetach(reason);
    }));
  };

  return { checkAgain, checkModel, execute, setUp, startSetUp, startCleanup };
}
