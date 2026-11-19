import React, { useState } from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  ExpandableSection,
  Flex,
  FlexItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { NodeSetupAction, StatusResponse } from "../types";
import { StatusLabel } from "./StatusLabel";
import { TooltipButton } from "./TooltipButton";
import { CopyableCode } from "./environment/UseIt";
import { NodeSetupDialog } from "./NodeSetupDialog";
import { useMutationBlocker } from "../state/AppInfo";
import { MIRROR_CREATE_COMMAND, MIRROR_NAME, MIRROR_REMOVE_COMMAND } from "../nodeSetupCommands";

/** The oc commands for whoever prefers the CLI. */
export const MirrorCommands: React.FC<{ exists?: boolean }> = ({ exists }) => (
  <Stack hasGutter>
    {!exists && (
      <>
        <StackItem><Content component="p">Create it (as a cluster-admin):</Content></StackItem>
        <StackItem><CopyableCode id="image-mirror-create-command" label="the command" code={MIRROR_CREATE_COMMAND} /></StackItem>
      </>
    )}
    <StackItem><Content component="p">{exists ? "Remove it (as a cluster-admin):" : "Remove it again:"}</Content></StackItem>
    <StackItem><CopyableCode id="image-mirror-remove-command" label="the removal command" code={MIRROR_REMOVE_COMMAND} /></StackItem>
  </Stack>
);

/**
 * The image mirror row of the cluster setup. On a cluster whose control
 * plane runs on the cluster (self-managed, or ROSA Classic and OpenShift
 * Dedicated, whose admission webhook allows a mirror of
 * registry.redhat.io/rhoai) the mirror is one ImageDigestMirrorSet: Create
 * and Remove make or delete it with the user's own permissions, after a
 * review (NodeSetupDialog). On a hosted control plane (ROSA HCP) it is
 * configured through OpenShift Cluster Manager, so the instructions stay;
 * when the control plane is unknown the instructions come first and Create
 * is an attempt whose refusal the dialog explains.
 */
export const ImageMirrorSetup: React.FC<{
  status: StatusResponse;
  readOnly: boolean;
  onShowInstructions: () => void;
  onStatusRefresh: () => void;
}> = ({ status, readOnly, onShowInstructions, onStatusRefresh }) => {
  const blocker = useMutationBlocker({ ignoreReconcile: true });
  const [dialog, setDialog] = useState<NodeSetupAction | null>(null);
  const [diyOpen, setDiyOpen] = useState(false);
  const cp = status.cluster.controlPlane;
  const mirror = status.imageMirror;
  const onCluster = cp === "self-managed" || cp === "managed";
  const unknown = cp === undefined;
  const ours = mirror.exists && mirror.name === MIRROR_NAME;

  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><strong id="setup-image-mirror">Image mirror (IDMS)</strong></FlexItem>
          <FlexItem>
            {mirror.exists
              ? <StatusLabel status="success">Ready</StatusLabel>
              : <StatusLabel status="danger">Missing</StatusLabel>}
          </FlexItem>
        </Flex>
        <Content component="p" className="pf-v6-u-text-color-subtle">
          Redirects registry.redhat.io/rhoai image pulls to quay.io/rhoai, where nightly images are published.
          {cp === "hosted" && !mirror.exists && " On ROSA HCP it is configured through OpenShift Cluster Manager (rosa create image-mirror), not with an object in the cluster."}
        </Content>
      </StackItem>
      {mirror.exists && (
        <StackItem>
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "8ch" }}>
            <DescriptionListGroup>
              <DescriptionListTerm>Name</DescriptionListTerm>
              <DescriptionListDescription>{mirror.name || "unknown"}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>Source</DescriptionListTerm>
              <DescriptionListDescription>{mirror.source || "registry.redhat.io/rhoai"}</DescriptionListDescription>
            </DescriptionListGroup>
          </DescriptionList>
        </StackItem>
      )}
      {!readOnly && !mirror.exists && (
        <StackItem>
          <ActionList>
            <ActionListGroup>
              {onCluster && (
                <ActionListItem>
                  <TooltipButton variant="primary" onClick={() => setDialog("mirror-create")} disabledReason={blocker}>Create the image mirror</TooltipButton>
                </ActionListItem>
              )}
              {!onCluster && (
                <ActionListItem>
                  <Button variant="secondary" onClick={onShowInstructions}>How to create the image mirror</Button>
                </ActionListItem>
              )}
              {unknown && (
                <ActionListItem>
                  <TooltipButton variant="secondary" onClick={() => setDialog("mirror-create")} disabledReason={blocker}>Try to create it here</TooltipButton>
                </ActionListItem>
              )}
            </ActionListGroup>
          </ActionList>
        </StackItem>
      )}
      {!readOnly && mirror.exists && onCluster && ours && (
        <StackItem>
          <ActionList>
            <ActionListGroup>
              <ActionListItem>
                <TooltipButton variant="secondary" isDanger onClick={() => setDialog("mirror-remove")} disabledReason={blocker}>Remove the image mirror</TooltipButton>
              </ActionListItem>
            </ActionListGroup>
          </ActionList>
        </StackItem>
      )}
      {!readOnly && onCluster && (mirror.exists ? ours : true) && (
        <StackItem>
          <ExpandableSection toggleText="Do it yourself instead" isExpanded={diyOpen} onToggle={(_e, open) => setDiyOpen(open)} isIndented>
            <MirrorCommands exists={mirror.exists} />
          </ExpandableSection>
        </StackItem>
      )}
      {dialog && <NodeSetupDialog action={dialog} onClose={() => setDialog(null)} onChanged={onStatusRefresh} />}
    </Stack>
  );
};
