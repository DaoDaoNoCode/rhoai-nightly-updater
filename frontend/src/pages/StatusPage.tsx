import React, { useCallback, useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  CodeBlock,
  CodeBlockCode,
  Content,
  Divider,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  Flex,
  FlexItem,
  Grid,
  GridItem,
  Label,
  Icon,
  PageSection,
  Card,
  CardHeader,
  CardTitle,
  CardBody,
  Stack,
  StackItem,
  Title,
  Tooltip,
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import CloneIcon from "@patternfly/react-icons/dist/esm/icons/clone-icon";
import type { NightlyTag, OperationResponse, StatusResponse, UpdateStep } from "../types";
import { fetchNightlyTags, streamRefresh, trackFeature, createDSC, getDSCPreview } from "../services/api";
import { prerequisitesMet as checkPrereqs, operatorInstalled } from "../utils";
import { StatusCards } from "../components/StatusCards";
import { PullSecretCard } from "../components/PullSecretCard";
import { UpdatePanel } from "../components/UpdatePanel";
import { type OperationType as PipelineOperationType } from "../components/UpdatePipeline";
import { ReinstallPanel } from "../components/ReinstallPanel";
import { SetupModal } from "../components/PrerequisitesPanel";
import { ActivityLog } from "../components/ActivityLog";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { OperationProgress } from "../components/OperationProgress";
import type { OperationType } from "../components/ReconciliationProgress";
import type { OperationPhase } from "../App";

interface StatusPageProps {
  status: StatusResponse | null;
  loading: boolean;
  error: string | null;
  reconciling: boolean;
  reconcileStartTime: number;
  reconcileTimedOut?: boolean;
  lastRefreshed: Date | null;
  setupOpen: boolean;
  setSetupOpen: (open: boolean) => void;
  refresh: () => void;
  handleMutationComplete: () => void;
  canMutate: boolean;
  operationPhase: OperationPhase;
  onStreamStart: () => void;
  onStreamEnd: (success: boolean) => void;
  onReconcileComplete: () => void;
}

const ONBOARDING_DISMISSED_KEY = "rhoai-onboarding-dismissed";

export const StatusPage: React.FC<StatusPageProps> = ({
  status,
  loading,
  error,
  reconciling,
  reconcileStartTime,
  reconcileTimedOut = false,
  lastRefreshed,
  setupOpen,
  setSetupOpen,
  refresh,
  handleMutationComplete,
  canMutate,
  operationPhase,
  onStreamStart,
  onStreamEnd,
  onReconcileComplete,
}) => {
  // Pipeline step state for UpdatePipeline visualization
  const [pipelineSteps, setPipelineSteps] = useState<UpdateStep[]>([]);
  const [pipelineActive, setPipelineActive] = useState(false);
  const [pipelineOpType, setPipelineOpType] = useState<PipelineOperationType>("update");

  const handlePipelineStreamStart = useCallback((opType?: PipelineOperationType) => {
    setPipelineActive(true);
    setPipelineSteps([]);
    if (opType) setPipelineOpType(opType);
    onStreamStart();
  }, [onStreamStart]);

  const handlePipelineStreamStep = useCallback((step: UpdateStep) => {
    // flushSync forces React to render each step immediately instead of
    // batching, so the user sees real-time progress during SSE streaming.
    flushSync(() => {
      setPipelineSteps((prev) => {
        const idx = prev.findIndex((s) => s.step === step.step);
        if (idx >= 0) {
          const next = [...prev];
          next[idx] = step;
          return next;
        }
        return [...prev, step];
      });
    });
  }, []);

  const handlePipelineStreamEnd = useCallback(
    (success: boolean) => {
      setPipelineActive(false);
      onStreamEnd(success);
    },
    [onStreamEnd],
  );

  const [onboardingDismissed, setOnboardingDismissed] = useState(() => {
    try {
      return localStorage.getItem(ONBOARDING_DISMISSED_KEY) === "true";
    } catch {
      return false;
    }
  });

  const isOnNightly = !!(status?.subscription.source && status.subscription.source !== status.stableSource);
  const prerequisitesMet = checkPrereqs(status);

  // Shared nightly tags — fetched once, used by both UpdatePanel and ReinstallPanel
  const [nightlyTags, setNightlyTags] = useState<NightlyTag[]>([]);
  const [tagsLoading, setTagsLoading] = useState(false);

  const loadTags = useCallback(async () => {
    if (!prerequisitesMet) return;
    setTagsLoading(true);
    try {
      const res = await fetchNightlyTags();
      setNightlyTags(res.tags || []);
    } catch {
      // Tags are optional — don't block the page
    } finally {
      setTagsLoading(false);
    }
  }, [prerequisitesMet]);

  useEffect(() => {
    loadTags();
  }, [loadTags]);

  // Item 5: Dynamic page title for Dashboard
  useEffect(() => {
    if (status) {
      document.title = `RHOAI Nightly Updater — ${status.csv.version || status.csv.name || "unknown"} (${status.csv.phase})`;
    } else {
      document.title = "RHOAI Nightly Updater";
    }
  }, [status]);

  // Refresh operator state
  const [lastOperationType, setLastOperationType] = useState<OperationType>("update");
  const [refreshConfirmOpen, setRefreshConfirmOpen] = useState(false);
  const [refreshLoading, setRefreshLoading] = useState(false);
  const [refreshResult, setRefreshResult] = useState<OperationResponse | null>(null);
  const [refreshError, setRefreshError] = useState<string | null>(null);

  // Abort controller for streaming operations
  const abortRef = useRef<AbortController | null>(null);

  // Abort in-flight requests on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  // Track when reconciliation ends for the 30s grace period
  const prevReconcilingRef = useRef(reconciling);
  const [showRecentComplete, setShowRecentComplete] = useState(false);

  useEffect(() => {
    // Detect transition from reconciling=true to reconciling=false — keep visible until dismissed
    if (prevReconcilingRef.current && !reconciling) {
      setShowRecentComplete(true);
    }
    prevReconcilingRef.current = reconciling;
  }, [reconciling]);

  // DSC creation state
  const [dscLoading, setDscLoading] = useState(false);
  const [dscResult, setDscResult] = useState<OperationResponse | null>(null);
  const [dscModalOpen, setDscModalOpen] = useState(false);
  const [dscPreviewYAML, setDscPreviewYAML] = useState<string>("");
  const [dscPreviewLoading, setDscPreviewLoading] = useState(false);

  const handleRefreshOperator = () => {
    trackFeature("refresh_operator");
    setRefreshError(null);
    setRefreshResult(null);
    setRefreshConfirmOpen(false);

    // Start streaming
    setRefreshLoading(true);
    handlePipelineStreamStart("refresh");

    abortRef.current?.abort();
    abortRef.current = streamRefresh(
      (step) => {
        handlePipelineStreamStep(step);
      },
      (success, errorMsg) => {
        setRefreshLoading(false);
        handlePipelineStreamEnd(success);
        if (success) {
          setRefreshResult({
            success: true,
            message: "Operator refresh initiated. OLM will reinstall with updated images.",
            logs: [],
          });
          setLastOperationType("refresh");
          handleMutationComplete();
        } else if (errorMsg) {
          setRefreshError(errorMsg);
        }
      },
      () => {
        // SSE connection dropped -- the backend is still running.
        setRefreshLoading(false);
        handlePipelineStreamEnd(false);
        handleMutationComplete();
      },
    );
  };

  const handleCreateDSC = async () => {
    trackFeature("create_dsc");
    setDscLoading(true);
    setDscResult(null);
    try {
      const res = await createDSC();
      setDscResult(res);
      if (res.success) {
        // Refresh status to update dscExists flag
        setTimeout(() => {
          refresh();
        }, 2000);
      }
    } catch (e) {
      setDscResult({
        success: false,
        message: e instanceof Error ? e.message : "Failed to create DSC",
        logs: [],
      });
    } finally {
      setDscLoading(false);
    }
  };

  const bothMissing =
    status && !status.pullSecret.exists && !status.imageMirror.exists;
  const showOnboarding = bothMissing && !onboardingDismissed;

  const handleDismissOnboarding = () => {
    setOnboardingDismissed(true);
    try {
      localStorage.setItem(ONBOARDING_DISMISSED_KEY, "true");
    } catch {
      // localStorage may be disabled
    }
  };

  return (
    <>
      {/* --- Header row: Cluster Status + Refresh --- */}
      <PageHeader
        title="Cluster Status"
        lastRefreshed={lastRefreshed}
        loading={loading || pipelineActive}
        onRefresh={refresh}
      />

      {/* --- HTTP error alerts --- */}
      {error && <ErrorAlert error={error} />}

      {/* --- Read-only access banner --- */}
      {!canMutate && (
        <PageSection>
          <Alert variant="info" title="Read-only access" isInline isPlain>
            You have read-only access. Mutation operations (update, refresh, reinstall) are disabled. Contact a cluster admin for write access.
          </Alert>
        </PageSection>
      )}

      {/* --- One-time cluster setup (combined onboarding + prerequisites) --- */}
      {status && !prerequisitesMet && (
        <PageSection>
          <Stack hasGutter>
            <StackItem>
              <Title headingLevel="h3">One-Time Cluster Setup</Title>
              <Content component="small">Complete these two steps before installing nightly builds.</Content>
            </StackItem>
            <StackItem>
              <Grid hasGutter>
                <PullSecretCard
                  pullSecret={status.pullSecret}
                  canMutate={canMutate}
                  onStatusRefresh={refresh}
                />
                <GridItem lg={6} md={6} sm={12}>
                  <Card isFullHeight isCompact>
                    <CardHeader>
                      <CardTitle>
                        <Flex alignItems={{ default: "alignItemsCenter" }} justifyContent={{ default: "justifyContentSpaceBetween" }} flexWrap={{ default: "nowrap" }}>
                          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                            <FlexItem><Icon><CloneIcon /></Icon></FlexItem>
                            <FlexItem>Image Mirror (IDMS)</FlexItem>
                          </Flex>
                          <FlexItem>
                            {status.imageMirror.exists
                              ? <Label color="green" variant="outline" isCompact>Ready</Label>
                              : <Label status="danger" variant="outline" isCompact>Missing</Label>}
                          </FlexItem>
                        </Flex>
                      </CardTitle>
                    </CardHeader>
                    <CardBody>
                      <Stack hasGutter>
                        <StackItem>
                          <Content component="small">
                            Redirects registry.redhat.io/rhoai image pulls to quay.io/rhoai where nightly images are hosted.
                          </Content>
                        </StackItem>
                        <StackItem>
                          <Button variant="link" isInline onClick={() => setSetupOpen(true)}>
                            View setup instructions
                          </Button>
                        </StackItem>
                      </Stack>
                    </CardBody>
                  </Card>
                </GridItem>
              </Grid>
            </StackItem>
          </Stack>
        </PageSection>
      )}

      {/* --- Status cards (operator + catalog) --- */}
      <PageSection>
        <Title headingLevel="h3" style={{ marginBottom: "0.75rem" }}>Operator Status</Title>
        <StatusCards
          status={status}
          loading={loading}
          error={error}
          reconciling={reconciling}
          onStatusRefresh={refresh}
          canMutate={canMutate}
        />
      </PageSection>

      {/* --- Activity Log --- */}
      {status && status.activity && status.activity.length > 0 && (
        <PageSection>
          <Title headingLevel="h3" style={{ marginBottom: "0.5rem" }}>Recent Activity</Title>
          <ActivityLog activity={status.activity} />
        </PageSection>
      )}

      {/* --- DSC Prompt Card (only show when operator is healthy but DSC doesn't exist) --- */}
      {status && status.csv.phase === "Succeeded" && status.dscExists === false && (
        <PageSection>
          <Card isLarge>
            <CardTitle>
              <Title headingLevel="h3">DataScienceCluster Required</Title>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The RHOAI operator is installed, but no DataScienceCluster has been created.
                    The DSC tells the operator which components to deploy (dashboard, pipelines,
                    model serving, etc.)
                  </Content>
                </StackItem>
                <StackItem>
                  <Tooltip
                    content="You don't have permission to create a DSC. Contact a cluster admin."
                    trigger={canMutate ? "manual" : "mouseenter focus"}
                  >
                    <Button
                      variant="primary"
                      onClick={async () => {
                        setDscModalOpen(true);
                        setDscPreviewLoading(true);
                        try {
                          const res = await getDSCPreview();
                          setDscPreviewYAML(res.yaml);
                        } catch {
                          setDscPreviewYAML("(Failed to load preview from upstream — will use built-in defaults)");
                        } finally {
                          setDscPreviewLoading(false);
                        }
                      }}
                      isLoading={dscLoading}
                      isDisabled={dscLoading || !canMutate}
                    >
                      Create DataScienceCluster
                    </Button>
                  </Tooltip>
                </StackItem>
                {dscResult && (
                  <StackItem>
                    <Alert
                      variant={dscResult.success ? "success" : "danger"}
                      title={dscResult.message}
                      isInline
                      actionClose={dscResult.success ? <AlertActionCloseButton onClose={() => setDscResult(null)} /> : undefined}
                    />
                  </StackItem>
                )}
              </Stack>
            </CardBody>
          </Card>

          {/* DSC Preview Modal */}
          <Modal
            aria-labelledby="dsc-preview-title"
            variant={ModalVariant.large}
            isOpen={dscModalOpen}
            onClose={() => setDscModalOpen(false)}
          >
            <ModalHeader title="Create DataScienceCluster" labelId="dsc-preview-title" />
            <ModalBody>
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    This will create a <code>default-dsc</code> DataScienceCluster with the following configuration.
                    You can change component states later from the Components page.
                  </Content>
                </StackItem>
                <StackItem>
                  {dscPreviewLoading ? (
                    <Content component="p">Loading preview from upstream...</Content>
                  ) : (
                    <CodeBlock>
                      <CodeBlockCode>{dscPreviewYAML}</CodeBlockCode>
                    </CodeBlock>
                  )}
                </StackItem>
              </Stack>
            </ModalBody>
            <ModalFooter>
              <Button
                variant="primary"
                onClick={() => { setDscModalOpen(false); handleCreateDSC(); }}
                isDisabled={dscLoading}
                isLoading={dscLoading}
              >
                Confirm &amp; Create
              </Button>
              <Button variant="link" onClick={() => setDscModalOpen(false)}>
                Cancel
              </Button>
            </ModalFooter>
          </Modal>
        </PageSection>
      )}

      {/* --- Unified Operation Progress (pipeline + reconciliation in one card) --- */}
      {(pipelineActive || pipelineSteps.length > 0 || reconciling || showRecentComplete) && (
        <PageSection>
          <OperationProgress
            pipelineSteps={pipelineSteps}
            pipelineActive={pipelineActive}
            pipelineOpType={pipelineOpType}
            onPipelineComplete={() => {
              handlePipelineStreamEnd(true);
              handleMutationComplete();
            }}
            onPipelineFailed={() => {
              handlePipelineStreamEnd(false);
            }}
            status={status}
            reconciling={reconciling}
            reconcileStartTime={reconcileStartTime}
            reconcileTimedOut={reconcileTimedOut}
            reconcileOpType={lastOperationType}
            showRecentComplete={showRecentComplete}
          />
        </PageSection>
      )}

      {/* --- Action Card 1: Install / Upgrade to Nightly Build --- */}
      <PageSection>
        <Card isLarge>
          <CardTitle>
            <Title headingLevel="h3">{status?.csv.phase && status.csv.phase !== "Not Found" ? "Upgrade to Nightly Build" : "Install Nightly Build"}</Title>
          </CardTitle>
          <CardBody>
            <UpdatePanel
              status={status}
              onComplete={() => { setLastOperationType("update"); handleMutationComplete(); }}
              canMutate={canMutate}
              onStreamStart={() => handlePipelineStreamStart("update")}
              onStreamStep={handlePipelineStreamStep}
              onStreamEnd={handlePipelineStreamEnd}
            />

            {/* --- Refresh Operator (same version, updated images) --- */}
            {isOnNightly && (
              <>
                <Divider style={{ marginTop: "1.5rem", marginBottom: "1rem" }} />
                <Stack hasGutter>
                  <StackItem>
                    <Content component="small">
                      Or refresh the operator with updated images from the current catalog:
                    </Content>
                  </StackItem>
                  <StackItem>
                    <Tooltip
                      content="You don't have permission to modify the operator. Contact a cluster admin."
                      trigger={canMutate ? "manual" : "mouseenter focus"}
                    >
                      <Button
                        variant="secondary"
                        icon={<SyncAltIcon />}
                        onClick={() => setRefreshConfirmOpen(true)}
                        isLoading={refreshLoading}
                        isDisabled={refreshLoading || !canMutate || !prerequisitesMet || !operatorInstalled(status)}
                        size="sm"
                      >
                        Refresh operator
                      </Button>
                    </Tooltip>
                  </StackItem>
                  {refreshError && (
                    <StackItem>
                      <Alert variant="danger" title="Refresh failed" isInline>
                        {refreshError}
                      </Alert>
                    </StackItem>
                  )}
                  {refreshResult && (
                    <StackItem>
                      <Stack hasGutter>
                        <StackItem>
                          <Alert
                            variant={
                              !refreshResult.success ? "danger"
                              : refreshResult.message.includes("Nothing to refresh") || refreshResult.message.includes("No RHOAI operator CSV") ? "info"
                              : "success"
                            }
                            title={refreshResult.message}
                            isInline
                          />
                        </StackItem>
                        {refreshResult.logs.length > 0 && (
                          <StackItem>
                            <CodeBlock>
                              <CodeBlockCode>
                                {refreshResult.logs.join("\n")}
                              </CodeBlockCode>
                            </CodeBlock>
                          </StackItem>
                        )}
                      </Stack>
                    </StackItem>
                  )}
                </Stack>
              </>
            )}
          </CardBody>
        </Card>
      </PageSection>

      {/* --- Action Card 2: Reinstall Operator (hidden until operator has a CSV) --- */}
      {status?.csv.phase && status.csv.phase !== "Not Found" && (
      <PageSection>
        <Card isLarge>
          <CardTitle>
            <Title headingLevel="h3">Reinstall Operator</Title>
          </CardTitle>
          <CardBody>
            <ReinstallPanel
              status={status}
              onComplete={() => { setLastOperationType("reinstall"); handleMutationComplete(); }}
              onStreamStart={(reinstallType) => handlePipelineStreamStart((reinstallType as PipelineOperationType) || "reinstall_stable")}
              onStreamStep={handlePipelineStreamStep}
              onStreamEnd={handlePipelineStreamEnd}
              nightlyTags={nightlyTags}
              tagsLoading={tagsLoading}
              canMutate={canMutate}
              prerequisitesMet={prerequisitesMet}
            />
          </CardBody>
        </Card>
      </PageSection>
      )}

      {/* --- Setup modal (opened from banner or help) --- */}
      {status && (
        <SetupModal
          isOpen={setupOpen}
          onClose={() => setSetupOpen(false)}
          status={status}
        />
      )}

      {/* --- Confirm Refresh Operator Modal --- */}
      <Modal
        aria-labelledby="confirm-refresh-title"
        variant={ModalVariant.small}
        isOpen={refreshConfirmOpen}
        onClose={() => setRefreshConfirmOpen(false)}
      >
        <ModalHeader title="Refresh Operator" labelId="confirm-refresh-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                This will delete the current CSV and let OLM reinstall the
                operator with updated images from the catalog. The Subscription,
                DSCI, DSC, and all user workloads are preserved.
              </Content>
            </StackItem>
            <StackItem>
              <Alert
                variant="warning"
                title="The operator will be briefly unavailable (1-3 minutes) while OLM reinstalls it."
                isInline
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={handleRefreshOperator}
            isLoading={refreshLoading}
            isDisabled={refreshLoading}
          >
            {refreshLoading ? "Refreshing..." : "Confirm Refresh"}
          </Button>
          <Button
            variant="link"
            onClick={() => setRefreshConfirmOpen(false)}
            isDisabled={refreshLoading}
          >
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
