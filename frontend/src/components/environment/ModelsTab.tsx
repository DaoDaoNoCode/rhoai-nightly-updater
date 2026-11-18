import React, { useState } from "react";
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
import { ComponentAlert, CopyableCode, NoProjectsAlert, shellQuote } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, catalogModel, dashboardProjectHref, gatewayStatus, modelIn, modelOptionReason, modelStatus, modelTypeWords, foreignGatewayStep, shortModelName } from "./featureState";

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
  /** Opens the MaaS tab (a published model is used through MaaS). */
  onMaaS: () => void;
}

const gatewayManaged = (gw: InferenceGatewayState | undefined) => gw?.managedByTool === true;

/** The model's token ServiceAccount, as the dashboard names it (<model>-sa). */
const tokenSA = (model: string) => `${model}-sa`;
const auth = `-H "Authorization: Bearer $TOKEN"`;

/**
 * The command that puts the model's own token in $TOKEN: the token Secret
 * this tool made as the dashboard does (pkg/cluster models_token.go), read
 * with the user's own oc. Only for this tool's model with that Secret;
 * otherwise null, and no command calls the model: the user's own token
 * never goes to a model, whose pod is in a project its editors control
 * (SECURITY.md, "Which token is used where"). Secret and project names are
 * Kubernetes names (no shell quoting).
 */
export function modelTokenLine(project: string, m: ModelState): string | null {
  return m.managedByTool === true && m.tokenSecret ? `TOKEN=$(oc get secret ${m.tokenSecret} -n ${project} -o jsonpath='{.data.token}' | base64 -d)` : null;
}

/** Commands that call a chat model with its token (modelTokenLine). Catalog names need no shell quoting; the address comes from the cluster, so it is single-quoted. */
export function modelCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} ${shellQuote(`${base}/v1/models`)}`,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"Say hello in one sentence."}],"max_tokens":64}'`,
  ].join("\n");
}

/**
 * A chat request that turns the model's reasoning (thinking) on for this
 * request only. It streams (curl -N prints each event as it arrives):
 * thinking on CPU can take over a minute, and the cloud load balancer in
 * front of the gateway closes a connection that sends nothing for 60 s.
 */
export function thinkingCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -N -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"Is 1001 a prime number?"}],"max_tokens":1024,"stream":true,"chat_template_kwargs":{"enable_thinking":true}}'`,
  ].join("\n");
}

/** A tool-calling request: the model answers with tool_calls when a tool fits. */
export function toolCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  const tool = `{"type":"function","function":{"name":"get_weather","description":"Current weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}`;
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"What is the weather in Brno?"}],"tools":[${tool}],"max_tokens":256}'`,
  ].join("\n");
}

/** Commands that ask an embedding model for a vector. */
export function embeddingCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/embeddings`)} \\`,
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

/**
 * Models (anyone's, such as one deployed from the dashboard) that route
 * through the gateway from a project it does not list: their routes are
 * rejected, so they never become Ready. For this tool's gateway, Allow
 * adds the project to its list; for someone else's, RHOAI's documented
 * step for a cluster admin.
 */
const NotAllowedAlert: React.FC<{ gw: InferenceGatewayState; project: string; models: string[]; baseReason: string | null; readOnly: boolean; setPending: (p: Pending) => void }> = ({ gw, project, models, baseReason, readOnly, setPending }) => {
  const one = models.length === 1;
  const title = `${models.join(", ")} can't be reached: ${one ? "its" : "their"} project isn't allowed on the inference gateway`;
  const why = `${one ? "Its route is" : "Their routes are"} rejected because ${project} is not on the gateway's list of projects, so the dashboard shows ${one ? "it" : "them"} as not ready.`;
  if (!gatewayManaged(gw)) {
    return (
      <Alert variant="warning" isInline component="div" title={title}>
        <Content component="p">{why} {foreignGatewayStep(project)}</Content>
      </Alert>
    );
  }
  const reason = readOnly ? "A cluster-admin makes this change." : baseReason;
  return (
    <Alert variant="warning" isInline component="div" title={title}
      actionLinks={!reason ? <AlertActionLink onClick={() => setPending({ kind: "allow-gateway-project", project })}>Allow {project}</AlertActionLink> : undefined}>
      <Content component="p">{why} Allow adds {project} to the list.</Content>
      {reason && <Content component="p">{readOnly ? reason : `Not available now: ${reason}`}</Content>}
    </Alert>
  );
};

