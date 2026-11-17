import React, { useCallback, useEffect, useState } from "react";
import { Alert, AlertActionCloseButton, AlertActionLink, PageSection, Stack, StackItem, Title } from "@patternfly/react-core";
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
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { type Pending, isTerminating, terminalReason } from "./environment/resourceRow";
import { StorageCard, storageConfirm } from "./environment/StorageCard";
import { PipelineServersCard, pipelineServerConfirm } from "./environment/PipelineServersCard";
import { MLflowCard, mlflowConfirm } from "./environment/MLflowCard";

export { nextStep, terminalReason } from "./environment/resourceRow";

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

/**
 * The Environment page's one-click setups: it loads and polls their status,
 * runs the confirmed actions and shows their result. Each card renders one
 * setup (environment/*Card.tsx).
 */
export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ mutateBlocker, onResult, renderHeader }) => {
  const [resStatus, setResStatus] = useState<ResourcesStatus | null>(null);
  const [resAction, setResAction] = useState<string | null>(null);
  const [resResult, setResResult] = useState<OperationResponse | null>(null);
  // errorCode of this tab's last MinIO teardown that did not succeed.
  const [lastMinIOTeardown, setLastMinIOTeardown] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [projectList, setProjectList] = useState<string[]>([]);

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
      case "migrate-minio": return handleResourceAction("setup-minio", setupMinIO);
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
  // After a teardown the namespace stays (deployed:false, managedByTool:true).
  // Offer Tear down again while tool objects are still reported (the data
  // PVC, a Route) or the last teardown from this tab did not finish.
  const minioLeftovers = !!minio && !minio.deployed && minio.managedByTool === true && (
    (minio.dataPVCs ?? []).length > 0 || !!minio.uiRoute || !!minio.apiRoute ||
    (lastMinIOTeardown !== null && ["partial_failure", "delete_failed", "in_progress", "forbidden", "unauthorized", "network"].includes(lastMinIOTeardown))
  );
  const actionProps = { baseReason, resAction, setPending };

  const modal = pending
    ? storageConfirm(pending, minio) ?? mlflowConfirm(pending, mlflow) ?? pipelineServerConfirm(pending, pipelineServers)
    : null;

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

          <StackItem>
            <section aria-labelledby="environment-data-pipelines">
              <Stack hasGutter>
                <StackItem>
                  <Title headingLevel="h2" size="xl" id="environment-data-pipelines">Data &amp; pipelines</Title>
                </StackItem>
                <StackItem>
                  <StorageCard minio={minio} minioLeftovers={minioLeftovers} {...actionProps} />
                </StackItem>
                <StackItem>
                  <PipelineServersCard
                    statusLoaded={!!resStatus}
                    minio={minio}
                    pipelineServers={pipelineServers}
                    availableProjects={availableProjects}
                    unmanagedProjects={unmanagedProjects}
                    {...actionProps}
                  />
                </StackItem>
                <StackItem>
                  <MLflowCard mlflow={mlflow} {...actionProps} />
                </StackItem>
              </Stack>
            </section>
          </StackItem>
        </Stack>
      </PageSection>

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
    </>
  );
};
