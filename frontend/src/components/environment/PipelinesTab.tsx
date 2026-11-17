import React from "react";
import {
  ActionListItem,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  EmptyState,
  EmptyStateBody,
  Form,
  FormGroup,
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
  isTerminating,
  pvcList,
  pvcsLost,
  terminalReason,
} from "./resourceRow";
import { ActionRow, CreatesList, FeatureHeader, FeatureSection, PartItem, PartList, PrimaryAction } from "./FeatureLayout";
import { AdminPassword } from "./AdminLogin";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { notManaged } from "./resourceStatus";
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

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

const joinNodes = (nodes: React.ReactNode[]) => nodes.map((n, i) => <React.Fragment key={i}>{i > 0 ? ", " : ""}{n}</React.Fragment>);

/** What the Pipelines tab needs from the page. */
export interface PipelinesTabProps {
  status: ResourcesStatus;
  /** The selected project (the URL's, else the default), or null. */
  project: string | null;
  projects: ProjectOption[];
  onProject: (project: string) => void;
  /** pipelinesState for the project: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  /** Why this user cannot read the admin password, or null. */
  readReason: string | null;
  /** Tool objects left after a storage teardown: Tear down stays offered. */
  minioLeftovers: boolean;
  setPending: (p: Pending) => void;
  onMigrate: () => void;
  onSetUp: (setup: Pending[]) => void;
}

/** Tear down is offered: the tool's storage exists, or storage of an earlier version does. */
const storageTeardownOffered = (minio: ResourceState | undefined, legacy: LegacyStorageState | null | undefined, leftovers: boolean) =>
  !!minio && !isTerminating(minio) && ((minio.managedByTool !== false && (minio.deployed || leftovers)) || !!legacy);

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

/** S3 storage (cluster): SeaweedFS in namespace seaweedfs, or storage of an earlier version to migrate. */
const StoragePart: React.FC<PipelinesTabProps> = ({ status, busy, baseReason, readReason, setPending, onMigrate }) => {
  const { minio, legacyStorage: legacy } = status;
  const unmanaged = notManaged(minio);
  const deployed = !!minio?.deployed;
  const actions: React.ReactNode[] = [];
  if (legacy) {
    const reason = busy.migrating ? null : baseReason ?? (legacy.terminating ? `Namespace ${legacy.namespace} is being deleted.` : null);
    actions.push(legacy.stage === "migrate"
      ? <ActionListItem key="migrate"><TooltipButton variant="primary" onClick={onMigrate} isLoading={busy.migrating} disabledReason={reason}>Migrate storage</TooltipButton></ActionListItem>
      : <ActionListItem key="remove"><TooltipButton variant="secondary" isDanger onClick={onMigrate} isLoading={busy.migrating} disabledReason={reason}>Remove old storage</TooltipButton></ActionListItem>);
  } else if (deployed && !unmanaged && (!!terminalReason(minio) || !!minio?.repairNeeded) && !isTerminating(minio)) {
    actions.push(<ActionListItem key="repair"><TooltipButton variant="secondary" onClick={() => setPending({ kind: "repair-minio" })} isLoading={busy.resAction === "setup-minio"}
      disabledReason={baseReason ?? minio?.setupBlockedReason ?? null}>Repair</TooltipButton></ActionListItem>);
  }
  if (minio?.ready && minio.uiRoute) actions.push(<ActionListItem key="open"><ExternalLink href={minio.uiRoute}>Open admin UI</ExternalLink></ActionListItem>);
  return (
    <PartItem id="storage" title="S3 storage" scope="cluster" status={storageStatus(status, busy)} unmanaged={unmanaged} actions={actions.length ? actions : undefined}>
      <Content component="p">SeaweedFS in namespace <code>{S3_NAMESPACE}</code>, for pipeline artifacts and test files.</Content>
      {legacy?.stage === "migrate" && (
        <Content component="p">An earlier version&apos;s storage, in namespace <code>{legacy.namespace}</code>, still holds the data. Migrate storage moves it here, with the pipeline servers this tool created.</Content>
      )}
      {legacy?.stage === "remove" && (
        <Content component="p">The data was moved here. The earlier version&apos;s storage is kept until you remove it.</Content>
      )}
      {minio && (!legacy || deployed) && <ProblemAlerts kind="minio" state={minio} />}
      {deployed && (
        <DescriptionList isCompact isHorizontal aria-label="S3 storage details">
          <DescriptionListGroup>
            <DescriptionListTerm>Endpoint</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image={S3_ENDPOINT} what="S3 endpoint" truncate={false} /></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Bucket</DescriptionListTerm>
            <DescriptionListDescription><code>pipelines</code>, region <code>us-east-1</code>, path-style; keys in Secret <code>seaweedfs-s3-credentials</code></DescriptionListDescription>
          </DescriptionListGroup>
          {minio?.uiUser && !unmanaged && (
            <>
              <DescriptionListGroup>
                <DescriptionListTerm>Admin UI user</DescriptionListTerm>
                <DescriptionListDescription><code>{minio.uiUser}</code></DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Password</DescriptionListTerm>
                <DescriptionListDescription><AdminPassword user={minio.uiUser} disabledReason={readReason} /></DescriptionListDescription>
              </DescriptionListGroup>
            </>
          )}
        </DescriptionList>
      )}
      {legacy && <LegacyStorageDetails legacy={legacy} />}
    </PartItem>
  );
};

