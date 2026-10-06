import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  CodeBlock,
  CodeBlockCode,
  Content,
  DataList,
  DataListCell,
  DataListContent,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  EmptyState,
  EmptyStateBody,
  Grid,
  GridItem,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Skeleton,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import type { LatestNightlyResponse, NightlyBuild, NightlyTag, OperationResponse } from "../types";
import { createDSC, fetchLatestNightly, fetchNightlyTags, getDSCPreview, toApiError, trackFeature } from "../services/api";
import { prerequisitesMet as checkPrereqs, operatorInstalled } from "../utils";
import { ClusterSetupCard, InstalledBuildCard } from "../components/StatusCards";
import { UpdateConfirmModal, UpdatePanel } from "../components/UpdatePanel";
import { ReinstallPanel } from "../components/ReinstallPanel";
import { SetupModal } from "../components/PrerequisitesPanel";
import { ActivityLog } from "../components/ActivityLog";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { TechnicalDetails } from "../components/LongText";
import { StepsPreview } from "../components/StepsPreview";
import { PageHeader } from "../components/PageHeader";
import { FBCContentModal } from "../components/FBCContentModal";
import { OperationProgress, useHasOperationProgress } from "../components/OperationProgress";
import { REVERT_DASHBOARD_EXPLANATION, describeDashboardSession, useOperatorActions } from "../components/OperatorActions";
import { TooltipButton } from "../components/TooltipButton";
import { describeError } from "../errors";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { isSessionExpired } from "../services/api";
import { useClusterStatus, useOperation } from "../state/AppState";
import { useClusterBusyHandler, useDashboardOverride, useMutationBlocker } from "../state/AppInfo";
import { STEP_SETS } from "../operationSteps";

const REFRESH_PLAN = STEP_SETS.refresh;

