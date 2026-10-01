import React, { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
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
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { DashboardState, OperationResponse } from "../types";
import {
  getDashboardState,
  deployPR,
  deployDashboardMain,
  revertDashboard,
  assistRollout,
  trackFeature,
} from "../services/api";
import { truncateImage } from "../utils";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { DashboardImages } from "../components/DashboardImages";
import { COMPONENTS_POLL_MS } from "../constants";

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
  const [deployMode, setDeployMode] = useState<"main" | "pr">("pr");
  const [reverting, setReverting] = useState(false);
  const [waitingFor, setWaitingFor] = useState<"main" | "pr" | "revert" | null>(null);
  const [waitStartTime, setWaitStartTime] = useState(0);
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [assisting, setAssisting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const stateRequestRef = useRef(0);
  const stateFetchInFlightRef = useRef(0);

  // Abort in-flight requests on unmount
  const abortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      stateRequestRef.current++;
    };
  }, []);

  const parsedPR = Number(prNumber);
  const validPR = /^\d+$/.test(prNumber) && Number.isSafeInteger(parsedPR) && parsedPR > 0;

  const fetchState = useCallback(async (background = false) => {
    const requestId = ++stateRequestRef.current;
    stateFetchInFlightRef.current++;
    if (!background) setLoading(true);
    setError(null);
    try {
      const s = await getDashboardState();
      if (requestId !== stateRequestRef.current) return;
      setDashState(s);
      setLastRefreshed(new Date());
    } catch (e) {
      if (requestId !== stateRequestRef.current) return;
      setError(e instanceof Error ? e.message : "Failed to load state");
    } finally {
      stateFetchInFlightRef.current--;
      if (requestId === stateRequestRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => { fetchState(); }, [fetchState]);

  const handleManualRefresh = useCallback(() => {
    setResult(null);
    fetchState();
  }, [fetchState]);

  useEffect(() => {
    const poll = () => {
      if (document.hidden || stateFetchInFlightRef.current > 0) return;
      void fetchState(true);
    };
    const id = setInterval(poll, deploying || reverting || waitingFor ? 5000 : COMPONENTS_POLL_MS);
    document.addEventListener("visibilitychange", poll);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", poll);
    };
  }, [deploying, reverting, waitingFor, fetchState]);

  useEffect(() => {
    if (!waitingFor || !dashState || deploying || reverting) return;
    const ready = dashState.operatorAvailable ? dashState.allDevImagesReady : dashState.podReady && !dashState.rolloutPending;
    if (ready) {
      if (!dashState.operatorError && (!dashState.operatorAvailable || dashState.devImagesMatchTarget !== false) && ((waitingFor === "pr" && dashState.isCustomPR && dashState.prNumber === parsedPR) || (waitingFor === "main" && dashState.devMode === "main" && dashState.operatorPaused))) {
        setWaitingFor(null);
        setWaitStartTime(0);
        setResult((prev) => ({ success: true, message: `${waitingFor === "main" ? "Latest main" : `PR #${dashState.prNumber}`} is running. All dashboard components are ready. Revert after testing.`, logs: prev?.logs ?? [] }));
        return;
      } else if (waitingFor === "revert" && (dashState.operatorAvailable ? !dashState.operatorPaused && dashState.defaultImagesRestored : !dashState.isCustomPR)) {
        setWaitingFor(null);
        setWaitStartTime(0);
        setPrNumber("");
        setResult((prev) => ({ success: true, message: "Dashboard components restored to the installed release. Operator reconciliation is active.", logs: prev?.logs ?? [] }));
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
  }, [waitingFor, dashState, waitStartTime, deploying, reverting, parsedPR]);

  useEffect(() => {
    document.title = "Dashboard Dev — RHOAI Nightly Updater";
  }, []);

  const handleDeployConfirm = async () => {
    trackFeature("deploy_pr");
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setDeploying(true);
    setDeployMode("pr");
    setResult(null);
    setWaitingFor(null);
    setConfirmOpen(false);
    try {
      const res = await deployPR(parsedPR);
      if (ac.signal.aborted) return;
      if (res.success) {
        setResult(res);
        setWaitingFor("pr");
        setWaitStartTime(Date.now());
        fetchState();
      } else {
        setResult(res);
        fetchState();
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

  const handleDeployMain = async () => {
    trackFeature("deploy_dashboard_main");
    setDeploying(true);
    setDeployMode("main");
    setResult(null);
    setWaitingFor(null);
    try {
      const res = await deployDashboardMain();
      setResult(res);
      if (res.success) { setWaitingFor("main"); setWaitStartTime(Date.now()); }
      fetchState();
    } catch (e) {
      setResult({ success: false, message: e instanceof Error ? e.message : "Deploy failed", logs: [] });
      fetchState();
    } finally { setDeploying(false); }
  };

  const handleRevert = async () => {
    trackFeature("revert_dashboard");
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setReverting(true);
    setResult(null);
    setWaitingFor(null);
    try {
      const res = await revertDashboard();
      if (ac.signal.aborted) return;
      if (res.success) {
        setResult(res);
        setWaitingFor("revert");
        setWaitStartTime(Date.now());
        fetchState();
      } else {
        setResult(res);
        fetchState();
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

  const isCustom = !!dashState && (dashState.isDevMode || dashState.isCustomPR || dashState.operatorPaused || !dashState.managed);
  const readyCount = dashState?.containersReady ?? 0;
  const totalCount = dashState?.containersTotal ?? 0;
  const allReady = dashState?.operatorAvailable ? !!dashState.allDevImagesReady : !!dashState?.podReady;
  const partial = isCustom && dashState?.operatorAvailable && !dashState.operatorError && dashState.devImagesMatchTarget === false;

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
          <Tab eventKey={0} title={<TabTitleText>Image Deploy</TabTitleText>}>
            <TabContent id="tab-pr-deploy">
              <TabContentBody hasPadding>
                <Card>
                  <CardHeader>
                  <CardTitle>
                    <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }}>
                      <FlexItem><Title headingLevel="h3">Dashboard builds</Title></FlexItem>
                      <FlexItem>
                        {dashState && (
                          deploying || reverting ? (
                            <Label color="orange" icon={<Spinner size="sm" aria-label="Applying images" />}>Applying</Label>
                          ) : dashState.operatorError ? (
                            <Label color="orange" icon={<ExclamationTriangleIcon />}>Status unavailable</Label>
                          ) : partial ? (
                            <Label color="orange" icon={<ExclamationTriangleIcon />}>Partial deployment</Label>
                          ) : dashState.rolloutPending || dashState.operatorAvailable && !allReady ? (
                            <Label color="orange" icon={<Spinner size="sm" aria-label="Rolling out" />}>Rolling out</Label>
                          ) : waitingFor ? (
                            <Label color="orange" icon={<Spinner size="sm" aria-label="Applying" />}>
                              {allReady ? "Applying" : `${readyCount}/${totalCount} ready`}
                            </Label>
                          ) : isCustom ? (
                            <Label color="blue" icon={<CheckCircleIcon />}>
                              {dashState.operatorPaused && dashState.defaultImagesRestored ? "Operator paused" : dashState.devMode === "main" ? "Latest main" : dashState.prNumber ? `PR #${dashState.prNumber}` : "Custom images"}
                            </Label>
                          ) : (
                            <Label color="green" icon={<CheckCircleIcon />}>Default</Label>
                          )
                        )}
                      </FlexItem>
                    </Flex>
                  </CardTitle>
                  </CardHeader>
                  <CardBody>
                    {!dashState && loading && (
                      <Flex justifyContent={{ default: "justifyContentCenter" }} className="pf-v6-u-py-lg">
                        <Spinner aria-label="Loading dashboard state" />
                      </Flex>
                    )}
                    <Stack hasGutter>
                      {dashState?.operatorError && <StackItem><Alert variant="warning" title="Cannot verify dashboard-operator" isInline>{dashState.operatorError}</Alert></StackItem>}
                      {result && <StackItem><Alert variant={result.success ? "success" : "danger"} title={result.message} isInline /></StackItem>}
                      {partial && !result && <StackItem><Alert variant="warning" title="Selected build is not fully applied" isInline>Retry the deployment to finish updating the dashboard, or revert to restore the installed release.</Alert></StackItem>}
                      <StackItem>
                        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
                          <FlexItem><Button variant="primary" onClick={handleDeployMain} isDisabled={!canMutate || !dashState?.operatorAvailable || !!dashState?.operatorError || deploying || reverting || !!waitingFor} isLoading={deploying && deployMode === "main"}>Deploy latest main</Button></FlexItem>
                          <FlexItem><Button variant="secondary" onClick={handleRevert} isDisabled={!canMutate || !isCustom || deploying || reverting} isLoading={reverting}>Revert to default</Button></FlexItem>
                        </Flex>
                        <Content component="p" className="pf-v6-u-mt-md">Test the latest main builds across dashboard components, or deploy only the images published for a PR.</Content>
                      </StackItem>
                      {dashState?.operatorAvailable && <StackItem><Flex gap={{ default: "gapSm" }}><FlexItem><Label isCompact color={dashState.operatorPaused ? "orange" : "green"}>Operator {dashState.operatorPaused ? "paused" : "running"}</Label></FlexItem>{dashState.deploymentMode && <FlexItem><Label isCompact>{dashState.deploymentMode}</Label></FlexItem>}</Flex></StackItem>}
                      {dashState && !dashState.operatorAvailable && (
                        <StackItem>
                          <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                            <FlexItem>
                              <Content component="small">
                                <Tooltip content={dashState.currentImage}>
                                  <code>{truncateImage(dashState.currentImage)}</code>
                                </Tooltip>
                              </Content>
                            </FlexItem>
                            {dashState.deploymentMode && (
                              <FlexItem>
                                <Label isCompact color={dashState.deploymentMode === "Standalone" ? "blue" : "grey"}>
                                  {dashState.deploymentMode}
                                </Label>
                              </FlexItem>
                            )}
                          </Flex>
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

                      {isCustom && allReady && !waitingFor && !partial && !deploying && !reverting && (
                        <StackItem>
                          <Alert variant="warning" title={`${dashState?.devMode === "main" ? "Latest main" : dashState?.prNumber ? `PR #${dashState.prNumber}` : "Custom dashboard images"} deployed on this shared cluster`} isInline isPlain>
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
                          </Flex>
                        </StackItem>
                      )}

                      <StackItem>
                        <Flex alignItems={{ default: "alignItemsFlexEnd" }} gap={{ default: "gapSm" }}>
                          <FlexItem>
                            <Content component="small" className="pf-v6-u-mb-xs">PR number</Content>
                            <TextInput type="number" value={prNumber} onChange={(_e, val) => setPrNumber(val)} placeholder="e.g. 7892" aria-label="PR number" className="pf-v6-u-w-initial" isDisabled={deploying || reverting || !!waitingFor} />
                          </FlexItem>
                          <FlexItem>
                            <Button variant="secondary" onClick={() => setConfirmOpen(true)} isDisabled={!canMutate || !validPR || !dashState || !!dashState.operatorError || deploying || reverting || !!waitingFor} isLoading={deploying && deployMode === "pr"}>Deploy PR</Button>
                          </FlexItem>
                        </Flex>
                      </StackItem>
                      {validPR && (
                        <StackItem>
                          <Content component="small">
                            <Button variant="link" isInline component="a" href={`https://github.com/opendatahub-io/odh-dashboard/pull/${parsedPR}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">PR #{parsedPR}</Button>
                            {" · "}
                            <code>pr-{parsedPR}</code> images for installed dashboard components
                          </Content>
                        </StackItem>
                      )}

                      {dashState?.devImages && <StackItem><DashboardImages images={dashState.devImages} /></StackItem>}
                      {result?.logs?.length ? <StackItem><details><summary>Deployment details</summary><pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{result.logs.join("\n")}</pre></details></StackItem> : null}
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
            <StackItem><Content component="small">Deploy published <code>pr-{parsedPR}</code> images for installed dashboard components. Components without a PR build keep their current images.</Content></StackItem>
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
