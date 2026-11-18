import React, { useState } from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
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
  MenuToggle,
  type MenuToggleElement,
  Select,
  SelectList,
  SelectOption,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { CatalogModel, InferenceGatewayState, ModelState, ResourcesStatus } from "../../types";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type ModelsPending, type Pending, ExternalLink, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { CleanupSection, CreatesList, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { envStatusText, notManaged } from "./resourceStatus";
import { ComponentAlert, CopyableCode, NoProjectsAlert } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, catalogModel, dashboardProjectHref, gatewayStatus, modelIn, modelOptionReason, modelStatus, modelTypeWords, shortModelName } from "./featureState";

/** "6Gi" as "6 GiB". */
const gib = (q: string) => q.replace(/^(\d+(?:\.\d+)?)Gi$/, "$1 GiB");

/** What a catalog model reserves on a worker. */
export const modelNeeds = (c: CatalogModel | undefined): string =>
  c ? `${c.cpu} CPUs and ${gib(c.memoryRequest)} of memory (up to ${gib(c.memory)})` : "several CPUs and GiB of memory";

/** The inference gateway's objects (pkg/cluster inference_gateway.go). */
const GATEWAY = "openshift-ai-inference";
const GATEWAY_NS = "openshift-ingress";

export interface ModelsTabProps {
  status: ResourcesStatus;
  project: string | null;
  projects: ProjectOption[];
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  onClearProject: () => void;
  missingProject: string | null;
  /** The catalog name of the model the tab shows. */
  model: string;
  onModel: (model: string) => void;
  /** modelsState for the project and model. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  consoleURL?: string;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
}

const gatewayManaged = (gw: InferenceGatewayState | undefined) => gw?.managedByTool === true;

const TOKEN = "TOKEN=$(oc whoami -t)";
const auth = `-H "Authorization: Bearer $TOKEN"`;

/** Commands that call a chat model with the user's own OpenShift token. Catalog names need no shell quoting. */
export function modelCurl(url: string, model: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    TOKEN,
    `curl -s ${auth} ${base}/v1/models`,
    `curl -s ${auth} -H "Content-Type: application/json" ${base}/v1/chat/completions \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"Say hello in one sentence."}],"max_tokens":64}'`,
  ].join("\n");
}

/** A chat request that turns the model's reasoning (thinking) on for this request only. */
export function thinkingCurl(url: string, model: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    TOKEN,
    `curl -s ${auth} -H "Content-Type: application/json" ${base}/v1/chat/completions \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"Is 1001 a prime number?"}],"max_tokens":512,"chat_template_kwargs":{"enable_thinking":true}}'`,
  ].join("\n");
}

/** A tool-calling request: the model answers with tool_calls when a tool fits. */
export function toolCurl(url: string, model: string): string {
  const base = url.replace(/\/+$/, "");
  const tool = `{"type":"function","function":{"name":"get_weather","description":"Current weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}`;
  return [
    TOKEN,
    `curl -s ${auth} -H "Content-Type: application/json" ${base}/v1/chat/completions \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"What is the weather in Brno?"}],"tools":[${tool}],"max_tokens":256}'`,
  ].join("\n");
}

/** Commands that ask an embedding model for a vector. */
export function embeddingCurl(url: string, model: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    TOKEN,
    `curl -s ${auth} -H "Content-Type: application/json" ${base}/v1/embeddings \\`,
    `  -d '{"model":"${model}","input":["Red Hat OpenShift AI serves models."]}'`,
  ].join("\n");
}

/** Which image a model runs, in words. */
function imageWords(m: ModelState): string {
  switch (m.imageKind) {
    case "released":
      return `Red Hat vLLM CPU${m.imageVersion ? ` ${m.imageVersion}` : ""}, the newest release (this nightly build's preset image is not published yet)`;
    case "override":
      return "The image VLLM_CPU_IMAGE names";
    default:
      return "Red Hat vLLM CPU, the cluster preset's image";
  }
}

