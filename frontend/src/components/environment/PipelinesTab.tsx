import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
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
import type { ConnectionWorkbench, ResourceState, ResourcesStatus } from "../../types";
import { TooltipButton } from "../TooltipButton";
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
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, READ_ONLY_SETUP, TabSection, WhatNext } from "./FeatureLayout";
import { OutsideProjectsAlert } from "./OutsideProjects";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, CopyableCode, NoProjectsAlert, kfpClientSnippet } from "./UseIt";
import { PipelineSample, pipelineTokenCommands, pipelineTokenRemoval } from "./PipelineSample";
import type { SampleOp } from "./samples";
import { MigrationPointer, S3_NAMESPACE, StoragePrerequisitePart } from "./StorageTab";
import {
  type Busy,
  type FeatureState,
  pipelineServerIn,
  pipelineServerStatus,
  pipelineServerUnmanaged,
} from "./featureState";

export { READ_ONLY_SETUP } from "./FeatureLayout";

/** Standard DataSciencePipelinesApplication name (pkg/cluster dspaName) that the dashboard and workbenches look for, the older default (previousDSPAName), and the S3 Secret object name (dspaSecretName). */
export const DSPA_NAME = "dspa";
export const PREVIOUS_DSPA_NAME = "nightly-dspa";
export const DSPA_SECRET_NAME = "nightly-dspa-s3";
/** Dashboard S3 Connection bundled with every pipeline server install (pkg/cluster connectionSecretName, connectionDisplayName). */
export const CONNECTION_NAME = "rhoai-nightly-updater-s3";
export const CONNECTION_DISPLAY_NAME = "Shared S3 storage (RHOAI Nightly Updater)";

/** Props the Pipelines tab receives from its parent page. */
export interface PipelinesTabProps {
  /** Cluster-side one-click prerequisites for Pipelines, rendered above the part list. */
  prerequisites?: React.ReactNode;
  /** When set, explains why setup must wait until cluster prerequisites finish; otherwise null. */
  prerequisitesReason?: string | null;
  status: ResourcesStatus;
  /** Active project from the URL when present, otherwise the default; null if unset. */
  project: string | null;
  projects: ProjectOption[];
  /** True once the project dropdown has finished loading—an empty array then means zero projects. */
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  /** Drops the current project selection while leaving project= empty in the URL. */
  onClearProject: () => void;
  /** Project slug from the URL that is not in the list anymore (removed outside this UI), or null. */
  missingProject: string | null;
  /** Displayed status belongs to a different project while that project's Connection fetch is in flight. */
  connectionLoading?: boolean;
  /** Per-project pipelinesState, matching what Overview shows. */
  state: FeatureState;
  busy: Busy;
  /** Present when mutations are blocked; explains why, or null when edits are allowed. */
  baseReason: string | null;
  /** View-only mode: hide change controls; Set up surfaces who can perform setup. */
  readOnly: boolean;
  /** OpenShift console base URL used for pod log links. */
  consoleURL?: string;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
  /** In-flight sample operation, if one is running. */
  sampleOp: SampleOp | null;
  /** Bumped when a sample operation completes so the sample pipeline state is re-fetched. */
  samplesRefresh: number;
}

/**
 * Reflect the dashboard Connection from what the Secret actually reports
 * (connectionState, scoped to the selected project)—not from DSPA annotations alone,
 * since the Connection may have been removed in the dashboard afterward.
 */
