import React, { useState } from "react";
import {
  ActionListItem,
  Alert,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  EmptyState,
  EmptyStateBody,
  HelperText,
  HelperTextItem,
  MenuToggle,
  MenuToggleElement,
  Select,
  SelectList,
  SelectOption,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import PlusCircleIcon from "@patternfly/react-icons/dist/esm/icons/plus-circle-icon";
import type { ResourceState } from "../../types";
import { TooltipButton } from "../TooltipButton";
import { CardList, CardListItem } from "../CardList";
import {
  type CardActionProps,
  type ConfirmSpec,
  type Pending,
  ExternalLink,
  RowTitle,
  StatusDetails,
  isTerminating,
  pvcsLost,
  rowActions,
  unmanagedNote,
} from "./resourceRow";

/** Pipeline servers (one DSPA per data science project), stored in the S3 storage. */
export const PipelineServersCard: React.FC<CardActionProps & {
  /** False until the first status has loaded. */
  statusLoaded: boolean;
  minio?: ResourceState;
  pipelineServers: ResourceState[];
  /** Projects offered for a new pipeline server. */
  availableProjects: string[];
  /** Projects with a pipeline server this tool did not create. */
  unmanagedProjects: Set<string>;
}> = ({ statusLoaded, minio, pipelineServers, availableProjects, unmanagedProjects, baseReason, resAction, setPending }) => {
  const [addProjectOpen, setAddProjectOpen] = useState(false);
  const minioReady = !!minio?.ready;
  // N10: every cause is a tooltip on the toggle, which stays focusable (aria-disabled).
  const addPipelineReason = baseReason ?? (!minioReady ? "Available once the S3 storage is running." : null);

  const addPipelineControl = addPipelineReason ? (
    <TooltipButton variant="secondary" icon={<PlusCircleIcon />} disabledReason={addPipelineReason}>Add to a project</TooltipButton>
  ) : (
    <Select
      isOpen={addProjectOpen}
      onOpenChange={setAddProjectOpen}
      onSelect={(_e, val) => {
        setAddProjectOpen(false);
        if (val) setPending({ kind: "setup-pipeline-server", project: val as string });
      }}
      popperProps={{ position: "right" }}
      toggle={(toggleRef: React.Ref<MenuToggleElement>) => (
        <MenuToggle ref={toggleRef} variant="secondary" icon={<PlusCircleIcon />} isExpanded={addProjectOpen} onClick={() => setAddProjectOpen(!addProjectOpen)}>
          {resAction === "setup-pipeline-server" ? "Setting up..." : "Add to a project"}
        </MenuToggle>
      )}
    >
      <SelectList aria-label="Data science projects">
        {availableProjects.map((p) => <SelectOption key={p} value={p}>{p}</SelectOption>)}
        {availableProjects.length === 0 && <SelectOption isDisabled value="">No project without a pipeline server</SelectOption>}
      </SelectList>
    </Select>
  );

  return (
    <Card>
      <CardHeader actions={{ actions: addPipelineControl, hasNoOffset: true }}>
        <CardTitle><Title headingLevel="h3" size="lg">Pipeline servers</Title></CardTitle>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          {statusLoaded && !minioReady && (
            <StackItem>
              <Alert
                variant="warning"
                title={minio?.deployed ? `Pipeline servers need the S3 storage, which is not ready${minio.waitingReason ? ` (${minio.waitingReason})` : ""}` : "Pipeline servers need the S3 storage: set up storage first"}
                isInline
                component="p"
              />
            </StackItem>
          )}
          <StackItem>
            {pipelineServers.length === 0 ? (
              <EmptyState headingLevel="h4" titleText="No pipeline servers" variant="xs">
                <EmptyStateBody>Add one to a data science project with the button above.</EmptyStateBody>
              </EmptyState>
            ) : (
              <CardList aria-label="Pipeline servers">
                {pipelineServers.map((ps) => (
                  <CardListItem key={ps.namespace} labelledBy={`ps-${ps.namespace}`} actions={rowActions(
                    <>
                      {ps.ready && ps.uiRoute && <ActionListItem><ExternalLink href={ps.uiRoute}>Open pipelines</ExternalLink></ActionListItem>}
                      {ps.managedByTool !== false && !isTerminating(ps) && (
                        <ActionListItem>
                          <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "teardown-pipeline-server", project: ps.namespace!, name: ps.name })}
                            isLoading={resAction === "teardown-pipeline-server"} disabledReason={baseReason ?? ps.teardownBlockedReason ?? null}>Tear down</TooltipButton>
                        </ActionListItem>
                      )}
                    </>,
                  )}>
                    <RowTitle id={`ps-${ps.namespace}`} name={ps.namespace ?? ""} state={ps} />
                    {ps.name && <Content component="p" className="pf-v6-u-text-color-subtle">DataSciencePipelinesApplication <code>{ps.name}</code>.{unmanagedNote(ps)}</Content>}
                    <StatusDetails kind="pipeline" state={ps} minioReady={minioReady} />
                  </CardListItem>
                ))}
              </CardList>
            )}
          </StackItem>
          {unmanagedProjects.size > 0 && (
            <StackItem>
              <HelperText>
                <HelperTextItem>
                  Not offered because they already have a pipeline server this tool did not create: {[...unmanagedProjects].join(", ")}.
                </HelperTextItem>
              </HelperText>
            </StackItem>
          )}
        </Stack>
      </CardBody>
    </Card>
  );
};

/** The confirmation of a pipeline server change, or null for another card's change. */
export function pipelineServerConfirm(pending: Pending, pipelineServers: ResourceState[]): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-pipeline-server":
      return {
        title: `Set up a pipeline server in ${pending.project}?`, confirm: "Set up",
        changes: [
          <>Project <code>{pending.project}</code>: DataSciencePipelinesApplication <code>nightly-dspa</code> and Secret <code>nightly-dspa-s3</code> are created, storing artifacts in the S3 storage (Service <code>minio-service</code>).</>,
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
