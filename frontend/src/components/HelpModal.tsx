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
import { useUpdateCheck, useVersion } from "../state/AppInfo";
import { GITOPS_PROFILE_UPDATER_RELEASES, isGitOpsProfile } from "../profile";
import { useClusterStatus } from "../state/AppState";
import { useRole, useSignedInUser } from "../state/Permissions";

const FBC_IMAGE: [string, React.ReactNode] = ["FBC image", <>File-Based Catalog: a container image that lists operator bundles. Nightly RHOAI builds are published as <code>quay.io/rhoai/rhoai-fbc-fragment:&lt;tag&gt;@sha256:&lt;digest&gt;</code>; the digest identifies one exact build.</>];
const CSV: [string, React.ReactNode] = ["CSV", <>ClusterServiceVersion: one installed operator version, e.g. <code>rhods-operator.3.6.0</code>. Its phase (Installing, Succeeded, Failed) is the operator&apos;s health. Every nightly of a release line has the same CSV version, which is why builds are told apart by digest.</>];

const GLOSSARY: [string, React.ReactNode][] = [
  FBC_IMAGE,
  ["CatalogSource", <>Tells OLM where to read a catalog. This tool manages <code>rhoai-catalog-dev</code> in openshift-marketplace for nightly builds; GA releases come from <code>redhat-operators</code>.</>],
  ["Subscription", <>Asks OLM to install an operator from a catalog and channel and keep it there. Deleting it leaves the running operator in place.</>],
  ["InstallPlan", <>The plan OLM creates to install a specific version (its CSV, CRDs and RBAC).</>],
  CSV,
  ["DSC / DSCI", <>DataScienceCluster and DSCInitialization: which RHOAI components run and how. The operator manages them; this tool never deletes them.</>],
];

/** The terms a user who is not a cluster-admin meets, on Status and in Build Explorer. */
const USER_GLOSSARY: [string, React.ReactNode][] = [
  FBC_IMAGE,
  CSV,
  ["DSC", <>DataScienceCluster: which RHOAI components run. Its management state says whether RHOAI runs a component (Managed) or not (Removed).</>],
];

const BUILD_NOTIFICATIONS = (
  <a href="https://redhat.enterprise.slack.com/archives/C07ANR2U56C" target="_blank" rel="noopener noreferrer">#rhoai-build-notifications</a>
);

/** The guide for cluster-admins: every page and every change. */
const AdminGuide: React.FC = () => (
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
      <strong> Build Explorer</strong>. Builds are posted in {BUILD_NOTIFICATIONS}.
    </Content>

    <Content component={ContentVariants.h3}>If something goes wrong</Content>
    <List>
      <ListItem><strong>Operator Failed after a nightly:</strong> update to a newer build; that is the usual fix.</ListItem>
      <ListItem><strong>Re-deploy operator:</strong> deletes and recreates the Subscription and CSV for the same version (images are pinned by digest, so it never fetches a newer build).</ListItem>
      <ListItem><strong>Install a different build:</strong> uninstalls the operator and installs the build you choose: the GA release, any nightly build (older ones too) or a custom catalog image; also the last resort when nothing else helps. Older versions need an explicit confirmation.</ListItem>
      <ListItem><strong>Diagnostics:</strong> read-only checks with explanations; fixes only run after you confirm them.</ListItem>
      <ListItem>A failed Update, Re-deploy or Install a different build restores the previous state; all three can be run again safely.</ListItem>
    </List>

    <Content component={ContentVariants.h3}>Dashboard Dev</Content>
    <Content component={ContentVariants.p}>
      Deploys an odh-dashboard PR or main build into RHOAI by pausing the dashboard module's operator (dashboard-operator on RHOAI 3.6). While it is paused, the
      dashboard does not follow RHOAI updates; a notice on every page says so. Update, Re-deploy and Reinstall
      offer to revert it first.
    </Content>

    <Content component={ContentVariants.h3}>Environment</Content>
    <Content component={ContentVariants.p}>
      Sets up S3 storage (SeaweedFS), MLflow and pipeline servers with defaults that work on a fresh cluster, and
      tears down only what this tool created. The storage is in namespace <code>seaweedfs</code>; pipeline servers
      reach it through Service <code>seaweedfs-s3</code>. Where an earlier version of this tool left its storage in
      namespace <code>minio</code>, the page shows it with the Storage card. Teardown keeps the storage namespaces;
      delete them with <code>oc delete project</code> once they are empty.
    </Content>

    <Content component={ContentVariants.h3}>One-time cluster setup</Content>
    <List>
      <ListItem><strong>Pull secret:</strong> <code>additional-pull-secret</code> in kube-system with credentials for quay.io/rhoai. On self-managed OpenShift the nodes pull with <code>openshift-config/pull-secret</code> instead: a cluster-admin adds the quay.io/rhoai entry with the command the setup shows.</ListItem>
      <ListItem><strong>Image mirror (IDMS):</strong> redirects registry.redhat.io/rhoai to quay.io/rhoai; created with the <code>rosa</code> CLI on ROSA HCP, with <code>oc</code> on self-managed OpenShift.</ListItem>
    </List>
    <Content component={ContentVariants.p}>The Status page shows both and how to set them up until they are ready.</Content>

    <Content component={ContentVariants.h3}>Glossary</Content>
  </Content>
);

