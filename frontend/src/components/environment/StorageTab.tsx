import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Checkbox,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Link } from "react-router-dom";
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
  terminalReason,
} from "./resourceRow";
import { CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, READ_ONLY_SETUP } from "./FeatureLayout";
import { CopyS3EnvButton, S3SecretField } from "./AdminLogin";
import { notManaged } from "./resourceStatus";
import { STORAGE_HREF, TAB_TITLES } from "./environmentTabs";
import { type Busy, type FeatureState, storageStatus } from "./featureState";

/** Namespace where storage runs and its in-cluster URL (pkg/cluster s3Namespace, s3Host). */
export const S3_NAMESPACE = "seaweedfs";
export const S3_ENDPOINT = "http://seaweedfs-s3.seaweedfs.svc:8333";
/** Bucket name used for artifacts from each pipeline server this tool provisions. */
export const PIPELINES_BUCKET = "pipelines";

const pvcWithSize = (p: KeptPVC) => <><code>{p.name}</code>{p.size ? ` (${p.size})` : ""}</>;

const joinNodes = (nodes: React.ReactNode[]) => nodes.map((n, i) => <React.Fragment key={i}>{i > 0 ? ", " : ""}{n}</React.Fragment>);

/**
 * PatternFly link-button target for the S3 storage tab (Button
 * component=, AlertActionLink component=): uses a real anchor so
 * middle-click or new-tab navigation works without reloading the SPA.
 */
export const StorageTabLink = React.forwardRef<HTMLAnchorElement, React.AnchorHTMLAttributes<HTMLAnchorElement>>((props, ref) => <Link ref={ref} {...props} to={STORAGE_HREF} />);
StorageTabLink.displayName = "StorageTabLink";

/** Props supplied by the parent page for the S3 storage tab. */
export interface StorageTabProps {
  status: ResourcesStatus;
  /** storageState: identical to what the Overview tab uses. */
  state: FeatureState;
  busy: Busy;
  /** When non-null, explains why mutations are currently blocked. */
  baseReason: string | null;
  /** Read-only mode hides actions; Set up points the user at someone who can change things. */
  readOnly: boolean;
  /** When set, the reason S3 key fields stay hidden for this user. */
  readReason: string | null;
  /** Incomplete teardown left tool-owned objects behind, so Delete remains available. */
  minioLeftovers: boolean;
  /** Base OpenShift console URL used when linking to pod logs. */
  consoleURL?: string;
  setPending: (p: Pending) => void;
  onMigrate: () => void;
  onRemoveOldStorage: () => void;
  onSetUp: (setup: Pending[]) => void;
}

/** True when teardown is meaningful: current tool storage or legacy storage is present. */
export const storageTeardownOffered = (minio: ResourceState | undefined, legacy: LegacyStorageState | null | undefined, leftovers: boolean) =>
  !!minio && !isTerminating(minio) && ((minio.managedByTool !== false && (minio.deployed || leftovers)) || !!legacy);

/** Renders legacy MinIO-era storage in namespace minio; sole UI that lists those objects by name. */
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

/** Tab-top banner for migration: why it is required, that workloads keep running, and what the action does. */
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

