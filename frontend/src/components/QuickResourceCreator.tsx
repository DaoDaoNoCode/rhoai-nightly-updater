import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Content,
  Drawer,
  DrawerContent,
  DrawerContentBody,
  EmptyState,
  EmptyStateBody,
  Form,
  FormGroup,
  Gallery,
  PageSection,
  Stack,
  StackItem,
  Tab,
  TabContent,
  TabContentBody,
  Tabs,
  TabTitleText,
  Title,
} from "@patternfly/react-core";
import t_global_breakpoint_xl from "@patternfly/react-tokens/dist/esm/t_global_breakpoint_xl";
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
import { DETAILS_TITLE_ID, DetailsPanel } from "./environment/ResourceCard";
import { envStatusText } from "./environment/resourceStatus";
import { StorageCard, StorageDetails, storageConfirm, storageNotReadyReason, storageStatus, type StorageProps } from "./environment/StorageCard";
import { PipelineServerCard, PipelineServerDetails, pipelineServerConfirm, pipelineServerStatus, type PipelineServerProps } from "./environment/PipelineServerCard";
import { MLflowCard, MLflowDetails, mlflowConfirm, mlflowStatus, type MLflowProps } from "./environment/MLflowCard";
import { MigrateStorageModal } from "./environment/MigrateStorageModal";
import { useMigrateStorage } from "./environment/useMigrateStorage";
import { ProjectSelect, type ProjectOption } from "./environment/ProjectSelect";
import { GoalCard, GoalRunModal, type Goal } from "./environment/GoalCard";
import { useGoalRun, type GoalRunStep } from "./environment/useGoalRun";

export { nextStep, terminalReason } from "./environment/resourceRow";

const BUSY_REASON = "Another operation is in progress";

/** The tabs, in the URL as ?tab= (overview is the default and is left out). */
export const ENVIRONMENT_TABS = ["overview", "cluster", "projects"] as const;
export type EnvironmentTab = (typeof ENVIRONMENT_TABS)[number];
const TAB_TITLES: Record<EnvironmentTab, string> = { overview: "Overview", cluster: "Cluster", projects: "Projects" };

type DetailsId = "storage" | "mlflow" | "pipeline-server";
const DETAILS_TAB: Record<DetailsId, EnvironmentTab> = { storage: "cluster", mlflow: "cluster", "pipeline-server": "projects" };

