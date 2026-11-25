import React, { useEffect, useState } from "react";
import {
  ActionGroup,
  Card,
  Checkbox,
  CardBody,
  CardHeader,
  CardTitle,
  Form,
  FormGroup,
  FormSection,
  Grid,
  GridItem,
  Radio,
  Stack,
  Switch,
  TextInput,
  Title,
} from "@patternfly/react-core";
import { fetchNightlyTags, getDSProjects, getResourcesStatus, toApiError } from "../../services/api";
import type { CatalogModel, NightlyTag } from "../../types";
import { SingleSelect } from "../SingleSelect";
import { TooltipButton } from "../TooltipButton";
import { FieldHelperText, NestedFields } from "../FormFields";
import {
  RECIPE_IMAGE_PREFIX, formProblems, listText, maasPublishReason, needsProjectReason, noProjectReason, pipelinesReason, playgroundReason, playgroundThroughMaaS,
  type DashboardDevChoice, type ExposureChoice, type RecipeForm,
} from "./recipeForm";

/** The nightly lines (GET /api/nightly-tags) and the data science projects, for the pickers. */
function useEditorChoices() {
  const [tags, setTags] = useState<NightlyTag[] | null>(null);
  const [tagsError, setTagsError] = useState<string | null>(null);
  const [projects, setProjects] = useState<string[] | null>(null);
  const [projectsError, setProjectsError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    fetchNightlyTags(controller.signal)
      .then((res) => { if (!controller.signal.aborted) setTags(res.tags ?? []); })
      .catch((e) => { if (!controller.signal.aborted) setTagsError(toApiError(e).message); });
    getDSProjects(controller.signal)
      .then((res) => { if (!controller.signal.aborted) setProjects(res.projects ?? []); })
      .catch((e) => { if (!controller.signal.aborted) setProjectsError(toApiError(e).message); });
    return () => controller.abort();
  }, []);
  return { tags, tagsError, projects, projectsError };
}

/** The model catalog (the Environment status's), read once Models is turned on. */
function useCatalog(wanted: boolean) {
  const [catalog, setCatalog] = useState<CatalogModel[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (!wanted || catalog) return;
    const controller = new AbortController();
    getResourcesStatus(null, controller.signal)
      .then((res) => { if (!controller.signal.aborted) setCatalog((res.models?.catalog ?? []).filter((m) => m.type === "chat")); })
      .catch((e) => { if (!controller.signal.aborted) setError(toApiError(e).message); });
    return () => controller.abort();
  }, [wanted, catalog]);
  return { catalog, error };
}

/** What else the recipe sets up in the project Kueue labels (the tab's own warning). */
function kueueOthers(f: RecipeForm): string[] {
  return [f.pipelines && "a pipeline server", f.models && "a model", f.models && f.playground && "a playground", f.mlflow && f.mlflowSamples && "the MLflow samples", f.workbench && "a workbench"].filter((x): x is string => !!x);
}

/** The ways the model can be reached, as the Models tab names them. */
const EXPOSURES: { value: ExposureChoice; label: string }[] = [
  { value: "", label: "The recommended way, chosen when the run gets there" },
  { value: "default-gateway", label: "Through the cluster's default gateway" },
  { value: "maas", label: "Through Models as a Service" },
  { value: "internal", label: "Only inside the cluster" },
];

/** A feature's switch with its helper text: the reason when another choice rules it out (it shows off), else what it does. */
const FeatureSwitch: React.FC<{
  id: string;
  label: string;
  checked: boolean;
  /** Why it cannot be on, or null. */
  reason?: string | null;
  onChange: (on: boolean) => void;
  help: React.ReactNode;
  children?: React.ReactNode;
}> = ({ id, label, checked, reason, onChange, help, children }) => (
  <FormGroup fieldId={id}>
    <Switch id={id} label={label} isChecked={checked && !reason} isDisabled={!!reason} onChange={(_e, v) => onChange(v)} aria-describedby={`${id}-helper`} />
    <FieldHelperText id={`${id}-helper`}>{reason ?? help}</FieldHelperText>
    {checked && !reason && children && <NestedFields><Stack hasGutter style={{ marginBlockStart: "var(--pf-t--global--spacer--sm)" }}>{children}</Stack></NestedFields>}
  </FormGroup>
);

