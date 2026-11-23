import React, { useEffect, useState } from "react";
import {
  ActionGroup,
  Card,
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
import { fetchNightlyTags, getDSProjects, toApiError } from "../../services/api";
import type { NightlyTag } from "../../types";
import { SingleSelect } from "../SingleSelect";
import { TooltipButton } from "../TooltipButton";
import { FieldHelperText, NestedFields } from "../FormFields";
import { RECIPE_IMAGE_PREFIX, formProblems, noProjectReason, pipelinesReason, type RecipeForm } from "./recipeForm";

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
                        text={form.tag || (tags === null && !tagsError ? "Loading the nightly lines..." : "Choose a nightly line")}
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
                      text={form.existingProject || (projects === null && !projectsError ? "Loading the projects..." : "Choose a project")}
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
          <LayerCard title="Features">
            <FormSection>
              <FormGroup fieldId="recipe-pipelines">
                <Switch id="recipe-pipelines" label="Pipelines" isChecked={form.pipelines} isDisabled={!!noPipelines && !form.pipelines}
                  onChange={(_e, v) => set("pipelines", v)} aria-describedby="recipe-pipelines-helper" />
                <FieldHelperText id="recipe-pipelines-helper">
                  {noPipelines && !form.pipelines ? noPipelines : "S3 storage and a pipeline server in the project. Turns on the pipelines component of the DataScienceCluster if it is off."}
                </FieldHelperText>
              </FormGroup>
              <FormGroup fieldId="recipe-mlflow">
                <Switch id="recipe-mlflow" label="MLflow" isChecked={form.mlflow} onChange={(_e, v) => set("mlflow", v)} aria-describedby="recipe-mlflow-helper" />
                <FieldHelperText id="recipe-mlflow-helper">
                  An MLflow server for the cluster. Turns on its DataScienceCluster component if it is off.
                </FieldHelperText>
              </FormGroup>
              <FieldHelperText>
                Models, the playground, Models as a Service and Dashboard Dev come to recipes in a later version. Set them up on the
                Environment and Dashboard Dev pages after the run.
              </FieldHelperText>
            </FormSection>
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
