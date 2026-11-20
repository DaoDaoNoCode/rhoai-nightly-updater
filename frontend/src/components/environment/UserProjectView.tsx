import React, { useEffect, useRef, useState } from "react";
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
import type { MLflowSamplesStatus, PlaygroundStatus, ProjectProblemCode, ProjectStatus, ResourceState } from "../../types";
import { getMLflowSamples, getPlayground, getProjectStatus, toApiError, type ApiError } from "../../services/api";
import { FORBIDDEN_FOR_USER_BODY, FORBIDDEN_FOR_USER_TITLE, describeError, isNotForUser } from "../../errors";
import { EnvStatusLabel, envStatus } from "./resourceStatus";
import { playgroundIn } from "./featureState";
import { PipelineSample } from "./PipelineSample";
import { SAMPLE_INFO, sampleLabel } from "./samples";
import { LinesSkeleton } from "./Loading";

/** What a cluster-admin does on this page for a user who is not one: set up or remove anything. */
export const ADMIN_SETS_UP = "A cluster-admin sets this up.";

interface ProjectRead<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  reload: () => void;
}

/**
 * One read for the chosen project, again when refreshKey changes. An answer
 * for a project no longer chosen is dropped; the error keeps its errorCode
 * (forbidden_for_user is "you cannot see this", not a failure).
 */
