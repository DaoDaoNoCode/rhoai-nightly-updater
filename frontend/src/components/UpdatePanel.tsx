import React, {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  Alert,
  Button,
  CodeBlock,
  CodeBlockCode,
  Content,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  Flex,
  FlexItem,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  Stack,
  StackItem,
  TextInput,
  Form,
  Label,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import InfoCircleIcon from "@patternfly/react-icons/dist/esm/icons/info-circle-icon";
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import { FBCContentModal } from "./FBCContentModal";
import { TooltipButton } from "./TooltipButton";
import { BuildSummary } from "./BuildSummary";
import { REVERT_DASHBOARD_EXPLANATION, describeDashboardSession } from "./OperatorActions";
import { prerequisitesMet as prerequisitesReady } from "../utils";
import type { LatestNightlyResponse, NightlyBuild, OperationResponse, StatusResponse } from "../types";
import {
  updateOperator,
  fetchLatestNightly,
  verifyNodes,
  trackFeature,
  toApiError,
  type OperatorOperationOptions,
} from "../services/api";
import { describeError, describeQuayError, describeQuayText } from "../errors";
import { useOperation } from "../state/AppState";
import { useDashboardOverride, useMutationBlocker } from "../state/AppInfo";
import { STEP_SETS } from "../operationSteps";
import { buildFromImage, compareTagToInstalled, parseImageRef } from "../build";

// ---------------------------------------------------------------------------
// Shared confirmation dialog (hero "Update to latest" and the form below)
// ---------------------------------------------------------------------------

/** What the backend does for an Update, in its order (pkg/cluster UpdateStreamWithOptions). */
const UPDATE_PLAN = STEP_SETS.update;
const REMOVE_STEP_NUMBER = UPDATE_PLAN.findIndex((s) => s.id === "delete_csv") + 1;

interface UpdateConfirmModalProps {
  /** The build to install; null closes the dialog. */
  target: NightlyBuild | null;
  status: StatusResponse | null;
  onConfirm: (options: OperatorOperationOptions) => void;
  onClose: () => void;
}

function installedDescription(status: StatusResponse | null): React.ReactNode {
  if (!status || status.csv.phase === "Not Found" || !status.csv.name) return "Not installed";
  if (status.nightly?.installed) return <BuildSummary build={status.nightly.installed} />;
  const source = status.subscription.source ? ` from ${status.subscription.source} / ${status.subscription.channel}` : "";
  return `${status.csv.name}${source}`;
}

/**
 * Confirmation for Update / Install. States what will run (the same step
 * list the progress view shows), what is kept, the recovery behaviour, and
 * any condition that changes the outcome (Failed operator, Dashboard Dev).
 */
export const UpdateConfirmModal: React.FC<UpdateConfirmModalProps> = ({ target, status, onConfirm, onClose }) => {
  const { override } = useDashboardOverride();
  const blocker = useMutationBlocker();
  const [verifying, setVerifying] = useState(false);
  const [verifyResult, setVerifyResult] = useState<OperationResponse | null>(null);
  const verifyAbort = useRef<AbortController | null>(null);
  useEffect(() => () => verifyAbort.current?.abort(), []);

  const isFirstInstall = !status?.csv.phase || status.csv.phase === "Not Found";
  const csvFailed = status?.csv.phase === "Failed";
  const sameAsInstalled = !!(target && status?.catalogSource.image && status.catalogSource.image === target.image);
  const dashboardActive = !!override?.active;

  const handleVerifyNodes = async () => {
    verifyAbort.current?.abort();
    const controller = new AbortController();
    verifyAbort.current = controller;
    setVerifying(true);
    setVerifyResult(null);
    try {
      const res = await verifyNodes(controller.signal);
      if (!controller.signal.aborted) setVerifyResult(res);
    } catch (e) {
      if (!controller.signal.aborted) setVerifyResult({ success: false, message: toApiError(e, "Verification failed").message, logs: [] });
    } finally {
      if (!controller.signal.aborted) setVerifying(false);
    }
  };

  const title = isFirstInstall ? "Install this RHOAI nightly?" : "Update RHOAI to this build?";
  const confirmLabel = dashboardActive
    ? `Revert Dashboard Dev and ${isFirstInstall ? "install" : "update"}`
    : isFirstInstall ? "Install" : "Update";

  return (
    <Modal
      aria-labelledby="confirm-update-title"
      variant={ModalVariant.medium}
      isOpen={!!target}
      onClose={onClose}
    >
      <ModalHeader title={title} labelId="confirm-update-title" />
      <ModalBody>
        {target && (
          <Stack hasGutter>
            <StackItem>
              <DescriptionList isHorizontal isCompact horizontalTermWidthModifier={{ default: "10ch" }}>
                <DescriptionListGroup>
                  <DescriptionListTerm>Installed</DescriptionListTerm>
                  <DescriptionListDescription>{installedDescription(status)}</DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>Target</DescriptionListTerm>
                  <DescriptionListDescription><BuildSummary build={target} /></DescriptionListDescription>
                </DescriptionListGroup>
                {!isFirstInstall && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Operator</DescriptionListTerm>
                    <DescriptionListDescription>{status?.csv.name} ({status?.csv.phase})</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
              </DescriptionList>
            </StackItem>

            {sameAsInstalled && (
              <StackItem>
                <Alert variant="info" isInline isPlain component="p" title="This is the build that is already installed.">
                  The update installs it again from a fresh catalog. To re-deploy without changing the catalog, use Re-deploy operator.
                </Alert>
              </StackItem>
            )}
            {csvFailed && (
              <StackItem>
                <Alert variant="info" isInline isPlain component="p" title="The installed operator is Failed.">
                  Updating removes the failed version and installs this build, which is the usual fix for a broken nightly.
                </Alert>
              </StackItem>
            )}
            {dashboardActive && (
              <StackItem>
                <Alert variant="warning" isInline component="p" title="A Dashboard Dev session is active">
                  dashboard-operator is paused ({describeDashboardSession(override)}). The update ends that session first, or the
                  dashboard would stay on its old images. {REVERT_DASHBOARD_EXPLANATION}
                </Alert>
              </StackItem>
            )}

            <StackItem>
              <Content component="p"><strong>What happens</strong></Content>
              <List component="ol">
                {UPDATE_PLAN.map((step) => (
                  <ListItem key={step.id}>
                    {step.label}
                    <span className="rhoai-subtle">: {step.description}</span>
                  </ListItem>
                ))}
              </List>
            </StackItem>
            <StackItem>
              <Content component="p">
                It usually takes a few minutes. The operator keeps running until step {REMOVE_STEP_NUMBER}; your notebooks, model servers and
                pipelines keep running throughout. If OLM reports a failure, the previous catalog and Subscription are
                restored automatically. If OLM is still installing after 8 minutes, the new state is kept and the tool
                keeps watching the operator status.
              </Content>
            </StackItem>
            {status && prerequisitesReady(status) && (
              <StackItem>
                <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                  <FlexItem><Content component="small">Just set up the pull secret or image mirror?</Content></FlexItem>
                  <FlexItem>
                    <Button variant="link" isInline onClick={handleVerifyNodes} isLoading={verifying} isDisabled={verifying}>
                      Check that the nodes can pull nightly images
                    </Button>
                  </FlexItem>
                </Flex>
                {verifyResult && (
                  <Alert
                    variant={verifyResult.success ? "success" : "warning"}
                    title={verifyResult.message}
                    isInline
                    isPlain
                    isLiveRegion
                    component="p"
                  />
                )}
              </StackItem>
            )}
          </Stack>
        )}
      </ModalBody>
      <ModalFooter>
        <TooltipButton
          variant="primary"
          onClick={() => onConfirm(dashboardActive ? { revertDashboardDev: true } : {})}
          disabledReason={blocker}
        >
          {confirmLabel}
        </TooltipButton>
        <Button variant="link" onClick={onClose}>Cancel</Button>
      </ModalFooter>
    </Modal>
  );
};

// ---------------------------------------------------------------------------
// "Update to a specific build" form
// ---------------------------------------------------------------------------

interface UpdatePanelProps {
  status: StatusResponse | null;
  /** The newest nightly overall (GET /api/latest-nightly), loaded by the page. */
  latest: LatestNightlyResponse | null;
  /** Open the Update confirmation for this build. */
  onRequestUpdate: (target: NightlyBuild) => void;
}

interface Preflight {
  label: string;
  state: "pass" | "fail" | "warn" | "info";
}

const PREFLIGHT_ICONS: Record<Preflight["state"], React.ReactNode> = {
  pass: <CheckCircleIcon color="var(--pf-t--global--icon--color--status--success--default)" aria-label="Ready" />,
  fail: <TimesCircleIcon color="var(--pf-t--global--icon--color--status--danger--default)" aria-label="Blocking" />,
  warn: <ExclamationTriangleIcon color="var(--pf-t--global--icon--color--status--warning--default)" aria-label="Warning" />,
  info: <InfoCircleIcon color="var(--pf-t--global--icon--color--status--info--default)" aria-label="Note" />,
};

export const UpdatePanel: React.FC<UpdatePanelProps> = ({ status, latest, onRequestUpdate }) => {
  const operation = useOperation();
  const blocker = useMutationBlocker();
  const { override } = useDashboardOverride();

  const [image, setImage] = useState("");
  const [dryRunning, setDryRunning] = useState(false);
  const [error, setError] = useState<{ title: string; body: string } | null>(null);
  const [dryRunResult, setDryRunResult] = useState<OperationResponse | null>(null);
  const [dryRunModalOpen, setDryRunModalOpen] = useState(false);
  const [fetchingLatest, setFetchingLatest] = useState(false);
  const [fetchLatestError, setFetchLatestError] = useState<string | null>(null);
  const [autoFilled, setAutoFilled] = useState(false);
  const [previewOpen, setPreviewOpen] = useState(false);
  const fetchLatestAbortRef = useRef<AbortController | null>(null);
  useEffect(() => () => fetchLatestAbortRef.current?.abort(), []);

  const prerequisitesMet = prerequisitesReady(status);
  const isFirstInstall = !status?.csv.phase || status.csv.phase === "Not Found";
  const trimmed = image.trim();
  const ref = trimmed ? parseImageRef(trimmed) : null;

  // Prefill with the newest nightly once it is known (the user can replace it).
  const autoFillDone = useRef(false);
  useEffect(() => {
    if (autoFillDone.current || !latest?.image || image) return;
    autoFillDone.current = true;
    setImage(latest.image);
    setAutoFilled(true);
  }, [latest, image]);

  // A build of an older release line is always refused by the backend
  // (downgradeCheck); same-line builds are checked there from the bundle.
  const olderLine = !!ref?.tag && compareTagToInstalled(ref.tag, status?.csv.version) === "older";

  const preflights = useMemo<Preflight[]>(() => {
    if (!status) return [];
    const items: Preflight[] = [
      {
        label: status.pullSecret.exists && status.pullSecret.valid
          ? "Pull secret can read quay.io/rhoai"
          : status.pullSecret.exists
            ? `Pull secret is invalid${status.pullSecret.detail ? `: ${status.pullSecret.detail}` : ""}`
            : "Pull secret is missing (see Cluster setup)",
        state: status.pullSecret.exists && status.pullSecret.valid ? "pass" : "fail",
      },
      {
        label: status.imageMirror.exists ? "Image mirror is configured" : "Image mirror is missing (see Cluster setup)",
        state: status.imageMirror.exists ? "pass" : "fail",
      },
    ];
    const phase = status.csv.phase;
    if (phase === "Failed") items.push({ label: "The operator is Failed; updating to a newer build is the usual fix", state: "warn" });
    else if (phase && phase !== "Succeeded" && phase !== "Not Found") items.push({ label: `The operator is ${phase}; updating replaces that install`, state: "warn" });
    else if (phase === "Succeeded") items.push({ label: `Operator ${status.csv.version || status.csv.name} is healthy`, state: "pass" });
    if (override?.active) items.push({ label: "A Dashboard Dev session is active; the update will offer to revert it", state: "info" });
    // The backend's view (any tab, any user), not only this tab's state.
    const busy = operation.server.inProgress || operation.running || operation.state.reconcile.active;
    items.push(busy && blocker ? { label: blocker, state: "fail" } : { label: "No other operation is running", state: "pass" });
    return items;
  }, [status, override, blocker, operation.server.inProgress, operation.running, operation.state.reconcile.active]);

  const disabledReason = blocker
    ?? (!prerequisitesMet ? "Finish the one-time cluster setup first (pull secret and image mirror)." : null)
    ?? (!trimmed ? "Enter an FBC image, or click Fetch latest." : null)
    ?? (olderLine ? `This build (${ref?.tag}) is from an older release line than the installed ${status?.csv.version}. Update can't go back; use Reinstall.` : null);

  // Warn when someone else changed the operator in the last 10 minutes.
  const recentUpdateByOther = useMemo(() => {
    if (!status?.activity?.length) return null;
    const now = Date.now();
    for (let i = status.activity.length - 1; i >= 0; i--) {
      const entry = status.activity[i];
      if ((entry.category ?? (entry.action === "update" ? "operator" : "")) !== "operator") continue;
      const ts = new Date(entry.timestamp).getTime();
      if (now - ts > 10 * 60_000) break;
      if (entry.user !== status.cluster.user) {
        const diffMin = Math.floor((now - ts) / 60_000);
        return { user: entry.user, label: (entry.label || entry.action).toLowerCase(), timeAgo: diffMin < 1 ? "just now" : `${diffMin} min ago` };
      }
    }
    return null;
  }, [status?.activity, status?.cluster.user]);

  const handleFetchLatest = useCallback(async () => {
    fetchLatestAbortRef.current?.abort();
    const controller = new AbortController();
    fetchLatestAbortRef.current = controller;
    setFetchingLatest(true);
    setFetchLatestError(null);
    setAutoFilled(false);
    try {
      const res = await fetchLatestNightly(controller.signal);
      if (controller.signal.aborted) return;
      if (res.error) setFetchLatestError(describeQuayText(res.error));
      else setImage(res.image);
    } catch (e) {
      if (!controller.signal.aborted) setFetchLatestError(describeQuayError(e));
    } finally {
      if (!controller.signal.aborted) setFetchingLatest(false);
    }
  }, []);

  const handleDryRun = async () => {
    if (!trimmed) return;
    trackFeature("dry_run");
    setDryRunning(true);
    setError(null);
    setDryRunResult(null);
    try {
      const res = await updateOperator(trimmed, true);
      setDryRunResult(res);
      setDryRunModalOpen(true);
    } catch (e) {
      const { title, body } = describeError(e, "Dry run failed");
      setError({ title, body });
    } finally {
      setDryRunning(false);
    }
  };

  const requestUpdate = () => {
    if (!trimmed) return;
    // Use the richer metadata the page already has for the same image.
    const known = [status?.nightly?.latest, status?.nightly?.installed, latest ? { ...buildFromImage(latest.image), buildDate: latest.buildDate } : undefined]
      .find((b) => b?.image === trimmed);
    onRequestUpdate(known ?? buildFromImage(trimmed));
  };


  return (
    <Stack hasGutter>
      {recentUpdateByOther && (
        <StackItem>
          <Alert variant="warning" title={`${recentUpdateByOther.user} changed the operator ${recentUpdateByOther.timeAgo}`} isInline component="p">
            Last action: {recentUpdateByOther.label}. Check with them before changing it again.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Form onSubmit={(e) => e.preventDefault()}>
          <FormGroup
            label="FBC image"
            labelInfo={autoFilled ? <Label isCompact color="blue">Newest nightly</Label> : undefined}
            fieldId="fbc-image"
          >
            <InputGroup>
              <InputGroupItem isFill>
                <TextInput
                  id="fbc-image"
                  value={image}
                  onChange={(_e, val) => {
                    setImage(val);
                    setAutoFilled(false);
                  }}
                  placeholder="quay.io/rhoai/rhoai-fbc-fragment:<tag>@sha256:<digest>"
                  aria-describedby="fbc-image-help"
                />
              </InputGroupItem>
              <InputGroupItem>
                <TooltipButton
                  variant="control"
                  icon={<SyncAltIcon />}
                  onClick={handleFetchLatest}
                  isLoading={fetchingLatest}
                  isDisabled={fetchingLatest}
                  disabledReason={!prerequisitesMet ? "Finish the one-time cluster setup first (pull secret and image mirror)." : null}
                >
                  Fetch latest
                </TooltipButton>
              </InputGroupItem>
            </InputGroup>
            <FormHelperText>
              <HelperText id="fbc-image-help">
                <HelperTextItem>
                  Paste a build from <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer">#rhoai-build-notifications</a>,
                  pick one in Build Explorer, or click Fetch latest.
                  {trimmed && (
                    <>
                      {" "}
                      <Button variant="link" isInline onClick={() => setPreviewOpen(true)} icon={<SearchIcon />}>
                        What&apos;s in this build
                      </Button>
                    </>
                  )}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
            {trimmed && <FBCContentModal image={trimmed} isOpen={previewOpen} onClose={() => setPreviewOpen(false)} />}
            {fetchLatestError && prerequisitesMet && (
              <Alert variant="danger" title="Could not fetch the latest nightly" isInline isPlain isLiveRegion component="p">
                {fetchLatestError}
              </Alert>
            )}
          </FormGroup>

          {preflights.length > 0 && (
            <FormGroup label="Before you update" fieldId="preflight-checks" role="group">
              <List isPlain id="preflight-checks">
                {preflights.map((check) => (
                  <ListItem key={check.label} icon={PREFLIGHT_ICONS[check.state]}>{check.label}</ListItem>
                ))}
              </List>
            </FormGroup>
          )}

          <Flex gap={{ default: "gapSm" }}>
            <FlexItem>
              <TooltipButton
                variant="primary"
                onClick={requestUpdate}
                disabledReason={disabledReason}
              >
                {isFirstInstall ? "Install this build" : "Update to this build"}
              </TooltipButton>
            </FlexItem>
            <FlexItem>
              <TooltipButton
                variant="secondary"
                onClick={handleDryRun}
                isLoading={dryRunning}
                isDisabled={dryRunning}
                disabledReason={disabledReason}
              >
                Dry run
              </TooltipButton>
            </FlexItem>
          </Flex>

          {error && (
            <Alert variant="danger" title={error.title} isInline isLiveRegion component="p">
              {error.body}
            </Alert>
          )}
        </Form>
      </StackItem>

      <Modal
        aria-labelledby="dry-run-result-title"
        variant={ModalVariant.medium}
        isOpen={dryRunModalOpen}
        onClose={() => setDryRunModalOpen(false)}
      >
        <ModalHeader title="Dry run result" labelId="dry-run-result-title" />
        <ModalBody>
          {dryRunResult && (
            <Stack hasGutter>
              <StackItem>
                <Alert variant={dryRunResult.success ? "success" : "danger"} title={dryRunResult.message} isInline component="p" />
              </StackItem>
              {(dryRunResult.logs?.length ?? 0) > 0 && (
                <StackItem>
                  <CodeBlock>
                    <CodeBlockCode>{(dryRunResult.logs ?? []).join("\n")}</CodeBlockCode>
                  </CodeBlock>
                </StackItem>
              )}
            </Stack>
          )}
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={() => setDryRunModalOpen(false)}>Close</Button>
        </ModalFooter>
      </Modal>
    </Stack>
  );
};
