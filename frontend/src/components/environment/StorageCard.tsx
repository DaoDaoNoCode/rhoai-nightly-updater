import React from "react";
import { ActionListItem, Alert, Card, CardBody, CardHeader, CardTitle, Content, FlexItem, Stack, StackItem, Title } from "@patternfly/react-core";
import type { KeptPVC, ResourceState } from "../../types";
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
  pvcsLost,
  rowActions,
  terminalReason,
  unmanagedNote,
} from "./resourceRow";

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

/** The S3 storage (SeaweedFS in namespace minio), or the MinIO of an earlier version until it is migrated. */
export const StorageCard: React.FC<CardActionProps & {
  minio?: ResourceState;
  /** Tool objects left after a teardown (or a teardown that did not finish): Tear down stays offered. */
  minioLeftovers: boolean;
}> = ({ minio, minioLeftovers, baseReason, resAction, setPending }) => {
  const keptPVCs = minio?.keptPVCs ?? [];
  return (
    <Card>
      <CardHeader><CardTitle><Title headingLevel="h3" size="lg">Storage</Title></CardTitle></CardHeader>
      <CardBody>
        <CardList aria-label="Storage">
          <CardListItem labelledBy="minio-item" actionsBesideFrom="xl" actions={rowActions(
            <>
              {minio?.ready && minio.uiRoute && <ActionListItem><ExternalLink href={minio.uiRoute}>{minio.migrationPending ? "Open MinIO console" : "Open admin UI"}</ExternalLink></ActionListItem>}
              {minio && !minio.deployed && !isTerminating(minio) && (
                <ActionListItem>
                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "setup-minio" })} isLoading={resAction === "setup-minio"}
                    disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Set up</TooltipButton>
                </ActionListItem>
              )}
              {minio?.migrationPending && minio.managedByTool !== false && !isTerminating(minio) && (
                <ActionListItem>
                  <TooltipButton variant="primary" onClick={() => setPending({ kind: "migrate-minio" })} isLoading={resAction === "setup-minio"}
                    disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Migrate to SeaweedFS</TooltipButton>
                </ActionListItem>
              )}
              {minio?.deployed && !minio.migrationPending && minio.managedByTool !== false && (!!terminalReason(minio) || !!minio.repairNeeded) && !isTerminating(minio) && (
                <ActionListItem>
                  <TooltipButton variant="secondary" onClick={() => setPending({ kind: "repair-minio" })} isLoading={resAction === "setup-minio"}
                    disabledReason={baseReason ?? minio.setupBlockedReason ?? null}>Repair</TooltipButton>
                </ActionListItem>
              )}
              {minio && minio.managedByTool !== false && !isTerminating(minio) && (minio.deployed || minioLeftovers) && (
                <ActionListItem>
                  <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-minio" })} isLoading={resAction === "teardown-minio"}
                    disabledReason={baseReason ?? minio.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                </ActionListItem>
              )}
            </>,
          )}>
            <RowTitle id="minio-item" name="S3 storage" state={minio}
              extra={minio?.deployed ? <FlexItem><TagLabel>{minio.migrationPending ? "MinIO (migration pending)" : "SeaweedFS"}</TagLabel></FlexItem> : undefined} />
            <Content component="p" className="pf-v6-u-text-color-subtle">
              {/* Until it is migrated, a MinIO from an earlier version serves (the tag and the warning say so). */}
              S3 storage{minio?.migrationPending ? "" : " (SeaweedFS)"} for pipeline artifacts and test files, in namespace <code>minio</code>, behind Service <code>minio-service</code>.{unmanagedNote(minio)}
            </Content>
            {minio && (
              <StatusDetails
                kind="minio"
                state={minio}
                teardownBlocked={(minio.deployed || pvcList(minio).length > 0) && minio.managedByTool !== false ? minio.teardownBlockedReason : null}
                notes={[
                  ...(minio.deployed && minio.uiUser ? [<React.Fragment key="login">Admin UI login: user <code>{minio.uiUser}</code>, password in Secret <code>minio-secret</code> (key <code>minio_root_password</code>): <code>oc extract -n minio secret/minio-secret --keys=minio_root_password --to=-</code></React.Fragment>] : []),
                  ...keptPVCs.map((p) => <React.Fragment key={p.name}>Data volume from the earlier MinIO storage (PersistentVolumeClaim {pvcWithSize(p)}), kept for a rollback or a manual copy (RUNBOOK §10.1); SeaweedFS does not use it. Tear down deletes it.</React.Fragment>),
                ]}
              />
            )}
          </CardListItem>
        </CardList>
      </CardBody>
    </Card>
  );
};

