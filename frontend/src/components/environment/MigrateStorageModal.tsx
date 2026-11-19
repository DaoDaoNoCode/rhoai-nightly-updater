import React, { useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import type { LegacyStorageState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { S3_ENDPOINT, S3_NAMESPACE } from "./PipelinesTab";
import { MIGRATE_STEPS, type MigrateKind, type MigrateRun } from "./useMigrateStorage";
import { StepProgress } from "./StepProgress";

/** The project of a "project/name" pipeline server. */
const projectOf = (n: string) => n.split("/")[0];

const codes = (items: string[]) => items.map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>);

/**
 * The ordered steps of a run. The step that stopped the run shows no text
 * of its own when the outcome alert says the same.
 */
const RunProgress: React.FC<{ run: MigrateRun }> = ({ run }) => (
  <StepProgress idPrefix="migrate" aria-label={run.kind === "migrate" ? "Migrate storage progress" : "Delete old storage progress"}
    steps={MIGRATE_STEPS[run.kind]} states={run.steps} running={run.running}
    shownElsewhere={run.result && !run.result.success ? run.result.message : undefined} />
);

/** What Migrate storage does, before it runs. */
const MigratePlan: React.FC<{ legacy: LegacyStorageState; confirmed: string[]; setConfirmed: (v: string[]) => void }> = ({ legacy, confirmed, setConfirmed }) => {
  const unrecorded = legacy.unrecorded ?? [];
  // Unrecorded ones move only when their project is confirmed.
  const repoint = (legacy.repoint ?? []).filter((n) => !unrecorded.includes(n) || confirmed.includes(projectOf(n)));
  const notMoved = legacy.notMoved ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          {legacy.server ?? "The storage"} in namespace <code>{legacy.namespace}</code> holds the data of earlier versions of this tool. Migrate storage moves it to namespace <code>{S3_NAMESPACE}</code>:
        </Content>
        <List component="ol">
          <ListItem>Sets up the storage in <code>{S3_NAMESPACE}</code> with new keys: a new access key and secret key. The old keys do not work on it.</ListItem>
          <ListItem>Copies every bucket (empty ones too) and every object with its metadata with Job <code>seaweedfs-migrate</code>, and checks each object byte for byte and that every bucket is there. The run reports how many objects and bytes it copied; any difference stops it here, with nothing else changed. Bucket policies, versioning and lifecycle rules are not carried over: set them again on the new storage if you used them.</ListItem>
          <ListItem>
            {repoint.length > 0
              ? <>Moves the pipeline server{repoint.length > 1 ? "s" : ""} this tool created to the new storage and its keys, one at a time, and waits until each reaches it: {codes(repoint)}.</>
              : <>No pipeline server of this tool uses the old storage, so none is moved.</>}
          </ListItem>
        </List>
      </StackItem>
      {unrecorded.length > 0 && (
        <StackItem>
          <Alert component="div" variant="warning" isInline title="Confirm the pipeline servers this tool set up">
            <Content component="p">
              {unrecorded.length > 1 ? "These pipeline servers carry" : "This pipeline server carries"} this tool&apos;s label, but this tool has no record that it set {unrecorded.length > 1 ? "them" : "it"} up:
              set up before version 3.0.1, or labelled by someone else (a project&apos;s editors can add the label).
              Moving one writes the new storage&apos;s keys into its project, and those keys open every project&apos;s pipeline artifacts.
              Tick only those you know this tool set up; the others stay on the old storage.
            </Content>
            {unrecorded.map((n) => (
              <Checkbox key={n} id={`migrate-confirm-${n.replace("/", "-")}`} label={<>Move <code>{n}</code>: this tool set it up</>}
                isChecked={confirmed.includes(projectOf(n))}
                onChange={(_e, v) => setConfirmed(v ? [...confirmed.filter((p) => p !== projectOf(n)), projectOf(n)] : confirmed.filter((p) => p !== projectOf(n)))} />
            ))}
          </Alert>
        </StackItem>
      )}
      {notMoved.length > 0 && (
        <StackItem>
          <Alert component="p" variant="info" isInline title={`Not moved: ${notMoved.length > 1 ? "these pipeline servers" : "this pipeline server"}`}>
            {notMoved.join("; ")}. Point {notMoved.length > 1 ? "them" : "it"} at the new storage yourself (the checklist is shown after the migration).
          </Alert>
        </StackItem>
      )}
      {uncopied.length > 0 && (
        <StackItem>
          <Alert component="p" variant="warning" isInline title="Data from before SeaweedFS can't be moved">
            {codes(uncopied)} holds MinIO&apos;s files from before SeaweedFS replaced it. No running server reads {uncopied.length > 1 ? "them" : "it"}, so {uncopied.length > 1 ? "they" : "it"} cannot be copied, and {uncopied.length > 1 ? "they are" : "it is"} deleted when you delete the old storage later.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Content component="p">Anything else that uses the old keys, such as your own S3 clients, needs the new ones: the S3 storage on the Pipelines tab shows them after the migration.</Content>
      </StackItem>
      <StackItem>
        <Content component="p">Each pipeline server it moves restarts on the new storage; the others keep running. How long the copy takes depends on how much data there is. The old storage is kept: you delete it later, from the S3 storage&apos;s menu, once nothing else uses it.</Content>
      </StackItem>
    </Stack>
  );
};

/** The summary and choices before the old storage is removed. */
const RemovePlan: React.FC<{ legacy: LegacyStorageState; deleteNamespace: boolean; setDeleteNamespace: (v: boolean) => void }> = ({ legacy, deleteNamespace, setDeleteNamespace }) => {
  const notMoved = legacy.notMoved ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  const pvcs = (legacy.dataPVCs ?? []).map((p) => p.name);
  return (
    <Stack hasGutter>
      <StackItem>
        <Title headingLevel="h3" size="md">Before you remove it</Title>
        <Content component="p">Update anything this tool did not create that still uses the old storage, such as data connections, workbenches and other pipeline servers:</Content>
        <List>
          <ListItem>Endpoint: <code>{S3_ENDPOINT}</code> instead of <code>http://{legacy.endpoint}</code>.</ListItem>
          <ListItem>Access key and secret key: the new ones, shown under S3 storage on the Pipelines tab. The old keys do not work on the new storage.</ListItem>
          <ListItem>Bucket names and object paths are the same (pipeline artifacts stay in <code>pipelines</code>).</ListItem>
          <ListItem>Anything that still uses <code>{legacy.endpoint.split(":")[0]}</code> stops working after this step.</ListItem>
        </List>
      </StackItem>
      {(legacy.removeBlockedReason || notMoved.length > 0) && (
        <StackItem>
          <Alert component="p" variant="warning" isInline title="The old storage is still in use">
            {legacy.removeBlockedReason || notMoved.join("; ")}
          </Alert>
        </StackItem>
      )}
      <StackItem>
        {legacy.namespaceDeletable ? (
          <Checkbox id="migrate-delete-namespace" label={<>Also delete namespace <code>{legacy.namespace}</code></>}
            description={<>
              Deletes namespace <code>{legacy.namespace}</code> and everything in it, including anything this tool did not create.{" "}
              {(legacy.otherObjects ?? []).length > 0
                ? <>Besides the objects this tool created, it sees there: {codes(legacy.otherObjects ?? [])}.</>
                : <>Besides the objects this tool created, it sees no Deployments or PersistentVolumeClaims there.</>}{" "}
              It cannot list every kind (Secrets, ConfigMaps and others): check with <code>oc get all,pvc,secret,configmap -n {legacy.namespace}</code>.
            </>}
            isChecked={deleteNamespace} onChange={(_e, v) => setDeleteNamespace(v)} />
        ) : (
          <Content component="p">{legacy.namespaceKeptReason}</Content>
        )}
      </StackItem>
      <StackItem>
        <Alert component="p" variant="danger" isInline title="Data is deleted and cannot be recovered">
          First the run copies and checks any object written to the old storage since the migration; it never overwrites an object in the new storage, so data written there since is kept, and an object that differs stops the run for you to decide. A large storage may need more than one run: each waits for the copy the first one started. Then it deletes what this tool created in namespace <code>{legacy.namespace}</code>: {codes(legacy.objects)}.
          {pvcs.length > 0 && <> Deleted with {pvcs.length > 1 ? "them" : "it"}: the data in {codes(pvcs)}.</>}
          {uncopied.length > 0 && <> {codes(uncopied)} (MinIO&apos;s files from before SeaweedFS) cannot be copied and is deleted.</>}
        </Alert>
      </StackItem>
    </Stack>
  );
};

/**
 * Migrate storage, or deleting the old storage afterwards: each its own
 * dialog, opened from its own action. It shows what the run does, runs it
 * with streamed progress, and ends with the outcome. A successful
 * migration ends there: deleting the old storage is a separate, later
 * decision, never offered in the same breath. Closing it does not stop a
 * run.
 */
export const MigrateStorageModal: React.FC<{
  isOpen: boolean;
  /** What this dialog does: migrate, or delete the old storage. */
  kind: MigrateKind;
  legacy: LegacyStorageState | null | undefined;
  run: MigrateRun | null;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  onStart: (kind: MigrateKind, deleteNamespace: boolean, confirmProjects: string[]) => void;
  onClose: () => void;
}> = ({ isOpen, kind, legacy, run, baseReason, onStart, onClose }) => {
  const [deleteNamespace, setDeleteNamespace] = useState(false);
  // Unticked by default: an unrecorded pipeline server is moved only when the user ticks its project.
  const [confirmed, setConfirmed] = useState<string[]>([]);
  const titleId = React.useId();
  const stage = kind;
  // A run of the other kind is not this dialog's.
  const ownRun = run?.kind === stage ? run : null;
  const finished = !!ownRun && !ownRun.running && !!ownRun.result;
  const succeeded = finished && !!ownRun?.result?.success;
  // The action until a run starts, and again after a failure (the outcome says why).
  const showAction = !!legacy && !ownRun?.running && !succeeded;
  const showPlan = showAction && !finished;
  const blocked = baseReason ?? (stage === "remove" ? legacy?.removeBlockedReason || null : null);
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={stage === "remove" ? (succeeded ? "Delete the old storage" : "Delete the old storage?") : "Migrate storage"} labelId={titleId}
        titleIconVariant={stage === "remove" && !succeeded ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {ownRun && (
            <StackItem>
              <RunProgress run={ownRun} />
            </StackItem>
          )}
          {ownRun?.result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={ownRun.result.success ? "success" : ownRun.result.errorCode === "in_progress" || ownRun.result.errorCode === "detached" ? "info" : "danger"}
                title={ownRun.result.success ? (ownRun.kind === "migrate" ? "The data is in the new storage" : "The old storage was deleted") : ownRun.result.errorCode === "in_progress" || ownRun.result.errorCode === "detached" ? "Still running" : "Stopped"}>
                {ownRun.result.message}
              </Alert>
            </StackItem>
          )}
          {showPlan && legacy && (
            <StackItem>
              {stage === "migrate" ? <MigratePlan legacy={legacy} confirmed={confirmed} setConfirmed={setConfirmed} /> : <RemovePlan legacy={legacy} deleteNamespace={deleteNamespace} setDeleteNamespace={setDeleteNamespace} />}
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {showAction && legacy && (
          <TooltipButton variant={stage === "remove" ? "danger" : "primary"} disabledReason={blocked}
            onClick={() => onStart(stage, stage === "remove" && legacy.namespaceDeletable && deleteNamespace, stage === "migrate" ? confirmed.filter((p) => (legacy.unrecorded ?? []).some((n) => projectOf(n) === p)) : [])}>
            {stage === "remove" ? "Delete old storage" : "Migrate storage"}
          </TooltipButton>
        )}
        <Button variant={showAction ? "link" : "primary"} onClick={onClose}>{ownRun?.running || finished ? "Close" : "Cancel"}</Button>
      </ModalFooter>
    </Modal>
  );
};
