import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Checkbox,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { ResourcesStatus, WorkbenchPlan, WorkbenchState, WorkbenchStatus } from "../../types";
import { type ConfirmSpec, type Pending, type WorkbenchPending, ExternalLink, StartingNote } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, NoProjectsAlert } from "./UseIt";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { LoadFailed, PartsSkeleton } from "./Loading";
import type { Busy, FeatureState } from "./featureState";
import { WORKBENCH_FILES, WORKBENCH_NAME, WORKBENCH_STORAGE, keptStorage, storageInUseText, profileText, storageText, workbenchIn, workbenchSetUpReason, workbenchStatusOf, workbenchUnreadable } from "./workbenchState";

export interface WorkbenchTabProps {
  status: ResourcesStatus;
  /** Response from GET /api/resources/workbench—remains null until fetched; wb.project names the associated project. */
  workbench: WorkbenchStatus | null;
  workbenchError: string | null;
  onRetry: () => void;
  project: string | null;
  projects: ProjectOption[];
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  onClearProject: () => void;
  missingProject: string | null;
  /** Per-project workbench state; mirrors the Overview tab. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
}

/** Image, hardware profile, storage, and connection fields use one layout both before and after Set up. */
/** A storage line whose parts never break inside ("storage class gp3-csi" stays whole). */
const StorageLine: React.FC<{ text: string }> = ({ text }) => (
  <>{text.split(", ").map((part, i) => <React.Fragment key={i}>{i > 0 && ", "}<span className="pf-v6-u-text-nowrap">{part}</span></React.Fragment>)}</>
);

const DetailsList: React.FC<{ label: string; image?: string; profile: string; storage?: string; connection: boolean }> = ({ label, image, profile, storage, connection }) => (
  <DescriptionList isCompact isHorizontal aria-label={label}>
    {image && (
      <DescriptionListGroup>
        <DescriptionListTerm>Image</DescriptionListTerm>
        <DescriptionListDescription>{image}</DescriptionListDescription>
      </DescriptionListGroup>
    )}
    <DescriptionListGroup>
      <DescriptionListTerm>Hardware profile</DescriptionListTerm>
      <DescriptionListDescription>{profile}</DescriptionListDescription>
    </DescriptionListGroup>
    {storage && (
      <DescriptionListGroup>
        <DescriptionListTerm>Storage</DescriptionListTerm>
        <DescriptionListDescription><StorageLine text={storage} /></DescriptionListDescription>
      </DescriptionListGroup>
    )}
    <DescriptionListGroup>
      <DescriptionListTerm>Connection</DescriptionListTerm>
      <DescriptionListDescription>{connection ? "The pipeline server's S3 connection (its credentials as environment variables)" : "None (a pipeline server in the project adds one)"}</DescriptionListDescription>
    </DescriptionListGroup>
  </DescriptionList>
);

const PlanDetails: React.FC<{ plan: WorkbenchPlan; label: string; kept?: boolean }> = ({ plan, label, kept }) => (
  <DetailsList label={label} image={`${plan.image} (${plan.imageTag})`}
    profile={`${profileText(plan.hardwareProfile, plan.cpu, plan.memory)}${plan.queue ? `, queue ${plan.queue}` : ""}`}
    storage={kept ? `${WORKBENCH_STORAGE}, kept from the deleted workbench, with its files` : storageText(plan.storageSize, plan.storageClass)} connection={!!plan.connection} />
);

const ItemDetails: React.FC<{ item: WorkbenchState }> = ({ item }) => (
  <DetailsList label="Workbench details" image={item.image} profile={profileText(item.hardwareProfile, item.cpu, item.memory)}
    storage={item.storage ? storageText(item.storage.size, item.storage.storageClass) : undefined} connection={!!item.connectionAttached} />
);

/** Headline for the tab alert when Set up is blocked—cluster scope versus this project. */
function planAlertTitle(code: string | undefined): string {
  if (code === "no_profile") return "No hardware profile is available for workbenches on this cluster";
  if (code === "no_profile_project") return "No hardware profile is available for workbenches in this project";
  return "A workbench cannot be set up in this project";
}

/** Project workbench row: current state with Open, Start, Stop, Delete; planned Set up output; or retained storage. */
const WorkbenchPart: React.FC<WorkbenchTabProps & { project: string }> = ({ workbench, project, busy, baseReason, readOnly, setPending, onRetry }) => {
  const item = workbenchIn(workbench, project);
  const exists = !!item?.deployed;
  const unmanaged = exists && item?.managedByTool === false;
  const unreadable = workbenchUnreadable(workbench, project) && !exists;
  const kept = keptStorage(workbench, project);
  const plan = workbench?.project === project ? workbench.plan : undefined;
  const planProblem = workbench?.project === project && !workbench.blocked && !workbench.unsupported ? workbench.planProblem : undefined;
  const { status, word } = workbenchStatusOf(workbench, project, busy);
  const mine = exists && !unmanaged && !readOnly && !item?.terminating;
  const canStop = mine && (item?.state === "running" || item?.state === "starting" || item?.state === "failed");
  const canStart = mine && item?.state === "stopped";
  const canOpen = item?.state === "running" && !!item.url;
  const menu: PartMenuItem[] = mine
    ? [{ key: "delete", label: "Delete workbench...", onClick: () => setPending({ kind: "teardown-workbench", project }), disabledReason: baseReason, isDanger: true }]
    : kept && !readOnly
      ? [{ key: "delete-storage", label: "Delete storage...", onClick: () => setPending({ kind: "delete-workbench-storage", project }), disabledReason: baseReason, isDanger: true }]
      : [];
  const actions = (
    <>
      {canOpen && <ActionListItem><ExternalLink variant="primary" href={item!.url!}>Open workbench</ExternalLink></ActionListItem>}
      {canStart && <ActionListItem><PrimaryAction label="Start" onClick={() => setPending({ kind: "start-workbench", project })} reason={baseReason} /></ActionListItem>}
      {canStop && <ActionListItem><PrimaryAction label="Stop" variant="secondary" onClick={() => setPending({ kind: "stop-workbench", project })} reason={baseReason} /></ActionListItem>}
    </>
  );
  let text: React.ReactNode;
  if (unreadable) text = `The workbench in ${project} cannot be read now, so its state is unknown.`;
  else if (unmanaged) text = `A workbench named ${WORKBENCH_NAME} exists in ${project}, but this tool did not create it: use it in the dashboard. This tool does not start, stop or delete it.`;
  else if (exists) text = <>Jupyter workbench {WORKBENCH_NAME}. Files saved under <code>{WORKBENCH_FILES}</code> are in its storage and stay when it stops.</>;
  else if (planProblem) text = `A workbench cannot be set up in ${project} now: the alert below says why.`;
  else if (kept && kept.usedBy?.length) text = <>Storage kept from a deleted workbench: <code>{WORKBENCH_STORAGE}</code>. Another workbench uses it now, so Set up cannot use it: Set up below says what to do.</>;
  else if (kept) text = <>Storage kept from a deleted workbench: <code>{WORKBENCH_STORAGE}</code>, reused by the next Set up with its files.</>;
  else text = `Not set up in ${project} yet. Set up creates a Jupyter workbench as the dashboard's Create workbench does; it starts in 1 to 3 minutes.`;
  return (
    <PartItem id="workbench" title="Workbench" scope={`In project ${project}`} status={status} statusText={word} unmanaged={unmanaged}
      actions={canStart || canStop || canOpen ? actions : undefined} menu={menu}>
      <Content component="p">{text}</Content>
      {unreadable && (
        <Alert variant="warning" isInline component="div" title="The workbench cannot be read" actionLinks={<AlertActionLink onClick={onRetry}>Retry</AlertActionLink>}>
          <Content component="p">{workbench?.itemsProblem ?? "The read failed."} This tool does not set up or delete anything here until it can read it.</Content>
        </Alert>
      )}
      {item?.state === "failed" && (
        <Alert variant="danger" isInline component="div" title="The workbench can't start">
          <Content component="p">{item.message ?? item.waitingReason}. Stop it, then check the image and the project&apos;s quota in the dashboard.</Content>
        </Alert>
      )}
      {item && item.state === "starting" && !item.terminating && <StartingNote state={item} typical="1 to 3 minutes" />}
      {exists && item && <ItemDetails item={item} />}
      {!exists && !unreadable && plan && <PlanDetails plan={plan} label="What Set up creates" kept={!!kept} />}
      {!exists && !unreadable && kept && !plan && (
        <DescriptionList isCompact isHorizontal aria-label="Kept storage">
          <DescriptionListGroup>
            <DescriptionListTerm>Storage</DescriptionListTerm>
            <DescriptionListDescription><StorageLine text={storageText(kept.size, kept.storageClass)} /></DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      )}
    </PartItem>
  );
};

/**
 * Workbench tab—project picker, that project's workbench (status and actions),
 * Set up (image, profile, storage), usage notes, what gets created, and Clean up.
 */
export const WorkbenchTab: React.FC<WorkbenchTabProps> = (props) => {
  const { status, workbench, workbenchError, onRetry, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, readOnly, baseReason, onSetUp, onCleanup } = props;
  const next = state.next;
  const noProjects = projectsLoaded && projects.length === 0;
  const current = !!project && workbench?.project === project;
  const item = project ? workbenchIn(workbench, project) : undefined;
  const planProblem = current && !workbench?.blocked && !workbench?.unsupported ? workbench?.planProblem : undefined;
  const offered = (workbench?.items ?? []).filter((i) => (i.deployed && i.managedByTool) || (!i.deployed && i.storage?.managedByTool));
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Workbench" purpose={FEATURE_PURPOSE.workbench} state={state} /></StackItem>
      {workbench?.blocked && <StackItem><ComponentAlert feature="Workbenches" block={workbench.blocked} /></StackItem>}
      {workbench?.unsupported && <StackItem><Alert variant="warning" isInline component="p" title="Workbenches cannot be set up here">{workbench.unsupported}</Alert></StackItem>}
      {workbench && workbenchError && (
        <StackItem>
          <Alert variant="danger" isInline component="p" title="Cannot read the workbench" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>{workbenchError}</Alert>
        </StackItem>
      )}
      {noProjects && <StackItem><NoProjectsAlert dashboardUrl={status.dashboardUrl} things="Workbenches" /></StackItem>}
      {missingProject && (
        <StackItem>
          <Alert variant="info" isInline component="p" title={`Project ${missingProject} no longer exists`}>It was deleted, or it is no longer a data science project. Choose another project.</Alert>
        </StackItem>
      )}
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="workbench-project">
            <ProjectSelect id="workbench-project" projects={projects} selected={project} onSelect={onProject} onClear={onClearProject} />
            <FormHelperText>
              <HelperText><HelperTextItem>Data science projects (made in the RHOAI dashboard). This tool sets up one workbench per project, named {WORKBENCH_NAME}.</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
        </Form>
      </StackItem>
      <StackItem>
        {project && !current ? (
          workbenchError ? <LoadFailed title="Cannot read the workbench" error={workbenchError} onRetry={onRetry} />
            : <PartsSkeleton aria-label="What Workbench needs" what="the workbench" count={1} />
        ) : project ? (
          <PartList aria-label="What Workbench needs"><WorkbenchPart {...props} project={project} /></PartList>
        ) : (
          <PartList aria-label="What Workbench needs">
            <PartItem id="workbench" title="Workbench" scope="In the project you choose" status="not-set-up" unmanaged={false}>
              <Content component="p">Choose a project above to see its workbench, or to set one up there.</Content>
            </PartItem>
          </PartList>
        )}
      </StackItem>
      {project && current && item?.storage?.managedByTool && storageInUseText(item.storage.usedBy) && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="Its storage is used by another workbench">
            {item.deployed ? `${storageInUseText(item.storage.usedBy)} Until then this tool does not delete the storage.` : "Until then this tool does not reuse or delete it."}
          </Alert>
        </StackItem>
      )}
      {planProblem && (
        <StackItem><Alert variant="warning" isInline component="p" title={planAlertTitle(workbench?.planProblemCode)}>{planProblem}</Alert></StackItem>
      )}
      {current && next?.kind === "set-up" && project && !planProblem && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp([{ kind: "setup-workbench", project }])} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? workbenchSetUpReason(workbench, project)} />
        </StackItem>
      )}
      {project && item?.state === "running" && item.url && (
        <StackItem>
          <TabSection title="Use it" id="workbench-use">
            <Content component="p">Open workbench starts JupyterLab; you sign in with your OpenShift account. The dashboard lists it in project {project}, under Workbenches.</Content>
            <WhatNext id="workbench-next" items={[
              <>Start a notebook and save it under <code>{WORKBENCH_FILES}</code>.</>,
              ...(item.connectionAttached ? [<>Read and write the project&apos;s S3 bucket: its credentials are environment variables (<code>AWS_S3_ENDPOINT</code>, <code>AWS_S3_BUCKET</code>, ...).</>] : []),
              <>Stop it when you are done; Start brings it back with the saved files.</>,
            ]} />
          </TabSection>
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>A PersistentVolumeClaim <code>{WORKBENCH_STORAGE}</code> for the workbench&apos;s files, as the dashboard makes one (its default size and storage class)</>,
            <>A Notebook <code>{WORKBENCH_NAME}</code> exactly as the dashboard&apos;s Create workbench makes it: the image the cluster marks as its default workbench image, the dashboard&apos;s default hardware profile, the pipeline server&apos;s S3 connection when the project has one</>,
            <>The platform adds the rest itself: sign-in and the link to open it</>,
            <>This tool remembers which workbench and storage it made, so it deletes only those</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && offered.length > 0 && (
        <StackItem>
          <CleanupSection id="workbench-cleanup" label="Clean up Workbench..." onClick={onCleanup} reason={baseReason}
            text="Deletes the workbenches this tool set up, in every project (Delete workbench in a workbench's menu deletes only that project's). Each workbench's storage is kept unless you tick Also delete its storage. You pick the workbenches; they are deleted in one operation." />
        </StackItem>
      )}
    </Stack>
  );
};

