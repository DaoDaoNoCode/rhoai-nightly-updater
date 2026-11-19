import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Checkbox,
  ClipboardCopy,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Skeleton,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { KeptPVC, LegacyStorageState, ResourceState, ResourcesStatus } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import {
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  ProblemAlerts,
  StartingNote,
  consolePodsHref,
  isTerminating,
  pvcList,
  pvcsLost,
  terminalReason,
} from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { CopyS3EnvButton, S3SecretField } from "./AdminLogin";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { notManaged } from "./resourceStatus";
import { ComponentAlert, CopyableCode, NoProjectsAlert, kfpClientSnippet } from "./UseIt";
import { PipelineSample, pipelineTokenCommands, pipelineTokenRemoval } from "./PipelineSample";
import type { SampleOp } from "./samples";
import {
  type Busy,
  type FeatureState,
  pipelineServerIn,
  pipelineServerStatus,
  pipelineServerUnmanaged,
  storageStatus,
} from "./featureState";

/** The storage's namespace and in-cluster address (pkg/cluster s3Namespace, s3Host). */
export const S3_NAMESPACE = "seaweedfs";
export const S3_ENDPOINT = "http://seaweedfs-s3.seaweedfs.svc:8333";
/** The pipeline server's name (pkg/cluster dspaName, the dashboard's and the one workbenches need), the earlier one (previousDSPAName) and its S3 Secret (dspaSecretName). */
export const DSPA_NAME = "dspa";
export const PREVIOUS_DSPA_NAME = "nightly-dspa";
export const DSPA_SECRET_NAME = "nightly-dspa-s3";
/** The dashboard S3 Connection set up with each pipeline server (pkg/cluster connectionSecretName, connectionDisplayName). */
export const CONNECTION_NAME = "rhoai-nightly-updater-s3";
export const CONNECTION_DISPLAY_NAME = "Shared S3 storage (RHOAI Nightly Updater)";

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

const joinNodes = (nodes: React.ReactNode[]) => nodes.map((n, i) => <React.Fragment key={i}>{i > 0 ? ", " : ""}{n}</React.Fragment>);

/** What the Pipelines tab needs from the page. */
export interface PipelinesTabProps {
  /** What Pipelines needs on the cluster (one-click prerequisites), above the parts. */
  prerequisites?: React.ReactNode;
  /** Why the feature cannot be set up until its cluster prerequisites are, or null. */
  prerequisitesReason?: string | null;
  status: ResourcesStatus;
  /** The selected project (the URL's, else the default), or null. */
  project: string | null;
  projects: ProjectOption[];
  /** The project list has loaded (an empty list then means there are none). */
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  /** Clears the project (the URL keeps an empty project=). */
  onClearProject: () => void;
  /** A project in the address that no longer exists (deleted elsewhere), or null. */
  missingProject: string | null;
  /** The status shown was read for another project: the project's Connection is still loading. */
  connectionLoading?: boolean;
  /** pipelinesState for the project: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  /** The user may only read: changes are hidden, and Set up says who can help. */
  readOnly: boolean;
  /** Why this user cannot read the S3 keys, or null. */
  readReason: string | null;
  /** Tool objects left after a storage deletion: Delete stays offered. */
  minioLeftovers: boolean;
  /** The OpenShift console's address, for links to pod logs. */
  consoleURL?: string;
  setPending: (p: Pending) => void;
  onMigrate: () => void;
  onRemoveOldStorage: () => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
  /** The sample operation running now, if any. */
  sampleOp: SampleOp | null;
  /** Changes when a sample operation ends, so the sample pipeline is read again. */
  samplesRefresh: number;
}

/** Delete is offered: the tool's storage exists, or storage of an earlier version does. */
export const storageTeardownOffered = (minio: ResourceState | undefined, legacy: LegacyStorageState | null | undefined, leftovers: boolean) =>
  !!minio && !isTerminating(minio) && ((minio.managedByTool !== false && (minio.deployed || leftovers)) || !!legacy);

/** Why Set up is not available to someone who may only read. */
export const READ_ONLY_SETUP = "Read-only access: setting this up needs a cluster-admin. Ask one to set it up for you.";

