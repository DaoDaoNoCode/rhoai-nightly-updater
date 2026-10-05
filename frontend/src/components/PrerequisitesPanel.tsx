import React, { useState } from 'react';
import {
  Alert,
  ClipboardCopy,
  Content,
  ExpandableSection,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ProgressStepper,
  ProgressStep,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import type { StatusResponse } from '../types';

const IDMSInstructions: React.FC = () => (
  <Stack hasGutter>
    <StackItem>
      <List isPlain>
        <ListItem>
          <Content component="p">
            <strong>1.</strong> Authenticate with Kerberos:
          </Content>
          <ClipboardCopy isBlock isReadOnly>
            {'kinit <username>@IPA.REDHAT.COM'}
          </ClipboardCopy>
        </ListItem>
        <ListItem>
          <Content component="p">
            <strong>2.</strong> Get AWS credentials (opens a new shell):
          </Content>
          <ClipboardCopy isBlock isReadOnly>
            rh-aws-saml-login
          </ClipboardCopy>
          <Content component="small">Select iaps-rhods-odh-dev when prompted. Continue in the new shell.</Content>
        </ListItem>
        <ListItem>
          <Content component="p">
            <strong>3.</strong> Log in to ROSA CLI (inside the new shell):
          </Content>
          <ClipboardCopy isBlock isReadOnly>
            rosa login --use-auth-code
          </ClipboardCopy>
        </ListItem>
        <ListItem>
          <Content component="p">
            <strong>4.</strong> Verify login:
          </Content>
          <ClipboardCopy isBlock isReadOnly>
            rosa whoami
          </ClipboardCopy>
        </ListItem>
        <ListItem>
          <Content component="p">
            <strong>5.</strong> Create the image mirror:
          </Content>
          <ClipboardCopy isBlock isReadOnly>
{`rosa create image-mirror --cluster=<your-cluster-name> \\
  --source=registry.redhat.io/rhoai --mirrors=quay.io/rhoai`}
          </ClipboardCopy>
        </ListItem>
      </List>
    </StackItem>
    <StackItem>
      <Alert component="p"
        variant="info"
        title="Requires OCM write access"
        isInline
        isPlain
      >
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
}> = ({ isOpen, onClose, status }) => {
  const pullSecretReady = (status?.pullSecret.exists && status?.pullSecret.valid) ?? false;
  const pullSecretInvalid = !!status?.pullSecret.exists && !status.pullSecret.valid;
  const idmsReady = status?.imageMirror.exists ?? false;
  const pullSecretDescription = pullSecretReady
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
      <ModalHeader title="One-Time Cluster Setup" labelId="setup-modal-title" />
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
                variant={pullSecretReady ? 'success' : pullSecretInvalid ? 'warning' : 'danger'}
                id="step-pull-secret"
                titleId="step-pull-secret-title"
                aria-label="Pull secret step"
                description={pullSecretDescription}
              >
                Pull Secret
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
                Image Mirror (IDMS)
              </ProgressStep>
            </ProgressStepper>
          </StackItem>

          <StackItem>
            <Content component="p">
              {pullSecretReady
                ? 'The pull secret is ready. You can test or replace it from the Pull secret card on the Status page.'
                : pullSecretInvalid
                  ? 'Replace the token from the Pull secret card on the Status page.'
                  : 'Create it from the Pull secret card on the Status page.'}
            </Content>
          </StackItem>

          <StackItem>
            <ExpandableSection
              toggleText={idmsReady ? 'Image mirror instructions (complete)' : 'Configure Image Mirror'}
              isExpanded={idmsExpanded}
              onToggle={(_e, val) => setIdmsExpanded(val)}
              isIndented
            >
              <IDMSInstructions />
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
    </Modal>
  );
};
