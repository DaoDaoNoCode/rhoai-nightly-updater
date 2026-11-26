import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { MLflowSampleId, MLflowSamplesStatus, PlaygroundStatus, ProjectProblemCode, ProjectStatus, ProjectStorageStatus, ResourceState, WorkbenchStatus } from "../../types";
import { getMLflowSamples, getPlayground, getProjectStatus, getProjectStorage, getWorkbench, toApiError, type ApiError } from "../../services/api";
import { Checkbox, TextInput } from "@patternfly/react-core";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { keptStorage, workbenchSettling, workbenchStatusOf } from "./workbenchState";
import { ProjectActionButton, ProjectCapabilitiesProvider, ProjectRunPanel, useProjectCapability, useProjectRun, type ProjectRunRequest } from "./projectActions";
import { FORBIDDEN_FOR_USER_BODY, FORBIDDEN_FOR_USER_TITLE, describeError, isNotForUser } from "../../errors";
import { EnvStatusLabel, envStatus } from "./resourceStatus";
import { IDLE, modelsWithoutPortText, playgroundIn } from "./featureState";
import { playgroundSettling } from "./usePlaygroundStatus";
import { isTerminating, terminalReason } from "./resourceRow";
import { useResourcePolling } from "./useResourceStatus";
import { useLatestRequest } from "../../hooks/useLatestRequest";
import { PipelineSample } from "./PipelineSample";
import { SAMPLE_INFO, sampleLabel, sampleNote } from "./samples";
import { LinesSkeleton } from "./Loading";
import { DeployProjectModel } from "./DeployProjectModel";

/** What a user does when the project's only chat models cannot be used by the playground. */
const ASK_FOR_GATEWAY_MODEL = "Ask a cluster-admin to expose a model through the inference gateway.";

/** What a cluster-admin does on this page for a user who is not one: set up or remove anything. */
export const ADMIN_SETS_UP = "A cluster-admin sets this up.";
// What a project's own admin can add themselves (a pipeline server, a model).
export const IN_PROJECT_HINT = "Add one in the RHOAI dashboard, or ask a cluster-admin to set one up here.";

/** A choice a delete's confirmation offers (a checkbox), and the request for each answer. */
interface ConfirmChoice {
  label: string;
  run: (checked: boolean) => ProjectRunRequest;
  /** What the checked choice adds to the changes. */
  changes: string[];
}

/** A choice a delete's confirmation offers by typing a name (data loss), and the request for each answer. */
interface TypedChoice {
  label: string;
  /** What must be typed for the choice to hold. */
  expect: string;
  run: (typed: boolean) => ProjectRunRequest;
  changes: string[];
}

/** A project action the user asked for and confirms first (deletes). */
interface PendingAction {
  title: string;
  changes: string[];
  confirm: string;
  run: ProjectRunRequest;
  choice?: ConfirmChoice;
  typed?: TypedChoice;
}

/** Starts a project action (after confirming a delete). */
type StartAction = (title: string, run: ProjectRunRequest, confirm?: { changes: string[]; label: string; choice?: ConfirmChoice; typed?: TypedChoice }) => void;

interface ProjectRead<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  /** Read again now, and check quickly again if polling gave up. */
  reload: () => void;
  /** A part still settling after 10 minutes: polling went back to its slow pace. */
  gaveUp: boolean;
}

/**
 * One read for the chosen project, again when refreshKey changes, and polled
 * as the admin's Environment status is (useResourcePolling): quickly while
 * busy (a project action runs) or while settling says a part of the answer
 * is still changing on its own (setting up, starting, deleting), else a slow
 * idle re-read, so a part another user changed shows up too. Only the newest
 * read's answer is shown; an answer for a project no longer chosen is
 * dropped; the error keeps its errorCode (forbidden_for_user is "you cannot
 * see this", not a failure).
 */
function useProjectRead<T>(load: (project: string, signal?: AbortSignal) => Promise<T>, project: string, refreshKey: number, settling: (data: T) => boolean, busy: boolean): ProjectRead<T> {
  const [state, setState] = useState<{ project: string; data: T | null; error: ApiError | null }>({ project: "", data: null, error: null });
  const latest = useLatestRequest();
  const fetchNow = useCallback(async () => {
    const ticket = latest.start();
    try {
      const data = await load(project, ticket.signal);
      if (ticket.isLatest()) setState({ project, data, error: null });
    } catch (e) {
      if (ticket.isLatest()) setState((s) => ({ project, data: s.project === project ? s.data : null, error: toApiError(e) }));
    }
  }, [load, project, latest]);
  useEffect(() => { void fetchNow(); }, [fetchNow, refreshKey]);
  const mine = state.project === project;
  const polling = useResourcePolling(fetchNow, busy || (mine && state.data !== null && settling(state.data)));
  return {
    data: mine ? state.data : null,
    error: mine ? state.error : null,
    loading: !mine,
    reload: () => { polling.restart(); void fetchNow(); },
    gaveUp: polling.gaveUp,
  };
}

