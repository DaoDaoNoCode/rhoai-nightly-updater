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
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import { FBCContentModal } from "./FBCContentModal";
import { TooltipButton, NO_PERMISSION_REASON, OPERATION_RUNNING_REASON } from "./TooltipButton";
import { prerequisitesMet as prerequisitesReady } from "../utils";
import type { OperationResponse, StatusResponse } from "../types";
import {
  updateOperator,
  fetchLatestNightly,
  verifyNodes,
  trackFeature,
  streamUpdate,
  toApiError,
} from "../services/api";
import { describeError, describeQuayError, describeQuayText } from "../errors";
import { useOperation } from "../state/AppState";

interface UpdatePanelProps {
  status: StatusResponse | null;
  canMutate: boolean;
}

const INSTALL_PLAN: string[] = [
  "Validate FBC image reference",
  "Create namespace and OperatorGroup",
  "Create CatalogSource with nightly image",
  "Create Subscription with auto-detected channel",
  "Wait for InstallPlan",
  "Wait for CSV to reach Succeeded phase",
];

const UPGRADE_PLAN: string[] = [
  "Validate FBC image reference",
  "Create / update CatalogSource with nightly image",
  "Patch Subscription source and channel",
  "Delete current CSV to trigger OLM resolution",
  "Wait for new InstallPlan",
  "Wait for new CSV to reach Succeeded phase",
];

