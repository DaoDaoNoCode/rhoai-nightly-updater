import React, { useState } from 'react';
import {
  Alert,
  AlertActionLink,
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

interface PrerequisitesPanelProps {
  status: StatusResponse | null;
}

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
{`rosa create image-mirror --cluster=razzmatazz-serving-demo \\
  --source=registry.redhat.io/rhoai --mirrors=quay.io/rhoai`}
          </ClipboardCopy>
        </ListItem>
      </List>
    </StackItem>
    <StackItem>
      <Alert
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
  const idmsReady = status?.imageMirror.exists ?? false;
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
                variant={pullSecretReady ? 'success' : 'danger'}
                id="step-pull-secret"
                titleId="step-pull-secret-title"
                aria-label="Pull secret step"
                description={pullSecretReady
                  ? 'additional-pull-secret exists in kube-system'
                  : 'additional-pull-secret is missing from kube-system'}
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
                ? 'Pull secret is configured. You can test or update it from the Pull Secret status card.'
                : 'Pull secret is missing. Use the Pull Secret status card to create it.'}
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
              <Alert
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

/**
 * PrerequisitesBanner -- an inline alert shown when prerequisites are not met.
 * Replaces the old cluttered expandable panel.
 */
export const PrerequisitesBanner: React.FC<PrerequisitesPanelProps & { onOpenSetup: () => void }> = ({
  status,
  onOpenSetup,
}) => {
  if (!status) return null;

  const pullSecretReady = status.pullSecret.exists;
  const idmsReady = status.imageMirror.exists;
  const allReady = pullSecretReady && idmsReady;

  if (allReady) return null;

  const missing: string[] = [];
  if (!pullSecretReady) missing.push('Pull Secret');
  if (!idmsReady) missing.push('Image Mirror');

  return (
    <Alert
      variant="warning"
      title="One-time cluster setup required"
      isInline
      actionLinks={
        <AlertActionLink onClick={onOpenSetup}>
          View setup instructions
        </AlertActionLink>
      }
    >
      Missing: {missing.join(', ')}. Complete the setup before installing nightly builds.
    </Alert>
  );
};