/** Progress steps emitted for a workbench run, in sequence (pkg/cluster RunWorkbench). */
export function workbenchSteps(p: WorkbenchPending): { id: string; title: string }[] {
  switch (p.kind) {
    case "setup-workbench":
      return [
        { id: "check", title: "Check what the workbench needs" },
        { id: "storage", title: `Create storage ${WORKBENCH_STORAGE}` },
        { id: "workbench", title: `Create workbench ${WORKBENCH_NAME}` },
      ];
    case "start-workbench":
      return [{ id: "workbench", title: `Start workbench ${WORKBENCH_NAME}` }];
    case "stop-workbench":
      return [{ id: "workbench", title: `Stop workbench ${WORKBENCH_NAME}` }];
    case "delete-workbench-storage":
      return [{ id: "storage", title: `Delete storage ${WORKBENCH_STORAGE}` }];
    default:
      return [
        { id: "workbench", title: `Delete workbench ${WORKBENCH_NAME}` },
        ...(p.deleteStorage ? [{ id: "storage", title: `Delete storage ${WORKBENCH_STORAGE}` }] : []),
      ];
  }
}

/** Explains in-memory loss on stop or teardown; shared wording across Stop, Delete, and Clean up. */
export const RUNNING_SESSION_LOST = `Unsaved work in the running session is lost: only files saved under ${WORKBENCH_FILES} are in the storage.`;

