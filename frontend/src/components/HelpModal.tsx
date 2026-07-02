import React, { useState } from "react";
import {
  Button,
  Content,
  ContentVariants,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
} from "@patternfly/react-core";
import QuestionCircleIcon from "@patternfly/react-icons/dist/esm/icons/question-circle-icon";

export const HelpButton: React.FC = () => {
  const [isOpen, setIsOpen] = useState(false);

  return (
    <>
      <Button variant="plain" aria-label="Help" onClick={() => setIsOpen(true)}>
        <QuestionCircleIcon />
      </Button>
      <Modal
        aria-labelledby="help-modal-title"
        variant={ModalVariant.medium}
        isOpen={isOpen}
        onClose={() => setIsOpen(false)}
      >
        <ModalHeader
          title="About RHOAI Nightly Updater"
          labelId="help-modal-title"
        />
        <ModalBody>
          <Content component={ContentVariants.h3}>
            What does this tool do?
          </Content>
          <Content component="p">
            This tool manages RHOAI (Red Hat OpenShift AI) operator nightly
            builds on a shared ROSA HCP cluster. It lets you switch the operator
            to a nightly FBC (File-Based Catalog) image or roll back to the
            stable release, all without needing direct <code>oc</code> CLI
            access.
          </Content>

          <Content component={ContentVariants.h3}>
            Where do I get the FBC image?
          </Content>
          <Content component="p">
            The nightly FBC image reference is posted daily in the{" "}
            <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer"><strong>#rhoai-build-notifications</strong></a> Slack channel. Copy the
            full image URI including the <code>@sha256:...</code> digest and
            paste it into the &quot;FBC Image&quot; field.
          </Content>

          <Content component={ContentVariants.h3}>Update vs Dry Run</Content>
          <List>
            <ListItem>
              <strong>Dry Run</strong> validates the image and shows what
              changes would be made without actually applying them. Use this to
              verify your image reference is correct.
            </ListItem>
            <ListItem>
              <strong>Update</strong> creates (or updates) a nightly
              CatalogSource with the given image, then switches the operator
              Subscription to use it. OLM will automatically install the new
              operator version.
            </ListItem>
          </List>

          <Content component={ContentVariants.h3}>How to roll back</Content>
          <Content component="p">
            Click the <strong>Rollback to stable</strong> link at the bottom of
            the action panel to switch the operator back to the official{" "}
            <code>redhat-operators</code> catalog on the stable channel. The
            nightly CatalogSource will be deleted and OLM will reinstall the
            latest stable release.
          </Content>

          <Content component={ContentVariants.h3}>
            One-time cluster setup
          </Content>
          <Content component="p">
            Before the first nightly install, the cluster needs two
            prerequisites:
          </Content>
          <List>
            <ListItem>
              <strong>Pull Secret</strong> -- an{" "}
              <code>additional-pull-secret</code> in <code>kube-system</code>{" "}
              with credentials for <code>quay.io/rhoai</code>. Get the
              credentials from Bitwarden (&quot;Openshift AI devel&quot;
              collection).
            </ListItem>
            <ListItem>
              <strong>Image Mirror (IDMS)</strong> -- an image mirror that
              redirects <code>registry.redhat.io/rhoai</code> to{" "}
              <code>quay.io/rhoai</code>. Created via the <code>rosa</code> CLI
              with OCM write access.
            </ListItem>
          </List>
          <Content component="p">
            If either prerequisite is missing, a banner will appear on the main
            page with a link to view detailed setup instructions.
          </Content>
        </ModalBody>
      </Modal>
    </>
  );
};