/** Whether a media query matches, following changes (window resizes). */
function useMediaQuery(query: string): boolean {
  const get = () => typeof window.matchMedia === "function" && window.matchMedia(query).matches;
  const [matches, setMatches] = useState(get);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(query);
    const onChange = () => setMatches(mq.matches);
    onChange();
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [query]);
  return matches;
}

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
 * The Environment page's one-click setups: it loads and polls their status,
 * shows them in three tabs (Overview with goals, Cluster, Projects) with a
 * details drawer, runs the confirmed actions and shows their result. The tab
 * and the project are in the URL (?tab=, ?project=), so links, reloads and
 * the browser's back and forward buttons keep the view.
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
  const tabParam = searchParams.get("tab");
  const tab: EnvironmentTab = (ENVIRONMENT_TABS as readonly string[]).includes(tabParam ?? "") ? (tabParam as EnvironmentTab) : "overview";
  const project = searchParams.get("project") || null;
  const [visited, setVisited] = useState<Set<EnvironmentTab>>(() => new Set([tab]));
  useEffect(() => setVisited((v) => (v.has(tab) ? v : new Set(v).add(tab))), [tab]);
  const navigate = useCallback((next: { tab?: EnvironmentTab; project?: string | null }) => {
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
    });
  }, [setSearchParams]);

  // The details drawer: which resource, and the element that opened it
  // (the focus goes back there when it closes).
  const [details, setDetails] = useState<DetailsId | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  const shownDetails = details && DETAILS_TAB[details] === tab && (details !== "pipeline-server" || !!project) ? details : null;
  const drawerInline = useMediaQuery(`(min-width: ${t_global_breakpoint_xl.value})`);
  const openDetails = useCallback((id: DetailsId, from: HTMLElement | null) => {
    opener.current = from;
    setDetails(id);
  }, []);
  const closeDetails = useCallback(() => {
    setDetails(null);
    opener.current?.focus();
  }, []);
  useEffect(() => {
    if (shownDetails) document.getElementById(DETAILS_TITLE_ID)?.focus();
  }, [shownDetails]);

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

  /** Runs one setup request; showResult puts its outcome in the page's alert (goal runs show theirs in their dialog). */
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

  const goalRun = useGoalRun();
  const [goalRunOpen, setGoalRunOpen] = useState(false);
  const [goalConfirm, setGoalConfirm] = useState<{ goal: string; title: string; specs: ConfirmSpec[]; steps: GoalRunStep[] } | null>(null);

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

  const baseReason = mutateBlocker ?? (resAction || migrating || goalRun.run?.running ? BUSY_REASON : null);
  const unmanagedProjects = new Set(resStatus?.unmanagedPipelineProjects ?? []);
  // After a teardown the namespace stays (deployed:false, managedByTool:true).
  // Offer Tear down again while tool objects are still reported (the data
  // PVC, a Route) or the last teardown from this tab did not finish.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute ||
    (lastMinIOTeardown !== null && ["partial_failure", "delete_failed", "in_progress", "forbidden", "unauthorized", "network"].includes(lastMinIOTeardown))
  );
  const actionProps = { baseReason, resAction, setPending };
  // Teardown keeps the storage namespaces and names them with the command
  // that deletes them (pkg/cluster s3KeptNamespaceHint).
  const keptNamespaces = [...(resResult?.message ?? "").matchAll(/`oc delete project ([a-z0-9-]+)`/g)].map((m) => m[1]);

  const modal = pending
    ? storageConfirm(pending, minio, legacy) ?? mlflowConfirm(pending, mlflow) ?? pipelineServerConfirm(pending, pipelineServers)
    : null;

  const storageProps: StorageProps = {
    ...actionProps, minio, legacy, minioLeftovers, migrating, readReason: readBlocker,
    onMigrate: () => setMigrateOpen(true),
  };
  const mlflowProps: MLflowProps = { ...actionProps, mlflow };
  const storageReason = storageNotReadyReason(minio, legacy, migrating);
  const serverIn = (ns: string) => pipelineServers.find((ps) => ps.namespace === ns);
  const pipelineProps = (ns: string): PipelineServerProps => ({
    ...actionProps, project: ns, server: serverIn(ns), unmanaged: unmanagedProjects.has(ns),
    storageReason, storageReady: !!minio?.ready, legacy, actionProject,
    onShowStorage: () => { navigate({ tab: "cluster" }); openDetails("storage", null); },
  });

  // The project picker lists what this page has set up in each project.
  const projectNames = [...new Set([...(projectList ?? []), ...pipelineServers.map((ps) => ps.namespace ?? "").filter(Boolean)])].sort();
  const projectOptions: ProjectOption[] = projectNames.map((name) => {
    const status = pipelineServerStatus({ server: serverIn(name), unmanaged: unmanagedProjects.has(name), resAction, actionProject, project: name });
    return { name, description: status === "not-set-up" ? undefined : `Pipeline server: ${envStatusText(status)}` };
  });
  const projectKnown = !project || projectList === null || projectNames.includes(project);

  // --- Goals (Overview) ---
  const showOn = (id: DetailsId) => () => { navigate({ tab: DETAILS_TAB[id] }); openDetails(id, null); };
  const sStatus = storageStatus(storageProps);
  const psProps = project ? pipelineProps(project) : null;
  const psStatus = psProps ? pipelineServerStatus(psProps) : "not-set-up";
  const mStatus = mlflowStatus(mlflowProps);
  const goals: Goal[] = [
    {
      id: "pipelines", title: "Run a data science pipeline", projectScoped: true,
      description: "Pipelines need the cluster's S3 storage and a pipeline server in the project.",
      steps: [
        { id: "storage", title: "Storage", noun: "the storage", scope: "Cluster", status: sStatus, canSetUp: sStatus === "not-set-up" && !legacy && !minio?.setupBlockedReason, show: showOn("storage") },
        { id: "pipeline-server", title: "Pipeline server", noun: "the pipeline server", scope: project ? `Project ${project}` : "Project", status: psStatus, canSetUp: psStatus === "not-set-up", show: showOn("pipeline-server") },
      ],
      open: psProps?.server?.ready && psProps.server.uiRoute ? { label: "Open pipelines", href: psProps.server.uiRoute } : undefined,
    },
    {
      id: "mlflow", title: "Track experiments with MLflow", projectScoped: false,
      description: "Log runs, metrics and models to an MLflow server on the cluster.",
      steps: [{ id: "mlflow", title: "MLflow", noun: "MLflow", scope: "Cluster", status: mStatus, canSetUp: mStatus === "not-set-up" && !mlflow?.setupBlockedReason, show: showOn("mlflow") }],
      open: mlflow?.ready && mlflow.uiRoute ? { label: "Open MLflow", href: mlflow.uiRoute } : undefined,
    },
  ];
  const goalSetUp = (goal: Goal) => {
    const specs: ConfirmSpec[] = [];
    const steps: GoalRunStep[] = [];
    for (const step of goal.steps) {
      if (!step.canSetUp) continue;
      const p: Pending | null = step.id === "storage" ? { kind: "setup-minio" } : step.id === "mlflow" ? { kind: "setup-mlflow" } : project ? { kind: "setup-pipeline-server", project } : null;
      if (!p) continue;
      const spec = storageConfirm(p, minio, legacy) ?? mlflowConfirm(p, mlflow) ?? pipelineServerConfirm(p, pipelineServers);
      if (spec) specs.push(spec);
      const [action, fn, target] = request(p);
      steps.push({ id: step.id, title: `Set up ${step.title.toLowerCase()}${target ? ` in ${target}` : ""}`, run: () => runAction(action, fn, target, false) });
    }
    setGoalConfirm({ goal: goal.title, title: `${goal.title}: set up ${steps.length > 1 ? `${steps.length} parts` : "1 part"}?`, specs, steps });
  };

  const tabContent = (key: EnvironmentTab, body: React.ReactNode) => visited.has(key) && (
    <TabContent key={key} eventKey={key} activeKey={tab} id={`environment-tab-${key}`} aria-labelledby={`environment-tab-${key}-title`} hidden={key !== tab}>
      <TabContentBody>{body}</TabContentBody>
    </TabContent>
  );

  const panel = shownDetails === "storage" ? (
    <DetailsPanel title="Storage" status={sStatus} onClose={closeDetails}><StorageDetails {...storageProps} /></DetailsPanel>
  ) : shownDetails === "mlflow" ? (
    <DetailsPanel title="MLflow" status={mStatus} onClose={closeDetails}><MLflowDetails {...mlflowProps} /></DetailsPanel>
  ) : shownDetails === "pipeline-server" && psProps ? (
    <DetailsPanel title={`Pipeline server in ${project}`} status={psStatus} onClose={closeDetails}><PipelineServerDetails {...psProps} /></DetailsPanel>
  ) : undefined;

  return (
    <>
      {renderHeader?.({ loading: statusLoading, lastRefreshed, onRefresh: checkAgain })}
      <PageSection type="tabs">
        <Tabs activeKey={tab} onSelect={(_e, k) => navigate({ tab: k as EnvironmentTab })} usePageInsets aria-label="Environment views">
          {ENVIRONMENT_TABS.map((key) => (
            <Tab key={key} eventKey={key} id={`environment-tab-${key}-title`} title={<TabTitleText>{TAB_TITLES[key]}</TabTitleText>} tabContentId={`environment-tab-${key}`} />
          ))}
        </Tabs>
      </PageSection>
      {!resStatus && !resError ? (
        <PageLoading title="Loading the environment" />
      ) : (
        <PageSection isFilled padding={{ default: "noPadding" }} aria-label="Environment">
          <Drawer isExpanded={!!panel} isInline={drawerInline}>
            <DrawerContent panelContent={panel}>
              <DrawerContentBody>
                {/* A page section inside the drawer content: the page's insets, like the header and the tabs. */}
                <PageSection>
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
                  <StackItem>
                    {tabContent("overview", (
                      <Stack hasGutter>
                        <StackItem><Title headingLevel="h2" size="xl">What do you want to do?</Title></StackItem>
                        <StackItem>
                          <Gallery hasGutter minWidths={{ default: "100%", md: "360px" }}>
                            {goals.map((goal) => (
                              <GoalCard key={goal.id} goal={goal} projects={projectOptions} project={project} baseReason={baseReason}
                                onProject={(p) => navigate({ project: p })} onSetUp={() => goalSetUp(goal)} />
                            ))}
                          </Gallery>
                        </StackItem>
                      </Stack>
                    ))}
                    {tabContent("cluster", (
                      <Stack hasGutter>
                        <StackItem><Title headingLevel="h2" size="xl">Shared by every project</Title></StackItem>
                        <StackItem>
                          <Gallery hasGutter minWidths={{ default: "100%", md: "320px" }}>
                            <StorageCard {...storageProps} detailsOpen={shownDetails === "storage"} onDetails={(el) => openDetails("storage", el)} />
                            <MLflowCard {...mlflowProps} detailsOpen={shownDetails === "mlflow"} onDetails={(el) => openDetails("mlflow", el)} />
                          </Gallery>
                        </StackItem>
                      </Stack>
                    ))}
                    {tabContent("projects", (
                      <Stack hasGutter>
                        <StackItem>
                          <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
                            <FormGroup label="Project" fieldId="environment-project">
                              <ProjectSelect id="environment-project" projects={projectOptions} selected={project} onSelect={(p) => navigate({ project: p })} />
                            </FormGroup>
                          </Form>
                        </StackItem>
                        <StackItem>
                          {!project ? (
                            <EmptyState headingLevel="h2" titleText="Choose a project" variant="sm">
                              <EmptyStateBody>The setups of a data science project show here once you choose it above.</EmptyStateBody>
                            </EmptyState>
                          ) : !projectKnown ? (
                            <Alert variant="warning" isInline component="p" title={`Project ${project} is not a data science project on this cluster`}>
                              It may have been deleted. Choose another project above.
                            </Alert>
                          ) : (
                            <Gallery hasGutter minWidths={{ default: "100%", md: "320px" }}>
                              <PipelineServerCard {...psProps!} detailsOpen={shownDetails === "pipeline-server"} onDetails={(el) => openDetails("pipeline-server", el)} />
                            </Gallery>
                          )}
                        </StackItem>
                      </Stack>
                    ))}
                  </StackItem>
                </Stack>
                </PageSection>
              </DrawerContentBody>
            </DrawerContent>
          </Drawer>
        </PageSection>
      )}

      <MigrateStorageModal
        isOpen={migrateOpen}
        legacy={legacy}
        run={migration.run}
        baseReason={migrating ? null : mutateBlocker ?? (resAction || goalRun.run?.running ? BUSY_REASON : null)}
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

      {goalConfirm && (
        <ConfirmActionModal
          isOpen
          title={goalConfirm.title}
          changes={goalConfirm.specs.flatMap((s) => s.changes)}
          confirmLabel="Set up"
          onConfirm={() => {
            const { goal, steps } = goalConfirm;
            setGoalConfirm(null);
            setGoalRunOpen(true);
            void goalRun.start(goal, steps);
          }}
          onCancel={() => setGoalConfirm(null)}
        >
          <Stack hasGutter>
            {goalConfirm.specs.map((s, i) => s.extra && <StackItem key={i}>{s.extra}</StackItem>)}
            {goalConfirm.steps.length > 1 && <StackItem><Content component="p">The parts are set up one after another; a part that fails stops the rest.</Content></StackItem>}
          </Stack>
        </ConfirmActionModal>
      )}

      <GoalRunModal run={goalRun.run} isOpen={goalRunOpen} onClose={() => { setGoalRunOpen(false); goalRun.reset(); }} />
    </>
  );
};
