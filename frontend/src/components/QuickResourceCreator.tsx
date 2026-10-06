import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  EmptyState,
  EmptyStateBody,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  MenuToggle,
  MenuToggleElement,
  PageSection,
  Select,
  SelectList,
  SelectOption,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import LockIcon from "@patternfly/react-icons/dist/esm/icons/lock-icon";
import PlusCircleIcon from "@patternfly/react-icons/dist/esm/icons/plus-circle-icon";
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
  toApiError,
} from "../services/api";
import { exponentialBackoff, usePolling } from "../hooks/usePolling";
import { RESOURCE_POLL_BASE_MS, RESOURCE_POLL_MAX_MS, RESOURCE_SETTLE_MAX_MS } from "../constants";
import { TooltipButton } from "./TooltipButton";
import { ConfirmActionModal } from "./ConfirmActionModal";
import { CardList, CardListItem } from "./CardList";
import { StatusLabel, TagLabel } from "./StatusLabel";
import { TruncatedText } from "./LongText";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";

/**
 * Container waiting reasons that do not resolve on their own (Kubernetes
 * docs, "Debug Pods": ImagePullBackOff/ErrImagePull, CrashLoopBackOff, and
 * configuration errors). Polling stops for these instead of spinning forever.
 */
const TERMINAL_REASONS = [
  "ImagePullBackOff",
  "ErrImagePull",
  "InvalidImageName",
  "CrashLoopBackOff",
  "CreateContainerConfigError",
  "CreateContainerError",
  "FailingToDeploy",
];

/** Why a deployed resource can't become ready without a change, or null. */
export function terminalReason(state: ResourceState | undefined): string | null {
  if (!state || !state.deployed || state.ready) return null;
  if (state.terminalError) return state.waitingReason || "Failed";
  const candidates = [state.waitingReason, state.message].filter((v): v is string => !!v);
  for (const text of candidates) {
    const reason = TERMINAL_REASONS.find((r) => text.includes(r));
    if (reason) return state.message && state.message !== reason ? state.message : reason;
  }
  return null;
}

function isTerminating(state: ResourceState | undefined): boolean {
  return !!state && (!!state.terminating || state.message === "Terminating");
}

type ResourceKind = "minio" | "mlflow" | "pipeline";

/** What to do about a resource that cannot start (A08-3, A06-5). */
export function nextStep(kind: ResourceKind, state: ResourceState, minioReady = true): string | null {
  if (!terminalReason(state)) return null;
  const text = `${state.waitingReason ?? ""} ${state.message ?? ""}`;
  const managed = state.managedByTool !== false;
  if (/ImagePullBackOff|ErrImagePull|InvalidImageName/.test(text)) {
    if (kind === "minio" && managed) return "The image cannot be pulled. Repair applies the MinIO Deployment again with the image this updater is configured to use; the stored data is kept.";
    if (kind === "mlflow" && state.prOverride) return "The PR image cannot be pulled. Revert to the image used before the PR, or deploy another PR.";
    return "The image cannot be pulled. Check that the image exists and that the cluster pull secret can read it.";
  }
  if (/CrashLoopBackOff/.test(text)) {
    if (kind === "mlflow" && state.prOverride) return "The PR build keeps crashing. Revert to the image used before the PR, or deploy another PR.";
    return "The container keeps crashing. Read its logs in the OpenShift console.";
  }
  if (/CreateContainerConfigError|CreateContainerError/.test(text)) {
    return "A Secret or ConfigMap the pod needs is missing or invalid. The message above names it.";
  }
  if (kind === "pipeline") {
    return minioReady
      ? "The pipeline server did not become ready within 5 minutes. Check its conditions in the project's Pipelines page."
      : "The pipeline server cannot reach MinIO. Fix MinIO first; the pipeline server recovers once MinIO serves requests.";
  }
  return null;
}

const BUSY_REASON = "Another operation is in progress";