/** A part's read that failed: "you cannot see this" when it is not the user's to see, else the error with Try again. */
const ReadProblem: React.FC<{ what: string; error: ApiError; onRetry: () => void }> = ({ what, error, onRetry }) => {
  if (isNotForUser(error)) {
    const { title, body } = describeError(error);
    return <Alert variant="info" isInline isPlain component="div" title={title}><Content component="p">{body}</Content></Alert>;
  }
  return (
    <Alert variant="warning" isInline component="div" title={`Cannot read ${what}`}
      actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>
      <Content component="p">{error.message}</Content>
    </Alert>
  );
};

/**
 * A part the backend answered but could not read (200 with a problem):
 * the user may not see it ("You cannot see this"), it is not installed on
 * the cluster, or the read failed (the message, with Try again).
 */
const PartProblem: React.FC<{ what: string; code?: ProjectProblemCode | string; message: string; onRetry: () => void }> = ({ what, code, message, onRetry }) => {
  if (code === "forbidden_for_user" || code === "forbidden") {
    return <Alert variant="info" isInline isPlain component="div" title={FORBIDDEN_FOR_USER_TITLE}><Content component="p">{FORBIDDEN_FOR_USER_BODY}</Content></Alert>;
  }
  if (code === "not_installed") return <Note>{message} {ADMIN_SETS_UP}</Note>;
  return (
    <Alert variant="warning" isInline component="div" title={`Cannot read ${what}`}
      actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>
      <Content component="p">{message}</Content>
    </Alert>
  );
};

/** The project's pipeline server: the tool's first, else another one. */
function projectServer(status: ProjectStatus | null, project: string): ResourceState | undefined {
  const servers = (status?.pipelineServers ?? []).filter((ps) => !ps.namespace || ps.namespace === project);
  return servers.find((ps) => ps.managedByTool) ?? servers[0];
}

const Part: React.FC<{ title: string; id: string; children: React.ReactNode }> = ({ title, id, children }) => (
  <Card component="section" aria-labelledby={id}>
    <CardHeader><CardTitle><Title headingLevel="h2" size="lg" id={id}>{title}</Title></CardTitle></CardHeader>
    <CardBody>{children}</CardBody>
  </Card>
);

const Note: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <Content component="p" className="pf-v6-u-text-color-subtle">{children}</Content>
);

const loadStatus = (project: string, signal?: AbortSignal) => getProjectStatus(project, signal);
const loadStorage = (project: string, signal?: AbortSignal) => getProjectStorage(project, signal);
const loadPlayground = (project: string, signal?: AbortSignal) => getPlayground(project, signal);
const loadSamples = (project: string, signal?: AbortSignal) => getMLflowSamples(project, signal);

/** A pipeline server or model that is starting or being deleted (the admin's resourcesSettling). */
const statusSettling = (st: ProjectStatus) => [...(st.pipelineServers ?? []), ...(st.models ?? [])]
  .some((r) => isTerminating(r) || (r.deployed && !r.ready && !terminalReason(r)));
/** Storage that is starting or being deleted; one that needs a repair waits for the user. */
const storageSettling = (st: ProjectStorageStatus) => st.terminating || (st.deployed && !st.ready && !(st.missing?.length) && !st.networkPolicyProblem);
const neverSettling = () => false;

