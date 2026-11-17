import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Content,
  PageSection,
  Stack,
  StackItem,
  Tab,
  Tabs,
  TabTitleText,
} from "@patternfly/react-core";
import { useSearchParams } from "react-router-dom";
import type { OperationResponse, ResourcesStatus } from "../types";
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
  toApiError,
} from "../services/api";
import { exponentialBackoff, usePolling } from "../hooks/usePolling";
import { RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS, RESOURCE_SETTLE_MAX_MS } from "../constants";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { TruncatedText } from "./LongText";
import { PageLoading } from "./PageStates";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { type ConfirmSpec, type Pending, isTerminating, terminalReason } from "./environment/resourceRow";
import { envStatusText } from "./environment/resourceStatus";
import { PipelinesTab, pipelineServerConfirm, storageConfirm } from "./environment/PipelinesTab";
import { MLflowTab, mlflowConfirm } from "./environment/MLflowTab";
import { Overview, type OverviewRow } from "./environment/Overview";
import { partTitleId } from "./environment/FeatureLayout";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
import { useMigrateStorage } from "./environment/useMigrateStorage";
import { type ProjectOption } from "./environment/ProjectSelect";
import { SetupRunModal } from "./environment/SetupRun";
import { useGoalRun, type GoalRunStep } from "./environment/useGoalRun";
import {
  type Busy,
  defaultProject,
  mlflowState,
  pipelineServerIn,
  pipelineServerStatus,
  pipelineServerUnmanaged,
  pipelinesState,
} from "./environment/featureState";

export { nextStep, terminalReason } from "./environment/resourceRow";

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
  /** Why this user cannot read credentials through the updater (not a cluster-admin), or null. */
  readBlocker?: string | null;
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
 * so links, reloads and back and forward keep the view. It loads and polls
 * the status, runs the confirmed actions and shows their result.
 */
