import { Alert, Content, Stack, StackItem } from "@patternfly/react-core";
import type { ModelExposureKind, ResourcesStatus } from "../../types";
import { type ConfirmSpec, type ModelsPending, type Pending } from "./resourceRow";
import { catalogModel, modelTypeWords, shortModelName } from "./featureState";
import { GATEWAY, GATEWAY_NS } from "./GatewayPart";
import { modelNeeds } from "./ModelPicker";
import { tokenSA } from "./modelCommands";

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