export const UpdatePanel: React.FC<UpdatePanelProps> = ({
  status,
  canMutate,
}) => {
  const operation = useOperation();
  const operationRunning = operation.running;
  const run = operation.run;

  // Update state
  const [image, setImage] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<{ title: string; body: string } | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Dry-run result modal state
  const [dryRunResult, setDryRunResult] = useState<OperationResponse | null>(null);
  const [dryRunModalOpen, setDryRunModalOpen] = useState(false);

  // Fetch latest state
  const [fetchingLatest, setFetchingLatest] = useState(false);
  const [fetchLatestError, setFetchLatestError] = useState<string | null>(null);

  // Auto-fill state
  const [autoFilled, setAutoFilled] = useState(false);
  const autoFillAttempted = useRef(false);

  // Ref for scrolling to error alert after modal closes
  const errorRef = useRef<HTMLDivElement>(null);

  // Node verification state
  const [verifying, setVerifying] = useState(false);
  const [verifyResult, setVerifyResult] = useState<OperationResponse | null>(null);

  const [previewOpen, setPreviewOpen] = useState(false);

  // Each ad-hoc request has its own controller; the operation stream lives in
  // the app-level store and is never aborted from here.
  const fetchLatestAbortRef = useRef<AbortController | null>(null);
  const verifyAbortRef = useRef<AbortController | null>(null);
  // The update run this panel instance started, to clear the image on success.
  const startedRunRef = useRef<number | null>(null);

  const prerequisitesMet = prerequisitesReady(status);
  const isFirstInstall = !status?.csv.phase || status.csv.phase === "Not Found";

  // --- Preflight checks ---
  const preflightChecks = useMemo(() => {
    if (!status || !image.trim()) return null;
    const operatorHealthy = status.csv.phase === "Succeeded" || status.csv.phase === "Not Found";
    const operatorHealthyLabel = operatorHealthy && status.csv.phase === "Succeeded" && status.dscExists === false
      ? "Operator healthy (no DSC yet)"
      : "Operator healthy";
    return [
      {
        label: "Pull secret configured and valid",
        passed: !!(status.pullSecret.exists && status.pullSecret.valid),
      },
      {
        label: "Image mirror set configured",
        passed: !!status.imageMirror.exists,
      },
      {
        label: operatorHealthyLabel,
        passed: operatorHealthy,
      },
      {
        label: "No concurrent operation",
        passed: !loading && !operationRunning,
      },
    ];
  }, [status, image, loading, operationRunning]);

  const allPreflightsPassed = preflightChecks
    ? preflightChecks.every((c) => c.passed)
    : false;

  const isSameImage = !!(
    status?.catalogSource.exists &&
    status.catalogSource.image &&
    image.trim() &&
    status.catalogSource.image === image.trim()
  );

  // Detect if the selected image would be a downgrade
  const isDowngrade = useMemo(() => {
    if (!status?.csv.version || !image.trim()) return false;
    // Extract tag from image (e.g., "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.1@sha256:..." -> "rhoai-3.5-ea.1")
    const tagMatch = image.trim().match(/:rhoai-(\d+\.\d+(?:\.\d+)?(?:-ea(?:\.\d+)?)?)(?=@|$)/);
    if (!tagMatch) return false;
    const selectedVersion = tagMatch[1]; // e.g., "3.5-ea.1"
    const currentVersion = status.csv.version.replace(/^v?/, "").split("+")[0]; // ignore build metadata

    // Parse both versions for comparison
    const parse = (v: string) => {
      const m = v.match(/^(\d+)\.(\d+)(?:\.(\d+))?(-ea(?:\.(\d+))?)?$/);
      if (!m) return null;
      return {
        major: +m[1],
        minor: +m[2],
        patch: +(m[3] || 0),
        ea: m[4] ? (m[5] ? +m[5] : 0) : -1,
      };
    };
    const selected = parse(selectedVersion);
    const current = parse(currentVersion);
    if (!selected || !current) return false;

    // Compare: if selected < current, it's a downgrade
    if (selected.major !== current.major) return selected.major < current.major;
    if (selected.minor !== current.minor) return selected.minor < current.minor;
    if (selected.patch !== current.patch) return selected.patch < current.patch;
    // GA (ea=-1) > any EA
    if (selected.ea === -1 && current.ea === -1) return false;
    if (selected.ea === -1) return false; // selected is GA, current is EA -> upgrade
    if (current.ea === -1) return true; // selected is EA, current is GA -> downgrade
    return selected.ea < current.ea;
  }, [status?.csv.version, image]);

  // Why Dry Run can't be used right now (null when it can).
  const dryRunDisabledReason = !canMutate ? NO_PERMISSION_REASON
    : !prerequisitesMet ? "Finish the one-time cluster setup above first (pull secret and image mirror)."
    : operationRunning ? OPERATION_RUNNING_REASON
    : !image.trim() ? "Enter an FBC image, or click Fetch latest."
    : isDowngrade ? "This image is older than the installed operator. Use Reinstall Operator to go back."
    : null;
  const installDisabledReason = dryRunDisabledReason
    ?? (!allPreflightsPassed ? `All preflight checks must pass before ${isFirstInstall ? "installing" : "updating"}.` : null);

  // Concurrent update detection: warn if someone else updated in the last 10 minutes
  const recentUpdateByOther = useMemo(() => {
    if (!status?.activity || status.activity.length === 0) return null;
    const now = Date.now();
    const tenMinMs = 10 * 60 * 1000;
    const currentUser = status.cluster.user;
    // Activity is stored oldest-first; check from newest
    for (let i = status.activity.length - 1; i >= 0; i--) {
      const entry = status.activity[i];
      if (entry.action !== "update") continue;
      const ts = new Date(entry.timestamp).getTime();
      if (now - ts > tenMinMs) break;
      if (entry.user !== currentUser) {
        const diffMin = Math.floor((now - ts) / 60_000);
        const timeAgo = diffMin < 1 ? "just now" : `${diffMin} min ago`;
        return { user: entry.user, timeAgo };
      }
    }
    return null;
  }, [status?.activity, status?.cluster.user]);

  // Scroll to error alert when an error occurs after modal has closed
  useEffect(() => {
    if (error && !confirmOpen) {
      // Use a short timeout to ensure the DOM has rendered the error alert
      const id = setTimeout(() => {
        errorRef.current?.scrollIntoView({ behavior: "smooth", block: "center" });
      }, 100);
      return () => clearTimeout(id);
    }
  }, [error, confirmOpen]);

  // Clear the image once the update this panel started has gone through.
  const runOutcome = run?.outcome;
  const runId = run?.id;
  useEffect(() => {
    if (runId !== undefined && runId === startedRunRef.current && runOutcome?.status === "succeeded") {
      startedRunRef.current = null;
      setImage("");
    }
  }, [runId, runOutcome]);

  // Auto-populate image on page load when prerequisites are met
  useEffect(() => {
    if (autoFillAttempted.current) return;
    if (!prerequisitesMet) return;
    if (image) return; // user already typed something

    autoFillAttempted.current = true;
    const controller = new AbortController();
    fetchLatestAbortRef.current = controller;

    (async () => {
      setFetchingLatest(true);
      setFetchLatestError(null);
      try {
        const res = await fetchLatestNightly(controller.signal);
        if (controller.signal.aborted) return;
        if (res.error) {
          setFetchLatestError(describeQuayText(res.error));
        } else {
          setImage(res.image);
          setAutoFilled(true);
        }
      } catch {
        // Silently fail auto-fill -- user can still manually fetch
      } finally {
        if (!controller.signal.aborted) setFetchingLatest(false);
      }
    })();
  }, [prerequisitesMet, image]);

  // Abort this panel's own requests on unmount
  useEffect(() => {
    return () => {
      fetchLatestAbortRef.current?.abort();
      verifyAbortRef.current?.abort();
    };
  }, []);

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
      if (res.error) {
        setFetchLatestError(describeQuayText(res.error));
      } else {
        setImage(res.image);
      }
    } catch (e) {
      if (controller.signal.aborted) return;
      setFetchLatestError(describeQuayError(e));
    } finally {
      if (!controller.signal.aborted) {
        setFetchingLatest(false);
      }
    }
  }, []);

  const handleVerifyNodes = useCallback(async () => {
    verifyAbortRef.current?.abort();
    const controller = new AbortController();
    verifyAbortRef.current = controller;

    setVerifying(true);
    setVerifyResult(null);
    try {
      const res = await verifyNodes(controller.signal);
      if (controller.signal.aborted) return;
      setVerifyResult(res);
    } catch (e) {
      if (controller.signal.aborted) return;
      setVerifyResult({
        success: false,
        message: toApiError(e, "Verification failed").message,
        logs: [],
      });
    } finally {
      if (!controller.signal.aborted) {
        setVerifying(false);
      }
    }
  }, []);

  const handleDryRun = async () => {
    if (!image.trim()) return;
    trackFeature("dry_run");
    setLoading(true);
    setError(null);
    setAutoFilled(false);
    try {
      setDryRunResult(null);
      const res = await updateOperator(image.trim(), true);
      setDryRunResult(res);
      setDryRunModalOpen(true);
    } catch (e) {
      // Classified by HTTP status and errorCode only (A06-9).
      const { title, body } = describeError(e, "Dry run failed");
      setError({ title, body });
    } finally {
      setLoading(false);
    }
  };

  const handleConfirmUpdate = useCallback(() => {
    const submittedImage = image.trim();
    if (!submittedImage) return;
    trackFeature("update");
    setAutoFilled(false);
    setError(null);
    setDryRunResult(null);
    setConfirmOpen(false);

    startedRunRef.current = operation.start(
      "update",
      (h) => streamUpdate(submittedImage, h.onStep, h.onDone, h.onDetach),
      submittedImage,
    );
  }, [image, operation]);

  // Result of the latest update run, shown here even after navigating away and back.
  const updateRun = run?.kind === "update" ? run : null;
  const updateOutcome = updateRun?.outcome;
  const failure = updateOutcome?.status === "failed"
    ? describeError({ name: "ApiError", status: updateOutcome.httpStatus ?? 0, errorCode: updateOutcome.errorCode ?? "operation_failed", message: updateOutcome.message }, "Update failed")
    : null;

  return (
    <Stack hasGutter>
      {recentUpdateByOther && (
        <StackItem>
          <Alert variant="warning" title="Recent update detected" isInline component="p">
            {recentUpdateByOther.user} updated the operator{" "}
            {recentUpdateByOther.timeAgo}. Check with them before making
            changes.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        {/* --- Update Section --- */}
        <Form onSubmit={(e) => e.preventDefault()}>
          <FormGroup
            label="FBC image"
            labelInfo={autoFilled ? (
              <Label isCompact color="blue">
                Auto-filled with latest nightly
              </Label>
            ) : undefined}
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
                  isDisabled={loading || operationRunning}
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
                  disabledReason={
                    !prerequisitesMet ? "Finish the one-time cluster setup above first (pull secret and image mirror)."
                    : operationRunning ? OPERATION_RUNNING_REASON
                    : null
                  }
                >
                  Fetch latest
                </TooltipButton>
              </InputGroupItem>
            </InputGroup>
            <FormHelperText>
              <HelperText id="fbc-image-help">
                <HelperTextItem>
                  Paste the image from <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer">#rhoai-build-notifications</a> or click
                  &quot;Fetch latest&quot;
                  {image.trim() && (
                    <>
                      {" "}
                      <Button variant="link" isInline onClick={() => setPreviewOpen(true)} size="sm">
                        <SearchIcon /> Preview contents
                      </Button>
                      <FBCContentModal image={image.trim()} isOpen={previewOpen} onClose={() => setPreviewOpen(false)} />
                    </>
                  )}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
            {fetchLatestError && prerequisitesMet && (
              <Alert
                variant="danger"
                title="Failed to fetch latest nightly"
                isInline
                isPlain
                isLiveRegion
                component="p"
              >
                {fetchLatestError}
              </Alert>
            )}
            {isSameImage && (
              <Alert
                variant="info"
                title="The cluster is already using this image — updating will refresh the operator with the latest images from this catalog"
                isInline
                isPlain
                component="p"
              />
            )}
            {isDowngrade && (
              <Alert
                variant="danger"
                title="Downgrade detected"
                isInline
                isPlain
                component="p"
              >
                This version is older than the currently installed{" "}
                {status?.csv.version}. OLM does not support downgrades — you can
                only update to a newer version. To go back to stable or switch
                to a different nightly, use the Reinstall Operator card below.
              </Alert>
            )}
          </FormGroup>

          {/* --- Preflight Checks --- */}
          {preflightChecks && (
            <FormGroup label="Preflight checks" fieldId="preflight-checks" role="group">
              <List isPlain id="preflight-checks">
                {preflightChecks.map((check) => (
                  <ListItem
                    key={check.label}
                    icon={
                      check.passed ? (
                        <CheckCircleIcon color="var(--pf-t--global--color--status--success--default)" aria-label="Passed" />
                      ) : (
                        <TimesCircleIcon color="var(--pf-t--global--color--status--danger--default)" aria-label="Not passed" />
                      )
                    }
                  >
                    {check.label}
                  </ListItem>
                ))}
              </List>
            </FormGroup>
          )}

          <Flex gap={{ default: "gapSm" }}>
            <FlexItem>
              <TooltipButton
                variant="secondary"
                onClick={handleDryRun}
                isDisabled={loading}
                disabledReason={dryRunDisabledReason}
                isLoading={loading}
              >
                Dry Run
              </TooltipButton>
            </FlexItem>
            <FlexItem>
              <TooltipButton
                variant="primary"
                onClick={() => setConfirmOpen(true)}
                isDisabled={loading}
                disabledReason={installDisabledReason}
              >
                {isFirstInstall ? "Install" : "Update"}
              </TooltipButton>
            </FlexItem>
          </Flex>

          {error && (
            <div ref={errorRef}>
              <Alert variant="danger" title={error.title} isInline component="p">
                {error.body}
              </Alert>
            </div>
          )}

          {failure && (
            <Alert variant={failure.variant} title={failure.title} isInline component="p">
              {failure.body}
            </Alert>
          )}

          {updateOutcome?.status === "detached" && operation.state.reconcile.active && (
            <Alert variant="warning" title="Live progress interrupted" isInline component="p">
              {updateOutcome.message} The update continues on the server; its
              progress is tracked through the operator status below.
            </Alert>
          )}

          {updateOutcome?.status === "succeeded" && (
            <Alert
              variant="success"
              title={`Update completed successfully${updateRun?.detail ? ` (image: ${updateRun.detail})` : ""}`}
              isInline
              component="p"
            />
          )}
        </Form>
      </StackItem>

      {/* --- Confirm Install/Update Modal --- */}
      <Modal
        aria-labelledby="confirm-update-title"
        variant={ModalVariant.medium}
        isOpen={confirmOpen}
        onClose={() => { if (!loading && !operationRunning) setConfirmOpen(false); }}
        onEscapePress={(event) => { if (loading || operationRunning) event.preventDefault(); }}
      >
        <ModalHeader title={isFirstInstall ? "Confirm Install" : "Confirm Update"} labelId="confirm-update-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                {isFirstInstall
                  ? "This will install the RHOAI operator on this cluster. Review the details below and confirm to proceed."
                  : "This will update the RHOAI operator on the shared cluster. Review the changes below and confirm to proceed."}
              </Content>
            </StackItem>

            {/* Current -> Target comparison */}
            <StackItem>
              <DescriptionList isHorizontal isCompact>
                {!isFirstInstall && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Current source</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status?.subscription.source || "unknown"} / {status?.subscription.channel || "unknown"}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                <DescriptionListGroup>
                  <DescriptionListTerm>{isFirstInstall ? "Catalog source" : "Target source"}</DescriptionListTerm>
                  <DescriptionListDescription>
                    rhoai-catalog-dev / auto-detected
                  </DescriptionListDescription>
                </DescriptionListGroup>
                {!isFirstInstall && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Current version</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status?.csv.version || "unknown"}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                <DescriptionListGroup>
                  <DescriptionListTerm>Image</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Content component="small">
                      <code style={{ wordBreak: "break-all" }}>{image}</code>
                    </Content>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              </DescriptionList>
            </StackItem>

            {/* Execution plan */}
            <StackItem>
              <Content component="p"><strong>Execution plan</strong></Content>
              <List isPlain>
                {(isFirstInstall ? INSTALL_PLAN : UPGRADE_PLAN).map((step, i) => (
                  <ListItem key={i}>
                    <Content component="small">{i + 1}. {step}</Content>
                  </ListItem>
                ))}
              </List>
              <Content component="small" style={{ marginTop: "0.5rem", color: "var(--pf-t--global--color--subtle)" }}>
                This typically takes 2-5 minutes.
              </Content>
            </StackItem>

            {prerequisitesMet && !verifyResult?.success && (
              <StackItem>
                <Alert
                  variant="info"
                  title="Recommended: verify node readiness before updating"
                  isInline
                  component="p"
                >
                  <Content component="small">
                    After setting up the pull secret and image mirror, it may
                    take a few minutes for credentials to propagate to all
                    cluster nodes.
                  </Content>
                  <Button
                    size="sm"
                    onClick={handleVerifyNodes}
                    isLoading={verifying}
                    isDisabled={verifying}
                  >
                    {verifying ? "Verifying..." : "Verify node readiness"}
                  </Button>
                </Alert>
              </StackItem>
            )}
            {verifyResult && (
              <StackItem>
                <Alert
                  variant={verifyResult.success ? "success" : "warning"}
                  title={verifyResult.message}
                  isInline
                  isLiveRegion
                  component="p"
                />
              </StackItem>
            )}
            <StackItem>
              <Alert
                variant="warning"
                title={isFirstInstall
                  ? "The installation typically takes 2-5 minutes. The operator will deploy into the redhat-ods-operator namespace."
                  : "The operator will be briefly unavailable (1-3 minutes) while OLM installs the new version. Existing workloads continue running."}
                isInline
                component="p"
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={handleConfirmUpdate}
            isDisabled={loading || operationRunning}
          >
            {isFirstInstall ? "Confirm Install" : "Confirm Update"}
          </Button>
          <Button
            variant="link"
            onClick={() => setConfirmOpen(false)}
            isDisabled={loading || operationRunning}
          >
            Cancel
          </Button>
        </ModalFooter>
      </Modal>

      {/* --- Dry-Run Result Modal --- */}
      <Modal
        aria-labelledby="dry-run-result-title"
        variant={ModalVariant.medium}
        isOpen={dryRunModalOpen}
        onClose={() => setDryRunModalOpen(false)}
      >
        <ModalHeader title="Dry Run Result" labelId="dry-run-result-title" />
        <ModalBody>
          {dryRunResult && (
            <Stack hasGutter>
              <StackItem>
                <Alert
                  variant={dryRunResult.success ? "success" : "danger"}
                  title={dryRunResult.message}
                  isInline
                  component="p"
                />
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
          <Button variant="primary" onClick={() => setDryRunModalOpen(false)}>
            Close
          </Button>
        </ModalFooter>
      </Modal>
    </Stack>
  );
};
