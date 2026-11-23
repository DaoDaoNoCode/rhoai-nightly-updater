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
import type { OperationResponse } from "../types";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { TruncatedText } from "./LongText";
import { PageErrorState } from "./PageStates";
import { type Pending, isTerminating, terminalReason } from "./environment/resourceRow";
import { ModelsTab } from "./environment/ModelsTab";
import { modelsConfirm } from "./environment/modelsConfirm";
import { PlaygroundTab, playgroundConfirm } from "./environment/PlaygroundTab";
import { MaaSTab, maasConfirm, useMaaSStatus } from "./environment/MaaSTab";
import { usePlaygroundStatus } from "./environment/usePlaygroundStatus";
import { PipelinesTab, pipelineServerConfirm } from "./environment/PipelinesTab";
import { StorageTab, storageConfirm } from "./environment/StorageTab";
import { MLflowTab, mlflowConfirm } from "./environment/MLflowTab";
import { Overview } from "./environment/Overview";
import { FEATURE_PURPOSE, READ_ONLY_SETUP, partTitleId } from "./environment/FeatureLayout";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
import { SecureStorageModal, secureExtraSteps } from "./environment/SecureStorageModal";
import { MIGRATE_TITLES, migrateStepNow, useMigrateStorage, type MigrateKind } from "./environment/useMigrateStorage";
import { type ProjectOption } from "./environment/ProjectSelect";
import { RunModal, runOutcome } from "./environment/SetupRun";
import { samplesConfirm } from "./environment/samples";
import { runStepNow } from "./environment/useGoalRun";
import { useRegisterBackgroundRun } from "./ProgressViews";
import { CleanupModal, cleanupCandidates } from "./environment/CleanupModal";
import {
  type Busy,
  type FeatureState,
  defaultModelsProject,
  defaultModelName,
  defaultProject,
  mlflowState,
  modelsState,
  defaultPlaygroundProject,
  playgroundState,
  maasState,
  pipelinesState,
  storageState,
  unreadableState,
  noProjectState,
} from "./environment/featureState";
import { FeatureLoading, LoadFailed } from "./environment/Loading";
import { PrerequisitesSection, RECOMMENDED_EXPOSURE, headerWithPrerequisites, prerequisitesConfirm, prerequisitesReason, usePrerequisites } from "./environment/Prerequisites";
import type { ModelExposureKind, PrerequisiteFeature } from "../types";
import { BUSY_REASON, PROJECT_TABS, TAB_PARTS, TAB_TITLES, ENVIRONMENT_TABS, type EnvironmentTab } from "./environment/environmentTabs";
import { useEnvironmentLocation } from "./environment/useEnvironmentLocation";
import { useResourcePolling, useResourceStatus } from "./environment/useResourceStatus";
import { environmentActions, useRunState } from "./environment/environmentRuns";
import { environmentProjectNames, modelsProjectOptions, pipelinesProjectOptions, playgroundProjectOptions } from "./environment/projectOptions";
import { overviewRows as buildOverviewRows } from "./environment/overviewRows";
import { runFollowUp } from "./environment/runFollowUp";

export { nextStep, terminalReason } from "./environment/resourceRow";
export { readyCheck } from "./environment/runRequests";
export { ENVIRONMENT_TABS, type EnvironmentTab } from "./environment/environmentTabs";

interface QuickResourceCreatorProps {
  /**
   * Null when mutations are allowed; otherwise a message from the global
   * useMutationBlocker (permissions, session, or an in-flight operation).
   */
  mutateBlocker: string | null;
  /** When set, the user is not a cluster-admin and cannot read credentials or mutate via the updater. */
  readBlocker?: string | null;
  /** OpenShift console base URL for external pod-log links. */
  consoleURL?: string;
  /** Notified on each operation response (including cluster_busy rejections). */
  onResult?: (res: OperationResponse) => void;
  /** Optional header slot driven by resource-status loading and refresh controls. */
  renderHeader?: (header: { loading: boolean; lastRefreshed: Date | null; onRefresh: () => void }) => React.ReactNode;
}

