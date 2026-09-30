import React, { useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  CodeBlock,
  CodeBlockCode,
  Content,
  Flex,
  FlexItem,
  FormGroup,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  MenuToggle,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  Radio,
  Select,
  SelectList,
  SelectOption,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Form,
  Tooltip,
} from "@patternfly/react-core";
import type { NightlyTag, OperationResponse, StatusResponse, UpdateStep } from "../types";
import { streamReinstall, trackFeature } from "../services/api";

interface ReinstallPanelProps {
  status: StatusResponse | null;
  onComplete: () => void;
  onStreamStart: (reinstallType?: string) => void;
  onStreamStep: (step: UpdateStep) => void;
  onStreamEnd: (success: boolean) => void;
  nightlyTags: NightlyTag[];
  tagsLoading: boolean;
  canMutate: boolean;
  prerequisitesMet: boolean;
}

export const ReinstallPanel: React.FC<ReinstallPanelProps> = ({
  status,
  onComplete,
  onStreamStart,
  onStreamStep,
  onStreamEnd,
  nightlyTags,
  tagsLoading,
  canMutate,
  prerequisitesMet,
}) => {
  // Target mode
  const [targetType, setTargetType] = useState<"stable" | "nightly" | "custom">("stable");

  // Nightly version selection
  const [tagSelectOpen, setTagSelectOpen] = useState(false);
  const [selectedImage, setSelectedImage] = useState("");
  const [customImage, setCustomImage] = useState("");
  const [channelOverride, setChannelOverride] = useState("");

  // Confirmation modal state
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [confirmText, setConfirmText] = useState("");

  // Operation state
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<OperationResponse | null>(null);

  // Tags come from props (shared with StatusPage)
  const tags = nightlyTags;

  // SSE abort controller
  const abortRef = useRef<AbortController | null>(null);

  // Abort in-flight requests on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  const channelValid = !channelOverride.trim() || /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(channelOverride.trim());
  const targetImage = targetType === "custom" ? customImage.trim() : selectedImage.trim();
  const customImageValid = /^quay\.io\/[a-zA-Z0-9._-]+(?:\/[a-zA-Z0-9._-]+)+(?::[a-zA-Z0-9._-]+)?(?:@sha256:[a-f0-9]{64})?$/.test(customImage.trim()) && /(?::[^/]+|@sha256:[a-f0-9]{64})$/.test(customImage.trim());
  const canConfirm =
    channelValid && (
      (targetType === "stable" && !!status?.stableChannel && !status?.stableDiscoveryError) ||
      (!!targetImage && (targetType !== "custom" || customImageValid) && prerequisitesMet)
    );

  const handleReinstall = () => {
    let completion: UpdateStep | undefined;
    trackFeature(targetType === "stable" ? "reinstall_stable" : targetType === "custom" ? "reinstall_custom" : "reinstall_nightly");
    setError(null);
    setResult(null);

    // Close modal immediately
    setConfirmOpen(false);
    setConfirmText("");

    // Start streaming
    setLoading(true);
    onStreamStart(targetType !== "stable" ? "reinstall_nightly" : "reinstall_stable");

    abortRef.current?.abort();
    abortRef.current = streamReinstall(
      targetType,
      targetType !== "stable" ? targetImage : undefined,
      targetType !== "stable" && channelOverride.trim() ? channelOverride.trim() : undefined,
      (step) => {
        if (step.step === "operation_complete") completion = step;
        onStreamStep(step);
      },
      (success, errorMsg) => {
        setLoading(false);
        onStreamEnd(success);
        if (success) {
          setResult({
            success: true,
            message: completion?.message || `Reinstall to ${targetType} initiated successfully`,
            logs: [],
          });
          onComplete();
        } else if (errorMsg) {
          setError(errorMsg);
        }
      },
      () => {
        // SSE connection dropped -- the backend is still running.
        setLoading(false);
        onStreamEnd(false);
        onComplete();
      },
    );
  };

  const handleTagSelect = (
    _event?: React.MouseEvent<Element, MouseEvent>,
    value?: string | number,
  ) => {
    if (typeof value === "string") {
      setSelectedImage(value);
    }
    setTagSelectOpen(false);
  };

  // Build the toggle for the Select dropdown
  const toggleRef = React.useRef<HTMLButtonElement>(null);
  const toggle = (
    <MenuToggle
      ref={toggleRef}
      onClick={() => setTagSelectOpen(!tagSelectOpen)}
      isExpanded={tagSelectOpen}
      isDisabled={loading || targetType !== "nightly"}
      variant="secondary"
      style={{ minWidth: "180px" }}
    >
      {tagsLoading ? (
        <>
          <Spinner size="sm" /> Loading tags...
        </>
      ) : selectedImage ? (
        tags.find((t) => t.image === selectedImage)?.tag || "Selected"
      ) : (
        "Select version"
      )}
    </MenuToggle>
  );

  const targetDescription =
    targetType === "stable"
      ? `${status?.stableSource} / ${status?.stableChannel}${status?.stableVersion ? ` (GA ${status.stableVersion})` : ""}`
      : targetImage
        ? targetImage +
          (channelOverride.trim() ? ` (channel: ${channelOverride.trim()})` : " (channel: auto-detect)")
        : "No FBC image selected";

  return (
    <Stack hasGutter>
      <StackItem>
        <Alert variant="warning" title="Destructive operation" isInline>
          <Content component="small">
            This will fully uninstall the current RHOAI operator and reinstall
            it from the selected target. The operator will be unavailable for
            5-10 minutes during this process.
          </Content>
        </Alert>
      </StackItem>

      <StackItem>
        <Stack hasGutter>
          <StackItem>
            <Radio
              id="reinstall-stable"
              name="reinstall-target"
              label={status?.stableChannelPinned ? "Configured GA channel" : "Latest stable"}
              description={
                <HelperText>
                  <HelperTextItem>
                    {status?.stableChannel ? (
                      <>
                        Reinstall from <strong>{status.stableSource}</strong> /{" "}
                        <strong>{status.stableChannel}</strong>
                        {status.stableVersion && <> — GA {status.stableVersion}</>}
                        {!status.stableChannelPinned && <>. Latest GA available in this cluster's catalog.</>}
                      </>
                    ) : status?.stableDiscoveryError ? (
                      "Stable release unavailable"
                    ) : (
                      "Discovering stable releases from the cluster catalog..."
                    )}
                  </HelperTextItem>
                </HelperText>
              }
              isChecked={targetType === "stable"}
              onChange={() => {
                setTargetType("stable");
                setSelectedImage("");
                setChannelOverride("");
              }}
              isDisabled={loading}
            />
            {targetType === "stable" && status?.stableDiscoveryError && (
              <Alert variant="warning" title="Could not discover the latest stable release" isInline style={{ marginTop: "0.5rem" }}>
                <Content component="small">{status.stableDiscoveryError}. Refresh cluster status after resolving the catalog issue.</Content>
              </Alert>
            )}
          </StackItem>
          <StackItem>
            <Radio
              id="reinstall-nightly"
              name="reinstall-target"
              label="Specific nightly"
              description={
                <HelperText>
                  <HelperTextItem>
                    Reinstall with a specific nightly FBC image
                  </HelperTextItem>
                </HelperText>
              }
              isChecked={targetType === "nightly"}
              onChange={() => setTargetType("nightly")}
              isDisabled={loading}
            />
          </StackItem>

          <StackItem>
            <Radio
              id="reinstall-custom"
              name="reinstall-target"
              label="Custom version"
              description="Reinstall any version or build using a Quay FBC image. Include a SHA256 digest to pin an exact build."
              isChecked={targetType === "custom"}
              onChange={() => setTargetType("custom")}
              isDisabled={loading}
            />
          </StackItem>

          {targetType !== "stable" && (
            <StackItem>
              {!prerequisitesMet && (
                <Alert
                  variant="warning"
                  title="Prerequisites required for nightly installs"
                  isInline
                  style={{ marginBottom: "1rem" }}
                >
                  <Content component="small">
                    The pull secret and image mirror must be configured before
                    reinstalling with a nightly build. Use the Setup panel to
                    complete prerequisite configuration.
                  </Content>
                </Alert>
              )}
              <Form>
                {targetType === "custom" ? (
                  <FormGroup label="Custom FBC image" fieldId="reinstall-custom-image" isRequired>
                    <TextInput
                      id="reinstall-custom-image"
                      value={customImage}
                      onChange={(_e, value) => setCustomImage(value)}
                      placeholder="quay.io/rhoai/rhoai-fbc-fragment:<tag>@sha256:<digest>"
                      isDisabled={loading}
                      validated={customImage.trim() && !customImageValid ? "error" : "default"}
                      aria-describedby="reinstall-custom-image-help"
                    />
                    <HelperText id="reinstall-custom-image-help">
                      <HelperTextItem variant={customImage.trim() && !customImageValid ? "error" : "default"}>
                        {customImage.trim() && !customImageValid
                          ? "Enter a Quay image reference with a tag or a full 64-character SHA256 digest."
                          : "The supplied digest is used exactly, even when its tag now points to a newer build. Any version or build can be selected."}
                      </HelperTextItem>
                    </HelperText>
                  </FormGroup>
                ) : (
                <FormGroup
                  label="Nightly version"
                  fieldId="reinstall-nightly-image"
                >
                  <Flex
                    gap={{ default: "gapSm" }}
                    alignItems={{ default: "alignItemsCenter" }}
                  >
                    <FlexItem>
                      <Select
                        isOpen={tagSelectOpen}
                        selected={selectedImage || undefined}
                        onSelect={handleTagSelect}
                        onOpenChange={(open) => setTagSelectOpen(open)}
                        toggle={{
                          toggleNode: toggle,
                          toggleRef: toggleRef,
                        }}
                        shouldFocusToggleOnSelect
                      >
                        <SelectList aria-label="Nightly version">
                          {!tagsLoading && tags.length === 0 && (
                            <SelectOption value="" isDisabled>
                              No tags available
                            </SelectOption>
                          )}
                          {tags.length > 0 &&
                            tags.map((t) => (
                              <SelectOption
                                key={t.tag}
                                value={t.image}
                                description={
                                  t.image.length > 60
                                    ? t.image.slice(0, 60) + "..."
                                    : t.image
                                }
                              >
                                {t.tag}
                              </SelectOption>
                            ))}
                        </SelectList>
                      </Select>
                    </FlexItem>
                  </Flex>
                </FormGroup>
                )}
                <FormGroup
                  label="Channel override"
                  fieldId="reinstall-channel-override"
                >
                  <TextInput
                    id="reinstall-channel-override"
                    value={channelOverride}
                    onChange={(_e, val) => setChannelOverride(val)}
                    placeholder="Leave empty to detect a channel from the selected catalog"
                    isDisabled={loading}
                    validated={channelValid ? "default" : "error"}
                  />
                  <HelperText>
                    <HelperTextItem>
                      Optional. Override the auto-detected channel if the update picks the wrong one.
                    </HelperTextItem>
                  </HelperText>
                </FormGroup>
              </Form>
            </StackItem>
          )}
        </Stack>
      </StackItem>

      <StackItem>
        <Tooltip
          content="You don't have permission to modify the operator. Contact a cluster admin."
          trigger={canMutate ? "manual" : "mouseenter focus"}
        >
          <Button
            variant="danger"
            onClick={() => setConfirmOpen(true)}
            isDisabled={!canConfirm || loading || !canMutate}
            isLoading={loading}
          >
            Reinstall Operator
          </Button>
        </Tooltip>
      </StackItem>

      {error && (
        <StackItem>
          <Alert variant="danger" title="Reinstall failed" isInline>
            {error}
          </Alert>
        </StackItem>
      )}

      {result && (
        <StackItem>
          <Stack hasGutter>
            <StackItem>
              <Alert
                variant={
                  result.success && result.message.includes("Already on stable") && status?.csv.phase !== "Succeeded"
                    ? "warning"
                    : result.success ? "success" : "danger"
                }
                title={result.message}
                isInline
              >
                {result.success && result.message.includes("Already on stable") && status?.csv.phase !== "Succeeded" && (
                  "The operator is on the stable channel but is not in a healthy state. Consider using Refresh Operator to re-deploy it."
                )}
              </Alert>
            </StackItem>
            {result.success && targetType === "stable" && (
              <StackItem>
                <Alert
                  variant="info"
                  title="The nightly CatalogSource has been cleaned up"
                  isInline
                  isPlain
                />
              </StackItem>
            )}
            {result.logs.length > 0 && (
              <StackItem>
                <CodeBlock>
                  <CodeBlockCode>{result.logs.join("\n")}</CodeBlockCode>
                </CodeBlock>
              </StackItem>
            )}
          </Stack>
        </StackItem>
      )}

      {/* --- Confirm Reinstall Modal --- */}
      <Modal
        aria-labelledby="confirm-reinstall-title"
        variant={ModalVariant.medium}
        isOpen={confirmOpen}
        onClose={() => {
          setConfirmOpen(false);
          setConfirmText("");
        }}
      >
        <ModalHeader
          title="Reinstall Operator — Destructive Operation"
          titleIconVariant="warning"
          labelId="confirm-reinstall-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Alert
                variant="danger"
                title="This will fully uninstall the current RHOAI operator and reinstall it. This is a destructive operation."
                isInline
              />
            </StackItem>
            <StackItem>
              <Content component="p">
                <strong>Current:</strong> {status?.subscription.source} /{" "}
                {status?.subscription.channel}
              </Content>
              <Content component="p">
                <strong>Target:</strong> <span style={{ overflowWrap: "anywhere" }}>{targetDescription}</span>
              </Content>
            </StackItem>
            <StackItem>
              <Content component="p">
                The following steps will be performed:
              </Content>
              <List isPlain={false} component="ol">
                {targetType !== "stable" && <ListItem>Validate the selected image and channel in a fresh temporary catalog</ListItem>}
                <ListItem>Remove the nightly CatalogSource</ListItem>
                <ListItem>Delete the current operator Subscription</ListItem>
                <ListItem>
                  Remove the current ClusterServiceVersion (operator)
                </ListItem>
                <ListItem>
                  Clean up stale validating and mutating webhooks
                </ListItem>
                <ListItem>
                  Patch CRD conversion webhooks to prevent API failures during
                  transition
                </ListItem>
                <ListItem>Wait for cleanup to propagate</ListItem>
                <ListItem>
                  {targetType === "stable"
                    ? "Create a fresh Subscription to the stable catalog"
                    : "Create CatalogSource with the selected FBC image and fresh Subscription"}
                </ListItem>
              </List>
            </StackItem>
            <StackItem>
              <Alert
                variant="warning"
                title="During this process (5-10 minutes), the RHOAI operator will be unavailable. Existing workloads (notebooks, model serving, pipelines) will continue running but cannot be modified until the operator is reinstalled."
                isInline
              />
            </StackItem>
            <StackItem>
              <Content component="p">
                <strong>Note:</strong> DSCI, DSC, CRDs, and user workloads will
                NOT be deleted. The operator will pick them up and reconcile. Do
                NOT manually delete DSCI or DSC resources during the reinstall —
                their finalizers require the operator to be running.
              </Content>
            </StackItem>
            <StackItem>
              <TextInput
                id="reinstall-confirm-input"
                value={confirmText}
                onChange={(_e, val) => setConfirmText(val)}
                placeholder='Type "reinstall" to confirm'
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="danger"
            onClick={handleReinstall}
            isLoading={loading}
            isDisabled={confirmText !== "reinstall" || loading || !canConfirm || !canMutate}
          >
            {loading ? "Reinstalling..." : "Confirm Reinstall"}
          </Button>
          <Button
            variant="link"
            onClick={() => {
              setConfirmOpen(false);
              setConfirmText("");
            }}
            isDisabled={loading}
          >
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </Stack>
  );
};
