import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  AlertActionLink,
  Content,
  DataList,
  DataListAction,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { MLflowSampleAction, MLflowSampleId, SampleStatus } from "../../types";
import { getMLflowSamples } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { ExternalLink, type Pending } from "./resourceRow";
import { EnvStatusLabel, NotManagedLabel } from "./resourceStatus";
import { TabSection } from "./FeatureLayout";
import type { FeatureState } from "./featureState";
import { READ_ONLY_SAMPLES, SAMPLE_INFO, type SampleOp, bulkEmptyReason, sampleLabel, sampleNote, samplesFor } from "./samples";
import { useSampleStatus } from "./useSampleStatus";

/** The id of the section's heading: the Overview's "Add sample data" moves the focus there. */
export const MLFLOW_SAMPLES_ID = "mlflow-samples";

const ACTION_LABEL: Record<MLflowSampleAction, string> = { add: "Add", remove: "Remove", regenerate: "Regenerate" };

/** One sample: its name, what it gives, its state and what that means, and its actions. */
const SampleRow: React.FC<{
  sample: SampleStatus;
  op: SampleOp | null;
  reason: string | null;
  reasonId: string;
  onAction: (action: MLflowSampleAction, id: MLflowSampleId) => void;
}> = ({ sample, op, reason, reasonId, onAction }) => {
  const info = SAMPLE_INFO[sample.id];
  const label = sampleLabel(sample, op);
  const note = sampleNote(sample);
  const titleId = `mlflow-sample-${sample.id}`;
  // Remove is never offered for the demo (the server has no right to delete it); its note says how.
  const actions = (["add", "regenerate", "remove"] as MLflowSampleAction[]).filter((a) => sample.actions.includes(a));
  return (
    <DataListItem aria-labelledby={titleId}>
      <DataListItemRow>
        <DataListItemCells dataListCells={[
          <DataListCell key="sample" width={3}>
            <Stack>
              <StackItem><strong id={titleId}>{info?.name ?? sample.name}</strong></StackItem>
              <StackItem><Content component="small">{info?.value}</Content></StackItem>
              {note && (
                <StackItem className="pf-v6-u-mt-xs">
                  <HelperText><HelperTextItem variant={sample.state === "partial" ? "warning" : "default"}>{note}</HelperTextItem></HelperText>
                </StackItem>
              )}
            </Stack>
          </DataListCell>,
          <DataListCell key="state" width={1}>
            {label ? <EnvStatusLabel status={label.status} text={label.text} /> : <NotManagedLabel />}
          </DataListCell>,
        ]} />
        {(actions.length > 0 || sample.link) && (
          <DataListAction id={`${titleId}-actions`} aria-labelledby={`${titleId} ${titleId}-actions`} aria-label={`${info?.name ?? sample.name} actions`}>
            <ActionList>
              <ActionListGroup>
                {actions.map((a) => (
                  <ActionListItem key={a}>
                    <TooltipButton variant="secondary" isDanger={a === "remove"} onClick={() => onAction(a, sample.id)} disabledReason={reason}
                      aria-describedby={reason ? reasonId : undefined} aria-label={`${ACTION_LABEL[a]} ${(info?.name ?? sample.name).toLowerCase()}`}>
                      {ACTION_LABEL[a]}
                    </TooltipButton>
                  </ActionListItem>
                ))}
                {sample.link && <ActionListItem><ExternalLink href={sample.link}>Open in MLflow</ExternalLink></ActionListItem>}
              </ActionListGroup>
            </ActionList>
          </DataListAction>
        )}
      </DataListItemRow>
    </DataListItem>
  );
};

export interface MLflowSamplesProps {
  /** MLflow's tab state (the server's health). */
  state: FeatureState;
  /** MLflow is ready and has an address: samples can be read. */
  ready: boolean;
  project: string | null;
  readOnly: boolean;
  /** Why changes cannot run now (permissions, another operation), or null. */
  baseReason: string | null;
  /** The sample operation running now, if any. */
  op: SampleOp | null;
  /** Changes when a sample operation ends: the state is read again. */
  refreshKey: number;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
}

/**
 * "Sample data": example data for the chosen project's MLflow workspace,
 * one row per sample with its state and actions, and Add all / Remove all
 * as one request each (the server allows one samples operation per user
 * every 30 seconds). Every change is confirmed and runs in the page's run
 * dialog.
 */
