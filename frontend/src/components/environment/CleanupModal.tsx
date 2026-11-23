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
import type { PlaygroundStatus, ResourcesStatus, MaaSStatus, RestorableComponent } from "../../types";
import type { CleanupRequest } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { isTerminating } from "./resourceRow";
import { S3_NAMESPACE, storageTeardownOffered, unrecordedPipelineServerNote } from "./PipelinesTab";
import { gatewayListRelease } from "./modelsConfirm";

/** This tool's pipeline servers: in the data science projects, and in projects that are no longer ones. */
function toolServers(status: ResourcesStatus) {
  return [...(status.pipelineServers ?? []), ...(status.outsidePipelineServers ?? [])].filter((ps) => ps.managedByTool !== false);
}

/** This tool's models: in the data science projects, and in projects that are no longer ones. */
function toolModels(status: ResourcesStatus) {
  return [...(status.models?.items ?? []), ...(status.models?.outside ?? [])].filter((m) => m.managedByTool !== false);
}

/** What a Clean up offers: one feature's parts, or everything. */
export type CleanupScope = "pipelines" | "mlflow" | "models" | "playground" | "all";

/** What this tool created that a clean-up of the scope can delete, from the status. */
export function cleanupCandidates(status: ResourcesStatus, scope: CleanupScope, minioLeftovers: boolean, playground: PlaygroundStatus | null = null, maas: MaaSStatus | null = null) {
  // MaaS: this tool's publications first (with the models, since a published
  // model cannot go without its publication), the platform last (Clean up
  // everything only).
  const maasPublished = scope === "all" || scope === "models" ? [...new Set((maas?.items ?? []).filter((i) => i.published && i.namespace).map((i) => i.namespace!))] : [];
  const maasPlatform = scope === "all" && !!maas?.platform.managedByTool;
  // The playgrounds this tool asked the dashboard for (playground status).
  const playgrounds = scope === "playground" || scope === "all"
    ? (playground?.items ?? []).filter((i) => i.managedByTool && i.deployed && !isTerminating(i) && i.namespace).map((i) => i.namespace!)
    : [];
  const pipelines = scope === "pipelines" || scope === "all";
  const modelsScope = scope === "models" || scope === "all";
  const models = modelsScope
    ? [...new Set(toolModels(status).filter((m) => !isTerminating(m) && m.namespace).map((m) => m.namespace!))]
    : [];
  const gw = status.inferenceGateway;
  const gateway = modelsScope && gw?.managedByTool === true && !isTerminating(gw);
  const servers = pipelines
    ? toolServers(status).filter((ps) => !isTerminating(ps) && ps.namespace).map((ps) => ps.namespace!)
    : [];
  const storage = pipelines && storageTeardownOffered(status.minio, status.legacyStorage, minioLeftovers);
  const minio = status.minio;
  const namespace = storage && minio?.managedByTool !== false && !!minio?.deployed && !!minio?.namespaceDeletable;
  const mlflow = (scope === "mlflow" || scope === "all") && !!status.mlflow?.deployed && status.mlflow.managedByTool !== false && !isTerminating(status.mlflow);
  return { servers, storage, namespace, mlflow, models, gateway, playgrounds, maasPublished, maasPlatform };
}

/** The steps a clean-up streams, in its order (pkg/cluster CleanUp). */
export function cleanupSteps(req: CleanupRequest): { id: string; title: string }[] {
  return [
    ...(req.maasPublished ?? []).map((p) => ({ id: `maas-unpublish:${p}`, title: `Unpublish the models in ${p} from MaaS` })),
    ...(req.pipelineServers ?? []).map((p) => ({ id: `pipeline-server:${p}`, title: `Delete the pipeline server in ${p}` })),
    ...(req.storage ? [{ id: "storage", title: req.deleteNamespace ? `Delete the S3 storage and namespace ${S3_NAMESPACE}` : "Delete the S3 storage" }] : []),
    ...(req.mlflow ? [{ id: "mlflow", title: "Delete MLflow" }] : []),
    ...(req.playgrounds ?? []).map((p) => ({ id: `playground:${p}`, title: `Delete the playground in ${p}` })),
    ...(req.models ?? []).map((p) => ({ id: `model:${p}`, title: `Delete the models in ${p}` })),
    ...(req.inferenceGateway ? [{ id: "inference-gateway", title: "Delete the inference gateway" }] : []),
    ...(req.maasPlatform ? [{ id: "maas-platform", title: "Remove the MaaS platform" }] : []),
    ...(req.components?.length ? [{ id: "components", title: `Turn ${req.components.join(", ")} back` }] : []),
  ];
}

/**
 * Why the selection cannot run, or null: the storage cannot go while a
 * pipeline server that is not selected still uses it (the server refuses
 * that step after deleting the rest).
 */
