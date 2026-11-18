import React from "react";
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
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { InferenceGatewayState, ModelState, ResourcesStatus } from "../../types";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type ModelsPending, type Pending, ExternalLink, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { CleanupSection, CreatesList, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { notManaged } from "./resourceStatus";
import { ComponentAlert, CopyableCode, NoProjectsAlert } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, dashboardProjectHref, gatewayStatus, modelIn, modelStatus } from "./featureState";

/** The catalog's model (pkg/cluster modelCatalog), for the words before the status has loaded. */
export const MODEL_NAME = "qwen25-05b-instruct";
export const MODEL_DISPLAY = "Qwen2.5 0.5B Instruct";

/** "6Gi" as "6 GiB". */
const gib = (q: string) => q.replace(/^(\d+(?:\.\d+)?)Gi$/, "$1 GiB");

/** What the catalog's model needs on a worker, from the status (pkg/cluster modelCatalog). */
export const modelNeeds = (status: ResourcesStatus | null | undefined): string => {
  const c = status?.models?.catalog?.[0];
  return c ? `${c.cpu} CPUs and ${gib(c.memoryRequest)} of memory (up to ${gib(c.memory)})` : "4 CPUs and several GiB of memory";
};
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
  /** modelsState for the project: the same as the Overview's. */
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

