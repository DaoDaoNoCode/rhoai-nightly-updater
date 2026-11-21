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
import { PipelinesTab, READ_ONLY_SETUP, pipelineServerConfirm, storageConfirm } from "./environment/PipelinesTab";
import { MLflowTab, mlflowConfirm } from "./environment/MLflowTab";
import { Overview } from "./environment/Overview";
import { FEATURE_PURPOSE, partTitleId } from "./environment/FeatureLayout";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
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
  unreadableState,
  noProjectState,
} from "./environment/featureState";
import { FeatureLoading, LoadFailed } from "./environment/Loading";
import { PrerequisitesSection, RECOMMENDED_EXPOSURE, headerWithPrerequisites, prerequisitesConfirm, prerequisitesReason, usePrerequisites } from "./environment/Prerequisites";
import type { ModelExposureKind, PrerequisiteFeature } from "../types";
import { BUSY_REASON, PROJECT_TABS, TAB_PARTS, TAB_TITLES, ENVIRONMENT_TABS, type EnvironmentTab } from "./environment/environmentTabs";
import { useEnvironmentLocation } from "./environment/useEnvironmentLocation";
import { useResourceStatus, useSettlePolling } from "./environment/useResourceStatus";
import { environmentActions, useRunState } from "./environment/environmentRuns";
import { environmentProjectNames, modelsProjectOptions, pipelinesProjectOptions, playgroundProjectOptions } from "./environment/projectOptions";
import { overviewRows as buildOverviewRows } from "./environment/overviewRows";
import { runFollowUp } from "./environment/runFollowUp";

export { nextStep, terminalReason } from "./environment/resourceRow";
export { readyCheck } from "./environment/runRequests";
export { ENVIRONMENT_TABS, type EnvironmentTab } from "./environment/environmentTabs";

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