function useProjectRead<T>(load: (project: string) => Promise<T>, project: string, refreshKey: number): ProjectRead<T> {
  const [state, setState] = useState<{ project: string; data: T | null; error: ApiError | null }>({ project: "", data: null, error: null });
  const [attempt, setAttempt] = useState(0);
  const current = useRef(project);
  current.current = project;
  useEffect(() => {
    let live = true;
    load(project).then(
      (data) => { if (live && current.current === project) setState({ project, data, error: null }); },
      (e: unknown) => {
        if (live && current.current === project) setState((s) => ({ project, data: s.project === project ? s.data : null, error: toApiError(e) }));
      },
    );
    return () => { live = false; };
  }, [load, project, refreshKey, attempt]);
  const mine = state.project === project;
  return {
    data: mine ? state.data : null,
    error: mine ? state.error : null,
    loading: !mine,
    reload: () => setAttempt((n) => n + 1),
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

const loadStatus = (project: string) => getProjectStatus(project);
const loadPlayground = (project: string) => getPlayground(project);
const loadSamples = (project: string) => getMLflowSamples(project);

const PipelinesPart: React.FC<{ project: string; read: ProjectRead<ProjectStatus> }> = ({ project, read }) => {
  const server = projectServer(read.data, project);
  let body: React.ReactNode;
  if (read.error && !read.data) body = <ReadProblem what="the pipeline server" error={read.error} onRetry={read.reload} />;
  else if (!read.data) body = <LinesSkeleton what="the pipeline server" lines={1} />;
  else if (read.data.pipelineServersProblem) body = <PartProblem what="the pipeline server" code={read.data.pipelineServersProblemCode} message={read.data.pipelineServersProblem} onRetry={read.reload} />;
  else if (!server || (!server.deployed && !server.terminating)) body = <Note>No pipeline server in {project}. {ADMIN_SETS_UP}</Note>;
  else {
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
        {!server.ready && server.message && <StackItem><Note>{server.message}</Note></StackItem>}
        {server.ready && <StackItem><Note>Run a sample pipeline below to check that runs work.</Note></StackItem>}
      </Stack>
    );
  }
  return <Part title="Pipelines" id="user-env-pipelines">{body}</Part>;
};

const ModelsPart: React.FC<{ project: string; read: ProjectRead<ProjectStatus> }> = ({ project, read }) => {
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
              <Th width={50}>Address</Th>
            </Tr>
          </Thead>
          <Tbody>
            {models.map((m) => (
              <Tr key={m.name ?? m.displayName}>
                <Td dataLabel="Model">{m.displayName || m.name}</Td>
                <Td dataLabel="Status"><EnvStatusLabel status={envStatus(m)} /></Td>
                <Td dataLabel="Address">{m.url ? <code className="pf-v6-u-text-break-word">{m.url}</code> : <span className="pf-v6-u-text-color-subtle">None</span>}</Td>
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
    body = !llm && !isvc
      ? <Note>No models in {project}. {ADMIN_SETS_UP}</Note>
      : <Stack hasGutter>{llm && <StackItem>{llm}</StackItem>}{isvc && <StackItem>{isvc}</StackItem>}</Stack>;
  }
  return <Part title="Models" id="user-env-models">{body}</Part>;
};

const PlaygroundPart: React.FC<{ project: string; read: ProjectRead<PlaygroundStatus> }> = ({ project, read }) => {
  const item = read.data ? playgroundIn(read.data, project) : undefined;
  let body: React.ReactNode;
  if (read.error && !read.data) body = <ReadProblem what="the playground" error={read.error} onRetry={read.reload} />;
  else if (!read.data) body = <LinesSkeleton what="the playground" lines={1} />;
  else if (!item && read.data.apiProblem) body = <PartProblem what="the playground" code={read.data.apiCode} message={read.data.apiProblem} onRetry={read.reload} />;
  else if (!item && read.data.blocked) body = <Note>{read.data.blocked.reason}</Note>;
  else if (!item || (!item.deployed && !item.terminating)) body = <Note>No playground in {project}. {ADMIN_SETS_UP}</Note>;
  else {
    body = (
      <Stack hasGutter>
        <StackItem>
          <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
            <FlexItem><strong>Gen AI playground</strong></FlexItem>
            <FlexItem><EnvStatusLabel status={envStatus(item)} /></FlexItem>
          </Flex>
        </StackItem>
        {!item.ready && item.message && <StackItem><Note>{item.message}</Note></StackItem>}
        {item.ready && <StackItem><Note>Open the playground from {project} in the RHOAI dashboard&apos;s Gen AI studio.</Note></StackItem>}
      </Stack>
    );
  }
  return <Part title="Playground" id="user-env-playground">{body}</Part>;
};

const SamplesPart: React.FC<{ project: string; read: ProjectRead<MLflowSamplesStatus> }> = ({ project, read }) => {
  const data = read.data;
  let body: React.ReactNode;
  if (read.error && !data) body = <ReadProblem what="the MLflow sample data" error={read.error} onRetry={read.reload} />;
  else if (!data) body = <LinesSkeleton what="the MLflow sample data" lines={2} />;
  else if (!data.available) body = <Note>{data.reason || "MLflow is not available."} {ADMIN_SETS_UP}</Note>;
  else {
    body = (
      <Stack hasGutter>
        <StackItem><Note>Example data in the MLflow workspace of {project}. A cluster-admin adds or removes it.</Note></StackItem>
        <StackItem>
          <Table aria-label={`MLflow sample data in ${project}`} variant="compact">
            <Thead>
              <Tr>
                <Th width={50}>Sample</Th>
                <Th width={25}>State</Th>
                <Th screenReaderText="Link" />
              </Tr>
            </Thead>
            <Tbody>
              {data.samples.map((s) => {
                const label = sampleLabel(s, null);
                return (
                  <Tr key={s.id}>
                    <Td dataLabel="Sample">{SAMPLE_INFO[s.id]?.name ?? s.name}</Td>
                    <Td dataLabel="State">{label ? <EnvStatusLabel status={label.status} text={label.text} /> : null}</Td>
                    <Td dataLabel="Link" modifier="nowrap">
                      {s.link && (
                        <Button variant="link" isInline component="a" href={s.link} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                          Open in MLflow
                        </Button>
                      )}
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

/**
 * What runs in one of the user's projects, read with their own token
 * (docs/research/per-user-views.md): the pipeline server and the sample
 * pipeline, the models, the playground and the MLflow sample data.
 * Read-only: every set up and removal is a cluster-admin's.
 */
export const UserProjectView: React.FC<{ project: string; refreshKey: number }> = ({ project, refreshKey }) => {
  const status = useProjectRead(loadStatus, project, refreshKey);
  const playground = useProjectRead(loadPlayground, project, refreshKey);
  const samples = useProjectRead(loadSamples, project, refreshKey);
  const server = projectServer(status.data, project);
  return (
    <Stack hasGutter>
      <StackItem><PipelinesPart project={project} read={status} /></StackItem>
      <StackItem><ModelsPart project={project} read={status} /></StackItem>
      <StackItem><PlaygroundPart project={project} read={playground} /></StackItem>
      <StackItem><SamplesPart project={project} read={samples} /></StackItem>
      {/* Last: the long how-to would push the project's state below the fold. */}
      {server?.ready && (
        <StackItem>
          <Card><CardBody><PipelineSample project={project} dspa={server.name} apiRoute={server.apiRoute} uiRoute={server.uiRoute} /></CardBody></Card>
        </StackItem>
      )}
    </Stack>
  );
};
