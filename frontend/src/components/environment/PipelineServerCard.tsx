import React from "react";
import {
  ActionListItem,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
} from "@patternfly/react-core";
import type { LegacyStorageState, ResourceState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  ProblemAlerts,
  createdBy,
  isTerminating,
  pvcsLost,
} from "./resourceRow";
import { CreatesList, DetailsActions, DetailsAlert, DetailsStack, HowToUse, ResourceCard } from "./ResourceCard";
import { envStatus, type EnvStatus } from "./resourceStatus";
import { S3_ENDPOINT } from "./StorageCard";

/** What the pipeline server card of one project and its details need. */
export interface PipelineServerProps extends CardActionProps {
  project: string;
  /** The project's pipeline server, if it has one: this tool's, or (managedByTool false) someone else's. */
  server?: ResourceState;
  /** The project has a pipeline server this tool did not create. */
  unmanaged: boolean;
  /** Why a pipeline server cannot be set up on the storage yet (storageNotReadyReason), or null. */
  storageReason: string | null;
  /** The storage serves requests (for the next step of a pipeline server that cannot start). */
  storageReady: boolean;
  legacy?: LegacyStorageState | null;
  /** The project the running action is for. */
  actionProject: string | null;
  /** Shows the Storage card and its details (Cluster tab). */
  onShowStorage: () => void;
}

export function pipelineServerStatus({ server, unmanaged, resAction, actionProject, project }: Pick<PipelineServerProps, "server" | "unmanaged" | "resAction" | "actionProject" | "project">): EnvStatus {
  const mine = actionProject === project;
  const busy = mine && resAction === "setup-pipeline-server" ? "setting-up" : mine && resAction === "teardown-pipeline-server" ? "deleting" : null;
  // Someone else's server whose health is not reported (an older backend).
  if (!server && unmanaged && !busy) return "unknown";
  return envStatus(server, { busy });
}

const onOldStorage = (server: ResourceState | undefined, legacy: LegacyStorageState | null | undefined) =>
  !!server && (legacy?.usedBy ?? []).includes(`${server.namespace}/${server.name}`);

/** The pipeline server of one project as a compact card. */
export const PipelineServerCard: React.FC<PipelineServerProps & { onDetails: (opener: HTMLElement) => void; detailsOpen: boolean }> = (props) => {
  const { project, server, unmanaged, storageReason, baseReason, resAction, setPending, onShowStorage, onDetails, detailsOpen } = props;
  let action: React.ReactNode = null;
  if (!server && !unmanaged) {
    action = <TooltipButton variant="primary" onClick={() => setPending({ kind: "setup-pipeline-server", project })} isLoading={resAction === "setup-pipeline-server" && props.actionProject === project}
      disabledReason={baseReason ?? storageReason}>Set up</TooltipButton>;
  } else if (server?.ready && server.uiRoute && !isTerminating(server)) {
    action = <ExternalLink href={server.uiRoute}>Open pipelines</ExternalLink>;
  }
  const needs = !server && !unmanaged && storageReason
    ? <>Needs: <Button variant="link" isInline onClick={onShowStorage}>Storage</Button> on the Cluster tab.</>
    : undefined;
  return (
    <ResourceCard id="pipeline-server" title="Pipeline server" status={pipelineServerStatus(props)} unmanaged={unmanaged} action={action} needs={needs} onDetails={onDetails} detailsOpen={detailsOpen}
      purpose={unmanaged
        ? "This project's pipeline server was not created by this tool; the tool leaves it alone."
        : "Runs data science pipelines in this project and keeps their artifacts in the S3 storage."} />
  );
};

