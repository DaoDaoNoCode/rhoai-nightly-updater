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
  Tooltip,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import { Link } from "react-router-dom";
import { FBCContentModal } from "./FBCContentModal";
import { prerequisitesMet as prerequisitesReady } from "../utils";
import type { OperationResponse, StatusResponse, UpdateStep } from "../types";
import {
  updateOperator,
  fetchLatestNightly,
  verifyNodes,
  trackFeature,
  streamUpdate,
} from "../services/api";

interface UpdatePanelProps {
  status: StatusResponse | null;
  onComplete: () => void;
  onStreamStart: () => void;
  onStreamStep: (step: UpdateStep) => void;
  onStreamEnd: (success: boolean) => void;
  canMutate: boolean;
}

const NO_PERMISSION_MSG = "You don't have permission to modify the operator. Contact a cluster admin.";

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
  onComplete,
  onStreamStart,
  onStreamStep,
  onStreamEnd,
  canMutate,
}) => {
  // Update state
  const [image, setImage] = useState("");
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
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

  // SSE connection drop state
  const [connectionLost, setConnectionLost] = useState(false);

  // Node verification state
  const [verifying, setVerifying] = useState(false);
  const [verifyResult, setVerifyResult] = useState<OperationResponse | null>(
    null,
  );

  // Streaming pipeline state
  const [pipelineActive, setPipelineActive] = useState(false);
  const [previewOpen, setPreviewOpen] = useState(false);
  const abortRef = useRef<AbortController | null>(null);

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
        passed: !loading && !pipelineActive,
      },
    ];
  }, [status, image, loading, pipelineActive]);

  const allPreflightsPassed = preflightChecks
    ? preflightChecks.every((c) => c.passed)
    : false;

  /** Map raw Go/network error strings to user-friendly messages. */
  const friendlyFetchError = useCallback((raw: string): string => {
    const lower = raw.toLowerCase();
    if (lower.includes("no such host") || lower.includes("dial tcp")) {
      return "Cannot reach the Quay registry. Check that the cluster has outbound network access to quay.io.";
    }
    if (lower.includes("timeout") || lower.includes("deadline exceeded") || lower.includes("context deadline")) {
      return "The request to Quay timed out. The registry may be slow or unreachable. Try again in a moment.";
    }
    if (lower.includes("401") || lower.includes("unauthorized")) {
      return "Quay authentication failed. The pull secret may be expired or invalid. Update it in the Setup panel.";
    }
    if (lower.includes("403") || lower.includes("forbidden")) {
      return "Access denied by Quay. Verify the pull secret has read access to the rhoai-fbc-fragment repository.";
    }
    if (lower.includes("429") || lower.includes("rate limit") || lower.includes("too many requests")) {
      return "Quay rate limit reached. Wait a few minutes and try again.";
    }
    if (lower.includes("tls") || lower.includes("certificate") || lower.includes("x509")) {
      return "TLS certificate error connecting to Quay. A proxy or firewall may be intercepting the connection.";
    }
    if (lower.includes("eof") || lower.includes("connection reset") || lower.includes("connection refused")) {
      return "Connection to Quay was interrupted. The registry may be temporarily unavailable. Try again.";
    }
    // Replace internal jargon with user-friendly messages
    if (lower.includes("sse connection") || lower.includes("eventsource")) {
      return "Connection to server lost. The operation is still running on the server. Refresh the page to check progress.";
    }
    if (lower.includes("sse stream") || lower.includes("streaming connection")) {
      return "Update progress tracking interrupted. The operation is still running on the server. Refresh the page to check progress.";
    }
    return raw;
  }, []);

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
    const tagMatch = image.match(/:rhoai-(\d+\.\d+(?:\.\d+)?(?:-ea\.\d+)?)/);
    if (!tagMatch) return false;
    const selectedVersion = tagMatch[1]; // e.g., "3.5-ea.1"
    const currentVersion = status.csv.version.replace(/^v?/, ""); // e.g., "3.5.0-ea.2"

    // Parse both versions for comparison
    const parse = (v: string) => {
      const m = v.match(/^(\d+)\.(\d+)(?:\.(\d+))?(?:-ea\.(\d+))?$/);
      if (!m) return null;
      return {
        major: +m[1],
        minor: +m[2],
        patch: +(m[3] || 0),
        ea: m[4] ? +m[4] : -1,
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

  const canOperate =
    prerequisitesMet && !!image.trim() && !loading && !pipelineActive && !isDowngrade;

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

  // Auto-populate image on page load when prerequisites are met
  useEffect(() => {
    if (autoFillAttempted.current) return;
    if (!prerequisitesMet) return;
    if (image) return; // user already typed something

    autoFillAttempted.current = true;

    let cancelled = false;

    (async () => {
      setFetchingLatest(true);
      setFetchLatestError(null);
      try {
        const res = await fetchLatestNightly();
        if (cancelled) return;
        if (res.error) {
          setFetchLatestError(friendlyFetchError(res.error));
        } else {
          setImage(res.image);
          setAutoFilled(true);
        }
      } catch (e) {
        // Silently fail auto-fill -- user can still manually fetch
      } finally {
        if (!cancelled) {
          setFetchingLatest(false);
        }
      }
    })();

    return () => { cancelled = true; };
  }, [prerequisitesMet, image, friendlyFetchError]);

  // Cleanup abort controller on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  const handleFetchLatest = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setFetchingLatest(true);
    setFetchLatestError(null);
    setAutoFilled(false);
    try {
      const res = await fetchLatestNightly();
      if (controller.signal.aborted) return;
      if (res.error) {
        setFetchLatestError(friendlyFetchError(res.error));
      } else {
        setImage(res.image);
      }
    } catch (e) {
      if (controller.signal.aborted) return;
      const raw = e instanceof Error ? e.message : "Failed to fetch latest nightly";
      setFetchLatestError(friendlyFetchError(raw));
    } finally {
      if (!controller.signal.aborted) {
        setFetchingLatest(false);
      }
    }
  }, [friendlyFetchError]);

  const handleVerifyNodes = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setVerifying(true);
    setVerifyResult(null);
    try {
      const res = await verifyNodes();
      if (controller.signal.aborted) return;
      setVerifyResult(res);
    } catch (e) {
      if (controller.signal.aborted) return;
      setVerifyResult({
        success: false,
        message: e instanceof Error ? e.message : "Verification failed",
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
      setResult(null);
      setDryRunResult(null);
      const submittedImage = image.trim();
      const res = await updateOperator(submittedImage, true);
      setDryRunResult(res);
      setDryRunModalOpen(true);
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      const lower = msg.toLowerCase();
      if (lower.includes("forbidden") || lower.includes("403")) {
        setError(
          "You do not have permission to update the operator. Ensure your account has RBAC access to the redhat-ods-operator namespace.",
        );
      } else if (lower.includes("unauthorized") || lower.includes("401")) {
        setError(
          "Your session has expired. Please refresh the page to re-authenticate.",
        );
      } else if (lower.includes("failed to fetch") || lower.includes("networkerror") || lower.includes("load failed")) {
        setError(
          "Connection to server lost. The operation is still running on the server. Refresh the page to check progress.",
        );
      } else {
        setError(friendlyFetchError(msg));
      }
    } finally {
      setLoading(false);
    }
  };

  const handleConfirmUpdate = useCallback(() => {
    if (!image.trim()) return;
    trackFeature("update");
    setAutoFilled(false);
    setError(null);
    setResult(null);
    setDryRunResult(null);
    setConnectionLost(false);

    // Close the modal
    setConfirmOpen(false);

    // Activate pipeline
    setPipelineActive(true);
    onStreamStart();

    const submittedImage = image.trim();

    abortRef.current?.abort();
    abortRef.current = streamUpdate(
      submittedImage,
      (step) => {
        onStreamStep(step);
      },
      (success, errorMsg) => {
        setPipelineActive(false);
        onStreamEnd(success);

        if (success) {
          setResult({
            success: true,
            message: `Update completed successfully (image: ${submittedImage})`,
            logs: [],
          });
          setImage("");
          onComplete();
        } else if (errorMsg) {
          setError(errorMsg);
        }
      },
      () => {
        // SSE connection dropped — the backend is still running.
        // Transition to reconciliation polling so the user can track progress.
        setConnectionLost(true);
        setTimeout(() => setConnectionLost(false), 8000);
        setPipelineActive(false);
        onStreamEnd(true);
        onComplete();
      },
    );
  }, [image, onComplete, onStreamStart, onStreamStep, onStreamEnd]);

  return (
    <Stack hasGutter>
      {recentUpdateByOther && (
        <StackItem>
          <Alert variant="warning" title="Recent update detected" isInline>
            {recentUpdateByOther.user} updated the operator{" "}
            {recentUpdateByOther.timeAgo}. Check with them before making
            changes.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        {/* --- Update Section --- */}
        <Form>
          <FormGroup
            label={
              <Flex gap={{ default: "gapSm" }}>
                <FlexItem>FBC Image</FlexItem>
                <FlexItem>
                  <Button
                    variant="secondary"
                    size="sm"
                    icon={<SyncAltIcon />}
                    onClick={handleFetchLatest}
                    isLoading={fetchingLatest}
                    isDisabled={fetchingLatest || !prerequisitesMet}
                  >
                    Fetch latest
                  </Button>
                </FlexItem>
                <FlexItem>
                  {autoFilled && (
                    <Label isCompact color="blue">
                      Auto-filled with latest nightly
                    </Label>
                  )}
                </FlexItem>
              </Flex>
            }
            fieldId="fbc-image"
          >
            <TextInput
              id="fbc-image"
              value={image}
              onChange={(_e, val) => {
                setImage(val);
                setAutoFilled(false);
              }}
              placeholder="quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:..."
              isDisabled={loading || pipelineActive}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Paste the image from <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer">#rhoai-build-notifications</a> or click
                  "Fetch latest"
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
              />
            )}
            {isDowngrade && (
              <Alert
                variant="danger"
                title="Downgrade detected"
                isInline
                isPlain
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
            <FormGroup label="Preflight checks" fieldId="preflight-checks">
              <List isPlain>
                {preflightChecks.map((check) => (
                  <ListItem
                    key={check.label}
                    icon={
                      check.passed ? (
                        <CheckCircleIcon color="var(--pf-t--global--color--status--success--default)" />
                      ) : (
                        <TimesCircleIcon color="var(--pf-t--global--color--status--danger--default)" />
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
              <Tooltip
                content={NO_PERMISSION_MSG}
                trigger={canMutate ? "manual" : "mouseenter focus"}
              >
                <Button
                  variant="secondary"
                  onClick={handleDryRun}
                  isDisabled={!canOperate || !canMutate}
                  isLoading={loading}
                >
                  Dry Run
                </Button>
              </Tooltip>
            </FlexItem>
            <FlexItem>
              <Tooltip
                content={
                  !allPreflightsPassed && canMutate
                    ? `All preflight checks must pass before ${isFirstInstall ? "installing" : "updating"}`
                    : NO_PERMISSION_MSG
                }
                trigger={canMutate && allPreflightsPassed ? "manual" : "mouseenter focus"}
              >
                <Button
                  variant="primary"
                  onClick={() => setConfirmOpen(true)}
                  isDisabled={!canOperate || !canMutate || !allPreflightsPassed}
                >
                  {isFirstInstall ? "Install" : "Update"}
                </Button>
              </Tooltip>
            </FlexItem>
          </Flex>

          {error && (
            <div ref={errorRef}>
              <Alert variant="danger" title="Operation failed" isInline>
                {error}
              </Alert>
            </div>
          )}

          {connectionLost && (
            <Alert
              variant="warning"
              title="SSE connection lost"
              isInline
            >
              The update is continuing on the server. Switching to status
              polling to track progress. You can monitor reconciliation
              below.
            </Alert>
          )}

          {result && (
            <Stack hasGutter>
              <StackItem>
                <Alert
                  variant={result.success ? "success" : "danger"}
                  title={result.message}
                  isInline
                />
              </StackItem>
              {result.logs.length > 0 && (
                <StackItem>
                  <CodeBlock>
                    <CodeBlockCode>{result.logs.join("\n")}</CodeBlockCode>
                  </CodeBlock>
                </StackItem>
              )}
            </Stack>
          )}
        </Form>
      </StackItem>

      {/* --- Confirm Install/Update Modal --- */}
      <Modal
        aria-labelledby="confirm-update-title"
        variant={ModalVariant.medium}
        isOpen={confirmOpen}
        onClose={() => { if (!loading && !pipelineActive) setConfirmOpen(false); }}
        onEscapePress={(event) => { if (loading || pipelineActive) event.preventDefault(); }}
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
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={handleConfirmUpdate}
            isDisabled={loading || pipelineActive}
          >
            {isFirstInstall ? "Confirm Install" : "Confirm Update"}
          </Button>
          <Button
            variant="link"
            onClick={() => setConfirmOpen(false)}
            isDisabled={loading || pipelineActive}
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
                />
              </StackItem>
              {dryRunResult.logs.length > 0 && (
                <StackItem>
                  <CodeBlock>
                    <CodeBlockCode>{dryRunResult.logs.join("\n")}</CodeBlockCode>
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
