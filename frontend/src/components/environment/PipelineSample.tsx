import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  HelperText,
  HelperTextItem,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { PipelineSampleStatus } from "../../types";
import { getPipelineSample } from "../../services/api";
import { TooltipButton } from "../TooltipButton";
import { ImageRef } from "../ImageRef";
import type { Pending } from "./resourceRow";
import { EnvStatusLabel } from "./resourceStatus";
import { TabSection } from "./FeatureLayout";
import { READ_ONLY_SAMPLES, type SampleOp, runActive, runLabel, runTime } from "./samples";
import { useSampleStatus } from "./useSampleStatus";

/** The id of the section's heading: the Overview's "Run a sample pipeline" moves the focus there. */
export const PIPELINE_SAMPLE_ID = "pipelines-sample";

const pollWhile = (s: PipelineSampleStatus) => runActive(s.latestRun);

export interface PipelineSampleProps {
  project: string;
  /** The project's pipeline server was not created by this tool: the sample is not offered. */
  unmanaged: boolean;
  readOnly: boolean;
  baseReason: string | null;
  op: SampleOp | null;
  refreshKey: number;
  setPending: (p: Pending) => void;
  /** The current time, for how long a run has been running (tests pass a fixed clock). */
  now?: () => number;
}

/**
 * "Sample pipeline": a two-step run on the project's pipeline server (the
 * one this tool set up), its latest run's state, read again every few
 * seconds while it runs, a link to it in the dashboard, and Remove.
 */
export const PipelineSample: React.FC<PipelineSampleProps> = ({ project, unmanaged, readOnly, baseReason, op, refreshKey, setPending, now = Date.now }) => {
  const reasonId = React.useId();
  const status = useSampleStatus(getPipelineSample, unmanaged ? null : project, refreshKey, pollWhile);
  const intro = <Content component="p">A two-step pipeline that checks the pipeline server works end to end: the first step writes a small file to the S3 storage, the second reads it back. It usually finishes in 1 to 2 minutes.</Content>;
  const section = (body: React.ReactNode) => (
    <TabSection title="Sample pipeline" id={PIPELINE_SAMPLE_ID}>
      {intro}
      {body}
    </TabSection>
  );

  if (unmanaged) {
    return section(<Content component="p">The sample runs only on a pipeline server this tool set up. The one in {project} was created outside this tool (for example in the dashboard), so the sample is not offered here.</Content>);
  }
  const data = status.data;
  if (!data) {
    if (status.error) {
      return section(
        <Alert variant="warning" isInline component="div" title={`Cannot read the sample pipeline of ${project}`}
          actionLinks={<AlertActionLink onClick={status.reload}>Try again</AlertActionLink>}>
          <Content component="p">{status.error}</Content>
        </Alert>,
      );
    }
    return section(
      <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
        <FlexItem><Spinner size="md" aria-hidden /></FlexItem>
        <FlexItem><Content component="p">Reading the sample pipeline of {project}...</Content></FlexItem>
      </Flex>,
    );
  }
  if (!data.available) {
    return section(<Alert variant="info" isInline component="p" title="The sample pipeline is not available now">{data.reason}</Alert>);
  }
  if (data.state === "foreign" || data.state === "unknown") {
    return section(
      <Alert variant="warning" isInline component="div"
        title={data.state === "foreign" ? "Something of the sample's name exists that this tool did not create" : `Cannot read the sample pipeline of ${project}`}
        actionLinks={data.state === "unknown" ? <AlertActionLink onClick={status.reload}>Try again</AlertActionLink> : undefined}>
        <Content component="p">{data.message}{data.state === "foreign" ? " The sample cannot run here." : ""}</Content>
      </Alert>,
    );
  }

  const run = data.latestRun;
  const busy = op?.feature === "pipeline" && op.project === project ? op : null;
  const label = busy ? (busy.action === "remove" ? { status: "deleting" as const, text: "Removing" } : { status: "setting-up" as const, text: "Starting a run" }) : runLabel(run);
  const when = run && !busy ? runTime(run, now()) : null;
  const reason = readOnly ? READ_ONLY_SAMPLES : baseReason;
  const present = data.state === "present";
  return section(
    <Stack hasGutter>
      <StackItem>
        <DescriptionList isCompact isHorizontal aria-label={`Sample pipeline in ${project}`}>
          <DescriptionListGroup>
            <DescriptionListTerm>Latest run</DescriptionListTerm>
            <DescriptionListDescription>
              <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                <FlexItem><EnvStatusLabel status={label.status} text={label.text} /></FlexItem>
                {when && <FlexItem><Content component="small">{when}</Content></FlexItem>}
                {run?.link && (
                  <FlexItem>
                    <Button variant="link" isInline component="a" href={run.link} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">View run in dashboard</Button>
                  </FlexItem>
                )}
              </Flex>
            </DescriptionListDescription>
          </DescriptionListGroup>
          {present && (
            <DescriptionListGroup>
              <DescriptionListTerm>Runs</DescriptionListTerm>
              <DescriptionListDescription>
                {data.runs === 1 ? "1 run" : `${data.runs} runs`} of the sample in {project}
                {data.runsLink && <>{" "}<Button variant="link" isInline component="a" href={data.runsLink} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">View runs in dashboard</Button></>}
              </DescriptionListDescription>
            </DescriptionListGroup>
          )}
          <DescriptionListGroup>
            <DescriptionListTerm>Image</DescriptionListTerm>
            <DescriptionListDescription><ImageRef image={data.image} /></DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </StackItem>
      {run?.state === "FAILED" && !busy && (
        <StackItem>
          <Alert variant="danger" isInline component="div" title="The sample run failed">
            <Content component="p">{run.error || "The run's page in the dashboard shows which step failed and why."} If a step cannot start because its image cannot be pulled (a disconnected cluster), mirror the image above, or set SAMPLE_PIPELINE_IMAGE on the updater to a copy the cluster can pull.</Content>
          </Alert>
        </StackItem>
      )}
      {status.gaveUp && runActive(run) && (
        <StackItem>
          <Content component="p">Stopped checking the run automatically after 10 minutes. <Button variant="link" isInline onClick={status.reload}>Check again</Button></Content>
        </StackItem>
      )}
      {status.error && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="The run's state may be out of date">{status.error}</Alert>
        </StackItem>
      )}
      <StackItem>
        <ActionList>
          <ActionListGroup>
            <ActionListItem>
              <TooltipButton variant="secondary" onClick={() => setPending({ kind: "pipeline-sample", project, action: "run", image: data.image })} disabledReason={reason} aria-describedby={reason ? reasonId : undefined}>
                Run sample pipeline
              </TooltipButton>
            </ActionListItem>
            {present && (
              <ActionListItem>
                <TooltipButton variant="secondary" isDanger onClick={() => setPending({ kind: "pipeline-sample", project, action: "remove", runs: data.runs })} disabledReason={reason} aria-describedby={reason ? reasonId : undefined}>
                  Remove sample
                </TooltipButton>
              </ActionListItem>
            )}
          </ActionListGroup>
        </ActionList>
      </StackItem>
      {reason && (
        <StackItem>
          <HelperText><HelperTextItem id={reasonId}>{reason}</HelperTextItem></HelperText>
        </StackItem>
      )}
    </Stack>,
  );
};
