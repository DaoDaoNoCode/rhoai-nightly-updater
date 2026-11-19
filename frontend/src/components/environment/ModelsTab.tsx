import React, { useEffect, useState } from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  Label,
  Radio,
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
  Skeleton,
  Stack,
  StackItem,
  Tooltip,
} from "@patternfly/react-core";
import type { CatalogModel, InferenceGatewayState, ModelCallCheck, ModelExposureKind, ModelExposureOption, ModelExposures, ModelState, ResourcesStatus } from "../../types";
import { getModelExposures, toApiError } from "../../services/api";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type ModelsPending, type Pending, ExternalLink, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { envStatusText, notManaged } from "./resourceStatus";
import { ComponentAlert, CopyableCode, NoProjectsAlert, shellQuote, shellWord } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, callCheckFailed, catalogModel, dashboardProjectHref, gatewayStatus, modelIn, modelOptionReason, modelStatus, modelTypeWords, foreignGatewayStep, shortModelName } from "./featureState";

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
  // --- Models routing ---
  /** What Models needs on the cluster (the one-click prerequisites), above the parts. */
  prerequisites?: React.ReactNode;
  /** Why the model cannot be set up because of the prerequisites, or null. */
  prerequisitesReason?: string | null;
  /** The chosen way to reach a new model (the prerequisites' plan depends on it). */
  onExposure?: (exposure: ModelExposureKind | null) => void;
  /** Check again in place (no dialog: it changes nothing); without it, a confirmation. */
  onCheckModel?: (project: string, model: string) => void;
  /** The model Check again is calling now, or null. */
  checking?: { project: string; model: string } | null;
  /** Why the last Check again could not run, or null. */
  checkError?: string | null;
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
 * (SECURITY.md, "Which token is used where"). The names are shell words
 * (shellWord): bare when plain, single-quoted otherwise.
 */
export function modelTokenLine(project: string, m: ModelState): string | null {
  return m.managedByTool === true && m.tokenSecret ? `TOKEN=$(oc get secret ${shellWord(m.tokenSecret)} -n ${shellWord(project)} -o jsonpath='{.data.token}' | base64 -d)` : null;
}

/**
 * A request's JSON body as one single-quoted shell word. The model name is
 * the model's spec.model.name, which anyone who can edit the project
 * chooses: it goes in through JSON.stringify and the quoting, so no
 * character of it can end the JSON string or the shell word.
 */
const jsonBody = (body: unknown) => shellQuote(JSON.stringify(body));

const HELLO = [{ role: "user", content: "Say hello in one sentence." }];
const EMBED_INPUT = ["Red Hat OpenShift AI serves models."];

/** Commands that call a chat model with its token (modelTokenLine). The address (from the cluster) and the body are single-quoted. */
export function modelCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} ${shellQuote(`${base}/v1/models`)}`,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d ${jsonBody({ model, messages: HELLO, max_tokens: 64 })}`,
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
    `  -d ${jsonBody({ model, messages: [{ role: "user", content: "Is 1001 a prime number?" }], max_tokens: 1024, stream: true, chat_template_kwargs: { enable_thinking: true } })}`,
  ].join("\n");
}