/** Cluster-scoped S3 block: SeaweedFS in seaweedfs, or legacy storage awaiting migration. */
const StoragePart: React.FC<StorageTabProps> = ({ status, busy, baseReason, readOnly, readReason, minioLeftovers, consoleURL, setPending, onMigrate, onRemoveOldStorage }) => {
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
    if (legacy?.stage === "remove") menu.push({ key: "remove-old", label: "Delete old storage...", onClick: onRemoveOldStorage, disabledReason: legacyReason || legacy.removeBlockedReason || null, isDanger: true });
    if (storageTeardownOffered(minio, legacy, minioLeftovers)) {
      menu.push({ key: "delete", label: "Delete S3 storage...", onClick: () => setPending({ kind: "teardown-minio" }), disabledReason: baseReason ?? minio!.teardownBlockedReason ?? null, isDanger: true });
    }
  }
  const blocked = !deployed && minio?.setupBlockedReason;
  const usedBy = minio?.usedBy ?? [];
  return (
    <PartItem id="storage" title="S3 storage" scope="Shared by the cluster" status={storageStatus(status, busy)} unmanaged={unmanaged} actions={actions.length ? actions : undefined} menu={menu}>
      {!minio?.deployed && (minio?.leftovers ?? []).length > 0 && (
        <Content component="p">A deletion that did not finish left {joinNodes((minio?.leftovers ?? []).map((l) => <code key={l}>{l}</code>))} in namespace <code>{S3_NAMESPACE}</code>{readOnly ? "." : "; Delete S3 storage removes it."}</Content>
      )}
      {blocked && (
        <Alert variant="warning" isInline component="div" title={`The S3 storage can't be set up: namespace ${S3_NAMESPACE} belongs to someone else`}>
          <Content component="p">{minio!.setupBlockedReason}</Content>
          <Content component="p">If nothing in it is used, a cluster-admin can delete it with <code>oc delete project {S3_NAMESPACE}</code>; otherwise ask its owner.</Content>
        </Alert>
      )}
      {!deployed && !legacy && !blocked && (minio?.leftovers ?? []).length === 0 && !isTerminating(minio) && (
        <Content component="p">Not set up yet. Set up S3 storage creates it, with a 20 GiB volume.</Content>
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
            <DescriptionListDescription><ImageRef image={PIPELINES_BUCKET} what="bucket name" truncate={false} /></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Region</DescriptionListTerm>
            <DescriptionListDescription><code>us-east-1</code>, path-style addressing</DescriptionListDescription>
          </DescriptionListGroup>
          {usedBy.length > 0 && (
            <DescriptionListGroup>
              <DescriptionListTerm>Used by</DescriptionListTerm>
              <DescriptionListDescription>{joinNodes(usedBy.map((u) => <code key={u}>{u}</code>))}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
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
 * Dedicated tab for cluster S3 storage (not only Pipelines). Covers identity
 * and consumers; migration when data still lives on an older stack; live
 * health, credentials, and actions (Repair, Delete); setup when absent;
 * and created resources. Cluster-wide—no project selector.
 */
export const StorageTab: React.FC<StorageTabProps> = (props) => {
  const { status, state, busy, baseReason, readOnly, onMigrate, onSetUp } = props;
  const legacy = status.legacyStorage;
  const next = state.next;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title={TAB_TITLES.storage} purpose={FEATURE_PURPOSE.storage} state={state} /></StackItem>
      <StackItem>
        <Content component="p">
          Every pipeline server this tool sets up keeps its artifacts here, in bucket <code>{PIPELINES_BUCKET}</code>; setting up Pipelines sets it up first when it is missing. Other features can use it too.
        </Content>
      </StackItem>
      {legacy && <StackItem><MigrationAlert legacy={legacy} onMigrate={onMigrate} readOnly={readOnly} reason={busy.migrating ? "Migrate storage is running." : baseReason} /></StackItem>}
      <StackItem>
        <PartList aria-label={`What ${TAB_TITLES.storage} needs`}>
          <StoragePart {...props} />
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? null} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster: namespace <code>{S3_NAMESPACE}</code>, labelled as managed by this tool, with Deployment <code>seaweedfs</code>, PersistentVolumeClaim <code>seaweedfs-data</code> (20Gi), Secret <code>seaweedfs-s3-credentials</code> (the S3 keys), Service <code>seaweedfs-s3</code>, Route <code>seaweedfs-admin</code> (admin UI), NetworkPolicy <code>seaweedfs-ingress</code> and bucket <code>{PIPELINES_BUCKET}</code></>,
          ]} />
        </FeatureSection>
      </StackItem>
    </Stack>
  );
};

/**
 * Pipelines-tab slice of storage: status only, non-editable, with navigation
 * to the full S3 tab for configuration, secrets, and destructive actions.
 */
export const StoragePrerequisitePart: React.FC<{ status: ResourcesStatus; busy: Busy }> = ({ status, busy }) => {
  const s = storageStatus(status, busy);
  const { minio, legacyStorage: legacy } = status;
  const unmanaged = notManaged(minio);
  const why = unmanaged ? <>Namespace <code>{S3_NAMESPACE}</code> was not created by this tool, so pipeline servers set up here do not use it.</>
    // Migration needed: the alert at the top of the tab says it, with the way there.
    : legacy?.stage === "migrate" ? null
    : s === "not-set-up" ? (minio?.setupBlockedReason ? "It can't be set up now; the S3 storage tab says why." : "Setting up a pipeline server sets it up first.")
    : s === "error" || s === "needs-attention" ? "The S3 storage tab says what needs attention."
    : null;
  return (
    <PartItem id="pipelines-storage" title="S3 storage" scope="Shared by the cluster" status={s} unmanaged={unmanaged}
      actions={<ActionListItem><Button variant="link" isInline component={StorageTabLink}>Open S3 storage</Button></ActionListItem>}>
      <Content component="p">Pipeline servers this tool sets up keep their artifacts in it, in bucket <code>{PIPELINES_BUCKET}</code>.{why && <> {why}</>}</Content>
    </PartItem>
  );
};

/** Build confirm dialog copy for a pending storage action; null if pending is unrelated. */
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
          <>Once SeaweedFS is ready: Service <code>seaweedfs-s3</code> (the S3 endpoint <code>{S3_ENDPOINT}</code>), the admin UI Route <code>seaweedfs-admin</code> and the bucket <code>{PIPELINES_BUCKET}</code>.</>,
        ],
      };
    case "teardown-minio": {
      const current = !!minio && (minio.deployed || pvcList(minio).length > 0);
      // Optional project deletion for seaweedfs mirrors minio: checkbox only
      // when pkg/cluster marks the namespace toolNamespaceDeletable (tool-owned),
      // so the user explicitly opts in.
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

/** Shown on Pipelines while migration is outstanding: blocks new servers and routes to the S3 tab action. */
export const MigrationPointer: React.FC = () => (
  <Alert variant="warning" isInline component="div" title="Your pipeline data is in storage from an earlier version"
    actionLinks={<AlertActionLink component={StorageTabLink}>Open S3 storage</AlertActionLink>}>
    <Content component="p">Your pipeline servers keep working on it meanwhile. New pipeline servers can be set up once Migrate storage, on the S3 storage tab, has moved the data.</Content>
  </Alert>
);