const PipelinesPart: React.FC<{ project: string; read: ProjectRead<ProjectStatus>; storage: ProjectStorageStatus | null; busy: boolean; start: StartAction }> = ({ project, read, storage, busy, start }) => {
  const server = projectServer(read.data, project);
  // Who sets up the project's S3 storage, which the pipeline server needs: the user (an admin of the project), or an admin of the project they ask.
  const storageSetup = useProjectCapability(project, "project-s3.setup");
  const noServer = storage?.deployed
    ? `No pipeline server in ${project}. Once the project's S3 storage is ready, its pipeline server can be set up here. ${IN_PROJECT_HINT}`
    : storage && storageSetup.allowed
      ? `No pipeline server in ${project}. Set up the project's S3 storage first; then its pipeline server can be set up here. ${IN_PROJECT_HINT}`
      : storage && storageSetup.code === "missing_permissions"
        ? `No pipeline server in ${project}. It can be set up here once the project's S3 storage is ready, which an admin of project ${project} sets up: ask one of them. ${IN_PROJECT_HINT}`
        : `No pipeline server in ${project}. Once the project's S3 storage is ready, its pipeline server can be set up here. ${IN_PROJECT_HINT}`;
  const ppRun = (action: "setup" | "teardown", confirmWorkbenches?: Record<string, string>): ProjectRunRequest =>
    ({ kind: "project-pipeline-server", req: { action, project, ...(confirmWorkbenches ? { confirmWorkbenches } : {}) } });
  let body: React.ReactNode;
  if (read.error && !read.data) body = <ReadProblem what="the pipeline server" error={read.error} onRetry={read.reload} />;
  else if (!read.data) body = <LinesSkeleton what="the pipeline server" lines={1} />;
  else if (read.data.pipelineServersProblem) body = <PartProblem what="the pipeline server" code={read.data.pipelineServersProblemCode} message={read.data.pipelineServersProblem} onRetry={read.reload} />;
  else if (!server || (!server.deployed && !server.terminating)) {
    body = storage?.ready && storage.managedByTool ? (
      <Stack hasGutter>
        <StackItem><Note>No pipeline server in {project}. Set up makes one on the project&apos;s own S3 storage, with the dashboard connection nightly-s3-connection.</Note></StackItem>
        <StackItem>
          <ProjectActionButton project={project} action="project-pipeline-server.setup" busy={busy} variant="primary"
            onClick={() => start(`Set up the pipeline server in ${project}`, ppRun("setup"))}>
            Set up pipeline server
          </ProjectActionButton>
        </StackItem>
      </Stack>
    ) : <Note>{noServer}</Note>;
  } else {
    const wbs = server.connectionWorkbenches ?? [];
    const deletable = server.managedByTool && server.projectStorage && !server.terminating;
    body = (
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><strong>{server.name || "Pipeline server"}</strong></FlexItem>
            <FlexItem><EnvStatusLabel status={envStatus(server)} /></FlexItem>
            {server.ready && server.uiRoute && (
              <FlexItem>
                <Button variant="link" isInline component="a" href={server.uiRoute} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                  Open pipelines in {project}
                </Button>
              </FlexItem>
            )}
          </Flex>
        </StackItem>
        {server.projectStorage && <StackItem><Note>On the project&apos;s own S3 storage, with the dashboard connection {server.connection || "nightly-s3-connection"}.</Note></StackItem>}
        {!server.ready && server.message && <StackItem><Note>{server.message}</Note></StackItem>}
        {server.ready && <StackItem><Note>Run a sample pipeline below to check that runs work.</Note></StackItem>}
        {deletable && (
          <StackItem>
            <ProjectActionButton project={project} action="project-pipeline-server.teardown" busy={busy} variant="danger"
              onClick={() => start(`Delete the pipeline server in ${project}`, ppRun("teardown", Object.fromEntries(wbs.map((w) => [w.name, w.uid]))), {
                changes: [
                  `Pipeline server ${server.name} in ${project}, with its database (pipeline runs and experiments)`,
                  `Its secret nightly-dspa-s3 and dashboard connection ${server.connection || "nightly-s3-connection"}`,
                  ...wbs.map((w) => `The connection comes off workbench ${w.name}${w.running ? ", which restarts" : ""}`),
                  "The project's S3 storage and its data stay",
                ], label: "Delete",
              })}>
              Delete pipeline server
            </ProjectActionButton>
          </StackItem>
        )}
      </Stack>
    );
  }
  return <Part title="Pipelines" id="user-env-pipelines">{body}</Part>;
};

/** The ready state of the project's S3 storage, for its label. */
function storageLabel(st: ProjectStorageStatus): { status: "ready" | "deleting" | "setting-up" | "needs-attention"; text: string } {
  if (st.terminating) return { status: "deleting", text: "Deleting" };
  if (st.ready && !st.networkPolicyProblem && !(st.missing?.length)) return { status: "ready", text: "Ready" };
  if (st.ready || st.missing?.length || st.networkPolicyProblem) return { status: "needs-attention", text: "Needs attention" };
  return { status: "setting-up", text: "Starting" };
}

