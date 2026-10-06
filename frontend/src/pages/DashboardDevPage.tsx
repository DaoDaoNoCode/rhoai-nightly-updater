import React, { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  EmptyStateVariant,
  Flex,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  List,
  ListItem,
  PageSection,
  Radio,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";
import CubesIcon from "@patternfly/react-icons/dist/esm/icons/cubes-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
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
import { PageErrorState, PageLoading } from "../components/PageStates";
import { StatusLabel } from "../components/StatusLabel";
import { TechnicalDetails, TruncatedText } from "../components/LongText";
import { DashboardImages } from "../components/DashboardImages";
import { DashboardSessionPanel, flavorName, sessionTitle } from "../components/DashboardSessionPanel";
import { ConfirmActionModal } from "../components/ConfirmActionModal";
import { TooltipButton } from "../components/TooltipButton";
import { COMPONENTS_POLL_MS } from "../constants";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { formatRelativeTime } from "../utils";
import { useClusterBusyHandler, useDashboardOverride, useMutationBlocker } from "../state/AppInfo";

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
  const onBusy = useClusterBusyHandler();
  const { refresh: refreshOverride } = useDashboardOverride();
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
    if (running && running !== "assist") return <StatusLabel status="progress">Applying</StatusLabel>;
    if (dashState.operatorError) return <StatusLabel status="warning">Status unavailable</StatusLabel>;
    if (dashState.rolloutStuck) return <StatusLabel status="danger">Rollout stuck</StatusLabel>;
    if (dashState.rolloutPending || (dashState.operatorAvailable && !dashState.allDevImagesReady)) return <StatusLabel status="progress">Rolling out</StatusLabel>;
    if (sessionActive && override) return <StatusLabel status="warning">{sessionTitle(override)}</StatusLabel>;
    if (sessionActive) return <StatusLabel status="warning">{dashState.devMode === "main" ? "Latest main" : dashState.prNumber ? `PR #${dashState.prNumber}` : "Custom images"}</StatusLabel>;
    return <StatusLabel status="success">Installed release</StatusLabel>;
  })();

  return (
    <>
      <PageHeader
        title="Dashboard Dev"
        description="Run an odh-dashboard pull request or the latest main build in this cluster's RHOAI, then revert to the release."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={() => { setResult(null); void fetchState(); }}
      />

      {error && !notDeployed && dashState && (
        <LoadErrorAlert error={error} genericTitle="Could not load the dashboard state" onRetry={() => void fetchState()} stale />
      )}
      {error && !notDeployed && !dashState && (
        <PageErrorState error={error} title="Can't load the dashboard state" onRetry={() => void fetchState()} />
      )}
      {!dashState && !error && loading && <PageLoading title="Loading the dashboard state" />}

      {/* A paused dashboard-operator is shown (with Revert) even when the dashboard itself is gone. */}
      {override?.active && (
        <PageSection>
          <DashboardSessionPanel
            override={override}
            dashboardURL={dashState?.dashboardURL}
            revertDisabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}
            reverting={running === "revert"}
            onRevert={() => setConfirm("revert")}
          />
        </PageSection>
      )}

      {notDeployed && (
        <PageSection isFilled>
          <EmptyState headingLevel="h2" icon={CubesIcon} titleText="The RHOAI dashboard is not deployed" variant={EmptyStateVariant.lg}>
            <EmptyStateBody>
              The dashboard appears once RHOAI is installed and the DataScienceCluster has the dashboard set
              to <code>Managed</code>.
              {error?.message && <TechnicalDetails text={error.message} toggleText="Show the server's answer" />}
            </EmptyStateBody>
            <EmptyStateFooter>
              <EmptyStateActions>
                <Button variant="primary" component={(props: React.AnchorHTMLAttributes<HTMLAnchorElement>) => <Link {...props} to="/components" />}>Open Components</Button>
              </EmptyStateActions>
              <EmptyStateActions>
                <Button variant="link" component={(props: React.AnchorHTMLAttributes<HTMLAnchorElement>) => <Link {...props} to="/" />}>Install RHOAI from the Status page</Button>
              </EmptyStateActions>
            </EmptyStateFooter>
          </EmptyState>
        </PageSection>
      )}

      {!notDeployed && dashState && (
        <PageSection isFilled>
          <Stack hasGutter>
            {result && (
              <StackItem>
                <Alert variant={outcomeVariant(result)} title={outcomeTitle(result, "The action failed")} isInline isLiveRegion component="p"
                  actionClose={<AlertActionCloseButton onClose={() => setResult(null)} />}>
                  {result.success ? undefined : result.message}
                  {(result.logs?.length ?? 0) > 0 && <TechnicalDetails text={result.logs!} />}
                </Alert>
              </StackItem>
            )}

            {dashState.rolloutStuck && (
              <StackItem>
                <Alert variant="danger" title="The dashboard rollout is stuck" isInline isLiveRegion component="p">
                  <Content component="p" className="pf-v6-u-text-break-word">{dashState.stuckReason || "A dashboard Deployment reports ProgressDeadlineExceeded."}</Content>
                  <Content component="p">Its progress deadline passed, so Kubernetes stopped waiting for it. Revert to restore the release images, or deploy another build.</Content>
                </Alert>
              </StackItem>
            )}

            {!dashState.rolloutStuck && blockedDashboardImages(dashState).length > 0 && (
              <StackItem>
                <Alert variant="danger" title="A dashboard container cannot start" isInline isLiveRegion component="p">
                  <List>
                    {blockedDashboardImages(dashState).map((i) => (
                      <ListItem key={`${i.deployment}/${i.container}`}>
                        <code>{i.deployment}/{i.container}</code>: {i.waitingReason}
                        {i.waitingMessage && <span className="pf-v6-u-text-break-word"> ({i.waitingMessage})</span>}
                      </ListItem>
                    ))}
                  </List>
                  <Content component="p">This does not clear on its own. Revert to restore the release images, or deploy another build.</Content>
                </Alert>
              </StackItem>
            )}

            <StackItem>
              <Card>
                <CardHeader actions={status ? { actions: status, hasNoOffset: true } : undefined}>
                  <CardTitle><Title headingLevel="h2" size="lg">Deploy a dashboard build</Title></CardTitle>
                </CardHeader>
                <CardBody>
                  <Stack hasGutter>
                    <StackItem>
                      <Content component="p" className="pf-v6-u-text-color-subtle">
                        Replace the dashboard on this cluster with the latest main build or with the images a pull request
                        published. Every user of the cluster sees it until someone reverts.
                      </Content>
                    </StackItem>
                    {dashState.operatorError && (
                      <StackItem>
                        <Alert variant="warning" title="Cannot read dashboard-operator" isInline component="p">
                          <TruncatedText>{dashState.operatorError}</TruncatedText>
                        </Alert>
                      </StackItem>
                    )}
                    <StackItem>
                      <Form onSubmit={(e) => { e.preventDefault(); }}>
                        {flavors.length > 1 && (
                          <FormGroup role="radiogroup" fieldId="dashboard-flavor" label="Build" isStack>
                            <Radio id="flavor-rhoai" name="dashboard-flavor" label="RHOAI build (Konflux)" isChecked={chosenFlavor === "rhoai"} onChange={() => setFlavor("rhoai")}
                              description={<>RHOAI branding and docs links; tags <code>odh-pr-N</code> and <code>odh-stable</code>.</>} />
                            <Radio id="flavor-odh" name="dashboard-flavor" label="ODH build (OpenShift CI)" isChecked={chosenFlavor === "odh"} onChange={() => setFlavor("odh")}
                              description={<>Open Data Hub branding; tags <code>pr-N</code> and <code>main</code>. Fewer components have a PR build.</>} />
                          </FormGroup>
                        )}
                        <FormGroup label="Pull request number" fieldId="dashboard-pr">
                          <InputGroup>
                            <InputGroupItem>
                              <TextInput id="dashboard-pr" type="text" inputMode="numeric" value={prNumber} onChange={(_e, val) => setPrNumber(val.trim())} placeholder="e.g. 10085"
                                validated={prNumber && !validPR ? "error" : "default"} aria-describedby="dashboard-pr-help" />
                            </InputGroupItem>
                            <InputGroupItem>
                              <TooltipButton variant="control" onClick={() => setConfirm("pr")} isLoading={running === "pr"}
                                disabledReason={mutateReason(true) ?? (!validPR ? "Enter a PR number first." : null)}>
                                Deploy PR
                              </TooltipButton>
                            </InputGroupItem>
                          </InputGroup>
                          <FormHelperText>
                            <HelperText id="dashboard-pr-help">
                              <HelperTextItem variant={prNumber && !validPR ? "error" : "default"}>
                                {prNumber && !validPR ? "Enter a positive whole number." : validPR ? (
                                  <>An opendatahub-io/odh-dashboard pull request:{" "}
                                    <a href={`https://github.com/opendatahub-io/odh-dashboard/pull/${parsedPR}`} target="_blank" rel="noopener noreferrer">
                                      PR #{parsedPR} on GitHub <ExternalLinkAltIcon />
                                    </a>
                                  </>
                                ) : "An opendatahub-io/odh-dashboard pull request."}
                              </HelperTextItem>
                            </HelperText>
                          </FormHelperText>
                        </FormGroup>
                        <Flex gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }}>
                          <TooltipButton variant="secondary" onClick={() => setConfirm("main")} isLoading={running === "main"}
                            disabledReason={mutateReason(true) ?? (!dashState.operatorAvailable ? "Latest main needs dashboard-operator (RHOAI 3.5 or later)." : null)}>
                            Deploy latest main
                          </TooltipButton>
                          {sessionActive && !override?.active && (
                            <TooltipButton variant="secondary" onClick={() => setConfirm("revert")} isLoading={running === "revert"} disabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}>
                              Revert to default
                            </TooltipButton>
                          )}
                        </Flex>
                      </Form>
                    </StackItem>

                    {waitingFor && !dashState.rolloutStuck && (
                      <StackItem>
                        <Alert variant="info" isInline isPlain isLiveRegion component="p" customIcon={<Spinner size="md" aria-hidden="true" />}
                          title="Waiting for the dashboard pods to roll out">
                          This page checks every 5 seconds.
                        </Alert>
                      </StackItem>
                    )}

                    {(dashState.rolloutPending || dashState.rolloutStuck) && (dashState.schedulingFailureReason || dashState.canAssistRollout) && (
                      <StackItem>
                        <Alert variant="warning" title="A new dashboard pod cannot be scheduled" isInline component="p"
                          actionLinks={dashState.canAssistRollout ? (
                            // Available as soon as the server offers it: the rollout it
                            // rescues is the one this page is waiting for.
                            <TooltipButton variant="link" isInline onClick={() => setConfirm("assist")} isLoading={running === "assist"}
                              disabledReason={blocker ?? (running ? "Another Dashboard Dev action is running." : null)}>
                              Assist rollout
                            </TooltipButton>
                          ) : undefined}>
                          {dashState.schedulingFailureReason && <TruncatedText>{dashState.schedulingFailureReason}</TruncatedText>}
                        </Alert>
                      </StackItem>
                    )}

                    {legacy && (
                      <StackItem>
                        <HelperText>
                          <HelperTextItem>This cluster has no dashboard-operator (RHOAI 3.4 or earlier): only <code>rhods-dashboard</code> is changed.</HelperTextItem>
                        </HelperText>
                      </StackItem>
                    )}
                  </Stack>
                </CardBody>
              </Card>
            </StackItem>

            {(dashState.devImages?.length ?? 0) > 0 && (
              <StackItem><DashboardImages images={dashState.devImages!} defaultExpanded={sessionActive} /></StackItem>
            )}
          </Stack>
        </PageSection>
      )}

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
