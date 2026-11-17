import React from "react";
import { ActionListItem, Alert, Card, CardBody, CardHeader, CardTitle, Content, FlexItem, List, ListItem, Title } from "@patternfly/react-core";
import type { KeptPVC, LegacyStorageState, ResourceState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { CardList, CardListItem } from "../CardList";
import { TagLabel } from "../StatusLabel";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  RowTitle,
  StatusDetails,
  isTerminating,
  pvcList,
  rowActions,
  terminalReason,
  unmanagedNote,
} from "./resourceRow";

/** The storage's namespace and in-cluster address (pkg/cluster s3Namespace, s3Host). */
export const S3_NAMESPACE = "seaweedfs";
export const S3_ENDPOINT = "http://seaweedfs-s3.seaweedfs.svc:8333";

/** Why Set up waits while storage from an earlier version exists (pkg/cluster legacyStorageSetupBlock). */
export const LEGACY_SETUP_REASON = "Storage from an earlier version of this tool is still in use (see below). It is migrated instead, so that its data and pipeline servers move to the new storage.";

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

const joinNodes = (nodes: React.ReactNode[]) => nodes.map((n, i) => <React.Fragment key={i}>{i > 0 ? ", " : ""}{n}</React.Fragment>);

/**
 * The S3 storage an earlier version left in namespace minio: the only
 * place the card names MinIO-era objects.
 */
const LegacyStorageDetail: React.FC<{ legacy: LegacyStorageState }> = ({ legacy }) => {
  const pvcs = legacy.dataPVCs ?? [];
  const usedBy = legacy.usedBy ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  return (
    <Alert component="h4" variant="warning" isInline isPlain title="Storage from an earlier version" className="pf-v6-u-mt-sm">
      <Content component="p">
        {legacy.server ? <>{legacy.server}{legacy.ready ? "" : " (not running)"}</> : "No server"} in namespace <code>{legacy.namespace}</code>, at <code>{legacy.endpoint}</code>, still holds the data of earlier versions of this tool.
        {usedBy.length > 0 && <> Pipeline server{usedBy.length > 1 ? "s" : ""} {joinNodes(usedBy.map((u) => <code key={u}>{u}</code>))} still use{usedBy.length > 1 ? "" : "s"} it.</>}
      </Content>
      <List isPlain>
        <ListItem>Objects this tool created there: {joinNodes(legacy.objects.map((o) => <code key={o}>{o}</code>))}.</ListItem>
        {pvcs.length > 0 && <ListItem>Data volumes: {joinNodes(pvcs.map((p) => <React.Fragment key={p.name}>{pvcWithSize(p)}</React.Fragment>))}.</ListItem>}
        {uncopied.length > 0 && <ListItem>{joinNodes(uncopied.map((n) => <code key={n}>{n}</code>))} holds MinIO&apos;s files from before SeaweedFS replaced it; no running server reads {uncopied.length > 1 ? "them" : "it"}, so {uncopied.length > 1 ? "they are" : "it is"} not copied.</ListItem>}
      </List>
    </Alert>
  );
};

/** The S3 storage (SeaweedFS in namespace seaweedfs), and what an earlier version left in namespace minio. */
export const StorageCard: React.FC<CardActionProps & {
  minio?: ResourceState;
  legacy?: LegacyStorageState | null;
  /** Tool objects left after a teardown (or a teardown that did not finish): Tear down stays offered. */
  minioLeftovers: boolean;
}> = ({ minio, legacy, minioLeftovers, baseReason, resAction, setPending }) => (
  <Card>
    <CardHeader><CardTitle><Title headingLevel="h3" size="lg">Storage</Title></CardTitle></CardHeader>
    <CardBody>
      <CardList aria-label="Storage">
        <CardListItem labelledBy="minio-item" actionsBesideFrom="xl" actions={rowActions(
          <>
            {minio?.ready && minio.uiRoute && <ActionListItem><ExternalLink href={minio.uiRoute}>Open admin UI</ExternalLink></ActionListItem>}
            {minio && !minio.deployed && !isTerminating(minio) && (
              <ActionListItem>
                <TooltipButton variant="secondary" onClick={() => setPending({ kind: "setup-minio" })} isLoading={resAction === "setup-minio"}
                  disabledReason={baseReason ?? (legacy ? LEGACY_SETUP_REASON : minio.setupBlockedReason) ?? null}>Set up</TooltipButton>
              </ActionListItem>
            )}
            {minio?.deployed && minio.managedByTool !== false && (!!terminalReason(minio) || !!minio.repairNeeded) && !isTerminating(minio) && (
              <ActionListItem>
                <TooltipButton variant="secondary" onClick={() => setPending({ kind: "repair-minio" })} isLoading={resAction === "setup-minio"}
                  disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Repair</TooltipButton>
              </ActionListItem>
            )}
            {minio && !isTerminating(minio) && ((minio.managedByTool !== false && (minio.deployed || minioLeftovers)) || !!legacy) && (
              <ActionListItem>
                <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-minio" })} isLoading={resAction === "teardown-minio"}
                  disabledReason={baseReason ?? minio.teardownBlockedReason ?? null}>Tear down</TooltipButton>
              </ActionListItem>
            )}
          </>,
        )}>
          <RowTitle id="minio-item" name="S3 storage" state={minio}
            extra={minio?.deployed ? <FlexItem><TagLabel>SeaweedFS</TagLabel></FlexItem> : undefined} />
          <Content component="p" className="pf-v6-u-text-color-subtle">
            S3 storage (SeaweedFS) for pipeline artifacts and test files, in namespace <code>{S3_NAMESPACE}</code>. Pipeline servers reach it at <code>{S3_ENDPOINT}</code>.{unmanagedNote(minio)}
          </Content>
          {minio && (
            <StatusDetails
              kind="minio"
              state={minio}
              teardownBlocked={((minio.deployed || pvcList(minio).length > 0) && minio.managedByTool !== false) || !!legacy ? minio.teardownBlockedReason : null}
              notes={minio.deployed && minio.uiUser ? [<React.Fragment key="login">Admin UI login: user <code>{minio.uiUser}</code>, password in Secret <code>seaweedfs-s3-credentials</code> (key <code>AWS_SECRET_ACCESS_KEY</code>): <code>oc extract -n seaweedfs secret/seaweedfs-s3-credentials --keys=AWS_SECRET_ACCESS_KEY --to=-</code></React.Fragment>] : []}
            />
          )}
          {legacy && <LegacyStorageDetail legacy={legacy} />}
        </CardListItem>
      </CardList>
    </CardBody>
  </Card>
);

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
