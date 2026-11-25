import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { MLflowSampleAction, MLflowSampleId, SampleStatus } from "../../types";
import { getMLflowSamples } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import type { Pending } from "./resourceRow";
import { EnvStatusLabel, NotManagedLabel } from "./resourceStatus";
import { TabSection } from "./FeatureLayout";
import type { FeatureState } from "./featureState";
import { READ_ONLY_SAMPLES, SAMPLE_INFO, type SampleOp, bulkEmptyReason, sampleLabel, sampleNote, samplesFor } from "./samples";
import { useSampleStatus } from "./useSampleStatus";
import { RowsSkeleton } from "./Loading";

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
  const name = info?.name ?? sample.name;
  // Remove is never offered for the demo (the server has no right to delete it); its note says how.
  const actions = (["add", "regenerate", "remove"] as MLflowSampleAction[]).filter((a) => sample.actions.includes(a));
  return (
    <Tr>
      <Td dataLabel="Sample">
        <Stack>
          <StackItem><strong>{name}</strong></StackItem>
          <StackItem>
            <Content component="small">
              {info?.value}
              {sample.link && <>{" "}<Button variant="link" isInline component="a" href={sample.link} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">Open in MLflow</Button></>}
            </Content>
          </StackItem>
          {note && <StackItem className="pf-v6-u-mt-xs"><Content component="small">{note}</Content></StackItem>}
        </Stack>
      </Td>
      <Td dataLabel="State">{label ? <EnvStatusLabel status={label.status} text={label.text} /> : <NotManagedLabel />}</Td>
      {/* Actions in a table cell are inline links, as PatternFly puts them there: they sit on the row's text line. */}
      <Td dataLabel="Actions" modifier="fitContent">
        {actions.length > 0 && (
          <Flex gap={{ default: "gapMd" }} flexWrap={{ default: "nowrap" }}>
            {actions.map((a) => (
              <FlexItem key={a}>
                <TooltipButton variant="link" isInline isDanger={a === "remove"} onClick={() => onAction(a, sample.id)} disabledReason={reason}
                  aria-describedby={reason ? reasonId : undefined} aria-label={`${ACTION_LABEL[a]} the ${info?.inSentence ?? sample.name}`}>
                  {ACTION_LABEL[a]}
                </TooltipButton>
              </FlexItem>
            ))}
          </Flex>
        )}
      </Td>
    </Tr>
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
          actionLinks={setUp ? <AlertActionLink onClick={() => onSetUp(setUp.setup)}>{setUp.label}</AlertActionLink> : undefined}>
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
  const run = (action: MLflowSampleAction, ids: MLflowSampleId[]) => {
    // What Add or Regenerate changes first (the demo's MLflow memory limit): the confirmation names it.
    const repairs = action === "remove" ? [] : samples.filter((s) => ids.includes(s.id) && s.repair).map((s) => s.repair!);
    setPending({ kind: "mlflow-samples", project, action, samples: ids, ...(repairs.length ? { repairs } : {}) });
  };
  const opHere = op?.feature === "mlflow" && op.project === project ? op : null;
  const samplesHead = (
    <Thead>
      <Tr>
        <Th width={60}>Sample</Th>
        <Th width={20}>State</Th>
        <Th screenReaderText="Actions" />
      </Tr>
    </Thead>
  );

  let body: React.ReactNode;
  if (status.error && !data) {
    body = (
      <Alert variant="warning" isInline component="div" title={`Cannot read the sample data of ${project}`}
        actionLinks={<AlertActionLink onClick={status.reload}>Try again</AlertActionLink>}>
        <Content component="p">{status.error}</Content>
      </Alert>
    );
  } else if (!data) {
    // The project's samples are read when it is chosen: the table, with a Skeleton row per sample.
    body = (
      <Table aria-label={`Sample data in ${project}`} aria-busy="true">
        {samplesHead}
        <RowsSkeleton what={`the sample data of ${project}`} columns={["Sample", "State", "Actions"]} rows={Object.keys(SAMPLE_INFO).length} />
      </Table>
    );
  } else if (!data.available) {
    body = <Alert variant="info" isInline component="p" title="Sample data is not available">{data.reason}</Alert>;
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
          <Table aria-label={`Sample data in ${project}`}>
            {samplesHead}
            <Tbody>
              {samples.map((s) => <SampleRow key={s.id} sample={s} op={opHere} reason={reason} reasonId={reasonId} onAction={(a, id) => run(a, [id])} />)}
            </Tbody>
          </Table>
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