/** The project's pipeline server. */
const PipelineServerPart: React.FC<PipelinesTabProps & { project: string }> = ({ status, project, busy }) => {
  const server = pipelineServerIn(status, project);
  const unmanaged = pipelineServerUnmanaged(status, project);
  const legacy = status.legacyStorage;
  const old = !!server && (legacy?.usedBy ?? []).includes(`${server.namespace}/${server.name}`);
  const storageReady = !!status.minio?.ready || (old && !!legacy?.ready);
  return (
    <PartItem id="pipeline-server" title="Pipeline server" scope={project} status={pipelineServerStatus(status, project, busy)} unmanaged={unmanaged}
      actions={server?.ready && server.uiRoute && !isTerminating(server) ? <ActionListItem><ExternalLink href={server.uiRoute}>Open pipelines</ExternalLink></ActionListItem> : undefined}>
      {unmanaged
        ? <Content component="p">This project&apos;s pipeline server was not created by this tool; the tool leaves it alone.</Content>
        : server
          ? <Content component="p">DataSciencePipelinesApplication <code>{server.name}</code>, which keeps its artifacts in {old ? <>the storage of an earlier version (<code>{legacy?.endpoint}</code>); Migrate storage moves it to the S3 storage</> : "the S3 storage above"}.</Content>
          : <Content component="p">Not set up in <code>{project}</code>. Set up creates a DataSciencePipelinesApplication that keeps its artifacts in the S3 storage.</Content>}
      {server && <ProblemAlerts kind="pipeline" state={server} minioReady={storageReady} />}
      {server && server.deployed && !server.ready && server.message && !terminalReason(server) && (
        <DescriptionList isCompact isHorizontal aria-label="Pipeline server details">
          <DescriptionListGroup>
            <DescriptionListTerm>State</DescriptionListTerm>
            <DescriptionListDescription>{server.message}</DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      )}
    </PartItem>
  );
};

/**
 * The Pipelines tab: S3 storage (cluster) and the selected project's
 * pipeline server, each with its health, actions and details; Set up for
 * what is missing; what it creates and Tear down below.
 */