const ConnectionState: React.FC<{ server: ResourceState; project: string; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ server, project, readOnly, baseReason, onSetUpAgain }) => {
  const name = <code>{CONNECTION_NAME}</code>;
  switch (server.connectionState) {
    case "present": {
      const users = server.connectionWorkbenches ?? [];
      return (
        <Stack hasGutter>
          <StackItem>{name}, listed as &quot;{CONNECTION_DISPLAY_NAME}&quot;: an S3 connection with the storage&apos;s keys, endpoint and bucket <code>pipelines</code>. Usable from workbenches and model deployments in {project}; in-cluster address only, so it does not work from outside the cluster.</StackItem>
          {users.length > 0 && (
            <StackItem>Used by {users.length === 1 ? "workbench" : "workbenches"} {workbenchList(null, users)}. Deleting the pipeline server removes the Connection from {users.length === 1 ? "it" : "them"} first; running workbenches restart.</StackItem>
          )}
          {(server.connectionEnvOnlyWorkbenches ?? []).length > 0 && (
            <StackItem>Also loaded without the annotation by {server.connectionEnvOnlyWorkbenches!.map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>)}: they will fail to start after the pipeline server is deleted; edit them in the dashboard and remove the Connection.</StackItem>
          )}
          {server.connectionWorkbenchesMessage && <StackItem>Cannot check which workbenches use it: {server.connectionWorkbenchesMessage}</StackItem>}
        </Stack>
      );
    }
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

/** What a delete of server confirms about the workbenches that use its Connection (as the status lists them). */
function connectionWorkbenchesOf(server: ResourceState): { workbenches?: ConnectionWorkbench[]; envOnly?: string[] } {
  return {
    ...(server.connectionWorkbenches?.length ? { workbenches: server.connectionWorkbenches } : {}),
    ...(server.connectionEnvOnlyWorkbenches?.length ? { envOnly: server.connectionEnvOnlyWorkbenches } : {}),
  };
}

/** A list of workbench names, each in code, running ones marked. */
function workbenchList(project: string | null, workbenches: ConnectionWorkbench[]): React.ReactNode {
  return workbenches.map((w, i) => <React.Fragment key={w.name}>{i > 0 ? ", " : ""}<code>{project ? `${project}/${w.name}` : w.name}</code>{w.running ? " (running)" : ""}</React.Fragment>);
}

/** The confirmation's line for the workbenches that lose the dashboard Connection before it is deleted. */
export function connectionWorkbenchesChange(project: string, workbenches: ConnectionWorkbench[]): React.ReactNode {
  return <>These workbenches use the Connection <code>{CONNECTION_NAME}</code>; it is removed from them first: {workbenchList(project, workbenches)}. Running workbenches restart; work not saved to disk is lost. Their other connections stay; attach it again in the dashboard once a new pipeline server made it again.</>;
}

/** The confirmation's line for the workbenches that load the Connection without the annotation (nothing changes them). */
export function connectionEnvOnlyChange(project: string, names: string[]): React.ReactNode {
  return <>These workbenches use the Connection without the annotation: {names.map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{project}/{n}</code></React.Fragment>)}. They will fail to start after the deletion; edit them in the dashboard and remove the Connection.</>;
}