/** A tool-calling request: the model answers with tool_calls when a tool fits. */
export function toolCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  const tool = { type: "function", function: { name: "get_weather", description: "Current weather in a city", parameters: { type: "object", properties: { city: { type: "string" } }, required: ["city"] } } };
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`,
    `  -d ${jsonBody({ model, messages: [{ role: "user", content: "What is the weather in Brno?" }], tools: [tool], max_tokens: 256 })}`,
  ].join("\n");
}

/** Commands that ask an embedding model for a vector. */
export function embeddingCurl(url: string, model: string, token: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    token,
    `curl -s ${auth} -H "Content-Type: application/json" ${shellQuote(`${base}/v1/embeddings`)} \\`,
    `  -d ${jsonBody({ model, input: EMBED_INPUT })}`,
  ].join("\n");
}

/** A request to a model reached only inside the cluster: no token, from a pod on the cluster. */
export function internalCurl(url: string, model: string, embedding: boolean): string {
  const base = url.replace(/\/+$/, "");
  return embedding
    ? [`curl -sk -H "Content-Type: application/json" ${shellQuote(`${base}/v1/embeddings`)} \\`, `  -d ${jsonBody({ model, input: EMBED_INPUT })}`].join("\n")
    : [`curl -sk -H "Content-Type: application/json" ${shellQuote(`${base}/v1/chat/completions`)} \\`, `  -d ${jsonBody({ model, messages: HELLO, max_tokens: 64 })}`].join("\n");
}

/** The two 404s a caller can get, and what each means. */
export const NotFoundNote: React.FC<{ name: string }> = ({ name }) => (
  <Content component="p">
    A 404 means one of two things. With <code>{`{"error":{..."type":"NotFoundError"}}`}</code> the request reached the model but its <code>&quot;model&quot;</code> is not <code>{name}</code> (the display name or the Hugging Face id are not it); with an empty body the request did not reach the model at all (on a gateway the path is <code>/&lt;project&gt;/&lt;model&gt;/v1/...</code>).
  </Content>
);

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
      <Content component="p">{why} Allow adds {project} to the list. Allow only a project you trust: anyone who can edit {project} can then attach routes to this shared gateway, which carries every model's calls and their tokens.</Content>
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

/**
 * Projects on this tool's gateway's list that no model uses (their editors
 * can still attach routes to the shared gateway), first those that no
 * longer exist (a new project of that name would be admitted at once), each
 * with Remove from the list.
 */
const UnusedProjectsAlert: React.FC<{ gw: InferenceGatewayState; reason: string | null; setPending: (p: Pending) => void }> = ({ gw, reason, setPending }) => {
  const gone = gw.goneProjects ?? [];
  const unused = (gw.unusedProjects ?? []).filter((p) => !gone.includes(p));
  if (gone.length === 0 && unused.length === 0) return null;
  const row = (p: string, isGone: boolean) => (
    <StackItem key={p}>
      <code>{p}</code> — {isGone ? "deleted" : "no model uses the gateway"}{" "}
      <Button variant="link" isInline isDanger isAriaDisabled={!!reason} aria-label={`Remove ${p} from the inference gateway`} onClick={() => setPending({ kind: "disallow-gateway-project", project: p, gone: isGone })}>Remove</Button>
    </StackItem>
  );
  return (
    <Alert variant={gone.length > 0 ? "warning" : "info"} isInline component="div" title="The gateway accepts projects that don't use it">
      <Stack hasGutter>
        <StackItem>
          <Content component="p">
            Anyone who can edit a project on the list can attach routes to this shared gateway, and a route there can take another model&apos;s calls and tokens.
            {gone.length > 0 ? " A project created later with a deleted project's name would be accepted at once." : ""}
          </Content>
        </StackItem>
        {gone.map((p) => row(p, true))}
        {unused.map((p) => row(p, false))}
        {reason && <StackItem><Content component="small">Not available now: {reason}</Content></StackItem>}
      </Stack>
    </Alert>
  );
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
  const setUpReason = readOnly ? READ_ONLY_SETUP : baseReason ?? gw?.setupBlockedReason ?? null;
  return (
    <PartItem id="gateway" title="Inference gateway" scope="Shared by the cluster" status={s} unmanaged={unmanaged} menu={menu}
      actions={!deployed && s === "not-set-up" && !readOnly ? (
        <ActionListItem>
          <Button variant="secondary" size="sm" isAriaDisabled={!!setUpReason} onClick={() => setPending({ kind: "setup-models", gateway: true, project: null, model: "" })}>Set up the inference gateway</Button>
        </ActionListItem>
      ) : undefined}>
      <Content component="p">
        A front door for models from outside the cluster: it answers at <code>{gw?.externalUrl ?? gw?.plannedUrl ?? "https://inference.<apps domain>"}</code>, sends each call to its model, and checks the caller&apos;s OpenShift token.
        {unmanaged
          ? " It was created outside this tool (for example by RHOAI's installation): models use it as it is, and this tool does not change or delete it."
          : <>
            {gw?.allProjectsAllowed ? null : " It accepts models only from the projects on its list, as RHOAI documents it; a model set up through it adds its project, and deleting a project's last model takes the project off again."}
            {deployed ? " It runs on a cloud load balancer, which costs money while it exists." : " A model does not need it: MaaS, another gateway or the cluster's inside are other ways. Setting it up creates a cloud load balancer, which costs money while it exists."}
          </>}
      </Content>
      {gw && <ProblemAlerts kind="gateway" state={gw} />}
      {gw?.deployed && gatewayManaged(gw) && !isTerminating(gw) && <UnusedProjectsAlert gw={gw} reason={readOnly ? READ_ONLY_SETUP : baseReason} setPending={setPending} />}
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

// --- Models routing: how a new model is reached ---------------------------------

/** GET /api/resources/models/exposures for the project and model, read again when refreshKey changes. */
export function useModelExposures(project: string | null, model: string, refreshKey: string, enabled: boolean) {
  const [data, setData] = useState<ModelExposures | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (!enabled || !project) {
      setData(null);
      return;
    }
    let live = true;
    getModelExposures(project, model)
      .then((d) => { if (live) { setData(d); setError(null); } })
      .catch((e) => { if (live) setError(toApiError(e, "Cannot read how the model can be reached").message); });
    return () => { live = false; };
  }, [project, model, refreshKey, enabled]);
  return { data, error };
}

/** How a model is reached, in a few words. */
export function exposureWords(m: Pick<ModelState, "exposure" | "gateway" | "maasOnlySetup">): string {
  switch (m.exposure) {
    case "maas":
      return m.maasOnlySetup ? "Through MaaS only" : "Through MaaS while published (Unpublish on the MaaS tab routes it as before)";
    case "gateway":
      return `Through gateway ${m.gateway ?? ""}`;
    case "internal":
      return "Only inside the cluster (no route)";
    case "default-gateway":
      return `Through the cluster's default inference gateway (${m.gateway ?? "KServe's default"})`;
    default:
      return "Unknown";
  }
}

