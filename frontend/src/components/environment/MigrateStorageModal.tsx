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

const codes = (items: string[]) => items.map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>);

/**
 * The ordered steps of a run. The step that stopped the run shows no text
 * of its own when the outcome alert says the same.
 */
const RunProgress: React.FC<{ run: MigrateRun }> = ({ run }) => (
  <StepProgress idPrefix="migrate" aria-label={run.kind === "migrate" ? "Migrate storage progress" : "Remove old storage progress"}
    steps={MIGRATE_STEPS[run.kind]} states={run.steps} running={run.running}
    shownElsewhere={run.result && !run.result.success ? run.result.message : undefined} />
);

/** What Migrate storage does, before it runs. */
const MigratePlan: React.FC<{ legacy: LegacyStorageState }> = ({ legacy }) => {
  const repoint = legacy.repoint ?? [];
  const notMoved = legacy.notMoved ?? [];
  const uncopied = legacy.uncopiedPVCs ?? [];
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          {legacy.server ?? "The storage"} in namespace <code>{legacy.namespace}</code> holds the data of earlier versions of this tool. Migrate storage moves it to namespace <code>{S3_NAMESPACE}</code>:
        </Content>
        <List component="ol">
          <ListItem>Sets up the storage in <code>{S3_NAMESPACE}</code> with the same access key and secret key.</ListItem>
          <ListItem>Copies every bucket and object with Job <code>seaweedfs-migrate</code> and checks each one byte for byte. The run reports how many objects and bytes it copied; any difference stops it here, with nothing else changed.</ListItem>
          <ListItem>
            {repoint.length > 0
              ? <>Moves the pipeline server{repoint.length > 1 ? "s" : ""} this tool created to the new storage, one at a time, and waits until each reaches it: {codes(repoint)}.</>
              : <>No pipeline server of this tool uses the old storage, so none is moved.</>}
          </ListItem>
        </List>
      </StackItem>
      {notMoved.length > 0 && (
        <StackItem>
          <Alert component="p" variant="info" isInline title={`Not moved: ${notMoved.length > 1 ? "these pipeline servers" : "this pipeline server"}`}>
            {notMoved.join("; ")}. Point {notMoved.length > 1 ? "them" : "it"} at the new storage yourself (the checklist is shown after the migration).
          </Alert>
        </StackItem>
      )}
      {uncopied.length > 0 && (
        <StackItem>
          <Content component="p">
            {codes(uncopied)} holds MinIO&apos;s files from before SeaweedFS replaced it. No running server reads {uncopied.length > 1 ? "them" : "it"}, so {uncopied.length > 1 ? "they" : "it"} cannot be copied; only a MinIO server could read that format.
          </Content>
        </StackItem>
      )}
      <StackItem>
        <Content component="p">The old storage is kept. You remove it in the last step, after reading the summary.</Content>
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
          <ListItem>The access key and the secret key are the same.</ListItem>
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
          First the run copies and checks any object written to the old storage since the migration. Then it deletes what this tool created in namespace <code>{legacy.namespace}</code>: {codes(legacy.objects)}.
          {pvcs.length > 0 && <> Deleted with {pvcs.length > 1 ? "them" : "it"}: the data in {codes(pvcs)}.</>}
          {uncopied.length > 0 && <> {codes(uncopied)} (MinIO&apos;s files from before SeaweedFS) cannot be copied and is deleted.</>}
        </Alert>
      </StackItem>
    </Stack>
  );
};

/**
 * Migrate storage: one dialog for the whole move. It shows what the next
 * part does (Migrate, then Remove the old storage), runs it with streamed
 * progress, and shows the outcome. Closing it does not stop a run.
 */
export const MigrateStorageModal: React.FC<{
  isOpen: boolean;
  legacy: LegacyStorageState | null | undefined;
  run: MigrateRun | null;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  onStart: (kind: MigrateKind, deleteNamespace: boolean) => void;
  onClose: () => void;
}> = ({ isOpen, legacy, run, baseReason, onStart, onClose }) => {
  const [deleteNamespace, setDeleteNamespace] = useState(false);
  const titleId = React.useId();
  // After a successful Migrate the next part is the removal, even before
  // the status has reloaded.
  const stage: MigrateKind = run?.kind === "migrate" && run.result?.success ? "remove" : legacy?.stage === "remove" ? "remove" : "migrate";
  const finished = !!run && !run.running && !!run.result;
  const removed = run?.kind === "remove" && run.result?.success;
  // The action for the next part, unless a run is going or all is done;
  // its plan unless that part just failed (the outcome says why).
  const showAction = !!legacy && !run?.running && !removed;
  const showPlan = showAction && !(finished && !run?.result?.success && run?.kind === stage);
  const blocked = baseReason ?? (stage === "remove" ? legacy?.removeBlockedReason || null : null);
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={stage === "remove" && !removed ? "Remove the old storage?" : "Migrate storage"} labelId={titleId}
        titleIconVariant={stage === "remove" && !removed ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {run && (
            <StackItem>
              <RunProgress run={run} />
            </StackItem>
          )}
          {run?.result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={run.result.success ? "success" : run.result.errorCode === "in_progress" || run.result.errorCode === "detached" ? "info" : "danger"}
                title={run.result.success ? (run.kind === "migrate" ? "Migrated" : "Old storage removed") : run.result.errorCode === "in_progress" || run.result.errorCode === "detached" ? "Still running" : "Stopped"}>
                {run.result.message}
              </Alert>
            </StackItem>
          )}
          {showPlan && legacy && (
            <StackItem>
              {stage === "migrate" ? <MigratePlan legacy={legacy} /> : <RemovePlan legacy={legacy} deleteNamespace={deleteNamespace} setDeleteNamespace={setDeleteNamespace} />}
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {showAction && legacy && (
          <TooltipButton variant={stage === "remove" ? "danger" : "primary"} disabledReason={blocked}
            onClick={() => onStart(stage, stage === "remove" && legacy.namespaceDeletable && deleteNamespace)}>
            {stage === "remove" ? "Remove old storage" : "Migrate storage"}
          </TooltipButton>
        )}
        <Button variant="link" onClick={onClose}>{run?.running || finished ? "Close" : "Cancel"}</Button>
      </ModalFooter>
    </Modal>
  );
};