export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ mutateBlocker, readBlocker = null, onResult, renderHeader }) => {
  const [resStatus, setResStatus] = useState<ResourcesStatus | null>(null);
  const [resAction, setResAction] = useState<string | null>(null);
  const [actionProject, setActionProject] = useState<string | null>(null);
  const [resResult, setResResult] = useState<OperationResponse | null>(null);
  // errorCode of this tab's last storage teardown that did not succeed.
  const [lastMinIOTeardown, setLastMinIOTeardown] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [projectList, setProjectList] = useState<string[] | null>(null);

  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab") ?? "";
  const tab: EnvironmentTab = (ENVIRONMENT_TABS as readonly string[]).includes(tabParam) ? (tabParam as EnvironmentTab) : OLD_TABS.includes(tabParam) ? "pipelines" : "overview";
  const urlProject = searchParams.get("project") || null;
  const navigate = useCallback((next: { tab?: EnvironmentTab; project?: string | null }, replace = false) => {
    setSearchParams((params) => {
      const p = new URLSearchParams(params);
      if (next.tab !== undefined) {
        if (next.tab === "overview") p.delete("tab");
        else p.set("tab", next.tab);
      }
      if (next.project !== undefined) {
        if (next.project) p.set("project", next.project);
        else p.delete("project");
      }
      return p;
    }, { replace });
  }, [setSearchParams]);
  // A link to the earlier layout's tabs opens Pipelines, replacing the old address.
  useEffect(() => {
    if (OLD_TABS.includes(tabParam)) navigate({ tab: "pipelines" }, true);
  }, [tabParam, navigate]);

  // "Show issue" moves the focus to the part once its tab shows.
  const [focusPart, setFocusPart] = useState<string | null>(null);
  useEffect(() => {
    if (!focusPart) return;
    const el = document.getElementById(partTitleId(focusPart));
    if (el) {
      el.focus();
      el.scrollIntoView?.({ block: "center" });
      setFocusPart(null);
    }
  }, [focusPart, tabParam, urlProject]);

  const [resError, setResError] = useState<string | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const fetchResources = useCallback(async () => {
    setStatusLoading(true);
    try {
      const s = await getResourcesStatus();
      setResStatus(s);
      setResError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      // Keep the last known state; show why it may be stale.
      setResError(toApiError(e, "Could not load resource status").message);
    } finally {
      setStatusLoading(false);
    }
  }, []);

  useEffect(() => { fetchResources(); }, [fetchResources]);

  const refreshProjects = useCallback(() => {
    getDSProjects().then((r) => setProjectList(r.projects || [])).catch(() => setProjectList((l) => l ?? []));
  }, []);

  useEffect(() => { refreshProjects(); }, [refreshProjects]);

  const minio = resStatus?.minio;
  const mlflow = resStatus?.mlflow;
  const legacy = resStatus?.legacyStorage;
  const pipelineServers = resStatus?.pipelineServers ?? [];
  const allResources = resStatus ? [resStatus.minio, resStatus.mlflow, ...pipelineServers] : [];
  // Settling: something is starting or terminating and may still change on its
  // own. Terminal states are not settling: they need a change (A08-3).
  const resourcesSettling = allResources.some((r) => isTerminating(r) || (r.deployed && !r.ready && !terminalReason(r)));

  // Poll quickly after a change, back off to 30 s, stop after 10 minutes or
  // once nothing is settling; hidden tabs don't poll (A06-5).
  const [pollRound, setPollRound] = useState(0);
  const [pollGaveUp, setPollGaveUp] = useState(false);
  usePolling(fetchResources, {
    enabled: !!resAction || resourcesSettling,
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
    fetchResources();
  };

  /** Runs one request; showResult puts its outcome in the page's alert (a Set up run shows its own in its dialog). */
  const runAction = async (action: string, fn: () => Promise<OperationResponse>, target: string | null, showResult: boolean): Promise<OperationResponse> => {
    trackFeature(action);
    setPending(null);
    setResAction(action);
    setActionProject(target);
    if (showResult) setResResult(null);
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    let res: OperationResponse;
    try {
      res = await fn();
    } catch (e) {
      res = errorResult(e, "Operation failed");
    }
    if (showResult) setResResult(res);
    setResAction(null);
    setActionProject(null);
    if (action === "teardown-minio") setLastMinIOTeardown(res.success ? null : res.errorCode || "failed");
    else if (action === "setup-minio" && res.success) setLastMinIOTeardown(null);
    onResult?.(res);
    void fetchResources();
    refreshProjects();
    return res;
  };

  const [migrateOpen, setMigrateOpen] = useState(false);
  const migration = useMigrateStorage(useCallback(() => {
    void fetchResources();
    refreshProjects();
  }, [fetchResources, refreshProjects]));
  const migrating = !!migration.run?.running;

  const setupRun = useGoalRun();
  const [setupRunOpen, setSetupRunOpen] = useState(false);
  const [setupConfirm, setSetupConfirm] = useState<{ feature: string; specs: ConfirmSpec[]; steps: GoalRunStep[] } | null>(null);

  const request = (p: Pending): [string, () => Promise<OperationResponse>, string | null] => {
    switch (p.kind) {
      case "setup-minio": return ["setup-minio", setupMinIO, null];
      case "repair-minio": return ["setup-minio", setupMinIO, null];
      case "teardown-minio": return ["teardown-minio", teardownMinIO, null];
      case "setup-mlflow": return ["setup-mlflow", setupMLflow, null];
      case "teardown-mlflow": return ["teardown-mlflow", teardownMLflow, null];
      case "deploy-mlflow-pr": return ["deploy-mlflow-pr", () => deployMLflowPR(p.pr), null];
      case "revert-mlflow": return ["revert-mlflow", revertMLflow, null];
      case "setup-pipeline-server": return ["setup-pipeline-server", () => setupPipelineServer(p.project), p.project];
      case "teardown-pipeline-server": return ["teardown-pipeline-server", () => teardownPipelineServer(p.project), p.project];
    }
  };
  const execute = (p: Pending) => {
    const [action, fn, target] = request(p);
    return runAction(action, fn, target, true);
  };
  const confirmOf = (p: Pending) => storageConfirm(p, minio, legacy) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, pipelineServers);

  /** Set up: confirm every change, then run the missing parts in order. */
  const setUp = (feature: string, setup: Pending[]) => {
    const specs: ConfirmSpec[] = [];
    const steps: GoalRunStep[] = [];
    for (const p of setup) {
      const spec = confirmOf(p);
      if (spec) specs.push(spec);
      const [action, fn, target] = request(p);
      const title = p.kind === "setup-minio" ? "Set up the S3 storage" : p.kind === "setup-mlflow" ? "Set up MLflow" : `Set up the pipeline server in ${target}`;
      steps.push({ id: p.kind, title, run: () => runAction(action, fn, target, false) });
    }
    if (steps.length > 0) setSetupConfirm({ feature, specs, steps });
  };

  const busy: Busy = { resAction, actionProject, migrating };
  const baseReason = mutateBlocker ?? (resAction || migrating || setupRun.run?.running ? BUSY_REASON : null);
  // After a teardown the namespace stays (deployed:false, managedByTool:true).
  // Offer Tear down again while tool objects are still reported (the data
  // PVC, a Route) or the last teardown from this tab did not finish.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute ||
    (lastMinIOTeardown !== null && ["partial_failure", "delete_failed", "in_progress", "forbidden", "unauthorized", "network"].includes(lastMinIOTeardown))
  );
  // Teardown keeps the storage namespaces and names them with the command
  // that deletes them (pkg/cluster s3KeptNamespaceHint).
  const keptNamespaces = [...(resResult?.message ?? "").matchAll(/`oc delete project ([a-z0-9-]+)`/g)].map((m) => m[1]);
  const modal = pending ? confirmOf(pending) : null;

  // Projects: the data science projects, and any project the status names.
  const projectNames = [...new Set([
    ...(projectList ?? []),
    ...pipelineServers.map((ps) => ps.namespace ?? ""),
    ...(resStatus?.unmanagedPipelineServers ?? []).map((ps) => ps.namespace ?? ""),
  ].filter(Boolean))].sort();
  const project = defaultProject(resStatus, projectNames, urlProject);
  const projectOptions: ProjectOption[] = projectNames.map((name) => {
    const status = pipelineServerStatus(resStatus, name, busy);
    const foreign = pipelineServerUnmanaged(resStatus, name);
    const has = !!pipelineServerIn(resStatus, name) || foreign;
    return { name, description: has ? `Pipeline server: ${envStatusText(status)}${foreign ? " · not managed by this tool" : ""}` : undefined };
  });

  const pipelines = pipelinesState(resStatus, project, projectNames, busy);
  const pipelinesSummary = pipelinesState(resStatus, null, projectNames, busy);
  const mlflowNow = mlflowState(resStatus, busy);
  const overviewRows: OverviewRow[] = [
    {
      id: "pipelines", name: "Pipelines", gives: "Run data science pipelines; artifacts in S3", state: pipelinesSummary,
      href: `?tab=pipelines${pipelinesSummary.project ? `&project=${encodeURIComponent(pipelinesSummary.project)}` : ""}`,
    },
    { id: "mlflow", name: "MLflow", gives: "Track experiments, runs and models", state: mlflowNow, href: "?tab=mlflow" },
  ];

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
        // fetched on request (the storage's admin password).
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
            {resResult && (
              <StackItem>
                <Alert
                  variant={outcomeVariant(resResult)}
                  title={outcomeTitle(resResult, "The action failed")}
                  isInline
                  isLiveRegion
                  component="p"
                  actionClose={<AlertActionCloseButton onClose={() => setResResult(null)} />}
                >
                  {resResult.success ? undefined : resResult.message}
                  {resResult.errorCode === "in_progress" ? " This page keeps checking." : ""}
                  {keptNamespaces.length > 0 && (
                    <> {keptNamespaces.length > 1 ? "Namespaces" : "Namespace"} {keptNamespaces.map((n, i) => <React.Fragment key={n}>{i > 0 ? " and " : ""}<code>{n}</code></React.Fragment>)} {keptNamespaces.length > 1 ? "were" : "was"} kept: run {keptNamespaces.map((n, i) => <React.Fragment key={n}>{i > 0 ? " and " : ""}<code>oc delete project {n}</code></React.Fragment>)} once you have checked {keptNamespaces.length > 1 ? "they are" : "it is"} empty.</>
                  )}
                </Alert>
              </StackItem>
            )}
            {resStatus && (
              <StackItem>
                {tab === "overview" && (
                  <Overview rows={overviewRows} baseReason={baseReason}
                    onSetUp={(row, setup) => setUp(row.name, setup)}
                    onShowIssue={(row, part) => { navigate({ tab: row.id as EnvironmentTab, ...(row.state.project ? { project: row.state.project } : {}) }); setFocusPart(part); }}
                    onOpenTab={(row) => navigate({ tab: row.id as EnvironmentTab })} />
                )}
                {tab === "pipelines" && (
                  <PipelinesTab status={resStatus} project={project} projects={projectOptions} onProject={(p) => navigate({ project: p })}
                    state={pipelines} busy={busy} baseReason={baseReason} readReason={readBlocker} minioLeftovers={minioLeftovers}
                    setPending={setPending} onMigrate={() => setMigrateOpen(true)} onSetUp={(setup) => setUp("Pipelines", setup)} />
                )}
                {tab === "mlflow" && (
                  <MLflowTab status={resStatus} state={mlflowNow} busy={busy} baseReason={baseReason} setPending={setPending} onSetUp={(setup) => setUp("MLflow", setup)} />
                )}
              </StackItem>
            )}
          </Stack>
        </PageSection>
      )}

      <MigrateStorageModal
        isOpen={migrateOpen}
        legacy={legacy}
        run={migration.run}
        baseReason={migrating ? null : mutateBlocker ?? (resAction || setupRun.run?.running ? BUSY_REASON : null)}
        onStart={(kind, deleteNamespace) => migration.start(kind, kind === "remove" ? { removeOldStorage: true, deleteNamespace } : {})}
        onClose={() => {
          setMigrateOpen(false);
          migration.reset();
        }}
      />

      {modal && (
        <ConfirmActionModal
          isOpen
          title={modal.title}
          changes={modal.changes}
          dataLoss={modal.dataLoss}
          confirmLabel={modal.confirm}
          confirmVariant={modal.danger ? "danger" : "primary"}
          isLoading={!!resAction}
          onConfirm={() => pending && execute(pending)}
          onCancel={() => setPending(null)}
        >
          {modal.extra}
        </ConfirmActionModal>
      )}

      {setupConfirm && (
        <ConfirmActionModal
          isOpen
          title={`Set up ${setupConfirm.feature}?`}
          changes={setupConfirm.specs.flatMap((s) => s.changes)}
          confirmLabel="Set up"
          onConfirm={() => {
            const { feature, steps } = setupConfirm;
            setSetupConfirm(null);
            setSetupRunOpen(true);
            void setupRun.start(`Set up ${feature}`, steps);
          }}
          onCancel={() => setSetupConfirm(null)}
        >
          <Stack hasGutter>
            {setupConfirm.specs.map((s, i) => s.extra && <StackItem key={i}>{s.extra}</StackItem>)}
            {setupConfirm.steps.length > 1 && <StackItem><Content component="p">The parts are set up one after another; a part that fails stops the rest.</Content></StackItem>}
          </Stack>
        </ConfirmActionModal>
      )}

      <SetupRunModal run={setupRun.run} isOpen={setupRunOpen} onClose={() => { setSetupRunOpen(false); setupRun.reset(); }} />
    </>
  );
};