/** The pipeline server's details, in the drawer. */
export const PipelineServerDetails: React.FC<PipelineServerProps> = (props) => {
  const { project, server, unmanaged, storageReady, legacy, baseReason, resAction, setPending, actionProject } = props;
  const old = onOldStorage(server, legacy);
  return (
    <DetailsStack>
      {unmanaged && (
        <DetailsAlert variant="info" title="Not created by this tool">
          Project <code>{project}</code> has a pipeline server this tool did not create, so it does not set up another one or change it.
        </DetailsAlert>
      )}
      {old && (
        <DetailsAlert variant="info" title="On the storage of an earlier version">
          It keeps its artifacts in the storage of an earlier version. Migrate storage (Storage, Cluster tab) moves it to the new storage.
        </DetailsAlert>
      )}
      {server && <ProblemAlerts kind="pipeline" state={server} minioReady={storageReady || (old && !!legacy?.ready)} />}
      <DescriptionList isCompact isHorizontal aria-label="Pipeline server details">
        <DescriptionListGroup>
          <DescriptionListTerm>Project</DescriptionListTerm>
          <DescriptionListDescription><code>{project}</code></DescriptionListDescription>
        </DescriptionListGroup>
        {server?.name && (
          <DescriptionListGroup>
            <DescriptionListTerm>Object</DescriptionListTerm>
            <DescriptionListDescription>DataSciencePipelinesApplication <code>{server.name}</code></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {/* Only for this tool's servers: someone else's may use any storage (the status does not read it). */}
        {server && server.managedByTool !== false && (
          <DescriptionListGroup>
            <DescriptionListTerm>Storage</DescriptionListTerm>
            <DescriptionListDescription><code>{old && legacy ? `http://${legacy.endpoint}` : S3_ENDPOINT}</code>, bucket <code>pipelines</code></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {server && !server.ready && server.deployed && server.message && (
          <DescriptionListGroup>
            <DescriptionListTerm>State</DescriptionListTerm>
            <DescriptionListDescription>{server.message}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {server && (
          <DescriptionListGroup>
            <DescriptionListTerm>Created by</DescriptionListTerm>
            <DescriptionListDescription>{createdBy(server)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      {server && server.managedByTool !== false && !isTerminating(server) && (
        <DetailsActions>
          <ActionListItem>
            <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-pipeline-server", project, name: server.name })}
              isLoading={resAction === "teardown-pipeline-server" && actionProject === project} disabledReason={baseReason ?? server.teardownBlockedReason ?? null}>Tear down</TooltipButton>
          </ActionListItem>
        </DetailsActions>
      )}
      {server && (
        <HowToUse text={<>
          <p>Import and run pipelines on the project&apos;s Pipelines page in the OpenShift AI dashboard{server.uiRoute ? <> (<a href={server.uiRoute} target="_blank" rel="noopener noreferrer">open it</a>)</> : null}, or with the Kubeflow Pipelines SDK.</p>
          {server.apiRoute && <p>The SDK connects to <code>{server.apiRoute}</code> with your OpenShift token.</p>}
        </>} />
      )}
      {!unmanaged && (
        <CreatesList items={[
          <>DataSciencePipelinesApplication <code>nightly-dspa</code> in project <code>{project}</code></>,
          <>Secret <code>nightly-dspa-s3</code>: the S3 keys, copied from the storage</>,
        ]} />
      )}
    </DetailsStack>
  );
};

/** The confirmation of a pipeline server change, or null for another card's change. */
export function pipelineServerConfirm(pending: Pending, pipelineServers: ResourceState[]): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-pipeline-server":
      return {
        title: `Set up a pipeline server in ${pending.project}?`, confirm: "Set up",
        changes: [
          <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>nightly-dspa</code> and Secret <code>nightly-dspa-s3</code> are created, storing artifacts in the S3 storage (Service <code>seaweedfs-s3</code> in namespace <code>seaweedfs</code>).</>,
        ],
        extra: <Content component="p">The pipeline server takes 1 to 3 minutes to become ready.</Content>,
      };
    case "teardown-pipeline-server": {
      const ps = pipelineServers.find((p) => p.namespace === pending.project);
      return {
        title: `Tear down the pipeline server in ${pending.project}?`, confirm: "Tear down", danger: true,
        changes: [<>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>{pending.name || "nightly-dspa"}</code> and its S3 credentials Secret are deleted. The project stays.</>],
        dataLoss: pvcsLost(ps, "the pipeline database (runs, experiments and their history)") ?? <>The pipeline database (runs, experiments and their history).</>,
      };
    }
    default:
      return null;
  }
}
