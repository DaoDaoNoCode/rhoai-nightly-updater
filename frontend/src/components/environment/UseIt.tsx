import React from "react";
import {
  Alert,
  AlertActionLink,
  ClipboardCopyButton,
  CodeBlock,
  CodeBlockAction,
  CodeBlockCode,
  Content,
} from "@patternfly/react-core";
import { useNavigate } from "react-router-dom";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { ComponentBlock } from "../../types";
import { TechnicalDetails } from "./resourceRow";

/** A few lines to copy (environment variables, a Python snippet), PatternFly's code block with a copy button. */
export const CopyableCode: React.FC<{ id: string; code: string; label: string }> = ({ id, code, label }) => {
  const [copied, setCopied] = React.useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };
  return (
    <CodeBlock actions={
      <CodeBlockAction>
        <ClipboardCopyButton id={`${id}-copy`} textId={`${id}-code`} aria-label={`Copy ${label}`} onClick={() => void copy()} exitDelay={copied ? 1500 : 600}
          variant="plain" onTooltipHidden={() => setCopied(false)}>
          {copied ? "Copied" : `Copy ${label}`}
        </ClipboardCopyButton>
      </CodeBlockAction>
    }>
      <CodeBlockCode id={`${id}-code`}>{code}</CodeBlockCode>
    </CodeBlock>
  );
};

/**
 * A feature that cannot be set up because the DataScienceCluster does not
 * turn on its component: shown before Set up, with the way to the fix.
 */
export const ComponentAlert: React.FC<{ feature: string; block: ComponentBlock }> = ({ feature, block }) => {
  const navigate = useNavigate();
  const title = block.dsc
    ? `${feature} is turned off on this cluster`
    : `${feature} is not available on this cluster`;
  const rows = [
    ...(block.dsc ? [{ term: "DataScienceCluster", value: block.dsc }] : []),
    { term: "Component", value: block.component },
    ...(block.dsc ? [{ term: "managementState", value: block.state || "not set" }] : []),
  ];
  return (
    <Alert variant="warning" isInline component="div" title={title}
      actionLinks={<AlertActionLink onClick={() => navigate("/components")}>Open Components</AlertActionLink>}>
      <Content component="p">{block.reason}</Content>
      <TechnicalDetails rows={rows} />
    </Alert>
  );
};

/** The dashboard's Projects page, where data science projects are made (odh-dashboard route /projects). */
export const dashboardProjectsHref = (dashboardUrl: string | undefined) =>
  dashboardUrl?.startsWith("https://") ? `${dashboardUrl.replace(/\/$/, "")}/projects` : undefined;

/** No data science project yet: where to make one. */
export const NoProjectsAlert: React.FC<{ dashboardUrl?: string }> = ({ dashboardUrl }) => {
  const href = dashboardProjectsHref(dashboardUrl);
  return (
    <Alert variant="info" isInline component="div" title="There are no data science projects yet"
      actionLinks={href ? <AlertActionLink component="a" href={href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">Create a project in the dashboard</AlertActionLink> : undefined}>
      <Content component="p">Pipeline servers and MLflow workspaces belong to a data science project: a project made in the RHOAI dashboard. Create one there, then choose it here.</Content>
    </Alert>
  );
};

/** MLflow's address for a project's workspace: its hash router's experiment list with ?workspace= (opendatahub-io/mlflow server/js MlflowRouter, WorkspaceUtils). */
export function mlflowWorkspaceHref(url: string, project: string | null): string {
  const base = url.replace(/\/+$/, "");
  return project ? `${base}/#/experiments?workspace=${encodeURIComponent(project)}` : base;
}

/** The environment that points an MLflow client at a project's workspace (MLFLOW_WORKSPACE becomes the X-MLflow-Workspace header). */
export function mlflowClientEnv(url: string, project: string): string {
  return [
    `export MLFLOW_TRACKING_URI=${url.replace(/\/+$/, "")}`,
    "export MLFLOW_TRACKING_TOKEN=$(oc whoami -t)",
    `export MLFLOW_WORKSPACE=${project}`,
  ].join("\n");
}

/** A KFP SDK client for a pipeline server's API. */
export function kfpClientSnippet(apiUrl: string): string {
  return [
    "import kfp",
    "",
    "# Your OpenShift token: oc whoami -t",
    `client = kfp.Client(host="${apiUrl}", existing_token="<token>")`,
    "print(client.list_experiments())",
  ].join("\n");
}