/**
 * The choice of how a new model is reached: every option with what it
 * means, the recommended one first chosen and why, and why the others
 * cannot be used now.
 */
export const ExposureChooser: React.FC<{
  exposures: ModelExposures | null; error: string | null; selected: string | null; onSelect: (key: string) => void;
  /** Set up the inference gateway (null: not offered, for example read-only). */
  onSetUpGateway?: (() => void) | null;
  /** Open the MaaS tab. */
  onMaaS?: () => void;
}> = ({ exposures, error, selected, onSelect, onSetUpGateway, onMaaS }) => {
  if (!exposures) {
    return error ? <Alert variant="warning" isInline component="p" title="Cannot read how the model can be reached">{error}</Alert> : (
      <Stack hasGutter>
        {[0, 1, 2].map((i) => <StackItem key={i}><Skeleton width={i === 0 ? "60%" : "45%"} screenreaderText={i === 0 ? "Reading the gateways on the cluster" : undefined} /></StackItem>)}
      </Stack>
    );
  }
  const chosen = exposures.options.find((o) => o.key === selected);
  const fixLink = (o: ModelExposureOption) => o.fix === "maas-tab" && onMaaS
    ? <Button variant="link" isInline onClick={onMaaS}>Go to the MaaS tab</Button>
    : o.fix === "inference-gateway" && onSetUpGateway
      ? <Button variant="link" isInline onClick={onSetUpGateway}>Set up the inference gateway</Button>
      : null;
  return (
    <Stack hasGutter>
      {exposures.defaultReason && <StackItem><Content component="small">Recommended: {exposures.defaultReason}</Content></StackItem>}
      {!exposures.default && exposures.recommendGateway && (
        <StackItem>
          <Alert variant="info" isInline component="div" title="No gateway can carry this model yet"
            actionLinks={onSetUpGateway ? <AlertActionLink onClick={onSetUpGateway}>Set up the inference gateway</AlertActionLink> : undefined}>
            <Content component="p">{exposures.recommendGatewayReason} Or choose Only inside the cluster.</Content>
          </Alert>
        </StackItem>
      )}
      {exposures.options.map((o) => (
        <StackItem key={o.key}>
          <Radio id={`models-exposure-${o.key}`} name="models-exposure" isChecked={selected === o.key} isDisabled={!o.available} onChange={() => onSelect(o.key)}
            label={<>{o.title}{o.key === exposures.default ? <> <Label isCompact color="green">Recommended</Label></> : null}{o.needsAuth ? <> <Label isCompact color="orange">Needs Connectivity Link</Label></> : null}</>}
            description={o.description}
            body={!o.available ? (
              <HelperText>
                <HelperTextItem variant="warning">{o.reason ?? "Not available now."}{fixLink(o) ? <> {fixLink(o)}</> : null}</HelperTextItem>
              </HelperText>
            ) : undefined} />
        </StackItem>
      ))}
      {chosen?.warning && <StackItem><Alert variant="warning" isInline component="p" title="Before you choose this">{chosen.warning}</Alert></StackItem>}
    </Stack>
  );
};

/** The setup of a model, with the chosen way to reach it. */
function withExposure(setup: Pending[], option: ModelExposureOption | undefined): Pending[] {
  if (!option) return setup;
  return setup.map((p) => (p.kind === "setup-models" && p.project ? { ...p, exposure: option.exposure, ...(option.gatewayRef && option.exposure === "gateway" ? { gatewayRef: option.gatewayRef } : {}), option } : p));
}