/**
 * The Environment page: one tab per feature (Pipelines, MLflow) after an
 * Overview of them all. The feature's state comes from one function per
 * feature (environment/featureState.ts), which the Overview and the tab
 * both render. The tab and the project are in the URL (?tab=, ?project=),
 * so links, reloads and back and forward keep the view
 * (useEnvironmentLocation). It loads and polls the status and the projects
 * (useResourceStatus), and runs every change in one dialog that shows its
 * progress and outcome (environmentRuns).
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
  // Settling: something is starting or terminating and may still change on its
  // own. Terminal states are not settling: they need a change (A08-3).
  const resourcesSettling = allResources.some((r) => isTerminating(r) || (r.deployed && !r.ready && !terminalReason(r)));

  const runs = useRunState();
  const { resAction, actionProject, lastMinIOTeardown, run, runOpen, setRunOpen, setupConfirm, setSetupConfirm, cleanupScope, setCleanupScope,
    sampleOp, samplesRefresh, modelsOp, playgroundOp, playgroundRefresh, maasOp, maasRefresh, prereqRefresh, checking, checkError } = runs;
  // Poll quickly after a change, back off to 30 s, stop after 10 minutes or
  // once nothing is settling; hidden tabs don't poll (A06-5).
  const polling = useSettlePolling(fetchResources, !!resAction || resourcesSettling || !!run.run?.running);

  const [migrateKind, setMigrateKind] = useState<MigrateKind | null>(null);
  const migration = useMigrateStorage(useCallback(() => { void fetchResources(); }, [fetchResources]));
  const migrating = !!migration.run?.running;

  // The Playground tab's default project, once the playgrounds are known.
  const [playgroundDefault, setPlaygroundDefault] = useState<string | null>(null);
  const playgroundProjectParam = searchParams.has("project") ? urlProject : playgroundDefault;
  const playgroundRead = usePlaygroundStatus(tab === "playground" ? playgroundProjectParam : null, playgroundRefresh, !!playgroundOp, tab === "overview" || tab === "playground" || !!cleanupScope);
  const pg = playgroundRead.data;
  const maasRead = useMaaSStatus(maasRefresh, !!maasOp, tab === "overview" || tab === "maas" || !!cleanupScope);
  const maas = maasRead.data;
  // One-click prerequisites: read for the feature tabs and the Overview;
  // Models' plan depends on how the model about to be set up is reached.
  const [modelsExposure, setModelsExposure] = useState<ModelExposureKind | null>(null);
  const prereqRead = usePrerequisites(prereqRefresh, tab === "models" ? modelsExposure : RECOMMENDED_EXPOSURE, tab !== "maas");
  const prereq = prereqRead.data;
  useEffect(() => {
    if (!searchParams.has("project")) setPlaygroundDefault(defaultPlaygroundProject(pg, null));
  }, [pg, searchParams]);

  const confirmOf = (p: Pending) => storageConfirm(p, minio, legacy, setPending) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, pipelineServers) ?? modelsConfirm(p, resStatus) ?? playgroundConfirm(p, pg) ?? maasConfirm(p, maas, setPending) ?? prerequisitesConfirm(p, prereq) ?? samplesConfirm(p);

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

  // While a run goes on with its dialog closed, the app's operation alert
  // names it and its step, with View progress to open the same dialog again.
  const reopenRun = useCallback(() => { setToast(false); setRunOpen(true); }, [setRunOpen]);
  const migrationKind = migration.run?.running ? migration.run.kind : null;
  const reopenMigration = useCallback(() => { if (migrationKind) setMigrateKind(migrationKind); }, [migrationKind]);
  const goal = run.run?.running ? run.run : null;
  useRegisterBackgroundRun(goal
    ? { title: goal.title, startedAt: goal.startedAt, step: runStepNow(goal), open: reopenRun }
    : migration.run?.running ? { title: MIGRATE_TITLES[migration.run.kind], startedAt: migration.run.startedAt, step: migrateStepNow(migration.run), open: reopenMigration } : null);

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

  const projectNames = environmentProjectNames(projectList, resStatus);
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
  }, [pipelinesProject, fetchResources, selectedPipelinesProject]);
  const projectOptions = pipelinesProjectOptions(resStatus, projectNames, busy);
  const workspaceOptions: ProjectOption[] = projectNames.map((name) => ({ name }));
  const modelOptions = modelsProjectOptions(resStatus, projectNames);
  // The Models tab's own default: the first project with a model, preferring a ready one.
  const modelsProject = missingProject ? null : searchParams.has("project") ? urlProject : defaultModelsProject(resStatus, projectNames, null);
  // The Models tab's model: the URL's (model=<catalog name>), else the project's first, else the default.
  const modelsModel = defaultModelName(resStatus, modelsProject, searchParams.get("model"));

  // After an explicit clear the tab says so instead of the cross-project
  // summary, which names a project the picker no longer shows.
  const cleared = (searchParams.has("project") && !project) || !!missingProject;
  const pipelines: FeatureState = cleared
    ? noProjectState()
    : pipelinesState(resStatus, project, projectNames, busy);
  const pipelinesSummary = pipelinesState(resStatus, null, projectNames, busy);
  const mlflowNow = mlflowState(resStatus, busy);
  const models: FeatureState = cleared
    ? noProjectState()
    : modelsState(resStatus, modelsProject, projectNames, busy, modelsModel);
  const modelsSummary = modelsState(resStatus, null, projectNames, busy);
  const playgroundProject = missingProject ? null : playgroundProjectParam;
  const playgroundNow: FeatureState = cleared
    ? noProjectState()
    : !pg && playgroundRead.error ? unreadableState(playgroundProject) : playgroundState(resStatus, pg, playgroundProject, busy);
  // A status that was never read is loading; one that could not be read says so.
  const playgroundSummary = !pg && playgroundRead.error ? unreadableState() : playgroundState(resStatus, pg, null, busy);
  const maasNow = !maas && maasRead.error ? unreadableState() : maasState(maas, busy);
  const playgroundOptions = playgroundProjectOptions(pg, projectNames);
  const overviewRows = buildOverviewRows({ pipelines: pipelinesSummary, mlflow: mlflowNow, models: modelsSummary, playground: playgroundSummary, maas: maasNow }, prereq);
  const anythingToClean = !!resStatus && (() => {
    const c = cleanupCandidates(resStatus, "all", minioLeftovers, pg, maas);
    return c.servers.length > 0 || c.storage || c.mlflow || c.models.length > 0 || c.gateway || c.playgrounds.length > 0 || c.maasPublished.length > 0 || c.maasPlatform;
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
          {polling.gaveUp && resourcesSettling && (
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