/** The models the gateway does not allow, by project, in the projects' order. */
function notAllowedByProject(gw: InferenceGatewayState): [string, string[]][] {
  const out = new Map<string, string[]>();
  for (const ref of gw.notAllowedModels ?? []) {
    const ns = ref.split("/")[0];
    out.set(ns, [...(out.get(ns) ?? []), ref]);
  }
  return [...out.entries()];
}

/** The projects the gateway allows, in words. */
function allowedWords(gw: InferenceGatewayState): React.ReactNode {
  if (gw.allProjectsAllowed) return "Every project";
  if (!gw.allowedProjects) return "Chosen by its namespace selector";
  if (gw.allowedProjects.length === 0) return "None";
  return gw.allowedProjects.map((p, i) => <React.Fragment key={p}>{i > 0 ? ", " : ""}<code>{p}</code></React.Fragment>);
}

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
          : <>
            {gw?.allProjectsAllowed ? null : " It accepts models only from the projects on its list, as RHOAI documents it; setting up a model adds its project."}
            {deployed ? " It runs on a cloud load balancer, which costs money while it exists." : " Setting it up creates a cloud load balancer, which costs money while it exists."}
          </>}
      </Content>
      {gw && <ProblemAlerts kind="gateway" state={gw} />}
      {gw?.deployed && !isTerminating(gw) && s !== "setting-up" && notAllowedByProject(gw).map(([ns, models]) => (
        <NotAllowedAlert key={ns} gw={gw} project={ns} models={models} baseReason={baseReason} readOnly={readOnly} setPending={setPending} />
      ))}
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
          {gw && (
            <DescriptionListGroup>
              <DescriptionListTerm>Projects allowed on the gateway</DescriptionListTerm>
              <DescriptionListDescription>{allowedWords(gw)}</DescriptionListDescription>
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

/**
 * What a catalog model is for, once: its use case as the dashboard shows it
 * (opendatahub.io/genai-use-case), and an embedding model's vector size.
 */
export function modelUseWords(c: CatalogModel): string {
  return c.type === "embedding" && c.embeddingDimension ? `${c.useCase}; vectors of ${c.embeddingDimension} numbers` : c.useCase;
}

/** One line about a catalog model: what it is for and what it reserves. */
export function modelOptionWords(c: CatalogModel): string {
  return `${modelUseWords(c)}. Reserves ${modelNeeds(c)}. Downloads about ${c.downloadSize}.`;
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
    <Select id="models-model-select" isOpen={isOpen} selected={selected} toggle={toggle} shouldFocusToggleOnSelect popperProps={{ width: "trigger" }}
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
          ? <Content component="p">{modelPurpose(c)} It runs on vLLM on CPU in {project} and reserves {modelNeeds(c)}. The dashboard lists it with the project&apos;s deployments{c?.type === "embedding" ? "." : ", and its Gen AI playground can chat with it."}</Content>
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
            <DescriptionListTerm>Use case</DescriptionListTerm>
            <DescriptionListDescription>{modelUseWords(c)}</DescriptionListDescription>
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
                : <>None fits: no hardware profile on this cluster allows {modelNeeds(c)}, so the dashboard's Deployments table shows &quot;No hardware profile&quot; and the model&apos;s details &quot;Hardware profile: Unknown&quot;. The model runs as set up.</>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Token authentication</DescriptionListTerm>
            <DescriptionListDescription>
              {model.tokenAuthOff
                ? <>Off: security.opendatahub.io/enable-auth is &quot;false&quot; on the model (set in the dashboard), so its address answers without a token. Turn &quot;Require token authentication&quot; back on in the dashboard&apos;s Edit.</>
                : model.tokenSecret
                  ? <>On, as the dashboard sets it up: ServiceAccount {model.tokenServiceAccount} may only get this model, and its token is in Secret <code>{model.tokenSecret}</code>, which the dashboard lists as the model&apos;s token &quot;default-name&quot;.</>
                  : <>On, but the model has no token of its own yet, so the dashboard lists none and its Edit would turn authentication off when saved. Set it up again to add the model&apos;s token.</>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
    </PartItem>
  );
};

/**
 * A ready model without commands: this tool's model with no token of its
 * own yet (Set up again creates it), or a model of a catalog name this
 * tool did not set up (its token is the dashboard's). Never the user's
 * own token: a model's pod is in a project its editors control.
 */
const NoModelCommands: React.FC<{ model: ModelState; project: string; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ model, project, readOnly, baseReason, onSetUpAgain }) => {
  const name = model.name!;
  if (model.managedByTool !== true) {
    return (
      <Content component="p">
        This model was not set up by this tool (for example, it was deployed in the dashboard), so there are no commands for it here. Call it with the token the dashboard shows under Token authentication for that deployment: open {project} in the dashboard, then the model&apos;s details. Use that token rather than your own OpenShift token: whoever can edit {project} controls what answers at the model&apos;s address.
      </Content>
    );
  }
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          The model has no token of its own yet, so there are no commands to call it: it was set up before this tool made the dashboard&apos;s token for each model. Setting it up again creates ServiceAccount <code>{tokenSA(name)}</code>, which may only call this model, and its token Secret <code>default-name-{tokenSA(name)}</code>; nothing else changes.
        </Content>
      </StackItem>
      <StackItem>
        {readOnly
          ? <Content component="p">A cluster-admin sets it up again to create its token.</Content>
          : <>
            <Button variant="secondary" onClick={onSetUpAgain} isAriaDisabled={!!baseReason}>Set up again to create its token</Button>
            {baseReason && <Content component="p">Not available now: {baseReason}</Content>}
          </>}
      </StackItem>
    </Stack>
  );
};