/** "Can it be called?" in words, with its result. */
/** How long ago an RFC 3339 time was, in words ("2 minutes ago"). */
export function agoWords(at: string, now: number = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(at).getTime()) / 1000));
  if (Number.isNaN(s)) return "at an unknown time";
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} ${m === 1 ? "minute" : "minutes"} ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} ${h === 1 ? "hour" : "hours"} ago`;
  const d = Math.round(h / 24);
  return `${d} ${d === 1 ? "day" : "days"} ago`;
}

/** The last check: its label and when; brief leaves out the message (a failed check's alert above the details says it). */
const CallCheckWords: React.FC<{ check: ModelCallCheck | undefined; brief?: boolean }> = ({ check, brief }) => {
  if (!check) return <>Not checked since this app started. Check again calls the model once.</>;
  // A failed check needs attention (orange, as the part's status), never an error label.
  const color = check.ok ? "green" : check.result === "not-checked" ? "grey" : "orange";
  const word = check.ok ? (check.result === "maas" ? "Listed by MaaS" : "Answers requests")
    : check.result === "not-checked" ? "Not checked" : check.result === "unauthenticated" ? "Answers without a token" : "Cannot be called";
  return (
    <Stack>
      <StackItem>
        <Label isCompact color={color}>{word}</Label>{" "}
        <Tooltip content={new Date(check.at).toLocaleString()}><Content component="small" tabIndex={0}>Checked {agoWords(check.at)}</Content></Tooltip>
      </StackItem>
      {!brief && <StackItem>{check.message}</StackItem>}
    </Stack>
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
const ModelPart: React.FC<ModelsTabProps> = ({ status, project, model: name, busy, baseReason, readOnly, setPending, onCheckModel, checking, checkError }) => {
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
    ? [{ key: "delete", label: "Delete model...", onClick: () => setPending({ kind: "teardown-model", project, model: name, ...(model?.maasOnlySetup ? { unpublish: true } : {}) }), disabledReason: baseReason ?? null, isDanger: true }]
    : [];
  const useReleased = () => setPending({ kind: "setup-models", gateway: false, project, model: name, releasedImage: true });
  const s = modelStatus(status, project, busy, name);
  const isChecking = !!checking && checking.project === project && checking.model === name;
  const checkAgain = () => (onCheckModel ? onCheckModel(project, name) : setPending({ kind: "check-model", project, model: name }));
  const checkButton = (
    <Button variant="secondary" size="sm" isLoading={isChecking} isAriaDisabled={isChecking || !!baseReason} onClick={checkAgain}>{isChecking ? "Checking..." : "Check again"}</Button>
  );
  const shortName = shortModelName(status, name);
  return (
    <PartItem id="model" title={model?.displayName || title} scope={`In project ${project}`} status={s} unmanaged={unmanaged} menu={menu}>
      {managed && model?.ready && !isTerminating(model) && callCheckFailed(model) && (
        <Alert variant="warning" isInline component="div" title={model.callCheck?.result === "unauthenticated" ? `${shortName} answers without a token` : `${shortName} is running but can't be called`}
          actionLinks={!readOnly && !baseReason && !isChecking ? <AlertActionLink onClick={checkAgain}>Check again</AlertActionLink> : undefined}>
          <Content component="p">{model.callCheck?.message}</Content>
        </Alert>
      )}
      {unmanaged
        ? <Content component="p">A model named <code>{model?.name}</code> was created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : model
          ? <Content component="p">{modelPurpose(c)} It runs on vLLM on CPU in {project} and reserves {modelNeeds(c)}. The dashboard lists it with the project&apos;s deployments{c?.type === "embedding" ? "." : ", and its Gen AI playground can chat with it."}{model.ready && href && !isTerminating(model) ? <> <ExternalLink href={href} isInline>Open {project} in the dashboard</ExternalLink></> : null}</Content>
          : <Content component="p">{modelPurpose(c)} Not set up in {project} yet: it needs {modelNeeds(c)} on one worker, downloads about {c?.downloadSize ?? "1 GB"}, and takes 3 to 5 minutes the first time.</Content>}
      {model?.presetImageMissing && managed && (
        <Alert variant="danger" isInline component="div" title={model.presetImageUncertain ? "The cluster preset's image may not be published" : "The cluster preset's image is not published yet"}
          actionLinks={!readOnly && !baseReason ? <AlertActionLink onClick={useReleased}>Use the newest released Red Hat image</AlertActionLink> : undefined}>
          {model.presetImageUncertain && (
            <Content component="p">
              The image pull has failed for several minutes. One registry answered that the image does not exist, and another refused the node&apos;s credentials, so either the image is not published or the node lacks the pull secret. Details: {model.message}
            </Content>
          )}
          <Content component="p">
            {model.presetImageUncertain ? "If it is not published: this" : "This"} cluster runs a nightly RHOAI build, whose CPU preset names a runtime image Red Hat has not published, so the model cannot start.
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
        {model?.exposure && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reached</DescriptionListTerm>
            <DescriptionListDescription>{exposureWords(model)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.url && model.exposure !== "internal" && (
          <DescriptionListGroup>
            <DescriptionListTerm>{model.exposure === "maas" ? "MaaS address" : "Address"}</DescriptionListTerm>
            <DescriptionListDescription>
              <ImageRef image={model.url} what={model.exposure === "maas" ? "MaaS address" : "model address"} truncate={false} isCode={false} />
              {model.exposure === "maas" && <HelperText><HelperTextItem>Call it with a MaaS API key: see the MaaS tab&apos;s commands.</HelperTextItem></HelperText>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.internalUrl && (
          <DescriptionListGroup>
            <DescriptionListTerm>In-cluster address</DescriptionListTerm>
            <DescriptionListDescription>
              <ImageRef image={model.internalUrl} what="in-cluster address" truncate={false} isCode={false} />
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.servedName && (
          <DescriptionListGroup>
            <DescriptionListTerm>Model name in requests</DescriptionListTerm>
            <DescriptionListDescription><code>{model.servedName}</code> (send exactly this as <code>&quot;model&quot;</code>; the display name or the Hugging Face id gets 404)</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.gatewayAuth && (
          <DescriptionListGroup>
            <DescriptionListTerm>Gateway authentication</DescriptionListTerm>
            <DescriptionListDescription>{model.gatewayAuth === "maas" ? "MaaS checks each call's API key and its subscription" : AUTH_WORDS[model.gatewayAuth]}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && model?.ready && !isTerminating(model) && (
          <DescriptionListGroup>
            <DescriptionListTerm>Can it be called?</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><CallCheckWords check={model.callCheck} brief={callCheckFailed(model)} /></StackItem>
                {!readOnly && !callCheckFailed(model) && <StackItem>{checkButton}</StackItem>}
                {!readOnly && checkError && !isChecking && <StackItem><HelperText><HelperTextItem variant="warning">{checkError}</HelperTextItem></HelperText></StackItem>}
              </Stack>
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
  // The name the model serves: the "model" every request sends.
  const name = model.servedName ?? model.name!;
  const internal = model.exposure === "internal";
  const url = internal ? (model.internalUrl ?? model.url!) : model.url!;
  const embedding = c?.type === "embedding";
  const published = !!model.maasPublished;
  const token = modelTokenLine(project, model);
  const ours = model.managedByTool === true;
  if (model.servedNameWithheld && !published) {
    // The backend withholds a served name that is not safe in commands (pkg/cluster safeServedName).
    return (
      <TabSection title="Use it" id="models-use">
        <Content component="p">
          There are no commands for this model: the name it serves (<code>spec.model.name</code>) has characters that are not safe to paste into a shell, so this tool does not show it. Someone who can edit {project} set it; check the model in the dashboard.
        </Content>
      </TabSection>
    );
  }
  return (
    <TabSection title="Use it" id="models-use">
      {callCheckFailed(model) && (
        <HelperText><HelperTextItem variant="warning">{model.callCheck?.result === "unauthenticated"
          ? "The last check found that the model also answers without a token: anyone who can reach its address can use it."
          : "The last check got no answer; these commands will fail until it passes."}</HelperTextItem></HelperText>
      )}
      <DescriptionList aria-label="How to use the model">
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            {published ? (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    {model.maasOnlySetup
                      ? "The model is reached through MaaS only: it is used through MaaS with an API key and in the dashboard's playground, and it has no direct address."
                      : "The model is published to Models as a Service. While it is published, it is used through MaaS with an API key and in the dashboard's playground; its direct address answers 404 until it is unpublished, because MaaS counts every call against a subscription, which a direct call does not have."}
                  </Content>
                </StackItem>
                <StackItem><Button variant="link" isInline onClick={onMaaS}>Use the MaaS tab&apos;s commands</Button></StackItem>
              </Stack>
            ) : internal ? (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p">
                    The model is reached only inside the cluster, at its in-cluster address, without a token (anyone on the cluster whose network reaches {project} can call it). Run the command from a pod on the cluster; <code>-k</code> because the address uses KServe&apos;s own certificate.
                  </Content>
                </StackItem>
                <StackItem><CopyableCode id="models-curl" label="the command" code={internalCurl(url, name, embedding)} /></StackItem>
                <StackItem><NotFoundNote name={name} /></StackItem>
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
                <StackItem><NotFoundNote name={name} /></StackItem>
              </Stack>
            )}
          </DescriptionListDescription>
        </DescriptionListGroup>
        {!published && !internal && token && c?.toolCalling && (
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
        {!published && !internal && token && c?.toolCalling && (
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
                  ? " In Gen AI studio it is listed under AI asset endpoints; the Playground tab here adds it to the playground as an embedding model."
                  : " In Gen AI studio, the model is listed under AI asset endpoints, where you can try it in the playground."}
              </Content>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="models-next" items={published ? [
        <>Run the MaaS tab&apos;s commands: they get an API key and ask the model to say hello.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
        model.maasOnlySetup
          ? <>Unpublishing it on the MaaS tab leaves it with no outside route, as before it was published: it then answers only at its in-cluster address. Deleting it here unpublishes it first.</>
          : <>To call its direct address again, unpublish it on the MaaS tab.</>,
      ] : internal ? [
        <>Run the command above from a pod on the cluster (for example a workbench in {project}): it asks the model {embedding ? "for the vector of one sentence" : "to say hello"}.</>,
        <>Point an OpenAI client that runs on the cluster at the in-cluster address, with <code>{name}</code> as the model. No token is needed.</>,
        <>To reach it from outside the cluster, delete it and set it up again through a gateway.</>,
      ] : !token ? [
        ours
          ? <>Set it up again to create its token; this tab then shows the commands that call it.</>
          : <>Open {project} in the dashboard and copy the deployment&apos;s token under Token authentication.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
      ] : embedding ? [
        <>Copy the commands above and run them: they ask the model for the vector of one sentence.</>,
        <>Point an OpenAI client&apos;s embeddings call at the address, with the token as the API key and <code>{name}</code> as the model.</>,
        <>Set up the playground on the Playground tab: it adds this model for searching your documents.</>,
      ] : [
        <>Copy the commands above and run them: they read the token, list the model, then ask it to say hello.</>,
        <>Point an OpenAI client at the address, with the token as the API key and <code>{name}</code> as the model.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI asset endpoints, then the playground.</>,
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
  // How a new model is reached: read when it is not set up yet, again when
  // the gateways change; the recommended one is chosen until the user
  // chooses.
  const gw = status.inferenceGateway;
  const choosing = !!project && !model?.deployed && next?.kind === "set-up";
  const exposuresRead = useModelExposures(project, name, `${gw?.deployed}|${gw?.ready}|${(gw?.allowedProjects ?? []).join(",")}`, choosing);
  const [choice, setChoice] = useState<string | null>(null);
  useEffect(() => { setChoice(null); }, [project, name]);
  const ex = exposuresRead.data;
  const selectedKey = choice ?? ex?.default ?? null;
  const option = ex?.options.find((o) => o.key === selectedKey);
  const { onExposure } = props;
  useEffect(() => { onExposure?.(choosing && option ? option.exposure : null); }, [choosing, option, onExposure]);
  const exposureReason = !choosing ? null : !ex ? "Reading how the model can be reached..." : !option ? "Choose how the model is reached." : !option.available ? option.reason ?? "This way cannot be used now." : option.needsAuth ? "Needs Red Hat Connectivity Link for this choice: Set up what Models needs (above) first." : null;
  const setUpGateway = readOnly || baseReason || gw?.deployed ? null : () => props.setPending({ kind: "setup-models", gateway: true, project: null, model: "" });
  // The inference gateway's card comes first only when the model uses it
  // (or the choice is not known yet): otherwise the model's own card leads.
  const toolGateway = `${GATEWAY_NS}/${GATEWAY}`;
  const usesGateway = model?.deployed
    ? model.exposure === "default-gateway" || (model.exposure === "gateway" && model.gateway === toolGateway)
    : !option || option.exposure === "default-gateway" || option.gatewayRef === toolGateway;
  const prerequisitesReason = props.prerequisitesReason ?? null;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Models" purpose={FEATURE_PURPOSE.models} state={state} /></StackItem>
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
          {choosing && (
            <FormGroup label="Model access" fieldId="models-exposure" role="radiogroup" isStack>
              <ExposureChooser exposures={ex} error={exposuresRead.error} selected={selectedKey} onSelect={setChoice} onSetUpGateway={setUpGateway} onMaaS={onMaaS} />
              <FormHelperText>
                <HelperText><HelperTextItem>To change this later, delete the model and set it up again.</HelperTextItem></HelperText>
              </FormHelperText>
            </FormGroup>
          )}
        </Form>
      </StackItem>
      {/* What the cluster needs comes after the choice it depends on, so the choices never move when it changes. */}
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {models?.blocked && <StackItem><ComponentAlert feature="Model serving" block={models.blocked} /></StackItem>}
      {!models?.blocked && models?.presetProblem && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="No CPU preset was found for models">{models.presetProblem}</Alert>
        </StackItem>
      )}
      <StackItem>
        <PartList aria-label="What Models needs">
          {usesGateway ? <GatewayPart {...props} /> : <ModelPart {...props} />}
          {usesGateway ? <ModelPart {...props} /> : <GatewayPart {...props} />}
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} variant={prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(withExposure(next.setup, option))} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? prerequisitesReason ?? exposureReason} />
        </StackItem>
      )}
      {project && model?.ready && (model.url || model.internalUrl) && !isTerminating(model) && (
        <StackItem>
          <UseModel model={model} catalog={c} project={project} dashboardUrl={status.dashboardUrl} onMaaS={onMaaS} readOnly={readOnly} baseReason={baseReason}
            onSetUpAgain={() => props.setPending({ kind: "setup-models", gateway: false, project, model: name })} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Only when you set up the inference gateway (its own part): GatewayClass <code>{GATEWAY}</code> unless one exists, and in namespace <code>{GATEWAY_NS}</code> ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> (hostname <code>inference.&lt;apps domain&gt;</code>), which accepts models only from the projects it lists by name, as RHOAI documents it. OpenShift adds a cloud load balancer and a DNS record for it; RHOAI adds its AuthPolicy once a model uses it</>,
            <>On this tool&apos;s gateway, each project you set up a model in through it is added to that list, and taken off again when the project&apos;s last model on the gateway is deleted. Allow adds a project whose model was deployed elsewhere, such as in the dashboard</>,
            <>Through MaaS only: the model is published (as on the MaaS tab). Through another gateway: only the model&apos;s route names it; the gateway is not changed. Only inside the cluster: no route at all</>,
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
        ...(p.project ? [{ id: "model", title: p.releasedImage ? `Switch ${p.model} in ${p.project} to the released image` : `Create ${p.model} in ${p.project}` }, { id: "model-ready", title: "Wait until the model runs" }] : []),
        ...(p.project && p.exposure === "maas" ? [{ id: "publish", title: "Publish it to MaaS" }] : []),
        ...(p.project ? [{ id: "callable", title: "Check that it can be called" }] : []),
      ];
    case "check-model":
      return [{ id: "callable", title: `Call ${p.model} in ${p.project} once` }];
    case "disallow-gateway-project":
      return [{ id: "check", title: "Check that no model there uses the gateway" }, { id: "gateway", title: `Take ${p.project} off the gateway's projects` }];
    case "teardown-model":
      return [
        ...(p.unpublish ? [{ id: "unpublish", title: `Unpublish ${p.model} from MaaS` }] : []),
        { id: "model", title: `Delete ${p.model} in ${p.project}` },
      ];
    case "teardown-gateway":
      return [{ id: "gateway", title: "Delete the inference gateway" }];
    case "allow-gateway-project":
      return [{ id: "check", title: "Check the gateway and the project" }, { id: "gateway", title: `Add ${p.project} to the gateway's projects` }];
  }
}

/**
 * Whether deleting these models of project takes the project off this
 * tool's gateway's list (pkg/cluster releaseGatewayProject): the tool
 * added it, and no other model there routes through the gateway. The
 * backend checks again and keeps it on any doubt.
 */
export function gatewayListRelease(status: ResourcesStatus | null, project: string, models?: string[]): boolean {
  const gw = status?.inferenceGateway;
  if (!gw?.deployed || gw.managedByTool !== true || !(gw.addedProjects ?? []).includes(project)) return false;
  const users = (gw.usedBy ?? []).filter((u) => u.split("/")[0] === project);
  const going = (u: string) => !models || models.includes(u.split("/")[1]);
  return users.length > 0 && users.every(going);
}

/** The change line of a project going off the gateway's list. */
export const gatewayReleaseLine = (project: string) => (
  <>Gateway <code>{GATEWAY_NS}/{GATEWAY}</code>: <code>{project}</code> is taken off the gateway&apos;s projects; routes made by hand there stop attaching.</>
);

/** Whether a model setup in project adds it to this tool's gateway's list. */
function setupAddsProject(status: ResourcesStatus | null, project: string | null, gateway: boolean, exposure?: ModelExposureKind): boolean {
  const gw = status?.inferenceGateway;
  if (!project || gateway || !gw?.deployed || gw.managedByTool !== true || (exposure && exposure !== "default-gateway")) return false;
  return !(gw.allowedProjects ?? []).includes(project);
}

/** The confirmation of a Models change, or null for another feature's change. */
export function modelsConfirm(pending: Pending, status: ResourcesStatus | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-models": {
      const { gateway, project, releasedImage, model: name, option } = pending;
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
            {project && <StackItem><Content component="p">Creates {c?.displayName ?? name}, {c ? `a ${modelTypeWords(c).toLowerCase()} model` : "a model"}, in {project}. It needs {modelNeeds(c)} on one worker, downloads about {c?.downloadSize ?? "1 GB"}, and takes 3 to 5 minutes the first time (the runtime image and the model are downloaded).</Content></StackItem>}
            {project && option && <StackItem><Content component="p">Reached: {option.title.charAt(0).toLowerCase() + option.title.slice(1)}. {option.description}{option.exposure === "maas" ? " Each caller can use up to 100,000 tokens a day (the MaaS docs' premium example); change who and how much on the MaaS tab." : null}{option.address && !option.description.includes(option.address) ? <> It answers at <code>{option.address}</code>.</> : null}</Content></StackItem>}
            {project && option?.warning && <StackItem><Alert variant="warning" isInline component="p" title="Before you set it up">{option.warning}</Alert></StackItem>}
            {project && <StackItem><Content component="p">Once it runs, the tool calls it once, as a user would, and reports whether it answers.</Content></StackItem>}
          </Stack>
        ),
        changes: [
          ...(gateway ? [
            <>GatewayClass <code>{GATEWAY}</code> (OpenShift&apos;s gateway controller) is created, unless one exists.</>,
            <>In namespace <code>{GATEWAY_NS}</code>: ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> with two HTTPS listeners: <code>inference.&lt;apps domain&gt;</code> on the cluster&apos;s default certificate, and the gateway&apos;s in-cluster name. Both accept models only from the projects they list by name: <code>{GATEWAY_NS}</code>{project ? <> and <code>{project}</code></> : null}. OpenShift adds a load balancer and a DNS record.</>,
          ] : []),
          ...(option?.changes ?? []).map((ch) => <>{ch}</>),
          ...(!option && setupAddsProject(status, project, gateway, pending.exposure) ? [
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
    case "check-model": {
      const short = shortModelName(status, pending.model);
      return {
        title: `Check that ${short} in ${pending.project} can be called?`, confirm: "Check", runTitle: `Check ${short} in ${pending.project}`,
        summary: <Content component="p">The tool sends the model one minimal request the way it is reached, with the model&apos;s own token (never yours), and reports what answered.</Content>,
        changes: [<>Nothing on the cluster changes.</>],
      };
    }
    case "disallow-gateway-project":
      return {
        title: `Remove ${pending.project} from the inference gateway's projects?`, confirm: `Remove ${pending.project}`, danger: true, runTitle: `Remove ${pending.project} from the gateway's projects`,
        summary: <Content component="p">{pending.gone
          ? `Project ${pending.project} was deleted, but the gateway still accepts it. If someone creates a project with that name, its routes could take other models' calls and tokens. Only the gateway's list of projects changes.`
          : `Routes in ${pending.project} stop working on the gateway, including any someone made by hand (this tool can't see those). The tool refuses if a model in ${pending.project} still uses the gateway.`}</Content>,
        changes: [<>Gateway <code>{GATEWAY_NS}/{GATEWAY}</code>: <code>{pending.project}</code> is taken off the projects both listeners allow. The other projects stay; nothing else changes.</>],
      };
    case "teardown-model": {
      const short = shortModelName(status, pending.model);
      const leaves = gatewayListRelease(status, pending.project, [pending.model]);
      return {
        title: `Delete ${short} in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete ${short} in ${pending.project}`,
        summary: <Content component="p">Deletes {short} in {pending.project}{pending.unpublish ? ", which is reached through MaaS only, so it is unpublished first" : ""}. The project, its other models, the inference gateway and everything else in the project stay{leaves ? `; ${pending.project} goes off the gateway's list of projects` : ""}. Set up brings it back (it is downloaded again).</Content>,
        changes: [
          ...(pending.unpublish ? [<>Unpublishes it from Models as a Service first (its subscription entry and model reference go).</>] : []),
          <>Project <code>{pending.project}</code>: LLMInferenceService <code>{pending.model}</code> is deleted; KServe then removes its Deployment, Service and route.</>,
          ...(leaves ? [gatewayReleaseLine(pending.project)] : []),
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
