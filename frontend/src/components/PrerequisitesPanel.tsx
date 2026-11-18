import React, { useState } from 'react';
import {
  Alert,
  Button,
  ClipboardCopy,
  ClipboardCopyButton,
  CodeBlock,
  CodeBlockAction,
  CodeBlockCode,
  Content,
  ExpandableSection,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  ProgressStepper,
  ProgressStep,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import type { StatusResponse } from '../types';
import { pullSecretNotUsed } from '../utils';

const MIRROR_COMMAND = `rosa create image-mirror --cluster=<your-cluster-name> \\
  --source=registry.redhat.io/rhoai --mirrors=quay.io/rhoai`;

/** A one-line shell command: monospace, with a copy button. */
const Command: React.FC<{ children: string; label: string }> = ({ children, label }) => (
  <ClipboardCopy variant="inline-compact" isBlock isCode hoverTip="Copy command" clickTip="Copied" copyAriaLabel={`Copy the ${label}`}>
    {children}
  </ClipboardCopy>
);

/** A multi-line shell command as a code block with a copy action, so its line breaks survive. */
const MultiLineCommand: React.FC<{ children: string; label: string; id: string }> = ({ children, label, id }) => {
  const [copied, setCopied] = useState(false);
  return (
    <CodeBlock
      actions={
        <CodeBlockAction>
          <ClipboardCopyButton
            id={`copy-${id}`}
            aria-label={`Copy the ${label}`}
            onClick={() => {
              void navigator.clipboard?.writeText(children).then(() => setCopied(true)).catch(() => {});
            }}
            exitDelay={copied ? 1500 : 600}
            onTooltipHidden={() => setCopied(false)}
            variant="plain"
          >
            {copied ? "Copied" : "Copy command"}
          </ClipboardCopyButton>
        </CodeBlockAction>
      }
    >
      <CodeBlockCode id={id}>{children}</CodeBlockCode>
    </CodeBlock>
  );
};

// The ImageDigestMirrorSet of a self-managed cluster, created with oc (docs/QUICKSTART.md).
// Creating it does not drain or reboot the nodes; deleting it later does.
const SELF_MANAGED_MIRROR_COMMAND = `oc apply -f - <<'EOF'
apiVersion: config.openshift.io/v1
kind: ImageDigestMirrorSet
metadata:
  name: rhoai-quay-mirror
spec:
  imageDigestMirrors:
    - source: registry.redhat.io/rhoai
      mirrors:
        - quay.io/rhoai
EOF`;

const SelfManagedIDMSInstructions: React.FC = () => (
  <Stack hasGutter>
    <StackItem>
      A cluster-admin creates the ImageDigestMirrorSet with <code>oc</code>. Creating it does not drain or reboot the
      nodes; deleting it later does.
    </StackItem>
    <StackItem><MultiLineCommand id="cmd-image-mirror-oc" label="image mirror command">{SELF_MANAGED_MIRROR_COMMAND}</MultiLineCommand></StackItem>
  </Stack>
);

/** How to create the image mirror: through OCM (rosa) on ROSA HCP, with oc on a self-managed cluster, both when the control plane is unknown. */
const MirrorInstructions: React.FC<{ controlPlane?: StatusResponse["cluster"]["controlPlane"] }> = ({ controlPlane }) => {
  if (controlPlane === "hosted") return <IDMSInstructions />;
  if (controlPlane === "self-managed") return <SelfManagedIDMSInstructions />;
  return (
    <Stack hasGutter>
      <StackItem><Content component="h4">On ROSA HCP</Content></StackItem>
      <StackItem><IDMSInstructions /></StackItem>
      <StackItem><Content component="h4">On self-managed OpenShift</Content></StackItem>
      <StackItem><SelfManagedIDMSInstructions /></StackItem>
    </Stack>
  );
};

const IDMSInstructions: React.FC = () => (
  <Stack hasGutter>
    <StackItem>
      <List component="ol">
        <ListItem>
          <Stack hasGutter>
            <StackItem>Authenticate with Kerberos:</StackItem>
            <StackItem><Command label="Kerberos command">{'kinit <username>@IPA.REDHAT.COM'}</Command></StackItem>
          </Stack>
        </ListItem>
        <ListItem>
          <Stack hasGutter>
            <StackItem>Get AWS credentials (opens a new shell). Select iaps-rhods-odh-dev when prompted, then continue in the new shell:</StackItem>
            <StackItem><Command label="AWS login command">rh-aws-saml-login</Command></StackItem>
          </Stack>
        </ListItem>
        <ListItem>
          <Stack hasGutter>
            <StackItem>Log in to the ROSA CLI (inside the new shell):</StackItem>
            <StackItem><Command label="ROSA login command">rosa login --use-auth-code</Command></StackItem>
          </Stack>
        </ListItem>
        <ListItem>
          <Stack hasGutter>
            <StackItem>Check the login:</StackItem>
            <StackItem><Command label="ROSA whoami command">rosa whoami</Command></StackItem>
          </Stack>
        </ListItem>
        <ListItem>
          <Stack hasGutter>
            <StackItem>Create the image mirror:</StackItem>
            <StackItem><MultiLineCommand id="cmd-image-mirror" label="image mirror command">{MIRROR_COMMAND}</MultiLineCommand></StackItem>
          </Stack>
        </ListItem>
      </List>
    </StackItem>
    <StackItem>
      <Alert component="p" variant="info" title="Requires OCM write access" isInline isPlain>
        Contact the cluster owner if you get a 403 error.
      </Alert>
    </StackItem>
  </Stack>
);

/**
 * SetupModal -- full setup instructions in a modal.
 * Triggered from the prerequisites banner or the help icon.
 */
export const SetupModal: React.FC<{
  isOpen: boolean;
  onClose: () => void;
  status: StatusResponse | null;
  /** The gitops profile: a missing pull secret is not used there. */
  gitOpsProfile?: boolean;
}> = ({ isOpen, onClose, status, gitOpsProfile = false }) => {
  const notUsed = pullSecretNotUsed(status, gitOpsProfile);
  const pullSecretReady = !!(status?.pullSecret.exists && status?.pullSecret.valid) || notUsed;
  const pullSecretInvalid = !!status?.pullSecret.exists && !status.pullSecret.valid;
  const idmsReady = status?.imageMirror.exists ?? false;
  const pullSecretDescription = notUsed
    ? "Not used: the gitops profile uses the cluster's pull secret, which Git or ACM manage"
    : pullSecretReady
      ? 'additional-pull-secret in kube-system can pull from quay.io/rhoai'
      : pullSecretInvalid
        ? `additional-pull-secret exists but is invalid${status?.pullSecret.detail ? `: ${status.pullSecret.detail}` : ''}`
        : 'additional-pull-secret is missing from kube-system';
  const [idmsExpanded, setIdmsExpanded] = useState(!idmsReady);

  return (
    <Modal
      aria-labelledby="setup-modal-title"
      variant={ModalVariant.large}
      isOpen={isOpen}
      onClose={onClose}
    >
      <ModalHeader title="One-time cluster setup" labelId="setup-modal-title" />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              These two prerequisites must be configured before you can install nightly
              builds. Each step only needs to be done once per cluster.
            </Content>
          </StackItem>

          <StackItem>
            <ProgressStepper isVertical>
              <ProgressStep
                variant={notUsed ? 'info' : pullSecretReady ? 'success' : pullSecretInvalid ? 'warning' : 'danger'}
                id="step-pull-secret"
                titleId="step-pull-secret-title"
                aria-label="Pull secret step"
                description={pullSecretDescription}
              >
                Pull secret
              </ProgressStep>
              <ProgressStep
                variant={idmsReady ? 'success' : 'danger'}
                id="step-idms"
                titleId="step-idms-title"
                aria-label="Image mirror step"
                description={idmsReady
                  ? 'Image mirror exists for registry.redhat.io/rhoai'
                  : 'Image mirror is missing for registry.redhat.io/rhoai'}
              >
                Image mirror (IDMS)
              </ProgressStep>
            </ProgressStepper>
          </StackItem>

          <StackItem>
            <Content component="p">
              {notUsed
                ? "The gitops profile pulls with the cluster's own pull secrets, so additional-pull-secret is not needed."
                : pullSecretReady && status?.pullSecret.nodes && !status.pullSecret.nodes.pulled
                ? "The updater's copy of the pull secret is ready. The nodes of a self-managed cluster pull with openshift-config/pull-secret: the Pull secret card on the Status page has the command that adds the quay.io/rhoai entry, and a check that the nodes can pull."
                : pullSecretReady
                ? 'The pull secret is ready. You can test or replace it from the Pull secret card on the Status page.'
                : pullSecretInvalid
                  ? 'Replace the token from the Pull secret card on the Status page.'
                  : 'Create it from the Pull secret card on the Status page.'}
            </Content>
          </StackItem>

          <StackItem>
            <ExpandableSection
              toggleText={idmsReady ? 'How the image mirror was created' : 'How to create the image mirror'}
              isExpanded={idmsExpanded}
              onToggle={(_e, val) => setIdmsExpanded(val)}
            >
              <MirrorInstructions controlPlane={status?.cluster.controlPlane} />
            </ExpandableSection>
          </StackItem>

          {pullSecretReady && idmsReady && (
            <StackItem>
              <Alert component="p"
                variant="success"
                title="All prerequisites are met"
                isInline
              >
                The cluster is ready for nightly build installations. You can close this dialog.
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
