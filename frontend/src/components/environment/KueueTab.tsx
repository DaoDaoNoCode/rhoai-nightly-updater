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
import type { KueueObject, KueueSample, KueueStatus } from "../../types";
import { type ConfirmSpec, type KueuePending, type Pending, ExternalLink, WarningAlert } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, READ_ONLY_SETUP, WhatNext } from "./FeatureLayout";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { ComponentAlert, NoProjectsAlert } from "./UseIt";
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
  raycluster: "Shows on the dashboard's Distributed workloads page (Observe & monitor, Workload metrics), with its CPU and memory use.",
  rayjob: "Shows as a row on the dashboard's Observe & monitor, Infrastructure, Workloads tab (and its Workload on the Distributed workloads page).",
};

/** User-facing explanation beside the Ray image field for why the default reference is chosen. */
export const IMAGE_HELP = "The Ray image the RHOAI docs use with the CodeFlare SDK on x86 (amd64). It is large (it carries CUDA), so the first start can take several minutes. The docs' CPU images (2.52.1-py311-cpu, -py312-cpu) are for IBM Power (ppc64le) only and do not run here.";

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
              <HelperText><HelperTextItem>Use a project of its own, for example {KUEUE_DEFAULT_PROJECT} (create it in the dashboard): Set up labels it <code>{KUEUE_LABEL}=true</code>, so Kueue admits the workloads sent to its queue.</HelperTextItem></HelperText>
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
          <PartItem id="kueue-queues" title="Queues" scope={shownProject ? `Cluster-wide, for project ${shownProject}` : "Cluster-wide, for one project"} status={qStatus}
            unmanaged={!!k?.objects.some((o) => o.foreign)} loading={!k && !kueueError}>
            <Content component="p">
              ResourceFlavor {KUEUE_FLAVOR}, ClusterQueue {KUEUE_CLUSTER_QUEUE} with a CPU and memory quota for the project only, and LocalQueue {KUEUE_LOCAL_QUEUE} in it (never named <code>default</code>, which would make Kueue queue every workload of the project).
            </Content>
            {k?.refusal && !setUp && <Content component="p">Set up cannot run yet: {k.refusal}</Content>}
            {k && k.instance.found && (
              <Content component="p">Kueue: {k.instance.ready ? "ready (KueueReady, and the Kueue CR cluster is Available)" : `not ready${k.instance.message ? `: ${k.instance.message}` : ""}`}.</Content>
            )}
            {k && (setUp || k.objects.some((o) => o.present)) && <Objects k={k} />}
          </PartItem>
          {(["raycluster", "rayjob"] as const).map((kind) => {
            const s = k?.samples.find((x) => x.kind === kind);
            const menu: PartMenuItem[] = s?.managedByTool && !readOnly && !s.terminating
              ? [{ key: "delete", label: `Delete the ${SAMPLE_TITLES[kind]}...`, onClick: () => setPending({ kind: "delete-kueue-sample", sample: kind }), disabledReason: baseReason, isDanger: true }]
              : [];
            const canAdd = setUp && !s?.present && !readOnly && !k?.rayBlocked && shownProject;
            return (
              <PartItem key={kind} id={`kueue-${kind}`} title={SAMPLE_TITLES[kind]} scope={shownProject ? `In project ${shownProject}` : "In the project"} status={sampleStatus(s)}
                statusText={s?.present ? s.state ?? "Waiting for Kueue" : "Not added"} unmanaged={!!s?.foreign} loading={!k && !kueueError} menu={menu}
                actions={canAdd ? <ActionListItem><PrimaryAction variant="secondary" label="Add" reason={baseReason}
                  onClick={() => setPending({ ...kueueSetupPending(shownProject, k), rayCluster: kind === "raycluster", rayJob: kind === "rayjob" } as Pending)} /></ActionListItem> : undefined}>
                <Content component="p">
                  {kind === "raycluster" ? "A small CPU-only RayCluster" : "A RayJob on the same cluster spec that prints the cluster's resources, runs five minutes and completes"}, sent to LocalQueue {KUEUE_LOCAL_QUEUE}. {SAMPLE_WHERE[kind]}
                </Content>
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
          <PartItem id="kueue-optin" title="Dashboard Kueue switch (opt-in)" scope="Shared by the cluster" status={optInOn ? "ready" : "not-set-up"} statusText={optInOn ? "On" : "Off"}
            unmanaged={!!optIn?.profile.foreign} loading={!k && !kueueError}
            actions={setUp && !readOnly ? <ActionListItem><PrimaryAction variant="secondary" label={optInOn ? "Turn off..." : "Turn on..."} reason={baseReason}
              onClick={() => setPending({ kind: optInOn ? "kueue-optout" : "kueue-optin" })} /></ActionListItem> : undefined}>
            <Content component="p">
              Off by default. On sets the dashboard&apos;s <code>disableKueue</code> to false, which makes every new dashboard project a Kueue project, and adds hardware profile {KUEUE_PROFILE} for LocalQueue {KUEUE_LOCAL_QUEUE}, so workbenches and models can start in {shownProject ?? "the project"}.
            </Content>
            {optIn && <Content component="p">The dashboard&apos;s disableKueue is {optIn.disableKueue === undefined ? "unset (the dashboard takes it as true)" : String(optIn.disableKueue)}{optIn.on && !optIn.setByTool ? "; someone changed it since this tool's opt-in, so Clean up leaves it" : ""}.</Content>}
          </PartItem>
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem><PrimaryAction label={next.label} variant={props.prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(next.setup)} reason={reason} /></StackItem>
      )}
      {setUp && (href || infra) && (
        <StackItem>
          <WhatNext id="kueue-next" items={[
            ...(href ? [<><ExternalLink href={href} isInline>Open Distributed workloads in the dashboard</ExternalLink>: the RayCluster sample&apos;s status, quota use and metrics.</>] : []),
            ...(infra ? [<><ExternalLink href={infra} isInline>Open Observe &amp; monitor, Infrastructure</ExternalLink>: the RayJob sample on its Workloads tab.</>] : []),
            <>Send your own Ray or training jobs to the queue with the label <code>kueue.x-k8s.io/queue-name: {KUEUE_LOCAL_QUEUE}</code>.</>,
          ]} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Prerequisites (above): the cert-manager Operator and the Red Hat build of Kueue from OperatorHub, then the DataScienceCluster&apos;s kueue set to Unmanaged; RHOAI then creates the Kueue CR cluster. The operators stay after Clean up.</>,
            <>ResourceFlavor <code>{KUEUE_FLAVOR}</code>, ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code> ({k?.quota ?? "a CPU and memory quota"}, for the project only, not preemptive) and LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code> in the project, as the RHOAI docs create them.</>,
            <>The label <code>{KUEUE_LABEL}=true</code> on the project, if it does not have it.</>,
            <>With the samples: RayCluster <code>nightly-raycluster</code> and RayJob <code>nightly-rayjob</code> (head and one worker, 1 CPU and 2 GiB each).</>,
            <>With the opt-in: <code>disableKueue: false</code> in the dashboard&apos;s settings and hardware profile <code>{KUEUE_PROFILE}</code>.</>,
          ]} />
          <Content component="p"><ExternalLink href={KUEUE_DOCS} isInline>RHOAI docs: Manage workloads with Kueue</ExternalLink></Content>
        </FeatureSection>
      </StackItem>
      {!readOnly && ours && (
        <StackItem>
          <CleanupSection id="kueue-cleanup" label="Clean up distributed workloads..." onClick={onCleanup} reason={baseReason}
            text="Deletes the samples, the queues and the project label this tool created, and turns its dashboard opt-in back. The operators stay." />
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
      return [{ id: "check", title: "Check the dashboard's settings" }, { id: "dashboard", title: "Set disableKueue to false" }, { id: "profile", title: `Create hardware profile ${KUEUE_PROFILE}` }];
    case "kueue-optout":
      return [{ id: "opt-in", title: "Turn the dashboard opt-in back" }];
    default:
      return [
        { id: "samples", title: "Delete the samples" },
        { id: "queues", title: "Delete the queues" },
        { id: "project", title: "Remove the project label this tool added" },
        { id: "opt-in", title: "Turn the dashboard opt-in back" },
      ];
  }
}

/** One-line description of dashboard opt-in behavior for projects created afterward. */
export const OPT_IN_EFFECT = "Every project created in the dashboard from now on gets the label kueue.openshift.io/managed=true, and its workbenches and model deployments then need a hardware profile with a local queue that the project has: without one, the dashboard offers none and they cannot be created.";

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
                  {others.join(", ")}. Kueue manages only what is sent to a queue, so they keep running; but a project of its own is safer, because once the dashboard&apos;s Kueue switch is on, new workbenches and models there need a Kueue hardware profile.
                </Alert>
              </StackItem>
            )}
            <StackItem>
              <Checkbox id="kueue-raycluster" label="RayCluster sample" isChecked={p.rayCluster} onChange={(_e, v) => setPending({ ...p, rayCluster: v })}
                description="Shows on the dashboard's Distributed workloads page, with usage metrics." />
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
            <>ResourceFlavor <code>{KUEUE_FLAVOR}</code> (empty), ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code> ({k?.quota ?? "CPU and memory quota"}, namespaceSelector: project {p.project} only, no preemption) and LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code> in {p.project}, created with your OpenShift account; their UIDs are recorded.</>,
            labelled ? <>Project {p.project} has the label <code>{KUEUE_LABEL}=true</code> already; it stays as it is.</> : <>Project {p.project} gets the label <code>{KUEUE_LABEL}=true</code> (recorded; Clean up removes it).</>,
          ]),
          ...(p.rayCluster ? [<>RayCluster <code>nightly-raycluster</code> in {p.project}: head and one worker, 1 CPU and 2 GiB each, image <code>{p.rayImage}</code>.</>] : []),
          ...(p.rayJob ? [<>RayJob <code>nightly-rayjob</code> in {p.project} on the same cluster, plus its submitter (0.5 CPU, 1 GiB).</>] : []),
          <>The dashboard&apos;s settings are not changed (the opt-in is a separate step).</>,
        ],
      };
    }
    case "delete-kueue-sample":
      return {
        title: `Delete the ${SAMPLE_TITLES[pending.sample]}?`, confirm: "Delete", danger: true, runTitle: `Delete the ${SAMPLE_TITLES[pending.sample]}`,
        summary: <Content component="p">Deletes {pending.sample === "rayjob" ? "RayJob nightly-rayjob" : "RayCluster nightly-raycluster"} in {k?.project}, only if it is the one this tool created.</Content>,
        changes: [<>Its Kueue Workload and its pods go with it.</>],
      };
    case "kueue-optin":
      return {
        title: "Turn on the dashboard's Kueue switch?", confirm: "Turn on", runTitle: "Turn on the dashboard's Kueue switch", titleIcon: "warning", changesOpen: true,
        summary: (
          <Stack hasGutter>
            <StackItem><Alert variant="warning" isInline component="p" title="This changes every new dashboard project">{OPT_IN_EFFECT}</Alert></StackItem>
            <StackItem><Content component="p">Existing projects keep their labels. Workbenches and models in project {k?.project} can then use hardware profile {KUEUE_PROFILE}.</Content></StackItem>
          </Stack>
        ),
        changes: [
          <>OdhDashboardConfig <code>odh-dashboard-config</code>: <code>spec.dashboardConfig.disableKueue: false</code> (it is {k?.optIn.disableKueue === undefined ? "unset" : String(k.optIn.disableKueue)}; the value before is recorded).</>,
          <>HardwareProfile <code>{KUEUE_PROFILE}</code> in the applications namespace: type Queue, local queue {KUEUE_LOCAL_QUEUE}, 1 CPU and 2 GiB by default (up to 2 CPUs and 4 GiB).</>,
          <>Turning it off (or Clean up) restores the value and deletes the profile, only if nobody changed them since.</>,
        ],
      };
    case "kueue-optout":
      return {
        title: "Turn off the dashboard's Kueue switch?", confirm: "Turn off", runTitle: "Turn off the dashboard's Kueue switch",
        summary: <Content component="p">Restores <code>disableKueue</code> to its value before this tool&apos;s opt-in ({k?.optIn.previous ?? "unset"}) and deletes hardware profile {KUEUE_PROFILE}.</Content>,
        changes: [
          <>Each only while nobody changed it since this tool&apos;s opt-in; otherwise it stays, and the result says so.</>,
          <>Projects the dashboard labelled while it was on keep their label.</>,
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
    <>The samples, then LocalQueue <code>{KUEUE_LOCAL_QUEUE}</code>, ClusterQueue <code>{KUEUE_CLUSTER_QUEUE}</code> and ResourceFlavor <code>{KUEUE_FLAVOR}</code>, each only while it is this tool&apos;s (its UID) and nothing else uses it.</>,
    k?.label?.addedByTool ? <>The label <code>{KUEUE_LABEL}</code> on {k.project}, once no Kueue workload is left there.</> : <>The project&apos;s labels stay (this tool added none).</>,
    k?.optIn.on ? <>The dashboard opt-in: disableKueue back to {k.optIn.previous ?? "unset"} and hardware profile {KUEUE_PROFILE} deleted, only if nobody changed them.</> : <>The dashboard&apos;s settings stay (this tool did not change them).</>,
    <>The Red Hat build of Kueue, cert-manager and the Kueue CR cluster stay: other users may rely on them.</>,
  ];
}