const StoragePart: React.FC<{ project: string; read: ProjectRead<ProjectStorageStatus>; busy: boolean; start: StartAction }> = ({ project, read, busy, start }) => {
  const st = read.data;
  const s3Run = (action: "setup" | "repair" | "delete", deleteData?: string): ProjectRunRequest =>
    ({ kind: "project-s3", req: { action, project, ...(deleteData ? { deleteData } : {}) } });
  let body: React.ReactNode;
  if (read.error && !st) body = <ReadProblem what="the S3 storage" error={read.error} onRetry={read.reload} />;
  else if (!st) body = <LinesSkeleton what="the S3 storage" lines={1} />;
  else if (st.problem) body = <PartProblem what="the S3 storage" code={st.problemCode} message={st.problem} onRetry={read.reload} />;
  else if (!st.deployed) {
    body = (
      <Stack hasGutter>
        <StackItem><Note>No S3 storage of this tool in {project}. Set up runs SeaweedFS in {project} for its pipeline server: a 20Gi volume, one key for the project (readable by its admins and editors), and a network policy that lets in only the S3 port from {project} and RHOAI. It has no route outside the cluster.</Note></StackItem>
        <StackItem>
          <ProjectActionButton project={project} action="project-s3.setup" busy={busy} variant="primary"
            onClick={() => start(`Set up the S3 storage in ${project}`, s3Run("setup"))}>
            Set up S3 storage
          </ProjectActionButton>
        </StackItem>
      </Stack>
    );
  } else {
    const label = storageLabel(st);
    const inUse = (st.usedBy ?? []).length > 0;
    body = (
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><strong>{st.name}</strong></FlexItem>
            <FlexItem><EnvStatusLabel status={label.status} text={label.text} /></FlexItem>
          </Flex>
        </StackItem>
        {!st.managedByTool && <StackItem><Note>Some of these objects were not created by this tool, so it does not change them.</Note></StackItem>}
        <StackItem><Note>Address in the cluster: <code>{st.endpoint}</code>, bucket <code>{st.bucket}</code>. This tool&apos;s network policy lets in only pods of {project} (such as its workbench and pipeline server) and of RHOAI&apos;s applications namespace (the pipelines operator checks the storage from there); it has no address outside the cluster.</Note></StackItem>
        {!st.ready && st.message && <StackItem><Note>{st.message}</Note></StackItem>}
        {st.networkPolicyProblem && <StackItem><Alert variant="warning" isInline isPlain component="div" title="Network policy changed"><Content component="p">{st.networkPolicyProblem}</Content></Alert></StackItem>}
        {(st.missing?.length ?? 0) > 0 && <StackItem><Alert variant="warning" isInline isPlain component="div" title="Objects missing"><Content component="p">Missing: {st.missing?.join(", ")}. Repair makes them again.</Content></Alert></StackItem>}
        {st.managedByTool && !st.terminating && (
          <StackItem>
            <Flex gap={{ default: "gapSm" }}>
              <FlexItem>
                <ProjectActionButton project={project} action="project-s3.repair" busy={busy}
                  onClick={() => start(`Repair the S3 storage in ${project}`, s3Run("repair"))}>
                  Repair
                </ProjectActionButton>
              </FlexItem>
              <FlexItem>
                {inUse ? (
                  <Note>Pipeline server {st.usedBy?.join(", ")} uses it: delete that first to delete the storage.</Note>
                ) : (
                  <ProjectActionButton project={project} action="project-s3.delete" busy={busy} variant="danger"
                    onClick={() => start(`Delete the S3 storage in ${project}`, s3Run("delete"), {
                      changes: [`SeaweedFS ${st.name} in ${project}: its Deployment, Service, key Secret and network policy`],
                      label: "Delete",
                      typed: { label: `The volume ${st.dataVolume} and its data are kept. To delete them too, type its name`, expect: st.dataVolume, run: (typed) => s3Run("delete", typed ? st.dataVolume : undefined), changes: [`The volume ${st.dataVolume} and all data on it`] },
                    })}>
                    Delete
                  </ProjectActionButton>
                )}
              </FlexItem>
            </Flex>
          </StackItem>
        )}
      </Stack>
    );
  }
  return <Part title="S3 storage" id="user-env-s3">{body}</Part>;
};

