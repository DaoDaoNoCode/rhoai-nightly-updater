import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { PlaygroundModel, PlaygroundModelRef, PlaygroundStatus, ResourcesStatus } from "../../types";
import { type ConfirmSpec, type Pending, type PlaygroundPending, ExternalLink, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, CopyableCode, NoProjectsAlert, shellQuote } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { LinesSkeleton, LoadFailed, PartsSkeleton } from "./Loading";
import {
  type Busy,
  type FeatureState,
  defaultPlaygroundModels,
  playgroundChatModels,
  playgroundHref,
  playgroundIn,
  playgroundModelStatus,
  playgroundModelsNotReady,
  playgroundStatusOf,
  playgroundOtherEmbeddingModels,
  playgroundUsableModels,
} from "./featureState";

export interface PlaygroundTabProps {
  /** What the playground needs on the cluster (one-click prerequisites), above the parts. */
  prerequisites?: React.ReactNode;
  /** Why the feature cannot be set up until its cluster prerequisites are, or null. */
  prerequisitesReason?: string | null;
  status: ResourcesStatus;
  /**
   * GET /api/resources/playground: null until it has been read; while
   * another project's answer is shown (playground.project), the project's
   * models are still loading.
   */
  playground: PlaygroundStatus | null;
  /** Why the project's playground status cannot be read, or null. */
  playgroundError: string | null;
  /** Reads it again (Try again). */
  onRetry: () => void;
  project: string | null;
  projects: ProjectOption[];
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  onClearProject: () => void;
  missingProject: string | null;
  /** playgroundState for the project: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
  /** Opens the Models tab for the project. */
  onModels: () => void;
}

const modelKey = (m: Pick<PlaygroundModel, "source" | "name">) => `${m.source}/${m.name}`;

/** A model's name for people: its display name, else its name; a Models as a Service model says so. */
function modelWords(m: Pick<PlaygroundModel, "source" | "name" | "displayName">): string {
  const name = m.displayName || m.name;
  return m.source === "maas" ? `${name} (Models as a Service)` : name;
}

/** The models a reference list names, as the project's model list describes them (a model no longer listed by its name only). */
function modelsOf(refs: PlaygroundModelRef[], pg: PlaygroundStatus | null): PlaygroundModel[] {
  return refs.map((r) => pg?.models?.find((m) => modelKey(m) === modelKey(r)) ?? { ...r, ready: false, chat: true });
}

/** Items in prose: "a", "a and b", "a, b and c". */
function joinWords(items: string[]): string {
  return items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}
const sourceWords = (m: PlaygroundModel) => (m.source === "maas" ? "Models as a Service" : "Deployed in the project");

/** A technical name that may wrap after each "/" (never inside a word). */
const BreakableName: React.FC<{ name: string }> = ({ name }) => (
  <code>{name.split("/").map((part, i) => <React.Fragment key={i}>{i > 0 && <>/<wbr /></>}{part}</React.Fragment>)}</code>
);

/** s as one single-quoted shell word (a ' inside becomes '\''). */

/** This tool's MaaS subscription (pkg/cluster maasSubscriptionName). */
const TOOL_MAAS_SUBSCRIPTION = "rhoai-nightly-updater-subscription";

/**
 * A chat request to the project's playground with the user's own OpenShift token. The model name comes from the dashboard, so the body is built as JSON and quoted for the shell.
 * A MaaS model's request names one of its subscriptions (this tool's when offered under it): the dashboard mints the MaaS key for that subscription, and without one the playground answers 500 (odh-dashboard rhoai-3.6 packages/gen-ai/bff lsd_responses_handler.go "subscription").
 */
export function playgroundCurl(dashboardUrl: string, project: string, model: string, source: string, subscriptions: string[] = []): string {
  const base = dashboardUrl.replace(/\/+$/, "");
  const subscription = source === "maas" ? (subscriptions.includes(TOOL_MAAS_SUBSCRIPTION) ? TOOL_MAAS_SUBSCRIPTION : subscriptions[0]) : undefined;
  const body = JSON.stringify({ model, model_source_type: source, ...(subscription ? { subscription } : {}), input: "Say hello in one sentence.", stream: false });
  return [
    "TOKEN=$(oc whoami -t)",
    `curl -s -X POST ${shellQuote(`${base}/gen-ai/api/v1/lsd/responses?namespace=${encodeURIComponent(project)}`)} \\`,
    `  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \\`,
    `  -d ${shellQuote(body)}`,
  ].join("\n");
}