/**
 * The guide for a user who is not a cluster-admin (docs/research/per-user-views.md):
 * only the pages they can open and what they can do there.
 */
const UserGuide: React.FC = () => (
  <Content>
    <Content component={ContentVariants.p}>
      Shows the RHOAI (Red Hat OpenShift AI) install on this OpenShift cluster, its nightly builds, and what runs in
      your data science projects. You can look at everything here; changes to the cluster are made by a cluster-admin.
    </Content>

    <Content component={ContentVariants.h3}>Status</Content>
    <Content component={ContentVariants.p}>
      The RHOAI version, its channel and the operator&apos;s phase; the nightly build the cluster runs and whether a
      newer one exists; the DataScienceCluster and whether each component is ready.
    </Content>

    <Content component={ContentVariants.h3}>Build Explorer</Content>
    <Content component={ContentVariants.p}>
      Every nightly build on Quay: filter them, see what a build contains, and find which builds contain a change
      (search by image, commit SHA, PR number such as <code>#5123</code> or PR URL). Builds are posted in{" "}
      {BUILD_NOTIFICATIONS}.
    </Content>

    <Content component={ContentVariants.h3}>Environment</Content>
    <Content component={ContentVariants.p}>
      Your data science projects. Pick one to see its pipeline server, models, playground and MLflow sample data, read
      with your own permissions. When its pipeline server is ready, a sample pipeline checks that runs work. To add a
      pipeline server, a model or a playground, use the RHOAI dashboard. No projects? Create one in the RHOAI dashboard,
      or ask a project admin to add you to theirs.
    </Content>

    <Content component={ContentVariants.h3}>While an update runs</Content>
    <Content component={ContentVariants.p}>
      A banner on every page says since when. What the pages show may change until it finishes.
    </Content>

    <Content component={ContentVariants.h3}>Glossary</Content>
  </Content>
);

export const HelpButton: React.FC = () => {
  const [isOpen, setIsOpen] = useState(false);
  const version = useVersion();
  const update = useUpdateCheck();
  const { status } = useClusterStatus();
  // Fail closed: the cluster-admin's guide only once the permission check says so.
  const admin = useRole() === "admin";
  const signedInUser = useSignedInUser();

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
          {admin ? <AdminGuide /> : <UserGuide />}
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="Glossary">
            {(admin ? GLOSSARY : USER_GLOSSARY).map(([term, text]) => (
              <DescriptionListGroup key={term}>
                <DescriptionListTerm>{term}</DescriptionListTerm>
                <DescriptionListDescription>{text}</DescriptionListDescription>
              </DescriptionListGroup>
            ))}
          </DescriptionList>

          <Title headingLevel="h3" size="md" className="pf-v6-u-mt-lg pf-v6-u-mb-sm">About this installation</Title>
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="About this installation">
            {admin && status && (
              <DescriptionListGroup>
                <DescriptionListTerm>Cluster</DescriptionListTerm>
                <DescriptionListDescription>OpenShift {status.cluster.version}, signed in as {status.cluster.user}</DescriptionListDescription>
              </DescriptionListGroup>
            )}
            {!admin && signedInUser && (
              <DescriptionListGroup>
                <DescriptionListTerm>Signed in as</DescriptionListTerm>
                <DescriptionListDescription>{signedInUser}</DescriptionListDescription>
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
            {/* Upgrading the updater is a cluster-admin's job, so only they see how. */}
            {admin && (update?.pinnedInGit || isGitOpsProfile(version)) && (
              <DescriptionListGroup>
                <DescriptionListTerm>Updater releases</DescriptionListTerm>
                <DescriptionListDescription>{GITOPS_PROFILE_UPDATER_RELEASES}</DescriptionListDescription>
              </DescriptionListGroup>
            )}
            {admin && update?.updateAvailable && update.latest && (
              <DescriptionListGroup>
                <DescriptionListTerm>Newer release</DescriptionListTerm>
                <DescriptionListDescription>
                  {update.latest}
                  {update.majorUpgrade ? " (a new major version: a full redeploy)" : ""}
                  {update.releaseNotesURL && (
                    <>
                      {" "}
                      <Button variant="link" isInline component="a" href={update.releaseNotesURL} target="_blank" rel="noopener noreferrer">
                        Release notes
                      </Button>
                    </>
                  )}
                </DescriptionListDescription>
              </DescriptionListGroup>
            )}
            {admin && version?.templateOutdated && (
              <DescriptionListGroup>
                <DescriptionListTerm>Deployment</DescriptionListTerm>
                <DescriptionListDescription>
                  {version.installMethod === "gitops"
                    ? <>The deployment template is out of date; an admin should set the chart&apos;s <code>targetRevision</code> in the GitOps repository to the release of the image (<code>docs/GITOPS.md</code>).</>
                    : <>The deployment template is out of date; an admin should run <code>make upgrade</code>.</>}
                </DescriptionListDescription>
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