/** Commands that call the model with the user's own OpenShift token. */
export function modelCurl(url: string, model: string): string {
  const base = url.replace(/\/+$/, "");
  return [
    "TOKEN=$(oc whoami -t)",
    `curl -s -H "Authorization: Bearer $TOKEN" ${base}/v1/models`,
    `curl -s -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" ${base}/v1/chat/completions \\`,
    `  -d '{"model":"${model}","messages":[{"role":"user","content":"Say hello in one sentence."}],"max_tokens":64}'`,
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

/** The model in the selected project, or a placeholder until a project is chosen. */
const ModelPart: React.FC<ModelsTabProps> = ({ status, project, busy, baseReason, readOnly, setPending }) => {
  if (!project) {
    return (
      <PartItem id="model" title="Model" scope="In the project you choose" status="not-set-up" unmanaged={false}>
        <Content component="p">Choose a project above to see its model, or to set one up there.</Content>
      </PartItem>
    );
  }
  const model = modelIn(status, project);
  const unmanaged = notManaged(model);
  const managed = !!model && model.managedByTool !== false;
  const href = dashboardProjectHref(status.dashboardUrl, project);
  const menu: PartMenuItem[] = managed && !readOnly && !isTerminating(model)
    ? [{ key: "delete", label: "Delete model...", onClick: () => setPending({ kind: "teardown-model", project }), disabledReason: baseReason ?? null, isDanger: true }]
    : [];
  const useReleased = () => setPending({ kind: "setup-models", gateway: false, project, releasedImage: true });
  const s = modelStatus(status, project, busy);
  return (
    <PartItem id="model" title={model?.displayName || `${MODEL_DISPLAY} (CPU)`} scope={`In project ${project}`} status={s} unmanaged={unmanaged}
      actions={model?.ready && href && !isTerminating(model) ? <ActionListItem><ExternalLink href={href}>Open {project} in the dashboard</ExternalLink></ActionListItem> : undefined}
      menu={menu}>
      {unmanaged
        ? <Content component="p">A model named <code>{model?.name}</code> was created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : model
          ? <Content component="p">A small chat model, served by vLLM on CPU ({modelNeeds(status)}) in {project}. The dashboard lists it with the project&apos;s deployments, and its Gen AI playground can chat with it.</Content>
          : <Content component="p">Not set up in {project} yet. Set up deploys {MODEL_DISPLAY} on CPU: it needs {modelNeeds(status)} on one worker, and takes 3 to 5 minutes the first time.</Content>}
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
      {model && (
        <DescriptionList isCompact isHorizontal aria-label="Model details">
          <DescriptionListGroup>
            <DescriptionListTerm>Model</DescriptionListTerm>
            <DescriptionListDescription><code>{model.name}</code></DescriptionListDescription>
          </DescriptionListGroup>
          {model.url && (
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
      )}
    </PartItem>
  );
};

/** How to call a ready model: its address, curl with the user's own token, the dashboard. */
const UseModel: React.FC<{ model: ModelState; project: string; dashboardUrl?: string }> = ({ model, project, dashboardUrl }) => {
  const href = dashboardProjectHref(dashboardUrl, project);
  return (
    <TabSection title="Use it" id="models-use">
      <DescriptionList aria-label="How to use the model">
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            <Stack hasGutter>
              <StackItem><Content component="p">The model speaks the OpenAI API. Call it with your own OpenShift token: anyone who can see {project} can, and no extra account or key is made.</Content></StackItem>
              <StackItem><CopyableCode id="models-curl" label="the commands" code={modelCurl(model.url!, model.name ?? MODEL_NAME)} /></StackItem>
            </Stack>
          </DescriptionListDescription>
        </DescriptionListGroup>
        {href && (
          <DescriptionListGroup>
            <DescriptionListTerm>In the dashboard</DescriptionListTerm>
            <DescriptionListDescription>
              <Content component="p">Open {project} in the dashboard to see the model with the project&apos;s deployments. In Gen AI studio, the model is listed under AI assets, where you can try it in the playground.</Content>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="models-next" items={[
        <>Copy the commands above and run them: the first lists the model, the second asks it to say hello.</>,
        <>Point an OpenAI client at the address, with your token as the API key and <code>{model.name ?? MODEL_NAME}</code> as the model.</>,
        <>Try it in the dashboard&apos;s Gen AI studio: AI assets, then the playground.</>,
      ]} />
    </TabSection>
  );
};

/**
 * The Models tab: what it needs (model serving turned on, a project), the
 * inference gateway (cluster) and the selected project's model, each with
 * its health, actions and details; Set up for what is missing; how to call
 * a ready model; what it creates; and Clean up.
 */
export const ModelsTab: React.FC<ModelsTabProps> = (props) => {
  const { status, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, baseReason, readOnly, onSetUp, onCleanup } = props;
  const models = status.models;
  const model = project ? modelIn(status, project) : undefined;
  const next = state.next;
  const noProjects = projectsLoaded && projects.length === 0;
  const toolModels = (models?.items ?? []).filter((m) => m.managedByTool !== false);
  const anythingToClean = toolModels.length > 0 || gatewayManaged(status.inferenceGateway);
  const capacity = models?.capacityWarning && project && (!model || !model.ready) ? models.capacityWarning : null;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Models" purpose="Serve a small chat model on CPU in a project, and call it with your OpenShift token or from the dashboard's Gen AI playground." state={state} /></StackItem>
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
              <HelperText><HelperTextItem>Data science projects (made in the RHOAI dashboard). Each can have its own model.</HelperTextItem></HelperText>
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
      {capacity && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="The model may not fit on a worker">{capacity}</Alert>
        </StackItem>
      )}
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason} />
        </StackItem>
      )}
      {project && model?.ready && model.url && !isTerminating(model) && <StackItem><UseModel model={model} project={project} dashboardUrl={status.dashboardUrl} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>On the cluster, unless one exists: GatewayClass <code>{GATEWAY}</code>, and in namespace <code>{GATEWAY_NS}</code> ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> (hostname <code>inference.&lt;apps domain&gt;</code>). OpenShift adds a cloud load balancer and a DNS record for it; RHOAI adds its AuthPolicy once a model uses it</>,
            <>In the project: LLMInferenceService <code>{MODEL_NAME}</code> ({MODEL_DISPLAY}, from Hugging Face), on the cluster&apos;s CPU preset{models?.preset ? <> <code>{models.preset}</code></> : null}. KServe adds its Deployment, Service and route</>,
            <>No ServiceAccount, role or token is made: you call the model with your own OpenShift token</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && anythingToClean && (
        <StackItem>
          <CleanupSection id="models-cleanup" label="Clean up Models..." onClick={onCleanup} reason={baseReason}
            text="Deletes what this tool created for Models: the models in every project, then the inference gateway and its load balancer. You choose what goes, then it runs as one operation." />
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
        ...(p.project ? [{ id: "model", title: p.releasedImage ? `Switch the model in ${p.project} to the released image` : `Create the model in ${p.project}` }, { id: "model-ready", title: "Wait until the model answers" }] : []),
      ];
    case "teardown-model":
      return [{ id: "model", title: `Delete the model in ${p.project}` }];
    case "teardown-gateway":
      return [{ id: "gateway", title: "Delete the inference gateway" }];
  }
}

/** The confirmation of a Models change, or null for another feature's change. */
export function modelsConfirm(pending: Pending, status: ResourcesStatus | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-models": {
      const { gateway, project, releasedImage } = pending;
      const capacity = status?.models?.capacityWarning;
      const reason = status?.models?.imagePlanReason;
      if (releasedImage && project) {
        return {
          title: `Use the newest released Red Hat image in ${project}?`, confirm: "Use the released image", runTitle: `Use the released image in ${project}`,
          summary: <Content component="p">The model restarts on the newest released Red Hat vLLM CPU image, looked up in Red Hat&apos;s container catalog and pinned by digest. It takes 3 to 5 minutes.</Content>,
          changes: [<>LLMInferenceService <code>{MODEL_NAME}</code> in <code>{project}</code>: the main container&apos;s image is set to that image, and its version is recorded on the model. Nothing else changes.</>],
        };
      }
      const title = gateway && project ? `Set up Models in ${project}` : gateway ? "Set up the inference gateway" : `Set up the model in ${project}`;
      return {
        title: `${title}?`, confirm: "Set up", runTitle: title,
        summary: (
          <Stack hasGutter>
            {gateway && <StackItem><Content component="p">On the cluster: the inference gateway, the front door to models. It creates a cloud load balancer, which costs money while it exists.</Content></StackItem>}
            {project && <StackItem><Content component="p">In {project}: {MODEL_DISPLAY} on CPU. It needs {modelNeeds(status)} on one worker, and takes 3 to 5 minutes the first time (the runtime image and the model are downloaded).</Content></StackItem>}
          </Stack>
        ),
        changes: [
          ...(gateway ? [
            <>GatewayClass <code>{GATEWAY}</code> (OpenShift&apos;s gateway controller) is created, unless one exists.</>,
            <>In namespace <code>{GATEWAY_NS}</code>: ConfigMap <code>{GATEWAY}-options</code> and Gateway <code>{GATEWAY}</code> with two HTTPS listeners: <code>inference.&lt;apps domain&gt;</code> on the cluster&apos;s default certificate, and the gateway&apos;s in-cluster name. OpenShift adds a load balancer and a DNS record.</>,
          ] : []),
          ...(project ? [
            <>Project <code>{project}</code>: LLMInferenceService <code>{MODEL_NAME}</code> ({MODEL_DISPLAY} from Hugging Face), labelled for the dashboard and its Gen AI studio, on the CPU preset{status?.models?.preset ? <> <code>{status.models.preset}</code></> : null}. KServe adds its Deployment, Service and route.</>,
            <>Runtime image: {status?.models?.imagePlan === "released" ? "the newest released Red Hat vLLM CPU image, pinned by digest" : status?.models?.imagePlan === "override" ? "the image VLLM_CPU_IMAGE names" : "the preset's own Red Hat vLLM CPU image"}.{reason ? ` ${reason}` : ""}</>,
          ] : []),
          <>No ServiceAccount, role or token is made.</>,
        ],
        extra: project && capacity ? <Alert component="p" variant="warning" isInline title="The model may not fit on a worker">{capacity}</Alert> : undefined,
      };
    }
    case "teardown-model":
      return {
        title: `Delete the model in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete the model in ${pending.project}`,
        summary: <Content component="p">Deletes the model in {pending.project}. The project, the inference gateway and everything else in the project stay. Set up brings it back (it is downloaded again).</Content>,
        changes: [
          <>Project <code>{pending.project}</code>: LLMInferenceService <code>{MODEL_NAME}</code> is deleted; KServe then removes its Deployment, Service and route.</>,
          <>A model of that name this tool did not create is kept.</>,
        ],
      };
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
