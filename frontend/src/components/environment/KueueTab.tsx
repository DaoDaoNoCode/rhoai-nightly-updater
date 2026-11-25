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
  TextInput,
} from "@patternfly/react-core";
import type { KueueObject, KueueSample, KueueStatus, RestorableComponent } from "../../types";
import { type ConfirmSpec, type KueuePending, type Pending, ExternalLink, WarningAlert } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, READ_ONLY_SETUP, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, NoProjectsAlert } from "./UseIt";
import { componentLabel } from "./CleanupModal";
import { LoadFailed } from "./Loading";
import type { Busy, FeatureState } from "./featureState";
import {
  KUEUE_CLUSTER_QUEUE, KUEUE_DEFAULT_IMAGE, KUEUE_DEFAULT_PROJECT, KUEUE_FLAVOR, KUEUE_LABEL, KUEUE_LOCAL_QUEUE, KUEUE_PROFILE,
  kueueDashboardHref, kueueInfrastructureHref, kueueSetUp, kueueSetupPending, queuesStatus, sampleStatus,
} from "./kueueState";

/** Documentation URL for RHOAI "Managing OpenShift AI" — the Kueue workload-management chapter. */
export const KUEUE_DOCS = "https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/latest/html-single/managing_openshift_ai/index";

const SAMPLE_TITLES: Record<KueueSample["kind"], string> = { raycluster: "RayCluster sample", rayjob: "RayJob sample" };
const SAMPLE_WHERE: Record<KueueSample["kind"], string> = {
  raycluster: "Shows on the dashboard's Workload metrics page (Observe & monitor), with its CPU and memory use.",
  rayjob: "Shows as a row on the dashboard's Observe & monitor, Infrastructure, Workloads tab (and its Workload on the Workload metrics page).",
};

/** User-facing explanation beside the Ray image field for why the default reference is chosen. */
export const IMAGE_HELP = "Ray 2.47.1 is the default because it is the newest x86 image the RHOAI docs list (with the CodeFlare SDK). It is large (it carries CUDA), so the first start can take several minutes. The docs' newer CPU-only images (2.52.1-py311-cpu, -py312-cpu) are for IBM Power (ppc64le) and do not run on x86 (amd64) clusters.";

/** The opt-in's one name, everywhere. */
export const OPT_IN_NAME = "Dashboard Kueue setting";

export interface KueueTabProps {
  prerequisites?: React.ReactNode;
  prerequisitesReason?: string | null;
  kueue: KueueStatus | null;
  kueueError: string | null;
  onRetry: () => void;
  dashboardUrl?: string;
  project: string | null;
  projects: ProjectOption[];
  projectsLoaded: boolean;
  onProject: (project: string) => void;
  onClearProject: () => void;
  missingProject: string | null;
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
}

const objectText = (o: KueueObject | undefined) => {
  if (!o?.present) return "Not created yet";
  if (o.foreign) return "Exists, not created by this tool";
  if (o.terminating) return "Being deleted";
  if (o.outdated) return "Made by an earlier version of this tool";
  if (o.kind !== "ResourceFlavor" && !o.active) return `Not active yet${o.message ? `: ${o.message}` : ""}`;
  return "Created by this tool";
};

const Objects: React.FC<{ k: KueueStatus }> = ({ k }) => (
  <DescriptionList isCompact isHorizontal aria-label="Queues">
    {k.objects.map((o) => (
      <DescriptionListGroup key={o.kind}>
        <DescriptionListTerm>{o.kind}</DescriptionListTerm>
        <DescriptionListDescription><code>{o.namespace ? `${o.namespace}/` : ""}{o.name}</code>: {objectText(o)}</DescriptionListDescription>
      </DescriptionListGroup>
    ))}
    <DescriptionListGroup>
      <DescriptionListTerm>Quota</DescriptionListTerm>
      <DescriptionListDescription>{k.quota}, for project {k.project ?? "(the one chosen)"} only; not preemptive</DescriptionListDescription>
    </DescriptionListGroup>
    {k.label && (
      <DescriptionListGroup>
        <DescriptionListTerm>Project label</DescriptionListTerm>
        <DescriptionListDescription><code>{KUEUE_LABEL}=true</code>: {k.label.present ? (k.label.addedByTool ? "added by this tool" : "set before this tool's setup; it stays") : "not set"}</DescriptionListDescription>
      </DescriptionListGroup>
    )}
  </DescriptionList>
);