export const StatusPage: React.FC = () => {
  const { status, loading, error, lastRefreshed, refresh } = useClusterStatus();
  const operation = useOperation();
  const blocker = useMutationBlocker();
  const onResult = useClusterBusyHandler();
  const { override } = useDashboardOverride();
  const showProgress = useHasOperationProgress();
  const { runOperator, followUps } = useOperatorActions();

  const prerequisitesMet = checkPrereqs(status);
  const installed = operatorInstalled(status);

  // --- Newest nightly overall (for the hero when not on nightly, and to prefill the form) ---
  const [latest, setLatest] = useState<LatestNightlyResponse | null>(null);
  const [latestLoading, setLatestLoading] = useState(false);
  useEffect(() => {
    if (!prerequisitesMet) return;
    const controller = new AbortController();
    setLatestLoading(true);
    fetchLatestNightly(controller.signal)
      .then((res) => { if (!controller.signal.aborted) setLatest(res); })
      .catch((e) => { if (!controller.signal.aborted) setLatest({ tag: "", image: "", error: toApiError(e).message }); })
      .finally(() => { if (!controller.signal.aborted) setLatestLoading(false); });
    return () => controller.abort();
  }, [prerequisitesMet]);

  // --- Nightly tags for Reinstall ---
  const [nightlyTags, setNightlyTags] = useState<NightlyTag[]>([]);
  const [tagsLoading, setTagsLoading] = useState(false);
  const loadTags = useCallback(async () => {
    if (!prerequisitesMet || !installed) return;
    setTagsLoading(true);
    try {
      const res = await fetchNightlyTags();
      setNightlyTags(res.tags || []);
    } catch {
      // Optional: Reinstall shows "No builds found".
    } finally {
      setTagsLoading(false);
    }
  }, [prerequisitesMet, installed]);
  useEffect(() => { loadTags(); }, [loadTags]);

  useEffect(() => {
    const nightlyTag = status?.nightly?.installed?.tag;
    document.title = status
      ? `${nightlyTag ? `${nightlyTag} · ` : ""}${status.csv.version || status.csv.name || "not installed"} (${status.csv.phase}) · RHOAI Nightly Updater`
      : "RHOAI Nightly Updater";
  }, [status]);

  // --- Dialogs ---
  const [updateTarget, setUpdateTarget] = useState<NightlyBuild | null>(null);
  const [refreshConfirmOpen, setRefreshConfirmOpen] = useState(false);
  const [previewImage, setPreviewImage] = useState<string | null>(null);
  const [setupOpen, setSetupOpen] = useState(false);
  const [reinstallExpanded, setReinstallExpanded] = useState(false);
  const updateFormRef = useRef<HTMLDivElement>(null);

  const chooseAnotherBuild = () => {
    updateFormRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
    document.getElementById("fbc-image")?.focus({ preventScroll: true });
  };

  // --- Re-deploy (refresh) result ---
  const refreshRun = operation.run?.kind === "refresh" ? operation.run : null;

  // --- DSC creation ---
  const [dscLoading, setDscLoading] = useState(false);
  const [dscResult, setDscResult] = useState<OperationResponse | null>(null);
  const [dscModalOpen, setDscModalOpen] = useState(false);
  const [dscPreviewYAML, setDscPreviewYAML] = useState<string>("");
  const [dscPreviewLoading, setDscPreviewLoading] = useState(false);
  const [dscPreviewError, setDscPreviewError] = useState("");

  const openDscPreview = async () => {
    setDscModalOpen(true);
    setDscPreviewLoading(true);
    setDscPreviewYAML("");
    setDscPreviewError("");
    try {
      setDscPreviewYAML((await getDSCPreview()).yaml);
    } catch (err) {
      setDscPreviewError(toApiError(err).message);
    } finally {
      setDscPreviewLoading(false);
    }
  };

  const handleCreateDSC = async () => {
    trackFeature("create_dsc");
    setDscLoading(true);
    setDscResult(null);
    let res: OperationResponse;
    try {
      res = await createDSC();
    } catch (e) {
      // A failed create is a 422 OperationResponse: keep its errorCode and logs (N7).
      res = errorResult(e, "Could not create the DataScienceCluster");
    }
    setDscResult(res);
    setDscLoading(false);
    onResult(res);
    if (res.success || res.errorCode === "nothing_to_do" || res.errorCode === "conflict") setTimeout(() => { refresh(); }, 2000);
  };

  const confirmUpdate = (options: { revertDashboardDev?: boolean }) => {
    const target = updateTarget;
    setUpdateTarget(null);
    if (target) runOperator({ kind: "update", image: target.image }, options);
  };

  const confirmRefresh = () => {
    setRefreshConfirmOpen(false);
    runOperator({ kind: "refresh", csvName: status?.csv.name }, override?.active ? { revertDashboardDev: true } : {});
  };

  const statusErrorIsSession = !!error && isSessionExpired(error);
  const staleDescription = error && status && !statusErrorIsSession ? describeError(error, "Could not refresh the status") : null;

  return (
    <>
      <PageHeader
        title="Status"
        description="What's installed on this cluster, whether a newer nightly exists, and the actions to change it."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={refresh}
      />

      {/* --- Status could not be loaded at all: one back-end failure state --- */}
      {!status && error && !statusErrorIsSession && (
        <PageErrorState error={error} title="Can't load the cluster status" onRetry={refresh} />
      )}
      {!status && !error && <PageLoading title="Loading the cluster status" />}

      {/* --- A later poll failed, or some checks failed: keep the data and say what may be stale --- */}
      {status && (staleDescription || (status.errors?.length ?? 0) > 0) && (
        <PageSection>
          <Stack hasGutter>
            {staleDescription && (
              <Alert
                variant="warning"
                isInline
                isExpandable
                component="p"
                title={`The latest refresh failed; showing the status from ${lastRefreshed?.toLocaleTimeString() ?? "earlier"}`}
                actionLinks={<AlertActionLink onClick={refresh}>Retry</AlertActionLink>}
              >
                {staleDescription.title}: {staleDescription.body}{staleDescription.hint && <> {staleDescription.hint}</>}
              </Alert>
            )}
            {(status.errors?.length ?? 0) > 0 && (
              <Alert component="p" variant="warning" isExpandable title="Some status checks failed; parts of this page may be incomplete" isInline>
                <List>
                  {status.errors!.map((err, i) => <ListItem key={i} className="pf-v6-u-text-break-word">{err}</ListItem>)}
                </List>
              </Alert>
            )}
          </Stack>
        </PageSection>
      )}

      {/* --- One-time setup comes first while it is incomplete: nothing else works without it --- */}
      {status && !prerequisitesMet && (
        <PageSection>
          <ClusterSetupCard status={status} onStatusRefresh={refresh} onShowInstructions={() => setSetupOpen(true)} />
        </PageSection>
      )}

      {/* --- Installed vs latest, with the primary action --- */}
      {status && (
        <PageSection>
          <InstalledBuildCard
            status={status}
            latest={latest}
            latestLoading={latestLoading}
            blocker={blocker}
            prerequisitesMet={prerequisitesMet}
            onUpdate={setUpdateTarget}
            onChooseBuild={chooseAnotherBuild}
            onPreview={setPreviewImage}
            onRedeploy={() => setRefreshConfirmOpen(true)}
          />
        </PageSection>
      )}

      {showProgress && (
        <PageSection aria-label="Operation progress">
          <OperationProgress />
        </PageSection>
      )}

      {status && status.csv.phase === "Succeeded" && status.dscExists === false && (
        <PageSection>
          <Card>
            <CardHeader><CardTitle><Title headingLevel="h2" size="lg">Create the DataScienceCluster</Title></CardTitle></CardHeader>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The operator is installed, but no DataScienceCluster exists yet. The DSC tells the operator which
                    components to deploy (dashboard, pipelines, model serving, ...).
                  </Content>
                </StackItem>
                <StackItem>
                  <TooltipButton variant="primary" onClick={openDscPreview} isLoading={dscLoading} isDisabled={dscLoading} disabledReason={blocker}>
                    Create DataScienceCluster...
                  </TooltipButton>
                </StackItem>
                {dscResult && (
                  <StackItem>
                    <Alert
                      variant={outcomeVariant(dscResult)}
                      title={outcomeTitle(dscResult, "Could not create the DataScienceCluster")}
                      isInline
                      isLiveRegion
                      component="p"
                      actionClose={<AlertActionCloseButton onClose={() => setDscResult(null)} />}
                    >
                      {dscResult.success ? undefined : dscResult.message}
                      {(dscResult.logs?.length ?? 0) > 0 && <TechnicalDetails text={dscResult.logs!} />}
                    </Alert>
                  </StackItem>
                )}
              </Stack>
            </CardBody>
          </Card>
          <Modal aria-labelledby="dsc-preview-title" variant={ModalVariant.large} isOpen={dscModalOpen} onClose={() => setDscModalOpen(false)}>
            <ModalHeader title="Create this DataScienceCluster?" labelId="dsc-preview-title" />
            <ModalBody>
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    Creates <code>default-dsc</code> with the defaults of the installed operator. You can change components
                    later on the Components page.
                  </Content>
                </StackItem>
                <StackItem>
                  {dscPreviewLoading ? (
                    <Skeleton height="12rem" screenreaderText="Loading the defaults" />
                  ) : dscPreviewError ? (
                    <Alert variant="danger" title="Could not load the DSC defaults" isInline component="p">{dscPreviewError}</Alert>
                  ) : (
                    <CodeBlock><CodeBlockCode>{dscPreviewYAML}</CodeBlockCode></CodeBlock>
                  )}
                </StackItem>
              </Stack>
            </ModalBody>
            <ModalFooter>
              <TooltipButton
                variant="primary"
                onClick={() => { setDscModalOpen(false); handleCreateDSC(); }}
                isDisabled={dscLoading || dscPreviewLoading || !dscPreviewYAML || !!dscPreviewError}
                disabledReason={blocker}
              >
                Create
              </TooltipButton>
              <Button variant="link" onClick={() => setDscModalOpen(false)}>Cancel</Button>
            </ModalFooter>
          </Modal>
        </PageSection>
      )}

      {status && (
        <PageSection>
          <Grid hasGutter>
            <GridItem lg={6} md={12}>
              <Card isFullHeight>
                <CardHeader>
                  <CardTitle><Title headingLevel="h2" size="lg">Update to a specific build</Title></CardTitle>
                </CardHeader>
                <CardBody>
                  {prerequisitesMet ? (
                    <div ref={updateFormRef}>
                      <UpdatePanel status={status} latest={latest} onRequestUpdate={setUpdateTarget} />
                    </div>
                  ) : (
                    <EmptyState headingLevel="h3" titleText="Finish the cluster setup first" variant="xs">
                      <EmptyStateBody>Nightly builds can be installed once the pull secret and the image mirror are ready.</EmptyStateBody>
                    </EmptyState>
                  )}
                </CardBody>
              </Card>
            </GridItem>
            <GridItem lg={6} md={12}>
              <ActivityLog activity={status.activity} />
            </GridItem>
          </Grid>
        </PageSection>
      )}

      {status && installed && (
        <PageSection>
          <Card>
            <CardHeader>
              <CardTitle>
                <Title headingLevel="h2" size="lg">Recovery</Title>
              </CardTitle>
            </CardHeader>
            <CardBody>
              <DataList aria-label="Recovery actions">
                <DataListItem aria-labelledby="recovery-redeploy">
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="text">
                          <Title headingLevel="h3" size="md" id="recovery-redeploy">Re-deploy the operator</Title>
                          <Content component="p" className="pf-v6-u-text-color-subtle">
                            Deletes and recreates the Subscription and CSV from the same catalog and channel. Keeps the Subscription
                            settings, DSC, DSCI and workloads. It does not fetch a newer build: use Update for that.
                          </Content>
                        </DataListCell>,
                        <DataListCell key="action" isFilled={false} alignRight>
                        <TooltipButton
                          variant="secondary"
                          onClick={() => setRefreshConfirmOpen(true)}
                          isLoading={!!refreshRun && !refreshRun.outcome}
                          disabledReason={blocker}
                        >
                          Re-deploy operator...
                        </TooltipButton>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                </DataListItem>
                <DataListItem aria-labelledby="recovery-reinstall" isExpanded={reinstallExpanded}>
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="text">
                          <Title headingLevel="h3" size="md" id="recovery-reinstall">Reinstall the operator</Title>
                          <Content component="p" className="pf-v6-u-text-color-subtle">
                            Uninstalls the operator and installs it again from the target you choose: the GA release, an older
                            build, or when Update and Re-deploy can&apos;t recover it. RHOAI can&apos;t be changed for 5-10 minutes;
                            running workloads keep running.
                          </Content>
                        </DataListCell>,
                        <DataListCell key="action" isFilled={false} alignRight>
                        <Button
                          variant="secondary"
                          onClick={() => setReinstallExpanded(!reinstallExpanded)}
                          aria-expanded={reinstallExpanded}
                          aria-controls="recovery-reinstall-content"
                        >
                          {reinstallExpanded ? "Hide reinstall options" : "Reinstall..."}
                        </Button>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                  <DataListContent id="recovery-reinstall-content" aria-label="Reinstall options" isHidden={!reinstallExpanded}>
                    <ReinstallPanel
                      status={status}
                      nightlyTags={nightlyTags}
                      tagsLoading={tagsLoading}
                      prerequisitesMet={prerequisitesMet}
                      runOperator={runOperator}
                    />
                  </DataListContent>
                </DataListItem>
              </DataList>
            </CardBody>
          </Card>
        </PageSection>
      )}

      {status && prerequisitesMet && (
        <PageSection isFilled>
          <ClusterSetupCard status={status} onStatusRefresh={refresh} onShowInstructions={() => setSetupOpen(true)} collapsible />
        </PageSection>
      )}

      {status && <SetupModal isOpen={setupOpen} onClose={() => setSetupOpen(false)} status={status} />}

      <UpdateConfirmModal target={updateTarget} status={status} onConfirm={confirmUpdate} onClose={() => setUpdateTarget(null)} />

      {previewImage && <FBCContentModal image={previewImage} isOpen onClose={() => setPreviewImage(null)} />}

      <Modal
        aria-labelledby="confirm-refresh-title"
        variant={ModalVariant.medium}
        isOpen={refreshConfirmOpen}
        onClose={() => setRefreshConfirmOpen(false)}
      >
        <ModalHeader title={`Re-deploy ${status?.csv.name || "the operator"}?`} labelId="confirm-refresh-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                Installs the same version again from {status?.subscription.source || "the current catalog"} /{" "}
                {status?.subscription.channel || "the current channel"}. The images are pinned by digest, so this
                never picks up a newer build; use Update for that.
              </Content>
            </StackItem>
            {override?.active && (
              <StackItem>
                <Alert variant="warning" isInline component="p" title="A Dashboard Dev session is active">
                  dashboard-operator is paused ({describeDashboardSession(override)}). The re-deploy ends that session first.
                  {" "}{REVERT_DASHBOARD_EXPLANATION}
                </Alert>
              </StackItem>
            )}
            <StackItem>
              <Content component="p">
                The Subscription keeps its config and approval mode; DSC, DSCI and workloads are untouched. The operator is
                unavailable for a few minutes while OLM installs it again. If that fails, the previous state is restored.
              </Content>
            </StackItem>
            <StackItem>
              <StepsPreview steps={REFRESH_PLAN} idPrefix="refresh-step" />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <TooltipButton variant="primary" onClick={confirmRefresh} disabledReason={blocker}>
            {override?.active ? "Revert Dashboard Dev and re-deploy" : "Re-deploy"}
          </TooltipButton>
          <Button variant="link" onClick={() => setRefreshConfirmOpen(false)}>Cancel</Button>
        </ModalFooter>
      </Modal>

      {followUps}
    </>
  );
};
