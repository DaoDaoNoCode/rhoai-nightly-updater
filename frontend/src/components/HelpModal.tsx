import React, { useState } from "react";
import {
  Button,
  Content,
  ContentVariants,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  List,
  ListItem,
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  Title,
} from "@patternfly/react-core";
import QuestionCircleIcon from "@patternfly/react-icons/dist/esm/icons/question-circle-icon";
import { useVersion } from "../state/AppInfo";
import { useClusterStatus } from "../state/AppState";

const GLOSSARY: [string, React.ReactNode][] = [
  ["FBC image", <>File-Based Catalog: a container image that lists operator bundles. Nightly RHOAI builds are published as <code>quay.io/rhoai/rhoai-fbc-fragment:&lt;tag&gt;@sha256:&lt;digest&gt;</code>; the digest identifies one exact build.</>],
  ["CatalogSource", <>Tells OLM where to read a catalog. This tool manages <code>rhoai-catalog-dev</code> in openshift-marketplace for nightly builds; GA releases come from <code>redhat-operators</code>.</>],
  ["Subscription", <>Asks OLM to install an operator from a catalog and channel and keep it there. Deleting it leaves the running operator in place.</>],
  ["InstallPlan", <>The plan OLM creates to install a specific version (its CSV, CRDs and RBAC).</>],
  ["CSV", <>ClusterServiceVersion: one installed operator version, e.g. <code>rhods-operator.3.6.0</code>. Its phase (Installing, Succeeded, Failed) is the operator&apos;s health. Every nightly of a release line has the same CSV version, which is why builds are told apart by digest.</>],
  ["DSC / DSCI", <>DataScienceCluster and DSCInitialization: which RHOAI components run and how. The operator manages them; this tool never deletes them.</>],
];

export const HelpButton: React.FC = () => {
  const [isOpen, setIsOpen] = useState(false);
  const version = useVersion();
  const { status } = useClusterStatus();

  return (
    <>
      <Button variant="plain" aria-label="Help" onClick={() => setIsOpen(true)} icon={<QuestionCircleIcon />} />
      <Modal
        aria-labelledby="help-modal-title"
        variant={ModalVariant.medium}
        isOpen={isOpen}
        onClose={() => setIsOpen(false)}
      >
        <ModalHeader title="About RHOAI Nightly Updater" labelId="help-modal-title" />
        <ModalBody>
          <Content>
            <Content component={ContentVariants.p}>
              Installs and updates RHOAI (Red Hat OpenShift AI) nightly builds on your OpenShift cluster through OLM, and
              helps you test odh-dashboard changes on it. Everything it does is also possible with <code>oc</code>; the
              tool runs the steps in a safe order and undoes a failed attempt.
            </Content>

            <Content component={ContentVariants.h3}>Daily update</Content>
            <List component="ol">
              <ListItem>Open <strong>Status</strong>. The top card shows the installed build and the newest build of the same stream; <em>Update available</em> means their digests differ.</ListItem>
              <ListItem>Click <strong>Update to latest</strong>, read the confirmation (it lists the exact steps), and confirm.</ListItem>
              <ListItem>Follow the progress. It is safe to leave the page or close the tab: the update keeps running on the server, and any tab (yours or a teammate&apos;s) shows it again.</ListItem>
            </List>
            <Content component={ContentVariants.p}>
              To install a specific build, paste it under <strong>Update to a specific build</strong>, or pick one in
              <strong> Build Explorer</strong>. Builds are posted in{" "}
              <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer">#rhoai-build-notifications</a>.
            </Content>

            <Content component={ContentVariants.h3}>If something goes wrong</Content>
            <List>
              <ListItem><strong>Operator Failed after a nightly:</strong> update to a newer build; that is the usual fix.</ListItem>
              <ListItem><strong>Re-deploy operator:</strong> deletes and recreates the Subscription and CSV for the same version (images are pinned by digest, so it never fetches a newer build).</ListItem>
              <ListItem><strong>Reinstall:</strong> uninstalls and installs again, to GA, to an older build or when nothing else helps. Older versions need an explicit confirmation.</ListItem>
              <ListItem><strong>Diagnostics:</strong> read-only checks with explanations; fixes only run after you confirm them.</ListItem>
              <ListItem>A failed Update, Re-deploy or Reinstall restores the previous state; all three can be run again safely.</ListItem>
            </List>

            <Content component={ContentVariants.h3}>Dashboard Dev</Content>
            <Content component={ContentVariants.p}>
              Deploys an odh-dashboard PR or main build into RHOAI by pausing dashboard-operator. While it is paused, the
              dashboard does not follow RHOAI updates; a notice on every page says so. Update, Re-deploy and Reinstall
              offer to revert it first.
            </Content>

            <Content component={ContentVariants.h3}>Test resources</Content>
            <Content component={ContentVariants.p}>
              Sets up S3 storage (SeaweedFS), MLflow and pipeline servers with defaults that work on a fresh cluster, and
              tears down only what this tool created. The storage keeps MinIO&apos;s Service name, <code>minio-service</code>, so
              pipeline servers set up against MinIO keep working; set up the storage again to replace a MinIO from an earlier
              version (it starts fresh and keeps the old volume). Teardown keeps the <code>minio</code> namespace; delete it
              with <code>oc delete project minio</code> once it is empty.
            </Content>

            <Content component={ContentVariants.h3}>One-time cluster setup</Content>
            <List>
              <ListItem><strong>Pull secret:</strong> <code>additional-pull-secret</code> in kube-system with credentials for quay.io/rhoai.</ListItem>
              <ListItem><strong>Image mirror (IDMS):</strong> redirects registry.redhat.io/rhoai to quay.io/rhoai; created with the <code>rosa</code> CLI.</ListItem>
            </List>
            <Content component={ContentVariants.p}>The Status page shows both and how to set them up until they are ready.</Content>

            <Content component={ContentVariants.h3}>Glossary</Content>
          </Content>
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="Glossary">
            {GLOSSARY.map(([term, text]) => (
              <DescriptionListGroup key={term}>
                <DescriptionListTerm>{term}</DescriptionListTerm>
                <DescriptionListDescription>{text}</DescriptionListDescription>
              </DescriptionListGroup>
            ))}
          </DescriptionList>

          <Title headingLevel="h3" size="md" className="pf-v6-u-mt-lg pf-v6-u-mb-sm">About this installation</Title>
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="About this installation">
            {status && (
              <DescriptionListGroup>
                <DescriptionListTerm>Cluster</DescriptionListTerm>
                <DescriptionListDescription>OpenShift {status.cluster.version}, signed in as {status.cluster.user}</DescriptionListDescription>
              </DescriptionListGroup>
            )}
            {version && (
              <DescriptionListGroup>
                <DescriptionListTerm>Updater build</DescriptionListTerm>
                <DescriptionListDescription>
                  {version.version}
                  {version.commit && version.commit !== "unknown" ? <> (commit <code>{version.commit.slice(0, 12)}</code>)</> : null}
                  {version.buildDate && version.buildDate !== "unknown" ? `, built ${version.buildDate}` : ""}
                </DescriptionListDescription>
              </DescriptionListGroup>
            )}
            {version?.templateOutdated && (
              <DescriptionListGroup>
                <DescriptionListTerm>Deployment</DescriptionListTerm>
                <DescriptionListDescription>The deployment template is out of date; an admin should run <code>make upgrade</code>.</DescriptionListDescription>
              </DescriptionListGroup>
            )}
          </DescriptionList>
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={() => setIsOpen(false)}>Close</Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