export const PipelinesTab: React.FC<PipelinesTabProps> = (props) => {
  const { status, project, projects, onProject, state, busy, baseReason, setPending, onSetUp, minioLeftovers } = props;
  const { minio, legacyStorage: legacy } = status;
  const server = project ? pipelineServerIn(status, project) : undefined;
  const serverManaged = !!server && server.managedByTool !== false;
  const next = state.next;
  const storageTeardown = storageTeardownOffered(minio, legacy, minioLeftovers);
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Pipelines" purpose="Run data science pipelines in a project; their artifacts go to the cluster's S3 storage." state={state} /></StackItem>
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="pipelines-project">
            <ProjectSelect id="pipelines-project" projects={projects} selected={project} onSelect={onProject} />
          </FormGroup>
        </Form>
      </StackItem>
      <StackItem>
        <PartList aria-label="What Pipelines needs">
          <StoragePart {...props} />
          {project && <PipelineServerPart {...props} project={project} />}
        </PartList>
      </StackItem>
      {!project && (
        <StackItem>
          <EmptyState headingLevel="h3" titleText="Choose a project" variant="xs">
            <EmptyStateBody>No project has a pipeline server yet. Choose the project to set one up in.</EmptyStateBody>
          </EmptyState>
        </StackItem>
      )}
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction>
            <TooltipButton variant="primary" onClick={() => onSetUp(next.setup)} disabledReason={baseReason ?? next.reason}>{next.label}</TooltipButton>
          </PrimaryAction>
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Namespace <code>{S3_NAMESPACE}</code>, labelled as managed by this tool, with Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-data</code> (20Gi), Secret <code>seaweedfs-s3-credentials</code> (the S3 keys), Service <code>seaweedfs-s3</code>, Route <code>seaweedfs-admin</code> (admin UI), NetworkPolicy <code>seaweedfs-ingress</code> and bucket <code>pipelines</code></>,
            <>In the project: DataSciencePipelinesApplication <code>nightly-dspa</code> and Secret <code>nightly-dspa-s3</code> (a copy of the S3 keys)</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {(serverManaged || storageTeardown || (!!server && !serverManaged) || notManaged(minio)) && (
        <StackItem>
          <FeatureSection title="Tear down">
            <Stack hasGutter>
              {project && server && !serverManaged && <StackItem><Content component="p">The pipeline server in <code>{project}</code> was not created by this tool, so it is not torn down here.</Content></StackItem>}
              {notManaged(minio) && <StackItem><Content component="p">Namespace <code>{S3_NAMESPACE}</code> was not created by this tool, so it is not torn down here.</Content></StackItem>}
              {serverManaged && storageTeardown && (
                <StackItem><Content component="p">Tear down the pipeline servers before the storage: the storage cannot be torn down while one uses it.</Content></StackItem>
              )}
              {((serverManaged && !isTerminating(server)) || storageTeardown) && (
              <StackItem>
                <ActionRow>
                  {serverManaged && !isTerminating(server) && (
                    <ActionListItem>
                      <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-pipeline-server", project: project!, name: server!.name })}
                        isLoading={busy.resAction === "teardown-pipeline-server" && busy.actionProject === project} disabledReason={baseReason ?? server!.teardownBlockedReason ?? null}>Tear down the pipeline server</TooltipButton>
                    </ActionListItem>
                  )}
                  {storageTeardown && (
                    <ActionListItem>
                      <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-minio" })} isLoading={busy.resAction === "teardown-minio"}
                        disabledReason={baseReason ?? minio!.teardownBlockedReason ?? null}>Tear down the S3 storage</TooltipButton>
                    </ActionListItem>
                  )}
                </ActionRow>
              </StackItem>
              )}
            </Stack>
          </FeatureSection>
        </StackItem>
      )}
    </Stack>
  );
};

