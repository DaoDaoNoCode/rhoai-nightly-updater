import { useState } from "react";
import type { ResourcesStatus } from "../../types";
import type { ConfirmSpec } from "./resourceRow";
import type { FeatureState, MaaSOp, ModelRegistryOp, ModelsOp, PlaygroundOp, WorkbenchOp } from "./featureState";
import type { KueueOp } from "./kueueState";
import type { SampleOp } from "./samples";
import type { CleanupRetry, CleanupScope } from "./CleanupModal";
import type { RunFeature } from "./environmentTabs";
import { useGoalRun, type GoalRunStep } from "./useGoalRun";

/** A Set up waiting for its confirmation. */
export interface SetupConfirm {
  feature: string;
  title: string;
  specs: ConfirmSpec[];
  steps: GoalRunStep[];
  done: string;
  /** The feature's state on fresh data, for the run's first check. */
  recheck: (s: ResourcesStatus | null, projects: string[]) => FeatureState;
}

/**
 * The Environment page's changes in progress: the run dialog's run, what
 * each feature's running operation changes (its parts show it busy), the
 * last run of each (for the run dialog's next step), and the keys that make
 * a feature read its state again. EnvironmentRunsProvider holds it, above
 * the routes, so it outlives the page.
 */
export function useRunState() {
  // The action running from this page ("setup-minio", ...) and its project.
  const [resAction, setResAction] = useState<string | null>(null);
  const [actionProject, setActionProject] = useState<string | null>(null);
  // errorCode of this tab's last storage deletion that did not succeed.
  const run = useGoalRun();
  const [runOpen, setRunOpen] = useState(false);
  const [setupConfirm, setSetupConfirm] = useState<SetupConfirm | null>(null);
  const [cleanupScope, setCleanupScope] = useState<CleanupScope | null>(null);
  // A clean-up refused because recipe projects hold what this tool did not
  // create: Clean up opens again with its choices and asks (null otherwise).
  const [cleanupRetry, setCleanupRetry] = useState<CleanupRetry | null>(null);
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
  // The Workbench operation running now, the last one, and a key that reads its state again.
  const [workbenchOp, setWorkbenchOp] = useState<WorkbenchOp | null>(null);
  const [workbenchRun, setWorkbenchRun] = useState<WorkbenchOp | null>(null);
  const [workbenchRefresh, setWorkbenchRefresh] = useState(0);
  const [maasOp, setMaaSOp] = useState<MaaSOp | null>(null);
  // The last MaaS operation, for the run dialog's next step.
  const [maasRun, setMaaSRun] = useState<MaaSOp | null>(null);
  // Which feature the run dialog's run belongs to, set by whatever starts
  // it: the dialog's next step and words come from it (never guessed from
  // the run's title or the last runs of other features).
  const [runFeature, setRunFeature] = useState<RunFeature>("other");
  const [maasRefresh, setMaaSRefresh] = useState(0);
  // The Model registry operation running now, and a key that reads its state again.
  const [modelRegistryOp, setModelRegistryOp] = useState<ModelRegistryOp | null>(null);
  const [modelRegistryRefresh, setModelRegistryRefresh] = useState(0);
  // The Distributed workloads operation running now, and a key that reads its state again.
  const [kueueOp, setKueueOp] = useState<KueueOp | null>(null);
  const [kueueRefresh, setKueueRefresh] = useState(0);
  const [prereqRefresh, setPrereqRefresh] = useState(0);
  // A model's Check again, which runs in place (no dialog), and why it could not run.
  const [checking, setChecking] = useState<{ project: string; model: string } | null>(null);
  const [checkError, setCheckError] = useState<string | null>(null);
  return {
    resAction, setResAction, actionProject, setActionProject,
    run, runOpen, setRunOpen, setupConfirm, setSetupConfirm, cleanupScope, setCleanupScope, cleanupRetry, setCleanupRetry,
    sampleOp, setSampleOp, sampleRun, setSampleRun, samplesRefresh, setSamplesRefresh,
    modelsOp, setModelsOp, modelsRun, setModelsRun,
    playgroundOp, setPlaygroundOp, playgroundRun, setPlaygroundRun, playgroundRefresh, setPlaygroundRefresh,
    workbenchOp, setWorkbenchOp, workbenchRun, setWorkbenchRun, workbenchRefresh, setWorkbenchRefresh,
    maasOp, setMaaSOp, maasRun, setMaaSRun, runFeature, setRunFeature, maasRefresh, setMaaSRefresh,
    modelRegistryOp, setModelRegistryOp, modelRegistryRefresh, setModelRegistryRefresh,
    kueueOp, setKueueOp, kueueRefresh, setKueueRefresh,
    prereqRefresh, setPrereqRefresh, checking, setChecking, checkError, setCheckError,
  };
}

export type RunState = ReturnType<typeof useRunState>;