export function cleanupBlocked(status: ResourcesStatus, req: CleanupRequest, published: string[] = []): string | null {
  const stillPublished = (req.models ?? []).filter((p) => published.includes(p) && !(req.maasPublished ?? []).includes(p));
  if (stillPublished.length > 0) {
    return `A model in ${stillPublished.join(", ")} is published to MaaS. Select the MaaS publications there too, or keep the models.`;
  }
  if (req.inferenceGateway) {
    const selected = new Set(req.models ?? []);
    const users = (status.inferenceGateway?.usedBy ?? []).filter((u) => !selected.has(u.split("/")[0]) || !toolModels(status).some((m) => `${m.namespace}/${m.name}` === u));
    if (users.length > 0) {
      return `Models route through the inference gateway: ${users.join(", ")}. Select ${users.length > 1 ? "those models" : "that model"} too if this tool created ${users.length > 1 ? "them" : "it"}, or keep the gateway.`;
    }
  }
  if (!req.storage) return null;
  const selected = new Set(req.pipelineServers ?? []);
  const users = (status.minio?.usedBy ?? []).filter((u) => !selected.has(u.split("/")[0]));
  if (users.length > 0) {
    const ours = new Set(toolServers(status).map((ps) => ps.namespace));
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
  /** The playground status, for the playgrounds this tool set up (null: none known). */
  playground?: PlaygroundStatus | null;
  /** The MaaS status, for this tool's publications and platform (null: none known). */
  maas?: MaaSStatus | null;
  minioLeftovers: boolean;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  /** One-click prerequisites: the DataScienceCluster components this tool turned on (each feature's restorable). */
  restorable?: RestorableComponent[];
  onConfirm: (req: CleanupRequest) => void;
  onCancel: () => void;
}> = ({ scope, status, playground = null, maas = null, minioLeftovers, baseReason, restorable = [], onConfirm, onCancel }) => {
  const titleId = React.useId();
  const reasonId = React.useId();
  const offered = cleanupCandidates(status, scope, minioLeftovers, playground, maas);
  const [published, setPublished] = useState<string[]>(offered.maasPublished);
  const chosenPublished = published.filter((p) => offered.maasPublished.includes(p));
  const togglePublished = (p: string, v: boolean) => setPublished((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const [maasPlatform, setMaaSPlatform] = useState(offered.maasPlatform);
  // Components are offered for the scope's feature (every feature's with "all"), unchecked: turning one back is a cluster change of its own.
  const offeredComponents = restorable.filter((r) => scope === "all" || r.feature === scope);
  const [components, setComponents] = useState<string[]>([]);
  const chosenComponents = components.filter((k) => offeredComponents.some((r) => r.component === k));
  const toggleComponent = (k: string, v: boolean) => setComponents((s) => (v ? [...s.filter((x) => x !== k), k] : s.filter((x) => x !== k)));
  const [playgrounds, setPlaygrounds] = useState<string[]>(offered.playgrounds);
  const chosenPlaygrounds = playgrounds.filter((p) => offered.playgrounds.includes(p));
  const togglePlayground = (p: string, v: boolean) => setPlaygrounds((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  // A pipeline server this tool has no record of is unticked: ticking it is the user's confirmation that it is this tool's.
  const unrecordedServers = toolServers(status).filter((ps) => ps.unrecorded && ps.namespace && offered.servers.includes(ps.namespace)).map((ps) => ps.namespace!);
  const [servers, setServers] = useState<string[]>(offered.servers.filter((p) => !unrecordedServers.includes(p)));
  const [storage, setStorage] = useState(offered.storage);
  const [namespace, setNamespace] = useState(false);
  const [mlflow, setMlflow] = useState(offered.mlflow);
  const [models, setModels] = useState<string[]>(offered.models);
  const [gateway, setGateway] = useState(offered.gateway);
  const chosenModels = models.filter((m) => offered.models.includes(m));
  const chosenServers = servers.filter((s) => offered.servers.includes(s));
  const confirmedServers = chosenServers.filter((s) => unrecordedServers.includes(s));
  const req: CleanupRequest = {
    ...(chosenPublished.length ? { maasPublished: chosenPublished } : {}),
    ...(chosenServers.length ? { pipelineServers: chosenServers } : {}),
    ...(confirmedServers.length ? { confirmUnrecorded: confirmedServers } : {}),
    ...(storage && offered.storage ? { storage: true, ...(namespace && offered.namespace ? { deleteNamespace: true } : {}) } : {}),
    ...(mlflow && offered.mlflow ? { mlflow: true } : {}),
    ...(chosenPlaygrounds.length ? { playgrounds: chosenPlaygrounds } : {}),
    ...(chosenModels.length ? { models: chosenModels } : {}),
    ...(gateway && offered.gateway ? { inferenceGateway: true } : {}),
    ...(maasPlatform && offered.maasPlatform ? { maasPlatform: true } : {}),
    ...(chosenComponents.length ? { components: chosenComponents } : {}),
  };
  const nothing = !req.pipelineServers && !req.storage && !req.mlflow && !req.models && !req.inferenceGateway && !req.playgrounds && !req.maasPublished && !req.maasPlatform && !req.components;
  const reason = baseReason ?? (nothing ? "Select something to delete." : cleanupBlocked(status, req, offered.maasPublished));
  const lost = [
    ...(req.pipelineServers ?? []).map((p) => <>The runs and experiments of the pipeline server in {p}.</>),
    ...(req.storage ? [<>Every file in the S3 storage: pipeline artifacts and uploaded files{status.legacyStorage ? ", and the storage of an earlier version" : ""}.</>] : []),
    ...(req.deleteNamespace ? [<>Everything else in namespace <code>{S3_NAMESPACE}</code>.</>] : []),
    ...(req.mlflow ? [<>Every MLflow experiment, run and model, in every workspace.</>] : []),
    ...(req.playgrounds ?? []).map((p) => <>The chats and uploaded documents of the playground in {p}.</>),
    ...(req.maasPlatform ? [<>MaaS API keys, and the MaaS database with their records.</>] : []),
  ];
  const toggleServer = (p: string, v: boolean) => setServers((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const toggleModel = (p: string, v: boolean) => setModels((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const nothingOffered = offered.servers.length === 0 && !offered.storage && !offered.mlflow && offered.models.length === 0 && !offered.gateway && offered.playgrounds.length === 0 && offered.maasPublished.length === 0 && !offered.maasPlatform && offeredComponents.length === 0;
  const title = scope === "all" ? "Clean up everything?" : scope === "pipelines" ? "Clean up Pipelines?" : scope === "models" ? "Clean up Models?" : scope === "playground" ? "Clean up Playground?" : "Clean up MLflow?";
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
                {offered.maasPublished.length > 0 && (
                  <FormGroup role="group" isStack label="MaaS publications" fieldId="cleanup-maas-published">
                    {offered.maasPublished.map((p) => (
                      <Checkbox key={p} id={`cleanup-maas-published-${p}`} label={<>Model in {p}, published to MaaS</>} description="Unpublished first, before anything else goes." isChecked={published.includes(p)} onChange={(_e, v) => togglePublished(p, v)} />
                    ))}
                  </FormGroup>
                )}
                {offered.servers.length > 0 && (
                  <FormGroup role="group" isStack label="Pipeline servers" fieldId="cleanup-servers">
                    {offered.servers.map((p) => (
                      <Checkbox key={p} id={`cleanup-server-${p}`} label={<>Pipeline server in {p}</>} isChecked={servers.includes(p)} onChange={(_e, v) => toggleServer(p, v)}
                        description={unrecordedServers.includes(p) ? unrecordedPipelineServerNote(p) : undefined} />
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
                {offered.playgrounds.length > 0 && (
                  <FormGroup role="group" isStack label="Playground" fieldId="cleanup-playgrounds">
                    {offered.playgrounds.map((p) => (
                      <Checkbox key={p} id={`cleanup-playground-${p}`} label={<>Playground in {p}</>} description="Deleted through the dashboard, before the models it uses." isChecked={playgrounds.includes(p)} onChange={(_e, v) => togglePlayground(p, v)} />
                    ))}
                  </FormGroup>
                )}
                {(offered.models.length > 0 || offered.gateway) && (
                  <FormGroup role="group" isStack label="Models" fieldId="cleanup-models">
                    {offered.models.map((p) => (
                      <Checkbox key={p} id={`cleanup-model-${p}`} label={<>Models in {p}</>} isChecked={models.includes(p)} onChange={(_e, v) => toggleModel(p, v)}
                        description={<>{toolModels(status).filter((m) => m.namespace === p).map((m) => m.name).join(", ")}{gatewayListRelease(status, p, toolModels(status).filter((m) => m.namespace === p).map((m) => m.name!)) ? <>. {p} is taken off the gateway&apos;s projects; routes made by hand there stop attaching.</> : null}</>} />
                    ))}
                    {offered.gateway && (
                      <Checkbox id="cleanup-gateway" label="Inference gateway" description="With its cloud load balancer and DNS record. Deleted after the models." isChecked={gateway} onChange={(_e, v) => setGateway(v)} />
                    )}
                  </FormGroup>
                )}
                {offered.maasPlatform && (
                  <FormGroup role="group" isStack label="MaaS" fieldId="cleanup-maas-platform">
                    <Checkbox id="cleanup-maas-platform" label="MaaS platform" description="Last: MaaS turned back off as it was, then the gateway and database this tool created." isChecked={maasPlatform} onChange={(_e, v) => setMaaSPlatform(v)} />
                  </FormGroup>
                )}
                {offeredComponents.length > 0 && (
                  <FormGroup role="group" isStack label="Cluster components this tool turned on" fieldId="cleanup-components">
                    {offeredComponents.map((r) => (
                      <Checkbox key={r.component} id={`cleanup-component-${r.component}`} label={<>Turn <code>{r.component}</code> back to {r.previous || "Removed"}</>}
                        description="Last, and only while nothing on the cluster uses it and nobody has changed it since. Someone who only set it to Managed again is not noticed, so then the check that nothing uses it is what keeps it on. The operators it needed stay." isChecked={chosenComponents.includes(r.component)} onChange={(_e, v) => toggleComponent(r.component, v)} />
                    ))}
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
