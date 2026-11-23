import React, { useEffect, useState } from "react";
import {
  Alert,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { ModelExposureKind, ResourcesStatus } from "../../types";
import { type Pending, isTerminating } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartList, PrimaryAction } from "./FeatureLayout";
import { OutsideProjectsAlert } from "./OutsideProjects";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, NoProjectsAlert } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, catalogModel, modelIn } from "./featureState";
import { GATEWAY, GATEWAY_NS, GatewayPart, gatewayManaged } from "./GatewayPart";
import { ModelPart } from "./ModelPart";
import { ModelPicker, modelOptionWords } from "./ModelPicker";
import { ExposureChooser, useModelExposures, withExposure } from "./ExposureChooser";
import { UseModel } from "./UseModel";
import { tokenSA } from "./modelCommands";

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
  const outside = models?.outside ?? [];
  const anythingToClean = toolModels.length > 0 || outside.length > 0 || gatewayManaged(status.inferenceGateway);
  // How a new model is reached: read when it is not set up yet, again when
  // the gateways change; the recommended one is chosen until the user
  // chooses.
  const gw = status.inferenceGateway;
  const choosing = !!project && !model?.deployed && next?.kind === "set-up";
  const exposuresRead = useModelExposures(project, name, `${gw?.deployed}|${gw?.ready}|${(gw?.allowedProjects ?? []).join(",")}`, choosing);
  // Another project or model starts from the recommended way again, in the
  // same render: an effect would reset it one render later, after a choice
  // made in between.
  const [choice, setChoice] = useState<string | null>(null);
  const choiceFor = `${project}/${name}`;
  const [choiceMadeFor, setChoiceMadeFor] = useState(choiceFor);
  if (choiceMadeFor !== choiceFor) {
    setChoiceMadeFor(choiceFor);
    setChoice(null);
  }
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
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} things="Models" /></StackItem>}
      {outside.length > 0 && (
        <StackItem>
          <OutsideProjectsAlert readOnly={readOnly} reason={baseReason} items={outside.map((m) => ({
            key: `${m.namespace}/${m.name}`,
            text: <>Model <code>{m.name}</code> in project {m.namespace} (no longer a data science project)</>,
            onDelete: () => props.setPending({ kind: "teardown-model", project: m.namespace!, model: m.name!, ...(m.maasOnlySetup ? { unpublish: true } : {}) }),
          }))} />
        </StackItem>
      )}
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
