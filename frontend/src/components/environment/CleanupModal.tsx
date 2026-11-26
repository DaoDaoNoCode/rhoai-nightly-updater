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
import { storageInUseText } from "./workbenchState";
import { confirmationOf, type ConnectionWorkbench, type KueueStatus, type ModelRegistryStatus, type ObservabilityStatus, type PlaygroundStatus, type ResourcesStatus, type MaaSStatus, type RestorableComponent, type WorkbenchStatus } from "../../types";
import type { CleanupRequest } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { isTerminating } from "./resourceRow";
import { connectionEnvOnlyChange, connectionWorkbenchesChange, unrecordedPipelineServerNote } from "./PipelinesTab";
import { S3_NAMESPACE, storageTeardownOffered } from "./StorageTab";
import { gatewayListRelease } from "./modelsConfirm";
import { kueueAnything } from "./kueueState";

/**
 * The model registry component's own words in the Clean up dialog (pkg/cluster/internal/envkit
 * ModelRegistryComponent): null for another component; for aiHub (RHOAI 3.6) what else it removes. Offered only
 * when this tool turned it on and still owns the field (restorable); the server turns it back only when no model
 * registry is left.
 */
export function modelRegistryComponentLine(component: string): string | null {
  if (component === "aiHub") return "this also removes the Model Catalog.";
  if (component === "modelregistry") return "";
  return null;
}

/** A restorable component's checkbox label (also the Distributed workloads tab's Turn back). */
export function componentLabel(r: RestorableComponent): React.ReactNode {
  if (r.component === "kueue") {
    return <>Turn the DataScienceCluster&apos;s <code>kueue</code> back from Unmanaged to {r.previous || "Removed"}, only once no ClusterQueue or LocalQueue is left on the cluster: the dashboard&apos;s Workload metrics page (Observe &amp; monitor) goes away. The Red Hat build of Kueue and cert-manager stay.</>;
  }
  const line = modelRegistryComponentLine(r.component);
  if (line === null) return <>Turn <code>{r.component}</code> back to {r.previous || "Removed"}</>;
  return <>Turn the model registry component (<code>{r.component}</code>) back to {r.previous || "Removed"}{line ? `; ${line}` : "."}</>;
}

/** This tool's pipeline servers: in the data science projects, and in projects that are no longer ones. */
function toolServers(status: ResourcesStatus) {
  return [...(status.pipelineServers ?? []), ...(status.outsidePipelineServers ?? [])].filter((ps) => ps.managedByTool !== false);
}

/** This tool's models: in the data science projects, and in projects that are no longer ones. */
function toolModels(status: ResourcesStatus) {
  return [...(status.models?.items ?? []), ...(status.models?.outside ?? [])].filter((m) => m.managedByTool !== false);
}

/** What a Clean up offers: one feature's parts, or everything. */
export type CleanupScope = "pipelines" | "workbench" | "mlflow" | "model-registry" | "models" | "playground" | "kueue" | "all";