/**
 * Renders the Distributed workloads tab — prerequisite state (cert-manager, Red Hat
 * build of Kueue, DataScienceCluster kueue Unmanaged), project selection, queues
 * owned by this app, optional samples, dashboard Kueue opt-in, and cleanup controls.
 */
export const KueueTab: React.FC<KueueTabProps> = (props) => {
  const { kueue: k, kueueError, project, projects, projectsLoaded, onProject, onClearProject, missingProject, state, busy, baseReason, readOnly, setPending, onSetUp, onCleanup } = props;
  const next = state.next;
  const setUp = kueueSetUp(k);
  const setUpIn = k?.setUpIn;
  const shownProject = k?.project ?? project;
  const ours = !!k && (k.objects.some((o) => o.managedByTool) || !!k.label?.addedByTool || k.samples.some((s) => s.managedByTool) || !!k.optIn.on);
  const href = kueueDashboardHref(k);
  const infra = kueueInfrastructureHref(k);
  const reason = readOnly ? READ_ONLY_SETUP : baseReason ?? props.prerequisitesReason ?? k?.refusal ?? null;
  const qStatus = queuesStatus(k, busy);
  const optIn = k?.optIn;
  const optInOn = !!optIn?.on;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Distributed workloads" purpose={FEATURE_PURPOSE.kueue} state={state} /></StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {!k && kueueError && <StackItem><LoadFailed title="Cannot read distributed workloads" error={kueueError} onRetry={props.onRetry} /></StackItem>}
      {k && kueueError && (
        <StackItem>
          <Alert variant="warning" title="Distributed workloads status may be out of date" isInline component="p" actionLinks={<AlertActionLink onClick={props.onRetry}>Retry</AlertActionLink>}>
            The data below is from the last successful refresh. {kueueError}
          </Alert>
        </StackItem>
      )}
      {k?.rayBlocked && <StackItem><ComponentAlert feature="Ray" block={k.rayBlocked} /></StackItem>}
      {k?.itemsProblem && <StackItem><WarningAlert text={k.itemsProblem} /></StackItem>}
      {projectsLoaded && projects.length === 0 && <StackItem><NoProjectsAlert dashboardUrl={props.dashboardUrl} things="Distributed workloads" /></StackItem>}
      {missingProject && (
        <StackItem><Alert variant="info" isInline component="p" title={`Project ${missingProject} no longer exists`}>Choose another project.</Alert></StackItem>
      )}
      {setUpIn && (
        <StackItem>
          <Alert variant="info" isInline component="p" title={`Set up in project ${setUpIn}`}>This tool sets distributed workloads up in one project; the queues below are those in {setUpIn}. Clean them up to use another project.</Alert>
        </StackItem>
      )}
      <StackItem>
        <Form isWidthLimited onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Project" fieldId="kueue-project">
            <ProjectSelect id="kueue-project" projects={projects} selected={shownProject} onSelect={onProject} onClear={onClearProject} />
            <FormHelperText>
              <HelperText><HelperTextItem>Best to use a project of its own, for example {KUEUE_DEFAULT_PROJECT}; create it in the dashboard first. Set up adds the label <code>{KUEUE_LABEL}=true</code> to it, so that Kueue admits the workloads sent to its queue.</HelperTextItem></HelperText>
            </FormHelperText>
          </FormGroup>
        </Form>
      </StackItem>
      {k?.defaultQueue && !setUp && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="This project has a LocalQueue named default">Once the project is labelled, Kueue would queue every new Deployment, StatefulSet and Pod in it there, so Set up refuses it. Choose another project.</Alert>
        </StackItem>
      )}
      <StackItem>
        <PartList aria-label="What distributed workloads need">
          <PartItem id="kueue-queues" title="Queues" scope={shownProject ? `Queues for project ${shownProject} (the ClusterQueue and ResourceFlavor are cluster-wide objects; only this project uses them)` : "Queues for one project (the ClusterQueue and ResourceFlavor are cluster-wide)"} status={qStatus}
            unmanaged={!!k?.objects.some((o) => o.foreign)} loading={!k && !kueueError}>
            <Content component="p">
              A queue for the project with a CPU and memory quota, as the RHOAI docs set it up (the objects are listed below and under What this creates). Its LocalQueue is never named <code>default</code>, because Kueue would put every workload of the project that has no queue label into a LocalQueue named <code>default</code>.
            </Content>
            {k?.refusal && !setUp && !(next?.kind === "set-up" && reason === k.refusal) && <Content component="p">{k.refusal}</Content>}
            {k && k.instance.found && (
              <Content component="p">{k.instance.ready ? "Kueue is running." : `Kueue is not running yet${k.instance.message ? `: ${k.instance.message}` : ""}.`}</Content>
            )}
            {k?.objects.map((o) => o.outdated && (
              <Alert key={o.kind} variant="warning" isInline component="p" title={`${o.kind} ${o.name} was made by an earlier version of this tool`}>
                {`${o.outdated[0].toUpperCase()}${o.outdated.slice(1)}. Set up keeps it as it is: Clean up, then Set up again.`}
              </Alert>
            ))}
            {k && (setUp || k.objects.some((o) => o.present)) && <Objects k={k} />}
          </PartItem>
          {(["raycluster", "rayjob"] as const).map((kind) => {
            const s = k?.samples.find((x) => x.kind === kind);
            const menu: PartMenuItem[] = s?.managedByTool && !readOnly && !s.terminating
              ? [{ key: "delete", label: `Delete the ${SAMPLE_TITLES[kind]}`, onClick: () => setPending({ kind: "delete-kueue-sample", sample: kind }), disabledReason: baseReason, isDanger: true }]
              : [];
            const canAdd = setUp && !s?.present && !readOnly && !k?.rayBlocked && shownProject;
            return (
              <PartItem key={kind} id={`kueue-${kind}`} title={SAMPLE_TITLES[kind]} scope={shownProject ? `In project ${shownProject}` : "In the project"} status={sampleStatus(s)}
                statusText={s?.present ? (s.outdated && !s.terminating ? "Outdated" : s.state ?? "Waiting for Kueue") : "Not added"} unmanaged={!!s?.foreign} loading={!k && !kueueError} menu={menu}
                actions={canAdd ? <ActionListItem><PrimaryAction variant="secondary" label="Add" reason={baseReason}
                  onClick={() => setPending({ ...kueueSetupPending(shownProject, k), rayCluster: kind === "raycluster", rayJob: kind === "rayjob" } as Pending)} /></ActionListItem> : undefined}>
                <Content component="p">
                  {kind === "raycluster" ? "A small CPU-only RayCluster" : "A RayJob on the same cluster spec that prints the cluster's resources, runs five minutes and completes"}, sent to LocalQueue {KUEUE_LOCAL_QUEUE}. {SAMPLE_WHERE[kind]}
                </Content>
                {s?.outdated && s.present && (
                  <Alert variant="warning" isInline component="p" title={`${SAMPLE_TITLES[kind]} made by an earlier version of this tool`}>
                    {`${s.outdated[0].toUpperCase()}${s.outdated.slice(1)}. Delete it and add it again, or Clean up, then Set up again.`}
                  </Alert>
                )}
                {s?.message && s.present && <Content component="p">Kueue: {s.message}</Content>}
                {s?.present && (s.image || s.rayState) && (
                  <DescriptionList isCompact isHorizontal aria-label={`${SAMPLE_TITLES[kind]} details`}>
                    {s.image && <DescriptionListGroup><DescriptionListTerm>Image</DescriptionListTerm><DescriptionListDescription><code>{s.image}</code></DescriptionListDescription></DescriptionListGroup>}
                    {s.rayState && <DescriptionListGroup><DescriptionListTerm>Ray</DescriptionListTerm><DescriptionListDescription>{s.rayState}</DescriptionListDescription></DescriptionListGroup>}
                  </DescriptionList>
                )}
              </PartItem>
            );
          })}
          <PartItem id="kueue-optin" title={OPT_IN_NAME} scope="Shared by the cluster" status={optInOn ? "ready" : "not-set-up"} statusText={optInOn ? "On" : "Off"}
            unmanaged={!!optIn?.profile.foreign} loading={!k && !kueueError}
            actions={setUp && !readOnly ? <ActionListItem><PrimaryAction variant="secondary" label={optInOn ? "Turn off" : "Turn on"} reason={baseReason}
              onClick={() => setPending({ kind: optInOn ? "kueue-optout" : "kueue-optin" })} /></ActionListItem> : undefined}>
            <Content component="p">
              Off by default. Turning it on makes every new dashboard project a Kueue project (<code>disableKueue: false</code>) and adds hardware profile {KUEUE_PROFILE} for LocalQueue {KUEUE_LOCAL_QUEUE}, so that workbenches and models can start in {shownProject ?? "the project"}.
            </Content>
            {optIn && <Content component="p">{optIn.disableKueue === false ? "Kueue is on in the dashboard." : optIn.disableKueue === undefined ? "Kueue is off in the dashboard (the default)." : "Kueue is off in the dashboard."}{optIn.on && !optIn.setByTool ? " Someone changed it after this tool turned it on, so Clean up leaves it as it is." : ""}</Content>}
          </PartItem>
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem><PrimaryAction label={next.label} variant={props.prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(next.setup)} reason={reason} /></StackItem>
      )}
      {setUp && (href || infra) && (
        <StackItem>
          <WhatNext id="kueue-next" items={[
            ...(href ? [<><ExternalLink href={href} isInline>Open Workload metrics in the dashboard</ExternalLink>: the RayCluster sample&apos;s status, quota use and metrics.</>] : []),
            ...(infra ? [<><ExternalLink href={infra} isInline>Open Observe &amp; monitor, Infrastructure</ExternalLink>: the RayJob sample on its Workloads tab.</>] : []),
            <>Send your own Ray or training jobs to the queue with the label <code>kueue.x-k8s.io/queue-name: {KUEUE_LOCAL_QUEUE}</code>.</>,
          ]} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Prerequisites (above): the cert-manager Operator and the Red Hat build of Kueue from OperatorHub, then the DataScienceCluster&apos;s kueue set to Unmanaged, which means RHOAI uses the Kueue operator you install instead of its own. RHOAI then starts Kueue (the Kueue object named <code>cluster</code>); the tab says &quot;Kueue is running&quot; once RHOAI reports it ready. The operators stay after Clean up.</>,
            <>ResourceFlavor <code>{KUEUE_FLAVOR}</code>: a name for the kind of nodes the quota is for (here any node).</>,
            <>ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code>: the quota, {k?.quota ?? "CPU and memory"}, for the project only. It is not preemptive: a waiting workload never stops a running one.</>,
            <>LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code> in the project: where its workloads are sent (label <code>kueue.x-k8s.io/queue-name</code>), drawing on the ClusterQueue&apos;s quota.</>,
            <>The label <code>{KUEUE_LABEL}=true</code> on the project, if it does not have it.</>,
            <>With the samples: RayCluster <code>nightly-raycluster</code> and RayJob <code>nightly-rayjob</code> (sized as the CodeFlare SDK&apos;s defaults: a head of 1 CPU and 5 GiB, up to 2 CPUs and 8 GiB, and one worker of 1 CPU and 3 GiB, up to 6 GiB).</>,
            <>With the {OPT_IN_NAME}: <code>disableKueue: false</code> in the dashboard&apos;s settings and hardware profile <code>{KUEUE_PROFILE}</code>.</>,
          ]} />
          <Content component="p" style={{ marginTop: "var(--pf-t--global--spacer--md)" }}><ExternalLink href={KUEUE_DOCS} isInline>RHOAI docs: Manage workloads with Kueue</ExternalLink></Content>
        </FeatureSection>
      </StackItem>
      {!readOnly && ours && (
        <StackItem>
          <CleanupSection id="kueue-cleanup" label="Clean up distributed workloads" onClick={onCleanup} reason={baseReason}
            text={`Deletes the samples, the queues and the project label this tool created, and turns the ${OPT_IN_NAME} back to its earlier value if nobody changed it. The operators stay.`} />
        </StackItem>
      )}
    </Stack>
  );
};