/** The storage an earlier version left in namespace minio: the only place that names MinIO-era objects. */
const LegacyStorageDetails: React.FC<{ legacy: LegacyStorageState }> = ({ legacy }) => {
  const pvcs = legacy.dataPVCs ?? [];
  const usedBy = legacy.usedBy ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  return (
    <FeatureSection title="Storage of an earlier version">
      <DescriptionList isCompact isHorizontal aria-label="Storage from an earlier version">
        <DescriptionListGroup>
          <DescriptionListTerm>Server</DescriptionListTerm>
          <DescriptionListDescription>{legacy.server ? `${legacy.server}${legacy.ready ? "" : " (not running)"}` : "No server"} in namespace <code>{legacy.namespace}</code>, at <code>{legacy.endpoint}</code></DescriptionListDescription>
        </DescriptionListGroup>
        {usedBy.length > 0 && (
          <DescriptionListGroup>
            <DescriptionListTerm>Used by</DescriptionListTerm>
            <DescriptionListDescription>{joinNodes(usedBy.map((u) => <code key={u}>{u}</code>))}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        <DescriptionListGroup>
          <DescriptionListTerm>Created by this tool</DescriptionListTerm>
          <DescriptionListDescription>{joinNodes(legacy.objects.map((o) => <code key={o}>{o}</code>))}</DescriptionListDescription>
        </DescriptionListGroup>
        {pvcs.length > 0 && (
          <DescriptionListGroup>
            <DescriptionListTerm>Data volumes</DescriptionListTerm>
            <DescriptionListDescription>
              {joinNodes(pvcs.map((p) => <React.Fragment key={p.name}>{pvcWithSize(p)}</React.Fragment>))}
              {uncopied.length > 0 && <>. {joinNodes(uncopied.map((n) => <code key={n}>{n}</code>))} holds MinIO&apos;s files from before SeaweedFS and cannot be copied.</>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
    </FeatureSection>
  );
};

/** Why migration is needed, at the top of the tab: the reason, that nothing stops meanwhile, and what Migrate does. */
const MigrationAlert: React.FC<{ legacy: LegacyStorageState; onMigrate: () => void; reason: string | null; readOnly: boolean }> = ({ legacy, onMigrate, reason, readOnly }) => {
  if (legacy.stage === "migrate") {
    return (
      <Alert variant="warning" isInline component="div" title="Your pipeline data is in storage from an earlier version"
        actionLinks={!readOnly && !reason ? <AlertActionLink onClick={onMigrate}>Migrate storage</AlertActionLink> : undefined}>
        <Content component="p">
          This version keeps pipeline data in the S3 storage in namespace <code>{S3_NAMESPACE}</code>; an earlier version put it in namespace <code>{legacy.namespace}</code>.
          Your pipeline servers keep working on the old storage meanwhile. Migrate storage copies every object to the new storage, checks each copy, and then moves the pipeline servers this tool created; nothing is deleted.
        </Content>
        {readOnly ? <Content component="p">A cluster-admin runs the migration.</Content> : reason && <Content component="p">Migrate storage is not available now: {reason}</Content>}
      </Alert>
    );
  }
  return (
    <Alert variant="info" isInline component="div" title="The old storage can be deleted">
      <Content component="p">
        The data was moved to the S3 storage. The storage of the earlier version, in namespace <code>{legacy.namespace}</code>, is kept until you delete it from the S3 storage&apos;s menu, after updating anything else that still uses it.
      </Content>
    </Alert>
  );
};

/** S3 storage (cluster): SeaweedFS in namespace seaweedfs, or storage of an earlier version to migrate. */
const StoragePart: React.FC<PipelinesTabProps> = ({ status, busy, baseReason, readOnly, readReason, minioLeftovers, consoleURL, setPending, onMigrate, onRemoveOldStorage }) => {
  const { minio, legacyStorage: legacy } = status;
  const unmanaged = notManaged(minio);
  const deployed = !!minio?.deployed;
  const actions: React.ReactNode[] = [];
  const menu: PartMenuItem[] = [];
  const legacyReason = busy.migrating ? "Migrate storage is running." : baseReason ?? (legacy?.terminating ? `Namespace ${legacy.namespace} is being deleted.` : null);
  if (legacy?.stage === "migrate" && !readOnly) {
    actions.push(<ActionListItem key="migrate"><TooltipButton variant="primary" onClick={onMigrate} isLoading={busy.migrating} disabledReason={busy.migrating ? null : legacyReason}>Migrate storage</TooltipButton></ActionListItem>);
  }
  const canRepair = deployed && !unmanaged && !legacy && (!!terminalReason(minio) || !!minio?.repairNeeded) && !isTerminating(minio);
  const repairReason = baseReason ?? minio?.setupBlockedReason ?? null;
  if (minio?.ready && minio.uiRoute) actions.push(<ActionListItem key="open"><ExternalLink href={minio.uiRoute}>Open admin UI</ExternalLink></ActionListItem>);
  if (!readOnly) {
    if (canRepair) menu.push({ key: "repair", label: "Repair", onClick: () => setPending({ kind: "repair-minio" }), disabledReason: repairReason });
    if (legacy?.stage === "remove") menu.push({ key: "remove-old", label: "Delete old storage…", onClick: onRemoveOldStorage, disabledReason: legacyReason || legacy.removeBlockedReason || null, isDanger: true });
    if (storageTeardownOffered(minio, legacy, minioLeftovers)) {
      menu.push({ key: "delete", label: "Delete S3 storage…", onClick: () => setPending({ kind: "teardown-minio" }), disabledReason: baseReason ?? minio!.teardownBlockedReason ?? null, isDanger: true });
    }
  }
  const blocked = !deployed && minio?.setupBlockedReason;
  return (
    <PartItem id="storage" title="S3 storage" scope="Shared by the cluster" status={storageStatus(status, busy)} unmanaged={unmanaged} actions={actions.length ? actions : undefined} menu={menu}>
      <Content component="p">Keeps pipeline artifacts and test files. Every pipeline server this tool sets up stores its artifacts here, in bucket <code>pipelines</code>.</Content>
      {blocked && (
        <Alert variant="warning" isInline component="div" title={`The S3 storage can't be set up: namespace ${S3_NAMESPACE} belongs to someone else`}>
          <Content component="p">{minio!.setupBlockedReason}</Content>
          <Content component="p">If nothing in it is used, a cluster-admin can delete it with <code>oc delete project {S3_NAMESPACE}</code>; otherwise ask its owner.</Content>
        </Alert>
      )}
      {legacy?.stage === "migrate" && (
        <Content component="p">Not set up yet: Migrate storage creates it, with new keys, and gives them to the pipeline servers it moves.</Content>
      )}
      {status.foreignLegacyNamespace && !legacy && (
        <Alert variant="info" isInline component="div" title="Namespace minio is not this tool's">
          <Content component="p">
            Namespace <code>minio</code> exists, but this tool did not create it (for example someone ran <code>oc new-project minio</code>), so whatever is in it is not used as storage from an earlier version: nothing is copied from it and nothing in it is changed. Setting up the S3 storage is not affected.
          </Content>
        </Alert>
      )}
      {minio && (!legacy || deployed) && (
        <ProblemAlerts kind="minio" state={minio} fix={canRepair && !readOnly ? { label: "Repair", onClick: () => setPending({ kind: "repair-minio" }), reason: repairReason } : null}
          logsHref={consolePodsHref(consoleURL, S3_NAMESPACE, "seaweedfs")} />
      )}
      {deployed && !minio?.ready && !terminalReason(minio) && !isTerminating(minio) && <StartingNote state={minio!} typical="under a minute" />}
      {deployed && (
        <DescriptionList isCompact isHorizontal aria-label="S3 storage details">
          <DescriptionListGroup>
            <DescriptionListTerm>Endpoint</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image={S3_ENDPOINT} what="S3 endpoint" truncate={false} /></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Bucket</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image="pipelines" what="bucket name" truncate={false} /></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Region</DescriptionListTerm>
            <DescriptionListDescription><code>us-east-1</code>, path-style addressing</DescriptionListDescription>
          </DescriptionListGroup>
          {!unmanaged && !readOnly && (
            <>
              <DescriptionListGroup>
                <DescriptionListTerm>Access key</DescriptionListTerm>
                <DescriptionListDescription><S3SecretField id="storage-access-key" field="accessKey" label="S3 access key" disabledReason={readReason} /></DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Secret key</DescriptionListTerm>
                <DescriptionListDescription>
                  <S3SecretField id="storage-secret-key" field="secretKey" label="S3 secret key" disabledReason={readReason} />
                </DescriptionListDescription>
              </DescriptionListGroup>
              {minio?.uiUser && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Admin UI login</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Stack>
                      <StackItem><Content component="p">User <code>{minio.uiUser}</code>, with a password of its own:</Content></StackItem>
                      <StackItem><S3SecretField id="storage-admin-password" field="password" label="admin UI password" disabledReason={readReason} /></StackItem>
                    </Stack>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              <DescriptionListGroup>
                <DescriptionListTerm>For a client</DescriptionListTerm>
                <DescriptionListDescription><CopyS3EnvButton endpoint={S3_ENDPOINT} disabledReason={readReason} /></DescriptionListDescription>
              </DescriptionListGroup>
            </>
          )}
          {!unmanaged && readOnly && (
            <DescriptionListGroup>
              <DescriptionListTerm>Keys</DescriptionListTerm>
              <DescriptionListDescription>Only cluster-admins can read the S3 keys here. Ask one for them.</DescriptionListDescription>
            </DescriptionListGroup>
          )}
        </DescriptionList>
      )}
      {legacy && <LegacyStorageDetails legacy={legacy} />}
    </PartItem>
  );
};

/**
 * The dashboard Connection as the Secret itself says (connectionState, read
 * for the selected project), never from the DSPA's annotation alone: it may
 * have been deleted in the dashboard since.
 */
const ConnectionState: React.FC<{ server: ResourceState; project: string; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ server, project, readOnly, baseReason, onSetUpAgain }) => {
  const name = <code>{CONNECTION_NAME}</code>;
  switch (server.connectionState) {
    case "present":
      return <>{name}, listed as &quot;{CONNECTION_DISPLAY_NAME}&quot;: an S3 connection with the storage&apos;s keys, endpoint and bucket <code>pipelines</code>. Usable from workbenches and model deployments in {project}; in-cluster address only, so it does not work from outside the cluster.</>;
    case "foreign":
      return <>A Secret {name} is in {project}, but it was not created by this tool, so it is not this storage&apos;s connection and the tool leaves it alone.</>;
    case "unreadable":
      return <>Cannot check {name}: {server.connectionMessage || "the Secret cannot be read"}.</>;
    default: {
      const why = server.connection ? <>Missing: {name} was deleted, for example in the dashboard.</> : <>None: {name} was not set up with this pipeline server.</>;
      if (server.nonStandardName) return <>{why} Recreate with the standard name creates it.</>;
      return (
        <Stack hasGutter>
          <StackItem>{why} Set up again creates it; runs and data are kept.</StackItem>
          {!readOnly && <StackItem><TooltipButton variant="secondary" onClick={onSetUpAgain} disabledReason={baseReason}>Set up again</TooltipButton></StackItem>}
        </Stack>
      );
    }
  }
};

/** The project's pipeline server, or a placeholder until a project is chosen. */
const PipelineServerPart: React.FC<PipelinesTabProps> = ({ status, project, busy, baseReason, readOnly, consoleURL, setPending, connectionLoading }) => {
  if (!project) {
    return (
      <PartItem id="pipeline-server" title="Pipeline server" scope="In the project you choose" status="not-set-up" unmanaged={false}>
        <Content component="p">Choose a project above to see its pipeline server, or to set one up there.</Content>
      </PartItem>
    );
  }
  const server = pipelineServerIn(status, project);
  const unmanaged = pipelineServerUnmanaged(status, project);
  const legacy = status.legacyStorage;
  const old = !!server && (legacy?.usedBy ?? []).includes(`${server.namespace}/${server.name}`);
  const storageReady = !!status.minio?.ready || (old && !!legacy?.ready);
  const managed = !!server && server.managedByTool !== false;
  const menu: PartMenuItem[] = managed && !readOnly && !isTerminating(server)
    ? [{ key: "delete", label: "Delete pipeline server…", onClick: () => setPending({ kind: "teardown-pipeline-server", project, name: server!.name }), disabledReason: baseReason ?? server!.teardownBlockedReason ?? null, isDanger: true }]
    : [];
  return (
    <PartItem id="pipeline-server" title="Pipeline server" scope={`In project ${project}`} status={pipelineServerStatus(status, project, busy)} unmanaged={unmanaged}
      actions={server?.ready && server.uiRoute && !isTerminating(server) ? <ActionListItem><ExternalLink href={server.uiRoute}>Open pipelines in {project}</ExternalLink></ActionListItem> : undefined}
      menu={menu}>
      {unmanaged
        ? <Content component="p">Created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : server
          ? <Content component="p">Runs your pipelines in {project} and keeps their artifacts in {old ? <>the storage of an earlier version, until Migrate storage moves it to the S3 storage</> : "the S3 storage"}.</Content>
          : <Content component="p">Not set up in {project} yet. Set up creates one that keeps its artifacts in the S3 storage.</Content>}
      {server && managed && server.nonStandardName && !isTerminating(server) && (
        <Alert variant="warning" isInline component="div" title="Workbench pipelines don't work with this pipeline server"
          actionLinks={!readOnly ? <AlertActionLink onClick={() => setPending({ kind: "recreate-pipeline-server", project, name: server.name ?? PREVIOUS_DSPA_NAME })} isDisabled={!!baseReason}>Recreate with the standard name</AlertActionLink> : undefined}>
          <Content component="p">
            An earlier version of this tool named it <code>{server.name ?? PREVIOUS_DSPA_NAME}</code>. Workbenches find their pipeline server only under the name <code>{DSPA_NAME}</code>, so Elyra pipelines from a workbench in {project} can&apos;t use it. The dashboard&apos;s pipelines pages and the KFP SDK work as they are.
          </Content>
          <Content component="p">
            {readOnly ? "A cluster-admin can recreate it with the standard name." : <>Recreate with the standard name deletes it, with its run history, and creates <code>{DSPA_NAME}</code> in its place.{baseReason ? ` Not available now: ${baseReason}` : ""}</>}
          </Content>
        </Alert>
      )}
      {server && <ProblemAlerts kind="pipeline" state={server} minioReady={storageReady} logsHref={consolePodsHref(consoleURL, server.namespace, `ds-pipeline-${server.name ?? DSPA_NAME}`)} />}
      {server && server.deployed && !server.ready && !terminalReason(server) && !isTerminating(server) && <StartingNote state={server} typical="1 to 3 minutes" />}
      {server && managed && !isTerminating(server) && (
        <DescriptionList isCompact isHorizontal aria-label="Pipeline server details">
          <DescriptionListGroup>
            <DescriptionListTerm>Name</DescriptionListTerm>
            <DescriptionListDescription>DataSciencePipelinesApplication <code>{server.name ?? DSPA_NAME}</code></DescriptionListDescription>
          </DescriptionListGroup>
          {(connectionLoading || server.connectionState) && (
            <DescriptionListGroup>
              <DescriptionListTerm>Connection in the dashboard</DescriptionListTerm>
              <DescriptionListDescription>
                {connectionLoading ? <Skeleton width="20rem" screenreaderText={`Loading the Connection in ${project}`} /> : (
                  <ConnectionState server={server} project={project} readOnly={readOnly} baseReason={baseReason}
                    onSetUpAgain={() => setPending({ kind: "setup-pipeline-server", project, again: true })} />
                )}
              </DescriptionListDescription>
            </DescriptionListGroup>
          )}
        </DescriptionList>
      )}
    </PartItem>
  );
};

/** How to use a ready pipeline server: the dashboard, and the KFP SDK. */
const UsePipelines: React.FC<{ server: ResourceState; project: string }> = ({ server, project }) => (
  <TabSection title="Use it" id="pipelines-use">
    <DescriptionList aria-label="How to use the pipeline server">
      {server.uiRoute && (
        <DescriptionListGroup>
          <DescriptionListTerm>In the dashboard</DescriptionListTerm>
          <DescriptionListDescription>
            <Content component="p">Open pipelines in {project} shows its pipelines and runs. Choose Import pipeline to upload a pipeline (a compiled YAML file), then Create run.</Content>
          </DescriptionListDescription>
        </DescriptionListGroup>
      )}
      {server.apiRoute && server.name && (
        <>
          <DescriptionListGroup>
            <DescriptionListTerm>API endpoint</DescriptionListTerm>
            <DescriptionListDescription>
              <ClipboardCopy isReadOnly hoverTip="Copy" clickTip="Copied" variant="inline-compact" aria-label={`Pipeline API endpoint of ${project}`}>{server.apiRoute}</ClipboardCopy>
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>From Python (KFP SDK)</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Use a token that may only use this server&apos;s API, not your OpenShift token: whoever can edit {project} controls what answers at the server&apos;s address. As an admin of {project}, run these commands to make a ServiceAccount with only that permission and a token for it that expires in 15 minutes, then start Python in the same shell.</Content></StackItem>
                <StackItem><CopyableCode id="pipelines-kfp-token" label="the token commands" code={pipelineTokenCommands(project, server.name)} /></StackItem>
                <StackItem><CopyableCode id="pipelines-kfp" label="the Python snippet" code={kfpClientSnippet(server.apiRoute)} /></StackItem>
                <StackItem><Content component="p">To remove them afterwards: <code>{pipelineTokenRemoval(project)}</code></Content></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        </>
      )}
    </DescriptionList>
  </TabSection>
);

/**
 * The Pipelines tab: what it needs (a turned-on component, a project), S3
 * storage (cluster) and the selected project's pipeline server, each with
 * its health, actions and details; Set up for what is missing; how to use
 * it once ready; what it creates; and Clean up.
 */
export const PipelinesTab: React.FC<PipelinesTabProps> = (props) => {
  const { status, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, busy, baseReason, readOnly, onSetUp, onMigrate, onCleanup, minioLeftovers } = props;
  const { minio, legacyStorage: legacy } = status;
  const server = project ? pipelineServerIn(status, project) : undefined;
  const next = state.next;
  const toolServers = (status.pipelineServers ?? []).filter((ps) => ps.managedByTool !== false);
  const anythingToClean = toolServers.length > 0 || storageTeardownOffered(minio, legacy, minioLeftovers);
  const noProjects = projectsLoaded && projects.length === 0;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Pipelines" purpose={FEATURE_PURPOSE.pipelines} state={state} /></StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {status.pipelinesBlocked && <StackItem><ComponentAlert feature="Data Science Pipelines" block={status.pipelinesBlocked} /></StackItem>}
      {legacy && <StackItem><MigrationAlert legacy={legacy} onMigrate={onMigrate} readOnly={readOnly} reason={busy.migrating ? "Migrate storage is running." : baseReason} /></StackItem>}
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} /></StackItem>}
      {missingProject && (
        <StackItem>
          <Alert variant="info" isInline component="p" title={`Project ${missingProject} no longer exists`}>
            It was deleted, or it is no longer a data science project. Choose another project.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="pipelines-project">
            <ProjectSelect id="pipelines-project" projects={projects} selected={project} onSelect={onProject} onClear={onClearProject} />
            <FormHelperText>
              <HelperText><HelperTextItem>Data science projects (made in the RHOAI dashboard). Each has its own pipeline server.</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
        </Form>
      </StackItem>
      <StackItem>
        <PartList aria-label="What Pipelines needs">
          <StoragePart {...props} />
          <PipelineServerPart {...props} />
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} variant={props.prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? props.prerequisitesReason ?? null} />
        </StackItem>
      )}
      {project && server?.ready && !isTerminating(server) && (
        <>
          <StackItem><UsePipelines server={server} project={project} /></StackItem>
          <StackItem>
            <PipelineSample project={project} dspa={server.name} apiRoute={server.apiRoute} uiRoute={server.uiRoute} />
          </StackItem>
          <StackItem>
            <WhatNext id="pipelines-next" items={[
              <>Run the sample pipeline above to check that runs and their artifacts work in {project}.</>,
              <>Open pipelines in {project}, then choose Import pipeline to upload a compiled pipeline.</>,
              <>Create a run; its steps write their artifacts to the S3 storage.</>,
              <>Follow the run and open its artifacts from the run&apos;s page.</>,
            ]} />
          </StackItem>
        </>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster: namespace <code>{S3_NAMESPACE}</code>, labelled as managed by this tool, with Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-data</code> (20Gi), Secret <code>seaweedfs-s3-credentials</code> (the S3 keys), Service <code>seaweedfs-s3</code>, Route <code>seaweedfs-admin</code> (admin UI), NetworkPolicy <code>seaweedfs-ingress</code> and bucket <code>pipelines</code></>,
            <>In the project: DataSciencePipelinesApplication <code>{DSPA_NAME}</code> (the pipeline server) and Secret <code>{DSPA_SECRET_NAME}</code> (a copy of the S3 keys), and the dashboard S3 Connection <code>{CONNECTION_NAME}</code> (the same keys, for workbenches and model deployments), both owned by the pipeline server</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && anythingToClean && (
        <StackItem>
          <CleanupSection id="pipelines-cleanup" label="Clean up Pipelines…" onClick={onCleanup} reason={baseReason}
            text="Deletes what this tool created for Pipelines: the pipeline servers in every project, then the S3 storage. You choose what goes, then it runs as one operation." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The confirmation of a Storage change, or null for another feature's change. */
export function storageConfirm(pending: Pending, minio: ResourceState | undefined, legacy?: LegacyStorageState | null, setPending?: (p: Pending) => void): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-minio":
    case "repair-minio":
      return {
        title: pending.kind === "repair-minio" ? "Repair the S3 storage?" : "Set up the S3 storage?",
        confirm: pending.kind === "repair-minio" ? "Repair" : "Set up",
        runTitle: pending.kind === "repair-minio" ? "Repair the S3 storage" : "Set up the S3 storage",
        summary: pending.kind === "repair-minio"
          ? <Content component="p">Sets up the S3 storage again with this version&apos;s settings and image, and starts it. Stored data is kept. Takes up to 90 seconds.</Content>
          : <Content component="p">On the cluster: S3 storage (20 GiB), shared by every pipeline server this tool sets up. Takes up to 90 seconds.</Content>,
        changes: [
          <>Namespace <code>{S3_NAMESPACE}</code> {pending.kind === "repair-minio" || minio?.managedByTool ? "is kept and reused" : "is created"}, labelled as managed by this tool.</>,
          <>SeaweedFS: Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-data</code>, Secret <code>seaweedfs-s3-credentials</code> (the S3 keys) and NetworkPolicy <code>seaweedfs-ingress</code> are applied with the image the server is configured to use.</>,
          <>Once SeaweedFS is ready: Service <code>seaweedfs-s3</code> (the S3 endpoint <code>{S3_ENDPOINT}</code>), the admin UI Route <code>seaweedfs-admin</code> and the bucket <code>pipelines</code>.</>,
        ],
      };
    case "teardown-minio": {
      const current = !!minio && (minio.deployed || pvcList(minio).length > 0);
      // Namespace seaweedfs goes too only when the user ticks it, and it is
      // offered only for the namespace this tool created (pkg/cluster
      // toolNamespaceDeletable): the same informed choice as for minio.
      const offerNamespace = current && minio?.managedByTool !== false && !!minio?.namespaceDeletable;
      const deleteNamespace = offerNamespace && pending.deleteNamespace === true;
      const others = minio?.otherObjects ?? [];
      const namespaces = [...(current && !deleteNamespace ? [S3_NAMESPACE] : []), ...(legacy ? [legacy.namespace] : [])];
      const pvcs = [...(current ? pvcList(minio).map((n) => `${S3_NAMESPACE}/${n}`) : []), ...(legacy?.dataPVCs ?? []).map((p) => `${legacy!.namespace}/${p.name}`)];
      return {
        title: "Delete the S3 storage?", confirm: "Delete", danger: true, runTitle: "Delete the S3 storage",
        summary: (
          <Content component="p">
            Deletes the S3 storage{legacy ? ", and the storage of an earlier version," : ""} with every file in it.{" "}
            {namespaces.length > 0
              ? <>{namespaces.length > 1 ? "Namespaces" : "Namespace"} {joinNodes(namespaces.map((n) => <code key={n}>{n}</code>))} {namespaces.length > 1 ? "are" : "is"} kept{offerNamespace && !deleteNamespace ? " unless you tick the box below" : ""}.</>
              : <>Namespace <code>{S3_NAMESPACE}</code> is deleted too.</>}
          </Content>
        ),
        changes: [
          ...(current ? [<>The S3 storage objects this tool created in namespace <code>{S3_NAMESPACE}</code> are deleted: Deployment <code>seaweedfs</code>, NetworkPolicy <code>seaweedfs-ingress</code>, Route <code>seaweedfs-admin</code>, Service <code>seaweedfs-s3</code>, Secret <code>seaweedfs-s3-credentials</code> and PersistentVolumeClaim <code>seaweedfs-data</code>.</>] : []),
          ...(legacy ? [<>The storage an earlier version left in namespace <code>{legacy.namespace}</code> is deleted: {joinNodes(legacy.objects.map((o) => <code key={o}>{o}</code>))}.</>] : []),
          ...(deleteNamespace ? [<>Then namespace <code>{S3_NAMESPACE}</code> is deleted, with everything in it.</>] : []),
          <>Objects with these names that the tool did not create are kept.{namespaces.length > 0 && <> To delete a kept namespace later: {joinNodes(namespaces.map((n) => <code key={n}>oc delete project {n}</code>))}, once it is empty.</>}</>,
        ],
        choice: !current ? undefined : offerNamespace ? (
          <Checkbox id="teardown-delete-namespace" label={<>Also delete namespace <code>{S3_NAMESPACE}</code> (created by this tool)</>}
            description={<>
              Deletes namespace <code>{S3_NAMESPACE}</code> and everything in it, including anything this tool did not create.{" "}
              {others.length > 0
                ? <>Besides the storage&apos;s objects, this tool sees there: {joinNodes(others.map((o) => <code key={o}>{o}</code>))}.</>
                : <>Besides the storage&apos;s objects, this tool sees no Deployments or PersistentVolumeClaims there.</>}{" "}
              It cannot list every kind (Secrets, ConfigMaps and others): check with <code>oc get all,pvc,secret,configmap -n {S3_NAMESPACE}</code>.
            </>}
            isChecked={deleteNamespace} onChange={(_e, v) => setPending?.({ kind: "teardown-minio", deleteNamespace: v })} />
        ) : minio?.namespaceKeptReason ? <Content component="p">{minio.namespaceKeptReason}</Content> : undefined,
        dataLoss: pvcs.length > 0
          ? <>Every object stored in it: pipeline artifacts and uploaded files{legacy ? ", and the data of earlier versions" : ""} (volume{pvcs.length > 1 ? "s" : ""} {joinNodes(pvcs.map((n) => <code key={n}>{n}</code>))}).</>
          : <>Every object stored in the S3 storage (pipeline artifacts, uploaded files).</>,
      };
    }
    default:
      return null;
  }
}

/** The confirmation of a pipeline server change, or null for another feature's change. */
export function pipelineServerConfirm(pending: Pending, pipelineServers: ResourceState[]): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-pipeline-server":
      if (pending.again) {
        return {
          title: `Set up the pipeline server in ${pending.project} again?`, confirm: "Set up again", runTitle: `Set up the pipeline server in ${pending.project} again`,
          summary: <Content component="p">Applies this tool&apos;s settings to the pipeline server in {pending.project} again and creates what is missing; runs and data are kept.</Content>,
          changes: [
            <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{DSPA_NAME}</code> and Secret <code>{DSPA_SECRET_NAME}</code> are applied again.</>,
            <>The dashboard S3 Connection <code>{CONNECTION_NAME}</code> ({CONNECTION_DISPLAY_NAME}) is created again, owned by the pipeline server.</>,
          ],
        };
      }
      return {
        title: `Set up the pipeline server in ${pending.project}?`, confirm: "Set up", runTitle: `Set up the pipeline server in ${pending.project}`,
        summary: <Content component="p">In {pending.project}: a pipeline server that keeps its artifacts in the S3 storage. Takes 1 to 3 minutes.</Content>,
        changes: [
          <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{DSPA_NAME}</code> (the name workbench pipelines use) and Secret <code>{DSPA_SECRET_NAME}</code> (owned by it) are created, storing artifacts in the S3 storage (Service <code>seaweedfs-s3</code> in namespace <code>seaweedfs</code>).</>,
          <>Also the dashboard S3 Connection <code>{CONNECTION_NAME}</code> ({CONNECTION_DISPLAY_NAME}), owned by the pipeline server: the same keys, usable from workbenches and model deployments in {pending.project}.</>,
        ],
      };
    case "recreate-pipeline-server": {
      const ps = pipelineServers.find((p) => p.namespace === pending.project);
      return {
        title: `Recreate the pipeline server in ${pending.project} with the standard name?`, confirm: "Recreate", danger: true,
        runTitle: `Recreate the pipeline server in ${pending.project}`,
        summary: <Content component="p">Replaces pipeline server <code>{pending.name}</code> in {pending.project} with one named <code>{DSPA_NAME}</code>, which workbench pipelines use. Everything is checked first; nothing is deleted if the new one would be refused. Takes 1 to 3 minutes, plus the old one&apos;s cleanup.</Content>,
        changes: [
          <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name}</code> and Secret <code>{DSPA_SECRET_NAME}</code> are deleted; its database goes with it.</>,
          <>Then DataSciencePipelinesApplication <code>{DSPA_NAME}</code> is created, storing artifacts in the same S3 storage, with Secret <code>{DSPA_SECRET_NAME}</code> and the dashboard S3 Connection <code>{CONNECTION_NAME}</code>, both owned by it.</>,
        ],
        dataLoss: <>
          The run history of <code>{pending.name}</code>: its runs, experiments and imported pipelines{pvcList(ps).length > 0 ? <> (volume {pvcList(ps).map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>)})</> : null}.
          Artifacts already in the S3 storage stay there (bucket <code>pipelines</code>), but no run links to them any more.
        </>,
      };
    }
    case "teardown-pipeline-server": {
      const ps = pipelineServers.find((p) => p.namespace === pending.project);
      return {
        title: `Delete the pipeline server in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete the pipeline server in ${pending.project}`,
        summary: <Content component="p">Deletes the pipeline server in {pending.project}, with its runs and experiments. The project and the S3 storage stay.</Content>,
        changes: [<>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name || DSPA_NAME}</code> and its S3 credentials Secret are deleted, with the dashboard S3 Connection <code>{CONNECTION_NAME}</code> this tool created (workbenches or model deployments that use it lose it). The project stays.</>],
        dataLoss: pvcsLost(ps, "the pipeline database: runs, experiments and their history") ?? <>The pipeline database: runs, experiments and their history.</>,
      };
    }
    default:
      return null;
  }
}
