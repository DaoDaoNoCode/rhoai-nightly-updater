import React from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Content,
  ExpandableSection,
  Flex,
  FlexItem,
  List,
  ListComponent,
  ListItem,
  OrderType,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import DownloadIcon from "@patternfly/react-icons/dist/esm/icons/download-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { PipelineSampleSpec } from "../../types";
import { getPipelineSampleSpec } from "../../services/api";
import { ImageRef } from "../ImageRef";
import { TabSection } from "./FeatureLayout";
import { CopyableCode, shellQuote } from "./UseIt";

/** The id of the section's heading: the Overview's "Run a sample pipeline" moves the focus there. */
export const PIPELINE_SAMPLE_ID = "pipelines-sample";

/**
 * The commands that upload the sample pipeline and start a run through the
 * pipeline server's API (KFP v2beta1), with the user's own OpenShift token.
 * They need curl and jq.
 */
export function samplePipelineCommands(apiUrl: string, project: string, spec: Pick<PipelineSampleSpec, "name" | "fileName">): string {
  const api = apiUrl.replace(/\/+$/, "");
  const auth = '-H "Authorization: Bearer $TOKEN"';
  return [
    `API=${shellQuote(api)}`,
    "TOKEN=$(oc whoami -t)",
    `# Upload ${spec.fileName} (run this in the folder you saved it to)`,
    `PIPELINE_ID=$(curl -fsS ${auth} -F "uploadfile=@${spec.fileName}" \\`,
    `  "$API/apis/v2beta1/pipelines/upload?name=${spec.name}&namespace=${project}" | jq -r .pipeline_id)`,
    `VERSION_ID=$(curl -fsS ${auth} \\`,
    `  "$API/apis/v2beta1/pipelines/$PIPELINE_ID/versions?page_size=1&sort_by=created_at%20desc" | jq -r '.pipeline_versions[0].pipeline_version_id')`,
    "# Start a run; run this again for another run",
    `curl -fsS ${auth} -H "Content-Type: application/json" "$API/apis/v2beta1/runs" \\`,
    `  -d "{\\"display_name\\":\\"Sample: write and read an artifact\\",\\"pipeline_version_reference\\":{\\"pipeline_id\\":\\"$PIPELINE_ID\\",\\"pipeline_version_id\\":\\"$VERSION_ID\\"}}" | jq -r .run_id`,
  ].join("\n");
}

/** The sample pipeline file as a link target the browser saves. */
export const pipelineFileHref = (yaml: string) => `data:application/yaml;charset=utf-8,${encodeURIComponent(yaml)}`;

export interface PipelineSampleProps {
  project: string;
  /** The pipeline server's API address (DSPA status.components.apiServer.externalUrl). */
  apiRoute?: string;
  /** The project's pipelines in the RHOAI dashboard. */
  uiRoute?: string;
}

/**
 * "Run a sample pipeline": a two-step pipeline (write a file to the S3
 * storage, read it back) to download, import in the dashboard or upload
 * and run with copyable commands. The user runs it with their own token;
 * this tool never calls the pipeline server's API (SECURITY.md).
 */
export const PipelineSample: React.FC<PipelineSampleProps> = ({ project, apiRoute, uiRoute }) => {
  const [spec, setSpec] = React.useState<PipelineSampleSpec | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [attempt, setAttempt] = React.useState(0);
  const [yamlShown, setYamlShown] = React.useState(false);
  React.useEffect(() => {
    let live = true;
    setError(null);
    getPipelineSampleSpec().then(
      (s) => { if (live) setSpec(s); },
      (e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)); },
    );
    return () => { live = false; };
  }, [attempt]);

  const section = (body: React.ReactNode) => (
    <TabSection title="Run a sample pipeline" id={PIPELINE_SAMPLE_ID}>
      <Content component="p">
        A two-step pipeline that checks the pipeline server works end to end: the first step writes a small file to the S3 storage, the second reads it back. It usually finishes in 1 to 2 minutes.
        You run it yourself, in the dashboard or with the commands below. They use your own OpenShift token, so you need access to {project}; this tool does not run it for you.
      </Content>
      {body}
    </TabSection>
  );
  if (!spec) {
    return section(error
      ? (
        <Alert variant="warning" isInline component="div" title="Cannot load the sample pipeline"
          actionLinks={<AlertActionLink onClick={() => setAttempt((n) => n + 1)}>Try again</AlertActionLink>}>
          <Content component="p">{error}</Content>
        </Alert>
      )
      : (
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><Spinner size="md" aria-hidden /></FlexItem>
          <FlexItem><Content component="p">Loading the sample pipeline...</Content></FlexItem>
        </Flex>
      ));
  }
  return section(
    <List component={ListComponent.ol} type={OrderType.number} aria-label={`Run a sample pipeline in ${project}`}>
      <ListItem>
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              Download the pipeline. Both steps run <ImageRef image={spec.image} />; on a disconnected cluster, mirror that image or set SAMPLE_PIPELINE_IMAGE on the updater to a copy the cluster can pull.
            </Content>
          </StackItem>
          <StackItem>
            <Button variant="secondary" component="a" href={pipelineFileHref(spec.yaml)} download={spec.fileName} icon={<DownloadIcon />}>
              Download {spec.fileName}
            </Button>
          </StackItem>
          <StackItem>
            <ExpandableSection toggleText={yamlShown ? "Hide the pipeline YAML" : "Show the pipeline YAML"} isExpanded={yamlShown} onToggle={(_e, open) => setYamlShown(open)}>
              <CopyableCode id="pipelines-sample-yaml" label="the pipeline YAML" code={spec.yaml} />
            </ExpandableSection>
          </StackItem>
        </Stack>
      </ListItem>
      <ListItem>
        <Stack hasGutter>
          <StackItem>
            <Content component="p">
              In the dashboard: open pipelines in {project}, choose Import pipeline and the file, then Create run.
            </Content>
          </StackItem>
          {uiRoute && (
            <StackItem>
              <Button variant="link" isInline component="a" href={uiRoute} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                Open pipelines in {project}
              </Button>
            </StackItem>
          )}
        </Stack>
      </ListItem>
      {apiRoute && (
        <ListItem>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                Or from a terminal where you are logged in with <code>oc login</code>: these commands upload the pipeline and start a run with your own token. They need curl and jq. A second upload of the same name fails; run only the last command for another run.
              </Content>
            </StackItem>
            <StackItem>
              <CopyableCode id="pipelines-sample-commands" label="the commands" code={samplePipelineCommands(apiRoute, project, spec)} />
            </StackItem>
          </Stack>
        </ListItem>
      )}
    </List>,
  );
};