/**
 * Environment UI: Overview first, then a tab per feature (S3 storage, Pipelines,
 * MLflow, ...). Each feature shares one state builder in environment/featureState.ts
 * between Overview rows and its tab body. ?tab= and ?project= in the URL preserve
 * navigation across reloads and history (useEnvironmentLocation). Status and project
 * lists load and poll via useResourceStatus; mutations run through a single progress
 * dialog (environmentRuns).
 */
export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ mutateBlocker, readBlocker = null, consoleURL, onResult, renderHeader }) => {
  const { searchParams, tab, urlProject, navigate, setFocusTarget, chooseModel } = useEnvironmentLocation();
  const [pending, setPending] = useState<Pending | null>(null);
  const readOnly = !!readBlocker;

  const resources = useResourceStatus(tab === "pipelines" ? urlProject : null);
  const { status: resStatus, statusRef, projectList, error: resError, projectsError, loading: statusLoading, lastRefreshed, statusProject, selectedPipelinesProject, fetchResources } = resources;
  const minio = resStatus?.minio;
  const mlflow = resStatus?.mlflow;
  const legacy = resStatus?.legacyStorage;
  const pipelineServers = resStatus?.pipelineServers ?? [];
  const allResources = resStatus ? [resStatus.minio, resStatus.mlflow, ...pipelineServers, ...(resStatus.inferenceGateway ? [resStatus.inferenceGateway] : []), ...(resStatus.models?.items ?? [])] : [];
  // "Settling" means a resource is booting or winding down and may flip without user
  // action. Steady terminal states are not settling—they await an explicit change (A08-3).
  const resourcesSettling = allResources.some((r) => isTerminating(r) || (r.deployed && !r.ready && !terminalReason(r)));

  const runs = useRunState();
  const { resAction, actionProject, run, runOpen, setRunOpen, setupConfirm, setSetupConfirm, cleanupScope, setCleanupScope, cleanupRetry, setCleanupRetry,
    sampleOp, samplesRefresh, modelsOp, playgroundOp, playgroundRefresh, maasOp, maasRefresh, prereqRefresh, checking, checkError } = runs;
  // Single poll loop: aggressive right after activity, then 30 s cadence for ten minutes,
  // else leisurely refresh; inactive tabs skip polling (A06-5).
  const settlingNow = !!resAction || resourcesSettling || !!run.run?.running;
  const polling = useResourcePolling(fetchResources, settlingNow);

  const [migrateKind, setMigrateKind] = useState<MigrateKind | null>(null);
  const migration = useMigrateStorage(useCallback(() => { void fetchResources(); }, [fetchResources]));
  // Secure S3 storage runs the same way but is not a migration: it shows as the storage's own action.
  const migrating = !!migration.run?.running && migration.run.kind !== "secure";
  const securing = !!migration.run?.running && migration.run.kind === "secure";

  // Cached default project for Playground after playground inventory arrives.
  const [playgroundDefault, setPlaygroundDefault] = useState<string | null>(null);
  const playgroundProjectParam = searchParams.has("project") ? urlProject : playgroundDefault;
  const playgroundRead = usePlaygroundStatus(tab === "playground" ? playgroundProjectParam : null, playgroundRefresh, !!playgroundOp, tab === "overview" || tab === "playground" || !!cleanupScope);
  const pg = playgroundRead.data;
  const maasRead = useMaaSStatus(maasRefresh, !!maasOp, tab === "overview" || tab === "maas" || !!cleanupScope);
  const maas = maasRead.data;
  // Prerequisites feed Overview and feature tabs; on Models, exposure picks which plan applies.
  const [modelsExposure, setModelsExposure] = useState<ModelExposureKind | null>(null);
  const prereqRead = usePrerequisites(prereqRefresh, tab === "models" ? modelsExposure : RECOMMENDED_EXPOSURE, tab !== "maas");
  const prereq = prereqRead.data;
  // Cleanup uses live data, not stale snapshots—opening it retriggers status, MaaS, and
  // playground reads (those fetches wake only while Clean up is open).
  const freshReads = useRef({ resources: fetchResources, maas: maasRead.reload, playground: playgroundRead.reload });
  freshReads.current = { resources: fetchResources, maas: maasRead.reload, playground: playgroundRead.reload };
  const cleanupOpen = !!cleanupScope;
  useEffect(() => {
    if (!cleanupOpen) return;
    const r = freshReads.current;
    void r.resources();
    if (tab === "overview") {
      void r.maas();
      void r.playground();
    }
    // Fires when Clean up opens, on whichever tab was active then.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cleanupOpen]);
  useEffect(() => {
    if (!searchParams.has("project")) setPlaygroundDefault(defaultPlaygroundProject(pg, null));
  }, [pg, searchParams]);

  const confirmOf = (p: Pending) => storageConfirm(p, minio, legacy, setPending) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, [...pipelineServers, ...(resStatus?.outsidePipelineServers ?? [])]) ?? modelsConfirm(p, resStatus) ?? playgroundConfirm(p, pg) ?? maasConfirm(p, maas, setPending) ?? prerequisitesConfirm(p, prereq) ?? samplesConfirm(p);

  // Finished runs whose dialog was dismissed surface as toast with a path back—outcomes stay
  // visible without shifting page layout.
  const [toast, setToast] = useState(false);
  const wasRunning = useRef(false);
  useEffect(() => {
    const now = !!run.run?.running;
    // When cleanup aborts before confirmation, reopen the Clean up flow.
    if (wasRunning.current && !now && !runOpen && !cleanupRetry) setToast(true);
    wasRunning.current = now;
  }, [run.run?.running, runOpen, cleanupRetry]);

  // Background runs (dialog closed) still appear in the global operation strip with step text
  // and View progress to restore the run dialog.
  const reopenRun = useCallback(() => { setToast(false); setRunOpen(true); }, [setRunOpen]);
  const migrationKind = migration.run?.running ? migration.run.kind : null;
  const reopenMigration = useCallback(() => { if (migrationKind) setMigrateKind(migrationKind); }, [migrationKind]);
  const goal = run.run?.running ? run.run : null;
  useRegisterBackgroundRun(goal
    ? { title: goal.title, startedAt: goal.startedAt, step: runStepNow(goal), open: reopenRun }
    : migration.run?.running ? { title: MIGRATE_TITLES[migration.run.kind], startedAt: migration.run.startedAt, step: migrateStepNow(migration.run), open: reopenMigration } : null);

  const runningSomething = !!resAction || migrating || securing || !!run.run?.running || !!checking;
  const busy: Busy = { resAction: securing ? "secure-minio" : resAction, actionProject, migrating, models: modelsOp, playground: playgroundOp, maas: maasOp };
  const baseReason = mutateBlocker ?? (runningSomething ? BUSY_REASON : null);
  // Post-delete the namespace may remain (deployed:false, managedByTool:true). Keep Delete
  // available while tool-owned storage artifacts persist—data PVC, Routes, or other leftovers.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute || (minio.leftovers ?? []).length > 0
  );
  const modal = pending ? confirmOf(pending) : null;

  const projectNames = environmentProjectNames(projectList, resStatus);
  // URL project names that no longer exist are ignored so Set up does not recreate them.
  const missingProject = urlProject && projectList !== null && !projectNames.includes(urlProject) ? urlProject : null;
  // Picker and body share one project: from the URL, explicit empty project= means none, and
  // omitting project= entirely falls back to the default on first visit.
  const project = missingProject ? null : searchParams.has("project") ? urlProject : defaultProject(resStatus, projectNames, null);
  const pipelinesProject = tab === "pipelines" ? project : null;
  useEffect(() => {
    if (selectedPipelinesProject.current === pipelinesProject) return;
    selectedPipelinesProject.current = pipelinesProject;
    if (pipelinesProject) void fetchResources();
  }, [pipelinesProject, fetchResources, selectedPipelinesProject]);
  const projectOptions = pipelinesProjectOptions(resStatus, projectNames, busy);
  const workspaceOptions: ProjectOption[] = projectNames.map((name) => ({ name }));
  const modelOptions = modelsProjectOptions(resStatus, projectNames);
  // Models tab default project: earliest project holding a model, favoring a ready instance.
  const modelsProject = missingProject ? null : searchParams.has("project") ? urlProject : defaultModelsProject(resStatus, projectNames, null);
  // Models selection: model= query if present, otherwise first model in the project, else the built-in default.
  const modelsModel = defaultModelName(resStatus, modelsProject, searchParams.get("model"));

  // Cleared or missing projects switch the tab to an explicit empty state rather than a
  // cross-project summary that references a hidden picker entry.
  const cleared = (searchParams.has("project") && !project) || !!missingProject;
  const pipelines: FeatureState = cleared
    ? noProjectState()
    : pipelinesState(resStatus, project, projectNames, busy);
  const pipelinesSummary = pipelinesState(resStatus, null, projectNames, busy);
  const storageNow = storageState(resStatus, busy);
  const mlflowNow = mlflowState(resStatus, busy);
  const models: FeatureState = cleared
    ? noProjectState()
    : modelsState(resStatus, modelsProject, projectNames, busy, modelsModel);
  const modelsSummary = modelsState(resStatus, null, projectNames, busy);
  const playgroundProject = missingProject ? null : playgroundProjectParam;
  const playgroundNow: FeatureState = cleared
    ? noProjectState()
    : !pg && playgroundRead.error ? unreadableState(playgroundProject) : playgroundState(resStatus, pg, playgroundProject, busy);
  // Missing playground payload with no error yet reads as loading; failures get an unreadable state.
  const playgroundSummary = !pg && playgroundRead.error ? unreadableState() : playgroundState(resStatus, pg, null, busy);
  const maasNow = !maas && maasRead.error ? unreadableState() : maasState(maas, busy);
  const playgroundOptions = playgroundProjectOptions(pg, projectNames);
  const overviewRows = buildOverviewRows({ storage: storageNow, pipelines: pipelinesSummary, mlflow: mlflowNow, models: modelsSummary, playground: playgroundSummary, maas: maasNow }, prereq);
  const anythingToClean = !!resStatus && (() => {
    const c = cleanupCandidates(resStatus, "all", minioLeftovers, pg, maas);
    return c.servers.length > 0 || c.storage || c.mlflow || c.models.length > 0 || c.gateway || c.playgrounds.length > 0 || c.maasPublished.length > 0 || c.maasPlatform || c.recipeProjects.length > 0;
  })();

  const { open: runNext, whatNext } = runFollowUp({
    run: run.run, runFeature: runs.runFeature, sampleRun: runs.sampleRun, modelsRun: runs.modelsRun, playgroundRun: runs.playgroundRun, maasRun: runs.maasRun,
    status: resStatus, pg, busy, projectNames, mlflowNow, maasNow, pipelines,
  });
  const outcome = run.run ? runOutcome(run.run) : null;

  const { checkAgain, checkModel, execute, setUp, startSetUp, startCleanup } = environmentActions({
    runs, status: resStatus, statusRef, fetchResources, readFresh: resources.readFresh, restartPolling: polling.restart,
    pg, maas, prereq, confirmOf, setPending, projectNames, onResult,
  });

  // Shared prerequisites block rendered above each feature tab's sections.
  const prereqNode = (feature: PrerequisiteFeature, exposure?: ModelExposureKind | null) => (
    <PrerequisitesSection feature={feature} prereq={prereq?.features?.[feature]} error={prereqRead.error} onRetry={() => void prereqRead.reload()}
      exposure={exposure ?? undefined} stale={feature === "models" && prereqRead.stale} readOnly={readOnly} baseReason={baseReason} setPending={setPending} />
  );
  const onModelsExposure = useCallback((e: ModelExposureKind | null) => setModelsExposure(e), []);

  // Overview skeleton placeholders while status, playground, or MaaS data is still in flight.
  const overview = (
    <Overview rows={overviewRows} baseReason={baseReason} setUpReason={readOnly ? READ_ONLY_SETUP : null}
      onSetUp={(row, setup) => setUp(row.name, setup)}
      onShowIssue={(row, part) => {
        // Storage problems always land on the storage tab even when another row links them (e.g. Pipelines).
        if (part === "storage") navigate({ tab: "storage" });
        else navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) });
        setFocusTarget(partTitleId(part));
      }}
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
        Mount only the selected tab so on-demand fetches drop when you leave (e.g. storage keys).
        Before data arrives, show skeleton layout—not blank or "not set up". Unreadable status
        surfaces an error with Retry instead.
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
          {polling.gaveUp && resourcesSettling && (
            <StackItem>
              <Alert variant="info" title="Stopped checking automatically after 10 minutes" isInline component="p"
                actionLinks={<AlertActionLink onClick={checkAgain}>Check again</AlertActionLink>} />
            </StackItem>
          )}
          {!resStatus && tab !== "maas" ? (
            <StackItem>
              {resError ? <PageErrorState inSection={false} error={resError} title="Can't load the environment" onRetry={checkAgain} />
                : tab === "overview" ? overview
                  : <FeatureLoading title={TAB_TITLES[tab]} purpose={FEATURE_PURPOSE[tab]} parts={TAB_PARTS[tab]} />}
            </StackItem>
          ) : (
            <StackItem>
              {tab === "overview" && overview}
              {tab === "storage" && resStatus && (
                <StorageTab status={resStatus} state={storageNow} busy={busy} baseReason={baseReason} readOnly={readOnly} readReason={readBlocker} minioLeftovers={minioLeftovers} consoleURL={consoleURL}
                  setPending={setPending} onMigrate={() => setMigrateKind("migrate")} onRemoveOldStorage={() => setMigrateKind("remove")} onSecure={() => setMigrateKind("secure")}
                  onSetUp={(setup) => setUp(TAB_TITLES.storage, setup)} />
              )}
              {tab === "pipelines" && resStatus && (
                <PipelinesTab prerequisites={prereqNode("pipelines")} status={resStatus} project={project} projects={projectOptions} projectsLoaded={projectList !== null}
                  onProject={(p) => navigate({ project: p })} onClearProject={() => navigate({ project: "" })} missingProject={missingProject}
                  connectionLoading={!!project && statusProject !== project}
                  state={headerWithPrerequisites(pipelines, prereq?.features?.pipelines)} prerequisitesReason={prerequisitesReason(prereq?.features?.pipelines, "pipelines")} busy={busy} baseReason={baseReason} readOnly={readOnly} consoleURL={consoleURL}
                  setPending={setPending} onSetUp={(setup) => setUp("Pipelines", setup)} onCleanup={() => setCleanupScope("pipelines")}
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
                  setPending={setPending} onRun={execute} onSetUp={(setup) => setUp("Models", setup)} onCleanup={() => setCleanupScope("models")}
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

      {migrateKind === "secure" && (
        <SecureStorageModal
          isOpen
          keys={resStatus?.s3Keys}
          run={migration.run}
          baseReason={securing ? null : mutateBlocker ?? (resAction || migrating || run.run?.running ? BUSY_REASON : null)}
          onStart={() => migration.start("secure", {}, secureExtraSteps(resStatus?.s3Keys))}
          onClose={() => {
            setMigrateKind(null);
            migration.reset();
          }}
        />
      )}

      {migrateKind && migrateKind !== "secure" && (
        <MigrateStorageModal
          isOpen
          kind={migrateKind}
          legacy={legacy}
          run={migration.run}
          baseReason={migrating ? null : mutateBlocker ?? (resAction || run.run?.running ? BUSY_REASON : null)}
          onStart={(kind, deleteNamespace, confirmProjects) => migration.start(kind, kind === "remove" ? { removeOldStorage: true, deleteNamespace } : confirmProjects.length > 0 ? { confirmProjects } : {})}
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
          retry={cleanupRetry} onConfirm={startCleanup} onCancel={() => { setCleanupScope(null); setCleanupRetry(null); }} />
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