/** What this tool created that a clean-up of the scope can delete, from the status. */
export function cleanupCandidates(status: ResourcesStatus, scope: CleanupScope, minioLeftovers: boolean, playground: PlaygroundStatus | null = null, maas: MaaSStatus | null = null, registry: ModelRegistryStatus | null = null, workbench: WorkbenchStatus | null = null, kueue: KueueStatus | null = null, observability: ObservabilityStatus | null = null) {
  // MaaS: this tool's publications first (with the models, since a published
  // model cannot go without its publication), the platform last (Clean up
  // everything only).
  const maasPublished = scope === "all" || scope === "models" ? [...new Set((maas?.items ?? []).filter((i) => i.published && i.namespace).map((i) => i.namespace!))] : [];
  const maasPlatform = scope === "all" && !!maas?.platform.managedByTool;
  // The playgrounds this tool asked the dashboard for (playground status).
  const playgrounds = scope === "playground" || scope === "all"
    ? (playground?.items ?? []).filter((i) => i.managedByTool && i.deployed && !isTerminating(i) && i.namespace).map((i) => i.namespace!)
    : [];
  // This tool's workbenches (workbench status): their storage is offered unticked.
  const workbenches = scope === "workbench" || scope === "all"
    ? (workbench?.items ?? []).filter((i) => i.managedByTool && i.deployed && !isTerminating(i) && i.namespace).map((i) => i.namespace!)
    : [];
  const workbenchStorage = workbenches.filter((p) => !!(workbench?.items ?? []).find((i) => i.namespace === p)?.storage?.managedByTool);
  // Storage this tool kept from a deleted workbench: offered on its own, unticked.
  const keptStorage = scope === "workbench" || scope === "all"
    ? (workbench?.items ?? []).filter((i) => !i.deployed && i.storage?.managedByTool && i.namespace).map((i) => i.namespace!)
    : [];
  // Workbenches whose running session a deletion ends.
  const runningWorkbenches = workbenches.filter((p) => ["running", "starting"].includes((workbench?.items ?? []).find((i) => i.namespace === p)?.state ?? ""));
  const storageOf = (p: string) => (workbench?.items ?? []).find((i) => i.namespace === p)?.storage;
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
  // The S3 storage is the cluster's (pipelines, MaaS observability, other teams): only Clean up everything offers it, besides Delete S3 storage on its own tab.
  const storage = scope === "all" && storageTeardownOffered(status.minio, status.legacyStorage, minioLeftovers);
  const minio = status.minio;
  const namespace = storage && minio?.managedByTool !== false && !!minio?.deployed && !!minio?.namespaceDeletable;
  const mlflow = (scope === "mlflow" || scope === "all") && !!status.mlflow?.deployed && status.mlflow.managedByTool !== false && !isTerminating(status.mlflow);
  // This tool's model registry (its UID is the recorded one).
  const modelRegistry = (scope === "model-registry" || scope === "all") && !!registry?.managedByTool && registry.state !== "absent" && registry.state !== "terminating";
  // This tool's distributed workloads setup (its record: queues, samples, label, opt-in).
  const kueueSetup = (scope === "kueue" || scope === "all") && kueueAnything(kueue);
  // The usage logs and dashboards this tool set up (Clean up everything only).
  const usageLogs = scope === "all" && !!observability?.managedByTool;
  // The projects recipes created that the cluster has (Clean up everything only).
  const recipeProjects = scope === "all" ? (status.recipeProjects ?? []).map((p) => p.name) : [];
  // The project S3 storages this tool recorded (Clean up everything only); each volume is offered unticked.
  const projectStorages = scope === "all" ? (status.projectStorages ?? []).map((p) => p.project) : [];
  return { projectStorages, workbenches, workbenchStorage, keptStorage, runningWorkbenches, storageOf, servers, storage, namespace, mlflow, modelRegistry, kueue: kueueSetup, observability: usageLogs, models, gateway, playgrounds, maasPublished, maasPlatform, recipeProjects };
}

/**
 * A clean-up the server refused before deleting anything because recipe
 * projects hold what this tool did not create: the request, and per
 * project what to confirm (the refused steps' detail, one per line).
 */
export interface CleanupRetry {
  req: CleanupRequest;
  contents: Record<string, string[]>;
  /** Per pipeline server's project, the workbenches that use its dashboard Connection (errorCode confirm_connection_workbenches). */
  workbenches?: Record<string, ConnectionWorkbench[]>;
}

/** The steps a clean-up streams, in its order (pkg/cluster CleanUp). */
export function cleanupSteps(req: CleanupRequest): { id: string; title: string }[] {
  return [
    ...(req.maasPublished ?? []).map((p) => ({ id: `maas-unpublish:${p}`, title: `Unpublish the models in ${p} from MaaS` })),
    ...(req.workbenches ?? []).map((p) => ({ id: `workbench:${p}`, title: (req.workbenchStorage ?? []).includes(p) ? `Delete the workbench and its storage in ${p}` : `Delete the workbench in ${p}` })),
    ...(req.workbenchStorage ?? []).filter((p) => !(req.workbenches ?? []).includes(p)).map((p) => ({ id: `workbench-storage:${p}`, title: `Delete the workbench storage kept in ${p}` })),
    ...(req.pipelineServers ?? []).map((p) => ({ id: `pipeline-server:${p}`, title: `Delete the pipeline server in ${p}` })),
    ...(req.projectStorages ?? []).map((p) => ({ id: `project-s3:${p}`, title: (req.projectStorageData ?? []).includes(p) ? `Delete the S3 storage of ${p} and its volume` : `Delete the S3 storage of ${p}` })),
    ...(req.observability ? [{ id: "observability", title: "Clean up the usage logs and dashboards" }] : []),
    ...(req.storage ? [{ id: "storage", title: req.deleteNamespace ? `Delete the S3 storage and namespace ${S3_NAMESPACE}` : "Delete the S3 storage" }] : []),
    ...(req.mlflow ? [{ id: "mlflow", title: "Delete MLflow" }] : []),
    ...(req.modelRegistry ? [{ id: "model-registry", title: "Delete the model registry" }] : []),
    ...(req.playgrounds ?? []).map((p) => ({ id: `playground:${p}`, title: `Delete the playground in ${p}` })),
    ...(req.models ?? []).map((p) => ({ id: `model:${p}`, title: `Delete the models in ${p}` })),
    ...(req.inferenceGateway ? [{ id: "inference-gateway", title: "Delete the inference gateway" }] : []),
    ...(req.kueue ? [{ id: "kueue", title: "Delete the distributed workloads setup" }] : []),
    ...(req.maasPlatform ? [{ id: "maas-platform", title: "Remove the MaaS platform" }] : []),
    ...(req.recipeProjects ?? []).map((p) => ({ id: `recipe-project:${p}`, title: `Delete project ${p}, which a recipe created` })),
    ...(req.components?.length ? [{ id: "components", title: `Turn ${req.components.join(", ")} back` }] : []),
  ];
}

