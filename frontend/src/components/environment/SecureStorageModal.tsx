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
import { RunProgress } from "./MigrateStorageModal";
import { secureServerStep, type MigrateRun } from "./useMigrateStorage";

const codes = (items: string[]) => items.map((n, i) => <React.Fragment key={n}>{i > 0 ? ", " : ""}<code>{n}</code></React.Fragment>);

/** KeepOldKey entries ("ns/name (reason)"): the name as code, the reason as words that do not wrap. */
const keptNodes = (items: string[]) => items.map((entry, i) => {
  const at = entry.indexOf(" (");
  const name = at < 0 ? entry : entry.slice(0, at);
  return <React.Fragment key={entry}>{i > 0 ? ", " : ""}<code>{name}</code>{at >= 0 && <> <span style={{ whiteSpace: "nowrap" }}>{entry.slice(at + 1)}</span></>}</React.Fragment>;
});

/** Pipeline servers associated with a run, ordered the same way pkg/cluster keyServers returns them. */
export const secureServers = (keys: S3KeysState | null | undefined) => [...(keys?.oldKey ?? []), ...(keys?.ownKey ?? [])].sort();

/** Extra stages appended once checks and rollout finish—one stage per pipeline server in the list above. */
export const secureExtraSteps = (keys: S3KeysState | null | undefined) => secureServers(keys).map(secureServerStep);

/** Static explanation of Secure S3 storage shown before the user starts a run. */
const SecurePlan: React.FC<{ keys: S3KeysState }> = ({ keys }) => {
  const servers = keys.oldKey ?? [];
  const kept = keys.keepOldKey ?? [];
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          {keys.secured
            ? "Finishes giving each pipeline server this tool set up a key of its own. Takes a few seconds per pipeline server."
            : "Gives the S3 storage a new admin key, kept out of every project, and each pipeline server this tool set up a key of its own (SeaweedFS user rnu-dspa-<project>, one per project). The old admin key, which every pipeline project holds, stops working. Takes up to 2 minutes; stored data is kept."}
        </Content>
      </StackItem>
      <StackItem>
        <Alert variant="warning" isInline component="div" title="Running pipelines and open workbenches">
          <Content component="p">Pipeline runs in progress may fail while the storage restarts and the keys change. Workbenches and model deployments that use the Connection or run pipelines (Elyra) keep the old key until they restart: restart them afterwards. This tool restarts none of them.</Content>
        </Alert>
      </StackItem>
      <StackItem>
        <Content component="p">This changes the shared cluster:</Content>
        <List>
          {!keys.secured && <ListItem>SeaweedFS restarts with a new admin key in Secret <code>seaweedfs-s3-credentials</code> and with per-server keys turned on: the admin key can then make a key for each pipeline server, and a pipeline server&apos;s key cannot make keys.</ListItem>}
          <ListItem>
            {servers.length > 0
              ? <>These pipeline servers get a key of their own, checked against the storage before it is written into their Secrets <code>nightly-dspa-s3</code> and <code>rhoai-nightly-updater-s3</code> (the dashboard Connection): {codes(servers)}.</>
              : <>No pipeline server this tool recorded uses the old key.</>}
          </ListItem>
          {kept.length > 0 && <ListItem>Not changed by this tool; they keep the old admin key, which then no longer works, and their owner must give them new S3 keys: {keptNodes(kept)}.</ListItem>}
          <ListItem>If it stops part-way, the storage is secured and the pipeline servers not done yet keep the old key: running it again finishes them and skips the ones done.</ListItem>
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
  // Primary button visible idle and after a non-success stop; success hides it until the user chooses to run again.
  const showAction = !!keys && !ownRun?.running && !succeeded;
  // Servers this tool did not give a key keep the old admin key, which no longer works: read from the live status.
  const leftBehind = !!keys?.secured && (keys.keepOldKey ?? []).length > 0;
  const waiting = ownRun?.result && (ownRun.result.errorCode === "in_progress" || ownRun.result.errorCode === "detached");
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={ownRun ? "Secure S3 storage" : "Secure the S3 storage?"} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          {ownRun && <StackItem><RunProgress run={ownRun} /></StackItem>}
          {ownRun?.result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={ownRun.result.success ? (leftBehind ? "warning" : "success") : waiting ? "info" : "danger"}
                title={ownRun.result.success ? (leftBehind ? "The S3 storage is secured; some pipeline servers lost their S3 access" : "The S3 storage is secured") : waiting ? "Still running" : "Stopped"}>
                {ownRun.result.message}
              </Alert>
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