/** Confirmation spec for a pending workbench action; null when pending targets another feature. */
export function workbenchConfirm(pending: Pending, wb: WorkbenchStatus | null, setPending?: (p: Pending) => void): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-workbench": {
      const { project } = pending;
      const plan = wb?.project === project ? wb.plan : undefined;
      const kept = !!keptStorage(wb, project);
      return {
        title: `Set up a workbench in ${project}?`, confirm: "Set up", runTitle: `Set up a workbench in ${project}`, changesOpen: true,
        summary: (
          <Stack hasGutter>
            <StackItem><Content component="p">Creates workbench {WORKBENCH_NAME} in {project}{plan ? <> with {plan.image} ({plan.imageTag})</> : null}, as the dashboard&apos;s Create workbench does. It starts in 1 to 3 minutes.</Content></StackItem>
            {plan && <StackItem><PlanDetails plan={plan} label="What the workbench gets" kept={kept} /></StackItem>}
          </Stack>
        ),
        changes: [
          kept ? <>Project <code>{project}</code>: the kept storage <code>{WORKBENCH_STORAGE}</code> is used again, with its files.</> : <>Project <code>{project}</code>: storage <code>{WORKBENCH_STORAGE}</code>{plan ? ` (${plan.storageSize})` : ""}.</>,
          <>Project <code>{project}</code>: workbench <code>{WORKBENCH_NAME}</code>; the platform adds sign-in and the link to open it.</>,
          <>This tool remembers which workbench and storage it made, so it deletes only those.</>,
        ],
      };
    }
    case "start-workbench":
      return {
        title: `Start the workbench in ${pending.project}?`, confirm: "Start", runTitle: `Start the workbench in ${pending.project}`,
        summary: <Content component="p">Starts workbench {WORKBENCH_NAME} again, as the dashboard&apos;s Start does. It is ready in 1 to 3 minutes, with the files saved under <code>{WORKBENCH_FILES}</code>.</Content>,
        changes: [<>Workbench <code>{WORKBENCH_NAME}</code>: the dashboard&apos;s stop mark is removed.</>],
      };
    case "stop-workbench":
      return {
        title: `Stop the workbench in ${pending.project}?`, confirm: "Stop", runTitle: `Stop the workbench in ${pending.project}`,
        summary: <Content component="p">Stops workbench {WORKBENCH_NAME}, as the dashboard&apos;s Stop does. {RUNNING_SESSION_LOST} Start brings it back with them.</Content>,
        changes: [<>Workbench <code>{WORKBENCH_NAME}</code>: marked stopped, as the dashboard does; its pod goes away.</>],
      };
    case "delete-workbench-storage": {
      const kept = keptStorage(wb, pending.project);
      const inUse = storageInUseText(kept?.usedBy);
      const what = <Content component="p">Deletes {storageText(kept?.size, kept?.storageClass)}, which this tool kept when its workbench was deleted. The next Set up creates new, empty storage.</Content>;
      return {
        title: `Delete the kept storage in ${pending.project}?`, confirm: "Delete", danger: true, runTitle: `Delete the kept storage in ${pending.project}`,
        changes: [<>Deleted: PersistentVolumeClaim <code>{WORKBENCH_STORAGE}</code> (only the one this tool created).</>],
        // In use, Delete is disabled: the reason comes first, and no data-loss
        // alert speaks of a deletion that cannot happen now.
        ...(inUse
          ? { confirmDisabled: true, summary: <Stack hasGutter><StackItem><Alert variant="warning" isInline component="p" title="The storage is in use">{inUse}</Alert></StackItem><StackItem>{what}</StackItem></Stack> }
          : { titleIcon: "danger" as const, summary: what, dataLoss: <>The files of the deleted workbench in <code>{WORKBENCH_STORAGE}</code> are lost.</> }),
      };
    }
    case "teardown-workbench": {
      const { project } = pending;
      const deleteStorage = pending.deleteStorage === true;
      const item = workbenchIn(wb, project);
      const storage = item?.storage;
      const ownStorage = !!storage?.managedByTool;
      const inUse = storageInUseText(storage?.usedBy);
      const running = item?.state === "running" || item?.state === "starting";
      return {
        title: `Delete the workbench in ${project}?`, confirm: "Delete", danger: true, runTitle: `Delete the workbench in ${project}`,
        ...(deleteStorage ? { titleIcon: "danger" as const } : {}),
        summary: (
          <Content component="p">
            Deletes workbench {WORKBENCH_NAME} in {project}. {running ? `${RUNNING_SESSION_LOST} ` : ""}
            {deleteStorage ? "Its storage is deleted too, with its files." : "Its storage and files are kept: setting up again uses them."}
          </Content>
        ),
        changes: [
          <>Deleted: workbench <code>{WORKBENCH_NAME}</code> (only the one this tool created); the platform removes its sign-in and link.</>,
          deleteStorage ? <>Deleted: storage <code>{WORKBENCH_STORAGE}</code>.</> : <>Kept: storage <code>{WORKBENCH_STORAGE}</code>.</>,
        ],
        ...(deleteStorage ? { dataLoss: <>The workbench&apos;s files in <code>{WORKBENCH_STORAGE}</code> are lost.</> } : {}),
        choice: ownStorage ? (
          <Checkbox id="teardown-workbench-storage" label="Also delete its storage (files are lost)" isChecked={deleteStorage && !inUse} isDisabled={!!inUse}
            description={inUse ?? storageText(storage?.size, storage?.storageClass)}
            onChange={(_e, v) => setPending?.({ kind: "teardown-workbench", project, deleteStorage: v })} />
        ) : undefined,
      };
    }
    default:
      return null;
  }
}
