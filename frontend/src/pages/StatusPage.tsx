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
  PageSection,
  Card,
  CardTitle,
  CardBody,
  Stack,
  StackItem,
  Title,
  Tooltip,
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import type { NightlyTag, OperationResponse, StatusResponse, UpdateStep } from "../types";
import { fetchNightlyTags, streamRefresh, trackFeature } from "../services/api";
import { StatusCards } from "../components/StatusCards";
import { UpdatePanel } from "../components/UpdatePanel";
import { type OperationType as PipelineOperationType } from "../components/UpdatePipeline";
import { ReinstallPanel } from "../components/ReinstallPanel";
import {
  PrerequisitesBanner,
  SetupModal,
} from "../components/PrerequisitesPanel";
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

  // Shared nightly tags — fetched once, used by both UpdatePanel and ReinstallPanel
  const [nightlyTags, setNightlyTags] = useState<NightlyTag[]>([]);
  const [tagsLoading, setTagsLoading] = useState(false);

  const loadTags = useCallback(async () => {
    setTagsLoading(true);
    try {
      const res = await fetchNightlyTags();
      setNightlyTags(res.tags || []);
    } catch {
      // Tags are optional — don't block the page
    } finally {
      setTagsLoading(false);
    }
  }, []);

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

  const isOnNightly = status?.subscription.source !== status?.stableSource;
  const prerequisitesMet = !!(status?.pullSecret.exists && status?.imageMirror.exists);

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
        loading={loading}
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

      {/* --- First-time onboarding banner --- */}
      {showOnboarding && (
        <PageSection>
          <Alert
            variant="info"
            title="Getting Started"
            isInline
            actionClose={
              <AlertActionCloseButton onClose={handleDismissOnboarding} />
            }
          >
            <p style={{ marginBottom: "0.5rem" }}>
              Welcome to RHOAI Nightly Updater! This tool helps you install and
              manage RHOAI nightly builds on this cluster.
            </p>
            <p style={{ marginBottom: "0.5rem" }}>
              Before you can start, complete the one-time cluster setup:
            </p>
            <List component="ol">
              <ListItem>
                Configure the pull secret (use the Pull Secret card below)
              </ListItem>
              <ListItem>
                Set up the image mirror (click &quot;View setup
                instructions&quot; in the prerequisites banner below)
              </ListItem>
            </List>
          </Alert>
        </PageSection>
      )}

      {/* --- Status cards (dashboard) --- */}
      <PageSection>
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

      {/* --- Prerequisites banner (only when not met) --- */}
      {status && (
        <PageSection>
          <PrerequisitesBanner
            status={status}
            onOpenSetup={() => setSetupOpen(true)}
          />
        </PageSection>
      )}

      {/* --- Action Card 1: Upgrade to Nightly Build --- */}
      <PageSection>
        <Card isLarge>
          <CardTitle>
            <Title headingLevel="h3">Upgrade to Nightly Build</Title>
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
                        isDisabled={refreshLoading || !canMutate}
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

      {/* --- Action Card 2: Reinstall Operator --- */}
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