/** Titles for each step in a distributed-workloads job (backed by pkg/cluster/internal/kueue Run). */
export function kueueSteps(p: KueuePending): { id: string; title: string }[] {
  switch (p.kind) {
    case "setup-kueue":
      return [
        { id: "check", title: "Check that Kueue can run workloads in the project" },
        { id: "queues", title: "Create the queues" },
        { id: "project", title: `Label the project ${KUEUE_LABEL}=true` },
        ...(p.rayCluster || p.rayJob ? [{ id: "samples", title: "Create the samples" }] : []),
      ];
    case "delete-kueue-sample":
      return [{ id: "samples", title: `Delete the ${SAMPLE_TITLES[p.sample]}` }];
    case "kueue-optin":
      return [{ id: "check", title: "Check the setup and the dashboard's settings" }, { id: "dashboard", title: "Turn Kueue on in the dashboard" }, { id: "profile", title: `Create hardware profile ${KUEUE_PROFILE}` }];
    case "kueue-optout":
      return [{ id: "opt-in", title: `Turn the ${OPT_IN_NAME} back` }];
    default:
      return [
        { id: "samples", title: "Delete the samples" },
        { id: "queues", title: "Delete the queues" },
        { id: "project", title: "Remove the project label this tool added" },
        { id: "opt-in", title: `Turn the ${OPT_IN_NAME} back` },
      ];
  }
}