interface QuickResourceCreatorProps {
  /**
   * Why cluster changes are disabled now, or null: the app-wide
   * useMutationBlocker (permissions, session, the operation lock).
   */
  mutateBlocker: string | null;
  /** Sees every action result (e.g. to pick up a cluster_busy refusal). */
  onResult?: (res: OperationResponse) => void;
  /** The page header, given the refresh state of the resource status. */
  renderHeader?: (header: { loading: boolean; lastRefreshed: Date | null; onRefresh: () => void }) => React.ReactNode;
}

const ResourceStatus: React.FC<{ state: ResourceState }> = ({ state }) => {
  if (isTerminating(state)) return <StatusLabel status="warning">Terminating</StatusLabel>;
  if (state.ready) return <StatusLabel status="success">Running</StatusLabel>;
  if (!state.deployed) return <StatusLabel status="neutral">Not deployed</StatusLabel>;
  const reason = terminalReason(state);
  if (reason) {
    const short = state.waitingReason || (reason !== "Failed" && reason.length <= 40 ? reason : "");
    return <StatusLabel status="danger">{short ? `Failed: ${short}` : "Failed"}</StatusLabel>;
  }
  return <StatusLabel status="progress">Starting</StatusLabel>;
};

/** The resource's message, warnings and next step as helper text under its description. */
const StatusDetails: React.FC<{ kind: ResourceKind; state: ResourceState; minioReady?: boolean; teardownBlocked?: string | null }> = ({ kind, state, minioReady, teardownBlocked }) => {
  const step = nextStep(kind, state, minioReady);
  // The label already says "Starting" / "Terminating": don't repeat it as the message.
  const showMessage = state.deployed && !state.ready && !!state.message && !["Terminating", "Starting"].includes(state.message);
  const items: React.ReactNode[] = [];
  if (showMessage) {
    items.push(<HelperTextItem key="message" variant={terminalReason(state) ? "error" : "default"}><span className="pf-v6-u-text-break-word"><TruncatedText>{state.message}</TruncatedText></span></HelperTextItem>);
  }
  if (state.warning) items.push(<HelperTextItem key="warning" variant="warning">{state.warning}</HelperTextItem>);
  if (step) items.push(<HelperTextItem key="step">Next step: {step}</HelperTextItem>);
  if (teardownBlocked) items.push(<HelperTextItem key="blocked" variant="warning">Tear down is blocked: {teardownBlocked}</HelperTextItem>);
  return items.length > 0 ? <HelperText className="pf-v6-u-mt-sm">{items}</HelperText> : null;
};

/** Why the tool leaves an object it did not create alone, appended to the row description. */
function unmanagedNote(state: ResourceState | undefined): string {
  return state?.managedByTool === false && state.teardownBlockedReason ? ` ${state.teardownBlockedReason}` : "";
}

/** Name, status and category tags of one resource row. */
const RowTitle: React.FC<{ id: string; name: string; state?: ResourceState; extra?: React.ReactNode }> = ({ id, name, state, extra }) => (
  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
    <FlexItem><strong id={id}>{name}</strong></FlexItem>
    {state && <FlexItem><ResourceStatus state={state} /></FlexItem>}
    {state?.managedByTool === false && <FlexItem><TagLabel icon={<LockIcon />}>Not managed by this tool</TagLabel></FlexItem>}
    {extra}
  </Flex>
);