/** The confirmation of a Storage change, or null for another feature's change. */
export function storageConfirm(pending: Pending, minio: ResourceState | undefined, legacy?: LegacyStorageState | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-minio":
    case "repair-minio":
      return {
        title: pending.kind === "repair-minio" ? "Repair S3 storage?" : "Set up S3 storage?",
        confirm: pending.kind === "repair-minio" ? "Repair S3 storage" : "Set up S3 storage",
        changes: [
          <>Namespace <code>{S3_NAMESPACE}</code> {pending.kind === "repair-minio" || minio?.managedByTool ? "is kept and reused" : "is created"}, labelled as managed by this tool.</>,
          <>SeaweedFS: Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-data</code>, Secret <code>seaweedfs-s3-credentials</code> (the S3 keys) and NetworkPolicy <code>seaweedfs-ingress</code> are applied with the image the server is configured to use.</>,
          <>Once SeaweedFS is ready: Service <code>seaweedfs-s3</code> (the S3 endpoint <code>{S3_ENDPOINT}</code>), the admin UI Route <code>seaweedfs-admin</code> and the bucket <code>pipelines</code>.</>,
        ],
        extra: <Content component="p">The server waits up to 90 seconds for SeaweedFS to become ready and reports the pod&apos;s reason if it cannot start.{pending.kind === "repair-minio" ? " Stored data is kept." : ""}</Content>,
      };
    case "teardown-minio": {
      const current = !!minio && (minio.deployed || pvcList(minio).length > 0);
      const namespaces = [...(current ? [S3_NAMESPACE] : []), ...(legacy ? [legacy.namespace] : [])];
      const pvcs = [...(current ? pvcList(minio).map((n) => `${S3_NAMESPACE}/${n}`) : []), ...(legacy?.dataPVCs ?? []).map((p) => `${legacy!.namespace}/${p.name}`)];
      return {
        title: "Tear down S3 storage?", confirm: "Tear down S3 storage", danger: true,
        changes: [
          ...(current ? [<>The S3 storage objects this tool created in namespace <code>{S3_NAMESPACE}</code> are deleted: Deployment <code>seaweedfs</code>, NetworkPolicy <code>seaweedfs-ingress</code>, Route <code>seaweedfs-admin</code>, Service <code>seaweedfs-s3</code>, Secret <code>seaweedfs-s3-credentials</code> and PersistentVolumeClaim <code>seaweedfs-data</code>.</>] : []),
          ...(legacy ? [<>The storage an earlier version left in namespace <code>{legacy.namespace}</code> is deleted: {joinNodes(legacy.objects.map((o) => <code key={o}>{o}</code>))}.</>] : []),
          <>Objects with these names that the tool did not create are kept. {namespaces.length > 1 ? "Namespaces" : "Namespace"} {joinNodes(namespaces.map((n) => <code key={n}>{n}</code>))} {namespaces.length > 1 ? "are" : "is"} kept: delete {namespaces.length > 1 ? "them" : "it"} yourself with {joinNodes(namespaces.map((n) => <code key={n}>oc delete project {n}</code>))} once empty.</>,
        ],
        dataLoss: pvcs.length > 0
          ? <>PersistentVolumeClaim{pvcs.length > 1 ? "s" : ""} {joinNodes(pvcs.map((n) => <code key={n}>{n}</code>))}, with every object stored in {pvcs.length > 1 ? "them" : "it"} (pipeline artifacts, uploaded files{legacy ? ", and the data of earlier versions" : ""}).</>
          : <>Every object stored in the S3 storage (pipeline artifacts, uploaded files).</>,
        extra: <Content component="p">The server refuses while a pipeline server still uses the storage; the message names it. If some objects cannot be deleted, run Tear down again.</Content>,
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
      return {
        title: `Set up a pipeline server in ${pending.project}?`, confirm: "Set up",
        changes: [
          <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>nightly-dspa</code> and Secret <code>nightly-dspa-s3</code> are created, storing artifacts in the S3 storage (Service <code>seaweedfs-s3</code> in namespace <code>seaweedfs</code>).</>,
        ],
        extra: <Content component="p">The pipeline server takes 1 to 3 minutes to become ready.</Content>,
      };
    case "teardown-pipeline-server": {
      const ps = pipelineServers.find((p) => p.namespace === pending.project);
      return {
        title: `Tear down the pipeline server in ${pending.project}?`, confirm: "Tear down", danger: true,
        changes: [<>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name || "nightly-dspa"}</code> and its S3 credentials Secret are deleted. The project stays.</>],
        dataLoss: pvcsLost(ps, "the pipeline database (runs, experiments and their history)") ?? <>The pipeline database (runs, experiments and their history).</>,
      };
    }
    default:
      return null;
  }
}