const AUTH_WORDS: Record<string, string> = {
  enforced: "On: every call needs an OpenShift token whose user can see the model's project",
  "not-enforced": "Not enforced yet",
  missing: "Added by RHOAI when the first model uses the gateway",
};

/** The inference gateway (cluster). */
const GatewayPart: React.FC<ModelsTabProps> = ({ status, busy, baseReason, readOnly, setPending }) => {
  const gw = status.inferenceGateway;
  const unmanaged = notManaged(gw);
  const deployed = !!gw?.deployed;
  const menu: PartMenuItem[] = !readOnly && gatewayManaged(gw) && !isTerminating(gw)
    ? [{ key: "delete", label: "Delete inference gateway...", onClick: () => setPending({ kind: "teardown-gateway" }), disabledReason: baseReason ?? gw?.teardownBlockedReason ?? null, isDanger: true }]
    : [];
  const s = gatewayStatus(status, busy);
  return (
    <PartItem id="gateway" title="Inference gateway" scope="Shared by the cluster" status={s} unmanaged={unmanaged} menu={menu}>
      <Content component="p">
        The front door to every model on the cluster: it answers at <code>{gw?.externalUrl ?? gw?.plannedUrl ?? "https://inference.<apps domain>"}</code>, sends each call to its model, and checks the caller&apos;s OpenShift token.
        {unmanaged
          ? " It was created outside this tool (for example by RHOAI's installation): models use it as it is, and this tool does not change or delete it."
          : deployed ? " It runs on a cloud load balancer, which costs money while it exists." : " Setting it up creates a cloud load balancer, which costs money while it exists."}
      </Content>
      {gw && <ProblemAlerts kind="gateway" state={gw} />}
      {deployed && s === "setting-up" && !terminalReason(gw) && !isTerminating(gw) && <StartingNote state={gw!} typical="under a minute" />}
      {deployed && (
        <DescriptionList isCompact isHorizontal aria-label="Inference gateway details">
          <DescriptionListGroup>
            <DescriptionListTerm>Gateway</DescriptionListTerm>
            <DescriptionListDescription><code>{gw?.namespace ?? GATEWAY_NS}/{gw?.name ?? GATEWAY}</code></DescriptionListDescription>
          </DescriptionListGroup>
          {gw?.externalUrl && (
            <DescriptionListGroup>
              <DescriptionListTerm>External address</DescriptionListTerm>
              <DescriptionListDescription>
                <ImageRef image={gw.externalUrl} what="external address" truncate={false} isCode={false} />
              </DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {gw?.dns && (
            <DescriptionListGroup>
              <DescriptionListTerm>DNS</DescriptionListTerm>
              <DescriptionListDescription>{gw.dns === "published" ? "Published" : "Not published yet; a new load balancer's name can take a few minutes"}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {gw?.ready && (
            <DescriptionListGroup>
              <DescriptionListTerm>Authentication</DescriptionListTerm>
              <DescriptionListDescription>{gw.authPolicy ? AUTH_WORDS[gw.authPolicy] : "Unknown"}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {(gw?.usedBy ?? []).length > 0 && (
            <DescriptionListGroup>
              <DescriptionListTerm>Used by</DescriptionListTerm>
              <DescriptionListDescription>{(gw?.usedBy ?? []).map((u, i) => <React.Fragment key={u}>{i > 0 ? ", " : ""}<code>{u}</code></React.Fragment>)}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
        </DescriptionList>
      )}
    </PartItem>
  );
};

/** One line about a catalog model: what it is and what it reserves. */
export function modelOptionWords(c: CatalogModel): string {
  return `${modelTypeWords(c)}. For: ${c.useCase}. Reserves ${modelNeeds(c)}. Downloads about ${c.downloadSize}.`;
}

/**
 * The model picker: PatternFly's single Select, each option with what the
 * model is, what it is for, what it reserves and its download, its state
 * in the project, and, when it cannot be set up now, why.
 */
export const ModelPicker: React.FC<{ status: ResourcesStatus; project: string | null; selected: string; onSelect: (name: string) => void; busy: Busy }> = ({ status, project, selected, onSelect, busy }) => {
  const [isOpen, setIsOpen] = useState(false);
  const catalog = status.models?.catalog ?? [];
  const current = catalogModel(status, selected);
  const toggle = (ref: React.Ref<MenuToggleElement>) => (
    <MenuToggle ref={ref} id="models-model" isExpanded={isOpen} isFullWidth onClick={() => setIsOpen(!isOpen)} aria-label="Model">
      {current?.displayName ?? selected}
    </MenuToggle>
  );
  return (
    <Select id="models-model-select" isOpen={isOpen} selected={selected} toggle={toggle} shouldFocusToggleOnSelect
      onOpenChange={setIsOpen} onSelect={(_e, v) => { if (typeof v === "string") onSelect(v); setIsOpen(false); }}>
      <SelectList aria-label="Models this tool can set up">
        {catalog.map((c) => {
          const reason = modelOptionReason(status, project, c.name);
          const here = project ? modelIn(status, project, c.name) : undefined;
          const state = project && here ? ` In ${project}: ${envStatusText(modelStatus(status, project, busy, c.name))}.` : "";
          return (
            <SelectOption key={c.name} value={c.name} isDisabled={!!reason}
              description={reason ? `${modelOptionWords(c)} ${reason}` : `${modelOptionWords(c)}${state}`}>
              {c.displayName}
            </SelectOption>
          );
        })}
      </SelectList>
    </Select>
  );
};

/** What the model is, in a sentence for the part. */
function modelPurpose(c: CatalogModel | undefined): string {
  if (!c) return "A model served by vLLM on CPU.";
  if (c.type === "embedding") return `An embedding model: it turns text into vectors of ${c.embeddingDimension ?? "fixed"} numbers, for semantic search and retrieval. It cannot chat.`;
  if (c.toolCalling) return "A chat model that can call tools you describe in a request. Its reasoning (thinking) is off unless a request turns it on.";
  return c.useCase.includes("smoke") ? "A very small chat model, to check quickly that model serving works. Its answers are simple." : "A small chat model.";
}

/** The chosen model in the selected project, or a placeholder until a project is chosen. */
const ModelPart: React.FC<ModelsTabProps> = ({ status, project, model: name, busy, baseReason, readOnly, setPending }) => {
  const c = catalogModel(status, name);
  const title = c?.displayName ?? name;
  if (!project) {
    return (
      <PartItem id="model" title={title} scope="In the project you choose" status="not-set-up" unmanaged={false}>
        <Content component="p">Choose a project above to see this model there, or to set it up.</Content>
      </PartItem>
    );
  }
  const model = modelIn(status, project, name);
  const unmanaged = notManaged(model);
  const managed = !!model && model.managedByTool !== false;
  const href = dashboardProjectHref(status.dashboardUrl, project);
  const menu: PartMenuItem[] = managed && !readOnly && !isTerminating(model)
    ? [{ key: "delete", label: "Delete model...", onClick: () => setPending({ kind: "teardown-model", project, model: name }), disabledReason: baseReason ?? null, isDanger: true }]
    : [];
  const useReleased = () => setPending({ kind: "setup-models", gateway: false, project, model: name, releasedImage: true });
  const s = modelStatus(status, project, busy, name);
  return (
    <PartItem id="model" title={model?.displayName || title} scope={`In project ${project}`} status={s} unmanaged={unmanaged}
      actions={model?.ready && href && !isTerminating(model) ? <ActionListItem><ExternalLink href={href}>Open {project} in the dashboard</ExternalLink></ActionListItem> : undefined}
      menu={menu}>
      {unmanaged
        ? <Content component="p">A model named <code>{model?.name}</code> was created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : model
          ? <Content component="p">{modelPurpose(c)} Served by vLLM on CPU ({modelNeeds(c)}) in {project}. The dashboard lists it with the project&apos;s deployments{c?.type === "embedding" ? "." : ", and its Gen AI playground can chat with it."}</Content>
          : <Content component="p">{modelPurpose(c)} Not set up in {project} yet: it needs {modelNeeds(c)} on one worker, downloads about {c?.downloadSize ?? "1 GB"}, and takes 3 to 5 minutes the first time.</Content>}
      {model?.presetImageMissing && managed && (
        <Alert variant="danger" isInline component="div" title="The cluster preset's image is not published yet"
          actionLinks={!readOnly && !baseReason ? <AlertActionLink onClick={useReleased}>Use the newest released Red Hat image</AlertActionLink> : undefined}>
          <Content component="p">
            This cluster runs a nightly RHOAI build, whose CPU preset names a runtime image Red Hat has not published, so the model cannot start.
            Use the newest released Red Hat vLLM CPU image instead: only the model&apos;s image changes. To use the preset&apos;s own image, the cluster needs a mirror of <code>registry.redhat.io/rhaii</code> to <code>registry.stage.redhat.io/rhaii</code> and a credential for that registry.
          </Content>
          {readOnly ? <Content component="p">A cluster-admin makes this change.</Content> : baseReason && <Content component="p">Not available now: {baseReason}</Content>}
        </Alert>
      )}
      {model && !model.presetImageMissing && <ProblemAlerts kind="model" state={model} />}
      {model && s === "setting-up" && !terminalReason(model) && !isTerminating(model) && <StartingNote state={model} typical="3 to 5 minutes the first time" />}
      <DescriptionList isCompact isHorizontal aria-label="Model details">
        <DescriptionListGroup>
          <DescriptionListTerm>Name</DescriptionListTerm>
          <DescriptionListDescription><code>{model?.name ?? name}</code></DescriptionListDescription>
        </DescriptionListGroup>
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>Type</DescriptionListTerm>
            <DescriptionListDescription>{modelTypeWords(c)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>For</DescriptionListTerm>
            <DescriptionListDescription>{c.useCase}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reserves</DescriptionListTerm>
            <DescriptionListDescription>{modelNeeds(c)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>From</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack>
                <StackItem><ImageRef image={c.uri} what="model source" isCode /></StackItem>
                <StackItem><Content component="small">Hugging Face, pinned to one commit; about {c.downloadSize} downloaded</Content></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>vLLM options</DescriptionListTerm>
            <DescriptionListDescription><code>{c.args}</code></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.url && (
          <DescriptionListGroup>
            <DescriptionListTerm>Address</DescriptionListTerm>
            <DescriptionListDescription>
              <ImageRef image={model.url} what="model address" isCode={false} />
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Runtime image</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack>
                <StackItem>{imageWords(model)}</StackItem>
                {model.currentImage && <StackItem><ImageRef image={model.currentImage} what="runtime image" /></StackItem>}
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Hardware profile</DescriptionListTerm>
            <DescriptionListDescription>
              {model.hardwareProfile
                ? <><code>{model.hardwareProfile}</code>, as the dashboard shows it</>
                : "None: no profile on this cluster allows the model's CPUs and memory, so the dashboard shows \"No hardware profile\""}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Token authentication</DescriptionListTerm>
            <DescriptionListDescription>
              On: every caller uses their own OpenShift token, so the dashboard lists no tokens for this model.
              If you edit it in the dashboard, add a token there before you save: saving it without one turns authentication off.
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
    </PartItem>
  );
};

/** How to call a ready model: its address, curl with the user's own token (by its type), the dashboard. */
export const UseModel: React.FC<{ model: ModelState; catalog?: CatalogModel; project: string; dashboardUrl?: string }> = ({ model, catalog: c, project, dashboardUrl }) => {
  const href = dashboardProjectHref(dashboardUrl, project);
  const name = model.name!;
  const url = model.url!;
  const embedding = c?.type === "embedding";
  return (
    <TabSection title="Use it" id="models-use">
      <DescriptionList aria-label="How to use the model">
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            <Stack hasGutter>
              <StackItem><Content component="p">The model speaks the OpenAI API. Call it with your own OpenShift token: anyone who can see {project} can, and no extra account or key is made.</Content></StackItem>
              <StackItem><CopyableCode id="models-curl" label="the commands" code={embedding ? embeddingCurl(url, name) : modelCurl(url, name)} /></StackItem>
              {embedding && <StackItem><Content component="p">The answer holds one vector of {c?.embeddingDimension ?? "fixed"} numbers per input text, under <code>data[].embedding</code>. Inputs longer than 512 tokens are refused.</Content></StackItem>}
            </Stack>
          </DescriptionListDescription>
        </DescriptionListGroup>
        {c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Tool calling</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Describe your tools in the request&apos;s <code>tools</code>; when one fits, the answer has <code>tool_calls</code> instead of text. Your program runs the tool and sends the result back in a <code>tool</code> message.</Content></StackItem>
                <StackItem><CopyableCode id="models-tools-curl" label="the tool-calling command" code={toolCurl(url, name)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reasoning</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Thinking is off by default, so answers come quickly. To turn it on for one request, add <code>{`"chat_template_kwargs": {"enable_thinking": true}`}</code>; the reasoning then comes in <code>reasoning_content</code>, apart from the answer.</Content></StackItem>
                <StackItem><CopyableCode id="models-thinking-curl" label="the command with thinking on" code={thinkingCurl(url, name)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {href && (
          <DescriptionListGroup>
            <DescriptionListTerm>In the dashboard</DescriptionListTerm>
            <DescriptionListDescription>
              <Content component="p">
                Open {project} in the dashboard to see the model with the project&apos;s deployments.
                {embedding
                  ? " In Gen AI studio it is listed under AI assets; the Playground tab here adds it to the playground as an embedding model."
                  : " In Gen AI studio, the model is listed under AI assets, where you can try it in the playground."}
              </Content>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="models-next" items={embedding ? [
        <>Copy the commands above and run them: they ask the model for the vector of one sentence.</>,
        <>Point an OpenAI client&apos;s embeddings call at the address, with your token as the API key and <code>{name}</code> as the model.</>,
        <>Set up the playground on the Playground tab: it adds this model for searching your documents.</>,
      ] : [
        <>Copy the commands above and run them: the first lists the model, the second asks it to say hello.</>,
        <>Point an OpenAI client at the address, with your token as the API key and <code>{name}</code> as the model.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI assets, then the playground.</>,
      ]} />
    </TabSection>
  );
};

/**
 * The Models tab: what it needs (model serving turned on, a project), the
 * model to show, the inference gateway (cluster) and that model in the
 * selected project, each with its health, actions and details; Set up for
 * what is missing; how to call a ready model; what it creates; and Clean
 * up.
 */
export const ModelsTab: React.FC<ModelsTabProps> = (props) => {
  const { status, project, projects, projectsLoaded, onProject, onClearProject, missingProject, model: name, onModel, state, baseReason, readOnly, onSetUp, onCleanup, busy } = props;
  const models = status.models;
  const c = catalogModel(status, name);
  const model = project ? modelIn(status, project, name) : undefined;
  const next = state.next;
  const noProjects = projectsLoaded && projects.length === 0;
  const toolModels = (models?.items ?? []).filter((m) => m.managedByTool !== false);
  const anythingToClean = toolModels.length > 0 || gatewayManaged(status.inferenceGateway);
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Models" purpose="Serve small models on CPU in a project: chat, tool calling and embeddings. Call them with your OpenShift token or from the dashboard's Gen AI playground." state={state} /></StackItem>
      {models?.blocked && <StackItem><ComponentAlert feature="Model serving" block={models.blocked} /></StackItem>}
      {!models?.blocked && models?.presetProblem && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="No CPU preset was found for models">{models.presetProblem}</Alert>
        </StackItem>
      )}
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} /></StackItem>}
      {missingProject && (
        <StackItem>
          <Alert variant="info" isInline component="p" title={`Project ${missingProject} no longer exists`}>
            It was deleted, or it is no longer a data science project. Choose another project.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="models-project">
            <ProjectSelect id="models-project" projects={projects} selected={project} onSelect={onProject} onClear={onClearProject} />
            <FormHelperText>
              <HelperText><HelperTextItem>Data science projects (made in the RHOAI dashboard). Each can have several of these models.</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Model" fieldId="models-model">
            <ModelPicker status={status} project={project} selected={name} onSelect={onModel} busy={busy} />
            <FormHelperText>
              <HelperText><HelperTextItem>{c ? modelOptionWords(c) : "The models this tool can set up."}</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
        </Form>
      </StackItem>
      <StackItem>
        <PartList aria-label="What Models needs">
          <GatewayPart {...props} />
          <ModelPart {...props} />
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason} />
        </StackItem>
      )}
      {project && model?.ready && model.url && !isTerminating(model) && <StackItem><UseModel model={model} catalog={c} project={project} dashboardUrl={status.dashboardUrl} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster, unless one exists: GatewayClass <code>{GATEWAY}</code>, and in namespace <code>{GATEWAY_NS}</code> ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> (hostname <code>inference.&lt;apps domain&gt;</code>). OpenShift adds a cloud load balancer and a DNS record for it; RHOAI adds its AuthPolicy once a model uses it</>,
            <>In the project, for each model you set up: an LLMInferenceService of the model&apos;s name (here <code>{name}</code>), downloaded from Hugging Face, on the cluster&apos;s CPU preset{models?.preset ? <> <code>{models.preset}</code></> : null}. KServe adds its Deployment, Service and route</>,
            <>No ServiceAccount, role or token is made: you call the model with your own OpenShift token</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && anythingToClean && (
        <StackItem>
          <CleanupSection id="models-cleanup" label="Clean up Models..." onClick={onCleanup} reason={baseReason}
            text="Deletes what this tool created for Models: every model in every project, then the inference gateway and its load balancer. You choose what goes, then it runs as one operation." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The steps a Models operation streams, in its order (pkg/cluster RunModels). */
export function modelsSteps(p: ModelsPending): { id: string; title: string }[] {
  switch (p.kind) {
    case "setup-models":
      return [
        { id: "check", title: "Check what every part needs" },
        ...(p.gateway ? [{ id: "gateway", title: "Create the inference gateway" }, { id: "gateway-ready", title: "Wait for the gateway and its load balancer" }] : []),
        ...(p.project ? [{ id: "model", title: p.releasedImage ? `Switch ${p.model} in ${p.project} to the released image` : `Create ${p.model} in ${p.project}` }, { id: "model-ready", title: "Wait until the model answers" }] : []),
      ];
    case "teardown-model":
      return [{ id: "model", title: `Delete ${p.model} in ${p.project}` }];
    case "teardown-gateway":
      return [{ id: "gateway", title: "Delete the inference gateway" }];
  }
}

/** The confirmation of a Models change, or null for another feature's change. */
export function modelsConfirm(pending: Pending, status: ResourcesStatus | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-models": {
      const { gateway, project, releasedImage, model: name } = pending;
      const c = catalogModel(status, name);
      const short = shortModelName(status, name);
      const reason = status?.models?.imagePlanReason;
      if (releasedImage && project) {
        return {
          title: `Use the newest released Red Hat image for ${short} in ${project}?`, confirm: "Use the released image", runTitle: `Use the released image for ${short} in ${project}`,
          summary: <Content component="p">The model restarts on the newest released Red Hat vLLM CPU image, looked up in Red Hat&apos;s container catalog and pinned by digest. It takes 3 to 5 minutes.</Content>,
          changes: [<>LLMInferenceService <code>{name}</code> in <code>{project}</code>: the main container&apos;s image is set to that image, and its version is recorded on the model. Nothing else changes.</>],
        };
      }
      const title = gateway && project ? `Set up Models in ${project}` : gateway ? "Set up the inference gateway" : `Set up ${short} in ${project}`;
      return {
        title: `${title}?`, confirm: "Set up", runTitle: title,
        summary: (
          <Stack hasGutter>
            {gateway && <StackItem><Content component="p">On the cluster: the inference gateway, the front door to models. It creates a cloud load balancer, which costs money while it exists.</Content></StackItem>}
            {project && <StackItem><Content component="p">In {project}: {c?.displayName ?? name}, {c ? modelTypeWords(c).toLowerCase() : "a model"}. It needs {modelNeeds(c)} on one worker, downloads about {c?.downloadSize ?? "1 GB"}, and takes 3 to 5 minutes the first time (the runtime image and the model are downloaded).</Content></StackItem>}
          </Stack>
        ),
        changes: [
          ...(gateway ? [
            <>GatewayClass <code>{GATEWAY}</code> (OpenShift&apos;s gateway controller) is created, unless one exists.</>,
            <>In namespace <code>{GATEWAY_NS}</code>: ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> with two HTTPS listeners: <code>inference.&lt;apps domain&gt;</code> on the cluster&apos;s default certificate, and the gateway&apos;s in-cluster name. OpenShift adds a load balancer and a DNS record.</>,
          ] : []),
          ...(project ? [
            <>Project <code>{project}</code>: LLMInferenceService <code>{name}</code> ({c?.displayName ?? name} from Hugging Face), labelled for the dashboard and its Gen AI studio, on the CPU preset{status?.models?.preset ? <> <code>{status.models.preset}</code></> : null}. KServe adds its Deployment, Service and route. Other models in the project are not changed.</>,
            <>Runtime image: {status?.models?.imagePlan === "released" ? "the newest released Red Hat vLLM CPU image, pinned by digest" : status?.models?.imagePlan === "override" ? "the image VLLM_CPU_IMAGE names" : "the preset's own Red Hat vLLM CPU image"}.{reason ? ` ${reason}` : ""}</>,
          ] : []),
          <>No ServiceAccount, role or token is made.</>,
        ],
      };
    }
    case "teardown-model": {
      const short = shortModelName(status, pending.model);
      return {
        title: `Delete ${short} in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete ${short} in ${pending.project}`,
        summary: <Content component="p">Deletes {short} in {pending.project}. The project, its other models, the inference gateway and everything else in the project stay. Set up brings it back (it is downloaded again).</Content>,
        changes: [
          <>Project <code>{pending.project}</code>: LLMInferenceService <code>{pending.model}</code> is deleted; KServe then removes its Deployment, Service and route.</>,
          <>A model of that name this tool did not create is kept.</>,
        ],
      };
    }
    case "teardown-gateway":
      return {
        title: "Delete the inference gateway?", confirm: "Delete", danger: true, runTitle: "Delete the inference gateway",
        summary: <Content component="p">Deletes the inference gateway and, with it, its load balancer and DNS record. Models can no longer be reached until it is set up again; it is not deleted while a model uses it.</Content>,
        changes: [
          <>Gateway <code>{GATEWAY_NS}/{GATEWAY}</code>, ConfigMap <code>{GATEWAY_NS}/{GATEWAY}-options</code> and GatewayClass <code>{GATEWAY}</code> are deleted, those of them this tool created.</>,
          <>Kept: anything of these names this tool did not create, every other gateway (for example <code>maas-default-gateway</code>), and the GatewayClass while another gateway uses it.</>,
        ],
      };
    default:
      return null;
  }
}
