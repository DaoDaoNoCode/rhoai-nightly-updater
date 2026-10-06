import React, { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Label,
  PageSection,
  Radio,
  Skeleton,
  Stack,
  StackItem,
  Tab,
  Tabs,
  TabTitleText,
  TextInput,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
import type { DashboardDevImage, DashboardOverride, DashboardState, OperationResponse } from "../types";
import {
  assistRolloutFor,
  deployDashboardLatestMain,
  deployDashboardPR,
  getDashboardState,
  revertDashboard,
  toApiError,
  trackFeature,
  type ApiError,
} from "../services/api";
import { usePolling } from "../hooks/usePolling";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { QuickResourceCreator } from "../components/QuickResourceCreator";
import { DashboardImages } from "../components/DashboardImages";
import { DashboardSessionPanel, flavorName, sessionTitle } from "../components/DashboardSessionPanel";
import { ConfirmActionModal } from "../components/ConfirmActionModal";
import { TooltipButton } from "../components/TooltipButton";
import { COMPONENTS_POLL_MS } from "../constants";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { formatRelativeTime } from "../utils";
import { useClusterBusyHandler, useDashboardOverride, useMutationBlocker, usePermissions } from "../state/AppInfo";

/**
 * Container states that do not clear without a change (image, config or
 * crash fix). Mirrors terminalWaitingReasons in pkg/cluster/resources.go.
 */
const TERMINAL_WAITING_REASONS = new Set([
  "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "CrashLoopBackOff",
  "CreateContainerConfigError", "CreateContainerError", "RunContainerError",
]);

/** Dashboard containers the server reports as stuck (ProgressDeadlineExceeded) or unable to start. */
export function blockedDashboardImages(state: DashboardState | null): DashboardDevImage[] {
  return (state?.devImages ?? []).filter((i) => !i.ready && (!!i.rolloutStuck || TERMINAL_WAITING_REASONS.has(i.waitingReason ?? "")));
}

type Action = "pr" | "main" | "revert" | "assist";

const DASHBOARD_NS = "redhat-ods-applications";

/** The override for a 404 dashboard_not_deployed body (B4 sends it while the operator is paused). */
function overrideFromError(err: ApiError): DashboardOverride | undefined {
  const details = err.details as { override?: DashboardOverride } | undefined;
  return details?.override?.active ? details.override : undefined;
}

export function isNotDeployed(err: ApiError | null): boolean {
  return !!err && (err.errorCode === "dashboard_not_deployed" || (err.status === 404 && err.errorCode === "not_found"));
}

export const DashboardDevPage: React.FC = () => {
  // One source for every cluster change: permissions, session and the global
  // operation lock (R4b c, N1).
  const blocker = useMutationBlocker();
  const permissions = usePermissions();
  const onBusy = useClusterBusyHandler();
  const { refresh: refreshOverride } = useDashboardOverride();
  const [searchParams, setSearchParams] = useSearchParams();
  const activeTab = searchParams.get("tab") === "resources" ? 1 : 0;
  const [dashState, setDashState] = useState<DashboardState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  const [prNumber, setPrNumber] = useState("");
  const [flavor, setFlavor] = useState<string | null>(null);
  const [running, setRunning] = useState<Action | null>(null);
  const [confirm, setConfirm] = useState<Action | null>(null);
  const [waitingFor, setWaitingFor] = useState<"main" | "pr" | "revert" | null>(null);
  const [result, setResult] = useState<OperationResponse | null>(null);
  const stateRequestRef = useRef(0);
  const inFlight = useRef(0);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const parsedPR = Number(prNumber);
  const validPR = /^\d+$/.test(prNumber) && Number.isSafeInteger(parsedPR) && parsedPR > 0;

  const fetchState = useCallback(async (background = false) => {
    const requestId = ++stateRequestRef.current;
    inFlight.current++;
    if (!background) setLoading(true);
    try {
      const s = await getDashboardState();
      if (!mounted.current || requestId !== stateRequestRef.current) return;
      setDashState(s);
      setError(null);
      setLastRefreshed(new Date());
    } catch (e) {
      if (!mounted.current || requestId !== stateRequestRef.current) return;
      const err = toApiError(e, "Could not load the dashboard state");
      setError(err);
      // "Not deployed" is a state: drop the old data. Other errors keep it (shown as stale).
      if (isNotDeployed(err)) setDashState(null);
    } finally {
      inFlight.current--;
      if (mounted.current && requestId === stateRequestRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => { void fetchState(); }, [fetchState]);

  const busy = !!running || !!waitingFor;
  usePolling(() => (inFlight.current > 0 ? undefined : fetchState(true)), {
    delay: busy ? 5000 : COMPONENTS_POLL_MS,
    restartKey: busy,
  });

  // Finish waiting when the server shows the requested state. A stuck
  // rollout is reported by the server (ProgressDeadlineExceeded), so there
  // is no client-side timer that a reload would reset (A04-7).
  useEffect(() => {
    if (!waitingFor || !dashState || running) return;
    // Stop waiting as soon as the server reports a rollout that cannot
    // finish on its own: ProgressDeadlineExceeded, or a container in a
    // terminal waiting state (ImagePullBackOff...), not only after 10 minutes.
    if (dashState.rolloutStuck || blockedDashboardImages(dashState).length > 0) {
      setWaitingFor(null);
      return;
    }
    const ready = dashState.operatorAvailable ? dashState.allDevImagesReady : dashState.podReady && !dashState.rolloutPending;
    if (!ready) return;
    const matches = !dashState.operatorAvailable || dashState.devImagesMatchTarget !== false;
    if (waitingFor === "pr" && matches && dashState.isCustomPR && dashState.prNumber === parsedPR) {
      setWaitingFor(null);
      setResult((prev) => ({ success: true, message: `PR #${dashState.prNumber} is running and every dashboard container is ready. Revert after testing.`, logs: prev?.logs ?? [] }));
    } else if (waitingFor === "main" && matches && dashState.devMode === "main" && dashState.operatorPaused) {
      setWaitingFor(null);
      setResult((prev) => ({ success: true, message: "Latest main is running and every dashboard container is ready. Revert after testing.", logs: prev?.logs ?? [] }));
    } else if (waitingFor === "revert" && (dashState.operatorAvailable ? !dashState.operatorPaused && dashState.defaultImagesRestored : !dashState.isCustomPR)) {
      setWaitingFor(null);
      setPrNumber("");
      setResult((prev) => ({ success: true, message: "The dashboard runs the installed release again, and dashboard-operator manages it.", logs: prev?.logs ?? [] }));
    }
  }, [waitingFor, dashState, running, parsedPR]);

  // The app-wide override (global banner, the Update dialog's "Revert
  // Dashboard Dev and update") follows each change here at once (N2).
  const wasWaiting = useRef(false);
  useEffect(() => {
    if (wasWaiting.current && !waitingFor) refreshOverride();
    wasWaiting.current = !!waitingFor;
  }, [waitingFor, refreshOverride]);

  useEffect(() => {
    document.title = "Dashboard Dev — RHOAI Nightly Updater";
  }, []);

  const notDeployed = isNotDeployed(error);
  const override = dashState?.override ?? (error ? overrideFromError(error) : undefined);
  const sessionActive = !!override?.active || !!dashState?.isDevMode || !!dashState?.isCustomPR || !!dashState?.operatorPaused || (!!dashState && !dashState.managed);
  const flavors = dashState?.availableFlavors ?? [];
  const chosenFlavor = flavor ?? dashState?.defaultFlavor ?? "rhoai";
  const containers = dashState?.devImages?.length ?? 0;
  const legacy = !!dashState && !dashState.operatorAvailable;

  const run = async (action: Action) => {
    setConfirm(null);
    setRunning(action);
    setResult(null);
    setWaitingFor(null);
    let res: OperationResponse;
    try {
      if (action === "pr") {
        trackFeature("deploy_pr");
        res = await deployDashboardPR(parsedPR, flavors.length > 1 ? chosenFlavor : undefined);
      } else if (action === "main") {
        trackFeature("deploy_dashboard_main");
        res = await deployDashboardLatestMain(flavors.length > 1 ? chosenFlavor : undefined);
      } else if (action === "revert") {
        trackFeature("revert_dashboard");
        res = await revertDashboard();
      } else {
        trackFeature("assist_rollout");
        res = await assistRolloutFor({ namespace: DASHBOARD_NS, deployment: "rhods-dashboard" });
      }
    } catch (e) {
      res = errorResult(e, "The request failed");
    }
    // A cluster_busy refusal names the running operation: show it and lock at once.
    onBusy(res);
    if (res.success) refreshOverride();
    if (!mounted.current) return;
    setResult(res);
    if (res.success && action !== "assist") setWaitingFor(action);
    setRunning(null);
    void fetchState(true);
  };

  const mutateReason = (needsOperator: boolean): string | null => {
    if (blocker) return blocker;
    if (running) return "Another Dashboard Dev action is running.";
    if (waitingFor) return "Waiting for the last change to roll out.";
    if (needsOperator && dashState?.operatorError) return "The state of dashboard-operator cannot be read.";
    if (needsOperator && override?.dashboardDeleting) return "A Dashboard CR is being deleted. Revert first.";
    if (needsOperator && !dashState) return "The dashboard state is not loaded.";
    return null;
  };

  const sessionOwner = override?.startedBy ? `${override.startedBy}'s session (${override.startedAt ? formatRelativeTime(override.startedAt) : "start unknown"})` : "the active session";
  const tagsFor = (kind: "pr" | "main") => {
    const n = kind === "pr" ? `${parsedPR}` : "";
    if (legacy) return kind === "pr" ? `pr-${n}` : "main";
    if (chosenFlavor === "odh") return kind === "pr" ? `pr-${n}` : "main";
    return kind === "pr" ? `odh-pr-${n} (or pr-${n} where no Konflux build exists)` : "odh-stable (or main where no Konflux build exists)";
  };

  const deployChanges = (kind: "pr" | "main"): React.ReactNode[] => legacy ? [
    <>Deployment <code>{DASHBOARD_NS}/rhods-dashboard</code> runs the <code>{tagsFor(kind)}</code> image and is marked <code>opendatahub.io/managed: &quot;false&quot;</code> so the operator leaves it alone until you revert.</>,
  ] : [
    <>Deployment <code>{DASHBOARD_NS}/dashboard-operator</code> is scaled to 0 so it stops resetting the dashboard images. The original replica count is saved for Revert.</>,
    <>All {containers || "the"} dashboard containers are updated: those with a <code>{tagsFor(kind)}</code> build run it (pinned by digest); the others run their release image.</>,
    ...(sessionActive ? [<>This replaces {sessionOwner}.</>] : []),
  ];

  const status = (() => {
    if (!dashState) return null;
    if (running && running !== "assist") return <Label color="blue" icon={<InProgressIcon />}>Applying</Label>;
    if (dashState.operatorError) return <Label color="orange" icon={<ExclamationTriangleIcon />}>Status unavailable</Label>;
    if (dashState.rolloutStuck) return <Label color="red" icon={<ExclamationTriangleIcon />}>Rollout stuck</Label>;
    if (dashState.rolloutPending || (dashState.operatorAvailable && !dashState.allDevImagesReady)) return <Label color="blue" icon={<InProgressIcon />}>Rolling out</Label>;
    if (sessionActive && override) return <Label color="orange">{sessionTitle(override)}</Label>;
    if (sessionActive) return <Label color="orange">{dashState.devMode === "main" ? "Latest main" : dashState.prNumber ? `PR #${dashState.prNumber}` : "Custom images"}</Label>;
    return <Label color="green" icon={<CheckCircleIcon />}>Installed release</Label>;
  })();

  return (
    <>
      <PageHeader title="Dashboard Dev" lastRefreshed={lastRefreshed} loading={loading} onRefresh={() => { setResult(null); void fetchState(); }} />

      {error && !notDeployed && (
        <LoadErrorAlert error={error} genericTitle="Could not load the dashboard state" onRetry={() => void fetchState()} stale={!!dashState} />
      )}

      {permissions.status === "denied" && (
        <PageSection>
          <Alert variant="info" title="Read-only access" isInline isPlain component="p">
            Deploy and revert are disabled. Ask a cluster admin for write access.
          </Alert>
        </PageSection>
      )}

      <PageSection>
        <Tabs activeKey={activeTab} onSelect={(_e, key) => setSearchParams(key === 1 ? { tab: "resources" } : {}, { replace: true })} aria-label="Dashboard Dev sections">
          <Tab eventKey={0} title={<TabTitleText>Dashboard builds</TabTitleText>}>
            <div style={{ paddingTop: "var(--pf-t--global--spacer--md)" }}>
              <Stack hasGutter>
                {notDeployed && (
                  <StackItem>
                    <Alert variant="info" title="The RHOAI Dashboard is not deployed" isInline component="p">
                      <p>
                        The dashboard appears once RHOAI is installed and the DataScienceCluster has the dashboard set
                        to <code>Managed</code>. See the <Link to="/components">Components</Link> page, or install RHOAI from the <Link to="/">Dashboard</Link>.
                      </p>
                      {error?.message && <p><small>Server: {error.message}</small></p>}
                    </Alert>
                  </StackItem>
                )}

                {override?.active && (
                  <StackItem>
                    <DashboardSessionPanel
                      override={override}
                      dashboardURL={dashState?.dashboardURL}
                      revertDisabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}
                      reverting={running === "revert"}
                      onRevert={() => setConfirm("revert")}
                    />
                  </StackItem>
                )}

                {result && (
                  <StackItem>
                    <Alert variant={outcomeVariant(result)} title={outcomeTitle(result, "The action failed")} isInline isLiveRegion component="p"
                      actionClose={<AlertActionCloseButton onClose={() => setResult(null)} />}>
                      {result.success ? undefined : result.message}
                      {(result.logs?.length ?? 0) > 0 && (
                        <details><summary>Details</summary><pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{result.logs!.join("\n")}</pre></details>
                      )}
                    </Alert>
                  </StackItem>
                )}

                {dashState?.rolloutStuck && (
                  <StackItem>
                    <Alert variant="danger" title="The dashboard rollout is stuck" isInline isLiveRegion component="p">
                      <p style={{ overflowWrap: "anywhere" }}>{dashState.stuckReason || "A dashboard Deployment reports ProgressDeadlineExceeded."}</p>
                      <p>Its progress deadline passed, so Kubernetes stopped waiting for it. Revert to restore the release images, or deploy another build.</p>
                    </Alert>
                  </StackItem>
                )}

                {!dashState?.rolloutStuck && blockedDashboardImages(dashState).length > 0 && (
                  <StackItem>
                    <Alert variant="danger" title="A dashboard container cannot start" isInline isLiveRegion component="p">
                      <ul>
                        {blockedDashboardImages(dashState).map((i) => (
                          <li key={`${i.deployment}/${i.container}`} style={{ overflowWrap: "anywhere" }}>
                            <code>{i.deployment}/{i.container}</code>: {i.waitingReason}{i.waitingMessage ? ` (${i.waitingMessage})` : ""}
                          </li>
                        ))}
                      </ul>
                      <p>This does not clear on its own. Revert to restore the release images, or deploy another build.</p>
                    </Alert>
                  </StackItem>
                )}

                {!notDeployed && (
                  <StackItem>
                    <Card>
                      <CardHeader actions={{ actions: status, hasNoOffset: true }}>
                        <CardTitle component="h2">Deploy a dashboard build</CardTitle>
                        <Content component="small">
                          Replace the dashboard on this cluster with the latest main build or with the images a pull request
                          published. Every user of the cluster sees it until someone reverts.
                        </Content>
                      </CardHeader>
                      <CardBody>
                        {!dashState && loading && <Skeleton height="6rem" screenreaderText="Loading the dashboard state" />}
                        {dashState && (
                          <Stack hasGutter>
                            {dashState.operatorError && (
                              <StackItem><Alert variant="warning" title="Cannot read dashboard-operator" isInline component="p">{dashState.operatorError}</Alert></StackItem>
                            )}
                            {flavors.length > 1 && (
                              <StackItem>
                                <FormGroup role="radiogroup" isInline fieldId="dashboard-flavor" label="Build" isStack>
                                  <Radio id="flavor-rhoai" name="dashboard-flavor" label="RHOAI build (Konflux)" isChecked={chosenFlavor === "rhoai"} onChange={() => setFlavor("rhoai")}
                                    description={<>RHOAI branding and docs links. Tags <code>odh-pr-N</code> and <code>odh-stable</code>; components without one fall back to the ODH build.</>} />
                                  <Radio id="flavor-odh" name="dashboard-flavor" label="ODH build (OpenShift CI)" isChecked={chosenFlavor === "odh"} onChange={() => setFlavor("odh")}
                                    description={<>Open Data Hub branding. Tags <code>pr-N</code> and <code>main</code>; usually fewer components have a PR build.</>} />
                                </FormGroup>
                              </StackItem>
                            )}
                            <StackItem>
                              <Flex alignItems={{ default: "alignItemsFlexEnd" }} gap={{ default: "gapMd" }}>
                                <FlexItem>
                                  <FormGroup label="Pull request number" fieldId="dashboard-pr">
                                    <TextInput id="dashboard-pr" type="text" inputMode="numeric" value={prNumber} onChange={(_e, val) => setPrNumber(val.trim())} placeholder="e.g. 10085"
                                      validated={prNumber && !validPR ? "error" : "default"} aria-describedby="dashboard-pr-help" style={{ maxWidth: "12rem" }} />
                                    <FormHelperText>
                                      <HelperText id="dashboard-pr-help">
                                        <HelperTextItem variant={prNumber && !validPR ? "error" : "default"}>
                                          {prNumber && !validPR ? "Enter a positive whole number." : "An opendatahub-io/odh-dashboard pull request."}
                                        </HelperTextItem>
                                      </HelperText>
                                    </FormHelperText>
                                  </FormGroup>
                                </FlexItem>
                                <FlexItem>
                                  <TooltipButton variant="primary" onClick={() => setConfirm("pr")} isLoading={running === "pr"}
                                    disabledReason={mutateReason(true) ?? (!validPR ? "Enter a PR number first." : null)}>
                                    Deploy PR
                                  </TooltipButton>
                                </FlexItem>
                                <FlexItem>
                                  <TooltipButton variant="secondary" onClick={() => setConfirm("main")} isLoading={running === "main"}
                                    disabledReason={mutateReason(true) ?? (!dashState.operatorAvailable ? "Latest main needs dashboard-operator (RHOAI 3.5 or later)." : null)}>
                                    Deploy latest main
                                  </TooltipButton>
                                </FlexItem>
                                {sessionActive && !override?.active && (
                                  <FlexItem>
                                    <TooltipButton variant="secondary" onClick={() => setConfirm("revert")} isLoading={running === "revert"} disabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}>
                                      Revert to default
                                    </TooltipButton>
                                  </FlexItem>
                                )}
                              </Flex>
                              {validPR && (
                                <Content component="small">
                                  <Button variant="link" isInline component="a" href={`https://github.com/opendatahub-io/odh-dashboard/pull/${parsedPR}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                                    PR #{parsedPR} on GitHub
                                  </Button>
                                </Content>
                              )}
                            </StackItem>

                            {waitingFor && !dashState.rolloutStuck && (
                              <StackItem>
                                <Content component="p" aria-live="polite"><InProgressIcon /> Waiting for the dashboard pods to roll out. This page checks every 5 seconds.</Content>
                              </StackItem>
                            )}

                            {(dashState.rolloutPending || dashState.rolloutStuck) && (dashState.schedulingFailureReason || dashState.canAssistRollout) && (
                              <StackItem>
                                <Alert variant="warning" title="A new dashboard pod cannot be scheduled" isInline component="p">
                                  <Stack hasGutter>
                                    {dashState.schedulingFailureReason && <StackItem>{dashState.schedulingFailureReason}</StackItem>}
                                    {dashState.canAssistRollout && (
                                      <StackItem>
                                        {/* (e) Available as soon as the server offers it: the rollout
                                            it rescues is the one this page is waiting for. */}
                                        <TooltipButton variant="secondary" size="sm" onClick={() => setConfirm("assist")} isLoading={running === "assist"}
                                          disabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}>
                                          Assist rollout
                                        </TooltipButton>
                                      </StackItem>
                                    )}
                                  </Stack>
                                </Alert>
                              </StackItem>
                            )}

                            {legacy && (
                              <StackItem>
                                <Content component="small">This cluster has no dashboard-operator (RHOAI 3.4 or earlier): only <code>rhods-dashboard</code> is changed.</Content>
                              </StackItem>
                            )}

                            {(dashState.devImages?.length ?? 0) > 0 && (
                              <StackItem><DashboardImages images={dashState.devImages!} defaultExpanded={sessionActive} /></StackItem>
                            )}
                          </Stack>
                        )}
                      </CardBody>
                    </Card>
                  </StackItem>
                )}
              </Stack>
            </div>
          </Tab>

          <Tab eventKey={1} title={<TabTitleText>Test resources</TabTitleText>}>
            <div style={{ paddingTop: "var(--pf-t--global--spacer--md)" }}>
              <QuickResourceCreator mutateBlocker={blocker} onResult={onBusy} />
            </div>
          </Tab>
        </Tabs>
      </PageSection>

      <ConfirmActionModal
        isOpen={confirm === "pr" || confirm === "main"}
        title={confirm === "pr" ? `Deploy PR #${parsedPR}?` : "Deploy the latest main build?"}
        changes={confirm === "pr" || confirm === "main" ? deployChanges(confirm) : []}
        confirmLabel={confirm === "pr" ? `Deploy PR #${parsedPR}` : "Deploy latest main"}
        isLoading={running === "pr" || running === "main"}
        onConfirm={() => confirm && run(confirm)}
        onCancel={() => setConfirm(null)}
      >
        {flavors.length > 1 && !legacy && <Content component="p">Build: <strong>{flavorName(chosenFlavor)}</strong>.</Content>}
        <Content component="p">The dashboard restarts and may be unavailable briefly. RHOAI updates do not reach the dashboard until you revert.</Content>
      </ConfirmActionModal>

      <ConfirmActionModal
        isOpen={confirm === "revert"}
        title="Revert the dashboard to the installed release?"
        changes={legacy ? [
          <>Deployment <code>{DASHBOARD_NS}/rhods-dashboard</code>: the operator takes it back and restores the release image.</>,
        ] : [
          <>Deployment <code>{DASHBOARD_NS}/dashboard-operator</code> is scaled back to its saved replica count (1 when it is unknown).</>,
          <>dashboard-operator then puts the release images back on all {containers || "the"} dashboard containers.</>,
          <>The saved session on dashboard-operator is removed{override?.startedBy ? `, ending ${sessionOwner}` : ""}.</>,
        ]}
        confirmLabel="Revert to default"
        isLoading={running === "revert"}
        onConfirm={() => run("revert")}
        onCancel={() => setConfirm(null)}
      >
        <Content component="p">The dashboard restarts and may be unavailable briefly. Reverting twice is safe.</Content>
      </ConfirmActionModal>

      <ConfirmActionModal
        isOpen={confirm === "assist"}
        title="Assist the dashboard rollout?"
        changes={[
          <>Deployment <code>{DASHBOARD_NS}/rhods-dashboard</code>: <code>spec.strategy.rollingUpdate.maxUnavailable</code> is set to 1, so one old pod stops and frees room for the new one.</>,
        ]}
        confirmLabel="Assist rollout"
        isLoading={running === "assist"}
        onConfirm={() => run("assist")}
        onCancel={() => setConfirm(null)}
      >
        <Content component="p">
          The dashboard may be unavailable briefly. The original value is saved in an annotation, and Diagnostics offers to restore it.
          The server re-checks first and changes nothing if the pod is no longer Unschedulable or an operator manages this field.
        </Content>
      </ConfirmActionModal>
    </>
  );
};
