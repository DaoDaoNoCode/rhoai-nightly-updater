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
  Divider,
  ExpandableSection,
  Grid,
  GridItem,
  Label,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import { useNavigate } from "react-router-dom";
import type { LatestNightlyResponse, NightlyBuild, NightlyTag, OperationResponse } from "../types";
import { createDSC, fetchLatestNightly, fetchNightlyTags, getDSCPreview, toApiError, trackFeature } from "../services/api";
import { prerequisitesMet as checkPrereqs, operatorInstalled } from "../utils";
import { InstalledBuildCard, SetupCards, StatusLoading } from "../components/StatusCards";
import { PullSecretCard } from "../components/PullSecretCard";
import { UpdateConfirmModal, UpdatePanel } from "../components/UpdatePanel";
import { ReinstallPanel } from "../components/ReinstallPanel";
import { SetupModal } from "../components/PrerequisitesPanel";
import { ActivityLog } from "../components/ActivityLog";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { FBCContentModal } from "../components/FBCContentModal";
import { OperationProgress, useHasOperationProgress } from "../components/OperationProgress";
import { REVERT_DASHBOARD_EXPLANATION, describeDashboardSession, useOperatorActions } from "../components/OperatorActions";
import { TooltipButton } from "../components/TooltipButton";
import { describeError } from "../errors";
import { isSessionExpired } from "../services/api";
import { useClusterStatus, useOperation } from "../state/AppState";
import { useDashboardOverride, useMutationBlocker, usePermissions } from "../state/AppInfo";
import { STEP_SETS } from "../operationSteps";

const REFRESH_PLAN = STEP_SETS.refresh;