/**
 * How to call a ready model: its address, curl with the model's own token
 * (by its type), the dashboard. A model published to MaaS answers only
 * through MaaS, so then it points at the MaaS tab's commands instead.
 */
export const UseModel: React.FC<{ model: ModelState; catalog?: CatalogModel; project: string; dashboardUrl?: string; onMaaS: () => void; readOnly: boolean; baseReason: string | null; onSetUpAgain: () => void }> = ({ model, catalog: c, project, dashboardUrl, onMaaS, readOnly, baseReason, onSetUpAgain }) => {
  const href = dashboardProjectHref(dashboardUrl, project);
  const name = model.name!;
  const url = model.url!;
  const embedding = c?.type === "embedding";
  const published = !!model.maasPublished;
  const token = modelTokenLine(project, model);
  const ours = model.managedByTool === true;
  return (
    <TabSection title="Use it" id="models-use">
      <DescriptionList aria-label="How to use the model">
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            {published ? (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The model is published to Models as a Service. While it is published, it is used through MaaS with an API key and in the dashboard&apos;s playground; its direct address answers 404 until it is unpublished, because MaaS counts every call against a subscription, which a direct call does not have.
                  </Content>
                </StackItem>
                <StackItem><Button variant="link" isInline onClick={onMaaS}>Use the MaaS tab&apos;s commands</Button></StackItem>
              </Stack>
            ) : !token ? (
              <NoModelCommands model={model} project={project} readOnly={readOnly} baseReason={baseReason} onSetUpAgain={onSetUpAgain} />
            ) : (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The model speaks the OpenAI API. The first command reads the model&apos;s own token from Secret <code>{model.tokenSecret}</code>: a token that can only call this model, the same one the dashboard shows for the model. Anyone who can read Secrets in {project} can use it.
                  </Content>
                </StackItem>
                <StackItem><CopyableCode id="models-curl" label="the commands" code={embedding ? embeddingCurl(url, name, token) : modelCurl(url, name, token)} /></StackItem>
                {!embedding && <StackItem><Content component="p">Requests that take longer than about a minute must stream (<code>{`"stream": true`}</code>), because the cloud load balancer closes a connection that sends nothing for 60 seconds (the AWS default).</Content></StackItem>}
                {embedding && <StackItem><Content component="p">The answer holds one vector of {c?.embeddingDimension ?? "fixed"} numbers per input text, under <code>data[].embedding</code>. Inputs longer than 512 tokens are refused.</Content></StackItem>}
              </Stack>
            )}
          </DescriptionListDescription>
        </DescriptionListGroup>
        {!published && token && c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Tool calling</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Describe your tools in the request&apos;s <code>tools</code>; when one fits, the answer has <code>tool_calls</code> instead of text. Your program runs the tool and sends the result back in a <code>tool</code> message.</Content></StackItem>
                <StackItem><CopyableCode id="models-tools-curl" label="the tool-calling command" code={toolCurl(url, name, token)} /></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {!published && token && c?.toolCalling && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reasoning</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><Content component="p">Thinking is off by default, so answers come quickly. To turn it on for one request, add <code>{`"chat_template_kwargs": {"enable_thinking": true}`}</code>; the reasoning then comes in <code>reasoning_content</code>, apart from the answer.</Content></StackItem>
                <StackItem><CopyableCode id="models-thinking-curl" label="the command with thinking on" code={thinkingCurl(url, name, token)} /></StackItem>
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
      <WhatNext id="models-next" items={published ? [
        <>Run the MaaS tab&apos;s commands: they get an API key and ask the model to say hello.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI assets, then the playground.</>,
        <>To call its direct address again, unpublish it on the MaaS tab.</>,
      ] : !token ? [
        ours
          ? <>Set it up again to create its token; this tab then shows the commands that call it.</>
          : <>Open {project} in the dashboard and copy the deployment&apos;s token under Token authentication.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI assets, then the playground.</>,
      ] : embedding ? [
        <>Copy the commands above and run them: they ask the model for the vector of one sentence.</>,
        <>Point an OpenAI client&apos;s embeddings call at the address, with the token as the API key and <code>{name}</code> as the model.</>,
        <>Set up the playground on the Playground tab: it adds this model for searching your documents.</>,
      ] : [
        <>Copy the commands above and run them: they read the token, list the model, then ask it to say hello.</>,
        <>Point an OpenAI client at the address, with the token as the API key and <code>{name}</code> as the model.</>,
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
  const { status, project, projects, projectsLoaded, onProject, onClearProject, missingProject, model: name, onModel, state, baseReason, readOnly, onSetUp, onCleanup, onMaaS, busy } = props;
  const models = status.models;
  const c = catalogModel(status, name);
  const model = project ? modelIn(status, project, name) : undefined;
  const next = state.next;
  const noProjects = projectsLoaded && projects.length === 0;
  const toolModels = (models?.items ?? []).filter((m) => m.managedByTool !== false);
  const anythingToClean = toolModels.length > 0 || gatewayManaged(status.inferenceGateway);
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Models" purpose="Serve small models on CPU in a project: chat, tool calling and embeddings. Call them with each model's own token or from the dashboard's Gen AI playground." state={state} /></StackItem>
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
      {project && model?.ready && model.url && !isTerminating(model) && (
        <StackItem>
          <UseModel model={model} catalog={c} project={project} dashboardUrl={status.dashboardUrl} onMaaS={onMaaS} readOnly={readOnly} baseReason={baseReason}
            onSetUpAgain={() => props.setPending({ kind: "setup-models", gateway: false, project, model: name })} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster, unless one exists: GatewayClass <code>{GATEWAY}</code>, and in namespace <code>{GATEWAY_NS}</code> ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> (hostname <code>inference.&lt;apps domain&gt;</code>), which accepts models only from the projects it lists by name, as RHOAI documents it. OpenShift adds a cloud load balancer and a DNS record for it; RHOAI adds its AuthPolicy once a model uses it</>,
            <>On this tool&apos;s gateway, each project you set up a model in is added to that list (and stays on it when the model is deleted, since other models there may use it). Allow adds a project whose model was deployed elsewhere, such as in the dashboard</>,
            <>In the project, for each model you set up: an LLMInferenceService of the model&apos;s name (here <code>{name}</code>), downloaded from Hugging Face, on the cluster&apos;s CPU preset{models?.preset ? <> <code>{models.preset}</code></> : null}. KServe adds its Deployment, Service and route</>,
            <>For each model, its token authentication as the dashboard&apos;s &quot;Require token authentication&quot; makes it: ServiceAccount <code>{tokenSA(name)}</code>, Role <code>{name}-view-role</code> (get on that model only), RoleBinding <code>{name}-view</code> and token Secret <code>default-name-{tokenSA(name)}</code>, all owned by the model, so they go with it</>,
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
    case "allow-gateway-project":
      return [{ id: "check", title: "Check the gateway and the project" }, { id: "gateway", title: `Add ${p.project} to the gateway's projects` }];
  }
}

/** Whether a model setup in project adds it to this tool's gateway's list. */
function setupAddsProject(status: ResourcesStatus | null, project: string | null, gateway: boolean): boolean {
  const gw = status?.inferenceGateway;
  if (!project || gateway || !gw?.deployed || gw.managedByTool !== true) return false;
  return !(gw.allowedProjects ?? []).includes(project);
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
            <>In namespace <code>{GATEWAY_NS}</code>: ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> with two HTTPS listeners: <code>inference.&lt;apps domain&gt;</code> on the cluster&apos;s default certificate, and the gateway&apos;s in-cluster name. Both accept models only from the projects they list by name: <code>{GATEWAY_NS}</code>{project ? <> and <code>{project}</code></> : null}. OpenShift adds a load balancer and a DNS record.</>,
          ] : []),
          ...(setupAddsProject(status, project, gateway) ? [
            <>Gateway <code>{GATEWAY_NS}/{GATEWAY}</code>: <code>{project}</code> is added to the projects it allows, before the model is created. The projects already on the list stay.</>,
          ] : []),
          ...(project ? [
            <>Project <code>{project}</code>: LLMInferenceService <code>{name}</code> ({c?.displayName ?? name} from Hugging Face), labelled for the dashboard and its Gen AI studio, on the CPU preset{status?.models?.preset ? <> <code>{status.models.preset}</code></> : null}. KServe adds its Deployment, Service and route. Other models in the project are not changed.</>,
            <>Runtime image: {status?.models?.imagePlan === "released" ? "the newest released Red Hat vLLM CPU image, pinned by digest" : status?.models?.imagePlan === "override" ? "the image VLLM_CPU_IMAGE names" : "the preset's own Red Hat vLLM CPU image"}.{reason ? ` ${reason}` : ""}</>,
          ] : []),
          ...(project ? [<>Token authentication, as the dashboard makes it: ServiceAccount {tokenSA(name)}, Role {name}-view-role (get on this model only), RoleBinding {name}-view and token Secret default-name-{tokenSA(name)}, owned by the model. Ones already there for this model are kept.</>] : []),
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
    case "allow-gateway-project":
      return {
        title: `Allow ${pending.project} on the inference gateway?`, confirm: "Allow", runTitle: `Allow ${pending.project} on the inference gateway`,
        summary: <Content component="p">The models in {pending.project} then attach to the gateway and can be reached. Anyone who may edit {pending.project} can then also attach routes to the gateway, as on every project on its list.</Content>,
        changes: [
          <>Gateway <code>{GATEWAY_NS}/{GATEWAY}</code>: <code>{pending.project}</code> is added to the projects both listeners allow (the values array of their <code>allowedRoutes</code>). The projects already on the list stay; nothing else changes.</>,
        ],
      };
    default:
      return null;
  }
}
