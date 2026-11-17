import React from "react";
import {
  ActionListItem,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
} from "@patternfly/react-core";
import type { KeptPVC, LegacyStorageState, ResourceState } from "../../types";
import { getS3AdminLogin } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  ProblemAlerts,
  createdBy,
  isTerminating,
  pvcList,
  terminalReason,
} from "./resourceRow";
import { CreatesList, DetailsActions, DetailsAlert, DetailsSection, DetailsStack, HowToUse, ResourceCard } from "./ResourceCard";
import { CopyFromServerButton } from "./CopyFromServerButton";
import { envStatus, notManaged, type EnvStatus } from "./resourceStatus";

/** The storage's namespace and in-cluster address (pkg/cluster s3Namespace, s3Host). */
export const S3_NAMESPACE = "seaweedfs";
export const S3_ENDPOINT = "http://seaweedfs-s3.seaweedfs.svc:8333";

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

const joinNodes = (nodes: React.ReactNode[]) => nodes.map((n, i) => <React.Fragment key={i}>{i > 0 ? ", " : ""}{n}</React.Fragment>);

/** What the Storage card and its details need. */
export interface StorageProps extends CardActionProps {
  minio?: ResourceState;
  legacy?: LegacyStorageState | null;
  /** Tool objects left after a teardown (or a teardown that did not finish): Tear down stays offered. */
  minioLeftovers: boolean;
  /** Opens Migrate storage. */
  onMigrate: () => void;
  /** Migrate storage is running from this page. */
  migrating: boolean;
  /** Why the admin password cannot be read by this user, or null. */
  readReason: string | null;
}

/** The storage's one status. Storage of an earlier version still holding the data needs attention. */
export function storageStatus({ minio, legacy, resAction, migrating }: Pick<StorageProps, "minio" | "legacy" | "resAction" | "migrating">): EnvStatus {
  const busy = migrating || resAction === "setup-minio" ? "setting-up" : resAction === "teardown-minio" ? "deleting" : null;
  return envStatus(minio, { busy, attention: legacy?.stage === "migrate" });
}

/**
 * Why a new pipeline server cannot be set up on the storage yet, or null.
 * The storage must be ready, and a migration must have copied the old
 * storage first: the copy's check compares both sides, so a pipeline
 * server writing to the new storage before it would make the check fail.
 */
export function storageNotReadyReason(minio: ResourceState | undefined, legacy: LegacyStorageState | null | undefined, migrating: boolean): string | null {
  if (migrating) return "Migrate storage is running. Wait until it has finished.";
  if (legacy?.stage === "migrate") return "Available once Migrate storage has moved the data of an earlier version to the new storage (Storage, Cluster tab).";
  // Pipeline servers set up here get a copy of the tool's storage keys; it
  // does not read the keys of storage someone else made (pkg/cluster
  // SetupPipelineServer).
  if (notManaged(minio)) return "Pipeline servers set up here use the S3 storage this tool sets up. Namespace seaweedfs was not created by this tool, so it is not used.";
  if (!minio?.ready) return minio?.deployed ? "Available once the S3 storage is ready (Storage, Cluster tab)." : "Available once the S3 storage is set up (Storage, Cluster tab).";
  return null;
}

/** Tear down is offered: the tool's storage exists, or storage of an earlier version does. */
const teardownOffered = ({ minio, legacy, minioLeftovers }: StorageProps) =>
  !!minio && !isTerminating(minio) && ((minio.managedByTool !== false && (minio.deployed || minioLeftovers)) || !!legacy);

/** The S3 storage (SeaweedFS in namespace seaweedfs) as a compact card. */
export const StorageCard: React.FC<StorageProps & { onDetails: (opener: HTMLElement) => void; detailsOpen: boolean }> = (props) => {
  const { minio, legacy, onMigrate, migrating, baseReason, resAction, setPending, onDetails, detailsOpen } = props;
  let action: React.ReactNode = null;
  if (legacy) {
    const disabledReason = migrating ? null : baseReason ?? (legacy.terminating ? `Namespace ${legacy.namespace} is being deleted.` : null);
    action = legacy.stage === "migrate"
      ? <TooltipButton variant="primary" onClick={onMigrate} isLoading={migrating} disabledReason={disabledReason}>Migrate storage</TooltipButton>
      : <TooltipButton variant="secondary" isDanger onClick={onMigrate} isLoading={migrating} disabledReason={disabledReason}>Remove old storage</TooltipButton>;
  } else if (minio && !minio.deployed && !isTerminating(minio)) {
    action = <TooltipButton variant="primary" onClick={() => setPending({ kind: "setup-minio" })} isLoading={resAction === "setup-minio"}
      disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Set up</TooltipButton>;
  } else if (minio?.deployed && minio.managedByTool !== false && (!!terminalReason(minio) || !!minio.repairNeeded) && !isTerminating(minio)) {
    action = <TooltipButton variant="primary" onClick={() => setPending({ kind: "repair-minio" })} isLoading={resAction === "setup-minio"}
      disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Repair</TooltipButton>;
  } else if (minio?.ready && minio.uiRoute) {
    action = <ExternalLink href={minio.uiRoute}>Open admin UI</ExternalLink>;
  }
  const needs = legacy?.stage === "migrate"
    ? "An earlier version's storage still holds the data: Migrate storage moves it here."
    : legacy?.stage === "remove"
      ? "The data was moved here. The earlier version's storage is kept until you remove it."
      : undefined;
  return (
    <ResourceCard id="storage" title="Storage" status={storageStatus(props)} unmanaged={notManaged(minio)} action={action} needs={needs} onDetails={onDetails} detailsOpen={detailsOpen}
      purpose="S3 storage (SeaweedFS) for pipeline artifacts and test files." />
  );
};