/**
 * The Distributed workloads tab's Turn back for the DataScienceCluster's kueue, or null: offered only while this tool's
 * setup left it as it is (features.StillAsSetUp: the prerequisites' restorable) and no ClusterQueue or LocalQueue is on
 * the cluster (the rule Clean up checks again when it runs).
 */
export function kueueTurnBack(restorable: RestorableComponent[] | undefined, k: KueueStatus | null,
  { readOnly = false, baseReason = null, running = false }: { readOnly?: boolean; baseReason?: string | null; running?: boolean } = {}): { restorable: RestorableComponent; reason: string | null } | null {
  const r = (restorable ?? []).find((c) => c.component === "kueue");
  // Hidden in read-only mode; otherwise shown, off with the reason.
  if (!r || readOnly) return null;
  if (baseReason) return { restorable: r, reason: baseReason };
  if (running) return { restorable: r, reason: "Wait until the distributed workloads change finishes." };
  if (!k) return { restorable: r, reason: "Reading which queues are on the cluster." };
  if (k.queuesUnknown || !Array.isArray(k.queues)) return { restorable: r, reason: "The queues on the cluster could not be read, so this tool cannot tell whether anything uses Kueue." };
  if (k.queues.length > 0) {
    const shown = k.queues.length > 3 ? [...k.queues.slice(0, 3), `${k.queues.length - 3} more`] : k.queues;
    return { restorable: r, reason: `In use: ${shown.join(", ")}. Kueue goes back only once no ClusterQueue or LocalQueue is left on the cluster.` };
  }
  return { restorable: r, reason: null };
}

