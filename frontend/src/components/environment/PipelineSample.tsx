import React from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Content,
  ExpandableSection,
  List,
  ListComponent,
  ListItem,
  OrderType,
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
import { LinesSkeleton } from "./Loading";

/** The id of the section's heading: the Overview's "Run a sample pipeline" moves the focus there. */
export const PIPELINE_SAMPLE_ID = "pipelines-sample";

/** The ServiceAccount, Role and RoleBinding the commands make for the pipeline API, in the user's project. */
const PIPELINE_API = "pipeline-api";

/**
 * The commands that put in $TOKEN a token that may only use one pipeline
 * server's API, and expires in 15 minutes. Never the user's own token: the
 * server's Route and Service are in a project its editors control, who can
 * make a pod receive what is sent there (SECURITY.md, "Which token is used
 * where"). The Role holds exactly what the server's kube-rbac-proxy checks
 * (data-science-pipelines-operator rhoai-3.6
 * config/internal/apiserver/default/kube-rbac-proxy-config.yaml.tmpl:
 * datasciencepipelinesapplications/api of that DSPA), with the verbs the
 * commands' calls map to (GET get, POST create; kube-rbac-proxy
 * pkg/proxy/proxy.go). Project and DSPA names are Kubernetes names (no
 * shell quoting).
 */
export function pipelineTokenCommands(project: string, dspa: string): string {
  return [
    `# Once per project: a ServiceAccount that may only use the API of pipeline server ${dspa}`,
    `oc create serviceaccount ${PIPELINE_API} -n ${project}`,
    `oc create role ${PIPELINE_API} -n ${project} --verb=get,create \\`,
    `  --resource=datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io/api --resource-name=${dspa}`,
    `oc create rolebinding ${PIPELINE_API} -n ${project} --role=${PIPELINE_API} --serviceaccount=${project}:${PIPELINE_API}`,
    "# A token for it that expires in 15 minutes",
    `export TOKEN=$(oc create token ${PIPELINE_API} -n ${project} --duration=15m)`,
  ].join("\n");
}

/** The command that deletes what pipelineTokenCommands makes. */
export const pipelineTokenRemoval = (project: string) =>
  `oc delete rolebinding/${PIPELINE_API} role/${PIPELINE_API} serviceaccount/${PIPELINE_API} -n ${project}`;

/**
 * The commands that upload the sample pipeline and start a run through the
 * pipeline server's API (KFP v2beta1), with a short-lived token that may
 * only use that API (pipelineTokenCommands). They need curl and jq.
 */
export function samplePipelineCommands(apiUrl: string, project: string, dspa: string, spec: Pick<PipelineSampleSpec, "name" | "fileName">): string {
  const api = apiUrl.replace(/\/+$/, "");
  const auth = '-H "Authorization: Bearer $TOKEN"';
  return [
    pipelineTokenCommands(project, dspa),
    `API=${shellQuote(api)}`,
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
  /** The pipeline server's (DSPA's) name: the commands' token may only use its API. */
  dspa?: string;
  /** The pipeline server's API address (DSPA status.components.apiServer.externalUrl). */
  apiRoute?: string;
  /** The project's pipelines in the RHOAI dashboard. */
  uiRoute?: string;
}

/**
 * "Run a sample pipeline": a two-step pipeline (write a file to the S3
 * storage, read it back) to download, import in the dashboard or upload
 * and run with copyable commands. The user runs it with a short-lived
 * token that may only use that server's API; this tool never calls the
 * pipeline server's API (SECURITY.md).
 */
export const PipelineSample: React.FC<PipelineSampleProps> = ({ project, dspa, apiRoute, uiRoute }) => {
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
        You run it yourself, in the dashboard or with the commands below; this tool does not run it for you.
        They do not send your OpenShift token to the pipeline server, because whoever can edit {project} controls what answers at its address: they make a ServiceAccount that may only use this server&apos;s API, and a token for it that expires in 15 minutes.
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
      : <LinesSkeleton what="the sample pipeline" lines={3} />);
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
      {apiRoute && dspa && (
        <ListItem>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                Or from a terminal where you are logged in with <code>oc login</code> as an admin of {project}: these commands make the ServiceAccount and its token, upload the pipeline and start a run. They need curl and jq. Run again, the first three say the ServiceAccount, Role and RoleBinding already exist, which is fine. A second upload of the same name fails; run only the last command for another run.
              </Content>
            </StackItem>
            <StackItem>
              <CopyableCode id="pipelines-sample-commands" label="the commands" code={samplePipelineCommands(apiRoute, project, dspa, spec)} />
            </StackItem>
            <StackItem>
              <Content component="p">To remove the ServiceAccount, Role and RoleBinding afterwards: <code>{pipelineTokenRemoval(project)}</code></Content>
            </StackItem>
          </Stack>
        </ListItem>
      )}
    </List>,
  );
};