/** Renders the selected project's pipeline server, or guidance until a project is picked. */
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
    ? [{ key: "delete", label: "Delete pipeline server…", onClick: () => setPending({ kind: "teardown-pipeline-server", project, name: server!.name, ...(server!.unrecorded ? { unrecorded: true, uid: server!.uid } : {}), ...connectionWorkbenchesOf(server!) }), disabledReason: baseReason ?? server!.teardownBlockedReason ?? null, isDanger: true }]
    : [];
  return (
    <PartItem id="pipeline-server" title="Pipeline server" scope={`In project ${project}`} status={pipelineServerStatus(status, project, busy)} unmanaged={unmanaged}
      actions={server?.ready && server.uiRoute && !isTerminating(server) ? <ActionListItem><ExternalLink href={server.uiRoute}>Open pipelines in {project}</ExternalLink></ActionListItem> : undefined}
      menu={menu}>
      {unmanaged
        ? <Content component="p">Created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : server
          ? <Content component="p">Runs your pipelines in {project} and keeps their artifacts in {old ? <>the storage of an earlier version, until Migrate storage moves them to the S3 storage</> : "the S3 storage"}.</Content>
          : <Content component="p">Not set up in {project} yet. Set up creates one that keeps its artifacts in the S3 storage.</Content>}
      {server && managed && server.nonStandardName && !isTerminating(server) && (
        <Alert variant="warning" isInline component="div" title="Workbench pipelines don't work with this pipeline server"
          actionLinks={!readOnly ? <AlertActionLink onClick={() => setPending({ kind: "recreate-pipeline-server", project, name: server.name ?? PREVIOUS_DSPA_NAME, ...(server.unrecorded ? { unrecorded: true, uid: server.uid } : {}), ...connectionWorkbenchesOf(server) })} isDisabled={!!baseReason}>Recreate with the standard name</AlertActionLink> : undefined}>
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

/** Instructions for a ready pipeline server: dashboard UI and KFP SDK usage. */
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
 * The Pipelines tab: what it needs (a turned-on component, a project), the
 * S3 storage's health (its details and changes are on its own tab) and the
 * selected project's pipeline server, with its health, actions and details;
 * Set up for what is missing (the storage first, when it is missing); how
 * to use it once ready; what it creates; and Clean up.
 */
export const PipelinesTab: React.FC<PipelinesTabProps> = (props) => {
  const { status, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, busy, baseReason, readOnly, onSetUp, onCleanup } = props;
  const legacy = status.legacyStorage;
  const migrating = legacy?.stage === "migrate";
  const server = project ? pipelineServerIn(status, project) : undefined;
  const next = state.next;
  const toolServers = (status.pipelineServers ?? []).filter((ps) => ps.managedByTool !== false);
  const outside = status.outsidePipelineServers ?? [];
  // The S3 storage is the cluster's, not Pipelines': its Delete is on its own tab and in Clean up everything.
  const anythingToClean = toolServers.length > 0 || outside.length > 0;
  const noProjects = projectsLoaded && projects.length === 0;
  return (
    <Stack hasGutter>
      <StackItem>
        <FeatureHeader title="Pipelines" state={state}
          purpose={migrating ? "Run data science pipelines in a project; their artifacts stay in the storage of an earlier version until Migrate storage moves them to the cluster's S3 storage." : FEATURE_PURPOSE.pipelines} />
      </StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {status.pipelinesBlocked && <StackItem><ComponentAlert feature="Data Science Pipelines" block={status.pipelinesBlocked} /></StackItem>}
      {migrating && <StackItem><MigrationPointer /></StackItem>}
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} things="Pipeline servers" /></StackItem>}
      {outside.length > 0 && (
        <StackItem>
          <OutsideProjectsAlert readOnly={readOnly} reason={baseReason} items={outside.map((ps) => ({
            key: `${ps.namespace}/${ps.name}`,
            text: <>Pipeline server <code>{ps.name ?? DSPA_NAME}</code> in project {ps.namespace} (no longer a data science project)</>,
            onDelete: () => props.setPending({ kind: "teardown-pipeline-server", project: ps.namespace!, name: ps.name, ...(ps.unrecorded ? { unrecorded: true, uid: ps.uid } : {}) }),
          }))} />
        </StackItem>
      )}
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
          <StoragePrerequisitePart status={status} busy={busy} />
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
              <>Create a run; its steps write their artifacts to the pipeline server&apos;s storage.</>,
              <>Follow the run and open its artifacts from the run&apos;s page.</>,
            ]} />
          </StackItem>
        </>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster, when it is missing: the S3 storage in namespace <code>{S3_NAMESPACE}</code> (the S3 storage tab lists its objects)</>,
            <>In the project: DataSciencePipelinesApplication <code>{DSPA_NAME}</code> (the pipeline server) and Secret <code>{DSPA_SECRET_NAME}</code> (a copy of the S3 keys), and the dashboard S3 Connection <code>{CONNECTION_NAME}</code> (the same keys, for workbenches and model deployments), both owned by the pipeline server</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && anythingToClean && (
        <StackItem>
          <CleanupSection id="pipelines-cleanup" label="Clean up Pipelines…" onClick={onCleanup} reason={baseReason}
            text={migrating
              ? "Deletes the pipeline servers this tool set up, in every project. You choose which, then it runs as one operation. The storage they use, from an earlier version, stays; delete it on the S3 storage tab."
              : "Deletes the pipeline servers this tool set up, in every project. You choose which, then it runs as one operation. The S3 storage stays: other features can use it. Delete it on the S3 storage tab, or with Clean up everything."} />
        </StackItem>
      )}
    </Stack>
  );
};