/** One-line description of dashboard opt-in behavior for projects created afterward. */
export const OPT_IN_EFFECT = "In every new project on this cluster, workbenches and model deployments can only use the queue hardware profile; a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one.";

/** "a pipeline server and a workbench". */
const listText = (items: string[]) => (items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`);

/**
 * Returns confirm-dialog content for a pending Kueue action, or null if pending
 * does not apply here. Parameter others names other features already set up in that project.
 */
export function kueueConfirm(pending: Pending, k: KueueStatus | null, setPending: (p: Pending) => void, others: string[] = []): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-kueue": {
      const p = pending;
      const exists = kueueSetUp(k);
      const imageProblem = (p.rayCluster || p.rayJob) && !/^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]*$/.test(p.rayImage);
      const labelled = !!k?.label?.present && k.project === p.project;
      return {
        title: exists ? "Add the samples?" : "Set up distributed workloads?", confirm: exists ? "Add" : "Set up",
        runTitle: exists ? `Add distributed workload samples in ${p.project}` : `Set up distributed workloads in ${p.project}`,
        confirmDisabled: imageProblem || (exists && !p.rayCluster && !p.rayJob),
        summary: <Content component="p">{exists
          ? <>Adds the ticked samples to project {p.project}, sent to LocalQueue {KUEUE_LOCAL_QUEUE}.</>
          : <>Queues with {k?.quota ?? "a CPU and memory quota"} for project {p.project}, as the RHOAI docs set them up, and the samples you tick.</>}</Content>,
        choice: (
          <Stack hasGutter>
            {others.length > 0 && (
              <StackItem>
                <Alert variant="warning" isInline component="p" title={`Project ${p.project} already has other setups`}>
                  This project already has {listText(others)}. Kueue manages only what is sent to a queue, so {others.length === 1 ? "it keeps" : "they keep"} running. A project of its own is still safer: once the {OPT_IN_NAME} is on, new workbenches and models there need the queue hardware profile.
                </Alert>
              </StackItem>
            )}
            <StackItem>
              <Checkbox id="kueue-raycluster" label="RayCluster sample" isChecked={p.rayCluster} onChange={(_e, v) => setPending({ ...p, rayCluster: v })}
                description="Shows on the dashboard's Workload metrics page (Observe & monitor), with usage metrics." />
            </StackItem>
            <StackItem>
              <Checkbox id="kueue-rayjob" label="RayJob sample" isChecked={p.rayJob} onChange={(_e, v) => setPending({ ...p, rayJob: v })}
                description="Shows on Observe & monitor, Infrastructure, Workloads; it completes after five minutes." />
            </StackItem>
            {(p.rayCluster || p.rayJob) && (
              <StackItem>
                <FormGroup label="Ray image" fieldId="kueue-ray-image">
                  <TextInput id="kueue-ray-image" value={p.rayImage} onChange={(_e, v) => setPending({ ...p, rayImage: v.trim() })} validated={imageProblem ? "error" : "default"} />
                  <FormHelperText><HelperText><HelperTextItem variant={imageProblem ? "error" : "default"}>{imageProblem ? "Enter an image reference, for example " + KUEUE_DEFAULT_IMAGE + "." : IMAGE_HELP}</HelperTextItem></HelperText></FormHelperText>
                </FormGroup>
              </StackItem>
            )}
          </Stack>
        ),
        changes: [
          ...(exists ? [] : [
            <>ResourceFlavor <code>{KUEUE_FLAVOR}</code>: the kind of nodes the quota is for (any node).</>,
            <>ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code>: {k?.quota ?? "a CPU and memory quota"}, for project {p.project} only; not preemptive: a waiting workload never stops a running one.</>,
            <>LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code> in {p.project}: where the project&apos;s workloads are sent.</>,
            <>They are created with your OpenShift account, and this tool records them, so Clean up deletes only these.</>,
            labelled ? <>Project {p.project} has the label <code>{KUEUE_LABEL}=true</code> already; it stays as it is.</> : <>Project {p.project} gets the label <code>{KUEUE_LABEL}=true</code> (recorded; Clean up removes it).</>,
          ]),
          ...(p.rayCluster ? [<>RayCluster <code>nightly-raycluster</code> in {p.project}: a head of 1 CPU and 5 GiB (up to 2 CPUs and 8 GiB) and one worker of 1 CPU and 3 GiB (up to 6 GiB), image <code>{p.rayImage}</code>.</>] : []),
          ...(p.rayJob ? [<>RayJob <code>nightly-rayjob</code> in {p.project} on the same cluster, plus its submitter (0.5 CPU, 1 GiB).</>] : []),
          <>The dashboard&apos;s settings are not changed (the {OPT_IN_NAME} is a separate step).</>,
        ],
      };
    }
    case "delete-kueue-sample":
      return {
        title: `Delete the ${SAMPLE_TITLES[pending.sample]}?`, confirm: "Delete", danger: true, runTitle: `Delete the ${SAMPLE_TITLES[pending.sample]}`,
        summary: <Content component="p">Deletes {pending.sample === "rayjob" ? "RayJob nightly-rayjob" : "RayCluster nightly-raycluster"} in {k?.project}, only if it is the one this tool created.</Content>,
        changes: [<>Its Kueue Workload and its pods go with it.</>],
      };
    case "kueue-optin": {
      const apps = k?.optIn.profile.namespace ?? "the applications namespace";
      const now = k?.optIn.disableKueue === false ? "on" : k?.optIn.disableKueue === undefined ? "off (the default)" : "off";
      const profile = <code className="pf-v6-u-text-nowrap">{KUEUE_PROFILE}</code>;
      return {
        title: `Turn on the ${OPT_IN_NAME}?`, confirm: "Turn on", runTitle: `Turn on the ${OPT_IN_NAME}`, changesOpen: true,
        summary: (
          <Stack hasGutter>
            <StackItem><Alert variant="warning" isInline component="p" title="This changes every new project">{OPT_IN_EFFECT}</Alert></StackItem>
            <StackItem><Content component="p">Existing projects keep their labels. Workbenches and models in project {k?.project} can then use hardware profile {profile}.</Content></StackItem>
            <StackItem><Content component="p">Turning it off, or Clean up, puts the setting back and deletes the profile. If someone changes either after this, Clean up leaves it as it is.</Content></StackItem>
          </Stack>
        ),
        changes: [
          <>OdhDashboardConfig <code>odh-dashboard-config</code> in {apps}: <code>spec.dashboardConfig.disableKueue: false</code>, which turns Kueue on in the dashboard. Now: {now}. This tool records it so Clean up can turn it back.</>,
          <>HardwareProfile {profile} in the applications namespace {apps}: type Queue, local queue {KUEUE_LOCAL_QUEUE}, 1 CPU and 2 GiB by default (up to 2 CPUs and 4 GiB).</>,
        ],
      };
    }
    case "kueue-optout":
      return {
        title: `Turn off the ${OPT_IN_NAME}?`, confirm: "Turn off", runTitle: `Turn off the ${OPT_IN_NAME}`,
        summary: <Content component="p">Turns Kueue in the dashboard back to how it was before ({k?.optIn.previous === "true" ? "off" : "off, the default"}) and deletes hardware profile {KUEUE_PROFILE}.</Content>,
        changes: [
          <>Each only if nobody changed it after this tool turned the setting on; otherwise it stays, and the result says so.</>,
          <>Projects the dashboard labelled while it was on keep their label.</>,
        ],
      };
    case "turn-back-kueue":
      return {
        title: "Turn Kueue back?", confirm: "Turn back", runTitle: "Turn the DataScienceCluster's kueue back",
        summary: <Content component="p">{componentLabel({ component: "kueue", previous: pending.previous, feature: "kueue" })}</Content>,
        changes: [
          <>DataScienceCluster: <code>kueue</code> goes back to {pending.previous || "Removed"}, as Clean up everything does it: only while this tool still owns the setting and no ClusterQueue or LocalQueue is left (checked again when it runs; otherwise it stays, and the result says why).</>,
        ],
      };
    case "teardown-kueue":
      return {
        title: "Clean up distributed workloads?", confirm: "Clean up", danger: true, runTitle: "Clean up distributed workloads",
        summary: <Content component="p">Deletes what this tool set up for distributed workloads in {k?.project}.</Content>,
        changes: kueueCleanupLines(k),
        dataLoss: <>The samples&apos; Ray clusters and their logs.</>,
      };
    default:
      return null;
  }
}

/** Ordered teardown bullets for Kueue resources this application provisioned. */
export function kueueCleanupLines(k: KueueStatus | null): React.ReactNode[] {
  return [
    <>The samples, then LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code>, ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code> and ResourceFlavor <code>{KUEUE_FLAVOR}</code>, each only if this tool still owns it and nothing else uses it.</>,
    <>Jobs people sent to LocalQueue {KUEUE_LOCAL_QUEUE} themselves are never admitted once the queues are gone (those jobs are deleted only when their project is deleted).</>,
    k?.label?.addedByTool ? <>The label <code>{KUEUE_LABEL}</code> on {k.project}, once no Kueue workload is left there.</> : <>The project&apos;s labels stay (this tool added none).</>,
    k?.optIn.on ? <>The {OPT_IN_NAME} goes back to how it was, and hardware profile {KUEUE_PROFILE} is deleted, each only if nobody changed it.</> : <>The dashboard&apos;s settings stay (this tool did not change them).</>,
    <>The Red Hat build of Kueue and cert-manager stay: others may rely on them.</>,
  ];
}
