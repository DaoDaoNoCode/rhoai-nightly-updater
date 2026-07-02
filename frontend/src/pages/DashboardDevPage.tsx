import React, { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  Tab,
  TabContent,
  TabContentBody,
  Tabs,
  TabTitleText,
  TextInput,
  Title,
  Tooltip,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { DashboardState, OperationResponse } from "../types";
import {
  getDashboardState,
  deployPR,
  revertDashboard,
  assistRollout,
  trackFeature,
} from "../services/api";
import { truncateImage } from "../utils";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { QuickResourceCreator } from "../components/QuickResourceCreator";

interface DashboardDevPageProps {
  canMutate: boolean;
}

export const DashboardDevPage: React.FC<DashboardDevPageProps> = ({
  canMutate,
}) => {
  const [activeTab, setActiveTab] = useState(0);
  const [dashState, setDashState] = useState<DashboardState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  const [prNumber, setPrNumber] = useState("");
  const [deploying, setDeploying] = useState(false);
  const [reverting, setReverting] = useState(false);
  const [waitingFor, setWaitingFor] = useState<"deploy" | "revert" | null>(null);
  const [waitStartTime, setWaitStartTime] = useState(0);
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [assisting, setAssisting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Abort in-flight requests on unmount
  const abortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  const parsedPR = parseInt(prNumber, 10);
  const validPR = !isNaN(parsedPR) && parsedPR > 0;

  const fetchState = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const s = await getDashboardState();
      setDashState(s);
      setLastRefreshed(new Date());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load state");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchState(); }, [fetchState]);

  const handleManualRefresh = useCallback(() => {
    setResult(null);
    fetchState();
  }, [fetchState]);

  useEffect(() => {
    if (!deploying && !reverting && !waitingFor) return;
    const id = setInterval(fetchState, 5000);
    return () => clearInterval(id);
  }, [deploying, reverting, waitingFor, fetchState]);

  useEffect(() => {
    if (!waitingFor || !dashState) return;
    const ready = dashState.podReady && !dashState.rolloutPending;
    if (ready) {
      if (waitingFor === "deploy" && dashState.isCustomPR) {
        setWaitingFor(null);
        setWaitStartTime(0);
        setResult({ success: true, message: `PR #${dashState.prNumber} is running. All containers ready.`, logs: [] });
        return;
      } else if (waitingFor === "revert" && !dashState.isCustomPR) {
        setWaitingFor(null);
        setWaitStartTime(0);
        setPrNumber("");
        setResult({ success: true, message: "Dashboard restored to default. Operator is managing the deployment again. The operator will now reconcile the dashboard deployment. This typically takes 2-5 minutes. Refresh the page to check progress.", logs: [] });
        return;
      }
    }
    if (waitStartTime > 0 && Date.now() - waitStartTime > 10 * 60 * 1000) {
      setWaitingFor(null);
      setWaitStartTime(0);
      const timeoutMessage = dashState?.schedulingFailureReason
        ? `Pod cannot be scheduled: ${dashState.schedulingFailureReason}. Click 'Assist Rollout' to free resources.`
        : "Rollout is taking too long. Check cluster resources or pod status in the OpenShift Console.";
      setResult({ success: false, message: timeoutMessage, logs: [] });
    }
  }, [waitingFor, dashState, waitStartTime]);

  useEffect(() => {
    document.title = "Dashboard Dev — RHOAI Nightly Updater";
  }, []);

  const handleDeployConfirm = async () => {
    trackFeature("deploy_pr");
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setDeploying(true);
    setResult(null);
    setWaitingFor(null);
    setConfirmOpen(false);
    try {
      const res = await deployPR(parsedPR);
      if (ac.signal.aborted) return;
      if (res.success) {
        setWaitingFor("deploy");
        setWaitStartTime(Date.now());
        fetchState();
      } else {
        setResult(res);
      }
    } catch (e) {
      if (ac.signal.aborted) return;
      setResult({ success: false, message: e instanceof Error ? e.message : "Deploy failed", logs: [] });
    } finally {
      if (!ac.signal.aborted) {
        setDeploying(false);
      }
    }
  };

  const handleRevert = async () => {
    trackFeature("revert_dashboard");
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setReverting(true);
    setResult(null);
    try {
      const res = await revertDashboard();
      if (ac.signal.aborted) return;
      if (res.success) {
        setWaitingFor("revert");
        setWaitStartTime(Date.now());
        fetchState();
      } else {
        setResult(res);
      }
    } catch (e) {
      if (ac.signal.aborted) return;
      setResult({ success: false, message: e instanceof Error ? e.message : "Revert failed", logs: [] });
    } finally {
      if (!ac.signal.aborted) {
        setReverting(false);
      }
    }
  };

  const handleAssistRollout = async () => {
    trackFeature("assist_rollout");
    setAssisting(true);
    setResult(null);
    try {
      const res = await assistRollout();
      setResult(res);
      if (res.success) {
        setWaitStartTime(Date.now());
      }
    } catch (e) {
      setResult({ success: false, message: e instanceof Error ? e.message : "Assist rollout failed", logs: [] });
    } finally {
      setAssisting(false);
    }
  };

  const isCustom = dashState?.isCustomPR ?? false;
  const readyCount = dashState?.containersReady ?? 0;
  const totalCount = dashState?.containersTotal ?? 0;
  const allReady = dashState?.podReady ?? false;

  return (
    <>
      <PageHeader
        title="Dashboard Dev"
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={handleManualRefresh}
      />

      {error && !error.includes("failed to get dashboard state") && <ErrorAlert error={error} genericTitle="Failed to load" />}

      {error && error.includes("failed to get dashboard state") && (
        <PageSection>
          <Alert variant="info" title="RHOAI Dashboard is not deployed yet" isInline>
            <p>The Dashboard Dev page lets you deploy PR builds of the RHOAI Dashboard for testing.</p>
            <p style={{ marginTop: "0.5rem" }}>
              First install RHOAI from the <Link to="/">Dashboard</Link>, then come back here to deploy PR builds.
            </p>
          </Alert>
        </PageSection>
      )}

      {!canMutate && (
        <PageSection>
          <Alert variant="info" title="Read-only access" isInline isPlain>
            Deploy and revert operations are disabled. Contact a cluster admin for write access.
          </Alert>
        </PageSection>
      )}

      <PageSection>
        <Tabs activeKey={activeTab} onSelect={(_e, key) => setActiveTab(key as number)}>
          <Tab eventKey={0} title={<TabTitleText>PR Deploy</TabTitleText>}>
            <TabContent id="tab-pr-deploy">
              <TabContentBody hasPadding>
                <Card>
                  <CardTitle>
                    <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
                      <FlexItem><Title headingLevel="h3">Dashboard Image</Title></FlexItem>
                      <FlexItem>
                        {dashState && (
                          dashState.rolloutPending ? (
                            <Label color="orange" icon={<Spinner size="sm" aria-label="Rolling out" />}>Rolling out</Label>
                          ) : waitingFor ? (
                            <Label color="orange" icon={<Spinner size="sm" aria-label="Applying" />}>
                              {allReady ? "Applying" : `${readyCount}/${totalCount} ready`}
                            </Label>
                          ) : isCustom ? (
                            <Label color="blue" icon={<CheckCircleIcon />}>
                              PR #{dashState.prNumber}{dashState.prContainers && dashState.prContainers.length > 1 ? ` (${dashState.prContainers.length} containers)` : ""}
                            </Label>
                          ) : (
                            <Label color="green" icon={<CheckCircleIcon />}>Default</Label>
                          )
                        )}
                      </FlexItem>
                    </Flex>
                  </CardTitle>
                  <CardBody>
                    {!dashState && loading && (
                      <Flex justifyContent={{ default: "justifyContentCenter" }} className="pf-v6-u-py-lg">
                        <Spinner aria-label="Loading dashboard state" />
                      </Flex>
                    )}
                    <Stack hasGutter>
                      {dashState && (
                        <StackItem>
                          <Content component="small">
                            <Tooltip content={dashState.currentImage}>
                              <code>{truncateImage(dashState.currentImage)}</code>
                            </Tooltip>
                          </Content>
                        </StackItem>
                      )}

                      {dashState?.rolloutPending && (
                        <StackItem>
                          <Alert variant="warning" title="Rollout in progress" isInline isPlain>
                            Old pod is still serving while the new pod starts up.
                          </Alert>
                        </StackItem>
                      )}

                      {dashState?.rolloutPending && dashState?.pods && dashState.pods.length > 1 && (
                        <StackItem>
                          <Stack hasGutter>
                            {dashState.pods.map((pod) => (
                              <StackItem key={pod.name}>
                                <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                                  <FlexItem>
                                    <Label isCompact color={pod.ready ? "green" : "orange"}>
                                      {pod.ready ? "Serving" : pod.phase === "Pending" ? "Pending" : `${pod.containers?.filter((c) => c.ready).length ?? 0}/${pod.containers?.length ?? 0} ready`}
                                    </Label>
                                  </FlexItem>
                                  <FlexItem><Content component="small">{pod.name}</Content></FlexItem>
                                  {pod.image && (
                                    <FlexItem>
                                      <Content component="small">
                                        <Tooltip content={pod.image}><code>{truncateImage(pod.image, 40)}</code></Tooltip>
                                      </Content>
                                    </FlexItem>
                                  )}
                                </Flex>
                              </StackItem>
                            ))}
                          </Stack>
                        </StackItem>
                      )}

                      {dashState?.rolloutPending && dashState?.schedulingFailureReason && (
                        <StackItem>
                          <Alert variant="warning" title="Pod cannot be scheduled" isInline>
                            <Stack hasGutter>
                              <StackItem>{dashState.schedulingFailureReason}</StackItem>
                              {dashState.canAssistRollout && (
                                <StackItem>
                                  <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                                    <FlexItem>
                                      <Button
                                        variant="secondary"
                                        size="sm"
                                        onClick={handleAssistRollout}
                                        isDisabled={!canMutate || assisting}
                                        isLoading={assisting}
                                      >
                                        Assist Rollout
                                      </Button>
                                    </FlexItem>
                                    <FlexItem>
                                      <Content component="small">Scales down old pod to free cluster resources for the new one</Content>
                                    </FlexItem>
                                  </Flex>
                                </StackItem>
                              )}
                            </Stack>
                          </Alert>
                        </StackItem>
                      )}

                      {waitingFor && !dashState?.rolloutPending && !allReady && (
                        <StackItem>
                          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                            <FlexItem><Spinner size="md" aria-label="Waiting" /></FlexItem>
                            <FlexItem><Content component="small">Waiting for pod rollout...</Content></FlexItem>
                          </Flex>
                        </StackItem>
                      )}

                      {isCustom && allReady && !waitingFor && (
                        <StackItem>
                          <Alert variant="warning" title={`PR #${dashState?.prNumber} is deployed on this shared cluster`} isInline isPlain>
                            Remember to revert after testing.
                            {dashState?.dashboardURL && (
                              <>{" "}<Button variant="link" isInline component="a" href={dashState.dashboardURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">Open dashboard</Button></>
                            )}
                          </Alert>
                        </StackItem>
                      )}

                      {isCustom && dashState?.prNumber && (
                        <StackItem>
                          <Flex gap={{ default: "gapMd" }}>
                            <FlexItem>
                              <Button variant="link" isInline component="a" href={`https://github.com/opendatahub-io/odh-dashboard/pull/${dashState.prNumber}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">PR #{dashState.prNumber}</Button>
                            </FlexItem>
                            <FlexItem>
                              <Button variant="link" isInline component="a" href={`https://quay.io/repository/opendatahub/odh-dashboard?tab=tags&tag=pr-${dashState.prNumber}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">Quay</Button>
                            </FlexItem>
                            <FlexItem>
                              <Button variant="danger" size="sm" onClick={handleRevert} isDisabled={!canMutate || reverting || !!waitingFor} isLoading={reverting}>Revert to default</Button>
                            </FlexItem>
                          </Flex>
                        </StackItem>
                      )}

                      <StackItem>
                        <Flex alignItems={{ default: "alignItemsFlexEnd" }} gap={{ default: "gapSm" }}>
                          <FlexItem>
                            <Content component="small" className="pf-v6-u-mb-xs">PR number</Content>
                            <TextInput type="number" value={prNumber} onChange={(_e, val) => setPrNumber(val)} placeholder="e.g. 7892" aria-label="PR number" className="pf-v6-u-w-initial" />
                          </FlexItem>
                          <FlexItem>
                            <Button variant="primary" onClick={() => setConfirmOpen(true)} isDisabled={!canMutate || !validPR || deploying || !!waitingFor} isLoading={deploying}>Deploy PR</Button>
                          </FlexItem>
                        </Flex>
                      </StackItem>
                      {validPR && (
                        <StackItem>
                          <Content component="small">
                            <Button variant="link" isInline component="a" href={`https://github.com/opendatahub-io/odh-dashboard/pull/${parsedPR}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">PR #{parsedPR}</Button>
                            {" · "}
                            <code>quay.io/opendatahub/odh-dashboard:pr-{parsedPR}</code>
                          </Content>
                        </StackItem>
                      )}

                      {result && (
                        <StackItem>
                          <Alert variant={result.success ? "success" : "danger"} title={result.message} isInline />
                        </StackItem>
                      )}
                    </Stack>
                  </CardBody>
                </Card>
              </TabContentBody>
            </TabContent>
          </Tab>

          <Tab eventKey={1} title={<TabTitleText>Resources</TabTitleText>}>
            <TabContent id="tab-resources">
              <TabContentBody hasPadding>
                <QuickResourceCreator canMutate={canMutate} />
              </TabContentBody>
            </TabContent>
          </Tab>
        </Tabs>
      </PageSection>

      {/* Deploy PR Confirmation Modal */}
      <Modal aria-labelledby="confirm-deploy-title" variant={ModalVariant.small} isOpen={confirmOpen} onClose={() => setConfirmOpen(false)}>
        <ModalHeader title={`Deploy PR #${parsedPR}`} labelId="confirm-deploy-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem><Content component="small"><code>quay.io/opendatahub/odh-dashboard:pr-{parsedPR}</code></Content></StackItem>
            <StackItem>
              <Alert variant="warning" title="Shared cluster impact" isInline>
                This replaces the dashboard for ALL users on this cluster. The operator will stop managing the deployment until you revert. Please revert after testing.
              </Alert>
            </StackItem>
            <StackItem><Alert variant="info" title="The dashboard may be briefly unavailable (1-2 min) during the rollout." isInline isPlain /></StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={handleDeployConfirm} isLoading={deploying} isDisabled={deploying}>Deploy</Button>
          <Button variant="link" onClick={() => setConfirmOpen(false)} isDisabled={deploying}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