/** A pipeline server with this tool's label that this tool has no record of: deleting it is the user's confirmation that it is this tool's. */
export function unrecordedPipelineServerNote(project: string): React.ReactNode {
  return <>This pipeline server carries this tool&apos;s label, but this tool has no record that it set it up (set up before version 3.0.1, or labelled by someone in {project}). Confirm only if it is this tool&apos;s.</>;
}

/** The confirmation of a pipeline server change, or null for another feature's change. */
export function pipelineServerConfirm(pending: Pending, pipelineServers: ResourceState[]): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-pipeline-server":
      if (pending.again) {
        const adopted = pipelineServers.find((p) => p.namespace === pending.project)?.unrecorded;
        return {
          title: `Set up the pipeline server in ${pending.project} again?`, confirm: "Set up again", runTitle: `Set up the pipeline server in ${pending.project} again`,
          summary: <Content component="p">Applies this tool&apos;s settings to the pipeline server in {pending.project} again and creates what is missing; runs and data are kept.</Content>,
          changes: [
            <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{DSPA_NAME}</code> and Secret <code>{DSPA_SECRET_NAME}</code> are applied again.</>,
            <>The dashboard S3 Connection <code>{CONNECTION_NAME}</code> ({CONNECTION_DISPLAY_NAME}) is created again, owned by the pipeline server.</>,
            ...(adopted
              ? [<>This pipeline server carries this tool&apos;s label, but this tool has no record that it set it up (set up before version 3.0.1, or labelled by someone in {pending.project}). Set up again adopts it as this tool&apos;s and writes the S3 storage&apos;s keys into {pending.project}, as a new setup there does.</>]
              : []),
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
        summary: <>
          <Content component="p">Replaces pipeline server <code>{pending.name}</code> in {pending.project} with one named <code>{DSPA_NAME}</code>, which workbench pipelines use. Everything is checked first; nothing is deleted if the new one would be refused. Takes 1 to 3 minutes, plus the old one&apos;s cleanup.</Content>
          {pending.unrecorded && <Content component="p">{unrecordedPipelineServerNote(pending.project)}</Content>}
          {!!pending.workbenches?.length && <Content component="p">{connectionWorkbenchesChange(pending.project, pending.workbenches)}</Content>}
          {!!pending.envOnly?.length && <Content component="p">{connectionEnvOnlyChange(pending.project, pending.envOnly)}</Content>}
        </>,
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
        summary: <>
          <Content component="p">Deletes the pipeline server in {pending.project}, with its runs and experiments. The project and the S3 storage stay.</Content>
          {pending.unrecorded && <Content component="p">{unrecordedPipelineServerNote(pending.project)}</Content>}
          {!!pending.workbenches?.length && <Content component="p">{connectionWorkbenchesChange(pending.project, pending.workbenches)}</Content>}
          {!!pending.envOnly?.length && <Content component="p">{connectionEnvOnlyChange(pending.project, pending.envOnly)}</Content>}
        </>,
        changes: [<>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name || DSPA_NAME}</code> and its S3 credentials Secret are deleted, with the dashboard S3 Connection <code>{CONNECTION_NAME}</code> this tool created (workbenches or model deployments that use it lose it). The project stays.</>],
        dataLoss: pvcsLost(ps, "the pipeline database: runs, experiments and their history") ?? <>The pipeline database: runs, experiments and their history.</>,
      };
    }
    default:
      return null;
  }
}