export const MLflowSamples: React.FC<MLflowSamplesProps> = ({ state, ready, project, readOnly, baseReason, op, refreshKey, setPending, onSetUp }) => {
  const reasonId = React.useId();
  const status = useSampleStatus(getMLflowSamples, ready ? project : null, refreshKey);
  const intro = <Content component="p">Example data for a project&apos;s MLflow workspace, added in one click, so you can see what MLflow and the dashboard show without writing code. Everything this tool adds is tagged, and Remove deletes only that.</Content>;

  if (!ready) {
    const next = state.next;
    const setUp = next?.kind === "set-up" && !readOnly && !baseReason && !next.reason ? next : null;
    const why = state.status === "setting-up" ? "MLflow is starting. Sample data can be added once it is ready."
      : state.status === "deleting" ? "MLflow is being deleted."
        : state.status === "error" || state.status === "needs-attention" ? "MLflow does not work right now: the MLflow server above says why."
          : "Sample data goes into MLflow, which is not set up yet. Set it up first, then add sample data to a project here.";
    return (
      <TabSection title="Sample data" id={MLFLOW_SAMPLES_ID}>
        {intro}
        <Alert variant="info" isInline component="div" title="Sample data needs a working MLflow"
          actionLinks={setUp ? <AlertActionLink onClick={() => onSetUp(setUp.setup)}>{setUp.label}...</AlertActionLink> : undefined}>
          <Content component="p">{why}{next?.kind === "set-up" && !setUp ? ` ${readOnly ? READ_ONLY_SAMPLES : baseReason ?? next.reason ?? ""}` : ""}</Content>
        </Alert>
      </TabSection>
    );
  }
  if (!project) {
    return (
      <TabSection title="Sample data" id={MLFLOW_SAMPLES_ID}>
        {intro}
        <Content component="p">Choose a project under Use it to see its sample data.</Content>
      </TabSection>
    );
  }

  const data = status.data;
  const samples = data?.available ? data.samples : [];
  const addAll = samplesFor(samples, "add");
  const removeAll = samplesFor(samples, "remove");
  const reason = readOnly ? READ_ONLY_SAMPLES : baseReason;
  const addReason = reason ?? (addAll.length === 0 ? bulkEmptyReason("add") : null);
  const removeReason = reason ?? (removeAll.length === 0 ? bulkEmptyReason("remove") : null);
  const run = (action: MLflowSampleAction, ids: MLflowSampleId[]) => setPending({ kind: "mlflow-samples", project, action, samples: ids });
  const opHere = op?.feature === "mlflow" && op.project === project ? op : null;

  let body: React.ReactNode;
  if (status.error && !data) {
    body = (
      <Alert variant="warning" isInline component="div" title={`Cannot read the sample data of ${project}`}
        actionLinks={<AlertActionLink onClick={status.reload}>Try again</AlertActionLink>}>
        <Content component="p">{status.error}</Content>
      </Alert>
    );
  } else if (!data) {
    body = (
      <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
        <FlexItem><Spinner size="md" aria-hidden /></FlexItem>
        <FlexItem><Content component="p">Reading the sample data of {project}...</Content></FlexItem>
      </Flex>
    );
  } else if (!data.available) {
    body = <Alert variant="info" isInline component="p" title="Sample data is not available now">{data.reason}</Alert>;
  } else {
    body = (
      <Stack hasGutter>
        <StackItem>
          <ActionList>
            <ActionListGroup>
              <ActionListItem>
                <TooltipButton variant="secondary" onClick={() => run("add", addAll)} disabledReason={addReason} aria-describedby={addReason ? (reason ? reasonId : `${reasonId}-add`) : undefined}>Add all</TooltipButton>
              </ActionListItem>
              <ActionListItem>
                <TooltipButton variant="secondary" isDanger onClick={() => run("remove", removeAll)} disabledReason={removeReason} aria-describedby={removeReason ? (reason ? reasonId : `${reasonId}-remove`) : undefined}>Remove all</TooltipButton>
              </ActionListItem>
            </ActionListGroup>
          </ActionList>
        </StackItem>
        {(addReason || removeReason) && (
          <StackItem>
            <HelperText>
              {reason
                ? <HelperTextItem id={reasonId}>{reason}</HelperTextItem>
                : (
                  <>
                    {addReason && <HelperTextItem id={`${reasonId}-add`}>Add all: {addReason}</HelperTextItem>}
                    {removeReason && <HelperTextItem id={`${reasonId}-remove`}>Remove all: {removeReason}</HelperTextItem>}
                  </>
                )}
            </HelperText>
          </StackItem>
        )}
        {status.error && (
          <StackItem>
            <Alert variant="warning" isInline component="p" title="The sample data may be out of date">{status.error}</Alert>
          </StackItem>
        )}
        <StackItem>
          <DataList aria-label={`Sample data in ${project}`}>
            {samples.map((s) => <SampleRow key={s.id} sample={s} op={opHere} reason={reason} reasonId={reasonId} onAction={(a, id) => run(a, [id])} />)}
          </DataList>
        </StackItem>
      </Stack>
    );
  }
  return (
    <TabSection title="Sample data" id={MLFLOW_SAMPLES_ID}>
      {intro}
      {body}
    </TabSection>
  );
};