/** The confirmation of a Storage change, or null for another card's change. */
export function storageConfirm(pending: Pending, minio: ResourceState | undefined): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-minio":
    case "repair-minio":
      return {
        title: pending.kind === "repair-minio" ? "Repair S3 storage?" : "Set up S3 storage?",
        confirm: pending.kind === "repair-minio" ? "Repair S3 storage" : "Set up S3 storage",
        changes: [
          <>Namespace <code>minio</code> {pending.kind === "repair-minio" || minio?.managedByTool ? "is kept and reused" : "is created"}, labelled as managed by this tool.</>,
          <>SeaweedFS: Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-pvc</code>, Secret <code>minio-secret</code> and NetworkPolicy <code>seaweedfs-ingress</code> are applied with the image the server is configured to use.</>,
          <>Once SeaweedFS is ready: Service <code>minio-service</code> (the S3 endpoint pipeline servers use; its name is kept from earlier versions), the admin UI Route <code>minio-ui</code> and the bucket <code>pipelines</code>.</>,
          <>Route <code>minio-api</code> (the S3 API outside the cluster) is removed if it exists; pipeline servers use the in-cluster service.</>,
        ],
        extra: <Content component="p">The server waits up to 90 seconds for SeaweedFS to become ready and reports the pod&apos;s reason if it cannot start.{pending.kind === "repair-minio" ? " Stored data is kept." : ""}</Content>,
      };
    case "migrate-minio":
      return {
        title: "Replace MinIO with SeaweedFS?",
        confirm: "Migrate and start fresh",
        changes: [
          <>SeaweedFS is deployed next to MinIO: Deployment <code>seaweedfs</code>, a new PersistentVolumeClaim <code>seaweedfs-pvc</code> and NetworkPolicy <code>seaweedfs-ingress</code>. It uses the credentials already in Secret <code>minio-secret</code> and creates the bucket <code>pipelines</code>.</>,
          <>Once SeaweedFS is ready, Service <code>minio-service</code> and the admin UI Route <code>minio-ui</code> switch to it. Pipeline servers keep their settings and need no edits.</>,
          <>Deployment <code>minio</code> and NetworkPolicy <code>minio-ingress</code> are removed. PersistentVolumeClaim {pvcList(minio).includes("minio-pvc") ? <code>minio-pvc</code> : <>minio-pvc</>} is kept, unused, for a rollback or a manual copy; Tear down deletes it later.</>,
          <>Route <code>minio-api</code> is removed if it exists and no pipeline server uses it.</>,
        ],
        extra: (
          <Stack hasGutter>
            <StackItem>
              <Alert component="p" variant="warning" isInline title="Start fresh: stored objects are not copied">
                SeaweedFS starts with an empty bucket. Artifacts, logs and cached outputs of earlier pipeline runs return 404; their files stay on <code>minio-pvc</code>. New runs work. If a cached step fails because its outputs are gone, run it again with caching disabled.
              </Alert>
            </StackItem>
            <StackItem><Content component="p">Until SeaweedFS is ready (up to 90 seconds), MinIO keeps serving; if SeaweedFS cannot start, nothing is switched and MinIO stays.</Content></StackItem>
          </Stack>
        ),
      };
    case "teardown-minio":
      return {
        title: "Tear down S3 storage?", confirm: "Tear down S3 storage", danger: true,
        changes: [
          <>The S3 storage objects this tool created in <code>{minio?.namespace || "minio"}</code> are deleted: Deployment <code>seaweedfs</code>, NetworkPolicy <code>seaweedfs-ingress</code>, Routes <code>minio-ui</code> and <code>minio-api</code>, Service <code>minio-service</code>, Secret <code>minio-secret</code> and PersistentVolumeClaim <code>seaweedfs-pvc</code>, and what is left of MinIO from an earlier version (Deployment <code>minio</code>, NetworkPolicy <code>minio-ingress</code>, PersistentVolumeClaim <code>minio-pvc</code>). Objects with these names that the tool did not create are kept.</>,
          <>Namespace <code>{minio?.namespace || "minio"}</code> is kept. Delete it yourself with <code>oc delete project minio</code> once it is empty.</>,
        ],
        dataLoss: pvcsLost(minio, pvcList(minio).includes("minio-pvc")
          ? "every object stored in them (pipeline artifacts, uploaded files, and the data of the earlier MinIO storage)"
          : "every object stored in them (pipeline artifacts, uploaded files)") ?? <>Every object stored in the S3 storage (pipeline artifacts, uploaded files).</>,
        extra: <Content component="p">The server refuses while a pipeline server still uses the storage; the message names it. If some objects cannot be deleted, run Tear down again.</Content>,
      };
    default:
      return null;
  }
}