/** A Card with an h3 title, one per layer of the recipe. */
const LayerCard: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => (
  <Card isFullHeight>
    <CardHeader><CardTitle><Title headingLevel="h3" size="md">{title}</Title></CardTitle></CardHeader>
    <CardBody>{children}</CardBody>
  </Card>
);

/**
 * The recipe editor: one card per layer (RHOAI build, DataScienceCluster,
 * project, features), each choice with what it does, a choice another one
 * rules out disabled with the reason, and Review and run. The runner fixes
 * the order; the user only says what they want.
 */
export const RecipeEditor: React.FC<{
  form: RecipeForm;
  onChange: (form: RecipeForm) => void;
  onReview: () => void;
  /** Why no recipe can run now (permissions, another operation), or null. */
  blocker: string | null;
  reviewing: boolean;
}> = ({ form, onChange, onReview, blocker, reviewing }) => {
  const { tags, tagsError, projects, projectsError } = useEditorChoices();
  // Errors show once the user tried to go on, or touched the field.
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [submitted, setSubmitted] = useState(false);
  const problems = formProblems(form);
  const shown = (field: keyof typeof problems) => (submitted || touched.has(field) ? problems[field] : undefined);
  const touch = (field: string) => setTouched((t) => new Set(t).add(field));
  const set = <K extends keyof RecipeForm>(key: K, value: RecipeForm[K]) => onChange({ ...form, [key]: value });
  const invalid = Object.keys(problems).length > 0;
  const lines = [...new Set((tags ?? []).map((t) => t.tag).filter(Boolean))];
  const noProject = noProjectReason(form);
  const noPipelines = pipelinesReason(form);
  const { catalog, error: catalogError } = useCatalog(form.models);
  const defaultModel = catalog?.find((m) => m.default);
  const modelText = form.model ? catalog?.find((m) => m.name === form.model)?.displayName ?? form.model
    : defaultModel ? `${defaultModel.displayName} (the default)` : catalog === null && !catalogError ? "Loading the models" : "The catalog's default";

  const review = () => {
    setSubmitted(true);
    if (!invalid) onReview();
  };

  return (
    <Form onSubmit={(e) => { e.preventDefault(); review(); }} aria-label="Recipe">
      <FormGroup label="Recipe name" fieldId="recipe-name" isRequired style={{ maxWidth: "32rem" }}>
        <TextInput id="recipe-name" value={form.name} onChange={(_e, v) => set("name", v)} onBlur={() => touch("name")}
          validated={shown("name") ? "error" : "default"} aria-describedby="recipe-name-helper" />
        <FieldHelperText id="recipe-name-helper" variant={shown("name") ? "error" : "default"}>
          {shown("name") ?? "Teammates see it in the notice while the recipe runs."}
        </FieldHelperText>
      </FormGroup>
      <Grid hasGutter>
        <GridItem xl={6}>
          <LayerCard title="RHOAI">
            <FormSection>
              <FormGroup role="radiogroup" isStack fieldId="recipe-build" label="Build">
                <Radio id="recipe-build-latest" name="recipe-build" label="The newest nightly" isChecked={form.build === "latest"}
                  onChange={() => set("build", "latest")} description="Looked up when the run starts, and kept for the whole run." />
                <Radio id="recipe-build-tag" name="recipe-build" label="The newest nightly of a line" isChecked={form.build === "tag"}
                  onChange={() => set("build", "tag")}
                  body={form.build === "tag" && (
                    <NestedFields>
                      <SingleSelect id="recipe-tag" label="Nightly line" selected={form.tag || undefined}
                        text={form.tag || (tags === null && !tagsError ? "Loading the nightly lines" : "Choose a nightly line")}
                        isPlaceholder={!form.tag} invalid={!!shown("tag")} isDisabled={tags === null || lines.length === 0}
                        options={lines.map((l) => ({ value: l, label: l }))} onSelect={(v) => set("tag", v)} />
                      {(shown("tag") || tagsError || (tags && lines.length === 0)) && (
                        <FieldHelperText variant={shown("tag") ? "error" : "warning"}>
                          {tagsError ? `The nightly lines could not be read: ${tagsError}` : tags && lines.length === 0 ? "No nightly lines found." : shown("tag")}
                        </FieldHelperText>
                      )}
                    </NestedFields>
                  )} />
                <Radio id="recipe-build-image" name="recipe-build" label="One catalog image" isChecked={form.build === "image"}
                  onChange={() => set("build", "image")}
                  body={form.build === "image" && (
                    <NestedFields>
                      {/* eslint-disable-next-line no-restricted-syntax -- omitted text: the dots stand for the digest */}
                      <TextInput id="recipe-image" aria-label="Catalog image" value={form.image} placeholder={`${RECIPE_IMAGE_PREFIX}@sha256:...`}
                        onChange={(_e, v) => set("image", v)} onBlur={() => touch("image")} validated={shown("image") ? "error" : "default"} />
                      <FieldHelperText variant={shown("image") ? "error" : "default"}>
                        {shown("image") ?? "A build of the RHOAI catalog, pinned by digest, as Build Explorer shows it."}
                      </FieldHelperText>
                    </NestedFields>
                  )} />
              </FormGroup>
              <FormGroup role="radiogroup" isStack fieldId="recipe-if-installed" label="If RHOAI is installed">
                <Radio id="recipe-keep" name="recipe-if-installed" label="Keep it" isChecked={form.ifInstalled === "keep"}
                  onChange={() => set("ifInstalled", "keep")} description="The installed operator stays as it is." />
                <Radio id="recipe-update" name="recipe-if-installed" label="Update it to this build" isChecked={form.ifInstalled === "update"}
                  onChange={() => set("ifInstalled", "update")} description="As Update on the Status page does." />
              </FormGroup>
            </FormSection>
          </LayerCard>
        </GridItem>
        <GridItem xl={6}>
          <LayerCard title="DataScienceCluster">
            <FormGroup fieldId="recipe-dsc">
              <Switch id="recipe-dsc" label="Create the default DataScienceCluster when there is none" isChecked={form.createDSC}
                onChange={(_e, v) => set("createDSC", v)} aria-describedby="recipe-dsc-helper" />
              <FieldHelperText id="recipe-dsc-helper">
                {form.createDSC
                  ? "With the installed release's default components. Change them later on the Components page."
                  : "The run uses the DataScienceCluster the cluster has, and stops at this step when there is none."}
              </FieldHelperText>
            </FormGroup>
          </LayerCard>
        </GridItem>
        <GridItem xl={6}>
          <LayerCard title="Project">
            <FormGroup role="radiogroup" isStack fieldId="recipe-project" aria-label="Project">
              <Radio id="recipe-project-create" name="recipe-project" label="Create a project" isChecked={form.project === "create"}
                onChange={() => set("project", "create")} description="A data science project the recipe owns: Clean up deletes it."
                body={form.project === "create" && (
                  <NestedFields>
                    <Stack hasGutter>
                    <FormGroup label="Name" fieldId="recipe-project-name" isRequired>
                      <TextInput id="recipe-project-name" value={form.newProject} onChange={(_e, v) => set("newProject", v)} onBlur={() => touch("newProject")}
                        validated={shown("newProject") ? "error" : "default"} aria-describedby="recipe-project-name-helper" />
                      <FieldHelperText id="recipe-project-name-helper" variant={shown("newProject") ? "error" : "default"}>
                        {shown("newProject") ?? "Not a name that exists: the run refuses a project a recipe did not create."}
                      </FieldHelperText>
                    </FormGroup>
                    <FormGroup label="Display name" fieldId="recipe-project-display">
                      <TextInput id="recipe-project-display" value={form.displayName} onChange={(_e, v) => set("displayName", v)} />
                    </FormGroup>
                    </Stack>
                  </NestedFields>
                )} />
              <Radio id="recipe-project-existing" name="recipe-project" label="Use one of my data science projects" isChecked={form.project === "existing"}
                onChange={() => set("project", "existing")} description="The recipe sets up in it, and never deletes it."
                body={form.project === "existing" && (
                  <NestedFields>
                    <SingleSelect id="recipe-existing-project" label="Project" selected={form.existingProject || undefined}
                      text={form.existingProject || (projects === null && !projectsError ? "Loading the projects" : "Choose a project")}
                      isPlaceholder={!form.existingProject} invalid={!!shown("existingProject")} isDisabled={!projects || projects.length === 0}
                      options={(projects ?? []).map((p) => ({ value: p, label: p }))} onSelect={(v) => set("existingProject", v)} />
                    {(shown("existingProject") || projectsError || (projects && projects.length === 0)) && (
                      <FieldHelperText variant={shown("existingProject") ? "error" : "warning"}>
                        {projectsError ? `The projects could not be read: ${projectsError}` : projects && projects.length === 0 ? "There is no data science project yet: create one instead." : shown("existingProject")}
                      </FieldHelperText>
                    )}
                  </NestedFields>
                )} />
              <Radio id="recipe-project-none" name="recipe-project" label="No project" isChecked={form.project === "none"}
                isDisabled={!!noProject} onChange={() => set("project", "none")}
                description={noProject ?? "For RHOAI, the DataScienceCluster and MLflow only."} />
            </FormGroup>
          </LayerCard>
        </GridItem>
        <GridItem xl={6}>
          <LayerCard title="Dashboard Dev">
            <FormGroup role="radiogroup" isStack fieldId="recipe-dashboard-dev" aria-label="Dashboard Dev">
              {([["none", "Keep the installed dashboard", null], ["main", "The newest main build", "The odh-dashboard main branch, as Deploy main on the Dashboard Dev page."],
                ["pr", "A pull request build", "As Deploy on the Dashboard Dev page."]] as [DashboardDevChoice, string, string | null][]).map(([value, label, description]) => (
                <Radio key={value} id={`recipe-dashboard-deploy-${value}`} name="recipe-dashboard-dev" label={label} isChecked={form.dashboardDev === value}
                  onChange={() => set("dashboardDev", value)} description={description ?? undefined}
                  body={value === "pr" && form.dashboardDev === "pr" && (
                    <NestedFields>
                      <FormGroup label="Pull request number" fieldId="recipe-dashboard-pr" isRequired>
                        <TextInput id="recipe-dashboard-pr" inputMode="numeric" value={form.dashboardPR} onChange={(_e, v) => set("dashboardPR", v)}
                          onBlur={() => touch("dashboardPR")} validated={shown("dashboardPR") ? "error" : "default"} aria-describedby="recipe-dashboard-pr-helper" />
                        <FieldHelperText id="recipe-dashboard-pr-helper" variant={shown("dashboardPR") ? "error" : "default"}>
                          {shown("dashboardPR") ?? "The run checks that its image is published before it starts."}
                        </FieldHelperText>
                      </FormGroup>
                    </NestedFields>
                  )} />
              ))}
              {form.dashboardDev !== "none" && (
                <FieldHelperText>
                  Deployed last: it pauses the dashboard's operator, so RHOAI updates wait until you revert it on the Dashboard Dev page.
                </FieldHelperText>
              )}
            </FormGroup>
          </LayerCard>
        </GridItem>
        <GridItem xl={12}>
          <LayerCard title="Features">
            <Grid hasGutter>
              <GridItem lg={6}>
                <FormSection>
                  <FormGroup fieldId="recipe-pipelines">
                    <Switch id="recipe-pipelines" label="Pipelines" isChecked={form.pipelines} isDisabled={!!noPipelines && !form.pipelines}
                      onChange={(_e, v) => set("pipelines", v)} aria-describedby="recipe-pipelines-helper" />
                    <FieldHelperText id="recipe-pipelines-helper">
                      {noPipelines && !form.pipelines ? noPipelines : "S3 storage and a pipeline server in the project. Turns on the pipelines component of the DataScienceCluster if it is off."}
                    </FieldHelperText>
                  </FormGroup>
                  <FeatureSwitch id="recipe-mlflow" label="MLflow" checked={form.mlflow} onChange={(v) => set("mlflow", v)}
                    help="An MLflow server for the cluster. Turns on its DataScienceCluster component if it is off.">
                    <Checkbox id="recipe-mlflow-samples" label="Add the MLflow samples to the project" isChecked={form.mlflowSamples && form.project !== "none"}
                      isDisabled={form.project === "none"} onChange={(_e, v) => set("mlflowSamples", v)}
                      description={needsProjectReason(form, "The samples need") ?? "As Add all on the MLflow tab."} />
                  </FeatureSwitch>
                  <FeatureSwitch id="recipe-workbench" label="Workbench" checked={form.workbench} reason={needsProjectReason(form, "A workbench needs")}
                    onChange={(v) => set("workbench", v)}
                    help={form.pipelines
                      ? "A workbench in the project, as the dashboard creates one. It gets the S3 Connection of the pipeline server."
                      : "A workbench in the project, as the dashboard creates one. Without Pipelines it has no S3 Connection: turn Pipelines on to attach one."} />
                  <FeatureSwitch id="recipe-model-registry" label="Model registry" checked={form.modelRegistry} onChange={(v) => set("modelRegistry", v)}
                    help="A model registry for the cluster, with its default database. Turns on its DataScienceCluster component if it is off.">
                    <Checkbox id="recipe-registry-sample" label="Register the sample model" isChecked={form.registrySample} onChange={(_e, v) => set("registrySample", v)}
                      description="Registered by the Model registry step, as the Model registry tab does." />
                  </FeatureSwitch>
                  <FeatureSwitch id="recipe-kueue" label="Distributed workloads" checked={form.kueue} reason={needsProjectReason(form, "Distributed workloads need")}
                    onChange={(v) => set("kueue", v)}
                    help="Kueue queues for the project, as the Distributed workloads tab sets them up: a ClusterQueue and ResourceFlavor (cluster-wide objects; only this project uses them) and its LocalQueue. First adds what they need: Kueue in the DataScienceCluster, cert-manager and the Kueue operator (not Ray: see the samples). Set up after the project's other parts.">
                    {kueueOthers(form).length > 0 && (
                      <FieldHelperText>
                        {`The project also gets ${listText(kueueOthers(form))}. Kueue manages only what is sent to a queue, so ${kueueOthers(form).length === 1 ? "it keeps" : "they keep"} running; a project of its own is still safer.`}
                      </FieldHelperText>
                    )}
                    <Checkbox id="recipe-kueue-raycluster" label="RayCluster sample" isChecked={form.kueueRayCluster} onChange={(_e, v) => set("kueueRayCluster", v)}
                      description="Added by the Distributed workloads step. Shows on the dashboard's Workload metrics page (Observe & monitor)." />
                    <Checkbox id="recipe-kueue-rayjob" label="RayJob sample" isChecked={form.kueueRayJob} onChange={(_e, v) => set("kueueRayJob", v)}
                      description="Added by the Distributed workloads step. Runs once, then finishes." />
                    {(form.kueueRayCluster || form.kueueRayJob) && (
                      <FieldHelperText>
                        The samples need the DataScienceCluster's ray component on. The recipe does not turn it on: if it is off, the step stops and says so (turn it on on the Components page).
                      </FieldHelperText>
                    )}
                    {(form.kueueRayCluster || form.kueueRayJob) && (
                      <FormGroup label="Sample image" fieldId="recipe-kueue-image">
                        <TextInput id="recipe-kueue-image" value={form.kueueRayImage} placeholder="The tab's default Ray image" onChange={(_e, v) => set("kueueRayImage", v)}
                          onBlur={() => touch("kueueRayImage")} validated={shown("kueueRayImage") ? "error" : "default"} aria-describedby="recipe-kueue-image-helper" />
                        <FieldHelperText id="recipe-kueue-image-helper" variant={shown("kueueRayImage") ? "error" : "default"}>
                          {shown("kueueRayImage") ?? "Empty uses the Distributed workloads tab's default image."}
                        </FieldHelperText>
                      </FormGroup>
                    )}
                    <Checkbox id="recipe-kueue-optin" label="Turn on the dashboard's Kueue setting" isChecked={form.kueueOptIn} onChange={(_e, v) => set("kueueOptIn", v)}
                      description="Off by default. It changes every new project on this cluster: workbenches and model deployments there can only use the queue hardware profile, and a project without its own LocalQueue cannot create them until an admin adds one." />
                  </FeatureSwitch>
                </FormSection>
              </GridItem>
              <GridItem lg={6}>
                <FormSection>
                  <FeatureSwitch id="recipe-models" label="Models" checked={form.models} reason={needsProjectReason(form, "Models needs")} onChange={(v) => set("models", v)}
                    help="A model of the catalog in the project, served on a CPU worker. First adds what Models needs: KServe and cert-manager, and Connectivity Link for a gateway. Usually about 9 minutes in all, up to 30.">
                    <FormGroup label="Model" fieldId="recipe-model">
                      <SingleSelect id="recipe-model" label="Model" selected={form.model || defaultModel?.name} text={modelText} isDisabled={!catalog || catalog.length === 0}
                        options={(catalog ?? []).map((m) => ({ value: m.name, label: m.displayName }))} onSelect={(v) => set("model", v === defaultModel?.name ? "" : v)} />
                      {catalogError && <FieldHelperText variant="warning">{`The model catalog could not be read: ${catalogError}. The run sets up the catalog's default.`}</FieldHelperText>}
                    </FormGroup>
                    <FormGroup label="How it is reached" fieldId="recipe-exposure">
                      <SingleSelect id="recipe-exposure" label="How it is reached" selected={form.exposure === "maas" && !form.maas ? "" : form.exposure}
                        text={EXPOSURES.find((e) => e.value === (form.exposure === "maas" && !form.maas ? "" : form.exposure))!.label}
                        options={EXPOSURES.filter((e) => e.value !== "maas" || form.maas)} onSelect={(v) => set("exposure", v as ExposureChoice)} />
                      <FieldHelperText>{form.maas ? "As the Models tab offers it." : "Through Models as a Service needs MaaS: turn it on below."}</FieldHelperText>
                    </FormGroup>
                  </FeatureSwitch>
                  <FeatureSwitch id="recipe-playground" label="Playground" checked={form.playground} reason={playgroundReason(form)} onChange={(v) => set("playground", v)}
                    help={playgroundThroughMaaS(form)
                      ? "Needs Models. The Gen AI playground in the project. The model is offered only through Models as a Service, so the playground reaches it there: the dashboard sets the playground up with your sign-in, as when you choose a Models as a Service model in its Gen AI studio. Turns on what it needs."
                      : "Needs Models. The Gen AI playground in the project, with its ready chat models. Turns on what it needs."} />
                  <FeatureSwitch id="recipe-maas" label="Models as a Service" checked={form.maas} onChange={(v) => set("maas", v)}
                    help="The MaaS platform for the cluster: Connectivity Link, its database and the MaaS gateway. Usually 10 minutes, up to 15.">
                    <FormGroup role="radiogroup" isStack fieldId="recipe-maas-database" label="Database">
                      <Radio id="recipe-maas-development" name="recipe-maas-database" label="Development" isChecked={form.maasDatabase === "development"}
                        onChange={() => set("maasDatabase", "development")} description="One pod without a volume: API keys are lost when it restarts." />
                      <Radio id="recipe-maas-cnpg" name="recipe-maas-database" label="CloudNativePG" isChecked={form.maasDatabase === "cnpg"}
                        onChange={() => set("maasDatabase", "cnpg")} description="Installs the CloudNativePG operator; the database keeps its data." />
                    </FormGroup>
                    <Checkbox id="recipe-maas-publish" label="Publish the model through MaaS" isChecked={form.maasPublish && !maasPublishReason(form)}
                      isDisabled={!!maasPublishReason(form)} onChange={(_e, v) => set("maasPublish", v)}
                      description={maasPublishReason(form) ?? "Needs Models. As Publish on the MaaS tab."} />
                  </FeatureSwitch>
                </FormSection>
              </GridItem>
            </Grid>
          </LayerCard>
        </GridItem>
      </Grid>
      <ActionGroup>
        <TooltipButton variant="primary" type="submit" isLoading={reviewing} isDisabled={reviewing} disabledReason={blocker}>
          Review and run
        </TooltipButton>
      </ActionGroup>
    </Form>
  );
};