/**
 * Why the selection cannot run, or null: the storage cannot go while a
 * pipeline server that is not selected still uses it (the server refuses
 * that step after deleting the rest).
 */
export function cleanupBlocked(status: ResourcesStatus, req: CleanupRequest, published: string[] = [], usageLogsOnStorage = false): string | null {
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
  if (usageLogsOnStorage && !req.observability) {
    return "RHOAI's usage logs (the Observability tab) keep their data in the S3 storage. Select Usage logs and dashboards too, or keep the S3 storage.";
  }
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
  /** The workbench status, for this tool's workbenches (null: none known). */
  workbench?: WorkbenchStatus | null;
  /** The MaaS status, for this tool's publications and platform (null: none known). */
  maas?: MaaSStatus | null;
  /** The model registry status, for this tool's registry (null: none known). */
  modelRegistry?: ModelRegistryStatus | null;
  /** The Distributed workloads status (null: none known). */
  kueue?: KueueStatus | null;
  /** The Observability status, for the usage logs this tool set up (null: none known). */
  observability?: ObservabilityStatus | null;
  minioLeftovers: boolean;
  /** Why changes are disabled now, or null. */
  baseReason: string | null;
  /** One-click prerequisites: the DataScienceCluster components this tool turned on (each feature's restorable). */
  restorable?: RestorableComponent[];
  /** A refused clean-up to run again: its choices, and what the user must confirm per recipe project. */
  retry?: CleanupRetry | null;
  onConfirm: (req: CleanupRequest) => void;
  onCancel: () => void;
}> = ({ scope, status, playground = null, workbench = null, maas = null, modelRegistry = null, kueue = null, observability = null, minioLeftovers, baseReason, restorable = [], retry = null, onConfirm, onCancel }) => {
  const titleId = React.useId();
  const reasonId = React.useId();
  const offered = cleanupCandidates(status, scope, minioLeftovers, playground, maas, modelRegistry, workbench, kueue, observability);
  // A refused clean-up comes back with the choices it was sent with.
  const was = retry?.req;
  const [workbenches, setWorkbenches] = useState<string[]>(was ? was.workbenches ?? [] : offered.workbenches);
  const chosenWorkbenches = workbenches.filter((p) => offered.workbenches.includes(p));
  const toggleWorkbench = (p: string, v: boolean) => setWorkbenches((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  // A workbench's storage holds its files: never ticked unless the user ticks it.
  const [workbenchStorage, setWorkbenchStorage] = useState<string[]>(was?.workbenchStorage ?? []);
  const chosenWorkbenchStorage = workbenchStorage.filter((p) => ((chosenWorkbenches.includes(p) && offered.workbenchStorage.includes(p)) || offered.keptStorage.includes(p)) && !offered.storageOf(p)?.usedBy?.length);
  const toggleWorkbenchStorage = (p: string, v: boolean) => setWorkbenchStorage((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const [published, setPublished] = useState<string[]>(was ? was.maasPublished ?? [] : offered.maasPublished);
  const chosenPublished = published.filter((p) => offered.maasPublished.includes(p));
  const togglePublished = (p: string, v: boolean) => setPublished((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const [maasPlatform, setMaaSPlatform] = useState(was ? !!was.maasPlatform : offered.maasPlatform);
  // Components are offered for the scope's feature (every feature's with "all"), unchecked: turning one back is a cluster change of its own.
  const offeredComponents = restorable.filter((r) => scope === "all" || r.feature === scope);
  const [components, setComponents] = useState<string[]>(was?.components ?? []);
  const chosenComponents = components.filter((k) => offeredComponents.some((r) => r.component === k));
  const toggleComponent = (k: string, v: boolean) => setComponents((s) => (v ? [...s.filter((x) => x !== k), k] : s.filter((x) => x !== k)));
  const [playgrounds, setPlaygrounds] = useState<string[]>(was ? was.playgrounds ?? [] : offered.playgrounds);
  const chosenPlaygrounds = playgrounds.filter((p) => offered.playgrounds.includes(p));
  const togglePlayground = (p: string, v: boolean) => setPlaygrounds((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  // A pipeline server this tool has no record of is unticked: ticking it is the user's confirmation that it is this tool's,
  // of that server (its UID). A refresh can make a ticked server unrecorded or replace a confirmed one: it is then unticked,
  // as only the user's own tick of the server now there confirms it.
  const unrecordedUID = new Map(toolServers(status).filter((ps) => ps.unrecorded && ps.namespace && offered.servers.includes(ps.namespace)).map((ps) => [ps.namespace!, ps.uid ?? ""]));
  const unrecordedServers = [...unrecordedUID.keys()];
  // A refused clean-up comes back with the servers the user confirmed (by UID): one replaced since is unticked.
  const [confirmed, setConfirmed] = useState<Record<string, string>>(was?.confirmUnrecorded ?? {});
  const [servers, setServers] = useState<string[]>(was ? was.pipelineServers ?? [] : offered.servers.filter((p) => !unrecordedServers.includes(p)));
  const serverTicked = (p: string) => servers.includes(p) && offered.servers.includes(p) && (!unrecordedUID.has(p) || (!!unrecordedUID.get(p) && confirmed[p] === unrecordedUID.get(p)));
  const [storage, setStorage] = useState(was ? !!was.storage : offered.storage);
  const [namespace, setNamespace] = useState(!!was?.deleteNamespace);
  const [mlflow, setMlflow] = useState(was ? !!was.mlflow : offered.mlflow);
  const [registry, setRegistry] = useState(was ? !!was.modelRegistry : offered.modelRegistry);
  const [kueueTicked, setKueueTicked] = useState(was ? !!was.kueue : offered.kueue);
  const [usageLogsTicked, setUsageLogsTicked] = useState(was ? !!was.observability : offered.observability);
  const [models, setModels] = useState<string[]>(was ? was.models ?? [] : offered.models);
  const [gateway, setGateway] = useState(was ? !!was.inferenceGateway : offered.gateway);
  const [recipeProjects, setRecipeProjects] = useState<string[]>(was ? was.recipeProjects ?? [] : offered.recipeProjects);
  const [projectStorages, setProjectStorages] = useState<string[]>(was ? was.projectStorages ?? [] : offered.projectStorages);
  const [projectStorageData, setProjectStorageData] = useState<string[]>(was?.projectStorageData ?? []);
  const chosenProjectStorages = projectStorages.filter((p) => offered.projectStorages.includes(p));
  const chosenProjectStorageData = projectStorageData.filter((p) => chosenProjectStorages.includes(p));
  const toggleIn = (set: React.Dispatch<React.SetStateAction<string[]>>) => (p: string, v: boolean) => set((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  // Each recipe project is deleted as the user reviewed it (its UID): pinned when the dialog opens (or kept from the
  // refused clean-up), and when the user ticks it. A refresh that shows one made again since (another UID) unticks it,
  // as for an unrecorded pipeline server: only the user's own tick of the project now there confirms it.
  const recipeProjectUID = new Map((status.recipeProjects ?? []).map((p) => [p.name, p.uid]));
  const [reviewedUIDs, setReviewedUIDs] = useState<Record<string, string>>(
    () => (was ? was.recipeProjectUIDs ?? {} : Object.fromEntries(offered.recipeProjects.map((p) => [p, recipeProjectUID.get(p) ?? ""]))));
  const recipeProjectTicked = (p: string) => recipeProjects.includes(p) && offered.recipeProjects.includes(p) && !!reviewedUIDs[p] && reviewedUIDs[p] === recipeProjectUID.get(p);
  const chosenRecipeProjects = offered.recipeProjects.filter(recipeProjectTicked);
  const toggleRecipeProject = (p: string, v: boolean) => {
    setRecipeProjects((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
    setReviewedUIDs(({ [p]: _dropped, ...rest }) => (v ? { ...rest, [p]: recipeProjectUID.get(p) ?? "" } : rest));
  };
  // The recipe projects to confirm (chosen ones the server refused), and those the user confirmed.
  const asking = chosenRecipeProjects.filter((p) => (retry?.contents[p] ?? []).length > 0);
  const [confirmedContents, setConfirmedContents] = useState<string[]>([]);
  const toggleConfirmedContents = (p: string, v: boolean) => setConfirmedContents((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const confirmProjectContents = Object.fromEntries(asking.filter((p) => confirmedContents.includes(p)).map((p) => [p, retry!.contents[p]]));
  const chosenModels = models.filter((m) => offered.models.includes(m));
  const chosenServers = servers.filter(serverTicked);
  const confirmedServers = Object.fromEntries(chosenServers.filter((s) => unrecordedUID.has(s)).map((s) => [s, unrecordedUID.get(s)!]));
  // The workbenches that lose each chosen server's dashboard Connection first: as a refused clean-up listed them, else as
  // the status does (the selected project's only); the request confirms exactly these.
  // An empty list from the refusal does not hide what the status shows now.
  const connectionUsers = (p: string): ConnectionWorkbench[] => {
    const refused = retry?.workbenches?.[p] ?? [];
    return refused.length > 0 ? refused : toolServers(status).find((ps) => ps.namespace === p)?.connectionWorkbenches ?? [];
  };
  const usersByServer = Object.fromEntries(chosenServers.map((p) => [p, connectionUsers(p)] as const).filter(([, w]) => w.length > 0));
  const confirmWorkbenches = Object.fromEntries(Object.entries(usersByServer).map(([p, w]) => [p, confirmationOf(w)]));
  // Workbenches that load the Connection without the annotation (as the status shows them): nothing changes them.
  const envOnlyByServer = Object.fromEntries(chosenServers.map((p) => [p, toolServers(status).find((ps) => ps.namespace === p)?.connectionEnvOnlyWorkbenches ?? []] as const).filter(([, w]) => w.length > 0));
  const affected = chosenServers.filter((p) => usersByServer[p] || envOnlyByServer[p]);
  const req: CleanupRequest = {
    ...(chosenPublished.length ? { maasPublished: chosenPublished } : {}),
    ...(chosenWorkbenches.length ? { workbenches: chosenWorkbenches } : {}),
    ...(chosenWorkbenchStorage.length ? { workbenchStorage: chosenWorkbenchStorage } : {}),
    ...(chosenServers.length ? { pipelineServers: chosenServers } : {}),
    ...(chosenProjectStorages.length ? { projectStorages: chosenProjectStorages } : {}),
    ...(chosenProjectStorageData.length ? { projectStorageData: chosenProjectStorageData } : {}),
    ...(Object.keys(confirmedServers).length ? { confirmUnrecorded: confirmedServers } : {}),
    ...(Object.keys(confirmWorkbenches).length ? { confirmWorkbenches } : {}),
    ...(storage && offered.storage ? { storage: true, ...(namespace && offered.namespace ? { deleteNamespace: true } : {}) } : {}),
    ...(mlflow && offered.mlflow ? { mlflow: true } : {}),
    ...(registry && offered.modelRegistry ? { modelRegistry: true } : {}),
    ...(kueueTicked && offered.kueue ? { kueue: true } : {}),
    ...(usageLogsTicked && offered.observability ? { observability: true } : {}),
    ...(chosenPlaygrounds.length ? { playgrounds: chosenPlaygrounds } : {}),
    ...(chosenModels.length ? { models: chosenModels } : {}),
    ...(gateway && offered.gateway ? { inferenceGateway: true } : {}),
    ...(maasPlatform && offered.maasPlatform ? { maasPlatform: true } : {}),
    ...(chosenRecipeProjects.length ? { recipeProjects: chosenRecipeProjects, recipeProjectUIDs: Object.fromEntries(chosenRecipeProjects.map((p) => [p, reviewedUIDs[p]])) } : {}),
    ...(Object.keys(confirmProjectContents).length ? { confirmProjectContents } : {}),
    ...(chosenComponents.length ? { components: chosenComponents } : {}),
  };
  const nothing = !req.workbenches && !req.workbenchStorage && !req.pipelineServers && !req.projectStorages && !req.storage && !req.mlflow && !req.modelRegistry && !req.kueue && !req.observability && !req.models && !req.inferenceGateway && !req.playgrounds && !req.maasPublished && !req.maasPlatform && !req.components && !req.recipeProjects;
  const unconfirmed = asking.filter((p) => !confirmedContents.includes(p));
  const reason = baseReason ?? (nothing ? "Select something to delete."
    : unconfirmed.length > 0 ? "Confirm that what the project holds is deleted with it." : cleanupBlocked(status, req, offered.maasPublished, !!observability?.usesS3Storage));
  const lost = [
    ...(req.workbenchStorage ?? []).map((p) => <>The files in storage <code>nightly-workbench-storage</code> in {p}.</>),
    ...offered.runningWorkbenches.filter((p) => chosenWorkbenches.includes(p)).map((p) => <>Unsaved work in the running workbench in {p}: only files saved under <code>/opt/app-root/src</code> are in its storage.</>),
    ...(req.pipelineServers ?? []).map((p) => <>The runs and experiments of the pipeline server in {p}.</>),
    ...(req.projectStorageData ?? []).map((p) => <>Every file in the S3 storage of {p} (volume <code>nightly-s3-data</code>).</>),
    ...(req.observability ? [<>Every usage log and metric: the usage-logs bucket{(observability?.volumes ?? []).some((v) => v.setUpByTool) ? <> and the disks of RHOAI&apos;s monitoring this tool&apos;s setup caused ({(observability?.volumes ?? []).filter((v) => v.setUpByTool).length})</> : null}.</>] : []),
    ...(req.storage ? [<>Every file in the S3 storage: pipeline artifacts and uploaded files{status.legacyStorage ? ", and the storage of an earlier version" : ""}.</>] : []),
    ...(req.deleteNamespace ? [<>Everything else in namespace <code>{S3_NAMESPACE}</code>.</>] : []),
    ...(req.mlflow ? [<>Every MLflow experiment, run and model, in every workspace.</>] : []),
    ...(req.modelRegistry ? [<>Everything in the model registry, which the whole cluster shares: every registered model, version and artifact record, including the models other people registered, and its database.</>] : []),
    ...(chosenComponents.includes("aiHub") ? [<>The Model Catalog, for every dashboard user.</>] : []),
    ...(req.playgrounds ?? []).map((p) => <>The chats and uploaded documents of the playground in {p}.</>),
    ...(req.maasPlatform ? [<>MaaS API keys, and the MaaS database with their records.</>] : []),
    ...(req.recipeProjects ?? []).map((p) => <>Everything in project {p}.</>),
  ];
  const toggleServer = (p: string, v: boolean) => {
    setServers((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
    setConfirmed(({ [p]: _dropped, ...rest }) => (v && unrecordedUID.has(p) ? { ...rest, [p]: unrecordedUID.get(p)! } : rest));
  };
  const toggleModel = (p: string, v: boolean) => setModels((s) => (v ? [...s.filter((x) => x !== p), p] : s.filter((x) => x !== p)));
  const nothingOffered = offered.projectStorages.length === 0 && offered.workbenches.length === 0 && offered.keptStorage.length === 0 && offered.servers.length === 0 && !offered.storage && !offered.mlflow && !offered.modelRegistry && !offered.kueue && !offered.observability && offered.models.length === 0 && !offered.gateway && offered.playgrounds.length === 0 && offered.maasPublished.length === 0 && !offered.maasPlatform && offeredComponents.length === 0 && offered.recipeProjects.length === 0;
  const title = scope === "all" ? "Clean up everything?" : scope === "pipelines" ? "Clean up Pipelines?" : scope === "models" ? "Clean up Models?" : scope === "playground" ? "Clean up Playground?" : scope === "model-registry" ? "Clean up the model registry?" : scope === "workbench" ? "Clean up Workbench?" : scope === "kueue" ? "Clean up distributed workloads?" : "Clean up MLflow?";
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={onCancel}>
      <ModalHeader title={title} labelId={titleId} titleIconVariant="warning" />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              {nothingOffered
                ? "This tool created nothing here that is left to delete."
                : scope === "pipelines"
                  ? "Deletes the pipeline servers you tick, as one operation. Something this tool did not create is never deleted."
                  : scope === "model-registry"
                    ? "Deletes the model registry this tool created, as one operation. Something this tool did not create is never deleted."
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
                {(offered.workbenches.length > 0 || offered.keptStorage.length > 0) && (
                  <FormGroup role="group" isStack label="Workbenches" fieldId="cleanup-workbenches">
                    {offered.workbenches.map((p) => {
                      const st = offered.storageOf(p);
                      return (
                        <Checkbox key={p} id={`cleanup-workbench-${p}`} label={<>Workbench in {p}</>} isChecked={workbenches.includes(p)} onChange={(_e, v) => toggleWorkbench(p, v)}
                          description={offered.runningWorkbenches.includes(p) ? "Running: unsaved work in its session is lost." : undefined}
                          body={workbenches.includes(p) && offered.workbenchStorage.includes(p) ? (
                            <Checkbox id={`cleanup-workbench-storage-${p}`} label="Also delete its storage (files are lost)" isChecked={workbenchStorage.includes(p) && !st?.usedBy?.length} isDisabled={!!st?.usedBy?.length} onChange={(_e, v) => toggleWorkbenchStorage(p, v)}
                              description={st?.usedBy?.length ? storageInUseText(st.usedBy) : <>PersistentVolumeClaim <code>nightly-workbench-storage</code>{st?.size ? `, ${st.size}` : ""}{st?.storageClass ? `, storage class ${st.storageClass}` : ""}</>} />
                          ) : undefined} />
                      );
                    })}
                    {offered.keptStorage.map((p) => {
                      const st = offered.storageOf(p);
                      return (
                        <Checkbox key={`kept-${p}`} id={`cleanup-workbench-kept-${p}`} label={<>Storage kept from the deleted workbench in {p} (files are lost)</>} isChecked={workbenchStorage.includes(p) && !st?.usedBy?.length} isDisabled={!!st?.usedBy?.length} onChange={(_e, v) => toggleWorkbenchStorage(p, v)}
                          description={st?.usedBy?.length ? storageInUseText(st.usedBy) : <>PersistentVolumeClaim <code>nightly-workbench-storage</code>{st?.size ? `, ${st.size}` : ""}{st?.storageClass ? `, storage class ${st.storageClass}` : ""}</>} />
                      );
                    })}
                  </FormGroup>
                )}
                {offered.servers.length > 0 && (
                  <FormGroup role="group" isStack label="Pipeline servers" fieldId="cleanup-servers">
                    {offered.servers.map((p) => (
                      <Checkbox key={p} id={`cleanup-server-${p}`} label={<>Pipeline server in {p}</>} isChecked={serverTicked(p)} onChange={(_e, v) => toggleServer(p, v)}
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
                {offered.modelRegistry && (
                  <FormGroup role="group" isStack label="Model registry" fieldId="cleanup-model-registry">
                    <Checkbox id="cleanup-model-registry" label={<code>{modelRegistry?.name}</code>} isChecked={registry} onChange={(_e, v) => setRegistry(v)}
                      description={`Its registered models and its database are deleted with it. The DataScienceCluster component that runs it stays on${offeredComponents.some((r) => r.feature === "model-registry") ? " unless you also tick it below" : ""}.`} />
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
                {offered.kueue && (
                  <FormGroup role="group" isStack label="Distributed workloads" fieldId="cleanup-kueue">
                    <Checkbox id="cleanup-kueue" label="Distributed workloads setup" isChecked={kueueTicked} onChange={(_e, v) => setKueueTicked(v)}
                      description={<>The samples, LocalQueue, ClusterQueue and ResourceFlavor{kueue?.label?.addedByTool ? `, and the project label on ${kueue.project}` : ""}, each only if this tool still owns it.{kueue?.optIn.on ? " The Dashboard Kueue setting goes back and hardware profile nightly-kueue-cpu is deleted, if nobody changed them." : ""} Jobs people sent to LocalQueue nightly-kueue themselves are never admitted once the queues are gone (those jobs are deleted only when their project is deleted). The operators stay.</>} />
                  </FormGroup>
                )}
                {offered.observability && (
                  <FormGroup role="group" isStack label="Observability" fieldId="cleanup-observability">
                    <Checkbox id="cleanup-observability" label="Usage logs and dashboards" isChecked={usageLogsTicked} onChange={(_e, v) => setUsageLogsTicked(v)}
                      description={<>Removes the monitoring fields this tool added to the DSCInitialization, then the disks and certificates the stack left, Secret <code>rhoai-nightly-updater-usage-logs</code>, the SeaweedFS user and the bucket this tool recorded. Runs before the S3 storage, which keeps their data. The operators stay.</>} />
                  </FormGroup>
                )}
                {offered.maasPlatform && (
                  <FormGroup role="group" isStack label="MaaS" fieldId="cleanup-maas-platform">
                    <Checkbox id="cleanup-maas-platform" label="MaaS platform" description="Last: MaaS turned back off as it was, then the gateway and database this tool created." isChecked={maasPlatform} onChange={(_e, v) => setMaaSPlatform(v)} />
                  </FormGroup>
                )}
                {offered.projectStorages.length > 0 && (
                  <FormGroup role="group" isStack label="Project S3 storage" fieldId="cleanup-project-storages">
                    {(status.projectStorages ?? []).map((ps) => (
                      <Checkbox key={ps.project} id={`cleanup-project-s3-${ps.project}`} label={<>S3 storage of {ps.project}</>} isChecked={projectStorages.includes(ps.project)} onChange={(_e, v) => toggleIn(setProjectStorages)(ps.project, v)}
                        description={<>Set up by {ps.by || "a project admin"}. Deleted after the pipeline servers; refused while a pipeline server still uses it.</>}
                        body={projectStorages.includes(ps.project) ? (
                          <Checkbox id={`cleanup-project-s3-data-${ps.project}`} label={<>Also delete its volume <code>{ps.dataVolume}</code> (its files are lost)</>} isChecked={projectStorageData.includes(ps.project)} onChange={(_e, v) => toggleIn(setProjectStorageData)(ps.project, v)} />
                        ) : undefined} />
                    ))}
                  </FormGroup>
                )}
                {offered.recipeProjects.length > 0 && (
                  <FormGroup role="group" isStack label="Projects recipes created" fieldId="cleanup-recipe-projects">
                    {(status.recipeProjects ?? []).map((p) => (
                      <Checkbox key={p.name} id={`cleanup-recipe-project-${p.name}`} label={<>Project {p.name}</>} isChecked={recipeProjectTicked(p.name)} onChange={(_e, v) => toggleRecipeProject(p.name, v)}
                        description={<>Created by recipe {p.recipe}. Deleted with everything in it, after the parts above, only while it is still the one the recipe created.</>} />
                    ))}
                  </FormGroup>
                )}
                {offeredComponents.length > 0 && (
                  <FormGroup role="group" isStack label="Cluster components this tool turned on" fieldId="cleanup-components">
                    {offeredComponents.map((r) => (
                      <Checkbox key={r.component} id={`cleanup-component-${r.component}`} label={componentLabel(r)}
                        description={r.component === "kueue" ? "Runs last. It also stays on if someone changed it since this tool turned it on." : "Runs last. It stays on while anything on the cluster still uses it, or if someone changed it since this tool turned it on. The operators it needed stay installed."} isChecked={chosenComponents.includes(r.component)} onChange={(_e, v) => toggleComponent(r.component, v)} />
                    ))}
                  </FormGroup>
                )}
              </Form>
            </StackItem>
          )}
          {scope === "all" && status.projectStoragesError && (
            <StackItem>
              <Alert variant="warning" isInline component="p" title="Project S3 storages are not offered">{status.projectStoragesError}</Alert>
            </StackItem>
          )}
          {scope === "all" && status.recipeProjectsError && (
            <StackItem>
              <Alert variant="warning" isInline component="p" title="Projects recipes created are not offered">{status.recipeProjectsError}</Alert>
            </StackItem>
          )}
          {asking.map((p) => (
            <StackItem key={`ask-${p}`}>
              <Alert variant="warning" isInline title={`Project ${p} holds what this tool did not create`}>
                <Stack hasGutter>
                  <StackItem>Nothing was deleted yet. Deleting the project deletes these too:</StackItem>
                  <StackItem>
                    <List>{retry!.contents[p].map((c) => <ListItem key={c}>{c}</ListItem>)}</List>
                  </StackItem>
                  <StackItem>Only Notebooks (workbenches), InferenceServices and LLMInferenceServices (models), PersistentVolumeClaims (volumes) and Pods are listed; anything else in the project is deleted with it.</StackItem>
                  <StackItem>
                    <Checkbox id={`cleanup-confirm-${p}`} label={`Delete ${retry!.contents[p].length === 1 ? "it" : "them"} with project ${p}`} isChecked={confirmedContents.includes(p)} onChange={(_e, v) => toggleConfirmedContents(p, v)} />
                  </StackItem>
                </Stack>
              </Alert>
            </StackItem>
          ))}
          {affected.map((p) => (
            <StackItem key={`workbenches-${p}`}>
              <Alert variant="warning" isInline component="div" title={`Workbenches in ${p} use the dashboard Connection`}>
                <Stack hasGutter>
                  {usersByServer[p] && <StackItem>{connectionWorkbenchesChange(p, usersByServer[p])}</StackItem>}
                  {envOnlyByServer[p] && <StackItem>{connectionEnvOnlyChange(p, envOnlyByServer[p])}</StackItem>}
                </Stack>
              </Alert>
            </StackItem>
          ))}
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