const ExternalLink: React.FC<{ href: string; children: React.ReactNode }> = ({ href, children }) => (
  <Button variant="link" component="a" href={href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{children}</Button>
);

type Pending =
  | { kind: "setup-minio" | "repair-minio" | "setup-mlflow" | "revert-mlflow" | "teardown-minio" | "teardown-mlflow" }
  | { kind: "deploy-mlflow-pr"; pr: number }
  | { kind: "setup-pipeline-server" | "teardown-pipeline-server"; project: string; name?: string };

export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ mutateBlocker, onResult, renderHeader }) => {
  const [resStatus, setResStatus] = useState<ResourcesStatus | null>(null);
  const [resAction, setResAction] = useState<string | null>(null);
  const [resResult, setResResult] = useState<OperationResponse | null>(null);
  // errorCode of this tab's last MinIO teardown that did not succeed.
  const [lastMinIOTeardown, setLastMinIOTeardown] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [projectList, setProjectList] = useState<string[]>([]);
  const [addProjectOpen, setAddProjectOpen] = useState(false);
  const [mlflowPR, setMlflowPR] = useState("");

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
    getDSProjects().then((r) => setProjectList(r.projects || [])).catch(() => {});
  }, []);

  useEffect(() => { refreshProjects(); }, [refreshProjects]);

  const minio = resStatus?.minio;
  const mlflow = resStatus?.mlflow;
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

  const handleResourceAction = async (action: string, fn: () => Promise<OperationResponse>) => {
    trackFeature(action);
    setPending(null);
    setResAction(action);
    setResResult(null);
    setPollGaveUp(false);
    setPollRound((n) => n + 1);
    let res: OperationResponse;
    try {
      res = await fn();
    } catch (e) {
      res = errorResult(e, "Operation failed");
    }
    setResResult(res);
    setResAction(null);
    if (action === "teardown-minio") setLastMinIOTeardown(res.success ? null : res.errorCode || "failed");
    else if (action === "setup-minio" && res.success) setLastMinIOTeardown(null);
    onResult?.(res);
    void fetchResources();
    refreshProjects();
  };

  const execute = (p: Pending) => {
    switch (p.kind) {
      case "setup-minio": return handleResourceAction("setup-minio", setupMinIO);
      case "repair-minio": return handleResourceAction("setup-minio", setupMinIO);
      case "teardown-minio": return handleResourceAction("teardown-minio", teardownMinIO);
      case "setup-mlflow": return handleResourceAction("setup-mlflow", setupMLflow);
      case "teardown-mlflow": return handleResourceAction("teardown-mlflow", teardownMLflow);
      case "deploy-mlflow-pr": return handleResourceAction("deploy-mlflow-pr", () => deployMLflowPR(p.pr));
      case "revert-mlflow": return handleResourceAction("revert-mlflow", revertMLflow);
      case "setup-pipeline-server": return handleResourceAction("setup-pipeline-server", () => setupPipelineServer(p.project));
      case "teardown-pipeline-server": return handleResourceAction("teardown-pipeline-server", () => teardownPipelineServer(p.project));
    }
  };

  const baseReason = mutateBlocker ?? (resAction ? BUSY_REASON : null);
  const unmanagedProjects = new Set(resStatus?.unmanagedPipelineProjects ?? []);
  const availableProjects = projectList.filter((p) => !pipelineServers.some((ps) => ps.namespace === p) && !unmanagedProjects.has(p));
  const mlflowPRValid = /^\d+$/.test(mlflowPR) && Number(mlflowPR) > 0;
  const minioReady = !!minio?.ready;
  // After a teardown the namespace stays (deployed:false, managedByTool:true).
  // Offer Tear down again while tool objects are still reported (the data
  // PVC, a Route) or the last teardown from this tab did not finish.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute ||
    (lastMinIOTeardown !== null && ["partial_failure", "delete_failed", "in_progress", "forbidden", "unauthorized", "network"].includes(lastMinIOTeardown))
  );
  // N10: every cause is a tooltip on the toggle, which stays focusable (aria-disabled).
  const addPipelineReason = baseReason ?? (!minioReady ? "Available once MinIO is running." : null);
  const pvcList = (state: ResourceState | undefined) => (state?.dataPVCs ?? []);

  const modal = (() => {
    if (!pending) return null;
    const pvcs = (s: ResourceState | undefined, what: string) => pvcList(s).length > 0
      ? <>PersistentVolumeClaim{pvcList(s).length > 1 ? "s" : ""} {pvcList(s).map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>)}, with {what}.</>
      : undefined;
    switch (pending.kind) {
      case "setup-minio":
      case "repair-minio":
        return {
          title: pending.kind === "repair-minio" ? "Repair MinIO?" : "Set up MinIO?",
          confirm: pending.kind === "repair-minio" ? "Repair MinIO" : "Set up MinIO",
          changes: [
            <>Namespace <code>minio</code> {pending.kind === "repair-minio" || minio?.managedByTool ? "is kept and reused" : "is created"}, labelled as managed by this tool.</>,
            <>Deployment <code>minio</code>, Service <code>minio-service</code>, Secret <code>minio-secret</code>, PersistentVolumeClaim <code>minio-pvc</code>, the console Route <code>minio-ui</code> and the bucket <code>pipelines</code> are applied with the image the server is configured to use.</>,
            <>Route <code>minio-api</code> (MinIO&apos;s S3 API outside the cluster) is removed if it exists; pipeline servers use the in-cluster service.</>,
          ],
          extra: <Content component="p">The server waits up to 90 seconds for MinIO to become ready and reports the pod&apos;s reason if it cannot start.{pending.kind === "repair-minio" ? " Stored data is kept." : ""}</Content>,
        };
      case "teardown-minio":
        return {
          title: "Tear down MinIO?", confirm: "Tear down MinIO", danger: true,
          changes: [
            <>The MinIO objects this tool created in <code>{minio?.namespace || "minio"}</code> are deleted: Deployment <code>minio</code>, Routes <code>minio-ui</code> and <code>minio-api</code>, Service <code>minio-service</code>, Secret <code>minio-secret</code> and PersistentVolumeClaim <code>minio-pvc</code>. Objects with these names that the tool did not create are kept.</>,
            <>Namespace <code>{minio?.namespace || "minio"}</code> is kept. Delete it yourself with <code>oc delete project minio</code> once it is empty.</>,
          ],
          dataLoss: pvcs(minio, "every object stored in MinIO (pipeline artifacts, uploaded files)") ?? <>Every object stored in MinIO (pipeline artifacts, uploaded files).</>,
          extra: <Content component="p">The server refuses while a pipeline server still uses MinIO; the message names it. If some objects cannot be deleted, run Tear down again.</Content>,
        };
      case "setup-mlflow":
        return {
          title: "Set up MLflow?", confirm: "Set up MLflow",
          changes: [<>MLflow CR <code>mlflow</code> is created; the MLflow operator deploys the server in <code>redhat-ods-applications</code> with its default (RHOAI) image.</>],
        };
      case "teardown-mlflow":
        return {
          title: "Tear down MLflow?", confirm: "Tear down MLflow", danger: true,
          changes: [<>MLflow CR <code>mlflow</code> is deleted; the operator removes the server.</>],
          dataLoss: pvcs(mlflow, "every experiment, run and artifact stored in it") ?? <>Every experiment, run and artifact stored in MLflow.</>,
        };
      case "deploy-mlflow-pr":
        return {
          title: `Deploy MLflow PR #${pending.pr}?`, confirm: `Deploy PR #${pending.pr}`,
          changes: [
            <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is set to <code>quay.io/opendatahub/mlflow:odh-pr-{pending.pr}</code>, pinned to its current digest.</>,
            ...(!mlflow?.prOverride ? [<>The image in use now is saved on the CR so Revert can restore it.</>] : []),
          ],
          extra: (
            <Stack hasGutter>
              <StackItem><Content component="p">The MLflow server restarts; it is unavailable until the new pod is ready.</Content></StackItem>
              {mlflow?.managedByTool === false && (
                <StackItem><Alert component="p" variant="warning" isInline title="This MLflow instance was not created by this tool">Someone else may rely on it. Revert restores its current image.</Alert></StackItem>
              )}
            </Stack>
          ),
        };
      case "revert-mlflow":
        return {
          title: "Revert MLflow?", confirm: "Revert MLflow",
          changes: [mlflow?.revertImage
            ? <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> goes back to <code>{mlflow.revertImage}</code>, the image used before the PR.</>
            : <>MLflow CR <code>mlflow</code>: <code>spec.image.image</code> is removed, so the operator&apos;s default (RHOAI) image is used again.</>],
          extra: <Content component="p">The MLflow server restarts.</Content>,
        };
      case "setup-pipeline-server":
        return {
          title: `Set up a pipeline server in ${pending.project}?`, confirm: "Set up",
          changes: [
            <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>nightly-dspa</code> and Secret <code>nightly-dspa-s3</code> are created, using MinIO for storage.</>,
          ],
          extra: <Content component="p">The pipeline server takes 1 to 3 minutes to become ready.</Content>,
        };
      case "teardown-pipeline-server": {
        const ps = pipelineServers.find((p) => p.namespace === pending.project);
        return {
          title: `Tear down the pipeline server in ${pending.project}?`, confirm: "Tear down", danger: true,
          changes: [<>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name || "nightly-dspa"}</code> and its S3 credentials Secret are deleted. The project stays.</>],
          dataLoss: pvcs(ps, "the pipeline database (runs, experiments and their history)") ?? <>The pipeline database (runs, experiments and their history).</>,
        };
      }
    }
  })();

  const addPipelineControl = addPipelineReason ? (
    <TooltipButton variant="secondary" icon={<PlusCircleIcon />} disabledReason={addPipelineReason}>Add to a project</TooltipButton>
  ) : (
    <Select
      isOpen={addProjectOpen}
      onOpenChange={setAddProjectOpen}
      onSelect={(_e, val) => {
        setAddProjectOpen(false);
        if (val) setPending({ kind: "setup-pipeline-server", project: val as string });
      }}
      popperProps={{ position: "right" }}
      toggle={(toggleRef: React.Ref<MenuToggleElement>) => (
        <MenuToggle ref={toggleRef} variant="secondary" icon={<PlusCircleIcon />} isExpanded={addProjectOpen} onClick={() => setAddProjectOpen(!addProjectOpen)}>
          {resAction === "setup-pipeline-server" ? "Setting up..." : "Add to a project"}
        </MenuToggle>
      )}
    >
      <SelectList aria-label="Data science projects">
        {availableProjects.map((p) => <SelectOption key={p} value={p}>{p}</SelectOption>)}
        {availableProjects.length === 0 && <SelectOption isDisabled value="">No project without a pipeline server</SelectOption>}
      </SelectList>
    </Select>
  );

  const rowActions = (children: React.ReactNode) => (
    <>
      <Flex gap={{ default: "gapSm" }} justifyContent={{ md: "justifyContentFlexEnd" }} flexWrap={{ default: "wrap" }}>{children}</Flex>
    </>
  );

  return (
    <>
      {renderHeader?.({ loading: statusLoading, lastRefreshed, onRefresh: checkAgain })}
      <PageSection isFilled>
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
                {/oc delete project minio/.test(resResult.message ?? "") && (
                  <> Namespace <code>minio</code> was kept: run <code>oc delete project minio</code> once you have checked it is empty.</>
                )}
              </Alert>
            </StackItem>
          )}

          {/* Storage */}
          <StackItem>
            <Card>
              <CardHeader><CardTitle><Title headingLevel="h2" size="lg">Storage</Title></CardTitle></CardHeader>
              <CardBody>
                <CardList aria-label="Storage">
                  <CardListItem labelledBy="minio-item" actions={rowActions(
                            <>
                              {minio?.ready && minio.uiRoute && <FlexItem><ExternalLink href={minio.uiRoute}>Open console</ExternalLink></FlexItem>}
                              {minio && !minio.deployed && !isTerminating(minio) && (
                                <FlexItem>
                                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "setup-minio" })} isLoading={resAction === "setup-minio"}
                                    disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Set up</TooltipButton>
                                </FlexItem>
                              )}
                              {minio?.deployed && minio.managedByTool !== false && !!terminalReason(minio) && !isTerminating(minio) && (
                                <FlexItem>
                                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "repair-minio" })} isLoading={resAction === "setup-minio"}
                                    disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Repair</TooltipButton>
                                </FlexItem>
                              )}
                              {minio && minio.managedByTool !== false && !isTerminating(minio) && (minio.deployed || minioLeftovers) && (
                                <FlexItem>
                                  <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-minio" })} isLoading={resAction === "teardown-minio"}
                                    disabledReason={baseReason ?? minio.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                                </FlexItem>
                              )}
                            </>,
                  )}>
                            <RowTitle id="minio-item" name="MinIO object storage" state={minio} />
                            <Content component="p" className="pf-v6-u-text-color-subtle">Namespace <code>minio</code>: S3 storage for pipeline artifacts and test files.{unmanagedNote(minio)}</Content>
                            {minio && (
                              <StatusDetails
                                kind="minio"
                                state={minio}
                                teardownBlocked={minio.deployed && minio.managedByTool !== false ? minio.teardownBlockedReason : null}
                              />
                            )}
                  </CardListItem>
                </CardList>
              </CardBody>
            </Card>
          </StackItem>

          {/* MLflow */}
          <StackItem>
            <Card>
              <CardHeader><CardTitle><Title headingLevel="h2" size="lg">MLflow</Title></CardTitle></CardHeader>
              <CardBody>
                <CardList aria-label="MLflow">
                  <CardListItem labelledBy="mlflow-item" actions={rowActions(
                            <>
                              {mlflow?.ready && mlflow.uiRoute && <FlexItem><ExternalLink href={mlflow.uiRoute}>Open MLflow</ExternalLink></FlexItem>}
                              {mlflow && !mlflow.deployed && (
                                <FlexItem>
                                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "setup-mlflow" })} isLoading={resAction === "setup-mlflow"}
                                    disabledReason={baseReason ?? mlflow.setupBlockedReason ?? null}>Set up</TooltipButton>
                                </FlexItem>
                              )}
                              {mlflow?.deployed && mlflow.managedByTool !== false && !isTerminating(mlflow) && (
                                <FlexItem>
                                  <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-mlflow" })} isLoading={resAction === "teardown-mlflow"}
                                    disabledReason={baseReason ?? mlflow.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                                </FlexItem>
                              )}
                            </>,
                  )}>
                            <RowTitle id="mlflow-item" name="MLflow server" state={mlflow}
                              extra={mlflow?.prOverride ? <FlexItem><TagLabel color="blue">PR #{mlflow.prNumber ?? "?"}</TagLabel></FlexItem> : undefined} />
                            <Content component="p" className="pf-v6-u-text-color-subtle">An MLflow instance in <code>redhat-ods-applications</code>, managed by the MLflow operator.{unmanagedNote(mlflow)}</Content>
                            {mlflow && <StatusDetails kind="mlflow" state={mlflow} />}
                            {mlflow?.deployed && (
                              <Form className="pf-v6-u-mt-md" onSubmit={(e) => e.preventDefault()}>
                                <FormGroup label="Deploy an opendatahub-io/mlflow PR" fieldId="mlflow-pr">
                                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsFlexStart" }}>
                                    <FlexItem>
                                      <InputGroup>
                                        <InputGroupItem>
                                          <TextInput id="mlflow-pr" type="text" inputMode="numeric" value={mlflowPR} onChange={(_e, val) => setMlflowPR(val.trim())} placeholder="PR number" aria-label="opendatahub-io/mlflow PR number" />
                                        </InputGroupItem>
                                        <InputGroupItem>
                                          <TooltipButton variant="control" onClick={() => setPending({ kind: "deploy-mlflow-pr", pr: Number(mlflowPR) })} isLoading={resAction === "deploy-mlflow-pr"}
                                            disabledReason={baseReason ?? (!mlflowPRValid ? "Enter an opendatahub-io/mlflow PR number first." : null)}>Deploy PR</TooltipButton>
                                        </InputGroupItem>
                                      </InputGroup>
                                    </FlexItem>
                                    {mlflow.prOverride && (
                                      <FlexItem>
                                        <TooltipButton variant="secondary" onClick={() => setPending({ kind: "revert-mlflow" })} isLoading={resAction === "revert-mlflow"} disabledReason={baseReason}>
                                          Revert the PR image
                                        </TooltipButton>
                                      </FlexItem>
                                    )}
                                  </Flex>
                                </FormGroup>
                              </Form>
                            )}
                  </CardListItem>
                </CardList>
              </CardBody>
            </Card>
          </StackItem>

          {/* Pipelines */}
          <StackItem>
            <Card>
              <CardHeader actions={{ actions: addPipelineControl, hasNoOffset: true }}>
                <CardTitle><Title headingLevel="h2" size="lg">Pipeline servers</Title></CardTitle>
              </CardHeader>
              <CardBody>
                <Stack hasGutter>
                  {resStatus && !minioReady && (
                    <StackItem>
                      <Alert
                        variant="warning"
                        title={minio?.deployed ? `Pipeline servers need MinIO, which is not ready${minio.waitingReason ? ` (${minio.waitingReason})` : ""}` : "Pipeline servers need MinIO: set up storage first"}
                        isInline
                        component="p"
                      />
                    </StackItem>
                  )}
                  <StackItem>
                    {pipelineServers.length === 0 ? (
                      <EmptyState headingLevel="h3" titleText="No pipeline servers" variant="xs">
                        <EmptyStateBody>Add one to a data science project with the button above.</EmptyStateBody>
                      </EmptyState>
                    ) : (
                      <CardList aria-label="Pipeline servers">
                        {pipelineServers.map((ps) => (
                          <CardListItem key={ps.namespace} labelledBy={`ps-${ps.namespace}`} actions={rowActions(
                                    <>
                                      {ps.ready && ps.uiRoute && <FlexItem><ExternalLink href={ps.uiRoute}>Open pipelines</ExternalLink></FlexItem>}
                                      {ps.managedByTool !== false && !isTerminating(ps) && (
                                        <FlexItem>
                                          <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-pipeline-server", project: ps.namespace!, name: ps.name })}
                                            isLoading={resAction === "teardown-pipeline-server"} disabledReason={baseReason ?? ps.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                                        </FlexItem>
                                      )}
                                    </>,
                          )}>
                                    <RowTitle id={`ps-${ps.namespace}`} name={ps.namespace ?? ""} state={ps} />
                                    {ps.name && <Content component="p" className="pf-v6-u-text-color-subtle">DataSciencePipelinesApplication <code>{ps.name}</code>.{unmanagedNote(ps)}</Content>}
                                    <StatusDetails kind="pipeline" state={ps} minioReady={minioReady} />
                          </CardListItem>
                        ))}
                      </CardList>
                    )}
                  </StackItem>
                  {unmanagedProjects.size > 0 && (
                    <StackItem>
                      <HelperText>
                        <HelperTextItem>
                          Not offered because they already have a pipeline server this tool did not create: {[...unmanagedProjects].join(", ")}.
                        </HelperTextItem>
                      </HelperText>
                    </StackItem>
                  )}
                </Stack>
              </CardBody>
            </Card>
          </StackItem>
        </Stack>
      </PageSection>

      {modal && (
        <ConfirmActionModal
          isOpen
          title={modal.title}
          changes={modal.changes}
          dataLoss={"dataLoss" in modal ? modal.dataLoss : undefined}
          confirmLabel={modal.confirm}
          confirmVariant={"danger" in modal && modal.danger ? "danger" : "primary"}
          isLoading={!!resAction}
          onConfirm={() => pending && execute(pending)}
          onCancel={() => setPending(null)}
        >
          {"extra" in modal ? modal.extra : undefined}
        </ConfirmActionModal>
      )}
    </>
  );
};