/** What the playground does with a model, in words (after Set up the same for every Models as a Service model: the playground lists the ones each user's subscriptions allow). */
function playgroundUse(m: PlaygroundModel, picked: string[] | null): string {
  if (picked) {
    if (picked.includes(modelKey(m))) return "Used";
    return m.source === "maas" ? "Not used unless you choose it (needs a Models as a Service subscription)" : "Not used";
  }
  return m.source === "maas" ? "Only with a Models as a Service subscription" : "Used";
}

/**
 * The model(s) the playground uses, in the project: a compact table (name,
 * where it runs, whether the playground uses it). Before Set up its rows
 * are selectable: the project's own models are chosen, as in the dashboard.
 */
const ModelsPart: React.FC<PlaygroundTabProps & { project: string; picked: string[] | null; onPick: ((keys: string[]) => void) | null }> = ({ playground, playgroundError, project, onModels, picked, onPick }) => {
  // The answer shown is another project's (the picker just changed): this one's models are on their way.
  if (playground?.project !== project) {
    return (
      <PartItem id="playground-models" title="Models" scope={`In project ${project}`} status="unknown" unmanaged={false} loading={!playgroundError}>
        {playgroundError ? <Content component="p">The project&apos;s models cannot be read now: the alert above says why.</Content> : <LinesSkeleton what={`the models in ${project}`} />}
      </PartItem>
    );
  }
  const s = playgroundModelStatus(playground, project);
  const chat = playgroundChatModels(playground, project);
  const usable = playgroundUsableModels(playground, project);
  const otherEmbedding = playgroundOtherEmbeddingModels(playground, project);
  const notReady = playgroundModelsNotReady(playground, project);
  const recorded = playgroundIn(playground, project)?.models ?? [];
  const toggle = (m: PlaygroundModel, on: boolean) => onPick?.(on ? [...(picked ?? []).filter((k) => k !== modelKey(m)), modelKey(m)] : (picked ?? []).filter((k) => k !== modelKey(m)));
  return (
    <PartItem id="playground-models" title="Models" scope={`In project ${project}`} status={s} unmanaged={false}
      actions={<ActionListItem><Button variant="link" isInline onClick={onModels}>{chat.length > 0 ? "Show on the Models tab" : "Set up a model on the Models tab"}</Button></ActionListItem>}>
      <Content component="p">
        {s === "unknown"
          ? playground?.modelsProblem ?? "The dashboard does not say which models the project has."
          : chat.length > 0
            ? onPick
              ? "The playground uses the models you choose: the project's own ready models are chosen, chat models and this tool's embedding model. A chat model added to the project later shows up in the playground without setting it up again."
              : recorded.length > 0
                ? `Set up chose ${joinWords(modelsOf(recorded, playground).map(modelWords))}. The playground offers the project's own ready models and the Models as a Service models your subscriptions allow.`
                : "The playground offers the project's own ready models and the Models as a Service models your subscriptions allow. A model added to the project later shows up without setting it up again."
            : `${project} has no ready chat model yet. The playground needs one: the Models tab sets up a small one on CPU.`}
      </Content>
      {usable.length > 0 && chat.length > 0 && (
        <Table variant="compact" aria-label="Models the playground can use">
          <Thead>
            <Tr>
              {onPick && <Th screenReaderText="Use" />}
              <Th modifier="wrap">Model</Th>
              <Th modifier="wrap">Type</Th>
              <Th modifier="wrap">Where it runs</Th>
              <Th modifier="wrap">In the playground</Th>
            </Tr>
          </Thead>
          <Tbody>
            {usable.map((m, i) => (
              <Tr key={modelKey(m)}>
                {onPick && <Td select={{ rowIndex: i, onSelect: (_e, on) => toggle(m, on), isSelected: !!picked?.includes(modelKey(m))}} />}
                <Td dataLabel="Model" modifier="wrap">
                  <Stack>
                    <StackItem>{m.displayName || m.name}</StackItem>
                    {m.displayName && m.displayName !== m.name && <StackItem><Content component="small"><BreakableName name={m.name} /></Content></StackItem>}
                  </Stack>
                </Td>
                <Td dataLabel="Type" modifier="wrap">{m.chat ? "Chat" : `Embedding (${m.embeddingDimension} dimensions), for searching documents`}</Td>
                <Td dataLabel="Where it runs" modifier="wrap">{sourceWords(m)}</Td>
                <Td dataLabel="In the playground" modifier="wrap">{playgroundUse(m, picked)}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
      {otherEmbedding.length > 0 && (
        <Alert variant="info" isInline component="div" title="Embedding models to add in the dashboard">
          <Content component="p">
            Set up here does not add {otherEmbedding.map((m) => m.name).join(", ")}: this tool did not set {otherEmbedding.length === 1 ? "it" : "them"} up, so the size of {otherEmbedding.length === 1 ? "its" : "their"} vectors is not known.
            To use one, open the playground&apos;s settings in the dashboard&apos;s Gen AI studio, choose <strong>Embedding</strong> as its type, and enter its embedding dimension (the model card on Hugging Face gives it).
          </Content>
        </Alert>
      )}
      {notReady.length > 0 && (
        <Alert variant="info" isInline component="p" title="Some models are not ready">
          These models are not ready, so Set up leaves them out: {notReady.map((m) => `${m.name} (${m.status ?? "unknown"})`).join(", ")}. Once one is ready, the playground offers it without setting it up again.
        </Alert>
      )}
    </PartItem>
  );
};

/** The dashboard's playground in the project. */
const PlaygroundPart: React.FC<PlaygroundTabProps & { project: string }> = ({ status, playground, project, busy, baseReason, readOnly, setPending }) => {
  const item = playgroundIn(playground, project);
  const exists = !!item?.deployed || !!item?.terminating;
  const unmanaged = exists && item?.managedByTool === false;
  const s = playgroundStatusOf(playground, project, busy);
  const href = playgroundHref(status.dashboardUrl, project);
  const menu: PartMenuItem[] = exists && !unmanaged && !readOnly && !isTerminating(item)
    ? [{ key: "delete", label: "Delete playground", onClick: () => setPending({ kind: "teardown-playground", project }), disabledReason: baseReason ?? null, isDanger: true }]
    : [];
  return (
    <PartItem id="playground" title="Playground" scope={`In project ${project}`} status={s} unmanaged={unmanaged}
      actions={item?.ready && href ? <ActionListItem><ExternalLink href={href}>Open the playground</ExternalLink></ActionListItem> : undefined}
      menu={menu}>
      <Content component="p">
        {unmanaged
          ? "Someone set this playground up in the dashboard: use it as it is. This tool does not change or delete it."
          : exists
            ? "The dashboard's Gen AI playground: a chat page where you try the project's models, add documents to ask about, and compare answers."
            : `Not set up in ${project} yet. The dashboard's Gen AI playground is a chat page where you try the project's models; Set up asks the dashboard to create it, as its own Gen AI studio does. It takes 1 to 2 minutes.`}
      </Content>
      {item?.terminalError
        ? <Alert variant="danger" isInline component="div" title="The playground can't start"><Content component="p">{item.message}</Content></Alert>
        : item && exists && <ProblemAlerts kind="playground" state={item} />}
      {item && s === "setting-up" && !terminalReason(item) && !isTerminating(item) && <StartingNote state={item} typical="1 to 2 minutes" />}
      {exists && (
        <DescriptionList isCompact isHorizontal aria-label="Playground details">
          {item?.phase !== "Ready" && (
            // Ready is the part's own label; another phase says more than it.
            <DescriptionListGroup>
              <DescriptionListTerm>State</DescriptionListTerm>
              <DescriptionListDescription>{item?.phase ?? "Not reported (unknown)"}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {item?.name && (
            <DescriptionListGroup>
              <DescriptionListTerm>Server</DescriptionListTerm>
              <DescriptionListDescription><code>{item.name}</code></DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {href && (
            <DescriptionListGroup>
              <DescriptionListTerm>Where</DescriptionListTerm>
              <DescriptionListDescription>In the dashboard: Gen AI studio, then Playground, then project {project}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
        </DescriptionList>
      )}
    </PartItem>
  );
};

/** How to use a ready playground: the dashboard page, and a chat request from a terminal. */
const UsePlayground: React.FC<{ dashboardUrl?: string; project: string; model?: PlaygroundModel }> = ({ dashboardUrl, project, model }) => {
  const href = playgroundHref(dashboardUrl, project);
  return (
    <TabSection title="Use it" id="playground-use">
      <DescriptionList aria-label="How to use the playground">
        {href && (
          <DescriptionListGroup>
            <DescriptionListTerm>In the dashboard</DescriptionListTerm>
            <DescriptionListDescription>
              <Content component="p">Open the playground, choose a model, and type a question. Upload a document to ask questions about it.</Content>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {dashboardUrl && model && (
          <DescriptionListGroup>
            <DescriptionListTerm>From a terminal</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">The playground answers through the dashboard&apos;s gen-ai API. Call it with your own OpenShift token: anyone who can use {project} can.</Content></StackItem>
                <StackItem><CopyableCode id="playground-curl" label="the commands" code={playgroundCurl(dashboardUrl, project, model.name, model.source, model.subscriptions)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="playground-next" items={[
        <>Open the playground and ask the model something.</>,
        <>Add another model to {project} on the Models tab: the playground offers it without setting up again.</>,
      ]} />
    </TabSection>
  );
};

/**
 * The Playground tab: what the dashboard's Gen AI playground needs (a ready
 * chat model in the project), the playground itself with its state and the
 * link to it, Set up (with the models it uses), how to use it, what it
 * creates, and Clean up.
 */
export const PlaygroundTab: React.FC<PlaygroundTabProps> = (props) => {
  const { status, playground, playgroundError, onRetry, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, baseReason, readOnly, onSetUp, onCleanup } = props;
  const next = state.next;
  const noProjects = projectsLoaded && projects.length === 0;
  const item = project ? playgroundIn(playground, project) : undefined;
  const toolPlaygrounds = (playground?.items ?? []).filter((i) => i.managedByTool && (i.deployed || i.terminating));
  const defaults = React.useMemo(() => (project ? defaultPlaygroundModels(playground, project).map(modelKey) : []), [playground, project]);
  // Another project starts from its defaults, in the same render: an effect
  // would reset the choice one render later, dropping a pick made in between.
  const [chosen, setChosen] = React.useState<string[] | null>(null);
  const [chosenFor, setChosenFor] = React.useState(project);
  if (chosenFor !== project) {
    setChosenFor(project);
    setChosen(null);
  }
  const picked = chosen ?? defaults;
  const chat = project ? playgroundChatModels(playground, project) : [];
  const usable = project ? playgroundUsableModels(playground, project) : [];
  const pickedModels = usable.filter((m) => picked.includes(modelKey(m)));
  const models: PlaygroundModelRef[] = pickedModels.map((m) => ({ name: m.name, source: m.source }));
  const settingUp = next?.kind === "set-up";
  const choosing = settingUp && !readOnly;
  const pickReason = settingUp && chat.length > 0 && !pickedModels.some((m) => m.chat) ? "Choose at least one chat model." : null;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Playground" purpose={FEATURE_PURPOSE.playground} state={state} /></StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {playground?.blocked && <StackItem><ComponentAlert feature="The Gen AI playground" block={playground.blocked} /></StackItem>}
      {playground && playgroundError && (
        <StackItem>
          <Alert variant="danger" isInline component="p" title="Cannot read the playground" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>{playgroundError}</Alert>
        </StackItem>
      )}
      {playground?.apiProblem && <StackItem><Alert variant="warning" isInline component="p" title="The dashboard's gen-ai API cannot be used">{playground.apiProblem}</Alert></StackItem>}
      {!playground?.blocked && playground?.genAiUi === "missing" && (
        <StackItem><Alert variant="warning" isInline component="p" title="Gen AI studio is not deployed">The dashboard&apos;s Gen AI studio, which makes the playground, is not on this cluster. A cluster-admin turns it on in the dashboard&apos;s settings (Gen AI studio).</Alert></StackItem>
      )}
      {!playground?.blocked && playground?.operatorReady === "False" && (
        <StackItem><Alert variant="warning" isInline component="p" title="The playground's operator is not ready">{playground.operatorMessage || "The DataScienceCluster reports it not ready."} The Components page shows the details.</Alert></StackItem>
      )}
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} things="Playgrounds" /></StackItem>}
      {missingProject && (
        <StackItem>
          <Alert variant="info" isInline component="p" title={`Project ${missingProject} no longer exists`}>
            It was deleted, or it is no longer a data science project. Choose another project.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="playground-project">
            <ProjectSelect id="playground-project" projects={projects} selected={project} onSelect={onProject} onClear={onClearProject} />
            <FormHelperText>
              <HelperText><HelperTextItem>Data science projects (made in the RHOAI dashboard). Each can have one playground, which uses the project&apos;s models.</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
        </Form>
      </StackItem>
      <StackItem>
        {project && !playground ? (
          playgroundError ? <LoadFailed title="Cannot read the playground" error={playgroundError} onRetry={onRetry} />
            : <PartsSkeleton aria-label="What Playground needs" what="what Playground needs" count={2} />
        ) : project ? (
          <PartList aria-label="What Playground needs">
            <ModelsPart {...props} project={project} picked={choosing ? picked : null} onPick={choosing ? setChosen : null} />
            <PlaygroundPart {...props} project={project} />
          </PartList>
        ) : (
          <PartList aria-label="What Playground needs">
            <PartItem id="playground" title="Playground" scope="In the project you choose" status="not-set-up" unmanaged={false}>
              <Content component="p">Choose a project above to see its playground, or to set one up there.</Content>
            </PartItem>
          </PartList>
        )}
      </StackItem>
      {next?.kind === "set-up" && project && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp([{ kind: "setup-playground", project, models }])} variant={props.prerequisitesReason ? "secondary" : "primary"} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? props.prerequisitesReason ?? pickReason} />
        </StackItem>
      )}
      {project && item?.ready && <StackItem><UsePlayground dashboardUrl={status.dashboardUrl} project={project} model={chat[0]} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>The dashboard creates the playground in the project, as a click in its Gen AI studio does: an OGXServer (the chat server, which the OGX operator runs as a Deployment), its configuration ConfigMaps, and a small PostgreSQL database with pgvector (a Deployment, a 5 GiB PersistentVolumeClaim, a Secret, a Service) for uploaded documents</>,
            <>This tool records, in its own namespace, that it asked for the playground, so that it deletes only playgrounds it asked for</>,
            <>Nothing else changes: the models stay as they are</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && toolPlaygrounds.length > 0 && (
        <StackItem>
          <CleanupSection id="playground-cleanup" label="Clean up Playground" onClick={onCleanup} reason={baseReason}
            text="Deletes the playgrounds this tool set up, in every project, through the dashboard. You choose which, then it runs as one operation." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The steps a Playground operation streams, in its order (pkg/cluster RunPlayground). */
export function playgroundSteps(p: PlaygroundPending): { id: string; title: string }[] {
  return p.kind === "setup-playground"
    ? [
      { id: "check", title: "Check what the playground needs" },
      { id: "install", title: `Ask the dashboard to set up the playground in ${p.project}` },
      { id: "ready", title: "Wait until the playground is ready" },
    ]
    : [
      { id: "delete", title: `Ask the dashboard to delete the playground in ${p.project}` },
      { id: "gone", title: "Wait until it is gone" },
    ];
}

/** The confirmation of a Playground change, or null for another feature's change. */
export function playgroundConfirm(pending: Pending, pg: PlaygroundStatus | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-playground": {
      const { project } = pending;
      const names = (pending.models?.length ? modelsOf(pending.models, pg) : defaultPlaygroundModels(pg, project)).map(modelWords);
      return {
        title: `Set up the playground in ${project}?`, confirm: "Set up", runTitle: `Set up the playground in ${project}`,
        summary: <Content component="p">The dashboard sets up its Gen AI playground in {project}, as its own Gen AI studio does, with {names.length ? joinWords(names) : "the project's ready chat models"}. It takes 1 to 2 minutes.{pending.models?.some((m) => m.source === "maas") && " The dashboard reaches the Models as a Service model with your sign-in, as when you choose one in its Gen AI studio."}</Content>,
        changes: [
          <>Project <code>{project}</code>: the dashboard creates an OGXServer (the chat server), its configuration ConfigMaps, and a PostgreSQL database with pgvector (Deployment, 5 GiB PersistentVolumeClaim, Secret, Service).</>,
          <>This tool records in its own namespace that it asked for the playground.</>,
          <>The models are not changed.</>,
        ],
      };
    }
    case "teardown-playground":
      return {
        title: `Delete the playground in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete the playground in ${pending.project}`,
        summary: <Content component="p">The dashboard deletes the playground in {pending.project}. Chats and the documents uploaded to it are lost. Set up brings back an empty one.</Content>,
        changes: [
          <>Deleted: the OGXServer, its configuration ConfigMaps, and the pgvector database with its data.</>,
          <>Kept: the project, its models, and everything else in it.</>,
        ],
      };
    default:
      return null;
  }
}
