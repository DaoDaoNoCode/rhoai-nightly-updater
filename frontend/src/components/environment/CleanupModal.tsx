import React, { useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  Form,
  FormGroup,
  HelperText,
  HelperTextItem,
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
import type { ResourcesStatus } from "../../types";
import type { CleanupRequest } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { isTerminating } from "./resourceRow";
import { S3_NAMESPACE, storageTeardownOffered } from "./PipelinesTab";

/** What a Clean up offers: one feature's parts, or everything. */
export type CleanupScope = "pipelines" | "mlflow" | "all";

/** What this tool created that a clean-up of the scope can delete, from the status. */
export function cleanupCandidates(status: ResourcesStatus, scope: CleanupScope, minioLeftovers: boolean) {
  const pipelines = scope !== "mlflow";
  const servers = pipelines
    ? (status.pipelineServers ?? []).filter((ps) => ps.managedByTool !== false && !isTerminating(ps) && ps.namespace).map((ps) => ps.namespace!)
    : [];
  const storage = pipelines && storageTeardownOffered(status.minio, status.legacyStorage, minioLeftovers);
  const minio = status.minio;
  const namespace = storage && minio?.managedByTool !== false && !!minio?.deployed && !!minio?.namespaceDeletable;
  const mlflow = scope !== "pipelines" && !!status.mlflow?.deployed && status.mlflow.managedByTool !== false && !isTerminating(status.mlflow);
  return { servers, storage, namespace, mlflow };
}

/** The steps a clean-up streams, in its order (pkg/cluster CleanUp). */
export function cleanupSteps(req: CleanupRequest): { id: string; title: string }[] {
  return [
    ...(req.pipelineServers ?? []).map((p) => ({ id: `pipeline-server:${p}`, title: `Delete the pipeline server in ${p}` })),
    ...(req.storage ? [{ id: "storage", title: req.deleteNamespace ? `Delete the S3 storage and namespace ${S3_NAMESPACE}` : "Delete the S3 storage" }] : []),
    ...(req.mlflow ? [{ id: "mlflow", title: "Delete MLflow" }] : []),
  ];
}

/**
 * Why the selection cannot run, or null: the storage cannot go while a
 * pipeline server that is not selected still uses it (the server refuses
 * that step after deleting the rest).
 */
export function cleanupBlocked(status: ResourcesStatus, req: CleanupRequest): string | null {
  if (!req.storage) return null;
  const selected = new Set(req.pipelineServers ?? []);
  const users = (status.minio?.usedBy ?? []).filter((u) => !selected.has(u.split("/")[0]));
  if (users.length > 0) {
    const ours = new Set((status.pipelineServers ?? []).filter((ps) => ps.managedByTool !== false).map((ps) => ps.namespace));
    const foreign = users.filter((u) => !ours.has(u.split("/")[0]));
    return foreign.length > 0
      ? `The S3 storage is used by ${foreign.join(", ")}, which this tool did not create. That pipeline server must be deleted or moved to other storage first.`
      : `The S3 storage is used by ${users.join(", ")}. Select ${users.length > 1 ? "those pipeline servers" : "that pipeline server"} too, or keep the storage.`;
  }
  if (!status.minio?.usedBy && status.minio?.teardownBlockedReason) return status.minio.teardownBlockedReason;
  return null;
}

/**
 * Clean up: one confirmation that lists everything this tool created for
 * the scope, each with a checkbox (all ticked; the namespace not), what is
 * lost, and one Delete that runs it all as one operation. Mount it when it
 * opens: it starts from what is offered then, and polling does not undo
 * the user's choices.
 */
export const CleanupModal: React.FC<{
  scope: CleanupScope;
  status: ResourcesStatus;
  minioLeftovers: boolean;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  onConfirm: (req: CleanupRequest) => void;
  onCancel: () => void;
}> = ({ scope, status, minioLeftovers, baseReason, onConfirm, onCancel }) => {
  const titleId = React.useId();
  const reasonId = React.useId();
  const offered = cleanupCandidates(status, scope, minioLeftovers);
  const [servers, setServers] = useState<string[]>(offered.servers);
  const [storage, setStorage] = useState(offered.storage);
  const [namespace, setNamespace] = useState(false);
  const [mlflow, setMlflow] = useState(offered.mlflow);
  const chosenServers = servers.filter((s) => offered.servers.includes(s));
  const req: CleanupRequest = {
    ...(chosenServers.length ? { pipelineServers: chosenServers } : {}),
    ...(storage && offered.storage ? { storage: true, ...(namespace && offered.namespace ? { deleteNamespace: true } : {}) } : {}),
    ...(mlflow && offered.mlflow ? { mlflow: true } : {}),
  };
  const nothing = !req.pipelineServers && !req.storage && !req.mlflow;
  const reason = baseReason ?? (nothing ? "Select something to delete." : cleanupBlocked(status, req));
  const lost = [
    ...(req.pipelineServers ?? []).map((p) => <>The runs and experiments of the pipeline server in {p}.</>),
    ...(req.storage ? [<>Every file in the S3 storage: pipeline artifacts and uploaded files{status.legacyStorage ? ", and the storage of an earlier version" : ""}.</>] : []),
    ...(req.deleteNamespace ? [<>Everything else in namespace <code>{S3_NAMESPACE}</code>.</>] : []),
    ...(req.mlflow ? [<>Every MLflow experiment, run and model, in every workspace.</>] : []),
  ];
  const toggleServer = (p: string, v: boolean) => setServers((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const nothingOffered = offered.servers.length === 0 && !offered.storage && !offered.mlflow;
  const title = scope === "all" ? "Clean up everything?" : scope === "pipelines" ? "Clean up Pipelines?" : "Clean up MLflow?";
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={onCancel}>
      <ModalHeader title={title} labelId={titleId} titleIconVariant="warning" />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              {nothingOffered
                ? "This tool created nothing here that is left to delete."
                : "Deletes what this tool created, in this order. Something this tool did not create is never deleted. The parts run one after another as one operation; a part that fails stops the rest."}
            </Content>
          </StackItem>
          {!nothingOffered && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                {offered.servers.length > 0 && (
                  <FormGroup role="group" isStack label="Pipeline servers" fieldId="cleanup-servers">
                    {offered.servers.map((p) => (
                      <Checkbox key={p} id={`cleanup-server-${p}`} label={<>Pipeline server in {p}</>} isChecked={servers.includes(p)} onChange={(_e, v) => toggleServer(p, v)} />
                    ))}
                  </FormGroup>
                )}
                {offered.storage && (
                  <FormGroup role="group" isStack label="Storage" fieldId="cleanup-storage">
                    <Checkbox id="cleanup-storage" label={status.legacyStorage ? "S3 storage, and the storage of an earlier version" : "S3 storage"} isChecked={storage} onChange={(_e, v) => setStorage(v)}
                      body={offered.namespace && storage ? (
                        <Checkbox id="cleanup-namespace" label={<>Also delete namespace <code>{S3_NAMESPACE}</code>, with everything in it</>} isChecked={namespace} onChange={(_e, v) => setNamespace(v)}
                          description={(status.minio?.otherObjects ?? []).length > 0
                            ? <>Besides the storage, this tool sees there: {(status.minio?.otherObjects ?? []).join(", ")}.</>
                            : <>Besides the storage, this tool sees no Deployments or PersistentVolumeClaims there; it cannot list every kind.</>} />
                      ) : undefined} />
                  </FormGroup>
                )}
                {offered.mlflow && (
                  <FormGroup role="group" isStack label="MLflow" fieldId="cleanup-mlflow">
                    <Checkbox id="cleanup-mlflow" label="MLflow server" isChecked={mlflow} onChange={(_e, v) => setMlflow(v)} />
                  </FormGroup>
                )}
              </Form>
            </StackItem>
          )}
          {lost.length > 0 && (
            <StackItem>
              <Alert component="div" variant="danger" isInline title="Data is deleted and cannot be recovered">
                <List>{lost.map((l, i) => <ListItem key={i}>{l}</ListItem>)}</List>
              </Alert>
            </StackItem>
          )}
          {!nothingOffered && reason && (
            <StackItem>
              <HelperText id={reasonId}><HelperTextItem variant={baseReason || nothing ? "default" : "warning"}>{reason}</HelperTextItem></HelperText>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {!nothingOffered && (
          <TooltipButton variant="danger" disabledReason={reason} aria-describedby={reason ? reasonId : undefined} onClick={() => onConfirm(req)}>Delete</TooltipButton>
        )}
        <Button variant="link" onClick={onCancel}>{nothingOffered ? "Close" : "Cancel"}</Button>
      </ModalFooter>
    </Modal>
  );
};
