import React from "react";
import {
  Alert,
  Button,
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
} from "@patternfly/react-core";
import type { S3KeysState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { CopyableCode } from "./UseIt";
import { RunProgress } from "./MigrateStorageModal";
import { secureServerStep, type MigrateRun } from "./useMigrateStorage";
import { Id, KeptServer, withIds } from "./s3KeysText";

/** Pipeline servers associated with a run, ordered the same way pkg/cluster keyServers returns them. */
export const secureServers = (keys: S3KeysState | null | undefined) => [...(keys?.oldKey ?? []), ...(keys?.ownKey ?? [])].sort();

/** Extra stages appended once checks and rollout finish—one stage per pipeline server in the list above. */
export const secureExtraSteps = (keys: S3KeysState | null | undefined) => secureServers(keys).map(secureServerStep);

/** What a rekeyed server's step carries: the command that restarts its API server (pkg/cluster/internal/storage RestartAPIServerCommand); anything else is not shown. */
const RESTART_COMMAND = /^oc delete pod -n [a-z0-9]([-a-z0-9]*[a-z0-9])? -l app=ds-pipeline-[a-z0-9]([-a-z0-9]*[a-z0-9])?,dspa=[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/** The restart commands of the pipeline servers whose API server started before their key was written, in step order. */
export const restartCommands = (run: MigrateRun | null) =>
  run ? Object.entries(run.steps).filter(([id, s]) => id.startsWith("server:") && s.status === "success" && RESTART_COMMAND.test(s.detail ?? "")).sort(([a], [b]) => a.localeCompare(b)).map(([, s]) => s.detail as string) : [];

/** Static explanation of Secure S3 storage shown before the user starts a run. */
const SecurePlan: React.FC<{ keys: S3KeysState }> = ({ keys }) => {
  const servers = keys.oldKey ?? [];
  const kept = keys.keepOldKey ?? [];
  const left = keys.leftovers ?? [];
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          {keys.secured
            ? "Finishes giving each pipeline server this tool set up a key of its own. Takes a few seconds per pipeline server."
            : <>Gives the S3 storage a new admin key, kept out of every project, and each pipeline server this tool set up a key of its own (SeaweedFS user <Id>rnu-dspa-&lt;project&gt;</Id>, one per project). The old admin key, which every pipeline project holds, stops working. Takes up to 2 minutes; stored data is kept.</>}
        </Content>
      </StackItem>
      <StackItem>
        <Alert variant="warning" isInline component="div" title="Running pipelines and open workbenches">
          <Content component="p">Pipeline runs in progress may fail while the storage restarts and the keys change. Workbenches and model deployments that use the Connection <Id>rhoai-nightly-updater-s3</Id>, and Elyra pipelines in workbenches, keep the old key until they restart: restart them afterwards. So does each pipeline server&apos;s API server, which reads its S3 key only when it starts: while it has not restarted since, the result gives the command that restarts it. This tool restarts none of them.</Content>
        </Alert>
      </StackItem>
      <StackItem>
        <Content component="p">This changes the shared cluster:</Content>
        <List>
          {!keys.secured && <ListItem>SeaweedFS restarts with a new admin key in Secret <Id>seaweedfs-s3-credentials</Id> and with per-server keys turned on, so the admin key can make a key for each pipeline server.</ListItem>}
          {servers.map((n) => (
            <ListItem key={n}><Id>{n}</Id> gets a key of its own (SeaweedFS user <Id>{keys.users?.[n] ?? `rnu-dspa-${n.split("/")[0]}`}</Id>), checked against the storage before it goes into its Secret <Id>nightly-dspa-s3</Id> and its Connection <Id>rhoai-nightly-updater-s3</Id>.</ListItem>
          ))}
          {kept.map((entry) => (
            <ListItem key={entry}><KeptServer entry={entry} /> keeps the old admin key, which then stops working; its owner must give it new S3 keys, or delete it and set it up again with this tool.</ListItem>
          ))}
          {left.map((l) => (
            <ListItem key={l.user}>
              The S3 key of project <Id>{l.project}</Id> (SeaweedFS user <Id>{l.user}</Id>), which has no pipeline server any more, is deleted.
              {l.connectionInUse && <> Its Connection <Id>rhoai-nightly-updater-s3</Id> still holds this key: workbenches and model deployments using it lose S3 access.</>}
            </ListItem>
          ))}
          <ListItem>If it stops part-way, the storage is secured, and the pipeline servers not done yet keep their old key, which no longer works until Secure S3 storage is run again.</ListItem>
        </List>
      </StackItem>
    </Stack>
  );
};

/**
 * Secure S3 storage dialog: plan first, then live step list (checks, rollout,
 * then one block per pipeline server), laid out like Migrate storage's progress
 * view. Work keeps running on the cluster if the modal closes or the browser
 * drops; this page still reflects outstanding steps.
 */
export const SecureStorageModal: React.FC<{
  isOpen: boolean;
  keys: S3KeysState | null | undefined;
  run: MigrateRun | null;
  /** Tooltip text when controls are disabled; absent when actions are allowed. */
  baseReason: string | null;
  onStart: () => void;
  onClose: () => void;
}> = ({ isOpen, keys, run, baseReason, onStart, onClose }) => {
  const titleId = React.useId();
  const ownRun = run?.kind === "secure" ? run : null;
  const finished = !!ownRun && !ownRun.running && !!ownRun.result;
  const succeeded = finished && !!ownRun?.result?.success;
  // Primary button visible idle and after a non-success stop (or an unfinished one); success hides it.
  const showAction = !!keys && !ownRun?.running && (!succeeded || ownRun?.result?.errorCode === "partial");
  // Servers this tool did not give a key keep the old admin key, which no longer works: read from the live status.
  const lost = keys?.secured ? (keys.keepOldKey ?? []).length : 0;
  // Some recorded servers still use the old key (the run said "partial", or the status still lists them).
  const unfinished = ownRun?.result?.errorCode === "partial" || (!!keys?.secured && (keys.oldKey ?? []).length > 0);
  const warn = unfinished || lost > 0;
  const outOfTime = ownRun?.result?.errorCode === "out_of_time";
  const doneTitle = unfinished
    ? "The S3 storage is secured; some pipeline servers still use the old key"
    : lost > 1 ? `The S3 storage is secured; ${lost} pipeline servers lost their S3 access`
    : lost === 1 ? "The S3 storage is secured; 1 pipeline server lost its S3 access"
    : "The S3 storage is secured";
  // Only a lost connection leaves the run going on the server; a run that stopped itself did stop.
  const waiting = ownRun?.result?.errorCode === "detached";
  const restart = ownRun?.result ? restartCommands(ownRun) : [];
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={ownRun ? "Secure S3 storage" : "Secure the S3 storage?"} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          {ownRun && <StackItem><RunProgress run={ownRun} formatMessage={withIds} /></StackItem>}
          {ownRun?.result && (
            <StackItem>
              <Alert component="div" isInline isLiveRegion variant={ownRun.result.success ? (warn ? "warning" : "success") : waiting ? "info" : outOfTime ? "warning" : "danger"}
                title={ownRun.result.success ? doneTitle : waiting ? "Still running" : outOfTime ? "Stopped: out of time; run Secure S3 storage again to continue" : "Stopped"}>
                <Content component="p">{withIds(ownRun.result.message)}</Content>
              </Alert>
            </StackItem>
          )}
          {restart.length > 0 && (
            <StackItem>
              <Content component="p">These pipeline servers&apos; API servers started before their key was written, so they still use the old key, which no longer works, until their pods restart. Delete the pods; their Deployment makes new ones, which read the new key:</Content>
              <CopyableCode id="secure-restart" label="the restart commands" code={restart.join("\n")} />
            </StackItem>
          )}
          {showAction && !finished && keys && <StackItem><SecurePlan keys={keys} /></StackItem>}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {showAction && (
          <TooltipButton variant="primary" disabledReason={baseReason ?? keys?.secureBlockedReason ?? null} onClick={onStart}>
            {finished ? "Run Secure S3 storage again" : "Secure S3 storage"}
          </TooltipButton>
        )}
        <Button variant={showAction ? "link" : "primary"} onClick={onClose}>{ownRun?.running || finished ? "Close" : "Cancel"}</Button>
      </ModalFooter>
    </Modal>
  );
};