/** The storage an earlier version left in namespace minio: the only place that names MinIO-era objects. */
const LegacyStorageDetails: React.FC<{ legacy: LegacyStorageState }> = ({ legacy }) => {
  const pvcs = legacy.dataPVCs ?? [];
  const usedBy = legacy.usedBy ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  return (
    <DetailsSection title="Storage of an earlier version">
      <DescriptionList isCompact aria-label="Storage from an earlier version">
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
    </DetailsSection>
  );
};

/** The storage's details, in the drawer. */
export const StorageDetails: React.FC<StorageProps> = (props) => {
  const { minio, legacy, baseReason, resAction, setPending, readReason } = props;
  const deployed = !!minio?.deployed;
  const adminLogin = deployed && !!minio?.uiUser && minio.managedByTool !== false;
  return (
    <DetailsStack>
      {legacy?.stage === "migrate" && (
        <DetailsAlert variant="warning" title="Migration needed">
          The storage of an earlier version of this tool, in namespace <code>{legacy.namespace}</code>, still holds the data. Migrate storage moves it to namespace <code>{S3_NAMESPACE}</code>, with the pipeline servers this tool created.
        </DetailsAlert>
      )}
      {legacy?.stage === "remove" && (
        <DetailsAlert variant="info" title="The old storage is kept until you remove it">
          The data and this tool&apos;s pipeline servers are in namespace <code>{S3_NAMESPACE}</code>. Remove old storage finishes the move.
        </DetailsAlert>
      )}
      {minio && (!legacy || deployed) && <ProblemAlerts kind="minio" state={minio} />}
      <DescriptionList isCompact isHorizontal aria-label="Storage details">
        <DescriptionListGroup>
          <DescriptionListTerm>Namespace</DescriptionListTerm>
          <DescriptionListDescription><code>{S3_NAMESPACE}</code></DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>S3 endpoint</DescriptionListTerm>
          <DescriptionListDescription><ImageRef image={S3_ENDPOINT} what="S3 endpoint" truncate={false} /></DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Bucket</DescriptionListTerm>
          <DescriptionListDescription><code>pipelines</code></DescriptionListDescription>
        </DescriptionListGroup>
        {deployed && minio?.uiRoute && (
          <DescriptionListGroup>
            <DescriptionListTerm>Admin UI</DescriptionListTerm>
            <DescriptionListDescription><a href={minio.uiRoute} target="_blank" rel="noopener noreferrer">{minio.uiRoute.replace(/^https:\/\//, "")}</a></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {deployed && minio?.currentImage && (
          <DescriptionListGroup>
            <DescriptionListTerm>Image</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image={minio.currentImage} /></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {deployed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Created by</DescriptionListTerm>
            <DescriptionListDescription>{createdBy(minio)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      {teardownOffered(props) && (
        <DetailsActions>
          <ActionListItem>
            <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-minio" })} isLoading={resAction === "teardown-minio"}
              disabledReason={baseReason ?? minio!.teardownBlockedReason ?? null}>Tear down</TooltipButton>
          </ActionListItem>
        </DetailsActions>
      )}
      <HowToUse text={<>
        <p>Pipeline servers set up from this page use the storage without further settings.</p>
        <p>
          Other S3 clients in the cluster use the endpoint above, region <code>us-east-1</code>, path-style addressing and the keys in Secret <code>seaweedfs-s3-credentials</code> in namespace <code>{S3_NAMESPACE}</code> (<code>AWS_ACCESS_KEY_ID</code> and <code>AWS_SECRET_ACCESS_KEY</code>).
        </p>
        {adminLogin && <p>Sign in to the admin UI as <code>{minio.uiUser}</code>; the password is the S3 secret key.</p>}
      </>}>
        {adminLogin && (
          <CopyFromServerButton fetchText={async () => (await getS3AdminLogin()).password} disabledReason={readReason}
            copiedText={`Copied the password of user ${minio.uiUser}.`}>Copy admin password</CopyFromServerButton>
        )}
      </HowToUse>
      {legacy && <LegacyStorageDetails legacy={legacy} />}
      <CreatesList items={[
        <>Namespace <code>{S3_NAMESPACE}</code>, labelled as managed by this tool</>,
        <>Deployment <code>seaweedfs</code> with PersistentVolumeClaim <code>seaweedfs-data</code> (20Gi)</>,
        <>Secret <code>seaweedfs-s3-credentials</code> (the S3 keys)</>,
        <>Service <code>seaweedfs-s3</code>, Route <code>seaweedfs-admin</code> (admin UI) and NetworkPolicy <code>seaweedfs-ingress</code></>,
        <>Bucket <code>pipelines</code></>,
      ]} />
    </DetailsStack>
  );
};

/** The confirmation of a Storage change, or null for another card's change. */
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
