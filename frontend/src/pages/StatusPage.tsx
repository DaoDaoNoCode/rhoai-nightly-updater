import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  CodeBlock,
  CodeBlockCode,
  Content,
  Divider,
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
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import CloneIcon from "@patternfly/react-icons/dist/esm/icons/clone-icon";
import type { NightlyTag, OperationResponse } from "../types";
import { fetchNightlyTags, streamRefresh, trackFeature, createDSC, getDSCPreview, toApiError } from "../services/api";
import { prerequisitesMet as checkPrereqs, operatorInstalled } from "../utils";
import { StatusCards } from "../components/StatusCards";
import { PullSecretCard } from "../components/PullSecretCard";
import { UpdatePanel } from "../components/UpdatePanel";
import { ReinstallPanel } from "../components/ReinstallPanel";
import { SetupModal } from "../components/PrerequisitesPanel";
import { ActivityLog } from "../components/ActivityLog";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { OperationProgress, useHasOperationProgress } from "../components/OperationProgress";
import { TooltipButton, NO_PERMISSION_REASON, OPERATION_RUNNING_REASON } from "../components/TooltipButton";
import { describeError } from "../errors";
import { useClusterStatus, useOperation } from "../state/AppState";

interface StatusPageProps {
  setupOpen: boolean;
  setSetupOpen: (open: boolean) => void;
  canMutate: boolean;
}

export const StatusPage: React.FC<StatusPageProps> = ({
  setupOpen,
  setSetupOpen,
  canMutate,
}) => {
  const { status, loading, error, lastRefreshed, refresh } = useClusterStatus();
  const operation = useOperation();
  const operationRunning = operation.running;
  const showProgress = useHasOperationProgress();

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

  // Refresh operator: confirmation only. The run and its result live in the
  // app-level operation store.
  const [refreshConfirmOpen, setRefreshConfirmOpen] = useState(false);
  const refreshRun = operation.run?.kind === "refresh" ? operation.run : null;
  const refreshOutcome = refreshRun?.outcome;
  const refreshFailure = refreshOutcome?.status === "failed"
    ? describeError({ name: "ApiError", status: refreshOutcome.httpStatus ?? 0, errorCode: refreshOutcome.errorCode ?? "operation_failed", message: refreshOutcome.message }, "Refresh failed")
    : null;

  // DSC creation state
  const [dscLoading, setDscLoading] = useState(false);
  const [dscResult, setDscResult] = useState<OperationResponse | null>(null);
  const [dscModalOpen, setDscModalOpen] = useState(false);
  const [dscPreviewYAML, setDscPreviewYAML] = useState<string>("");
  const [dscPreviewLoading, setDscPreviewLoading] = useState(false);
  const [dscPreviewError, setDscPreviewError] = useState("");

  const handleRefreshOperator = () => {
    trackFeature("refresh_operator");
    setRefreshConfirmOpen(false);
    operation.start("refresh", (h) => streamRefresh(h.onStep, h.onDone, h.onDetach), status?.csv.name);
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
        message: toApiError(e, "Failed to create DSC").message,
        logs: [],
      });
    } finally {
      setDscLoading(false);
    }
  };

  return (
    <>
      {/* --- Header row: Cluster Status + Refresh --- */}
      <PageHeader
        title="Cluster Status"
        lastRefreshed={lastRefreshed}
        loading={loading || operationRunning}
        onRefresh={refresh}
      />

      {/* --- HTTP error alerts --- */}
      {error && <ErrorAlert error={error} genericTitle="Failed to load status" />}

      {/* --- Read-only access banner --- */}
      {!canMutate && (
        <PageSection>
          <Alert variant="info" title="Read-only access" isInline isPlain component="p">
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
          error={error?.message ?? null}
          reconciling={operation.state.reconcile.active}
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
                  <TooltipButton
                      variant="primary"
                      onClick={async () => {
                        setDscModalOpen(true);
                        setDscPreviewLoading(true);
                        setDscPreviewYAML("");
                        setDscPreviewError("");
                        try {
                          const res = await getDSCPreview();
                          setDscPreviewYAML(res.yaml);
                        } catch (err) {
                          setDscPreviewError(toApiError(err).message);
                        } finally {
                          setDscPreviewLoading(false);
                        }
                      }}
                      isLoading={dscLoading}
                      isDisabled={dscLoading}
                      disabledReason={!canMutate ? "You don't have permission to create a DSC. Contact a cluster admin." : null}
                    >
                      Create DataScienceCluster
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
                  ) : dscPreviewError ? (
                    <Alert variant="danger" title="Could not fetch version-matched DSC defaults" isInline component="p">{dscPreviewError}</Alert>
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
                isDisabled={dscLoading || dscPreviewLoading || !dscPreviewYAML || !!dscPreviewError || !canMutate}
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
      {showProgress && (
        <PageSection aria-label="Operation progress">
          <OperationProgress />
        </PageSection>
      )}

      {/* --- Action Card 1: Install / Upgrade to Nightly Build --- */}
      <PageSection>
        <Card isLarge>
          <CardTitle>
            <Title headingLevel="h3">{status?.csv.phase && status.csv.phase !== "Not Found" ? "Upgrade to Nightly Build" : "Install Nightly Build"}</Title>
          </CardTitle>
          <CardBody>
            <UpdatePanel status={status} canMutate={canMutate} />

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
                    <TooltipButton
                      variant="secondary"
                      icon={<SyncAltIcon />}
                      onClick={() => setRefreshConfirmOpen(true)}
                      isLoading={operationRunning && !!refreshRun && !refreshRun.outcome}
                      isDisabled={operationRunning}
                      disabledReason={
                        !canMutate ? NO_PERMISSION_REASON
                        : operationRunning ? OPERATION_RUNNING_REASON
                        : !prerequisitesMet ? "Finish the one-time cluster setup first (pull secret and image mirror)."
                        : !operatorInstalled(status) ? "The operator is not installed."
                        : null
                      }
                      size="sm"
                    >
                      Refresh operator
                    </TooltipButton>
                  </StackItem>
                  {refreshFailure && (
                    <StackItem>
                      <Alert variant={refreshFailure.variant} title={refreshFailure.title} isInline component="p">
                        {refreshFailure.body}
                      </Alert>
                    </StackItem>
                  )}
                  {refreshOutcome?.status === "succeeded" && (
                    <StackItem>
                      <Alert
                        variant="success"
                        title="Operator refresh initiated. OLM will reinstall with updated images."
                        isInline
                        component="p"
                      />
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
                component="p"
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={handleRefreshOperator}
            isDisabled={operationRunning}
          >
            Confirm Refresh
          </Button>
          <Button
            variant="link"
            onClick={() => setRefreshConfirmOpen(false)}
          >
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
