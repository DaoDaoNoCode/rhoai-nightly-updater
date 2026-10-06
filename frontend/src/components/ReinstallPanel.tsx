import React, { useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  HelperText,
  HelperTextItem,
  MenuToggle,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Radio,
  Select,
  SelectList,
  SelectOption,
  Spinner,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import type { NightlyTag, StatusResponse } from "../types";
import type { OperatorOperationOptions } from "../services/api";
import { useOperation } from "../state/AppState";
import { useDashboardOverride, useMutationBlocker } from "../state/AppInfo";
import { STEP_SETS } from "../operationSteps";
import { compareTagToInstalled, compareVersions, parseImageRef, shortDigest } from "../build";
import { TooltipButton } from "./TooltipButton";
import { StatusLabel, TagLabel } from "./StatusLabel";
import { StepsPreview } from "./StepsPreview";
import { REVERT_DASHBOARD_EXPLANATION, describeDashboardSession, type OperatorRequest, type ReinstallTargetType } from "./OperatorActions";

/** pkg/api customFBCImageRegex: only builds of the RHOAI FBC repository, with a tag and/or digest. */
export const CUSTOM_FBC_IMAGE = /^quay\.io\/rhoai\/rhoai-fbc-fragment(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$/;
const CHANNEL_NAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/;

export function isValidCustomImage(image: string): boolean {
  const m = CUSTOM_FBC_IMAGE.exec(image.trim());
  return !!m && !!(m[1] || m[2]);
}

type Direction = "downgrade" | "upgrade" | "same" | "same-line" | "unknown";

const DIRECTION_LABELS: Record<Direction, React.ReactNode> = {
  downgrade: <StatusLabel status="warning">Downgrade</StatusLabel>,
  upgrade: <TagLabel color="blue">Newer</TagLabel>,
  same: <TagLabel>Same version</TagLabel>,
  "same-line": <TagLabel>Same release line</TagLabel>,
  unknown: null,
};

interface ReinstallPanelProps {
  status: StatusResponse | null;
  nightlyTags: NightlyTag[];
  tagsLoading: boolean;
  prerequisitesMet: boolean;
  runOperator: (request: OperatorRequest, options?: OperatorOperationOptions) => boolean;
}

export const ReinstallPanel: React.FC<ReinstallPanelProps> = ({
  status,
  nightlyTags,
  tagsLoading,
  prerequisitesMet,
  runOperator,
}) => {
  // No target is preselected: the stable target is often a downgrade.
  const [targetType, setTargetType] = useState<ReinstallTargetType | null>(null);
  const [tagSelectOpen, setTagSelectOpen] = useState(false);
  const [selectedImage, setSelectedImage] = useState("");
  const [customImage, setCustomImage] = useState("");
  const [channelOverride, setChannelOverride] = useState("");
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [confirmText, setConfirmText] = useState("");
  const [downgradeAck, setDowngradeAck] = useState(false);

  const operation = useOperation();
  const blocker = useMutationBlocker();
  const { override } = useDashboardOverride();
  const reinstallRun = operation.run && (operation.run.kind === "reinstall_stable" || operation.run.kind === "reinstall_nightly") ? operation.run : null;
  const outcome = reinstallRun?.outcome;
  const running = !!reinstallRun && !outcome;

  const installedVersion = status?.csv.phase && status.csv.phase !== "Not Found" ? status.csv.version : undefined;
  const channelValid = !channelOverride.trim() || CHANNEL_NAME.test(channelOverride.trim());
  const targetImage = targetType === "custom" ? customImage.trim() : targetType === "nightly" ? selectedImage.trim() : "";
  const customImageValid = isValidCustomImage(customImage);
  const selectedTag = targetType === "nightly"
    ? nightlyTags.find((t) => t.image === selectedImage)?.tag
    : targetType === "custom" ? parseImageRef(customImage).tag : undefined;

  const stableDirection = ((): Direction => {
    const cmp = compareVersions(status?.stableVersion, installedVersion);
    if (cmp === null) return "unknown";
    return cmp < 0 ? "downgrade" : cmp > 0 ? "upgrade" : "same";
  })();
  const tagDirection = (tag?: string): Direction => {
    switch (compareTagToInstalled(tag, installedVersion)) {
      case "older": return "downgrade";
      case "newer": return "upgrade";
      case "same-line": return "same-line";
      default: return "unknown";
    }
  };
  const direction: Direction = targetType === "stable" ? stableDirection : targetType ? tagDirection(selectedTag) : "unknown";
  const knownDowngrade = direction === "downgrade";

  const targetReason = (() => {
    if (!targetType) return "Choose what to reinstall first.";
    if (targetType === "stable") {
      return status?.stableChannel && !status.stableDiscoveryError ? null : "The stable release could not be found in the cluster catalog.";
    }
    if (!prerequisitesMet) return "Finish the one-time cluster setup first (pull secret and image mirror).";
    if (targetType === "nightly" && !selectedImage) return "Select a nightly build first.";
    if (targetType === "custom" && !customImageValid) return "Enter a quay.io/rhoai/rhoai-fbc-fragment image with a tag or a digest.";
    if (!channelValid) return "The channel override is not a valid channel name.";
    return null;
  })();
  const disabledReason = blocker ?? targetReason;

  const closeConfirm = () => {
    setConfirmOpen(false);
    setConfirmText("");
    setDowngradeAck(false);
  };

  const handleReinstall = () => {
    if (!targetType) return;
    const image = targetType !== "stable" ? targetImage : undefined;
    const channel = targetType !== "stable" && channelOverride.trim() ? channelOverride.trim() : undefined;
    const detail = targetType === "stable" ? `${status?.stableSource ?? "stable"} / ${status?.stableChannel ?? ""}` : targetImage;
    const options: OperatorOperationOptions = {};
    if (knownDowngrade && downgradeAck) options.allowDowngrade = true;
    if (override?.active) options.revertDashboardDev = true;
    closeConfirm();
    runOperator({ kind: "reinstall", targetType, image, channel, detail }, options);
  };

  const succeededMessage = outcome?.status === "succeeded" ? outcome.message : null;
  const alreadyOnStable = !!succeededMessage?.startsWith("Already on stable");

  const targetName = targetType === "stable"
    ? `RHOAI ${status?.stableVersion ?? ""} (GA, ${status?.stableSource} / ${status?.stableChannel})`
    : selectedTag
      ? `${selectedTag}${shortDigest(parseImageRef(targetImage).digest) ? ` · ${shortDigest(parseImageRef(targetImage).digest)}` : ""}`
      : targetImage;
  const modalTitle = knownDowngrade
    ? targetType === "stable"
      ? `Reinstall RHOAI ${status?.stableVersion} (downgrade from ${installedVersion})?`
      : `Reinstall an older release (${selectedTag}) over ${installedVersion}?`
    : "Reinstall the RHOAI operator?";
  const steps = STEP_SETS[targetType === "stable" ? "reinstall_stable" : "reinstall_nightly"];

  const tagToggleRef = React.useRef<HTMLButtonElement>(null);
  const selectedTagInfo = nightlyTags.find((t) => t.image === selectedImage);

  return (
    <Stack hasGutter>
      <StackItem>
        <FormGroup role="radiogroup" fieldId="reinstall-target" label="Reinstall to" isStack>
          <Radio
            id="reinstall-stable"
            name="reinstall-target"
            label={
              <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                <FlexItem>{status?.stableChannelPinned ? "Configured GA channel" : "Latest stable (GA)"}{status?.stableVersion ? ` ${status.stableVersion}` : ""}</FlexItem>
                {DIRECTION_LABELS[stableDirection] && <FlexItem>{DIRECTION_LABELS[stableDirection]}</FlexItem>}
              </Flex>
            }
            description={
              status?.stableChannel
                ? `From ${status.stableSource} / ${status.stableChannel}${stableDirection === "downgrade" ? `; older than the installed ${installedVersion}` : ""}.`
                : status?.stableDiscoveryError ? "The stable release could not be found in the cluster catalog." : "Looking up the stable release in the cluster catalog..."
            }
            isChecked={targetType === "stable"}
            onChange={() => { setTargetType("stable"); setChannelOverride(""); }}
            isDisabled={running}
          />
          {targetType === "stable" && status?.stableDiscoveryError && (
            <Alert component="p" variant="warning" title="Could not find the stable release" isInline isPlain>
              {status.stableDiscoveryError}. Refresh the status after fixing the catalog.
            </Alert>
          )}
          <Radio
            id="reinstall-nightly"
            name="reinstall-target"
            label="A nightly build"
            description="Pick one of the recent nightly builds."
            isChecked={targetType === "nightly"}
            onChange={() => setTargetType("nightly")}
            isDisabled={running}
          />
          <Radio
            id="reinstall-custom"
            name="reinstall-target"
            label="A specific FBC image"
            description="Any build of quay.io/rhoai/rhoai-fbc-fragment, by tag or digest. A digest pins that exact build."
            isChecked={targetType === "custom"}
            onChange={() => setTargetType("custom")}
            isDisabled={running}
          />
        </FormGroup>
      </StackItem>

      {(targetType === "nightly" || targetType === "custom") && (
        <StackItem>
          {!prerequisitesMet && (
            <Alert component="p" variant="warning" title="Finish the cluster setup first" isInline isPlain>
              Nightly builds need the pull secret and the image mirror.
            </Alert>
          )}
          <Form onSubmit={(e) => e.preventDefault()}>
            {targetType === "custom" ? (
              <FormGroup label="FBC image" fieldId="reinstall-custom-image" isRequired>
                <TextInput
                  id="reinstall-custom-image"
                  value={customImage}
                  onChange={(_e, value) => setCustomImage(value)}
                  placeholder="quay.io/rhoai/rhoai-fbc-fragment:<tag>@sha256:<digest>"
                  isDisabled={running}
                  validated={customImage.trim() && !customImageValid ? "error" : "default"}
                  aria-describedby="reinstall-custom-image-help"
                />
                <HelperText id="reinstall-custom-image-help">
                  <HelperTextItem variant={customImage.trim() && !customImageValid ? "error" : "default"}>
                    {customImage.trim() && !customImageValid
                      ? "Only quay.io/rhoai/rhoai-fbc-fragment images, with a tag, a full sha256 digest, or both."
                      : "The digest is used exactly, even if its tag now points to a newer build."}
                  </HelperTextItem>
                </HelperText>
              </FormGroup>
            ) : (
              <FormGroup label="Nightly build" fieldId="reinstall-nightly-image">
                <Select
                  isOpen={tagSelectOpen}
                  selected={selectedImage || undefined}
                  onSelect={(_e, value) => {
                    if (typeof value === "string") setSelectedImage(value);
                    setTagSelectOpen(false);
                  }}
                  onOpenChange={setTagSelectOpen}
                  toggle={{
                    toggleRef: tagToggleRef,
                    toggleNode: (
                      <MenuToggle
                        ref={tagToggleRef}
                        id="reinstall-nightly-image"
                        onClick={() => setTagSelectOpen(!tagSelectOpen)}
                        isExpanded={tagSelectOpen}
                        isDisabled={running}
                      >
                        {tagsLoading ? <><Spinner size="sm" aria-label="Loading builds" /> Loading builds...</> : selectedTagInfo?.tag || "Select a build"}
                      </MenuToggle>
                    ),
                  }}
                  shouldFocusToggleOnSelect
                >
                  <SelectList aria-label="Nightly builds">
                    {!tagsLoading && nightlyTags.length === 0 && <SelectOption value="" isDisabled>No builds found</SelectOption>}
                    {nightlyTags.map((t) => {
                      const dir = tagDirection(t.tag);
                      return (
                        <SelectOption
                          key={t.image}
                          value={t.image}
                          description={`${shortDigest(parseImageRef(t.image).digest) ?? ""}${dir === "downgrade" ? " · older than installed" : ""}`}
                        >
                          {t.tag}
                        </SelectOption>
                      );
                    })}
                  </SelectList>
                </Select>
                {selectedTag && DIRECTION_LABELS[direction] && (
                  <HelperText>
                    <HelperTextItem>
                      {direction === "downgrade"
                        ? `Older release line than the installed ${installedVersion}.`
                        : direction === "same-line"
                          ? `Same release line as the installed ${installedVersion}; the exact version is checked before anything changes.`
                          : `Newer than the installed ${installedVersion}.`}
                    </HelperTextItem>
                  </HelperText>
                )}
              </FormGroup>
            )}
            <FormGroup label="Channel override" fieldId="reinstall-channel-override">
              <TextInput
                id="reinstall-channel-override"
                value={channelOverride}
                onChange={(_e, val) => setChannelOverride(val)}
                placeholder="Detected from the catalog"
                isDisabled={running}
                validated={channelValid ? "default" : "error"}
                aria-describedby="reinstall-channel-help"
              />
              <HelperText id="reinstall-channel-help">
                <HelperTextItem variant={channelValid ? "default" : "error"}>
                  {channelValid ? "Optional. Only set it if the detected channel is wrong." : "Letters, digits, '.', '_' and '-' only."}
                </HelperTextItem>
              </HelperText>
            </FormGroup>
          </Form>
        </StackItem>
      )}

      <StackItem>
        <TooltipButton
          variant="danger"
          onClick={() => setConfirmOpen(true)}
          disabledReason={disabledReason}
          isLoading={running}
        >
          {running ? "Reinstalling..." : "Reinstall operator..."}
        </TooltipButton>
      </StackItem>

      {succeededMessage && (
        <StackItem>
          <Alert variant={alreadyOnStable ? "info" : "success"} title={succeededMessage} isInline component="p">
            {reinstallRun?.kind === "reinstall_stable" && !alreadyOnStable && "The nightly catalog was removed."}
          </Alert>
        </StackItem>
      )}

      <Modal
        aria-labelledby="confirm-reinstall-title"
        variant={ModalVariant.medium}
        isOpen={confirmOpen}
        onClose={closeConfirm}
      >
        <ModalHeader title={modalTitle} titleIconVariant="warning" labelId="confirm-reinstall-title" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <DescriptionList isHorizontal isCompact horizontalTermWidthModifier={{ default: "10ch" }}>
                <DescriptionListGroup>
                  <DescriptionListTerm>Installed</DescriptionListTerm>
                  <DescriptionListDescription>
                    {status?.csv.name || "nothing"}{status?.subscription.source ? ` (${status.subscription.source} / ${status.subscription.channel})` : ""}
                  </DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>Target</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                      <FlexItem className="pf-v6-u-text-break-word">{targetName}</FlexItem>
                      {DIRECTION_LABELS[direction] && <FlexItem>{DIRECTION_LABELS[direction]}</FlexItem>}
                    </Flex>
                    {channelOverride.trim() && <div className="pf-v6-u-text-color-subtle">Channel: {channelOverride.trim()}</div>}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              </DescriptionList>
            </StackItem>
            {knownDowngrade && (
              <StackItem>
                <Alert variant="danger" isInline component="p" title="This installs an older operator version">
                  OLM cannot downgrade an operator, so Reinstall removes it and installs the older one. The CRDs keep the
                  newer schema, and the older operator may reject or ignore fields that the newer version created in
                  your DataScienceCluster and DSCInitialization.
                </Alert>
              </StackItem>
            )}
            {knownDowngrade && (
              <StackItem>
                <Checkbox
                  id="reinstall-downgrade-ack"
                  isChecked={downgradeAck}
                  onChange={(_e, checked) => setDowngradeAck(checked)}
                  label="I understand and want to install the older version"
                />
              </StackItem>
            )}
            {override?.active && (
              <StackItem>
                <Alert variant="warning" isInline component="p" title="A Dashboard Dev session is active">
                  dashboard-operator is paused ({describeDashboardSession(override)}). The reinstall ends that session first.
                  {" "}{REVERT_DASHBOARD_EXPLANATION}
                </Alert>
              </StackItem>
            )}
            <StackItem>
              <StepsPreview steps={steps} idPrefix="reinstall-step" />
            </StackItem>
            <StackItem>
              <Content component="p">
                DSCInitialization, DataScienceCluster, CRDs and your workloads are not deleted; the new operator adopts them.
                Don&apos;t delete the DSC or DSCI while the operator is gone: their finalizers need it. If the new install fails,
                the previous operator is restored.
              </Content>
            </StackItem>
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label='Type "reinstall" to confirm' fieldId="reinstall-confirm-input" isRequired>
                  <TextInput
                    id="reinstall-confirm-input"
                    value={confirmText}
                    onChange={(_e, val) => setConfirmText(val)}
                    isRequired
                    autoComplete="off"
                  />
                </FormGroup>
              </Form>
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <TooltipButton
            variant="danger"
            onClick={handleReinstall}
            isDisabled={confirmText !== "reinstall" || (knownDowngrade && !downgradeAck)}
            disabledReason={disabledReason}
          >
            {knownDowngrade ? "Reinstall older version" : "Reinstall"}
          </TooltipButton>
          <Button variant="link" onClick={closeConfirm}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </Stack>
  );
};