export const StatusPage: React.FC = () => {
  const { status, loading, error, lastRefreshed, refresh } = useClusterStatus();
  const operation = useOperation();
  const blocker = useMutationBlocker();
  const permissions = usePermissions();
  const { override } = useDashboardOverride();
  const showProgress = useHasOperationProgress();
  const { runOperator, followUps } = useOperatorActions();
  const navigate = useNavigate();

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
  const [setupExpanded, setSetupExpanded] = useState(false);
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
    try {
      const res = await createDSC();
      setDscResult(res);
      if (res.success) setTimeout(() => { refresh(); }, 2000);
    } catch (e) {
      const { title, body } = describeError(e, "Could not create the DataScienceCluster");
      setDscResult({ success: false, message: `${title}: ${body}`, logs: [] });
    } finally {
      setDscLoading(false);
    }
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

  const pullSecretCard = status ? (
    <PullSecretCard
      pullSecret={status.pullSecret}
      onStatusRefresh={refresh}
      disabledReason={permissions.reason ?? (operation.server.inProgress ? blocker : null)}
    />
  ) : null;

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

      {/* --- Status could not be loaded at all: one message, the cause, the next step --- */}
      {!status && error && !statusErrorIsSession && (
        <PageSection>
          <ErrorAlert
            inline
            error={error}
            genericTitle="Can't load the cluster status"
            onRetry={refresh}
            extraActions={<AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>}
          />
        </PageSection>
      )}

      {/* --- A later poll failed: keep the last good data and say it may be stale --- */}
      {staleDescription && (
        <PageSection padding={{ default: "noPadding" }} style={{ padding: "var(--pf-t--global--spacer--md) var(--pf-t--global--spacer--lg) 0" }}>
          <Alert
            variant="warning"
            isInline
            component="p"
            title={`Showing the status from ${lastRefreshed?.toLocaleTimeString() ?? "earlier"}: the latest refresh failed`}
            actionLinks={<AlertActionLink onClick={refresh}>Retry</AlertActionLink>}
          >
            {staleDescription.title}: {staleDescription.body}{staleDescription.hint && <> {staleDescription.hint}</>}
          </Alert>
        </PageSection>
      )}

      {status?.errors && status.errors.length > 0 && (
        <PageSection padding={{ default: "noPadding" }} style={{ padding: "var(--pf-t--global--spacer--md) var(--pf-t--global--spacer--lg) 0" }}>
          <Alert component="p" variant="warning" title="Some status checks failed; parts of this page may be incomplete" isInline>
            <List>
              {status.errors.map((err, i) => <ListItem key={i}>{err}</ListItem>)}
            </List>
          </Alert>
        </PageSection>
      )}

      {/* --- One-time setup comes first while it is incomplete: nothing else works without it --- */}
      {status && !prerequisitesMet && (
        <PageSection aria-labelledby="setup-title">
          <Stack hasGutter>
            <StackItem>
              <Title headingLevel="h2" size="lg" id="setup-title">One-time cluster setup</Title>
              <Content component="p" className="rhoai-subtle">Nightly builds come from quay.io/rhoai. The cluster needs these two before the first install.</Content>
            </StackItem>
            <StackItem>
              <Grid hasGutter>
                <SetupCards status={status} pullSecretCard={pullSecretCard} onShowInstructions={() => setSetupOpen(true)} />
              </Grid>
            </StackItem>
          </Stack>
        </PageSection>
      )}

      {/* --- Installed vs latest, with the primary action --- */}
      <PageSection>
        {status ? (
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
        ) : !error ? (
          <StatusLoading />
        ) : null}
      </PageSection>

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
                      variant={dscResult.success ? "success" : "danger"}
                      title={dscResult.message}
                      isInline
                      isLiveRegion
                      component="p"
                      actionClose={dscResult.success ? <AlertActionCloseButton onClose={() => setDscResult(null)} /> : undefined}
                    />
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
                    <Content component="p">Loading the defaults...</Content>
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
            <GridItem lg={7} md={12}>
              <Card isFullHeight>
                <CardHeader>
                  <CardTitle><Title headingLevel="h2" size="lg">Update to a specific build</Title></CardTitle>
                </CardHeader>
                <CardBody>
                  <div ref={updateFormRef} style={{ scrollMarginTop: "var(--pf-t--global--spacer--lg)" }}>
                    <UpdatePanel status={status} latest={latest} onRequestUpdate={setUpdateTarget} />
                  </div>
                </CardBody>
              </Card>
            </GridItem>
            <GridItem lg={5} md={12}>
              <Card isFullHeight>
                <CardHeader>
                  <CardTitle><Title headingLevel="h2" size="lg">Recent activity</Title></CardTitle>
                </CardHeader>
                <CardBody>
                  <ActivityLog activity={status.activity} />
                </CardBody>
              </Card>
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
              <Stack hasGutter>
                <StackItem>
                  <Title headingLevel="h3" size="md">Re-deploy the operator</Title>
                  <Content component="p" className="rhoai-subtle">
                    Deletes and recreates the Subscription and CSV from the same catalog and channel. Keeps the Subscription
                    settings, DSC, DSCI and workloads. It does not fetch a newer build: use Update for that.
                  </Content>
                </StackItem>
                <StackItem>
                  <TooltipButton
                    variant="secondary"
                    icon={<SyncAltIcon />}
                    onClick={() => setRefreshConfirmOpen(true)}
                    isLoading={!!refreshRun && !refreshRun.outcome}
                    disabledReason={blocker}
                  >
                    Re-deploy operator...
                  </TooltipButton>
                </StackItem>
                <StackItem><Divider /></StackItem>
                <StackItem>
                  <ExpandableSection
                    toggleContent={<Title headingLevel="h3" size="md">Reinstall the operator</Title>}
                    isExpanded={reinstallExpanded}
                    onToggle={(_e, expanded) => setReinstallExpanded(expanded)}
                  >
                    <ReinstallPanel
                      status={status}
                      nightlyTags={nightlyTags}
                      tagsLoading={tagsLoading}
                      prerequisitesMet={prerequisitesMet}
                      runOperator={runOperator}
                    />
                  </ExpandableSection>
                </StackItem>
              </Stack>
            </CardBody>
          </Card>
        </PageSection>
      )}

      {status && prerequisitesMet && (
        <PageSection>
          <ExpandableSection
            toggleContent={
              <span>
                Cluster setup <Label isCompact color="green" variant="outline">Pull secret ready</Label>{" "}
                <Label isCompact color="green" variant="outline">Image mirror ready</Label>
              </span>
            }
            isExpanded={setupExpanded}
            onToggle={(_e, expanded) => setSetupExpanded(expanded)}
          >
            <Grid hasGutter>
              <SetupCards status={status} pullSecretCard={pullSecretCard} onShowInstructions={() => setSetupOpen(true)} />
            </Grid>
          </ExpandableSection>
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
              <Content component="p"><strong>What happens</strong></Content>
              <List component="ol">
                {REFRESH_PLAN.map((step) => (
                  <ListItem key={step.id}>{step.label}<span className="rhoai-subtle">: {step.description}</span></ListItem>
                ))}
              </List>
            </StackItem>
            <StackItem>
              <Content component="p">
                The Subscription keeps its config and approval mode; DSC, DSCI and workloads are untouched. The operator is
                unavailable for a few minutes while OLM installs it again. If that fails, the previous state is restored.
              </Content>
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