const ModelsPart: React.FC<{ project: string; read: ProjectRead<ProjectStatus>; busy: boolean; start: StartAction; onDeploy: () => void }> = ({ project, read, busy, start, onDeploy }) => {
  const data = read.data;
  const models = data?.models ?? [];
  const inferenceServices = data?.inferenceServices ?? [];
  let body: React.ReactNode;
  if (read.error && !data) body = <ReadProblem what="the models" error={read.error} onRetry={read.reload} />;
  else if (!data) body = <LinesSkeleton what="the models" lines={2} />;
  else {
    const llm = data.modelsProblem
      ? <PartProblem what="the models" code={data.modelsProblemCode} message={data.modelsProblem} onRetry={read.reload} />
      : models.length > 0 ? (
        <Table aria-label={`Models in ${project}`} variant="compact">
          <Thead>
            <Tr>
              <Th width={30}>Model</Th>
              <Th width={20}>Status</Th>
              <Th width={40}>Address</Th>
              <Th screenReaderText="Actions" />
            </Tr>
          </Thead>
          <Tbody>
            {models.map((m) => (
              <Tr key={m.name ?? m.displayName}>
                <Td dataLabel="Model">{m.displayName || m.name}</Td>
                <Td dataLabel="Status"><EnvStatusLabel status={envStatus(m)} /></Td>
                <Td dataLabel="Address">
                  {m.url ? <code className="pf-v6-u-text-break-word">{m.url}</code>
                    : m.internalUrl ? <><code className="pf-v6-u-text-break-word">{m.internalUrl}</code> <span className="pf-v6-u-text-color-subtle">(inside the cluster)</span></>
                      : <span className="pf-v6-u-text-color-subtle">None</span>}
                </Td>
                <Td dataLabel="Actions" modifier="nowrap">
                  {m.managedByTool && !m.terminating && m.name && (
                    <ProjectActionButton project={project} action="models.teardown" busy={busy} variant="link"
                      onClick={() => start(`Delete ${m.displayName || m.name} in ${project}`, { kind: "models", req: { action: "teardown", project, model: m.name } },
                        { changes: [`Model ${m.name} in ${project}, with its token objects if it has them`], label: "Delete" })}>
                      Delete
                    </ProjectActionButton>
                  )}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      ) : null;
    const isvc = data.inferenceServicesProblem
      ? <PartProblem what="the InferenceServices" code={data.inferenceServicesProblemCode} message={data.inferenceServicesProblem} onRetry={read.reload} />
      : inferenceServices.length > 0 ? (
        <Table aria-label={`InferenceServices in ${project}`} variant="compact">
          <Thead>
            <Tr>
              <Th width={30}>InferenceService</Th>
              <Th width={20}>Status</Th>
              <Th width={50}>Address</Th>
            </Tr>
          </Thead>
          <Tbody>
            {inferenceServices.map((s) => (
              <Tr key={s.name}>
                <Td dataLabel="InferenceService">{s.name}</Td>
                <Td dataLabel="Status">
                  <EnvStatusLabel status={s.terminating ? "deleting" : s.ready ? "ready" : "needs-attention"} text={s.terminating ? "Deleting" : s.ready ? "Ready" : "Not ready"} />
                  {!s.ready && s.message && <div className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm">{s.message}</div>}
                </Td>
                <Td dataLabel="Address">{s.url ? <code className="pf-v6-u-text-break-word">{s.url}</code> : <span className="pf-v6-u-text-color-subtle">None</span>}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      ) : null;
    const deploy = (data.modelCatalog ?? []).length > 0 && !data.modelsProblem && (
      <ProjectActionButton project={project} action="models.setup" busy={busy} variant="secondary" onClick={onDeploy}>
        Deploy a model
      </ProjectActionButton>
    );
    body = (
      <Stack hasGutter>
        {!llm && !isvc && <StackItem><Note>No models in {project}. Deploy a small CPU model of this tool&apos;s catalog here, or add one in the RHOAI dashboard.</Note></StackItem>}
        {llm && <StackItem>{llm}</StackItem>}
        {isvc && <StackItem>{isvc}</StackItem>}
        {deploy && <StackItem>{deploy}</StackItem>}
      </Stack>
    );
  }
  return <Part title="Models" id="user-env-models">{body}</Part>;
};

const PlaygroundPart: React.FC<{ project: string; read: ProjectRead<PlaygroundStatus>; busy: boolean; start: StartAction }> = ({ project, read, busy, start }) => {
  const item = read.data ? playgroundIn(read.data, project) : undefined;
  // The project's own ready models, chat and embedding, as the admin's Set up chooses them; those the playground cannot use are left out.
  const ready = (read.data?.models ?? []).filter((m) => m.source === "namespace" && m.ready);
  const models = ready.filter((m) => !m.unusable);
  const hasChat = models.some((m) => m.chat);
  const withoutPort = ready.filter((m) => m.chat && m.unusable);
  let body: React.ReactNode;
  if (read.error && !read.data) body = <ReadProblem what="the playground" error={read.error} onRetry={read.reload} />;
  else if (!read.data) body = <LinesSkeleton what="the playground" lines={1} />;
  else if (!item && read.data.apiProblem) body = <PartProblem what="the playground" code={read.data.apiCode} message={read.data.apiProblem} onRetry={read.reload} />;
  else if (!item && read.data.blocked) body = <Note>{read.data.blocked.reason}</Note>;
  else if (!item || (!item.deployed && !item.terminating)) {
    body = (
      <Stack hasGutter>
        <StackItem>
          <Note>{hasChat
            ? `No playground in ${project}. Set up asks the RHOAI dashboard for one with the project's ready models.${withoutPort.length > 0 ? ` Left out: ${modelsWithoutPortText(withoutPort)}` : ""}`
            : withoutPort.length > 0
              ? `No playground in ${project}. ${modelsWithoutPortText(withoutPort)} ${ASK_FOR_GATEWAY_MODEL}`
              : `No playground in ${project}. It needs a ready chat model in the project; add one in the RHOAI dashboard.`}</Note>
        </StackItem>
        {(hasChat || withoutPort.length > 0) && (
          <StackItem>
            <ProjectActionButton project={project} action="playground.setup" busy={busy} variant="primary"
              blockedReason={hasChat ? null : `None of the project's chat models can be used by the playground. ${ASK_FOR_GATEWAY_MODEL}`}
              onClick={() => start(`Set up the playground in ${project}`, { kind: "playground", req: { action: "setup", project, models: models.map((m) => ({ name: m.name, source: m.source })) } })}>
              Set up playground
            </ProjectActionButton>
          </StackItem>
        )}
      </Stack>
    );
  } else {
    body = (
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><strong>Gen AI playground</strong></FlexItem>
            <FlexItem><EnvStatusLabel status={envStatus(item)} /></FlexItem>
            {item.managedByTool && !item.terminating && (
              <FlexItem>
                <ProjectActionButton project={project} action="playground.teardown" busy={busy} variant="danger"
                  onClick={() => start(`Delete the playground in ${project}`, { kind: "playground", req: { action: "teardown", project } },
                    { changes: [`The playground server, its configuration and its database in ${project}, through the RHOAI dashboard`], label: "Delete" })}>
                  Delete
                </ProjectActionButton>
              </FlexItem>
            )}
          </Flex>
        </StackItem>
        {!item.ready && item.message && <StackItem><Note>{item.message}</Note></StackItem>}
        {item.ready && <StackItem><Note>Open the playground from {project} in the RHOAI dashboard&apos;s Gen AI studio.</Note></StackItem>}
      </Stack>
    );
  }
  return <Part title="Playground" id="user-env-playground">{body}</Part>;
};

const SamplesPart: React.FC<{ project: string; read: ProjectRead<MLflowSamplesStatus>; busy: boolean; start: StartAction }> = ({ project, read, busy, start }) => {
  const sampleRun = (action: "add" | "remove", id: MLflowSampleId): ProjectRunRequest => ({ kind: "mlflow-samples", req: { project, action, samples: [id] } });
  const data = read.data;
  let body: React.ReactNode;
  if (read.error && !data) body = <ReadProblem what="the MLflow sample data" error={read.error} onRetry={read.reload} />;
  else if (!data) body = <LinesSkeleton what="the MLflow sample data" lines={2} />;
  else if (!data.available) body = <Note>{data.reason || "MLflow is not available."} {ADMIN_SETS_UP}</Note>;
  else {
    body = (
      <Stack hasGutter>
        <StackItem><Note>Example data in the MLflow workspace of {project}. Add or remove it here when your role in the project allows it.</Note></StackItem>
        <StackItem>
          <Table aria-label={`MLflow sample data in ${project}`} variant="compact">
            <Thead>
              <Tr>
                <Th width={50}>Sample</Th>
                <Th width={25}>State</Th>
                <Th screenReaderText="Actions and link" />
              </Tr>
            </Thead>
            <Tbody>
              {data.samples.map((s) => {
                const label = sampleLabel(s, null);
                const note = sampleNote(s);
                return (
                  <Tr key={s.id}>
                    <Td dataLabel="Sample">
                      {SAMPLE_INFO[s.id]?.name ?? s.name}
                      {note && <Content component="small" className="pf-v6-u-display-block pf-v6-u-mt-xs">{note}</Content>}
                    </Td>
                    <Td dataLabel="State">{label ? <EnvStatusLabel status={label.status} text={label.text} /> : null}</Td>
                    <Td dataLabel="Link" modifier="nowrap">
                      <Flex gap={{ default: "gapMd" }} flexWrap={{ default: "nowrap" }}>
                      {s.actions.includes("add") && (
                        <ProjectActionButton project={project} action="mlflow-samples.add" busy={busy} variant="link"
                          onClick={() => start(`Add ${SAMPLE_INFO[s.id]?.name ?? s.name} in ${project}`, sampleRun("add", s.id))}>
                          Add
                        </ProjectActionButton>
                      )}
                      {s.actions.includes("remove") && (
                        <ProjectActionButton project={project} action="mlflow-samples.remove" busy={busy} variant="link"
                          onClick={() => start(`Remove ${SAMPLE_INFO[s.id]?.name ?? s.name} from ${project}`, sampleRun("remove", s.id),
                            { changes: [`${SAMPLE_INFO[s.id]?.name ?? s.name} in the MLflow workspace of ${project}`], label: "Remove" })}>
                          Remove
                        </ProjectActionButton>
                      )}
                      {s.link && (
                        <Button variant="link" isInline component="a" href={s.link} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                          Open in MLflow
                        </Button>
                      )}
                      </Flex>
                    </Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        </StackItem>
      </Stack>
    );
  }
  return <Part title="MLflow sample data" id="user-env-mlflow">{body}</Part>;
};

const loadWorkbench = (project: string, signal?: AbortSignal) => getWorkbench(project, signal);

const WorkbenchPart: React.FC<{ project: string; read: ProjectRead<WorkbenchStatus>; busy: boolean; start: StartAction }> = ({ project, read, busy, start }) => {
  const data = read.data;
  const item = data?.items?.find((i) => !i.namespace || i.namespace === project);
  const kept = keptStorage(data, project);
  const exists = !!item?.deployed || !!item?.terminating;
  const wbRun = (action: "setup" | "start" | "stop" | "teardown" | "delete-storage", deleteStorage = false): ProjectRunRequest => ({ kind: "workbench", req: { action, project, ...(deleteStorage ? { deleteStorage } : {}) } });
  const storageName = item?.storage?.name ?? "nightly-workbench-storage";
  let body: React.ReactNode;
  if (read.error && !data) body = <ReadProblem what="the workbench" error={read.error} onRetry={read.reload} />;
  else if (!data) body = <LinesSkeleton what="the workbench" lines={1} />;
  else if (data.blocked) body = <Note>{data.blocked.reason}</Note>;
  else if (data.unsupported) body = <Note>{data.unsupported}</Note>;
  else if (!item || !exists) {
    const setupText = data.plan
      ? `No workbench of this tool in ${project}. Set up creates nightly-workbench with ${data.plan.image} on hardware profile ${data.plan.hardwareProfile} and ${kept ? `the kept storage ${kept.name} with its files` : `${data.plan.storageSize} of storage`}.`
      : `No workbench of this tool in ${project}. ${data.planProblem ?? ""}`;
    body = (
      <Stack hasGutter>
        <StackItem><Note>{setupText}</Note></StackItem>
        {(data.plan || kept) && (
          <StackItem>
            <Flex gap={{ default: "gapSm" }}>
              {data.plan && (
                <FlexItem>
                  <ProjectActionButton project={project} action="workbench.setup" busy={busy} variant="primary" onClick={() => start(`Set up the workbench in ${project}`, wbRun("setup"))}>
                    Set up workbench
                  </ProjectActionButton>
                </FlexItem>
              )}
              {kept && (
                <FlexItem>
                  <ProjectActionButton project={project} action="workbench.delete-storage" busy={busy} variant="danger"
                    onClick={() => start(`Delete the kept storage in ${project}`, wbRun("delete-storage"),
                      { changes: [`Storage ${kept.name} in ${project} and the files on it`], label: "Delete storage" })}>
                    Delete storage
                  </ProjectActionButton>
                </FlexItem>
              )}
            </Flex>
          </StackItem>
        )}
      </Stack>
    );
  } else {
    const ours = item.managedByTool;
    const shown = workbenchStatusOf(data, project, IDLE);
    body = (
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><strong>{item.name}</strong></FlexItem>
            <FlexItem><EnvStatusLabel status={shown.status} text={shown.word} /></FlexItem>
            {item.url && item.state === "running" && (
              <FlexItem>
                <Button variant="link" isInline component="a" href={item.url} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">Open workbench</Button>
              </FlexItem>
            )}
          </Flex>
        </StackItem>
        {!ours && <StackItem><Note>This workbench was not created by this tool, so it is not changed here. Use it in the RHOAI dashboard.</Note></StackItem>}
        {ours && !item.terminating && (
          <StackItem>
            <Flex gap={{ default: "gapSm" }}>
              {item.state === "stopped" ? (
                <FlexItem><ProjectActionButton project={project} action="workbench.start" busy={busy} onClick={() => start(`Start the workbench in ${project}`, wbRun("start"))}>Start</ProjectActionButton></FlexItem>
              ) : (
                <FlexItem><ProjectActionButton project={project} action="workbench.stop" busy={busy} onClick={() => start(`Stop the workbench in ${project}`, wbRun("stop"))}>Stop</ProjectActionButton></FlexItem>
              )}
              <FlexItem>
                <ProjectActionButton project={project} action="workbench.teardown" busy={busy} variant="danger"
                  onClick={() => start(`Delete the workbench in ${project}`, wbRun("teardown"), {
                    changes: [`Workbench ${item.name} in ${project}; its storage ${storageName} is kept for the next Set up`], label: "Delete",
                    choice: { label: `Also delete its storage ${storageName} and the files on it`, run: (checked) => wbRun("teardown", checked), changes: [`Storage ${storageName} and the files on it`] },
                  })}>
                  Delete
                </ProjectActionButton>
              </FlexItem>
            </Flex>
          </StackItem>
        )}
      </Stack>
    );
  }
  return <Part title="Workbench" id="user-env-workbench">{body}</Part>;
};

/**
 * What runs in one of the user's projects, read with their own token
 * (docs/research/per-user-views.md): the pipeline server and the sample
 * pipeline, the models, the workbench, the playground and the MLflow
 * sample data. The workbench, the playground, the samples and the models of
 * this tool's catalog (B6: inside the cluster, or through the inference
 * gateway when it admits the project) can be set up and removed here when
 * the user's role in the project allows it, as can the project's own S3
 * storage (its admins: B5) and a pipeline server on it (the non-admin
 * design, B3: GET /api/resources/projects/capabilities); a control they may
 * not use is disabled with the reason. A pipeline server on the cluster's
 * shared storage stays a cluster-admin's.
 */
export const UserProjectView: React.FC<{ project: string; refreshKey: number }> = ({ project, refreshKey }) => {
  const [ran, setRan] = useState(0);
  const key = refreshKey + ran;
  return (
    <ProjectCapabilitiesProvider project={project} refreshKey={key}>
      <UserProjectParts project={project} refreshKey={key} onRunEnd={() => setRan((n) => n + 1)} />
    </ProjectCapabilitiesProvider>
  );
};

const UserProjectParts: React.FC<{ project: string; refreshKey: number; onRunEnd: () => void }> = ({ project, refreshKey, onRunEnd }) => {
  const { run, start: startRun, running } = useProjectRun(onRunEnd);
  const status = useProjectRead(loadStatus, project, refreshKey, statusSettling, running);
  const playground = useProjectRead(loadPlayground, project, refreshKey, playgroundSettling, running);
  const samples = useProjectRead(loadSamples, project, refreshKey, neverSettling, running);
  const workbench = useProjectRead(loadWorkbench, project, refreshKey, workbenchSettling, running);
  const storage = useProjectRead(loadStorage, project, refreshKey, storageSettling, running);
  const reads = [status, playground, samples, workbench, storage];
  const server = projectServer(status.data, project);
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [choiceChecked, setChoiceChecked] = useState(false);
  const [typedValue, setTypedValue] = useState("");
  const [deploying, setDeploying] = useState(false);
  const start: StartAction = (title, r, confirm) => {
    if (confirm) {
      setChoiceChecked(false);
      setTypedValue("");
      setPending({ title, changes: confirm.changes, confirm: confirm.label, run: r, choice: confirm.choice, typed: confirm.typed });
    } else startRun(title, r);
  };
  return (
    <Stack hasGutter>
      {run && <StackItem><Card><CardBody><ProjectRunPanel run={run} idPrefix="user-env-run" /></CardBody></Card></StackItem>}
      {reads.some((r) => r.gaveUp) && (
        <StackItem>
          <Alert variant="info" title="Stopped checking automatically after 10 minutes" isInline component="p"
            actionLinks={<AlertActionLink onClick={() => reads.forEach((r) => r.reload())}>Check again</AlertActionLink>} />
        </StackItem>
      )}
      <StackItem><StoragePart project={project} read={storage} busy={running} start={start} /></StackItem>
      <StackItem><PipelinesPart project={project} read={status} storage={storage.data} busy={running} start={start} /></StackItem>
      <StackItem><ModelsPart project={project} read={status} busy={running} start={start} onDeploy={() => setDeploying(true)} /></StackItem>
      <StackItem><WorkbenchPart project={project} read={workbench} busy={running} start={start} /></StackItem>
      <StackItem><PlaygroundPart project={project} read={playground} busy={running} start={start} /></StackItem>
      <StackItem><SamplesPart project={project} read={samples} busy={running} start={start} /></StackItem>
      {/* Last: the long how-to would push the project's state below the fold. */}
      {server?.ready && (
        <StackItem>
          <Card><CardBody><PipelineSample project={project} dspa={server.name} apiRoute={server.apiRoute} uiRoute={server.uiRoute} /></CardBody></Card>
        </StackItem>
      )}
      {deploying && (
        <DeployProjectModel project={project} catalog={status.data?.modelCatalog ?? []} deployed={(status.data?.models ?? []).map((m) => m.name ?? "")} onClose={() => setDeploying(false)}
          onDeploy={(choice) => { setDeploying(false); startRun(choice.title, { kind: "models", req: choice.req }); }} />
      )}
      {pending && (
        <ConfirmActionModal isOpen title={`${pending.title}?`} confirmLabel={pending.confirm} confirmVariant="danger" changesIntro={`This changes project ${project}:`}
          changes={pending.choice && choiceChecked ? [...pending.changes, ...pending.choice.changes]
            : pending.typed && typedValue === pending.typed.expect ? [...pending.changes, ...pending.typed.changes] : pending.changes}
          choice={pending.choice ? (
            <Checkbox id="user-env-confirm-choice" label={pending.choice.label} isChecked={choiceChecked} onChange={(_e, checked) => setChoiceChecked(checked)} />
          ) : pending.typed && (
            <Stack>
              <StackItem><label htmlFor="user-env-confirm-typed">{pending.typed.label}</label></StackItem>
              <StackItem><TextInput id="user-env-confirm-typed" value={typedValue} onChange={(_e, v) => setTypedValue(v)} aria-label={pending.typed.label} /></StackItem>
            </Stack>
          )}
          onConfirm={() => {
            const p = pending;
            setPending(null);
            startRun(p.title, p.choice ? p.choice.run(choiceChecked) : p.typed ? p.typed.run(typedValue === p.typed.expect) : p.run);
          }}
          onCancel={() => setPending(null)} />
      )}
    </Stack>
  );
};
